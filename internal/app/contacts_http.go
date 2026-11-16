package app

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) RegisterContactMethodRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /contact-methods", a.contactMethodsPage)
	mux.HandleFunc("POST /contact-methods/save", a.saveContactMethodHTTP)
	mux.HandleFunc("POST /contact-methods/remove", a.removeContactMethodHTTP)
	mux.HandleFunc("POST /contact-methods/test", a.testContactMethodHTTP)
	mux.HandleFunc("GET /deliveries/{id}", a.deliveryPage)
	mux.HandleFunc("POST /contact-methods/outgoing-webhook-signing-secrets", a.addWebhookSigningSecretHTTP)
	mux.HandleFunc("POST /contact-methods/outgoing-webhook-signing-secrets/revoke", a.revokeWebhookSigningSecretHTTP)
	mux.HandleFunc("POST /contact-methods/outgoing-webhook-bearer-token", a.webhookBearerTokenHTTP)
	mux.HandleFunc("POST /connect-telegram", a.connectTelegramHTTP)
}

type contactMethodView struct {
	ContactMethod
	Facts                            ContactMethodTypeFacts
	Value                            string
	CardTitleInSentence              string
	OwnerName                        string
	CSRF                             string
	LastTestView                     *deliveryPageView
	SigningSecretIDs                 []string
	CanEdit, Info, Warn, SOS, Shared bool
}
type contactMethodPageData struct {
	CSRF, Error, Notice string
	Admin               bool
	ContactMethods      []contactMethodView
	Deliveries          []deliveryPageView
	OlderDeliveries     []deliveryPageView
	OtherContactMethods []contactMethodView
	// New holds the add form of each kind of contact method, by contact method type.
	New                         map[string]*contactMethodView
	ShownSecret                 *WebhookSigningSecretShownOnce
	MissingSendingServiceSetups []SendingServiceSetup
	// Phone number form and contact-card state.
	PhoneNumbersAvailable, HasPhoneNumber, HasAutomatedVoiceCall bool
	PhoneNumberConsent, NewPhoneNumber                           string
	ContactCard                                                  ContactCardState
}

// deliveryPageView keeps retry timestamps separate so pages use the team's clock.
type deliveryPageView struct {
	Delivery
	Outcome, Recipient string
	RetryAt            time.Time
}

func viewDelivery(d Delivery, viewerID string, names map[string]string) deliveryPageView {
	outcome, _, retry := strings.Cut(d.OutcomeLabel(), ", will retry at ")
	v := deliveryPageView{Delivery: d, Outcome: outcome, Recipient: deliveryRecipient(d, viewerID, names)}
	if retry {
		v.RetryAt = d.NextAttempt
	}
	if d.State == "suppressed" || d.State == "cancelled" {
		v.Outcome = "Not sent"
	}
	return v
}

// deliveryRecipient names the method and person without exposing a personal destination.
// Shared contact methods keep their destination because they have no member owner.
func deliveryRecipient(d Delivery, viewerID string, names map[string]string) string {
	to := d.SentTo
	if d.MemberID != "" {
		to = memberNameForReader(d.MemberID, viewerID, names)
	}
	return contactMethodTypeName(d) + " to " + to
}

func memberNameForReader(id, viewerID string, names map[string]string) string {
	if id == viewerID {
		return "you"
	}
	if name, ok := names[id]; ok {
		return name
	}
	return "a former member"
}

const deliveryOutcomeTemplate = `{{define "delivery-outcome"}}{{.Outcome}}{{if not .RetryAt.IsZero}}, tries again at {{clock .RetryAt}}{{end}}{{end}}`

const contactMethodSubscriptionFields = `<div><p class="small muted">Alerts to send through the contact method</p><div class="row contact-choices">{{if not .Facts.SOSOnly}}<label class="field"><span><input type="checkbox" name="severity" value="info" {{if .Info}}checked{{end}}> Info</span></label><label class="field"><span><input type="checkbox" name="severity" value="warn" {{if .Warn}}checked{{end}}> Warning</span></label>{{end}}<label class="field"><span><input type="checkbox" name="severity" value="sos" {{if .SOS}}checked{{end}}> SOS</span></label></div></div><label class="field"><span><input type="checkbox" name="send_while_unavailable_or_muted" value="true" {{if .SendWhileUnavailableOrMuted}}checked{{end}}> {{if .Shared}}Send even when the team is muted{{else if .Facts.WakesPeople}}Send even when I'm unavailable{{else}}Send even when I'm unavailable or the team is muted{{end}}</span></label>{{if .Facts.WakesPeople}}<p class="faint small">Team mute always stops this contact method.</p>{{end}}`

const contactMethodPanelTemplate = `{{define "contact-method"}}{{$csrf := .CSRF}}<section class="panel">
<div class="panel-head"><h3>{{.Facts.CardTitle}}</h3><span class="small faint">{{if .Shared}}Shared with the team{{else if .CanEdit}}Yours{{else}}Owned by {{.OwnerName}}{{end}}</span></div><div class="panel-body contact-panel">
<p class="contact-destination">{{.SentTo}}</p><p class="small{{with .LastTest}}{{if or (eq .State "failed") (eq .State "uncertain")}} contact-problem{{else}} muted{{end}}{{else}} faint{{end}}">Last test: {{with .LastTestView}}<a href="/deliveries/{{.ID}}">{{template "delivery-outcome" .}}</a>{{with .ReasonText}}: {{.}}{{end}}{{else}}Not tested yet{{end}}</p>
{{if .CanEdit}}<form class="stack" method="post" action="/contact-methods/save"><input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="contact_method_type" value="{{.ContactMethodType}}"><input type="hidden" name="revision" value="{{.Revision}}">{{if .Shared}}<input type="hidden" name="shared_with_team" value="true">{{end}}{{if and (not .Facts.ConnectsThroughSendingService) (not .Facts.ValueFixed)}}<label class="field">{{.Facts.ValueLabel}}<input type="{{.Facts.ValueInputType}}" name="{{.Facts.ValueField}}" value="{{.Value}}" required></label>{{end}}` + contactMethodSubscriptionFields + `<button class="btn primary">Save {{.CardTitleInSentence}}</button></form>
<div class="row"><form method="post" action="/contact-methods/test"><input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{.ID}}"><button class="btn">Send test</button></form><form method="post" action="/contact-methods/remove"><input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="revision" value="{{.Revision}}"><button class="btn quiet">Remove</button></form></div>
{{if .SigningSecretIDs}}{{$method := .}}<details class="fold"><summary>Webhook signing secrets and bearer token</summary><div class="stack"><p class="small muted">The oldest signing secret signs every request. To rotate signing secrets, add a second signing secret, let your receiver accept the old and new signing secrets, then revoke the old signing secret.</p>{{range $index, $id := .SigningSecretIDs}}<form class="stack" method="post" action="/contact-methods/outgoing-webhook-signing-secrets/revoke"><input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{$method.ID}}"><input type="hidden" name="signing_secret_id" value="{{$id}}"><p><code>{{$id}}</code>{{if eq $index 0}} (signs requests){{end}}</p>{{if gt (len $method.SigningSecretIDs) 1}}<button class="btn">Revoke this webhook signing secret</button>{{end}}</form>{{end}}{{if lt (len .SigningSecretIDs) 2}}<form method="post" action="/contact-methods/outgoing-webhook-signing-secrets"><input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{.ID}}"><button class="btn">Add a second webhook signing secret (for rotation)</button></form>{{end}}<form class="stack" method="post" action="/contact-methods/outgoing-webhook-bearer-token"><input type="hidden" name="csrf" value="{{$csrf}}"><input type="hidden" name="id" value="{{.ID}}"><label class="field">Webhook bearer token (optional)<input name="outgoing_webhook_bearer_token" autocomplete="off" placeholder="Leave empty to remove the token"></label><button class="btn">Save webhook bearer token</button></form></div></details>{{end}}
{{else if .Shared}}<p class="faint small">Only administrators can edit shared contact methods.</p>{{else}}<p class="faint small">Only {{.OwnerName}} can edit this personal contact method.</p>{{end}}</div></section>{{end}}`

const contactMethodPageBody = deliveryOutcomeTemplate + contactMethodPanelTemplate + `{{$page := .}}<div class="stack contacts-page">
{{if .Error}}<p class="contact-note contact-problem" role="alert">{{.Error}}</p>{{end}}{{if .Notice}}<p class="contact-note" role="status">{{.Notice}}</p>{{end}}
{{with .ShownSecret}}<section class="panel"><div class="panel-head"><h2>Save your webhook signing secret now</h2></div><div class="panel-body"><p>The webhook signing secret is shown only once.</p><p>Signing secret ID: <code>{{.SigningSecretID}}</code></p><p>Webhook signing secret (hex): <code>{{.SecretHex}}</code></p><p class="small muted">Each request carries <code>Mmhotline-Signature: timestamp_unix_seconds=…,hmac_sha256_hex=…</code>. Decode the hex secret into 32 bytes and compute HMAC-SHA256 over the timestamp, a dot, and the exact request body.</p></div></section>{{end}}
{{if .ContactCard.Available}}{{if .ContactCard.Changed}}<p class="contact-note" role="status">MMHotline now calls from new numbers. <a href="/mmhotline-contact-card.vcf">Download the MMHotline contact card</a> again and allow the MMHotline contact through Do Not Disturb.</p>{{else if and .HasAutomatedVoiceCall .ContactCard.NeverDownloaded}}<section class="panel"><div class="panel-head"><h2>Make sure SOS calls wake you</h2></div><div class="panel-body"><ol><li><a href="/mmhotline-contact-card.vcf">Download the MMHotline contact card</a> and add the MMHotline contact to your contacts.</li><li>Allow the MMHotline contact through Do Not Disturb. On iPhone, also turn on Emergency Bypass for the MMHotline contact.</li><li>Turn on Do Not Disturb and press “Send test” on your automated voice call panel.</li></ol></div></section>{{else if .HasPhoneNumber}}<p class="small"><a href="/mmhotline-contact-card.vcf">Download the MMHotline contact card</a></p>{{end}}{{end}}
<section class="stack"><h2>Your contact methods</h2><div class="contact-panels">{{range .ContactMethods}}{{template "contact-method" .}}{{else}}<section class="panel"><div class="panel-body"><p>No contact methods yet.</p><p class="muted small">Add a contact method below to receive alerts.</p></div></section>{{end}}</div></section>
<details class="more"{{if or .Error (not .ContactMethods)}} open{{end}}><summary>Add a contact method</summary><div class="body stack"><p class="small muted">Saving a new contact method queues a test. Alerts can use the saved contact method immediately.</p><details class="fold"><summary>Which alerts should I choose?</summary><div class="small muted"><p>Info is for information only. Warning needs attention but wakes nobody. An SOS is urgent and wakes the person on call until someone claims the incident.</p><p>Each contact method also receives messages when the selected alerts are resolved.</p></div></details>
{{range .MissingSendingServiceSetups}}<p class="small muted">{{if $page.Admin}}<a href="/sending-services/{{.SendingService}}">Set up {{.SendingServiceTitle}}</a>{{else}}Ask an administrator to set up {{.SendingServiceTitle}}{{end}} before connecting a contact method that uses the sending service.</p>{{end}}
<div class="contact-panels">
{{with index .New "email"}}<section class="panel"><div class="panel-head"><h3>Add email contact method</h3></div><form class="panel-body stack" method="post" action="/contact-methods/save"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="contact_method_type" value="email"><label class="field">Email address<input type="email" name="email_address" value="{{.Value}}" required></label>{{if $page.Admin}}<label class="field"><span><input type="checkbox" name="shared_with_team" value="true" {{if .Shared}}checked{{end}}> Shared with the team</span></label><p class="faint small">For a shared contact method, only team mute affects delivery.</p>{{end}}` + contactMethodSubscriptionFields + `<button class="btn primary">Add email</button></form></section>{{end}}
{{with index .New "outgoing_webhook"}}<section class="panel"><div class="panel-head"><h3>Add outgoing webhook contact method</h3></div><form class="panel-body stack" method="post" action="/contact-methods/save"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="contact_method_type" value="outgoing_webhook"><p class="small muted">MMHotline sends each alert as signed JSON in a POST request to a public HTTPS URL.</p><label class="field">Outgoing webhook URL<input type="url" name="outgoing_webhook_url" value="{{.Value}}" placeholder="https://example.com/alerts-from-mmhotline" required></label><label class="field">Webhook bearer token (optional)<input name="outgoing_webhook_bearer_token" autocomplete="off"></label>{{if $page.Admin}}<label class="field"><span><input type="checkbox" name="shared_with_team" value="true" {{if .Shared}}checked{{end}}> Shared with the team</span></label>{{end}}` + contactMethodSubscriptionFields + `<button class="btn primary">Add outgoing webhook</button></form></section>{{end}}
{{if and .PhoneNumbersAvailable (index .New "automated_voice_call")}}<section class="panel"><div class="panel-head"><h3>Add a phone number</h3></div><form class="panel-body stack" method="post" action="/contact-methods/phone-number"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><p class="small muted">MMHotline calls your number when an SOS needs you. Adding a phone number queues a test call.</p><label class="field">Phone number<input type="tel" name="phone_number_to_call" value="{{.NewPhoneNumber}}" placeholder="+1 202 555 0123" autocomplete="tel" required></label><p class="small muted">{{.PhoneNumberConsent}}</p><button class="btn primary">Add phone number</button></form></section>{{end}}
{{with index .New "pushover_notification"}}<section class="panel"><div class="panel-head"><h3>Add Pushover notification</h3></div><form class="panel-body stack" method="post" action="/contact-methods/save"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="contact_method_type" value="pushover_notification"><p class="small muted">Use your personal Pushover user key. Saving queues an emergency test alert. Acknowledging an SOS in Pushover also claims the incident for you.</p><label class="field">Pushover user key<input name="pushover_user_key" value="{{.Value}}" minlength="30" maxlength="30" pattern="[A-Za-z0-9]{30}" required></label>` + contactMethodSubscriptionFields + `<button class="btn primary">Add Pushover notification</button></form></section>{{end}}
{{with index .New "telegram_message"}}<section class="panel"><div class="panel-head"><h3>Add Telegram message</h3></div><div class="panel-body"><p class="small muted">SOS messages include buttons to claim or resolve the incident.</p><form method="post" action="/connect-telegram"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="telegram_connection" value="personal_chat"><button class="btn primary">Connect Telegram</button></form>{{if $page.Admin}}<form method="post" action="/connect-telegram"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="telegram_connection" value="team_group"><button class="btn">Add the MMHotline bot to a Telegram group</button></form>{{end}}</div></section>{{end}}
{{with index .New "slack_message"}}<section class="panel"><div class="panel-head"><h3>Add Slack message</h3></div><div class="panel-body"><p class="small muted">SOS messages include buttons to claim or resolve the incident.</p><form method="post" action="/connect-slack"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="slack_connection" value="personal_direct_message"><button class="btn primary">Connect Slack</button></form>{{if $page.Admin}}<form method="post" action="/connect-slack"><input type="hidden" name="csrf" value="{{$page.CSRF}}"><input type="hidden" name="slack_connection" value="shared_channel"><button class="btn">Add a shared Slack channel</button></form>{{end}}</div></section>{{end}}
</div></div></details>
<section class="panel"><div class="panel-head"><h2>Recent notifications</h2><span class="faint small">Newest first · up to 50 records</span></div>{{template "notification-list" .Deliveries}}{{with .OlderDeliveries}}<div class="panel-body"><details class="fold"><summary>Older notifications ({{len .}})</summary><div>{{template "notification-list" .}}</div></details></div>{{end}}<p class="panel-foot">Acceptance means the sending service or webhook receiver took the request. Acceptance does not prove a person read the notification or claimed the incident.</p></section>
{{with .OtherContactMethods}}<details class="more"><summary>Other people's contact methods ({{len .}})</summary><div class="body contact-panels">{{range .}}{{template "contact-method" .}}{{end}}</div></details>{{end}}
</div>
{{define "notification-list"}}<ul class="contact-notifications">{{range .}}<li><div class="row spread"><strong class="small">{{.PurposeLabel}}</strong><time class="faint small">{{clock .CreatedAt}}</time></div><p class="small contact-destination">{{.Recipient}}</p><p class="small"><a href="/deliveries/{{.ID}}">{{template "delivery-outcome" .}}</a>{{with .ReasonText}}: {{.}}{{end}}</p></li>{{else}}<li class="muted small">No delivery records yet.</li>{{end}}</ul>{{end}}`

func (a *App) viewContactMethod(ctx context.Context, p Principal, c ContactMethod, m Member) contactMethodView {
	facts, _ := contactMethodTypeFacts(c.ContactMethodType)
	v := contactMethodView{ContactMethod: c, Facts: facts, Value: c.Setting(facts.ValueField), CanEdit: canEditContactMethod(m, c), Info: containsMember(c.Subscriptions, "info"), Warn: containsMember(c.Subscriptions, "warn"), SOS: containsMember(c.Subscriptions, "sos"), Shared: c.MemberID == ""}
	v.CardTitleInSentence = cardTitleInSentence(facts)
	if c.LastTest != nil {
		last := viewDelivery(*c.LastTest, m.ID, nil)
		v.LastTestView = &last
	}
	if c.ContactMethodType == "outgoing_webhook" && v.CanEdit {
		v.SigningSecretIDs, _ = a.WebhookSigningSecretIDs(ctx, p, c.ID)
	}
	return v
}
func (a *App) contactMethodPrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, err := a.BrowserPrincipal(r)
	if err != nil {
		a.browserAccessError(w, r, err)
		return p, false
	}
	return p, true
}
func (a *App) contactMethodsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodPrincipal(w, r)
	if !ok {
		return
	}
	a.renderContactMethods(w, r, p, 200, "", nil, nil)
}
func (a *App) renderContactMethods(w http.ResponseWriter, r *http.Request, p Principal, status int, message string, in *ContactMethodInput, shown *WebhookSigningSecretShownOnce) {
	a.renderContactMethodsPage(w, r, p, status, message, in, shown, "", "")
}

// renderContactMethodsWithPhoneNumber shows the page after adding a phone
// number: a notice, or an error with the typed number kept in the form.
func (a *App) renderContactMethodsWithPhoneNumber(w http.ResponseWriter, r *http.Request, p Principal, status int, message, notice, typedPhoneNumber string) {
	a.renderContactMethodsPage(w, r, p, status, message, nil, nil, notice, typedPhoneNumber)
}

func (a *App) renderContactMethodsPage(w http.ResponseWriter, r *http.Request, p Principal, status int, message string, in *ContactMethodInput, shown *WebhookSigningSecretShownOnce, notice, typedPhoneNumber string) {
	m, err := a.CurrentMember(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	contactMethods, err := a.ContactMethods(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	deliveries, err := a.Deliveries(r.Context(), p, 50)
	if err != nil {
		HTTPError(w, err)
		return
	}
	fresh := contactMethodView{Info: true, Warn: true, SOS: true, ContactMethod: ContactMethod{SendWhileUnavailableOrMuted: true}}
	data := contactMethodPageData{CSRF: a.CSRFToken(w, r), Error: message, Admin: m.Role == RoleAdministrator, New: map[string]*contactMethodView{}, ShownSecret: shown, PhoneNumbersAvailable: a.twilioSendingService() != nil, PhoneNumberConsent: phoneNumberConsentText}
	people, err := a.membersForHistory(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	owners := map[string]string{}
	for _, person := range people {
		owners[person.ID] = person.Name
	}
	// The query already returns the newest 50 records; fold older rows without a second query.
	for i, d := range deliveries {
		if i < 5 {
			data.Deliveries = append(data.Deliveries, viewDelivery(d, m.ID, owners))
		} else {
			data.OlderDeliveries = append(data.OlderDeliveries, viewDelivery(d, m.ID, owners))
		}
	}
	if err = a.Store.WithTransaction(r.Context(), func(tx *sql.Tx) error {
		var e error
		data.ContactCard, e = a.contactCardState(r.Context(), tx, m.ID)
		return e
	}); err != nil {
		HTTPError(w, err)
		return
	}
	for _, c := range contactMethods {
		facts, _ := contactMethodTypeFacts(c.ContactMethodType)
		if c.MemberID != m.ID || !facts.ValueFixed {
			continue
		}
		data.HasPhoneNumber = true
		data.HasAutomatedVoiceCall = true
	}
	for _, c := range contactMethodTypes {
		// A contact method type connected through its sending service can be added only when
		// that sending service is set up on this server.
		if _, needsSetup := c.(needsSendingService); (needsSetup || c.Facts().ConnectsThroughSendingService) && a.configuredContactMethodTypes()[c.Name()] == nil {
			if owner, needsSetup := c.(needsSendingService); needsSetup {
				definition := owner.Setup()
				data.MissingSendingServiceSetups = append(data.MissingSendingServiceSetups, SendingServiceSetup{SendingService: definition.Name, SendingServiceTitle: definition.Title})
			}
			continue
		}
		form := fresh
		form.Facts = c.Facts()
		if c.Facts().WakesPeople {
			form.SendWhileUnavailableOrMuted = false
		}
		if c.Facts().SOSOnly {
			form.Info = false
			form.Warn = false
		}
		data.New[c.Name()] = &form
	}
	switch r.URL.Query().Get("done") {
	case "saved":
		data.Notice = "Contact method saved."
	case "test":
		data.Notice = "Test delivery queued. The test delivery outcome appears below."
	case "removed":
		data.Notice = "Contact method removed."
	case "secret_revoked":
		data.Notice = "Webhook signing secret revoked."
	case "bearer_token":
		data.Notice = "Webhook bearer token saved."

	}
	if notice != "" {
		data.Notice = notice
	}
	data.NewPhoneNumber = typedPhoneNumber
	for _, c := range contactMethods {
		v := a.viewContactMethod(r.Context(), p, c, m)
		if in != nil && in.ID == c.ID && v.CanEdit {
			v.Value = in.Fields[v.Facts.ValueField]
			v.SendWhileUnavailableOrMuted = in.SendWhileUnavailableOrMuted
			v.Info = containsMember(in.Subscriptions, "info")
			v.Warn = containsMember(in.Subscriptions, "warn")
			v.SOS = containsMember(in.Subscriptions, "sos")
		}
		v.OwnerName = owners[c.MemberID]
		v.CSRF = data.CSRF
		if c.MemberID != "" && c.MemberID != m.ID {
			data.OtherContactMethods = append(data.OtherContactMethods, v)
		} else {
			data.ContactMethods = append(data.ContactMethods, v)
		}
	}
	if in != nil && in.ID == "" {
		if facts, known := contactMethodTypeFacts(in.ContactMethodType); known {
			data.New[in.ContactMethodType] = &contactMethodView{Facts: facts, ContactMethod: ContactMethod{SendWhileUnavailableOrMuted: in.SendWhileUnavailableOrMuted}, Value: in.Fields[facts.ValueField], Shared: in.Shared, Info: containsMember(in.Subscriptions, "info"), Warn: containsMember(in.Subscriptions, "warn"), SOS: containsMember(in.Subscriptions, "sos")}
		}
	}
	RenderPage(w, status, "Contact methods", contactMethodPageBody, data)
}
func (a *App) contactMethodMutation(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := a.contactMethodPrincipal(w, r)
	if !ok {
		return p, false
	}
	if err := a.CheckBrowserMutation(w, r); err != nil {
		HTTPError(w, err)
		return p, false
	}
	return p, true
}
func revisionForm(r *http.Request) (int64, error) {
	v := r.PostForm.Get("revision")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, ErrInvalid
	}
	return n, nil
}
func (a *App) saveContactMethodHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	revision, err := revisionForm(r)
	if err != nil {
		HTTPError(w, err)
		return
	}
	fields := map[string]string{}
	if facts, known := contactMethodTypeFacts(r.PostForm.Get("contact_method_type")); known {
		for _, name := range facts.SettingsFields {
			if value, ok := r.PostForm[name]; ok && len(value) == 1 {
				fields[name] = value[0]
			}
		}
	}
	in := ContactMethodInput{ID: r.PostForm.Get("id"), ContactMethodType: r.PostForm.Get("contact_method_type"), Fields: fields, ExpectedRevision: revision, Shared: r.PostForm.Get("shared_with_team") == "true", SendWhileUnavailableOrMuted: r.PostForm.Get("send_while_unavailable_or_muted") == "true", Subscriptions: r.PostForm["severity"]}
	saved, err := a.SaveContactMethod(r.Context(), p, in)
	if err != nil {
		a.renderContactMethods(w, r, p, errorStatus(err), publicError(err), &in, nil)
		return
	}
	if shown := webhookSecretShownOnce(saved); shown != nil {
		a.renderContactMethods(w, r, p, 200, "", nil, shown)
		return
	}
	if saved.TestNotSentReason != "" {
		a.renderContactMethodsWithPhoneNumber(w, r, p, 200, "", "Contact method saved. "+saved.TestNotSentReason, "")
		return
	}
	http.Redirect(w, r, "/contact-methods?done=saved", http.StatusSeeOther)
}
func (a *App) removeContactMethodHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	revision, err := revisionForm(r)
	if err == nil {
		err = a.RemoveContactMethod(r.Context(), p, r.PostForm.Get("id"), revision)
	}
	if err != nil {
		a.renderContactMethods(w, r, p, errorStatus(err), publicError(err), nil, nil)
		return
	}
	http.Redirect(w, r, "/contact-methods?done=removed", http.StatusSeeOther)
}
func (a *App) testContactMethodHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	if err := a.SendContactMethodTest(r.Context(), p, r.PostForm.Get("id")); err != nil {
		a.renderContactMethods(w, r, p, errorStatus(err), publicError(err), nil, nil)
		return
	}
	http.Redirect(w, r, "/contact-methods?done=test", http.StatusSeeOther)
}
func (a *App) deliveryPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodPrincipal(w, r)
	if !ok {
		return
	}
	selected, err := a.DeliveryByID(r.Context(), p, r.PathValue("id"))
	if err != nil {
		HTTPError(w, err)
		return
	}
	attempts, err := a.DeliveryAttempts(r.Context(), p, selected.ID)
	if err != nil {
		HTTPError(w, err)
		return
	}
	member, err := a.CurrentMember(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	people, err := a.membersForHistory(r.Context(), p)
	if err != nil {
		HTTPError(w, err)
		return
	}
	names := map[string]string{}
	for _, person := range people {
		names[person.ID] = person.Name
	}
	RenderPage(w, 200, "Delivery outcome", deliveryPageBody, struct {
		Delivery deliveryPageView
		Attempts []DeliveryAttempt
	}{viewDelivery(selected, member.ID, names), attempts})
}

func (a *App) addWebhookSigningSecretHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	shown, err := a.AddWebhookSigningSecret(r.Context(), p, r.PostForm.Get("id"))
	if err != nil {
		a.renderContactMethods(w, r, p, errorStatus(err), publicError(err), nil, nil)
		return
	}
	a.renderContactMethods(w, r, p, 200, "", nil, &shown)
}
func (a *App) revokeWebhookSigningSecretHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	if err := a.RevokeWebhookSigningSecret(r.Context(), p, r.PostForm.Get("id"), r.PostForm.Get("signing_secret_id")); err != nil {
		a.renderContactMethods(w, r, p, errorStatus(err), publicError(err), nil, nil)
		return
	}
	http.Redirect(w, r, "/contact-methods?done=secret_revoked", http.StatusSeeOther)
}
func (a *App) webhookBearerTokenHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	if err := a.SetWebhookBearerToken(r.Context(), p, r.PostForm.Get("id"), r.PostForm.Get("outgoing_webhook_bearer_token")); err != nil {
		a.renderContactMethods(w, r, p, errorStatus(err), publicError(err), nil, nil)
		return
	}
	http.Redirect(w, r, "/contact-methods?done=bearer_token", http.StatusSeeOther)
}

// connectTelegramHTTP makes a single-use Telegram start link and shows it.
// The page links to Telegram instead of redirecting, because the browser's
// form-action rule allows form responses to go to this site only.
func (a *App) connectTelegramHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	connection := r.PostForm.Get("telegram_connection")
	link, err := a.StartTelegramConnection(r.Context(), p, connection)
	if err != nil {
		a.renderContactMethods(w, r, p, errorStatus(err), publicError(err), nil, nil)
		return
	}
	RenderPage(w, 200, "Connect Telegram", `<div class="stack"><section class="panel"><div class="panel-head"><h2>{{if .Group}}Connect a team group{{else}}Connect your Telegram chat{{end}}</h2></div><div class="panel-body">{{if .Group}}<p>Open the Telegram link, choose the Telegram group, and add the MMHotline bot. Team alerts then arrive in the selected Telegram group.</p>{{else}}<p>Open the Telegram link on the device where you use Telegram and press Start. Your alerts then arrive in the connected Telegram chat.</p>{{end}}<p><a class="btn primary" href="{{.Link}}">Open Telegram</a></p><p class="faint small">The Telegram link works once, for 15 minutes.</p></div></section><p><a href="/contact-methods">Back to contact methods</a></p></div>`, struct {
		Link  string
		Group bool
	}{link, connection == "team_group"})
}

const deliveryPageBody = deliveryOutcomeTemplate + `<div class="stack delivery-page"><section class="panel"><div class="panel-head"><h2>{{with .Delivery}}{{template "delivery-outcome" .}}{{end}}</h2></div><div class="panel-body">{{with .Delivery}}{{with .ReasonText}}<p>{{capitalize .}}</p>{{end}}<p class="contact-destination">{{.Recipient}}</p>{{with .IncidentID}}<p><a href="/incidents/{{.}}">Back to the incident</a></p>{{end}}<dl class="kv"><dt>Purpose</dt><dd>{{.PurposeLabel}}</dd><dt>Queued</dt><dd>{{clock .CreatedAt}}, {{relativeTime .CreatedAt}}</dd>{{if .CombinedInto}}<dt>{{if eq .State "combined"}}Sent in{{else}}Combined into{{end}}</dt><dd><a href="/deliveries/{{.CombinedInto}}">The combined message</a></dd>{{end}}</dl>{{if and (or (eq .State "accepted") (eq .State "delivered")) (ne .CallResult "calling")}}<p class="faint small">Acceptance does not prove a person read the notification or claimed the incident.</p>{{end}}{{end}}</div></section>
<section class="panel"><div class="panel-head"><h2>Message as sent</h2></div><div class="panel-body"><dl class="kv"><dt>Subject</dt><dd>{{.Delivery.Subject}}</dd></dl><pre class="contact-message">{{.Delivery.Message}}</pre></div></section>
<details class="more"><summary>Delivery attempts ({{len .Attempts}})</summary><div class="body"><ol class="contact-notifications">{{range .Attempts}}<li><div class="row spread"><strong>Attempt {{.Number}}: {{.OutcomeLabel}}</strong><span class="faint small">{{clock .StartedAt}}</span></div>{{if not .CompletedAt.IsZero}}<p class="faint small">Finished {{clock .CompletedAt}}</p>{{end}}{{with .Diagnostic}}<pre>{{.}}</pre>{{end}}</li>{{else}}<li class="muted small">No external attempt yet.</li>{{end}}</ol></div></details></div>`
