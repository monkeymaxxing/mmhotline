package app

import (
	"context"
	"errors"
	"fmt"
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
