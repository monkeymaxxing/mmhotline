package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// foldingSource sets the alert source to fold repeated warnings, as the
// settings file's repeated_warnings does.
func foldingSource(t *testing.T, a *App, source CreatedSource) {
	t.Helper()
	if _, err := a.Store.db.Exec(`UPDATE sources SET repeated_warnings=? WHERE id=?`, repeatedWarningsFold, source.Source.ID); err != nil {
		t.Fatal(err)
	}
}

// nodeWarn is a warn as an MMWatchdog node sends it while the hub is down:
// the finding's key, the exact finding on the first line, and the node's
// own name in the title and the rest of the message.
func nodeWarn(senderAlertID, key, firstLine, node string) string {
	return fmt.Sprintf(`{"sender_alert_id":%q,"sender_incident_key":%q,"severity":"warn","title":"%s: %s","message":"%s\nSeen on %s."}`, senderAlertID, key, node, firstLine, firstLine, node)
}

type intakeAnswer struct {
	AlertID                        string `json:"mmhotline_alert_id"`
	AlreadyReceived                bool   `json:"already_received"`
	RecordedAsRepeatOfEarlierAlert bool   `json:"recorded_as_repeat_of_earlier_alert"`
}

func readIntakeAnswer(t *testing.T, a *App, p Principal, source CreatedSource, body string) intakeAnswer {
	t.Helper()
	w := intakeHTTP(a, p, source, body, a.Now())
	if w.Code != 200 {
		t.Fatalf("intake of %s: %d %s", body, w.Code, w.Body.String())
	}
	var answer intakeAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// deliveredWarnings counts the warn alerts of the source that queued a
// delivery.
func deliveredWarnings(t *testing.T, a *App, sourceID string) int64 {
	t.Helper()
	return scalarTeam(t, a, `SELECT count(DISTINCT alert_id) FROM deliveries WHERE alert_id IN (SELECT id FROM alerts WHERE source_id=? AND severity='warn')`, sourceID)
}

// An alert source without the setting, or set to keep_each, delivers every
// warning, also one with the same key and first line as an earlier one: for
// most senders a changed message under one key is new news.
func TestRepeatedWarningsKeepEachDeliversEveryWarning(t *testing.T) {
	t.Parallel()
	a, p, leftOut := alertFixture(t)
	savedEmail(t, a, p, true)
	keepEach, err := createAlertSource(context.Background(), a, p, "Fleet hub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.db.Exec(`UPDATE sources SET repeated_warnings='keep_each' WHERE id=?`, keepEach.Source.ID); err != nil {
		t.Fatal(err)
	}
	for _, source := range []CreatedSource{leftOut, keepEach} {
		first := readIntakeAnswer(t, a, p, source, nodeWarn("w1", "disk_full", "/var is 95% full", "web-1"))
		second := readIntakeAnswer(t, a, p, source, nodeWarn("w2", "disk_full", "/var is 95% full", "web-2"))
		if first.AlertID == second.AlertID || second.RecordedAsRepeatOfEarlierAlert {
			t.Fatalf("%s: the second warning was folded: %+v %+v", source.Source.Name, first, second)
		}
		if n := deliveredWarnings(t, a, source.Source.ID); n != 2 {
			t.Fatalf("%s: delivered %d warnings, want 2", source.Source.Name, n)
		}
	}
}

// During a hub outage thousands of servers send one finding directly. A
// folding source delivers it once, counts the repeats on the first warn,
// and the burst leaves room under both intake limits for unrelated alerts.
func TestAHubOutageBurstIsDeliveredOnceAndLeavesRoomForOtherAlerts(t *testing.T) {
	t.Parallel()
	a, p, nodes := alertFixture(t)
	savedEmail(t, a, p, true)
	foldingSource(t, a, nodes)
	other, err := createAlertSource(context.Background(), a, p, "Backups")
	if err != nil {
		t.Fatal(err)
	}
	// The installation's hourly limit has room for only a few more alerts.
	if _, err := a.Store.db.Exec(`INSERT INTO team_alert_rate(id,hour_bucket,warning_and_info_alerts) VALUES(1,?,?)`, a.Now().Unix()/3600, installationMaximumNewWarningAndInfoAlertsPerHour-3); err != nil {
		t.Fatal(err)
	}
	first := readIntakeAnswer(t, a, p, nodes, nodeWarn("node-0", "openssh_cve", "CVE-2026-1234 in openssh 9.6", "node-0"))
	ctx := context.Background()
	for n := 1; n < 5000; n++ {
		body := []byte(nodeWarn(fmt.Sprintf("node-%d", n), "openssh_cve", "CVE-2026-1234 in openssh 9.6", fmt.Sprintf("node-%d", n)))
		alert, outcome, _, err := a.AdmitGeneric(ctx, nodes.Source.ID, alertSignature(nodes.Secret, a.Now(), body), body)
		if err != nil || alert.ID != first.AlertID || !outcome.RecordedAsRepeatOfEarlierAlert || outcome.AlreadyReceived {
			t.Fatalf("repeat %d: %s %+v %v", n, alert.ID, outcome, err)
		}
	}
	repeat := readIntakeAnswer(t, a, p, nodes, nodeWarn("node-5000", "openssh_cve", "CVE-2026-1234 in openssh 9.6", "node-5000"))
	if repeat != (intakeAnswer{AlertID: first.AlertID, RecordedAsRepeatOfEarlierAlert: true}) {
		t.Fatalf("answer to a repeat: %+v", repeat)
	}
	readIntakeAnswer(t, a, p, nodes, `{"sender_alert_id":"unrelated","sender_incident_key":"disk_full","severity":"warn","title":"Disk full","message":"/var is 95% full"}`)
	readIntakeAnswer(t, a, p, other, `{"sender_alert_id":"backup","severity":"warn","title":"Backup slow","message":""}`)
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE source_id=? AND sender_incident_key='openssh_cve'`, nodes.Source.ID); n != 1 {
		t.Fatalf("the burst saved %d alerts, want 1", n)
	}
	if n := deliveredWarnings(t, a, nodes.Source.ID); n != 2 {
		t.Fatalf("delivered %d of the source's alerts, want the finding once and the unrelated warning", n)
	}
	alert, err := a.GetAlert(ctx, p, first.AlertID)
	if err != nil || alert.RepeatCount != 5000 || alert.LastRepeatTitle != "node-5000: CVE-2026-1234 in openssh 9.6" {
		t.Fatalf("the first warn's repeats: %d %q %v", alert.RepeatCount, alert.LastRepeatTitle, err)
	}

	// alerts list shows the repeats in text and JSON.
	listed := a.commandAlert(alert)
	if want := "Repeated 5000 more times within the hour, and not delivered again. The latest repeat's title: node-5000: CVE-2026-1234 in openssh 9.6"; !strings.Contains(listed.Text(), want) {
		t.Fatalf("alerts list text:\n%s", listed.Text())
	}
	raw, err := json.Marshal(listed)
	if err != nil || !strings.Contains(string(raw), `"repeat_count":5000,"last_repeat_title":"node-5000: CVE-2026-1234 in openssh 9.6"`) {
		t.Fatalf("alerts list JSON: %s %v", raw, err)
	}
	unrepeated, err := json.Marshal(a.commandAlert(Alert{ID: "x"}))
	if err != nil || strings.Contains(string(unrepeated), "repeat") {
		t.Fatalf("an alert without repeats: %s %v", unrepeated, err)
	}
}

// Only a warn with a key, the same key, and the same first line is a
// repeat. A keyless warn may be unrelated; a new exact finding under the key
// is new news; an SOS is never folded.
func TestWarningsThatAreNotRepeats(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	savedEmail(t, a, p, true)
	foldingSource(t, a, s)
	keyless := `{"severity":"warn","title":"Queue slow","message":"Queue slow"}`
	a1 := readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"k1",`+keyless[1:])
	a2 := readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"k2",`+keyless[1:])
	if a1.AlertID == a2.AlertID || a2.RecordedAsRepeatOfEarlierAlert {
		t.Fatalf("a keyless warn was folded: %+v", a2)
	}

	first := readIntakeAnswer(t, a, p, s, nodeWarn("f1", "openssh", "CVE-2026-1234 in openssh 9.6", "node-1"))
	newFinding := readIntakeAnswer(t, a, p, s, nodeWarn("f2", "openssh", "CVE-2026-9999 in openssh 9.6", "node-1"))
	if newFinding.AlertID == first.AlertID || newFinding.RecordedAsRepeatOfEarlierAlert {
		t.Fatalf("a new exact finding under the key was folded: %+v", newFinding)
	}
	// A warn without severity is a warn, and folds as one.
	noSeverity := readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"f3","sender_incident_key":"openssh","title":"node-2","message":"CVE-2026-1234 in openssh 9.6\nSeen on node-2."}`)
	if noSeverity.AlertID != first.AlertID || !noSeverity.RecordedAsRepeatOfEarlierAlert {
		t.Fatalf("a warn without severity: %+v", noSeverity)
	}

	sos1 := readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"s1","sender_incident_key":"db","severity":"sos","title":"Database down","message":"down"}`)
	sos2 := readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"s2","sender_incident_key":"db","severity":"sos","title":"Database down","message":"down"}`)
	if sos1.AlertID == sos2.AlertID || sos2.RecordedAsRepeatOfEarlierAlert {
		t.Fatalf("an SOS was folded: %+v", sos2)
	}
	if n := deliveredWarnings(t, a, s.Source.ID); n != 4 {
		t.Fatalf("delivered %d warnings, want the 2 keyless warns and the 2 findings", n)
	}
}

// A resolved-warn ends the folding window, so the next outage starts fresh,
// and so does an hour.
func TestTheFoldingWindowEndsWithAResolvedWarnOrAfterAnHour(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	savedEmail(t, a, p, true)
	foldingSource(t, a, s)
	start := a.Now()
	first := readIntakeAnswer(t, a, p, s, nodeWarn("w1", "disk", "/var is 95% full", "web-1"))
	readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"r1","sender_incident_key":"disk","severity":"resolved-warn","title":"Disk ok","message":""}`)
	afterResolved := readIntakeAnswer(t, a, p, s, nodeWarn("w2", "disk", "/var is 95% full", "web-1"))
	if afterResolved.AlertID == first.AlertID || afterResolved.RecordedAsRepeatOfEarlierAlert {
		t.Fatalf("a warn after the resolved-warn was folded: %+v", afterResolved)
	}

	almost := start.Add(time.Hour - time.Second)
	a.Now = func() time.Time { return almost }
	if repeat := readIntakeAnswer(t, a, p, s, nodeWarn("w3", "disk", "/var is 95% full", "web-2")); repeat.AlertID != afterResolved.AlertID {
		t.Fatalf("a repeat within the hour: %+v", repeat)
	}
	hourLater := start.Add(time.Hour)
	a.Now = func() time.Time { return hourLater }
	afterAnHour := readIntakeAnswer(t, a, p, s, nodeWarn("w4", "disk", "/var is 95% full", "web-3"))
	if afterAnHour.AlertID == afterResolved.AlertID || afterAnHour.RecordedAsRepeatOfEarlierAlert {
		t.Fatalf("a warn an hour later was folded: %+v", afterAnHour)
	}
	if n := deliveredWarnings(t, a, s.Source.ID); n != 3 {
		t.Fatalf("delivered %d warnings, want 3", n)
	}
}

// A folded repeat keeps its retry identity: a sender's retry after a lost
// receipt is the duplicate it is, even once a resolved-warn ended the
// window, and a different alert reusing its sender_alert_id is refused.
func TestARetryOfAFoldedRepeatIsADuplicate(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	savedEmail(t, a, p, true)
	foldingSource(t, a, s)
	first := readIntakeAnswer(t, a, p, s, nodeWarn("w1", "disk", "/var is 95% full", "web-1"))
	repeat := nodeWarn("w2", "disk", "/var is 95% full", "web-2")
	readIntakeAnswer(t, a, p, s, repeat)
	readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"r1","sender_incident_key":"disk","severity":"resolved-warn","title":"Disk ok","message":""}`)
	retryAt := a.Now().Add(30 * time.Second)
	a.Now = func() time.Time { return retryAt }
	retry := readIntakeAnswer(t, a, p, s, repeat)
	if retry != (intakeAnswer{AlertID: first.AlertID, AlreadyReceived: true, RecordedAsRepeatOfEarlierAlert: true}) {
		t.Fatalf("answer to a retry: %+v", retry)
	}
	if n := scalarTeam(t, a, `SELECT repeat_count FROM alerts WHERE id=?`, first.AlertID); n != 1 {
		t.Fatalf("repeat count %d after a retry, want 1", n)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE source_id=? AND severity='warn'`, s.Source.ID); n != 1 {
		t.Fatalf("the retry was saved as a new alert: %d warns", n)
	}
	w := intakeHTTP(a, p, s, nodeWarn("w2", "disk", "/var is 97% full", "web-2"), a.Now())
	if w.Code != 400 || !strings.Contains(w.Body.String(), "sender_alert_id already used for a different alert") {
		t.Fatalf("a reused sender_alert_id: %d %s", w.Code, w.Body.String())
	}
}

// Past the bound of kept retry identities a repeat is not folded: it is
// admitted as its own warning, whose row keeps its identity, so a retry of
// it after a resolved-warn is still the duplicate it is, not a new warning.
func TestPastTheRetryIdentityBoundARepeatIsNotFolded(t *testing.T) {
	t.Parallel()
	a, p, s := alertFixture(t)
	savedEmail(t, a, p, true)
	foldingSource(t, a, s)
	if _, err := a.Store.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?) INSERT INTO folded_warning_repeats(source_id,replay_key,alert_id,content_sha256,received_at) SELECT ?,'earlier-'||i,'',x'00',? FROM n`, maxFoldedRepeatRetryIdentitiesPerAlertSource, s.Source.ID, a.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	readIntakeAnswer(t, a, p, s, nodeWarn("w1", "disk", "/var is 95% full", "web-1"))
	repeat := nodeWarn("w2", "disk", "/var is 95% full", "web-2")
	admitted := readIntakeAnswer(t, a, p, s, repeat)
	if admitted.RecordedAsRepeatOfEarlierAlert || admitted.AlreadyReceived {
		t.Fatalf("answer to a repeat past the bound: %+v; want it admitted as its own warning", admitted)
	}
	readIntakeAnswer(t, a, p, s, `{"sender_alert_id":"r1","sender_incident_key":"disk","severity":"resolved-warn","title":"Disk ok","message":""}`)
	retryAt := a.Now().Add(30 * time.Second)
	a.Now = func() time.Time { return retryAt }
	if retry := readIntakeAnswer(t, a, p, s, repeat); retry != (intakeAnswer{AlertID: admitted.AlertID, AlreadyReceived: true}) {
		t.Fatalf("answer to a retry: %+v; want the duplicate of %s", retry, admitted.AlertID)
	}
	if n := scalarTeam(t, a, `SELECT count(*) FROM alerts WHERE source_id=? AND severity='warn'`, s.Source.ID); n != 2 {
		t.Fatalf("%d warns; want the first and the repeat, and no retry", n)
	}
}

// The settings file's repeated_warnings reaches intake: a source set to
// fold folds a repeat after settings apply.
func TestTheSettingsFileSetsRepeatedWarnings(t *testing.T) {
	t.Parallel()
	f := newSettingsFileFixture(t, replaced(t, exampleSettingsYAML, "    sos_after_no_check_in_for_minutes: 5\n", "    sos_after_no_check_in_for_minutes: 5\n    repeated_warnings: fold_by_sender_incident_key_and_first_message_line\n"))
	f.start()
	ctx := context.Background()
	var ids []string
	for n := range 2 {
		body := []byte(nodeWarn(fmt.Sprintf("w%d", n), "disk", "/var is 95% full", fmt.Sprintf("web-%d", n)))
		alert, _, _, err := f.a.AdmitGeneric(ctx, "monitoring", alertSignature(exampleMonitoringSecret, f.a.Now(), body), body)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, alert.ID)
	}
	if ids[0] != ids[1] {
		t.Fatalf("the source set to fold saved two alerts: %v", ids)
	}
}

// The alert's page shows the repeats folded into it.
func TestTheAlertPageShowsItsRepeats(t *testing.T) {
	t.Parallel()
	r := newOnCallTeamRig(t)
	p := Principal{SignInEmailAddress: "maya@example.test"}
	source, err := createAlertSource(context.Background(), r.h.app, p, "Nodes while the hub is down")
	if err != nil {
		t.Fatal(err)
	}
	foldingSource(t, r.h.app, source)
	id := alertReceipt(t, intakeHTTP(r.h.app, p, source, nodeWarn("w1", "disk", "/var is 95% full", "web-1"), r.h.app.Now()))
	alertReceipt(t, intakeHTTP(r.h.app, p, source, nodeWarn("w2", "disk", "/var is 95% full", "web-2"), r.h.app.Now()))
	text := pageText(r.maya.get("/alerts/"+id, 200))
	if want := "Repeated 1 more time within the hour, and not delivered again. The latest repeat's title: web-2: /var is 95% full"; !strings.Contains(text, want) {
		t.Fatalf("the alert page lacks %q: %s", want, text)
	}
}

// The upgrade to repeated warnings keeps each alert source and alert of an
// earlier release: the source keeps every warning, as before, and the alert
// has no repeats.
func TestTheUpgradeKeepsAlertSourcesAndAlerts(t *testing.T) {
	t.Parallel()
	formatOne, err := os.ReadFile(filepath.Join("testdata", "database_format_1.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeDatabase(t, dir, 1, string(formatOne),
		`INSERT INTO team_info(name) VALUES('Released team')`,
		`INSERT INTO sources(id,name,kind,active,created_at) VALUES('monitoring','Monitoring','generic',1,1)`,
		`INSERT INTO alerts(id,source_id,sender_alert_id,actor,replay_key,incident_id,sender_incident_key,severity,title,message,notes,received_at) VALUES('a1','monitoring','w1','source:monitoring','sender:w1','','disk','warn','Disk full','/var is 95% full','',1)`)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var setting, title, lastRepeatTitle string
	var repeats int
	if err := store.db.QueryRow(`SELECT repeated_warnings FROM sources WHERE id='monitoring'`).Scan(&setting); err != nil || setting != repeatedWarningsKeepEach {
		t.Fatalf("the upgraded alert source: %q %v", setting, err)
	}
	if err := store.db.QueryRow(`SELECT title,repeat_count,last_repeat_title FROM alerts WHERE id='a1'`).Scan(&title, &repeats, &lastRepeatTitle); err != nil || title != "Disk full" || repeats != 0 || lastRepeatTitle != "" {
		t.Fatalf("the upgraded alert: %q %d %q %v", title, repeats, lastRepeatTitle, err)
	}
}
