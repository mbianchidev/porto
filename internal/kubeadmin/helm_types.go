package kubeadmin

import "encoding/json"

const HelmVersion = "4.3.1"

type HelmScope struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
}
type HelmRepository struct {
	Name             string `json:"name"`
	URL              string `json:"url"`
	CredentialStored bool   `json:"credentialStored"`
}
type HelmRepositoryRequest struct {
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Username string  `json:"username"`
	Password *string `json:"password,omitempty"`
}
type HelmHook struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Phase string `json:"phase"`
}
type HelmRelease struct {
	Name       string     `json:"name"`
	Namespace  string     `json:"namespace"`
	Revision   int        `json:"revision"`
	Status     string     `json:"status"`
	Chart      string     `json:"chart"`
	AppVersion string     `json:"appVersion"`
	Updated    string     `json:"updated"`
	Hooks      []HelmHook `json:"hooks,omitempty"`
}
type HelmRequest struct {
	HelmScope
	Release       string `json:"release"`
	Action        string `json:"action"`
	Repository    string `json:"repository"`
	Chart         string `json:"chart"`
	Version       string `json:"version"`
	ChartID       string `json:"chartId,omitempty"`
	Values        string `json:"values"`
	Revision      int    `json:"revision,omitempty"`
	SecretConsent bool   `json:"secretConsent"`
}
type HelmPreview struct {
	Token string `json:"token"`
	HelmScope
	Release             string          `json:"release"`
	Action              string          `json:"action"`
	Revision            int             `json:"revision"`
	ChartID             string          `json:"chartId,omitempty"`
	Before              json.RawMessage `json:"before"`
	After               json.RawMessage `json:"after"`
	Warnings            []string        `json:"warnings"`
	StorageConsequences []string        `json:"storageConsequences"`
	ExpiresAt           string          `json:"expiresAt"`
}
type storedHelm struct {
	Request           HelmRequest
	Identity, ChartID string
	Revision          int
	ReleaseIdentity   string
}
type pendingHelm struct {
	Preview HelmPreview
	Account string
}
