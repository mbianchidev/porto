//go:build windows

package windowscontainers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	namespacesapi "github.com/containerd/containerd/api/services/namespaces/v1"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/mbianchidev/porto/internal/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var workerGate sync.RWMutex

func Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		workerGate.RLock()
		defer workerGate.RUnlock()
		next.ServeHTTP(w, r)
	})
}

func runtimeConfiguration(i Installation) map[string]string {
	quote := strconv.Quote
	containerd := "version = 3\nroot = " + quote(filepath.Join(i.Data(), "containerd")) +
		"\nstate = " + quote(filepath.Join(i.Data(), "state")) +
		"\ndisabled_plugins = ['io.containerd.cri.v1.images', 'io.containerd.cri.v1.runtime', 'io.containerd.grpc.v1.cri']\n" +
		"[grpc]\naddress = " + quote(i.ContainerdPipe()) + "\n"
	nerdctl := "address = " + quote(i.ContainerdPipe()) +
		"\nnamespace = " + quote(Namespace) + "\nsnapshotter = 'windows'" +
		"\ndata_root = " + quote(filepath.Join(i.Data(), "nerdctl")) +
		"\ncni_path = " + quote(i.Bin()) +
		"\ncni_netconfpath = " + quote(filepath.Join(i.Data(), "cni")) +
		"\nhosts_dir = [" + quote(filepath.Join(i.Data(), "certs.d")) + "]\n"
	return map[string]string{"containerd.toml": containerd, "nerdctl.toml": nerdctl}
}

func writeRuntimeConfiguration(i Installation) error {
	for name, data := range runtimeConfiguration(i) {
		path := filepath.Join(i.Deployment(), name)
		file, err := newProtectedFile(path)
		if err != nil {
			return err
		}
		_, writeErr := file.Write([]byte(data))
		if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}

func RunService(ctx context.Context, sid string, serve Serve) error {
	service, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !service || user.User.Sid.String() != "S-1-5-18" {
		return errors.New("the native Windows worker only runs as its explicitly installed LocalSystem service")
	}
	expected, err := installationFor(sid)
	if err != nil {
		return err
	}
	i, err := readInstallation(expected)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil || !strings.EqualFold(executable, i.Binary()) {
		return fmt.Errorf("%w: worker executable does not match its protected installation", ErrOwnership)
	}
	owned, err := openService(i, true)
	if err != nil {
		return err
	}
	defer owned.Close()
	if err := requireOwnedService(owned, i); err != nil {
		return err
	}
	for _, entry := range append(systemEnvironment(i),
		"DOCKER_CONFIG="+filepath.Join(i.Data(), "docker-client"),
		"PORTO_HOME="+filepath.Join(i.Data(), "worker"),
		"CONTAINERD_ADDRESS="+i.ContainerdPipe(), "CONTAINERD_NAMESPACE="+Namespace, "NERDCTL_NAMESPACE="+Namespace) {
		name, value, _ := strings.Cut(entry, "=")
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	if err := os.Unsetenv(RuntimeDirectoryEnv); err != nil {
		return err
	}
	return svc.Run(i.ServiceName(), &nativeService{parent: ctx, installation: i, serve: serve})
}

type nativeService struct {
	parent       context.Context
	installation Installation
	serve        Serve
}

func (s *nativeService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(s.parent)
	defer cancel()
	statuses <- svc.Status{State: svc.StartPending, WaitHint: 90_000}
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- Supervise(ctx, s.installation, ready, s.serve) }()
	running, clean := false, false
	var unlock func()
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	for {
		select {
		case <-ready:
			ready = nil
			running = true
			statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
		case err := <-done:
			if err == nil && clean {
				if err := writeJSON(filepath.Join(s.installation.Root, "clean-stop.json"), s.installation.Generation); err != nil {
					return true, 2
				}
				return false, 0
			}
			if err != nil {
				file, openErr := os.OpenFile(filepath.Join(s.installation.Root, "service.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
				if openErr == nil {
					_, _ = fmt.Fprintf(file, "\n%s native supervisor: %v\n", time.Now().UTC().Format(time.RFC3339), err)
					_ = file.Close()
				}
				return true, 1
			}
			return false, 0
		case request, ok := <-requests:
			if !ok {
				cancel()
				continue
			}
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				if unlock != nil {
					continue
				}
				if !running && request.Cmd != svc.Shutdown {
					continue
				}
				// Freeze resource requests before checking tasks. A new run or
				// pull must not race a verified idle stop/update.
				locked := false
				waitUntil := time.Now().Add(30 * time.Second)
				for time.Now().Before(waitUntil) {
					if workerGate.TryLock() {
						locked = true
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if !locked {
					statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
					continue
				}
				unlock = workerGate.Unlock
				checkContext, stop := context.WithTimeout(context.Background(), 30*time.Second)
				err := assertIdle(checkContext, s.installation, false)
				stop()
				if err != nil && request.Cmd != svc.Shutdown {
					unlock()
					unlock = nil
					statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
					continue
				}
				clean = err == nil
				statuses <- svc.Status{State: svc.StopPending, WaitHint: 30_000}
				cancel()
			}
		}
	}
}

func Supervise(ctx context.Context, i Installation, ready chan<- struct{}, serve Serve) (err error) {
	host, err := Probe(ctx)
	if err != nil {
		return err
	}
	if err := host.Validate(); err != nil {
		return err
	}
	i.Host = host
	if err := os.Remove(filepath.Join(i.Deployment(), "admitted.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := verifyDeployment(i); err != nil {
		return err
	}
	if err := requireAbsentPipe(i.ContainerdPipe()); err != nil {
		return err
	}
	if err := requireAbsentPipe(i.Pipe()); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(i.Root, "clean-stop.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	log, err := os.OpenFile(filepath.Join(i.Root, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	command := process.NewCommand(context.Background(), "", filepath.Join(i.Bin(), "containerd.exe"),
		"--config", filepath.Join(i.Deployment(), "containerd.toml"))
	command.Env, command.Stdout, command.Stderr = systemEnvironment(i), log, log
	if err := command.Start(); err != nil {
		return fmt.Errorf("start private Windows containerd: %w", err)
	}
	containerdDone := make(chan error, 1)
	go func() { containerdDone <- command.Wait() }()
	workerContext, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	defer func() {
		// Only terminate the exact process started above. No taskkill, service
		// discovery, default containerd pipe, or foreign process is involved.
		killErr := command.Process.Kill()
		if !errors.Is(killErr, os.ErrProcessDone) {
			err = errors.Join(err, killErr)
		}
		select {
		case <-containerdDone:
		case <-time.After(10 * time.Second):
			err = errors.Join(err, errors.New("private containerd did not exit after supervisor cancellation"))
		}
	}()
	startContext, stopStart := context.WithTimeout(ctx, 60*time.Second)
	defer stopStart()
	for {
		select {
		case exit := <-containerdDone:
			containerdDone <- exit
			return fmt.Errorf("private containerd exited before readiness: %v", exit)
		case <-startContext.Done():
			return startContext.Err()
		default:
		}
		connection, dialErr := Dial(startContext, i.ContainerdPipe())
		if dialErr == nil {
			ownerErr := pipeProcess(connection, uint32(command.Process.Pid))
			connection.Close()
			if ownerErr != nil {
				return ownerErr
			}
			break
		}
		select {
		case <-startContext.Done():
			return startContext.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := CheckNetworks(i); err != nil {
		return err
	}
	if err := ensureNamespace(startContext, i, uint32(command.Process.Pid)); err != nil {
		return err
	}
	keyBytes, _ := hexKey(i.OwnerSID)
	subnet := fmt.Sprintf("10.253.%d.0/24", keyBytes)
	var newCNI string
	if _, err := runNerdctl(startContext, i, "network", "inspect", i.DefaultNetwork()); err != nil {
		if !strings.Contains(err.Error(), "no network found matching: "+i.DefaultNetwork()) {
			return err
		}
		output, err := runNerdctl(startContext, i, "network", "create", "--driver", "nat",
			"--subnet", subnet, "--label", "io.porto.windows.owner="+i.OwnerSID, i.DefaultNetwork())
		if err != nil {
			return err
		}
		newCNI = strings.TrimSpace(string(output))
	}
	// Allocate CNI before HNS so nerdctl does not reject our own live subnet.
	if _, err := EnsureNetwork(i, i.DefaultNetwork(), subnet); err != nil {
		if newCNI == "" {
			return err
		}
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, rollbackErr := runNerdctl(rollbackContext, i, "network", "rm", newCNI)
		return errors.Join(err, rollbackErr)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(workerContext, i, ready) }()
	select {
	case err := <-serveDone:
		return err
	case exit := <-containerdDone:
		containerdDone <- exit
		stopWorker()
		<-serveDone
		return fmt.Errorf("owned native containerd exited unexpectedly: %v", exit)
	}
}

func MarkAdmitted(i Installation) error {
	return writeJSON(filepath.Join(i.Deployment(), "admitted.json"), i.Generation)
}

func hexKey(sid string) (uint64, error) {
	return strconv.ParseUint(OwnerKey(sid)[:2], 16, 8)
}

func ensureNamespace(ctx context.Context, i Installation, pid uint32) error {
	connection, err := grpc.NewClient("passthrough:///porto-owned-windows-containerd",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			pipe, err := Dial(ctx, i.ContainerdPipe())
			if err != nil {
				return nil, err
			}
			if err := pipeProcess(pipe, pid); err != nil {
				pipe.Close()
				return nil, err
			}
			return pipe, nil
		}))
	if err != nil {
		return err
	}
	defer connection.Close()
	client := namespacesapi.NewNamespacesClient(connection)
	response, err := client.List(ctx, &namespacesapi.ListNamespacesRequest{}, grpc.WaitForReady(true))
	if err != nil {
		return fmt.Errorf("inspect owned containerd namespace: %w", err)
	}
	for _, namespace := range response.Namespaces {
		if namespace.Name != Namespace {
			continue
		}
		if namespace.Labels["io.porto.windows.owner"] != i.OwnerSID || namespace.Labels[defaults.DefaultSnapshotterNSLabel] != Snapshotter {
			return fmt.Errorf("%w: native containerd namespace marker or snapshotter differs", ErrOwnership)
		}
		return nil
	}
	_, err = client.Create(ctx, &namespacesapi.CreateNamespaceRequest{Namespace: &namespacesapi.Namespace{
		Name: Namespace, Labels: map[string]string{"io.porto.windows.owner": i.OwnerSID, defaults.DefaultSnapshotterNSLabel: Snapshotter},
	}})
	return err
}
