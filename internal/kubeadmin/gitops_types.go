package kubeadmin

import "encoding/json"

const (
	FluxVersion   = "2.10.0"
	ArgoCDVersion = "3.5.7"
)

type GitOpsScope struct {
	Context             string `json:"context"`
	Namespace           string `json:"namespace"`
	Controller          string `json:"controller"`
	ControllerNamespace string `json:"controllerNamespace"`
}

type GitOpsController struct {
	GitOpsScope
	Installed        bool     `json:"installed"`
	Supported        bool     `json:"supported"`
	Ready            bool     `json:"ready"`
	Managed          bool     `json:"managed"`
	Protected        bool     `json:"protected"`
	CanWrite         bool     `json:"canWrite"`
	CanBootstrap     bool     `json:"canBootstrap"`
	BootstrapVersion string   `json:"bootstrapVersion"`
	Capabilities     []string `json:"capabilities"`
	Message          string   `json:"message"`
}

type GitOpsSource struct {
	GitOpsScope
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	URL              string `json:"url"`
	CredentialStored bool   `json:"credentialStored"`
}

type GitOpsSourceRequest struct {
	GitOpsScope
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	URL      string  `json:"url"`
	Username string  `json:"username"`
	Password *string `json:"password,omitempty"`
}

type GitOpsRequest struct {
	GitOpsScope
	Action          string   `json:"action"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	UID             string   `json:"uid,omitempty"`
	ResourceVersion string   `json:"resourceVersion,omitempty"`
	Source          string   `json:"source,omitempty"`
	Revision        string   `json:"revision,omitempty"`
	Path            string   `json:"path,omitempty"`
	Chart           string   `json:"chart,omitempty"`
	Renderer        string   `json:"renderer,omitempty"`
	ServiceAccount  string   `json:"serviceAccount,omitempty"`
	Project         string   `json:"project,omitempty"`
	Dependencies    []string `json:"dependencies,omitempty"`
	Automatic       bool     `json:"automatic"`
	SelfHeal        bool     `json:"selfHeal"`
	Prune           bool     `json:"prune"`
	DeletePolicy    string   `json:"deletePolicy,omitempty"`
	Inventory       []Target `json:"inventory,omitempty"`
	HelmRelease     string   `json:"helmRelease,omitempty"`
}

type GitOpsPreview struct {
	GitOpsScope
	Token        string       `json:"token"`
	Action       string       `json:"action"`
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	Differences  []Difference `json:"differences"`
	Warnings     []string     `json:"warnings"`
	Consequences []string     `json:"consequences"`
	ExpiresAt    string       `json:"expiresAt"`
}

type gitOpsStep struct {
	Target     Target
	Data       json.RawMessage
	BeforeHash string
	Delete     bool
	Apply      bool
}

type gitOpsGuard struct {
	Target Target
	Hash   string
}

type storedGitOps struct {
	Request                GitOpsRequest
	Identity, SourceDigest string
	Schemas                map[string]string
	Steps                  []gitOpsStep
	Guards                 []gitOpsGuard
	Wait                   bool
	OneShot                bool
	ReconcileNonce         string
	ExpectedRevision       string
	PauseTargets           []Target
}

type pendingGitOps struct {
	Preview GitOpsPreview
	Account string
}

type GitOpsCondition struct {
	Type         string `json:"type"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	TransitionAt string `json:"transitionAt,omitempty"`
}

type GitOpsResource struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type GitOpsApplication struct {
	GitOpsScope
	Target            Target              `json:"target"`
	Name              string              `json:"name"`
	Kind              string              `json:"kind"`
	Source            string              `json:"source,omitempty"`
	DesiredRevision   string              `json:"desiredRevision"`
	SourceRevision    string              `json:"sourceRevision"`
	AppliedRevision   string              `json:"appliedRevision"`
	Phase             string              `json:"phase"`
	Health            string              `json:"health"`
	Drift             string              `json:"drift"`
	FailureStage      string              `json:"failureStage,omitempty"`
	Conditions        []GitOpsCondition   `json:"conditions"`
	DependenciesReady bool                `json:"dependenciesReady"`
	LastSuccess       string              `json:"lastSuccess,omitempty"`
	Automatic         bool                `json:"automatic"`
	SelfHeal          bool                `json:"selfHeal"`
	Prune             bool                `json:"prune"`
	Managed           bool                `json:"managed"`
	Adoption          string              `json:"adoption,omitempty"`
	Inventory         []GitOpsResource    `json:"inventory"`
	Message           string              `json:"message"`
	Configuration     GitOpsConfiguration `json:"configuration"`
}

type GitOpsConfiguration struct {
	Path           string   `json:"path,omitempty"`
	Chart          string   `json:"chart,omitempty"`
	Renderer       string   `json:"renderer,omitempty"`
	ServiceAccount string   `json:"serviceAccount,omitempty"`
	Project        string   `json:"project,omitempty"`
	Dependencies   []string `json:"dependencies"`
}
