package systemsrepository

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// CommitSigningKeyFingerprint checks one OpenSSH public key line, the key
// type, the key, and an optional comment, and returns the fingerprint Git
// reports for SSH signatures. As in OpenSSH, the comment is everything after
// the key, spaces included, such as "util01, the triage agent".
func CommitSigningKeyFingerprint(line string) (string, error) {
	if strings.ContainsAny(line, "\r\n\x00") {
		return "", errors.New("must be one OpenSSH public key line, without a line break or a NUL character")
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", errors.New("must be one OpenSSH public key line: the key type, such as ssh-ed25519, then the key, then an optional comment")
	}
	keyType, key := fields[0], fields[1]
	switch keyType {
	case "ssh-ed25519", "ssh-rsa", "ssh-dss", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
	default:
		return "", fmt.Errorf("starts with %q, which is not an SSH key type: the line must start with a key type such as ssh-ed25519", keyType)
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(key)
	if err != nil {
		return "", fmt.Errorf("has a key that is not valid base64: the field after %s must be the key as ssh-keygen wrote it in the .pub file", keyType)
	}
	// The key is a run of strings, each after its 4-byte length: the key type,
	// then the key's own parts. A key cut short leaves a string incomplete or
	// has no parts, and would match no signature, so every commit would pass.
	var parts []string
	for rest := blob; len(rest) > 0; {
		if len(rest) < 4 || uint64(binary.BigEndian.Uint32(rest[:4])) > uint64(len(rest)-4) {
			parts = nil
			break
		}
		length := binary.BigEndian.Uint32(rest[:4])
		parts = append(parts, string(rest[4:4+length]))
		rest = rest[4+length:]
	}
	if len(parts) < 2 {
		return "", errors.New("has a key that is cut short: copy the whole line of the .pub file")
	}
	if parts[0] != keyType {
		return "", fmt.Errorf("says the key type is %s, but the key itself is a %q key", keyType, parts[0])
	}
	digest := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]), nil
}

// The reasons a commit waits for a person's review before any MMX run, and
// any triage agent run, goes past it.
const (
	// NotSignedByATrustedKey is a commit without a good signature by a key
	// the team trusts or by the triage agent's key: unsigned, signed by
	// another key, or with a signature Git cannot verify. Nobody the team
	// trusts made the commit, so it may come from a break-in.
	NotSignedByATrustedKey = "not_signed_by_a_trusted_key"
	// TriageAgentChangeNeedsReview is a commit signed by the triage agent's
	// key that changes paths outside its folders, or adds a file type there
	// that the push filter refuses.
	TriageAgentChangeNeedsReview = "triage_agent_change_needs_review"
)

// RefusedCommit holds the facts a person needs to review a commit that waits.
// Keeping the facts with the refusal preserves the review after a history
// change.
type RefusedCommit struct {
	ID, Author, Message string
	CommittedAt         time.Time
	// ReasonKind is NotSignedByATrustedKey or TriageAgentChangeNeedsReview.
	ReasonKind string
	// Paths are the triage agent's changes that need review. A commit no
	// trusted key signed lists none: every change it makes needs review.
	Paths []PathThatWaits
	// ChangedPaths are every path the commit changes, against each parent
	// of a merge, whole even when the diff is cut: the paths only a person
	// may approve are judged against them.
	ChangedPaths  []string
	Diff          string
	DiffTruncated bool
}

// MaximumCommitReviewDiffBytes keeps a large commit from overwhelming the review
// page and its stored record. Paths and reasons are always kept in full.
const MaximumCommitReviewDiffBytes = 128 << 10

// CommitSigningKeys are the public keys the check trusts, each one OpenSSH
// public key line: Trusted for the team's people and machines, such as each
// person's desk and util01, and TriageAgent for the triage agent, whose
// commits may change only its own folders.
type CommitSigningKeys struct {
	Trusted     []string
	TriageAgent string
}

// checkedRange is the revision range a check walks: LAST..target, or all of
// target's history when LAST is empty, missing, or neither an ancestor nor a
// descendant of target. A target LAST already holds has nothing to check,
// and behindLast says so. usedLast is the LAST the range starts after, or ""
// for the whole history.
func checkedRange(ctx context.Context, directory, last, target string) (revision, usedLast string, behindLast bool, err error) {
	if err := checkCommitID(target); err != nil {
		return "", "", false, err
	}
	if last == "" {
		return target, "", false, nil
	}
	if err := checkCommitID(last); err != nil {
		return "", "", false, err
	}
	// A removed object is also a lost checkpoint, so check the whole history.
	if _, err := commitID(ctx, directory, last); err != nil {
		return target, "", false, nil
	}
	if ancestor, err := IsAncestor(ctx, directory, last, target); err != nil || ancestor {
		return last + ".." + target, last, false, err
	}
	// Such as a check of an older commit: every commit through LAST passed.
	if behind, err := IsAncestor(ctx, directory, target, last); err != nil || behind {
		return last + ".." + target, last, behind, err
	}
	return target, "", false, nil
}

// CommitsThatNeedReview checks each commit of LAST..target, or all of
// target's history when LAST is missing or no longer an ancestor, roots and
// merges included. A good signature by a trusted key passes. A good signature
// by the triage agent's key passes when the push filter's path and file-type
// rules allow every change against every parent. Every other commit needs a
// person's review.
func CommitsThatNeedReview(ctx context.Context, directory, last, target string, keys CommitSigningKeys) ([]RefusedCommit, error) {
	revision, _, _, err := checkedRange(ctx, directory, last, target)
	if err != nil {
		return nil, err
	}
	// The fingerprint Git reports with a good signature says whose key signed.
	triageAgentFingerprint, err := CommitSigningKeyFingerprint(keys.TriageAgent)
	if err != nil {
		return nil, err
	}
	allowedSigners := "triage-agent namespaces=\"git\" " + strings.TrimSpace(keys.TriageAgent) + "\n"
	trusted := map[string]bool{}
	for _, key := range keys.Trusted {
		fingerprint, err := CommitSigningKeyFingerprint(key)
		if err != nil {
			return nil, err
		}
		trusted[fingerprint] = true
		allowedSigners += "trusted namespaces=\"git\" " + strings.TrimSpace(key) + "\n"
	}
	signers, err := os.CreateTemp("", "mmhotline-commit-signers-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(signers.Name()) // private temporary file, never repository data
	_, writeErr := signers.WriteString(allowedSigners)
	closeErr := signers.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	output, err := runGit(ctx, directory, nil, "-c", "gpg.ssh.allowedSignersFile="+signers.Name(), "-c", "gpg.ssh.program=ssh-keygen", "-c", "log.showSignature=false", "log", "--no-decorate", "--no-show-signature", "--no-notes", "--no-patch", "-z", "--format=%H%x00%G?%x00%GF", revision, "--")
	if err != nil {
		return nil, fmt.Errorf("check the signatures of systems repository commits: %w", err)
	}
	if output == "" {
		return nil, nil
	}
	fields := strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("Git returned an unreadable commit signature list")
	}
	var refused []RefusedCommit
	for index := 0; index < len(fields); index += 3 {
		id, good, fingerprint := fields[index], fields[index+1] == "G", fields[index+2]
		commit := RefusedCommit{ID: id, ReasonKind: NotSignedByATrustedKey}
		if good && trusted[fingerprint] {
			continue
		}
		changes, err := commitChanges(ctx, directory, id)
		if err != nil {
			return nil, err
		}
		if good && fingerprint == triageAgentFingerprint {
			if commit.Paths, err = pathsThatWait(changes); err != nil {
				return nil, err
			}
			if len(commit.Paths) == 0 {
				continue
			}
			commit.ReasonKind = TriageAgentChangeNeedsReview
		}
		if commit.ChangedPaths, err = pathsOfChanges(changes); err != nil {
			return nil, err
		}
		if err := readCommitFacts(ctx, directory, &commit); err != nil {
			return nil, err
		}
		refused = append(refused, commit)
	}
	return refused, nil
}

// readCommitFacts adds the author, commit date, message, and diff that a
// person reads on the review page.
func readCommitFacts(ctx context.Context, directory string, commit *RefusedCommit) error {
	facts, err := runGit(ctx, directory, nil, "show", "--no-patch", "--no-show-signature", "--no-notes", "--format=%an <%ae>%x00%cI%x00%B", commit.ID, "--")
	if err != nil {
		return err
	}
	parts := strings.SplitN(facts, "\x00", 3)
	if len(parts) != 3 {
		return fmt.Errorf("Git returned unreadable facts for commit %s", commit.ID)
	}
	if commit.CommittedAt, err = time.Parse(time.RFC3339, parts[1]); err != nil {
		return err
	}
	commit.Author, commit.Message = parts[0], strings.TrimSpace(parts[2])
	diff, err := runGit(ctx, directory, nil, "diff-tree", "--root", "-r", "-m", "--no-commit-id", "--no-renames", "--ignore-submodules=none", "--no-ext-diff", "--no-textconv", "--no-color", "-p", commit.ID, "--")
	if err != nil {
		return err
	}
	commit.DiffTruncated = len(diff) > MaximumCommitReviewDiffBytes
	if commit.DiffTruncated {
		diff = strings.ToValidUTF8(diff[:MaximumCommitReviewDiffBytes], "�")
	}
	commit.Diff = diff
	return nil
}

// TrustedThrough returns the newest commit on target's first-parent line
// whose history after LAST holds none of the waiting commits, so every
// commit through it passed the check or a person reviewed it. A merge that
// brings in a waiting commit holds it too. TrustedThrough returns target when
// nothing waits, LAST when the first commit after LAST holds a waiting
// commit, and "" when the whole history was checked and its first commit
// holds one.
//
// checkpoint is the commit the checked-through commit may become: the
// trusted commit, or LAST when target is older than LAST, so a check of an
// older commit never moves the checkpoint back.
func TrustedThrough(ctx context.Context, directory, last, target string, waiting []string) (trusted, checkpoint string, err error) {
	revision, usedLast, behindLast, err := checkedRange(ctx, directory, last, target)
	if err != nil {
		return "", "", err
	}
	if behindLast {
		return target, last, nil
	}
	if len(waiting) == 0 {
		return target, target, nil
	}
	// Each waiting commit and every commit after it, through target.
	holdsAWaitingCommit := map[string]bool{}
	for _, commit := range waiting {
		if err := checkCommitID(commit); err != nil {
			return "", "", err
		}
		holdsAWaitingCommit[commit] = true
		after, err := runGit(ctx, directory, nil, "rev-list", "--ancestry-path", commit+".."+target, "--")
		if err != nil {
			return "", "", fmt.Errorf("list the commits after waiting commit %s: %w", commit, err)
		}
		for _, descendant := range strings.Fields(after) {
			holdsAWaitingCommit[descendant] = true
		}
	}
	line, err := runGit(ctx, directory, nil, "rev-list", "--first-parent", "--reverse", revision, "--")
	if err != nil {
		return "", "", fmt.Errorf("list the first-parent commits through %s: %w", target, err)
	}
	newest := usedLast
	for _, commit := range strings.Fields(line) {
		if holdsAWaitingCommit[commit] {
			break
		}
		newest = commit
	}
	return newest, newest, nil
}

// HasCommit says whether the copy at directory holds commitID, a full commit ID.
func HasCommit(ctx context.Context, directory, commitID string) (bool, error) {
	if err := checkCommitID(commitID); err != nil {
		return false, err
	}
	_, err := runGit(ctx, directory, nil, "cat-file", "-e", commitID+"^{commit}")
	if exitCode(err) > 0 {
		return false, nil
	}
	return err == nil, err
}

// pathsOfChanges lists each path of git diff-tree -z output once, in Git's
// order.
func pathsOfChanges(rawChanges string) ([]string, error) {
	paths := []string{}
	if rawChanges == "" {
		return paths, nil
	}
	fields := strings.Split(strings.TrimSuffix(rawChanges, "\x00"), "\x00")
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("unexpected git diff-tree output %q", rawChanges)
	}
	for index := 1; index < len(fields); index += 2 {
		if !slices.Contains(paths, fields[index]) {
			paths = append(paths, fields[index])
		}
	}
	return paths, nil
}

// commitChanges includes roots and each parent of a merge. Both the push
// filter and the pre-MMX check must judge the same changes.
func commitChanges(ctx context.Context, directory, commit string) (string, error) {
	return runGit(ctx, directory, nil, "diff-tree", "--root", "-r", "-z", "-m", "--no-commit-id", "--no-renames", "--ignore-submodules=none", commit, "--")
}
