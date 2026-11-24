package app

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Which alerts a report or a playbook run request of the triage agent is
// about: docs/changing-mmhotline/design/triage-agent.md#reports-and-requests-name-their-alerts.
//
// The agent names them with --about-alert-id, once per alert. A run that
// carried exactly one alert may leave the option out: the report or request
// then covers that one alert. One problem is not enough for that, because
// two warns can share one sender_incident_key, such as two affected versions
// of one CVE. An info report covers only the alerts it names, so an info
// report the agent sends on the way, or sends by mistake, cannot swallow a
// held warn that then reaches nobody. A named alert covers something only
// when the run carried it.
// A row records one alert one item covers; item_id is the request's ID
// within the run, as triage_agent_requests keys it, for a report as for a
// playbook run request. A held warn is taken over only by an item that
// covers it (heldWarnTakenOver), and a report that names alerts goes with
// their problems' incidents.
const triageAgentCoveredAlertsSchema = `
CREATE TABLE triage_agent_covered_alerts (
 run_id TEXT NOT NULL REFERENCES triage_agent_runs(id),
 item_kind TEXT NOT NULL CHECK(item_kind IN ('report','playbook_request')),
 item_id TEXT NOT NULL,
 alert_id TEXT NOT NULL REFERENCES alerts(id),
 PRIMARY KEY(run_id, item_kind, item_id, alert_id)
);
`

// triageAgentRequestReportAlertIDColumn is
// triage_agent_requests.report_alert_id, in the table and in the upgrade
// step that adds it (addHoldRules): the alert a report sent, so a later
// report about the same problem goes with the incident it opened.
const triageAgentRequestReportAlertIDColumn = `report_alert_id TEXT NOT NULL DEFAULT ''`

// The item kinds of triage_agent_covered_alerts.
const (
	coveredByReport          = "report"
	coveredByPlaybookRequest = "playbook_request"
)

// alertCoverage is what one report or playbook run request covers.
type alertCoverage struct {
	// carried is how many alerts the run carried to the agent; named says
	// whether the agent named alerts.
	carried int
	named   bool
	// infoReport: the item is a report at info, which covers only the
	// alerts it names.
	infoReport bool
	// covered are the alerts the item covers; notCarried are the alerts the
	// agent named that the run did not carry, which cover nothing.
	covered, notCarried []string
}

// recordCoveredAlerts records which alerts the item of itemKind and itemID
// of run runID covers, from the alert IDs the agent named; severity is a
// report's alert severity, "" for a playbook run request. The alerts a run
// carried are those its prompt showed: the alert MMHotline adds for an SOS
// report's own incident joins the session as sent by the run, but it is
// MMHotline's own and was never shown, so triageSessionAlertCondition leaves
// it out.
func recordCoveredAlerts(ctx context.Context, tx *sql.Tx, runID, itemKind, itemID, severity string, named []string) (alertCoverage, error) {
	carried, err := queryStrings(ctx, tx, `SELECT s.alert_id FROM triage_session_alerts s JOIN alerts ON alerts.id=s.alert_id WHERE s.sent_in_run_id=? AND `+triageSessionAlertCondition+` ORDER BY alerts.sequence`, runID)
	if err != nil {
		return alertCoverage{}, err
	}
	coverage := alertCoverage{carried: len(carried), named: len(named) > 0, infoReport: itemKind == coveredByReport && severity == "info"}
	switch {
	case coverage.named:
		for _, id := range named {
			switch {
			case slices.Contains(coverage.covered, id) || slices.Contains(coverage.notCarried, id):
			case slices.Contains(carried, id):
				coverage.covered = append(coverage.covered, id)
			default:
				coverage.notCarried = append(coverage.notCarried, id)
			}
		}
	case len(carried) == 1 && !coverage.infoReport:
		coverage.covered = carried
	}
	for _, id := range coverage.covered {
		if _, err = tx.ExecContext(ctx, `INSERT INTO triage_agent_covered_alerts(run_id,item_kind,item_id,alert_id) VALUES(?,?,?,?)`, runID, itemKind, itemID, id); err != nil {
			return alertCoverage{}, err
		}
	}
	return coverage, nil
}

// answerText is what MMHotline's answer to the agent adds about the
// coverage of its report or request, named by what: the named alerts the run
// did not carry, and, when the run carried several alerts, or one and the
// item is an info report, and the agent named none, that nothing is covered.
// With no alert carried there is nothing to name, and with one the item
// covers it.
func (c alertCoverage) answerText(what string) string {
	var parts []string
	if len(c.notCarried) > 0 {
		parts = append(parts, "MMHotline ignored --about-alert-id "+strings.Join(c.notCarried, ", ")+": this run did not carry those alerts, so your "+what+" does not cover them.")
	}
	if !c.named && c.carried > 1 {
		parts = append(parts, fmt.Sprintf("This run carried %d alerts, and your %s named none of them with --about-alert-id, so it covers no warning. A warning that no report or playbook run request of this run names reaches people when this run ends. Name each alert a report or request is about with --about-alert-id ALERT_ID, once per alert, as the prompt lists them.", c.carried, what))
	}
	if !c.named && c.carried == 1 && c.infoReport {
		parts = append(parts, "Your info report named no alert with --about-alert-id, so it covers no warning: an info report covers only the alerts it names. A warning that no report or playbook run request of this run names reaches people when this run ends. To close a warning with an info report, name the warning with --about-alert-id ALERT_ID.")
	}
	return strings.Join(parts, " ")
}

// incidentsOfNamedProblems lists the incidents of the session's alerts about
// the problems of the alerts the report requestID of run runID covers, named
// or the one alert its run carried, the one whose alert joined last first.
// A problem is an alert source and a sender_incident_key; an alert without a
// key is a problem of its own. The incident an earlier report about the same
// problem opened, with its own alert, is one of them, so a second report
// opens no second incident. covers is false when the report covers no
// alert; the caller then goes by the run's work (triageRunIncidents).
func incidentsOfNamedProblems(ctx context.Context, tx *sql.Tx, sessionID, runID, requestID string) (incidents []Incident, covers bool, err error) {
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_covered_alerts WHERE run_id=? AND item_kind=? AND item_id=?)`, runID, coveredByReport, requestID).Scan(&covers); err != nil || !covers {
		return nil, false, err
	}
	const sameProblem = `(%[1]s.id=%[2]s.id OR (%[1]s.sender_incident_key!='' AND %[1]s.source_id=%[2]s.source_id AND %[1]s.sender_incident_key=%[2]s.sender_incident_key))`
	incidents, err = incidentsOfTriageSessionAlerts(ctx, tx, `s.triage_session_id=? AND EXISTS(SELECT 1 FROM triage_agent_covered_alerts c JOIN alerts n ON n.id=c.alert_id WHERE c.run_id=? AND c.item_kind=? AND c.item_id=? AND (`+fmt.Sprintf(sameProblem, "n", "e")+`
		OR e.id IN (SELECT r.report_alert_id FROM triage_agent_requests r JOIN triage_agent_covered_alerts earlier ON earlier.run_id=r.run_id AND earlier.item_kind=? AND earlier.item_id=r.request_id JOIN alerts m ON m.id=earlier.alert_id WHERE r.report_alert_id!='' AND `+fmt.Sprintf(sameProblem, "m", "n")+`)))`, sessionID, runID, coveredByReport, requestID, coveredByReport)
	return incidents, true, err
}
