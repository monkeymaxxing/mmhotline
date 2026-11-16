package systemsrepository

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// PrepareAgentCopy makes the agent's copy at directory start exactly at base,
// the commit BundleThrough bundled, with a clean working tree. It
// creates the copy when it is missing and discards what an earlier run did not
// share, so every run starts from MMHotline's main branch. It runs inside the
// sandbox as the agent's user. The agent's commit name, email address, and
// signing key come from its ~/.gitconfig, which the team's server setup
// writes; the copy's own configuration sets none of them.
func PrepareAgentCopy(ctx context.Context, directory string, bundle []byte, baseCommitID string) error {
	if err := checkCommitID(baseCommitID); err != nil {
		return err
	}
	if err := checkBundleSize(int64(len(bundle))); err != nil {
		return err
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
		// --force discards changes to tracked files and any merge in progress.
		{"check out the base commit", []string{"checkout", "--quiet", "--force", "-B", "main", baseCommitID}},
		// -ff also removes repositories the agent cloned inside the copy.
		{"remove files the base commit does not have", []string{"clean", "--quiet", "-ffd"}},
	} {
		if _, err := runGit(ctx, directory, nil, step.args...); err != nil {
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
	count, err := runGit(ctx, directory, nil, "rev-list", "--count", commits)
	if err != nil {
		return nil, fmt.Errorf("count the agent's commits: %w", err)
	}
	if strings.TrimSpace(count) == "0" {
		return nil, nil
	}
	bundle, err := createBundle(ctx, directory, commits)
	if err != nil {
		return nil, fmt.Errorf("bundle the agent's commits: %w", err)
	}
	return bundle, nil
}
