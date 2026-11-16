package systemsrepository

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func signingKey(t *testing.T, root, name string) string {
	t.Helper()
	key := filepath.Join(root, name)
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, output)
	}
	return key
}

func publicKeyLine(t *testing.T, key string) string {
	t.Helper()
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(public))
}

// signWith makes the clone at directory sign its next commits with key, or
// sign nothing when key is "".
func signWith(t *testing.T, directory, key string) {
	t.Helper()
	if key == "" {
		git(t, directory, "config", "commit.gpgsign", "false")
		return
	}
	git(t, directory, "config", "gpg.format", "ssh")
	git(t, directory, "config", "user.signingkey", key)
	git(t, directory, "config", "commit.gpgsign", "true")
}

// commitKeys are a test's keys: one the team trusts, the triage agent's,
// and one nobody listed.
type commitKeys struct {
	trusted, triageAgent, unknown string
	CommitSigningKeys
}

func newCommitKeys(t *testing.T, root string) commitKeys {
	t.Helper()
	k := commitKeys{trusted: signingKey(t, root, "trusted-key"), triageAgent: signingKey(t, root, "triage-key"), unknown: signingKey(t, root, "unknown-key")}
	k.CommitSigningKeys = CommitSigningKeys{Trusted: []string{publicKeyLine(t, k.trusted)}, TriageAgent: publicKeyLine(t, k.triageAgent)}
	return k
}

func TestCommitsThatNeedReview(t *testing.T) {
	for _, test := range []struct {
		kind, waits string
	}{
		{"trusted key", ""},
		{"triage agent's notes", ""},
		{"triage agent outside its folders", TriageAgentChangeNeedsReview},
		{"triage agent's root", TriageAgentChangeNeedsReview},
		{"triage agent's merge of an outside change", TriageAgentChangeNeedsReview},
		{"unsigned", NotSignedByATrustedKey},
		{"unknown key", NotSignedByATrustedKey},
	} {
		t.Run(test.kind, func(t *testing.T) {
			r := setUpRepositories(t)
			keys := newCommitKeys(t, r.root)
			base := git(t, r.person, "rev-parse", "HEAD")
			signing := map[string]string{"trusted key": keys.trusted, "unsigned": "", "unknown key": keys.unknown}
			key, found := signing[test.kind]
			if !found {
				key = keys.triageAgent
			}
			path := "roles/x.yml"
			switch test.kind {
			case "triage agent's root":
				git(t, r.person, "switch", "--orphan", "new-root")
			case "triage agent's merge of an outside change":
				// A trusted person's change, which passes, merged by the
				// triage agent: the merge itself changes roles/x.yml.
				signWith(t, r.person, keys.trusted)
				git(t, r.person, "switch", "-c", "outside")
				writeFile(t, r.person, path, "outside\n", 0o644)
				commitAll(t, r.person, "Outside change")
				git(t, r.person, "switch", "main")
				path = notePath
			case "triage agent's notes":
				path = notePath
			}
			writeFile(t, r.person, path, "outside\n", 0o644)
			signWith(t, r.person, key)
			commitAll(t, r.person, "Change servers")
			if test.kind == "triage agent's merge of an outside change" {
				git(t, r.person, "merge", "--no-ff", "outside", "-m", "Triage merge")
			}
			tip := git(t, r.person, "rev-parse", "HEAD")
			waiting, err := CommitsThatNeedReview(t.Context(), r.person, base, tip, keys.CommitSigningKeys)
			if err != nil {
				t.Fatal(err)
			}
			if test.waits == "" {
				if len(waiting) != 0 {
					t.Fatalf("unexpected refusal: %+v", waiting)
				}
				return
			}
			if len(waiting) != 1 || waiting[0].ID != tip || waiting[0].ReasonKind != test.waits || waiting[0].Author != "Barbara <barbara@example.com>" || !strings.Contains(waiting[0].Diff, "outside") {
				t.Fatalf("waiting: %+v", waiting)
			}
			// The triage agent's commits name the paths that need review; a
			// commit nobody trusted made has all of its changes in review.
			if wantPaths := test.waits == TriageAgentChangeNeedsReview; wantPaths != (len(waiting[0].Paths) > 0 && waiting[0].Paths[0].Path == "roles/x.yml") {
				t.Fatalf("paths: %+v", waiting[0].Paths)
			}
			// Every waiting commit names each path it changes once, a merge
			// against each parent, for the paths only a person may approve.
			changed := waiting[0].ChangedPaths
			if !slices.Contains(changed, "roles/x.yml") || len(slices.Compact(slices.Sorted(slices.Values(changed)))) != len(changed) {
				t.Fatalf("changed paths: %q", changed)
			}
			// Lost or unrelated checkpoints must not hide the waiting commit.
			// The whole history then also holds the fixture's unsigned first
			// commit, except after the triage agent's new root.
			for _, last := range []string{"", strings.Repeat("a", 40)} {
				checked, err := CommitsThatNeedReview(t.Context(), r.person, last, tip, keys.CommitSigningKeys)
				if err != nil || len(checked) == 0 || checked[0].ID != tip {
					t.Fatalf("checkpoint %q: %+v, %v", last, checked, err)
				}
			}
			checked, err := CommitsThatNeedReview(t.Context(), r.person, tip, tip, keys.CommitSigningKeys)
			if err != nil || len(checked) != 0 {
				t.Fatalf("already checked: %+v, %v", checked, err)
			}
		})
	}
}

// A trusted person merges a branch that holds a commit nobody trusted made.
// The merge passes, the unsigned commit waits, and the trusted state ends at
// the commit before the merge on the first-parent line, so it holds none of
// the unsigned commit.
func TestATrustedMergeOfAnUnsignedCommitIsTrustedOnlyBeforeTheMerge(t *testing.T) {
	r := setUpRepositories(t)
	keys := newCommitKeys(t, r.root)
	base := git(t, r.person, "rev-parse", "HEAD")
	signWith(t, r.person, keys.trusted)
	writeFile(t, r.person, "roles/a.yml", "first\n", 0o644)
	beforeMerge := commitAll(t, r.person, "A trusted change")
	git(t, r.person, "switch", "-c", "contribution")
	signWith(t, r.person, "")
	writeFile(t, r.person, "roles/b.yml", "unsigned\n", 0o644)
	unsigned := commitAll(t, r.person, "A change nobody signed")
	git(t, r.person, "switch", "main")
	signWith(t, r.person, keys.trusted)
	git(t, r.person, "merge", "--no-ff", "contribution", "-m", "Merge the contribution")
	writeFile(t, r.person, "roles/c.yml", "after\n", 0o644)
	tip := commitAll(t, r.person, "A trusted change after the merge")

	waiting, err := CommitsThatNeedReview(t.Context(), r.person, base, tip, keys.CommitSigningKeys)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 || waiting[0].ID != unsigned || waiting[0].ReasonKind != NotSignedByATrustedKey {
		t.Fatalf("waiting: %+v", waiting)
	}
	for _, test := range []struct {
		last, target, want, checkpoint string
		waiting                        []string
	}{
		{base, tip, beforeMerge, beforeMerge, []string{unsigned}},
		// Once a person reviewed the commit, nothing waits.
		{base, tip, tip, tip, nil},
		// The first commit after the checkpoint holds the waiting commit.
		{beforeMerge, tip, beforeMerge, beforeMerge, []string{unsigned}},
		// An older commit than the checkpoint passed already; the
		// checkpoint stays where it is.
		{tip, beforeMerge, beforeMerge, tip, nil},
		// A first commit that waits leaves no trusted commit in a history
		// checked whole.
		{"", tip, "", "", []string{git(t, r.person, "rev-list", "--max-parents=0", "HEAD")}},
	} {
		got, checkpoint, err := TrustedThrough(t.Context(), r.person, test.last, test.target, test.waiting)
		if err != nil || got != test.want || checkpoint != test.checkpoint {
			t.Errorf("from %q to %s with %v waiting: %q and checkpoint %q, %v; want %q and %q", test.last, test.target, test.waiting, got, checkpoint, err, test.want, test.checkpoint)
		}
	}
	if waiting, err := CommitsThatNeedReview(t.Context(), r.person, tip, beforeMerge, keys.CommitSigningKeys); err != nil || len(waiting) != 0 {
		t.Fatalf("an older commit was checked again: %+v, %v", waiting, err)
	}
}

func TestCommitSigningKeyFingerprint(t *testing.T) {
	root := t.TempDir()
	key := signingKey(t, root, "triage-key")
	publicKey, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(publicKey))
	fingerprint, err := CommitSigningKeyFingerprint(line)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("ssh-keygen", "-l", "-f", key+".pub").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), fingerprint) {
		t.Fatalf("fingerprint %s differs from ssh-keygen: %s", fingerprint, output)
	}
	// OpenSSH's comment is everything after the key, spaces included, or
	// nothing; the comment never changes the fingerprint.
	keyType, encodedKey := strings.Fields(line)[0], strings.Fields(line)[1]
	for _, valid := range []string{keyType + " " + encodedKey + " util01, the triage agent", keyType + " " + encodedKey} {
		if got, err := CommitSigningKeyFingerprint(valid); err != nil || got != fingerprint {
			t.Errorf("%q: fingerprint %q, %v; want %s", valid, got, err, fingerprint)
		}
	}
	// The message names the part of the line that is wrong.
	for _, test := range []struct{ line, says string }{
		{"", "the key type, such as ssh-ed25519, then the key"},
		{line + "\n" + line, "without a line break"},
		{"unknown " + encodedKey, `starts with "unknown", which is not an SSH key type`},
		{"ssh-ed25519 !!!", "not valid base64"},
		{"ssh-ed25519 AAAA", "cut short"},
		{keyType + " " + encodedKey[:20], "cut short"},
		{keyType + " " + encodedKey[:len(encodedKey)-4], "cut short"},
		{strings.Replace(line, "ssh-ed25519", "ssh-rsa", 1), `says the key type is ssh-rsa, but the key itself is a "ssh-ed25519" key`},
	} {
		if _, err := CommitSigningKeyFingerprint(test.line); err == nil || !strings.Contains(err.Error(), test.says) {
			t.Errorf("%q: %v; want an error that says %q", test.line, err, test.says)
		}
	}
}

func TestCommitReviewDiffIsCutWithoutLosingPaths(t *testing.T) {
	r := setUpRepositories(t)
	keys := newCommitKeys(t, r.root)
	base := git(t, r.person, "rev-parse", "HEAD")
	signWith(t, r.person, keys.triageAgent)
	writeFile(t, r.person, "roles/x.yml", strings.Repeat("long change\n", 14000), 0o644)
	writeFile(t, r.person, "roles/y.yml", "another outside path\n", 0o644)
	commit := commitAll(t, r.person, "Large outside change")
	refused, err := CommitsThatNeedReview(t.Context(), r.person, base, commit, keys.CommitSigningKeys)
	if err != nil {
		t.Fatal(err)
	}
	if len(refused) != 1 || !refused[0].DiffTruncated || len(refused[0].Diff) > MaximumCommitReviewDiffBytes || len(refused[0].Paths) != 2 {
		t.Fatalf("large diff lost the complete review: %d commits", len(refused))
	}
}
