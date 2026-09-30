package datafiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	MaxTextBytes    = 256 * 1024
	MaxUploadBytes  = 64 * 1024 * 1024
	MaxArchiveBytes = int64(64 * 1024 * 1024 * 1024)
	MaxEntries      = 100000
	MaxListing      = 2000
	MaxPathBytes    = 4096
)

var (
	ErrInvalid     = errors.New("invalid filesystem request")
	ErrConflict    = errors.New("filesystem changed; refresh the preview before retrying")
	ErrUnsupported = errors.New("filesystem capability is unavailable")
	ErrLimit       = errors.New("filesystem operation exceeds its safety limit")
)

type Resource struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt,omitempty"`
	Backend   string `json:"backend,omitempty"`
	ReadOnly  bool   `json:"readOnly"`
}

func (r Resource) Fingerprint() string {
	document, _ := json.Marshal(r)
	hash := sha256.Sum256(document)
	return hex.EncodeToString(hash[:])
}

type Entry struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	UID        int    `json:"uid"`
	GID        int    `json:"gid"`
	ModifiedAt string `json:"modifiedAt"`
	LinkTarget string `json:"linkTarget,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

type Manifest struct {
	Version     int       `json:"version"`
	Resource    Resource  `json:"resource"`
	CreatedAt   time.Time `json:"createdAt"`
	Consistency string    `json:"consistency"`
	Bytes       int64     `json:"bytes"`
	Entries     []Entry   `json:"entries"`
	SHA256      string    `json:"sha256"`
}

type RestoreOptions struct {
	PreserveOwnership bool
	AvailableBytes    func(string) (uint64, error)
}

type Listing struct {
	Resource  Resource `json:"resource"`
	Identity  string   `json:"identity"`
	Path      string   `json:"path"`
	Entries   []Entry  `json:"entries"`
	Truncated bool     `json:"truncated"`
	ReadOnly  bool     `json:"readOnly"`
	Mounts    []Mount  `json:"mounts,omitempty"`
	Message   string   `json:"message,omitempty"`
}

type Mount struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Source   string `json:"source,omitempty"`
	ReadOnly bool   `json:"readOnly"`
}

type Content struct {
	Path     string `json:"path"`
	Text     string `json:"text"`
	SHA256   string `json:"sha256"`
	ReadOnly bool   `json:"readOnly"`
}

type Request struct {
	Resource Resource `json:"resource"`
	Identity string   `json:"identity"`
	Action   string   `json:"action"`
	Path     string   `json:"path,omitempty"`
	SHA256   string   `json:"sha256,omitempty"`
	Text     string   `json:"text,omitempty"`
	Confirm  bool     `json:"confirm,omitempty"`
	Writable bool     `json:"writable,omitempty"`
	Token    string   `json:"token,omitempty"`
}

type contextReader struct {
	reader io.Reader
	check  func() error
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.check(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer[:min(len(buffer), 64*1024)])
}
