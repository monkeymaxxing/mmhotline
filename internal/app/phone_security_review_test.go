package app

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// Automated voice call markup and Send test checks.

func TestTeamNamesAndTitlesCannotAddCallMarkup(t *testing.T) {
	message := sosCallMessage(`Ops & "Co" </Say><Dial>+19005550100</Dial>`, []string{`x'"></Say><Redirect>https://evil.example/t</Redirect><Say>`, "<speak><prosody rate='x-slow'>hi</prosody></speak>"}, false)
	decoder := xml.NewDecoder(strings.NewReader(callMarkup(message)))
	var elements []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("not well-formed: %v", err)
		}
		if start, ok := token.(xml.StartElement); ok {
			elements = append(elements, start.Name.Local)
		}
	}
	if got := strings.Join(elements, ","); got != "Response,Say,Say,Hangup" {
		t.Fatalf("elements: %s", got)
	}
}

func TestSendTestOnCallsIsLimited(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	call := r.addPhone(t, p, barbaraNumber)
	for i := range 5 {
		r.advance(time.Minute)
		err := r.a.SendContactMethodTest(ctx, p, call.ID)
		if i < 4 && err != nil || i == 4 && (!errors.Is(err, ErrRateLimited) || !strings.Contains(publicError(err), "tested 5 times today")) {
			t.Fatalf("test %d: %v", i+1, err)
		}
		r.dispatch(t)
		if i < 4 {
			if _, err := r.fake.AnswerCall(r.lastCallTo(t, barbaraNumber).IDFromSendingService, fakesendingservices.CallNoAnswer); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Other members provide enough distinct call cards to reach the team limit.
	tests := 5
	for i := range 4 {
		member, _ := addMember(t, r.a, fmt.Sprintf("Member %d", i), RoleMember)
		other := r.addPhone(t, member, fmt.Sprintf("+155555530%02d", i))
		tests++ // the automatic save test also counts
		for range 4 {
			r.advance(time.Minute)
			err := r.a.SendContactMethodTest(ctx, member, other.ID)
			if tests < 20 && err != nil {
				t.Fatalf("team test %d: %v", tests+1, err)
			}
			if tests >= 20 && (!errors.Is(err, ErrRateLimited) || !strings.Contains(publicError(err), "this installation's automated voice call contact methods were tested")) {
				t.Fatalf("team test %d: %v", tests+1, err)
			}
			if err == nil {
				r.dispatch(t)
				if _, err := r.fake.AnswerCall(r.lastCallTo(t, other.SentTo).IDFromSendingService, fakesendingservices.CallNoAnswer); err != nil {
					t.Fatal(err)
				}
			}
			tests++
		}
	}
	r.advance(24 * time.Hour)
	if err := r.a.SendContactMethodTest(ctx, p, call.ID); err != nil {
		t.Fatalf("limits did not reset: %v", err)
	}
}

// Adding and removing a number again and again must not place unlimited test
// calls: the test made when a number is added counts against the Send Test
// limits, and past the limit the number is still added, with a visible reason.
func TestAddingAndRemovingANumberIsLimitedLikeSendTest(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	p := r.s.Principal
	for i := range phoneCallTestsPerInstallationPerDay {
		r.advance(time.Second)
		added, err := r.a.AddPhoneNumber(ctx, p, barbaraNumber)
		if err != nil {
			t.Fatalf("add %d: %v", i+1, err)
		}
		c := added.ContactMethods[0]
		if c.TestNotSentReason != "" {
			t.Fatalf("add %d: test skipped too early: %s", i+1, c.TestNotSentReason)
		}
		if err = r.a.RemoveContactMethod(ctx, p, c.ID, c.Revision); err != nil {
			t.Fatal(err)
		}
	}
	r.advance(time.Second)
	added, err := r.a.AddPhoneNumber(ctx, p, barbaraNumber)
	if err != nil {
		t.Fatalf("the number must still be added at the limit: %v", err)
	}
	c := added.ContactMethods[0]
	if !strings.Contains(c.TestNotSentReason, "Test not sent: ") || !strings.Contains(c.TestNotSentReason, "Try again tomorrow") {
		t.Fatalf("reason: %q", c.TestNotSentReason)
	}
	var queued int
	if err = r.a.Store.db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE contact_method_id=?`, c.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("a test was queued at the limit: %d %v", queued, err)
	}
	// Send Test reports the same limit.
	r.advance(time.Minute)
	if err = r.a.SendContactMethodTest(ctx, p, c.ID); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Send Test: %v", err)
	}
	r.advance(24 * time.Hour)
	if err = r.a.SendContactMethodTest(ctx, p, c.ID); err != nil {
		t.Fatalf("limits did not reset: %v", err)
	}
}

func TestPersonalCommunicationsAndInterexchangeNumbersAreRefused(t *testing.T) {
	for _, typed := range []string{"+1 500 555 0123", "+1 533 555 0123", "+1 700 555 0123", "+1 900 555 0123"} {
		if _, _, err := parsePhoneNumber(typed); err == nil || !strings.Contains(err.Error(), "not a toll-free or premium number") {
			t.Errorf("%s: %v", typed, err)
		}
	}
	if _, _, err := parsePhoneNumber("+1 503 555 0123"); err != nil {
		t.Fatalf("Portland, Oregon: %v", err)
	}
}
