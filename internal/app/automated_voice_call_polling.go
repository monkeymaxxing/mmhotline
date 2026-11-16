package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const callPollInterval = 5 * time.Second

type phoneCallPoll struct {
	attemptID, idFromSendingService string
	expiresAt, nextPollAt           time.Time
}

// activeCallPolls reads only calls with a known ID that can still be active.
// Poll deadlines live in memory: an unchanged sending service answer writes nothing.
// A restarted process waits five seconds before its first fetch of each call.
func (p *TwilioSendingService) activeCallPolls(ctx context.Context, tx *sql.Tx, now time.Time) ([]phoneCallPoll, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,id_from_sending_service,started_at FROM delivery_attempts WHERE contact_method_type='automated_voice_call' AND sending_service='twilio' AND id_from_sending_service!='' AND call_result='calling' AND call_ended_at=0 AND started_at>? ORDER BY started_at,id`, now.Add(-callActiveAtMost).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nextPoll == nil {
		p.nextPoll = map[string]time.Time{}
	}
	active := map[string]bool{}
	var calls []phoneCallPoll
	for rows.Next() {
		var call phoneCallPoll
		var started int64
		if err = rows.Scan(&call.attemptID, &call.idFromSendingService, &started); err != nil {
			return nil, err
		}
		call.expiresAt = time.Unix(started, 0).Add(callActiveAtMost)
		active[call.attemptID] = true
		next, exists := p.nextPoll[call.attemptID]
		if !exists {
			next = now.Add(callPollInterval)
			p.nextPoll[call.attemptID] = next
		}
		call.nextPollAt = next
		calls = append(calls, call)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for id := range p.nextPoll {
		if !active[id] {
			delete(p.nextPoll, id)
		}
	}
	return calls, nil
}

func (a *App) nextPhoneCallDeadline(ctx context.Context, tx *sql.Tx, now time.Time) (time.Time, error) {
	p := a.twilioSendingService()
	if p == nil {
		return time.Time{}, nil
	}
	calls, err := p.activeCallPolls(ctx, tx, now)
	var next time.Time
	for _, call := range calls {
		next = earliest(next, call.nextPollAt, call.expiresAt)
	}
	return next, err
}

// ProcessPhoneCall polls one due call through the ordinary delivery report path.
// Reserving in memory before HTTP also prevents overlapping scheduler visits
// from fetching the same call more often than once every five seconds.
func (a *App) ProcessPhoneCall(ctx context.Context) (bool, error) {
	p := a.twilioSendingService()
	if p == nil {
		return false, nil
	}
	now := a.Now()
	var selected phoneCallPoll
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		calls, err := p.activeCallPolls(ctx, tx, now)
		if err != nil {
			return err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, call := range calls {
			if !p.nextPoll[call.attemptID].After(now) {
				selected = call
				p.nextPoll[call.attemptID] = now.Add(callPollInterval)
				break
			}
		}
		return nil
	})
	if err != nil || selected.attemptID == "" {
		return false, err
	}
	status, data, err := p.twilioGet(ctx, "/Calls/"+url.PathEscape(selected.idFromSendingService)+".json")
	if err != nil {
		return true, err
	}
	if status != http.StatusOK {
		return true, fmt.Errorf("call lookup through Twilio returned HTTP %d", status)
	}
	report, ended, err := readTwilioCallAnswer(data, selected.idFromSendingService)
	if err != nil || !ended {
		return true, err
	}
	return true, a.RecordDeliveryReport(ctx, selected.attemptID, report)
}

// readTwilioCallAnswer reads Twilio's answer to a fetch of the call callSID.
// ended is false while the call waits, rings, or runs, and the report is
// then empty.
// https://www.twilio.com/docs/voice/api/call-resource#call-status-values
func readTwilioCallAnswer(data []byte, callSID string) (report DeliveryReport, ended bool, err error) {
	var answer struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
		To     string `json:"to"`
	}
	if json.Unmarshal(data, &answer) != nil || answer.SID != callSID {
		return DeliveryReport{}, false, fmt.Errorf("could not read the call result returned by Twilio")
	}
	report = DeliveryReport{SendingService: sendingServiceTwilio, IDFromSendingService: answer.SID, RecipientNumber: answer.To}
	switch answer.Status {
	case "queued", "ringing", "in-progress":
		return DeliveryReport{}, false, nil
	case "completed":
		report.CallStatus = "completed"
	case "busy":
		report.CallStatus = callBusy
	case "no-answer", "canceled":
		report.CallStatus = callNoAnswer
	case "failed":
		report.CallStatus, report.State, report.Diagnostic = callFailed, "failed", "Call failed"
	default:
		return DeliveryReport{}, false, fmt.Errorf("unknown call status returned by Twilio")
	}
	return report, true, nil
}
