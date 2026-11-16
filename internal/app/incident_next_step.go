package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// incidentNextStep is what MMHotline does next by itself about an open SOS
// incident, unless someone claims or resolves it first.
type incidentNextStep struct {
	// HeldForTriageAgent: nobody is woken yet, and the next step is the
	// first one that wakes someone.
	HeldForTriageAgent bool
	// HeldForAServerReplacement: the SOS waits longer, because the triage
	// agent is replacing a server, and At is the replacement's limit.
	HeldForAServerReplacement bool
	// The current step started at StepStartedAt, and the next one starts at
	// At, the incident's deadline. At is zero when the current step has no
	// deadline (everyone available, or the team's only member): MMHotline
	// then keeps waking the same people until someone claims it.
	StepStartedAt, At time.Time
	// Step is the next step as the escalation code names it, or the current
	// one when At is zero.
	Step string
	// EveryMember: nobody is available, so the step wakes every member.
	EveryMember bool
	// Wakes are the people the step wakes whom the incident has not woken
	// before; WakesAgain are those it wakes again. With At zero, WakesAgain
	// are the people MMHotline keeps waking. Only people with a waking
	// contact method are woken, so both may be empty.
	Wakes, WakesAgain []stepPerson
}

// stepPerson is a person an escalation step wakes.
type stepPerson struct{ ID, Name string }

// nextIncidentStep predicts the next step of open incident i by running the
// escalation code's own choice on a copy of it at its deadline:
// chooseInitialTarget for an incident held for the triage agent, as
// wakeIncidentHeldForTriageAgent does, and chooseNextTarget otherwise, as
// ReconcileIncidents does for an overdue step. The rules of escalation stay
// in those two functions.
//
// The choice uses the coverage at the deadline, not now: CoverageAt reads
// every fact that ends by itself (an unavailability, a schedule's handoff, a
// custom schedule's stored shift, an on-call duty someone took, the team
// mute) against the time it is given, so it answers for the deadline with
// what MMHotline knows now. A change before then, such as a person marking
// themselves unavailable, changes the prediction, and the page shows it at
// its next refresh.
func nextIncidentStep(ctx context.Context, tx *sql.Tx, i Incident) (incidentNextStep, error) {
	step := incidentNextStep{HeldForTriageAgent: i.Step == triageAgentStep, StepStartedAt: i.stepStartedAt(), At: i.Deadline, Step: i.Step, EveryMember: i.IgnorePersonal}
	if step.HeldForTriageAgent {
		// The hold started when the SOS arrived.
		step.StepStartedAt = i.CreatedAt
		var err error
		if step.HeldForAServerReplacement, err = wasHeldForAServerReplacement(ctx, tx, i.ID); err != nil {
			return step, err
		}
	}
	next := i
	if !i.Deadline.IsZero() {
		c, err := CoverageAt(ctx, tx, i.Deadline)
		if err != nil {
			return step, err
		}
		window, err := escalateIfUnclaimedAfter(ctx, tx)
		if err != nil {
			return step, err
		}
		if step.HeldForTriageAgent {
			chooseInitialTarget(&next, c, window, i.Deadline)
		} else {
			chooseNextTarget(&next, c, window, i.Deadline)
		}
		step.Step, step.EveryMember = next.Step, next.IgnorePersonal
	}
	woken, err := incidentWokenMemberIDs(ctx, tx, i.ID)
	if err != nil {
		return step, err
	}
	wakeable, _, err := wakeableMembers(ctx, tx, next.TargetMemberIDs)
	if err != nil {
		return step, err
	}
	for _, id := range wakeable {
		name, err := incidentMemberName(ctx, tx, id)
		if err != nil {
			return step, err
		}
		if !i.Deadline.IsZero() && !containsMember(woken, id) {
			step.Wakes = append(step.Wakes, stepPerson{id, name})
		} else {
			step.WakesAgain = append(step.WakesAgain, stepPerson{id, name})
		}
	}
	return step, nil
}

// incidentWokenMemberIDs lists the people an incident has woken: its history
// keeps the targets of each round of waking. The current step's targets are
// not enough, because the person on call is no longer a target once the
// backup's step starts.
func incidentWokenMemberIDs(ctx context.Context, tx *sql.Tx, incidentID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT target_member_ids FROM incident_actions WHERE incident_id=? AND kind='sos_escalation_due'`, incidentID)
	if err != nil {
		return nil, err
	}
	var woken []string
	for rows.Next() {
		var targets string
		var ids []string
		if err = rows.Scan(&targets); err == nil {
			err = json.Unmarshal([]byte(targets), &ids)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		for _, id := range ids {
			if !containsMember(woken, id) {
				woken = append(woken, id)
			}
		}
	}
	return woken, errors.Join(rows.Err(), rows.Close())
}

// countdownSentence says what MMHotline does next by itself about an open
// incident, unless someone claims it, to the person viewerID, whom it calls
// "you": "Claim the incident by 17:42, or MMHotline also wakes Priya Natarajan." It
// names only people the incident has not woken yet, unless the next step
// wakes nobody new.
func countdownSentence(step incidentNextStep, viewerID string, zone *time.Location, now time.Time) string {
	nobodyCanBeWoken := len(step.Wakes) == 0 && len(step.WakesAgain) == 0 && !step.HeldForTriageAgent
	if step.At.IsZero() {
		switch {
		case nobodyCanBeWoken:
			return "MMHotline can wake nobody: nobody it would wake has a contact method that wakes people for SOS alerts."
		case step.Step == "only_member" && len(step.WakesAgain) == 1 && step.WakesAgain[0].ID == viewerID:
			return "MMHotline keeps waking you until you claim the incident."
		case step.Step == "only_member":
			return "MMHotline keeps waking " + peopleText(step.WakesAgain, viewerID) + " until someone claims the incident."
		case step.EveryMember:
			return "Nobody is available, so MMHotline keeps waking everyone on the team until someone claims the incident."
		}
		return "MMHotline keeps waking everyone who is available until someone claims the incident."
	}
	if step.HeldForAServerReplacement && len(step.Wakes) > 0 {
		return "MMHotline wakes " + peopleText(step.Wakes, viewerID) + " at " + pageClock(step.At, now, zone) + " unless the replacement ends first."
	}
	claim := "Claim the incident by " + pageClock(step.At, now, zone) + ", or MMHotline "
	if nobodyCanBeWoken {
		return claim + "tries the next step: nobody it would wake next has a contact method that wakes people for SOS alerts."
	}
	if len(step.Wakes) == 0 {
		if step.Step == "everyone_available" {
			return claim + "wakes " + peopleText(step.WakesAgain, viewerID) + " again: nobody else is available."
		}
		return claim + "wakes " + peopleText(step.WakesAgain, viewerID) + " again."
	}
	// Nobody is woken before the first waking step, so nobody is woken
	// "also", and there is nobody "else".
	also, others := "also ", "everyone else"
	if step.HeldForTriageAgent {
		also, others = "", "everyone"
	}
	people := peopleText(step.Wakes, viewerID)
	switch {
	case step.Step == "everyone_available" && step.EveryMember:
		return claim + also + "wakes " + others + " on the team, because nobody is available: " + people + "."
	case step.Step == "everyone_available":
		return claim + also + "wakes " + others + " who is available: " + people + "."
	case step.HeldForTriageAgent && step.Step == "primary":
		return claim + "wakes " + people + ", the person on call."
	}
	return claim + also + "wakes " + people + "."
}

// peopleText names people in a sentence, the person viewerID first as "you":
// "you and Priya Natarajan".
func peopleText(people []stepPerson, viewerID string) string {
	var names, others []string
	for _, person := range people {
		if person.ID == viewerID {
			names = append(names, "you")
		} else {
			others = append(others, person.Name)
		}
	}
	return namesInSentence(append(names, others...))
}

// incidentCountdown is what a page shows of an open incident's countdown:
// the sentence and, while its step has a deadline, the step's length and the
// time gone since it started, in seconds, as a <progress> element's max and
// value, and the time left in words, such as "in 2 minutes".
type incidentCountdown struct {
	Sentence                    string
	StepSeconds, ElapsedSeconds int64
	TimeLeft                    string
}

// viewIncidentCountdown is the countdown of an incident's next step as the
// person viewerID sees it at now.
func viewIncidentCountdown(step incidentNextStep, viewerID string, zone *time.Location, now time.Time) incidentCountdown {
	countdown := incidentCountdown{Sentence: countdownSentence(step, viewerID, zone, now)}
	if !step.At.IsZero() {
		countdown.StepSeconds = int64(step.At.Sub(step.StepStartedAt) / time.Second)
		countdown.ElapsedSeconds = min(max(int64(now.Sub(step.StepStartedAt)/time.Second), 0), countdown.StepSeconds)
		countdown.TimeLeft = relativeTime(step.At, now)
	}
	return countdown
}
