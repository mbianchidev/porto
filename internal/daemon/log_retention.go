package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/porto-containers/porto/internal/app"
)

// projectLogRetentionInterval only checks the cutoff; rows are deleted when the
// calendar cutoff moves, at startup, or after the retention setting changes.
const projectLogRetentionInterval = time.Hour

// projectLogCutoff matches diagnostic log retention: the configured number of
// calendar days, including today, in local time.
func projectLogCutoff(now time.Time, days int) time.Time {
	year, month, day := now.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
}

func (s *Server) projectLogRetentionLoop(ctx context.Context) {
	ticker := time.NewTicker(projectLogRetentionInterval)
	defer ticker.Stop()
	var applied time.Time
	for {
		applied = s.applyProjectLogRetention(ctx, applied)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// applyProjectLogRetention deletes project log lines older than the configured
// retention and returns the cutoff it applied, or previous when nothing ran.
func (s *Server) applyProjectLogRetention(ctx context.Context, previous time.Time) time.Time {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("Unable to load project log retention; retrying next hour", "error", err)
		}
		return previous
	}
	days := settings.LogRetentionDays
	if days < 1 || days > app.MaximumLogRetentionDays {
		days = app.DefaultLogRetentionDays
	}
	cutoff := projectLogCutoff(time.Now(), days)
	if cutoff.Equal(previous) {
		return previous
	}
	removed, err := s.store.PruneLogsBefore(ctx, cutoff)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("Unable to delete expired project logs; retrying next hour",
				"cutoff", cutoff.Format(time.RFC3339), "removed", removed, "error", err)
		}
		return previous
	}
	if removed > 0 {
		slog.Info("Deleted expired project logs", "lines", removed, "retentionDays", days, "cutoff", cutoff.Format(time.RFC3339))
	}
	return cutoff
}
