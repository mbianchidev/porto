package etcqlite

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

type genesisMember struct {
	ID        uint64 `json:"id"`
	PID       uint64 `json:"pid"`
	Name      string `json:"name"`
	ClientURL string `json:"client_url"`
	PeerURL   string `json:"peer_url"`
}

type genesis struct {
	ClusterID       uint64          `json:"cluster_id"`
	ConfigurationID uint32          `json:"configuration_id"`
	Members         []genesisMember `json:"members"`
}

func randomID() (string, error) {
	var raw [8]byte
	for {
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		if id := binary.BigEndian.Uint64(raw[:]); id != 0 {
			return strconv.FormatUint(id, 10), nil
		}
	}
}

func validateCreate(request CreateRequest) (result CreateRequest, err error) {
	defer func() {
		if err != nil {
			err = errors.Join(ErrInvalid, err)
		}
	}()
	if !namePattern.MatchString(request.Name) {
		return request, fmt.Errorf("Etcqlite name must match %s", namePattern)
	}
	if request.Members == 0 {
		request.Members = 1
	}
	if request.Members != 1 && request.Members != 3 {
		return request, errors.New("Etcqlite members must be 1 (standalone) or 3 (local quorum)")
	}
	if request.AdvertiseHost == "" {
		request.AdvertiseHost = "127.0.0.1"
	}
	if request.ListenHost == "" {
		request.ListenHost = "127.0.0.1"
	}
	if net.ParseIP(request.AdvertiseHost) == nil && !hostPattern.MatchString(request.AdvertiseHost) {
		return request, errors.New("Etcqlite advertiseHost must be a bare IP address or DNS hostname")
	}
	if ip := net.ParseIP(request.AdvertiseHost); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
		return request, errors.New("Etcqlite advertiseHost must identify a client destination, not a wildcard or multicast address")
	}
	ip := net.ParseIP(request.ListenHost)
	if ip == nil {
		return request, errors.New("Etcqlite listenHost must be an explicit local IP address")
	}
	if ip.IsLoopback() && !loopbackHost(request.AdvertiseHost) {
		return request, errors.New("non-loopback Etcqlite advertisement requires an explicit reachable listenHost, such as 0.0.0.0")
	}
	return request, nil
}

func loopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost")
}

func allocateMembers(request CreateRequest) ([]Member, error) {
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	var members []Member
	ids := make(map[string]bool)
	for index := 1; index <= request.Members; index++ {
		client, err := net.Listen("tcp", net.JoinHostPort(request.ListenHost, "0"))
		if err != nil {
			return nil, fmt.Errorf("reserve Etcqlite client port: %w", err)
		}
		listeners = append(listeners, client)
		peer, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve Etcqlite peer port: %w", err)
		}
		listeners = append(listeners, peer)
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		for ids[id] {
			id, err = randomID()
			if err != nil {
				return nil, err
			}
		}
		ids[id] = true
		_, port, err := net.SplitHostPort(client.Addr().String())
		if err != nil {
			return nil, err
		}
		members = append(members, Member{
			Name: fmt.Sprintf("member-%d", index), ID: id,
			ClientURL:  "https://" + net.JoinHostPort(request.AdvertiseHost, port),
			PeerURL:    "https://" + peer.Addr().String(),
			Listen:     net.JoinHostPort(request.ListenHost, port),
			PeerListen: peer.Addr().String(),
		})
	}
	return members, nil
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Etcqlite state directory %s must not be a symlink or non-directory", path)
	}
	return os.Chmod(path, 0o700)
}

func writeExclusive(path string, data []byte) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func writeJSONAtomic(path string, value any) (err error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("Etcqlite metadata must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".metadata-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = file.Close()
		}
		if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if err := json.NewEncoder(file).Encode(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (m *Manager) instancePath(name string) string {
	return filepath.Join(m.root, "instances", name)
}

func (m *Manager) memberPath(instance Instance, member Member) string {
	return filepath.Join(m.instancePath(instance.Name), "members", member.Name)
}

func (m *Manager) checkNode(instance Instance, member Member) error {
	for _, path := range []string{
		filepath.Join(m.instancePath(instance.Name), "members"),
		m.memberPath(instance, member),
		filepath.Join(m.memberPath(instance, member), "data"),
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(ErrConflict, fmt.Errorf("Etcqlite %s data must remain in its original non-symlink directory; missing data is never recreated", member.Name), err)
		}
	}
	return requireLocalStorage(filepath.Join(m.memberPath(instance, member), "data"))
}

func (m *Manager) writeInstance(instance Instance) error {
	copy := instance
	copy.Members = append([]Member(nil), instance.Members...)
	for index := range copy.Members {
		copy.Members[index].Usage = nil
	}
	return writeJSONAtomic(filepath.Join(m.instancePath(instance.Name), "metadata.json"), copy)
}

func (m *Manager) loadInstance(name string) (Instance, error) {
	return loadInstanceDirectory(m.instancePath(name), name)
}

func loadInstanceDirectory(directory, name string) (Instance, error) {
	var instance Instance
	if !namePattern.MatchString(name) {
		return instance, errors.New("invalid Etcqlite instance name")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return instance, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return instance, errors.New("Etcqlite instance is not an owned regular directory")
	}
	path := filepath.Join(directory, "metadata.json")
	info, err = os.Lstat(path)
	if err != nil {
		return instance, err
	}
	if !info.Mode().IsRegular() {
		return instance, errors.New("Etcqlite metadata is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return instance, err
	}
	if err := json.Unmarshal(data, &instance); err != nil {
		return instance, fmt.Errorf("decode Etcqlite ownership: %w", err)
	}
	if instance.Format != 1 || instance.Name != name || (len(instance.Members) != 1 && len(instance.Members) != 3) {
		return instance, errors.New("Etcqlite ownership metadata has an invalid format or topology")
	}
	tlsInfo, err := os.Lstat(filepath.Join(directory, "tls"))
	incomplete := false
	for _, member := range instance.Members {
		incomplete = incomplete || !member.Initialized
	}
	if err != nil && !(errors.Is(err, os.ErrNotExist) && instance.State == "error" && incomplete) {
		return instance, errors.Join(ErrConflict, errors.New("Etcqlite mTLS directory is missing or no longer owned"), err)
	}
	if err == nil && (!tlsInfo.IsDir() || tlsInfo.Mode()&os.ModeSymlink != 0) {
		return instance, errors.Join(ErrConflict, errors.New("Etcqlite mTLS directory is missing or no longer owned"), err)
	}
	ids, addresses := make(map[string]bool), make(map[string]bool)
	for index, member := range instance.Members {
		if member.Name != fmt.Sprintf("member-%d", index+1) || ids[member.ID] {
			return instance, errors.New("Etcqlite ownership metadata has an invalid member identity")
		}
		if _, err := identityETag(instance.ClusterID, member.ID); err != nil {
			return instance, err
		}
		ids[member.ID] = true
		for _, address := range []string{member.Listen, member.PeerListen} {
			host, port, err := net.SplitHostPort(address)
			number, parseErr := strconv.Atoi(port)
			if err != nil || parseErr != nil || number < 1024 || number > 65535 || net.ParseIP(host) == nil || addresses[address] {
				return instance, errors.New("Etcqlite ownership metadata has an invalid listener")
			}
			addresses[address] = true
		}
		for _, endpoint := range []string{member.ClientURL, member.PeerURL} {
			parsed, err := url.Parse(endpoint)
			if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" ||
				parsed.Fragment != "" || parsed.Port() == "" || parsed.Hostname() == "" {
				return instance, errors.New("Etcqlite ownership metadata has an invalid HTTPS endpoint")
			}
		}
	}
	return instance, nil
}

func (m *Manager) names() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, "instances"))
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || !namePattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("unrecognized entry in Etcqlite-owned instances: %s", entry.Name())
		}
		names = append(names, entry.Name())
	}
	return names, nil
}
