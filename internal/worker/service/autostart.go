package service

import (
	"errors"
	"fmt"
	"strings"
)

// The Windows Worker runs as the user who installs it (security fix S1). It
// is started at logon by a value in the user's own Run key,
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run, which needs no
// administrator: Windows runs the command line of each value when that user
// logs on (https://learn.microsoft.com/windows/win32/setupapi/run-and-runonce-registry-keys).
// The command is "minion.exe worker launch", a small supervisor (like
// Syncthing's monitor process) that starts "minion worker run" without a
// console window and starts it again when it ends with an error or for a
// self-update.

// AutostartRunKey is the Run key below HKEY_CURRENT_USER.
const AutostartRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// AutostartValueName is the name of the Worker's value in the Run key.
const AutostartValueName = "MinionWorker"

// LaunchCommand is the hidden subcommand of "minion worker" that the Run
// entry starts: the supervisor of the Worker.
const LaunchCommand = "launch"

// maxRunCommand is the longest command line a Run value may hold.
const maxRunCommand = 260

// ErrAutostartTooLong refuses a command line Windows would not run from the
// Run key.
var ErrAutostartTooLong = errors.New("the command line is longer than the 260 characters a Run entry may hold")

// AutostartCommand returns the Run value that starts exe as the Worker's
// supervisor with args (passed on to "worker run"), quoted the way Windows
// splits command lines.
func AutostartCommand(exe string, args []string) (string, error) {
	if strings.TrimSpace(exe) == "" {
		return "", errors.New("the path of minion.exe is empty")
	}
	parts := []string{quoteArg(exe), "worker", LaunchCommand}
	for _, a := range args {
		parts = append(parts, quoteArg(a))
	}
	cmd := strings.Join(parts, " ")
	if len(cmd) > maxRunCommand {
		return "", fmt.Errorf("%w (%d): move minion.exe or the state directory to a shorter path", ErrAutostartTooLong, len(cmd))
	}
	return cmd, nil
}

// quoteArg quotes a for a Windows command line (the rules of
// CommandLineToArgvW, as syscall.EscapeArg applies them): plain words stay as
// they are.
func quoteArg(a string) string {
	if a == "" {
		return `""`
	}
	if !strings.ContainsAny(a, " \t\"") {
		return a
	}
	b := []byte{'"'}
	slashes := 0
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b = append(b, '\\')
			}
			b = append(b, '\\')
		default:
			slashes = 0
		}
		b = append(b, c)
	}
	for ; slashes > 0; slashes-- {
		b = append(b, '\\')
	}
	return string(append(b, '"'))
}
