package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/app"
)

func TestCleanupBranchesDeletesOnlyFullyMergedLocalBranches(t *testing.T) {
	repo := initTestRepo(t)
	runTestGit(t, repo, "switch", "-c", "merged")
	writeAndCommit(t, repo, "merged.txt", "merged", "add merged work")
	runTestGit(t, repo, "switch", "main")
	runTestGit(t, repo, "merge", "--no-ff", "merged", "-m", "merge branch")

	runTestGit(t, repo, "switch", "-c", "keep-me")
	writeAndCommit(t, repo, "keep.txt", "keep", "add protected work")
	runTestGit(t, repo, "switch", "main")
	runTestGit(t, repo, "merge", "--no-ff", "keep-me", "-m", "merge protected branch")

	runTestGit(t, repo, "switch", "-c", "unmerged")
	writeAndCommit(t, repo, "unmerged.txt", "unmerged", "add unmerged work")
	runTestGit(t, repo, "switch", "main")

	result, err := CleanupBranches(repo, app.Settings{
		CleanupLocalMerged: true,
		ProtectedBranches:  []string{"main", "keep-*"},
	})
	if err != nil {
		t.Fatalf("cleanup branches: %v", err)
	}
	if len(result.LocalDeleted) != 1 || result.LocalDeleted[0] != "merged" {
		t.Fatalf("deleted local branches = %v, want [merged]", result.LocalDeleted)
	}
	if refExists(repo, "refs/heads/merged") {
		t.Fatal("merged branch still exists")
	}
	if !refExists(repo, "refs/heads/keep-me") {
		t.Fatal("protected branch was deleted")
	}
	if !refExists(repo, "refs/heads/unmerged") {
		t.Fatal("unmerged branch was deleted")
	}
}

func TestCleanupBranchesKeepsCurrentBranch(t *testing.T) {
	repo := initTestRepo(t)
	runTestGit(t, repo, "switch", "-c", "current")

	result, err := CleanupBranches(repo, app.Settings{CleanupLocalMerged: true})
	if err != nil {
		t.Fatalf("cleanup branches: %v", err)
	}
	if len(result.LocalDeleted) != 0 {
		t.Fatalf("deleted current branch: %v", result.LocalDeleted)
	}
	if !refExists(repo, "refs/heads/current") {
		t.Fatal("current branch was deleted")
	}
}

func TestCleanupBranchesValidatesRemoteBeforeDeletingLocalBranches(t *testing.T) {
	repo := initTestRepo(t)
	runTestGit(t, repo, "switch", "-c", "merged")
	writeAndCommit(t, repo, "merged.txt", "merged", "add merged work")
	runTestGit(t, repo, "switch", "main")
	runTestGit(t, repo, "merge", "--no-ff", "merged", "-m", "merge branch")

	_, err := CleanupBranches(repo, app.Settings{
		CleanupLocalMerged:  true,
		CleanupRemoteMerged: true,
	})
	if err == nil {
		t.Fatal("cleanup without a remote succeeded")
	}
	if !refExists(repo, "refs/heads/merged") {
		t.Fatal("local branch was deleted before remote validation")
	}
}

func TestCleanupBranchesDeletesFullyMergedRemoteBranch(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/main")

	repo := filepath.Join(root, "repo")
	runTestGit(t, root, "clone", remote, repo)
	runTestGit(t, repo, "config", "user.name", "Porto Tests")
	runTestGit(t, repo, "config", "user.email", "porto@example.com")
	runTestGit(t, repo, "config", "commit.gpgsign", "false")
	writeAndCommit(t, repo, "README.md", "test", "initial commit")
	runTestGit(t, repo, "push", "-u", "origin", "main")

	runTestGit(t, repo, "switch", "-c", "merged-remote")
	writeAndCommit(t, repo, "remote.txt", "merged", "add remote work")
	runTestGit(t, repo, "push", "-u", "origin", "merged-remote")
	runTestGit(t, repo, "switch", "main")
	runTestGit(t, repo, "merge", "--no-ff", "merged-remote", "-m", "merge remote branch")
	runTestGit(t, repo, "push", "origin", "main")

	runTestGit(t, repo, "switch", "-c", "current-merged")
	writeAndCommit(t, repo, "current.txt", "current", "add current work")
	runTestGit(t, repo, "push", "-u", "origin", "current-merged")
	runTestGit(t, repo, "switch", "main")
	runTestGit(t, repo, "merge", "--no-ff", "current-merged", "-m", "merge current branch")
	runTestGit(t, repo, "push", "origin", "main")
	runTestGit(t, repo, "switch", "current-merged")
	runTestGit(t, repo, "branch", "-f", "main", "main~2")

	result, err := CleanupBranches(repo, app.Settings{
		CleanupLocalMerged:  true,
		CleanupRemoteMerged: true,
		PruneRemoteTracking: true,
		ProtectedBranches:   []string{"main"},
	})
	if err != nil {
		t.Fatalf("cleanup branches: %v", err)
	}
	if len(result.RemoteDeleted) != 1 || result.RemoteDeleted[0] != "merged-remote" {
		t.Fatalf("deleted remote branches = %v, want [merged-remote]", result.RemoteDeleted)
	}
	if len(result.LocalDeleted) != 1 || result.LocalDeleted[0] != "merged-remote" {
		t.Fatalf("deleted local branches = %v, want [merged-remote]", result.LocalDeleted)
	}
	if output := testGitOutput(t, repo, "ls-remote", "--heads", "origin", "refs/heads/merged-remote"); output != "" {
		t.Fatalf("remote branch still exists: %s", output)
	}
	if !refExists(repo, "refs/heads/current-merged") {
		t.Fatal("current local branch was deleted")
	}
	if output := testGitOutput(t, repo, "ls-remote", "--heads", "origin", "refs/heads/current-merged"); output == "" {
		t.Fatal("current remote branch was deleted")
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runTestGit(t, repo, "init", "-b", "main")
	runTestGit(t, repo, "config", "user.name", "Porto Tests")
	runTestGit(t, repo, "config", "user.email", "porto@example.com")
	runTestGit(t, repo, "config", "commit.gpgsign", "false")
	writeAndCommit(t, repo, "README.md", "test", "initial commit")
	return repo
}

func writeAndCommit(t *testing.T, repo, name, content, message string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runTestGit(t, repo, "add", name)
	runTestGit(t, repo, "commit", "-m", message)
}

func runTestGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	_ = testGitOutput(t, repo, args...)
}

func testGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	} else {
		return string(out)
	}
	return ""
}
