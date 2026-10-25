//go:build windows

package localhttps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/mbianchidev/porto/internal/process"
	"golang.org/x/sys/windows"
)

func prepareNativeDaemonOwner(string) error { return nil }

func nativeUnprivilegedDaemon(t *testing.T, ctx context.Context, binary, home, ui string) (*exec.Cmd, error) {
	command := process.NewCommand(ctx, "", binary, "daemon", "start")
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_HOME="+home, "PORTO_UI_DIR="+ui)
	if !windows.GetCurrentProcessToken().IsElevated() {
		return command, nil
	}
	var source windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &source); err != nil {
		return nil, fmt.Errorf("open source token for restricted daemon: %w", err)
	}
	defer source.Close()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	disabled := windows.SIDAndAttributes{Sid: administrators}
	var token windows.Token
	create := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
	// Keep the original logon identity while disabling administrator rights.
	result, _, callErr := create.Call(uintptr(source), 1, 1, uintptr(unsafe.Pointer(&disabled)), 0, 0, 0, 0, uintptr(unsafe.Pointer(&token)))
	if result == 0 {
		return nil, errors.Join(errors.New("CI cannot create a non-administrator daemon token"), callErr)
	}
	medium, err := windows.StringToSid("S-1-16-8192")
	if err != nil {
		token.Close()
		return nil, err
	}
	label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: medium, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(token, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)),
		uint32(unsafe.Sizeof(label))+uint32(medium.Len())); err != nil {
		token.Close()
		return nil, fmt.Errorf("set restricted daemon medium integrity: %w", err)
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		token.Close()
		return nil, err
	}
	for _, group := range groups.AllGroups() {
		if group.Sid.String() == "S-1-5-32-544" && group.Attributes&windows.SE_GROUP_ENABLED != 0 {
			token.Close()
			return nil, errors.New("refusing to run actual daemon acceptance with enabled administrator membership")
		}
	}
	t.Cleanup(func() { token.Close() })
	command.SysProcAttr.Token = syscall.Token(token)
	return command, nil
}

func startNativeDaemon(t *testing.T, command *exec.Cmd) (err error) {
	if command.SysProcAttr.Token == 0 {
		return command.Start()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" +
		user.User.Sid.String() + ")S:(ML;;NW;;;ME)")
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor}
	user32 := windows.NewLazySystemDLL("user32.dll")
	getStation := user32.NewProc("GetProcessWindowStation")
	setStation := user32.NewProc("SetProcessWindowStation")
	createStation := user32.NewProc("CreateWindowStationW")
	closeStation := user32.NewProc("CloseWindowStation")
	getDesktop := user32.NewProc("GetThreadDesktop")
	setDesktop := user32.NewProc("SetThreadDesktop")
	createDesktop := user32.NewProc("CreateDesktopW")
	closeDesktop := user32.NewProc("CloseDesktop")
	oldStation, _, callErr := getStation.Call()
	if oldStation == 0 {
		return fmt.Errorf("read daemon test window station: %w", callErr)
	}
	oldDesktop, _, callErr := getDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	if oldDesktop == 0 {
		return fmt.Errorf("read daemon test desktop: %w", callErr)
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("PortoHTTPS-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		return err
	}
	station, _, callErr := createStation.Call(uintptr(unsafe.Pointer(name)), 1, 0xf037f, uintptr(unsafe.Pointer(&attributes)))
	if station == 0 {
		return fmt.Errorf("create isolated daemon test window station: %w", callErr)
	}
	t.Cleanup(func() { closeStation.Call(station) })
	if result, _, callErr := setStation.Call(station); result == 0 {
		return fmt.Errorf("select isolated daemon test station: %w", callErr)
	}
	defer func() {
		if result, _, restoreErr := setStation.Call(oldStation); result == 0 {
			err = errors.Join(err, fmt.Errorf("restore test process station: %w", restoreErr))
		}
		if result, _, restoreErr := setDesktop.Call(oldDesktop); result == 0 {
			err = errors.Join(err, fmt.Errorf("restore test thread desktop: %w", restoreErr))
		}
	}()
	desktopName, _ := windows.UTF16PtrFromString("Default")
	desktop, _, callErr := createDesktop.Call(uintptr(unsafe.Pointer(desktopName)), 0, 0, 0, 0xf01ff, uintptr(unsafe.Pointer(&attributes)))
	if desktop == 0 {
		return fmt.Errorf("create isolated daemon test desktop: %w", callErr)
	}
	t.Cleanup(func() { closeDesktop.Call(desktop) })
	if result, _, callErr := setDesktop.Call(desktop); result == 0 {
		return fmt.Errorf("select isolated daemon test desktop: %w", callErr)
	}
	// The restricted child cannot use the hosted administrator's desktop ACL.
	// Give it a private, medium-integrity desktop without changing that ACL.
	return command.Start()
}
