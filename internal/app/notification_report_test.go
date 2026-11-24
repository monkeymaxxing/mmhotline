package app

import (
	"context"
	"database/sql"
	"html"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mmhotline/internal/fakesendingservices"
)

const reportNotificationExample = "# Test of the alerts on web01: nothing is wrong\n\n## What happened\n\nThe alert came from the test the setup sends. Nothing on web01 needs a fix. Claim it to end the test.\n\n## Evidence\n\n- Oct 29 06:33:20 web01 sshd[812]: Accepted publickey for root from 203.0.113.7 port 50212 ssh2: ED25519 SHA256:..."
const reportNotificationSummary = "The alert came from the test the setup sends. Nothing on web01 needs a fix. Claim it to end the test.\n\nOpen MMHotline for the whole report."

func TestTriageReportNotificationParagraph(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, report, want string }{
		{"plain text", "# A report\n\n## What happened\n\nThe **service** on `web01`\nneeds [attention](https://example.com).\n\n## Evidence\nPrivate evidence", "The service on web01 needs attention."},
		{"no paragraph", "# A report\n\n- Private evidence", ""},
		{"title only", "# A report", ""},
		{"empty", "", ""},
		{"heading ends paragraph", "# Title\nSummary\n## Evidence\nPrivate evidence", "Summary"},
		{"list ends paragraph", "# Title\nSummary\n1. Private evidence", "Summary"},
		{"fence ends paragraph", "# Title\nSummary\n```text\nPrivate evidence", "Summary"},
		{"tilde fence ends paragraph", "# Title\nSummary\n~~~\nPrivate evidence", "Summary"},
		{"quote ends paragraph", "# Title\nSummary\n> Private evidence", "Summary"},
		{"table ends paragraph", "# Title\nSummary\n| Private | evidence |", "Summary"},
		{"word bound", "# Title\n\n" + strings.Repeat("word ", 80), strings.TrimSpace(strings.Repeat("word ", 59)) + "…"},
		{"Unicode bound", "# Title\n\n" + strings.Repeat("café ", 80), strings.TrimSpace(strings.Repeat("café ", 59)) + "…"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := "Open MMHotline for the whole report."
			if test.want != "" {
				want = test.want + "\n\n" + want
			}
			got := triageAgentReportNotificationMessage(test.report)
			if got != want {
				t.Fatalf("got %q; want %q", got, want)
			}
			if summary, _, _ := strings.Cut(got, "\n\n"); utf8.RuneCountInString(summary) > 300 {
				t.Fatal("summary exceeds 300 characters")
			}
		})
	}
}

func TestTriageReportNotificationsKeepEvidenceOnPages(t *testing.T) {
	t.Parallel()
	for _, severity := range []string{"warn", "info", "sos"} {
		t.Run(severity, func(t *testing.T) {
			r := newPushoverRig(t)
			r.add(t, fakesendingservices.PushoverUserKey)
			ctx := context.Background()
			runID := NewID()
			if _, err := r.a.Store.db.Exec(`INSERT INTO triage_agent_runs(id,purpose,coding_agent,credential_type,state,started_at,started_by,ended_at) VALUES(?,'software_review','codex','api_key','finished',?,'MMHotline',?)`, runID, r.a.Now().Unix(), r.a.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			if err := r.a.deliverTriageAgentReport(ctx, runID, "", false, severity, reportNotificationExample); err != nil {
				t.Fatal(err)
			}
			alert := reportAlert(t, r.a, "The AI triage agent reports: Test of the alerts on web01: nothing is wrong")
			request := httptest.NewRequest("GET", r.a.WebURL+"/alerts/"+alert.ID, nil)
			addAccessToken(request, r.principal.SignInEmailAddress)
			response := httptest.NewRecorder()
			r.a.Handler().ServeHTTP(response, request)
			if response.Code != 200 || !strings.Contains(html.UnescapeString(response.Body.String()), reportNotificationExample) {
				t.Fatalf("report page lost the report: %d %s", response.Code, response.Body.String())
			}
			if severity == "sos" {
				var message string
				if err := r.a.Store.db.QueryRow(`SELECT alert_message FROM deliveries WHERE incident_id=? AND purpose='sos_escalation'`, alert.IncidentID).Scan(&message); err != nil {
					t.Fatal(err)
				}
				if message != reportNotificationSummary {
					t.Fatalf("escalation: %q", message)
				}
			}
			r.dispatch(t)
			sent := r.fake.PushoverMessages()
			last := sent[len(sent)-1]
			want := reportNotificationSummary
			if severity == "sos" {
				want += "\n\nAcknowledge this to tell MMHotline you are handling it. MMHotline then stops contacting other people."
			} else {
				want += "\nAt: " + alert.ReceivedAt.UTC().Format(time.RFC3339)
			}
			if last.Title != "["+severityLabel(severity)+"] "+alert.Title || last.Message != want || strings.Contains(last.Message, "Evidence") {
				t.Fatalf("sent: %+v; want message %q", last, want)
			}
			t.Logf("Pushover title: %s\nPushover message: %s", last.Title, last.Message)
			if severity == "sos" {
				incident, err := r.a.GetIncident(ctx, r.principal, alert.IncidentID)
				if err != nil {
					t.Fatal(err)
				}
				// A resolving alert can be from another actor; the message is still
				// the report that opened the incident.
				err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
					resolvingAlert, err := admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", GenericAlert{Severity: "info", Title: "Recovered", Message: "Recovery detail"}, nil, r.a.Now())
					if err != nil {
						return err
					}
					_, err = resolveIncidentTx(ctx, tx, incident, "system", resolvingAlert.ID, "", "", r.a.Now())
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				var message string
				if err := r.a.Store.db.QueryRow(`SELECT alert_message FROM deliveries WHERE incident_id=? AND severity='resolved-sos'`, incident.ID).Scan(&message); err != nil {
					t.Fatal(err)
				}
				if message != reportNotificationSummary {
					t.Fatalf("resolved: %q", message)
				}
			}
		})
	}
}

func TestOtherActorsKeepWholeNotificationMessage(t *testing.T) {
	t.Parallel()
	r := newPushoverRig(t)
	r.add(t, fakesendingservices.PushoverUserKey)
	err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		return RecordSystemAlert(context.Background(), tx, "other_actor", "", "warn", "Report from another sender", reportNotificationExample, r.a.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	var message string
	if err := r.a.Store.db.QueryRow(`SELECT alert_message FROM deliveries WHERE severity='warn'`).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if message != reportNotificationExample {
		t.Fatalf("other sender's message changed: %q", message)
	}
}

func TestTriageReportSummaryReachesEveryContactMethodType(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	p := memberFixture(t, a, "ana@example.test").Principal
	member, err := a.CurrentMember(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	types := []string{"email", "outgoing_webhook", "telegram_message", "slack_message", "pushover_notification", "automated_voice_call"}
	for _, kind := range types {
		id := NewID()
		if _, err := a.Store.db.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted,deleted) VALUES(?,?,?,'{}','example',1,1,0)`, id, member.ID, kind); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Store.db.Exec(`INSERT INTO contact_method_subscriptions(contact_method_id,severity) VALUES(?,'warn')`, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, report := range []string{reportNotificationExample, "# Report\n\n- Evidence only"} {
		err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
			return RecordSystemAlert(context.Background(), tx, "triage_agent", "", "warn", triageAgentReportTitle(report), report, a.Now())
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := a.Store.db.Query(`SELECT contact_method_type,title,alert_message,message FROM deliveries ORDER BY contact_method_type`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind, title, raw, rendered string
		if err := rows.Scan(&kind, &title, &raw, &rendered); err != nil {
			t.Fatal(err)
		}
		want := reportNotificationSummary
		if title == "The AI triage agent reports: Report" {
			want = "Open MMHotline for the whole report."
		}
		if raw != want || !strings.Contains(rendered, want) || strings.Contains(rendered, "Evidence") {
			t.Fatalf("%s notification: %q / %q", kind, raw, rendered)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2*len(types) {
		t.Fatalf("got %d notifications; want %d", count, 2*len(types))
	}
}
