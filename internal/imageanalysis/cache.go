package imageanalysis

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mbianchidev/porto/internal/localtools"
)

const cacheAdmissionBytes = int64(1024 * 1024 * 1024)

func (m *Manager) checkCacheBudget() error {
	var total int64
	for _, directory := range []string{"reports", "cache", "sboms"} {
		err := filepath.WalkDir(filepath.Join(m.root, directory), func(_ string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				total += info.Size()
				if total >= cacheAdmissionBytes {
					return fmt.Errorf("%w: saved analyses reached the 1 GiB admission limit; export needed reports and clear saved analyses before rescanning", ErrBusy)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ClearCache() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) != 0 {
		return fmt.Errorf("%w: cancel or finish active scans before clearing saved analyses", ErrBusy)
	}
	var result error
	for _, directory := range []string{"reports", "cache", "sboms"} {
		result = errors.Join(result, os.RemoveAll(filepath.Join(m.root, directory)))
	}
	return result
}

func boundedReport(data []byte) error {
	if len(data) > localtools.MaxOutput {
		return errors.New("analysis report exceeds its 64 MiB limit")
	}
	return nil
}
