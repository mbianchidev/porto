package kubeadmin

import (
	"encoding/json"
	"fmt"
)

type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string      { return e.Message }
func invalid(message string) error  { return &Error{400, message} }
func conflict(message string) error { return &Error{409, message} }

type Target struct {
	Context         string `json:"context"`
	Namespace       string `json:"namespace"`
	Group           string `json:"group"`
	Version         string `json:"version"`
	Resource        string `json:"resource"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

func (t Target) Description() string {
	scope := t.Namespace
	if scope == "" {
		scope = "cluster"
	}
	return fmt.Sprintf("%s / %s / %s/%s", t.Context, scope, t.Resource, t.Name)
}

type Resource struct {
	Group      string   `json:"group"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"`
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	Verbs      []string `json:"verbs"`
}

type Discovery struct {
	Resources []Resource `json:"resources"`
	Partial   bool       `json:"partial"`
	Message   string     `json:"message,omitempty"`
}

type Permission struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}
type Access struct {
	Target Target                `json:"target"`
	Verbs  map[string]Permission `json:"verbs"`
}

type Object struct {
	Target   Target          `json:"target"`
	Kind     string          `json:"kind"`
	Manifest json.RawMessage `json:"manifest"`
	Secret   bool            `json:"secret"`
	Revealed bool            `json:"revealed"`
}

type ObjectList struct {
	Resource Resource `json:"resource"`
	Items    []Object `json:"items"`
	Continue string   `json:"continue,omitempty"`
}

type NameList struct {
	Names    []string `json:"names"`
	Continue string   `json:"continue,omitempty"`
}

type Schema struct {
	State      string          `json:"state"`
	Version    string          `json:"version,omitempty"`
	Definition json.RawMessage `json:"definition,omitempty"`
	Message    string          `json:"message,omitempty"`
}

type ManifestRequest struct {
	Context       string `json:"context"`
	Namespace     string `json:"namespace"`
	Manifest      string `json:"manifest"`
	Mode          string `json:"mode"`
	SecretConsent bool   `json:"secretConsent"`
}

type Difference struct {
	Target   Target          `json:"target"`
	Kind     string          `json:"kind"`
	Before   json.RawMessage `json:"before"`
	After    json.RawMessage `json:"after"`
	Managers []string        `json:"managers"`
}

type Preview struct {
	Token       string       `json:"token"`
	Context     string       `json:"context"`
	Namespace   string       `json:"namespace"`
	Differences []Difference `json:"differences"`
	Warnings    []string     `json:"warnings"`
	ExpiresAt   string       `json:"expiresAt"`
}

type storedObject struct {
	Target Target
	Data   json.RawMessage
	Exists bool
}
type storedManifest struct {
	Context, Namespace, Mode, Identity string
	Objects                            []storedObject
}
type pendingManifest struct {
	Preview Preview
	Account string
}

type WorkloadRequest struct {
	Target
	Action             string `json:"action"`
	Replicas           *int32 `json:"replicas,omitempty"`
	Revision           int64  `json:"revision,omitempty"`
	Confirm            bool   `json:"confirm"`
	AcknowledgeStorage bool   `json:"acknowledgeStorage"`
}

type RolloutRevision struct {
	Revision  int64    `json:"revision"`
	CreatedAt string   `json:"createdAt"`
	Images    []string `json:"images"`
}
