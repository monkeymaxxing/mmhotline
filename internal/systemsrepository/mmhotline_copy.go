package systemsrepository

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// agentFolders are the only folders the agent's commits may change without a
// person: its dated one-time MMX playbooks and one note per triage session.
var agentFolders = []string{"one_time_playbooks/", "triage_agent_notes/"}

// MMHotline's merge commits carry the identity configured in its copy, the
// mmhotline user's, which the server's signature check may compare with the
// signing key; this one stands in only when the copy names nobody.
var mergeIdentity = []string{
	"GIT_AUTHOR_NAME=MMHotline triage service", "GIT_AUTHOR_EMAIL=mmhotline@localhost",
	"GIT_COMMITTER_NAME=MMHotline triage service", "GIT_COMMITTER_EMAIL=mmhotline@localhost",
}

// pushAttempts bounds the fetch, merge, and push rounds while origin keeps
// moving; a branch that busy needs a person to merge.
const pushAttempts = 3

// TrustedCommit answers, for the commit origin's branch ends at, the trusted
// commit: the newest commit through which every commit passed the commit
// check or a person reviewed it, or "" when no commit is trusted yet. The
// application's commit check answers it.
type TrustedCommit func(ctx context.Context, originCommitID string) (string, error)

// bringCheckoutForward is the one place MMHotline moves the working tree of
// its copy: forward to the trusted commit for originCommitID, never past it
// and never back. Origin's branch ref may be ahead; the checkout, which MMX,
// mmx explain, mmx nodes, and every read of MMHotline's copy use, holds no
// commit that waits for a person's review. It returns why the working tree
// stayed where it was, or "" when it is at the trusted commit. The caller
// holds the copy's lock.
func bringCheckoutForward(ctx context.Context, directory, branch, originCommitID string, trusted TrustedCommit) (notMovedBecause string, err error) {
	target, err := trusted(ctx, originCommitID)
	if err != nil {
		return "", err
	}
	if target == "" {
		return "no commit of origin's " + branch + " is trusted yet: each commit waits for a person's review", nil
	}
	return fastForwardWorkingTree(ctx, directory, branch, target), nil
}

// UpdateFromOrigin fetches origin's branch into MMHotline's copy at
// directory, under the copy's lock, returns the commit the branch ends at,
// and brings the checkout forward to the trusted commit when it can. The
// branch is the one MMHotline's copy has checked out, as the team's server
// setup cloned it, usually main. It refuses a copy whose branch has commits
// origin lacks.
func UpdateFromOrigin(ctx context.Context, directory string, trusted TrustedCommit) (originCommitID string, err error) {
	err = withCopyLock(ctx, directory, CopyLockOptions{}, func() error {
		branch, err := checkedOutBranch(ctx, directory)
		if err != nil {
			return err
		}
		if originCommitID, err = fetchOrigin(ctx, directory, branch); err != nil {
			return err
		}
		localCommit, err := commitID(ctx, directory, "refs/heads/"+branch)
		if err != nil {
			return err
		}
		behind, err := IsAncestor(ctx, directory, localCommit, originCommitID)
		if err != nil {
			return err
		}
		if !behind {
			return fmt.Errorf("the %s branch of MMHotline's copy of the systems repository has commits missing from origin; a person must push or remove the local commits", branch)
		}
		// A run's bundle never comes from the working tree, so a working
		// tree left behind, such as for uncommitted changes, harms nothing.
		_, err = bringCheckoutForward(ctx, directory, branch, originCommitID, trusted)
		return err
	})
	return originCommitID, err
}

// runBaseRef names the run's base only while MMHotline bundles it: a bundle
// carries refs, not bare commit IDs.
const runBaseRef = "refs/mmhotline/run-base"

// BundleThrough returns a bundle of baseCommitID and its history, where the
// agent's copy starts. The bundle holds nothing after baseCommitID, so a
// commit that waits for a person's review never reaches the agent's copy.
func BundleThrough(ctx context.Context, directory, baseCommitID string) (bundle []byte, err error) {
	if err := checkCommitID(baseCommitID); err != nil {
		return nil, err
	}
	err = withCopyLock(ctx, directory, CopyLockOptions{}, func() error {
		if _, err := runGit(ctx, directory, nil, "update-ref", runBaseRef, baseCommitID); err != nil {
			return err
		}
		bundle, err = createBundle(ctx, directory, runBaseRef)
		// A ref left behind names a commit already in the copy; it harms nothing.
		_, _ = runGit(ctx, directory, nil, "update-ref", "-d", runBaseRef)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("bundle commit %s of MMHotline's copy of the systems repository: %w", baseCommitID, err)
	}
	return bundle, nil
}

// ReceiveAgentCommits handles one run's commits: bundle is what
// AgentCommitsBundle made in the agent's copy, and base is the commit
// the run started from. The commits go to origin only when every
// change is inside one_time_playbooks/ or triage_agent_notes/ and adds or
// changes only regular files; otherwise they stay under a private ref for a
// person. It never touches MMHotline's working tree before the push, because
// MMX may be using it.
//
// An error means MMHotline could not finish, such as when origin cannot be
// reached; the result still names the private ref once the commits are in it.
func ReceiveAgentCommits(ctx context.Context, directory string, bundle []byte, baseCommitID string, trusted TrustedCommit) (result ReceivedAgentCommits, err error) {
	lockErr := withCopyLock(ctx, directory, CopyLockOptions{}, func() error {
		result, err = receiveAgentCommits(ctx, directory, bundle, baseCommitID, trusted)
		return nil
	})
	if lockErr != nil {
		return ReceivedAgentCommits{}, lockErr
	}
	return result, err
}

func receiveAgentCommits(ctx context.Context, directory string, bundle []byte, baseCommitID string, trusted TrustedCommit) (ReceivedAgentCommits, error) {
	if err := checkCommitID(baseCommitID); err != nil {
		return ReceivedAgentCommits{}, err
	}
	refused := func(reason string) (ReceivedAgentCommits, error) {
		return ReceivedAgentCommits{Outcome: Refused, Reason: reason}, nil
	}
	if err := checkBundleSize(int64(len(bundle))); err != nil {
		return refused(err.Error())
	}
	path, remove, err := temporaryBundleFile(bundle)
	if err != nil {
		return ReceivedAgentCommits{}, err
	}
	defer remove()
	if _, err := runGit(ctx, directory, nil, "bundle", "verify", path); err != nil {
		return refused("the agent's bundle is not usable: " + err.Error())
	}
	privateRef := newPrivateRef()
	// FETCH_HEAD stays as it was: a git pull in MMHotline's copy, such as
	// a person's helper, merges what FETCH_HEAD names and must never merge these commits.
	if _, err := runGit(ctx, directory, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", path, "+refs/heads/main:"+privateRef); err != nil {
		return refused("the agent's bundle has no usable main branch: " + err.Error())
	}
	result := ReceivedAgentCommits{PrivateRef: privateRef}
	if result.AgentCommitID, err = commitID(ctx, directory, privateRef); err != nil {
		return result, err
	}
	fromBase, err := IsAncestor(ctx, directory, baseCommitID, result.AgentCommitID)
	if err != nil {
		return result, err
	}
	if !fromBase {
		result.Outcome = Refused
		result.Reason = fmt.Sprintf("the agent's commits do not start from %s, the commit its run started at", baseCommitID)
		return result, nil
	}
	// Each commit is checked, not only the base against the tip: the MMX
	// run check judges each triage-signed commit against what it changes,
	// so a change a later commit undoes must wait too.
	commits, err := runGit(ctx, directory, nil, "rev-list", baseCommitID+".."+result.AgentCommitID)
	if err != nil {
		return result, fmt.Errorf("list the agent's commits: %w", err)
	}
	var changes strings.Builder
	for _, commit := range strings.Fields(commits) {
		// diff-tree, unlike git diff, ignores diff settings in the configuration;
		// --ignore-submodules=none shows every submodule change even so, and
		// -m lists a merge's changes against each parent.
		changed, err := commitChanges(ctx, directory, commit)
		if err != nil {
			return result, fmt.Errorf("list the agent's changes: %w", err)
		}
		changes.WriteString(changed)
	}
	if result.PathsThatWait, err = pathsThatWait(changes.String()); err != nil {
		return result, err
	}
	if len(result.PathsThatWait) > 0 {
		result.Outcome = WaitsForAPerson
		result.Reason = "a person must approve these changes: " + pathsThatWaitText(result.PathsThatWait)
		return result, nil
	}
	branch, err := checkedOutBranch(ctx, directory)
	if err != nil {
		return result, err
	}
	pushed, waitsBecause, err := pushAgentCommits(ctx, directory, branch, result.AgentCommitID)
	if err != nil {
		return result, err
	}
	if waitsBecause != "" {
		result.Outcome, result.Reason = WaitsForAPerson, waitsBecause
		return result, nil
	}
	result.Outcome, result.PushedCommitID = Pushed, pushed
	// The commits are on origin now. A ref that could not be removed stays
	// named in the result, which harms nothing.
	if _, err := runGit(ctx, directory, nil, "update-ref", "-d", privateRef); err == nil {
		result.PrivateRef = ""
	}
	// The push is done; a check that fails leaves the working tree, and says why.
	if result.WorkingTreeNotUpdatedBecause, err = bringCheckoutForward(ctx, directory, branch, pushed, trusted); err != nil {
		result.WorkingTreeNotUpdatedBecause = "MMHotline could not check the systems repository commits: " + err.Error()
	}
	return result, nil
}

// pathsThatWait reads git diff-tree -r -z output, whose records are
// ":<old mode> <new mode> <old object> <new object> <status>" and a path.
func pathsThatWait(rawChanges string) ([]PathThatWaits, error) {
	if rawChanges == "" {
		return nil, nil
	}
	fields := strings.Split(strings.TrimSuffix(rawChanges, "\x00"), "\x00")
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("unexpected git diff-tree output %q", rawChanges)
	}
	var waiting []PathThatWaits
	for index := 0; index < len(fields); index += 2 {
		record := strings.Fields(strings.TrimPrefix(fields[index], ":"))
		if !strings.HasPrefix(fields[index], ":") || len(record) != 5 {
			return nil, fmt.Errorf("unexpected git diff-tree record %q", fields[index])
		}
		// Several commits can change one path the same way; list it once.
		if reason := changeWaitsBecause(fields[index+1], record[1]); reason != "" && !slices.Contains(waiting, PathThatWaits{Path: fields[index+1], Reason: reason}) {
			waiting = append(waiting, PathThatWaits{Path: fields[index+1], Reason: reason})
		}
	}
	return waiting, nil
}

func pathsThatWaitText(paths []PathThatWaits) string {
	var reasons []string
	for _, path := range paths {
		reasons = append(reasons, path.Path+" "+path.Reason)
	}
	return strings.Join(reasons, "; ")
}

// changeWaitsBecause says why a change to path, which Git now gives newMode,
// needs a person, or returns "" when MMHotline may push it.
func changeWaitsBecause(path, newMode string) string {
	for _, part := range strings.Split(path, "/") {
		// A crafted tree can hold these names; a ".git" part breaks every checkout.
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Sprintf("has the path part %q", part)
		}
	}
	inside := false
	for _, folder := range agentFolders {
		inside = inside || strings.HasPrefix(path, folder)
	}
	if !inside {
		return "is outside " + strings.Join(agentFolders, " and ")
	}
	switch newMode {
	case "100644", "000000": // a regular file, or a deletion
		return ""
	case "100755":
		return "is an executable file"
	case "120000":
		return "is a symlink"
	case "160000":
		return "is a submodule"
	}
	return "is not a regular file (Git mode " + newMode + ")"
}

// pushAgentCommits pushes the agent's commit to origin's branch, merging it
// first with commits on origin that the agent's commit lacks, such as a
// person's. The merge is built with merge-tree and commit-tree, which never
// touch the working tree or run a hook. It returns the pushed commit, or why
// the commits wait for a person.
func pushAgentCommits(ctx context.Context, directory, branch, agentCommitID string) (pushed, waitsBecause string, err error) {
	for range pushAttempts {
		originCommit, err := fetchOrigin(ctx, directory, branch)
		if err != nil {
			return "", "", err
		}
		// Every bundle of a run starts at the run's base, so a later one
		// carries the commits an earlier request already pushed; a merge of
		// those would change nothing.
		contained, err := IsAncestor(ctx, directory, agentCommitID, originCommit)
		if err != nil {
			return "", "", err
		}
		if contained {
			return originCommit, "", nil
		}
		// Origin still at the base, or moved only to commits the agent's
		// commit has, such as those an earlier request of the same run pushed:
		// the agent's commit goes as it is.
		fastForward, err := IsAncestor(ctx, directory, originCommit, agentCommitID)
		if err != nil {
			return "", "", err
		}
		target := agentCommitID
		if !fastForward {
			output, err := runGit(ctx, directory, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", originCommit, agentCommitID)
			if exitCode(err) == 1 {
				conflicts := strings.Split(strings.TrimSpace(output), "\n")[1:]
				return "", fmt.Sprintf("the agent's commits conflict with newer commits on origin in %s", strings.Join(conflicts, ", ")), nil
			}
			if err != nil {
				return "", "", fmt.Errorf("merge the agent's commits with origin's %s: %w", branch, err)
			}
			mergeTree := strings.TrimSpace(output)
			// The merge follows a person's renames, so it can carry an agent's
			// edit out of the agent folders; what it adds to origin is judged
			// like the agent's own commits.
			changes, err := runGit(ctx, directory, nil, "diff-tree", "-r", "-z", "--no-renames", "--ignore-submodules=none", originCommit, mergeTree)
			if err != nil {
				return "", "", fmt.Errorf("list the changes the merge of the agent's commits makes: %w", err)
			}
			waiting, err := pathsThatWait(changes)
			if err != nil {
				return "", "", err
			}
			if len(waiting) > 0 {
				return "", "a person must approve these changes, which the merge with newer commits on origin makes: " + pathsThatWaitText(waiting), nil
			}
			signing, err := signsCommits(ctx, directory)
			if err != nil {
				return "", "", err
			}
			arguments := []string{"commit-tree", mergeTree, "-p", originCommit, "-p", agentCommitID, "-m", "Merge the triage agent's commits"}
			if signing {
				arguments = append(arguments, "-S")
			}
			var identity []string
			if configured, _ := runGit(ctx, directory, nil, "config", "--get", "user.email"); strings.TrimSpace(configured) == "" {
				identity = mergeIdentity
			}
			merge, err := runGit(ctx, directory, identity, arguments...)
			if err != nil && signing {
				// The server applies only signed commits, so an unsigned merge is no use.
				return "", "the merge of the agent's commits could not be signed: " + err.Error(), nil
			}
			if err != nil {
				return "", "", fmt.Errorf("commit the merge of the agent's commits: %w", err)
			}
			target = strings.TrimSpace(merge)
		}
		// A plain push: Git refuses it when origin moved since the fetch.
		_, pushErr := runGit(ctx, directory, nil, "push", "--quiet", "origin", target+":refs/heads/"+branch)
		if pushErr == nil {
			return target, "", nil
		}
		// Only a push refused because origin moved is tried again.
		now, err := fetchOrigin(ctx, directory, branch)
		if err != nil || now == originCommit {
			return "", "", fmt.Errorf("push the agent's commits to origin: %w", pushErr)
		}
	}
	return "", fmt.Sprintf("origin's %s changed during each of %d tries to push the agent's commits", branch, pushAttempts), nil
}

// signsCommits reads whether MMHotline's copy is set up to sign commits, with
// commit.gpgSign and the signing key and format beside it. commit-tree,
// unlike git commit, ignores commit.gpgSign, so MMHotline passes -S itself.
func signsCommits(ctx context.Context, directory string) (bool, error) {
	output, err := runGit(ctx, directory, nil, "config", "--type=bool", "--get", "commit.gpgsign")
	if exitCode(err) == 1 { // not set
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read commit.gpgsign in MMHotline's copy of the systems repository: %w", err)
	}
	return strings.TrimSpace(output) == "true", nil
}

func checkedOutBranch(ctx context.Context, directory string) (string, error) {
	output, err := runGit(ctx, directory, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("find the branch MMHotline's copy of the systems repository has checked out: %w", err)
	}
	return strings.TrimSpace(output), nil
}

// fetchOrigin updates MMHotline's record of origin's branch and returns the
// commit the branch ends at.
func fetchOrigin(ctx context.Context, directory, branch string) (string, error) {
	tracking := "refs/remotes/origin/" + branch
	if _, err := runGit(ctx, directory, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "origin", "+refs/heads/"+branch+":"+tracking); err != nil {
		return "", fmt.Errorf("fetch %s from origin: %w", branch, err)
	}
	return commitID(ctx, directory, tracking)
}

// fastForwardWorkingTree moves MMHotline's checked-out branch and working tree
// to commit, which descends from the branch, only when the branch is still
// checked out and no tracked file has uncommitted changes. It returns why it
// left the working tree as it was, or "" when it moved it.
func fastForwardWorkingTree(ctx context.Context, directory, branch, commit string) string {
	if current, err := checkedOutBranch(ctx, directory); err != nil || current != branch {
		return fmt.Sprintf("MMHotline's copy of the systems repository no longer has branch %s checked out", branch)
	}
	// GIT_OPTIONAL_LOCKS=0 keeps a read from updating the index.
	changes, err := runGit(ctx, directory, []string{"GIT_OPTIONAL_LOCKS=0"}, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "MMHotline could not check the systems repository copy for uncommitted changes: " + err.Error()
	}
	if changes != "" {
		return "MMHotline's copy of the systems repository has uncommitted changes to tracked files"
	}
	if _, err := runGit(ctx, directory, nil, "merge", "--ff-only", "--quiet", commit); err != nil {
		return "MMHotline's copy of the systems repository could not be fast-forwarded: " + err.Error()
	}
	return ""
}

// datedNoteName is a triage session's note, such as
// 2026-11-03_disk_full_on_app002.md, named as the task prompt asks; other
// files in the folder, such as a README.md, are not notes.
var datedNoteName = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}_[^/]*\.md$`)

// NewestTriageAgentNote returns the path of the last session's note: the
// note in triage_agent_notes/ that the most recent commit of MMHotline's
// checkout, the trusted commit, added or changed, or "" when it has no note.
// MMHotline pushes the agent's notes itself and moves its checkout to them
// unless a commit waits for review, so the checkout has the last session's
// note; a note past a waiting commit, which the agent's copy lacks too, does
// not count. The date in a name says only the
// day, so two sessions of one day would sort by their words; the commits
// give the order. A note a later commit removed no longer counts, and when
// one commit added or changed several notes, the greatest name wins.
func NewestTriageAgentNote(ctx context.Context, directory string) (string, error) {
	branch, err := checkedOutBranch(ctx, directory)
	if err != nil {
		return "", err
	}
	tip := "refs/heads/" + branch
	output, err := runGit(ctx, directory, nil, "ls-tree", "-z", tip, "triage_agent_notes/")
	if err != nil {
		return "", fmt.Errorf("list the notes in MMHotline's copy of the systems repository: %w", err)
	}
	notes := map[string]bool{}
	for _, entry := range strings.Split(output, "\x00") {
		// "<mode> <type> <object>\t<path>"
		fields, path, found := strings.Cut(entry, "\t")
		if found && strings.Contains(fields, " blob ") && datedNoteName.MatchString(strings.TrimPrefix(path, "triage_agent_notes/")) {
			notes[path] = true
		}
	}
	if len(notes) == 0 {
		return "", nil
	}
	// Plumbing, newest commit first: no setting such as log.showSignature
	// changes the output. A merge lists no paths of its own; the commits it
	// merged follow it.
	commits, err := runGit(ctx, directory, nil, "rev-list", tip, "--", "triage_agent_notes/")
	if err != nil {
		return "", fmt.Errorf("list the commits that changed the notes in MMHotline's copy of the systems repository: %w", err)
	}
	for _, commit := range strings.Fields(commits) {
		paths, err := runGit(ctx, directory, nil, "diff-tree", "-z", "-r", "--root", "--no-commit-id", "--no-renames", "--diff-filter=AM", "--name-only", commit, "--", "triage_agent_notes/")
		if err != nil {
			return "", fmt.Errorf("list the notes commit %s changed in MMHotline's copy of the systems repository: %w", commit, err)
		}
		newest := ""
		for _, path := range strings.Split(paths, "\x00") {
			if notes[path] {
				newest = max(newest, path)
			}
		}
		if newest != "" {
			return newest, nil
		}
	}
	return "", nil
}

// FileAtCommit is path as commitID holds it in the copy at directory: the
// committed text, never a file outside the repository that a symlink names.
func FileAtCommit(ctx context.Context, directory, commitID, path string) (string, error) {
	return runGit(ctx, directory, nil, "cat-file", "blob", commitID+":"+path)
}

// FileAtCommitIfPresent is FileAtCommit for a file the commit may lack:
// found is false when commitID holds nothing at path. A symlink or a
// directory at path is an error, because neither is the file's text.
func FileAtCommitIfPresent(ctx context.Context, directory, commitID, path string) (text string, found bool, err error) {
	listing, err := runGit(ctx, directory, nil, "ls-tree", "-z", commitID, "--", path)
	if err != nil {
		return "", false, err
	}
	entry, _, _ := strings.Cut(listing, "\x00")
	if entry == "" {
		return "", false, nil
	}
	// "<mode> <type> <object>\t<path>"
	fields, _, _ := strings.Cut(entry, "\t")
	parts := strings.Fields(fields)
	if len(parts) != 3 || (parts[0] != "100644" && parts[0] != "100755") {
		return "", true, fmt.Errorf("%s at commit %s is not a regular file", path, commitID)
	}
	text, err = runGit(ctx, directory, nil, "cat-file", "blob", parts[2])
	return text, true, err
}

// ChangedPaths lists every path that differs between the trees of commits
// from and to in the copy at directory, added, changed, or deleted.
func ChangedPaths(ctx context.Context, directory, from, to string) ([]string, error) {
	// diff-tree, unlike git diff, ignores diff settings in the configuration.
	output, err := runGit(ctx, directory, nil, "diff-tree", "-r", "-z", "--no-renames", "--name-only", "--ignore-submodules=none", from, to)
	if err != nil {
		return nil, fmt.Errorf("list the changes from %s to %s: %w", from, to, err)
	}
	var paths []string
	for _, path := range strings.Split(output, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// SymlinksUnder lists the symlinks anywhere under folder in commitID's tree.
func SymlinksUnder(ctx context.Context, directory, commitID, folder string) ([]string, error) {
	output, err := runGit(ctx, directory, nil, "ls-tree", "-r", "-z", commitID, "--", folder+"/")
	if err != nil {
		return nil, fmt.Errorf("list %s/ at %s: %w", folder, commitID, err)
	}
	var symlinks []string
	for _, entry := range strings.Split(output, "\x00") {
		// "<mode> <type> <object>\t<path>"
		if fields, path, found := strings.Cut(entry, "\t"); found && strings.HasPrefix(fields, "120000 ") {
			symlinks = append(symlinks, path)
		}
	}
	return symlinks, nil
}

// Commit is one commit of MMHotline's copy, as the dashboard lists it.
type Commit struct {
	ID          string
	CommittedAt time.Time
	// Subject is the first line of the commit's message.
	Subject string
}

// LatestCommits returns at most count commits of the branch MMHotline's copy
// has checked out, newest first. MMHotline moves that branch only to the
// trusted commit, so it holds only commits that passed the commit check or
// that a person reviewed: a commit that waits stays on origin's branch, so
// it never shows as a change to the system. Reading
// refs and objects needs no lock, so this never waits for an MMX run.
func LatestCommits(ctx context.Context, directory string, count int) ([]Commit, error) {
	// rev-list is plumbing: no setting such as log.showSignature changes
	// its output.
	output, err := runGit(ctx, directory, nil, "rev-list", "--max-count="+strconv.Itoa(count), "--no-commit-header", "--format=%H %ct %s", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("list the commits of MMHotline's copy of the systems repository: %w", err)
	}
	var commits []Commit
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected git rev-list line %q", line)
		}
		seconds, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected git rev-list line %q", line)
		}
		commits = append(commits, Commit{ID: parts[0], CommittedAt: time.Unix(seconds, 0).UTC(), Subject: parts[2]})
	}
	return commits, nil
}

// ReadWorkingTree runs read on MMHotline's working tree and passes it the
// commit the files are: the trusted commit, where bringCheckoutForward
// last moved the checkout, which can be behind origin's branch while a
// commit waits for review. It holds the copy's lock, so no MMX run or deploy
// changes the files meanwhile. It refuses a working tree with uncommitted
// changes to tracked files, because read would then see files that no
// commit holds.
func ReadWorkingTree(ctx context.Context, directory string, read func(commitID string) error) error {
	return withCopyLock(ctx, directory, CopyLockOptions{}, func() error {
		if _, err := checkedOutBranch(ctx, directory); err != nil {
			return err
		}
		head, err := commitID(ctx, directory, "HEAD")
		if err != nil {
			return err
		}
		// GIT_OPTIONAL_LOCKS=0 keeps a read from updating the index.
		changes, err := runGit(ctx, directory, []string{"GIT_OPTIONAL_LOCKS=0"}, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			return fmt.Errorf("check MMHotline's copy of the systems repository for uncommitted changes: %w", err)
		}
		if changes != "" {
			return errors.New("MMHotline's copy of the systems repository has uncommitted changes")
		}
		return read(head)
	})
}

// WithCopyForMMX holds the copy's lock while it brings the checkout forward
// to the trusted commit and while run runs MMX. It fetches and fast-forwards
// as for the agent's commits, never past the trusted commit, and refuses
// uncommitted changes to tracked files, so MMX runs only files that are in
// Git and that the team trusts. run still decides whether a commit that
// waits on origin's branch stops MMX. It calls run only when the copy is then at
// judgedCommitID. When the copy moved past that commit, it returns the newer
// commit, and nothing ran; any other commit is an error.
func WithCopyForMMX(ctx context.Context, directory, judgedCommitID string, options CopyLockOptions, trusted TrustedCommit, run func() error) (newerCommitID string, err error) {
	err = withCopyLock(ctx, directory, options, func() error {
		if err := checkCommitID(judgedCommitID); err != nil {
			return err
		}
		branch, err := checkedOutBranch(ctx, directory)
		if err != nil {
			return err
		}
		origin, err := fetchOrigin(ctx, directory, branch)
		if err != nil {
			return err
		}
		reason, err := bringCheckoutForward(ctx, directory, branch, origin, trusted)
		if err != nil {
			return err
		}
		if reason != "" {
			return errors.New(reason)
		}
		head, err := commitID(ctx, directory, "HEAD")
		if err != nil {
			return err
		}
		if head == judgedCommitID {
			return run()
		}
		newer, err := IsAncestor(ctx, directory, judgedCommitID, head)
		if err != nil {
			return err
		}
		if !newer {
			return fmt.Errorf("MMHotline's copy of the systems repository is at %s, which is not the judged commit %s or a descendant", head, judgedCommitID)
		}
		newerCommitID = head
		return nil
	})
	return newerCommitID, err
}
