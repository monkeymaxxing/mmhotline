package main

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"mmhotline/internal/app"
)

// The flags of the read-only commands, which show what happened: incidents,
// alerts, triage sessions, and approval requests. They change nothing,
// except incidents claim and resolve, which act like the incident page's
// buttons, and incidents send-test-sos.

const incidentsClaimHelp = `usage: sudo mmhotline incidents claim --incident-id ID --sign-in-email-address ADDRESS [--note TEXT] [--command-socket PATH] [--json]

Claims an SOS incident for a member, as the incident page's Claim does.
Claiming stops MMHotline contacting people about this incident.

`

func incidentsFlags(flags *flag.FlagSet, action string, in *app.IncidentsCommandRequest, waitForClaimMinutes *int) func() error {
	if action == "send-test-sos" {
		flags.StringVar(&in.SignInEmailAddress, "sign-in-email-address", "", "send the test SOS only to this person, now, whether or not they are on call; MMHotline contacts nobody else. Without it the test goes to the person on call, then escalates as an SOS does")
		flags.IntVar(waitForClaimMinutes, "wait-for-claim-minutes", defaultWaitForClaimMinutes, "how many minutes to wait for someone to acknowledge the test SOS; at least 1")
		return func() error {
			if *waitForClaimMinutes < 1 {
				return errors.New("--wait-for-claim-minutes must be at least 1")
			}
			return nil
		}
	}
	if action == "list" {
		timeFlag(flags, "opened-since", "list the incidents opened at or after `TIME`, resolved or not", &in.OpenedSince)
		timeFlag(flags, "opened-before", "list the incidents opened before `TIME`, resolved or not", &in.OpenedBefore)
		return func() error { return nil }
	}
	flags.StringVar(&in.IncidentID, "incident-id", "", "SOS incident's ID")
	if action == "claim" || action == "resolve" {
		flags.StringVar(&in.Note, "note", "", "one line of plain text for the incident's history, such as what you found")
	}
	if action == "claim" {
		flags.Usage = func() {
			fmt.Fprint(flags.Output(), incidentsClaimHelp)
			flags.PrintDefaults()
		}
		// A Unix user is not linked to a member, so the claim names one.
		flags.StringVar(&in.SignInEmailAddress, "sign-in-email-address", "", "sign-in email address of the member who claims the incident; the history also names the Unix user who ran the command")
		return func() error {
			if in.SignInEmailAddress == "" {
				return errors.New("--sign-in-email-address is required: it names the member who claims the incident")
			}
			return requiredFlag("incident-id", &in.IncidentID)()
		}
	}
	return requiredFlag("incident-id", &in.IncidentID)
}

func alertsFlags(flags *flag.FlagSet, in *app.AlertsCommandRequest) func() error {
	flags.StringVar(&in.Severity, "severity", "", "info, warn, sos, resolved-info, resolved-warn, or resolved-sos")
	flags.StringVar(&in.AlertSourceID, "alert-source-id", "", "alert source's ID; system for MMHotline's own alerts, manual for SOS incidents a person created; mmhotline_test_sos for the test SOS of incidents send-test-sos")
	flags.StringVar(&in.IncidentID, "incident-id", "", "SOS incident's ID")
	timeFlag(flags, "received-since", "list the alerts received at or after `TIME`", &in.ReceivedSince)
	timeFlag(flags, "received-before", "list the alerts received before `TIME`", &in.ReceivedBefore)
	flags.StringVar(&in.OlderThanAlertID, "older-than-alert-id", "", "ID of the oldest alert in the previous list; list older alerts to continue after the previous 100")
	return func() error { return nil }
}

func triageSessionsFlags(flags *flag.FlagSet, action string, in *app.TriageSessionsCommandRequest) func() error {
	if action == "list" {
		return func() error { return nil }
	}
	flags.StringVar(&in.TriageSessionID, "triage-session-id", "", "triage session's ID")
	return requiredFlag("triage-session-id", &in.TriageSessionID)
}

func approvalRequestsFlags(flags *flag.FlagSet, action string, in *app.ApprovalRequestsCommandRequest) func() error {
	if action == "list" {
		return func() error { return nil }
	}
	flags.StringVar(&in.ApprovalRequestID, "approval-request-id", "", "approval request's ID")
	return requiredFlag("approval-request-id", &in.ApprovalRequestID)
}

// commitReviewsFlags are the flags of commit-reviews: check and
// mark-reviewed act like the Approvals page, list and show only read.
func commitReviewsFlags(flags *flag.FlagSet, action string, in *app.CommitReviewsCommandRequest) func() error {
	if action == "list" {
		return func() error { return nil }
	}
	usage := map[string]string{
		"check":         "full ID of the commit to check, such as the commit util01 is about to apply; MMHotline fetches origin first and checks every commit since the last checked commit through it",
		"trust-through": "full ID of a commit of origin's branch; MMHotline trusts it and every commit before it, such as after a person's first run on a new or rebuilt server",
		"mark-reviewed": "full ID of the commit that waits for review",
		"show":          "full ID of a commit that waits or waited for review",
	}[action]
	flags.StringVar(&in.CommitID, "commit-id", "", usage)
	return requiredFlag("commit-id", &in.CommitID)
}

// timeFlag reads an RFC 3339 time, such as 2026-10-28T09:00:00Z, into target.
// The usage names its value `TIME`, which help shows after the flag.
func timeFlag(flags *flag.FlagSet, name, usage string, target *time.Time) {
	flags.Func(name, usage+"; TIME is an RFC 3339 time, such as 2026-10-28T09:00:00Z", func(value string) error {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return errors.New("use an RFC 3339 time, such as 2026-10-28T09:00:00Z")
		}
		*target = parsed.UTC()
		return nil
	})
}

func requiredFlag(name string, value *string) func() error {
	return func() error {
		if *value == "" {
			return errors.New("--" + name + " is required")
		}
		return nil
	}
}
