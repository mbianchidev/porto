package kubeadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type nativeGitOpsConfig struct {
	name string
	raw  clientcmdapi.Config
}

func (c nativeGitOpsConfig) AdminConfig(name string) (*rest.Config, clientcmdapi.Config, error) {
	if name != c.name {
		return nil, clientcmdapi.Config{}, errors.New("the disposable fixture context must match exactly")
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(c.raw, name, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err == nil {
		config.Timeout = 30 * time.Second
	}
	return config, c.raw, err
}

func nativeGitOpsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic Git fixture command failed: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func nativeGitOpsSources(t *testing.T, host string) (string, []string, []byte) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeGitOpsGit(t, work, "init", "--initial-branch=main")
	nativeGitOpsGit(t, work, "config", "user.name", "Synthetic Fixture")
	nativeGitOpsGit(t, work, "config", "user.email", "fixture@example.invalid")
	write := func(stage string) {
		for _, controller := range []string{"flux", "argocd"} {
			directory := filepath.Join(work, controller)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- configmap.yaml\n", "configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + controller + "-gitops-synthetic\ndata:\n  stage: " + stage + "\n"}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		nativeGitOpsGit(t, work, "add", "flux", "argocd")
		nativeGitOpsGit(t, work, "commit", "--no-gpg-sign", "-m", "Synthetic "+stage)
	}
	write("first")
	first := nativeGitOpsGit(t, work, "rev-parse", "HEAD")
	write("second")
	second := nativeGitOpsGit(t, work, "rev-parse", "HEAD")
	nativeGitOpsGit(t, root, "clone", "--bare", work, filepath.Join(root, "fixture.git"))
	nativeGitOpsGit(t, root, "--git-dir", filepath.Join(root, "fixture.git"), "config", "http.receivepack", "false")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-gitops-fixture"}, DNSNames: []string{host}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	if ip := net.ParseIP(host); ip != nil {
		certificate.IPAddresses = append(certificate.IPAddresses, ip)
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	chart := nativeGitOpsChart(t)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(200)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "synthetic" || password != "synthetic-fixture-token" {
			w.Header().Set("WWW-Authenticate", `Basic realm="synthetic"`)
			http.Error(w, "source authentication required", 401)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/fixture.git/"):
			backend.ServeHTTP(w, r)
		case r.URL.Path == "/charts/index.yaml":
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = io.WriteString(w, "apiVersion: v1\nentries:\n  synthetic-chart:\n  - apiVersion: v2\n    name: synthetic-chart\n    version: 0.1.0\n    urls:\n    - synthetic-chart-0.1.0.tgz\n")
		case r.URL.Path == "/charts/synthetic-chart-0.1.0.tgz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(chart)
		default:
			http.NotFound(w, r)
		}
	})}
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	probe := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}}
	response, err := probe.Get("https://127.0.0.1:" + port + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	probe.CloseIdleConnections()
	if response.StatusCode != 200 {
		t.Fatal("synthetic source is not responsive")
	}
	return "https://" + net.JoinHostPort(host, port), []string{first, second}, ca
}

func nativeGitOpsChart(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	files := map[string]string{"synthetic-chart/Chart.yaml": "apiVersion: v2\nname: synthetic-chart\nversion: 0.1.0\n", "synthetic-chart/templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\ndata:\n  stage: chart\n"}
	for name, content := range files {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(archive, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func nativeGitOpsOperation(t *testing.T, manager *Manager, scope GitOpsScope, preview GitOpsPreview, controller bool) {
	t.Helper()
	var op operations.Operation
	var err error
	if controller {
		op, err = manager.ApplyGitOpsController(context.Background(), scope, preview.Token, true, true)
	} else {
		op, err = manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, err := manager.operations.Wait(ctx, op.ID)
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("native GitOps operation failed: %+v %v", result, err)
	}
}

func TestNativeGitOpsDelivery(t *testing.T) {
	filename, name, host := os.Getenv("PORTO_TEST_GITOPS_KUBECONFIG"), os.Getenv("PORTO_TEST_GITOPS_CONTEXT"), os.Getenv("PORTO_TEST_GITOPS_SOURCE_HOST")
	if filename == "" && name == "" {
		t.Skip("set PORTO_TEST_GITOPS_KUBECONFIG, PORTO_TEST_GITOPS_CONTEXT and PORTO_TEST_GITOPS_SOURCE_HOST for disposable native controller acceptance")
	}
	if filename == "" || host == "" || !strings.Contains(name, "porto-gitops-acceptance-") || os.Getenv("PORTO_TEST_GITOPS_CONFIRM") != "disposable" {
		t.Fatal("native GitOps acceptance requires an explicitly confirmed disposable porto-gitops-acceptance-* context and a cluster-reachable source hostname")
	}
	raw, err := clientcmd.LoadFromFile(filename)
	if err != nil {
		t.Fatal("unable to read the explicit disposable kubeconfig")
	}
	configs := nativeGitOpsConfig{name: name, raw: *raw}
	config, _, err := configs.AdminConfig(name)
	if err != nil {
		t.Fatal("unable to resolve the disposable identity")
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_HOME", t.TempDir())
	root := t.TempDir()
	ops := operations.New(filepath.Join(root, "operations"))
	manager := New(root, configs, ops, &fakeVault{values: map[string]string{}}, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := ops.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	address, commits, ca := nativeGitOpsSources(t, host)
	for _, controller := range []string{"flux", "argocd"} {
		t.Run(controller, func(t *testing.T) {
			scope := GitOpsScope{Context: name, Namespace: "porto-gitops-fixture-" + controller, Controller: controller, ControllerNamespace: "flux-system"}
			if controller == "argocd" {
				scope.ControllerNamespace = "argocd"
			}
			if _, err := core.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: scope.Namespace}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			preview, err := manager.PreviewGitOpsController(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "bootstrap"})
			if err != nil {
				t.Fatal(err)
			}
			nativeGitOpsOperation(t, manager, scope, preview, true)
			_, err = core.CoreV1().ServiceAccounts(scope.Namespace).Create(context.Background(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-reconciler"}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = core.RbacV1().Roles(scope.Namespace).Create(context.Background(), &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-reconciler"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets", "events"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}}}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = core.RbacV1().RoleBindings(scope.Namespace).Create(context.Background(), &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-reconciler"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "synthetic-reconciler"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "synthetic-reconciler", Namespace: scope.Namespace}}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			client := mustGitOpsClient(t, manager, scope)
			if controller == "argocd" {
				trust, err := core.CoreV1().ConfigMaps(scope.ControllerNamespace).Get(context.Background(), "argocd-tls-certs-cm", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if trust.Data == nil {
					trust.Data = map[string]string{}
				}
				trust.Data[host] = string(ca)
				if _, err := core.CoreV1().ConfigMaps(scope.ControllerNamespace).Update(context.Background(), trust, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				project := gitOpsObject(scope, "AppProject", "synthetic-project", "", map[string]any{"sourceRepos": []any{address + "/fixture.git", address + "/charts/"}, "destinations": []any{map[string]any{"server": "https://kubernetes.default.svc", "namespace": scope.Namespace}}, "clusterResourceWhitelist": []any{}, "namespaceResourceWhitelist": []any{map[string]any{"group": "", "kind": "ConfigMap"}}})
				if _, err := client.objects(gitOpsTarget(scope, "AppProject", "synthetic-project")).Create(context.Background(), project, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				deployment, err := core.AppsV1().Deployments(scope.ControllerNamespace).Get(context.Background(), "argocd-repo-server", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if deployment.Spec.Template.Annotations == nil {
					deployment.Spec.Template.Annotations = map[string]string{}
				}
				deployment.Spec.Template.Annotations["porto.dev/gitops-fixture-ca"] = time.Now().UTC().Format(time.RFC3339Nano)
				if _, err := core.AppsV1().Deployments(scope.ControllerNamespace).Update(context.Background(), deployment, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := manager.waitGitOpsController(context.Background(), scope, func(string) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			for _, sourceKind := range []string{"git", "helm"} {
				t.Run(sourceKind, func(t *testing.T) {
					password := "synthetic-fixture-token"
					sourceURL, kind, revision, path, chart := address+"/fixture.git", "Kustomization", commits[0], controller, ""
					if sourceKind == "helm" {
						sourceURL, kind, revision, path, chart = address+"/charts/", "HelmRelease", "0.1.0", "", "synthetic-chart"
					}
					if controller == "argocd" {
						kind = "Application"
					}
					sourceName := "synthetic-" + sourceKind
					appName := controller + "-synthetic-" + sourceKind
					if _, err := manager.SaveGitOpsSource(GitOpsSourceRequest{GitOpsScope: scope, Name: sourceName, Kind: sourceKind, URL: sourceURL, Username: "synthetic", Password: &password}); err != nil {
						t.Fatal(err)
					}
					request := GitOpsRequest{GitOpsScope: scope, Action: "install", Name: appName, Kind: kind, Source: sourceName, Revision: revision, Path: path, Chart: chart, Renderer: "kustomize", ServiceAccount: "synthetic-reconciler", Project: "synthetic-project"}
					preview, err := manager.PreviewGitOps(context.Background(), request)
					if err != nil {
						t.Fatal(err)
					}
					nativeGitOpsOperation(t, manager, scope, preview, false)
					trustSource := func() {
						if controller != "flux" {
							return
						}
						secret, err := core.CoreV1().Secrets(scope.Namespace).Get(context.Background(), appName+"-access", metav1.GetOptions{})
						if err != nil {
							t.Fatal(err)
						}
						if secret.Data == nil {
							secret.Data = map[string][]byte{}
						}
						secret.Data["ca.crt"] = ca
						if _, err := core.CoreV1().Secrets(scope.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
							t.Fatal(err)
						}
						sourceResourceKind := "GitRepository"
						if sourceKind == "helm" {
							sourceResourceKind = "HelmRepository"
						}
						target := gitOpsTarget(scope, sourceResourceKind, appName+"-source")
						source, err := client.objects(target).Get(context.Background(), target.Name, metav1.GetOptions{})
						if err != nil {
							t.Fatal(err)
						}
						if _, found, _ := unstructured.NestedMap(source.Object, "spec", "certSecretRef"); sourceKind == "helm" && !found {
							_ = unstructured.SetNestedMap(source.Object, map[string]any{"name": appName + "-access"}, "spec", "certSecretRef")
							if _, err := client.objects(target).Update(context.Background(), source, metav1.UpdateOptions{}); err != nil {
								t.Fatal(err)
							}
						}
					}
					reconcile := func(action, revision string) {
						current, err := manager.GitOpsApplication(context.Background(), scope, kind, appName)
						if err != nil {
							t.Fatal(err)
						}
						preview, err := manager.PreviewGitOps(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: action, Name: appName, Kind: kind, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion, Revision: revision})
						if err != nil {
							t.Fatal(err)
						}
						nativeGitOpsOperation(t, manager, scope, preview, false)
					}
					trustSource()
					reconcile("reconcile", "")
					resourceName := controller + "-gitops-synthetic"
					if sourceKind == "helm" {
						resourceName = appName
					}
					check := func(want string) {
						value, err := core.CoreV1().ConfigMaps(scope.Namespace).Get(context.Background(), resourceName, metav1.GetOptions{})
						if err != nil || value.Data["stage"] != want {
							t.Fatalf("native workload did not converge: want %s, error %v", want, err)
						}
					}
					if sourceKind == "git" {
						check("first")
						current, err := manager.GitOpsApplication(context.Background(), scope, kind, appName)
						if err != nil {
							t.Fatal(err)
						}
						request.Action, request.UID, request.ResourceVersion, request.Revision = "update", current.Target.UID, current.Target.ResourceVersion, commits[1]
						preview, err := manager.PreviewGitOps(context.Background(), request)
						if err != nil {
							t.Fatal(err)
						}
						nativeGitOpsOperation(t, manager, scope, preview, false)
						trustSource()
						reconcile("reconcile", "")
						check("second")
						reconcile("recover", commits[0])
						check("first")
					} else {
						check("chart")
					}
					current, err := manager.GitOpsApplication(context.Background(), scope, kind, appName)
					if err != nil {
						t.Fatal(err)
					}
					preview, err = manager.PreviewGitOps(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "delete", Name: appName, Kind: kind, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion, DeletePolicy: "orphan"})
					if err != nil {
						t.Fatal(err)
					}
					nativeGitOpsOperation(t, manager, scope, preview, false)
					if _, err := core.CoreV1().ConfigMaps(scope.Namespace).Get(context.Background(), resourceName, metav1.GetOptions{}); err != nil {
						t.Fatal("orphan deletion did not preserve the synthetic workload")
					}
				})
			}
		})
	}
}
