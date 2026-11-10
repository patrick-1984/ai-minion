package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Process exit statuses of the op commands (AD-15 outcome display). Status 1 is
// every other error and 2 a problem with the command line.
const (
	ExitOK            = 0
	ExitError         = 1
	ExitUsage         = 2
	ExitFailedNotSent = 3
	ExitPartial       = 4 // partial or unknown
)

// exitCodeError ends the process with a status without an error message: the
// command has already printed its result.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// outcomeExitCode maps the outcome of a write to the exit status: 0 for
// delivered, 3 for failed_not_sent, 4 for partial, unknown and anything this
// version does not know (never 0 for a result that is not a confirmed
// delivery).
func outcomeExitCode(o ops.Outcome) int {
	switch o {
	case ops.OutcomeDelivered:
		return ExitOK
	case ops.OutcomeFailedNotSent:
		return ExitFailedNotSent
	default:
		return ExitPartial
	}
}

// printer renders op results, in human form or as JSON (--json).
type printer struct {
	w    io.Writer
	json bool
}

// result prints the final result of op. A streaming op has already printed its
// elements, so it has no final output.
func (p printer) result(op ops.Op, raw json.RawMessage) error {
	if op.Stream {
		return nil
	}
	return p.value(op, raw)
}

// element prints one element of a streaming op as it arrives.
func (p printer) element(op ops.Op, raw json.RawMessage) error { return p.value(op, raw) }

func (p printer) value(op ops.Op, raw json.RawMessage) error {
	if p.json {
		return p.compact(raw)
	}
	v := op.NewResult()
	if len(raw) == 0 || json.Unmarshal(raw, v) != nil {
		return p.indented(raw)
	}
	ew := &errWriter{w: p.w}
	switch r := v.(type) {
	case *ops.WriteResult:
		renderWrite(ew, *r)
		renderExtras(ew, raw)
	case *ops.SessionsListResult:
		renderSessions(ew, *r)
	case *ops.SessionSnapshotResult:
		renderScreen(ew, r.Screen)
	case *ops.SessionCaptureResult:
		renderScreen(ew, r.Screen)
	case *ops.PipeReadResult:
		if len(r.Turns) == 0 {
			_, _ = io.WriteString(ew, "no pipe turns\n")
		}
		for _, t := range r.Turns {
			renderTurn(ew, t)
		}
	case *ops.PipeTurn:
		renderTurn(ew, *r)
	default:
		return p.indented(raw)
	}
	return ew.err
}

// errWriter remembers the first error of the writer under it and refuses every
// write after it, so that a renderer needs no check per line and the caller
// still learns that the output was lost (a full disk, a closed pipe).
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(b []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(b)
	if err != nil {
		e.err = err
	}
	return n, err
}

func (p printer) compact(raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return fmt.Errorf("the result is not valid JSON: %w", err)
	}
	b.WriteByte('\n')
	_, err := p.w.Write(b.Bytes())
	return err
}

func (p printer) indented(raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return fmt.Errorf("the result is not valid JSON: %w", err)
	}
	b.WriteByte('\n')
	_, err := p.w.Write(b.Bytes())
	return err
}

// safe removes control characters (except newline and tab) from text that
// came from the other side, so that a hostile Hub or a remote pane cannot
// drive the user's terminal with escape sequences.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), r == unicode.ReplacementChar:
			return -1
		}
		return r
	}, s)
}

// oneLine is safe text on a single line.
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(safe(s))
}

// renderWrite prints the outcome, reason, forceable flag and warnings of a
// write result (AD-15).
func renderWrite(w *errWriter, r ops.WriteResult) {
	_, _ = fmt.Fprintf(w, "outcome: %s\n", oneLine(string(r.Outcome)))
	if r.Reason != "" {
		_, _ = fmt.Fprintf(w, "reason: %s\n", oneLine(r.Reason))
	}
	_, _ = fmt.Fprintf(w, "forceable: %t\n", r.Forceable)
	if r.Forced {
		_, _ = fmt.Fprintln(w, "forced: true")
	}
	for _, wn := range r.Warnings {
		_, _ = fmt.Fprintf(w, "warning: %s: %s\n", oneLine(wn.Code), oneLine(wn.Message))
	}
	if r.Pane != nil {
		_, _ = fmt.Fprintf(w, "pane: %s %s (%d.%d)", oneLine(r.Pane.Session), oneLine(r.Pane.ID), r.Pane.Window, r.Pane.Index)
		if r.Pane.Kind != "" {
			_, _ = fmt.Fprintf(w, " kind=%s", oneLine(r.Pane.Kind))
		}
		if r.Pane.State != "" {
			_, _ = fmt.Fprintf(w, " state=%s", oneLine(r.Pane.State))
		}
		_, _ = fmt.Fprintln(w)
	}
	if r.Forceable {
		_, _ = fmt.Fprintln(w, "hint: repeat the command with --force to override this refusal")
	}
}

// writeResultMembers are the members of a WriteResult. A result of a write op
// may carry more (the Hub's user_create returns the new account and its
// temporary password next to them); renderExtras prints those.
var writeResultMembers = map[string]bool{
	"outcome": true, "reason": true, "forceable": true, "forced": true, "warnings": true, "pane": true,
}

// renderExtras prints the members of a write result that are not part of the
// WriteResult contract, one "name: value" line each in name order. A one-time
// credential such as temporary_password would otherwise be lost, because the
// human output is the only chance to see it.
func renderExtras(w *errWriter, raw json.RawMessage) {
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return
	}
	var names []string
	for k := range all {
		if !writeResultMembers[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		var str string
		var b bytes.Buffer
		switch {
		case json.Unmarshal(all[k], &str) == nil:
		case json.Compact(&b, all[k]) == nil:
			str = b.String()
		default:
			continue
		}
		_, _ = fmt.Fprintf(w, "%s: %s\n", oneLine(k), oneLine(str))
	}
	if _, ok := all["temporary_password"]; ok {
		_, _ = fmt.Fprintln(w, "note: the temporary password is shown only this once")
	}
}

func renderSessions(w *errWriter, r ops.SessionsListResult) {
	if len(r.Sessions) == 0 {
		_, _ = fmt.Fprintln(w, "no sessions")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tWINDOWS\tATTACHED\tKIND\tSTATE\tTASK")
	for _, s := range r.Sessions {
		kind, state := "-", "-"
		if s.Pane != nil {
			kind, state = dash(oneLine(s.Pane.Kind)), dash(oneLine(s.Pane.State))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%t\t%s\t%s\t%s\n", oneLine(s.Name), s.Windows, s.Attached, kind, state, oneLine(s.Task))
	}
	if err := tw.Flush(); err != nil && w.err == nil {
		w.err = err
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func renderScreen(w *errWriter, s ops.Screen) {
	for _, l := range s.Lines {
		_, _ = fmt.Fprintln(w, safe(strings.ReplaceAll(l, "\n", " ")))
	}
}

// renderTurn prints one pipe turn: a header with the cursor (the turn id, for
// --after-turn-id), then the text indented by two spaces.
func renderTurn(w *errWriter, t ops.PipeTurn) {
	_, _ = fmt.Fprintf(w, "%s  %s  %s\n", oneLine(t.TurnID), t.At.Time().Format("2006-01-02T15:04:05.000Z"), oneLine(t.Direction))
	for _, line := range strings.Split(strings.TrimRight(safe(t.Text), "\n"), "\n") {
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
}
