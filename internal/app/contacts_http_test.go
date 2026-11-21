package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestContactPagesExplainOutcomesAndPreserveMessage(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	person := memberFixture(t, a, "outcomes@example.test")
	p := person.Principal
	ctx := context.Background()
	savedEmail(t, a, p, true)
	member, err := a.CurrentMember(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SaveSchedule(ctx, p, ScheduleConfig{Kind: "day_of_week", TimeZone: "America/New_York", Assignments: []string{member.ID, member.ID, member.ID, member.ID, member.ID, member.ID, member.ID}, BackupKind: "everyone_available"}, 0); err != nil {
		t.Fatal(err)
	}
	message := "SOS: Disk pressure\n\nAt: 2026-09-29T12:00:00Z\n  Keep spaces.\n"
	reasons := []string{"sos_escalation step changed", "unavailable", "SOS contact method type follows the active target"}
	var ids []string
	for i := range 8 {
		advance(time.Minute)
		queueDelivery(t, a, p, fmt.Sprintf("page-%d", i), "warn")
		d := findDelivery(t, a, p, fmt.Sprintf("page-%d", i))
		ids = append(ids, d.ID)
		reason := reasons[i%len(reasons)]
		if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE deliveries SET state='suppressed',reason=?,message=? WHERE id=?`, reason, message, d.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string) string {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "https://mmhotline.example.com"+path, nil)
		addAccessToken(r, p.SignInEmailAddress)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	page := get("/contact-methods")
	text := pageText(page)
	for _, want := range []string{"Newest first", "Older notifications (4)", "Not sent", "MMHotline moved on to the next person", "the person is unavailable", "this contact method only wakes the people MMHotline is waking", "08:08"} {
		if !strings.Contains(text, want) {
			t.Errorf("contact methods lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, reasons[0]) || strings.Contains(text, "SOS contact method type follows") {
		t.Fatal("notification list exposes internal reasons")
	}
	if strings.Index(page, "/deliveries/"+ids[7]) > strings.Index(page, "/deliveries/"+ids[6]) {
		t.Fatal("newest notification is not first")
	}
	detail := get("/deliveries/" + ids[7])
	for _, want := range []string{"Not sent", "The person is unavailable", "Message as sent", "Delivery attempts (0)", "08:08"} {
		if !strings.Contains(pageText(detail), want) {
			t.Errorf("delivery detail lacks %q", want)
		}
	}
	if !strings.Contains(detail, message) {
		t.Fatal("delivery page changed the message as sent")
	}
	if strings.Index(detail, "the person is unavailable") > strings.Index(detail, "Message as sent") {
		t.Fatal("message appears before the outcome reason")
	}
	// A scheduled retry uses the team's clock, while the message keeps its UTC timestamp.
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE deliveries SET state='failed',reason='Mail server refused the request',next_attempt=? WHERE id=?`, a.Now().Add(time.Minute).Unix(), ids[7])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	detail = get("/deliveries/" + ids[7])
	if !strings.Contains(pageText(detail), "Failed, tries again at 08:09") {
		t.Fatalf("retry lacks team time: %s", pageText(detail))
	}
}

// contactMethodForm finds the form by its submitted contact method type,
// rather than by heading markup or visual styling.
func contactMethodForm(t *testing.T, page, kind string) string {
	t.Helper()
	for _, form := range regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`).FindAllString(page, -1) {
		if strings.Contains(form, `name="contact_method_type" value="`+kind+`"`) {
			return form
		}
	}
	t.Fatalf("no form for %s", kind)
	return ""
}

func TestContactMethodsPageNamesOwnersAndKeepsPersonalFormsPrivate(t *testing.T) {
	t.Parallel()
	a, admin, people := coverageFixture(t)
	owner := Principal{SignInEmailAddress: people[1].SignInEmailAddress}
	personal := savedEmail(t, a, owner, true)
	shared, err := a.SaveContactMethod(context.Background(), admin, ContactMethodInput{ContactMethodType: "email", Shared: true, Fields: map[string]string{"email_address": "shared@example.test"}, Subscriptions: []string{"warn"}})
	if err != nil {
		t.Fatal(err)
	}
	get := func(p Principal) string {
		t.Helper()
		request := httptest.NewRequest("GET", "https://mmhotline.example.com/contact-methods", nil)
		addAccessToken(request, p.SignInEmailAddress)
		response := httptest.NewRecorder()
		a.Handler().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		return response.Body.String()
	}
	adminPage := get(admin)
	mustContain(t, "administrator's contact methods", pageText(adminPage), "Owned by Backup", "Only Backup can edit this personal contact method.", "Other people's contact methods (1)", "Shared with the team")
	for _, form := range regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`).FindAllString(adminPage, -1) {
		if strings.Contains(form, personal.ID) {
			t.Fatal("administrator has an action on another person's personal contact method")
		}
	}
	memberPage := get(owner)
	mustContain(t, "member's contact methods", pageText(memberPage), "Yours", "Only administrators can edit shared contact methods.")
	for _, form := range regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`).FindAllString(memberPage, -1) {
		if strings.Contains(form, shared.ID) || strings.Contains(form, `name="shared_with_team"`) {
			t.Fatal("member can edit or add a shared contact method")
		}
	}
	emptyPage := get(Principal{SignInEmailAddress: people[2].SignInEmailAddress})
	// A shared method remains visible, even when a member has no personal method.
	mustContain(t, "member without personal methods", pageText(emptyPage), "shared@example.test", "Add a contact method")
	if strings.Contains(pageText(emptyPage), personal.SentTo) {
		t.Fatal("member sees someone else's personal destination")
	}
}

func TestDeliveryPagesNameThePersonAndMethodAndLinkToTheIncident(t *testing.T) {
	t.Parallel()
	a, admin, people := coverageFixture(t)
	owner := Principal{SignInEmailAddress: people[1].SignInEmailAddress}
	method := savedEmail(t, a, owner, true)
	deliveries, err := a.Deliveries(context.Background(), admin, 50)
	if err != nil {
		t.Fatal(err)
	}
	var deliveryID string
	for _, d := range deliveries {
		if d.ContactMethodID == method.ID {
			deliveryID = d.ID
			break
		}
	}
	if deliveryID == "" {
		t.Fatal("saving the contact method did not queue its test")
	}
	get := func(p Principal, path string) string {
		t.Helper()
		r := httptest.NewRequest("GET", "https://mmhotline.example.com"+path, nil)
		addAccessToken(r, p.SignInEmailAddress)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: HTTP %d: %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	for _, viewer := range []struct {
		principal Principal
		recipient string
	}{
		{admin, "Email to Backup"},
		{owner, "Email to you"},
		{Principal{SignInEmailAddress: people[2].SignInEmailAddress}, "Email to Backup"},
	} {
		page := get(viewer.principal, "/contact-methods")
		// Inspect the notifications section, because administrators may see raw
		// destinations separately in the contact method panels.
		var notifications string
		for _, section := range regexp.MustCompile(`(?s)<section\b[^>]*>.*?</section>`).FindAllString(page, -1) {
			if strings.Contains(pageText(section), "Recent notifications") {
				notifications = section
			}
		}
		if !strings.Contains(pageText(notifications), viewer.recipient) || strings.Contains(notifications, method.SentTo) || strings.Contains(notifications, "Sent to private contact method") {
			t.Fatalf("notification has no clear person and method, or reveals a private destination: %s", notifications)
		}
		detail := get(viewer.principal, "/deliveries/"+deliveryID)
		if !strings.Contains(pageText(detail), viewer.recipient) || strings.Contains(detail, method.SentTo) || strings.Contains(pageText(detail), "Back to the incident") {
			t.Fatalf("test delivery names the wrong recipient or offers an unrelated incident: %s", detail)
		}
	}
	incident, err := a.ManualSOS(context.Background(), admin, "Database down", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []struct {
		name, deliveryState, callResult string
		accepted                        bool
	}{
		{"queued", "queued", "", false},
		{"sending", "sending", "", false},
		{"calling", "accepted", "calling", false},
		{"accepted", "accepted", "", true},
		{"delivered", "delivered", "", true},
	} {
		t.Run(state.name, func(t *testing.T) {
			if _, err := a.Store.db.Exec(`UPDATE deliveries SET state=?,call_result=?,incident_id=? WHERE id=?`, state.deliveryState, state.callResult, incident.ID, deliveryID); err != nil {
				t.Fatal(err)
			}
			page := get(admin, "/deliveries/"+deliveryID)
			if !strings.Contains(page, `href="/incidents/`+incident.ID+`"`) {
				t.Fatal("delivery does not link to its incident")
			}
			if strings.Contains(pageText(page), "Acceptance does not prove") != state.accepted {
				t.Fatalf("acceptance explanation is wrong for %s: %s", state.name, pageText(page))
			}
		})
	}
}
