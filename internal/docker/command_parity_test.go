package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type parityOperations struct {
	*fakeContainerOperations
	console         [2]uint
	limits          ContainerUpdate
	checkpointCalls []string
}

func (operations *parityOperations) CreateNamedCheckpoint(_ context.Context, id, name string, exit bool) error {
	operations.checkpointCalls = append(operations.checkpointCalls, fmt.Sprintf("create %s %s %t", id, name, exit))
	return nil
}

func (operations *parityOperations) ListNamedCheckpoints(_ context.Context, id string) ([]string, error) {
	operations.checkpointCalls = append(operations.checkpointCalls, "list "+id)
	return []string{"saved"}, nil
}

func (operations *parityOperations) DeleteNamedCheckpoint(_ context.Context, id, name string) error {
	operations.checkpointCalls = append(operations.checkpointCalls, "delete "+id+" "+name)
	return nil
}

func (operations *parityOperations) DiskUsage(context.Context, string) (int64, int64, error) {
	return 42, 142, nil
}

func (operations *parityOperations) ConfigureConsole(_ context.Context, _ string, size [2]uint) error {
	operations.console = size
	return nil
}

func (operations *parityOperations) ResizeConsole(_ context.Context, _ string, width, height uint32) error {
	operations.console = [2]uint{uint(height), uint(width)}
	return nil
}

func (operations *parityOperations) UpdateResources(ctx context.Context, id string, update ContainerUpdate) error {
	operations.limits = update
	return operations.fakeContainerOperations.UpdateResources(ctx, id, update)
}

func TestDockerCreateTranslatesLinuxFlags(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		if args == "image inspect synthetic:latest" {
			return []byte(`[{"Config":{"ExposedPorts":{"80/tcp":{}}}}]`), nil
		}
		for _, expected := range []string{
			"--memory 268435456", "--cpus 1.5", "--pids-limit 64", "--read-only",
			"--cap-add NET_ADMIN", "--cap-drop MKNOD", "--dns 192.0.2.53",
			"--dns-search example.test", "--dns-option ndots:1", "--add-host demo:192.0.2.10",
			"--group-add 1234", "--ulimit nofile=1024:2048",
		} {
			if !strings.Contains(args, expected) {
				t.Errorf("missing %q in %s", expected, args)
			}
			if !strings.Contains(args, "--publish ") || !strings.Contains(args, ":80/tcp") {
				t.Errorf("publish-all did not choose an explicit host port: %s", args)
			}
		}
		return []byte("synthetic-container\n"), nil
	}}
	body := `{"Image":"synthetic:latest","Tty":true,"HostConfig":{
		"ConsoleSize":[24,80],"Memory":268435456,"NanoCpus":1500000000,"PidsLimit":64,
		"ReadonlyRootfs":true,"CapAdd":["NET_ADMIN"],"CapDrop":["MKNOD"],"Dns":["192.0.2.53"],
		"DnsSearch":["example.test"],"DnsOptions":["ndots:1"],"ExtraHosts":["demo:192.0.2.10"],
		"GroupAdd":["1234"],"Ulimits":[{"Name":"nofile","Soft":1024,"Hard":2048}],"PublishAllPorts":true}}`
	response := httptest.NewRecorder()
	manager := New(runner)
	operations := &parityOperations{fakeContainerOperations: &fakeContainerOperations{}}
	manager.operationsConnector = func(context.Context) (containerOperations, error) { return operations, nil }
	NewAPI(manager, "").ServeHTTP(response, httptest.NewRequest(http.MethodPost,
		"/v1.47/containers/create", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	if operations.console != [2]uint{24, 80} {
		t.Fatalf("initial console size = %v", operations.console)
	}
}

func TestDockerProcessAndFilesystemRoutes(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"nerdctl top demo -eo pid,args": []byte("PID COMMAND\n42 sleep 60\n"),
		"nerdctl diff demo":             []byte("A /synthetic file\nC /etc\nD /removed\n"),
	}}
	handler := NewAPI(New(runner), "")
	for _, test := range []struct{ path, expected string }{
		{"/containers/demo/top?ps_args=-eo%20pid,args", `"Processes":[["42","sleep 60"]]`},
		{"/containers/demo/changes", `"Path":"/synthetic file","Kind":1`},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.47"+test.path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.expected) {
			t.Fatalf("%s = %d: %s", test.path, response.Code, response.Body.String())
		}
	}
}

func TestDockerResourceUpdateAllowsCombinedPolicyAndLimits(t *testing.T) {
	operations := &parityOperations{fakeContainerOperations: &fakeContainerOperations{}}
	manager := New(&fakeRunner{})
	manager.operationsConnector = func(context.Context) (containerOperations, error) { return operations, nil }
	handler := NewAPI(manager, "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1.47/containers/demo/update",
		strings.NewReader(`{"Memory":268435456,"CpuShares":512,"PidsLimit":64,"RestartPolicy":{"Name":"always"}}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", response.Code, response.Body.String())
	}
	if len(operations.calls) != 2 || operations.limits.Restart != "always" ||
		operations.limits.CPUShares != 512 || operations.limits.PidsLimit == nil || *operations.limits.PidsLimit != 64 {
		t.Fatalf("combined update was not applied: %v", operations.calls)
	}
}

func TestDockerContainerListLastSinceAndBefore(t *testing.T) {
	manager := New(&fakeRunner{outputs: map[string][]byte{
		"nerdctl ps -a --no-trunc --format {{json .}}": []byte(
			"{\"ID\":\"new\",\"Names\":\"new\",\"State\":\"exited\",\"CreatedAt\":\"2026-01-03T00:00:00Z\"}\n" +
				"{\"ID\":\"old\",\"Names\":\"old\",\"State\":\"running\",\"CreatedAt\":\"2026-01-01T00:00:00Z\"}\n" +
				"{\"ID\":\"middle\",\"Names\":\"middle\",\"State\":\"exited\",\"CreatedAt\":\"2026-01-02T00:00:00Z\"}\n"),
	}})
	handler := NewAPI(manager, "")
	for _, test := range []struct{ query, id string }{
		{"limit=1", "new"},
		{"since=old&before=new", "middle"},
		{`filters=%7B%22since%22%3A%5B%22old%22%5D%2C%22before%22%3A%5B%22new%22%5D%7D&all=1`, "middle"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.47/containers/json?"+test.query, nil))
		var items []struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || len(items) != 1 || items[0].ID != test.id {
			t.Fatalf("%s = %d: %s", test.query, response.Code, response.Body.String())
		}
	}
}

func TestDockerContainerResizeAndCheckpointRoutes(t *testing.T) {
	operations := &parityOperations{fakeContainerOperations: &fakeContainerOperations{}}
	manager := New(&fakeRunner{})
	manager.operationsConnector = func(context.Context) (containerOperations, error) { return operations, nil }
	handler := NewAPI(manager, "")
	for _, test := range []struct {
		method, path, body, fragment string
		status                       int
	}{
		{"POST", "/containers/demo/resize?h=30&w=100", "", "", 200},
		{"POST", "/containers/demo/resize?h=0&w=100", "", "positive terminal", 400},
		{"POST", "/containers/demo/checkpoints", `{"CheckpointID":"saved","Exit":true}`, "", 201},
		{"GET", "/containers/demo/checkpoints", "", `[{"Name":"saved"}]`, 200},
		{"DELETE", "/containers/demo/checkpoints/saved", "", "", 204},
		{"POST", "/containers/demo/checkpoints", `{"CheckpointID":"../escape"}`, "invalid checkpoint", 400},
		{"GET", "/containers/demo/checkpoints?dir=/tmp", "", "custom checkpoint directories", 501},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, "/v1.47"+test.path, strings.NewReader(test.body)))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.fragment) {
			t.Fatalf("%s %s = %d: %s", test.method, test.path, response.Code, response.Body.String())
		}
	}
	if operations.console != [2]uint{30, 100} ||
		!reflect.DeepEqual(operations.checkpointCalls, []string{"create demo saved true", "list demo", "delete demo saved"}) {
		t.Fatalf("operations = %+v", operations)
	}
}

func TestDockerImageHistoryAndAllImages(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{
		"nerdctl history --no-trunc --human=false --format {{json .}} example.test/demo:v1": []byte("{\"Snapshot\":\"sha256:layer\",\"CreatedAt\":\"2026-01-01T00:00:00Z\",\"CreatedBy\":\"COPY synthetic /data\",\"Size\":\"42\",\"Comment\":\"synthetic\"}\n"),
		"nerdctl image inspect example.test/demo:v1":                                        []byte(`[{"Id":"sha256:config","RepoTags":["example.test/demo:v1"]}]`),
		"nerdctl images --digests --no-trunc --format {{json .}}":                           nil,
		"nerdctl image rm example.test/demo:v1":                                             nil,
	}}
	handler := NewAPI(New(runner), "")
	for _, test := range []struct{ method, path, expected string }{
		{"GET", "/images/example.test%2Fdemo:v1/history", `"Id":"sha256:config","Created":1767225600`},
		{"GET", "/images/json?all=1", "[]"},
		{"DELETE", "/images/example.test%2Fdemo:v1?noprune=1", `"Deleted":"example.test/demo:v1"`},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, "/v1.47"+test.path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), test.expected) {
			t.Fatalf("%s = %d: %s", test.path, response.Code, response.Body.String())
		}
	}
}

type registryTestTransport func(*http.Request) (*http.Response, error)

func (transport registryTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestDockerRegistryLoginVerifiesCredentialsWithoutPersistingThem(t *testing.T) {
	handler := NewAPI(New(&fakeRunner{}), "")
	requests := 0
	handler.registryClient = &http.Client{Transport: registryTestTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.String() != "https://registry.example.test/v2/" {
			t.Fatalf("unexpected authentication destination: %s", request.URL)
		}
		response := &http.Response{Request: request, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
		username, password, ok := request.BasicAuth()
		if ok && username == "synthetic-user" && password == "synthetic-token" {
			response.StatusCode = 200
		} else {
			response.StatusCode = 401
			response.Header.Set("WWW-Authenticate", `Basic realm="synthetic"`)
		}
		return response, nil
	})}
	for _, test := range []struct {
		password string
		status   int
	}{
		{"synthetic-token", 200}, {"invalid-token", 401},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST", "/v1.47/auth", strings.NewReader(
			`{"username":"synthetic-user","password":"`+test.password+`","serveraddress":"registry.example.test"}`)))
		if response.Code != test.status || strings.Contains(response.Body.String(), test.password) {
			t.Fatalf("login = %d: %s", response.Code, response.Body.String())
		}
	}
	if requests < 4 {
		t.Fatal("registry authentication was not challenged and verified")
	}
}

func TestRegistryAuthenticationRefusesPlaintextTokenRealms(t *testing.T) {
	called := false
	transport := httpsRegistryTransport{registryTestTransport(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not send credentials")
	})}
	request, _ := http.NewRequest("GET", "http://auth.example.test/token", nil)
	if _, err := transport.RoundTrip(request); err == nil || called {
		t.Fatalf("plaintext authentication was allowed: %v", err)
	}
}

func TestDockerRegistrySearchAppliesFiltersAndLimit(t *testing.T) {
	handler := NewAPI(New(&fakeRunner{}), "")
	handler.registryClient = &http.Client{Transport: registryTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "index.docker.io" || request.URL.Query().Get("q") != "synthetic" || request.URL.Query().Get("n") != "2" {
			t.Fatalf("unexpected search request: %s", request.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":[
					{"name":"synthetic-low","star_count":1,"is_official":true},
					{"name":"synthetic-ok","star_count":9,"is_official":true},
					{"name":"synthetic-community","star_count":99,"is_official":false}]}`))}, nil
	})}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET",
		"/v1.47/images/search?term=synthetic&limit=2&filters="+url.QueryEscape(`{"stars":["5"],"is-official":["true"]}`), nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"name":"synthetic-ok"`) ||
		strings.Contains(response.Body.String(), "synthetic-low") || strings.Contains(response.Body.String(), "synthetic-community") {
		t.Fatalf("search = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerRemoteImportStreamsHTTPAndRejectsFailedDownloads(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.Error(w, "missing", 404)
			return
		}
		_, _ = io.WriteString(w, "synthetic-rootfs")
	}))
	defer source.Close()
	imports := 0
	runner := &fakeRunner{streamer: func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		imports++
		data, err := io.ReadAll(command.StdinReader)
		if err != nil || string(data) != "synthetic-rootfs" {
			t.Fatalf("archive = %q: %v", data, err)
		}
		return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("sha256:synthetic\n")})
	}}
	handler := NewAPI(New(runner), "")
	for _, test := range []struct {
		source string
		status int
	}{
		{source.URL + "/rootfs", 200}, {source.URL + "/missing", 500}, {"file:///synthetic.tar", 400},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("POST",
			"/v1.47/images/create?repo=synthetic&fromSrc="+url.QueryEscape(test.source), nil))
		if response.Code != test.status {
			t.Fatalf("import = %d: %s", response.Code, response.Body.String())
		}
	}
	if imports != 1 {
		t.Fatalf("failed downloads reached the backend: %d imports", imports)
	}
}

func TestDockerSecurityProfilesArePrivateAndRemoved(t *testing.T) {
	var profilePath string
	profile := `{"defaultAction":"SCMP_ACT_ALLOW"}`
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		for _, argument := range command.Args {
			if strings.HasPrefix(argument, "seccomp=") {
				profilePath = strings.TrimPrefix(argument, "seccomp=")
				data, err := os.ReadFile(profilePath)
				if err != nil || string(data) != profile {
					t.Fatalf("staged profile = %q: %v", data, err)
				}
				info, err := os.Stat(profilePath)
				if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
					t.Fatalf("profile permissions: %v %v", info, err)
				}
			}
		}
		return []byte("synthetic-container\n"), nil
	}}
	_, err := New(runner).CreateContainer(context.Background(), CreateContainerRequest{
		Image: "synthetic", SecurityOpt: []string{"seccomp=" + profile},
	})
	if err != nil {
		t.Fatal(err)
	}
	if profilePath == "" {
		t.Fatal("profile was not staged")
	}
	if _, err := os.Stat(profilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile was not removed: %v", err)
	}
}

func TestContainerCommandArgumentsAreNotParsedAsDaemonSecurityOptions(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if !strings.HasSuffix(strings.Join(command.Args, " "), "synthetic:latest program --security-opt seccomp=not-json") {
			t.Fatalf("container arguments changed: %v", command.Args)
		}
		return []byte("synthetic-container\n"), nil
	}}
	if _, err := New(runner).CreateContainer(context.Background(), CreateContainerRequest{
		Image: "synthetic:latest", Command: []string{"program", "--security-opt", "seccomp=not-json"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDockerResourceValidationRejectsInvalidLimitsBeforeMutation(t *testing.T) {
	for _, body := range []string{
		`{"CpuShares":1}`, `{"MemorySwap":-2}`, `{"PidsLimit":-2}`,
		`{"CpuPeriod":999}`, `{"NanoCpus":1000000000,"CpuQuota":100000}`,
		`{"CpusetCpus":"3-1"}`, `{"MemorySwappiness":101}`,
	} {
		runner := &fakeRunner{}
		response := httptest.NewRecorder()
		NewAPI(New(runner), "").ServeHTTP(response, httptest.NewRequest("POST",
			"/v1.47/containers/demo/update", strings.NewReader(body)))
		if response.Code != 400 || len(runner.commands) != 0 {
			t.Fatalf("%s = %d: %s; commands=%v", body, response.Code, response.Body.String(), runner.commands)
		}
	}
}
