package docker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	registrydocker "github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/registries"
	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func (m *Manager) ownerRegistryResolver(image string, auth *RegistryAuth) (remotes.Resolver, error) {
	name, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, err
	}
	server, err := registries.ServerForImage(image)
	if err != nil {
		return nil, err
	}
	username, secret := "", ""
	if auth != nil {
		if auth.RegistryToken != "" {
			return nil, fmt.Errorf("%w: raw registry tokens for configured registry transport", ErrUnsupported)
		}
		if auth.ServerAddress != "" {
			claimed, err := registries.NormalizeServer(auth.ServerAddress)
			if err != nil || claimed != server {
				return nil, errors.New("registry credentials do not belong to this image registry")
			}
		}
		username, secret = auth.Username, auth.Password
		if auth.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
			if err != nil {
				return nil, err
			}
			var ok bool
			username, secret, ok = strings.Cut(string(decoded), ":")
			if !ok {
				return nil, errors.New("invalid registry authentication")
			}
		}
		if auth.IdentityToken != "" {
			username, secret = "", auth.IdentityToken
		}
	}
	host := reference.Domain(name)
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	policy := m.NetworkPolicy()
	if policy == nil {
		policy, err = networking.Default()
		if err != nil {
			return nil, err
		}
	}
	configuration, err := policy.ExportRuntime()
	if err != nil {
		return nil, err
	}
	local, err := registrydocker.MatchLocalhost(host)
	if err != nil {
		return nil, err
	}
	plainLocal := local && !m.IsNativeWindows() && len(configuration.CAPEM) == 0
	client := registryHTTPClientWithPolicy(policy)
	if plainLocal {
		// Preserve the existing explicit loopback-only development registry
		// contract. Imported CA trust selects HTTPS; remote hosts never fall back.
		client.Transport = networking.RegistryTransport{Manager: policy}
	}
	client.Timeout = 0
	authorizer := registrydocker.NewDockerAuthorizer(registrydocker.WithAuthClient(client),
		registrydocker.WithAuthCreds(func(requested string) (string, string, error) {
			if requested != host {
				return "", "", errors.New("registry credentials were requested for another host")
			}
			return username, secret, nil
		}))
	hosts := registrydocker.ConfigureDefaultRegistries(registrydocker.WithClient(client),
		registrydocker.WithAuthorizer(authorizer), registrydocker.WithPlainHTTP(func(requested string) (bool, error) {
			return requested == host && plainLocal, nil
		}))
	return registrydocker.NewResolver(registrydocker.ResolverOptions{Hosts: hosts}), nil
}

func (m *Manager) configuredRegistryTransport() (bool, error) {
	if m.NetworkPolicy() != nil {
		return true, nil
	}
	manager, err := networking.Default()
	if err != nil {
		return false, err
	}
	generation, err := manager.Generation()
	return generation != "", err
}

func (m *Manager) pullConfiguredImage(ctx context.Context, image, platform string, auth *RegistryAuth) (err error) {
	resolver, err := m.ownerRegistryResolver(image, auth)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	runtime, err := m.registryRuntime(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runtime.Close()) }()
	snapshotter := runtime.snapshotter
	if m.IsNativeWindows() {
		snapshotter = windowscontainers.Snapshotter
	}
	options := []containerd.RemoteOpt{containerd.WithResolver(resolver), containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(snapshotter)}
	if platform != "" {
		options = append(options, containerd.WithPlatform(platform))
	}
	// Fetch in the owner process: containerd's transfer service cannot inherit
	// client proxy credentials, and external containerd services are not ours to reconfigure.
	_, err = runtime.client.Pull(namespaces.WithNamespace(ctx, runtime.namespace), image, options...)
	if err != nil {
		return fmt.Errorf("pull through verified owner registry transport: %w", registryOperationError{err})
	}
	return nil
}

func (m *Manager) registryRuntime(ctx context.Context) (*grpcContainerRuntime, error) {
	connection, err := m.connectContainerRuntime(ctx)
	if err != nil {
		return nil, err
	}
	var runtime *grpcContainerRuntime
	switch value := connection.(type) {
	case *grpcContainerRuntime:
		runtime = value
	case windowsInventory:
		runtime, _ = value.containerRuntime.(*grpcContainerRuntime)
	}
	if runtime == nil || runtime.client == nil {
		return nil, errors.Join(fmt.Errorf("%w: configured registry transport requires the verified containerd client", ErrUnavailable), connection.Close())
	}
	return runtime, nil
}

func (m *Manager) pushConfiguredImage(ctx context.Context, image, platform string, auth *RegistryAuth) (err error) {
	resolver, err := m.ownerRegistryResolver(image, auth)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	runtime, err := m.registryRuntime(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runtime.Close()) }()
	ctx = namespaces.WithNamespace(ctx, runtime.namespace)
	stored, err := runtime.client.GetImage(ctx, normalizeNerdctlReference(m.windowsImageReference(image)))
	if err != nil {
		return err
	}
	options := []containerd.RemoteOpt{containerd.WithResolver(resolver)}
	if platform != "" {
		options = append(options, containerd.WithPlatform(platform))
	}
	err = runtime.client.Push(ctx, image, stored.Target(), options...)
	if err != nil {
		return fmt.Errorf("push through verified owner registry transport: %w", registryOperationError{err})
	}
	return nil
}

type registryOperationError struct{ cause error }

func (err registryOperationError) Error() string { return networking.Redact(err.cause.Error()) }
func (err registryOperationError) Unwrap() error { return err.cause }
