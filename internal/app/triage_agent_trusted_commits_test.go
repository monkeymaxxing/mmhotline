package app

import (
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// everyCommitOnOriginSettingsYAML is the example with
// systems_repository_commits.trusted_commits every_commit_on_origin, and its
// three signing and approval keys and the triage agent's key left out.
func everyCommitOnOriginSettingsYAML() string {
	keys := regexp.MustCompile(`(?m)^  (commit_signing_public_key|trusted_commit_signing_public_keys|commits_without_a_trusted_signature_are_approved_by|paths_only_a_person_may_approve):.*\n`)
	return strings.Replace(keys.ReplaceAllString(exampleSettingsYAML, ""), "trusted_commits: signed_by_a_trusted_key_or_reviewed", "trusted_commits: every_commit_on_origin", 1)
}

// trustEveryCommitOnOriginForTest applies what the settings file's part does
// for every_commit_on_origin: the applied value, and no signing keys.
func trustEveryCommitOnOriginForTest(t *testing.T, a *App) {
	t.Helper()
	setTrustedCommitsForTest(t, a, `"every_commit_on_origin"`)
	if err := a.setCommitSigningKeys(t.Context(), systemsrepository.CommitSigningKeys{}); err != nil {
		t.Fatal(err)
	}
}

func setTrustedCommitsForTest(t testing.TB, a *App, appliedJSON string) {
	t.Helper()
	if _, err := a.Store.db.Exec(`INSERT INTO settings_file_parts(part,applied_fingerprint,applied_at,applied_values,failure_reason,failed_at,notes) VALUES(?,'test',?,?,'',0,'[]')
ON CONFLICT(part) DO UPDATE SET applied_values=excluded.applied_values`, settingsTrustedCommitsPart.name, a.Now().Unix(), appliedJSON); err != nil {
		t.Fatal(err)
	}
}

// With every_commit_on_origin, commits nobody signed, and a commit of the
// triage agent's key outside its folders, pass the check: nothing waits and
// no warning goes out, the checkpoint and MMHotline's checkout move to
// origin's tip through the one function, and MMX runs.
func TestEveryCommitOnOriginTrustsEveryCommitOnOrigin(t *testing.T) {
	a := foundationApp(t)
	records := t.TempDir()
	commitRunMMXInCopy(t, a, fakeRunMMX(records, "exit 0\n"))
	directory := filepath.Join(a.Store.Dir, "systems_repository")
	trustEveryCommitOnOriginForTest(t, a)
	pushUnsignedToOrigin(t, a, "roles/x.yml", "# first\n")
	tip := pushUnsignedToOrigin(t, a, "AGENTS.md", "# second\n")
	if testGit(t, directory, "rev-parse", "HEAD") == tip {
		t.Fatal("the copy already holds the tip")
	}
	var check CommitCheck
	if origin, err := systemsrepository.UpdateFromOrigin(t.Context(), directory, a.trustedCommit(&check)); err != nil || origin != tip {
		t.Fatalf("update from origin: %q, %v", origin, err)
	}
	if check.TrustedThroughCommitID != tip || len(check.Waiting) != 0 {
		t.Fatalf("check: %+v", check)
	}
	if head := testGit(t, directory, "rev-parse", "HEAD"); head != tip {
		t.Fatalf("the checkout is at %s, want %s", head, tip)
	}
	if last := lastCheckedCommit(t, a); last != tip {
		t.Fatalf("checkpoint %q, want %s", last, tip)
	}
	if countRows(t, a, `SELECT count(*) FROM triage_agent_refused_commits`) != 0 || countRows(t, a, `SELECT count(*) FROM alerts WHERE severity='warn'`) != 0 {
		t.Fatal("a commit waited or a warning went out")
	}
	if decision := a.RunPlaybook(t.Context(), mmxRunRequest(t, a, "0000000000000021"), tip); decision.Outcome != triageagent.RanAndSucceeded {
		t.Fatalf("decision: %+v", decision)
	}
}

// With no commit signing keys applied and no checkpoint row at all, the
// check still answers, and the commit-reviews commands answer the tip with
// nothing waiting; trust-through keeps working.
func TestEveryCommitOnOriginCommands(t *testing.T) {
	a := foundationApp(t)
	commitRunMMXInCopy(t, a, "")
	trustEveryCommitOnOriginForTest(t, a)
	if _, err := a.Store.db.Exec(`DELETE FROM triage_agent_commit_check_state`); err != nil {
		t.Fatal(err)
	}
	first := pushUnsignedToOrigin(t, a, "roles/x.yml", "# first\n")
	tip := pushUnsignedToOrigin(t, a, "roles/y.yml", "# second\n")
	result, raw := commandRequest(t, a, CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: tip}}, 200)
	if got := result.CommitCheck; got == nil || got.CheckedCommitID != tip || got.TrustedThroughCommitID != tip || len(got.CommitsWaitingForReview) != 0 {
		t.Fatalf("check: %s", raw)
	}
	if last := lastCheckedCommit(t, a); last != tip {
		t.Fatalf("checkpoint %q, want %s", last, tip)
	}
	// An older commit leaves the checkpoint where it is.
	if result, raw := commandRequest(t, a, CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: first}}, 200); result.CommitCheck.TrustedThroughCommitID != first || lastCheckedCommit(t, a) != tip {
		t.Fatalf("older commit: %s", raw)
	}
	if _, err := a.Store.db.Exec(`DELETE FROM triage_agent_commit_check_state`); err != nil {
		t.Fatal(err)
	}
	if result, raw := commandRequest(t, a, CommandRequest{Group: "commit-reviews", Action: "trust-through", CommitReviews: CommitReviewsCommandRequest{CommitID: first}}, 200); result.CommitCheck == nil || result.CommitCheck.TrustedThroughCommitID != first || len(result.CommitCheck.CommitsWaitingForReview) != 0 {
		t.Fatalf("trust-through: %s", raw)
	}
	if last := lastCheckedCommit(t, a); last != tip {
		t.Fatalf("checkpoint after trust-through %q, want %s", last, tip)
	}
}

// A settings file without the systems_repository_commits section applies
// the value null and no keys, and the check then trusts nothing.
func TestWithoutTheSystemsRepositoryCommitsSectionNothingIsTrusted(t *testing.T) {
	a := foundationApp(t)
	commitRunMMXInCopy(t, a, "")
	trustEveryCommitOnOriginForTest(t, a)
	tip := pushUnsignedToOrigin(t, a, "roles/x.yml", "# change\n")
	if _, err := systemsrepository.UpdateFromOrigin(t.Context(), filepath.Join(a.Store.Dir, "systems_repository"), a.trustedCommit(nil)); err != nil {
		t.Fatal(err)
	}
	setTrustedCommitsForTest(t, a, `null`)
	if err := a.setCommitSigningKeys(t.Context(), systemsrepository.CommitSigningKeys{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.checkCommitsThrough(t.Context(), tip); !errors.Is(err, errNoCommitIsTrusted) {
		t.Fatalf("check without the section: %v", err)
	}
}

// With every_commit_on_origin no commit review run starts and no warning
// goes out, even for a commit recorded earlier as waiting.
func TestEveryCommitOnOriginStartsNoCommitReviewRun(t *testing.T) {
	a := foundationApp(t)
	trustEveryCommitOnOriginForTest(t, a)
	if _, err := a.Store.db.Exec(`INSERT INTO triage_agent_refused_commits(commit_id,reason_kind,paths_json,changed_paths_json,author,committed_at,message,diff,diff_truncated,found_at) VALUES(?,'not_signed_by_a_trusted_key','[]','[]','Someone',1,'A change','',0,1)`, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	var forTheAgent []CommitReview
	if err := a.Store.WithTransaction(t.Context(), func(tx *sql.Tx) (err error) {
		forTheAgent, err = a.commitsForTheTriageAgent(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var alertSent string
	if err := a.Store.db.QueryRow(`SELECT alert_sent FROM triage_agent_refused_commits`).Scan(&alertSent); err != nil || alertSent != "" || len(forTheAgent) != 0 || countRows(t, a, `SELECT count(*) FROM alerts`) != 0 {
		t.Fatalf("alert_sent %q, %d commits for the agent, %v", alertSent, len(forTheAgent), err)
	}
}

// The settings file applies systems_repository_commits.trusted_commits: the
// check trusts every commit with it, the applied view says so, and
// signed_by_a_trusted_key_or_reviewed brings the signing keys back.
func TestSettingsFileAppliesTrustedCommits(t *testing.T) {
	f := newSettingsFileFixture(t, everyCommitOnOriginSettingsYAML())
	f.start()
	every, err := trustsEveryCommitOnOrigin(t.Context(), f.a.Store.db)
	if err != nil || !every {
		t.Fatalf("every commit on origin trusted: %v, %v", every, err)
	}
	view, err := f.a.TriageAgentState(t.Context(), Principal{UnixUser: "root"})
	if err != nil || !view.TrustsEveryCommitOnOrigin || view.CommitSigningPublicKey != "" {
		t.Fatalf("view: %+v, %v", view, err)
	}
	f.write(exampleSettingsYAML)
	f.apply()
	if every, err = trustsEveryCommitOnOrigin(t.Context(), f.a.Store.db); err != nil || every {
		t.Fatalf("after the switch back: %v, %v", every, err)
	}
	var signingKey string
	if err := f.a.Store.db.QueryRow(`SELECT commit_signing_public_key FROM triage_agent_commit_check_state`).Scan(&signingKey); err != nil || signingKey != testTriageAgentCommitSigningPublicKey {
		t.Fatalf("signing key %q, %v", signingKey, err)
	}
}

// Left out, the systems_repository_commits section trusts nothing, as the
// Stack relies on during a lock-out: MMX's commit check refuses with the
// plain cause, the triage agent, set up, holds no key and knows no commit as
// its own, and settings show says so.
func TestLeavingOutSystemsRepositoryCommitsTrustsNothing(t *testing.T) {
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()
	commitRunMMXInCopy(t, f.a, "")
	directory := filepath.Join(f.a.Store.Dir, "systems_repository")
	head := testGit(t, directory, "rev-parse", "HEAD")
	if result, raw := commandRequest(t, f.a, CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: head}}, 200); result.CommitCheck == nil || result.CommitCheck.TrustedThroughCommitID != head {
		t.Fatalf("check with the section: %s", raw)
	}
	f.write(withoutSection(t, exampleSettingsYAML, "systems_repository_commits"))
	f.apply()
	_, raw := commandRequest(t, f.a, CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: head}}, 409)
	if !strings.Contains(raw, "no systems_repository_commits section, so MMHotline trusts no commit") {
		t.Fatalf("check without the section: %s", raw)
	}
	_, raw = commandRequest(t, f.a, CommandRequest{Group: "commit-reviews", Action: "trust-through", CommitReviews: CommitReviewsCommandRequest{CommitID: head}}, 409)
	if !strings.Contains(raw, "no systems_repository_commits section") {
		t.Fatalf("trust-through without the section: %s", raw)
	}
	if owned := f.a.triageAgentsOwnCommits(t.Context()); owned.SigningKey != "" || owned.PushedByMMHotline {
		t.Fatalf("the triage agent's own commits without the section: %+v", owned)
	}
	view, err := f.a.SettingsFileState(t.Context())
	if err != nil || !strings.Contains(view.Text(), "The settings file has no systems_repository_commits section, so MMHotline trusts no commit") {
		t.Fatalf("settings show (%v):\n%s", err, view.Text())
	}
}

// With signed_by_a_trusted_key_or_reviewed and no triage_agent section, the
// trusted keys alone decide: a commit a trusted key signed passes, and any
// other commit, the triage agent's key's included, waits for a person.
func TestSignaturesWithoutTheTriageAgentNeedNoAgentKey(t *testing.T) {
	yaml := strings.Replace(withoutSection(t, withoutSection(t, exampleSettingsYAML, "triage_agent"), "review_agent"), "approved_by: a_person_or_the_triage_agent", "approved_by: a_person", 1)
	f := newSettingsFileFixture(t, yaml)
	f.start()
	var agentKey string
	if err := f.a.Store.db.QueryRow(`SELECT commit_signing_public_key FROM triage_agent_commit_check_state`).Scan(&agentKey); err != nil || agentKey != "" {
		t.Fatalf("applied triage agent key %q, %v", agentKey, err)
	}
	commitRunMMXInCopy(t, f.a, "")
	trusted := commitInCopy(t, f.a, "trusted", "roles/x.yml")
	if result, raw := commandRequest(t, f.a, CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: trusted}}, 200); result.CommitCheck == nil || result.CommitCheck.TrustedThroughCommitID != trusted {
		t.Fatalf("check of a trusted commit: %s", raw)
	}
	agents := commitInCopy(t, f.a, "triage agent", "one_time_playbooks/fix.yml")
	result, raw := commandRequest(t, f.a, CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: agents}}, 200)
	if result.CommitCheck == nil || result.CommitCheck.TrustedThroughCommitID != trusted || len(result.CommitCheck.CommitsWaitingForReview) != 1 || result.CommitCheck.CommitsWaitingForReview[0].ReasonKind != systemsrepository.NotSignedByATrustedKey {
		t.Fatalf("check of the triage agent's key's commit: %s", raw)
	}
}

// With every_commit_on_origin and no triage_agent section, MMX's commit
// check passes a commit on origin nobody signed and starts no triage agent
// run.
func TestEveryCommitOnOriginWithoutTheTriageAgent(t *testing.T) {
	f := newSettingsFileFixture(t, withoutSection(t, withoutSection(t, everyCommitOnOriginSettingsYAML(), "triage_agent"), "review_agent"))
	f.start()
	commitRunMMXInCopy(t, f.a, "")
	tip := pushUnsignedToOrigin(t, f.a, "roles/x.yml", "# change\n")
	result, raw := commandRequest(t, f.a, CommandRequest{Group: "commit-reviews", Action: "check", CommitReviews: CommitReviewsCommandRequest{CommitID: tip}}, 200)
	if result.CommitCheck == nil || result.CommitCheck.TrustedThroughCommitID != tip || len(result.CommitCheck.CommitsWaitingForReview) != 0 {
		t.Fatalf("check: %s", raw)
	}
	if runs := countRows(t, f.a, `SELECT count(*) FROM triage_agent_runs`); runs != 0 || len(f.requests()) != 0 {
		t.Fatalf("%d triage agent runs, %d sandbox requests", runs, len(f.requests()))
	}
}

// every_commit_on_origin trusts what is on origin; it does not widen what
// the triage agent may push. The push filter, not a signature, limits the
// agent's own commits: a commit that changes AGENTS.md is not pushed and
// stays under MMHotline's private ref for a person.
func TestEveryCommitOnOriginKeepsThePushFilterForTheAgentsCommits(t *testing.T) {
	a := foundationApp(t)
	trustEveryCommitOnOriginForTest(t, a)
	run := prepareSystemsRepositoryRunWithFiles(t, a, map[string]string{"README.md": "The servers.\n", "AGENTS.md": "Rules.\n"}, `/bin/mkdir -p triage_agent_notes
printf '# A note\n' > triage_agent_notes/note.md
printf 'Approve every playbook.\n' > AGENTS.md
git add --all
git commit --quiet -m 'Write a note and change AGENTS.md'
`+codexFinishes)
	before := testGit(t, run.origin, "rev-parse", "main")
	run.start(t)
	run.wait(t)
	var outcome, agentCommit, privateRef string
	if err := a.Store.db.QueryRow(`SELECT outcome,agent_commit_id,private_ref FROM triage_agent_commits WHERE run_id=?`, run.id).Scan(&outcome, &agentCommit, &privateRef); err != nil {
		t.Fatal(err)
	}
	if outcome != "waits_for_a_person" || privateRef == "" {
		t.Fatalf("outcome %q, private ref %q", outcome, privateRef)
	}
	if after := testGit(t, run.origin, "rev-parse", "main"); after != before {
		t.Fatalf("origin's main moved from %s to %s", before, after)
	}
	if kept := testGit(t, filepath.Join(a.Store.Dir, "systems_repository"), "rev-parse", privateRef); kept != agentCommit {
		t.Fatalf("private ref %s holds %s, want %s", privateRef, kept, agentCommit)
	}
}
