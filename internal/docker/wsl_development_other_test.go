//go:build !windows

package docker

import (
	"context"
	"testing"
)

func testWSLDevelopmentWorkflow(t *testing.T, _ context.Context, _ *Manager, _, _ string) {
	t.Fatal("real WSL development acceptance requires Windows")
}
