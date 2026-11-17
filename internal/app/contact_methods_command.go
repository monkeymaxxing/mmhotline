package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ContactMethodsCommandRequest struct {
	PushoverUserKey             string    `json:"pushover_user_key,omitempty"`
	SignInEmailAddress          string    `json:"sign_in_email_address,omitempty"`
	SharedWithTeam              bool      `json:"shared_with_team,omitempty"`
	ContactMethodID             string    `json:"contact_method_id,omitempty"`
	ContactMethodType           string    `json:"contact_method_type,omitempty"`
	EmailAddress                string    `json:"email_address,omitempty"`
	OutgoingWebhookURL          string    `json:"outgoing_webhook_url,omitempty"`
	OutgoingWebhookBearerToken  string    `json:"outgoing_webhook_bearer_token,omitempty"`
	PhoneNumberToCall           string    `json:"phone_number_to_call,omitempty"`
	AlertSeverities             *[]string `json:"alert_severities,omitempty"`
	SendWhileUnavailableOrMuted *bool     `json:"send_while_unavailable_or_muted,omitempty"`
}
type CommandLastTestMessage struct {
	DeliveryState            string    `json:"delivery_state"`
	DeliveryStateExplanation string    `json:"delivery_state_explanation"`
	QueuedAt                 time.Time `json:"queued_at"`
}
type CommandContactMethod struct {
	ContactMethodID             string                  `json:"contact_method_id"`
	SignInEmailAddress          string                  `json:"sign_in_email_address"`
	SharedWithTeam              bool                    `json:"shared_with_team"`
	ContactMethodType           string                  `json:"contact_method_type"`
	SendsTo                     string                  `json:"sends_to"`
	AlertSeverities             []string                `json:"alert_severities"`
	SendWhileUnavailableOrMuted bool                    `json:"send_while_unavailable_or_muted"`
	LastTestMessage             *CommandLastTestMessage `json:"last_test_message"`
	// PushoverAcknowledgementProblem is set when an acknowledgement in Pushover
	// came from another Pushover user, so it did not claim an incident. It says
	// the problem and the fix.
	PushoverAcknowledgementProblem string `json:"pushover_acknowledgement_problem,omitempty"`
}

func (a *App) contactMethodsCommand(ctx context.Context, p Principal, action string, request ContactMethodsCommandRequest, result *CommandResult) error {
	if request.SharedWithTeam && request.SignInEmailAddress != "" {
		return fmt.Errorf("%w: use either --sign-in-email-address or --shared-with-team", ErrInvalid)
	}
	members, err := a.TeamMembers(ctx, p)
	if err != nil {
		return err
	}
	signInEmailAddresses := map[string]string{}
	for _, m := range members {
		signInEmailAddresses[m.ID] = m.SignInEmailAddress
	}
	methods, err := a.ContactMethods(ctx, p)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, m := range members {
		names[m.ID] = m.Name
	}
	var selected ContactMethod
	for _, c := range methods {
		if c.ID == request.ContactMethodID {
			selected = c
		}
	}
	switch action {
	case "list":
		if request.SignInEmailAddress != "" {
			found := false
			for _, m := range members {
				if strings.EqualFold(m.SignInEmailAddress, request.SignInEmailAddress) {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%w: no member has sign-in email address %s", ErrNotFound, request.SignInEmailAddress)
			}
		}
	case "add":
		if request.ContactMethodType == "telegram_message" || request.ContactMethodType == "slack_message" {
			return fmt.Errorf("%w: the person connects %s from the Contact methods page: %s/contact-methods", ErrInvalid, request.ContactMethodType, a.WebURL)
		}
		if request.ContactMethodType != "email" && request.ContactMethodType != "outgoing_webhook" && request.ContactMethodType != "automated_voice_call" && request.ContactMethodType != "pushover_notification" {
			return fmt.Errorf("%w: --contact-method-type must be email, outgoing_webhook, automated_voice_call, or pushover_notification", ErrInvalid)
		}
		if !request.SharedWithTeam && request.SignInEmailAddress == "" {
			return fmt.Errorf("%w: --sign-in-email-address or --shared-with-team is required", ErrInvalid)
		}
		if request.AlertSeverities == nil && request.ContactMethodType == "pushover_notification" {
			values := pushoverDefaultSubscriptions()
			request.AlertSeverities = &values
		}
		if request.AlertSeverities == nil {
			return fmt.Errorf("%w: --alert-severities is required", ErrInvalid)
		}
		in := ContactMethodInput{ContactMethodType: request.ContactMethodType, Shared: request.SharedWithTeam, OwnerSignInEmailAddress: request.SignInEmailAddress, Subscriptions: *request.AlertSeverities, Fields: map[string]string{"email_address": request.EmailAddress, "outgoing_webhook_url": request.OutgoingWebhookURL, "outgoing_webhook_bearer_token": request.OutgoingWebhookBearerToken, "phone_number_to_call": request.PhoneNumberToCall, "pushover_user_key": request.PushoverUserKey}}
		if request.SendWhileUnavailableOrMuted != nil {
			in.SendWhileUnavailableOrMuted = *request.SendWhileUnavailableOrMuted
		}
		selected, err = a.SaveContactMethod(ctx, p, in)
		if err != nil {
			return err
		}
		if shown := webhookSecretShownOnce(selected); shown != nil {
			result.OutgoingWebhookSigningSecretShownOnce = shown.SecretHex
			result.OutgoingWebhookSigningSecretID = shown.SigningSecretID
		}
		result.Message = "Contact method saved. A test message for the contact method is queued. Inspect the last test result to check delivery."
		if selected.TestNotSentReason != "" {
			result.Message = "Contact method saved. " + selected.TestNotSentReason
		}
		if selected.PushoverGroupKey && selected.MemberID != "" {
			result.PushoverGroupKeyWarning = pushoverGroupKeyWarning(names[selected.MemberID])
			result.Message += "\n\n" + result.PushoverGroupKeyWarning + "\n"
		}
	case "change", "remove", "send-test":
		if selected.ID == "" {
			return fmt.Errorf("%w: no contact method has ID %s", ErrNotFound, request.ContactMethodID)
		}
		switch action {
		case "remove":
			err = a.RemoveContactMethod(ctx, p, selected.ID, selected.Revision)
			result.Message = "Contact method removed."
		case "send-test":
			err = a.SendContactMethodTest(ctx, p, selected.ID)
			result.Message = "Test message queued; inspect the last test result for delivery."
		case "change":
			if request.AlertSeverities == nil && request.SendWhileUnavailableOrMuted == nil {
				return fmt.Errorf("%w: provide --alert-severities or --send-while-unavailable-or-muted", ErrInvalid)
			}
			fields := map[string]string{}
			facts, _ := contactMethodTypeFacts(selected.ContactMethodType)
			for _, field := range facts.SettingsFields {
				fields[field] = selected.Setting(field)
			}
			in := ContactMethodInput{ID: selected.ID, ContactMethodType: selected.ContactMethodType, Fields: fields, Shared: selected.MemberID == "", ExpectedRevision: selected.Revision, Subscriptions: selected.Subscriptions, SendWhileUnavailableOrMuted: selected.SendWhileUnavailableOrMuted}
			if request.AlertSeverities != nil {
				in.Subscriptions = *request.AlertSeverities
			}
			if request.SendWhileUnavailableOrMuted != nil {
				in.SendWhileUnavailableOrMuted = *request.SendWhileUnavailableOrMuted
			}
			selected, err = a.SaveContactMethod(ctx, p, in)
			result.Message = "Contact method preferences saved."
		}
		if err != nil {
			return err
		}
	default:
		return ErrNotFound
	}
	methods, err = a.ContactMethods(ctx, p)
	if err != nil {
		return err
	}
	result.ContactMethods = []CommandContactMethod{}
	acknowledgedByAnotherUser, err := a.contactMethodsAcknowledgedByAnotherPushoverUser(ctx)
	if err != nil {
		return err
	}
	for _, c := range methods {
		if action != "list" && c.ID != selected.ID {
			continue
		}
		if action == "list" && ((request.SharedWithTeam && c.MemberID != "") || (request.SignInEmailAddress != "" && !strings.EqualFold(signInEmailAddresses[c.MemberID], request.SignInEmailAddress))) {
			continue
		}
		out := CommandContactMethod{ContactMethodID: c.ID, SignInEmailAddress: signInEmailAddresses[c.MemberID], SharedWithTeam: c.MemberID == "", ContactMethodType: c.ContactMethodType, SendsTo: c.SentTo, AlertSeverities: append([]string{}, c.Subscriptions...), SendWhileUnavailableOrMuted: c.SendWhileUnavailableOrMuted}
		if c.LastTest != nil {
			out.LastTestMessage = &CommandLastTestMessage{DeliveryState: c.LastTest.State, DeliveryStateExplanation: c.LastTest.Reason, QueuedAt: c.LastTest.CreatedAt}
		}
		if acknowledgedByAnotherUser[c.ID] {
			out.PushoverAcknowledgementProblem = pushoverAcknowledgementProblemText(personNameOrThePerson(names, c.MemberID))
		}
		result.ContactMethods = append(result.ContactMethods, out)
	}
	if action == "list" {
		result.Message = "Listed " + countText(len(result.ContactMethods), "contact method", "contact methods") + "."
	}
	return nil
}

// contactMethodsAcknowledgedByAnotherPushoverUser are the contact methods
// that have a delivery whose Pushover acknowledgement came from another user.
func (a *App) contactMethodsAcknowledgedByAnotherPushoverUser(ctx context.Context) (map[string]bool, error) {
	found := map[string]bool{}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT contact_method_id FROM deliveries WHERE contact_method_type='pushover_notification' AND reason=?`, pushoverAcknowledgementFromAnotherUserReason)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return err
			}
			found[id] = true
		}
		return rows.Err()
	})
	return found, err
}

// pushoverAcknowledgementProblemText names the problem and the fix.
func pushoverAcknowledgementProblemText(name string) string {
	if name == "The person" {
		name = "the person"
	}
	return fmt.Sprintf("An acknowledgement in Pushover came from another Pushover user, so it did not claim the incident. This contact method uses a key that isn't %[1]s's own Pushover user key, such as a group key. To fix it, remove this contact method and add it again with %[1]s's own Pushover user key.", name)
}
