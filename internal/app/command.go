package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CommandRequest is accepted only on the kernel-peer-checked command socket.
// The group inputs use typed fields so unknown options fail at the boundary.
type CommandRequest struct {
	AgentPlaybookRules AgentPlaybookRulesCommandRequest `json:"agent_playbook_rules"`
	UnixUser           string                           `json:"unix_user"`
	Group              string                           `json:"group"`
	Action             string                           `json:"action"`
	People             PeopleCommandRequest             `json:"people"`
	ContactMethods     ContactMethodsCommandRequest     `json:"contact_methods"`
	AlertSources       AlertSourcesCommandRequest       `json:"alert_sources"`
	Schedules          SchedulesCommandRequest          `json:"schedules"`
	StatusPage         StatusPageCommandRequest         `json:"status_page"`
	// The read-only groups: what happened, for people and coding agents on
	// the server, who cannot reach the pages behind Cloudflare Access.
	Incidents        IncidentsCommandRequest        `json:"incidents"`
	Alerts           AlertsCommandRequest           `json:"alerts"`
	TriageSessions   TriageSessionsCommandRequest   `json:"triage_sessions"`
	ApprovalRequests ApprovalRequestsCommandRequest `json:"approval_requests"`
	CommitReviews    CommitReviewsCommandRequest    `json:"commit_reviews"`
}

type CommandResult struct {
	// SettingsFile is what settings apply and settings show print.
	SettingsFile       *SettingsFileView             `json:"settings_file,omitempty"`
	TriageAgent        *TriageAgentView              `json:"triage_agent,omitempty"`
	TriageAgentRunID   string                        `json:"triage_agent_run_id,omitempty"`
	ReviewAgent        *ReviewAgentView              `json:"review_agent,omitempty"`
	ReviewAgentRunID   string                        `json:"review_agent_run_id,omitempty"`
	AgentPlaybookRules []InventoryAgentPlaybookRules `json:"agent_playbook_rules,omitempty"`
	// ServicesWhoseChangesAPersonMustApprove is the list agent-playbook-rules show prints.
	ServicesWhoseChangesAPersonMustApprove *ServicesWhoseChangesAPersonMustApprove `json:"services_whose_changes_a_person_must_approve,omitempty"`
	// WhetherAPlaybookNeedsAPerson is what agent-playbook-rules
	// check-whether-a-playbook-needs-a-person prints.
	WhetherAPlaybookNeedsAPerson *CommandWhetherAPlaybookNeedsAPerson `json:"whether_a_playbook_needs_a_person,omitempty"`
	PeopleCommandResult
	SendingServiceSetups          []SendingServiceSetup  `json:"sending_service_setups,omitempty"`
	ContactMethods                []CommandContactMethod `json:"contact_methods,omitempty"`
	PushoverGroupKeyWarning       string                 `json:"pushover_group_key_warning,omitempty"`
	PushoverPhoneNotProvedWarning string                 `json:"pushover_phone_not_proved_warning,omitempty"`
	// PushoverDeviceNames are the devices Pushover lists for a user key
	// that connect-pushover checked.
	PushoverDeviceNames                   []string                `json:"pushover_device_names,omitempty"`
	AlertSources                          []CommandAlertSource    `json:"alert_sources,omitempty"`
	AlertIntakeAddress                    string                  `json:"alert_intake_address,omitempty"`
	OutgoingWebhookSigningSecretID        string                  `json:"outgoing_webhook_signing_secret_id,omitempty"`
	OutgoingWebhookSigningSecretShownOnce string                  `json:"outgoing_webhook_signing_secret_shown_once,omitempty"`
	OutgoingWebhookSigningSecretFile      string                  `json:"outgoing_webhook_signing_secret_file,omitempty"`
	ScheduleState                         *CommandScheduleState   `json:"schedule_state,omitempty"`
	Preview                               []CommandScheduleChange `json:"preview,omitempty"`
	StatusPage                            *StatusPageView         `json:"status_page,omitempty"`
	StatusPageNoticeID                    int64                   `json:"notice_id,omitempty"`
	DownAlarm                             *DownAlarmView          `json:"mmhotline_down_alarm,omitempty"`
	// A list command's list is present, also when empty.
	Incidents []CommandIncident `json:"incidents,omitzero"`
	Incident  *CommandIncident  `json:"incident,omitempty"`
	// TestSOS is what `incidents send-test-sos` prints.
	TestSOS          *CommandTestSOS          `json:"test_sos,omitempty"`
	Alerts           []CommandAlert           `json:"alerts,omitzero"`
	TriageSessions   []CommandTriageSession   `json:"triage_sessions,omitzero"`
	TriageSession    *CommandTriageSession    `json:"triage_session,omitempty"`
	ApprovalRequests []CommandApprovalRequest `json:"approval_requests,omitzero"`
	ApprovalRequest  *CommandApprovalRequest  `json:"approval_request,omitempty"`
	CommitCheck      *CommandCommitCheck      `json:"commit_check,omitempty"`
	CommitReviews    []CommandCommitReview    `json:"commit_reviews,omitzero"`
	CommitReview     *CommandCommitReview     `json:"commit_review,omitempty"`
	// CommitsTheTriageAgentApproved is in commit-reviews list, newest first.
	CommitsTheTriageAgentApproved []CommandCommitReview `json:"commits_the_triage_agent_approved,omitzero"`
}

// CommandHandler must only be served after checking Unix peer credentials.
// The web and intake listeners never register this route.
func (a *App) CommandHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /command", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var request CommandRequest
		// Custom schedule source is bounded at 64 KiB; JSON escaping and 50 test
		// cases need more room. Domain validation retains its own smaller bounds.
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			commandJSONError(w, fmt.Errorf("%w: invalid command JSON", ErrInvalid))
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			commandJSONError(w, ErrInvalid)
			return
		}
		if request.UnixUser == "" {
			request.UnixUser = "root"
		}
		principal := Principal{UnixUser: request.UnixUser}
		result := CommandResult{PeopleCommandResult: PeopleCommandResult{WebURL: a.WebURL}}
		var err error
		switch request.Group {
		case "settings":
			result.WebURL = ""
			err = a.settingsCommand(r.Context(), request.Action, &result)
		case "triage-agent":
			err = a.triageAgentCommand(r.Context(), principal, request.Action, &result)
		case "review-agent":
			err = a.reviewAgentCommand(r.Context(), principal, request.Action, &result)
		case "agent-playbook-rules":
			err = a.agentPlaybookRulesCommand(r.Context(), principal, request.Action, request.AgentPlaybookRules, &result)
		case "sending-services":
			err = a.sendingServicesCommand(r.Context(), principal, request.Action, &result)
		case "people":
			err = a.peopleCommand(r.Context(), principal, request.Action, request.People, &result)
		case "contact-methods":
			err = a.contactMethodsCommand(r.Context(), principal, request.Action, request.ContactMethods, &result)
		case "alert-sources":
			result.WebURL = ""
			result.AlertIntakeAddress = a.alertIntakeAddress()
			err = a.alertSourcesCommand(r.Context(), principal, request.Action, request.AlertSources, &result)
		case "schedules":
			err = a.schedulesCommand(r.Context(), principal, request.Action, request.Schedules, &result)
		case "down-alarm":
			result.WebURL = ""
			err = a.downAlarmCommand(r.Context(), principal, request.Action, &result)
		case "status-page":
			result.WebURL = ""
			err = a.statusPageCommand(r.Context(), principal, request.Action, request.StatusPage, &result)
		case "incidents":
			err = a.incidentsCommand(r.Context(), principal, request.Action, request.Incidents, &result)
		case "alerts":
			err = a.alertsCommand(r.Context(), principal, request.Action, request.Alerts, &result)
		case "triage-sessions":
			err = a.triageSessionsCommand(r.Context(), principal, request.Action, request.TriageSessions, &result)
		case "approval-requests":
			err = a.approvalRequestsCommand(r.Context(), principal, request.Action, request.ApprovalRequests, &result)
		case "commit-reviews":
			err = a.commitReviewsCommand(r.Context(), principal, request.Action, request.CommitReviews, &result)
		default:
			err = ErrNotFound
		}
		if err != nil {
			commandJSONError(w, err)
			return
		}
		// The operation is done; a failed write means the command went away.
		_ = json.NewEncoder(w).Encode(result)
	})
	return mux
}
func commandJSONError(w http.ResponseWriter, err error) {
	w.WriteHeader(errorStatus(err))
	// A failed write means the command went away; nothing is left to do.
	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
	}{publicError(err)})
}

// Text is shared by every client command; JSON contains the same facts.
func (r CommandResult) Text() string {
	var lines []string
	if r.SettingsFile != nil {
		lines = append(lines, r.SettingsFile.Text())
	}
	if r.TriageAgent != nil {
		lines = append(lines, r.TriageAgent.Text())
	}
	if r.ReviewAgent != nil {
		lines = append(lines, r.ReviewAgent.Text())
	}
	if r.ServicesWhoseChangesAPersonMustApprove != nil {
		lines = append(lines, r.ServicesWhoseChangesAPersonMustApprove.Text())
	}
	lines = append(lines, agentPlaybookRulesText(r.AgentPlaybookRules)...)
	if r.WhetherAPlaybookNeedsAPerson != nil {
		lines = append(lines, r.WhetherAPlaybookNeedsAPerson.Text())
	}
	for _, setup := range r.SendingServiceSetups {
		if !setup.IsSetUp {
			lines = append(lines, setup.SendingServiceTitle+": not set up.")
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: set up by %s at %s. %s.", setup.SendingServiceTitle, setup.SetUpBy, commandTime(*setup.SetUpAt), strings.TrimSuffix(setup.Description(), ".")))
	}
	for _, person := range r.People {
		lines = append(lines, fmt.Sprintf("%s (%s) is %s.", person.Name, person.SignInEmailAddress, person.Role))
	}
	for _, c := range r.ContactMethods {
		owner := c.SignInEmailAddress
		if c.SharedWithTeam {
			owner = "the team"
		}
		test := "no test message yet"
		if c.LastTestMessage != nil {
			test = c.LastTestMessage.DeliveryState + " (queued at " + commandTime(c.LastTestMessage.QueuedAt) + ")"
			if c.LastTestMessage.DeliveryStateExplanation != "" {
				test += ": " + c.LastTestMessage.DeliveryStateExplanation
			}
		}
		lines = append(lines, fmt.Sprintf("Contact method %s belongs to %s.\nContact method type: %s. Sends to: %s. Alert severities: %s.\nSend while unavailable or muted: %t. Last test message: %s.", c.ContactMethodID, owner, c.ContactMethodType, c.SendsTo, strings.Join(c.AlertSeverities, ","), c.SendWhileUnavailableOrMuted, test))
		if c.PushoverEmergencyTest != nil {
			lines = append(lines, c.PushoverEmergencyTest.Explanation)
		}
		if c.PushoverAcknowledgementProblem != "" {
			lines = append(lines, "Problem: "+c.PushoverAcknowledgementProblem)
		}
	}
	for _, s := range r.AlertSources {
		status := "active"
		if !s.Active {
			status = "disabled"
		}
		lines = append(lines, fmt.Sprintf("Alert source %s (%s) is %s.", s.Name, s.AlertSourceID, status), "Intake URL: "+s.IntakeURL, "Check-in URL: "+s.CheckInURL)
		checkIns := s.CheckInState
		if checkIns.Expected {
			lines = append(lines, fmt.Sprintf("Last check-in received: %s. Sent from server: %s.", commandTime(checkIns.LastCheckInAt), checkIns.SentByNodeName))
		} else {
			lines = append(lines, "No check-in has arrived yet. MMHotline starts watching for missing check-ins when the first check-in arrives.")
		}
		warning := "Warn after " + plural(checkIns.WarnAfterNoCheckInForMinutes, "minute") + " without a check-in."
		if checkIns.WarnAfterNoCheckInForMinutes == 0 {
			warning = "No warning for missing check-ins."
		}
		lines = append(lines, fmt.Sprintf("Last heard from the alert source: %s. %s Wake the person on call (SOS) after %s without a check-in.", commandTime(checkIns.LastHeardAt), warning, plural(checkIns.SOSAfterNoCheckInForMinutes, "minute")))
		for _, secret := range s.SigningSecrets {
			lines = append(lines, fmt.Sprintf("Signing secret %s was created at %s.", secret.SigningSecretID, commandTime(secret.CreatedAt)))
		}
		for _, node := range s.CheckInState.NodesInWholeNodeMaintenance {
			lines = append(lines, fmt.Sprintf("Server %s is in maintenance until %s, started by %s: %s.", node.NodeName, commandTime(node.MaintenanceUntil), node.StartedBy, node.Reason))
		}
		if downtime := s.CheckInState.PlannedDowntime; downtime != nil {
			lines = append(lines, downtime.Text())
		}
	}
	if s := r.ScheduleState; s != nil {
		lines = append(lines, fmt.Sprintf("Active schedule type: %s. Schedule revision: %d.", s.ActiveScheduleType, s.Revision))
		lines = append(lines, "Time to claim before MMHotline escalates: "+plural(s.EscalateIfUnclaimedAfterMinutes, "minute")+". Each escalation step waits this long for a claim before the next escalation step starts.")
		for _, c := range s.ScheduleConfigurations {
			lines = append(lines, c.Text())
		}
		if s.OnCallNow != nil {
			lines = append(lines, s.OnCallNow.Text())
		}
	}
	for _, change := range r.Preview {
		lines = append(lines, fmt.Sprintf("At %s, primary: %s; backup option: %s; backup: %s.", commandTime(change.At), displaySignInEmailAddress(change.PrimarySignInEmailAddress), change.BackupOption, commandBackup(change.BackupOption, change.BackupSignInEmailAddress)))
	}
	for _, value := range []struct{ label, value string }{
		{"Alert intake address", r.AlertIntakeAddress},
		{"Outgoing webhook signing secret ID", r.OutgoingWebhookSigningSecretID}, {"Outgoing webhook signing secret (shown only once)", r.OutgoingWebhookSigningSecretShownOnce}, {"Outgoing webhook signing secret file", r.OutgoingWebhookSigningSecretFile},
	} {
		if value.value != "" {
			lines = append(lines, value.label+": "+value.value)
		}
	}
	if r.DownAlarm != nil {
		lines = append(lines, r.DownAlarm.Text())
	}
	if r.StatusPage != nil {
		lines = append(lines, r.StatusPage.Text())
	}
	if r.SignInLinkURL != "" {
		lines = append(lines, "Sign-in link: "+r.SignInLinkURL, "Sign-in link valid until (UTC): "+r.SignInLinkValidUntil, "SSH forward command: "+r.SSHForwardCommand)
	}
	for _, incident := range r.Incidents {
		lines = append(lines, incident.Text(), "")
	}
	if r.Incident != nil {
		lines = append(lines, r.Incident.Text())
	}
	if r.TestSOS != nil {
		lines = append(lines, r.TestSOS.Text())
	}
	for _, alert := range r.Alerts {
		lines = append(lines, alert.Text(), "")
	}
	for _, session := range r.TriageSessions {
		lines = append(lines, session.Text(), "")
	}
	if r.TriageSession != nil {
		lines = append(lines, r.TriageSession.Text())
	}
	for _, request := range r.ApprovalRequests {
		lines = append(lines, request.Line())
	}
	if r.ApprovalRequest != nil {
		lines = append(lines, r.ApprovalRequest.Text())
	}
	if r.CommitCheck != nil {
		lines = append(lines, r.CommitCheck.text()...)
	}
	for _, review := range r.CommitReviews {
		lines = append(lines, commitReviewText(review)...)
		lines = append(lines, "")
	}
	if len(r.CommitsTheTriageAgentApproved) > 0 {
		lines = append(lines, "Commits the triage agent approved, newest first:", "")
	}
	for _, review := range r.CommitsTheTriageAgentApproved {
		lines = append(lines, commitReviewText(review)...)
		lines = append(lines, "")
	}
	if r.CommitReview != nil {
		lines = append(lines, commitReviewText(*r.CommitReview)...)
	}
	lines = append(lines, r.Message)
	return strings.Join(lines, "\n")
}
func displaySignInEmailAddress(signInEmailAddress string) string {
	if signInEmailAddress == "" {
		return "no one scheduled"
	}
	return signInEmailAddress
}

func commandBackup(option, signInEmailAddress string) string {
	if option == "everyone_available" && signInEmailAddress == "" {
		return "everyone available"
	}
	return displaySignInEmailAddress(signInEmailAddress)
}
func commandTime(at time.Time) string {
	if at.IsZero() {
		return "not recorded"
	}
	return at.UTC().Format(time.RFC3339)
}
