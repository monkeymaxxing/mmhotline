package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// triageSummary is the latest triage agent plain-English report explaining an
// alert: directly via triage_agent_covered_alerts, via an earlier alert of the
// same (source_id, sender_incident_key) cycle for resolved alerts, or via an
// SOS incident's latest triage_agent_report action.
type triageSummary struct {
	Title         string
	Message       string
	Severity      string
	ReceivedAt    time.Time
	ReportAlertID string
	SessionID     string
}

// triageSummaryForAlert loads the triage agent summary for one alert.
func (a *App) triageSummaryForAlert(ctx context.Context, p Principal, e Alert) (*triageSummary, error) {
	if e.SourceID == systemAlertSourceID {
		return nil, nil
	}
	var summary *triageSummary
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		summary, err = triageSummaryForAlertTx(ctx, tx, e)
		return err
	})
	return summary, err
}

// triageSummariesForAlerts batch-loads triage agent summaries for a slice of
// alerts without N+1 queries.
func (a *App) triageSummariesForAlerts(ctx context.Context, p Principal, alerts []Alert) (map[string]*triageSummary, error) {
	var summaries map[string]*triageSummary
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		summaries, err = triageSummariesForAlertsTx(ctx, tx, alerts)
		return err
	})
	return summaries, err
}

// triageSummaryForAlertTx loads the triage agent summary for one alert inside
// the caller's transaction.
func triageSummaryForAlertTx(ctx context.Context, tx *sql.Tx, e Alert) (*triageSummary, error) {
	if e.SourceID == systemAlertSourceID {
		return nil, nil
	}
	summaries, err := triageSummariesForAlertsTx(ctx, tx, []Alert{e})
	if err != nil {
		return nil, err
	}
	return summaries[e.ID], nil
}

// triageSummariesForAlertsTx batch-loads the latest triage agent report for
// each non-system alert in alerts using at most three queries:
//  1. Direct coverage in triage_agent_covered_alerts (item_kind='report').
//  2. For resolved-* alerts with sender_incident_key, coverage of prior
//     non-resolved alerts in the same (source_id, sender_incident_key) cycle.
//  3. Fallback for remaining alerts with incident_id to the latest
//     triage_agent_report row in incident_actions.
func triageSummariesForAlertsTx(ctx context.Context, tx *sql.Tx, alerts []Alert) (map[string]*triageSummary, error) {
	summaries := make(map[string]*triageSummary)
	var candidateIDs []string
	candidates := make([]Alert, 0, len(alerts))
	for _, e := range alerts {
		if e.SourceID == systemAlertSourceID || e.ID == "" {
			continue
		}
		candidates = append(candidates, e)
		candidateIDs = append(candidateIDs, e.ID)
	}
	if len(candidates) == 0 {
		return summaries, nil
	}

	// 1. Direct coverage via triage_agent_covered_alerts -> triage_agent_requests -> alerts.
	if err := readDirectTriageSummaries(ctx, tx, candidateIDs, summaries); err != nil {
		return nil, err
	}

	// 2. Resolved alerts with sender_incident_key: look up prior non-resolved
	// alerts in the same incident cycle (after any earlier resolved-* of that key).
	var resolvedIDs []string
	for _, e := range candidates {
		if summaries[e.ID] != nil {
			continue
		}
		if strings.HasPrefix(e.Severity, "resolved-") && e.SenderIncidentKey != "" {
			resolvedIDs = append(resolvedIDs, e.ID)
		}
	}
	if len(resolvedIDs) > 0 {
		if err := readResolvedCycleTriageSummaries(ctx, tx, resolvedIDs, summaries); err != nil {
			return nil, err
		}
	}

	// 3. Fallback for remaining alerts attached to an SOS incident: latest
	// triage_agent_report action on that incident_id.
	incidentToAlertIDs := make(map[string][]string)
	var incidentIDs []string
	for _, e := range candidates {
		if summaries[e.ID] != nil || e.IncidentID == "" {
			continue
		}
		if _, seen := incidentToAlertIDs[e.IncidentID]; !seen {
			incidentIDs = append(incidentIDs, e.IncidentID)
		}
		incidentToAlertIDs[e.IncidentID] = append(incidentToAlertIDs[e.IncidentID], e.ID)
	}
	if len(incidentIDs) > 0 {
		if err := readIncidentActionTriageSummaries(ctx, tx, incidentIDs, incidentToAlertIDs, summaries); err != nil {
			return nil, err
		}
	}

	return summaries, nil
}

func stringSliceArgs(values []string) []any {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return args
}

func readDirectTriageSummaries(ctx context.Context, tx *sql.Tx, alertIDs []string, out map[string]*triageSummary) error {
	query := `SELECT c.alert_id, ra.id, ra.title, COALESCE(NULLIF(q.report_text, ''), ra.message), ra.severity, ra.received_at, COALESCE(r.triage_session_id, '')
FROM triage_agent_covered_alerts c
JOIN triage_agent_requests q ON q.run_id=c.run_id AND q.request_id=c.item_id
JOIN alerts ra ON ra.id=q.report_alert_id
LEFT JOIN triage_agent_runs r ON r.id=c.run_id
WHERE c.item_kind=? AND q.report_alert_id!='' AND c.alert_id IN (` + sqlPlaceholders(len(alertIDs)) + `)
ORDER BY ra.sequence DESC, q.requested_at DESC, q.rowid DESC`
	args := append([]any{coveredByReport}, stringSliceArgs(alertIDs)...)
	return scanAlertTriageSummaries(ctx, tx, query, args, out)
}

func readResolvedCycleTriageSummaries(ctx context.Context, tx *sql.Tx, resolvedIDs []string, out map[string]*triageSummary) error {
	query := `SELECT e.id, ra.id, ra.title, COALESCE(NULLIF(q.report_text, ''), ra.message), ra.severity, ra.received_at, COALESCE(r.triage_session_id, '')
FROM alerts e
JOIN alerts p ON p.source_id=e.source_id
 AND p.sender_incident_key=e.sender_incident_key
 AND p.severity NOT LIKE 'resolved-%'
 AND p.sequence<e.sequence
 AND p.sequence>COALESCE((
   SELECT MAX(prev.sequence) FROM alerts prev
   WHERE prev.source_id=e.source_id
     AND prev.sender_incident_key=e.sender_incident_key
     AND prev.severity LIKE 'resolved-%'
     AND prev.sequence<e.sequence
 ), 0)
JOIN triage_agent_covered_alerts c ON c.alert_id=p.id AND c.item_kind=?
JOIN triage_agent_requests q ON q.run_id=c.run_id AND q.request_id=c.item_id AND q.report_alert_id!=''
JOIN alerts ra ON ra.id=q.report_alert_id
LEFT JOIN triage_agent_runs r ON r.id=c.run_id
WHERE e.id IN (` + sqlPlaceholders(len(resolvedIDs)) + `)
ORDER BY ra.sequence DESC, q.requested_at DESC, q.rowid DESC`
	args := append([]any{coveredByReport}, stringSliceArgs(resolvedIDs)...)
	return scanAlertTriageSummaries(ctx, tx, query, args, out)
}

func scanAlertTriageSummaries(ctx context.Context, tx *sql.Tx, query string, args []any, out map[string]*triageSummary) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var alertID, reportAlertID, rawTitle, message, severity, sessionID string
		var receivedAt int64
		if err = rows.Scan(&alertID, &reportAlertID, &rawTitle, &message, &severity, &receivedAt, &sessionID); err != nil {
			rows.Close()
			return err
		}
		if out[alertID] != nil {
			continue
		}
		out[alertID] = &triageSummary{
			Title:         plainTriageReportTitle(rawTitle, message),
			Message:       message,
			Severity:      severity,
			ReceivedAt:    deadlineTime(receivedAt),
			ReportAlertID: reportAlertID,
			SessionID:     sessionID,
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

func readIncidentActionTriageSummaries(ctx context.Context, tx *sql.Tx, incidentIDs []string, incidentToAlertIDs map[string][]string, out map[string]*triageSummary) error {
	query := `SELECT ia.incident_id, ia.related_alert_id, COALESCE(ra.title, ''), COALESCE(NULLIF(ia.message, ''), COALESCE(ra.message, '')), COALESCE(ra.severity, ''), COALESCE(ra.received_at, ia.occurred_at), COALESCE(r.triage_session_id, '')
FROM incident_actions ia
LEFT JOIN alerts ra ON ra.id=ia.related_alert_id AND ia.related_alert_id!=''
LEFT JOIN triage_agent_requests q ON q.report_alert_id=ia.related_alert_id AND ia.related_alert_id!=''
LEFT JOIN triage_agent_runs r ON r.id=q.run_id
WHERE ia.kind='triage_agent_report' AND ia.incident_id IN (` + sqlPlaceholders(len(incidentIDs)) + `)
ORDER BY ia.id DESC`
	rows, err := tx.QueryContext(ctx, query, stringSliceArgs(incidentIDs)...)
	if err != nil {
		return err
	}
	seenIncident := make(map[string]bool, len(incidentIDs))
	for rows.Next() {
		var incidentID, reportAlertID, rawTitle, message, severity, sessionID string
		var receivedAt int64
		if err = rows.Scan(&incidentID, &reportAlertID, &rawTitle, &message, &severity, &receivedAt, &sessionID); err != nil {
			rows.Close()
			return err
		}
		if seenIncident[incidentID] {
			continue
		}
		seenIncident[incidentID] = true
		summary := &triageSummary{
			Title:         plainTriageReportTitle(rawTitle, message),
			Message:       message,
			Severity:      severity,
			ReceivedAt:    deadlineTime(receivedAt),
			ReportAlertID: reportAlertID,
			SessionID:     sessionID,
		}
		for _, alertID := range incidentToAlertIDs[incidentID] {
			if out[alertID] == nil {
				out[alertID] = summary
			}
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// plainTriageReportTitle returns the plain-English headline of a triage agent
// report without the "The AI triage agent reports: " prefix.
func plainTriageReportTitle(alertTitle, reportText string) string {
	trimmed := strings.TrimSpace(alertTitle)
	if stripped, ok := strings.CutPrefix(trimmed, "The AI triage agent reports: "); ok {
		trimmed = strings.TrimSpace(stripped)
	}
	if trimmed != "" && trimmed != "The AI triage agent sent a report" {
		return trimmed
	}
	return reportTitleLine(reportText)
}

// resolvedDisplayTitle formats a plain-English title for display or
// notification, prefixing "Resolved: " for resolved-* severities unless the
// title already starts with "Resolved".
func resolvedDisplayTitle(severity, title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	if !strings.HasPrefix(severity, "resolved-") {
		return title
	}
	lower := strings.ToLower(title)
	if strings.HasPrefix(lower, "resolved:") || strings.HasPrefix(lower, "resolved ") || lower == "resolved" {
		return title
	}
	return "Resolved: " + title
}

// displayAlertTitle picks the plain-English title for an alert: the triage
// agent's summary title when present, otherwise a humanized title for
// non-system alerts, leaving system alerts and ordinary titles unchanged.
func displayAlertTitle(e Alert, summary *triageSummary) string {
	if summary != nil && summary.Title != "" {
		return resolvedDisplayTitle(e.Severity, summary.Title)
	}
	if e.SourceID != systemAlertSourceID {
		return humanizeAlertTitle(e.Title)
	}
	return e.Title
}

// humanizeAlertTitle replaces raw monitoring check slugs of the form
// check_group/check_name inside an alert title with plain English (using
// plainCheckName), leaving ordinary human titles, URLs, and file paths
// completely untouched.
func humanizeAlertTitle(title string) string {
	if !strings.Contains(title, "/") || !strings.Contains(title, "_") {
		return title
	}
	var out strings.Builder
	changed := false
	i := 0
	for i < len(title) {
		r, size := utf8.DecodeRuneInString(title[i:])
		if unicode.IsSpace(r) {
			if changed {
				out.WriteString(title[i : i+size])
			}
			i += size
			continue
		}
		start := i
		for i < len(title) {
			r2, s2 := utf8.DecodeRuneInString(title[i:])
			if unicode.IsSpace(r2) {
				break
			}
			i += s2
		}
		token := title[start:i]
		replaced := humanizeCheckSlugToken(token)
		if replaced != token {
			if !changed {
				out.Grow(len(title))
				out.WriteString(title[:start])
				changed = true
			}
			out.WriteString(replaced)
		} else if changed {
			out.WriteString(token)
		}
	}
	if !changed {
		return title
	}
	return out.String()
}

// humanizeCheckSlugToken checks one whitespace-delimited token for a
// monitoring check slug (<lower_ident>/<lower_ident> where at least one side
// contains an underscore) optionally wrapped in ordinary punctuation, and
// replaces only the slug with plainCheckName. URLs, file paths, filenames with
// extensions, and ordinary prose tokens are returned unchanged.
func humanizeCheckSlugToken(token string) string {
	if !strings.Contains(token, "/") || !strings.Contains(token, "_") {
		return token
	}
	lead := 0
	for lead < len(token) {
		r, size := utf8.DecodeRuneInString(token[lead:])
		if !isCheckSlugLeadingPunctuation(r) {
			break
		}
		lead += size
	}
	trail := len(token)
	for trail > lead {
		r, size := utf8.DecodeLastRuneInString(token[lead:trail])
		if !isCheckSlugTrailingPunctuation(r) {
			break
		}
		trail -= size
	}
	core := token[lead:trail]
	left, right, ok := strings.Cut(core, "/")
	if !ok || strings.Contains(right, "/") {
		return token
	}
	if !isCheckSlugIdent(left) || !isCheckSlugIdent(right) {
		return token
	}
	if !strings.Contains(left, "_") && !strings.Contains(right, "_") {
		return token
	}
	return token[:lead] + plainCheckName(core) + token[trail:]
}

func isCheckSlugLeadingPunctuation(r rune) bool {
	switch r {
	case '(', '[', '"', '\'', '“':
		return true
	}
	return false
}

func isCheckSlugTrailingPunctuation(r rune) bool {
	switch r {
	case ':', ',', ';', ')', ']', '"', '\'', '”', '!', '?', '.':
		return true
	}
	return false
}

func isCheckSlugIdent(s string) bool {
	if len(s) == 0 || s[0] < 'a' || s[0] > 'z' || s[len(s)-1] == '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// applyPlainEnglishNotification rewrites a non-system notification's title to
// lead with plain English: the triage agent's plain-English title for
// resolved-warn / resolved-sos alerts when available (preserving a server
// replacement's "Your site works again: ..." partner title), or a humanized
// check title otherwise. When the title changes, the original raw title is
// preserved at the bottom of the notification body.
func applyPlainEnglishNotification(ctx context.Context, tx *sql.Tx, n *Notification) error {
	if n.AlertID == "" || n.Purpose == "system_notice" {
		return nil
	}
	e, err := readAlertBy(ctx, tx, "id=?", n.AlertID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil || e.SourceID == systemAlertSourceID {
		return err
	}
	if e.IncidentID == "" && n.IncidentID != "" {
		e.IncidentID = n.IncidentID
	}
	newTitle := ""
	if n.Severity == "resolved-warn" || (n.Severity == "resolved-sos" && !strings.HasPrefix(n.Title, "Your site works again: ")) {
		summary, err := triageSummaryForAlertTx(ctx, tx, e)
		if err != nil {
			return err
		}
		if summary != nil && summary.Title != "" {
			newTitle = resolvedDisplayTitle(n.Severity, summary.Title)
		}
	}
	if newTitle == "" {
		newTitle = humanizeAlertTitle(n.Title)
	}
	if newTitle == "" || newTitle == n.Title {
		return nil
	}
	rawLine := "Original alert: " + n.Title
	if strings.TrimSpace(n.Message) == "" {
		n.Message = rawLine
	} else {
		n.Message = strings.TrimRight(n.Message, "\n") + "\n\n" + rawLine
	}
	n.Title = newTitle
	return nil
}
