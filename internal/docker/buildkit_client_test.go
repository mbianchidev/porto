package docker

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type delayedDialBuildKitControl struct {
	echoBuildKitControl
}

func (*delayedDialBuildKitControl) Info(context.Context, *controlapi.InfoRequest) (*controlapi.InfoResponse, error) {
	return &controlapi.InfoResponse{}, nil
}

func TestBuildKitConnectionAllowsSlowTunnelSetupWithoutExtendingRPCDeadlines(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		deadline time.Duration
		want     codes.Code
	}{
		{"slow-SSH-startup", 45 * time.Second, codes.OK},
		{"shorter-caller-deadline", 10 * time.Second, codes.DeadlineExceeded},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				listener := bufconn.Listen(1 << 20)
				defer listener.Close()
				server := grpc.NewServer()
				controlapi.RegisterControlServer(server, &delayedDialBuildKitControl{})
				go func() { _ = server.Serve(listener) }()
				defer server.Stop()
				var attempts atomic.Int32
				connection, err := newBuildKitControlConnection(func(ctx context.Context) (net.Conn, error) {
					attempts.Add(1)
					timer := time.NewTimer(25 * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return nil, context.Cause(ctx)
					case <-timer.C:
						return listener.DialContext(ctx)
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				ctx, cancel := context.WithTimeout(context.Background(), scenario.deadline)
				defer cancel()
				started := time.Now()
				_, err = controlapi.NewControlClient(connection).Info(ctx, &controlapi.InfoRequest{})
				if status.Code(err) != scenario.want {
					t.Fatalf("BuildKit Info = %v, want %s", err, scenario.want)
				}
				if attempts.Load() != 1 {
					t.Fatalf("slow tunnel setup was discarded and restarted %d times", attempts.Load())
				}
				wantElapsed := min(25*time.Second, scenario.deadline)
				if elapsed := time.Since(started); elapsed != wantElapsed {
					t.Fatalf("elapsed = %s, want %s", elapsed, wantElapsed)
				}
			})
		})
	}
}

type buildKitUpgradeRecorder struct {
	*httptest.ResponseRecorder
	connection net.Conn
}

func (w *buildKitUpgradeRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.connection, bufio.NewReadWriter(bufio.NewReader(w.connection), bufio.NewWriter(w.connection)), nil
}

func TestBuildKitHTTPUpgradesDoNotWaitForRemoteTunnelSetup(t *testing.T) {
	for _, scenario := range []struct {
		name, endpoint string
		deadline       time.Duration
		code           codes.Code
	}{
		{"control", "/grpc", 45 * time.Second, codes.OK},
		{"control caller deadline", "/grpc", 10 * time.Second, codes.DeadlineExceeded},
		{"session", "/session", 45 * time.Second, codes.OK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				listener := bufconn.Listen(1 << 20)
				defer listener.Close()
				server := grpc.NewServer()
				controlapi.RegisterControlServer(server, &delayedDialBuildKitControl{})
				go func() { _ = server.Serve(listener) }()
				defer server.Stop()

				ctx, cancel := context.WithCancel(context.Background())
				client, remote := net.Pipe()
				defer remote.Close()
				release := make(chan struct{})
				var attempts atomic.Int32
				manager := NewWithStateDir(&fakeRunner{}, t.TempDir())
				manager.lookPath = func(name string) (string, error) { return name, nil }
				manager.dialBuildKit = func(ctx context.Context) (net.Conn, error) {
					attempts.Add(1)
					select {
					case <-ctx.Done():
						return nil, context.Cause(ctx)
					case <-release:
					}
					timer := time.NewTimer(25 * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return nil, context.Cause(ctx)
					case <-timer.C:
						return listener.DialContext(ctx)
					}
				}
				api := NewAPI(manager, "")
				request := httptest.NewRequest(http.MethodPost, scenario.endpoint, nil)
				request.Header.Set("Upgrade", "h2c")
				request = request.WithContext(context.WithValue(ctx, dockerServerContextKey{}, ctx))
				writer := &buildKitUpgradeRecorder{ResponseRecorder: httptest.NewRecorder(), connection: remote}
				done := make(chan struct{})
				go func() {
					if scenario.endpoint == "/grpc" {
						api.buildKitControl(writer, request)
					} else {
						api.buildKitSession(writer, request)
					}
					close(done)
				}()
				defer func() {
					cancel()
					_ = client.Close()
					<-done
				}()

				started := time.Now()
				if err := client.SetReadDeadline(started.Add(20 * time.Second)); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(client)
				response, err := http.ReadResponse(reader, request)
				if err != nil {
					t.Fatalf("local HTTP upgrade waited for a blocked remote tunnel: %v", err)
				}
				if response.StatusCode != http.StatusSwitchingProtocols || time.Since(started) != 0 {
					t.Fatalf("upgrade status = %d after %s; remote setup must not delay HTTP 101", response.StatusCode, time.Since(started))
				}
				if err := client.SetDeadline(started.Add(45 * time.Second)); err != nil {
					t.Fatal(err)
				}
				close(release)
				if scenario.endpoint == "/grpc" {
					connection, err := grpc.NewClient("passthrough:///http-upgrade",
						grpc.WithTransportCredentials(insecure.NewCredentials()),
						grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
							return &bufferedReadConn{Conn: client, reader: reader}, nil
						}))
					if err != nil {
						t.Fatal(err)
					}
					defer connection.Close()
					rpcContext, cancelRPC := context.WithTimeout(ctx, scenario.deadline)
					defer cancelRPC()
					_, err = controlapi.NewControlClient(connection).Info(rpcContext, &controlapi.InfoRequest{})
					if status.Code(err) != scenario.code {
						t.Fatalf("BuildKit Info = %v, want %s", err, scenario.code)
					}
				} else {
					const payload = "synthetic session"
					if _, err := io.WriteString(client, payload); err != nil {
						t.Fatal(err)
					}
					result := make([]byte, len(payload))
					if _, err := io.ReadFull(reader, result); err != nil {
						t.Fatal(err)
					}
					if string(result) != payload {
						t.Fatalf("session response = %q", result)
					}
				}
				if attempts.Load() != 1 {
					t.Fatalf("remote tunnel attempts = %d, want 1", attempts.Load())
				}
				if elapsed := time.Since(started); elapsed != min(25*time.Second, scenario.deadline) {
					t.Fatalf("forwarding took %s, want the unchanged tunnel/RPC allowance", elapsed)
				}
			})
		})
	}
}

func TestBuildKitRejectsInvalidUpgradeWithoutOpeningBackend(t *testing.T) {
	for _, endpoint := range []string{"/grpc", "/session"} {
		t.Run(endpoint, func(t *testing.T) {
			manager := NewWithStateDir(&fakeRunner{}, t.TempDir())
			manager.lookPath = func(name string) (string, error) { return name, nil }
			manager.dialBuildKit = func(context.Context) (net.Conn, error) {
				t.Fatal("an invalid HTTP upgrade opened a backend connection")
				return nil, nil
			}
			api := NewAPI(manager, "")
			response := httptest.NewRecorder()
			api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, endpoint, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid upgrade status = %d: %s", response.Code, response.Body.String())
			}
		})
	}
}
