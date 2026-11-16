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
	e, duplicate, secret, err := a.AdmitGeneric(r.Context(), r.PathValue("alert_source_id"), signature, body)
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
	}{e.ID, e.IncidentID, duplicate}, receiptKey, now)
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
	RenderPage(w, 200, "Alert sources", sourcesBody, data)
}

var sourcesBody = `{{$page := .}}<div class="stack alert-pages"><section class="panel"><div class="panel-head"><h2>Senders</h2></div>{{if .Sources}}<ul class="items">{{range .Sources}}<li><div class="what"><div class="row"><a class="title" href="/alert-sources/{{.ID}}">{{.Name}}</a><span class="tag {{.Tone}}">{{.Status}}</span></div>{{if .Active}}<p class="small muted">{{if eq .Kind "generic"}}Generic signed JSON{{else}}{{.Kind}}{{end}}</p>{{else}}<p class="small muted">The settings file no longer lists this source, so the source's signing secrets no longer work. Earlier alerts and check-ins stay.</p>{{end}}<p class="small"><a href="/alert-sources/{{.ID}}">Check-ins and planned downtime</a></p><p class="small {{if .RejectedTotal}}line-warn{{else}}faint{{end}}">Rejected requests in the last 24 hours: {{.RejectedTotal}}</p>{{if .RejectedReasons}}<ul class="bullets small">{{range .RejectedReasons}}<li>{{.Reason}}: {{.Count}}</li>{{end}}</ul>{{end}}
<details class="fold"><summary>Connection details</summary><div class="source-connection"><dl class="kv"><dt>Alert intake address</dt><dd><code>{{$page.AlertIntakeAddress}}</code></dd><dt>Alert source ID</dt><dd><code>{{.ID}}</code></dd><dt>Intake URL</dt><dd><code>POST {{$page.AlertIntakeAddress}}/alert-intake/{{.ID}}</code></dd><dt>Check-in URL</dt><dd><code>POST {{$page.AlertIntakeAddress}}/check-in/{{.ID}}</code></dd></dl>{{if and .Active $page.Admin}}<p class="small faint">Signing secret IDs identify the configured secrets. Secret values are never shown.</p><ul class="bullets small">{{range .Secrets}}<li>Signing secret ID: <code>{{.ID}}</code>, added {{clock .CreatedAt}}</li>{{end}}</ul>{{end}}</div></details></div></li>{{end}}</ul>{{else}}<div class="panel-body"><p>No alert sources are set up.</p><p class="small muted">An administrator adds the sending tool or script under <code>alert_sources</code> in the settings file. Open “Where alert sources are set” for the file and apply command.</p></div>{{end}}</section>
<details class="more"><summary>Where alert sources are set</summary><div class="body">` + settingsFileNotice("<code>alert_sources</code>") + `</div></details>
<details class="more"><summary>Request format and signing</summary><div class="body"><p class="small muted">Send signed JSON to the source's Intake URL. The sender uses the signing secret from the team's vault.</p><pre>{"sender_alert_id":"alert-1","sender_incident_key":"backup","severity":"sos","title":"Backup failed","message":"Disk full"}</pre><p class="small"><code>Mmhotline-Signature: timestamp_unix_seconds=&lt;unix seconds&gt;,hmac_sha256_hex=&lt;hex HMAC-SHA256&gt;</code>. Decode the 64 hexadecimal characters of the signing secret into 32 bytes, and compute HMAC-SHA256 over the timestamp, a dot, and the exact JSON bytes. The timestamp must be within five minutes. Reuse a sender alert ID when retrying.</p><p class="small">An SOS is the urgent alert that wakes the person on call, even at night. A <code>resolved-sos</code> alert with the same sender incident key closes the matching unresolved SOS incident.</p><p class="small">A sender that checks in posts a signed message every 30 seconds to the Check-in URL. MMHotline starts watching for missing check-ins after the first check-in arrives.</p></div></details></div>`
