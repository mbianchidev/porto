package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const execRetention = 10 * time.Minute

type execInstance struct {
	mu          sync.Mutex
	id          string
	containerID string
	request     ExecRequest
	started     bool
	running     bool
	exitCode    int
	pid         int
}

func (a *API) createExec(w http.ResponseWriter, r *http.Request) {
	containerID := r.PathValue("id")
	inspected, err := a.manager.InspectContainer(r.Context(), containerID)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	var container struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		HostConfig struct {
			Privileged bool `json:"Privileged"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(inspected, &container); err != nil {
		writeDockerError(w, fmt.Errorf("decode container state: %w", err))
		return
	}
	if !container.State.Running {
		writeDockerJSON(w, http.StatusConflict, map[string]string{"message": "container is not running"})
		return
	}
	var request struct {
		AttachStdin  bool     `json:"AttachStdin"`
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		DetachKeys   string   `json:"DetachKeys"`
		TTY          bool     `json:"Tty"`
		Privileged   bool     `json:"Privileged"`
		User         string   `json:"User"`
		WorkingDir   string   `json:"WorkingDir"`
		Env          []string `json:"Env"`
		Cmd          []string `json:"Cmd"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if len(request.Cmd) == 0 {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "exec command is required"})
		return
	}
	if request.TTY {
		writeDockerUnsupported(w, "TTY exec")
		return
	}
	if request.Privileged && !container.HostConfig.Privileged {
		writeDockerUnsupported(w, "privileged exec in a non-privileged container")
		return
	}
	id, err := randomResourceName()
	if err != nil {
		writeDockerError(w, err)
		return
	}
	instance := &execInstance{
		id: id, containerID: containerID, exitCode: -1,
		request: ExecRequest{
			ContainerID:  containerID,
			Command:      append([]string(nil), request.Cmd...),
			Environment:  append([]string(nil), request.Env...),
			WorkingDir:   request.WorkingDir,
			User:         request.User,
			Privileged:   request.Privileged,
			AttachStdin:  request.AttachStdin,
			AttachStdout: request.AttachStdout,
			AttachStderr: request.AttachStderr,
			TTY:          request.TTY,
		},
	}
	a.execMu.Lock()
	a.execs[id] = instance
	a.execMu.Unlock()
	time.AfterFunc(execRetention, func() {
		instance.mu.Lock()
		started := instance.started
		instance.mu.Unlock()
		if !started {
			a.deleteExec(instance)
		}
	})
	writeDockerJSON(w, http.StatusCreated, map[string]string{"Id": id})
}

func (a *API) startExec(w http.ResponseWriter, r *http.Request) {
	instance := a.execInstance(r.PathValue("id"))
	if instance == nil {
		writeDockerJSON(w, http.StatusNotFound, map[string]string{"message": "no such exec instance"})
		return
	}
	var request struct {
		Detach bool `json:"Detach"`
		TTY    bool `json:"Tty"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if request.Detach {
		writeDockerUnsupported(w, "detached exec")
		return
	}
	instance.mu.Lock()
	if instance.started {
		instance.mu.Unlock()
		writeDockerJSON(w, http.StatusConflict, map[string]string{"message": "exec instance has already started"})
		return
	}
	if request.TTY != instance.request.TTY {
		instance.mu.Unlock()
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "exec TTY setting does not match its creation request"})
		return
	}
	instance.started = true
	instance.mu.Unlock()

	processContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	process, err := a.manager.StartExec(processContext, instance.request)
	if err != nil {
		instance.complete(-1)
		a.retainExec(instance)
		writeDockerError(w, err)
		return
	}
	instance.mu.Lock()
	instance.running = true
	instance.pid = process.PID()
	instance.mu.Unlock()
	connection, err := hijackDockerStream(w, r)
	if err != nil {
		_ = process.Kill()
		instance.complete(-1)
		a.retainExec(instance)
		return
	}
	exitCode := serveProcessStream(
		processContext,
		connection,
		process,
		instance.request.TTY,
		instance.request.AttachStdin,
		instance.request.AttachStdout,
		instance.request.AttachStderr,
	)
	instance.complete(exitCode)
	_ = connection.Close()
	a.retainExec(instance)
}

func (a *API) inspectExec(w http.ResponseWriter, r *http.Request) {
	instance := a.execInstance(r.PathValue("id"))
	if instance == nil {
		writeDockerJSON(w, http.StatusNotFound, map[string]string{"message": "no such exec instance"})
		return
	}
	instance.mu.Lock()
	defer instance.mu.Unlock()
	command := ""
	arguments := []string{}
	if len(instance.request.Command) > 0 {
		command = instance.request.Command[0]
		arguments = append(arguments, instance.request.Command[1:]...)
	}
	writeDockerJSON(w, http.StatusOK, map[string]any{
		"ID":          instance.id,
		"Running":     instance.running,
		"ExitCode":    instance.exitCode,
		"OpenStdin":   instance.request.AttachStdin,
		"OpenStdout":  instance.request.AttachStdout,
		"OpenStderr":  instance.request.AttachStderr,
		"CanRemove":   false,
		"ContainerID": instance.containerID,
		"Pid":         instance.pid,
		"ProcessConfig": map[string]any{
			"privileged": instance.request.Privileged,
			"user":       instance.request.User,
			"tty":        instance.request.TTY,
			"entrypoint": command,
			"arguments":  arguments,
		},
	})
}

func (a *API) attachContainer(w http.ResponseWriter, r *http.Request) {
	if !dockerBool(r, "stream") {
		writeDockerUnsupported(w, "non-streaming container attach")
		return
	}
	if dockerBool(r, "logs") {
		writeDockerUnsupported(w, "container attach with historical logs")
		return
	}
	tty, err := a.manager.ContainerTTY(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	processContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	process, err := a.manager.StartAttach(processContext, r.PathValue("id"), dockerBool(r, "stdin"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	connection, err := hijackDockerStream(w, r)
	if err != nil {
		_ = process.Kill()
		return
	}
	_ = serveProcessStream(
		processContext,
		connection,
		process,
		tty,
		dockerBool(r, "stdin"),
		dockerBool(r, "stdout"),
		dockerBool(r, "stderr"),
	)
	_ = connection.Close()
}

func (a *API) execInstance(id string) *execInstance {
	a.execMu.Lock()
	defer a.execMu.Unlock()
	return a.execs[id]
}

func (e *execInstance) complete(exitCode int) {
	e.mu.Lock()
	e.running = false
	e.exitCode = exitCode
	e.pid = 0
	e.mu.Unlock()
}

func (a *API) retainExec(instance *execInstance) {
	time.AfterFunc(execRetention, func() {
		a.deleteExec(instance)
	})
}

func (a *API) deleteExec(instance *execInstance) {
	a.execMu.Lock()
	if a.execs[instance.id] == instance {
		delete(a.execs, instance.id)
	}
	a.execMu.Unlock()
}

type bufferedDockerConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedDockerConn) Read(data []byte) (int, error) {
	return c.reader.Read(data)
}

func hijackDockerStream(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if r.Header.Get("Upgrade") == "" {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "Docker stream requires a connection upgrade"})
		return nil, errors.New("Docker stream upgrade header is missing")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeDockerJSON(w, http.StatusInternalServerError, map[string]string{"message": "Docker API connection cannot be hijacked"})
		return nil, errors.New("Docker API connection cannot be hijacked")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	response := fmt.Sprintf(
		"HTTP/1.1 101 Switching Protocols\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n",
		r.Header.Get("Upgrade"),
	)
	if _, err := buffered.WriteString(response); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := buffered.Flush(); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return &bufferedDockerConn{Conn: connection, reader: buffered.Reader}, nil
}

func serveProcessStream(
	ctx context.Context,
	connection net.Conn,
	process runtimes.Process,
	tty bool,
	attachStdin bool,
	attachStdout bool,
	attachStderr bool,
) int {
	var writeMu sync.Mutex
	copyOutput := func(stream byte, enabled bool, output io.Reader, done chan<- struct{}) {
		defer func() { done <- struct{}{} }()
		if !enabled {
			_, _ = io.Copy(io.Discard, output)
			return
		}
		writer := &dockerProcessWriter{connection: connection, stream: stream, tty: tty, mu: &writeMu}
		if _, err := io.Copy(writer, output); err != nil {
			_ = process.Kill()
			_, _ = io.Copy(io.Discard, output)
		}
	}
	outputDone := make(chan struct{}, 2)
	go copyOutput(1, attachStdout, process.Stdout(), outputDone)
	go copyOutput(2, attachStderr, process.Stderr(), outputDone)
	if attachStdin {
		go func() {
			_, _ = io.Copy(process.Stdin(), connection)
			_ = process.Stdin().Close()
		}()
	} else {
		_ = process.Stdin().Close()
	}
	remainingOutputs := 2
	contextDone := ctx.Done()
	for remainingOutputs > 0 {
		select {
		case <-outputDone:
			remainingOutputs--
		case <-contextDone:
			_ = process.Kill()
			contextDone = nil
		}
	}
	_ = process.Stdin().Close()
	waitErr := process.Wait()
	if waitErr == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

type dockerProcessWriter struct {
	connection net.Conn
	stream     byte
	tty        bool
	mu         *sync.Mutex
}

func (w *dockerProcessWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	output := data
	if !w.tty {
		output = dockerStreamFrame(w.stream, data)
	}
	if _, err := w.connection.Write(output); err != nil {
		return 0, err
	}
	return len(data), nil
}
