package systemsrepository

import (
	"fmt"
	"strings"
	"testing"
)

// The agent may change, delete, or rename only the one-time playbooks it
// wrote and changed last: a person who read a playbook may later run it by
// hand with mmx run, and must run what the person read. Each case runs with
// both ways of telling the agent's commits: its signature, and MMHotline's
// record of the commits it pushed when no signature is read.
func TestOnlyTheAgentsOwnOneTimePlaybooksChangeWithoutAPerson(t *testing.T) {
	const playbook = "one_time_playbooks/2026-11-01_rotate_logs_on_app001.yml"
	var lines []string
	for line := 1; line <= 10; line++ {
		lines = append(lines, fmt.Sprintf("# line %02d", line))
	}
	original := strings.Join(lines, "\n") + "\n"
	withLineChanged := func(text string, line int, by string) string {
		old := fmt.Sprintf("# line %02d", line)
		return strings.Replace(text, old, old+", changed by "+by, 1)
	}
	current := func(t *testing.T, directory string) string {
		t.Helper()
		return readTestFile(t, directory+"/"+playbook)
	}
	agentEdits := func(t *testing.T, r repositories) {
		t.Helper()
		writeFile(t, r.agent, playbook, withLineChanged(current(t, r.agent), 10, "the agent"), 0o644)
		commitAll(t, r.agent, "Edit the playbook")
	}
	personEdits := func(t *testing.T, r repositories) {
		t.Helper()
		git(t, r.person, "pull", "--quiet", "--ff-only")
		r.personPushes(t, playbook, withLineChanged(current(t, r.person), 1, "a person"))
	}
	// agentRun runs the agent once: during is what happens on origin while
	// the agent works, and work is the agent's own work.
	agentRun := func(t *testing.T, r repositories, during, work func()) (ReceivedAgentCommits, string) {
		t.Helper()
		base := r.startRun(t)
		if during != nil {
			during()
		}
		work()
		before := r.originMain(t)
		return r.endRun(t, base), before
	}
	agentWrites := func(t *testing.T, r repositories) {
		t.Helper()
		result, _ := agentRun(t, r, nil, func() {
			writeFile(t, r.agent, playbook, original, 0o644)
			commitAll(t, r.agent, "Write the playbook")
		})
		if result.Outcome != Pushed {
			t.Fatalf("the agent's new playbook: %+v", result)
		}
	}

	for _, test := range []struct {
		name string
		// waiting is the path that waits for a person, or "" when the
		// commits are pushed.
		waiting string
		run     func(t *testing.T, r repositories) (ReceivedAgentCommits, string)
	}{
		{"a person's playbook, edited", playbook, func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			r.personPushes(t, playbook, original)
			return agentRun(t, r, nil, func() { agentEdits(t, r) })
		}},
		{"a person's playbook, deleted", playbook, func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			r.personPushes(t, playbook, original)
			return agentRun(t, r, nil, func() {
				git(t, r.agent, "rm", "--quiet", playbook)
				git(t, r.agent, "commit", "--quiet", "-m", "Delete the playbook")
			})
		}},
		{"a person's playbook, renamed", playbook, func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			r.personPushes(t, playbook, original)
			return agentRun(t, r, nil, func() {
				git(t, r.agent, "mv", playbook, "one_time_playbooks/2026-11-01_renamed.yml")
				git(t, r.agent, "commit", "--quiet", "-m", "Rename the playbook")
			})
		}},
		{"the agent's playbook that a person changed last", playbook, func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			agentWrites(t, r)
			personEdits(t, r)
			return agentRun(t, r, nil, func() { agentEdits(t, r) })
		}},
		{"the agent's playbook, edited again", "", func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			agentWrites(t, r)
			return agentRun(t, r, nil, func() { agentEdits(t, r) })
		}},
		{"a new playbook, written and edited in one run", "", func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			return agentRun(t, r, nil, func() {
				writeFile(t, r.agent, playbook, original, 0o644)
				commitAll(t, r.agent, "Write the playbook")
				agentEdits(t, r)
			})
		}},
		// The merge with origin would carry the agent's edit over the
		// person's newer one.
		{"the agent's playbook that a person changes during the run", playbook, func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			agentWrites(t, r)
			return agentRun(t, r, func() { personEdits(t, r) }, func() { agentEdits(t, r) })
		}},
		// The agent fetches a person's newer commit from origin and builds
		// on it: the person's commit, in the agent's bundle, is not the
		// agent's own.
		{"a person's commit from origin in the agent's bundle", playbook, func(t *testing.T, r repositories) (ReceivedAgentCommits, string) {
			agentWrites(t, r)
			return agentRun(t, r, func() { personEdits(t, r) }, func() {
				git(t, r.agent, "fetch", "--quiet", r.origin, "main")
				git(t, r.agent, "merge", "--quiet", "--ff-only", "FETCH_HEAD")
				agentEdits(t, r)
			})
		}},
	} {
		for _, mode := range []string{"signed_by_a_trusted_key_or_reviewed", "every_commit_on_origin"} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				r := setUpRepositories(t)
				if mode == "signed_by_a_trusted_key_or_reviewed" {
					// The agent signs from its ~/.gitconfig; the person and
					// MMHotline's merges sign with keys the team trusts.
					keys := newCommitKeys(t, r.root)
					git(t, r.root, "config", "--global", "gpg.format", "ssh")
					git(t, r.root, "config", "--global", "user.signingkey", keys.triageAgent)
					git(t, r.root, "config", "--global", "commit.gpgsign", "true")
					signWith(t, r.person, keys.trusted)
					signWith(t, r.mmhotline, keys.trusted)
					r.owned = TriageAgentsOwnCommits{SigningKey: keys.TriageAgent}
				}
				result, before := test.run(t, r)
				if test.waiting == "" {
					if result.Outcome != Pushed || r.originMain(t) != result.PushedCommitID {
						t.Fatalf("result: %+v", result)
					}
					return
				}
				if result.Outcome != WaitsForAPerson || !strings.Contains(result.Reason, test.waiting+" "+othersPlaybookReason) {
					t.Fatalf("result: %+v", result)
				}
				if main := r.originMain(t); main != before {
					t.Fatalf("origin's main moved to %s", main)
				}
			})
		}
	}
}
