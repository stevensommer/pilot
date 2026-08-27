// project_path_resolver.go — GH-3716 worktree-resolution follow-up.
//
// Pilot's default execution mode (use_worktree: true) runs every task in
// an ephemeral git worktree such as
// /var/folders/.../pilot-worktree-GH-613-..., not in the project's
// configured checkout path. cmd/pilot/adapters.go's
// newProjectQualityCheckerFactory resolved the task's project via
// cfg.FindProjectByPath(executionPath), which compares with raw string
// equality — so it never matched a worktree, and per-project `quality:`
// gates were silently unreachable for the normal execution path. Every
// task instead fell through to quality.AutoDetectConfig run against the
// worktree, which can select entirely the wrong stack's gate on a
// polyglot repo.
//
// This is the same defect GH-3050 hit and fixed for the repo allowlist
// (cmd/pilot/repo_allowlist.go) by comparing git common-dirs instead of
// paths. projectPathResolver applies the identical technique (via the
// shared gitCommonDir helper in gitcommondir.go) so both call sites use
// identical logic and can't drift apart again.
package main

import (
	"path/filepath"
	"sync"

	"github.com/qf-studio/pilot/internal/config"
)

// projectPathResolver resolves a task's execution path back to the
// *config.ProjectConfig it belongs to, falling back from raw path equality
// to a git-common-dir comparison so an ephemeral worktree of a registered
// project still resolves to that project.
//
// Each configured project's own git common-dir is resolved at most once
// per resolver instance and cached in dirCache, since a project's checkout
// path doesn't change for the lifetime of the daemon; the execution path
// passed to find is never cached, since a fresh worktree is created per
// task.
type projectPathResolver struct {
	cfg *config.Config

	mu       sync.Mutex
	dirCache map[string]string // project.Path -> resolved git common-dir ("" if unresolvable)
}

// newProjectPathResolver constructs a resolver backed by cfg. cfg may be
// nil; find then always returns nil.
func newProjectPathResolver(cfg *config.Config) *projectPathResolver {
	return &projectPathResolver{cfg: cfg, dirCache: make(map[string]string)}
}

// find returns the configured project matching executionPath.
//
// It first tries exact path equality (config.Config.FindProjectByPath) —
// the cheap, common case when a task happens to execute directly in a
// project's checkout (use_worktree: false). If that misses, it resolves
// executionPath's git common-dir and compares it against each configured
// project's own common-dir: a worktree of a registered project shares that
// project's common-dir even though its filesystem path differs.
//
// An unrelated clone of the same upstream repo at a different,
// unregistered path does not alias a configured project: its common-dir is
// its own .git, matching neither the exact-path check nor any configured
// project's common-dir. This preserves the GH-3050 guarantee that two
// different checkouts of the same repo can't be confused for one another.
//
// Returns nil if cfg is nil, executionPath is empty, or no project matches
// either way.
func (r *projectPathResolver) find(executionPath string) *config.ProjectConfig {
	if r == nil || r.cfg == nil || executionPath == "" {
		return nil
	}
	if proj := r.cfg.FindProjectByPath(executionPath); proj != nil {
		return proj
	}

	commonDir := gitCommonDir(executionPath)
	if commonDir == "" {
		return nil
	}
	for _, p := range r.cfg.Projects {
		if p == nil || p.Path == "" {
			continue
		}
		if dir := r.projectCommonDir(p.Path); dir != "" && dir == commonDir {
			return p
		}
	}
	return nil
}

// projectCommonDir returns projectPath's own git common-dir (its .git,
// resolved through symlinks), memoized across calls on this resolver.
// Unlike gitCommonDir(executionPath), this does not shell out to git:
// projectPath is a project's registered checkout, presumed to be the
// primary (non-worktree) working tree, so its .git is a real directory
// rather than a worktree's gitdir-pointer file.
func (r *projectPathResolver) projectCommonDir(projectPath string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if dir, ok := r.dirCache[projectPath]; ok {
		return dir
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(projectPath, ".git"))
	if err != nil {
		dir = ""
	}
	r.dirCache[projectPath] = dir
	return dir
}
