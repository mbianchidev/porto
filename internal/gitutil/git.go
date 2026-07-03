package gitutil

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var branchNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@-]*$`)

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
	if !branchNamePattern.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") {
		return errors.New("invalid branch name")
	}
	_, err := git(path, "switch", "--", branch)
	return err
}

func Pull(path string) (string, error) {
	return git(path, "pull", "--ff-only")
}

func git(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = path
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
