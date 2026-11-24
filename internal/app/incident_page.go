package app

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// The SOS incident page, /incidents/{id}
// (docs/changing-mmhotline/design/incidents.md#the-incident-page). A person woken at night opens
// it from a notification, often on a phone, so it answers first, in one loud
// box, whom MMHotline wakes, what it does next by itself, and Claim. Then
// come where the incident is, what the triage agent found, an approval it
// waits for, one "What happened" list, and folded technical detail.

// incidentPage is what the incident page shows one member, the reader, whom
// it calls "you".
type incidentPage struct {
	CSRF     string
	Incident Incident
	// SentBy names who raised the SOS: an alert source, the person who
	// raised it by hand, the triage agent, or MMHotline.
	SentBy string
	// Say opens the decide box: whom MMHotline wakes, who claimed the
	// incident, or who resolved it and how.
	Say string
	// Countdown is what MMHotline does next by itself while the incident
	// waits for a claim; nil once someone claimed or resolved it.
	Countdown *incidentCountdown
	// HeldForTriageAgent: nobody is woken yet, because the triage agent
	// looks first.
	HeldForTriageAgent, ClaimedByYou bool
	// ResolvedNote is the note written with the resolve, with its writer.
	ResolvedNote string
	// ApprovalRequestID names the approval request MMHotline raised this SOS
	// for, to wake the person on call; "" for any other SOS.
	ApprovalRequestID string
	Steps             []incidentStep
	History           []incidentEvent
	People            incidentPeople
	// ReaderID is the member who reads the page.
	ReaderID string
	// Escalate lists the team's members for "Escalate to someone else".
	Escalate      []escalateOption
	TriageSession *TriageSessionLiveView
	// TriageReport is the triage agent's latest report about the incident.
	TriageReport *triageReport
	// OpenApprovals wait for a person: an approval request, or a warned run
	// someone may still stop. Decided are the rest.
	OpenApprovals, Decided []incidentApproval
	Deliveries             []incidentDelivery
	// LaterAlerts counts the alerts about the incident after the one that
	// opened it.
	LaterAlerts       int
	StatusPageCanPost bool
	// StatusPageNoticeID is the open notice this incident posted, or 0.
	StatusPageNoticeID int64
	// ReturnToIncidentID brings the triage agent's and the approvals' forms
	// back to this page.
	ReturnToIncidentID                             string
	MaximumNoteCharacters, MaximumReasonCharacters int
}

// incidentStep is one stage of "where it is": SOS received, woken, waiting
// for a claim or claimed, and resolved. State is "done", "now", or "" for a
// stage still to come.
type incidentStep struct{ Label, When, State string }

// incidentEvent is one line of "What happened".
type incidentEvent struct {
	At time.Time
	// Tone colors the line's dot: "sos" for the SOS and each round of
	// waking, "warn" for what waits for a decision, "agent" for the triage
	// agent, "person" for a person's action, "next" for what MMHotline does
	// next by itself, and "" for MMHotline's other steps.
	Tone, Text string
	// Note is what a person wrote with a claim or a resolve.
	Note string
	// AlertID links the alert the line is about, under AlertLabel, or "The
	// alert" when it is empty.
	AlertID, AlertLabel string
	// Reached says how a round of waking reached people.
	Reached string
	// Again counts the later rounds that woke the same people again, the
	// last at LastAgain.
	Again     int
	LastAgain time.Time
}

// incidentPeople is the People panel: who is on call, whom the incident woke,
// whom it wakes next, and who cannot be woken now.
type incidentPeople struct {
	OnCall, Backup, Woken, Skipped string
	// Next names whom the next step wakes at NextAt, while the incident
	// waits for a claim and the step has a deadline.
	Next   string
	NextAt time.Time
}

// escalateOption is a member in "Escalate to someone else"; a member who is
// unavailable until UnavailableUntil cannot be chosen.
type escalateOption struct {
	ID, Name         string
	UnavailableUntil time.Time
}

// triageReport is a report of the triage agent: its first paragraph, the
// whole text, and when it came.
type triageReport struct {
	Summary, Text string
	At            time.Time
}

// incidentApproval is an approval request or warned run as the incident page
// shows it, in short: the approval's own page, /approvals/{id}, lists each
// task and the agent's whole diagnosis.
type incidentApproval struct {
	ApprovalRequestView
	// ApproveLabel says whether this approval starts the run.
	ApproveLabel string
	// Why says why it waits for a person.
	Why             string
	RunsAt, Changed time.Time
}

// incidentDelivery is one notification about the incident, with its outcome
// and its reason in plain words.
type incidentDelivery struct {
	ID                    string
	At                    time.Time
	What, Outcome, Reason string
}

func (a *App) incidentPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	page, err := a.readIncidentPage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	page.CSRF = a.CSRFToken(w, r)
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "SOS incident: "+page.Incident.Title, frame, incidentHeading(frame, page), incidentBody(frame, page))
}

// readIncidentPage reads incident id's page for member p, through the same
// operations the command and the other pages use.
func (a *App) readIncidentPage(ctx context.Context, p Principal, id string) (incidentPage, error) {
	page := incidentPage{MaximumNoteCharacters: maximumIncidentNoteCharacters, MaximumReasonCharacters: maximumRefusalReasonCharacters}
	i, err := a.GetIncident(ctx, p, id)
	if err != nil {
		return page, err
	}
	page.Incident, page.ReturnToIncidentID = i, i.ID
	members, err := a.membersForHistory(ctx, p)
	if err != nil {
		return page, err
	}
	audit, err := a.IncidentAudit(ctx, p, i.ID)
	if err != nil {
		return page, err
	}
	alerts, err := a.ListAlerts(ctx, p, AlertFilter{IncidentID: i.ID, Limit: 30})
	if err != nil {
		return page, err
	}
	deliveries, err := a.IncidentDeliveries(ctx, p, i.ID)
	if err != nil {
		return page, err
	}
	if page.TriageSession, err = a.IncidentTriageSession(ctx, p, i.ID); err != nil {
		return page, err
	}
	approvals, err := a.IncidentApprovalRequests(ctx, p, i.ID)
	if err != nil {
		return page, err
	}
	if page.StatusPageCanPost, err = a.statusPageCanPost(ctx); err != nil {
		return page, err
	}
	if page.StatusPageCanPost {
		if err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			page.StatusPageNoticeID, err = incidentOpenStatusPageNotice(ctx, tx, i.ID)
			return err
		}); err != nil {
			return page, err
		}
	}
	coverage, err := a.CurrentCoverage(ctx, p)
	if err != nil {
		return page, err
	}
	reader := incidentReader{names: map[string]string{}, now: a.Now().UTC()}
	for _, m := range members {
		reader.names[m.ID] = m.Name
	}
	var step incidentNextStep
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		viewer, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		reader.id = viewer.ID
		if reader.zone, err = teamTimeZone(ctx, tx); err != nil {
			return err
		}
		if i.Status == "open" {
			if step, err = nextIncidentStep(ctx, tx, i); err != nil {
				return err
			}
		}
		for _, m := range members {
			eligible, reason, err := MemberEligible(ctx, tx, m.ID, reader.now)
			if err != nil {
				return err
			}
			// A former member is no one to wake.
			if reason == "member removed" {
				continue
			}
			option := escalateOption{ID: m.ID, Name: m.Name}
			if !eligible {
				availability, err := ReadPersonalAvailability(ctx, tx, m.ID)
				if err != nil {
					return err
				}
				option.UnavailableUntil = availability.UnavailableUntil
			}
			page.Escalate = append(page.Escalate, option)
		}
		return nil
	})
	if err != nil {
		return page, err
	}

	page.HeldForTriageAgent = i.Status == "open" && i.Step == triageAgentStep
	page.ClaimedByYou = i.Status == "claimed" && i.OwnerMemberID == reader.id
	page.Say, page.ResolvedNote = reader.say(i, audit, coverage)
	page.Steps = reader.steps(i, audit, step)
	page.History = reader.history(audit, alerts, deliveries)
	page.People = reader.peoplePanel(i, audit, coverage, step, page.Escalate)
	page.ReaderID = reader.id
	if i.Status == "open" {
		countdown := viewIncidentCountdown(step, reader.id, reader.zone, reader.now)
		page.Countdown = &countdown
		page.History = append(page.History, incidentEvent{At: step.At, Tone: "next", Text: countdown.Sentence})
	}
	for _, action := range audit {
		switch action.Kind {
		case "opened":
			page.SentBy = reader.sentBy(action)
		case "triage_agent_report":
			page.TriageReport = &triageReport{Summary: reportSummary(action.Message), Text: action.Message, At: action.At}
		}
	}
	// MMHotline raises an SOS of its own to ask for an approval when no
	// incident of the asking run's work is open: the oldest request of the
	// incident is the one it was raised for.
	if len(audit) > 0 && audit[0].Actor == playbookRunApprovalsActor {
		for _, v := range approvals {
			if v.IncidentID == i.ID {
				page.ApprovalRequestID = v.ID
			}
		}
	}
	page.OpenApprovals, page.Decided = viewIncidentApprovals(approvals)
	page.Deliveries = reader.deliveries(deliveries)
	for _, e := range alerts {
		if e.ID != i.OpeningAlertID {
			page.LaterAlerts++
		}
	}
	return page, nil
}

// incidentReader is the member who reads the page, whom it calls "you", with
// every member's name and the team's clock.
type incidentReader struct {
	id    string
	names map[string]string
	now   time.Time
	zone  *time.Location
}

func (r incidentReader) clock(t time.Time) string { return pageClock(t, r.now, r.zone) }

// person names a member inside a sentence: "you", or their name.
func (r incidentReader) person(id string) string {
	return memberNameForReader(id, r.id, r.names)
}

// people names members inside a sentence, the reader first: "you and Priya
// Natarajan".
func (r incidentReader) people(ids []string) string {
	var people []stepPerson
	for _, id := range ids {
		people = append(people, stepPerson{id, r.person(id)})
	}
	return peopleText(people, r.id)
}

// actor names who did an action, to start a sentence: "You", a member's or
// an alert source's name, "The triage agent", or "MMHotline".
func (r incidentReader) actor(action IncidentAction) string {
	switch action.Actor {
	case "member:" + r.id:
		return "You"
	case "triage_agent":
		return "The triage agent"
	}
	return action.ActorName
}

// note is the note a person wrote with a claim or a resolve, with its writer.
func (r incidentReader) note(action IncidentAction) string {
	if action.Note != "" && action.Actor == "member:"+r.id {
		return "Your note: “" + action.Note + "”"
	}
	return action.NoteText()
}

// sentBy names who raised the SOS, from the action that opened it.
func (r incidentReader) sentBy(opened IncidentAction) string {
	switch {
	case opened.Actor == "member:"+r.id:
		return "you, by hand"
	case strings.HasPrefix(opened.Actor, "member:"):
		return opened.ActorName + ", by hand"
	case opened.Actor == "triage_agent":
		return "the triage agent"
	}
	return opened.ActorName
}

// wokenOrder lists the members the incident woke, in the order its rounds of
// waking first woke them, with when.
func wokenOrder(audit []IncidentAction) ([]string, map[string]time.Time) {
	var order []string
	at := map[string]time.Time{}
	for _, action := range audit {
		if action.Kind != "sos_escalation_due" {
			continue
		}
		for _, id := range action.TargetMemberIDs {
			if _, woken := at[id]; !woken {
				order, at[id] = append(order, id), action.At
			}
		}
	}
	return order, at
}

// say opens the decide box: while the incident waits for a claim, whom
// MMHotline wakes and since when; then who claimed it; once resolved, who
// resolved it and how, with the note written with the resolve.
func (r incidentReader) say(i Incident, audit []IncidentAction, coverage Coverage) (say, resolvedNote string) {
	switch i.Status {
	case "claimed":
		return capitalize(r.person(i.OwnerMemberID)) + " claimed the incident at " + r.clock(i.ClaimedAt) + ". MMHotline wakes nobody else about the incident.", ""
	case "resolved":
		return r.resolved(i, audit)
	}
	if i.Step == triageAgentStep {
		if heldForAServerReplacementIn(audit) {
			return "The triage agent is replacing a server, so MMHotline has woken nobody yet.", ""
		}
		return "The triage agent looks first, so MMHotline has woken nobody yet.", ""
	}
	if len(i.TargetMemberIDs) == 0 {
		return "MMHotline has nobody to wake yet: the team has no members.", ""
	}
	// An SOS of a triage session that already woke its people says so
	// until its step changes (createIncident).
	for _, action := range audit {
		if action.Kind == "already_woken_for_triage_session" && action.Revision == i.Revision {
			return "MMHotline already woke " + r.people(i.TargetMemberIDs) + " for this incident's triage session, so this incident wakes nobody now.", ""
		}
	}
	var sentences []string
	if !coverage.PrimaryIsEveryoneAvailable && slices.Contains(coverage.PrimaryIDs, r.id) {
		sentences = append(sentences, "You are on call.")
	}
	_, wokenAt := wokenOrder(audit)
	at, wokeYou := wokenAt[r.id]
	others := slices.DeleteFunc(slices.Clone(i.TargetMemberIDs), func(id string) bool { return id == r.id })
	if wokeYou && len(others) < len(i.TargetMemberIDs) {
		sentences = append(sentences, "MMHotline woke you at "+r.clock(at)+".")
		if len(others) > 0 {
			sentences = append(sentences, "MMHotline is also waking "+r.people(others)+".")
		}
	} else {
		sentences = append(sentences, "MMHotline is waking "+r.people(i.TargetMemberIDs)+".")
		if wokeYou {
			sentences = append(sentences, "MMHotline woke you at "+r.clock(at)+".")
		}
	}
	return strings.Join(sentences, " "), ""
}

// resolved says who resolved the incident, when, and how, from its history:
// a person, on the page or in a notification, or the alert source's resolve
// alert.
func (r incidentReader) resolved(i Incident, audit []IncidentAction) (string, string) {
	for index := len(audit) - 1; index >= 0; index-- {
		action := audit[index] //nolint:nilaway // index counts down from len(audit)-1 to 0, so audit is not nil.
		if action.Kind != "resolved" {
			continue
		}
		at := " at " + r.clock(action.At)
		if strings.HasPrefix(action.Actor, "source:") {
			return action.ActorName + " sent a resolve alert" + at + ", which resolved the incident.", ""
		}
		return r.actor(action) + " resolved the incident" + at + actionHow(action, "Resolved") + ".", r.note(action)
	}
	return "Resolved at " + r.clock(i.ResolvedAt) + ".", ""
}

// actionHow says where a person claimed or resolved an incident outside its
// page, as the history entry records it after "Claimed by NAME" or
// "Resolved by NAME" (notificationActionPlaces): " in Telegram", or " by
// acknowledging the alert in Pushover".
func actionHow(action IncidentAction, verb string) string {
	how, found := strings.CutPrefix(action.Message, verb+" by "+action.ActorName)
	if !found {
		return ""
	}
	return strings.Replace(how, ", who acknowledged", " by acknowledging", 1)
}

// lowerFirst writes a sentence's first letter small, to go on after a name.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// steps is "where it is": received, woken (or held for the triage agent),
// waiting for a claim or claimed, and resolved.
func (r incidentReader) steps(i Incident, audit []IncidentAction, step incidentNextStep) []incidentStep {
	steps := []incidentStep{{Label: "SOS received", When: r.clock(i.CreatedAt), State: "done"}}
	held := i.Status == "open" && i.Step == triageAgentStep
	order, wokenAt := wokenOrder(audit)
	switch {
	case held && heldForAServerReplacementIn(audit):
		steps = append(steps, incidentStep{Label: "The triage agent is replacing a server", When: "until " + r.clock(i.Deadline), State: "now"})
	case held:
		steps = append(steps, incidentStep{Label: "The triage agent looks first", When: "until " + r.clock(i.Deadline), State: "now"})
	case len(order) > 0:
		var woken []string
		for _, id := range order {
			woken = append(woken, capitalize(r.person(id))+" at "+r.clock(wokenAt[id]))
		}
		steps = append(steps, incidentStep{Label: "Woken", When: strings.Join(woken, ", "), State: "done"})
	default:
		steps = append(steps, incidentStep{Label: "Nobody woken", State: "done"})
	}
	switch {
	case held:
		steps = append(steps, incidentStep{Label: "Waiting for a claim"})
	case i.Status == "open":
		waiting := incidentStep{Label: "Waiting for a claim", State: "now"}
		if !step.At.IsZero() && len(step.Wakes) > 0 {
			waiting.When = "Wakes " + peopleText(step.Wakes, r.id) + " at " + r.clock(step.At)
		}
		steps = append(steps, waiting)
	case !i.ClaimedAt.IsZero():
		claimed := incidentStep{Label: "Claimed", When: "By " + r.person(i.OwnerMemberID) + " at " + r.clock(i.ClaimedAt), State: "now"}
		if i.Status == "resolved" {
			claimed.State = "done"
		}
		steps = append(steps, claimed)
	}
	if i.Status == "resolved" {
		return append(steps, incidentStep{Label: "Resolved", When: r.clock(i.ResolvedAt), State: "done"})
	}
	return append(steps, incidentStep{Label: "Resolved"})
}

// peoplePanel is the People panel.
func (r incidentReader) peoplePanel(i Incident, audit []IncidentAction, coverage Coverage, step incidentNextStep, members []escalateOption) incidentPeople {
	var panel incidentPeople
	everyone := func(everyoneAvailable bool, ids []string) string {
		switch {
		case everyoneAvailable:
			return "Everyone who is available"
		case len(ids) == 0:
			return "Nobody"
		}
		return capitalize(r.people(ids))
	}
	panel.OnCall = everyone(coverage.PrimaryIsEveryoneAvailable, coverage.PrimaryIDs)
	if !coverage.PrimaryIsEveryoneAvailable && len(coverage.BackupIDs) > 0 {
		panel.Backup = everyone(coverage.BackupIsEveryoneAvailable, coverage.BackupIDs)
	}
	order, wokenAt := wokenOrder(audit)
	var woken []string
	for _, id := range order {
		woken = append(woken, r.person(id)+" at "+r.clock(wokenAt[id]))
	}
	panel.Woken = "Nobody yet"
	if len(woken) > 0 {
		panel.Woken = capitalize(strings.Join(woken, ", "))
	}
	if i.Status == "open" && !step.At.IsZero() {
		panel.NextAt = step.At
		if len(step.Wakes) > 0 {
			panel.Next = capitalize(peopleText(step.Wakes, r.id))
		} else {
			panel.Next = capitalize(peopleText(step.WakesAgain, r.id)) + ", again"
		}
	}
	// Who cannot be woken now matters only while the incident may wake
	// someone, or be escalated.
	var skipped []string
	for _, m := range members {
		if i.Status != "resolved" && !m.UnavailableUntil.IsZero() {
			skipped = append(skipped, capitalize(r.person(m.ID))+", unavailable until "+r.clock(m.UnavailableUntil))
		}
	}
	panel.Skipped = strings.Join(skipped, "; ")
	return panel
}

// history is "What happened", oldest first: the incident's actions, the
// alerts about it, and its rounds of waking with how each reached people.
// Each line says what happened in plain words; the stored messages stay as
// the incidents command prints them. A round that wakes the same people
// again folds into the first round of its step, and MMHotline's own alert to
// the team about an escalation, which repeats the escalation's line, is
// shown only as a link on that line.
//
//nolint:nilaway // Each events[...] index comes from len(events)-1 or from firstRound, which holds only lines already appended.
func (r incidentReader) history(audit []IncidentAction, alerts []Alert, deliveries []Delivery) []incidentEvent {
	alertsByID := map[string]Alert{}
	for _, e := range alerts {
		alertsByID[e.ID] = e
	}
	alertTitle := func(action IncidentAction) string {
		if e, ok := alertsByID[action.RelatedAlertID]; ok {
			return e.Title + "."
		}
		return action.Message
	}
	var events []incidentEvent
	// firstRound is the line of each step's first round of waking, by the
	// incident's revision, which a new step changes.
	firstRound := map[int64]int{}
	// escalationLine is the line that says an escalation, by its time.
	referenced, escalationLine := map[string]bool{}, map[int64]int{}
	for index, action := range audit {
		referenced[action.RelatedAlertID] = true
		event := incidentEvent{At: action.At, AlertID: action.RelatedAlertID, Note: r.note(action), Text: action.Message}
		switch action.Kind {
		case "opened":
			event.Tone, event.Text = "sos", r.openedText(action)
		case "held_for_triage_agent":
			event.Tone, event.Text = "agent", "MMHotline held the incident for the triage agent to investigate first. MMHotline wakes nobody "+strings.TrimPrefix(action.Message, "Held for the triage agent ")+"."
		case "triage_agent_hold_ended":
			event.Tone, event.Text = "agent", "The hold for the triage agent ended. "+holdEndedReason(action.Message)+"."
		case "already_woken_for_triage_session", heldForAServerReplacement, runLimitSOSWokeKind:
			event.Tone = "agent"
		case "sos_escalation_due":
			if first, ok := firstRound[action.Revision]; ok {
				events[first].Again++
				events[first].LastAgain = action.At
				continue
			}
			reached, sentNothing := r.reached(action, deliveries)
			event.Tone, event.Text, event.Reached = "sos", r.wokeText(action, sentNothing), reached
			if index > 0 && audit[index-1].Kind == "escalated" && audit[index-1].Revision == action.Revision {
				if audit[index-1].Actor != "system" {
					// The person's line says whom they chose; it gets how
					// the round reached them, and the rounds after it.
					events[len(events)-1].Reached = event.Reached
					firstRound[action.Revision] = len(events) - 1
					continue
				}
				if strings.HasPrefix(event.Text, "MMHotline ") {
					event.Text = "Nobody had claimed the incident, so " + event.Text
				}
				escalationLine[audit[index-1].At.Unix()] = len(events)
			}
			firstRound[action.Revision] = len(events)
		case "could_not_wake":
			// "Could not wake Priya Natarajan: Priya Natarajan has no …",
			// naming the reader "you".
			who, has := r.people(action.TargetMemberIDs), " has"
			if who == "you" {
				has = " have"
			}
			event.Tone, event.Text = "warn", "MMHotline could not wake "+who+": "+who+has+" no contact method that wakes people for SOS alerts."
		case "escalated":
			// MMHotline's own escalation: the round of waking that
			// follows says whom it woke.
			if action.Actor == "system" {
				continue
			}
			escalationLine[action.At.Unix()] = len(events)
			event.Tone, event.Text = "person", r.actor(action)+" chose to wake "+r.people(action.TargetMemberIDs)+" now."
		case "claimed":
			event.Tone, event.Text = "person", r.actor(action)+" claimed the incident"+actionHow(action, "Claimed")+"."
		case "reclaimed":
			event.Tone, event.Text = "person", r.actor(action)+" took over the incident from "+r.person(action.PreviousOwnerMemberID)+"."
		case "resolved":
			event.Text = r.actor(action) + " resolved the incident" + actionHow(action, "Resolved") + "."
			switch {
			case strings.HasPrefix(action.Actor, "member:"):
				event.Tone = "person"
			case strings.HasPrefix(action.Actor, "source:"):
				event.Text = action.ActorName + " sent a resolve alert, which resolved the incident."
			}
		case "related_alert":
			event.Tone, event.Text = "sos", "Another SOS about the incident arrived and woke nobody new: “"+strings.TrimPrefix(action.Message, "Related alert: ")+"”."
		case "related_alert_sent_to_claimer":
			event.Tone, event.Text = "sos", "Another SOS about the incident arrived. MMHotline sent it to the person who claimed the incident: “"+strings.TrimPrefix(action.Message, "Related alert: ")+"”."
		case "triage_agent_report":
			event.Tone, event.Text = "agent", "The triage agent reported: "+reportSummary(action.Message)
		case "triage_agent_could_not_run", "playbook_run_refused_by_the_review_agent":
			event.Tone = "agent"
		case "triage_agent_stopped":
			event.Tone, event.Text = "person", r.actor(action)+" took over from the triage agent. The triage agent finishes the current step, reports the findings, and runs nothing more."
		case "playbook_run_approval_asked", "playbook_run_warning_sent":
			event.Tone, event.Text = "warn", alertTitle(action)
		case "playbook_run_approved", "playbook_run_refused", "playbook_run_stopped":
			// "Approved …", "Refused …: reason", "Stopped …": the
			// member's own words follow their name.
			event.Tone, event.Text = "person", r.actor(action)+" "+lowerFirst(action.Message)
		}
		events = append(events, event)
	}
	for _, e := range alerts {
		if referenced[e.ID] {
			continue
		}
		if line, ok := escalationLine[e.ReceivedAt.Unix()]; ok && e.SourceID == "system" && events[line].AlertID == "" {
			events[line].AlertID, events[line].AlertLabel = e.ID, "MMHotline's alert to the team"
			continue
		}
		events = append(events, incidentEvent{At: e.ReceivedAt, Tone: severityTone(e.Severity), Text: severityLabel(e.Severity) + " about the incident: “" + e.Title + "”.", AlertID: e.ID})
	}
	sort.SliceStable(events, func(a, b int) bool { return events[a].At.Before(events[b].At) })
	return events
}

// reportSummary is a triage agent report's first paragraph, as plain text.
func reportSummary(report string) string {
	if summary := firstProseParagraph(report); summary != "" {
		return summary
	}
	return "The triage agent's report has no text before the first list or heading. The whole report is in the triage agent's panel."
}

// openedText says how the SOS arrived, from the action that opened it.
func (r incidentReader) openedText(action IncidentAction) string {
	switch {
	case strings.HasPrefix(action.Actor, "source:"):
		return "The SOS arrived from " + action.ActorName + "."
	case strings.HasPrefix(action.Actor, "member:"):
		return r.actor(action) + " raised this SOS by hand."
	case action.Actor == "triage_agent":
		return "The triage agent raised this SOS with an urgent report."
	case action.Actor == playbookRunApprovalsActor:
		return "MMHotline raised this SOS to ask the team to approve a playbook run."
	}
	return "MMHotline raised this SOS."
}

// holdEndedReason is why the hold for the triage agent ended, as its history
// entry records it, without "waking the person on call", which the round of
// waking after it says.
// heldForAServerReplacementIn says whether a server replacement extended the
// wait of the incident whose history is audit.
func heldForAServerReplacementIn(audit []IncidentAction) bool {
	return slices.ContainsFunc(audit, func(action IncidentAction) bool { return action.Kind == heldForAServerReplacement })
}

func holdEndedReason(message string) string {
	reason := strings.TrimSuffix(message, "; waking the person on call")
	return strings.Replace(reason, "reported at sos", "sent an urgent report", 1)
}

// wokeText says whom a round of waking woke, and why them. The round's
// stored message, from incidentContactingText, says the same for the
// incidents command; the page names the reader "you".
// When the round sent nothing, as when someone claimed the incident first,
// MMHotline "was about to wake" them instead.
func (r incidentReader) wokeText(action IncidentAction, sentNothing bool) string {
	people := r.people(action.TargetMemberIDs)
	woke := "MMHotline woke "
	if sentNothing {
		woke = "MMHotline was about to wake "
	}
	switch {
	case len(action.TargetMemberIDs) == 0:
		// "Nobody to contact yet: the team has no members. …"
		return action.Message
	case strings.Contains(action.Message, "Nobody is available"):
		return woke + "every member, because nobody was available: " + people + "."
	case strings.HasPrefix(action.Message, "Contacting everyone available"):
		return woke + "everyone who is available: " + people + "."
	case strings.HasSuffix(action.Message, " (on call)"):
		return woke + people + " (on call)."
	case strings.HasSuffix(action.Message, " (backup)"):
		return woke + people + " (backup)."
	}
	return woke + people + "."
}

// reached says how a round of waking reached people: each notification of
// the round, with its outcome; and whether the round had notifications and
// sent none of them.
func (r incidentReader) reached(action IncidentAction, deliveries []Delivery) (string, bool) {
	var reached []string
	sentNothing := true
	// Deliveries come newest first.
	for index := len(deliveries) - 1; index >= 0; index-- {
		d := deliveries[index] //nolint:nilaway // index counts down from len(deliveries)-1 to 0, so deliveries is not nil.
		if d.Purpose != "sos_escalation" || d.IncidentRevision != action.Revision || !d.CreatedAt.Equal(action.At) {
			continue
		}
		how := contactMethodTypeName(d)
		if len(action.TargetMemberIDs) > 1 {
			how += " to " + r.person(d.MemberID)
		}
		outcome, _ := r.outcome(d)
		reached = append(reached, how+": "+lowerFirst(outcome))
		sentNothing = sentNothing && (d.State == "suppressed" || d.State == "cancelled")
	}
	return strings.Join(reached, "; "), sentNothing && len(reached) > 0
}

// deliveries lists the incident's notifications, oldest first.
func (r incidentReader) deliveries(deliveries []Delivery) []incidentDelivery {
	var rows []incidentDelivery
	for index := len(deliveries) - 1; index >= 0; index-- {
		d := deliveries[index]
		what := deliveryRecipient(d, r.id, r.names)
		switch d.Purpose {
		case "sos_escalation":
			what += ", waking for this SOS"
		case "subscribed_alert":
			what += ", the alert “" + d.Title + "”"
		case "system_notice":
			// A notice about no alert, such as a Pushover claim result, has
			// only a subject.
			name := d.Title
			if name == "" {
				name = d.Subject
			}
			what += ", MMHotline's notice “" + name + "”"
		}
		row := incidentDelivery{ID: d.ID, At: d.CreatedAt, What: what}
		row.Outcome, row.Reason = r.outcome(d)
		rows = append(rows, row)
	}
	return rows
}

// outcome says how a notification went, and why when it was not sent, in
// the team's time.
func (r incidentReader) outcome(d Delivery) (outcome, reason string) {
	reason = d.ReasonText()
	switch d.State {
	case "suppressed", "cancelled":
		return "Not sent", reason
	case "combined":
		if reason != "" {
			return reason, ""
		}
	case "failed", "uncertain":
		if !d.NextAttempt.IsZero() {
			label, _, _ := strings.Cut(d.OutcomeLabel(), ", will retry at ")
			return label + ", tries again at " + r.clock(d.NextAttempt), reason
		}
	}
	return d.OutcomeLabel(), reason
}

// contactMethodTypeName names a delivery's contact method type, such as
// "Pushover notification".
func contactMethodTypeName(d Delivery) string {
	if facts, ok := contactMethodTypeFacts(d.ContactMethodType); ok {
		return facts.CardTitle
	}
	return d.ContactMethodType
}

// severityTone is the dot of an alert's line in "What happened".
func severityTone(severity string) string {
	switch severity {
	case "sos", "warn":
		return severity
	}
	return ""
}

// viewIncidentApprovals splits the incident's approval requests and warned
// runs into those that wait for a person and the rest.
func viewIncidentApprovals(views []ApprovalRequestView) (open, decided []incidentApproval) {
	for _, v := range views {
		approval := incidentApproval{ApprovalRequestView: v, RunsAt: v.request.runsAt(), Changed: v.request.StateChangedAt, ApproveLabel: "Approve"}
		if len(v.countedApprovers())+1 >= v.ApprovalsNeeded {
			approval.ApproveLabel = "Approve and run"
		}
		why := []string{"The playbook run waits for a person because the rule is “" + v.RuleTitle + "”."}
		if v.Warned {
			why = []string{"The playbook run's rule is “" + v.RuleTitle + "”, so the playbook runs automatically unless someone stops the playbook run."}
		}
		if n := len(v.TasksOnServicesWhoseChangesAPersonMustApprove); n > 0 {
			why = append(why, countText(n, "task changes", "tasks change")+" a service whose changes a person must approve.")
		}
		if n := len(v.UnrecognizedTasks); n > 0 {
			why = append(why, "MMHotline can't check what "+countText(n, "task does", "tasks do")+".")
		}
		if v.Review != "" {
			why = append(why, cutAtWord(plainTitleText(v.Review), maximumReportSummaryRunes))
		}
		if v.AlsoApproves != "" {
			why = append(why, v.AlsoApproves)
		}
		approval.Why = strings.Join(why, " ")
		if v.Open() && (!v.Warned || v.Stoppable) {
			open = append(open, approval)
		} else {
			decided = append(decided, approval)
		}
	}
	return open, decided
}
