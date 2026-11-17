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
	Device                           string // Pushover device name typed into the add form
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

// lastTestClass colors the "Last test" line: a failed or uncertain test is a problem, and a
// contact method never tested is faint.
func lastTestClass(method contactMethodView) string {
	switch {
	case method.LastTest == nil:
		return "small faint"
	case method.LastTest.State == "failed" || method.LastTest.State == "uncertain":
		return "small contact-problem"
	}
	return "small muted"
}

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
			data.New[in.ContactMethodType] = &contactMethodView{Facts: facts, ContactMethod: ContactMethod{SendWhileUnavailableOrMuted: in.SendWhileUnavailableOrMuted}, Value: in.Fields[facts.ValueField], Device: in.Fields["pushover_device_name"], Shared: in.Shared, Info: containsMember(in.Subscriptions, "info"), Warn: containsMember(in.Subscriptions, "warn"), SOS: containsMember(in.Subscriptions, "sos")}
		}
	}
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, status, "Contact methods", frame, nil, contactMethodsPageBody(frame, data))
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
	if saved.PushoverGroupKey && saved.MemberID != "" {
		if owner, err := a.CurrentMember(r.Context(), p); err == nil {
			a.renderContactMethodsWithPhoneNumber(w, r, p, 200, "", "Contact method saved. "+pushoverGroupKeyWarning(owner.Name)+" "+pushoverUserKeyHelp, "")
			return
		}
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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "Delivery outcome", frame, nil, deliveryPageBody(frame, deliveryPageData{viewDelivery(selected, member.ID, names), attempts}))
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
	frame, ok := readFrameForPage(w)
	if !ok {
		return
	}
	renderPage(w, 200, "Connect Telegram", frame, nil, connectTelegramBody(link, connection == "team_group"))
}
