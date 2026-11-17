package app

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// Adding a Pushover contact method sends a quiet notification that needs no
// acknowledgement, and says the phone has not proved yet that it wakes its
// owner: the add output stands out, and list repeats it until a test SOS
// proves it.
func TestAddingAPushoverPhoneSendsAQuietTestAndSaysTheWakeIsNotProvedYet(t *testing.T) {
	r := newPushoverRig(t)
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: "pushover@example.test", ContactMethodType: "pushover_notification", PushoverUserKey: fakesendingservices.PushoverUserKey}}, 200)
	r.dispatch(t)
	messages := r.fake.PushoverMessages()
	if len(messages) != 1 || messages[0].Priority != 0 || messages[0].Title != "MMHotline can reach this phone" || messages[0].Receipt != "" {
		t.Fatalf("the test notification: %+v", messages)
	}
	var receipts int
	if err := r.a.Store.db.QueryRow(`SELECT count(*) FROM pushover_receipts`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("a test notification waits for an acknowledgement: %d receipts, %v", receipts, err)
	}
	command := "sudo mmhotline incidents send-test-sos --sign-in-email-address pushover@example.test"
	warning := "THIS PHONE HAS NOT PROVED IT WAKES YOU YET. RUN: " + command
	if result.PushoverPhoneNotProvedWarning != warning || !strings.Contains(result.Text(), "\n\n"+warning+"\n") {
		t.Fatalf("the add output lacks the stand-out line: %q", result.Text())
	}
	assertCommandKeys(t, raw, "pushover_phone_not_proved_warning", "pushover_emergency_test")
	listed, raw := commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "list"}, 200)
	line := "Emergency test: not yet acknowledged. Run `" + command + "` to check that an SOS wakes this phone."
	if len(listed.ContactMethods) != 1 || listed.ContactMethods[0].PushoverEmergencyTest == nil || listed.ContactMethods[0].PushoverEmergencyTest.Acknowledged || !strings.Contains(listed.Text(), line) || !strings.Contains(raw, `"acknowledged":false`) {
		t.Fatalf("list lacks the unproved line:\n%s", listed.Text())
	}
	// The Contact methods page says it too.
	page := r.a.viewContactMethod(context.Background(), r.principal, listedMethod(t, r), Member{})
	if !page.EmergencyTestAcknowledgedAt.IsZero() || page.EmergencyTestCommand() != command {
		t.Fatalf("page view: %+v", page)
	}
}

func listedMethod(t *testing.T, r *pushoverRig) ContactMethod {
	t.Helper()
	methods, err := r.a.ContactMethods(context.Background(), r.principal)
	if err != nil || len(methods) != 1 {
		t.Fatalf("contact methods: %+v, %v", methods, err)
	}
	return methods[0]
}

// Only the owner's own acknowledgement of a test SOS proves the wake.
func TestOnlyAnAcknowledgedTestSOSOfTheOwnKeyProvesThePhoneWakes(t *testing.T) {
	proved := func(r *triageHoldRig, email string) time.Time {
		t.Helper()
		listed, _ := commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "list", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: email}}, 200)
		for _, c := range listed.ContactMethods {
			if c.ContactMethodType == "pushover_notification" {
				return c.PushoverEmergencyTest.AcknowledgedAt
			}
		}
		t.Fatalf("%s has no Pushover contact method: %+v", email, listed.ContactMethods)
		return time.Time{}
	}
	acknowledge := func(r *triageHoldRig, id, as string) {
		t.Helper()
		var receipt string
		for _, m := range r.fake.PushoverMessages() {
			if m.Priority == 2 && strings.HasSuffix(m.URL, "/incidents/"+id) {
				receipt = m.Receipt
			}
		}
		if err := r.fake.PushoverAcknowledgeAs(receipt, as); err != nil {
			t.Fatal(err)
		}
		r.advance(5 * time.Second)
		backgroundRounds(t, r.a)
	}
	t.Run("the owner's own acknowledgement", func(t *testing.T) {
		r := newTriageHoldRig(t)
		email := r.s.Principal.SignInEmailAddress
		if !proved(r, email).IsZero() {
			t.Fatal("proved before any test SOS")
		}
		id := r.sendTestSOS(t)
		backgroundRounds(t, r.a)
		if !proved(r, email).IsZero() {
			t.Fatal("proved before anyone acknowledged")
		}
		acknowledge(r, id, fakesendingservices.PushoverUserKey)
		at := proved(r, email)
		listed, _ := commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "list", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: email}}, 200)
		if at.IsZero() || at.After(r.a.Now()) || !strings.Contains(listed.Text(), "Emergency test: acknowledged at "+at.UTC().Format(time.RFC3339)+".") {
			t.Fatalf("after the acknowledgement:\n%s", listed.Text())
		}
		// The backup's phone never rang, so it proved nothing.
		if !proved(r, r.barbara.SignInEmailAddress).IsZero() {
			t.Fatal("a phone that was never tested counts as proved")
		}
	})
	t.Run("another Pushover user's acknowledgement", func(t *testing.T) {
		r := newTriageHoldRig(t)
		id := r.sendTestSOS(t)
		backgroundRounds(t, r.a)
		acknowledge(r, id, fakesendingservices.PushoverGroupKey)
		if !proved(r, r.s.Principal.SignInEmailAddress).IsZero() {
			t.Fatal("an acknowledgement that could not claim counts as proof")
		}
	})
	t.Run("an acknowledged real SOS", func(t *testing.T) {
		r := newPushoverRig(t)
		r.add(t, fakesendingservices.PushoverUserKey)
		id := r.sos(t, "Database down")
		r.dispatch(t)
		messages := r.fake.PushoverMessages()
		if err := r.fake.PushoverAcknowledge(messages[len(messages)-1].Receipt); err != nil {
			t.Fatal(err)
		}
		r.advance(5 * time.Second)
		r.poll(t)
		r.status(t, id, "claimed")
		if !listedMethod(t, r).EmergencyTestAcknowledgedAt.IsZero() {
			t.Fatal("a real SOS counts as the test SOS")
		}
	})
	// Installations from before the change keep their proof: an emergency
	// contact test the owner acknowledged.
	t.Run("an emergency contact test acknowledged before this change", func(t *testing.T) {
		for _, otherUser := range []bool{false, true} {
			r := newPushoverRig(t)
			r.add(t, fakesendingservices.PushoverUserKey)
			reason := ""
			if otherUser {
				reason = pushoverAcknowledgementFromAnotherUserReason
			}
			err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
				if _, err := tx.Exec(`INSERT INTO pushover_receipts(delivery_id,receipt,state) SELECT id,'old','acknowledged' FROM deliveries WHERE purpose='contact_method_test'`); err != nil {
					return err
				}
				_, err := tx.Exec(`UPDATE deliveries SET reason=? WHERE purpose='contact_method_test'`, reason)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := !listedMethod(t, r).EmergencyTestAcknowledgedAt.IsZero(); got == otherUser {
				t.Fatalf("from another Pushover user %v: proved %v", otherUser, got)
			}
		}
	})
}

// A test SOS for one person goes to that person alone, though they are not on
// call, and never on to the person on call, the backup, or anyone else.
func TestTestSOSForOnePersonWakesOnlyThatPerson(t *testing.T) {
	r := newTriageHoldRig(t)
	result, raw := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "send-test-sos", Incidents: IncidentsCommandRequest{SignInEmailAddress: " Barbara@Example.test "}}, 200)
	id := result.TestSOS.IncidentID
	if result.TestSOS.SentOnlyToName != "Barbara" || result.TestSOS.SentOnlyToSignInEmailAddress != "barbara@example.test" {
		t.Fatalf("the output: %s", result.Text())
	}
	assertCommandKeys(t, raw, "test_sos", "sent_only_to_name", "sent_only_to_sign_in_email_address")
	backgroundRounds(t, r.a)
	if r.emergencies(id, fakesendingservices.PushoverOtherUserKey) != 1 || r.woken(id) {
		t.Fatalf("Barbara got %d emergencies; the person on call was woken: %v", r.emergencies(id, fakesendingservices.PushoverOtherUserKey), r.woken(id))
	}
	// Nobody claims: the time to claim ends and nothing goes to anyone else.
	r.advance(10*time.Minute + time.Second)
	backgroundRounds(t, r.a)
	if r.woken(id) {
		t.Fatalf("the test went on to the person on call: %v", r.history(t, id))
	}
	if h := strings.Join(r.history(t, id), "\n"); !strings.Contains(h, "Contacting Barbara only") || strings.Contains(h, "Contacting Responder") {
		t.Fatalf("history: %q", h)
	}
	// Barbara acknowledges: the test is claimed and resolved, as any test SOS.
	for _, m := range r.fake.PushoverMessages() {
		if m.Priority == 2 && m.UserKey == fakesendingservices.PushoverOtherUserKey && strings.HasSuffix(m.URL, "/incidents/"+id) {
			if err := r.fake.PushoverAcknowledge(m.Receipt); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.advance(5 * time.Second)
	backgroundRounds(t, r.a)
	r.status(t, id, "resolved")
	listed, _ := commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "list", ContactMethods: ContactMethodsCommandRequest{SignInEmailAddress: "barbara@example.test"}}, 200)
	if proved := listed.ContactMethods[0]; proved.PushoverEmergencyTest == nil || !proved.PushoverEmergencyTest.Acknowledged {
		t.Fatalf("Barbara's phone is not proved:\n%s", listed.Text())
	}
}

func TestTestSOSForOnePersonRefusesAPersonItCannotWake(t *testing.T) {
	r := newTriageHoldRig(t)
	for email, refusal := range map[string]struct {
		code int
		want string
	}{"nobody@example.test": {404, "no member has sign-in email address"}, "not an address": {400, "sign-in email address must have"}} {
		want := refusal.want
		result, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "send-test-sos", Incidents: IncidentsCommandRequest{SignInEmailAddress: email}}, refusal.code)
		if !strings.Contains(result.Message, want) {
			t.Errorf("%s: %q", email, result.Message)
		}
	}
	// A person with no contact method that wakes people.
	addPerson(t, r.a, "quiet@example.test", "Quiet", RoleMember)
	result, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "send-test-sos", Incidents: IncidentsCommandRequest{SignInEmailAddress: "quiet@example.test"}}, 400)
	if !strings.Contains(result.Message, "Quiet has no contact method that wakes people for SOS alerts") {
		t.Errorf("%q", result.Message)
	}
}

// When the one person becomes unavailable, the test wakes nobody and times
// out; it does not go on to the person on call or the backup.
func TestTestSOSForOnePersonDoesNotGoOnWhenThatPersonBecomesUnavailable(t *testing.T) {
	r := newTriageHoldRig(t)
	result, _ := commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "send-test-sos", Incidents: IncidentsCommandRequest{SignInEmailAddress: "barbara@example.test"}}, 200)
	id := result.TestSOS.IncidentID
	backgroundRounds(t, r.a)
	if _, err := r.a.SetPersonalAvailability(context.Background(), Principal{SignInEmailAddress: r.barbara.SignInEmailAddress}, "unavailable", r.a.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	r.advance(10*time.Minute + time.Second)
	backgroundRounds(t, r.a)
	if r.woken(id) || r.emergencies(id, fakesendingservices.PushoverOtherUserKey) != 1 {
		t.Fatalf("the test went on to someone else: %v", r.history(t, id))
	}
	r.status(t, id, "open")
}
