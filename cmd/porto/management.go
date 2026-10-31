package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/environments"
	"github.com/mbianchidev/porto/internal/extensions"
	"github.com/mbianchidev/porto/internal/filelock"
)

func managementAPI(ctx context.Context, method, path string, body any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+config.DaemonAddr+path, input)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Minute}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return errors.New("management response exceeded 16 MiB")
	}
	if response.StatusCode >= 300 {
		var problem environments.Error
		if json.Unmarshal(data, &problem) == nil && problem.Message != "" {
			return fmt.Errorf("HTTP %d (%s): %s", response.StatusCode, problem.Code, problem.Message)
		}
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) == 0 {
		return nil
	}
	_, err = os.Stdout.Write(append(bytes.TrimSpace(data), '\n'))
	return err
}

func environmentsCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto environments list|add|remove|revoke|rotate|listen|stop-listener|enroll|identities|revoke-identity|request|fleet-preview|fleet-run|fleet-status|fleet-cancel")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	base := "/api/environments"
	switch args[0] {
	case "list", "identities", "listener", "fleet-list":
		if len(args) != 1 {
			return errors.New("this environment command does not accept arguments")
		}
		path := base
		if args[0] == "identities" {
			path += "/identities"
		}
		if args[0] == "listener" {
			path += "/listener"
		}
		if args[0] == "fleet-list" {
			path += "/fleet/runs"
		}
		return managementAPI(ctx, "GET", path, nil)
	case "add", "fleet-preview":
		if len(args) != 2 {
			return errors.New("provide a JSON profile or fleet-request file")
		}
		if args[0] == "add" {
			var request environments.AddRequest
			if err := extensions.ReadJSON(args[1], &request, environments.MaxBody); err != nil {
				return err
			}
			return managementAPI(ctx, "POST", base, request)
		}
		var request environments.FleetRequest
		if err := extensions.ReadJSON(args[1], &request, 1<<20); err != nil {
			return err
		}
		return managementAPI(ctx, "POST", base+"/fleet/previews", request)
	case "remove", "revoke", "rotate", "revoke-identity", "fleet-status", "fleet-cancel":
		flags := flag.NewFlagSet("environments "+args[0], flag.ContinueOnError)
		confirm := flags.Bool("confirm", false, "confirm exact target revocation or removal")
		localOnly := flags.Bool("local-only", false, "explicitly forget locally while remote revocation is unconfirmed")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("provide one explicit environment, identity or fleet-run ID")
		}
		id := url.PathEscape(flags.Arg(0))
		method, path := "POST", base+"/"+id
		switch args[0] {
		case "remove":
			if !*confirm {
				return errors.New("environment removal requires --confirm; remote workload data is not deleted")
			}
			method = "DELETE"
			if *localOnly {
				path += "?localOnly=true"
			}
		case "revoke":
			if !*confirm {
				return errors.New("revocation requires --confirm and closes active target access")
			}
			path += "/revocation"
		case "rotate":
			path += "/rotation"
		case "revoke-identity":
			if !*confirm {
				return errors.New("identity revocation requires --confirm")
			}
			method, path = "DELETE", base+"/identities/"+id
		case "fleet-status":
			method, path = "GET", base+"/fleet/runs/"+id
		case "fleet-cancel":
			path = base + "/fleet/runs/" + id + "/cancel"
		}
		return managementAPI(ctx, method, path, nil)
	case "listen":
		flags := flag.NewFlagSet("environments listen", flag.ContinueOnError)
		address := flags.String("listen", "", "explicit listen host:port")
		hostname := flags.String("hostname", "", "verified TLS hostname or IP")
		public := flags.String("public-address", "", "advertised HTTPS origin")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *address == "" || *hostname == "" || *public == "" {
			return errors.New("usage: porto environments listen --listen HOST:PORT --hostname NAME --public-address https://NAME:PORT")
		}
		return managementAPI(ctx, "POST", base+"/listener", environments.ListenerOptions{Address: *address, Hostname: *hostname, PublicAddress: *public})
	case "stop-listener":
		if len(args) != 2 || args[1] != "--confirm" {
			return errors.New("usage: porto environments stop-listener --confirm (revokes all enrolled identities, retains workload data)")
		}
		return managementAPI(ctx, "DELETE", base+"/listener", nil)
	case "enroll":
		flags := flag.NewFlagSet("environments enroll", flag.ContinueOnError)
		name := flags.String("name", "", "identity display name")
		kind := flags.String("kind", "client", "client or edge")
		capabilities := flags.String("capabilities", environments.InventoryRead, "comma-separated explicit capabilities")
		ttl := flags.Int("ttl", 300, "single-use enrollment validity in seconds, 60-900")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *name == "" {
			return errors.New("usage: porto environments enroll --name NAME [--kind client|edge] [--capabilities inventory.read,workload.operate] [--ttl 300]")
		}
		return managementAPI(ctx, "POST", base+"/enrollments", environments.EnrollmentRequest{Name: *name, Kind: *kind, Capabilities: strings.Split(*capabilities, ","), TTLSeconds: *ttl})
	case "fleet-run":
		flags := flag.NewFlagSet("environments fleet-run", flag.ContinueOnError)
		confirm := flags.String("confirmation", "", "exact target-count confirmation from preview")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *confirm == "" {
			return errors.New("usage: porto environments fleet-run --confirmation 'stop 2 targets' PREVIEW_ID")
		}
		return managementAPI(ctx, "POST", base+"/fleet/runs", map[string]string{"preview": flags.Arg(0), "confirmation": *confirm})
	case "request":
		if len(args) < 4 || len(args) > 5 {
			return errors.New("usage: porto environments request ENVIRONMENT_ID METHOD /api/PATH [body.json]")
		}
		if _, err := url.ParseRequestURI(args[3]); err != nil || !strings.HasPrefix(args[3], "/api/") {
			return errors.New("an explicit supported relative /api/ path is required")
		}
		var body json.RawMessage
		if len(args) == 5 {
			if err := extensions.ReadJSON(args[4], &body, 1<<20); err != nil {
				return err
			}
		}
		return managementAPI(ctx, args[2], base+"/"+url.PathEscape(args[1])+args[3], body)
	default:
		return fmt.Errorf("unknown environments command %q", args[0])
	}
}

func agentCmd(args []string) (err error) {
	if len(args) == 0 {
		return errors.New("usage: porto agent serve|connect|forget")
	}
	if args[0] == "serve" {
		return environmentsCmd(append([]string{"listen"}, args[1:]...))
	}
	if args[0] != "connect" && args[0] != "forget" {
		return errors.New("usage: porto agent serve|connect|forget")
	}
	flags := flag.NewFlagSet("agent "+args[0], flag.ContinueOnError)
	enrollment := flags.String("enrollment", "", "explicit single-use Edge enrollment JSON file; omit to reconnect an existing identity")
	confirm := flags.Bool("confirm", false, "confirm forgetting Edge identity")
	localOnly := flags.Bool("local-only", false, "forget local state after independently revoking on the manager")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected agent arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root, err := config.Dir()
	if err != nil {
		return err
	}
	root = filepath.Join(root, "edge-agent")
	lockCtx, cancelLock := context.WithTimeout(ctx, time.Second)
	lock, err := filelock.Acquire(lockCtx, filepath.Join(root, "agent.lock"))
	cancelLock()
	if err != nil {
		return fmt.Errorf("another outbound agent owns this enrollment; stop it before reconnecting or forgetting: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	manager, err := environments.Open(root, nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, manager.Close(context.Background())) }()
	if args[0] == "forget" {
		if !*confirm {
			return errors.New("porto agent forget requires --confirm; remote workload data is never removed")
		}
		return manager.ForgetEdge(ctx, *localOnly)
	}
	if !daemonUp() {
		return errors.New("start a compatible Porto daemon on this enrolled host before connecting the outbound agent")
	}
	local, err := environments.LoopbackHandler("http://" + config.DaemonAddr)
	if err != nil {
		return err
	}
	var invite *environments.Enrollment
	if *enrollment != "" {
		invite = &environments.Enrollment{}
		if err := extensions.ReadJSON(*enrollment, invite, 1<<20); err != nil {
			return err
		}
	}
	err = manager.RunEdge(ctx, invite, local)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return err
}

func extensionCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto extension list|install|enable|disable|remove|invoke|cancel|catalog|trust|revoke-publisher|secret|pack|keygen")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	base := "/api/extensions"
	switch args[0] {
	case "list", "catalog", "publishers":
		if len(args) != 1 {
			return errors.New("this extension command does not accept arguments")
		}
		path := base
		if args[0] != "list" {
			path += "/" + args[0]
		}
		return managementAPI(ctx, "GET", path, nil)
	case "install", "update":
		flags := flag.NewFlagSet("extension install", flag.ContinueOnError)
		digest := flags.String("sha256", "", "independently verified package digest")
		developer := flags.Bool("developer", false, "explicitly approve an unsigned developer package (still installed disabled)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 || *digest == "" {
			return errors.New("usage: porto extension install --sha256 HASH [--developer] PACKAGE.porto-extension")
		}
		var p extensions.Package
		if err := extensions.ReadJSON(flags.Arg(0), &p, extensions.MaxPackageBytes); err != nil {
			return err
		}
		return managementAPI(ctx, "POST", base, extensions.InstallRequest{Package: p, ExpectedSHA256: *digest, Developer: *developer})
	case "catalog-install":
		if len(args) != 3 {
			return errors.New("usage: porto extension catalog-install EXTENSION_ID VERIFIED_SHA256 (explicitly downloads a trusted catalog artifact, installed disabled)")
		}
		return managementAPI(ctx, "POST", base+"/catalog/install", map[string]string{"id": args[1], "sha256": args[2]})
	case "enable":
		flags := flag.NewFlagSet("extension enable", flag.ContinueOnError)
		approve := flags.Bool("approve", false, "explicitly approve this grant document")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 2 || !*approve {
			return errors.New("usage: porto extension enable --approve EXTENSION_ID grants.json")
		}
		var grants extensions.Grants
		if err := extensions.ReadJSON(flags.Arg(1), &grants, 1<<20); err != nil {
			return err
		}
		return managementAPI(ctx, "PUT", base+"/"+url.PathEscape(flags.Arg(0))+"/configuration", extensions.Configuration{Enabled: true, Approve: true, Grants: grants})
	case "disable", "remove", "cancel":
		flags := flag.NewFlagSet("extension "+args[0], flag.ContinueOnError)
		confirm := flags.Bool("confirm", false, "confirm extension-owned package and credential removal")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("provide one extension ID")
		}
		path := base + "/" + url.PathEscape(flags.Arg(0))
		if args[0] == "remove" {
			if !*confirm {
				return errors.New("extension removal requires --confirm")
			}
			return managementAPI(ctx, "DELETE", path, nil)
		}
		if args[0] == "cancel" {
			return managementAPI(ctx, "POST", path+"/cancellation", nil)
		}
		return managementAPI(ctx, "PUT", path+"/configuration", extensions.Configuration{})
	case "invoke":
		flags := flag.NewFlagSet("extension invoke", flag.ContinueOnError)
		environment := flags.String("environment", "", "explicit environment ID, including local")
		confirm := flags.Bool("confirm", false, "confirm this invocation's approved mutations/native side effects")
		resourceKind := flags.String("resource-kind", "", "declared resource contribution kind")
		resourceID := flags.String("resource-id", "", "explicit resource identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() < 2 || flags.NArg() > 3 || *environment == "" {
			return errors.New("usage: porto extension invoke --environment ENVIRONMENT_ID [--confirm] [--resource-kind KIND --resource-id ID] EXTENSION_ID ACTION_ID [args.json]")
		}
		var body json.RawMessage
		if flags.NArg() == 3 {
			if err := extensions.ReadJSON(flags.Arg(2), &body, 64<<10); err != nil {
				return err
			}
		}
		return managementAPI(ctx, "POST", base+"/"+url.PathEscape(flags.Arg(0))+"/invocations", extensions.Invocation{Action: flags.Arg(1), EnvironmentID: *environment, ResourceKind: *resourceKind, ResourceID: *resourceID, Args: body, Confirm: *confirm})
	case "catalog-import", "trust", "revoke-publisher", "secret":
		if len(args) != 2 {
			return errors.New("provide a JSON catalog, publisher or extension-secret request file")
		}
		var body json.RawMessage
		if err := extensions.ReadJSON(args[1], &body, 1<<20); err != nil {
			return err
		}
		path := base + "/publishers"
		if args[0] == "catalog-import" {
			path = base + "/catalog"
		}
		if args[0] == "revoke-publisher" {
			var request struct {
				PublicKey string `json:"publicKey"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				return err
			}
			return managementAPI(ctx, "POST", path, map[string]any{"publicKey": request.PublicKey, "revoke": true})
		}
		if args[0] == "secret" {
			var request struct {
				Extension string `json:"extension"`
				Name      string `json:"name"`
				Value     string `json:"value"`
			}
			if err := json.Unmarshal(body, &request); err != nil || request.Extension == "" {
				return errors.New("secret request needs extension, name and value")
			}
			return managementAPI(ctx, "POST", base+"/"+url.PathEscape(request.Extension)+"/secrets", map[string]string{"name": request.Name, "value": request.Value})
		}
		return managementAPI(ctx, "POST", path, body)
	case "pack":
		return packExtension(args[1:])
	case "keygen":
		return extensionKeygen(args[1:])
	default:
		return fmt.Errorf("unknown extension command %q", args[0])
	}
}

func packExtension(args []string) error {
	flags := flag.NewFlagSet("extension pack", flag.ContinueOnError)
	directory := flags.String("dir", "", "authoring directory containing manifest.json, view.html and action.wasm")
	output := flags.String("out", "", "new separately installable package file")
	keyFile := flags.String("signing-key", "", "explicit private base64 Ed25519 key file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *directory == "" || *output == "" {
		return errors.New("usage: porto extension pack --dir DIRECTORY --out PACKAGE.porto-extension [--signing-key FILE]")
	}
	var key []byte
	if *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		key, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil {
			return errors.New("signing key must contain base64 Ed25519 private-key bytes")
		}
	}
	p, err := extensions.Pack(*directory, key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := writeNewPrivateFile(*output, data); err != nil {
		return err
	}
	return writeOutput(map[string]string{"package": *output, "sha256": p.SHA256, "publisher": p.PublicKey})
}
func writeNewPrivateFile(path string, data []byte) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
func extensionKeygen(args []string) error {
	flags := flag.NewFlagSet("extension keygen", flag.ContinueOnError)
	private := flags.String("private", "", "new owner-only private signing-key path")
	public := flags.String("public", "", "new public-key path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *private == "" || *public == "" {
		return errors.New("usage: porto extension keygen --private KEY.private --public KEY.public (keep the private key offline; files are never overwritten)")
	}
	key, secret, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writeNewPrivateFile(*private, []byte(base64.StdEncoding.EncodeToString(secret)+"\n")); err != nil {
		return err
	}
	if err := writeNewPrivateFile(*public, []byte(base64.StdEncoding.EncodeToString(key)+"\n")); err != nil {
		return err
	}
	return writeOutput(map[string]string{"publicKey": base64.StdEncoding.EncodeToString(key), "privateKeyFile": *private})
}
