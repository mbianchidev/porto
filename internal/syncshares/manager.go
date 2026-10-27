package syncshares

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const supportedVersion = "0.18.1-porto.1"

var sessionIdentifier = regexp.MustCompile(`\bsync_[A-Za-z0-9]+\b`)
var errSessionMissing = errors.New("the owned synchronization session is missing")

type Endpoint struct {
	OwnerID       string
	URL           string
	GuestPath     string
	SSHConfig     []byte
	KnownHosts    []byte
	Host          string
	GuestIdentity string
}

type Provider interface {
	Endpoint(context.Context, string, bool) (Endpoint, error)
	InUse(context.Context, string) error
}

type Problem struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type EndpointState struct {
	Protocol           string          `json:"protocol"`
	Path               string          `json:"path"`
	Host               string          `json:"host,omitempty"`
	Connected          bool            `json:"connected"`
	Scanned            bool            `json:"scanned"`
	Files              uint64          `json:"files,omitempty"`
	Directories        uint64          `json:"directories,omitempty"`
	ScanProblems       []Problem       `json:"scanProblems,omitempty"`
	TransitionProblems []Problem       `json:"transitionProblems,omitempty"`
	StagingProgress    json.RawMessage `json:"stagingProgress,omitempty"`
}

type Session struct {
	Identifier        string            `json:"identifier"`
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels"`
	Paused            bool              `json:"paused"`
	Status            string            `json:"status"`
	Mode              string            `json:"mode"`
	LastError         string            `json:"lastError,omitempty"`
	SuccessfulCycles  uint64            `json:"successfulCycles,omitempty"`
	Conflicts         []json.RawMessage `json:"conflicts,omitempty"`
	ExcludedConflicts uint64            `json:"excludedConflicts,omitempty"`
	Alpha             EndpointState     `json:"alpha"`
	Beta              EndpointState     `json:"beta"`
}

type Share struct {
	HostIdentity  string    `json:"hostIdentity"`
	GuestIdentity string    `json:"guestIdentity,omitempty"`
	ID            string    `json:"id"`
	ProjectID     int64     `json:"projectId"`
	HostPath      string    `json:"hostPath"`
	GuestPath     string    `json:"guestPath"`
	OwnerID       string    `json:"ownerId"`
	RemoteHost    string    `json:"remoteHost,omitempty"`
	SessionID     string    `json:"sessionId,omitempty"`
	Mode          string    `json:"mode"`
	Ignore        []string  `json:"ignore"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Session       *Session  `json:"session,omitempty"`
}

type Status struct {
	Supported bool    `json:"supported"`
	Version   string  `json:"version,omitempty"`
	Message   string  `json:"message,omitempty"`
	Shares    []Share `json:"shares"`
}

type Manager struct {
	mu       sync.Mutex
	opMu     sync.Mutex
	root     string
	runner   runtimes.Runner
	provider Provider
	lookPath func(string) (string, error)
	goos     string
}

func New(root string, runner runtimes.Runner, provider Provider) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{root: root, runner: runner, provider: provider, lookPath: runtimes.LookPath, goos: runtime.GOOS}
}

func (m *Manager) load() ([]Share, error) {
	if m.root == "" {
		return nil, errors.New("persistent project storage is required for synchronized shares")
	}
	data, err := os.ReadFile(filepath.Join(m.root, "sync-shares.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []Share{}, nil
	}
	if err != nil {
		return nil, err
	}
	var shares []Share
	if err := json.Unmarshal(data, &shares); err != nil {
		return nil, fmt.Errorf("decode synchronized-share ownership: %w", err)
	}
	for _, share := range shares {
		if !validID(share.ID) || share.OwnerID == "" || !filepath.IsAbs(share.HostPath) ||
			(share.Mode != "synchronized" && share.Mode != "bind") ||
			share.SessionID != "" && sessionIdentifier.FindString(share.SessionID) != share.SessionID {
			return nil, errors.New("invalid synchronized-share ownership record; no session was adopted")
		}
	}
	return shares, nil
}

func (m *Manager) save(shares []Share) error {
	data, err := json.MarshalIndent(shares, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.root, "sync-shares.json"), append(data, '\n'), 0o600)
}

func (m *Manager) tool(ctx context.Context) (string, string, error) {
	if m.goos != "darwin" && m.goos != "linux" {
		return "", "", errors.New("optimized synchronized shares require a supported POSIX host; ordinary bind mounts and selected WSL shares are unchanged")
	}
	if len(filepath.Join(m.root, "sync", "daemon.sock")) >= 104 {
		return "", "", errors.New("the private synchronization socket path is too long; use a shorter PORTO_HOME")
	}
	tool, err := m.lookPath("mutagen")
	if err != nil {
		return "", "", errors.New("Porto's patched synchronization tool is unavailable; build the documented synchronization runtime or use ordinary bind mounts")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.runner.Run(ctx, runtimes.Command{Name: tool, Args: []string{"version"}})
	if err != nil {
		return "", "", runtimes.CommandError("inspect synchronization tool", output, err)
	}
	version := strings.TrimSpace(string(output))
	if version != supportedVersion {
		return "", version, errors.New("use Porto's patched Mutagen 0.18.1-porto.1 toolchain; upstream prebuilt binaries use an older vulnerable Go runtime")
	}
	return tool, version, nil
}

func (m *Manager) command(ctx context.Context, args ...string) ([]byte, error) {
	tool, _, err := m.tool(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	output, err := m.runner.Run(ctx, runtimes.Command{Name: tool, Args: args, Env: []string{
		"MUTAGEN_DATA_DIRECTORY=" + filepath.Join(m.root, "sync"),
		"MUTAGEN_SSH_PATH=" + filepath.Join(m.root, "sync-ssh"),
	}})
	if err != nil {
		return output, runtimes.CommandError("manage owned synchronized share", output, err)
	}
	return output, nil
}

func (m *Manager) configureSSH(endpoint Endpoint) error {
	if len(endpoint.SSHConfig) == 0 {
		return nil
	}
	if len(endpoint.KnownHosts) == 0 {
		return errors.New("verified guest SSH host keys are required for synchronized shares")
	}
	directory := filepath.Join(m.root, "sync-ssh")
	configuration, knownHosts := filepath.Join(directory, "config"), filepath.Join(directory, "known_hosts")
	if err := config.WriteFileAtomic(configuration, endpoint.SSHConfig, 0o600); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(knownHosts, endpoint.KnownHosts, 0o600); err != nil {
		return err
	}
	for _, name := range []string{"ssh", "scp"} {
		executable, err := m.lookPath(name)
		if err != nil {
			return fmt.Errorf("synchronized sharing requires OpenSSH %s: %w", name, err)
		}
		arguments := []string{executable, "-F", configuration, "-o", "ControlMaster=no", "-o", "ControlPath=none",
			"-o", "ControlPersist=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no",
			"-o", "ClearAllForwardings=yes", "-o", "StrictHostKeyChecking=yes", "-o", "NoHostAuthenticationForLocalhost=no",
			"-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no", "-o", "UserKnownHostsFile=" + strconv.Quote(knownHosts)}
		quoted := make([]string, len(arguments))
		for index, argument := range arguments {
			quoted[index] = "'" + strings.ReplaceAll(argument, "'", "'\\''") + "'"
		}
		wrapper := "#!/bin/sh\nexec " + strings.Join(quoted, " ") + " \"$@\"\n"
		if err := config.WriteFileAtomic(filepath.Join(directory, name), []byte(wrapper), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Create(ctx context.Context, projectID int64, root string, ignores []string) (share Share, err error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if _, _, err := m.tool(ctx); err != nil {
		return share, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return share, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return share, errors.New("select an existing project directory, not a symlink root")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return share, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return share, err
	}
	if root == filepath.Dir(root) || root == home {
		return share, errors.New("a filesystem root or whole home directory cannot be synchronized")
	}
	shares, err := m.load()
	if err != nil {
		return share, err
	}
	for _, existing := range shares {
		if within(existing.HostPath, root) || within(root, existing.HostPath) {
			return share, errors.New("the selected project overlaps an existing synchronized root")
		}
	}
	for _, ignore := range ignores {
		if ignore == "" || strings.ContainsAny(ignore, "\r\n\x00") {
			return share, errors.New("ignore rules must be non-empty single-line patterns")
		}
	}
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return share, err
	}
	share = Share{ID: hex.EncodeToString(token[:]), ProjectID: projectID, HostPath: root,
		Mode: "synchronized", Ignore: slices.Clone(ignores), Status: "creating", UpdatedAt: time.Now().UTC()}
	share.HostIdentity, err = directoryIdentity(root)
	if err != nil {
		return share, err
	}
	endpoint, err := m.provider.Endpoint(ctx, share.ID, false)
	if err != nil {
		return share, err
	}
	share.OwnerID, share.GuestPath = endpoint.OwnerID, endpoint.GuestPath
	share.RemoteHost = endpoint.Host
	shares = append(shares, share)
	if err := m.save(shares); err != nil {
		return share, err
	}
	index := len(shares) - 1
	defer func() {
		share.UpdatedAt = time.Now().UTC()
		if err != nil {
			share.Status, share.Error = "failed", err.Error()
		}
		shares[index] = share
		err = errors.Join(err, m.save(shares))
	}()
	endpoint, err = m.provider.Endpoint(ctx, share.ID, true)
	if err != nil {
		return share, err
	}
	if endpoint.OwnerID != share.OwnerID || endpoint.GuestPath != share.GuestPath {
		return share, errors.New("engine ownership changed before creating the synchronized share")
	}
	share.GuestIdentity = endpoint.GuestIdentity
	if share.RemoteHost != "" && share.GuestIdentity == "" {
		return share, errors.New("the selected guest root has no observable identity; synchronization was not started")
	}
	if err := m.configureSSH(endpoint); err != nil {
		return share, err
	}
	arguments := []string{"sync", "create", "--name=porto-share-" + share.ID, "--label=porto.share=" + share.ID,
		"--label=porto.owner=" + share.OwnerID, "--no-global-configuration", "--mode=two-way-safe", "--hash=sha256",
		"--symlink-mode=portable", "--permissions-mode=portable", "--watch-mode=portable", "--ignore-vcs"}
	for _, ignore := range ignores {
		arguments = append(arguments, "--ignore="+ignore)
	}
	output, err := m.command(ctx, append(arguments, "--", root, endpoint.URL)...)
	if err != nil {
		return share, err
	}
	share.SessionID = sessionIdentifier.FindString(string(output))
	if share.SessionID == "" {
		return share, errors.New("synchronization tool did not return an owned session identifier")
	}
	shares[index] = share
	if err := m.save(shares); err != nil {
		return share, err
	}
	if _, err := m.command(ctx, "sync", "flush", share.SessionID); err != nil {
		return share, err
	}
	session, err := m.session(ctx, share)
	if err != nil {
		return share, err
	}
	share.Session, share.Status = &session, session.Status
	return share, sessionReady(session)
}

func (m *Manager) session(ctx context.Context, share Share) (Session, error) {
	output, err := m.command(ctx, "sync", "list", "--label-selector=porto.share="+share.ID, "--template={{json .}}")
	if err != nil {
		return Session{}, err
	}
	var sessions []Session
	if err := json.Unmarshal(output, &sessions); err != nil {
		return Session{}, fmt.Errorf("decode synchronized share progress: %w", err)
	}
	if len(sessions) == 0 {
		return Session{}, errSessionMissing
	}
	if len(sessions) != 1 {
		return Session{}, errors.New("the owned synchronization session is ambiguous")
	}
	session := sessions[0]
	if session.Name != "porto-share-"+share.ID || session.Labels["porto.owner"] != share.OwnerID ||
		session.Alpha.Path != share.HostPath || session.Beta.Path != share.GuestPath ||
		session.Alpha.Protocol != "local" || session.Beta.Host != share.RemoteHost ||
		share.RemoteHost != "" && session.Beta.Protocol != "ssh" ||
		share.SessionID != "" && session.Identifier != share.SessionID {
		return Session{}, errors.New("synchronization session ownership or endpoints changed")
	}
	return session, nil
}

func sessionReady(session Session) error {
	if session.Mode != "two-way-safe" {
		return errors.New("unsafe synchronization mode was detected; automatic mount mapping was refused")
	}
	if session.Paused || !session.Alpha.Connected || !session.Beta.Connected || !session.Alpha.Scanned || !session.Beta.Scanned {
		return errors.New("synchronized share is paused, disconnected, or still scanning")
	}
	if session.LastError != "" || len(session.Conflicts) > 0 || session.ExcludedConflicts > 0 ||
		len(session.Alpha.ScanProblems)+len(session.Beta.ScanProblems)+len(session.Alpha.TransitionProblems)+len(session.Beta.TransitionProblems) > 0 {
		return errors.New("synchronized share has conflicts or filesystem problems; inspect both sides before resolving them")
	}
	return nil
}

func (m *Manager) Status(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	shares, err := m.load()
	if err != nil {
		return Status{}, err
	}
	_, version, capabilityErr := m.tool(ctx)
	status := Status{Supported: capabilityErr == nil, Version: version, Shares: shares}
	if capabilityErr != nil {
		status.Message = capabilityErr.Error()
		return status, nil
	}
	for index := range shares {
		share := &shares[index]
		endpoint, err := m.provider.Endpoint(ctx, share.ID, false)
		if err == nil && share.GuestIdentity != "" && endpoint.GuestIdentity != share.GuestIdentity {
			err = errors.New("guest share directory identity changed; no replacement root was adopted")
			if _, pauseErr := m.command(ctx, "sync", "pause", "--label-selector=porto.share="+share.ID); pauseErr != nil {
				err = errors.Join(err, pauseErr)
			}
		}
		if identity, identityErr := directoryIdentity(share.HostPath); identityErr != nil || identity != share.HostIdentity {
			err = errors.Join(err, errors.New("selected host root was replaced or disconnected"))
			if _, pauseErr := m.command(ctx, "sync", "pause", "--label-selector=porto.share="+share.ID); pauseErr != nil {
				err = errors.Join(err, pauseErr)
			}
		}
		if err == nil && endpoint.OwnerID != share.OwnerID {
			err = errors.New("engine ownership changed; the original guest data was not retargeted")
			if _, pauseErr := m.command(ctx, "sync", "pause", "--label-selector=porto.share="+share.ID); pauseErr != nil {
				err = errors.Join(err, fmt.Errorf("pause stale synchronization: %w", pauseErr))
			}
		}
		if err == nil {
			err = m.configureSSH(endpoint)
		}
		if err == nil {
			session, sessionErr := m.session(ctx, *share)
			err = sessionErr
			if sessionErr == nil {
				share.SessionID, share.Session, share.Status = session.Identifier, &session, session.Status
				err = sessionReady(session)
			}
		}
		if err != nil {
			share.Error = err.Error()
		} else {
			share.Error = ""
		}
		share.UpdatedAt = time.Now().UTC()
	}
	status.Shares = shares
	return status, nil
}

func (m *Manager) Resolve(ctx context.Context, source string) (string, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !filepath.IsAbs(source) {
		return source, nil
	}
	shares, err := m.load()
	if err != nil {
		return "", err
	}
	for _, share := range shares {
		if share.Mode != "synchronized" || !selectsSource(share.HostPath, source) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil || !within(share.HostPath, resolved) {
			return "", errors.New("synchronized bind source is missing or escapes its explicitly selected root")
		}
		identity, err := directoryIdentity(share.HostPath)
		if err != nil || identity != share.HostIdentity {
			return "", errors.New("the selected host directory was recreated; synchronization was not retargeted")
		}
		endpoint, err := m.provider.Endpoint(ctx, share.ID, false)
		if err != nil {
			return "", err
		}
		if endpoint.OwnerID != share.OwnerID || endpoint.GuestPath != share.GuestPath || endpoint.Host != share.RemoteHost {
			return "", errors.New("synchronized share engine ownership changed; files were not transferred to a replacement guest")
		}
		if share.GuestIdentity != "" && endpoint.GuestIdentity != share.GuestIdentity {
			return "", errors.New("the guest share directory was recreated; synchronization was not retargeted")
		}
		if err := m.configureSSH(endpoint); err != nil {
			return "", err
		}
		session, err := m.session(ctx, share)
		if err != nil {
			return "", err
		}
		share.SessionID = session.Identifier
		if _, err := m.command(ctx, "sync", "flush", share.SessionID); err != nil {
			return "", err
		}
		status, err := m.Status(ctx)
		if err != nil {
			return "", err
		}
		for _, current := range status.Shares {
			if current.ID != share.ID {
				continue
			}
			if current.Error != "" || current.Session == nil {
				return "", fmt.Errorf("synchronized share is not ready: %s", current.Error)
			}
			relative, err := filepath.Rel(share.HostPath, resolved)
			if err != nil {
				return "", err
			}
			return filepath.ToSlash(filepath.Join(current.GuestPath, relative)), nil
		}
		return "", errors.New("synchronized share disappeared before mounting")
	}
	return source, nil
}

func (m *Manager) SetMode(ctx context.Context, id, mode string) error {
	if mode != "bind" && mode != "synchronized" {
		return errors.New("share mode must be bind or synchronized")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	shares, err := m.load()
	if err != nil {
		return err
	}
	for index, share := range shares {
		if share.ID != id {
			continue
		}
		session, err := m.session(ctx, share)
		if err != nil {
			return err
		}
		action := "pause"
		if mode == "synchronized" {
			endpoint, err := m.provider.Endpoint(ctx, id, false)
			if err != nil {
				return err
			}
			if endpoint.OwnerID != share.OwnerID {
				return errors.New("engine ownership changed; synchronization was not resumed")
			}
			if identity, err := directoryIdentity(share.HostPath); err != nil || identity != share.HostIdentity ||
				endpoint.GuestPath != share.GuestPath || endpoint.Host != share.RemoteHost ||
				share.GuestIdentity != "" && endpoint.GuestIdentity != share.GuestIdentity {
				return errors.New("a selected share root was replaced; synchronization was not resumed")
			}
			if err := m.configureSSH(endpoint); err != nil {
				return err
			}
			action = "resume"
		}
		if _, err := m.command(ctx, "sync", action, session.Identifier); err != nil {
			return err
		}
		shares[index].Mode = mode
		shares[index].SessionID = session.Identifier
		shares[index].UpdatedAt = time.Now().UTC()
		return m.save(shares)
	}
	return errors.New("synchronized share was not found")
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	shares, err := m.load()
	if err != nil {
		return err
	}
	for index, share := range shares {
		if share.ID != id {
			continue
		}
		if err := m.provider.InUse(ctx, share.GuestPath); err != nil {
			return err
		}
		session, err := m.session(ctx, share)
		if err != nil && !errors.Is(err, errSessionMissing) {
			return err
		}
		if err == nil {
			if _, err := m.command(ctx, "sync", "terminate", session.Identifier); err != nil {
				return err
			}
		}
		remaining := append(shares[:index], shares[index+1:]...)
		if err := m.save(remaining); err != nil {
			return err
		}
		if len(remaining) == 0 {
			_, err := m.command(ctx, "daemon", "stop")
			return err
		}
		return nil
	}
	return errors.New("synchronized share was not found")
}

func (m *Manager) Active() (bool, error) {
	shares, err := m.load()
	for _, share := range shares {
		if share.Mode == "synchronized" {
			return true, err
		}
	}
	return false, err
}

func (m *Manager) Monitor(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		active, err := m.Active()
		if err != nil {
			log.Printf("Observe synchronized shares: %v", err)
		} else if active {
			if _, err := m.Status(ctx); err != nil && ctx.Err() == nil {
				log.Printf("Refresh synchronized shares: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) Close(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	shares, err := m.load()
	if err != nil || len(shares) == 0 {
		return err
	}
	_, err = m.command(ctx, "daemon", "stop")
	return err
}

func validID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 12
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func selectsSource(root, source string) bool {
	for current := filepath.Clean(source); ; current = filepath.Dir(current) {
		if within(root, current) {
			return true
		}
		if resolved, err := filepath.EvalSymlinks(current); err == nil && within(root, resolved) {
			return true
		}
		if current == filepath.Dir(current) {
			return false
		}
	}
}
