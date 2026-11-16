package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mmhotline/internal/downalarm"
)

// AutomatedVoiceCallContactMethodType wakes people for SOS incidents through Twilio.
// People open Pushover or MMHotline to claim; calls only speak the message.
type AutomatedVoiceCallContactMethodType struct {
	SendingService *TwilioSendingService
}

const (
	// callActiveAtMost: a call counts as active from its creation until its
	// final status report, or for 3 minutes when none arrives.
	callActiveAtMost = 3 * time.Minute
	// After the first 30 minutes, answered calls slow repeats to 5 minutes.
	callEveryMinuteFor           = 30 * time.Minute
	callAfterAnswered            = 5 * time.Minute
	callingPeriodEndsAfterGap    = 10 * time.Minute
	callWaitingForAnotherCall    = "Waiting: another call to this number is still going"
	callWaitingForSendingService = "Waiting for the phone sending service (M M Hotline calls no faster than the sending service allows)"
	callSpokenTitleMaximumRune   = 100
)

// Call results, stored on attempts and deliveries as call_result.
const (
	callCalling          = "calling"
	callAnswered         = "answered"
	callNoAnswer         = "no_answer"
	callBusy             = "busy"
	callFailed           = "call_failed"
	callFinalResultsList = "'answered','no_answer','busy','call_failed'"
)

// callResultLabel is the delivery list's words for how a call went.
func callResultLabel(result string) string {
	switch result {
	case callCalling:
		return "Calling"
	case callAnswered:
		return "Answered (by a person or voicemail)"
	case callNoAnswer:
		return "No answer"
	case callBusy:
		return "Busy"
	case callFailed:
		return "Call failed"
	}
	return ""
}

func callResultFinal(result string) bool {
	return strings.Contains(callFinalResultsList, "'"+result+"'")
}

func (*AutomatedVoiceCallContactMethodType) Name() string { return "automated_voice_call" }

// Facts: a call whose outcome is unknown is never retried; the next repeat
// is the retry.
func (*AutomatedVoiceCallContactMethodType) Facts() ContactMethodTypeFacts {
	return ContactMethodTypeFacts{
		CardTitle: "Automated voice call", ValueLabel: "Phone number", ValueField: "phone_number_to_call",
		ValueInputType: "tel", SettingsFields: []string{"phone_number_to_call"},
		WakesPeople: true, WakesSleepingPeople: true, SaysTeamName: true,
		CombinesMessages: true, SOSOnly: true, ValueFixed: true, CombinesSOSEscalation: true,
		AcceptedLabel: "Calling",
	}
}

func (*AutomatedVoiceCallContactMethodType) CheckSettings(_ context.Context, input ContactMethodSettingsInput) (ContactMethodSettings, error) {
	return checkPhoneNumberSettings(input)
}

func (c *AutomatedVoiceCallContactMethodType) Send(ctx context.Context, m ContactMethodSend) DeliveryResult {
	var settings phoneNumberSettings
	if json.Unmarshal([]byte(m.Settings), &settings) != nil || settings.PhoneNumberToCall == "" {
		return DeliveryResult{State: "failed", Diagnostic: "Not called: the automated voice call settings are incomplete"}
	}
	if c.SendingService == nil {
		return DeliveryResult{State: "failed", Diagnostic: "Not called: no phone sending service is set up"}
	}
	var markup string
	switch m.Purpose {
	case "contact_method_test":
		markup = callMarkup(strings.Join(strings.Fields(m.Text), " ") + " " + testCallMessage)
	default:
		titles := []string{m.Title}
		if len(m.CombinedAlerts) > 0 {
			titles = titles[:0]
			for _, a := range m.CombinedAlerts {
				titles = append(titles, a.Title)
			}
		}
		markup = callMarkup(sosCallMessage(m.TeamName, titles, m.AcknowledgementClaimsSetUp))
	}
	answer := c.SendingService.createCall(ctx, settings.PhoneNumberToCall, markup)
	result := DeliveryResult{State: answer.State, IDFromSendingService: answer.IDFromSendingService, Diagnostic: answer.Diagnostic, SendingService: sendingServiceTwilio}
	if answer.TooManyRequests {
		// The sending service made no call and takes calls again a moment later:
		// the call is made again a second later, not at the next repeat a
		// minute later.
		result.Retryable, result.RetryAfter = true, twilioWaitRetry
	}
	return result
}

// PaceDelivery makes a call wait while another call to the same number is
// active, slows repeated SOS calls answered after the first 30
// minutes, and paces each sending service's calls.
func (c *AutomatedVoiceCallContactMethodType) PaceDelivery(ctx context.Context, tx *sql.Tx, d Delivery, now time.Time, reserving bool) (time.Time, string, string, error) {
	var active int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(t.started_at),0) FROM delivery_attempts t JOIN deliveries d ON d.id=t.delivery_id WHERE d.contact_method_id IN (SELECT id FROM contact_methods WHERE contact_method_type='automated_voice_call' AND sent_to=?) AND d.id!=? AND t.id_from_sending_service!='' AND t.call_result=? AND t.call_ended_at=0 AND t.started_at>?`, d.SentTo, d.ID, callCalling, now.Add(-callActiveAtMost).Unix()).Scan(&active)
	if err != nil {
		return time.Time{}, "", "", err
	}
	if active != 0 {
		return time.Unix(active, 0).Add(callActiveAtMost), callWaitingForAnotherCall, "", nil
	}
	if d.Purpose == "sos_escalation" {
		next, err := voicemailPacing(ctx, tx, d.SentTo, now)
		if err != nil {
			return time.Time{}, "", "", err
		}
		if next.After(now) {
			return next, fmt.Sprintf("Waiting until %s: the last call was answered, so calls to this number go every 5 minutes", displayTime(next)), "", nil
		}
	}
	if c.SendingService == nil {
		return time.Time{}, "", "", nil
	}
	if !reserving {
		return time.Time{}, "", "", nil
	}
	priority, err := phoneDeliveryPriority(ctx, tx, d)
	if err != nil {
		return time.Time{}, "", "", err
	}
	if c.SendingService.Twilio != nil && !c.SendingService.takeSlot(priority, now) {
		return now.Add(twilioWaitRetry), callWaitingForSendingService, "", nil
	}
	return time.Time{}, "", sendingServiceTwilio, nil
}

// voicemailPacing: during the first 30 minutes of calling a number about
// SOS, it is called every minute no matter what. After that, when the last
// call was answered (by a person or voicemail), the next
// call waits until 5 minutes after it. A call with no final status counts as
// answered; one that rang unanswered costs nothing and keeps the
// one-minute pace.
func voicemailPacing(ctx context.Context, tx *sql.Tx, number string, now time.Time) (time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.started_at,t.call_result,t.id_from_sending_service FROM delivery_attempts t JOIN deliveries d ON d.id=t.delivery_id WHERE d.contact_method_id IN (SELECT id FROM contact_methods WHERE contact_method_type='automated_voice_call' AND sent_to=?) AND d.purpose='sos_escalation' AND t.started_at>? ORDER BY t.started_at DESC LIMIT 2000`, number, now.Add(-24*time.Hour).Unix())
	if err != nil {
		return time.Time{}, err
	}
	type call struct {
		started                      int64
		result, idFromSendingService string
	}
	var calls []call
	for rows.Next() {
		var c call
		if err = rows.Scan(&c.started, &c.result, &c.idFromSendingService); err != nil {
			rows.Close()
			return time.Time{}, err
		}
		calls = append(calls, c)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(calls) == 0 {
		return time.Time{}, err
	}
	last := calls[0]
	if now.Sub(time.Unix(last.started, 0)) > callingPeriodEndsAfterGap {
		return time.Time{}, nil
	}
	periodStart := last.started
	for _, c := range calls[1:] {
		if time.Duration(periodStart-c.started)*time.Second > callingPeriodEndsAfterGap {
			break
		}
		periodStart = c.started
	}
	if now.Sub(time.Unix(periodStart, 0)) < callEveryMinuteFor {
		return time.Time{}, nil
	}
	noFinalStatus := last.idFromSendingService != "" && !callResultFinal(last.result) && now.Sub(time.Unix(last.started, 0)) >= callActiveAtMost
	if last.result == callAnswered || noFinalStatus {
		return time.Unix(last.started, 0).Add(callAfterAnswered), nil
	}
	return time.Time{}, nil
}

// recordDeliveryOutcome records call progress, releases waiting calls when
// a call ends, and sends the SOS through other contact methods after failure.
func (c *AutomatedVoiceCallContactMethodType) recordDeliveryOutcome(ctx context.Context, tx *sql.Tx, d Delivery, attemptID string, o deliveryOutcome, now time.Time) error {
	var current string
	var number int
	if err := tx.QueryRowContext(ctx, `SELECT call_result,number FROM delivery_attempts WHERE id=?`, attemptID).Scan(&current, &number); err != nil {
		return err
	}
	next := current
	switch {
	case !o.FromReport && o.AttemptState == "accepted" && current == "":
		next = callCalling
	case o.CallStatus == "completed" && !callResultFinal(current):
		next = callAnswered
	case (o.CallStatus == callNoAnswer || o.CallStatus == callBusy || o.CallStatus == callFailed) && !callResultFinal(current):
		next = o.CallStatus
	case !o.FromReport && o.AttemptState == "failed":
		next = callFailed
	}
	if next != current {
		if err := setCallResult(ctx, tx, d.ID, attemptID, number, next); err != nil {
			return err
		}
	}
	// A final status releases calls waiting for this number.
	switch o.CallStatus {
	case "completed", callNoAnswer, callBusy, callFailed:
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET call_ended_at=? WHERE id=? AND call_ended_at=0`, now.Unix(), attemptID); err != nil {
			return err
		}
		if err := releaseWaitingCalls(ctx, tx, d.SentTo, now); err != nil {
			return err
		}
	}
	if next == callFailed && current != callFailed {
		return afterWakingDeliveryFailed(ctx, tx, d, now)
	}
	return nil
}

// releaseWaitingCalls makes the calls waiting for the call to a number due
// now; reservation checks the voicemail pacing again.
func releaseWaitingCalls(ctx context.Context, tx *sql.Tx, number string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE deliveries SET next_attempt=?,reason='' WHERE contact_method_id IN (SELECT id FROM contact_methods WHERE contact_method_type='automated_voice_call' AND sent_to=?) AND state='queued' AND reason=?`, now.Unix(), number, callWaitingForAnotherCall)
	return err
}

func setCallResult(ctx context.Context, tx *sql.Tx, deliveryID, attemptID string, number int, result string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET call_result=? WHERE id=?`, result, attemptID); err != nil {
		return err
	}
	// The delivery shows its newest attempt's call.
	_, err := tx.ExecContext(ctx, `UPDATE deliveries SET call_result=? WHERE id=? AND attempts=?`, result, deliveryID, number)
	return err
}

// What the calls say. English only at launch, in the sending service's default
// voice, with short sentences.
const testCallMessage = "This is a test call from M M Hotline. Open M M Hotline to check the call result."

// sosCallMessage is what a call about one or several SOS incidents says.
func sosCallMessage(team string, titles []string, pushoverSetUp bool) string {
	howToClaim := func(it string) string {
		if pushoverSetUp {
			return fmt.Sprintf(" Open M M Hotline and claim %s, or acknowledge the alert in Pushover, which also claims %s. Claiming makes you responsible and stops the calls.", it, it)
		}
		return fmt.Sprintf(" Open M M Hotline and claim %s. Claiming makes you responsible and stops the calls.", it)
	}
	team = spokenText(team, 60)
	if len(titles) == 1 {
		return fmt.Sprintf("Hello. This is M M Hotline. An SOS incident needs someone now. Team: %s. %s.", team, spokenText(titles[0], callSpokenTitleMaximumRune)) + howToClaim("it")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Hello. This is M M Hotline. %d SOS incidents need someone now. Team: %s.", len(titles), team)
	for i, ordinal := range []string{"First", "Second", "Third"} {
		if i < len(titles) {
			fmt.Fprintf(&b, " %s: %s.", ordinal, spokenText(titles[i], callSpokenTitleMaximumRune))
		}
	}
	if len(titles) > 3 {
		fmt.Fprintf(&b, " And %d more.", len(titles)-3)
	}
	b.WriteString(howToClaim("them"))
	return b.String()
}

var (
	spokenURLPattern      = regexp.MustCompile(`(?i)\b(?:[a-z][a-z0-9+.-]*://|www\.)\S+`)
	spokenDigitsPattern   = regexp.MustCompile(`\d{6,}`)
	spokenHexPattern      = regexp.MustCompile(`(?i)\b[0-9a-f]*[0-9][0-9a-f]*\b`)
	spokenSpacesPattern   = regexp.MustCompile(`\s+`)
	spokenMarkupCharacter = strings.NewReplacer("<", " ", ">", " ", "&", " and ")
)

// spokenText prepares a title to be spoken: URLs and long runs of digits or
// hex (8 or more characters with a digit) are removed, then the first
// maximum characters are kept.
func spokenText(s string, maximum int) string {
	s = spokenURLPattern.ReplaceAllString(s, " ")
	s = spokenHexPattern.ReplaceAllStringFunc(s, func(word string) string {
		if len(word) >= 8 {
			return " "
		}
		return word
	})
	s = spokenDigitsPattern.ReplaceAllString(s, " ")
	s = strings.TrimSpace(spokenSpacesPattern.ReplaceAllString(spokenMarkupCharacter.Replace(s), " "))
	if runes := []rune(s); len(runes) > maximum {
		s = strings.TrimSpace(string(runes[:maximum]))
	}
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "No title"
	}
	return s
}

var xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// speakableText replaces each character that must not reach the call markup
// with a space, so the words around it stay apart. Those characters are:
//   - every control character (C0, DEL, and C1: unicode.IsControl). XML 1.0
//     forbids most C0 controls, such as the ESC of a forwarded ANSI color
//     code, and Twilio may refuse or fail on markup that is not well-formed.
//     A voice never reads the others aloud either, and tab, LF, and CR are
//     only whitespace here.
//   - U+FFFE and U+FFFF, which XML 1.0 forbids.
//   - bytes that are not valid UTF-8, which XML cannot carry. Ranging over
//     a string yields U+FFFD for each, so the replacement character itself
//     (nothing to say aloud) goes too.
//
// It is the single rule for outside text (alert titles, team names, test
// call text) and is applied in callMarkup, which every call goes through:
// SOS calls, the MMHotline-down alarm's held calls, and test calls.
func speakableText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '￾' || r == '￿' || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
}

// callMarkup repeats the escaped message twice, then ends the call.
func callMarkup(message string) string {
	say := "<Say>" + xmlText.Replace(speakableText(message)) + "</Say>"
	return `<?xml version="1.0" encoding="UTF-8"?><Response>` + say + say + `<Hangup/></Response>`
}

// phoneDeliveryPriority: the first call to a person about an SOS goes
// before repeats and test calls.
func phoneDeliveryPriority(ctx context.Context, tx *sql.Tx, d Delivery) (int, error) {
	switch {
	case d.Purpose == "sos_escalation":
		var earlier int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE incident_id=? AND contact_method_id=? AND purpose='sos_escalation' AND attempts>0 AND id!=?`, d.IncidentID, d.ContactMethodID, d.ID).Scan(&earlier)
		if err != nil || earlier > 0 {
			return priorityUrgent, err
		}
		return priorityFirstContact, nil
	}
	return priorityUrgent, nil
}

// downAlarmRequests builds the MMHotline-down alarm's held call through the team's
// Twilio account, and its test call.
func (c *AutomatedVoiceCallContactMethodType) downAlarmRequests(contactMethod ContactMethod, text downAlarmText) (downalarm.HeldRequest, downalarm.HeldRequest, error) {
	var settings phoneNumberSettings
	if json.Unmarshal([]byte(contactMethod.Settings), &settings) != nil || settings.PhoneNumberToCall == "" {
		return downalarm.HeldRequest{}, downalarm.HeldRequest{}, errors.New("the automated voice call settings are incomplete")
	}
	if c.SendingService == nil || c.SendingService.Twilio == nil {
		return downalarm.HeldRequest{}, downalarm.HeldRequest{}, errors.New("Twilio is not set up")
	}
	build := func(message string) (downalarm.HeldRequest, error) {
		return heldRequest(c.SendingService.twilioPostRequest(context.Background(), "/Calls.json", c.SendingService.callForm(settings.PhoneNumberToCall, callMarkup(message))))
	}
	request, err := build("This is MMHotline for " + text.TeamName + ". MMHotline stopped checking in with the MMHotline-down alarm. MMHotline may be down and unable to send urgent alerts. Check the MMHotline server.")
	if err != nil {
		return request, downalarm.HeldRequest{}, err
	}
	test, err := build("This is a test of the MMHotline-down alarm for " + text.TeamName + ". When MMHotline stops checking in, the MMHotline-down alarm calls you the same way as this test call. Nothing is wrong.")
	return request, test, err
}
