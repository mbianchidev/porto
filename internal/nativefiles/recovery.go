package nativefiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) Recover(ctx context.Context, release func(context.Context, Attachment) error) error {
	entries, err := os.ReadDir(m.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		recordPath := filepath.Join(m.root, entry.Name())
		info, err := os.Lstat(recordPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			result = errors.Join(result, errors.New("native attachment recovery record is invalid"), err)
			continue
		}
		document, err := os.ReadFile(recordPath)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		var record Attachment
		if err := json.Unmarshal(document, &record); err != nil {
			result = errors.Join(result, err)
			continue
		}
		expected := attachmentID(record.Resource, !record.ReadOnly)
		if record.ID != expected || entry.Name() != expected+".json" || record.Identity != record.Resource.Fingerprint() {
			result = errors.Join(result, datafiles.ErrConflict)
			continue
		}
		if record.Local {
			expectedPath := ""
			if record.Backend != nil {
				expectedPath = filepath.Join("/run/porto-native", record.Backend.Owner, record.Token)
				if record.NamespacePID > 0 {
					expectedPath = fmt.Sprintf("/proc/%d/root%s", record.NamespacePID, expectedPath)
				}
			}
			if record.Backend == nil || record.Path != expectedPath {
				result = errors.Join(result, datafiles.ErrConflict)
				continue
			}
		} else if record.Path != filepath.Join(m.root, expected) {
			result = errors.Join(result, datafiles.ErrConflict)
			continue
		}
		if record.Driver != "" && record.Driver != "winfsp" {
			result = errors.Join(result, datafiles.ErrInvalid)
			continue
		}
		if record.Driver == "winfsp" && (record.BridgePID < 1 || record.BridgeStart == 0 || record.Serial == 0) {
			result = errors.Join(result, datafiles.ErrInvalid)
			continue
		}
		captured := record
		target := Target{
			Resource: record.Resource, Identity: record.Identity, Local: record.Local,
			Verify: func(context.Context) error {
				return fmt.Errorf("%w: detached daemon incarnation requires explicit reconnect", datafiles.ErrConflict)
			},
			Release: func(ctx context.Context) error {
				if captured.Backend == nil {
					return nil
				}
				return release(ctx, captured)
			},
		}
		m.mu.Lock()
		m.attachments[record.ID] = &liveAttachment{record: record, target: target}
		m.mu.Unlock()
		result = errors.Join(result, m.Detach(ctx, record.ID))
	}
	return result
}

func (m *Manager) mounted(ctx context.Context, record Attachment) (bool, error) {
	output, err := m.runner.Run(ctx, runtimes.Command{Name: "mount"})
	if err != nil {
		return false, err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for line := range strings.SplitSeq(string(output), "\n") {
		source, location, ok := strings.Cut(line, " on ")
		if !ok {
			continue
		}
		if options := strings.LastIndex(location, " ("); options >= 0 {
			location = location[:options]
		}
		if unescape.Replace(location) != record.Path {
			filesystem := strings.LastIndex(location, " type ")
			if filesystem < 0 || unescape.Replace(location[:filesystem]) != record.Path {
				continue
			}
		}
		if unescape.Replace(source) != "porto-"+record.ID {
			return false, fmt.Errorf("%w: native mount ownership changed; refusing to open or detach an unrelated filesystem", datafiles.ErrConflict)
		}
		return true, nil
	}
	return false, nil
}
