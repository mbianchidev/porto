package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/porto-containers/porto/internal/gitutil"
)

// projectDefaultBranchTTL bounds how long a project listing may report a stale
// default branch after its remote HEAD, remotes, or main/master refs change.
const projectDefaultBranchTTL = 30 * time.Second

type defaultBranchEntry struct {
	branch  string
	expires time.Time
}

// defaultBranchCache memoizes repository default branches for project listings.
// Resolving one costs two to five Git processes per project, and the dashboard
// lists projects every few seconds while the default branch rarely changes.
// The zero value is ready to use.
type defaultBranchCache struct {
	mu      sync.Mutex
	entries map[string]defaultBranchEntry
	ttl     time.Duration
	now     func() time.Time
	resolve func(context.Context, string) (string, error)
}

func (c *defaultBranchCache) lookup(ctx context.Context, repoPath string) string {
	c.mu.Lock()
	now := c.clock()
	entry, cached := c.entries[repoPath]
	resolve := c.resolve
	c.mu.Unlock()
	if cached && now.Before(entry.expires) {
		return entry.branch
	}
	if resolve == nil {
		resolve = gitutil.DefaultBranchContext
	}
	// Repositories without a resolvable default branch are cached as "" too;
	// they are the most expensive case. Cancelled lookups are not cached.
	branch, _ := resolve(ctx, repoPath)
	if ctx.Err() != nil {
		return branch
	}
	c.remember(repoPath, branch)
	return branch
}

// remember records a freshly resolved default branch so listings agree with
// branch operations that resolved it themselves.
func (c *defaultBranchCache) remember(repoPath, branch string) {
	ttl := c.ttl
	if ttl <= 0 {
		ttl = projectDefaultBranchTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.entries == nil {
		c.entries = make(map[string]defaultBranchEntry)
	}
	for path, existing := range c.entries {
		if !now.Before(existing.expires) {
			delete(c.entries, path)
		}
	}
	c.entries[repoPath] = defaultBranchEntry{branch: branch, expires: now.Add(ttl)}
}

func (c *defaultBranchCache) invalidate(repoPath string) {
	c.mu.Lock()
	delete(c.entries, repoPath)
	c.mu.Unlock()
}

func (c *defaultBranchCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
