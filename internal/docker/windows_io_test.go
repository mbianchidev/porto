package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type testWindowsPipeListener struct {
	connection net.Conn
	err        error
}

func (l testWindowsPipeListener) Accept() (net.Conn, error) { return l.connection, l.err }
func (testWindowsPipeListener) Close() error                { return nil }
func (testWindowsPipeListener) Addr() net.Addr              { return nil }

type testWindowsPipeConnection struct {
	data                        string
	readErr, writeErr, closeErr error
}

func (c *testWindowsPipeConnection) Read(data []byte) (int, error) {
	if c.data != "" {
		n := copy(data, c.data)
		c.data = c.data[n:]
		return n, nil
	}
	return 0, c.readErr
}
func (c *testWindowsPipeConnection) Write(data []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(data), nil
}
func (c *testWindowsPipeConnection) Close() error                   { return c.closeErr }
func (*testWindowsPipeConnection) LocalAddr() net.Addr              { return nil }
func (*testWindowsPipeConnection) RemoteAddr() net.Addr             { return nil }
func (*testWindowsPipeConnection) SetDeadline(time.Time) error      { return nil }
func (*testWindowsPipeConnection) SetReadDeadline(time.Time) error  { return nil }
func (*testWindowsPipeConnection) SetWriteDeadline(time.Time) error { return nil }

func TestNativeWindowsContainersPipeAcceptAndCopyFailuresReachReaders(t *testing.T) {
	for _, operation := range []string{"accept", "copy"} {
		t.Run(operation, func(t *testing.T) {
			hub, err := memoryWindowsIO(false, false, "")
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			sub := hub.subscribe(ctx, false)
			defer sub.cancel()
			sentinel := errors.New("synthetic named pipe " + operation + " failure")
			listener := testWindowsPipeListener{err: sentinel}
			if operation == "copy" {
				listener = testWindowsPipeListener{connection: &testWindowsPipeConnection{readErr: sentinel}}
			}
			hub.serveWindowsPipe(listener, "stdout", false, func() bool { return false }, func(net.Conn) bool { return true })
			_, readErr := io.ReadAll(sub.output)
			if !errors.Is(readErr, sentinel) {
				t.Fatalf("native %s transport failure became EOF: %v", operation, readErr)
			}
			hub.mu.Lock()
			recorded := hub.err
			hub.mu.Unlock()
			if !errors.Is(recorded, sentinel) {
				t.Fatalf("native pipe owner lost %s failure: %v", operation, recorded)
			}
		})
	}
}

func TestNativeWindowsContainersIntentionalPipeCloseIsNotTransportFailure(t *testing.T) {
	hub, err := memoryWindowsIO(false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	hub.serveWindowsPipe(testWindowsPipeListener{err: net.ErrClosed}, "stdout", false, func() bool { return true }, func(net.Conn) bool { return true })
	hub.serveWindowsPipe(testWindowsPipeListener{connection: &testWindowsPipeConnection{readErr: io.EOF}}, "stdout", false, func() bool { return false }, func(net.Conn) bool { return true })
	hub.mu.Lock()
	recorded := hub.err
	hub.mu.Unlock()
	if recorded != nil {
		t.Fatalf("normal native pipe shutdown reported failure: %v", recorded)
	}
}

func TestNativeWindowsContainersSlowAttachConsumerFailsWithoutStoppingTask(t *testing.T) {
	manager, engine, _ := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attached, err := engine.Attach(ctx, id, false, false, [2]uint{})
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	session := engine.sessions[id]
	engine.mu.Unlock()
	for i := 0; i < 512; i++ {
		if _, err := session.hub.writer("stdout").Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	_, readErr := io.ReadAll(attached.Stdout())
	if readErr == nil || !strings.Contains(readErr.Error(), "bounded output buffer") {
		t.Fatalf("slow native subscriber became successful EOF: %v", readErr)
	}
	if attached.(interface{ KillOnDisconnect() bool }).KillOnDisconnect() {
		t.Fatal("attach transport failure can kill container init")
	}
	if err := attached.Kill(); err != nil {
		t.Fatal(err)
	}
	status, err := session.process.Status(context.Background())
	if err != nil || status.Status != "running" {
		t.Fatalf("slow attach consumer stopped container: %+v, %v", status, err)
	}
	session.hub.mu.Lock()
	globalErr := session.hub.err
	session.hub.mu.Unlock()
	if globalErr != nil {
		t.Fatalf("slow consumer poisoned unrelated native task I/O: %v", globalErr)
	}
}
