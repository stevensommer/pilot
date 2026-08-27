// repo_allowlist.go — TASK-286 / GH-3027
//
// Concrete adapter implementing executor.RepoAllowlist over *config.Config.
// Lives in cmd/pilot/ (not internal/executor/) so the executor package
// remains decoupled from the top-level config types — the same reason
// FindProjectByRepo is only invoked from cmd/pilot in the existing codebase.

package main

import (
	"fmt"
	"path/filepath"

	"github.com/qf-studio/pilot/internal/config"
	"github.com/qf-studio/pilot/internal/executor"
)

// configRepoAllowlist adapts *config.Config to the
// executor.RepoAllowlist interface used by the sub-issue creation
// guardrail.
type configRepoAllowlist struct {
	cfg *config.Config
}

// newConfigRepoAllowlist constructs an allowlist backed by cfg. cfg must
// not be nil; pass nil to Runner.SetRepoAllowlist if you genuinely want to
// disable the allowlist (the guardrail will then refuse without
// PILOT_ALLOW_UNMANAGED_REPO=1).
func newConfigRepoAllowlist(cfg *config.Config) executor.RepoAllowlist {
	if cfg == nil {
		return nil
	}
	return &configRepoAllowlist{cfg: cfg}
}

// RepoIsAllowed implements executor.RepoAllowlist.
//
// A repo is allowed if some configured project matches both:
//   - GitHub owner+repo (case-sensitive match against ProjectGitHubConfig)
//   - When projectPath is non-empty, projectPath either equals the matched
//     project's filesystem Path OR is a git worktree of it (shares the same
//     git common-dir). The worktree case is the common path in production:
//     Pilot creates ephemeral worktrees under /tmp/pilot-worktree-* for
//     each task (see internal/executor/worktree.go), and the original
//     strict equality check rejected all of them. GH-3050 follow-up.
//
// The "right repo, wrong working tree" guard is preserved: an unrelated
// clone of the same upstream repo at a different path will fail BOTH the
// equality check and the worktree check (its git common-dir is its own
// .git, not the configured project's).
func (a *configRepoAllowlist) RepoIsAllowed(owner, repo, projectPath string) bool {
	if a == nil || a.cfg == nil {
		return false
	}
	match := a.cfg.FindProjectByRepo(fmt.Sprintf("%s/%s", owner, repo))
	if match == nil {
		return false
	}
	if projectPath == "" || match.Path == projectPath {
		return true
	}
	return isWorktreeOf(projectPath, match.Path)
}

// isWorktreeOf reports whether worktreePath is a git worktree whose common
// .git directory belongs to projectPath. Uses the shared gitCommonDir
// helper (see gitcommondir.go), which resolves `git rev-parse
// --git-common-dir` — the canonical .git location shared by all worktrees
// of a repository.
//
// Returns false (deny) on any error — git missing, path not a repo,
// timeout. The guardrail prefers false negatives (reject worktree) over
// false positives (allow unmanaged repo).
func isWorktreeOf(worktreePath, projectPath string) bool {
	if worktreePath == "" || projectPath == "" {
		return false
	}
	commonDir := gitCommonDir(worktreePath)
	if commonDir == "" {
		return false
	}
	expected, err := filepath.EvalSymlinks(filepath.Join(projectPath, ".git"))
	if err != nil {
		return false
	}
	return commonDir == expected
}

// ConfiguredRepos returns all configured "owner/repo" pairs. Used only for
// error and log messages.
func (a *configRepoAllowlist) ConfiguredRepos() []string {
	if a == nil || a.cfg == nil {
		return nil
	}
	out := make([]string, 0, len(a.cfg.Projects))
	for _, p := range a.cfg.Projects {
		if p == nil || p.GitHub == nil {
			continue
		}
		if p.GitHub.Owner == "" || p.GitHub.Repo == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s/%s", p.GitHub.Owner, p.GitHub.Repo))
	}
	return out
}
