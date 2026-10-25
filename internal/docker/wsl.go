package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/wsl"
	"github.com/mbianchidev/porto/internal/wslbridge"
)

type wslContextKey struct{}

func (m *Manager) SetWSLIntegration(manager *wsl.Manager) {
	m.wslResolve = manager.Authorize
	m.wslShares = manager.Shares
	m.wslShareGuard = m.verifyWSLMounts
}

func (m *Manager) wslRequestContext(r *http.Request) (context.Context, error) {
	distribution, owner := r.Header.Get(wslbridge.DistributionHeader), r.Header.Get(wslbridge.OwnerHeader)
	if distribution == "" && owner == "" {
		return r.Context(), nil
	}
	if m.wslResolve == nil || distribution == "" || owner == "" {
		return nil, errors.New("this WSL distribution has not opted in to Porto")
	}
	integration, err := m.wslResolve(r.Context(), distribution, owner)
	if err != nil {
		return nil, err
	}
	return context.WithValue(r.Context(), wslContextKey{}, integration), nil
}

func (m *Manager) translateWSLMounts(ctx context.Context, binds []string, mounts []dockerStructuredMount, environment []string) ([]string, []dockerStructuredMount, []string, bool, error) {
	integration, selected := ctx.Value(wslContextKey{}).(wsl.Integration)
	if !selected {
		return binds, mounts, environment, false, nil
	}
	used := map[string]bool{}
	translate := func(source string) (string, error) {
		destination, err := wsl.TranslatePath(integration, source)
		if err != nil {
			return "", err
		}
		for _, share := range integration.Shares {
			if destination == share.GuestPath || strings.HasPrefix(destination, share.GuestPath+"/") {
				used[share.GuestPath] = true
			}
		}
		return destination, nil
	}
	binds = append([]string(nil), binds...)
	mounts = append([]dockerStructuredMount(nil), mounts...)
	for index, bind := range binds {
		parts := strings.SplitN(bind, ":", 3)
		if len(parts) < 2 || !path.IsAbs(parts[0]) {
			continue
		}
		source, err := translate(parts[0])
		if err != nil {
			return nil, nil, nil, false, err
		}
		parts[0] = source
		binds[index] = strings.Join(parts, ":")
	}
	for index := range mounts {
		if strings.EqualFold(mounts[index].Type, "bind") {
			source, err := translate(mounts[index].Source)
			if err != nil {
				return nil, nil, nil, false, err
			}
			mounts[index].Source = source
		}
	}
	if len(used) == 0 {
		return binds, mounts, environment, false, nil
	}
	paths := make([]string, 0, len(used))
	for path := range used {
		paths = append(paths, path)
	}
	if m.wslShareGuard != nil {
		if err := m.wslShareGuard(ctx, paths); err != nil {
			return nil, nil, nil, false, err
		}
	}
	environment = append([]string(nil), environment...)
	for _, setting := range []string{"CHOKIDAR_USEPOLLING=true", "WATCHPACK_POLLING=true", "PORTO_WSL_POLLING=1"} {
		name, _, _ := strings.Cut(setting, "=")
		present := false
		for _, existing := range environment {
			if strings.HasPrefix(existing, name+"=") {
				present = true
				break
			}
		}
		if !present {
			environment = append(environment, setting)
		}
	}
	return binds, mounts, environment, true, nil
}

func (m *Manager) verifyWSLMounts(ctx context.Context, roots []string) error {
	backend, err := m.backendForCommand(ctx)
	if err != nil {
		return err
	}
	if backend.limaInstance == "" {
		return fmt.Errorf("%w: WSL bind mounts require Porto's owned Lima/QEMU Linux engine", ErrUnsupported)
	}
	state, err := m.readEngineState()
	if err != nil {
		return err
	}
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		return err
	}
	for _, root := range roots {
		_, err := m.runCommand(ctx, 10*time.Second, "verify selected WSL share", nil,
			"limactl", "shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c",
			`test -d "$1" && mountpoint -q -- "$1"`, "porto-wsl-mount", root)
		if err != nil {
			return fmt.Errorf("%w: selected WSL share is not mounted; apply it with 'porto wsl apply-mounts --restart-engine' after saving work: %w", ErrUnavailable, err)
		}
	}
	return nil
}
