//go:build windows

package localhttps

import (
	"golang.org/x/sys/windows"
)

func nativeBrowserArguments() []string { return nil }

func assertNativeHelperLeastPrivilege() error {
	service, err := openHTTPSService(true)
	if err != nil {
		return err
	}
	defer service.Close()
	return requireOwnedService(service)
}

func crashNativeHelper() error {
	service, err := openHTTPSService(true)
	if err != nil {
		return err
	}
	defer service.Close()
	if err := requireOwnedService(service); err != nil {
		return err
	}
	status, err := service.Query()
	if err != nil {
		return err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, status.ProcessId)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.TerminateProcess(handle, 37)
}
