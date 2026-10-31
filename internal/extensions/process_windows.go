//go:build windows

package extensions

import "os/exec"

func cleanupProcess(*exec.Cmd) error {
	return failure(501, "extension_process_platform", "Native process grants are not supported on Windows; use isolated WebAssembly actions.")
}
