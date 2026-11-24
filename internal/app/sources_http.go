package app

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"time"
)

// alertIntakeAddress is where senders post: the intake listener, never the
// web address, which sits behind Cloudflare Access.
func (a *App) alertIntakeAddress() string { return "http://" + a.AlertIntakeListenAddress }

func (a *App) RegisterSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /alert-sources", a.sourcesPage)
}

// readSignedSenderRequest reads the body and signature of alert intake or an
// check-in, answering the request itself when it cannot be read.
func readSignedSenderRequest(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		http.Error(w, "body: Content-Type must be application/json", 400)
		return nil, "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxAlertBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "body exceeds 64 KiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "body could not be read", 400)
		}
		return nil, "", false
	}
	if len(r.Header.Values("Mmhotline-Signature")) != 1 {
		http.Error(w, "source authentication failed", http.StatusUnauthorized)
		return nil, "", false
	}
	return body, r.Header.Get("Mmhotline-Signature"), true
}

// senderRefusal answers a refused alert or check-in.
func senderRefusal(w http.ResponseWriter, err error, receiptKey *receipt, now time.Time) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		writeSenderText(w, 401, "source authentication failed", nil, now)
	case errors.Is(err, ErrInvalid):
		writeSenderText(w, 400, publicError(err), receiptKey, now)
	case errors.Is(err, ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		writeSenderText(w, 429, "admission limit reached; retry later", receiptKey, now)
	default:
		writeSenderText(w, 503, "could not be committed; retry later", nil, now)
	}
}

func (a *App) alertSourceCheckInHTTP(w http.ResponseWriter, r *http.Request) {
	body, signature, ok := readSignedSenderRequest(w, r)
	if !ok {
		return
	}
	received, secret, err := a.ReceiveAlertSourceCheckIn(r.Context(), r.PathValue("alert_source_id"), signature, body)
	receiptKey := newReceipt(secret, "check-in", signature)
	if err != nil {
		senderRefusal(w, err, receiptKey, a.Now())
		return
	}
	writeSenderJSON(w, 200, struct {
		CheckInReceivedAt string `json:"check_in_received_at"`
	}{received.UTC().Format(time.RFC3339)}, receiptKey, a.Now())
}

func (a *App) genericIntake(w http.ResponseWriter, r *http.Request) {
	body, signature, ok := readSignedSenderRequest(w, r)
	if !ok {
		return
	}
	e, outcome, secret, err := a.AdmitGeneric(r.Context(), r.PathValue("alert_source_id"), signature, body)
	receiptKey := newReceipt(secret, "alert-intake", signature)
	now := a.Now()
	if err != nil {
		senderRefusal(w, err, receiptKey, now)
		return
	}
	writeSenderJSON(w, 200, struct {
		MmhotlineAlertID    string `json:"mmhotline_alert_id"`
		MmhotlineIncidentID string `json:"mmhotline_incident_id,omitempty"`
		AlreadyReceived     bool   `json:"already_received"`
		// Only when true: the alert was folded into the earlier warn that
		// mmhotline_alert_id names.
		RecordedAsRepeatOfEarlierAlert bool `json:"recorded_as_repeat_of_earlier_alert,omitempty"`
	}{e.ID, e.IncidentID, outcome.AlreadyReceived, outcome.RecordedAsRepeatOfEarlierAlert}, receiptKey, now)
}

type sourcesPageData struct {
	AlertIntakeAddress string
	Admin              bool
	Sources            []sourceView
	settingsFileNoticeView
}

// sourceView adds the alert source's refused requests of the last 24 hours.
type sourceView struct {
	Source
	Status, Tone    string
	RejectedTotal   int64
	RejectedReasons []RejectedRequest
}

// sourcesPage lists the alert sources, which the settings file sets.
func (a *App) sourcesPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	data := sourcesPageData{AlertIntakeAddress: a.alertIntakeAddress(), settingsFileNoticeView: a.settingsFileNoticeView()}
	m, err := a.CurrentMember(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	data.Admin = m.Role == RoleAdministrator
	sources, err := a.Sources(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	for _, s := range sources {
		checkIns, err := a.AlertSourceCheckInStatus(r.Context(), p, s.ID)
		if err != nil {
			HTTPError(w, err)
			return
		}
		summary := alertSourcePageData{Source: s, CheckIns: checkIns}
		summary.checkInSummary(a.Now())
		v := sourceView{Source: s, Status: summary.Status, Tone: summary.Tone}
		v.RejectedTotal, v.RejectedReasons = a.rejected.last24Hours(s.ID, a.Now())
		data.Sources = append(data.Sources, v)
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "Alert sources", frame, nil, sourcesPageBody(frame, data))
}

// sourceRejectedClass marks the refused-requests line when a source had refused requests.
func sourceRejectedClass(rejectedTotal int64) string {
	if rejectedTotal != 0 {
		return "small line-warn"
	}
	return "small faint"
}
