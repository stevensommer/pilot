package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// generateTestGPGKey creates a throwaway, unprotected (no passphrase) GPG
// key inside an isolated GNUPGHOME so signing tests never touch the
// developer's/CI runner's real keyring. Returns the GNUPGHOME directory, the
// key's fingerprint, and the Name-Real/Name-Email used to generate it.
//
// Skips the calling test (rather than failing it) when gpg isn't available
// or key generation doesn't work in this environment -- the signing
// behavior this exercises is best-effort verified where gpg exists, not a
// hard requirement of every environment this test suite runs in.
func generateTestGPGKey(t *testing.T) (gnupgHome, fingerprint, name, email string) {
	t.Helper()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed, skipping signing test")
	}

	// gpg-agent communicates over a Unix domain socket named
	// "$GNUPGHOME/S.gpg-agent", and Unix socket paths are capped at ~104-108
	// bytes on macOS/BSD. t.TempDir() nests under a per-test directory named
	// after the test function (e.g.
	// $TMPDIR/TestGitOperationsCommit_SignsWithConfiguredIdentity123/001),
	// which is routinely long enough to blow that limit and make gpg-agent
	// fail to bind ("File name too long" / "No agent running"). Use a short,
	// flat path directly under /tmp instead.
	var err error
	gnupgHome, err = os.MkdirTemp("/tmp", "pgt")
	if err != nil {
		t.Skipf("could not create short GNUPGHOME under /tmp, skipping signing test: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(gnupgHome) })
	if err := os.Chmod(gnupgHome, 0o700); err != nil {
		t.Skipf("could not chmod GNUPGHOME, skipping signing test: %v", err)
	}

	name = "Pilot Signing Test"
	email = "pilot-signing-test@example.invalid"

	batch := fmt.Sprintf(`%%no-protection
Key-Type: RSA
Key-Length: 2048
Name-Real: %s
Name-Email: %s
Expire-Date: 0
%%commit
`, name, email)

	batchFile := filepath.Join(gnupgHome, "keygen.batch")
	if err := os.WriteFile(batchFile, []byte(batch), 0o600); err != nil {
		t.Fatalf("write gpg batch file: %v", err)
	}

	cmd := exec.Command("gpg", "--batch", "--gen-key", batchFile)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+gnupgHome)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("gpg key generation failed, skipping signing test: %v: %s", err, out)
	}

	listCmd := exec.Command("gpg", "--list-secret-keys", "--with-colons", email)
	listCmd.Env = append(os.Environ(), "GNUPGHOME="+gnupgHome)
	out, err := listCmd.Output()
	if err != nil {
		t.Skipf("gpg --list-secret-keys failed, skipping signing test: %v", err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "fpr:") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) > 9 && fields[9] != "" {
			fingerprint = fields[9]
			break
		}
	}
	if fingerprint == "" {
		t.Skipf("could not determine fingerprint from gpg output, skipping signing test: %s", out)
	}

	t.Cleanup(func() {
		killCmd := exec.Command("gpgconf", "--kill", "gpg-agent")
		killCmd.Env = append(os.Environ(), "GNUPGHOME="+gnupgHome)
		_ = killCmd.Run()
	})

	return gnupgHome, fingerprint, name, email
}

// TestGitOperationsCommit_SignsWithConfiguredIdentity is the AC4 test: a
// commit made via GitOperations.Commit must be signed and attributed to the
// project's own configured git identity (user.name/user.email/
// user.signingkey/commit.gpgsign read from the project's checkout), not
// whatever ambient environment variables the commit subprocess would
// otherwise inherit. The two incidents this fixes both showed the fallback
// WIP commit landing as author "Work" with an unverifiable signature while
// ordinary commits in the same execution resolved correctly -- this test
// deliberately pollutes the ambient GIT_AUTHOR_*/GIT_COMMITTER_* env vars
// with a bogus "Work" identity to prove the explicit -c overrides win.
func TestGitOperationsCommit_SignsWithConfiguredIdentity(t *testing.T) {
	repoDir, _ := setupFreshnessRepo(t)
	gnupgHome, fingerprint, name, email := generateTestGPGKey(t)
	t.Setenv("GNUPGHOME", gnupgHome)

	runGit(t, repoDir, "config", "user.name", name)
	runGit(t, repoDir, "config", "user.email", email)
	runGit(t, repoDir, "config", "user.signingkey", fingerprint)
	runGit(t, repoDir, "config", "commit.gpgsign", "true")

	// Pollute the ambient environment with a conflicting identity -- the
	// exact incident shape (author landed as "Work"). If Commit ever falls
	// back to relying on inherited env instead of the explicit -c overrides,
	// this is what would leak through.
	t.Setenv("GIT_AUTHOR_NAME", "Work")
	t.Setenv("GIT_AUTHOR_EMAIL", "work@ambient.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Work")
	t.Setenv("GIT_COMMITTER_EMAIL", "work@ambient.invalid")

	if err := os.WriteFile(filepath.Join(repoDir, "signed_work.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	git := NewGitOperations(repoDir)
	sha, err := git.Commit(context.Background(), "feat: signed commit")
	if err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	if sha == "" {
		t.Fatal("expected non-empty commit SHA")
	}

	gotName := strings.TrimSpace(gitOutput(t, repoDir, "log", "-1", "--format=%an"))
	gotEmail := strings.TrimSpace(gitOutput(t, repoDir, "log", "-1", "--format=%ae"))
	if gotName != name {
		t.Errorf("commit author name = %q, want %q (ambient env must not win)", gotName, name)
	}
	if gotEmail != email {
		t.Errorf("commit author email = %q, want %q (ambient env must not win)", gotEmail, email)
	}

	sigStatus := strings.TrimSpace(gitOutput(t, repoDir, "log", "-1", "--format=%G?"))
	if sigStatus != "G" {
		t.Errorf("commit signature status (%%G?) = %q, want \"G\" (good signature)", sigStatus)
	}

	sigKey := strings.TrimSpace(gitOutput(t, repoDir, "log", "-1", "--format=%GF"))
	if sigKey != fingerprint && !strings.HasSuffix(fingerprint, sigKey) {
		t.Errorf("commit signing fingerprint = %q, want it to match configured key %q", sigKey, fingerprint)
	}
}

// TestGitOperationsCommit_SigningFailureReturnsError is the AC5 test (half
// one): when commit.gpgsign is enabled but the underlying git commit
// invocation cannot actually sign (here: user.signingkey points at a key
// that doesn't exist in the keyring), Commit must return an error -- not
// silently produce an unsigned commit. errors.Is against ErrSigningFailed
// lets callers distinguish this from an ordinary commit failure.
func TestGitOperationsCommit_SigningFailureReturnsError(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed, skipping signing test")
	}

	repoDir, _ := setupFreshnessRepo(t)

	// A fresh, empty GNUPGHOME with no keys at all -- signing with any key
	// ID is guaranteed to fail.
	t.Setenv("GNUPGHOME", t.TempDir())

	runGit(t, repoDir, "config", "user.signingkey", "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF")
	runGit(t, repoDir, "config", "commit.gpgsign", "true")

	if err := os.WriteFile(filepath.Join(repoDir, "unsignable.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	git := NewGitOperations(repoDir)
	sha, err := git.Commit(context.Background(), "feat: should never land unsigned")
	if err == nil {
		t.Fatalf("expected Commit to return an error when signing fails, got sha %q", sha)
	}
	if !errors.Is(err, ErrSigningFailed) {
		t.Errorf("expected error to wrap ErrSigningFailed, got: %v", err)
	}

	// Git's commit is all-or-nothing under -c commit.gpgsign=true: confirm
	// no unsigned commit was silently created as a fallback.
	log := gitOutput(t, repoDir, "log", "--oneline")
	if strings.Contains(log, "should never land unsigned") {
		t.Errorf("expected no commit to exist after a signing failure, but found one in log: %q", log)
	}
}

// TestPreserveDirtyWorktreeAsWIP_SigningFailureFailsClosed is the AC5 test
// (other half): preserveDirtyWorktreeAsWIP must treat a signing failure
// from Commit the same way it already treats a push failure -- falling back
// to the recovery-ref path -- rather than registering an unverifiable
// commit as a PR head. Because git's commit is all-or-nothing under
// gpgsign, a signing failure never produces a commit at all, so there is
// nothing to push; this test's central assertion is that nothing gets
// pushed to origin and no bad SHA is reported as preserved.
func TestPreserveDirtyWorktreeAsWIP_SigningFailureFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed, skipping signing test")
	}

	repoDir, bareDir := setupFreshnessRepo(t)
	t.Setenv("GNUPGHOME", t.TempDir())

	runGit(t, repoDir, "config", "user.signingkey", "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF")
	runGit(t, repoDir, "config", "commit.gpgsign", "true")

	branch := "pilot/GH-1-signing-failclosed"
	runGit(t, repoDir, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repoDir, "work.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	git := NewGitOperations(repoDir)
	task := &Task{ID: "GH-1-SIGNING-FAILCLOSED", Branch: branch, BaseBranch: "main"}

	sha, preserved := preserveDirtyWorktreeAsWIP(context.Background(), git, task, log)

	if preserved {
		t.Errorf("expected preserved=false on signing failure, got true (sha=%q)", sha)
	}
	if sha != "" {
		t.Errorf("expected empty sha on signing failure, got %q", sha)
	}

	// Nothing should have been pushed to origin for this branch.
	checkCmd := exec.Command("git", "-C", bareDir, "rev-parse", "--verify", "refs/heads/"+branch)
	if out, err := checkCmd.CombinedOutput(); err == nil {
		t.Errorf("expected branch %s to not exist on origin (nothing should have been pushed as a PR head), got ref: %s", branch, out)
	}

	if !strings.Contains(logBuf.String(), "signing failure") {
		t.Errorf("expected a WARN log mentioning the signing failure, got: %s", logBuf.String())
	}
}
