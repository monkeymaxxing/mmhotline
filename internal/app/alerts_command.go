package app

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AlertsCommandRequest is the input of `mmhotline alerts list`. Every field
// narrows the list; OlderThanAlertID continues a list that was cut.
type AlertsCommandRequest struct {
	Severity         string    `json:"severity,omitempty"`
	AlertSourceID    string    `json:"alert_source_id,omitempty"`
	IncidentID       string    `json:"incident_id,omitempty"`
	ReceivedSince    time.Time `json:"received_since,omitzero"`
	ReceivedBefore   time.Time `json:"received_before,omitzero"`
	OlderThanAlertID string    `json:"older_than_alert_id,omitempty"`
}

// CommandAlert is an alert as the command prints it, with the facts of its
// page. A warn or info alert has no incident.
type CommandAlert struct {
	AlertID           string    `json:"alert_id"`
	Severity          string    `json:"severity"`
	Title             string    `json:"title"`
	Message           string    `json:"message"`
	Notes             string    `json:"notes,omitempty"`
	ReceivedAt        time.Time `json:"received_at"`
	HappenedAt        time.Time `json:"happened_at,omitzero"`
	AlertSourceID     string    `json:"alert_source_id"`
	IncidentID        string    `json:"incident_id,omitempty"`
	SenderAlertID     string    `json:"sender_alert_id,omitempty"`
	SenderIncidentKey string    `json:"sender_incident_key,omitempty"`
	// RepeatCount and LastRepeatTitle are the later warns folded into this
	// one; absent when none were.
	RepeatCount     int64  `json:"repeat_count,omitempty"`
	LastRepeatTitle string `json:"last_repeat_title,omitempty"`
	PageURL         string `json:"page_url"`
}

// maximumAlertsListed is as many alerts as one ListAlerts read gives, which
// holds the database no longer than the Alert history page does.
const maximumAlertsListed = 100

func (a *App) alertsCommand(ctx context.Context, p Principal, action string, request AlertsCommandRequest, result *CommandResult) error {
	if action != "list" {
		return ErrNotFound
	}
	filter := AlertFilter{Severity: request.Severity, SourceID: request.AlertSourceID, IncidentID: request.IncidentID, From: request.ReceivedSince, Until: request.ReceivedBefore, Limit: maximumAlertsListed}
	if request.OlderThanAlertID != "" {
		older, err := a.GetAlert(ctx, p, request.OlderThanAlertID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: no alert has ID %s", ErrNotFound, request.OlderThanAlertID)
		}
		if err != nil {
			return err
		}
		filter.Before = older.Sequence
	}
	alerts, err := a.ListAlerts(ctx, p, filter)
	if err != nil {
		return err
	}
	result.Alerts = a.commandAlerts(alerts)
	result.Message = "Listed " + countText(len(alerts), "alert", "alerts") + ", newest first."
	if len(alerts) == maximumAlertsListed {
		result.Message += fmt.Sprintf(" Older alerts may also match the filters. To list older alerts, repeat the command with --older-than-alert-id %s.", alerts[len(alerts)-1].ID)
	}
	return nil
}

func (a *App) commandAlerts(alerts []Alert) []CommandAlert {
	out := []CommandAlert{}
	for _, alert := range alerts {
		out = append(out, a.commandAlert(alert))
	}
	return out
}

func (a *App) commandAlert(alert Alert) CommandAlert {
	return CommandAlert{AlertID: alert.ID, Severity: alert.Severity, Title: alert.Title, Message: alert.Message, Notes: alert.Notes, ReceivedAt: alert.ReceivedAt, HappenedAt: alert.HappenedAt, AlertSourceID: alert.SourceID, IncidentID: alert.IncidentID, SenderAlertID: alert.SenderAlertID, SenderIncidentKey: alert.SenderIncidentKey, RepeatCount: alert.RepeatCount, LastRepeatTitle: alert.LastRepeatTitle, PageURL: a.WebURL + "/alerts/" + alert.ID}
}

// Line is the alert in one line, as lists print it.
func (alert CommandAlert) Line() string {
	line := fmt.Sprintf("%s %s %s: %s", commandTime(alert.ReceivedAt), severityLabel(alert.Severity), alert.AlertID, alert.Title)
	if alert.IncidentID != "" {
		line += " (SOS incident " + alert.IncidentID + ")"
	}
	return line
}

// Text is the alert's line with its message, its alert source, and its page.
func (alert CommandAlert) Text() string {
	text := alert.Line() + "\n  Alert source: " + alert.AlertSourceID + ". Page: " + alert.PageURL + "\n  " + indentedText(alert.Message, "  ")
	if alert.Notes != "" {
		text += "\n  Note: " + alert.Notes
	}
	if repeats := repeatsSentence(alert.RepeatCount, alert.LastRepeatTitle); repeats != "" {
		text += "\n  " + repeats
	}
	if gap := happenedGap(alert.HappenedAt, alert.ReceivedAt); gap != "" {
		text += "\n  " + gap
	}
	return text
}
