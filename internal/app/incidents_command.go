package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// IncidentsCommandRequest is the input of `mmhotline incidents list` and
// `show`. A list with OpenedSince or OpenedBefore lists the incidents opened
// in that span, resolved or not; without them, the incidents not resolved.
type IncidentsCommandRequest struct {
	IncidentID   string    `json:"incident_id,omitempty"`
	OpenedSince  time.Time `json:"opened_since,omitzero"`
	OpenedBefore time.Time `json:"opened_before,omitzero"`
	// SignInEmailAddress is send-test-sos's one person to test.
	SignInEmailAddress string `json:"sign_in_email_address,omitempty"`
}

// CommandIncident is an SOS incident as the command prints it, with the
// facts the dashboard and the incident page show. Alerts, History, and
// TriageSessionID are filled by show only.
type CommandIncident struct {
	IncidentID string `json:"incident_id"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	// Status is open (not claimed yet), claimed, or resolved.
	Status     string    `json:"status"`
	OpenedAt   time.Time `json:"opened_at"`
	ClaimedBy  string    `json:"claimed_by,omitempty"`
	ClaimedAt  time.Time `json:"claimed_at,omitzero"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
	// EscalationStep is primary, backup, everyone_available, only_member,
	// chosen_person, or triage_agent.
	EscalationStep     string `json:"escalation_step"`
	HeldForTriageAgent bool   `json:"held_for_triage_agent"`
	// BeingContactedNow and NextPersonContactedAt are set while the incident
	// is open, as the dashboard shows them.
	BeingContactedNow     string                        `json:"being_contacted_now,omitempty"`
	NextPersonContactedAt time.Time                     `json:"next_person_contacted_at,omitzero"`
	OpeningAlertID        string                        `json:"opening_alert_id"`
	AlertSourceID         string                        `json:"alert_source_id"`
	SenderIncidentKey     string                        `json:"sender_incident_key,omitempty"`
	PageURL               string                        `json:"page_url"`
	TriageSessionID       string                        `json:"triage_session_id,omitempty"`
	Alerts                []CommandAlert                `json:"alerts,omitzero"`
	History               []CommandIncidentHistoryEntry `json:"history,omitzero"`
}

// CommandIncidentHistoryEntry is one entry of the incident page's Action
// history.
type CommandIncidentHistoryEntry struct {
	Kind           string    `json:"kind"`
	Label          string    `json:"label"`
	Message        string    `json:"message"`
	By             string    `json:"by"`
	OccurredAt     time.Time `json:"occurred_at"`
	RelatedAlertID string    `json:"related_alert_id,omitempty"`
	// Note is what the person wrote with a claim or a resolve on the
	// incident page; absent when they wrote none.
	Note string `json:"note,omitempty"`
}

// maximumIncidentAlertsShown is as many alerts as one ListAlerts read gives:
// show prints the incident's newest ones, and `alerts list --incident-id`
// pages through the rest.
const maximumIncidentAlertsShown = 100

func (a *App) incidentsCommand(ctx context.Context, p Principal, action string, request IncidentsCommandRequest, result *CommandResult) error {
	if action == "send-test-sos" {
		return a.sendTestSOSCommand(ctx, p, request.SignInEmailAddress, result)
	}
	if action != "list" && action != "show" {
		return ErrNotFound
	}
	members, err := a.membersForHistory(ctx, p)
	if err != nil {
		return err
	}
	names := memberNames(members)
	if action == "list" {
		var incidents []Incident
		span := !request.OpenedSince.IsZero() || !request.OpenedBefore.IsZero()
		if span {
			incidents, err = a.ListIncidentsOpened(ctx, p, request.OpenedSince, request.OpenedBefore)
		} else {
			incidents, err = a.ListIncidents(ctx, p)
		}
		if err != nil {
			return err
		}
		result.Incidents = []CommandIncident{}
		for _, i := range incidents {
			result.Incidents = append(result.Incidents, a.commandIncident(i, names))
		}
		listed := countText(len(incidents), "SOS incident", "SOS incidents")
		result.Message = "Listed " + listed + " not resolved, newest first."
		if span {
			result.Message = fmt.Sprintf("Listed %s opened %s, resolved or not, newest first.", listed, spanText(request.OpenedSince, request.OpenedBefore))
			if len(incidents) == maxUnresolvedIncidents {
				result.Message += fmt.Sprintf(" The list shows only the newest %d SOS incidents. To find older incidents, narrow the time range.", maxUnresolvedIncidents)
			}
		}
		return nil
	}
	if request.IncidentID == "" {
		return fmt.Errorf("%w: --incident-id is required", ErrInvalid)
	}
	i, err := a.GetIncident(ctx, p, request.IncidentID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: no SOS incident has ID %s", ErrNotFound, request.IncidentID)
	}
	if err != nil {
		return err
	}
	history, err := a.IncidentAudit(ctx, p, i.ID)
	if err != nil {
		return err
	}
	alerts, err := a.ListAlerts(ctx, p, AlertFilter{IncidentID: i.ID, Limit: maximumIncidentAlertsShown})
	if err != nil {
		return err
	}
	session, err := a.IncidentTriageSession(ctx, p, i.ID)
	if err != nil {
		return err
	}
	incident := a.commandIncident(i, names)
	incident.Alerts = a.commandAlerts(alerts)
	incident.History = []CommandIncidentHistoryEntry{}
	for _, action := range history {
		incident.History = append(incident.History, CommandIncidentHistoryEntry{Kind: action.Kind, Label: incidentActionLabel(action.Kind), Message: action.Message, By: action.ActorName, OccurredAt: action.At, RelatedAlertID: action.RelatedAlertID, Note: action.Note})
	}
	deliveries, err := a.IncidentDeliveries(ctx, p, i.ID)
	if err != nil {
		return err
	}
	for _, d := range deliveries {
		if d.Reason == pushoverAcknowledgementFromAnotherUserReason {
			name := personNameOrThePerson(names, d.MemberID)
			incident.History = append(incident.History, CommandIncidentHistoryEntry{Kind: "pushover_acknowledgement_did_not_claim", Label: "Pushover acknowledgement did not claim", Message: pushoverAcknowledgementDidNotClaimMessage(name), By: name, OccurredAt: d.CreatedAt})
		}
	}
	sort.SliceStable(incident.History, func(a, b int) bool { return incident.History[a].OccurredAt.Before(incident.History[b].OccurredAt) })
	if session != nil {
		incident.TriageSessionID = session.SessionID
	}
	result.Incident = &incident
	if len(alerts) == maximumIncidentAlertsShown {
		result.Message = fmt.Sprintf("The list shows the incident's newest %d alerts. To list older alerts: mmhotline alerts list --incident-id %s --older-than-alert-id %s", maximumIncidentAlertsShown, i.ID, alerts[len(alerts)-1].ID)
	}
	return nil
}

// commandIncident is i with the names and times the dashboard shows.
func (a *App) commandIncident(i Incident, names map[string]string) CommandIncident {
	view := viewIncident(i, names)
	incident := CommandIncident{IncidentID: i.ID, Title: i.Title, Message: i.Message, Status: i.Status, OpenedAt: i.CreatedAt, ClaimedAt: i.ClaimedAt, ResolvedAt: i.ResolvedAt, EscalationStep: i.Step, HeldForTriageAgent: view.HeldForTriageAgent, OpeningAlertID: i.OpeningAlertID, AlertSourceID: i.SourceID, SenderIncidentKey: i.SenderIncidentKey, PageURL: a.WebURL + "/incidents/" + i.ID}
	if i.OwnerMemberID != "" {
		incident.ClaimedBy = view.Owner
	}
	if i.Status == "open" {
		incident.BeingContactedNow, incident.NextPersonContactedAt = view.Targets, i.Deadline
	}
	return incident
}

// countText says a count with its noun, such as "1 alert" or "2 alerts".
func countText(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, many)
}

// spanText says a time span of a list, such as "since
// 2026-10-28T09:00:00Z and before 2026-10-28T12:00:00Z".
func spanText(since, before time.Time) string {
	var parts []string
	if !since.IsZero() {
		parts = append(parts, "since "+commandTime(since))
	}
	if !before.IsZero() {
		parts = append(parts, "before "+commandTime(before))
	}
	return strings.Join(parts, " and ")
}

// Text is the incident's lines; the history and alerts follow for show.
func (i CommandIncident) Text() string {
	lines := []string{fmt.Sprintf("SOS incident %s: %s", i.IncidentID, i.Title), fmt.Sprintf("Status: %s. Opened at %s.", incidentStatusLabel(i.Status), commandTime(i.OpenedAt))}
	if i.ClaimedBy != "" {
		lines = append(lines, fmt.Sprintf("Claimed by %s at %s.", i.ClaimedBy, commandTime(i.ClaimedAt)))
	}
	if i.Status == "resolved" {
		lines = append(lines, "Resolved at "+commandTime(i.ResolvedAt)+".")
	}
	if i.Status == "open" {
		line := "Being contacted now: " + i.BeingContactedNow + "."
		switch {
		case i.HeldForTriageAgent:
			line += " MMHotline wakes the person on call at " + commandTime(i.NextPersonContactedAt) + " unless someone claims or resolves this incident first."
		case !i.NextPersonContactedAt.IsZero():
			line += " If nobody claims the incident by " + commandTime(i.NextPersonContactedAt) + ", MMHotline contacts the next person."
		}
		lines = append(lines, line)
	}
	lines = append(lines, "Page: "+i.PageURL)
	if i.History == nil {
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "Message: "+indentedText(i.Message, "  "))
	if i.TriageSessionID != "" {
		lines = append(lines, "Triage session: "+i.TriageSessionID)
	}
	lines = append(lines, "Action history:")
	for _, entry := range i.History {
		text := indentedText(strings.TrimSuffix(entry.Message, ".")+".", "    ")
		if note := incidentNoteText(entry.By, entry.Note); note != "" {
			text += " " + note
		}
		lines = append(lines, fmt.Sprintf("  %s %s: %s By: %s.", commandTime(entry.OccurredAt), entry.Label, text, entry.By))
	}
	lines = append(lines, "Related alerts, newest first:")
	for _, alert := range i.Alerts {
		lines = append(lines, "  "+alert.Line())
	}
	return strings.Join(lines, "\n")
}

// indentedText indents the later lines of a text that has several, so they
// stay under the line it starts.
func indentedText(text, indent string) string {
	return strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n"+indent)
}

// testSOSTitle and testSOSMessage are what the person reads on the phone for
// the test SOS. The title says the whole story, because a phone shows little
// more than the title when it wakes someone.
const (
	testSOSTitle   = "Test of the alerts. Nothing is wrong: acknowledge this."
	testSOSMessage = "MMHotline sent this test on purpose. Nothing is wrong. Acknowledge this message, and MMHotline closes the test for you."
)

// CommandTestSOS is what `mmhotline incidents send-test-sos` prints. The
// server fills IncidentID and IncidentPageURL when it opens the test; the
// command fills the rest while it waits for the claim.
type CommandTestSOS struct {
	IncidentID      string `json:"incident_id"`
	IncidentPageURL string `json:"incident_page_url"`
	// SentOnlyToName and SentOnlyToSignInEmailAddress are set when the test
	// went to one chosen person (--sign-in-email-address) instead of the
	// person on call.
	SentOnlyToName               string `json:"sent_only_to_name,omitempty"`
	SentOnlyToSignInEmailAddress string `json:"sent_only_to_sign_in_email_address,omitempty"`
	// WaitedForClaimMinutes is the time limit the command waited with.
	WaitedForClaimMinutes int `json:"waited_for_claim_minutes,omitempty"`
	// Claimed is true when a person claimed the test before the time limit.
	Claimed bool `json:"claimed"`
	// ClaimedBy, ClaimedHow, and ClaimedAt say who claimed it, how (such as
	// "by acknowledging the alert in Pushover"; empty for the incident page),
	// and when.
	ClaimedBy  string    `json:"claimed_by,omitempty"`
	ClaimedHow string    `json:"claimed_how,omitempty"`
	ClaimedAt  time.Time `json:"claimed_at,omitzero"`
	// ResolvedAt is when MMHotline closed the test after the claim.
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}

// sendTestSOSCommand opens the test SOS and returns at once; the command
// waits for the claim, as `triage-agent check-sign-in` waits for its check.
func (a *App) sendTestSOSCommand(ctx context.Context, p Principal, signInEmailAddress string, result *CommandResult) error {
	incident, person, err := a.SendTestSOS(ctx, p, signInEmailAddress)
	if err != nil {
		return err
	}
	result.TestSOS = &CommandTestSOS{IncidentID: incident.ID, IncidentPageURL: a.WebURL + "/incidents/" + incident.ID}
	if person.ID != "" {
		result.TestSOS.SentOnlyToName, result.TestSOS.SentOnlyToSignInEmailAddress = person.Name, person.SignInEmailAddress
	}
	return nil
}

// SendTestSOS opens an SOS incident from MMHotline itself, marked as a test
// by its alert source ID (testAlertSourceID). It wakes the person on call as
// a real SOS does, through the same escalation, and skips the triage agent.
// A test SOS that nobody claimed is closed first, so a second test never
// leaves two tests ringing a phone. Claiming the test resolves it
// (claimIncidentTx).
//
// With a sign-in email address, the test goes to that one person now, whether
// or not they are on call, and to nobody else: no backup step and no
// escalation. The person is the second result.
func (a *App) SendTestSOS(ctx context.Context, p Principal, signInEmailAddress string) (Incident, Member, error) {
	var opened Incident
	var person Member
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var recipients []string
		if signInEmailAddress != "" {
			normalized, err := normalizeSignInEmailAddress(signInEmailAddress)
			if err != nil {
				return err
			}
			if person, err = personBySignInEmailAddress(ctx, tx, normalized); err != nil {
				return err
			}
			if eligible, reason, err := MemberEligible(ctx, tx, person.ID, a.Now().UTC()); err != nil || !eligible {
				if err == nil {
					err = fmt.Errorf("%w: %s cannot receive the test SOS now: %s", ErrInvalid, person.Name, reason)
				}
				return err
			}
			if wakeable, _, err := wakeableMembers(ctx, tx, []string{person.ID}); err != nil || len(wakeable) == 0 {
				if err == nil {
					err = fmt.Errorf("%w: %s has no contact method that wakes people for SOS alerts; add a Pushover notification or a phone number for them first", ErrInvalid, person.Name)
				}
				return err
			}
			recipients = []string{person.ID}
		}
		at := a.Now().UTC()
		rows, err := tx.QueryContext(ctx, `SELECT id FROM incidents WHERE source_id=? AND status!='resolved'`, testAlertSourceID)
		if err != nil {
			return err
		}
		var earlier []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			earlier = append(earlier, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range earlier {
			i, err := readIncident(ctx, tx, id)
			if err != nil {
				return err
			}
			if _, err = resolveIncidentTx(ctx, tx, i, "system", "", " because a newer test replaced it", "", at); err != nil {
				return err
			}
		}
		alert, err := admitAlert(ctx, tx, testAlertSourceID, "system", "local:"+NewID(), "", GenericAlert{Severity: "sos", Title: testSOSTitle, Message: testSOSMessage}, recipients, at)
		if err != nil {
			return err
		}
		opened, err = readIncident(ctx, tx, alert.IncidentID)
		return err
	})
	return opened, person, err
}

// Update fills the claim facts from the test incident as `incidents show`
// reports it, and says whether the test was claimed. The claim's history
// entry says how: " in Telegram", or ", who acknowledged the alert in
// Pushover".
func (t *CommandTestSOS) Update(i CommandIncident) bool {
	t.ClaimedBy, t.ClaimedHow, t.ClaimedAt, t.ResolvedAt = "", "", time.Time{}, time.Time{}
	for _, entry := range i.History {
		if entry.Kind != "claimed" {
			continue
		}
		t.ClaimedBy, t.ClaimedAt = i.ClaimedBy, entry.OccurredAt
		how := strings.TrimPrefix(entry.Message, "Claimed by "+entry.By)
		t.ClaimedHow = strings.TrimPrefix(strings.Replace(how, ", who acknowledged", " by acknowledging", 1), " ")
		if i.Status == "resolved" {
			t.ResolvedAt = i.ResolvedAt
		}
	}
	t.Claimed = t.ClaimedBy != ""
	return t.Claimed
}

// Text is the test's lines: the incident's page, and who claimed the test,
// how, and when.
func (t CommandTestSOS) Text() string {
	lines := []string{"Incident: " + t.IncidentPageURL}
	if !t.Claimed {
		return strings.Join(lines, "\n")
	}
	how := ""
	if t.ClaimedHow != "" {
		how = " " + t.ClaimedHow
	}
	lines = append(lines, fmt.Sprintf("%s claimed the test%s, at %s.", t.ClaimedBy, how, commandTime(t.ClaimedAt)))
	if !t.ResolvedAt.IsZero() {
		lines = append(lines, "MMHotline resolved the test after the claim, at "+commandTime(t.ResolvedAt)+".")
	}
	return strings.Join(lines, "\n")
}

// personNameOrThePerson is the member's name, or "The person" when the
// member is no longer on the team.
func personNameOrThePerson(names map[string]string, memberID string) string {
	if name := names[memberID]; memberID != "" && name != "" {
		return name
	}
	return "The person"
}

// pushoverAcknowledgementDidNotClaimMessage says why an acknowledgement in
// Pushover left the incident unclaimed. The history shows it where the claim
// would have been.
func pushoverAcknowledgementDidNotClaimMessage(name string) string {
	return fmt.Sprintf("%[1]s's phone acknowledged, but the acknowledgement came from another Pushover user, so it did not claim the incident. The contact method uses a key that isn't %[1]s's own Pushover user key, such as a group key.", name)
}
