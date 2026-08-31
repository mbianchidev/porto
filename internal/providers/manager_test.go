package providers

import (
	"context"
	"strings"
	"testing"
)

func TestInstallRejectsUnknownProvider(t *testing.T) {
	_, err := New(nil).Install(context.Background(), "unknown")
	if err == nil || !strings.Contains(err.Error(), "unknown runtime provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}
