package app

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

func TestContactCardAndItsChangeNotice(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	fields := fakePhoneSetupFields()
	fields["extra_phone_numbers_on_the_contact_card"] = "+12025550199"
	if _, err := r.a.SetUpSendingService(ctx, p, "twilio", fields); err != nil {
		t.Fatal(err)
	}
	r.sendingServices = r.a.twilioSendingService()
	r.addPhone(t, p, barbaraNumber)
	if err := r.a.AnnounceContactCardChange(ctx); err != nil {
		t.Fatal(err)
	}
	c := r.browser(t)
	res, err := c.Get(r.server.URL + "/mmhotline-contact-card.vcf")
	if err != nil {
		t.Fatal(err)
	}
	card, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/vcard; charset=utf-8" || !strings.Contains(res.Header.Get("Content-Disposition"), "mmhotline-contact-card.vcf") {
		t.Fatalf("contact card answer: %d %v", res.StatusCode, res.Header)
	}
	for _, number := range []string{fakesendingservices.TwilioPhoneNumberToCallFrom, "+12025550199"} {
		if !strings.Contains(string(card), "TEL;TYPE=VOICE:"+number+"\r\n") {
			t.Fatalf("card lacks %s: %s", number, card)
		}
	}
	if !strings.HasPrefix(string(card), "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:MMHotline\r\n") {
		t.Fatalf("card: %s", card)
	}
	page := getPage(t, c, r.server.URL+"/contact-methods")
	if strings.Contains(page, "from new numbers") || strings.Contains(page, "Make sure SOS calls wake you") {
		t.Fatalf("notice before any change: %s", page)
	}
	// New numbers: every member with a phone call is told once, and the page
	// shows a notice until they download the card again.
	fields["twilio_phone_number_to_call_from"] = "+12025550103"
	if _, err := r.a.SetUpSendingService(ctx, p, "twilio", fields); err != nil {
		t.Fatal(err)
	}
	r.sendingServices = r.a.twilioSendingService()
	for range 2 {
		if err = r.a.AnnounceContactCardChange(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(alertTitles(t, r.a, p), "MMHotline calls from new numbers: download its contact card again"); n != 1 {
		t.Fatalf("change notices: %d", n)
	}
	if page = getPage(t, c, r.server.URL+"/contact-methods"); !strings.Contains(page, "MMHotline now calls from new numbers.") {
		t.Fatalf("no change notice: %s", page)
	}
	getPage(t, c, r.server.URL+"/mmhotline-contact-card.vcf")
	if page = getPage(t, c, r.server.URL+"/contact-methods"); strings.Contains(page, "from new numbers") {
		t.Fatal("the notice stayed after the download")
	}
}

// The on-call page's team list says what can wake each person at night,
// and, apart from that, whether MMHotline wakes them now. A person who is
// unavailable shows as unavailable and skipped, whatever could wake them:
// before the redesign the page said "Can be woken for SOS: Yes" for them.
func TestTheOnCallPageSkipsAnUnavailablePersonWhoCouldBeWoken(t *testing.T) {
	r := newPhoneRig(t)
	p := r.s.Principal
	addMember(t, r.a, "Carol", RoleMember)
	r.addPhone(t, p, barbaraNumber)
	c := r.browser(t)
	page := getPage(t, c, r.server.URL+"/on-call")
	mustContain(t, "the team", screenReaderText(page, "team-now"), "Carol Available Nothing set up can wake them at night Member", "Responder (you) Available Woken by automated voice call Administrator")
	mustContain(t, "whom an SOS wakes", screenReaderText(page, "on-call-now"), "Everyone who is available: You Carol")

	if _, err := r.a.SetPersonalAvailability(context.Background(), p, "unavailable", r.a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	page = getPage(t, c, r.server.URL+"/on-call")
	team := screenReaderText(page, "team-now")
	mustContain(t, "the team", team, "Responder (you) Unavailable until ", "Woken by automated voice call Administrator")
	if strings.Contains(team, "Responder (you) Available") {
		t.Fatalf("an unavailable person shows as available: %s", team)
	}
	order := screenReaderText(page, "on-call-now")
	mustContain(t, "whom an SOS wakes", order, "Everyone who is available: Carol", "You (skipped: unavailable until ")
}

func TestFailingPhoneCannotBeWoken(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	carol, _ := addMember(t, r.a, "Carol", RoleMember)
	r.fake.SetNumberBehavior(barbaraNumber, fakesendingservices.NumberInvalid)
	added, err := r.a.AddPhoneNumber(ctx, carol, barbaraNumber)
	if err != nil {
		t.Fatal(err)
	}
	r.dispatchTeam(t)
	ds := deliveriesOf(t, r.a, added.ContactMethods[0].ID)
	if len(ds) != 1 || ds[0].State != "failed" {
		t.Fatalf("save test failure: %+v", ds)
	}
	browser := r.browser(t)
	page := getPage(t, browser, r.server.URL+"/on-call")
	if team := screenReaderText(page, "team-now"); !strings.Contains(team, "Carol Available Nothing set up can wake them at night") {
		t.Fatalf("failing phone counted as working: %s", team)
	}
	// A successful later test restores the displayed ability to wake Carol.
	r.fake.SetNumberBehavior(barbaraNumber, "")
	r.advance(time.Minute)
	if err = r.a.SendContactMethodTest(ctx, carol, added.ContactMethods[0].ID); err != nil {
		t.Fatal(err)
	}
	r.dispatchTeam(t)
	page = getPage(t, browser, r.server.URL+"/on-call")
	if team := screenReaderText(page, "team-now"); !strings.Contains(team, "Carol Available Woken by automated voice call") {
		t.Fatalf("recovered phone still failing: %s", team)
	}
}
