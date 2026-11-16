package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A large outage pages many members in one team. Twilio accepts one
// call per second; all first calls must fit the bound without HTTP 429.
func TestManyMembersPagedAtOnceUnderSendingServiceLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}
	const members = 40
	r := newPhoneRig(t)
	ctx := context.Background()
	var numbers []string
	for i := range members {
		s := r.s
		if i > 0 {
			s = memberFixture(t, r.a, fmt.Sprintf("member-%d@example.test", i))
		}
		number := fmt.Sprintf("+1555555%04d", i)
		r.addPhone(t, s.Principal, number)
		numbers = append(numbers, number)
	}
	start := r.a.Now()
	if _, err := r.a.ManualSOS(ctx, r.s.Principal, "Cloud region down", "Every node is unreachable"); err != nil {
		t.Fatal(err)
	}
	firstCall := map[string]time.Time{}
	for second := 0; len(firstCall) < members && second < 120; second++ {
		// Drain the team's due work, then move the controllable clock.
		for range 400 {
			work, err := r.a.DispatchOne(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !work {
				break
			}
		}
		for _, number := range numbers {
			if _, done := firstCall[number]; !done && len(r.callsTo(number)) > 0 {
				firstCall[number] = r.a.Now()
			}
		}
		r.advance(time.Second)
	}
	var last time.Duration
	for _, at := range firstCall {
		last = max(last, at.Sub(start))
	}
	// Twilio accepts one call a second.
	bound := time.Duration(members+5) * time.Second
	if len(firstCall) != members || last > bound {
		t.Fatalf("first calls: %d of %d, the last %s after the SOS (bound %s)", len(firstCall), members, last, bound)
	}
	// MMHotline never went over a sending service's limit: no attempt was refused
	// with HTTP 429 (the fake refuses before recording a call).
	if refused := refusedForRate(t, r.a); refused != 0 {
		t.Fatalf("%d attempts were refused with HTTP 429", refused)
	}
	t.Logf("%d members: the last first call went %s after the SOS", members, last)
}

// refusedForRate counts the attempts a sending service refused with HTTP 429.
func refusedForRate(t *testing.T, a *App) int {
	t.Helper()
	return int(scalarTeam(t, a, `SELECT count(*) FROM delivery_attempts WHERE diagnostic LIKE '%HTTP 429%'`))
}

// The scenario's rate check can fail: when MMHotline paces itself faster
// than the sending service allows, the refusals show.
func TestScenarioRateCheckSeesRefusals(t *testing.T) {
	r := newPhoneRig(t)
	ctx := context.Background()
	r.fake.SetSendingServiceRateLimit("twilio", "call", 0.25)
	for i := range 4 {
		s := memberFixture(t, r.a, fmt.Sprintf("rate-%d@example.test", i))
		r.addPhone(t, s.Principal, fmt.Sprintf("+1555555%04d", i))
	}
	if _, err := r.a.ManualSOS(ctx, r.s.Principal, "Cloud region down", "x"); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := r.a.DispatchOne(ctx); err != nil {
			t.Fatal(err)
		}
		r.advance(time.Second)
	}
	if refusedForRate(t, r.a) == 0 {
		t.Fatal("a sending service limit below MMHotline's pace caused no refusal")
	}
}
