package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func incidentTeam(t *testing.T) (*App, Principal, []Member) {
	t.Helper()
	a, p, _ := alertFixture(t)
	ctx := context.Background()
	first, err := a.CurrentMember(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	members := []Member{first}
	for _, name := range []string{"Backup", "Third"} {
		members = append(members, addPerson(t, a, strings.ToLower(name)+"@example.test", name, RoleMember))
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for _, m := range members {
			for _, contactMethodType := range []string{"test_waking", "email"} {
				id := NewID()
				if _, err := tx.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) VALUES(?,?,?,'{}',?,1,1,0)`, id, m.ID, contactMethodType, m.ID+"@example.test"); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,'sos')`, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: []string{first.ID, first.ID, first.ID, first.ID, first.ID, first.ID, first.ID}, BackupKind: "fixed_person", BackupMemberID: members[1].ID}
	if _, err = a.SaveSchedule(ctx, p, c, 0); err != nil {
		t.Fatal(err)
	}
	return a, p, members
}

// giveWakingContactMethod gives the member with the sign-in email address a
// contact method that wakes people and receives SOS alerts, as a responder
// has: an SOS wakes only a person with one.
func giveWakingContactMethod(t *testing.T, a *App, signInEmailAddress string) {
	t.Helper()
	id := NewID()
	result, err := a.Store.db.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) SELECT ?,id,'test_waking','{}','waking contact method',1,0,0 FROM members WHERE sign_in_email_address=? AND removed_at IS NULL`, id, strings.ToLower(signInEmailAddress))
	if err != nil {
		t.Fatal(err)
	}
	if added, _ := result.RowsAffected(); added != 1 {
		t.Fatalf("no member signs in with %s", signInEmailAddress)
	}
	if _, err = a.Store.db.Exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,'sos')`, id); err != nil {
		t.Fatal(err)
	}
}
func newIncidentFixture(t *testing.T, a *App, p Principal) Incident {
	t.Helper()
	e, err := a.ManualSOS(context.Background(), p, "Database down", "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	i, err := a.GetIncident(context.Background(), p, e.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	return i
}
func TestOverdueIncidentAdvancesOnceAndConcurrentClaimsCancelSOSEscalation(t *testing.T) {
	a, p, members := incidentTeam(t)
	ctx := context.Background()
	now := a.Now()
	i := newIncidentFixture(t, a, p)
	if i.Step != "primary" || !sameTargets(i.TargetMemberIDs, []string{members[0].ID}) {
		t.Fatalf("initial: %+v", i)
	}
	now = now.Add(30 * time.Minute)
	a.Now = func() time.Time { return now }
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	i, _ = a.GetIncident(ctx, p, i.ID)
	if i.Step != "backup" || !sameTargets(i.TargetMemberIDs, []string{members[1].ID}) || !i.Deadline.Equal(now.Add(5*time.Minute)) || i.RepeatGeneration != 2 {
		t.Fatalf("overdue must grant one fresh backup window: %+v", i)
	}
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	same, _ := a.GetIncident(ctx, p, i.ID)
	if same.Revision != i.Revision || same.RepeatGeneration != i.RepeatGeneration {
		t.Fatal("same tick replayed sos_escalation")
	}
	oldRevision := i.Revision
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, m := range members[:2] {
		m := m
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.ClaimIncident(ctx, Principal{SignInEmailAddress: m.SignInEmailAddress}, i.ID, oldRevision, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("claims success=%d conflict=%d", success, conflict)
	}
	i, _ = a.GetIncident(ctx, p, i.ID)
	if i.Status != "claimed" {
		t.Fatalf("claimed %+v", i)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND state='queued'`, i.ID); n != 0 {
		t.Fatalf("queued after claim: %d", n)
	}
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		valid, _, err := ValidateSOSEscalation(ctx, tx, i.ID, oldRevision, members[1].ID, now)
		if valid {
			return fmt.Errorf("stale work valid")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner := i.OwnerMemberID
	i, err := a.ClaimIncident(ctx, Principal{SignInEmailAddress: members[2].SignInEmailAddress}, i.ID, i.Revision, "")
	if err != nil || i.OwnerMemberID != members[2].ID {
		t.Fatalf("reclaim: %+v %v", i, err)
	}
	audit, _ := a.IncidentAudit(ctx, p, i.ID)
	if audit[len(audit)-1].PreviousOwnerMemberID != owner {
		t.Fatal("previous owner absent from audit")
	}
	i, err = a.EscalateIncident(ctx, p, i.ID, members[1].ID, i.Revision)
	if err != nil || i.Status != "open" || i.OwnerMemberID != "" || i.Step != "chosen_person" {
		t.Fatalf("manual escalation: %+v %v", i, err)
	}
	i, err = a.ResolveIncident(ctx, p, i.ID, i.Revision, "")
	if err != nil || i.Status != "resolved" {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err = a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	resolved, _ := a.GetIncident(ctx, p, i.ID)
	if resolved.Revision != i.Revision || resolved.Status != "resolved" {
		t.Fatal("resolved incident resumed")
	}
	if got := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND contact_method_type='email' AND severity='sos'`, i.ID); got != 3 {
		t.Fatalf("sos_escalation repeats generated email: %d", got)
	}
	if got := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND contact_method_type='phone' AND severity='resolved-sos' AND state!='suppressed'`, i.ID); got != 0 {
		t.Fatal("resolution queued a call")
	}
}
func TestCoverageChangesPreserveTargetUntilIneligible(t *testing.T) {
	a, p, m := incidentTeam(t)
	ctx := context.Background()
	i := newIncidentFixture(t, a, p)
	c := ScheduleConfig{Kind: "day_of_week", TimeZone: "UTC", Assignments: []string{m[2].ID, m[2].ID, m[2].ID, m[2].ID, m[2].ID, m[2].ID, m[2].ID}, BackupKind: "fixed_person", BackupMemberID: m[1].ID}
	if _, err := a.SaveSchedule(ctx, p, c, 1); err != nil {
		t.Fatal(err)
	}
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := a.GetIncident(ctx, p, i.ID)
	if unchanged.Revision != i.Revision || !sameTargets(unchanged.TargetMemberIDs, []string{m[0].ID}) {
		t.Fatalf("coverage silently transferred target: %+v", unchanged)
	}
	if _, err := a.SetPersonalAvailability(ctx, p, "unavailable", a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	backup, _ := a.GetIncident(ctx, p, i.ID)
	if backup.Step != "backup" || !sameTargets(backup.TargetMemberIDs, []string{m[1].ID}) {
		t.Fatalf("did not advance to fixed backup: %+v", backup)
	}
	// Current effective coverage promoting the backup must not skip its time to claim.
	a2, p2, m2 := incidentTeam(t)
	i2 := newIncidentFixture(t, a2, p2)
	if _, err := a2.SetPersonalAvailability(ctx, p2, "unavailable", a2.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if err := a2.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	backup2, _ := a2.GetIncident(ctx, p2, i2.ID)
	if backup2.Step != "backup" || !sameTargets(backup2.TargetMemberIDs, []string{m2[1].ID}) {
		t.Fatalf("effective-primary shortcut skipped backup: %+v", backup2)
	}
}
func TestTeamMuteNoCoverageFallbackAndUnmuteCoalesceCurrentWork(t *testing.T) {
	a, p, m := incidentTeam(t)
	ctx := context.Background()
	now := a.Now()
	a.Now = func() time.Time { return now }
	for _, member := range m {
		if _, err := a.SetPersonalAvailability(ctx, Principal{SignInEmailAddress: member.SignInEmailAddress}, "unavailable", now.Add(2*time.Hour), 0); err != nil {
			t.Fatal(err)
		}
	}
	mute, err := a.SetTeamMute(ctx, p, now.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	i := newIncidentFixture(t, a, p)
	if !i.IgnorePersonal || i.Step != "everyone_available" || len(i.TargetMemberIDs) != 3 {
		t.Fatalf("fallback: %+v", i)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND state!='suppressed'`, i.ID); n != 0 {
		t.Fatal("team mute bypassed")
	}
	if err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		ok, reason, err := ValidateSOSEscalation(ctx, tx, i.ID, i.Revision, m[0].ID, now)
		if ok || reason != "team mute" {
			t.Fatalf("dispatch while muted: %v %s", ok, reason)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if err = a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := a.GetIncident(ctx, p, i.ID)
	if after.RepeatGeneration != 2 {
		t.Fatalf("missed repeats replayed: %+v", after)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND state='queued'`, i.ID); n != 3 {
		t.Fatalf("expected one current target round: %d", n)
	}
	if err = a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err = a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND state='queued'`, i.ID); n != 3 {
		t.Fatalf("queued current work duplicated: %d", n)
	}
	if err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		state, err := ReadPersonalAvailability(ctx, tx, m[0].ID)
		if !state.UnavailableUntil.After(now) {
			t.Fatal("team mute changed personal unavailability")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = mute
}
func TestIncidentOperationsPreserveClaimedOwnership(t *testing.T) {
	a, p, m := incidentTeam(t)
	ctx := context.Background()
	i := newIncidentFixture(t, a, p)
	i, err := a.ClaimIncident(ctx, p, i.ID, i.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	pBackup := Principal{SignInEmailAddress: m[1].SignInEmailAddress}
	if _, err = a.SetOnCallClaim(ctx, pBackup, a.Now().Add(time.Hour), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err = a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	current, _ := a.GetIncident(ctx, p, i.ID)
	if current.OwnerMemberID != m[0].ID || current.Revision != i.Revision {
		t.Fatal("coverage transferred claimed incident")
	}
}

func TestIncidentHistoryAndNoticesUseNamesAndTitles(t *testing.T) {
	a, p, members := coverageFixture(t)
	ctx := context.Background()
	for _, m := range members {
		giveWakingContactMethod(t, a, m.SignInEmailAddress)
	}
	if _, err := a.SaveSchedule(ctx, p, dayOfWeekFor(members[0].ID), 0); err != nil {
		t.Fatal(err)
	}
	alert, err := a.ManualSOS(ctx, p, "Database down", "")
	if err != nil {
		t.Fatal(err)
	}
	incident, _ := a.GetIncident(ctx, p, alert.IncidentID)
	firstClaim, err := a.ClaimIncident(ctx, p, incident.ID, incident.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	second := Principal{SignInEmailAddress: members[1].SignInEmailAddress}
	takeover, err := a.ClaimIncident(ctx, second, incident.ID, firstClaim.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	escalated, err := a.EscalateIncident(ctx, second, incident.ID, members[2].ID, takeover.Revision)
	if err != nil {
		t.Fatal(err)
	}
	history, err := a.IncidentAudit(ctx, p, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages := ""
	for _, action := range history {
		messages += action.Message + "\n"
		for _, member := range members {
			if strings.Contains(action.Message, member.ID) || strings.Contains(action.ActorName, member.ID) {
				t.Fatal("member ID shown in history")
			}
		}
		if strings.Contains(action.Message, incident.ID) {
			t.Fatal("incident ID shown in history")
		}
		if action.Kind == "reclaimed" && action.PreviousOwnerMemberID != members[0].ID {
			t.Fatal("previous owner identity not retained")
		}
		if action.Kind == "escalated" && (len(action.TargetMemberIDs) != 1 || action.TargetMemberIDs[0] != members[2].ID || action.ActorName != members[1].Name) {
			t.Fatalf("history identities: %+v", action)
		}
	}
	for _, text := range []string{"Contacting " + members[0].Name + " (on call)", "Claimed by " + members[0].Name, "Taken over by " + members[1].Name + " from " + members[0].Name, "Escalated to " + members[2].Name + " by " + members[1].Name, "Contacting " + members[2].Name + " (chosen by " + members[1].Name + ")"} {
		if !strings.Contains(messages, text) {
			t.Fatalf("missing %q in %s", text, messages)
		}
	}
	notices, err := a.ListAlerts(ctx, p, AlertFilter{Query: "Database down"})
	if err != nil {
		t.Fatal(err)
	}
	expected := members[1].Name + " escalated the SOS incident “Database down” to " + members[2].Name + ", so MMHotline contacts " + members[2].Name + " now."
	found := false
	for _, notice := range notices {
		if notice.SourceID == "system" && notice.Message == expected {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing notice %q", expected)
	}
	if _, err := a.ClaimIncident(ctx, p, incident.ID, takeover.Revision, ""); publicError(err) != "The incident changed before your action was saved. Reload the page and try again." {
		t.Fatalf("stale action: %v", err)
	}
	now := a.Now()
	a.Now = func() time.Time { return now.Add(time.Minute) }
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	history, _ = a.IncidentAudit(ctx, p, incident.ID)
	if got := history[len(history)-1].Message; got != "Contacting "+members[2].Name+" again" {
		t.Fatalf("repeat: %s", got)
	}
	// Keep readable snapshots after a member is renamed.
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE members SET name='Renamed' WHERE id=?`, members[1].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	history, _ = a.IncidentAudit(ctx, p, incident.ID)
	for _, action := range history {
		if action.Kind == "reclaimed" && action.ActorName != members[1].Name {
			t.Fatal("historical actor name changed")
		}
	}
	if escalated.Step != "chosen_person" {
		t.Fatal(escalated.Step)
	}
}

func TestOnlyMemberFirstContactAndRepeatText(t *testing.T) {
	a := foundationApp(t)
	person := memberFixture(t, a, "only-member@example.test")
	giveWakingContactMethod(t, a, "only-member@example.test")
	ctx := context.Background()
	alert, err := a.ManualSOS(ctx, person.Principal, "Service down", "")
	if err != nil {
		t.Fatal(err)
	}
	history, err := a.IncidentAudit(ctx, person.Principal, alert.IncidentID)
	if err != nil || len(history) != 2 {
		t.Fatalf("initial history: %+v %v", history, err)
	}
	if strings.Contains(history[1].Message, "again") || !strings.Contains(history[1].Message, "(only team member)") {
		t.Fatalf("first contact: %s", history[1].Message)
	}
	now := a.Now()
	a.Now = func() time.Time { return now.Add(time.Minute) }
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	history, err = a.IncidentAudit(ctx, person.Principal, alert.IncidentID)
	if err != nil || !strings.HasSuffix(history[len(history)-1].Message, " again") {
		t.Fatalf("repeat history: %+v %v", history, err)
	}
}

// escalateIfUnclaimedAfterRig is incidentTeam, whose primary is members[0] and whose
// backup is members[1], with a clock the test moves from start.
type escalateIfUnclaimedAfterRig struct {
	a          *App
	p          Principal
	members    []Member
	start, now time.Time
}

func newEscalateIfUnclaimedAfterRig(t *testing.T) *escalateIfUnclaimedAfterRig {
	t.Helper()
	a, p, members := incidentTeam(t)
	r := &escalateIfUnclaimedAfterRig{a: a, p: p, members: members, start: a.Now(), now: a.Now()}
	a.Now = func() time.Time { return r.now }
	return r
}

// setEscalateIfUnclaimedAfter sets the window as the settings file does, and checks
// that schedules show reports it.
func (r *escalateIfUnclaimedAfterRig) setEscalateIfUnclaimedAfter(t *testing.T, minutes int) {
	t.Helper()
	if err := r.a.SetEscalateIfUnclaimedAfter(context.Background(), Principal{SettingsFile: "/etc/mmhotline/mmhotline_settings.yml"}, minutes); err != nil {
		t.Fatal(err)
	}
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "schedules", Action: "show"}, 200)
	if result.ScheduleState == nil || result.ScheduleState.EscalateIfUnclaimedAfterMinutes != minutes {
		t.Fatalf("schedules show result: %s", raw)
	}
}

// stepAt moves the clock to after since start, lets MMHotline do the
// incident's due work, and returns the incident.
func (r *escalateIfUnclaimedAfterRig) stepAt(t *testing.T, id string, after time.Duration) Incident {
	t.Helper()
	r.now = r.start.Add(after)
	if err := r.a.ReconcileIncidents(context.Background()); err != nil {
		t.Fatal(err)
	}
	i, err := r.a.GetIncident(context.Background(), r.p, id)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

// An unclaimed SOS reaches the backup when the window the command set ends,
// and not before.
func TestTheEscalateIfUnclaimedAfterSetByTheCommandTimesEachEscalationStep(t *testing.T) {
	r := newEscalateIfUnclaimedAfterRig(t)
	r.setEscalateIfUnclaimedAfter(t, 2)
	id := newIncidentFixture(t, r.a, r.p).ID
	if i := r.stepAt(t, id, 2*time.Minute-time.Second); i.Step != "primary" {
		t.Fatalf("before 2 minutes the step is %s, want primary", i.Step)
	}
	i := r.stepAt(t, id, 2*time.Minute)
	if i.Step != "backup" || !sameTargets(i.TargetMemberIDs, []string{r.members[1].ID}) {
		t.Fatalf("at 2 minutes: step %s, targets %v; want the backup", i.Step, i.TargetMemberIDs)
	}
	if i = r.stepAt(t, id, 4*time.Minute-time.Second); i.Step != "backup" {
		t.Fatalf("before 4 minutes the step is %s, want backup", i.Step)
	}
	if i = r.stepAt(t, id, 4*time.Minute); i.Step != "everyone_available" {
		t.Fatalf("at 4 minutes the step is %s, want everyone_available", i.Step)
	}
}

// A changed window does not move the end of a step already under way; the
// next step gets the new window.
func TestAChangedEscalateIfUnclaimedAfterReachesTheStepsThatStartAfterTheChange(t *testing.T) {
	r := newEscalateIfUnclaimedAfterRig(t)
	id := newIncidentFixture(t, r.a, r.p).ID
	r.now = r.start.Add(time.Minute)
	r.setEscalateIfUnclaimedAfter(t, 2)
	// The primary's step started with the default 5 minutes.
	if i := r.stepAt(t, id, 5*time.Minute-time.Second); i.Step != "primary" {
		t.Fatalf("before 5 minutes the step is %s, want primary", i.Step)
	}
	if i := r.stepAt(t, id, 5*time.Minute); i.Step != "backup" {
		t.Fatalf("at 5 minutes the step is %s, want backup", i.Step)
	}
	if i := r.stepAt(t, id, 7*time.Minute-time.Second); i.Step != "backup" {
		t.Fatalf("before 7 minutes the step is %s, want backup", i.Step)
	}
	if i := r.stepAt(t, id, 7*time.Minute); i.Step != "everyone_available" {
		t.Fatalf("at 7 minutes the step is %s, want everyone_available: the backup's step gets the new 2 minutes", i.Step)
	}
}

// One incident whose handling fails is undone and reported, and the other
// open incidents still escalate in the same visit. Before, every incident
// shared one transaction, so one stored row the code could not handle
// stopped escalation for all of them, every second.
func TestOneIncidentThatFailsDoesNotStopTheOthersEscalating(t *testing.T) {
	a, p, members := incidentTeam(t)
	ctx := context.Background()
	now := a.Now()
	a.Now = func() time.Time { return now }
	broken := newIncidentFixture(t, a, p)
	healthy := newIncidentFixture(t, a, p)
	if broken.ID == healthy.ID {
		t.Fatal("the fixture opened one incident, not two")
	}
	// Every change to the broken incident fails, as a stored row that the
	// code cannot handle does.
	if _, err := a.Store.db.Exec(`CREATE TRIGGER a_row_the_code_cannot_handle BEFORE UPDATE ON incidents WHEN OLD.id='` + broken.ID + `' BEGIN SELECT RAISE(ABORT,'a row the code cannot handle'); END`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	err := a.ReconcileIncidents(ctx)
	if err == nil || !strings.Contains(err.Error(), broken.ID) || !strings.Contains(err.Error(), "a row the code cannot handle") {
		t.Fatalf("the failing incident was not reported: %v", err)
	}
	healthy, _ = a.GetIncident(ctx, p, healthy.ID)
	if healthy.Step != "backup" || !sameTargets(healthy.TargetMemberIDs, []string{members[1].ID}) {
		t.Fatalf("the other incident did not escalate to the backup: %+v", healthy)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND revision>1`, broken.ID); n != 0 {
		t.Fatalf("the failing incident's work was not undone: %d history lines", n)
	}
}

// Every escalation step is one the stored CHECK accepts and every switch
// over steps knows, and the CHECK refuses any other step, so a step added in
// one place only is refused when it is saved.
func TestEveryEscalationStepIsKnownWhereverStepsAreUsed(t *testing.T) {
	a, p, members := incidentTeam(t)
	ctx := context.Background()
	at := a.Now().Add(time.Minute)
	i := newIncidentFixture(t, a, p)
	for _, step := range incidentSteps {
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			i.Step, i.TargetMemberIDs = step, []string{members[0].ID}
			if _, err := tx.Exec(`UPDATE incidents SET step=? WHERE id=?`, step, i.ID); err != nil {
				return fmt.Errorf("the stored CHECK refused it: %w", err)
			}
			setStepTimes(&i, time.Minute, at)
			if step != triageAgentStep && !i.stepStartedAt().Equal(at) {
				return fmt.Errorf("setStepTimes and stepStartedAt disagree: %s", i.stepStartedAt())
			}
			if step == triageAgentStep {
				// An incident held for the triage agent wakes nobody, so
				// no round of waking is ever described for it.
				return nil
			}
			if step == "chosen_person" {
				i.Revision++
				if err := recordIncidentAction(ctx, tx, i, "member:"+members[1].ID, "escalated", "Escalated", "", "", at); err != nil {
					return err
				}
			}
			for _, repeat := range []bool{false, true} {
				if _, err := incidentContactingText(ctx, tx, i, repeat); err != nil {
					return fmt.Errorf("incidentContactingText (repeat %t): %w", repeat, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("step %s: %v", step, err)
		}
	}
	if _, err := a.Store.db.Exec(`UPDATE incidents SET step='second_backup' WHERE id=?`, i.ID); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("an unknown step was saved: %v", err)
	}
}

// The reconcile visit and the dashboard include every open incident. Before,
// both read only the oldest 1,000, so an SOS under MMHotline's own source,
// which may open past the limit and is then among the newest, was woken once
// and never escalated, and the dashboard left it out: MMHotline's own SOS
// about refused SOS alerts, and each urgent report of a software review.
func TestMMHotlinesOwnSOSPastTheIncidentLimitEscalatesAndShowsOnTheDashboard(t *testing.T) {
	a, p, members := incidentTeam(t)
	ctx := context.Background()
	now := a.Now()
	a.Now = func() time.Time { return now }
	source, err := createAlertSource(ctx, a, p, "Monitoring")
	if err != nil {
		t.Fatal(err)
	}
	// The fillers come from many alert sources, so no alert source reaches
	// its share of pending SOS deliveries before the incident limit.
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for n := range maxUnresolvedIncidents {
			if _, err := admitAlert(ctx, tx, fmt.Sprintf("filler-%d", n), "test", fmt.Sprintf("fill-%d", n), "", GenericAlert{Severity: "sos", SenderIncidentKey: fmt.Sprintf("k%d", n), Title: "Filler", Message: ""}, nil, now.Add(-time.Second)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := intakeHTTP(a, p, source, `{"sender_alert_id":"one-more","sender_incident_key":"new","severity":"sos","title":"One more","message":""}`, now); w.Code != 429 {
		t.Fatalf("SOS over the incident limit: HTTP %d", w.Code)
	}
	var own string
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT id FROM incidents WHERE source_id='system' AND sender_incident_key=?`, "refusing-sos:"+source.Source.ID).Scan(&own)
	}); err != nil {
		t.Fatalf("MMHotline's own SOS did not open: %v", err)
	}
	// A software review's run has no triage session, so its urgent report
	// opens an SOS of its own, past the limit too.
	now = now.Add(time.Second)
	var review Alert
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		review, _, err = admitAlertAboutTriageRun(ctx, tx, "", "software-review-run", "triage_agent", GenericAlert{Severity: "sos", Title: "The software review found a fix that cannot wait", Message: "Upgrade OpenSSH on app002."}, now)
		return err
	}); err != nil || review.IncidentID == "" {
		t.Fatalf("the software review's SOS did not open: %+v %v", review, err)
	}
	now = now.Add(30 * time.Minute)
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{own, review.IncidentID} {
		i, err := a.GetIncident(ctx, p, id)
		if err != nil {
			t.Fatal(err)
		}
		if i.Step != "backup" || !sameTargets(i.TargetMemberIDs, []string{members[1].ID}) {
			t.Fatalf("the SOS %q past the limit did not escalate: step %s, targets %v", i.Title, i.Step, i.TargetMemberIDs)
		}
	}
	view, err := a.readDashboard(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]int{}
	for _, waiting := range view.Waiting {
		shown[waiting.ID]++
	}
	if shown[own] != 1 || shown[review.IncidentID] != 1 || len(view.Waiting) != maxUnresolvedIncidents+2 {
		t.Fatalf("the dashboard shows MMHotline's own SOS %d times and the software review's %d times, with %d incidents waiting, want 1, 1, and %d", shown[own], shown[review.IncidentID], len(view.Waiting), maxUnresolvedIncidents+2)
	}
	listed, err := a.ListIncidents(ctx, p)
	if err != nil || len(listed) != maxUnresolvedIncidents+2 || listed[0].ID != review.IncidentID || listed[1].ID != own {
		t.Fatalf("the incidents command lists %d incidents, want %d with the two newest first: %v", len(listed), maxUnresolvedIncidents+2, err)
	}
}

// A person the step targets who has no waking contact method is not woken,
// so MMHotline says so, tells the administrators once, and moves to the next
// step at once instead of waiting out the time to claim. Before, the
// history said "Contacting" them and the incident page said MMHotline woke
// them, while nothing was sent.
func TestAPersonWithNoWakingContactMethodIsSkippedAtOnce(t *testing.T) {
	a, p, members := incidentTeam(t)
	ctx := context.Background()
	now := a.Now()
	a.Now = func() time.Time { return now }
	// The person on call has only email left.
	if _, err := a.Store.db.Exec(`UPDATE contact_methods SET deleted=1 WHERE member_id=? AND contact_method_type='test_waking'`, members[0].ID); err != nil {
		t.Fatal(err)
	}
	i := newIncidentFixture(t, a, p)
	if i.Step != "backup" || !sameTargets(i.TargetMemberIDs, []string{members[1].ID}) {
		t.Fatalf("the backup was not woken at once: step %s, targets %v", i.Step, i.TargetMemberIDs)
	}
	couldNotWake := "Could not wake " + members[0].Name + ": " + members[0].Name + " has no contact method that wakes people for SOS alerts"
	notices := `SELECT count(*) FROM alerts WHERE source_id='system' AND title LIKE 'MMHotline could not wake%'`
	check := func(when string) {
		t.Helper()
		if n := scalarTeam(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='could_not_wake' AND message=?`, i.ID, couldNotWake); n != 1 {
			t.Fatalf("%s: %d history lines say MMHotline could not wake the person on call, want 1", when, n)
		}
		if n := scalarTeam(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='sos_escalation_due' AND target_member_ids LIKE ?`, i.ID, "%"+members[0].ID+"%"); n != 0 {
			t.Fatalf("%s: %d rounds of waking name the person on call", when, n)
		}
		if n := scalarTeam(t, a, notices); n != 1 {
			t.Fatalf("%s: %d notices to the administrators, want 1", when, n)
		}
	}
	check("at once")
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND member_id=?`, i.ID, members[1].ID); n == 0 {
		t.Fatal("the backup was not sent a waking message")
	}
	// Read by another member, the page names the person on call.
	page := incidentPageAs(t, a, Principal{SignInEmailAddress: members[2].SignInEmailAddress}, i.ID)
	if !strings.Contains(page, "MMHotline could not wake "+members[0].Name) || strings.Contains(page, "MMHotline woke "+members[0].Name) {
		t.Fatalf("the incident page does not say MMHotline could not wake the person on call: %s", page)
	}
	// The backup's repeats add no more lines or notices about them.
	for range 3 {
		now = now.Add(time.Minute)
		if err := a.ReconcileIncidents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check("after repeats")
}

// When nobody the incident can reach has a waking contact method, the
// incident still reaches everyone available, says once whom MMHotline could
// not wake, and tells the administrators once; the rounds that follow repeat
// neither.
func TestAnSOSThatNobodyCanBeWokenForReachesEveryoneAvailableAndSaysSoOnce(t *testing.T) {
	a, p, members := incidentTeam(t)
	ctx := context.Background()
	now := a.Now()
	a.Now = func() time.Time { return now }
	if _, err := a.Store.db.Exec(`UPDATE contact_methods SET deleted=1 WHERE contact_method_type='test_waking'`); err != nil {
		t.Fatal(err)
	}
	i := newIncidentFixture(t, a, p)
	if i.Step != "everyone_available" {
		t.Fatalf("the incident stopped at %s, want everyone_available", i.Step)
	}
	lines := func() int64 {
		return scalarTeam(t, a, `SELECT count(*) FROM incident_actions WHERE incident_id=? AND kind='could_not_wake'`, i.ID)
	}
	// The person on call, the backup, and then everyone available, each
	// once at their own step.
	want := int64(1 + 1 + len(members))
	if n := lines(); n != want {
		t.Fatalf("%d history lines say MMHotline could not wake someone, want %d", n, want)
	}
	for range 5 {
		now = now.Add(time.Minute)
		if err := a.ReconcileIncidents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := lines(); n != want {
		t.Fatalf("after five more minutes, %d history lines say MMHotline could not wake someone, want %d", n, want)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE source_id='system' AND title LIKE 'MMHotline could not wake%'`); n != 1 {
		t.Fatalf("%d notices to the administrators, want 1", n)
	}
	// A person who adds a waking contact method is woken at the next round.
	if _, err := a.Store.db.Exec(`UPDATE contact_methods SET deleted=0 WHERE member_id=? AND contact_method_type='test_waking'`, members[2].ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE incident_id=? AND purpose='sos_escalation' AND member_id=?`, i.ID, members[2].ID); n == 0 {
		t.Fatal("the person who added a waking contact method was not woken")
	}
}
