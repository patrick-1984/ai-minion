package devctl

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxCommandLen bounds the command line of one shell request.
const MaxCommandLen = 64 << 10

// MaxInputText bounds the text of one `input text`.
const MaxInputText = 1024

// ErrInvalidArgs is wrapped by every refusal of [ShellCommand] and
// [InputCommand]: the request is refused locally and nothing is sent (exit 2).
var ErrInvalidArgs = errors.New("invalid arguments")

func invalidArgs(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgs, fmt.Sprintf(format, a...))
}

// ShellCommand builds the command line of `minion adb shell -- ARGS...`. One
// argument is the command line itself, as `adb shell "ls | wc -l"` takes it;
// several are quoted one by one, so `-- sh -c 'exit 7'` runs exactly that.
func ShellCommand(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", invalidArgs("shell needs a command after --")
	}
	var cmd string
	if len(argv) == 1 {
		cmd = argv[0]
	} else {
		q := make([]string, len(argv))
		for i, a := range argv {
			q[i] = Quote(a)
		}
		cmd = strings.Join(q, " ")
	}
	if strings.TrimSpace(cmd) == "" {
		return "", invalidArgs("shell needs a command after --")
	}
	if strings.ContainsRune(cmd, 0) {
		return "", invalidArgs("the command holds a NUL byte")
	}
	if len(cmd) > MaxCommandLen {
		return "", invalidArgs("the command is longer than %d bytes", MaxCommandLen)
	}
	return cmd, nil
}

// Quote quotes s for a POSIX shell (mksh on Android) as one word.
func Quote(s string) string {
	if s != "" && safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var (
	safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	keycode  = regexp.MustCompile(`^(KEYCODE_[A-Z0-9_]{1,40}|[0-9]{1,3})$`)
)

// Input subcommands.
const (
	InputKeyevent = "keyevent"
	InputText     = "text"
	InputTap      = "tap"
	InputSwipe    = "swipe"
)

// maxCoord bounds a screen coordinate of tap and swipe.
const maxCoord = 10000

// InputCommand validates `minion adb input SUB ARGS...` and builds the
// `input` command line that the bridge runs over shell,v2. Anything it does
// not accept is refused here, before anything is sent.
func InputCommand(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", invalidArgs("input needs keyevent, text, tap or swipe")
	}
	sub, args := argv[0], argv[1:]
	switch sub {
	case InputKeyevent:
		if len(args) < 1 || len(args) > 8 {
			return "", invalidArgs("keyevent takes 1 to 8 key codes (KEYCODE_HOME or a number)")
		}
		for _, k := range args {
			if !keycode.MatchString(k) {
				return "", invalidArgs("keyevent: %q is not a key code (KEYCODE_NAME or a number)", short(k))
			}
		}
		return "input keyevent " + strings.Join(args, " "), nil
	case InputText:
		if len(args) != 1 || args[0] == "" {
			return "", invalidArgs("text takes exactly one non-empty argument")
		}
		t := args[0]
		if len(t) > MaxInputText {
			return "", invalidArgs("text is longer than %d bytes", MaxInputText)
		}
		for _, r := range t {
			if r < 0x20 || r > 0x7e {
				return "", invalidArgs("text accepts printable ASCII only")
			}
		}
		// `input text` reads %s as a space; a literal space must be %s too.
		return "input text " + Quote(strings.ReplaceAll(t, " ", "%s")), nil
	case InputTap:
		if len(args) != 2 {
			return "", invalidArgs("tap takes X Y")
		}
		if err := coords(args); err != nil {
			return "", err
		}
		return "input tap " + strings.Join(args, " "), nil
	case InputSwipe:
		if len(args) != 4 && len(args) != 5 {
			return "", invalidArgs("swipe takes X1 Y1 X2 Y2 [DURATION_MS]")
		}
		if err := coords(args[:4]); err != nil {
			return "", err
		}
		if len(args) == 5 {
			n, err := strconv.Atoi(args[4])
			if err != nil || n < 0 || n > 10000 || strconv.Itoa(n) != args[4] {
				return "", invalidArgs("swipe duration must be 0 to 10000 ms")
			}
		}
		return "input swipe " + strings.Join(args, " "), nil
	}
	return "", invalidArgs("input needs keyevent, text, tap or swipe, not %q", short(sub))
}

func coords(args []string) error {
	for _, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil || n < 0 || n > maxCoord || strconv.Itoa(n) != a {
			return invalidArgs("coordinates must be whole numbers from 0 to %d", maxCoord)
		}
	}
	return nil
}

// short cuts a refused value for an error text (it may be a typo of a secret).
func short(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}
