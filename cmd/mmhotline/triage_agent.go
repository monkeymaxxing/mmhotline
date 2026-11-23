package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"mmhotline/internal/app"
	"mmhotline/internal/triageagent"
)

// waitSignInCheck waits for the sign-in check that check-sign-in of group,
// triage-agent or review-agent, started, which asks the coding agent for
// the setup test's sentence.
func waitSignInCheck(socket, unixUser, group string, result app.CommandResult) (app.CommandResult, error) {
	agent := strings.ReplaceAll(group, "-", " ")
	test := agent + " sign-in check"
	latest := func(result app.CommandResult) *app.TriageAgentRun {
		if group == "review-agent" && result.ReviewAgent != nil {
			return result.ReviewAgent.LatestSignInCheck
		}
		if group == "triage-agent" && result.TriageAgent != nil {
			return result.TriageAgent.LatestSignInCheck
		}
		return nil
	}
	runID := result.TriageAgentRunID
	if group == "review-agent" {
		runID = result.ReviewAgentRunID
	}
	if runID == "" {
		return result, commandError{fmt.Errorf("the server did not return a %s run ID", agent)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for {
		if run := latest(result); run != nil {
			if run.ID != runID {
				return result, commandError{fmt.Errorf("a newer %s replaced the result of the sign-in check this command started; run mmhotline %s show to see the newer result", test, group)}
			}
			if run.State != "running" {
				if run.Passed() {
					return result, nil
				}
				reason := run.FailureReason
				if reason == "" {
					reason = "the " + test + " did not pass"
				}
				return result, commandError{errors.New(reason)}
			}
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, commandError{fmt.Errorf("the %s did not finish within 5 minutes; run mmhotline %s show", test, group)}
		case <-timer.C:
		}
		next, err := sendCommandContext(ctx, socket, app.CommandRequest{Group: group, Action: "show", UnixUser: unixUser})
		if err != nil {
			return result, err
		}
		result = next
	}
}

func runTriageAgentInsideSandbox(args []string) error {
	flags := flag.NewFlagSet("triage-agent run-inside-sandbox", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: mmhotline triage-agent run-inside-sandbox --proxy-socket PATH\nSystemd starts run-inside-sandbox for each triage agent run. People do not run run-inside-sandbox directly.")
		flags.PrintDefaults()
	}
	socket := flags.String("proxy-socket", "", "required MMHotline logging proxy Unix socket")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return commandError{err}
	}
	if *socket == "" {
		return commandError{errors.New("--proxy-socket is required")}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("unexpected positional arguments")}
	}
	// The agent's SSH settings run this same program as their ProxyCommand;
	// on Linux the path comes from /proc/self/exe, which the run's unit starts
	// as /usr/local/bin/mmhotline.
	program, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the mmhotline program's own path: %w", err)
	}
	// The agent's home is its fixed path, not $HOME: a run clears the
	// agent's home, so a person who ran this command by hand must not lose
	// their own.
	return triageagent.RunInsideSandbox(context.Background(), os.Stdin, os.Stdout, triageagent.SandboxSettings{ProxySocketPath: *socket, Home: triageAgentHome, SearchPath: "/usr/local/bin:/usr/bin:/bin", MMHotlineProgram: program})
}
