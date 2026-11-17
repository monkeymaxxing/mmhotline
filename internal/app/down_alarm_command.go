package app

import (
	"context"
	"fmt"
)

func (a *App) downAlarmCommand(ctx context.Context, p Principal, action string, result *CommandResult) error {
	var view DownAlarmView
	var err error
	switch action {
	case "show":
		view, err = a.DownAlarm(ctx, p)
	case "send-test":
		view, err = a.SendDownAlarmTest(ctx, p)
		result.Message = DownAlarmTestSummary(view.TestSends)
	default:
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	result.DownAlarm = &view
	return nil
}

// DownAlarmTestFailed is true when a test would leave someone unwoken: a
// service did not accept a test, or the alarm holds nothing at all. The
// command then exits 1.
func DownAlarmTestFailed(sends []DownAlarmSendView) bool {
	if len(sends) == 0 {
		return true
	}
	for _, send := range sends {
		if send.SendOutcome != "delivered" || downAlarmReceiptFailed(send) {
			return true
		}
	}
	return false
}

// downAlarmReceiptFailed is true when Pushover's receipt did not prove a
// Pushover test: no receipt, a refused receipt, or a cancel that failed.
func downAlarmReceiptFailed(send DownAlarmSendView) bool {
	return send.PushoverReceiptOutcome != "" && send.PushoverReceiptOutcome != pushoverReceiptProven
}

// DownAlarmTestSummary is the last line of down-alarm send-test.
func DownAlarmTestSummary(sends []DownAlarmSendView) string {
	if len(sends) == 0 {
		return "The MMHotline-down alarm holds no message, so it would wake nobody."
	}
	refused := 0
	for _, send := range sends {
		if send.SendOutcome != "delivered" {
			refused++
		}
	}
	unproven := 0
	for _, send := range sends {
		if downAlarmReceiptFailed(send) {
			unproven++
		}
	}
	switch {
	case refused == 0 && unproven > 0:
		return fmt.Sprintf("The MMHotline-down alarm sent a test alert for each held message, and each service accepted it, but Pushover's receipt did not prove %d of the Pushover notifications. Read the lines above for what to check.", unproven)
	case refused == 0:
		return "The MMHotline-down alarm sent a test alert for each held message, and each service accepted it."
	case refused == 1:
		return fmt.Sprintf("The MMHotline-down alarm sent a test alert for each held message; 1 of %d was not accepted, so that person would not be woken. Check the contact method with sudo mmhotline contact-methods send-test.", len(sends))
	default:
		return fmt.Sprintf("The MMHotline-down alarm sent a test alert for each held message; %d of %d were not accepted, so those people would not be woken. Check the contact methods with sudo mmhotline contact-methods send-test.", refused, len(sends))
	}
}
