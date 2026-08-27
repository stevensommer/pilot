// gitcommondir.go — shared git-common-dir helper.
//
// GH-3050 discovered that comparing raw filesystem paths to decide whether
// a task's execution path "is" a configured project silently breaks for
// every git worktree: Pilot's default execution mode (use_worktree: true)
// runs each task in an ephemeral worktree such as
// /var/folders/.../pilot-worktree-GH-613-..., which is never string-equal
// to the project's configured checkout path. `git rev-parse
// --git-common-dir`, run from inside a worktree, resolves back to the
// primary checkout's real .git directory, giving a canonical identity to
// compare against instead. GH-3050 fixed the repo allowlist
// (cmd/pilot/repo_allowlist.go) with this technique; the identical defect
// also made per-project quality gate resolution (GH-3716) unreachable for
// worktree executions, so the helper lives here where both call sites (and
// any future ones) can share it without drifting apart.
package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitCommonDir resolves the canonical git "common dir" for the working
// tree at path — the real .git directory shared by every worktree of a
// repository. For a linked worktree this points back at the primary
// checkout's .git; for a plain (non-worktree) checkout it is that repo's
// own .git.
//
// Returns "" on any error — path is empty, not a git working tree, git is
// missing, or the call times out — so callers should treat "" as "does not
// match" rather than a fatal condition. A 2s timeout bounds the git call
// so a hung or missing git binary can't stall task dispatch.
func gitCommonDir(path string) string {
	if path == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	commonDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(path, commonDir)
	}
	resolved, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return ""
	}
	return resolved
}
