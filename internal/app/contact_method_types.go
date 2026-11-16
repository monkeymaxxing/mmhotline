package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// ContactMethodType is one way MMHotline reaches people: email, outgoing webhook, Telegram
// message, Slack message, Pushover notification, and automated voice call. Each contact method type is one file
// (contact_method_type_<name>.go) and one line in contactMethodTypes. Shared code reads the
// contact method type's facts and never tests for a contact method type by name.
type ContactMethodType interface {
	Name() string
	Facts() ContactMethodTypeFacts
	// CheckSettings validates what a person entered and returns the settings
	// to store. It never contacts the destination.
	CheckSettings(ctx context.Context, input ContactMethodSettingsInput) (ContactMethodSettings, error)
	// Send makes exactly one attempt. The delivery outbox owns every retry.
	Send(ctx context.Context, send ContactMethodSend) DeliveryResult
}

type ContactMethodTypeFacts struct {
	AttemptTimeLimit                  time.Duration // zero uses the shared 15-second limit
	CardTitle, ValueLabel, ValueField string
	CardTitleIsAProductName           bool     // "Slack", "Telegram": keeps its capital inside a sentence
	ValueInputType                    string   // HTML input type for the value: "email", "url", "tel"
	SettingsFields                    []string // every form field the contact method type reads, such as "outgoing_webhook_url"
	WakesPeople                       bool     // follows the SOS target; team mute stops it
	ConnectsThroughSendingService     bool     // settings come from the sending service connection, never a form
	ClaimMethod                       string   // "", "button", "acknowledgement"
	CombinesMessages                  bool     // false only for outgoing_webhook
	RetryUncertain                    bool     // may resend after an unknown outcome
	AllowedForTeamAccount             bool     // shared contact methods (never automated_voice_call or pushover_notification)
	SOSOnly                           bool     // automated_voice_call: only SOS, never Info, Warning, or resolved messages
	ForMachines                       bool     // read by programs (outgoing_webhook): never gets notices meant for people
	MaximumPerPerson                  int      // bounds accidental notification fan-out per person
	MaximumPerInstallation            int      // 0 means only the overall contact method limit
	AcceptedLabel                     string   // "Accepted by the mail server"
	WakesSleepingPeople               bool     // phone calls and Pushover Critical Alerts can wake a sleeping person
	ValueFixed                        bool     // phone numbers: added with their own form; to change one, remove it and add the new one
	CombinesSOSEscalation             bool     // automated_voice_call and pushover_notification: SOS escalation messages due together combine
	SaysTeamName                      bool     // automated_voice_call: the message names the team, read from team_info
}

// ContactMethodSettingsInput holds the form fields a person entered, keyed by the
// field names the contact method type declares.
type ContactMethodSettingsInput struct {
	Fields map[string]string
}

// ContactMethodSettings is what a contact method stores: the contact method type's validated
// settings as JSON, a secret that is stored only when the contact method is
// created and never shown again, and SentTo, the plain description of the
// destination that deliveries copy.
type ContactMethodSettings struct {
	JSON, Secret, SentTo string
}

// ContactMethodSend is one immutable attempt request. Every retry of a delivery
// carries the same DeliveryID and content. SentFrom is empty on the first
// attempt; afterwards it is the sender the contact method type reported, so retries keep
// the same sender even if the server administrator's configuration changed.
//
// Subject and Text are the plain rendering (email uses them). Title,
// AlertMessage, NotesFromMmhotline, the IDs, IncidentStatus, and
// AlertReceivedAt are the raw facts for contact method types that format their own
// messages; they were copied when the delivery was queued, so every retry
// carries the same content.
type ContactMethodSend struct {
	// AcknowledgementClaimsSetUp: a configured contact method type claims an
	// incident when a person acknowledges its alert (Pushover).
	AcknowledgementClaimsSetUp                     bool
	DeliveryID, Purpose, Severity                  string
	SentTo, SentFrom                               string
	Subject, Text                                  string
	Settings, Secret                               string
	Title, AlertMessage, NotesFromMmhotline        string
	AlertID, IncidentID, IncidentStatus, PublicURL string
	AlertReceivedAt                                time.Time
	// CombinedAlerts lists every alert in a combined message, this delivery's
	// own first; it is empty for a message about one alert. Subject and Text
	// already hold the plain combined rendering.
	CombinedAlerts []CombinedAlert
	// ClaimButtons asks a contact method type whose claim method is a button to add
	// "Claim this incident" and "Resolve". Shared code decides: only an SOS
	// message about one incident has them, never a combined message.
	ClaimButtons bool
	// DeliveryAttemptID is MMHotline's own ID for this attempt (the attempt
	// ID). Sending services that call back get it in the callback URL.
	DeliveryAttemptID string
	// MemberName and TeamName are for messages that say who asked for a
	// test or which team an SOS is for. MemberName is the contact method's
	// owner, empty for a shared contact method.
	MemberName, TeamName string
	// ResolvesAnSOSThatWaitedForAServerReplacement: a resolved message of
	// an SOS that waited while the triage agent replaced a server, whose
	// start the person heard as a warn alert (server_replacements.go).
	ResolvesAnSOSThatWaitedForAServerReplacement bool
}

// DeliveryResult distinguishes sending service acceptance from actual receipt and
// uncertainty from a request known to have been rejected.
type DeliveryResult struct {
	State, IDFromSendingService, Diagnostic, SentFrom string
	Retryable                                         bool
	// SendingService is the sending service the attempt went through, for signed call callbacks.
	SendingService string
	// RetryAfter replaces the shared backoff before the next attempt, such as
	// retrying a call one second after HTTP 429.
	RetryAfter time.Duration
}

// DeliveryReport is what a sending service reported later about one attempt, found
// by its delivery attempt ID. Reports only move an attempt forward.
type DeliveryReport struct {
	SendingService string
	// State is "accepted", "delivered", or "failed", or empty when the
	// report only says how a call is going.
	State, IDFromSendingService, Diagnostic string
	// CallStatus is a phone call's final status: "completed",
	// "no_answer", "busy", or "call_failed".
	CallStatus string
	// RecipientNumber is the number the report says the call went
	// to. It decides whether a report may give an attempt its first
	// sending service ID.
	RecipientNumber string
}

// deliveryPacer is a contact method type that decides when its next delivery may go:
// one phone call to a number at a time,
// and each phone sending service's request rate. When reserving, a delivery that
// may go now also gets the sending service it goes through.
type deliveryPacer interface {
	PaceDelivery(ctx context.Context, tx *sql.Tx, d Delivery, now time.Time, reserving bool) (waitUntil time.Time, reason, sendingService string, err error)
}

// deliveryOutcomeRecorder is a contact method type that records what an attempt's result
// or a later report means for its call, in the transaction that saves the result.
type deliveryOutcomeRecorder interface {
	recordDeliveryOutcome(ctx context.Context, tx *sql.Tx, d Delivery, attemptID string, outcome deliveryOutcome, now time.Time) error
}

// ringingEmergencySender is a contact method type whose emergency keeps
// ringing until someone acknowledges it (Pushover). After an unknown outcome
// the emergency may already ring, so it is never sent again, even when the
// contact method type may resend after an unknown outcome (RetryUncertain).
// A new SOS escalation round waits while the last emergency still rings.
type ringingEmergencySender interface {
	resendsAfterUnknownOutcome(d Delivery) bool
	escalationRoundStillRinging(ctx context.Context, tx *sql.Tx, contactMethodID, incidentID string, at time.Time) (bool, error)
}

// deliveryOutcome is one saved result or report of an attempt.
type deliveryOutcome struct {
	AttemptState, CallStatus string
	FromReport               bool
}

// contactMethodTypes lists every contact method type MMHotline knows. The values here carry no
// configuration; they answer Name and Facts. Configured contact method types that can
// send live in App.ContactMethodTypes.
var contactMethodTypes = []ContactMethodType{EmailContactMethodType{}, OutgoingWebhookContactMethodType{}, &TelegramMessageContactMethodType{}, &SlackMessageContactMethodType{}, &AutomatedVoiceCallContactMethodType{}, &PushoverNotificationContactMethodType{}}

func knownContactMethodType(name string) (ContactMethodType, bool) {
	for _, c := range contactMethodTypes {
		if c.Name() == name {
			return c, true
		}
	}
	return nil, false
}

// contactMethodTypeFor returns the configured contact method type of this name, or the unconfigured
// one when it cannot send here, so settings are always checked the same way
// as they will be sent.
func (a *App) contactMethodTypeFor(name string) (ContactMethodType, bool) {
	if c, ok := a.configuredContactMethodTypes()[name]; ok {
		return c, true
	}
	return knownContactMethodType(name)
}

func contactMethodTypeFacts(name string) (ContactMethodTypeFacts, bool) {
	c, ok := knownContactMethodType(name)
	if !ok {
		return ContactMethodTypeFacts{}, false
	}
	return c.Facts(), true
}

// cardTitleInSentence writes a card title inside a sentence: "automated voice call",
// "email", and product names such as
// "Slack message" and "Telegram message", which keep their capital.
func cardTitleInSentence(facts ContactMethodTypeFacts) string {
	if facts.CardTitleIsAProductName {
		return facts.CardTitle
	}
	return strings.ToLower(facts.CardTitle)
}

// cardTitleWithArticle writes "an automated voice call", "an email", "a Slack message".
func cardTitleWithArticle(facts ContactMethodTypeFacts) string {
	word := cardTitleInSentence(facts)
	if word == "" {
		return word
	}
	first := word[0]
	if strings.IndexByte("aeiouAEIOU", first) >= 0 {
		return "an " + word
	}
	return "a " + word
}

// settingsJSON encodes a contact method type's settings struct with its explicit field names.
func settingsJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	return string(data), err
}

// settingValue reads one named field from stored settings for display.
func settingValue(settings, field string) string {
	var values map[string]any
	if json.Unmarshal([]byte(settings), &values) != nil {
		return ""
	}
	s, _ := values[field].(string)
	return s
}

// deliveryPreparationRecorder freezes a contact method type-specific notification identity
// before the external attempt, in the same transaction as the final send check.
type deliveryPreparationRecorder interface {
	prepareDelivery(context.Context, *sql.Tx, Delivery, []CombinedAlert, time.Time) error
}
