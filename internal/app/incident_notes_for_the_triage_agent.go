package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Notes from the team: what a person wrote with a claim or a resolve on the
// incident page reaches the triage agent once, in the prompt of the next run
// of the session the incident belongs to, or else of the first run of a
// later session with that incident or an alert of its cause.
// docs/changing-mmhotline/design/triage-agent.md#notes-from-the-team.
//
// The note lives on its history entry, incident_actions.note.
// incident_actions.note_given_in_triage_run_id is the run whose prompt
// carried it, "" while it waits. A software review gathers none.

// triageSessionPromptIncidentNote is one note as the prompt shows it.
type triageSessionPromptIncidentNote struct {
	// Kind is the history entry's kind: claimed, reclaimed, or resolved.
	PersonName, Kind, IncidentTitle, Note string
	WrittenAt                             time.Time
}

// maximumTriageSessionPromptNotesBytes bounds the notes in a prompt, as they
// travel in JSON: a quarter of the prompt, so the alerts, the evidence,
// always have the rest. It holds dozens of notes of 500 characters; when
// more wait, as after months of notes without the triage agent, the newest
// are shown.
const maximumTriageSessionPromptNotesBytes = maximumTriageSessionPromptBytes / 4

// gatherIncidentNotes puts in the plan each note not yet given on an
// incident of the session, or on any earlier incident of the same cause as
// one of alertIDs, the alerts the run carries. A cause is an alert source
// and a sender incident key, as alertStartsANewTriageSession reads a
// problem; an alert without a key has no earlier incident.
func (plan *triageSessionRunPlan) gatherIncidentNotes(ctx context.Context, tx *sql.Tx, alertIDs []string) error {
	// A note on any incident of the session reaches the session's next run,
	// whatever that run carries, because the session's work is all of them.
	incidents, err := triageSessionIncidents(ctx, tx, plan.sessionID)
	if err != nil {
		return err
	}
	var incidentIDs []string
	for _, i := range incidents {
		incidentIDs = append(incidentIDs, i.ID)
	}
	args := make([]any, 0, len(incidentIDs)+len(alertIDs))
	for _, id := range incidentIDs {
		args = append(args, id)
	}
	for _, id := range alertIDs {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.actor_name,a.kind,e.title,a.note,a.occurred_at FROM incident_actions a JOIN incidents i ON i.id=a.incident_id JOIN alerts e ON e.id=i.opening_alert_id
		WHERE a.note!='' AND a.note_given_in_triage_run_id='' AND (a.incident_id IN (`+sqlPlaceholders(len(incidentIDs))+`)
		OR i.sender_incident_key!='' AND EXISTS(SELECT 1 FROM alerts n WHERE n.id IN (`+sqlPlaceholders(len(alertIDs))+`) AND n.source_id=i.source_id AND n.sender_incident_key=i.sender_incident_key))
		ORDER BY a.id`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, written int64
		var note triageSessionPromptIncidentNote
		if err = rows.Scan(&id, &note.PersonName, &note.Kind, &note.IncidentTitle, &note.Note, &written); err != nil {
			rows.Close()
			return err
		}
		note.WrittenAt = time.Unix(written, 0).UTC()
		plan.incidentNoteIDs, plan.incidentNotes = append(plan.incidentNoteIDs, id), append(plan.incidentNotes, note)
	}
	return errors.Join(rows.Err(), rows.Close())
}

// incidentNotesGiven records that the run's prompt carried the notes, in the
// transaction that records the run, so each note is given once. A note given
// meanwhile keeps its run.
func incidentNotesGiven(ctx context.Context, tx *sql.Tx, runID string, ids []int64) error {
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE incident_actions SET note_given_in_triage_run_id=? WHERE id=? AND note_given_in_triage_run_id=''`, runID, id); err != nil {
			return err
		}
	}
	return nil
}

// incidentNotesWaitAgain makes the notes a run carried wait again, for a run
// that MMHotline's own stop or restart cut short, which is resumed with them
// as with its alerts.
func incidentNotesWaitAgain(ctx context.Context, tx *sql.Tx, runID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE incident_actions SET note_given_in_triage_run_id='' WHERE note_given_in_triage_run_id=?`, runID)
	return err
}

// incidentNotesText shows the notes apart from the alerts, as a team
// member's own words: the newest that fit maximumTriageSessionPromptNotesBytes,
// oldest first, then a line that says how many older ones were left out.
// With no notes, it adds nothing.
func incidentNotesText(notes []triageSessionPromptIncidentNote) string {
	if len(notes) == 0 {
		return ""
	}
	var lines []string
	size := 0
	for index := len(notes) - 1; index >= 0; index-- {
		line := "\n- " + incidentNoteLine(notes[index])
		if size+jsonTextSize(line) > maximumTriageSessionPromptNotesBytes {
			break
		}
		size += jsonTextSize(line)
		lines = append(lines, line)
	}
	slices.Reverse(lines)
	text := "\n\nNotes from the team: team members wrote the notes below when claiming or resolving an incident. The notes contain the team members' own words, not alerts:" + strings.Join(lines, "")
	if left := len(notes) - len(lines); left > 0 {
		text += "\nAnd " + countText(left, "older note", "older notes") + ", which MMHotline left out to keep this prompt short."
	}
	return text
}

// incidentNoteLine is one note, quoted as written, with who wrote it, on
// which action of which incident, and when:
// Ana Silva, on resolving “Disk of app002 is 97% full” at 2026-11-03 09:12:00 UTC: “This was a test.”
func incidentNoteLine(note triageSessionPromptIncidentNote) string {
	action := "claiming"
	switch note.Kind {
	case "reclaimed":
		action = "taking over"
	case "resolved":
		action = "resolving"
	}
	return fmt.Sprintf("%s, on %s “%s” at %s: “%s”", note.PersonName, action, strings.Join(strings.Fields(note.IncidentTitle), " "), note.WrittenAt.UTC().Format("2006-01-02 15:04:05 UTC"), note.Note)
}
