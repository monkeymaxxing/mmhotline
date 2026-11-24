package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxUnresolvedIncidents = 1000

const incidentSchema = `
CREATE TABLE sources(id TEXT PRIMARY KEY,name TEXT NOT NULL,kind TEXT NOT NULL,active INTEGER NOT NULL,created_at INTEGER NOT NULL,last_heard_at INTEGER NOT NULL DEFAULT 0,warn_after_no_check_in_for INTEGER NOT NULL DEFAULT 120,sos_after_no_check_in_for INTEGER NOT NULL DEFAULT 300,` + repeatedWarningsColumn + `);
CREATE TABLE signing_secrets(id TEXT PRIMARY KEY,source_id TEXT NOT NULL REFERENCES sources(id),secret BLOB NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX signing_secrets_source ON signing_secrets(source_id);
CREATE TABLE source_rates(source_id TEXT PRIMARY KEY REFERENCES sources(id),bucket INTEGER NOT NULL,alert_count INTEGER NOT NULL);
CREATE TABLE team_alert_rate(id INTEGER PRIMARY KEY CHECK(id=1),hour_bucket INTEGER NOT NULL,warning_and_info_alerts INTEGER NOT NULL);
CREATE TABLE alerts(sequence INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,source_id TEXT NOT NULL,sender_alert_id TEXT NOT NULL,actor TEXT NOT NULL,replay_key TEXT NOT NULL,incident_id TEXT NOT NULL,sender_incident_key TEXT NOT NULL,severity TEXT NOT NULL,title TEXT NOT NULL,message TEXT NOT NULL,notes TEXT NOT NULL,received_at INTEGER NOT NULL,happened_at INTEGER NOT NULL DEFAULT 0,` + alertRepeatCountColumn + `,` + alertLastRepeatTitleColumn + `,UNIQUE(source_id,replay_key));
CREATE INDEX alerts_incident ON alerts(incident_id,sequence);
CREATE INDEX alerts_received ON alerts(received_at,sequence);
CREATE TABLE incident_settings(id INTEGER PRIMARY KEY CHECK(id=1),response_seconds INTEGER NOT NULL);
INSERT INTO incident_settings VALUES(1,300);
CREATE TABLE incidents(id TEXT PRIMARY KEY,opening_alert_id TEXT NOT NULL REFERENCES alerts(id),source_id TEXT NOT NULL,sender_incident_key TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('open','claimed','resolved')),owner_member_id TEXT NOT NULL,step TEXT NOT NULL CHECK(step IN ('triage_agent','primary','chosen_person','backup','only_member','everyone_available')),revision INTEGER NOT NULL,targets TEXT NOT NULL,ignore_personal INTEGER NOT NULL,created_at INTEGER NOT NULL,claimed_at INTEGER NOT NULL,resolved_at INTEGER NOT NULL,deadline INTEGER NOT NULL,repeat_at INTEGER NOT NULL,repeat_generation INTEGER NOT NULL,team_mute_revision INTEGER NOT NULL,primary_at INTEGER NOT NULL,backup_at INTEGER NOT NULL,everyone_available_at INTEGER NOT NULL);
CREATE UNIQUE INDEX incidents_active_key ON incidents(source_id,sender_incident_key) WHERE status!='resolved' AND sender_incident_key!='';
CREATE INDEX incidents_pending ON incidents(status,deadline,repeat_at);
CREATE TABLE incident_actions(id INTEGER PRIMARY KEY AUTOINCREMENT,incident_id TEXT NOT NULL REFERENCES incidents(id),revision INTEGER NOT NULL,actor TEXT NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL,actor_name TEXT NOT NULL,target_member_ids TEXT NOT NULL,previous_owner_member_id TEXT NOT NULL,related_alert_id TEXT NOT NULL,occurred_at INTEGER NOT NULL,note TEXT NOT NULL DEFAULT '',note_given_in_triage_run_id TEXT NOT NULL DEFAULT '');
CREATE INDEX incident_actions_incident ON incident_actions(incident_id,id);
`

// incidentSteps are the escalation steps an incident can be at. The
// incidents table's CHECK lists the same steps, so a step that every switch
// over steps does not know is refused when it is saved, not when the
// background work reads it. TestEveryEscalationStepIsKnownWhereverStepsAreUsed
// walks each step through those switches.
var incidentSteps = []string{triageAgentStep, "primary", "chosen_person", "backup", "only_member", "everyone_available"}

type Incident struct {
	ID, OpeningAlertID, SourceID, SenderIncidentKey, Status, OwnerMemberID, Step, Title, Message   string
	Revision                                                                                       int64
	TargetMemberIDs                                                                                []string
	IgnorePersonal                                                                                 bool
	CreatedAt, ClaimedAt, ResolvedAt, Deadline, RepeatAt, PrimaryAt, BackupAt, EveryoneAvailableAt time.Time
	RepeatGeneration, TeamMuteRevision                                                             int64
}
type IncidentAction struct {
	ID, Revision                                                                       int64
	IncidentID, Actor, Kind, Message, ActorName, PreviousOwnerMemberID, RelatedAlertID string
	// Note is what the person wrote with a claim or a resolve on the
	// incident page, "" for none. MMHotline gives it once to the triage
	// agent; note_given_in_triage_run_id names the run whose prompt carried
	// it (incident_notes_for_the_triage_agent.go).
	Note            string
	TargetMemberIDs []string
	At              time.Time
}

// NoteText is the note as the history shows it after the entry's own text:
// Note from Ana Silva: “This was a test.”
func (action IncidentAction) NoteText() string {
	return incidentNoteText(action.ActorName, action.Note)
}

func incidentNoteText(by, note string) string {
	if note == "" {
		return ""
	}
	return "Note from " + by + ": “" + note + "”"
}

// maximumIncidentNoteCharacters bounds a note on a claim or a resolve, as a
// refusal's reason is bounded: a short line for the team and the triage
// agent, not a report.
const maximumIncidentNoteCharacters = maximumRefusalReasonCharacters

// checkIncidentNote returns the note without surrounding spaces, or why it
// is refused: a note is one line of plain text, so it reads the same in the
// history, in a terminal, and in the triage agent's prompt.
func checkIncidentNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if !utf8.ValidString(note) || strings.ContainsFunc(note, unicode.IsControl) {
		return "", fmt.Errorf("%w: a note is one line of plain text, without a line break, a tab, or another control character", ErrInvalid)
	}
	if length := utf8.RuneCountInString(note); length > maximumIncidentNoteCharacters {
		return "", fmt.Errorf("%w: an incident note must be at most %d characters; the submitted note has %d", ErrInvalid, maximumIncidentNoteCharacters, length)
	}
	return note, nil
}

const incidentColumns = `i.id,i.opening_alert_id,i.source_id,i.sender_incident_key,i.status,i.owner_member_id,i.step,i.revision,i.targets,i.ignore_personal,i.created_at,i.claimed_at,i.resolved_at,i.deadline,i.repeat_at,i.repeat_generation,i.team_mute_revision,i.primary_at,i.backup_at,i.everyone_available_at,e.title,e.message`

func scanIncident(row scanner) (Incident, error) {
	var i Incident
	var targets string
	var created, claimed, resolved, deadline, repeat, primary, backup, team int64
	err := row.Scan(&i.ID, &i.OpeningAlertID, &i.SourceID, &i.SenderIncidentKey, &i.Status, &i.OwnerMemberID, &i.Step, &i.Revision, &targets, &i.IgnorePersonal, &created, &claimed, &resolved, &deadline, &repeat, &i.RepeatGeneration, &i.TeamMuteRevision, &primary, &backup, &team, &i.Title, &i.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	if err = json.Unmarshal([]byte(targets), &i.TargetMemberIDs); err != nil {
		return i, err
	}
	i.CreatedAt = deadlineTime(created)
	i.ClaimedAt = deadlineTime(claimed)
	i.ResolvedAt = deadlineTime(resolved)
	i.Deadline = deadlineTime(deadline)
	i.RepeatAt = deadlineTime(repeat)
	i.PrimaryAt = deadlineTime(primary)
	i.BackupAt = deadlineTime(backup)
	i.EveryoneAvailableAt = deadlineTime(team)
	return i, nil
}
func readIncidentBy(ctx context.Context, tx *sql.Tx, where string, args ...any) (Incident, error) {
	return scanIncident(tx.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE `+where, args...))
}
func readIncident(ctx context.Context, tx *sql.Tx, id string) (Incident, error) {
	return readIncidentBy(ctx, tx, "i.id=?", id)
}
func saveIncident(ctx context.Context, tx *sql.Tx, i Incident) error {
	targets, err := json.Marshal(i.TargetMemberIDs)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE incidents SET status=?,owner_member_id=?,step=?,revision=?,targets=?,ignore_personal=?,claimed_at=?,resolved_at=?,deadline=?,repeat_at=?,repeat_generation=?,team_mute_revision=?,primary_at=?,backup_at=?,everyone_available_at=? WHERE id=?`, i.Status, i.OwnerMemberID, i.Step, i.Revision, string(targets), i.IgnorePersonal, deadlineUnix(i.ClaimedAt), deadlineUnix(i.ResolvedAt), deadlineUnix(i.Deadline), deadlineUnix(i.RepeatAt), i.RepeatGeneration, i.TeamMuteRevision, deadlineUnix(i.PrimaryAt), deadlineUnix(i.BackupAt), deadlineUnix(i.EveryoneAvailableAt), i.ID)
	return err
}
func recordIncidentAction(ctx context.Context, tx *sql.Tx, i Incident, actor, kind, message, relatedAlertID, previousOwnerMemberID string, at time.Time) error {
	return recordIncidentActionWithNote(ctx, tx, i, actor, kind, message, relatedAlertID, previousOwnerMemberID, "", at)
}

// recordIncidentActionWithNote records an action with the note its person
// wrote, "" for none; only a claim or a resolve on the incident page has one.
func recordIncidentActionWithNote(ctx context.Context, tx *sql.Tx, i Incident, actor, kind, message, relatedAlertID, previousOwnerMemberID, note string, at time.Time) error {
	actorName, err := incidentActorName(ctx, tx, actor)
	if err != nil {
		return err
	}
	targets, err := json.Marshal(i.TargetMemberIDs)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO incident_actions(incident_id,revision,actor,kind,message,actor_name,target_member_ids,previous_owner_member_id,related_alert_id,occurred_at,note) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, i.ID, i.Revision, actor, kind, message, actorName, string(targets), previousOwnerMemberID, relatedAlertID, at.Unix(), note)
	return err
}

// escalateIfUnclaimedAfter is how long each escalation step waits for a claim. A step
// copies it into the incident's deadline when the step starts, so a changed
// window reaches only the steps that start after the change.
func escalateIfUnclaimedAfter(ctx context.Context, tx *sql.Tx) (time.Duration, error) {
	var seconds int64
	err := tx.QueryRowContext(ctx, `SELECT response_seconds FROM incident_settings WHERE id=1`).Scan(&seconds)
	return time.Duration(seconds) * time.Second, err
}

// EscalateIfUnclaimedAfter returns the team's time to claim before MMHotline escalates, for any member.
func (a *App) EscalateIfUnclaimedAfter(ctx context.Context, p Principal) (time.Duration, error) {
	var window time.Duration
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		window, err = escalateIfUnclaimedAfter(ctx, tx)
		return err
	})
	return window, err
}

// SetEscalateIfUnclaimedAfter sets the time to claim to whole minutes, for an
// administrator, or root through the command; the pages show no control for
// it. Steps already under way keep their deadlines.
func (a *App) SetEscalateIfUnclaimedAfter(ctx context.Context, p Principal, minutes int) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		if minutes < 1 || minutes > 60 {
			return fmt.Errorf("%w: the time to claim before MMHotline escalates must be a whole number of minutes from 1 to 60", ErrInvalid)
		}
		_, err := tx.ExecContext(ctx, `UPDATE incident_settings SET response_seconds=? WHERE id=1`, minutes*60)
		return err
	})
}

// createIncident opens the incident of an SOS. A non-empty chosen list names
// the only people to contact: they are contacted now, whether or not they are
// on call, and the SOS has no time to claim, backup, or escalation to anyone
// else (the test SOS for one person is the one caller that names people).
func createIncident(ctx context.Context, tx *sql.Tx, e Alert, actor string, at time.Time, chosen []string) (Incident, error) {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM incidents WHERE status!='resolved'`).Scan(&active); err != nil {
		return Incident{}, err
	}
	// MMHotline's own SOS about refused SOS alerts is exempt: at most one per
	// alert source is open, and it must open when the team is at the limit.
	if active >= maxUnresolvedIncidents && e.SourceID != "system" {
		return Incident{}, errTooManyIncidents
	}
	i := Incident{ID: e.IncidentID, OpeningAlertID: e.ID, SourceID: e.SourceID, SenderIncidentKey: e.SenderIncidentKey, Status: "open", Revision: 1, CreatedAt: at, Title: e.Title, Message: e.Message}
	c, err := CoverageAt(ctx, tx, at)
	if err != nil {
		return i, err
	}
	window, err := escalateIfUnclaimedAfter(ctx, tx)
	if err != nil {
		return i, err
	}
	i.TeamMuteRevision = c.TeamMute.Revision
	waits, err := sosWaitsForTriageAgent(ctx, tx, e.ID)
	if err != nil {
		return i, err
	}
	wait, err := sosWaitsForTheAgent(ctx, tx)
	if err != nil {
		return i, err
	}
	// A wait of 0 wakes the person on call at once; the agent still looks.
	waits = waits && wait > 0
	// alreadyWokeFor is the incident of the triage session that already woke
	// the people this SOS would wake first (triageSessionAlreadyWoke).
	var alreadyWokeFor string
	if waits {
		next := i
		chooseInitialTarget(&next, c, window, at)
		if alreadyWokeFor, err = triageSessionAlreadyWoke(ctx, tx, next.TargetMemberIDs); err != nil {
			return i, err
		}
	}
	switch {
	case len(chosen) > 0:
		i.Step, i.TargetMemberIDs = "chosen_person", append([]string{}, chosen...)
		setStepTimes(&i, window, at)
		i.Deadline = time.Time{}
	case waits && alreadyWokeFor == "":
		// The first waking step waits for the triage agent; ReconcileIncidents
		// starts it at the deadline, or the triage session sooner.
		i.Step, i.TargetMemberIDs = triageAgentStep, []string{}
		i.Deadline = at.Add(wait)
		i.RepeatAt = i.Deadline
	case alreadyWokeFor != "":
		// The first waking step starts now, without waking: its people were
		// woken for the session. The time to claim runs as usual, and an
		// unclaimed SOS then goes on as any other, to the backup or a repeat,
		// so it is never left unseen.
		chooseInitialTarget(&i, c, window, at)
		i.RepeatAt = at.Add(window)
	default:
		chooseInitialTarget(&i, c, window, at)
	}
	targets, _ := json.Marshal(i.TargetMemberIDs)
	_, err = tx.ExecContext(ctx, `INSERT INTO incidents(id,opening_alert_id,source_id,sender_incident_key,status,owner_member_id,step,revision,targets,ignore_personal,created_at,claimed_at,resolved_at,deadline,repeat_at,repeat_generation,team_mute_revision,primary_at,backup_at,everyone_available_at) VALUES(?,?,?,?,'open','',?,?,?,?,?,0,0,?,?,0,?,?,?,?)`, i.ID, i.OpeningAlertID, i.SourceID, i.SenderIncidentKey, i.Step, i.Revision, string(targets), i.IgnorePersonal, at.Unix(), deadlineUnix(i.Deadline), deadlineUnix(i.RepeatAt), i.TeamMuteRevision, deadlineUnix(i.PrimaryAt), deadlineUnix(i.BackupAt), deadlineUnix(i.EveryoneAvailableAt))
	if err != nil {
		return i, err
	}
	if err = recordIncidentAction(ctx, tx, i, actor, "opened", "SOS incident opened: "+i.Title, e.ID, "", at); err != nil {
		return i, err
	}
	if alreadyWokeFor != "" {
		names, err := incidentTargetNames(ctx, tx, i)
		if err != nil {
			return i, err
		}
		return i, recordIncidentAction(ctx, tx, i, "system", "already_woken_for_triage_session", "MMHotline already woke "+names+" for this triage session (“"+alreadyWokeFor+"”), so it does not wake them again now. Unless someone claims it by "+i.RepeatAt.UTC().Format("15:04")+" UTC, MMHotline goes on with its escalation then.", "", "", at)
	}
	if waits {
		if err = recordIncidentAction(ctx, tx, i, "system", "held_for_triage_agent", "Held for the triage agent until "+i.Deadline.UTC().Format("15:04")+" UTC", "", "", at); err != nil {
			return i, err
		}
		// An SOS that joins while the session's server replacement goes on
		// waits for the same limit.
		return i, serverReplacementHoldsANewSOS(ctx, tx, i, at)
	}
	err = queueSOSEscalation(ctx, tx, &i, at)
	return i, err
}
func chooseInitialTarget(i *Incident, c Coverage, window time.Duration, at time.Time) {
	i.IgnorePersonal = false
	switch {
	case len(c.EligibleMemberIDs) == 0:
		i.Step = "everyone_available"
		i.TargetMemberIDs = append([]string{}, c.AllMemberIDs...)
		i.IgnorePersonal = true
	case len(c.AllMemberIDs) == 1:
		i.Step = "only_member"
		i.TargetMemberIDs = append([]string{}, c.EligibleMemberIDs...)
	case c.PrimaryIsEveryoneAvailable:
		i.Step = "everyone_available"
		i.TargetMemberIDs = append([]string{}, c.EligibleMemberIDs...)
	default:
		i.Step = "primary"
		i.TargetMemberIDs = append([]string{}, c.PrimaryIDs...)
	}
	setStepTimes(i, window, at)
}
func setStepTimes(i *Incident, window time.Duration, at time.Time) {
	i.RepeatAt = at
	i.Deadline = time.Time{}
	switch i.Step {
	case "primary", "only_member", "chosen_person":
		i.PrimaryAt = at
	case "backup":
		i.BackupAt = at
	case "everyone_available":
		i.EveryoneAvailableAt = at
	}
	if i.Step != "everyone_available" && i.Step != "only_member" {
		i.Deadline = at.Add(window)
	}
}

// stepStartedAt is when the incident's current escalation step started.
func (i Incident) stepStartedAt() time.Time {
	switch i.Step {
	case "backup":
		return i.BackupAt
	case "everyone_available":
		return i.EveryoneAvailableAt
	}
	return i.PrimaryAt
}
func chooseNextTarget(i *Incident, c Coverage, window time.Duration, at time.Time) {
	if len(c.EligibleMemberIDs) == 0 || len(c.AllMemberIDs) == 1 {
		chooseInitialTarget(i, c, window, at)
		return
	}
	if (i.Step == "primary" || i.Step == "chosen_person") && c.AssignedBackupID != "" && containsMember(c.EligibleMemberIDs, c.AssignedBackupID) && !containsMember(i.TargetMemberIDs, c.AssignedBackupID) {
		i.Step = "backup"
		i.TargetMemberIDs = []string{c.AssignedBackupID}
		i.IgnorePersonal = false
	} else {
		i.Step = "everyone_available"
		i.TargetMemberIDs = append([]string{}, c.EligibleMemberIDs...)
		i.IgnorePersonal = false
	}
	setStepTimes(i, window, at)
}
func sameTargets(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
func queueSOSEscalation(ctx context.Context, tx *sql.Tx, i *Incident, at time.Time) error {
	i.RepeatGeneration++
	i.RepeatAt = at.Add(time.Minute)
	if err := saveIncident(ctx, tx, *i); err != nil {
		return err
	}
	var previousRounds int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND revision=? AND kind='sos_escalation_due'`, i.ID, i.Revision).Scan(&previousRounds); err != nil {
		return err
	}
	if len(i.TargetMemberIDs) == 0 {
		// A team with no members yet still keeps every SOS: a refused one
		// would be dropped by its sender. The incident shows on the
		// dashboard, and the visit after someone is added contacts them,
		// because the everyone-available step follows the team.
		if previousRounds > 0 {
			return nil
		}
		return recordIncidentAction(ctx, tx, *i, "system", "sos_escalation_due", "Nobody to contact yet: the team has no members. MMHotline contacts people as soon as someone is added.", "", "", at)
	}
	// woken is the incident as this round wakes it: its history names only
	// the people the round can wake.
	woken := *i
	wakeable, unwakeable, err := wakeableMembers(ctx, tx, i.TargetMemberIDs)
	woken.TargetMemberIDs = wakeable
	if err != nil {
		return err
	}
	if err = recordCouldNotWake(ctx, tx, *i, unwakeable, at); err != nil {
		return err
	}
	if len(woken.TargetMemberIDs) == 0 {
		// Nobody the step targets can be woken, so waiting out the time
		// to claim wakes nobody: the next step starts now. Everyone available
		// and the only member have no next step; each later round tries
		// them again, and wakes whoever has added a waking contact method.
		if i.Deadline.IsZero() {
			return nil
		}
		c, err := CoverageAt(ctx, tx, at)
		if err != nil {
			return err
		}
		window, err := escalateIfUnclaimedAfter(ctx, tx)
		if err != nil {
			return err
		}
		i.Revision++
		chooseNextTarget(i, c, window, at)
		if err = CancelIncidentSOSEscalation(ctx, tx, i.ID, "sos_escalation step changed"); err != nil {
			return err
		}
		return queueSOSEscalation(ctx, tx, i, at)
	}
	if err := QueueNotification(ctx, tx, Notification{Key: fmt.Sprintf("sos_escalation:%s:%d:%d", i.ID, i.Revision, i.RepeatGeneration), AlertID: i.OpeningAlertID, IncidentID: i.ID, IncidentRevision: i.Revision, Severity: "sos", Title: i.Title, Message: i.Message, Purpose: "sos_escalation", Recipients: woken.TargetMemberIDs, IgnorePersonal: i.IgnorePersonal, At: at}); err != nil {
		return err
	}
	message, err := incidentContactingText(ctx, tx, woken, previousRounds > 0)
	if err != nil {
		return err
	}
	if i.IgnorePersonal {
		message += ". Nobody is available; contacting every member while respecting team mute."
	}
	return recordIncidentAction(ctx, tx, woken, "system", "sos_escalation_due", message, "", "", at)
}

// wakeableMembers splits the members of ids into those who have a contact
// method that wakes people and receives SOS alerts, the contact methods
// QueueNotification sends an SOS escalation to, and those who have none.
func wakeableMembers(ctx context.Context, tx *sql.Tx, ids []string) (wakeable, unwakeable []string, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.member_id,c.contact_method_type FROM contact_methods c JOIN contact_method_subscriptions s ON s.contact_method_id=c.id WHERE c.deleted=0 AND s.severity='sos'`)
	if err != nil {
		return nil, nil, err
	}
	canWake := map[string]bool{}
	for rows.Next() {
		var memberID, contactMethodType string
		if err = rows.Scan(&memberID, &contactMethodType); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if facts, known := contactMethodTypeFacts(contactMethodType); known && facts.WakesPeople {
			canWake[memberID] = true
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	wakeable = []string{}
	for _, id := range ids {
		if canWake[id] {
			wakeable = append(wakeable, id)
		} else {
			unwakeable = append(unwakeable, id)
		}
	}
	return wakeable, unwakeable, nil
}

// recordCouldNotWake records, once per escalation step, that MMHotline could
// not wake each of ids, and tells the administrators once per incident, so
// a person missing a waking contact method is found before the next SOS.
func recordCouldNotWake(ctx context.Context, tx *sql.Tx, i Incident, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	var told int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='could_not_wake'`, i.ID).Scan(&told); err != nil {
		return err
	}
	var names []string
	for _, id := range ids {
		name, err := incidentMemberName(ctx, tx, id)
		if err != nil {
			return err
		}
		names = append(names, name)
		message := "Could not wake " + name + ": " + name + " has no contact method that wakes people for SOS alerts"
		var recorded int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND revision=? AND kind='could_not_wake' AND message=?`, i.ID, i.Revision, message).Scan(&recorded); err != nil {
			return err
		}
		if recorded > 0 {
			continue
		}
		person := i
		person.TargetMemberIDs = []string{id}
		if err = recordIncidentAction(ctx, tx, person, "system", "could_not_wake", message, "", "", at); err != nil {
			return err
		}
	}
	if told > 0 {
		return nil
	}
	// The notice never stops the SOS: when the ordinary part of the delivery
	// queue is full, the notice is left out, and the history line remains.
	who := strings.Join(names, ", ")
	bestEffort(ctx, tx, "could_not_wake_notice", func() error {
		return RecordAdministratorNotice(ctx, tx, "warn", truncateRunes("MMHotline could not wake "+who+" for an SOS incident: "+i.Title, 256), fmt.Sprintf("MMHotline could not wake %s for the SOS incident “%s”. Nothing set up can wake them: a person needs an automated voice call or a Pushover notification that receives SOS alerts. Until they add one on their Contact methods page, MMHotline skips them when it wakes people for an SOS.", who, i.Title), at)
	})
	return nil
}

// resolveIncidentTx resolves an open or claimed incident. how is empty or
// says where the action came from, such as " in Telegram"; note is the
// person's note from the incident page, or "".
func resolveIncidentTx(ctx context.Context, tx *sql.Tx, i Incident, actor, resolvingAlertID, how, note string, at time.Time) (Incident, error) {
	if i.Status == "resolved" {
		return i, errIncidentChanged
	}
	i.Status = "resolved"
	i.Revision++
	i.ResolvedAt = at
	i.Deadline = time.Time{}
	i.RepeatAt = time.Time{}
	if err := saveIncident(ctx, tx, i); err != nil {
		return i, err
	}
	if err := endServerReplacementOfResolvedIncident(ctx, tx, i.ID, at); err != nil {
		return i, err
	}
	if err := CancelIncidentSOSEscalation(ctx, tx, i.ID, "incident resolved"); err != nil {
		return i, err
	}
	actorName, err := incidentActorName(ctx, tx, actor)
	if err != nil {
		return i, err
	}
	if err := recordIncidentActionWithNote(ctx, tx, i, actor, "resolved", "Resolved by "+actorName+how, resolvingAlertID, "", note, at); err != nil {
		return i, err
	}
	alertID := resolvingAlertID
	if alertID == "" {
		alertID = i.OpeningAlertID
	}
	// Nobody needs to hear that a test is over.
	if i.SourceID == testAlertSourceID {
		return i, nil
	}
	title, message, err := resolvedSOSText(ctx, tx, i)
	if err != nil {
		return i, err
	}
	err = QueueNotification(ctx, tx, Notification{Key: fmt.Sprintf("resolved:%s:%d", i.ID, i.Revision), AlertID: alertID, IncidentID: i.ID, Severity: "resolved-sos", Title: title, Message: message, Purpose: "subscribed_alert", At: at})
	return i, err
}

// ClaimIncident claims an incident for the member, with an optional note,
// "" for none, which the incident's history keeps and the triage agent gets
// once (incident_notes_for_the_triage_agent.go).
func (a *App) ClaimIncident(ctx context.Context, p Principal, id string, expectedRevision int64, note string) (Incident, error) {
	var i Incident
	note, err := checkIncidentNote(note)
	if err != nil {
		return i, err
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		i, err = readIncident(ctx, tx, id)
		if err != nil {
			return err
		}
		if i.Revision != expectedRevision || i.Status == "resolved" {
			return errIncidentChanged
		}
		// Claiming your own claim changes nothing.
		if i.Status == "claimed" && i.OwnerMemberID == m.ID {
			return nil
		}
		i, err = claimIncidentTx(ctx, tx, i, m, "", note, a.Now().UTC())
		return err
	})
	return i, err
}

// claimIncidentTx claims an open or claimed incident for m and stops its SOS
// repeats. how is empty or says where the claim came from, such as
// " in Telegram"; note is the person's note from the incident page, or "".
func claimIncidentTx(ctx context.Context, tx *sql.Tx, i Incident, m Member, how, note string, at time.Time) (Incident, error) {
	oldOwner := i.OwnerMemberID
	i.Status = "claimed"
	i.OwnerMemberID = m.ID
	i.Revision++
	i.ClaimedAt = at
	i.Deadline = time.Time{}
	i.RepeatAt = time.Time{}
	if err := saveIncident(ctx, tx, i); err != nil {
		return i, err
	}
	if err := CancelIncidentSOSEscalation(ctx, tx, i.ID, "incident claimed"); err != nil {
		return i, err
	}
	kind := "claimed"
	message := "Claimed by " + m.Name + how
	if oldOwner != "" {
		kind = "reclaimed"
		oldName, err := incidentMemberName(ctx, tx, oldOwner)
		if err != nil {
			return i, err
		}
		message = "Taken over by " + m.Name + " from " + oldName + how
	}
	if err := recordIncidentActionWithNote(ctx, tx, i, "member:"+m.ID, kind, message, "", oldOwner, note, i.ClaimedAt); err != nil {
		return i, err
	}
	// A test SOS has done its job once someone claims it: it proved that the
	// phone woke a person. MMHotline closes it at once, so the person has
	// nothing more to do.
	if i.SourceID == testAlertSourceID {
		return resolveIncidentTx(ctx, tx, i, "system", "", " after the claim, because this incident was a test", "", at)
	}
	return i, nil
}

// ResolveIncident resolves an incident, with an optional note, as
// ClaimIncident takes one.
func (a *App) ResolveIncident(ctx context.Context, p Principal, id string, expectedRevision int64, note string) (Incident, error) {
	var i Incident
	note, err := checkIncidentNote(note)
	if err != nil {
		return i, err
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		i, err = readIncident(ctx, tx, id)
		if err != nil {
			return err
		}
		if i.Revision != expectedRevision || i.Status == "resolved" {
			return errIncidentChanged
		}
		i, err = resolveIncidentTx(ctx, tx, i, "member:"+m.ID, "", "", note, a.Now().UTC())
		return err
	})
	return i, err
}
func (a *App) EscalateIncident(ctx context.Context, p Principal, id, targetMemberID string, expectedRevision int64) (Incident, error) {
	var i Incident
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		i, err = readIncident(ctx, tx, id)
		if err != nil {
			return err
		}
		if i.Revision != expectedRevision || i.Status == "resolved" {
			return errIncidentChanged
		}
		eligible, reason, err := MemberEligible(ctx, tx, targetMemberID, now)
		if err != nil {
			return err
		}
		if !eligible {
			return fmt.Errorf("%w: the selected person cannot receive the escalation: %s", ErrInvalid, reason)
		}
		window, err := escalateIfUnclaimedAfter(ctx, tx)
		if err != nil {
			return err
		}
		i.Status = "open"
		i.OwnerMemberID = ""
		i.ClaimedAt = time.Time{}
		i.Revision++
		i.Step = "chosen_person"
		i.TargetMemberIDs = []string{targetMemberID}
		i.IgnorePersonal = false
		setStepTimes(&i, window, now)
		if err = CancelIncidentSOSEscalation(ctx, tx, i.ID, "incident escalated"); err != nil {
			return err
		}
		if err = saveIncident(ctx, tx, i); err != nil {
			return err
		}
		targetName, err := incidentMemberName(ctx, tx, targetMemberID)
		if err != nil {
			return err
		}
		if err = recordIncidentAction(ctx, tx, i, "member:"+m.ID, "escalated", "Escalated to "+targetName+" by "+m.Name, "", "", now); err != nil {
			return err
		}
		if err = RecordSystemAlert(ctx, tx, "member:"+m.ID, i.ID, "warn", truncateRunes(m.Name+" asked MMHotline to contact "+targetName+" about an SOS incident: "+i.Title, 256), fmt.Sprintf("%s escalated the SOS incident “%s” to %s, so MMHotline contacts %s now.", m.Name, i.Title, targetName, targetName), now); err != nil {
			return err
		}
		return queueSOSEscalation(ctx, tx, &i, now)
	})
	return i, err
}

// ValidateSOSEscalation checks facts in the same team transaction as reservation.
// Delivery still rechecks team mute, personal unavailability, and immutable contact method identity.
func ValidateSOSEscalation(ctx context.Context, tx *sql.Tx, id string, revision int64, memberID string, at time.Time) (bool, string, error) {
	i, err := readIncident(ctx, tx, id)
	if errors.Is(err, ErrNotFound) {
		return false, "incident missing", nil
	}
	if err != nil {
		return false, "", err
	}
	if i.Status != "open" || i.Revision != revision || !containsMember(i.TargetMemberIDs, memberID) {
		return false, "incident or target changed", nil
	}
	if !i.Deadline.IsZero() && !i.Deadline.After(at) {
		return false, "time to claim ran out", nil
	}
	mute, err := ReadTeamMute(ctx, tx, at)
	if err != nil {
		return false, "", err
	}
	if mute.Active {
		return false, "team mute", nil
	}
	eligible, reason, err := MemberEligible(ctx, tx, memberID, at)
	if err != nil {
		return false, "", err
	}
	if reason == "member removed" {
		return false, reason, nil
	}
	if i.IgnorePersonal {
		c, err := CoverageAt(ctx, tx, at)
		if err != nil {
			return false, "", err
		}
		if len(c.EligibleMemberIDs) != 0 {
			return false, "Someone is available again.", nil
		}
		return true, "", nil
	}
	return eligible, reason, nil
}

// nextIncidentDeadline returns when ReconcileIncidents next has something to
// do for an incident that nobody has claimed or resolved: its response
// deadline, at which the next person is contacted, or its next repeat. What
// else changes such an incident is a change to the team's database, or the
// end of an unavailability or of the team mute, which nextCoverageDeadline
// returns.
func nextIncidentDeadline(ctx context.Context, tx *sql.Tx, now time.Time) (time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT deadline,repeat_at FROM incidents WHERE status='open'`)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	var next time.Time
	for rows.Next() {
		var deadline, repeat int64
		if err = rows.Scan(&deadline, &repeat); err != nil {
			return time.Time{}, err
		}
		if repeat == 0 {
			// Not contacted yet: ReconcileIncidents does that at once.
			return now, nil
		}
		next = earliest(next, deadlineTime(deadline), deadlineTime(repeat))
	}
	return next, rows.Err()
}

// ReconcileIncidents moves each open incident to its next escalation step
// when its step is overdue or its people changed, and wakes again those it
// is due to wake again. Each incident is handled in its own savepoint: an
// incident that fails is undone and reported, and the others still escalate.
func (a *App) ReconcileIncidents(ctx context.Context) error {
	now := a.Now().UTC()
	var failures []error
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		failures = nil
		if err := ObserveCoverageHandoff(ctx, tx, now); err != nil {
			return err
		}
		if _, err := ExpireCoverage(ctx, tx, now); err != nil {
			return err
		}
		c, err := CoverageAt(ctx, tx, now)
		if err != nil {
			return err
		}
		window, err := escalateIfUnclaimedAfter(ctx, tx)
		if err != nil {
			return err
		}
		// Every open incident, with no limit: intake limits how many are
		// open, and MMHotline's own SOS may go past that limit.
		rows, err := tx.QueryContext(ctx, `SELECT `+incidentColumns+` FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE i.status='open' ORDER BY i.created_at,i.id`)
		if err != nil {
			return err
		}
		var incidents []Incident
		for rows.Next() {
			i, err := scanIncident(rows)
			if err != nil {
				rows.Close()
				return err
			}
			incidents = append(incidents, i)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, i := range incidents {
			if _, err = tx.ExecContext(ctx, `SAVEPOINT reconcile_incident`); err != nil {
				return err
			}
			if failure := reconcileIncident(ctx, tx, i, c, window, now); failure != nil {
				if ctx.Err() != nil {
					return failure
				}
				if _, err = tx.ExecContext(ctx, `ROLLBACK TO reconcile_incident`); err != nil {
					return errors.Join(failure, err)
				}
				failures = append(failures, fmt.Errorf("incident %s: %w", i.ID, failure))
			}
			if _, err = tx.ExecContext(ctx, `RELEASE reconcile_incident`); err != nil {
				return err
			}
		}
		return nil
	})
	return errors.Join(err, errors.Join(failures...))
}

// reconcileIncident does ReconcileIncidents' work for one open incident.
func reconcileIncident(ctx context.Context, tx *sql.Tx, i Incident, c Coverage, window time.Duration, now time.Time) error {
	if i.Step == triageAgentStep {
		// Nobody is contacted until the deadline, whoever is on call.
		if i.Deadline.After(now) {
			return nil
		}
		why := "The triage agent's time ran out; waking the person on call"
		replacement, err := wasHeldForAServerReplacement(ctx, tx, i.ID)
		if err != nil {
			return err
		}
		if replacement {
			if why, err = serverReplacementLimitText(ctx, tx); err != nil {
				return err
			}
			why += "; waking the person on call"
		}
		return wakeIncidentHeldForTriageAgent(ctx, tx, i, why, now)
	}
	teamMuteChanged := i.TeamMuteRevision != c.TeamMute.Revision
	force := teamMuteChanged && !c.TeamMute.Active
	i.TeamMuteRevision = c.TeamMute.Revision
	eligibleTargets := 0
	for _, id := range i.TargetMemberIDs {
		ok, _, err := MemberEligible(ctx, tx, id, now)
		if err != nil {
			return err
		}
		if ok {
			eligibleTargets++
		}
	}
	noCoverageChanged := i.IgnorePersonal && len(c.EligibleMemberIDs) > 0 || !i.IgnorePersonal && eligibleTargets == 0
	// A test SOS for one person never goes on to anyone else, even when that
	// person becomes unavailable: the test then wakes nobody and times out.
	if i.SourceID == testAlertSourceID && i.Step == "chosen_person" {
		noCoverageChanged = false
	}
	overdue := !i.Deadline.IsZero() && !i.Deadline.After(now)
	transition := noCoverageChanged || overdue
	if transition {
		i.Revision++
		if noCoverageChanged && i.IgnorePersonal {
			chooseInitialTarget(&i, c, window, now)
		} else {
			chooseNextTarget(&i, c, window, now)
		}
		if err := CancelIncidentSOSEscalation(ctx, tx, i.ID, "sos_escalation step changed"); err != nil {
			return err
		}
		targetMessage, err := incidentContactingText(ctx, tx, i, false)
		if err != nil {
			return err
		}
		if err := recordIncidentAction(ctx, tx, i, "system", "escalated", targetMessage, "", "", now); err != nil {
			return err
		}
		targetName := "everyone available"
		if i.Step != "everyone_available" {
			targetName, err = incidentTargetNames(ctx, tx, i)
			if err != nil {
				return err
			}
		}
		if err := RecordSystemAlert(ctx, tx, "system", i.ID, "warn", truncateRunes("Nobody claimed an SOS incident in time, so MMHotline contacts "+targetName+": "+i.Title, 256), fmt.Sprintf("Nobody claimed the SOS incident “%s” in time, so MMHotline escalated it to %s.", i.Title, targetName), now); err != nil {
			return err
		}
		force = true
	} else if i.Step == "everyone_available" {
		desired := c.EligibleMemberIDs
		ignore := false
		if len(desired) == 0 {
			desired = c.AllMemberIDs
			ignore = true
		}
		if !sameTargets(i.TargetMemberIDs, desired) || i.IgnorePersonal != ignore {
			i.TargetMemberIDs = append([]string{}, desired...)
			i.IgnorePersonal = ignore
			i.Revision++
			if err := CancelIncidentSOSEscalation(ctx, tx, i.ID, "team eligibility changed"); err != nil {
				return err
			}
			force = true
		}
	}
	if force || i.RepeatAt.IsZero() || !i.RepeatAt.After(now) {
		if err := queueSOSEscalation(ctx, tx, &i, now); err != nil {
			return err
		}
	} else if teamMuteChanged {
		// Nothing else about the incident changed in this visit.
		if err := saveIncident(ctx, tx, i); err != nil {
			return err
		}
	}
	return nil
}
func (a *App) GetIncident(ctx context.Context, p Principal, id string) (Incident, error) {
	var i Incident
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		i, err = readIncident(ctx, tx, id)
		return err
	})
	return i, err
}

// ListIncidents lists every incident that is not resolved, newest first, as
// the dashboard shows them.
func (a *App) ListIncidents(ctx context.Context, p Principal) ([]Incident, error) {
	return a.listIncidents(ctx, p, 0, `i.status!='resolved'`)
}

// ListIncidentsOpened lists the incidents opened at or after since and before
// before, resolved or not, newest first; a zero time leaves that end open.
// It lists at most maxUnresolvedIncidents: a team that opened more in one
// span narrows the span.
func (a *App) ListIncidentsOpened(ctx context.Context, p Principal, since, before time.Time) ([]Incident, error) {
	where, args := []string{"1=1"}, []any{}
	if !since.IsZero() {
		where, args = append(where, "i.created_at>=?"), append(args, since.Unix())
	}
	if !before.IsZero() {
		where, args = append(where, "i.created_at<?"), append(args, before.Unix())
	}
	return a.listIncidents(ctx, p, maxUnresolvedIncidents, strings.Join(where, " AND "), args...)
}

// listIncidents lists the incidents that condition picks, newest first, at
// most limit of them; a limit of 0 lists them all.
func (a *App) listIncidents(ctx context.Context, p Principal, limit int, condition string, args ...any) ([]Incident, error) {
	var result []Incident
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		// Within one second, the opening alerts' order says which is newer.
		query := `SELECT ` + incidentColumns + ` FROM incidents i JOIN alerts e ON e.id=i.opening_alert_id WHERE ` + condition + ` ORDER BY i.created_at DESC,e.sequence DESC`
		if limit > 0 {
			query += fmt.Sprintf(" LIMIT %d", limit)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			i, err := scanIncident(rows)
			if err != nil {
				return err
			}
			result = append(result, i)
		}
		return rows.Err()
	})
	return result, err
}
func (a *App) IncidentAudit(ctx context.Context, p Principal, id string) ([]IncidentAction, error) {
	var result []IncidentAction
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		if _, err := readIncident(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,incident_id,revision,actor,kind,message,actor_name,target_member_ids,previous_owner_member_id,related_alert_id,occurred_at,note FROM incident_actions WHERE incident_id=? ORDER BY id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var action IncidentAction
			var at int64
			var targets string
			if err = rows.Scan(&action.ID, &action.IncidentID, &action.Revision, &action.Actor, &action.Kind, &action.Message, &action.ActorName, &targets, &action.PreviousOwnerMemberID, &action.RelatedAlertID, &at, &action.Note); err != nil {
				return err
			}
			if err = json.Unmarshal([]byte(targets), &action.TargetMemberIDs); err != nil {
				return err
			}
			action.At = time.Unix(at, 0).UTC()
			result = append(result, action)
		}
		return rows.Err()
	})
	return result, err
}

var errIncidentChanged = fmt.Errorf("%w: This incident changed before your action was saved. Reload the page and try again.", ErrConflict)

func incidentMemberName(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM members WHERE id=?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "Former member", nil
	}
	return name, err
}
func incidentActorName(ctx context.Context, tx *sql.Tx, actor string) (string, error) {
	if strings.HasPrefix(actor, "member:") {
		return incidentMemberName(ctx, tx, strings.TrimPrefix(actor, "member:"))
	}
	if strings.HasPrefix(actor, "source:") {
		var name string
		err := tx.QueryRowContext(ctx, `SELECT name FROM sources WHERE id=?`, strings.TrimPrefix(actor, "source:")).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return "Former alert source", nil
		}
		return name, err
	}
	if actor == "triage_agent" {
		return "Triage agent", nil
	}
	// The mmhotline command's Unix user, as commit-reviews names its reviewer.
	if strings.HasPrefix(actor, "unix_user:") {
		return "the mmhotline command, run by " + strings.TrimPrefix(actor, "unix_user:"), nil
	}
	return "MMHotline", nil
}
func incidentTargetNames(ctx context.Context, tx *sql.Tx, i Incident) (string, error) {
	var names []string
	for _, id := range i.TargetMemberIDs {
		name, err := incidentMemberName(ctx, tx, id)
		if err != nil {
			return "", err
		}
		names = append(names, name)
	}
	return strings.Join(names, ", "), nil
}
func incidentContactingText(ctx context.Context, tx *sql.Tx, i Incident, repeat bool) (string, error) {
	if i.Step == "everyone_available" {
		return "Contacting everyone available", nil
	}
	names, err := incidentTargetNames(ctx, tx, i)
	if err != nil {
		return "", err
	}
	if repeat {
		return "Contacting " + names + " again", nil
	}
	switch i.Step {
	case "primary":
		return "Contacting " + names + " (on call)", nil
	case "backup":
		return "Contacting " + names + " (backup)", nil
	case "only_member":
		return "Contacting " + names + " (only team member)", nil
	case "chosen_person":
		var actorName string
		err := tx.QueryRowContext(ctx, `SELECT actor_name FROM incident_actions WHERE incident_id=? AND revision=? AND kind='escalated' ORDER BY id DESC LIMIT 1`, i.ID, i.Revision).Scan(&actorName)
		if errors.Is(err, sql.ErrNoRows) {
			// Nobody escalated it: it was opened for these people only.
			return "Contacting " + names + " only", nil
		}
		if err != nil {
			return "", err
		}
		return "Contacting " + names + " (chosen by " + actorName + ")", nil
	}
	return "", ErrInvalid
}

// notificationActionPlaces says where a claim or resolve from a notification
// came from, for the incident history: "Claimed by Alice in Telegram". The
// key is the claimedHow (or resolvedHow) value the contact method type passes.
var notificationActionPlaces = map[string]string{
	"slack_button":             " in Slack",
	"telegram_button":          " in Telegram",
	"pushover_acknowledgement": ", who acknowledged the alert in Pushover",
}

// NotificationActionResult is what happened to one incident a notification
// was about. Text is the plain answer for the person: "Claimed by Alice",
// "Already claimed by Bob", "Resolved by Alice", or "This incident is
// already resolved."
type NotificationActionResult struct {
	IncidentID, Title, Status, OwnerName, Text string
	// ClaimedNow is true when this action claimed the incident, false when
	// the member already held it or it was claimed by someone else or
	// resolved.
	ClaimedNow bool
}

// ClaimIncidentsFromNotification claims, for one member, every incident one
// notification (a delivery) was about. It is the one operation behind
// pressing 1 on a phone call and the Slack and Telegram buttons. An open incident is claimed; one the member already holds stays
// theirs; one claimed by someone else is never taken over; a resolved one is
// reported. Sending services call it and never change incidents themselves. The
// operation reads each incident's current revision itself, because a key
// press or a button cannot know it, and the rules above make that safe.
func (a *App) ClaimIncidentsFromNotification(ctx context.Context, deliveryID, memberID, claimedHow string) ([]NotificationActionResult, error) {
	return a.actOnNotificationIncidents(ctx, deliveryID, memberID, claimedHow, false)
}

// ResolveIncidentsFromNotification resolves every incident one notification
// was about, under the ordinary resolve rules. Only a contact method type whose claim
// method is a button (Slack, Telegram) may resolve; a phone key or a link
// never does.
func (a *App) ResolveIncidentsFromNotification(ctx context.Context, deliveryID, memberID, resolvedHow string) ([]NotificationActionResult, error) {
	return a.actOnNotificationIncidents(ctx, deliveryID, memberID, resolvedHow, true)
}

func (a *App) actOnNotificationIncidents(ctx context.Context, deliveryID, memberID, how string, resolve bool) ([]NotificationActionResult, error) {
	var results []NotificationActionResult
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		results, err = actOnNotificationIncidentsTx(ctx, tx, deliveryID, memberID, how, resolve, a.Now().UTC())
		return err
	})
	return results, err
}

func actOnNotificationIncidentsTx(ctx context.Context, tx *sql.Tx, deliveryID, memberID, how string, resolve bool, now time.Time) ([]NotificationActionResult, error) {
	place, known := notificationActionPlaces[how]
	if !known {
		return nil, ErrInvalid
	}
	var results []NotificationActionResult
	var m Member
	err := tx.QueryRowContext(ctx, `SELECT id,sign_in_email_address,name,role FROM members WHERE id=? AND removed_at IS NULL`, memberID).Scan(&m.ID, &m.SignInEmailAddress, &m.Name, &m.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	var contactMethodType, incidentID string
	err = tx.QueryRowContext(ctx, `SELECT contact_method_type,incident_id FROM deliveries WHERE id=?`, deliveryID).Scan(&contactMethodType, &incidentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	facts, _ := contactMethodTypeFacts(contactMethodType)
	if facts.ClaimMethod == "" || (resolve && facts.ClaimMethod != "button") {
		return nil, ErrForbidden
	}
	incidentIDs, err := notificationIncidentIDs(ctx, tx, deliveryID, incidentID)
	if err != nil {
		return nil, err
	}
	for _, id := range incidentIDs {
		i, err := readIncident(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		claimedNow := false
		switch {
		case i.Status == "resolved":
		case resolve:
			if i, err = resolveIncidentTx(ctx, tx, i, "member:"+m.ID, "", place, "", now); err != nil {
				return nil, err
			}
			results = append(results, NotificationActionResult{IncidentID: i.ID, Title: i.Title, Status: i.Status, Text: "Resolved by " + m.Name})
			continue
		case i.Status == "open":
			if i, err = claimIncidentTx(ctx, tx, i, m, place, "", now); err != nil {
				return nil, err
			}
			claimedNow = true
		}
		result := NotificationActionResult{IncidentID: i.ID, Title: i.Title, Status: i.Status, ClaimedNow: claimedNow}
		switch i.Status {
		case "resolved":
			result.Text = "This incident is already resolved."
			if claimedNow && i.SourceID == testAlertSourceID {
				result.Text = "Thank you. MMHotline closed the test."
			}
		case "claimed":
			if result.OwnerName, err = incidentMemberName(ctx, tx, i.OwnerMemberID); err != nil {
				return nil, err
			}
			result.Text = "Claimed by " + result.OwnerName
			if i.OwnerMemberID != m.ID {
				result.Text = "Already claimed by " + result.OwnerName
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// notificationIncidentIDs lists the incidents one notification was about: its
// own and, for a combined message, those of the deliveries sent together
// with it.
func notificationIncidentIDs(ctx context.Context, tx *sql.Tx, deliveryID, ownIncidentID string) ([]string, error) {
	frozen, err := pushoverCountedIncidentIDs(ctx, tx, deliveryID)
	if err != nil {
		return nil, err
	}
	if len(frozen) > 0 {
		return frozen, nil
	}
	ids := []string{}
	if ownIncidentID != "" {
		ids = append(ids, ownIncidentID)
	}
	// A message that left before this one was sent (its incident was claimed
	// meanwhile) is no longer combined into it.
	rows, err := tx.QueryContext(ctx, `SELECT incident_id FROM deliveries WHERE combined_into=? AND state='combined' AND incident_id!='' ORDER BY created_at,id`, deliveryID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, ErrNotFound
	}
	return ids, nil
}
