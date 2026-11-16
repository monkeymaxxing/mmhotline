package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// "Warn, wait, then act": MMHotline warns the team at warn, and the
// background loop runs the playbook at its run time unless someone pressed
// Stop first: docs/changing-mmhotline/design/triage-agent.md#warn-wait-then-act. A warned run is
// a row of triage_agent_playbook_runs_waiting_for_a_person whose rule
// is warn_wait_then_act; from its run time on, it runs and reaches
// the agent as an approved request does (playbook_run_approvals.go).

// warnedRunTimeColumn is a warned run's run time in SQL: the judging time
// plus the wait, both saved with the row when MMHotline judged.
const warnedRunTimeColumn = `(judged_at+warning_wait_minutes*60)`

// runsAt is when MMHotline runs a warned run unless someone pressed Stop.
func (r approvalRequest) runsAt() time.Time {
	return r.JudgedAt.Add(time.Duration(r.PlaybookRule.WarningWaitMinutes) * time.Minute)
}

// warnedRunTimeText names a run time for people and the agent, such as
// "03:25 UTC", with its date when it falls on another day than from, as
// after a wait of a day.
func warnedRunTimeText(runsAt, from time.Time) string {
	runsAt, from = runsAt.UTC(), from.UTC()
	if runsAt.Format(time.DateOnly) != from.Format(time.DateOnly) {
		return runsAt.Format("15:04 UTC on 2006-01-02")
	}
	return runsAt.Format("15:04 UTC")
}

// warnOfPlaybookRun warns the team, at warn, that MMHotline runs r at its run
// time unless someone presses Stop. The alert goes with an incident of the
// run's work as the approval request's alert does, and it links the run's
// own page, which holds the Stop button; the incident page shows the run in
// short, with Stop too. Being MMHotline's own warn, it never starts or
// joins a session, never resumes the agent, and wakes nobody: nobody needs
// to act for the fix to run. The diagnosis supplies the short reason.
func (a *App) warnOfPlaybookRun(ctx context.Context, tx *sql.Tx, r approvalRequest) error {
	reason, err := playbookRunReason(ctx, tx, r)
	if err != nil {
		return err
	}
	incidents, err := triageRunIncidents(ctx, tx, r.TriageSessionID, r.RunID)
	if err != nil {
		return err
	}
	where := "Press Stop on the playbook run page: " + strings.TrimRight(a.WebURL, "/") + "/approvals/" + r.ID
	if len(incidents) > 0 {
		// A warn alert goes with an incident of the run's work, whose page
		// its notification links.
		where = "The incident page shows a summary of the playbook run, with a Stop button. " + where
	}
	at := warnedRunTimeText(r.runsAt(), r.JudgedAt)
	changes := r.serversItChanges()
	in := GenericAlert{
		Severity: "warn",
		Title:    truncateRunes("MMHotline will run "+playbookInInventoryText(r.Playbook, r.Inventory)+" for the triage agent at "+at+" unless someone presses Stop"+changes, 256),
		Message:  truncateRunes(reason+"\n\nMMHotline runs the playbook at "+at+" unless someone presses Stop before then. The rule is "+r.PlaybookRule.Title()+"."+r.listedPlaybookSentence()+"\n\n"+where, maximumAlertMessageBytes/4),
	}
	alert, _, err := admitAlertAboutTriageRun(ctx, tx, r.TriageSessionID, r.RunID, playbookRunApprovalsActor, in, r.JudgedAt)
	if err != nil || alert.IncidentID == "" {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET incident_id=? WHERE id=?`, alert.IncidentID, r.ID); err != nil {
		return err
	}
	incident, err := readIncident(ctx, tx, alert.IncidentID)
	if err != nil {
		return err
	}
	return recordIncidentAction(ctx, tx, incident, playbookRunApprovalsActor, "playbook_run_warning_sent", fmt.Sprintf("MMHotline runs %s, at commit %s, at %s unless someone presses Stop%s. %s%s", playbookInInventoryText(r.Playbook, r.Inventory), r.CommitID, at, changes, r.PlaybookRule.Title()+"."+r.listedPlaybookSentence(), r.unrecognizedTasksText()), alert.ID, "", r.JudgedAt)
}

// warningSentText ends the answer to the agent, which ends its turn, with
// when the playbook runs. A software review's run has no session to
// resume.
func warningSentText(runsAt, now time.Time, inSession bool) string {
	text := " MMHotline warned the team that MMHotline runs the playbook at " + warnedRunTimeText(runsAt, now) + " unless someone presses Stop."
	if !inSession {
		return text + " This run has no triage session to resume, so the result stays on the Approvals page. End your turn now and report."
	}
	return text + " When the playbook run ends or someone stops the playbook run, MMHotline resumes your triage session with the result. Commit nothing more until MMHotline resumes you: a newer commit can make the warning stale, and the run then waits again. End your turn now and report."
}

// StopWarnedPlaybookRun records a team member's Stop of a warned run before
// its run time, with an optional short reason: nothing runs, and the agent
// hears who stopped it. From the run time on, MMHotline runs it, so a Stop is
// refused even before the background loop has started the run.
func (a *App) StopWarnedPlaybookRun(ctx context.Context, p Principal, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > maximumRefusalReasonCharacters {
		return fmt.Errorf("%w: the reason for stopping a playbook run must be at most %d characters", ErrInvalid, maximumRefusalReasonCharacters)
	}
	now := a.Now().UTC()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		// The history and the agent name the person who stopped it, which the
		// mmhotline command, with no member ID, is not.
		if m.ID == "" {
			return fmt.Errorf("%w: only a signed-in team member stops a playbook run", ErrForbidden)
		}
		r, err := readWaitingPlaybookRun(ctx, tx, id, warnWaitThenAct)
		if err != nil {
			return err
		}
		if r.State != approvalOpen {
			return fmt.Errorf("%w: this playbook run can no longer be stopped: %s", ErrConflict, approvalStateLabel(r.State))
		}
		if !now.Before(r.runsAt()) {
			return fmt.Errorf("%w: the playbook run can no longer be stopped because the scheduled run time, %s, has arrived. MMHotline runs the playbook", ErrConflict, displayTime(r.runsAt()))
		}
		return stopWarnedRun(ctx, tx, r, m, reason, now)
	})
}

// stopWarnedRun records m's Stop of the open warned run r before its run
// time, with the history and the result for the agent. A person who takes
// a triage session over stops its warned runs the same way.
func stopWarnedRun(ctx context.Context, tx *sql.Tx, r approvalRequest, m Member, reason string, now time.Time) error {
	playbook := playbookInInventoryText(r.Playbook, r.Inventory)
	history, result := "Stopped "+playbook+".", m.Name+" stopped "+playbook+", so nothing ran."
	if reason != "" {
		history, result = "Stopped "+playbook+": "+reason, result+" "+m.Name+"'s reason: "+reason
	}
	if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_playbook_runs_waiting_for_a_person SET stopped_by_member_id=?,stop_reason=? WHERE id=?`, m.ID, reason, r.ID); err != nil {
		return err
	}
	if r.IncidentID != "" {
		incident, err := readIncident(ctx, tx, r.IncidentID)
		if err != nil {
			return err
		}
		if err = recordIncidentAction(ctx, tx, incident, "member:"+m.ID, "playbook_run_stopped", history, "", "", now); err != nil {
			return err
		}
	}
	return finishApprovalRequest(ctx, tx, r, warnedRunStopped, result, true, now)
}

// nextWarnedRunTime returns the earliest run time of a warned run that waits,
// so the background loop visits at that time even when nothing else happens.
func nextWarnedRunTime(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var next sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT min(`+warnedRunTimeColumn+`) FROM triage_agent_playbook_runs_waiting_for_a_person WHERE state=? AND rule=?`, approvalOpen, warnWaitThenAct).Scan(&next)
	if err != nil || !next.Valid {
		return time.Time{}, err
	}
	return time.Unix(next.Int64, 0).UTC(), nil
}

// runsInText is a warned run's countdown in plain words, such as "Runs in 7
// minutes", rounded up so a fresh 10-minute wait reads 10 minutes.
func runsInText(left time.Duration) string {
	minutes := int((left + time.Minute - 1) / time.Minute)
	hours, minutes := minutes/60, minutes%60
	var parts []string
	if hours > 0 {
		parts = append(parts, plural(hours, "hour"))
	}
	if minutes > 0 || hours == 0 {
		parts = append(parts, plural(max(minutes, 1), "minute"))
	}
	return "Runs in " + strings.Join(parts, " ")
}

func plural(count int, unit string) string {
	if count == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", count, unit)
}
