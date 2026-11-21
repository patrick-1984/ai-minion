//go:build windows

package cli

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

type replaceConsole struct {
	io.Reader
	io.Writer
	in, out *os.File
}

func (t *replaceConsole) Close() error { return errors.Join(t.in.Close(), t.out.Close()) }

func openReplaceTerminal() (io.ReadWriteCloser, error) {
	in, err := os.Open("CONIN$")
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	return &replaceConsole{Reader: in, Writer: out, in: in, out: out}, nil
}

func replaceAutostartState() string {
	entry, ok, _ := service.Autostart()
	if !ok {
		return ""
	}
	args, err := windows.DecomposeCommandLine(entry)
	if err != nil {
		return ""
	}
	for i, arg := range args {
		if arg == "--state-dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func saveReplaceRegistration() (func() error, error) {
	entry, ok, err := service.Autostart()
	if err != nil {
		return nil, err
	}
	if !ok {
		return service.DeleteAutostart, nil
	}
	return func() error { return service.SetAutostart(entry) }, nil
}
