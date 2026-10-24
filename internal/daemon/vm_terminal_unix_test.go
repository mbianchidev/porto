//go:build !windows

package daemon

import (
	"os"
	"testing"

	"github.com/creack/pty"
)

type resizePTYFile struct{ *os.File }

func (file resizePTYFile) Resize(cols, rows int) error {
	return pty.Setsize(file.File, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func TestApplyTerminalResize(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open PTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	handled, err := applyTerminalResize(resizePTYFile{master}, []byte(`{"type":"resize","cols":132,"rows":41}`))
	if err != nil {
		t.Fatalf("apply resize: %v", err)
	}
	if !handled {
		t.Fatal("resize message was not handled")
	}
	rows, cols, err := pty.Getsize(master)
	if err != nil {
		t.Fatalf("read PTY size: %v", err)
	}
	if rows != 41 || cols != 132 {
		t.Fatalf("PTY size = %dx%d, want 41x132", rows, cols)
	}
}

func TestApplyTerminalResizeLeavesInputFramesUntouched(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open PTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	handled, err := applyTerminalResize(resizePTYFile{master}, []byte("echo hello"))
	if err != nil {
		t.Fatalf("apply input frame: %v", err)
	}
	if handled {
		t.Fatal("terminal input was mistaken for a resize message")
	}
}
