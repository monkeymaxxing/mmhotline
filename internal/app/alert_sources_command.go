package app

import (
	"context"
	"fmt"
	"time"
)

// AlertSourcesCommandRequest is what alert-sources list takes: nothing. The
// settings file's alert_sources part sets the alert sources.
type AlertSourcesCommandRequest struct{}
type CommandSigningSecret struct {
	SigningSecretID string    `json:"signing_secret_id"`
	CreatedAt       time.Time `json:"created_at"`
}
type CommandAlertSourceCheckInState struct {
	Expected                     bool                 `json:"expected"`
	LastCheckInAt                time.Time            `json:"last_check_in_at"`
	LastHeardAt                  time.Time            `json:"last_heard_at"`
	SentByNodeName               string               `json:"sent_by_node_name"`
	WarnAfterNoCheckInForMinutes int                  `json:"warn_after_no_check_in_for_minutes"`
	SOSAfterNoCheckInForMinutes  int                  `json:"sos_after_no_check_in_for_minutes"`
	NodesInWholeNodeMaintenance  []NodeInMaintenance  `json:"nodes_in_whole_node_maintenance"`
	PlannedDowntime              *PlannedDowntimeView `json:"planned_downtime"`
}
type CommandAlertSource struct {
	IntakeURL      string                         `json:"intake_url"`
	CheckInURL     string                         `json:"check_in_url"`
	AlertSourceID  string                         `json:"alert_source_id"`
	Name           string                         `json:"name"`
	Active         bool                           `json:"active"`
	SigningSecrets []CommandSigningSecret         `json:"signing_secrets"`
	CheckInState   CommandAlertSourceCheckInState `json:"check_in_state"`
}

func (a *App) alertSourcesCommand(ctx context.Context, p Principal, action string, _ AlertSourcesCommandRequest, result *CommandResult) error {
	if action != "list" {
		return ErrNotFound
	}
	sources, err := a.Sources(ctx, p)
	if err != nil {
		return err
	}
	result.AlertSources = []CommandAlertSource{}
	for _, source := range sources {
		checkIns, err := a.AlertSourceCheckInStatus(ctx, p, source.ID)
		if err != nil {
			return err
		}
		out := CommandAlertSource{IntakeURL: result.AlertIntakeAddress + "/alert-intake/" + source.ID, CheckInURL: result.AlertIntakeAddress + "/check-in/" + source.ID, AlertSourceID: source.ID, Name: source.Name, Active: source.Active, SigningSecrets: []CommandSigningSecret{}, CheckInState: CommandAlertSourceCheckInState{Expected: checkIns.Expected, LastCheckInAt: checkIns.LastCheckInAt, LastHeardAt: checkIns.LastHeardAt, SentByNodeName: checkIns.SentByNodeName, WarnAfterNoCheckInForMinutes: int(checkIns.WarnAfterNoCheckInFor / time.Minute), SOSAfterNoCheckInForMinutes: int(checkIns.SOSAfterNoCheckInFor / time.Minute), NodesInWholeNodeMaintenance: append([]NodeInMaintenance{}, checkIns.NodesInWholeNodeMaintenance...), PlannedDowntime: checkIns.PlannedDowntime}}
		for _, secret := range source.Secrets {
			out.SigningSecrets = append(out.SigningSecrets, CommandSigningSecret{SigningSecretID: secret.ID, CreatedAt: secret.CreatedAt})
		}
		result.AlertSources = append(result.AlertSources, out)
	}
	result.Message = fmt.Sprintf("Listed %s. The alert sources are set in %s, in alert_sources.", countText(len(result.AlertSources), "alert source", "alert sources"), a.SettingsFilePath())
	return nil
}
