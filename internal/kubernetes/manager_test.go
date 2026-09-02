package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

type fakeRunner struct {
	mu        sync.Mutex
	commands  []runtimes.Command
	instances map[string]bool
	handler   func(runtimes.Command) ([]byte, error)
}

type deadlineRecordingRunner struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{instances: map[string]bool{}}
}

func (f *fakeRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if f.handler != nil {
		return f.handler(command)
	}
	joined := strings.Join(command.Args, " ")
	switch command.Name {
	case "kubectl":
		switch {
		case strings.Contains(joined, "version -o json"):
			return []byte(`{"clientVersion":{"gitVersion":"v1.33.0"},"serverVersion":{"gitVersion":"v1.33.1"}}`), nil
		case strings.Contains(joined, "config current-context"):
			return []byte("porto-dev\n"), nil
		case strings.Contains(joined, "config view --raw -o json"):
			return []byte(`{"apiVersion":"v1","kind":"Config","current-context":"porto-dev","clusters":[{"name":"porto-dev","cluster":{"server":"https://127.0.0.1:54321"}}],"contexts":[{"name":"porto-dev","context":{"cluster":"porto-dev","user":"porto-dev"}}],"users":[{"name":"porto-dev","user":{"token":"test"}}]}`), nil
		case strings.Contains(joined, "config view"):
			return []byte(`{"current-context":"porto-dev","contexts":[{"name":"porto-dev","context":{"cluster":"porto-dev","user":"porto-dev","namespace":"default"}}]}`), nil
		case strings.Contains(joined, "get pods"):
			return []byte(`{"items":[{"metadata":{"name":"api-1","namespace":"dev","uid":"pod-uid","creationTimestamp":"2026-08-30T20:00:00Z"},"spec":{"nodeName":"worker-1","containers":[{"name":"api","image":"porto/api:latest"}]},"status":{"phase":"Running","podIP":"10.42.0.2","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"api","ready":true,"restartCount":1,"state":{"running":{}}}]}}]}`), nil
		case strings.Contains(joined, "get services"):
			return []byte(`{"items":[]}`), nil
		case strings.Contains(joined, "get configmap api-config"):
			return []byte(`{"metadata":{"name":"api-config","namespace":"dev","creationTimestamp":"2026-08-30T20:00:00Z","resourceVersion":"42","labels":{"app":"api"},"annotations":{"owner":"platform"}},"immutable":true,"data":{"LOG_LEVEL":"debug","config.yaml":"port: 8080"},"binaryData":{"logo.png":"aW1hZ2U="}}`), nil
		case strings.Contains(joined, "get configmaps"):
			return []byte("dev\tapi-config\ttrue\t2026-08-30T20:00:00Z\t42\tLOG_LEVEL,config.yaml,\tlogo.png,\n"), nil
		case strings.Contains(joined, "get secrets"):
			return []byte("dev\tapi-credentials\tOpaque\ttrue\t2026-08-30T20:00:00Z\tpassword,username,\n"), nil
		case strings.Contains(joined, "get nodes"):
			return []byte(`{"items":[]}`), nil
		case strings.Contains(joined, "config rename-context"):
			return []byte("Context renamed"), nil
		}
	case "limactl":
		if len(command.Args) > 0 {
			switch command.Args[0] {
			case "create":
				for index, arg := range command.Args {
					if arg == "--name" && index+1 < len(command.Args) {
						f.instances[command.Args[index+1]] = true
					}
				}
				return nil, nil
			case "start", "stop":
				return nil, nil
			case "delete":
				name := command.Args[len(command.Args)-1]
				delete(f.instances, name)
				return nil, nil
			case "list":
				var output strings.Builder
				for name := range f.instances {
					output.WriteString(`{"name":"` + name + `","status":"Running","arch":"aarch64","cpus":2,"memory":2147483648,"disk":21474836480}`)
					output.WriteByte('\n')
				}
				return []byte(output.String()), nil
			case "shell":
				switch {
				case strings.Contains(joined, "hostname -I"):
					return []byte("192.168.105.2\n"), nil
				case strings.Contains(joined, "server/node-token"):
					return []byte("test-token\n"), nil
				case strings.Contains(joined, "k0s token create"):
					return []byte("test-k0s-token\n"), nil
				case strings.Contains(joined, "k3s.yaml"):
					return []byte("apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\ncontexts:\n- context:\n    cluster: default\n    user: default\n  name: default\ncurrent-context: default\n"), nil
				case strings.Contains(joined, "k0s kubeconfig admin"):
					return []byte("apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\ncontexts:\n- context:\n    cluster: default\n    user: default\n  name: default\ncurrent-context: default\n"), nil
				default:
					return nil, nil
				}
			}
		}
	}
	return nil, nil
}

func (r *deadlineRecordingRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("capability probe has no deadline")
	}
	r.mu.Lock()
	r.deadlines = append(r.deadlines, deadline)
	r.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	args := strings.Join(command.Args, " ")
	switch {
	case strings.Contains(args, "-- sh -c exit 0"), strings.Contains(args, "command -v"):
		return nil, nil
	case strings.Contains(args, "-- bash -c exit 0"):
		return []byte(`exec: "bash": executable file not found in $PATH`), errors.New("exit status 1")
	case strings.Contains(args, "-- ash -c exit 0"):
		return []byte(`exec: "ash": executable file not found in $PATH`), errors.New("exit status 1")
	default:
		return nil, errors.New("unexpected command")
	}
}

func TestStatusAndPodDecoding(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.KubernetesClusterFileToken("dev")+".yaml"), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewWithKubeconfigRoot(newFakeRunner(), dir)
	status := manager.Status(context.Background(), "porto-dev")
	if !status.Available || status.Context != "porto-dev" || status.ServerVersion != "v1.33.1" {
		t.Fatalf("unexpected status: %+v", status)
	}
	pods, err := manager.Pods(context.Background(), "porto-dev", "all")
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods) != 1 || pods[0].UID != "pod-uid" || !pods[0].PodReady || pods[0].Ready != "1/1" || pods[0].Restarts != 1 || pods[0].Containers[0].State != "running" {
		t.Fatalf("unexpected pods: %+v", pods)
	}
	contexts, err := manager.Contexts(context.Background())
	if err != nil {
		t.Fatalf("list contexts: %v", err)
	}
	if len(contexts) != 1 || contexts[0].Cluster != "porto-dev" || !contexts[0].Current {
		t.Fatalf("unexpected contexts: %+v", contexts)
	}
}

func TestManagedContextUsesPrivateKubeconfig(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, config.KubernetesClusterFileToken("dev")+".yaml")
	if err := os.WriteFile(kubeconfigPath, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := newFakeRunner()
	manager := NewWithKubeconfigRoot(runner, dir)
	if _, err := manager.Pods(context.Background(), "porto-dev", "all"); err != nil {
		t.Fatalf("list managed pods: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	args := runner.commands[len(runner.commands)-1].Args
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--kubeconfig "+kubeconfigPath+" --context porto-dev") {
		t.Fatalf("managed context did not use private kubeconfig: %v", args)
	}
}

func TestManagedK3sContextMigratesToProviderQualifiedName(t *testing.T) {
	dir := t.TempDir()
	token := config.KubernetesClusterFileToken("local")
	kubeconfigPath := filepath.Join(dir, token+".yaml")
	if err := os.WriteFile(kubeconfigPath, []byte("old kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(ClusterRequest{Name: "local", Provider: "k3s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, token+".json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name == "kubectl" {
			joined := strings.Join(command.Args, " ")
			if strings.Contains(joined, "config view --raw -o json") {
				return []byte(`{
				"current-context":"porto-local",
				"clusters":[{"name":"porto-local","cluster":{"server":"https://127.0.0.1:6443","certificate-authority-data":"Q0E="}}],
				"contexts":[{"name":"porto-local","context":{"cluster":"porto-local","user":"porto-local"}}],
				"users":[{"name":"porto-local","user":{"client-certificate-data":"Q0VSVA==","client-key-data":"S0VZ"}}]
			}`), nil
			}
			if strings.Contains(joined, "get pods") {
				return []byte(`{"items":[]}`), nil
			}
		}
		return nil, errors.New("unexpected command")
	}

	contexts, err := NewWithKubeconfigRoot(runner, dir).Contexts(context.Background())
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if len(contexts) != 1 || contexts[0].Name != "porto-k3s-local" || contexts[0].Cluster != "porto-k3s-local" {
		t.Fatalf("contexts = %+v", contexts)
	}
	updated, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "porto-local") || !strings.Contains(string(updated), "porto-k3s-local") {
		t.Fatalf("kubeconfig was not migrated: %s", updated)
	}
	if !strings.Contains(string(updated), `"certificate-authority-data": "Q0E="`) ||
		!strings.Contains(string(updated), `"client-certificate-data": "Q0VSVA=="`) {
		t.Fatalf("kubeconfig credentials were not preserved: %s", updated)
	}
	manager := NewWithKubeconfigRoot(runner, dir)
	if _, err := manager.Pods(context.Background(), "porto-k3s-local", "all"); err != nil {
		t.Fatalf("Pods: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	lastCommand := strings.Join(runner.commands[len(runner.commands)-1].Args, " ")
	if !strings.Contains(lastCommand, "--kubeconfig "+kubeconfigPath+" --context porto-k3s-local") {
		t.Fatalf("provider-qualified context did not use its private kubeconfig: %s", lastCommand)
	}
	foundRawMigration := false
	for _, command := range runner.commands {
		if strings.Contains(strings.Join(command.Args, " "), "config view --raw -o json") {
			foundRawMigration = true
			break
		}
	}
	if !foundRawMigration {
		t.Fatal("managed kubeconfig migration did not request raw credential data")
	}
}

func TestConfigMapsAndSecretsDecoding(t *testing.T) {
	manager := New(newFakeRunner())

	configMaps, err := manager.ConfigMaps(context.Background(), "porto-dev", "dev")
	if err != nil {
		t.Fatalf("list config maps: %v", err)
	}
	if len(configMaps) != 1 || configMaps[0].Name != "api-config" || !configMaps[0].Immutable {
		t.Fatalf("unexpected config maps: %+v", configMaps)
	}
	if strings.Join(configMaps[0].Keys, ",") != "LOG_LEVEL,config.yaml" || strings.Join(configMaps[0].BinaryKeys, ",") != "logo.png" {
		t.Fatalf("unexpected config map keys: %+v", configMaps[0])
	}
	configMap, err := manager.ConfigMap(context.Background(), "porto-dev", "dev", "api-config")
	if err != nil {
		t.Fatalf("get config map: %v", err)
	}
	if configMap.Data["LOG_LEVEL"] != "debug" ||
		configMap.BinaryData["logo.png"] != "aW1hZ2U=" ||
		configMap.ResourceVersion != "42" ||
		configMap.Labels["app"] != "api" ||
		configMap.Annotations["owner"] != "platform" ||
		strings.Join(configMap.BinaryKeys, ",") != "logo.png" {
		t.Fatalf("unexpected config map data: %+v", configMap)
	}

	secrets, err := manager.Secrets(context.Background(), "porto-dev", "dev")
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets) != 1 || secrets[0].Name != "api-credentials" || secrets[0].Type != "Opaque" || !secrets[0].Immutable {
		t.Fatalf("unexpected secrets: %+v", secrets)
	}
	if strings.Join(secrets[0].Keys, ",") != "password,username" {
		t.Fatalf("unexpected secret keys: %+v", secrets[0].Keys)
	}
	encoded, err := json.Marshal(secrets)
	if err != nil {
		t.Fatalf("encode secrets: %v", err)
	}
	if strings.Contains(string(encoded), "c3VwZXItc2VjcmV0") || strings.Contains(string(encoded), "super-secret") {
		t.Fatalf("secret values leaked into response: %s", encoded)
	}

	runner := manager.runner.(*fakeRunner)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var configMapListSafe, secretListSafe bool
	for _, command := range runner.commands {
		joined := strings.Join(command.Args, " ")
		if strings.Contains(joined, "get configmaps") {
			configMapListSafe = strings.Contains(joined, "go-template=") && !strings.Contains(joined, "-o json")
		}
		if strings.Contains(joined, "get secrets") {
			secretListSafe = strings.Contains(joined, "go-template=") && !strings.Contains(joined, "-o json")
		}
	}
	if !configMapListSafe || !secretListSafe {
		t.Fatalf("resource inventories requested complete values: %+v", runner.commands)
	}
}

func TestServicesReturnEmptyExternalIPArray(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Contains(strings.Join(command.Args, " "), "get services") {
			return []byte(`{"items":[{"metadata":{"name":"metrics-server","namespace":"kube-system"},"spec":{"type":"ClusterIP","clusterIP":"10.96.0.10","ports":[]},"status":{}}]}`), nil
		}
		return nil, errors.New("unexpected command")
	}
	services, err := New(runner).Services(context.Background(), "porto-dev", "kube-system")
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	if len(services) != 1 || services[0].ExternalIPs == nil {
		t.Fatalf("unexpected services: %+v", services)
	}
	encoded, err := json.Marshal(services)
	if err != nil {
		t.Fatalf("encode services: %v", err)
	}
	if !strings.Contains(string(encoded), `"externalIPs":[]`) {
		t.Fatalf("external IPs were not encoded as an array: %s", encoded)
	}
}

func TestStatsIdentifiesUnavailableMetricsAPI(t *testing.T) {
	for _, message := range []string{
		"error: Metrics API not available",
		"Error from server (ServiceUnavailable): the server is currently unable to handle the request (get pods.metrics.k8s.io)",
	} {
		runner := newFakeRunner()
		runner.handler = func(command runtimes.Command) ([]byte, error) {
			if command.Name == "kubectl" {
				joined := strings.Join(command.Args, " ")
				switch {
				case strings.Contains(joined, "get pod"):
					return []byte(`{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"ready":true}]}}`), nil
				case strings.Contains(joined, "top pod"):
					return []byte(message), errors.New("exit status 1")
				}
			}
			return nil, errors.New("unexpected command")
		}

		_, err := New(runner).Stats(context.Background(), "porto-dev", "default", "api")
		if !errors.Is(err, ErrMetricsUnavailable) {
			t.Fatalf("message %q: error = %v, want ErrMetricsUnavailable", message, err)
		}
	}
}

func TestStatsSkipsPodsThatAreNotReadyOrNoLongerExist(t *testing.T) {
	for name, podResult := range map[string]struct {
		output []byte
		err    error
	}{
		"succeeded": {
			output: []byte(`{"status":{"phase":"Succeeded","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"ready":false}]}}`),
		},
		"not-found": {
			output: []byte(`Error from server (NotFound): pods "job" not found`),
			err:    errors.New("exit status 1"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			topCalled := false
			runner := newFakeRunner()
			runner.handler = func(command runtimes.Command) ([]byte, error) {
				joined := strings.Join(command.Args, " ")
				switch {
				case command.Name == "kubectl" && strings.Contains(joined, "get pod"):
					return podResult.output, podResult.err
				case command.Name == "kubectl" && strings.Contains(joined, "top pod"):
					topCalled = true
					return nil, errors.New("top should not be called")
				default:
					return nil, errors.New("unexpected command")
				}
			}

			stats, err := New(runner).Stats(context.Background(), "porto-dev", "default", "job")
			if err != nil {
				t.Fatalf("Stats: %v", err)
			}
			if topCalled || len(stats) != 0 {
				t.Fatalf("topCalled=%t stats=%+v", topCalled, stats)
			}
		})
	}
}

func TestResourceListTemplatesExcludeValues(t *testing.T) {
	for _, test := range []struct {
		name     string
		template string
		input    string
		want     string
		forbid   string
	}{
		{
			name:     "config maps",
			template: configMapListTemplate,
			input:    `{"items":[{"metadata":{"name":"api-config","namespace":"dev","creationTimestamp":"2026-08-30T20:00:00Z","resourceVersion":"42"},"data":{"config.yaml":"password: secret"},"binaryData":{"logo.png":"aW1hZ2U="}}]}`,
			want:     "dev\tapi-config\tfalse\t2026-08-30T20:00:00Z\t42\tconfig.yaml,\tlogo.png,\n",
			forbid:   "password: secret",
		},
		{
			name:     "secrets",
			template: secretListTemplate,
			input:    `{"items":[{"metadata":{"name":"api-credentials","namespace":"dev","creationTimestamp":"2026-08-30T20:00:00Z"},"type":"Opaque","data":{"password":"c3VwZXItc2VjcmV0"}}]}`,
			want:     "dev\tapi-credentials\tOpaque\tfalse\t2026-08-30T20:00:00Z\tpassword,\n",
			forbid:   "c3VwZXItc2VjcmV0",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input any
			if err := json.Unmarshal([]byte(test.input), &input); err != nil {
				t.Fatalf("decode template input: %v", err)
			}
			parsed, err := template.New(test.name).Parse(test.template)
			if err != nil {
				t.Fatalf("parse template: %v", err)
			}
			var output bytes.Buffer
			if err := parsed.Execute(&output, input); err != nil {
				t.Fatalf("execute template: %v", err)
			}
			if output.String() != test.want {
				t.Fatalf("template output = %q, want %q", output.String(), test.want)
			}
			if strings.Contains(output.String(), test.forbid) {
				t.Fatalf("template leaked value %q: %s", test.forbid, output.String())
			}
		})
	}
}

func TestStatusDoesNotUseUnmanagedCurrentContext(t *testing.T) {
	runner := newFakeRunner()
	status := NewWithKubeconfigRoot(runner, t.TempDir()).Status(context.Background(), "")
	if status.Available || status.Context != "" || !strings.Contains(status.Message, "No Porto-managed") {
		t.Fatalf("unexpected unmanaged-context status: %+v", status)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 0 {
		t.Fatalf("status consulted global kubectl context: %+v", runner.commands)
	}
}

func TestStatusCondensesOfflineContextError(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, config.KubernetesClusterFileToken("offline")+".yaml")
	if err := os.WriteFile(kubeconfigPath, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name == "kubectl" && strings.Contains(strings.Join(command.Args, " "), "version -o json") {
			return []byte(`{"clientVersion":{"gitVersion":"v1.36.1"}}` + "\nThe connection to the server 127.0.0.1:49800 was refused"), errors.New("exit status 1")
		}
		return nil, nil
	}
	status := NewWithKubeconfigRoot(runner, dir).Status(context.Background(), "porto-offline")
	if status.Available {
		t.Fatalf("offline context was reported available: %+v", status)
	}
	if status.Message != "Kubernetes context porto-offline is offline; start its managed cluster or select another context" {
		t.Fatalf("unexpected offline message: %q", status.Message)
	}
}

func TestStatusSelectsReachableManagedContext(t *testing.T) {
	dir := t.TempDir()
	tokens := map[string]string{
		"offline": config.KubernetesClusterFileToken("offline"),
		"online":  config.KubernetesClusterFileToken("online"),
	}
	for name, token := range tokens {
		if err := os.WriteFile(filepath.Join(dir, token+".yaml"), []byte("apiVersion: v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		metadata, err := json.Marshal(ClusterRequest{Name: name, Provider: "k0s"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, token+".json"), metadata, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "kubectl" && strings.Contains(joined, "config view --raw -o json"):
			name := "offline"
			if strings.Contains(joined, tokens["online"]+".yaml") {
				name = "online"
			}
			contextName := "porto-" + name
			return []byte(fmt.Sprintf(
				`{"current-context":%q,"contexts":[{"name":%q,"context":{"cluster":%q,"user":%q}}]}`,
				contextName,
				contextName,
				contextName,
				contextName,
			)), nil
		case command.Name == "kubectl" && strings.Contains(joined, "--context porto-offline version -o json"):
			return []byte("The connection to the server 127.0.0.1:49800 was refused"), errors.New("exit status 1")
		case command.Name == "kubectl" && strings.Contains(joined, "--context porto-online version -o json"):
			return []byte(`{"clientVersion":{"gitVersion":"v1.36.1"},"serverVersion":{"gitVersion":"v1.36.3"}}`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s %s", command.Name, joined)
		}
	}
	status := NewWithKubeconfigRoot(runner, dir).Status(context.Background(), "")
	if !status.Available || status.Context != "porto-online" || status.ServerVersion != "v1.36.3" {
		t.Fatalf("reachable managed context was not selected: %+v", status)
	}
}

func TestFileOperationsKeepPathAsArgument(t *testing.T) {
	runner := newFakeRunner()
	manager := New(runner)
	if err := manager.WriteFile(
		context.Background(),
		"porto-dev",
		"dev",
		"api-1",
		"api",
		"/app/config.json",
		[]byte(`{"ok":true}`),
	); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 1 {
		t.Fatalf("unexpected commands: %+v", runner.commands)
	}
	command := runner.commands[0]
	if command.Args[len(command.Args)-1] != "/app/config.json" {
		t.Fatalf("path was not passed as a separate argument: %+v", command.Args)
	}
	if string(command.Stdin) != `{"ok":true}` {
		t.Fatalf("unexpected stdin: %q", command.Stdin)
	}
}

func TestFilesReportsShelllessContainer(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(runtimes.Command) ([]byte, error) {
		return []byte(`error executing command in container: exec: "sh": executable file not found in $PATH`), errors.New("exit status 1")
	}
	_, err := New(runner).Files(context.Background(), "porto-dev", "kube-system", "coredns", "coredns", "/")
	if err == nil {
		t.Fatal("expected shellless container error")
	}
	if err.Error() != "container does not include sh; file inspection is unavailable for shellless images" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestContainerCapabilitiesDetectShelllessImage(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		for _, shell := range []string{"sh", "bash", "ash"} {
			if strings.Contains(args, "-- "+shell+" -c exit 0") {
				return []byte(`exec: "` + shell + `": executable file not found in $PATH`), errors.New("exit status 1")
			}
		}
		return nil, errors.New("unexpected command")
	}
	capabilities, err := New(runner).ContainerCapabilities(
		context.Background(),
		"porto-dev",
		"kube-system",
		"metrics-server",
		"metrics-server",
		false,
	)
	if err != nil {
		t.Fatalf("inspect capabilities: %v", err)
	}
	if len(capabilities.Shells) != 0 || capabilities.FileInspection {
		t.Fatalf("unexpected shellless capabilities: %+v", capabilities)
	}
	if !strings.Contains(capabilities.Message, "shellless") {
		t.Fatalf("unexpected capability message: %q", capabilities.Message)
	}
}

func TestContainerCapabilitiesChecksFileUtilities(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(args, "-- sh -c exit 0"):
			return nil, nil
		case strings.Contains(args, "-- bash -c exit 0"):
			return []byte(`exec: "bash": executable file not found in $PATH`), errors.New("exit status 1")
		case strings.Contains(args, "-- ash -c exit 0"):
			return []byte(`exec: "ash": executable file not found in $PATH`), errors.New("exit status 1")
		case strings.Contains(args, "command -v"):
			return []byte("head\n"), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}
	capabilities, err := New(runner).ContainerCapabilities(
		context.Background(),
		"porto-dev",
		"default",
		"api",
		"api",
		true,
	)
	if err != nil {
		t.Fatalf("inspect capabilities: %v", err)
	}
	if strings.Join(capabilities.Shells, ",") != "sh" || capabilities.FileInspection {
		t.Fatalf("unexpected capabilities: %+v", capabilities)
	}
	if !strings.Contains(capabilities.Message, "head") {
		t.Fatalf("missing utility was not reported: %q", capabilities.Message)
	}
}

func TestContainerCapabilitiesDoesNotHideMissingKubectl(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(runtimes.Command) ([]byte, error) {
		return nil, errors.New(`exec: "kubectl": executable file not found in $PATH`)
	}
	_, err := New(runner).ContainerCapabilities(
		context.Background(),
		"porto-dev",
		"default",
		"api",
		"api",
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "kubectl") {
		t.Fatalf("missing kubectl was hidden: %v", err)
	}
}

func TestContainerCapabilitiesTreatsCrossedShellErrorsAsUnavailable(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(runtimes.Command) ([]byte, error) {
		return []byte(`exec: "ash": executable file not found in $PATH`), errors.New("exit status 1")
	}
	capabilities, err := New(runner).ContainerCapabilities(
		context.Background(),
		"porto-dev",
		"kube-system",
		"konnectivity-agent",
		"konnectivity-agent",
		false,
	)
	if err != nil {
		t.Fatalf("inspect capabilities: %v", err)
	}
	if len(capabilities.Shells) != 0 {
		t.Fatalf("unexpected shells: %+v", capabilities)
	}
}

func TestContainerCapabilitiesCachesProbeResult(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		if strings.Contains(args, "-- sh -c exit 0") {
			return nil, nil
		}
		if strings.Contains(args, "-- bash -c exit 0") {
			return []byte(`exec: "bash": executable file not found in $PATH`), errors.New("exit status 1")
		}
		return []byte(`exec: "ash": executable file not found in $PATH`), errors.New("exit status 1")
	}
	manager := New(runner)
	for range 2 {
		if _, err := manager.ContainerCapabilities(context.Background(), "porto-dev", "default", "api", "api", false); err != nil {
			t.Fatalf("inspect capabilities: %v", err)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 3 {
		t.Fatalf("capability probes = %d, want 3 cached probes", len(runner.commands))
	}
}

func TestContainerCapabilitiesBoundsEachSequentialProbe(t *testing.T) {
	runner := &deadlineRecordingRunner{}
	manager := New(runner)
	manager.capabilityProbeTimeout = time.Second
	capabilities, err := manager.ContainerCapabilities(context.Background(), "porto-dev", "default", "api", "api", true)
	if err != nil {
		t.Fatalf("inspect capabilities: %v", err)
	}
	if !capabilities.FileInspection {
		t.Fatalf("file inspection unavailable: %+v", capabilities)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.deadlines) != 4 {
		t.Fatalf("probe deadlines = %d, want 4", len(runner.deadlines))
	}
	for index := 1; index < len(runner.deadlines); index++ {
		if !runner.deadlines[index].After(runner.deadlines[index-1]) {
			t.Fatalf("probe %d reused the previous deadline: %v", index, runner.deadlines)
		}
	}
}

func TestStartDebugContainerUsesPinnedToolboxAndTargetMounts(t *testing.T) {
	runner := newFakeRunner()
	var debugName string
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(joined, "get pod api-1 --namespace dev -o json") && debugName == "":
			return []byte(`{
				"metadata":{"name":"api-1","namespace":"dev","uid":"pod-uid","resourceVersion":"42"},
				"spec":{"containers":[{"name":"api","image":"example/api","env":[
					{"name":"HOME","value":"/root"},
					{"name":"CONFIG_DIR","value":"$(HOME)/config"},
					{"name":"PATH","value":"/app/bin"},
					{"name":"TOOL","value":"$(PATH)/tool"}
				],"envFrom":[{"configMapRef":{"name":"api-env"}}],"volumeMounts":[
					{"name":"kube-api-access","mountPath":"/var/run/secrets/kubernetes.io/serviceaccount","readOnly":true},
					{"name":"config","mountPath":"/app/config","subPathExpr":"$(CONFIG_DIR)","readOnly":true,"recursiveReadOnly":"Enabled"}
				]}]},
				"status":{"phase":"Running"}
			}`), nil
		case strings.Contains(joined, "patch pod api-1"):
			var patch []struct {
				Operation string          `json:"op"`
				Path      string          `json:"path"`
				Value     json.RawMessage `json:"value"`
			}
			for index, arg := range command.Args {
				if arg == "--patch" && index+1 < len(command.Args) {
					if err := json.Unmarshal([]byte(command.Args[index+1]), &patch); err != nil {
						return nil, err
					}
				}
			}
			if len(patch) != 3 ||
				patch[0].Operation != "test" || patch[0].Path != "/metadata/uid" || string(patch[0].Value) != `"pod-uid"` ||
				patch[1].Operation != "test" || patch[1].Path != "/metadata/resourceVersion" || string(patch[1].Value) != `"42"` ||
				patch[2].Operation != "add" || patch[2].Path != "/spec/ephemeralContainers" {
				return nil, fmt.Errorf("unexpected atomic debug patch: %+v", patch)
			}
			var containers []struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(patch[2].Value, &containers); err != nil {
				return nil, err
			}
			if len(containers) != 1 {
				return nil, fmt.Errorf("unexpected debug containers: %s", patch[2].Value)
			}
			debugName = containers[0].Name
			profile := patch[2].Value
			homeIndex := bytes.Index(profile, []byte(`"name":"HOME","value":"/tmp"`))
			configIndex := bytes.Index(profile, []byte(`"name":"CONFIG_DIR"`))
			pathIndex := bytes.Index(profile, []byte(`"name":"PATH","value":"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"`))
			toolIndex := bytes.Index(profile, []byte(`"name":"TOOL"`))
			if !bytes.Contains(profile, []byte(`"image":"`+DebugToolboxImage+`"`)) ||
				!bytes.Contains(profile, []byte(`"targetContainerName":"api"`)) ||
				!bytes.Contains(profile, []byte(`"command":["/bin/sh","-c","sleep \"$1\"","porto-debug","3600"]`)) ||
				!bytes.Contains(profile, []byte(`"name":"kube-api-access"`)) ||
				!bytes.Contains(profile, []byte(`"name":"config"`)) ||
				bytes.Contains(profile, []byte(`subPathExpr`)) ||
				!bytes.Contains(profile, []byte(`"recursiveReadOnly":"Enabled"`)) ||
				!bytes.Contains(profile, []byte(`"name":"api-env"`)) ||
				homeIndex < 0 || configIndex < 0 || homeIndex > configIndex ||
				pathIndex < 0 || toolIndex < 0 || pathIndex > toolIndex ||
				bytes.Contains(profile, []byte(`"name":"HOME","value":"/root"`)) ||
				bytes.Contains(profile, []byte(`"name":"PATH","value":"/app/bin"`)) ||
				!bytes.Contains(profile, []byte(`"runAsNonRoot":true`)) ||
				!bytes.Contains(profile, []byte(`"drop":["ALL"]`)) {
				return nil, fmt.Errorf("debug profile is incomplete: %s", profile)
			}
			if debugName == "" {
				return nil, errors.New("debug container name missing")
			}
			return nil, nil
		case strings.Contains(joined, "get pod api-1 --namespace dev -o json"):
			return []byte(fmt.Sprintf(
				`{"metadata":{"uid":"pod-uid","resourceVersion":"43"},"spec":{"ephemeralContainers":[{"name":%q,"image":%q,"targetContainerName":"api"}]},"status":{"ephemeralContainerStatuses":[{"name":%q,"state":{"running":{}}}]}}`,
				debugName,
				DebugToolboxImage,
				debugName,
			)), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", joined)
		}
	}

	debugContainer, err := New(runner).StartDebugContainer(
		context.Background(),
		"porto-dev",
		"dev",
		"api-1",
		"pod-uid",
		"api",
	)
	if err != nil {
		t.Fatalf("start debug container: %v", err)
	}
	if debugContainer.Name != debugName ||
		debugContainer.Image != DebugToolboxImage ||
		debugContainer.TargetContainer != "api" ||
		debugContainer.PodUID != "pod-uid" ||
		debugContainer.LifetimeSeconds != 3600 ||
		!debugContainer.Ready ||
		debugContainer.State != "running" {
		t.Fatalf("unexpected debug container: %+v", debugContainer)
	}
}

func TestStartDebugContainerPreservesIdentityWhenStatusCheckFails(t *testing.T) {
	runner := newFakeRunner()
	var debugName string
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(joined, "get pod api-1 --namespace dev -o json") && debugName == "":
			return []byte(`{"metadata":{"uid":"pod-uid","resourceVersion":"42"},"spec":{"containers":[{"name":"api","image":"example/api"}]},"status":{"phase":"Running"}}`), nil
		case strings.Contains(joined, "patch pod api-1"):
			for index, arg := range command.Args {
				if arg != "--patch" || index+1 >= len(command.Args) {
					continue
				}
				var patch []struct {
					Value json.RawMessage `json:"value"`
				}
				if err := json.Unmarshal([]byte(command.Args[index+1]), &patch); err != nil {
					return nil, err
				}
				var containers []struct {
					Name string `json:"name"`
				}
				if len(patch) != 3 {
					return nil, fmt.Errorf("unexpected patch: %+v", patch)
				}
				if err := json.Unmarshal(patch[2].Value, &containers); err != nil {
					return nil, err
				}
				if len(containers) == 1 {
					debugName = containers[0].Name
				}
			}
			return nil, nil
		case strings.Contains(joined, "get pod api-1 --namespace dev -o json"):
			return nil, errors.New("temporary API failure")
		default:
			return nil, fmt.Errorf("unexpected command: %s", joined)
		}
	}

	debugContainer, err := New(runner).StartDebugContainer(
		context.Background(),
		"porto-dev",
		"dev",
		"api-1",
		"pod-uid",
		"api",
	)
	if err != nil {
		t.Fatalf("start debug container: %v", err)
	}
	if debugContainer.Name != debugName || debugContainer.State != "pending" ||
		!strings.Contains(debugContainer.Message, "temporary API failure") {
		t.Fatalf("debug container identity was lost: %+v", debugContainer)
	}
}

func TestStartDebugContainerRejectsTerminatingPod(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		if strings.Contains(joined, "get pod api-1 --namespace dev -o json") {
			return []byte(`{
				"metadata":{
					"uid":"pod-uid",
					"resourceVersion":"42",
					"deletionTimestamp":"2026-09-02T16:00:00Z"
				},
				"spec":{"containers":[{"name":"api","image":"example/api"}]},
				"status":{"phase":"Running"}
			}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}

	_, err := New(runner).StartDebugContainer(
		context.Background(),
		"porto-dev",
		"dev",
		"api-1",
		"pod-uid",
		"api",
	)
	if err == nil || !strings.Contains(err.Error(), "terminating") {
		t.Fatalf("terminating pod was not rejected: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 1 {
		t.Fatalf("terminating pod triggered mutation: %+v", runner.commands)
	}
}

func TestProvisionClusterCreatesVMBackedNodesAndKubeconfig(t *testing.T) {
	runner := newFakeRunner()
	vmManager := vm.New(runner)
	dir := t.TempDir()
	provisioner := NewClusterProvisioner(vmManager, runner, dir)
	cluster, err := provisioner.Create(context.Background(), ClusterRequest{
		Name:         "dev",
		Provider:     "k3s",
		ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
		NodeGroups: []NodeGroupSpec{{
			Name: "workers", Count: 2, Machine: MachineSpec{CPUs: 4, MemoryMiB: 4096, DiskGiB: 30},
		}},
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	if len(cluster.Nodes) != 3 || cluster.Context != "porto-k3s-dev" {
		t.Fatalf("unexpected cluster: %+v", cluster)
	}
	data, err := os.ReadFile(provisioner.clusterKubeconfigPath("dev"))
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	if !strings.Contains(string(data), "https://127.0.0.1:") || strings.Contains(string(data), "https://127.0.0.1:6443") {
		t.Fatalf("kubeconfig did not use the forwarded host API port: %s", data)
	}
	if strings.Contains(string(data), `"name": "default"`) || !strings.Contains(string(data), `"name": "porto-k3s-dev"`) {
		t.Fatalf("kubeconfig identifiers were not made cluster-specific: %s", data)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	foundTraefikManifest := false
	foundTraefikPatch := false
	for _, command := range runner.commands {
		joined := strings.Join(command.Args, " ")
		if command.Name == "limactl" &&
			strings.Contains(joined, "/var/lib/rancher/k3s/server/manifests/porto-traefik-config.yaml") {
			foundTraefikManifest = strings.Contains(string(command.Stdin), "type: ClusterIP")
		}
		if command.Name == "kubectl" && strings.Contains(joined, "patch service traefik") {
			foundTraefikPatch = true
		}
	}
	if !foundTraefikManifest || !foundTraefikPatch {
		t.Fatalf("Traefik ClusterIP defaults were not applied: manifest=%t patch=%t", foundTraefikManifest, foundTraefikPatch)
	}
	if _, err := os.Stat(provisioner.clusterMetadataPath("dev")); err != nil {
		t.Fatalf("cluster metadata missing: %v", err)
	}
}

func TestProvisionKindClusterWithoutLima(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	cluster, err := provisioner.Create(context.Background(), ClusterRequest{
		Name:     "kind-dev",
		Provider: "kind",
		NodeGroups: []NodeGroupSpec{{
			Name: "workers", Count: 1,
		}},
	})
	if err != nil {
		t.Fatalf("create kind cluster: %v", err)
	}
	if cluster.Provider != "kind" || len(cluster.Nodes) != 2 || cluster.Nodes[0] != "porto-kind-dev-control-plane" {
		t.Fatalf("unexpected kind cluster: %+v", cluster)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	foundUpdate := false
	foundMetricsApply := false
	foundMetricsWait := false
	for _, command := range runner.commands {
		if command.Name == "kind" || command.Name == "docker" {
			environment := strings.Join(command.Env, "\n")
			if !strings.Contains(environment, "DOCKER_HOST=") || !strings.Contains(environment, "DOCKER_CONTEXT=") {
				t.Fatalf("%s did not receive the isolated Porto Docker environment: %+v", command.Name, command.Env)
			}
		}
		if command.Name == "limactl" {
			t.Fatalf("kind provider invoked Lima: %+v", command)
		}
		if command.Name == "docker" && len(command.Args) > 0 && command.Args[0] == "update" {
			foundUpdate = true
			joined := strings.Join(command.Args, " ")
			if strings.Contains(joined, "--cpus 0") || strings.Contains(joined, "--memory 0m") {
				t.Fatalf("kind resources were not normalized: %+v", command)
			}
		}
		if command.Name == "kubectl" && strings.Contains(strings.Join(command.Args, " "), "apply -f -") {
			foundMetricsApply = true
			manifest := string(command.Stdin)
			if !strings.Contains(manifest, "metrics-server:v0.9.0") ||
				!strings.Contains(manifest, "--kubelet-insecure-tls") {
				t.Fatalf("unexpected metrics-server manifest: %s", manifest)
			}
		}
		if command.Name == "kubectl" && strings.Contains(strings.Join(command.Args, " "), "apiservice/v1beta1.metrics.k8s.io") {
			foundMetricsWait = true
		}
	}
	if !foundUpdate {
		t.Fatal("kind node resources were not applied")
	}
	if !foundMetricsApply || !foundMetricsWait {
		t.Fatalf("metrics-server was not installed and awaited: apply=%t wait=%t", foundMetricsApply, foundMetricsWait)
	}
}

func TestEnsureMetricsServerRepairsOwnedKindCluster(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, root)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "kind-smoke", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provisioner.clusterKubeconfigPath("kind-smoke"), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	handled, err := provisioner.EnsureMetricsServer(context.Background(), "porto-kind-smoke")
	if err != nil {
		t.Fatalf("EnsureMetricsServer: %v", err)
	}
	if !handled {
		t.Fatal("owned kind context was not handled")
	}
}

func TestKindListDoesNotTreatMissingNodeMessageAsRunning(t *testing.T) {
	runner := newFakeRunner()
	runnerHandler := func(command runtimes.Command) ([]byte, error) {
		if command.Name == "kind" && strings.Join(command.Args, " ") == "get nodes --name porto-kind-smoke" {
			return []byte("No kind nodes found for cluster \"porto-kind-smoke\".\n"), nil
		}
		return runner.Run(context.Background(), command)
	}
	recordingRunner := &fakeRunner{handler: runnerHandler}
	provisioner := NewClusterProvisioner(vm.New(recordingRunner), recordingRunner, t.TempDir())
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "kind-smoke", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}

	clusters, err := provisioner.List(context.Background())
	if err != nil {
		t.Fatalf("list clusters: %v", err)
	}
	if len(clusters) != 1 || clusters[0].State != "stopped" || len(clusters[0].Nodes) != 0 {
		t.Fatalf("missing KinD nodes were reported as running: %+v", clusters)
	}
}

func TestKindListInspectsNodeContainerState(t *testing.T) {
	runner := newFakeRunner()
	runnerHandler := func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "kind" && joined == "get nodes --name porto-kind-smoke":
			return []byte("porto-kind-smoke-control-plane\n"), nil
		case command.Name == "docker" && strings.Contains(joined, "inspect"):
			return []byte("false\n"), nil
		default:
			return runner.Run(context.Background(), command)
		}
	}
	recordingRunner := &fakeRunner{handler: runnerHandler}
	provisioner := NewClusterProvisioner(vm.New(recordingRunner), recordingRunner, t.TempDir())
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "kind-smoke", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}

	clusters, err := provisioner.List(context.Background())
	if err != nil {
		t.Fatalf("list clusters: %v", err)
	}
	if len(clusters) != 1 || clusters[0].State != "stopped" || len(clusters[0].Nodes) != 1 {
		t.Fatalf("stopped KinD node was reported incorrectly: %+v", clusters)
	}
}

func TestProvisionK0sClusterOnLima(t *testing.T) {
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	cluster, err := provisioner.Create(context.Background(), ClusterRequest{
		Name:         "k0s-dev",
		Provider:     "k0s",
		ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
	})
	if err != nil {
		t.Fatalf("create k0s cluster: %v", err)
	}
	if cluster.Provider != "k0s" || len(cluster.Nodes) != 1 {
		t.Fatalf("unexpected k0s cluster: %+v", cluster)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	foundInstall := false
	for _, command := range runner.commands {
		if command.Name == "limactl" && strings.Contains(strings.Join(command.Args, " "), "k0s install controller") {
			foundInstall = true
			break
		}
	}
	if !foundInstall {
		t.Fatal("k0s controller installation was not invoked")
	}
}

func TestImportImageCopiesArchiveToEveryClusterNode(t *testing.T) {
	runner := newFakeRunner()
	runner.instances["porto-dev-server-1"] = true
	runner.instances["porto-dev-workers-1"] = true
	runner.instances["unrelated"] = true
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	if err := provisioner.writeClusterMetadata(ClusterRequest{
		Name: "dev", Provider: "k3s",
		NodeGroups: []NodeGroupSpec{{
			Name: "workers", Count: 1,
		}},
	}); err != nil {
		t.Fatalf("write cluster metadata: %v", err)
	}
	if err := provisioner.ImportImage(context.Background(), "dev", "example:dev"); err != nil {
		t.Fatalf("import image: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	imports := 0
	for _, command := range runner.commands {
		if command.Name == "limactl" && strings.Contains(strings.Join(command.Args, " "), "k3s ctr images import") {
			imports++
		}
	}
	if imports != 2 {
		t.Fatalf("image imports = %d, want 2", imports)
	}
}

func TestClusterDeletionUsesExactMetadataOwnership(t *testing.T) {
	runner := newFakeRunner()
	runner.instances["porto-app-server-1"] = true
	runner.instances["porto-app-v2-server-1"] = true
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "app", Provider: "k3s"}); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "app-v2", Provider: "k3s"}); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Delete(context.Background(), "app"); err != nil {
		t.Fatalf("delete app cluster: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.instances["porto-app-server-1"] {
		t.Fatal("app server VM was not deleted")
	}
	if !runner.instances["porto-app-v2-server-1"] {
		t.Fatal("app-v2 server VM was deleted by prefix collision")
	}
}
