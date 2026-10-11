package dataops

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
)

var ErrBusy = errors.New("a data operation already owns this resource")

type Selection struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	ID          string `json:"id"`
	Destination string `json:"destination,omitempty"`
}

type SourceTemporary struct {
	Context  string `json:"context"`
	Endpoint string `json:"endpoint"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	ID       string `json:"id,omitempty"`
	Owner    string `json:"owner"`
}

type Request struct {
	Action            string             `json:"action"`
	Resource          datafiles.Resource `json:"resource"`
	Identity          string             `json:"identity"`
	Destination       string             `json:"destination,omitempty"`
	Archive           string             `json:"archive,omitempty"`
	Directory         string             `json:"directory,omitempty"`
	Categories        []string           `json:"categories,omitempty"`
	Selections        []Selection        `json:"selections,omitempty"`
	Context           string             `json:"context,omitempty"`
	IncludeSensitive  bool               `json:"includeSensitive,omitempty"`
	AllowSourceHelper bool               `json:"allowSourceHelper,omitempty"`
	Confirm           bool               `json:"confirm"`
	Preview           string             `json:"preview,omitempty"`
	ScheduleID        int64              `json:"scheduleId,omitempty"`
	Retention         int                `json:"retention,omitempty"`
	Trigger           string             `json:"trigger,omitempty"`
}

type Step struct {
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	ID          string `json:"id,omitempty"`
}

type Archive struct {
	Path        string             `json:"path"`
	SHA256      string             `json:"sha256"`
	Manifest    string             `json:"manifest"`
	Bytes       int64              `json:"bytes"`
	Resource    datafiles.Resource `json:"resource"`
	Consistency string             `json:"consistency"`
}

type Result struct {
	Archive        *Archive `json:"archive,omitempty"`
	Steps          []Step   `json:"steps,omitempty"`
	BytesReclaimed *int64   `json:"bytesReclaimed,omitempty"`
	Message        string   `json:"message,omitempty"`
}

type Operation struct {
	ID          int64   `json:"id"`
	Request     Request `json:"request"`
	Status      string  `json:"status"`
	Phase       string  `json:"phase"`
	Bytes       int64   `json:"bytes"`
	StartedAt   string  `json:"startedAt"`
	CompletedAt string  `json:"completedAt,omitempty"`
	Result      Result  `json:"result"`
	Error       string  `json:"error,omitempty"`
}

type Schedule struct {
	ID            int64              `json:"id"`
	Resource      datafiles.Resource `json:"resource"`
	Enabled       bool               `json:"enabled"`
	IntervalHours int                `json:"intervalHours"`
	Retention     int                `json:"retention"`
	Directory     string             `json:"directory"`
	NextRunAt     string             `json:"nextRunAt"`
}

func (s Schedule) Validate() error {
	if s.Resource.Kind != "volume" || s.Resource.ID == "" || s.Resource.Name == "" || s.Resource.ReadOnly {
		return errors.New("backup schedule requires an identity-bearing writable local volume")
	}
	if s.IntervalHours < 1 || s.IntervalHours > 8760 || s.Retention < 1 || s.Retention > 365 {
		return errors.New("invalid backup schedule: interval must be 1-8760 hours and retention 1-365 verified archives")
	}
	if !filepath.IsAbs(s.Directory) {
		return errors.New("backup directory must be an absolute local path")
	}
	return nil
}

func (s Schedule) Deadline(now time.Time) string {
	return now.UTC().Add(time.Duration(s.IntervalHours) * time.Hour).Format(time.RFC3339Nano)
}

func (r Request) ResourceKey() string {
	if r.Resource.ID != "" {
		return r.Resource.Fingerprint()
	}
	if r.Action == "prune" || r.Action == "migration" {
		return "runtime-data"
	}
	return fmt.Sprintf("%s:%s", r.Action, r.Destination)
}
