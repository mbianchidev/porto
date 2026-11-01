package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"
)

const (
	APIVersion      = "porto.extensions/v1"
	RuntimeRead     = "runtime.read"
	RuntimeControl  = "runtime.control"
	FilesRead       = "host.files.read"
	FilesWrite      = "host.files.write"
	ProcessExec     = "process.exec"
	SecretsRead     = "secrets.read"
	NetworkFetch    = "network.fetch"
	MaxPackageBytes = 16 << 20
)

var capabilities = []string{RuntimeRead, RuntimeControl, FilesRead, FilesWrite, ProcessExec, SecretsRead, NetworkFetch}
var extensionID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+){1,7}$`)
var contributionID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Manifest struct {
	APIVersion  string   `json:"apiVersion"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
	Views       []View   `json:"views"`
	Actions     []Action `json:"actions"`
}
type View struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Location     string `json:"location"`
	ResourceKind string `json:"resourceKind,omitempty"`
}
type Action struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	ResourceKind string `json:"resourceKind,omitempty"`
	Destructive  bool   `json:"destructive"`
}
type Content struct {
	Manifest Manifest `json:"manifest"`
	UI       string   `json:"ui"`
	Module   []byte   `json:"module"`
}
type Package struct {
	Content   Content `json:"content"`
	SHA256    string  `json:"sha256"`
	PublicKey string  `json:"publicKey,omitempty"`
	Signature string  `json:"signature,omitempty"`
}

func Digest(value Content) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type InstallRequest struct {
	Package        Package `json:"package"`
	ExpectedSHA256 string  `json:"expectedSHA256"`
	Developer      bool    `json:"developer"`
}
type CommandGrant struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
}
type Grants struct {
	Permissions  []string       `json:"permissions"`
	Environments []string       `json:"environments"`
	Files        []string       `json:"files"`
	Commands     []CommandGrant `json:"commands"`
	Secrets      []string       `json:"secrets"`
	Destinations []string       `json:"destinations"`
}
type Configuration struct {
	Enabled bool   `json:"enabled"`
	Approve bool   `json:"approve"`
	Grants  Grants `json:"grants"`
}
type Installed struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	Publisher   string    `json:"publisher,omitempty"`
	Source      string    `json:"source"`
	Enabled     bool      `json:"enabled"`
	Approved    bool      `json:"approved"`
	Compatible  bool      `json:"compatible"`
	Generation  uint64    `json:"generation"`
	Manifest    Manifest  `json:"manifest"`
	Grants      Grants    `json:"grants"`
	State       string    `json:"state"`
	LastError   string    `json:"lastError,omitempty"`
	LastRun     time.Time `json:"lastRun,omitzero"`
	SecretNames []string  `json:"secretNames,omitempty"`
}
type Invocation struct {
	Action        string          `json:"action"`
	EnvironmentID string          `json:"environmentId"`
	ResourceKind  string          `json:"resourceKind,omitempty"`
	ResourceID    string          `json:"resourceId,omitempty"`
	Args          json.RawMessage `json:"args,omitempty"`
	Confirm       bool            `json:"confirm"`
}
type Result struct {
	EnvironmentID string          `json:"environmentId"`
	Action        string          `json:"action"`
	Data          json.RawMessage `json:"data"`
}
type brokerRequest struct {
	Capability string          `json:"capability"`
	Method     string          `json:"method,omitempty"`
	Path       string          `json:"path,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	File       string          `json:"file,omitempty"`
	Content    string          `json:"content,omitempty"`
	Executable string          `json:"executable,omitempty"`
	Args       []string        `json:"args,omitempty"`
	URL        string          `json:"url,omitempty"`
	Secret     string          `json:"secret,omitempty"`
}
type RuntimeBroker func(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error)

type CatalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	Publisher   string `json:"publisher"`
}
type Catalog struct {
	Version   int            `json:"version"`
	Entries   []CatalogEntry `json:"entries"`
	PublicKey string         `json:"publicKey"`
	Signature string         `json:"signature"`
}
