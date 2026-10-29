package kubeadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mbianchidev/porto/internal/credentials"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/operations"
	authorization "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type ConfigProvider interface {
	AdminConfig(string) (*rest.Config, clientcmdapi.Config, error)
}
type Manager struct {
	root         string
	configs      ConfigProvider
	operations   *operations.Manager
	vault        *credentials.EncryptedFiles
	runner       localtools.Runner
	mu           sync.Mutex
	previews     map[string]pendingManifest
	helmPreviews map[string]pendingHelm
	repoMu       sync.Mutex
}

func New(root string, configs ConfigProvider, ops *operations.Manager, vault credentials.Store, runner localtools.Runner) *Manager {
	if runner == nil {
		runner = localtools.ExecRunner{}
	}
	return &Manager{root: root, configs: configs, operations: ops, vault: credentials.NewEncryptedFiles(filepath.Join(root, "sealed"), "dev.mbianchi.porto.kubernetes-admin", vault), runner: runner, previews: map[string]pendingManifest{}, helmPreviews: map[string]pendingHelm{}}
}

type warningCount struct{ count atomic.Int32 }

func (w *warningCount) HandleWarningHeader(_ int, _, _ string) { w.count.Add(1) }

type session struct {
	dynamic   dynamic.Interface
	metadata  metadata.Interface
	discovery *discovery.DiscoveryClient
	core      kubernetes.Interface
	identity  string
	warnings  *warningCount
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (m *Manager) client(contextName string) (*session, error) {
	if strings.TrimSpace(contextName) == "" {
		return nil, invalid("Select an explicit Kubernetes context; the global default is never used.")
	}
	config, raw, err := m.configs.AdminConfig(contextName)
	if err != nil {
		return nil, &Error{503, "The selected managed Kubernetes identity is unavailable. Check its kubeconfig and authentication; no other context was used."}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	config = rest.CopyConfig(config)
	warnings := &warningCount{}
	config.WarningHandler = warnings
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, apiError(err)
	}
	metadataClient, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, apiError(err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, apiError(err)
	}
	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, apiError(err)
	}
	return &session{dynamic: dynamicClient, metadata: metadataClient, discovery: discoveryClient, core: coreClient, identity: hex.EncodeToString(hash[:]), warnings: warnings}, nil
}

func apiError(err error) error {
	if err == nil {
		return nil
	}
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	code, message := http.StatusServiceUnavailable, "The selected Kubernetes API or identity is unavailable. Check cluster state, authentication and connectivity."
	switch {
	case apierrors.IsForbidden(err):
		code, message = 403, "Kubernetes RBAC denied this operation for the selected identity. No administrator identity or unrelated context was substituted."
	case apierrors.IsUnauthorized(err):
		code, message = 401, "The selected Kubernetes identity is not authenticated."
	case apierrors.IsConflict(err):
		code, message = 409, "Kubernetes reported a resourceVersion or field-manager conflict. Refresh the object and review ownership; force-overwrite is not used."
	case apierrors.IsAlreadyExists(err):
		code, message = 409, "The selected Kubernetes object already exists. Refresh and preview an explicit edit."
	case apierrors.IsNotFound(err):
		code, message = 404, "The selected Kubernetes API version or object no longer exists. Refresh API discovery and resource identity."
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		code, message = 422, "Kubernetes schema validation or admission rejected the requested fields. Review the selected served schema and dry-run input; sensitive API response values were withheld."
	case apierrors.IsMethodNotSupported(err):
		code, message = 405, "This served resource version does not support the requested operation."
	}
	return &Error{code, message}
}

func mutationError(err error, applied int) error {
	safe := apiError(err)
	message := publicError(safe)
	var known *Error
	if applied > 0 {
		return operations.Partial(message + " Earlier resources were applied; inspect them explicitly. No automatic rollback or full success is implied.")
	}
	if errors.As(safe, &known) && known.Code >= 500 {
		return operations.Partial(message + " Mutation completion could not be confirmed. Inspect the exact selected object: the request may already have been applied. No rollback is implied.")
	}
	return operations.Failure(message)
}

func publicError(err error) string {
	if err == nil {
		return ""
	}
	var known *Error
	if errors.As(err, &known) {
		return known.Message
	}
	return operations.PublicMessage(err)
}

func contextKey(contextName string) string { return "kubernetes:" + contextName }

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)

func validateTarget(target Target, requireName bool) error {
	if target.Context == "" || target.Version == "" || target.Resource == "" {
		return invalid("Context, served API version and resource are required.")
	}
	for _, value := range []string{target.Version, target.Resource} {
		if !safeSegment.MatchString(value) || strings.Contains(value, "..") {
			return invalid("Invalid Kubernetes API resource or version.")
		}
	}
	if target.Group != "" && (!safeSegment.MatchString(target.Group) || strings.Contains(target.Group, "..")) {
		return invalid("Invalid Kubernetes API group.")
	}
	if target.Namespace != "" && (!safeSegment.MatchString(target.Namespace) || strings.Contains(target.Namespace, ":")) {
		return invalid("An explicit valid namespace or cluster scope is required.")
	}
	if requireName && (!safeSegment.MatchString(target.Name) || strings.Contains(target.Name, "..")) {
		return invalid("An explicit valid resource name is required.")
	}
	return nil
}

func (s *session) resource(target Target) (Resource, error) {
	if err := validateTarget(target, false); err != nil {
		return Resource{}, err
	}
	groupVersion := target.Version
	if target.Group != "" {
		groupVersion = target.Group + "/" + target.Version
	}
	resources, err := s.discovery.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return Resource{}, apiError(err)
	}
	for _, resource := range resources.APIResources {
		if resource.Name != target.Resource {
			continue
		}
		if resource.Namespaced && target.Namespace == "" {
			return Resource{}, invalid("Select an explicit namespace for this namespaced resource.")
		}
		if !resource.Namespaced && target.Namespace != "" {
			return Resource{}, invalid("Select explicit cluster scope for this cluster-scoped resource; namespace will not be ignored.")
		}
		return Resource{Group: target.Group, Version: target.Version, Resource: resource.Name, Kind: resource.Kind, Namespaced: resource.Namespaced, Verbs: resource.Verbs}, nil
	}
	return Resource{}, &Error{404, "The resource is not served in the selected API version. Refresh discovery."}
}

func gvr(target Target) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: target.Group, Version: target.Version, Resource: target.Resource}
}
func (s *session) objects(target Target) dynamic.ResourceInterface {
	resource := s.dynamic.Resource(gvr(target))
	if target.Namespace != "" {
		return resource.Namespace(target.Namespace)
	}
	return resource
}

func (s *session) permission(ctx context.Context, target Target, verb, subresource string) (Permission, error) {
	review, err := s.core.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorization.SelfSubjectAccessReview{Spec: authorization.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorization.ResourceAttributes{Namespace: target.Namespace, Verb: verb, Group: target.Group, Version: target.Version, Resource: target.Resource, Subresource: subresource, Name: target.Name}}}, metav1.CreateOptions{})
	if err != nil {
		return Permission{}, apiError(err)
	}
	reason := ""
	if !review.Status.Allowed {
		reason = "The selected Kubernetes identity does not have this permission."
	}
	if review.Status.EvaluationError != "" {
		return Permission{Reason: "Kubernetes could not evaluate this permission reliably."}, &Error{503, "Kubernetes authorization evaluation was inconclusive; the operation was not authorized locally."}
	}
	return Permission{Allowed: review.Status.Allowed, Reason: reason}, nil
}

func (s *session) require(ctx context.Context, target Target, verb, subresource string) error {
	permission, err := s.permission(ctx, target, verb, subresource)
	if err != nil {
		return err
	}
	if !permission.Allowed {
		return &Error{403, fmt.Sprintf("Kubernetes RBAC does not allow %s on the selected %s%s.", verb, target.Resource, func() string {
			if subresource != "" {
				return "/" + subresource
			}
			return ""
		}())}
	}
	return nil
}
