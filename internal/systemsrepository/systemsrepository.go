// Package systemsrepository moves commits between MMHotline's copy of the
// team's systems repository and the triage agent's copy as Git bundles, and
// pushes the agent's commits to the team's Git host only when they change
// nothing outside the agent's two folders:
// docs/changing-mmhotline/design/triage-agent.md#the-systems-repository-copies.
package systemsrepository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DirectoryName names both copies: <state directory>/systems_repository is
// MMHotline's, and <agent home>/systems_repository is the agent's.
const DirectoryName = "systems_repository"

// MaximumBundleSize bounds every bundle, because one JSON line on the run
// socket carries it; a systems repository is text and far smaller.
const MaximumBundleSize = 64 << 20

// Outcome is what MMHotline did with the agent's commits from one run.
type Outcome string

const (
	// Pushed: the commits are on origin's branch, merged with newer commits
	// there when origin moved during the run.
	Pushed Outcome = "pushed"
	// WaitsForAPerson: the commits change something only a person may approve,
	// or they conflict with newer commits on origin. They stay under PrivateRef.
	WaitsForAPerson Outcome = "waits_for_a_person"
	// Refused: the bundle is unusable, or its commits do not start from the
	// commit the run started at.
	Refused Outcome = "refused"
)

// ReceivedAgentCommits is what ReceiveAgentCommits did with one run's commits.
type ReceivedAgentCommits struct {
	Outcome Outcome
	// Reason says why the commits wait for a person or were refused.
	Reason string
	// PathsThatWait lists each changed path that needs a person, with why.
	PathsThatWait []PathThatWaits
	// AgentCommitID is the last commit of the agent's main branch.
	AgentCommitID string
	// PushedCommitID is what origin's branch now ends at: the agent's commit,
	// or MMHotline's merge of it with newer commits on origin.
	PushedCommitID string
	// PrivateRef holds the agent's commits in MMHotline's copy, such as
	// refs/mmhotline/agent-commits/0f3a9c2e5b7d1a48, for a person to look at.
	// It is empty after a push, which removes it.
	PrivateRef string
	// WorkingTreeNotUpdatedBecause says why MMHotline's working tree was left
	// behind origin after a push; it is empty when the working tree moved.
	WorkingTreeNotUpdatedBecause string
}

// PathThatWaits is one changed path in the agent's commits that needs a person.
type PathThatWaits struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// gitCommandTimeLimit ends a Git command that hangs, such as a fetch from a
// Git host that stopped answering, so it cannot hold the one triage run.
const gitCommandTimeLimit = 2 * time.Minute

var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func checkCommitID(commitID string) error {
	if !commitIDPattern.MatchString(commitID) {
		return fmt.Errorf("%q is not a full Git commit ID", commitID)
	}
	return nil
}

func checkBundleSize(size int64) error {
	if size > MaximumBundleSize {
		return fmt.Errorf("the systems repository Git bundle is %d bytes, over the limit of %d bytes (64 MiB)", size, MaximumBundleSize)
	}
	return nil
}

// gitError is a failed Git command with Git's own message.
type gitError struct {
	command  string // such as "git fetch"
	exitCode int    // -1 when Git did not start or did not finish
	message  string
}

func (failure *gitError) Error() string { return failure.command + ": " + failure.message }

func exitCode(err error) int {
	var failure *gitError
	if errors.As(err, &failure) {
		return failure.exitCode
	}
	return -1
}

// runGit runs git in a copy's directory and returns its standard output.
// environment adds variables, such as a commit's identity.
//
// Every command runs with core.hooksPath=/dev/null, so no hook runs, the push
// included. In MMHotline's copy a hook runs as the mmhotline user, and nothing
// of the team's should run while MMHotline handles the agent's commits; a push
// runs pre-push and reference-transaction there. The Git host's own hooks
// still run. In the agent's copy, a hook an earlier run left could change the
// copy after MMHotline reset it.
func runGit(ctx context.Context, directory string, environment []string, args ...string) (string, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, gitCommandTimeLimit)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	command.Dir = directory
	command.Env = append(gitEnvironment(directory), environment...)
	// A killed git can leave ssh holding its output open.
	command.WaitDelay = 5 * time.Second
	var output, errorOutput bytes.Buffer
	command.Stdout, command.Stderr = &output, &errorOutput
	err = command.Run()
	if err == nil {
		return output.String(), nil
	}
	failure := &gitError{command: "git " + args[0], exitCode: -1, message: strings.TrimSpace(errorOutput.String())} //nolint:nilaway // Every caller names a git subcommand.
	var exitError *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		failure.message = fmt.Sprintf("did not finish within %s", gitCommandTimeLimit)
	case ctx.Err() != nil:
		failure.message = ctx.Err().Error()
	case errors.As(err, &exitError):
		failure.exitCode = exitError.ExitCode()
	}
	if failure.message == "" {
		failure.message = err.Error()
	}
	return output.String(), failure
}

// gitEnvironment is the process's environment without its Git variables, so
// an inherited GIT_DIR or GIT_INDEX_FILE cannot point a command at another
// repository. Git never prompts for a credential, ignores the server-wide
// /etc/gitconfig, and never takes a repository above the copy for the copy.
func gitEnvironment(directory string) []string {
	var environment []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			environment = append(environment, variable)
		}
	}
	return append(environment, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES="+filepath.Dir(directory))
}

func commitID(ctx context.Context, directory, revision string) (string, error) {
	output, err := runGit(ctx, directory, nil, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("read the commit of %s: %w", revision, err)
	}
	return strings.TrimSpace(output), nil
}

// IsAncestor says whether ancestor is descendant or one of its ancestors.
func IsAncestor(ctx context.Context, directory, ancestor, descendant string) (bool, error) {
	_, err := runGit(ctx, directory, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// createBundle runs git bundle create for revisions and reads the bundle back.
func createBundle(ctx context.Context, directory string, revisions ...string) ([]byte, error) {
	path, remove, err := temporaryBundleFile(nil)
	if err != nil {
		return nil, err
	}
	defer remove()
	if _, err := runGit(ctx, directory, nil, append([]string{"bundle", "create", path}, revisions...)...); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if err := checkBundleSize(info.Size()); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// temporaryBundleFile writes bundle to a new file, because git reads and
// writes bundles only as files.
func temporaryBundleFile(bundle []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "mmhotline-systems-repository-*.bundle")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.Remove(file.Name()) }
	_, err = file.Write(bundle)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		remove()
		return "", nil, err
	}
	return file.Name(), remove, nil
}

func newPrivateRef() string {
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	return "refs/mmhotline/agent-commits/" + hex.EncodeToString(random)
}
