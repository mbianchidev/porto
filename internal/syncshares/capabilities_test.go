package syncshares

import (
	"context"
	"testing"
)

func TestWindowsSynchronizedSharingFailsBeforeCommandsOrMutation(t *testing.T) {
	manager := New(t.TempDir(), &syntheticSyncRunner{}, nil)
	manager.goos = "windows"
	manager.lookPath = func(string) (string, error) {
		t.Fatal("unsupported host tried to locate or execute a synchronization tool")
		return "", nil
	}
	if _, _, err := manager.tool(context.Background()); err == nil {
		t.Fatal("unsupported Windows optimized sharing was accepted")
	}
}
