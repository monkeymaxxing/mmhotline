package app

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// countdownFor predicts the next step of incident id and says it to viewer,
// as a page does.
func countdownFor(t *testing.T, a *App, id string, viewer Member) (incidentNextStep, string) {
	t.Helper()
	var step incidentNextStep
	err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		i, err := readIncident(context.Background(), tx, id)
		if err != nil {
			return err
		}
		step, err = nextIncidentStep(context.Background(), tx, i)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return step, countdownSentence(step, viewer.ID, time.UTC, a.Now())
}

// An incident wakes Responder, who is on call; nobody claims it. The
// countdown names the backup, then everyone available whom MMHotline has
// not woken yet, never Responder again, and ends when MMHotline keeps waking
// everyone available.
func TestTheCountdownNamesWhomTheNextStepWakes(t *testing.T) {
	a, p, members := incidentTeam(t)
	responder, backup, third := members[0], members[1], members[2]
	ctx := context.Background()
	opened := a.Now()
	i := newIncidentFixture(t, a, p)

	step, sentence := countdownFor(t, a, i.ID, responder)
	if want := "Claim the incident by 12:05, or MMHotline also wakes Backup."; sentence != want {
		t.Fatalf("on the person on call's step: %q, want %q", sentence, want)
	}
	if !step.StepStartedAt.Equal(opened) || !step.At.Equal(opened.Add(5*time.Minute)) {
		t.Fatalf("the countdown runs from %v to %v", step.StepStartedAt, step.At)
	}
	if _, sentence = countdownFor(t, a, i.ID, backup); sentence != "Claim the incident by 12:05, or MMHotline also wakes you." {
		t.Fatalf("to the backup: %q", sentence)
	}

	// Nobody claimed it in time: MMHotline wakes the backup.
	now := opened.Add(6 * time.Minute)
	a.Now = func() time.Time { return now }
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	_, sentence = countdownFor(t, a, i.ID, responder)
	if want := "Claim the incident by 12:11, or MMHotline also wakes everyone else who is available: Third."; sentence != want {
		t.Fatalf("on the backup's step: %q, want %q", sentence, want)
	}
	if _, sentence = countdownFor(t, a, i.ID, third); !strings.HasSuffix(sentence, "everyone else who is available: you.") {
		t.Fatalf("to Third: %q", sentence)
	}

	// Nobody claimed it again: MMHotline wakes everyone available, and keeps
	// waking them; there is no next step to count to.
	now = now.Add(6 * time.Minute)
	if err := a.ReconcileIncidents(ctx); err != nil {
		t.Fatal(err)
	}
	step, sentence = countdownFor(t, a, i.ID, responder)
	if !step.At.IsZero() || sentence != "MMHotline keeps waking everyone who is available until someone claims the incident." {
		t.Fatalf("on the everyone-available step: %q, until %v", sentence, step.At)
	}
}

// The prediction uses the coverage at the deadline: an unavailable backup
// is passed over, unless their unavailability ends before the deadline.
func TestTheCountdownPassesOverABackupWhoIsUnavailableAtTheDeadline(t *testing.T) {
	for _, test := range []struct {
		unavailableFor time.Duration
		want           string
	}{
		{2 * time.Hour, "Claim the incident by 12:05, or MMHotline also wakes everyone else who is available: Third."},
		{3 * time.Minute, "Claim the incident by 12:05, or MMHotline also wakes Backup."},
	} {
		a, p, members := incidentTeam(t)
		i := newIncidentFixture(t, a, p)
		if _, err := a.SetPersonalAvailability(context.Background(), Principal{SignInEmailAddress: members[1].SignInEmailAddress}, "unavailable", a.Now().Add(test.unavailableFor), 0); err != nil {
			t.Fatal(err)
		}
		if _, sentence := countdownFor(t, a, i.ID, members[0]); sentence != test.want {
			t.Errorf("backup unavailable for %v: %q, want %q", test.unavailableFor, sentence, test.want)
		}
	}
}

// An SOS that waits for the triage agent wakes nobody yet; the countdown
// runs to the end of the hold, when MMHotline wakes the person on call.
func TestTheCountdownOfAnSOSHeldForTheTriageAgent(t *testing.T) {
	a, p, members := incidentTeam(t)
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_setup(id,coding_agent,credential_type,credential,model,reasoning_effort,set_up_at,set_up_by) VALUES(1,'codex','api_key','key','test-model','high',1,'administrator')`); err != nil {
		t.Fatal(err)
	}
	source, err := createAlertSource(context.Background(), a, p, "Monitoring")
	if err != nil {
		t.Fatal(err)
	}
	sos := admitSignedAlert(t, a, source, "sos", "Disk of app002 is full", "")
	i, err := a.GetIncident(context.Background(), p, sos.IncidentID)
	if err != nil || i.Step != triageAgentStep {
		t.Fatalf("the SOS is not held for the triage agent: %+v %v", i, err)
	}
	step, sentence := countdownFor(t, a, i.ID, members[1])
	if want := "Claim the incident by 12:03, or MMHotline wakes Responder, the person on call."; sentence != want {
		t.Fatalf("to the backup: %q, want %q", sentence, want)
	}
	if !step.StepStartedAt.Equal(i.CreatedAt) || !step.At.Equal(i.CreatedAt.Add(sosWaitsForTheAgentMinutesBeforeTheSetting*time.Minute)) {
		t.Fatalf("the countdown runs from %v to %v", step.StepStartedAt, step.At)
	}
	if _, sentence = countdownFor(t, a, i.ID, members[0]); sentence != "Claim the incident by 12:03, or MMHotline wakes you, the person on call." {
		t.Fatalf("to the person on call: %q", sentence)
	}
}
