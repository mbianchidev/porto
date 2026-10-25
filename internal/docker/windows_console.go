package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) validateNativeExec(request ExecRequest) error {
	if !m.IsNativeWindows() {
		return nil
	}
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		return errors.New("invalid native Windows exec command")
	}
	if request.Privileged {
		return fmt.Errorf("%w: native Windows privileged exec", ErrUnsupported)
	}
	if request.TTY {
		if err := runtimes.ValidatePTYSize(request.ConsoleSize); err != nil {
			return err
		}
		if !runtimes.PTYSupported() {
			return fmt.Errorf("%w: Windows ConPTY is unavailable", ErrUnsupported)
		}
		if _, ok := m.runner.(runtimes.PTYRunner); !ok {
			return fmt.Errorf("%w: native ConPTY process runner", ErrUnsupported)
		}
	}
	if request.WorkingDir != "" && !windowsPath(request.WorkingDir) {
		return errors.New("invalid native Windows exec working directory")
	}
	for _, value := range append(append([]string(nil), request.Environment...), request.User) {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid native Windows exec option")
		}
	}
	for _, value := range request.Command {
		if strings.ContainsRune(value, 0) {
			return errors.New("invalid native Windows exec command")
		}
	}
	return nil
}

func (m *Manager) startWindowsExec(ctx context.Context, request ExecRequest, args []string) (runtimes.Process, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	if request.TTY {
		if _, ok := m.runner.(runtimes.PTYRunner); !ok {
			return nil, fmt.Errorf("%w: native ConPTY process runner", ErrUnsupported)
		}
	} else if _, ok := m.runner.(runtimes.ProcessRunner); !ok {
		return nil, fmt.Errorf("%w: native exec process runner", ErrUnsupported)
	}
	name, err := randomResourceName()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(m.stateDir, "native-exec-"+name+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	command := runtimes.Command{Name: backend.name,
		Args: append(append(append([]string(nil), backend.prefix...), "--log-file", path), args...)}
	var process runtimes.Process
	if request.TTY {
		process, err = m.runner.(runtimes.PTYRunner).StartPTY(ctx, command, request.ConsoleSize)
	} else if runner, ok := m.runner.(runtimes.ProcessRunner); ok {
		process, err = runner.Start(ctx, command)
	} else {
		err = fmt.Errorf("%w: native exec process runner", ErrUnsupported)
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &windowsExecProcess{Process: process, logPath: path}, nil
}

func (m *Manager) createWindowsTerminalContainer(ctx context.Context, args []string) ([]byte, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	runner, ok := m.runner.(runtimes.PTYRunner)
	if !ok {
		return nil, fmt.Errorf("%w: native ConPTY process runner", ErrUnsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	process, err := runner.StartPTY(ctx, runtimes.Command{Name: backend.name,
		Args: append(append([]string(nil), backend.prefix...), args...)}, [2]uint{})
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	var copies sync.WaitGroup
	copies.Add(2)
	go func() { defer copies.Done(); _, _ = io.Copy(&stdout, process.Stdout()) }()
	go func() { defer copies.Done(); _, _ = io.Copy(&stderr, process.Stderr()) }()
	_ = process.Stdin().Close()
	copies.Wait()
	if err := process.Wait(); err != nil {
		return nil, runtimes.CommandError("create native Windows terminal container", append(stdout.Bytes(), stderr.Bytes()...), err)
	}
	// A host terminal can add VT cursor/title sequences around nerdctl's ID.
	// Native nerdctl emits a full hexadecimal ID; never treat an escape as one.
	ids := nativeWindowsContainerID.FindAll(stdout.Bytes(), -1)
	if len(ids) == 0 {
		return nil, errors.New("native Windows terminal creation returned no full container ID")
	}
	return ids[len(ids)-1], nil
}

var nativeWindowsContainerID = regexp.MustCompile(`[0-9a-f]{64}`)

type windowsExecProcess struct {
	runtimes.Process
	logPath string
	wait    sync.Once
	waitErr error
}

func (p *windowsExecProcess) Wait() error {
	p.wait.Do(func() {
		p.waitErr = p.Process.Wait()
		defer os.Remove(p.logPath)
		if p.waitErr == nil {
			return
		}
		if errors.Is(p.waitErr, context.Canceled) || errors.Is(p.waitErr, context.DeadlineExceeded) {
			return
		}
		// nerdctl v2.4.1 reports an exec's nonzero exit via its own fatal log
		// and exits 1. Only that private CLI log, never container output, is trusted.
		file, err := os.Open(p.logPath)
		if err != nil {
			return
		}
		defer file.Close()
		if info, err := file.Stat(); err == nil && info.Size() > 64*1024 {
			_, _ = file.Seek(info.Size()-64*1024, io.SeekStart)
		}
		data, err := io.ReadAll(io.LimitReader(file, 64*1024))
		if err != nil {
			return
		}
		if code, ok := windowsExecLogExitCode(data); ok {
			p.waitErr = windowsExecError{cause: p.waitErr, code: code}
		}
	})
	return p.waitErr
}
func (p *windowsExecProcess) Resize(ctx context.Context, w, h uint32) error {
	if resize, ok := p.Process.(resizableProcess); ok {
		return resize.Resize(ctx, w, h)
	}
	return fmt.Errorf("%w: native exec resize", ErrUnsupported)
}
func (p *windowsExecProcess) InputEOF() {
	if notify, ok := p.Process.(interface{ InputEOF() }); ok {
		notify.InputEOF()
	}
}

type windowsExecError struct {
	cause error
	code  int
}

func (e windowsExecError) Error() string {
	return fmt.Sprintf("native Windows exec exited with code %d: %v", e.code, e.cause)
}
func (e windowsExecError) Unwrap() error { return e.cause }
func (e windowsExecError) ExitCode() int { return e.code }

func windowsExecLogExitCode(data []byte) (int, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	code, found := 0, false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		var record struct {
			Level   string `json:"level"`
			Message string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			at := strings.Index(line, `msg="`)
			if at < 0 || !strings.Contains(line, "level=fatal") {
				continue
			}
			message, err := strconv.Unquote(line[at+4:])
			if err != nil {
				continue
			}
			record.Level, record.Message = "fatal", message
		}
		if record.Level != "fatal" {
			continue
		}
		const prefix = "exec failed with exit code "
		if !strings.HasPrefix(record.Message, prefix) {
			continue
		}
		value, err := strconv.Atoi(strings.TrimPrefix(record.Message, prefix))
		if err == nil && value > 0 && uint64(value) <= (1<<32)-1 {
			code, found = value, true
		}
	}
	return code, found
}

type windowsAttachProcess struct {
	runtimes.Process
	release func()
	wait    sync.Once
	waitErr error
}

func (p *windowsAttachProcess) Wait() error {
	p.wait.Do(func() { p.waitErr = p.Process.Wait(); p.release() })
	return p.waitErr
}
func (p *windowsAttachProcess) Resize(ctx context.Context, w, h uint32) error {
	if resize, ok := p.Process.(resizableProcess); ok {
		return resize.Resize(ctx, w, h)
	}
	return fmt.Errorf("%w: native attach resize", ErrUnsupported)
}
func (p *windowsAttachProcess) InputEOF() {
	if notify, ok := p.Process.(interface{ InputEOF() }); ok {
		notify.InputEOF()
	}
}

func (m *Manager) registerWindowsAttach(id string, process runtimes.Process) runtimes.Process {
	m.windows.attachMu.Lock()
	if m.windows.attachProcesses == nil {
		m.windows.attachProcesses = make(map[string]map[runtimes.Process]struct{})
	}
	if m.windows.attachProcesses[id] == nil {
		m.windows.attachProcesses[id] = make(map[runtimes.Process]struct{})
	}
	m.windows.attachProcesses[id][process] = struct{}{}
	m.windows.attachMu.Unlock()
	return &windowsAttachProcess{Process: process, release: func() {
		m.windows.attachMu.Lock()
		delete(m.windows.attachProcesses[id], process)
		if len(m.windows.attachProcesses[id]) == 0 {
			delete(m.windows.attachProcesses, id)
		}
		m.windows.attachMu.Unlock()
	}}
}

func (m *Manager) withWindowsConsole(ctx context.Context, operation func(containerConsoleOperations) error) (err error) {
	client, err := m.runtimeConnector(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	wrapped, ok := client.(windowsInventory)
	if !ok {
		return fmt.Errorf("%w: native console runtime", ErrUnsupported)
	}
	console, ok := wrapped.containerRuntime.(containerConsoleOperations)
	if !ok {
		return fmt.Errorf("%w: native console resize/configuration", ErrUnsupported)
	}
	return operation(console)
}

func (m *Manager) resizeWindowsConsole(ctx context.Context, id string, w, h uint32) error {
	if w == 0 || h == 0 || w > runtimes.MaxPTYDimension || h > runtimes.MaxPTYDimension {
		return errors.New("invalid terminal dimensions")
	}
	canonical, err := m.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	m.windows.attachMu.Lock()
	processes := make([]runtimes.Process, 0, len(m.windows.attachProcesses[canonical]))
	for process := range m.windows.attachProcesses[canonical] {
		processes = append(processes, process)
	}
	m.windows.attachMu.Unlock()
	for _, process := range processes {
		if resize, ok := process.(resizableProcess); ok {
			if err := resize.Resize(ctx, w, h); err != nil {
				return err
			}
		}
	}
	return m.withWindowsConsole(ctx, func(console containerConsoleOperations) error { return console.ResizeConsole(ctx, canonical, w, h) })
}
