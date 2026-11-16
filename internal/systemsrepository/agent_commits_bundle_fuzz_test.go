package systemsrepository

import (
	"sort"
	"strings"
	"testing"
)

// fuzzedEntryModes are the Git modes a fuzzed tree entry can have; a
// record's first byte picks one by its remainder. Deletion removes the path,
// and the empty tree is a folder with nothing in it.
var fuzzedEntryModes = []string{"100644", "100755", "120000", "160000", "deletion", "040000"}

const emptyTreeID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// treeEntry is one entry of `git ls-tree -r`.
type treeEntry struct{ mode, kind, object string }

// FuzzAgentCommitsBundle runs a run's end on a new systems repository: the
// agent commits a tree built from fuzzed entries on top of the base, any
// paths and modes Git's own tree objects can hold, the sandbox bundles the
// commit, a fuzzed byte of the bundle may change on the way, and MMHotline
// receives it. Origin's main moves only when MMHotline pushed, and then to
// the commit it names; a push changes nothing outside one_time_playbooks/
// and triage_agent_notes/, holds only regular files there, and no path part
// that is empty, ".", "..", or .git in any case
// (docs/changing-mmhotline/design/triage-agent.md#the-agents-commits). Commits that change only
// regular files in those two folders are pushed.
//
// A record of entries is a mode byte and a path, and records are separated
// by NUL, which no Git path holds.
func FuzzAgentCommitsBundle(f *testing.F) {
	for _, seed := range []struct {
		entries  string
		damageAt uint32
	}{
		{"0" + playbookPath + "\x000" + notePath, 0},
		{"0" + playbookPath + "\x000" + notePath, 10},
		{"4" + earlierNote, 0},
		{"1one_time_playbooks/clean.sh", 0},
		{"2one_time_playbooks/link", 0},
		{"3one_time_playbooks/tool", 0},
		{"0run_mmx", 0},
		{"4run_mmx", 0},
		{"0one_time_playbooks/../run_mmx", 0},
		{"0triage_agent_notes/.Git/config", 0},
		{"5one_time_playbooks/empty", 0},
		{"0one_time_playbooks", 0},
	} {
		f.Add(seed.entries, seed.damageAt, byte('x'))
	}
	f.Fuzz(func(t *testing.T, entries string, damageAt uint32, damageTo byte) {
		records := strings.Split(entries, "\x00")
		if len(records) > 8 || len(entries) > 2000 {
			t.Skip("more entries than a run's commit needs to show a fault")
		}
		r := setUpRepositories(t)
		base := r.startRun(t)
		tree := listTree(t, r.agent, base)
		blob := gitWithInput(t, r.agent, "fuzzed content\n", "hash-object", "-w", "--stdin")
		allowed := true
		for _, record := range records {
			if record == "" {
				continue
			}
			mode, path := fuzzedEntryModes[int(record[0])%len(fuzzedEntryModes)], record[1:]
			allowed = allowed && (mode == "100644" || mode == "deletion") && mayBePushed(path)
			switch mode {
			case "deletion":
				delete(tree, path)
			case "160000":
				tree[path] = treeEntry{mode, "commit", base}
			case "040000":
				tree[path] = treeEntry{mode, "tree", emptyTreeID}
			default:
				tree[path] = treeEntry{mode, "blob", blob}
			}
		}
		top, ok := makeTree(t, r.agent, tree)
		if !ok {
			t.Skip("Git cannot hold these entries in a tree")
		}
		commit := git(t, r.agent, "commit-tree", top, "-p", base, "-m", "Fuzzed commit")
		git(t, r.agent, "update-ref", "refs/heads/main", commit)
		bundle, err := AgentCommitsBundle(t.Context(), r.agent, base)
		switch {
		case err != nil && !allowed:
			// Git refuses to pack some trees, such as one with an empty
			// name; the sandbox then fails the run with Git's reason, and
			// nothing reaches MMHotline.
			t.Skip("Git refuses to bundle these entries")
		case err != nil || len(bundle) == 0:
			t.Fatalf("bundle: %d bytes, %v", len(bundle), err)
		}
		damaged := false
		if damageAt != 0 {
			index := int(damageAt % uint32(len(bundle)))
			damaged = bundle[index] != damageTo
			bundle[index] = damageTo
		}

		result, err := ReceiveAgentCommits(t.Context(), r.mmhotline, bundle, base, originIsTrusted)
		main := r.originMain(t)
		pushed := err == nil && result.Outcome == Pushed
		switch {
		case err != nil && !damaged:
			t.Fatalf("receiving an undamaged bundle failed: %v", err)
		case pushed && main != result.PushedCommitID:
			t.Fatalf("origin's main is %s, but MMHotline pushed %s", main, result.PushedCommitID)
		case !pushed && main != base:
			t.Fatalf("origin's main moved to %s without a push: %+v, %v", main, result, err)
		case err == nil && !pushed && (result.Reason == "" || result.Outcome != WaitsForAPerson && result.Outcome != Refused):
			t.Fatalf("result: %+v", result)
		case !pushed && allowed && !damaged:
			t.Fatalf("commits that change only regular files in the agent's folders were not pushed: %+v", result)
		}
		if !pushed {
			return
		}
		before, after := listTree(t, r.origin, base), listTree(t, r.origin, main)
		for path := range changedPaths(before, after) {
			if entry, present := after[path]; !mayBePushed(path) || present && entry.mode != "100644" {
				t.Fatalf("pushed %q as %+v with the entries %q", path, entry, entries)
			}
		}
	})
}

// mayBePushed is the rule for a path the agent's commits may change without
// a person.
func mayBePushed(path string) bool {
	if !strings.HasPrefix(path, "one_time_playbooks/") && !strings.HasPrefix(path, "triage_agent_notes/") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// listTree reads every file, link, and submodule of a commit, by path.
func listTree(t *testing.T, directory, commit string) map[string]treeEntry {
	t.Helper()
	entries := map[string]treeEntry{}
	listing, err := runTestGit(directory, "", "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(listing, "\x00"), "\x00") {
		if line == "" {
			continue
		}
		description, path, _ := strings.Cut(line, "\t")
		fields := strings.Fields(description)
		entries[path] = treeEntry{fields[0], fields[1], fields[2]}
	}
	return entries
}

func changedPaths(before, after map[string]treeEntry) map[string]bool {
	changed := map[string]bool{}
	for path, entry := range before {
		if after[path] != entry {
			changed[path] = true
		}
	}
	for path, entry := range after {
		if before[path] != entry {
			changed[path] = true
		}
	}
	return changed
}

// makeTree writes the tree of entries, folder by folder, with git mktree,
// which takes any name without a slash. It is not ok when a path is both a
// file and a folder or Git refuses a name.
func makeTree(t *testing.T, directory string, entries map[string]treeEntry) (string, bool) {
	t.Helper()
	here := map[string]treeEntry{}
	below := map[string]map[string]treeEntry{}
	for path, entry := range entries {
		name, rest, inFolder := strings.Cut(path, "/")
		if !inFolder {
			here[name] = entry
			continue
		}
		if below[name] == nil {
			below[name] = map[string]treeEntry{}
		}
		below[name][rest] = entry
	}
	for name, folder := range below {
		if _, clash := here[name]; clash {
			return "", false
		}
		tree, ok := makeTree(t, directory, folder)
		if !ok {
			return "", false
		}
		here[name] = treeEntry{"040000", "tree", tree}
	}
	names := make([]string, 0, len(here))
	for name := range here {
		names = append(names, name)
	}
	sort.Strings(names)
	var input strings.Builder
	for _, name := range names {
		entry := here[name]
		input.WriteString(entry.mode + " " + entry.kind + " " + entry.object + "\t" + name + "\x00")
	}
	tree, err := runTestGit(directory, input.String(), "mktree", "-z")
	return strings.TrimSpace(tree), err == nil
}
