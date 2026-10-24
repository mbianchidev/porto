//go:build windows

package wsl

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/mbianchidev/porto/internal/runtimes"
	"golang.org/x/sys/windows/registry"
)

func platformDistributions(ctx context.Context, runner runtimes.Runner) ([]Distribution, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Lxss`, registry.ENUMERATE_SUB_KEYS)
	if errors.Is(err, registry.ErrNotExist) {
		return []Distribution{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read current user's WSL registrations: %w", err)
	}
	defer key.Close()
	names, err := key.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}
	runningOutput, err := runner.Run(ctx, runtimes.Command{Name: "wsl.exe", Args: []string{"--list", "--running", "--quiet"}})
	if err != nil {
		return nil, runtimes.CommandError("inspect running WSL distributions without starting them", runningOutput, err)
	}
	runningText := string(runningOutput)
	if len(runningOutput) >= 2 && (runningOutput[1] == 0 || runningOutput[0] == 0xff) {
		words := make([]uint16, len(runningOutput)/2)
		for index := range words {
			words[index] = binary.LittleEndian.Uint16(runningOutput[index*2:])
		}
		runningText = strings.TrimPrefix(string(utf16.Decode(words)), "\ufeff")
	}
	running := map[string]bool{}
	for _, name := range strings.Split(runningText, "\n") {
		running[strings.TrimSpace(strings.ReplaceAll(name, "\x00", ""))] = true
	}
	distributions := []Distribution{}
	for _, id := range names {
		child, err := registry.OpenKey(key, id, registry.QUERY_VALUE)
		if err != nil {
			return nil, err
		}
		name, _, nameErr := child.GetStringValue("DistributionName")
		version, _, versionErr := child.GetIntegerValue("Version")
		_ = child.Close()
		if nameErr != nil || versionErr != nil {
			return nil, fmt.Errorf("read WSL registration %s: %w", id, errors.Join(nameErr, versionErr))
		}
		distributions = append(distributions, Distribution{Name: name, ID: id, Version: int(version), Running: running[name]})
	}
	sort.Slice(distributions, func(i, j int) bool { return distributions[i].Name < distributions[j].Name })
	return distributions, nil
}
