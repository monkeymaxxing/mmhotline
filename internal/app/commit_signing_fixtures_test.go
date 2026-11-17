package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"mmhotline/internal/systemsrepository"
)

// The tests' commit signing keys, as the team's server setup and the
// settings file would have them: Barbara's key, which the team trusts, signs
// every fixture commit, and the triage agent's key signs the agent's commits.
// The private keys are test fixtures only.
const (
	testTrustedCommitSigningPublicKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKkV3VQGyLmAYtl/HR7x93Qw22ugM6k9OhTepuWlHFPr Barbara, a person the team trusts"
	testTrustedCommitSigningPrivateKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACCpFd1UBsi5gGLZfx0e8fd0MNtroDOpPToU3qblpRxT6wAAAKi9XfGAvV3x
gAAAAAtzc2gtZWQyNTUxOQAAACCpFd1UBsi5gGLZfx0e8fd0MNtroDOpPToU3qblpRxT6w
AAAEAlpgHXKBKEoVcDo7ioUXLqbjV08VCofpapwZpHripfHakV3VQGyLmAYtl/HR7x93Qw
22ugM6k9OhTepuWlHFPrAAAAIUJhcmJhcmEsIGEgcGVyc29uIHRoZSB0ZWFtIHRydXN0cw
ECAwQ=
-----END OPENSSH PRIVATE KEY-----
`
	testTriageAgentCommitSigningPublicKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGUdoc+44+a4QWDPzegMsM38OwWeeUot9ceymJ5D1H0i the triage agent"
	testTriageAgentCommitSigningPrivateKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACBlHaHPuOPmuEFgz83oDLDN/DsFnnlKLfXHspieQ9R9IgAAAJgoIrjxKCK4
8QAAAAtzc2gtZWQyNTUxOQAAACBlHaHPuOPmuEFgz83oDLDN/DsFnnlKLfXHspieQ9R9Ig
AAAEAdmvVZWkGKCuxF9X3tFqbN0V2t500dyjlT2bjbMXEiFGUdoc+44+a4QWDPzegMsM38
OwWeeUot9ceymJ5D1H0iAAAAEHRoZSB0cmlhZ2UgYWdlbnQBAgMEBQ==
-----END OPENSSH PRIVATE KEY-----
`
)

// testSigningFiles are the key files and Git settings of one test process,
// written once and removed by TestMain.
type testSigningFileSet struct {
	once      sync.Once
	directory string
	// trustedGitConfig is the global Git configuration testGit uses: it
	// signs as Barbara.
	trustedKey, triageAgentKey, trustedGitConfig string
	sshKeygen                                    string
	err                                          error
}

var testSigningFiles testSigningFileSet

func testSigning(t testing.TB) *testSigningFileSet {
	t.Helper()
	files := &testSigningFiles
	files.once.Do(func() {
		if files.sshKeygen, files.err = exec.LookPath("ssh-keygen"); files.err != nil {
			return
		}
		if files.directory, files.err = os.MkdirTemp("", "mmhotline-test-signing-"); files.err != nil {
			return
		}
		files.trustedKey = filepath.Join(files.directory, "trusted_key")
		files.triageAgentKey = filepath.Join(files.directory, "triage_agent_key")
		files.trustedGitConfig = filepath.Join(files.directory, "trusted_gitconfig")
		for path, content := range map[string]string{
			files.trustedKey:       testTrustedCommitSigningPrivateKey,
			files.triageAgentKey:   testTriageAgentCommitSigningPrivateKey,
			files.trustedGitConfig: signingGitConfig("Barbara", "barbara@example.com", files.trustedKey, files.sshKeygen),
		} {
			if files.err = os.WriteFile(path, []byte(content), 0o600); files.err != nil {
				return
			}
		}
	})
	if files.err != nil {
		t.Fatal(files.err)
	}
	return files
}

// signingGitConfig is a ~/.gitconfig that signs every commit with key.
func signingGitConfig(name, email, key, sshKeygen string) string {
	return fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n\tsigningkey = %s\n[gpg]\n\tformat = ssh\n[gpg \"ssh\"]\n\tprogram = %s\n[commit]\n\tgpgsign = true\n", name, email, key, sshKeygen)
}

func removeTestSigningFiles() {
	if testSigningFiles.directory != "" {
		os.RemoveAll(testSigningFiles.directory)
	}
}

// recordCommitSigningKeyForTest applies the tests' keys and
// signed_by_a_trusted_key_or_reviewed, as the settings file's
// systems_repository_commits and triage_agent sections would.
func recordCommitSigningKeyForTest(t testing.TB, a *App) {
	t.Helper()
	setTrustedCommitsForTest(t, a, `"signed_by_a_trusted_key_or_reviewed"`)
	if err := a.setCommitSigningKeys(context.Background(), systemsrepository.CommitSigningKeys{TriageAgent: testTriageAgentCommitSigningPublicKey, Trusted: []string{testTrustedCommitSigningPublicKey}}); err != nil {
		t.Fatal(err)
	}
}

// signMergesInCopyForTest makes MMHotline's copy sign its merges as Barbara,
// as the team's server setup sets up util01's signing key there.
func signMergesInCopyForTest(t *testing.T, directory string) {
	t.Helper()
	signing := testSigning(t)
	testGit(t, directory, "config", "gpg.format", "ssh")
	testGit(t, directory, "config", "gpg.ssh.program", signing.sshKeygen)
	testGit(t, directory, "config", "user.signingkey", signing.trustedKey)
	testGit(t, directory, "config", "commit.gpgsign", "true")
}
