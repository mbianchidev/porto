package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDefaultBranchCacheReusesLookupsUntilExpiry(t *testing.T) {
	now := time.Unix(1_000, 0)
	calls := map[string]int{}
	cache := defaultBranchCache{
		ttl: time.Minute,
		now: func() time.Time { return now },
		resolve: func(_ context.Context, repoPath string) (string, error) {
			calls[repoPath]++
			if repoPath == "/no-default" {
				return "", errors.New("cannot determine the repository default branch")
			}
			return "main", nil
		},
	}
	ctx := context.Background()

	for range 3 {
		if branch := cache.lookup(ctx, "/repo"); branch != "main" {
			t.Fatalf("default branch = %q", branch)
		}
		if branch := cache.lookup(ctx, "/no-default"); branch != "" {
			t.Fatalf("unresolvable default branch = %q", branch)
		}
	}
	if calls["/repo"] != 1 || calls["/no-default"] != 1 {
		t.Fatalf("cached lookups resolved again: %v", calls)
	}

	cache.remember("/repo", "trunk")
	if branch := cache.lookup(ctx, "/repo"); branch != "trunk" || calls["/repo"] != 1 {
		t.Fatalf("remembered branch = %q after %d lookups", branch, calls["/repo"])
	}

	now = now.Add(time.Minute)
	if branch := cache.lookup(ctx, "/repo"); branch != "main" || calls["/repo"] != 2 {
		t.Fatalf("expired entry = %q after %d lookups", branch, calls["/repo"])
	}

	cache.invalidate("/repo")
	cache.lookup(ctx, "/repo")
	if calls["/repo"] != 3 {
		t.Fatalf("invalidated entry was reused: %v", calls)
	}
}

func TestDefaultBranchCacheDoesNotStoreCancelledLookups(t *testing.T) {
	calls := 0
	cache := defaultBranchCache{resolve: func(ctx context.Context, _ string) (string, error) {
		calls++
		return "", ctx.Err()
	}}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cache.lookup(cancelled, "/repo")
	cache.lookup(context.Background(), "/repo")
	if calls != 2 {
		t.Fatalf("cancelled lookup was cached: %d resolutions", calls)
	}
}
