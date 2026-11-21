package app

import (
	"context"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

const incidentNoteHint = "Claim and Resolve each keep your note in the incident history. The triage agent reads your note when the agent next looks into the incident."

// incidentNoteRig is a team of one, Ana Silva, signed in on the pages
// through a test server, as a person who claims and resolves.
type incidentNoteRig struct {
	a   *App
	p   Principal
	ana *standInBrowser
}

func newIncidentNoteRig(t *testing.T) *incidentNoteRig {
	t.Helper()
	a, p, _ := alertFixture(t)
	if _, err := a.Store.db.Exec(`UPDATE members SET name='Ana Silva'`); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	t.Cleanup(server.Close)
	return &incidentNoteRig{a: a, p: p, ana: signInStandIn(t, a, server.URL, "intake@example.test")}
}

// sos opens an SOS incident by hand and returns its ID.
func (r *incidentNoteRig) sos(t *testing.T, title string) string {
	t.Helper()
	alert, err := r.a.ManualSOS(context.Background(), r.p, title, "")
	if err != nil {
		t.Fatal(err)
	}
	return alert.IncidentID
}

// act posts the incident page's claim or resolve form with a note, as a
// person does, and returns MMHotline's answer.
func (r *incidentNoteRig) act(t *testing.T, incidentID, action, note string) (int, string) {
	t.Helper()
	page := r.ana.page("/incidents/" + incidentID)
	return r.ana.request("POST", "/incidents/"+incidentID+"/"+action, url.Values{
		"csrf":     {standInField(t, standInCSRFField, page)},
		"revision": {standInField(t, standInRevisionField, page)},
		"note":     {note},
	})
}

func (r *incidentNoteRig) show(t *testing.T, incidentID string) (CommandResult, string) {
	t.Helper()
	return commandRequest(t, r.a, CommandRequest{Group: "incidents", Action: "show", Incidents: IncidentsCommandRequest{IncidentID: incidentID}}, 200)
}

// A person may write a note when claiming or resolving on the incident page.
// The history keeps it with that entry, and the incident page and incidents
// show give it after the entry's own text; an action without one shows none.
func TestClaimAndResolveKeepANote(t *testing.T) {
	t.Parallel()
	r := newIncidentNoteRig(t)
	disk, backup := r.sos(t, "Disk of app002 is 97% full"), r.sos(t, "Nightly backup of db001 failed")

	// One note field, of at most 500 characters, which Claim and Resolve
	// both send (TestAnSOSThatWaitsForAClaimSaysWhomItWakesAndOffersClaim).
	page := r.ana.page("/incidents/" + disk)
	if fields := regexp.MustCompile(`<input name="note" form="[^"]+" maxlength="500"`).FindAllString(page, -1); len(fields) != 1 || !strings.Contains(page, incidentNoteHint) {
		t.Fatalf("the page has %d note fields; want 1, with its hint:\n%s", len(fields), page)
	}

	for _, step := range []struct{ incidentID, action, note string }{
		{disk, "claim", "  Looking at it now.  "},
		{disk, "resolve", ""},
		{backup, "claim", ""},
		{backup, "resolve", "This was a test."},
	} {
		if status, body := r.act(t, step.incidentID, step.action, step.note); status != 303 {
			t.Fatalf("%s with the note %q: HTTP %d: %s", step.action, step.note, status, body)
		}
	}

	for _, want := range []struct {
		incidentID, claimNote, resolveNote, line string
	}{
		{disk, "Looking at it now.", "", "Claimed: Claimed by Ana Silva. Note from Ana Silva: “Looking at it now.” By: Ana Silva."},
		{backup, "", "This was a test.", "Resolved: Resolved by Ana Silva. Note from Ana Silva: “This was a test.” By: Ana Silva."},
	} {
		result, raw := r.show(t, want.incidentID)
		notes := map[string]string{}
		for _, entry := range result.Incident.History {
			if entry.Note != "" {
				notes[entry.Kind] = entry.Note
			}
		}
		if notes["claimed"] != want.claimNote || notes["resolved"] != want.resolveNote || len(notes) != 1 {
			t.Errorf("incident %s: the history's notes are %q", want.incidentID, notes)
		}
		if count := strings.Count(raw, `"note":`); count != 1 {
			t.Errorf("incidents show --json gives %d note fields; want 1: %s", count, raw)
		}
		if text := result.Text(); !strings.Contains(text, want.line) {
			t.Errorf("incidents show lacks %q:\n%s", want.line, text)
		}
	}
	if happened := liveSectionText(r.ana.page("/incidents/"+backup), "incident-audit"); !strings.Contains(happened, "You resolved the incident. Your note: “This was a test.”") || strings.Contains(happened, "You claimed the incident. Your note") {
		t.Fatalf("the page's history: %q", happened)
	}
}

// A note longer than 500 characters, or with a line break or another control
// character, is refused with the reason, and the incident stays as it was. A
// note of 500 characters is kept, though it takes more bytes.
func TestALongNoteOrAControlCharacterIsRefused(t *testing.T) {
	t.Parallel()
	r := newIncidentNoteRig(t)
	incidentID := r.sos(t, "app003 stopped answering")
	before, _ := r.show(t, incidentID)
	for _, refused := range []struct{ action, note, reason string }{
		{"claim", strings.Repeat("a", 501), "an incident note must be at most 500 characters; the submitted note has 501"},
		{"claim", "This was a test.\nThe backup job does this every night.", "a note is one line of plain text, without a line break, a tab, or another control character"},
		{"resolve", "This was\ta test.", "a note is one line of plain text, without a line break, a tab, or another control character"},
	} {
		if status, body := r.act(t, incidentID, refused.action, refused.note); status != 400 || strings.TrimSpace(body) != refused.reason {
			t.Errorf("%s with the note %q: HTTP %d: %s", refused.action, refused.note, status, body)
		}
		after, _ := r.show(t, incidentID)
		if after.Incident.Status != "open" || len(after.Incident.History) != len(before.Incident.History) {
			t.Fatalf("after the refused %s: status %s, %d history entries; want open and %d", refused.action, after.Incident.Status, len(after.Incident.History), len(before.Incident.History))
		}
	}
	incident, err := r.a.GetIncident(context.Background(), r.p, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Revision != 1 {
		t.Fatalf("the refused notes changed the incident's revision to %d", incident.Revision)
	}
	longest := strings.Repeat("é", 500)
	if status, body := r.act(t, incidentID, "claim", longest); status != 303 {
		t.Fatalf("a note of 500 characters: HTTP %d: %s", status, body)
	}
	if after, _ := r.show(t, incidentID); after.Incident.Status != "claimed" || after.Incident.History[len(after.Incident.History)-1].Note != longest {
		t.Fatalf("after the claim: %+v", after.Incident)
	}
}
