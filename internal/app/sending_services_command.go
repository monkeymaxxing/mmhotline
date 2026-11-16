package app

import (
	"context"
	"fmt"
)

func (a *App) sendingServicesCommand(ctx context.Context, p Principal, action string, result *CommandResult) error {
	if action != "list" {
		return ErrNotFound
	}
	setups, err := a.SendingServiceSetups(ctx, p)
	if err != nil {
		return err
	}
	result.SendingServiceSetups = setups
	result.Message = fmt.Sprintf("Listed %s, set in %s, in sending_services.", countText(len(setups), "sending service", "sending services"), a.SettingsFilePath())
	return nil
}
