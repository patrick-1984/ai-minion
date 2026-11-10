package fakeagent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const marker = "-minion-fake-agent"

// Modes of the agent.
const (
	ModeLine   = "line"
	ModeBare   = "bare"
	ModeBP     = "bp"
	ModeDump   = "dump"
	ModeDumpBP = "dumpbp"
)

// Argv returns the argv that runs the agent in a pane: the test binary itself.
// ctl is the control file the test appends commands to, and out the file the
// agent appends its log to.
func Argv(mode, ctl, out string) []string { return ArgvFor(os.Args[0], mode, ctl, out) }

// ArgvFor is Argv for another path of the test binary (a symbolic link whose
// name is the process name the pane reports).
func ArgvFor(exe, mode, ctl, out string) []string {
	return []string{exe, marker, mode, ctl, out}
}

// Run runs the agent when this process was started with Argv and reports
// whether it did. A test binary calls it first in TestMain and exits when it
// returns true.
func Run() bool {
	if len(os.Args) != 5 || os.Args[1] != marker {
		return false
	}
	mode, ctl, out := os.Args[2], os.Args[3], os.Args[4]
	log, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return true
	}
	defer func() { _ = log.Close() }()
	a := &agent{ctl: ctl, log: log, out: os.Stdout}
	switch mode {
	case ModeLine:
		a.line("> ", "")
	case ModeBare:
		a.line("", "  ctrl+p commands\r\n")
	case ModeBP:
		a.raw()
		_, _ = a.out.WriteString("\x1b[?2004h")
		a.bp()
	case ModeDump, ModeDumpBP:
		a.raw()
		if mode == ModeDumpBP {
			_, _ = a.out.WriteString("\x1b[?2004h")
		}
		// "ready" tells the test that raw mode (and bracketed paste) is on:
		// the terminal handles the output in order.
		_, _ = a.out.WriteString("ready\r\n")
		a.dump()
	}
	return true
}

type agent struct {
	ctl  string
	off  int64
	log  *os.File
	out  *os.File
	rows int // the counter of "rows"
}

func (a *agent) raw() {
	cmd := exec.Command("stty", "raw", "-echo")
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

func (a *agent) print(s string) { _, _ = io.WriteString(a.out, s) }

// line reads lines in the terminal's line mode. The prompt is printed before
// every message and footer after every reply.
func (a *agent) line(prompt, footer string) {
	in := bufio.NewReader(os.Stdin)
	for {
		a.print(prompt)
		text, err := in.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(a.log, "submitted %q\n", strings.TrimSuffix(text, "\n"))
		if !a.reply() {
			return
		}
		a.print(footer)
	}
}

// bp is the input loop of an agent with bracketed paste: a pasted LF is part of
// the message, only a typed CR submits it.
func (a *agent) bp() {
	in := bufio.NewReader(os.Stdin)
	var msg []byte
	paste := false
	a.print("> ")
	for {
		b, err := in.ReadByte()
		if err != nil {
			return
		}
		switch {
		case b == 0x1b:
			seq, _ := in.Peek(5)
			switch {
			case strings.HasPrefix(string(seq), "[200~"):
				_, _ = in.Discard(5)
				paste = true
			case strings.HasPrefix(string(seq), "[201~"):
				_, _ = in.Discard(5)
				paste = false
			}
		case b == '\r' && !paste:
			_, _ = fmt.Fprintf(a.log, "submitted %q\n", msg)
			msg = nil
			a.print("\r\n")
			if !a.reply() {
				return
			}
			a.print("> ")
		case b == '\n' && paste:
			msg = append(msg, b)
			a.print("\r\n  ")
		case b >= 0x20 || b == '\t':
			msg = append(msg, b)
			a.print(string(b))
		}
	}
}

// dump appends every byte received to the log, as two hex digits, so that the
// test compares exactly what arrived.
func (a *agent) dump() {
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			_, _ = fmt.Fprintf(a.log, "%02x", buf[0])
		}
		if err != nil {
			return
		}
	}
}

// decode turns the escapes of a control text into bytes: \e is ESC and \n a
// carriage return and line feed.
func decode(s string) string {
	r := strings.NewReplacer(`\e`, "\x1b", `\n`, "\r\n")
	return r.Replace(s)
}

// reply runs control commands until "done" and reports whether the agent goes
// on (false after "exit").
//
// The commands, one per line of the control file:
//
//	rows N       N rows "r<k>", k counting up over the whole run
//	text S       the text S and a line break (S may use \e and \n)
//	raw S        the text S as it is, without a line break
//	sleep MS     wait
//	done         the reply is over: print the prompt, read the next message
//	exit         end the program
func (a *agent) reply() bool {
	for {
		line, ok := a.next()
		if !ok {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		cmd, arg, _ := strings.Cut(line, " ")
		switch cmd {
		case "rows":
			n, _ := strconv.Atoi(arg)
			for i := 0; i < n; i++ {
				a.rows++
				a.print(fmt.Sprintf("r%d\r\n", a.rows))
			}
		case "text":
			a.print(decode(arg) + "\r\n")
		case "raw":
			a.print(decode(arg))
		case "sleep":
			ms, _ := strconv.Atoi(arg)
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "done":
			return true
		case "exit":
			return false
		}
	}
}

// next returns the next complete line of the control file.
func (a *agent) next() (string, bool) {
	f, err := os.Open(a.ctl)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(a.off, io.SeekStart); err != nil {
		return "", false
	}
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil {
		return "", false
	}
	a.off += int64(len(line))
	return strings.TrimSuffix(line, "\n"), true
}
