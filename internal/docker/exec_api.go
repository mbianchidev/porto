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
	"sync/atomic"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const execRetention = 10 * time.Minute
const containerAttachStartTimeout = 20 * time.Second

var errContainerAlreadyStarted = errors.New("container is already started")

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

type containerAttachSession struct {
	start          chan containerAttachStart
	startRequested bool
	startHandled   bool
	done           chan struct{}
	exitCode       int
	authoritative  bool
	err            error
	completeOnce   sync.Once
}

type containerAttachStart struct {
	result chan error
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
	details, session, err := a.prepareContainerAttach(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	processContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	stdin := dockerBool(r, "stdin")
	if session == nil {
		connection, err := hijackDockerStream(w, r)
		if err != nil {
			return
		}
		process, err := a.manager.StartAttach(processContext, details.ID, stdin)
		if err != nil {
			writeDockerStreamError(connection, details.TTY, err)
			_ = connection.Close()
			return
		}
		_ = serveProcessStream(
			processContext,
			connection,
			process,
			details.TTY,
			stdin,
			dockerBool(r, "stdout"),
			dockerBool(r, "stderr"),
		)
		_ = connection.Close()
		return
	}
	defer a.unregisterContainerAttach(details.ID, session)
	connection, err := hijackDockerStream(w, r)
	if err != nil {
		session.abort(err)
		return
	}
	start, err := waitForContainerAttachStart(processContext, session, containerAttachStartTimeout)
	if err != nil {
		session.abort(err)
		_ = connection.Close()
		return
	}
	current, err := a.manager.containerAttachDetails(processContext, details.ID)
	if err != nil {
		session.abort(err)
		start.result <- err
		writeDockerStreamError(connection, details.TTY, err)
		_ = connection.Close()
		return
	}
	if current.State.active() {
		err = errContainerAlreadyStarted
		session.abort(err)
		start.result <- err
		writeDockerStreamError(connection, details.TTY, err)
		_ = connection.Close()
		return
	}
	baseline := a.manager.containerStartBaseline(current.State)
	process, err := a.manager.StartContainerAttached(processContext, details.ID, stdin)
	if err != nil {
		session.abort(err)
		start.result <- err
		writeDockerStreamError(connection, details.TTY, err)
		_ = connection.Close()
		return
	}
	streamDone := make(chan processStreamResult, 1)
	go func() {
		result := serveProcessStreamResult(
			processContext,
			connection,
			process,
			details.TTY,
			stdin,
			dockerBool(r, "stdout"),
			dockerBool(r, "stderr"),
		)
		streamDone <- result
	}()
	started := make(chan error, 1)
	go func() {
		started <- a.manager.waitForContainerStart(processContext, details.ID, baseline)
	}()
	streamFinished, streamResult, startErr := a.acknowledgeContainerStart(
		processContext,
		details.ID,
		baseline,
		started,
		streamDone,
	)
	if startErr == nil {
		a.markContainerAttachStarted(details.ID, session)
	} else {
		session.abort(startErr)
	}
	start.result <- startErr
	if startErr != nil {
		_ = process.Kill()
	}
	if !streamFinished {
		streamResult = <-streamDone
	}
	if startErr == nil {
		session.complete(streamResult)
	}
	_ = connection.Close()
}

func (a *API) startContainer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("t") != "" {
		a.containerAction("start")(w, r)
		return
	}
	handled, err := a.startContainerAttach(r.Context(), r.PathValue("id"))
	if !handled {
		a.containerAction("start")(w, r)
		return
	}
	if err != nil {
		if errors.Is(err, errContainerAlreadyStarted) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) prepareContainerAttach(
	ctx context.Context,
	id string,
) (containerAttachDetails, *containerAttachSession, error) {
	a.attachMu.Lock()
	defer a.attachMu.Unlock()
	details, err := a.manager.containerAttachDetails(ctx, id)
	if err != nil {
		return containerAttachDetails{}, nil, err
	}
	if details.State.active() {
		return details, nil, nil
	}
	if details.State.Status != "created" && details.State.Status != "exited" {
		return containerAttachDetails{}, nil, fmt.Errorf(
			"container %q is not running: state is %s",
			id,
			details.State.Status,
		)
	}
	if _, exists := a.attaches[details.ID]; exists {
		return containerAttachDetails{}, nil, fmt.Errorf("container %q already has a pending attach", id)
	}
	session := &containerAttachSession{
		start: make(chan containerAttachStart, 1),
		done:  make(chan struct{}),
	}
	a.attaches[details.ID] = session
	return details, session, nil
}

func (a *API) unregisterContainerAttach(id string, session *containerAttachSession) {
	a.attachMu.Lock()
	if a.attaches[id] == session {
		delete(a.attaches, id)
	}
	a.attachMu.Unlock()
}

func (a *API) markContainerAttachStarted(id string, session *containerAttachSession) {
	a.attachMu.Lock()
	if a.attaches[id] == session {
		session.startHandled = true
	}
	a.attachMu.Unlock()
}

func (a *API) containerAttachStartRequested(session *containerAttachSession) bool {
	a.attachMu.Lock()
	requested := session.startRequested
	a.attachMu.Unlock()
	return requested
}

func (a *API) startContainerAttach(ctx context.Context, id string) (bool, error) {
	a.attachMu.Lock()
	if len(a.attaches) == 0 {
		a.attachMu.Unlock()
		return false, nil
	}
	containerID := id
	session := a.attaches[containerID]
	if session == nil {
		resolvedID, err := a.manager.resolveContainerID(ctx, id)
		if err != nil {
			a.attachMu.Unlock()
			return false, nil
		}
		containerID = resolvedID
		session = a.attaches[containerID]
	}
	if session == nil {
		a.attachMu.Unlock()
		return false, nil
	}
	if session.startHandled {
		state, err := a.manager.containerWaitState(ctx, containerID)
		if err == nil && state.active() {
			a.attachMu.Unlock()
			return true, errContainerAlreadyStarted
		}
		delete(a.attaches, containerID)
		a.attachMu.Unlock()
		return false, nil
	}
	if session.startRequested {
		a.attachMu.Unlock()
		return true, fmt.Errorf("container %q start is already in progress", containerID)
	}
	session.startRequested = true
	a.attachMu.Unlock()

	start := containerAttachStart{result: make(chan error, 1)}
	select {
	case session.start <- start:
	case <-ctx.Done():
		return true, context.Cause(ctx)
	}
	select {
	case err := <-start.result:
		return true, err
	case <-session.done:
		_, _, sessionErr := session.result()
		if sessionErr != nil {
			return true, sessionErr
		}
		select {
		case err := <-start.result:
			return true, err
		case <-ctx.Done():
			return true, context.Cause(ctx)
		}
	case <-ctx.Done():
		return true, context.Cause(ctx)
	}
}

func (a *API) containerAttach(ctx context.Context, id string) *containerAttachSession {
	a.attachMu.Lock()
	defer a.attachMu.Unlock()
	if len(a.attaches) == 0 {
		return nil
	}
	if session := a.attaches[id]; session != nil {
		return session
	}
	containerID, err := a.manager.resolveContainerID(ctx, id)
	if err != nil {
		return nil
	}
	return a.attaches[containerID]
}

func (a *API) acknowledgeContainerStart(
	ctx context.Context,
	id string,
	baseline containerStartBaseline,
	started <-chan error,
	streamDone <-chan processStreamResult,
) (bool, processStreamResult, error) {
	select {
	case startErr := <-started:
		if startErr == nil {
			return false, processStreamResult{}, nil
		}
		select {
		case streamResult := <-streamDone:
			observed, observedErr := a.manager.containerStartObserved(ctx, id, baseline)
			if observed || streamResult.exitCode == 0 {
				return true, streamResult, nil
			}
			return true, streamResult, errors.Join(
				startErr,
				observedErr,
				fmt.Errorf(
					"start attached Porto container exited with code %d before the task started",
					streamResult.exitCode,
				),
			)
		default:
			return false, processStreamResult{}, startErr
		}
	case streamResult := <-streamDone:
		observed, observedErr := a.manager.containerStartObserved(ctx, id, baseline)
		if observed || streamResult.exitCode == 0 {
			return true, streamResult, nil
		}
		startErr := <-started
		if startErr == nil {
			return true, streamResult, nil
		}
		return true, streamResult, errors.Join(
			startErr,
			observedErr,
			fmt.Errorf(
				"start attached Porto container exited with code %d before the task started",
				streamResult.exitCode,
			),
		)
	}
}

func (session *containerAttachSession) complete(result processStreamResult) {
	session.finish(result.exitCode, result.authoritative, nil)
}

func (session *containerAttachSession) abort(err error) {
	session.finish(0, false, err)
}

func (session *containerAttachSession) finish(exitCode int, authoritative bool, err error) {
	session.completeOnce.Do(func() {
		session.exitCode = exitCode
		session.authoritative = authoritative
		session.err = err
		close(session.done)
	})
}

func (session *containerAttachSession) result() (int, bool, error) {
	<-session.done
	return session.exitCode, session.authoritative, session.err
}

func waitForContainerAttachStart(
	ctx context.Context,
	session *containerAttachSession,
	timeout time.Duration,
) (containerAttachStart, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case start := <-session.start:
		return start, nil
	case <-ctx.Done():
		return containerAttachStart{}, context.Cause(ctx)
	case <-timer.C:
		return containerAttachStart{}, errors.New("timed out waiting for container start")
	}
}

func writeDockerStreamError(connection net.Conn, tty bool, err error) {
	var writeMu sync.Mutex
	writer := dockerProcessWriter{connection: connection, stream: 2, tty: tty, mu: &writeMu}
	_, _ = writer.Write([]byte(err.Error() + "\n"))
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
	return serveProcessStreamResult(
		ctx,
		connection,
		process,
		tty,
		attachStdin,
		attachStdout,
		attachStderr,
	).exitCode
}

type processStreamResult struct {
	exitCode      int
	authoritative bool
}

func serveProcessStreamResult(
	ctx context.Context,
	connection net.Conn,
	process runtimes.Process,
	tty bool,
	attachStdin bool,
	attachStdout bool,
	attachStderr bool,
) processStreamResult {
	var writeMu sync.Mutex
	var interrupted atomic.Bool
	copyOutput := func(stream byte, enabled bool, output io.Reader, done chan<- struct{}) {
		defer func() { done <- struct{}{} }()
		if !enabled {
			_, _ = io.Copy(io.Discard, output)
			return
		}
		writer := &dockerProcessWriter{connection: connection, stream: stream, tty: tty, mu: &writeMu}
		if _, err := io.Copy(writer, output); err != nil {
			interrupted.Store(true)
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
			interrupted.Store(true)
			_ = process.Kill()
			contextDone = nil
		}
	}
	_ = process.Stdin().Close()
	waitErr := process.Wait()
	if waitErr == nil {
		return processStreamResult{exitCode: 0, authoritative: !interrupted.Load()}
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return processStreamResult{
			exitCode:      exitError.ExitCode(),
			authoritative: !interrupted.Load(),
		}
	}
	return processStreamResult{exitCode: -1}
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
