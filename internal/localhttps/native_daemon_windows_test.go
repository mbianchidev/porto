//go:build windows

package localhttps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
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

func startNativeDaemon(t *testing.T, ctx context.Context, command *exec.Cmd) (err error) {
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
	createDesktop := user32.NewProc("CreateDesktopW")
	closeDesktop := user32.NewProc("CloseDesktop")
	oldStation, _, callErr := getStation.Call()
	if oldStation == 0 {
		return fmt.Errorf("read daemon test window station: %w", callErr)
	}
	stationName := fmt.Sprintf("PortoHTTPS-%d-%d", os.Getpid(), time.Now().UnixNano())
	name, err := windows.UTF16PtrFromString(stationName)
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
	}()
	desktopName, _ := windows.UTF16PtrFromString("Default")
	desktop, _, callErr := createDesktop.Call(uintptr(unsafe.Pointer(desktopName)), 0, 0, 0, 0xf01ff, uintptr(unsafe.Pointer(&attributes)))
	if desktop == 0 {
		return fmt.Errorf("create isolated daemon test desktop: %w", callErr)
	}
	t.Cleanup(func() { closeDesktop.Call(desktop) })
	// The restricted child cannot use the hosted administrator's desktop ACL.
	// exec.Cmd cannot set STARTUPINFO.lpDesktop, so start it explicitly on the
	// private medium-integrity desktop without changing any interactive ACL.
	privateDesktop, _ := windows.UTF16PtrFromString(stationName + `\Default`)
	executable, err := windows.UTF16PtrFromString(command.Path)
	if err != nil {
		return err
	}
	arguments := make([]string, len(command.Args))
	for n, argument := range command.Args {
		arguments[n] = syscall.EscapeArg(argument)
	}
	line, err := windows.UTF16PtrFromString(strings.Join(arguments, " "))
	if err != nil {
		return err
	}
	environment := utf16.Encode([]rune(strings.Join(command.Env, "\x00") + "\x00\x00"))
	input, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer input.Close()
	output, ok := command.Stdout.(*os.File)
	if !ok || command.Stderr != command.Stdout {
		return errors.New("native daemon fixture requires one owned log file for both streams")
	}
	duplicate := func(file *os.File) (windows.Handle, error) {
		var handle windows.Handle
		err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(),
			&handle, 0, true, windows.DUPLICATE_SAME_ACCESS)
		return handle, err
	}
	inputHandle, err := duplicate(input)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(inputHandle)
	outputHandle, err := duplicate(output)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(outputHandle)
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Desktop: privateDesktop,
		Flags:      windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW,
		ShowWindow: windows.SW_HIDE, StdInput: inputHandle, StdOutput: outputHandle, StdErr: outputHandle}
	var information windows.ProcessInformation
	if err := windows.CreateProcessAsUser(windows.Token(command.SysProcAttr.Token), executable, line, nil, nil,
		true, command.SysProcAttr.CreationFlags|windows.CREATE_UNICODE_ENVIRONMENT, &environment[0], nil, &startup, &information); err != nil {
		return fmt.Errorf("start restricted daemon on explicit private desktop: %w", err)
	}
	windows.CloseHandle(information.Thread)
	defer windows.CloseHandle(information.Process)
	command.Process, err = os.FindProcess(int(information.ProcessId))
	if err != nil {
		_ = windows.TerminateProcess(information.Process, 1)
		return err
	}
	go func(process *os.Process) { <-ctx.Done(); _ = process.Kill() }(command.Process)
	return nil
}
