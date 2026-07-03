package gitutil

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

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
	_, err := git(path, "checkout", branch)
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
