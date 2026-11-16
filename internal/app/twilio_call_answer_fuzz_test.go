package app

import (
	"encoding/json"
	"testing"
)

// FuzzTwilioCallAnswer reads one answer to MMHotline's poll of a call, as
// Twilio sends it. An answer about another call, or with a status Twilio's
// documentation does not list, is an error and reports nothing. A call that
// waits, rings, or runs reports nothing yet. An ended call reports one of the
// four call results about this call only, and only a failed call fails the
// delivery attempt.
func FuzzTwilioCallAnswer(f *testing.F) {
	const callSID = "CA00000000000000000000000000000007"
	for _, seed := range []string{
		// The fake Twilio's answers to a call fetch, after Twilio's documentation.
		`{"sid":"CA00000000000000000000000000000007","status":"queued","to":"+12025550123"}`,
		`{"sid":"CA00000000000000000000000000000007","status":"completed","to":"+12025550123"}`,
		`{"sid":"CA00000000000000000000000000000007","status":"no-answer","to":"+12025550123"}`,
		`{"sid":"CA00000000000000000000000000000007","status":"failed","to":"+12025550123"}`,
		`{"sid":"CA00000000000000000000000000000008","status":"busy","to":"+12025550123"}`,
		`{"code":20404,"message":"The requested resource was not found","more_info":"https://www.twilio.com/docs/errors/20404","status":404}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		report, ended, err := readTwilioCallAnswer(data, callSID)
		var answer struct {
			SID    string `json:"sid"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(data, &answer)
		switch {
		case err != nil || !ended:
			if report != (DeliveryReport{}) || ended {
				t.Fatalf("no report expected: %+v, %v, %v", report, ended, err)
			}
			if err == nil && answer.Status != "queued" && answer.Status != "ringing" && answer.Status != "in-progress" {
				t.Fatalf("the call status %q was taken as still going", answer.Status)
			}
		case answer.SID != callSID || report.IDFromSendingService != callSID || report.SendingService != sendingServiceTwilio:
			t.Fatalf("a report about another call: %+v from %s", report, data)
		default:
			want := map[string]string{"completed": "completed", "busy": callBusy, "no-answer": callNoAnswer, "canceled": callNoAnswer, "failed": callFailed}[answer.Status]
			if want == "" || report.CallStatus != want || (report.State == "failed") != (want == callFailed) || (report.State != "" && report.State != "failed") {
				t.Fatalf("the call status %q was reported as %+v", answer.Status, report)
			}
		}
	})
}
