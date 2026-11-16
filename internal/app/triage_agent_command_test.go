package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"mmhotline/internal/triageagent"
)

func TestTriageAgentCommandsShareOperations(t *testing.T) {
	a := foundationApp(t)
	runs := triageRunFixture(t, a)
	command := func(action string, status int) CommandResult {
		t.Helper()
		result, raw := commandRequest(t, a, CommandRequest{Group: "triage-agent", Action: action}, status)
		if strings.Contains(raw, "secret-credential") || strings.Contains(result.Text(), "secret-credential") {
			t.Fatal("credential exposed")
		}
		return result
	}
	result := command("show", 200)
	if result.TriageAgent == nil || result.TriageAgent.InternetAccess != "listed_hosts_only" || !strings.Contains(result.Text(), "Triage agent: not set up") {
		t.Fatal(result)
	}
	id, err := a.StartTriageAgentSetupTest(context.Background(), Principal{UnixUser: "fixture"}, "claude_code", "api_key", "secret-credential")
	if err != nil {
		t.Fatal(err)
	}
	run := <-runs
	sendTriageEvent(t, run, triageagent.Event{Event: "step", Step: &triageagent.Step{Kind: "message", Text: "secret-credential"}})
	sendTriageEvent(t, run, triageagent.Event{Event: "finished", Answer: triageAgentTestSentence + " secret-credential"})
	waitTriageRun(t, a, id)
	run.connection.Close()
	result = command("show", 200)
	if result.TriageAgent.Setup == nil || !strings.Contains(result.Text(), "passed") || !strings.Contains(result.Text(), "model test-model, reasoning_effort high") {
		t.Fatal("passing result missing")
	}
	// The settings file sets the internet access and the listed hosts; show
	// prints them.
	settings := Principal{SettingsFile: "/etc/mmhotline/mmhotline_settings.yml"}
	if err := a.SetTriageAgentInternetAccess(context.Background(), settings, "invalid"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an invalid internet access: %v", err)
	}
	if err := a.SetTriageAgentInternetAccess(context.Background(), settings, "any_site"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetTriageAgentListedHosts(context.Background(), settings, []SettingsListedHost{{Host: "git.example", Port: 22}}); err != nil {
		t.Fatal(err)
	}
	result = command("show", 200)
	if result.TriageAgent.InternetAccess != "any_site" || !strings.Contains(result.Text(), "git.example:22") {
		t.Fatal(result)
	}
	if err := a.SetTriageAgentListedHosts(context.Background(), settings, nil); err != nil {
		t.Fatal(err)
	}
	if result = command("show", 200); strings.Contains(result.Text(), "git.example:22") {
		t.Fatal(result)
	}
	// The settings file sets the agent up; these commands are gone.
	for _, action := range []string{"set-up", "remove-setup", "unknown"} {
		command(action, 404)
	}
}
