package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mmhotline/internal/systemsrepository"
)

// The check of the systems repository's commits, the reviews of the commits
// that wait, and the run's base:
// docs/changing-mmhotline/design/triage-agent.md#checking-commits-before-mmx-and-triage-agent-runs.

// triage_agent_commit_check_state holds the applied signing keys and the
// checked-through commit: every commit through it passed the check or a
// person reviewed it.
//
// triage_agent_refused_commits keeps each commit that waits or waited, with
// the facts the review page shows and who reviewed it (reviewer_kind): a
// member on the page, a Unix user through the command socket, directly or by
// marking every commit through a later commit trusted, or the triage agent
// in its commit review run. triage_agent_review_run_id is the commit review
// run the commit was given to. alert_sent says which one alert people got
// about the commit, so a restart never sends a second: MMHotline's own
// warning, the triage agent's approval, or the triage agent's report that a
// person must review it; empty while the triage agent reviews it.
//
// triage_agent_commit_trust_throughs keeps each commit-reviews trust-through:
// who on which server vouched for every commit through which commit, when.
const triageAgentCommitCheckSchema = `
CREATE TABLE triage_agent_commit_trust_throughs (
 id INTEGER PRIMARY KEY,
 commit_id TEXT NOT NULL,
 unix_user TEXT NOT NULL, server_name TEXT NOT NULL,
 trusted_at INTEGER NOT NULL
);
CREATE TABLE triage_agent_commit_check_state (
 id INTEGER PRIMARY KEY CHECK(id=1),
 commit_signing_public_key TEXT NOT NULL,
 trusted_commit_signing_public_keys_json TEXT NOT NULL,
 last_checked_commit_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE triage_agent_refused_commits (
 commit_id TEXT PRIMARY KEY,
 reason_kind TEXT NOT NULL CHECK(reason_kind IN ('not_signed_by_a_trusted_key','triage_agent_change_needs_review')),
 paths_json TEXT NOT NULL, changed_paths_json TEXT NOT NULL,
 author TEXT NOT NULL, committed_at INTEGER NOT NULL, message TEXT NOT NULL,
 diff TEXT NOT NULL, diff_truncated INTEGER NOT NULL,
 found_at INTEGER NOT NULL,
 reviewed_at INTEGER,
 reviewed_by_member_id TEXT REFERENCES members(id),
 reviewed_by_unix_user TEXT NOT NULL DEFAULT '',
 reviewed_through_trust_through_id INTEGER REFERENCES triage_agent_commit_trust_throughs(id),
 reviewer_kind TEXT NOT NULL DEFAULT '' CHECK(reviewer_kind IN ('','member','unix_user','trust_through','triage_agent')),
 triage_agent_review_run_id TEXT REFERENCES triage_agent_runs(id),
 alert_sent TEXT NOT NULL DEFAULT '' CHECK(alert_sent IN ('','mmhotline_warning','triage_agent_approval','triage_agent_report'))
);
`

// The values of systems_repository_commits.trusted_commits.
const (
	commitsTrustedWhenSignedByATrustedKeyOrReviewed = "signed_by_a_trusted_key_or_reviewed"
	commitsTrustedEveryCommitOnOrigin               = "every_commit_on_origin"
)

// settingsTrustedCommitsPart keeps systems_repository_commits.trusted_commits
// as an applied value, which appliedTrustedCommits reads: only the applied
// value counts. Without the systems_repository_commits section the value is
// null, and nothing is trusted.
var settingsTrustedCommitsPart = settingsFilePart{
	name: "systems_repository_commits.trusted_commits",
	value: fileValue(func(s Settings) any {
		if s.SystemsRepositoryCommits == nil {
			return nil
		}
		return s.SystemsRepositoryCommits.TrustedCommits
	}),
	apply:       func(context.Context, *App, Settings) error { return nil },
	keepsValues: true,
}

// appliedTrustedCommits is the applied systems_repository_commits.trusted_commits:
// signed_by_a_trusted_key_or_reviewed, every_commit_on_origin, or "" before
// the settings file applies the section and while it is left out, when
// nothing is trusted.
func appliedTrustedCommits(ctx context.Context, db rowQuerier) (string, error) {
	var trustedCommits string
	_, err := appliedSettingsValue(ctx, db, settingsTrustedCommitsPart.name, &trustedCommits)
	return trustedCommits, err
}

// trustsEveryCommitOnOrigin says whether the applied settings trust every
// commit on origin's branch (every_commit_on_origin).
func trustsEveryCommitOnOrigin(ctx context.Context, db rowQuerier) (bool, error) {
	trustedCommits, err := appliedTrustedCommits(ctx, db)
	return trustedCommits == commitsTrustedEveryCommitOnOrigin, err
}

// checkSystemsRepositoryCommitsSettings checks the systems_repository_commits
// section and the triage agent's own key, which goes with it: with
// every_commit_on_origin the keys of the check must be left out, since
// nothing reads them, and with signed_by_a_trusted_key_or_reviewed they are
// required. Without the section nothing is trusted, and the triage agent's
// key is unused but allowed, so a lock-out removes one section only.
func checkSystemsRepositoryCommitsSettings(s Settings, problems *settingsProblems) {
	const trustedCommits = "systems_repository_commits.trusted_commits"
	const triageAgentKeyPath = "triage_agent.commit_signing_public_key"
	commits := s.SystemsRepositoryCommits
	if commits == nil {
		return
	}
	agent := s.TriageAgent
	// A text key is left out when it is empty and a list key when it is nil;
	// a list written as [] is present.
	keysPresent := []struct {
		path    string
		present bool
	}{
		{"systems_repository_commits.trusted_commit_signing_public_keys", commits.TrustedCommitSigningPublicKeys != nil},
		{"systems_repository_commits.commits_without_a_trusted_signature_are_approved_by", commits.CommitsWithoutATrustedSignatureAreApprovedBy != ""},
		{"systems_repository_commits.paths_only_a_person_may_approve", commits.PathsOnlyAPersonMayApprove != nil},
	}
	switch commits.TrustedCommits {
	case commitsTrustedEveryCommitOnOrigin:
		unused := "used only with " + trustedCommits + ": " + commitsTrustedWhenSignedByATrustedKeyOrReviewed + "; remove it"
		for _, key := range keysPresent {
			if key.present {
				problems.add(key.path, unused)
			}
		}
		if agent != nil && agent.CommitSigningPublicKey != "" {
			problems.add(triageAgentKeyPath, unused)
		}
		problems.note(trustedCommits, "MMHotline trusts every commit on origin: it checks no signature, no commit waits for review, and the triage agent reviews no commit. Use this only when origin is on the team's own server and only keys that already hold root on every server can push to it.")
		return
	case commitsTrustedWhenSignedByATrustedKeyOrReviewed:
	default:
		problems.add(trustedCommits, "must be "+commitsTrustedWhenSignedByATrustedKeyOrReviewed+" or "+commitsTrustedEveryCommitOnOrigin)
		return
	}
	for _, key := range keysPresent {
		if !key.present {
			problems.add(key.path, "required")
		}
	}
	triageAgentFingerprint := ""
	if agent != nil {
		if agent.CommitSigningPublicKey == "" {
			problems.add(triageAgentKeyPath, "required with "+trustedCommits+": "+commitsTrustedWhenSignedByATrustedKeyOrReviewed+", so MMHotline knows the triage agent's commits")
		} else if fingerprint, err := systemsrepository.CommitSigningKeyFingerprint(agent.CommitSigningPublicKey); err != nil {
			problems.addError(triageAgentKeyPath, err)
		} else {
			triageAgentFingerprint = fingerprint
		}
	}
	for index, key := range commits.TrustedCommitSigningPublicKeys {
		path := fmt.Sprintf("systems_repository_commits.trusted_commit_signing_public_keys[%d]", index)
		fingerprint, err := systemsrepository.CommitSigningKeyFingerprint(key)
		switch {
		case err != nil:
			problems.addError(path, err)
		case fingerprint == triageAgentFingerprint:
			// A trusted key's commits pass whatever they change, so the
			// triage agent's commits would leave its two folders unchecked.
			problems.add(path, "is the triage agent's key, "+triageAgentKeyPath+"; list only the keys of the team's people and machines, because the triage agent's commits may change only one_time_playbooks/ and triage_agent_notes/")
		}
	}
	checkCommitApprovalSettings(*commits, agent != nil, problems)
}

// settingsCommitSigningPublicKeysPart saves the keys the commit check uses.
// They apply without an outside test. A changed key leaves the
// checked-through commit as it is: every commit through it passed with the
// keys of its time, and checking the history again with new keys would make
// every commit of a person who left, or of an earlier triage agent key, wait
// for review.
var settingsCommitSigningPublicKeysPart = settingsFilePart{
	name: "triage_agent.commit_signing_public_key, systems_repository_commits.trusted_commit_signing_public_keys",
	value: fileValue(func(s Settings) any {
		if s.SystemsRepositoryCommits == nil {
			return nil
		}
		keys := commitSigningKeysOf(s)
		return []any{keys.TriageAgent, keys.Trusted}
	}),
	apply: func(ctx context.Context, a *App, s Settings) error {
		return a.setCommitSigningKeys(ctx, commitSigningKeysOf(s))
	},
	shownNotes: commitSigningKeysSettingsNotes,
}

// commitSigningKeysOf are the keys the settings give the commit check: none
// without the systems_repository_commits section, and no triage agent key
// without the triage_agent section, whose commits then never exist.
func commitSigningKeysOf(s Settings) systemsrepository.CommitSigningKeys {
	if s.SystemsRepositoryCommits == nil {
		return systemsrepository.CommitSigningKeys{}
	}
	keys := systemsrepository.CommitSigningKeys{Trusted: s.SystemsRepositoryCommits.TrustedCommitSigningPublicKeys}
	if s.TriageAgent != nil {
		keys.TriageAgent = s.TriageAgent.CommitSigningPublicKey
	}
	return keys
}

// commitSigningKeysSettingsNotes lists the keys the commit check uses, as
// applied, so a person sees which keys MMHotline trusts without reading the
// file's long key lines: each key's type, comment, and fingerprint. A team
// without the systems_repository_commits section and without the triage
// agent runs no commit check of its own, so the part then has no note.
func commitSigningKeysSettingsNotes(ctx context.Context, tx *sql.Tx) ([]string, error) {
	trustedCommits, err := appliedTrustedCommits(ctx, tx)
	if err != nil {
		return nil, err
	}
	switch trustedCommits {
	case commitsTrustedEveryCommitOnOrigin:
		return []string{"The settings trust every commit on origin, so MMHotline checks no commit signing keys."}, nil
	case "":
		var triageAgentSetUp bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM triage_agent_setup)`).Scan(&triageAgentSetUp); err != nil || !triageAgentSetUp {
			return nil, err
		}
		return []string{"The settings file has no systems_repository_commits section, so MMHotline trusts no commit and refuses every MMX run and commit check."}, nil
	}
	var triageAgent, trustedJSON string
	err = tx.QueryRowContext(ctx, `SELECT commit_signing_public_key,trusted_commit_signing_public_keys_json FROM triage_agent_commit_check_state WHERE id=1`).Scan(&triageAgent, &trustedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		trustedJSON, err = "[]", nil
	}
	if err != nil {
		return nil, err
	}
	var trusted []string
	if err := json.Unmarshal([]byte(trustedJSON), &trusted); err != nil {
		return nil, err
	}
	var notes []string
	if triageAgent != "" {
		notes = append(notes, "Applied triage agent key: "+describeCommitSigningKey(triageAgent))
	}
	if len(trusted) == 0 {
		if triageAgent != "" {
			notes = append(notes, "No trusted keys are applied, so every commit not signed by the triage agent's key waits for review.")
		} else {
			notes = append(notes, "No trusted keys are applied, so every commit waits for review.")
		}
	}
	for _, key := range trusted {
		notes = append(notes, "Applied trusted key: "+describeCommitSigningKey(key))
	}
	return notes, nil
}

// describeCommitSigningKey is a key's type, comment, and fingerprint.
func describeCommitSigningKey(line string) string {
	fields := strings.Fields(line)
	fingerprint, err := systemsrepository.CommitSigningKeyFingerprint(line)
	if err != nil || len(fields) < 2 {
		return "a key MMHotline cannot read"
	}
	described := fields[0]
	if comment := strings.Join(fields[2:], " "); comment != "" {
		described += " " + comment
	}
	return described + " (" + fingerprint + ")"
}

// setCommitSigningKeys saves the keys the check uses, and keeps the
// checked-through commit. Without the systems_repository_commits section,
// such as during a lock-out, the keys go, but the checked-through commit
// stays: the commits through it were checked, so after the section returns
// the check need not read the whole history again.
func (a *App) setCommitSigningKeys(ctx context.Context, keys systemsrepository.CommitSigningKeys) error {
	for _, key := range append([]string{keys.TriageAgent}, keys.Trusted...) {
		if key == "" {
			continue
		}
		if _, err := systemsrepository.CommitSigningKeyFingerprint(key); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalid, err)
		}
	}
	trusted, err := json.Marshal(append([]string{}, keys.Trusted...))
	if err != nil {
		return err
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO triage_agent_commit_check_state(id,commit_signing_public_key,trusted_commit_signing_public_keys_json) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET commit_signing_public_key=excluded.commit_signing_public_key,trusted_commit_signing_public_keys_json=excluded.trusted_commit_signing_public_keys_json`, keys.TriageAgent, string(trusted))
		return err
	})
}

// errNoCommitIsTrusted is the check's refusal while the settings trust no
// commit. It is a conflict, so the command and pages show its words: during
// a lock-out the settings file has no systems_repository_commits section,
// and the cause must be plain to the person who runs the check then.
var errNoCommitIsTrusted = fmt.Errorf("%w: %s", ErrConflict, noCommitIsTrustedReason)

const noCommitIsTrustedReason = "the applied settings have no systems_repository_commits section, so MMHotline trusts no commit, as during a lock-out. Add systems_repository_commits to the settings file and apply the settings"

// CommitCheck is the answer of one check of the systems repository's
// commits through CheckedCommitID.
type CommitCheck struct {
	CheckedCommitID string
	// TrustedThroughCommitID is the newest commit on the checked commit's
	// first-parent line through which every commit passed the check or a
	// person reviewed it; "" when no commit is trusted yet.
	TrustedThroughCommitID string
	// Waiting are the commits through CheckedCommitID that wait for a
	// person's review, newest first.
	Waiting []CommitReview
}

// checkCommitsThrough is the one check of the systems repository's commits,
// before MMX runs, at the start of each triage agent run in the systems
// repository, and for the commit-reviews check command. It records each
// commit that newly waits, with one warn alert, and moves the checked-through
// commit forward to the trusted commit. The copy must hold target; the
// caller fetched origin first.
func (a *App) checkCommitsThrough(ctx context.Context, target string) (CommitCheck, error) {
	check := CommitCheck{CheckedCommitID: target}
	var keys systemsrepository.CommitSigningKeys
	var trusted, last string
	err := a.Store.db.QueryRowContext(ctx, `SELECT commit_signing_public_key,trusted_commit_signing_public_keys_json,last_checked_commit_id FROM triage_agent_commit_check_state WHERE id=1`).Scan(&keys.TriageAgent, &trusted, &last)
	if errors.Is(err, sql.ErrNoRows) {
		trusted, err = "[]", nil
	}
	if err != nil {
		return check, err
	}
	trustedCommits, err := appliedTrustedCommits(ctx, a.Store.db)
	if err != nil {
		return check, err
	}
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	switch trustedCommits {
	case "":
		return check, errNoCommitIsTrusted
	case commitsTrustedEveryCommitOnOrigin:
		// The settings trust every commit on origin: no signature is read,
		// no commit waits, and the checkpoint moves to the tip.
		trustedThrough, checkpoint, err := systemsrepository.TrustedThrough(ctx, directory, last, target, nil)
		if err != nil {
			return check, err
		}
		check.TrustedThroughCommitID = trustedThrough
		return check, a.moveCommitCheckpoint(ctx, last, checkpoint)
	}
	// signed_by_a_trusted_key_or_reviewed: without the triage_agent section
	// no triage agent key is applied, and only the trusted keys pass.
	if err := json.Unmarshal([]byte(trusted), &keys.Trusted); err != nil {
		return check, err
	}
	refused, err := systemsrepository.CommitsThatNeedReview(ctx, directory, last, target, keys)
	if err != nil {
		return check, err
	}
	var waitingIDs []string
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		check.Waiting, waitingIDs = nil, nil
		for _, commit := range refused {
			paths, err := json.Marshal(append([]systemsrepository.PathThatWaits{}, commit.Paths...))
			if err != nil {
				return err
			}
			changed, err := json.Marshal(append([]string{}, commit.ChangedPaths...))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO triage_agent_refused_commits(commit_id,reason_kind,paths_json,changed_paths_json,author,committed_at,message,diff,diff_truncated,found_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, commit.ID, commit.ReasonKind, string(paths), string(changed), commit.Author, commit.CommittedAt.Unix(), commit.Message, commit.Diff, commit.DiffTruncated, a.Now().Unix()); err != nil {
				return err
			}
			// The row exists: inserted now, or found by an earlier check.
			reviews, err := readCommitReviews(ctx, tx, `c.commit_id=?`, commit.ID)
			if err != nil {
				return err
			}
			if review := reviews[0]; !review.Reviewed() {
				check.Waiting = append(check.Waiting, review)
				waitingIDs = append(waitingIDs, review.ID)
			}
		}
		// One alert per commit: MMHotline's warning at once for a commit the
		// triage agent may not or cannot review; none for one it reviews,
		// whose commit review run starts at the background loop's next visit.
		_, err := a.commitsForTheTriageAgent(ctx, tx)
		return err
	})
	if err != nil {
		return check, err
	}
	trustedThrough, checkpoint, err := systemsrepository.TrustedThrough(ctx, directory, last, target, waitingIDs)
	if err != nil {
		return check, err
	}
	check.TrustedThroughCommitID = trustedThrough
	return check, a.moveCommitCheckpoint(ctx, last, checkpoint)
}

// moveCommitCheckpoint saves checkpoint, the commit every commit through
// which is trusted, when it differs from last. The state row may not exist
// yet, before any keys are applied.
func (a *App) moveCommitCheckpoint(ctx context.Context, last, checkpoint string) error {
	if checkpoint == "" || checkpoint == last {
		return nil
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return setCommitCheckpointTx(ctx, tx, checkpoint)
	})
}

func setCommitCheckpointTx(ctx context.Context, tx *sql.Tx, commitID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO triage_agent_commit_check_state(id,commit_signing_public_key,trusted_commit_signing_public_keys_json,last_checked_commit_id) VALUES(1,'','[]',?) ON CONFLICT(id) DO UPDATE SET last_checked_commit_id=excluded.last_checked_commit_id`, commitID)
	return err
}

// trustedCommit is the commit check as the copy asks for it whenever it
// brings its checkout forward (systemsrepository.TrustedCommit). The whole
// answer goes to check when check is not nil.
func (a *App) trustedCommit(check *CommitCheck) systemsrepository.TrustedCommit {
	return func(ctx context.Context, originCommitID string) (string, error) {
		answer, err := a.checkCommitsThrough(ctx, originCommitID)
		if check != nil {
			*check = answer
		}
		if err != nil {
			return "", fmt.Errorf("MMHotline could not check the systems repository commits: %w", err)
		}
		return answer.TrustedThroughCommitID, nil
	}
}

// triageAgentsOwnCommits says how the push filter knows the triage agent's
// own commits: by the agent's signature, or, when the settings trust every
// commit on origin and no signature is read, by MMHotline's record of the
// commits it pushed for the agent. A failed read counts no commit as the
// agent's, so the agent's changes to existing one-time playbooks wait.
func (a *App) triageAgentsOwnCommits(ctx context.Context) systemsrepository.TriageAgentsOwnCommits {
	every, err := trustsEveryCommitOnOrigin(ctx, a.Store.db)
	if err != nil || every {
		return systemsrepository.TriageAgentsOwnCommits{PushedByMMHotline: err == nil}
	}
	var key string
	_ = a.Store.db.QueryRowContext(ctx, `SELECT commit_signing_public_key FROM triage_agent_commit_check_state WHERE id=1`).Scan(&key)
	return systemsrepository.TriageAgentsOwnCommits{SigningKey: key}
}

// mmxRefusal is the plain refusal of an MMX run while a commit on origin's
// branch waits for review, or when the check failed; "" lets MMX run. MMX
// runs only when nothing waits on origin's branch, so a request judged at
// the trusted commit while a commit waits is refused, not judged again.
func (a *App) mmxRefusal(check CommitCheck, err error) string {
	if errors.Is(err, errNoCommitIsTrusted) {
		return "MMHotline did not run the playbook: " + noCommitIsTrustedReason + "."
	}
	if err != nil {
		return "MMHotline did not run the playbook: " + err.Error()
	}
	var messages []string
	for _, review := range check.Waiting {
		messages = append(messages, review.refusalMessage(a.WebURL))
	}
	return strings.Join(messages, "\n\n")
}

// waitingCommitsRefusal is the review refusal of each commit that waits, or
// "" when none waits. A failed read only loses the explanation, so it gives "".
func (a *App) waitingCommitsRefusal(ctx context.Context) string {
	var reviews []CommitReview
	_ = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		reviews, err = readCommitReviews(ctx, tx, `c.reviewed_at IS NULL ORDER BY c.found_at,c.commit_id`)
		return err
	})
	return a.mmxRefusal(CommitCheck{Waiting: reviews}, nil)
}

// trustedRunBase fetches origin and returns the run's base and its bundle:
// the trusted commit of origin's branch, where the copy's checkout is too, so
// the agent's copy never holds a commit that waits for a person's review,
// and the agent's rules and instructions come from commits the team trusts.
// The base is origin's branch itself when nothing waits.
func (a *App) trustedRunBase(ctx context.Context) (bundle []byte, base string, err error) {
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	var check CommitCheck
	if _, err := systemsrepository.UpdateFromOrigin(ctx, directory, a.trustedCommit(&check)); err != nil {
		return nil, "", err
	}
	if check.TrustedThroughCommitID == "" {
		return nil, "", fmt.Errorf("no commit of the systems repository is trusted yet: %s for a person's review at %s/approvals", countText(len(check.Waiting), "commit waits", "commits wait"), strings.TrimRight(a.WebURL, "/"))
	}
	bundle, err = systemsrepository.BundleThrough(ctx, directory, check.TrustedThroughCommitID)
	return bundle, check.TrustedThroughCommitID, err
}

// CommitReview is a commit that waits or waited for review, and the facts
// and decision shown on its page.
type CommitReview struct {
	systemsrepository.RefusedCommit
	FoundAt, ReviewedAt time.Time
	ReviewedBy          string
	// ReviewerKind is member, unix_user, trust_through, or triage_agent once
	// reviewed.
	ReviewerKind string
	// TriageAgentReviewRunID is the commit review run the commit was given to.
	TriageAgentReviewRunID string
	// AlertSent is the one alert people got about the commit, '' while the
	// triage agent reviews it.
	AlertSent string
	// TriageAgentReview is the triage agent's latest decision on the commit,
	// with its report; nil when it gave none.
	TriageAgentReview *TriageAgentCommitReview
}

// TriageAgentCommitReview is the triage agent's decision on a commit,
// approve or needs_a_person, as its review-commit request recorded it.
type TriageAgentCommitReview struct {
	RunID string `json:"run_id"`
	// Decision is approve or needs_a_person.
	Decision string `json:"decision"`
	// AlertSeverity is the report's, with needs_a_person; approve sends one
	// info alert.
	AlertSeverity string    `json:"alert_severity,omitempty"`
	Report        string    `json:"report"`
	DecidedAt     time.Time `json:"decided_at"`
}

// NeedsAPerson says the triage agent decided that a person must review.
func (v TriageAgentCommitReview) NeedsAPerson() bool { return v.Decision == "needs_a_person" }

func (v CommitReview) ShortID() string { return v.ID[:min(len(v.ID), 7)] }

// NotSignedByATrustedKey says nobody the team trusts made the commit.
func (v CommitReview) NotSignedByATrustedKey() bool {
	return v.ReasonKind == systemsrepository.NotSignedByATrustedKey
}

// Reason is the reason the commit waits, in words that follow its ID.
func (v CommitReview) Reason() string {
	if v.NotSignedByATrustedKey() {
		return "which no key the team trusts signed"
	}
	return "signed by the triage agent's key, which changes paths that need a person's review"
}

func (v CommitReview) Question() string {
	if v.NotSignedByATrustedKey() {
		return "Allow MMX runs past commit " + v.ShortID() + ", which no key the team trusts signed?"
	}
	return "Allow MMX runs past commit " + v.ShortID() + ", signed by the triage agent's key?"
}
func (v CommitReview) URL() string    { return "/approvals/commits/" + v.ID }
func (v CommitReview) Reviewed() bool { return !v.ReviewedAt.IsZero() }

func (v CommitReview) alertTitle() string {
	if v.NotSignedByATrustedKey() {
		return "Review needed for commit " + v.ShortID() + ", which no key the team trusts signed"
	}
	return "Review needed for commit " + v.ShortID() + ", signed by the triage agent's key"
}

// alertMessage is MMHotline's warning; notReviewedBecause, when set, says
// why the triage agent did not review the commit.
func (v CommitReview) alertMessage(webURL, notReviewedBecause string) string {
	review := strings.TrimRight(webURL, "/") + v.URL()
	if v.NotSignedByATrustedKey() {
		because := ""
		if notReviewedBecause != "" {
			because = " The triage agent did not review the commit: " + notReviewedBecause + "."
		}
		return "MMX runs wait for a person's review of commit " + v.ShortID() + ", which no key the team trusts signed." + because + " Until then, the triage agent works from the commit before it. If nobody on the team made the commit, someone may have broken in: follow the team's break-in procedure. Read the commit's changes at " + review + "."
	}
	return "MMX runs wait for a person's review of commit " + v.ShortID() + ", signed by the triage agent's key. Read the commit's changes and reasons at " + review + "."
}

func (v CommitReview) refusalMessage(webURL string) string {
	review := strings.TrimRight(webURL, "/") + v.URL()
	if v.NotSignedByATrustedKey() {
		return fmt.Sprintf("MMHotline did not run the playbook: commit %s, which no key the team trusts signed, waits for a person's review at %s. MMHotline can then run MMX past the reviewed commit on the next playbook run request.", v.ShortID(), review)
	}
	var reasons []string
	for _, path := range v.Paths {
		reasons = append(reasons, path.Path+" "+path.Reason)
	}
	return fmt.Sprintf("MMHotline did not run the playbook: commit %s, signed by the triage agent's key, changes paths that need a person's review: %s. A person reviews the commit at %s. MMHotline can then run MMX past the reviewed commit on the next playbook run request.", v.ShortID(), strings.Join(reasons, "; "), review)
}

// readCommitReviews reads the commits of condition, with the triage agent's
// latest decision on each: its latest commit_review request MMHotline
// recorded.
func readCommitReviews(ctx context.Context, tx *sql.Tx, condition string, args ...any) ([]CommitReview, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.commit_id,c.reason_kind,c.paths_json,c.changed_paths_json,c.author,c.committed_at,c.message,c.diff,c.diff_truncated,c.found_at,
 CASE c.reviewer_kind WHEN 'member' THEN coalesce(m.name,'') WHEN 'unix_user' THEN 'the mmhotline command, run by '||c.reviewed_by_unix_user WHEN 'trust_through' THEN 'the mmhotline command, run by '||c.reviewed_by_unix_user||', marking every commit through '||substr(t.commit_id,1,7)||' trusted' WHEN 'triage_agent' THEN 'the triage agent' ELSE '' END,
 c.reviewed_at,c.reviewer_kind,coalesce(c.triage_agent_review_run_id,''),c.alert_sent,
 coalesce(r.run_id,''),coalesce(r.commit_review_decision,''),coalesce(r.alert_severity,''),coalesce(r.report_text,''),coalesce(r.answered_at,0)
 FROM triage_agent_refused_commits c LEFT JOIN members m ON m.id=c.reviewed_by_member_id LEFT JOIN triage_agent_commit_trust_throughs t ON t.id=c.reviewed_through_trust_through_id
 LEFT JOIN triage_agent_requests r ON r.rowid=(SELECT rowid FROM triage_agent_requests WHERE kind='commit_review' AND commit_id=c.commit_id AND outcome='recorded' ORDER BY answered_at DESC,rowid DESC LIMIT 1)
 WHERE `+condition, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []CommitReview
	for rows.Next() {
		var v CommitReview
		var paths, changed string
		var committed, found int64
		var reviewed sql.NullInt64
		var agent TriageAgentCommitReview
		var decided int64
		if err := rows.Scan(&v.ID, &v.ReasonKind, &paths, &changed, &v.Author, &committed, &v.Message, &v.Diff, &v.DiffTruncated, &found, &v.ReviewedBy, &reviewed, &v.ReviewerKind, &v.TriageAgentReviewRunID, &v.AlertSent,
			&agent.RunID, &agent.Decision, &agent.AlertSeverity, &agent.Report, &decided); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(paths), &v.Paths); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(changed), &v.ChangedPaths); err != nil {
			return nil, err
		}
		if agent.RunID != "" {
			agent.DecidedAt = time.Unix(decided, 0).UTC()
			v.TriageAgentReview = &agent
		}
		v.CommittedAt = time.Unix(committed, 0).UTC()
		v.FoundAt = time.Unix(found, 0).UTC()
		if reviewed.Valid {
			v.ReviewedAt = time.Unix(reviewed.Int64, 0).UTC()
		}
		reviews = append(reviews, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return reviews, namePathsOnlyAPersonMayApprove(ctx, tx, reviews)
}

// namePathsOnlyAPersonMayApprove adds, to each commit no trusted key signed,
// the changed paths of systems_repository_commits.paths_only_a_person_may_approve, so the
// commit's page and commit-reviews show say why the triage agent left the
// commit to a person. The check records paths only for the triage agent's
// own commits; for these, the list as applied now decides.
func namePathsOnlyAPersonMayApprove(ctx context.Context, tx *sql.Tx, reviews []CommitReview) error {
	settings, err := readCommitApprovalSettings(ctx, tx)
	if err != nil || len(settings.PathsOnlyAPersonMayApprove) == 0 {
		return err
	}
	for i := range reviews {
		if !reviews[i].NotSignedByATrustedKey() {
			continue
		}
		for _, path := range pathsOnlyAPersonMayApprove(settings.PathsOnlyAPersonMayApprove, reviews[i].ChangedPaths) {
			reviews[i].Paths = append(reviews[i].Paths, systemsrepository.PathThatWaits{Path: path, Reason: "is on systems_repository_commits.paths_only_a_person_may_approve, so only a person may approve the commit"})
		}
	}
	return nil
}

// CommitReviews are the commits that wait for review, oldest found first.
func (a *App) CommitReviews(ctx context.Context, p Principal) ([]CommitReview, error) {
	var reviews []CommitReview
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		reviews, err = readCommitReviews(ctx, tx, `c.reviewed_at IS NULL ORDER BY c.found_at,c.commit_id`)
		return err
	})
	return reviews, err
}

// maximumListedCommitsTheTriageAgentApproved bounds the Approvals page's and
// commit-reviews list's list of the triage agent's approvals, newest first,
// so the page stays short; commit-reviews show reads any one by its ID.
const maximumListedCommitsTheTriageAgentApproved = 50

// CommitsTheTriageAgentApproved are the commits the triage agent marked
// reviewed, newest first.
func (a *App) CommitsTheTriageAgentApproved(ctx context.Context, p Principal) ([]CommitReview, error) {
	var reviews []CommitReview
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		reviews, err = readCommitReviews(ctx, tx, `c.reviewer_kind='triage_agent' ORDER BY c.reviewed_at DESC,c.commit_id LIMIT ?`, maximumListedCommitsTheTriageAgentApproved)
		return err
	})
	return reviews, err
}

// ReportTitle is the first line of the triage agent's report.
func (v TriageAgentCommitReview) ReportTitle() string { return reportTitleLine(v.Report) }

func (a *App) CommitReview(ctx context.Context, p Principal, id string) (CommitReview, error) {
	var v CommitReview
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		reviews, err := readCommitReviews(ctx, tx, `c.commit_id=?`, id)
		if err != nil {
			return err
		}
		if len(reviews) == 0 {
			return ErrNotFound
		}
		v = reviews[0]
		return nil
	})
	return v, err
}

// ReviewCommit records the review of any active member, or of root through
// the command socket, once, and resolves the warning in the same
// transaction. A review permits later runs past the commit; it starts none.
func (a *App) ReviewCommit(ctx context.Context, p Principal, id string) error {
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, false)
		if err != nil {
			return err
		}
		reviews, err := readCommitReviews(ctx, tx, `c.commit_id=?`, id)
		if err != nil {
			return err
		}
		if len(reviews) == 0 {
			return ErrNotFound
		}
		if reviews[0].Reviewed() {
			return nil
		}
		return a.markCommitReviewedTx(ctx, tx, commitReviewer{member: m, unixUser: p.UnixUser}, reviews[0])
	})
}

// commitReviewer is who marks a commit reviewed: a member on the page, a
// Unix user through the command socket, a Unix user's commit-reviews
// trust-through (trustThroughID), or the triage agent in its commit review
// run (triageAgentRunID).
type commitReviewer struct {
	member           Member
	unixUser         string
	trustThroughID   int64
	triageAgentRunID string
}

func (r commitReviewer) kind() string {
	switch {
	case r.triageAgentRunID != "":
		return "triage_agent"
	case r.trustThroughID != 0:
		return "trust_through"
	case r.member.ID != "":
		return "member"
	}
	return "unix_user"
}

// markCommitReviewedTx is the one way a commit becomes reviewed. It resolves
// MMHotline's warning about the commit in the same transaction, when one
// went out.
func (a *App) markCommitReviewedTx(ctx context.Context, tx *sql.Tx, reviewer commitReviewer, review CommitReview) error {
	member := sql.NullString{String: reviewer.member.ID, Valid: reviewer.member.ID != ""}
	trustThrough := sql.NullInt64{Int64: reviewer.trustThroughID, Valid: reviewer.trustThroughID != 0}
	if _, err := tx.ExecContext(ctx, `UPDATE triage_agent_refused_commits SET reviewed_by_member_id=?,reviewed_by_unix_user=?,reviewed_through_trust_through_id=?,reviewer_kind=?,reviewed_at=? WHERE commit_id=?`, member, reviewer.unixUser, trustThrough, reviewer.kind(), a.Now().Unix(), review.ID); err != nil {
		return err
	}
	if review.AlertSent != "mmhotline_warning" {
		return nil
	}
	source := "member:" + reviewer.member.ID
	if reviewer.member.ID == "" {
		source = "unix-user:" + reviewer.unixUser
	}
	return RecordSystemAlert(ctx, tx, source, "", "resolved-warn", review.alertTitle(), reviewer.member.Name+" reviewed commit "+review.ShortID()+". MMHotline can run MMX past the reviewed commit on the next playbook run request, and the triage agent's next run starts past it. Review: "+strings.TrimRight(a.WebURL, "/")+review.URL(), a.Now())
}

// trustCommitsThrough is commit-reviews trust-through: a person on the
// server vouches for commitID and every commit before it. A person's
// explicit act is the root of trust; a setting in the systems repository
// could be moved by the commit it is meant to judge. It is for a system that
// already runs, such as after an upgrade, a rebuilt server, or a reset
// database, whose history the first check would otherwise read whole
// against today's keys. It returns the check through commitID afterwards and
// what changed.
func (a *App) trustCommitsThrough(ctx context.Context, p Principal, commitID string) (CommitCheck, string, error) {
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	// The fetch runs no check: a first check would read the whole history,
	// which this command exists to avoid.
	leaveTheCheckout := func(context.Context, string) (string, error) { return "", nil }
	origin, err := systemsrepository.UpdateFromOrigin(ctx, directory, leaveTheCheckout)
	if err != nil {
		return CommitCheck{}, "", fmt.Errorf("MMHotline could not fetch origin into its copy of the systems repository: %w", err)
	}
	found, err := systemsrepository.HasCommit(ctx, directory, commitID)
	if err != nil {
		return CommitCheck{}, "", fmt.Errorf("%w: --commit-id %s", ErrInvalid, err)
	}
	if !found {
		return CommitCheck{}, "", fmt.Errorf("%w: MMHotline's copy of the systems repository has no commit %s, even after fetching origin", ErrNotFound, commitID)
	}
	if onBranch, err := systemsrepository.IsAncestor(ctx, directory, commitID, origin); err != nil {
		return CommitCheck{}, "", err
	} else if !onBranch {
		return CommitCheck{}, "", fmt.Errorf("%w: commit %s is not on origin's branch, which ends at %s; trust only a commit of the branch MMHotline runs", ErrInvalid, commitID, origin)
	}
	var last string
	err = a.Store.db.QueryRowContext(ctx, `SELECT last_checked_commit_id FROM triage_agent_commit_check_state WHERE id=1`).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return CommitCheck{}, "", err
	}
	if trustedCommits, err := appliedTrustedCommits(ctx, a.Store.db); err != nil {
		return CommitCheck{}, "", err
	} else if trustedCommits == "" {
		return CommitCheck{}, "", errNoCommitIsTrusted
	}
	short := commitID[:7]
	message := ""
	// The checkpoint never moves back. A lost checkpoint object is no
	// checkpoint, so its error counts as not behind.
	behind := false
	if last != "" {
		behind, _ = systemsrepository.IsAncestor(ctx, directory, commitID, last)
	}
	if behind {
		message = "Every commit through " + short + " is already trusted: the checked-through commit " + last[:7] + " holds it. Nothing changed."
	} else {
		var vouched []CommitReview
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			var err error
			vouched, err = readCommitReviews(ctx, tx, `c.reviewed_at IS NULL ORDER BY c.found_at,c.commit_id`)
			return err
		})
		if err != nil {
			return CommitCheck{}, "", err
		}
		// Only the waiting commits that commitID holds; Git, not the database, knows.
		var before []CommitReview
		for _, review := range vouched {
			if holds, err := systemsrepository.IsAncestor(ctx, directory, review.ID, commitID); err == nil && holds {
				before = append(before, review)
			}
		}
		server, _ := os.Hostname()
		err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			m, err := RequireMember(ctx, tx, p, true)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `INSERT INTO triage_agent_commit_trust_throughs(commit_id,unix_user,server_name,trusted_at) VALUES(?,?,?,?)`, commitID, p.UnixUser, server, a.Now().Unix())
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return err
			}
			if err := setCommitCheckpointTx(ctx, tx, commitID); err != nil {
				return err
			}
			for _, review := range before {
				if err := a.markCommitReviewedTx(ctx, tx, commitReviewer{member: m, unixUser: p.UnixUser, trustThroughID: id}, review); err != nil {
					return err
				}
			}
			// The bulk trust is visible to the team.
			return RecordSystemAlert(ctx, tx, "unix-user:"+p.UnixUser, "", "info", p.UnixUser+" on "+server+" marked every commit through "+short+" trusted", p.UnixUser+" on "+server+" marked every commit through "+short+" trusted with sudo mmhotline commit-reviews trust-through. MMX runs and triage agent runs pass every commit through it; later commits still go through the commit check.", a.Now())
		})
		if err != nil {
			return CommitCheck{}, "", err
		}
		message = fmt.Sprintf("Marked every commit through %s trusted, and %s that waited for review as reviewed.", short, countText(len(before), "commit", "commits"))
	}
	// The checkout moves forward through the one function, which also checks
	// the commits after commitID.
	if _, err := systemsrepository.UpdateFromOrigin(ctx, directory, a.trustedCommit(nil)); err != nil {
		return CommitCheck{}, "", err
	}
	check, err := a.checkCommitsThrough(ctx, commitID)
	return check, message, err
}
