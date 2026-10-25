//go:build linux || windows

package localhttps

import (
	"context"
	"testing"
)

func nativeHeadlessHTTPSCommand(*testing.T, context.Context, string, string, ...string) ([]byte, bool, error) {
	return nil, false, nil
}
