package gitutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

var branchNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@-]*$`)

type ResolvedBranch string

func Branch(path string) string {
	out, err := git(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(out) == "" {
		return "main"
	}
	return strings.TrimSpace(out)
}

func Dirty(path string) bool {
	out, err := git(path, "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) != ""
}

func Checkout(path, branch string) error {
	if err := validateBranchName(branch); err != nil {
		return err
	}
	if refExists(path, "refs/heads/"+branch) {
		out, err := git(path, "switch", "--", branch)
		if err != nil {
			return gitFailure("switch branch", out, err)
		}
		return nil
	}
	remote, err := primaryRemote(path)
	if err != nil {
		return err
	}
	if remote == "" || !refExists(path, "refs/remotes/"+remote+"/"+branch) {
		return fmt.Errorf("branch %q is unavailable", branch)
	}
	out, err := git(path, "switch", "--track", "-c", branch, remote+"/"+branch)
	if err != nil {
		return gitFailure("switch branch", out, err)
	}
	return nil
}

func Branches(repoPath string) ([]string, error) {
	local, err := refs(repoPath, "refs/heads")
	if err != nil {
		return nil, err
	}

	remote, err := primaryRemote(repoPath)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(local))
	branches := append([]string(nil), local...)
	for _, branch := range local {
		seen[branch] = true
	}
	if remote != "" {
		remoteBranches, err := refs(repoPath, "refs/remotes/"+remote)
		if err != nil {
			return nil, err
		}
		for _, remoteBranch := range remoteBranches {
			branch, ok := strings.CutPrefix(remoteBranch, remote+"/")
			if !ok || branch == "HEAD" || seen[branch] {
				continue
			}
			seen[branch] = true
			branches = append(branches, branch)
		}
	}
	slices.Sort(branches)
	return branches, nil
}

func ResolveBranch(repoPath, requested string) (ResolvedBranch, error) {
	if err := validateBranchName(requested); err != nil {
		return "", err
	}
	branches, err := Branches(repoPath)
	if err != nil {
		return "", err
	}
	for _, branch := range branches {
		if branch == requested {
			return ResolvedBranch(branch), nil
		}
	}
	return "", fmt.Errorf("branch %q is unavailable", requested)
}

func DefaultBranch(repoPath string) (string, error) {
	remote, err := primaryRemote(repoPath)
	if err != nil {
		return "", err
	}
	return defaultBranch(repoPath, remote)
}

func CanCheckout(repoPath, branch string) error {
	if err := validateBranchName(branch); err != nil {
		return err
	}
	if branch == Branch(repoPath) {
		return nil
	}
	if Dirty(repoPath) {
		return errors.New("commit or stash local changes before switching branches")
	}
	worktrees, err := branchWorktrees(repoPath)
	if err != nil {
		return err
	}
	if worktree := worktrees[branch]; worktree != "" && filepath.Clean(worktree) != filepath.Clean(repoPath) {
		return fmt.Errorf("branch %q is already checked out at %s", branch, worktree)
	}
	return nil
}

func CreateWorktree(repoPath, worktreeRoot string, resolvedBranch ResolvedBranch) (string, error) {
	branch := string(resolvedBranch)
	if branch == "" {
		return "", errors.New("resolved branch required")
	}
	worktrees, err := branchWorktrees(repoPath)
	if err != nil {
		return "", err
	}
	if path := worktrees[branch]; path != "" {
		return "", fmt.Errorf("branch %q is already checked out at %s", branch, path)
	}
	sourceSum := sha256.Sum256([]byte(repoPath))
	branchSum := sha256.Sum256([]byte(branch))
	worktreePath := filepath.Join(worktreeRoot, hex.EncodeToString(sourceSum[:6]), hex.EncodeToString(branchSum[:8]))
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return "", fmt.Errorf("create worktree parent: %w", err)
	}
	args := []string{"worktree", "add", "--", worktreePath, branch}
	if !refExists(repoPath, "refs/heads/"+branch) {
		remote, err := primaryRemote(repoPath)
		if err != nil {
			return "", err
		}
		if remote == "" || !refExists(repoPath, "refs/remotes/"+remote+"/"+branch) {
			return "", fmt.Errorf("branch %q is unavailable", branch)
		}
		args = []string{"worktree", "add", "--track", "-b", branch, "--", worktreePath, remote + "/" + branch}
	}
	out, err := git(repoPath, args...)
	if err != nil {
		return "", gitFailure("create Git worktree", out, err)
	}
	return worktreePath, nil
}

func RemoveWorktree(repoPath, worktreePath string) error {
	return removeWorktree(repoPath, worktreePath, false)
}

func RemoveWorktreeForce(repoPath, worktreePath string) error {
	if _, err := os.Stat(worktreePath); errors.Is(err, os.ErrNotExist) {
		out, pruneErr := git(repoPath, "worktree", "prune")
		if pruneErr != nil {
			return gitFailure("prune missing Git worktree", out, pruneErr)
		}
		return nil
	}
	return removeWorktree(repoPath, worktreePath, true)
}

func removeWorktree(repoPath, worktreePath string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", worktreePath)
	out, err := git(repoPath, args...)
	if err != nil {
		return gitFailure("remove Git worktree", out, err)
	}
	return nil
}

func Pull(path string) (string, error) {
	return git(path, "pull", "--ff-only")
}

func CleanupBranches(repoPath string, settings app.Settings) (app.BranchCleanupResult, error) {
	result := app.BranchCleanupResult{
		LocalDeleted:  []string{},
		RemoteDeleted: []string{},
	}
	if !settings.CleanupLocalMerged && !settings.CleanupRemoteMerged {
		return result, errors.New("enable local or remote branch cleanup first")
	}

	protected, err := NormalizeProtectedPatterns(settings.ProtectedBranches)
	if err != nil {
		return result, err
	}
	remote, err := primaryRemote(repoPath)
	if err != nil {
		return result, err
	}
	if settings.CleanupRemoteMerged {
		if remote == "" {
			return result, errors.New("remote branch cleanup requires a Git remote")
		}
		args := []string{"fetch"}
		if settings.PruneRemoteTracking {
			args = append(args, "--prune")
		}
		args = append(args, remote)
		if out, err := git(repoPath, args...); err != nil {
			return result, gitFailure("fetch remote branches", out, err)
		}
		result.Pruned = settings.PruneRemoteTracking
	}

	defaultBranch, err := defaultBranch(repoPath, remote)
	if err != nil {
		return result, err
	}
	current := Branch(repoPath)
	remoteBase := ""
	if settings.CleanupRemoteMerged {
		remoteBase = remote + "/" + defaultBranch
		if !refExists(repoPath, "refs/remotes/"+remoteBase) {
			return result, fmt.Errorf("remote default branch %q is unavailable", remoteBase)
		}
	}
	if settings.CleanupLocalMerged {
		bases := []string{}
		if refExists(repoPath, "refs/heads/"+defaultBranch) {
			bases = append(bases, defaultBranch)
		}
		if remote != "" && refExists(repoPath, "refs/remotes/"+remote+"/"+defaultBranch) {
			bases = append(bases, remote+"/"+defaultBranch)
		}
		if len(bases) == 0 {
			return result, fmt.Errorf("default branch %q is unavailable locally", defaultBranch)
		}
		branches, err := refs(repoPath, "refs/heads")
		if err != nil {
			return result, err
		}
		checkedOut, err := checkedOutBranches(repoPath)
		if err != nil {
			return result, err
		}
		for _, branch := range branches {
			if checkedOut[branch] || branch == current || branch == defaultBranch || isProtected(branch, protected) {
				continue
			}
			oid, err := revParse(repoPath, branch)
			if err != nil {
				return result, err
			}
			merged, err := isAncestorOfAny(repoPath, branch, bases)
			if err != nil {
				return result, err
			}
			if !merged {
				continue
			}
			if out, err := git(repoPath, "update-ref", "-d", "refs/heads/"+branch, oid); err != nil {
				return result, gitFailure("delete local branch "+branch, out, err)
			}
			result.LocalDeleted = append(result.LocalDeleted, branch)
		}
	}

	if settings.CleanupRemoteMerged {
		branches, err := refs(repoPath, "refs/remotes/"+remote)
		if err != nil {
			return result, err
		}
		for _, remoteBranch := range branches {
			branch, ok := strings.CutPrefix(remoteBranch, remote+"/")
			if !ok || branch == "HEAD" || branch == current || branch == defaultBranch || isProtected(branch, protected) {
				continue
			}
			oid, err := revParse(repoPath, remoteBranch)
			if err != nil {
				return result, err
			}
			merged, err := isAncestor(repoPath, remoteBranch, remoteBase)
			if err != nil {
				return result, err
			}
			if !merged {
				continue
			}
			lease := "--force-with-lease=refs/heads/" + branch + ":" + oid
			if out, err := git(repoPath, "push", lease, remote, ":refs/heads/"+branch); err != nil {
				return result, gitFailure("delete remote branch "+remoteBranch, out, err)
			}
			result.RemoteDeleted = append(result.RemoteDeleted, branch)
		}
	}

	return result, nil
}

func checkedOutBranches(repoPath string) (map[string]bool, error) {
	out, err := git(repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, gitFailure("list Git worktrees", out, err)
	}
	checkedOut := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		if branch, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			checkedOut[branch] = true
		}
	}
	return checkedOut, nil
}

func branchWorktrees(repoPath string) (map[string]string, error) {
	out, err := git(repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, gitFailure("list Git worktrees", out, err)
	}
	worktrees := map[string]string{}
	currentPath := ""
	for line := range strings.SplitSeq(out, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			currentPath = path
			continue
		}
		if branch, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			worktrees[branch] = currentPath
		}
	}
	return worktrees, nil
}

func validateBranchName(branch string) error {
	if !branchNamePattern.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") {
		return errors.New("invalid branch name")
	}
	return nil
}

func revParse(repoPath, ref string) (string, error) {
	out, err := git(repoPath, "rev-parse", "--verify", ref)
	if err != nil {
		return "", gitFailure("resolve branch "+ref, out, err)
	}
	return strings.TrimSpace(out), nil
}

func isAncestorOfAny(repoPath, branch string, bases []string) (bool, error) {
	for _, base := range bases {
		merged, err := isAncestor(repoPath, branch, base)
		if err != nil {
			return false, err
		}
		if merged {
			return true, nil
		}
	}
	return false, nil
}

func primaryRemote(repoPath string) (string, error) {
	out, err := git(repoPath, "remote")
	if err != nil {
		return "", gitFailure("list Git remotes", out, err)
	}
	remotes := strings.Fields(out)
	if slices.Contains(remotes, "origin") {
		return "origin", nil
	}
	if len(remotes) == 0 {
		return "", nil
	}
	return remotes[0], nil
}

func defaultBranch(repoPath, remote string) (string, error) {
	if remote != "" {
		out, err := git(repoPath, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD")
		if err == nil {
			if branch, ok := strings.CutPrefix(strings.TrimSpace(out), remote+"/"); ok && branch != "" {
				return branch, nil
			}
		}
	}
	for _, branch := range []string{"main", "master"} {
		if refExists(repoPath, "refs/heads/"+branch) ||
			(remote != "" && refExists(repoPath, "refs/remotes/"+remote+"/"+branch)) {
			return branch, nil
		}
	}
	return "", errors.New("cannot determine the repository default branch; configure the remote HEAD or use main/master")
}

func refs(repoPath, prefix string) ([]string, error) {
	out, err := git(repoPath, "for-each-ref", "--format=%(refname:short)", prefix)
	if err != nil {
		return nil, gitFailure("list branches", out, err)
	}
	return strings.Fields(out), nil
}

func refExists(repoPath, ref string) bool {
	_, err := git(repoPath, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

func isAncestor(repoPath, branch, base string) (bool, error) {
	out, err := git(repoPath, "merge-base", "--is-ancestor", branch, base)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, gitFailure(fmt.Sprintf("compare %s with %s", branch, base), out, err)
}

func NormalizeProtectedPatterns(patterns []string) ([]string, error) {
	normalized := make([]string, 0, len(patterns))
	seen := map[string]bool{}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || seen[pattern] {
			continue
		}
		if _, err := path.Match(pattern, "branch"); err != nil {
			return nil, fmt.Errorf("invalid protected branch pattern %q: %w", pattern, err)
		}
		seen[pattern] = true
		normalized = append(normalized, pattern)
	}
	return normalized, nil
}

func isProtected(branch string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, _ := path.Match(pattern, branch)
		if matched {
			return true
		}
	}
	return false
}

func gitFailure(action, output string, err error) error {
	message := strings.TrimSpace(output)
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("%s: %s", action, message)
}

func git(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = path
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
