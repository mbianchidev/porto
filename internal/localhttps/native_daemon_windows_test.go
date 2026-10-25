//go:build windows

package localhttps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
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
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &source); err != nil {
		return nil, err
	}
	defer source.Close()
	token, linkedErr := source.GetLinkedToken()
	if linkedErr != nil {
		create := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
		result, _, callErr := create.Call(uintptr(source), 5, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&token)))
		if result == 0 {
			return nil, errors.Join(errors.New("CI cannot create a non-administrator daemon token"), callErr)
		}
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
		return nil, err
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
