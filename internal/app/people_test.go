package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func foundationApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	if err := Setup(context.Background(), dir, "Fixture team"); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := New(s)
	recordCommitSigningKeyForTest(t, a)
	a.WebURL = "https://mmhotline.example.com"
	a.WebListenAddress = "127.0.0.1:8480"
	a.AlertIntakeListenAddress = "10.20.0.1:8484"
	configureAccessSignIn(t, a)
	t.Cleanup(func() { a.Store.Close(); os.RemoveAll(dir) })
	recordEmailSetUpForTest(t, s)
	return a
}

// recordEmailSetUpForTest records an email sending service setup, as an administrator's
// setup would, so that tests of other behavior can save email contact methods.
// It contacts no mail server; tests that send email set their own contact method type.
func recordEmailSetUpForTest(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO sending_service_setups(sending_service,settings_json,secret_settings_json,set_up_at,set_up_by) VALUES('mail_server','{"smtp_server":"127.0.0.1:25","smtp_connection_security":"unencrypted","smtp_user_name":"","email_from_address":"alerts@example.com"}','{"smtp_password":""}',0,'a test fixture') ON CONFLICT(sending_service) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
}

func browserClient(s *httptest.Server) *http.Client {
	clientValue := *s.Client()
	c := &clientValue
	c.Jar, _ = cookiejar.New(nil)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([A-Za-z0-9_-]+)"`)

func pageToken(t *testing.T, c *http.Client, base, path string) string {
	t.Helper()
	res, err := c.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, res.StatusCode, b)
	}
	matches := csrfPattern.FindSubmatch(b)
	if len(matches) != 2 {
		t.Fatalf("no csrf token: %s", b)
	}
	return string(matches[1])
}
func postForm(t *testing.T, c *http.Client, base, path string, v url.Values, want int) *http.Response {
	t.Helper()
	res, err := c.PostForm(base+path, v)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("POST %s: %d want %d: %s", path, res.StatusCode, want, b)
	}
	return res
}

func TestSetupRefusesExistingState(t *testing.T) {
	a := foundationApp(t)
	s := memberFixture(t, a, "keep@example.test")
	if err := Setup(context.Background(), a.Store.Dir, "Fixture team"); err == nil {
		t.Fatal("setup replaced deployment")
	}
	if _, err := a.CurrentMember(context.Background(), s.Principal); err != nil {
		t.Fatal(err)
	}
}

func TestSetupAndOpenWithARelativeStateDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := Setup(context.Background(), "state", "Fixture team"); err != nil {
		t.Fatal(err)
	}
	s, err := Open("state")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestSetupTeamNameAndSingleTeam(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"", "  ", strings.Repeat("x", 101), string([]byte{0xff})} {
		if err := Setup(ctx, t.TempDir(), name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("team name %q: %v", name, err)
		}
	}
	dir := t.TempDir()
	name := strings.Repeat("界", 100)
	if err := Setup(ctx, dir, name); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var storedName string
	if err := store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT name FROM team_info`).Scan(&storedName)
	}); err != nil || storedName != name {
		t.Fatalf("team name: %q %v", storedName, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	databases := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".db") {
			databases++
		}
	}
	if databases != 1 {
		t.Fatalf("created %d databases, want 1", databases)
	}
}

func TestOpenRequiresExactlyOneTeam(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			a := foundationApp(t)
			if count == 0 {
				if _, err := a.Store.db.Exec(`DELETE FROM team_info`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := a.Store.db.Exec(`INSERT INTO team_info(name) VALUES(?)`, "Second team"); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(a.Store.Dir)
			if err == nil {
				store.Close()
				t.Fatal("opened state without exactly one team")
			}
			if !strings.Contains(err.Error(), "run setup") {
				t.Fatal(err)
			}
		})
	}
}

type personFixture struct{ Principal Principal }

func memberFixture(t *testing.T, a *App, signInEmailAddress string) personFixture {
	t.Helper()
	if a.AccessVerifier == nil {
		configureAccessSignIn(t, a)
	}
	principal := Principal{UnixUser: "fixture"}
	people, err := a.TeamMembers(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	role := RoleMember
	if len(people) == 0 {
		role = RoleAdministrator
	}
	member := addPerson(t, a, signInEmailAddress, "Responder", role)
	return personFixture{Principal: Principal{SignInEmailAddress: member.SignInEmailAddress}}
}

// changePeople changes the team the way production does, through the
// settings file's people part as mmhotline settings apply runs it: the
// file's check, then the part. change gets the team's people in the file's
// form and returns the people the file lists.
func changePeople(t *testing.T, a *App, change func([]SettingsPerson) []SettingsPerson) error {
	t.Helper()
	ctx := context.Background()
	members, err := a.TeamMembers(ctx, Principal{UnixUser: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var people []SettingsPerson
	for _, m := range members {
		people = append(people, SettingsPerson{Name: m.Name, SignInEmailAddress: m.SignInEmailAddress, Role: m.Role})
	}
	people = change(people)
	problems := &settingsProblems{}
	checkPeopleSettings(people, problems)
	if len(problems.list) > 0 {
		return fmt.Errorf("%w: %s %s", ErrInvalid, problems.list[0].KeyPath, problems.list[0].Problem)
	}
	// The part applies as the settings file, which a test App may not name.
	if a.settingsFile.path == "" {
		a.settingsFile.path = "/etc/mmhotline/mmhotline_settings.yml"
		defer func() { a.settingsFile.path = "" }()
	}
	return applyPeopleSettings(ctx, a, Settings{People: people})
}

// addPerson adds one person to the settings file's people and applies it.
func addPerson(t *testing.T, a *App, signInEmailAddress, name, role string) Member {
	t.Helper()
	if err := changePeople(t, a, func(people []SettingsPerson) []SettingsPerson {
		return append(people, SettingsPerson{Name: name, SignInEmailAddress: signInEmailAddress, Role: role})
	}); err != nil {
		t.Fatal(err)
	}
	address, err := normalizeSignInEmailAddress(signInEmailAddress)
	if err != nil {
		t.Fatal(err)
	}
	var m Member
	if err = a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		m, err = personBySignInEmailAddress(context.Background(), tx, address)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

// removePerson leaves one person out of the settings file's people and
// applies it, which deactivates the person or says why not.
func removePerson(t *testing.T, a *App, signInEmailAddress string) error {
	t.Helper()
	return changePeople(t, a, func(people []SettingsPerson) []SettingsPerson {
		return slices.DeleteFunc(people, func(person SettingsPerson) bool {
			return strings.EqualFold(person.SignInEmailAddress, signInEmailAddress)
		})
	})
}

// changePerson changes one person's name or role in the settings file's
// people and applies it; an empty name or role keeps the person's.
func changePerson(t *testing.T, a *App, signInEmailAddress, name, role string) error {
	t.Helper()
	return changePeople(t, a, func(people []SettingsPerson) []SettingsPerson {
		for index, person := range people {
			if strings.EqualFold(person.SignInEmailAddress, signInEmailAddress) {
				if name != "" {
					people[index].Name = name
				}
				if role != "" {
					people[index].Role = role
				}
			}
		}
		return people
	})
}

type signInTransport struct {
	next               http.RoundTripper
	signInEmailAddress string
}

func (transport signInTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	addAccessToken(request, transport.signInEmailAddress)
	return transport.next.RoundTrip(request)
}
func useSignInEmailAddress(client *http.Client, signInEmailAddress string) {
	next := client.Transport
	if current, ok := next.(signInTransport); ok {
		next = current.next
	}
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = signInTransport{next: next, signInEmailAddress: signInEmailAddress}
}

func TestSignInAccessAndRemovalOnNextRequest(t *testing.T) {
	a := foundationApp(t)
	a.WebURL = "https://util01.example-tailnet.ts.net"
	memberFixture(t, a, "admin@example.test")
	member := addPerson(t, a, "Alice@Example.com", "Alice <Example>", RoleMember)
	handler := a.Handler()
	request := func(path, signInEmailAddress string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "https://mmhotline.example.com"+path, nil)
		if signInEmailAddress != "" {
			addAccessToken(r, signInEmailAddress)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/dashboard", "/people", "/on-call", "/alert-sources", "/alerts", "/contact-methods", "/incidents/one"} {
		w := request(path, "")
		if w.Code != 401 || !strings.Contains(w.Body.String(), a.WebURL) || !strings.Contains(w.Body.String(), "create-sign-in-link") || strings.Contains(w.Body.String(), member.Name) || strings.Contains(w.Body.String(), member.SignInEmailAddress) {
			t.Fatalf("anonymous %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := request("/dashboard", "stranger@example.com")
	for _, expected := range []string{"stranger@example.com", "Fixture team", "add your sign-in email address to <code>people</code> in MMHotline's settings file", "apply the file."} {
		if w.Code != 403 || !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("unknown: %d %s", w.Code, w.Body.String())
		}
	}
	w = request("/people", "ALICE@EXAMPLE.COM")
	if w.Code != 200 || !strings.Contains(pageText(w.Body.String()), "Alice <Example> (you)") || strings.Contains(w.Body.String(), "Add a person") {
		t.Fatalf("member page: %d %s", w.Code, w.Body.String())
	}
	if err := removePerson(t, a, member.SignInEmailAddress); err != nil {
		t.Fatal(err)
	}
	w = request("/people", "ALICE@EXAMPLE.COM")
	if w.Code != 403 {
		t.Fatalf("removed member next request: %d", w.Code)
	}
	if _, err := a.CurrentCoverage(context.Background(), Principal{SignInEmailAddress: member.SignInEmailAddress}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed principal: %v", err)
	}
}

// The settings file's people keep each sign-in email address once, as
// MMHotline saves it, and keep an administrator. The role the file gives
// decides what a person may do at the person's next request.
func TestPeopleLastAdministratorAndDuplicateSignInEmailAddress(t *testing.T) {
	a := foundationApp(t)
	ctx := context.Background()
	admin := memberFixture(t, a, "admin@example.test").Principal
	person := addPerson(t, a, "  Alice@Example.com  ", "Alice", RoleMember)
	if person.SignInEmailAddress != "alice@example.com" {
		t.Fatalf("add: %+v", person)
	}
	if err := changePeople(t, a, func(people []SettingsPerson) []SettingsPerson {
		return append(people, SettingsPerson{Name: "Other", SignInEmailAddress: "ALICE@EXAMPLE.COM", Role: RoleMember})
	}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "alice@example.com is also the sign-in email address of people[") {
		t.Fatalf("duplicate: %v", err)
	}
	if err := changePerson(t, a, admin.SignInEmailAddress, "", RoleMember); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "needs at least one person with role administrator") {
		t.Fatalf("last administrator demotion: %v", err)
	}
	if err := removePerson(t, a, admin.SignInEmailAddress); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "needs at least one person with role administrator") {
		t.Fatalf("last administrator removal: %v", err)
	}
	requireAdministrator := func(p Principal) error {
		return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			_, err := RequireMember(ctx, tx, p, true)
			return err
		})
	}
	if err := requireAdministrator(Principal{SignInEmailAddress: person.SignInEmailAddress}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member as administrator: %v", err)
	}
	if err := changePeople(t, a, func(people []SettingsPerson) []SettingsPerson {
		for index := range people {
			people[index].Role = map[string]string{admin.SignInEmailAddress: RoleMember, person.SignInEmailAddress: RoleAdministrator}[people[index].SignInEmailAddress]
		}
		return people
	}); err != nil {
		t.Fatal(err)
	}
	if err := requireAdministrator(Principal{SignInEmailAddress: person.SignInEmailAddress}); err != nil {
		t.Fatalf("promoted person: %v", err)
	}
	if err := requireAdministrator(admin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("demoted principal still administrator: %v", err)
	}
}

func TestPeopleInputValidationAndTransactionRollback(t *testing.T) {
	a := foundationApp(t)
	ctx := context.Background()
	admin := memberFixture(t, a, "admin@example.test").Principal
	for _, signInEmailAddress := range []string{"a@b", strings.Repeat("x", 242) + "@example.com"} {
		if person := addPerson(t, a, signInEmailAddress, "Valid address", RoleMember); person.SignInEmailAddress != signInEmailAddress {
			t.Fatalf("valid sign-in email address %q: %+v", signInEmailAddress, person)
		}
	}
	add := func(signInEmailAddress, name, role string) error {
		return changePeople(t, a, func(people []SettingsPerson) []SettingsPerson {
			return append(people, SettingsPerson{Name: name, SignInEmailAddress: signInEmailAddress, Role: role})
		})
	}
	for _, signInEmailAddress := range []string{"", "alice", "@", "alice@@example.com", "ali ce@example.com", "@alice", "alice@", "alice\n@example.com", "alice\x00@example.com", strings.Repeat("x", 243) + "@example.com", string([]byte{0xff}) + "@example.com"} {
		if err := add(signInEmailAddress, "Alice", RoleMember); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), ".sign_in_email_address") {
			t.Fatalf("sign-in email address %q: %v", signInEmailAddress, err)
		}
	}
	for _, name := range []string{"", "  ", strings.Repeat("x", 101), string([]byte{0xff})} {
		if err := add("alice@example.com", name, RoleMember); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), ".name") {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if err := add("alice@example.com", "Alice", "owner"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), ".role") {
		t.Fatalf("role: %v", err)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM members WHERE sign_in_email_address='alice@example.com'`); n != 0 {
		t.Fatalf("a refused person was saved: %d", n)
	}
	failure := errors.New("rollback")
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec(`UPDATE members SET name='unsaved'`); e != nil {
			return e
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	m, err := a.CurrentMember(ctx, admin)
	if err != nil || m.Name != "Responder" {
		t.Fatalf("partial transaction: %+v %v", m, err)
	}
}

func TestRemovingPersonCancelsWorkAndPreservesHistory(t *testing.T) {
	a := foundationApp(t)
	ctx := context.Background()
	admin := memberFixture(t, a, "admin@example.test").Principal
	person := addPerson(t, a, "alice@example.com", "Alice Example", RoleMember)
	p := Principal{SignInEmailAddress: person.SignInEmailAddress}
	savedEmail(t, a, p, true)
	alert, err := a.ManualSOS(ctx, p, "Incident history", "Testing removed member names")
	if err != nil {
		t.Fatal(err)
	}
	incident, err := a.GetIncident(ctx, p, alert.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimIncident(ctx, p, incident.ID, incident.Revision, ""); err != nil {
		t.Fatal(err)
	}
	claim, err := a.SetOnCallClaim(ctx, p, a.Now().Add(time.Hour), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	// A claimed incident reminds only its owner, and the on-call claim
	// decides who is woken: removing their holder is refused.
	if err = removePerson(t, a, person.SignInEmailAddress); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "owns 1 claimed incident") {
		t.Fatalf("removal with a claimed incident: %v", err)
	}
	claimed, err := a.GetIncident(ctx, p, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ResolveIncident(ctx, p, incident.ID, claimed.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if err = removePerson(t, a, person.SignInEmailAddress); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "has taken on-call") {
		t.Fatalf("removal with the on-call claim: %v", err)
	}
	if _, err = a.EndOnCallClaim(ctx, p, claim.Revision); err != nil {
		t.Fatal(err)
	}
	// Queue ordinary work after claiming, so removal must cancel it independently.
	queueDelivery(t, a, p, "queued-for-removed-person", "warn")
	if err = removePerson(t, a, person.SignInEmailAddress); err != nil {
		t.Fatal(err)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM contact_methods WHERE member_id=? AND deleted=0`, person.ID); n != 0 {
		t.Fatalf("remaining personal contact methods: %d", n)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM deliveries WHERE member_id=? AND state='queued'`, person.ID); n != 0 {
		t.Fatalf("remaining queued deliveries: %d", n)
	}
	coverage, err := a.CurrentCoverage(ctx, admin)
	if err != nil || containsMember(coverage.AllMemberIDs, person.ID) {
		t.Fatalf("removed person in coverage: %+v %v", coverage, err)
	}
	history, err := a.IncidentAudit(ctx, admin, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if history[len(history)-1].ActorName != "Alice Example" {
		t.Fatalf("history: %+v", history)
	}
	request := httptest.NewRequest("GET", "https://mmhotline.example.com/incidents/"+incident.ID, nil)
	addAccessToken(request, admin.SignInEmailAddress)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if body := response.Body.String(); response.Code != 200 || !strings.HasPrefix(liveSectionText(body, "incident-state"), "Alice Example resolved the incident at ") || !strings.Contains(liveSectionText(body, "incident-audit"), "Alice Example claimed the incident.") {
		t.Fatalf("incident names: %d %s", response.Code, body)
	}
	request = httptest.NewRequest("GET", "https://mmhotline.example.com/on-call", nil)
	addAccessToken(request, admin.SignInEmailAddress)
	response = httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Alice Example") {
		t.Fatalf("on-call claim lost removed name: %d %s", response.Code, response.Body.String())
	}
	// Re-adding a sign-in email address is a new membership; the removed member's name is immutable.
	if replacement := addPerson(t, a, person.SignInEmailAddress, "New Alice", RoleMember); replacement.ID == person.ID {
		t.Fatalf("new membership: %+v", replacement)
	}
	if _, err := a.ClaimIncidentsFromNotification(ctx, "unused", person.ID, "telegram_button"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed member notification action: %v", err)
	}
}

// The People page lists the team, which the settings file sets: it has no
// form, and a post to it is refused. The page forms people still have keep
// their CSRF and cross-origin checks.
func TestPeoplePageIsReadOnlyAndFormsKeepTheirChecks(t *testing.T) {
	a := foundationApp(t)
	a.UseSettingsFile("/etc/mmhotline/mmhotline_settings.yml", nil)
	admin := memberFixture(t, a, "admin@example.test").Principal
	addPerson(t, a, "alice@example.com", "Alice <Example>", RoleMember)
	server := httptest.NewTLSServer(a.Handler())
	defer server.Close()
	client := browserClient(server)
	useSignInEmailAddress(client, admin.SignInEmailAddress)
	response, err := client.Get(server.URL + "/people")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, text := range []string{"Alice <Example>", "alice@example.com", "Member", "Where people are set", "/etc/mmhotline/mmhotline_settings.yml"} {
		if !strings.Contains(pageText(string(body)), text) {
			t.Fatalf("the People page lacks %q: %s", text, body)
		}
	}
	if strings.Contains(string(body), "<form") {
		t.Fatalf("the People page has a form: %s", body)
	}
	csrf := pageToken(t, client, server.URL, "/on-call")
	postForm(t, client, server.URL, "/people", url.Values{"csrf": {csrf}, "action": {"add"}, "sign_in_email_address": {"bruno@example.com"}, "name": {"Bruno"}, "role": {RoleMember}}, 405).Body.Close()
	if people, err := a.TeamMembers(context.Background(), admin); err != nil || len(people) != 2 {
		t.Fatalf("a post changed the people: %+v %v", people, err)
	}
	form := url.Values{"csrf": {csrf}, "action": {"unavailable"}, "revision": {"0"}, "duration": {"1h"}}
	request, _ := http.NewRequest("POST", server.URL+"/on-call", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	form.Del("csrf")
	postForm(t, client, server.URL, "/on-call", form, 403).Body.Close()
	form.Set("csrf", csrf)
	postForm(t, client, server.URL, "/on-call", form, 303).Body.Close()
}

func TestCSRFCookieLifetimeAndSecurity(t *testing.T) {
	a := foundationApp(t)
	p := memberFixture(t, a, "admin@example.test").Principal
	request := httptest.NewRequest("GET", "https://mmhotline.example.com/on-call", nil)
	addAccessToken(request, p.SignInEmailAddress)
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			if cookie.MaxAge != int(CSRFCookieLifetime.Seconds()) || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
				t.Fatalf("CSRF cookie: %+v", cookie)
			}
			return
		}
	}
	t.Fatal("no CSRF cookie")
}

func TestIntakeOnlyHandlerIgnoresSignInIdentityAndSignsBothRoutes(t *testing.T) {
	a, _, source := alertFixture(t)
	handler := a.IntakeHandler()
	for _, signInEmailAddress := range []string{"", "admin@example.test", "made-up@example.com"} {
		for _, route := range []string{"alert-intake", "check-in"} {
			body := `{"title":"Signed intake","message":""}`
			if route == "check-in" {
				body = `{"sent_at":"` + a.Now().UTC().Format("2006-01-02T15:04:05Z07:00") + `","sent_by_node_name":"util"}`
			}
			request := httptest.NewRequest("POST", "http://private.example.com/"+route+"/"+source.Source.ID, strings.NewReader(body))
			addAccessToken(request, signInEmailAddress)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Mmhotline-Signature", alertSignature(source.Secret, a.Now(), []byte(body)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 || !verifyReceipt(t, source.Secret, route, body, a.Now(), response) {
				t.Fatalf("%s with sign-in email address %q: %d %s", route, signInEmailAddress, response.Code, response.Body.String())
			}
		}
		request := httptest.NewRequest("GET", "http://private.example.com/people", nil)
		addAccessToken(request, signInEmailAddress)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 404 {
			t.Fatalf("intake exposes page: %d", response.Code)
		}
	}
}
