package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/credentials"
	"github.com/mbianchidev/porto/internal/environments"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/zalando/go-keyring"
)

func validateGrants(manifest Manifest, grants Grants) error {
	if runtime.GOOS == "windows" && slices.Contains(grants.Permissions, ProcessExec) {
		return failure(501, "extension_process_platform", "Native process grants are unavailable on Windows until Job Object confinement is supported. Use portable WebAssembly actions and the target-scoped runtime broker.")
	}
	if len(grants.Environments) > 64 || len(grants.Files) > 32 || len(grants.Commands) > 16 || len(grants.Secrets) > 32 || len(grants.Destinations) > 32 {
		return failure(400, "extension_grants", "Grant scope exceeds its documented resource limits.")
	}
	seen := map[string]bool{}
	for _, permission := range grants.Permissions {
		if !slices.Contains(manifest.Permissions, permission) || seen[permission] {
			return failure(403, "extension_grants", "Every grant must be a unique capability requested by this manifest.")
		}
		seen[permission] = true
	}
	for _, environment := range grants.Environments {
		if environment == "" || len(environment) > 128 || strings.ContainsAny(environment, "/\\ \r\n\x00") {
			return failure(400, "extension_environment", "Grant explicit environment IDs, never addresses or implicit defaults.")
		}
	}
	for _, file := range grants.Files {
		info, err := os.Lstat(file)
		if !filepath.IsAbs(file) || filepath.Clean(file) != file || err != nil || !info.Mode().IsRegular() {
			return failure(400, "extension_file", "File grants require existing, absolute regular files, not directories or symlinks.")
		}
	}
	for _, command := range grants.Commands {
		info, err := os.Lstat(command.Executable)
		if !filepath.IsAbs(command.Executable) || err != nil || !info.Mode().IsRegular() || len(command.Args) > 32 {
			return failure(400, "extension_process", "Process grants require an existing absolute executable and at most 32 exact arguments.")
		}
		for _, arg := range command.Args {
			if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
				return failure(400, "extension_process", "Process arguments exceed their size bound or contain NUL.")
			}
		}
	}
	for _, name := range grants.Secrets {
		if !contributionID.MatchString(name) {
			return failure(400, "extension_secret", "Secret grants use explicit extension-owned names.")
		}
	}
	for _, destination := range grants.Destinations {
		parsed, err := httpsURL(destination)
		if err != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
			return failure(400, "extension_network", "Network grants must be exact HTTPS origins without path, credentials or query.")
		}
	}
	return nil
}

func (m *Manager) dispatch(ctx context.Context, value Installed, invocation Invocation, request brokerRequest) (json.RawMessage, error) {
	if request.Capability == "result" {
		if len(request.Data) == 0 || !json.Valid(request.Data) {
			return nil, failure(422, "extension_result", "A pure result must contain valid JSON data.")
		}
		return request.Data, nil
	}
	if !slices.Contains(value.Manifest.Permissions, request.Capability) || !slices.Contains(value.Grants.Permissions, request.Capability) {
		return nil, failure(403, "extension_permission_denied", "The requested broker capability is not declared and granted. Review and explicitly approve only the scopes you intend.")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if slices.Contains([]string{RuntimeControl, FilesWrite, ProcessExec}, request.Capability) && !invocation.Confirm {
		return nil, failure(403, "extension_confirmation", "Host-file writes, process execution and runtime control require explicit confirmation of this invocation, regardless of the extension's action label.")
	}
	switch request.Capability {
	case RuntimeRead, RuntimeControl:
		if !slices.Contains(value.Grants.Environments, invocation.EnvironmentID) {
			return nil, failure(403, "extension_target_denied", "The selected environment is outside this extension's grant. No local or other-target fallback was used.")
		}
		target, err := url.ParseRequestURI(request.Path)
		if err != nil || !strings.HasPrefix(request.Path, "/api/") || target.Host != "" {
			return nil, failure(400, "extension_runtime_path", "Runtime requests must use a supported relative /api/ path.")
		}
		required, err := environments.RequiredCapability(request.Method, target.EscapedPath(), target.Query())
		if err != nil {
			return nil, err
		}
		if required != environments.InventoryRead && required != environments.WorkloadOperate {
			return nil, failure(403, "extension_runtime_scope", "This broker does not expose terminals, infrastructure ownership, credentials, secret reveals or streams through a runtime grant.")
		}
		if request.Capability == RuntimeRead && (request.Method != "GET" || len(request.Data) > 0) {
			return nil, failure(403, "extension_read_only", "runtime.read permits GET requests without a body only.")
		}
		if request.Method != "GET" && request.Capability != RuntimeControl {
			return nil, failure(403, "extension_control", "Runtime mutations require runtime.control.")
		}
		if request.Method != "GET" {
			parts := strings.Split(strings.TrimPrefix(target.Path, "/api/"), "/")
			if request.Method != "POST" || len(parts) != 4 || parts[0] != "docker" || parts[1] != "containers" ||
				!slices.Contains([]string{"start", "stop", "restart", "pause", "unpause", "kill", "remove", "remove-force"}, parts[3]) {
				return nil, failure(403, "extension_runtime_control_scope", "Version 1 runtime control exposes existing container lifecycle actions only, not host paths, builds, creates, exec, application definitions or arbitrary manifests.")
			}
			if len(parts[2]) < 12 || len(parts[2]) > 64 {
				return nil, failure(400, "extension_resource_identity", "Runtime control requires an immutable hexadecimal container ID, never a reusable name.")
			}
			if _, err := hex.DecodeString(parts[2]); err != nil {
				return nil, failure(400, "extension_resource_identity", "Runtime control requires an immutable hexadecimal container ID.")
			}
			if invocation.ResourceKind == "container" && parts[2] != invocation.ResourceID {
				return nil, failure(403, "extension_resource_target", "The action's immutable resource target does not match the broker request.")
			}
		}
		if m.broker == nil {
			return nil, failure(503, "extension_runtime_unavailable", "The target-scoped runtime broker is unavailable.")
		}
		return m.broker(ctx, invocation.EnvironmentID, request.Method, request.Path, request.Data)
	case FilesRead, FilesWrite:
		if !slices.Contains(value.Grants.Files, request.File) {
			return nil, failure(403, "extension_file_denied", "This exact host file is not selected in the extension grant.")
		}
		return accessFile(request)
	case ProcessExec:
		allowed := false
		for _, command := range value.Grants.Commands {
			if command.Executable == request.Executable && slices.Equal(command.Args, request.Args) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, failure(403, "extension_process_denied", "The exact executable and argument vector were not approved.")
		}
		return runProcess(ctx, request)
	case SecretsRead:
		if !slices.Contains(value.Grants.Secrets, request.Secret) || !slices.Contains(value.SecretNames, request.Secret) {
			return nil, failure(403, "extension_secret_denied", "This named extension-owned secret has not been stored and granted. Porto and other extensions' credentials are never exposed.")
		}
		secret, err := m.vault.Get("dev.mbianchi.porto.extensions", m.secretAccount(value.ID, request.Secret))
		if err != nil {
			return nil, failure(503, "extension_secret_unavailable", "The selected extension-owned secret could not be read from the OS credential store.")
		}
		return json.Marshal(map[string]string{"value": secret})
	case NetworkFetch:
		target, err := httpsURL(request.URL)
		if err != nil {
			return nil, err
		}
		allowed := false
		for _, destination := range value.Grants.Destinations {
			parsed, _ := httpsURL(destination)
			if parsed != nil && parsed.Scheme == target.Scheme && parsed.Host == target.Host {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, failure(403, "extension_network_denied", "The exact HTTPS destination is outside this extension's network grant.")
		}
		body, contentType, err := fetchHTTPS(ctx, request.URL, 1<<20)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"contentType": contentType, "content": base64.StdEncoding.EncodeToString(body)})
	default:
		return nil, failure(403, "extension_capability", "Unknown broker capability; no host API was exposed.")
	}
}

func accessFile(request brokerRequest) (json.RawMessage, error) {
	info, err := os.Lstat(request.File)
	if err != nil || !info.Mode().IsRegular() {
		return nil, failure(409, "extension_file_changed", "The selected host file is missing, replaced by a symlink or no longer a regular file. Select it again explicitly.")
	}
	root, err := os.OpenRoot(filepath.Dir(request.File))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	flags := os.O_RDONLY
	if request.Capability == FilesWrite {
		flags = os.O_WRONLY
	}
	file, err := root.OpenFile(filepath.Base(request.File), flags, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, failure(409, "extension_file_changed", "The selected file changed while opening; no data was written.")
	}
	if request.Capability == FilesRead {
		data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return nil, err
		}
		if len(data) > 1<<20 {
			return nil, failure(413, "extension_file_size", "Host-file reads are limited to 1 MiB.")
		}
		return json.Marshal(map[string]string{"content": base64.StdEncoding.EncodeToString(data)})
	}
	data, err := base64.StdEncoding.DecodeString(request.Content)
	if err != nil || len(data) > 1<<20 {
		return nil, failure(400, "extension_file_content", "File writes require at most 1 MiB of base64 content.")
	}
	if err := file.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := file.Write(data); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"written":true}`), nil
}

func runProcess(parent context.Context, request brokerRequest) (result json.RawMessage, err error) {
	if runtime.GOOS == "windows" {
		return nil, failure(501, "extension_process_platform", "Native process grants are unavailable on Windows; use isolated WebAssembly actions.")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "porto-extension-process-")
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(dir))
	}()
	info, err := os.Lstat(request.Executable)
	if err != nil || !info.Mode().IsRegular() {
		return nil, failure(409, "extension_process_changed", "The approved executable is unavailable or no longer a regular file.")
	}
	cmd := process.NewCommand(ctx, dir, request.Executable, request.Args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "TMP=" + dir, "TEMP=" + dir}
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	stdout, stderr := &limitedBuffer{limit: 64 << 10}, &limitedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	cleanupErr := cleanupProcess(cmd)
	if cleanupErr != nil {
		return nil, cleanupErr
	}
	if runErr != nil {
		return nil, failure(422, "extension_process_failed", "The approved process failed, exceeded output bounds or was canceled. Its process group was stopped; inspect explicitly granted side effects before retrying.")
	}
	return json.Marshal(map[string]string{"stdout": stdout.String(), "stderr": stderr.String()})
}

func (m *Manager) secretAccount(id, name string) string {
	sum := sha256.Sum256([]byte(m.root))
	return hex.EncodeToString(sum[:8]) + ":" + id + ":" + name
}
func (m *Manager) SetSecret(id, name, value string) error {
	if !contributionID.MatchString(name) || len(value) == 0 || len(value) > 64<<10 {
		return failure(400, "extension_secret", "A named nonempty extension secret up to 64 KiB is required.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	installed, ok := m.state.Installed[id]
	if !ok {
		return failure(404, "extension_missing", "Install the extension before adding its own secret.")
	}
	if err := m.vault.Set("dev.mbianchi.porto.extensions", m.secretAccount(id, name), value); err != nil {
		return fmt.Errorf("save extension-owned secret in OS credential store: %w", err)
	}
	if !slices.Contains(installed.SecretNames, name) {
		installed.SecretNames = append(installed.SecretNames, name)
	}
	m.state.Installed[id] = installed
	return m.saveLocked()
}

func httpsURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, failure(400, "extension_https", "Only explicit HTTPS URLs without embedded credentials or fragments are supported.")
	}
	return parsed, nil
}

var nonPublicNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	for _, prefix := range nonPublicNetworks {
		if prefix.Contains(address.Unmap()) {
			return false
		}
	}
	return true
}
func fetchHTTPS(parent context.Context, address string, limit int64) ([]byte, string, error) {
	target, err := httpsURL(address)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, target.Hostname())
	if err != nil || len(ips) == 0 {
		return nil, "", failure(503, "extension_dns", "The approved HTTPS destination could not be resolved.")
	}
	for _, ip := range ips {
		if !publicIP(ip.IP) {
			return nil, "", failure(403, "extension_network_private", "Network fetches cannot reach loopback, private, link-local or metadata addresses. Use the explicit runtime broker for Porto targets.")
		}
	}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second, DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host != target.Hostname() {
			return nil, errors.New("network destination changed")
		}
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ips[0].IP.String(), port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirect destinations require separate approval")
	}}
	request, err := http.NewRequestWithContext(ctx, "GET", target.String(), nil)
	if err != nil {
		return nil, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", failure(503, "extension_network_failed", "The approved HTTPS fetch failed or attempted a redirect. No proxy credentials, cookies or daemon secrets were sent.")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", failure(502, "extension_network_status", "The approved destination returned a non-success status.")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", failure(413, "extension_network_size", "The network response exceeded its bounded size.")
	}
	return data, response.Header.Get("Content-Type"), nil
}

func deleteSecret(vault credentials.Store, service, account string) error {
	err := vault.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
