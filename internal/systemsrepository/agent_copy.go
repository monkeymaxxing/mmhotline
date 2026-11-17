package systemsrepository

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// agentCopyEnvironment keeps MMHotline's Git in the agent's copy from reading
// the agent's ~/.gitconfig, ~/.config/git/config, and ~/.config/git/attributes.
// A misled run can write them, and a setting there names a program, such as a
// smudge filter or core.fsmonitor, that Git would run while MMHotline prepares
// a later run's copy: before a commit review's coding agent starts
// (docs/changing-mmhotline/design/triage-agent.md#rules-for-the-git-commands).
// The agent's own Git, which the coding agent runs, still reads ~/.gitconfig
// for the agent's commit name, email address, and signing key.
var agentCopyEnvironment = []string{
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.attributesFile", "GIT_CONFIG_VALUE_0=/dev/null",
}

// PrepareAgentCopy makes the agent's copy at directory new, at base, the
// commit BundleThrough bundled, with a clean working tree. It removes an
// earlier run's copy first, its .git directory included, so no Git setting,
// attribute, or commit an earlier run left reaches this run, and every run
// starts from MMHotline's main branch. The bundle holds the whole history. It
// runs inside the sandbox as the agent's user. The agent's commit name, email
// address, and signing key come from its ~/.gitconfig, which the team's server
// setup writes; the copy's own configuration sets none of them.
func PrepareAgentCopy(ctx context.Context, directory string, bundle []byte, baseCommitID string) error {
	if err := checkCommitID(baseCommitID); err != nil {
		return err
	}
	if err := checkBundleSize(int64(len(bundle))); err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove the earlier run's copy: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path, remove, err := temporaryBundleFile(bundle)
	if err != nil {
		return err
	}
	defer remove()
	for _, step := range []struct {
		doing string
		args  []string
	}{
		{"create the agent's copy", []string{"init", "--quiet"}},
		// unbundle stores the objects without a ref; the checkout names the base.
		{"read MMHotline's bundle", []string{"bundle", "unbundle", path}},
		{"check out the base commit", []string{"checkout", "--quiet", "-B", "main", baseCommitID}},
	} {
		if _, err := runGit(ctx, directory, agentCopyEnvironment, step.args...); err != nil {
			return fmt.Errorf("%s: %w", step.doing, err)
		}
	}
	return nil
}

// AgentCommitsBundle returns a bundle of the commits on the agent's main
// branch after base, or nil when there are none. Uncommitted changes are not
// sent: the agent shares only what it committed.
func AgentCommitsBundle(ctx context.Context, directory, baseCommitID string) ([]byte, error) {
	if err := checkCommitID(baseCommitID); err != nil {
		return nil, err
	}
	commits := baseCommitID + "..refs/heads/main"
	count, err := runGit(ctx, directory, agentCopyEnvironment, "rev-list", "--count", commits)
	if err != nil {
		return nil, fmt.Errorf("count the agent's commits: %w", err)
	}
	if strings.TrimSpace(count) == "0" {
		return nil, nil
	}
	bundle, err := createBundle(ctx, directory, agentCopyEnvironment, commits)
	if err != nil {
		return nil, fmt.Errorf("bundle the agent's commits: %w", err)
	}
	return bundle, nil
}
