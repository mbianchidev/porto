//go:build !windows

package docker

import (
	"context"
	"testing"
)

func testWindowsLinuxCoexistence(t *testing.T, _ context.Context, _ *Manager, _, _, _, _ string) {
	t.Helper()
	t.Fatal("PORTO_TEST_WINDOWS_COEXISTENCE requires the disposable native Windows AMD64 Lima client fixture")
}
