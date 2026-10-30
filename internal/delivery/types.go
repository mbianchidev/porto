package delivery

import (
	"context"
	"errors"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/deliverysource"
)

var (
	ErrInvalid  = errors.New("invalid application delivery request")
	ErrConflict = errors.New("application delivery conflict")
)

type Source struct {
	Kind            string             `json:"kind"`
	Git             deliverysource.Git `json:"git"`
	Template        string             `json:"template,omitempty"`
	TemplateVersion int                `json:"templateVersion,omitempty"`
}

type Request struct {
	ProjectID      int64             `json:"projectId"`
	Name           string            `json:"name"`
	Target         string            `json:"target"`
	Source         Source            `json:"source"`
	Compose        string            `json:"compose"`
	Variables      map[string]string `json:"variables"`
	Profiles       []string          `json:"profiles"`
	Secrets        map[string]string `json:"secrets"`
	GitToken       *string           `json:"gitToken,omitempty"`
	PollSeconds    int               `json:"pollSeconds"`
	WebhookEnabled bool              `json:"webhookEnabled"`
	WebhookSecret  *string           `json:"webhookSecret,omitempty"`
	DetachGit      bool              `json:"detachGit"`
	ConfirmDrift   bool              `json:"confirmDrift"`
}

type Configuration struct {
	Variables map[string]string `json:"variables"`
	Profiles  []string          `json:"profiles"`
}

type Revision struct {
	ID              string            `json:"id"`
	OperationID     string            `json:"operationId"`
	Commit          string            `json:"commit,omitempty"`
	ConfigDigest    string            `json:"configDigest"`
	EffectiveDigest string            `json:"effectiveDigest"`
	Images          map[string]string `json:"images"`
	CreatedAt       string            `json:"createdAt"`
	RollbackOf      string            `json:"rollbackOf,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
	Profiles        []string          `json:"profiles,omitempty"`
}

type Application struct {
	ProjectID       int64      `json:"projectId"`
	Name            string     `json:"name"`
	Target          string     `json:"target"`
	Source          Source     `json:"source"`
	Profiles        []string   `json:"profiles,omitempty"`
	CurrentRevision string     `json:"currentRevision"`
	LastOperation   string     `json:"lastOperation,omitempty"`
	PollSeconds     int        `json:"pollSeconds"`
	WebhookEnabled  bool       `json:"webhookEnabled"`
	WebhookURL      string     `json:"webhookUrl,omitempty"`
	NextPollAt      string     `json:"nextPollAt,omitempty"`
	Failures        int        `json:"failures"`
	LastError       string     `json:"lastError,omitempty"`
	Drift           bool       `json:"drift"`
	Revisions       []Revision `json:"revisions"`
}

type Preview struct {
	Token        string   `json:"token"`
	ProjectID    int64    `json:"projectId"`
	BaseRevision string   `json:"baseRevision"`
	Commit       string   `json:"commit,omitempty"`
	Before       string   `json:"before"`
	After        string   `json:"after"`
	Warnings     []string `json:"warnings"`
	Profiles     []string `json:"profiles"`
	ExpiresAt    string   `json:"expiresAt"`
}

type definition struct {
	Request   Request
	Effective []byte
	Directory string
	Commit    string
}

type pending struct {
	Preview       Preview
	Definition    string
	RuntimeDigest string
	RollbackOf    string
	BaseIdentity  string
}

type Backend interface {
	Validate(context.Context, app.Project, string, []byte, map[string]string, ...string) ([]byte, error)
	Apply(context.Context, app.Project, func() error, func(string) error) error
	PinImages(context.Context, app.Project) (map[string]string, error)
	AppliedDefinition(context.Context, app.Project) ([]byte, error)
}

type ProjectStore interface {
	UpsertProject(context.Context, app.Project) (int64, error)
	GetProjectByID(context.Context, int64) (app.Project, error)
}

type Template struct {
	Name        string   `json:"name"`
	Version     int      `json:"version"`
	Description string   `json:"description"`
	Compose     string   `json:"compose"`
	Required    []string `json:"required"`
	SecretNames []string `json:"secretNames"`
}
