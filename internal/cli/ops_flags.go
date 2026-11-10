package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// This file derives CLI flags from the argument struct of a catalog op
// (AD-2). The JSON member name of every exported field becomes a kebab-case
// flag; the kind of the Go type picks the flag type. Only flags the user
// passes become members of the request, so a missing flag is a missing JSON
// member, which decodes to the zero value exactly like on every other surface.

// argKind is the flag type derived from the Go type of an argument field.
type argKind int

const (
	kindString     argKind = iota // string (named string types too); "-" reads stdin
	kindBool                      // bool and *bool
	kindInt                       // every signed integer
	kindUint                      // every unsigned integer
	kindFloat                     // float32 and float64
	kindStringList                // []string, repeatable flag, no comma splitting
	kindStringLike                // a type that encodes as a JSON string, such as ops.Timestamp
	kindTarget                    // ops.Target: the --target flag
	kindJSON                      // anything else: the flag value is a JSON document
)

// argField is one JSON member of an op's argument struct.
type argField struct {
	JSON string       // JSON member name, snake_case
	Flag string       // flag name, kebab-case
	Kind argKind      // flag type
	Type reflect.Type // Go type of the field (pointers kept)
}

// secret reports whether the member holds a secret by its name. A secret is
// never taken from argv, where ps, shell history and logs would show it (NFR1,
// AD-19): only "-" (standard input) is accepted for it.
func (f argField) secret() bool {
	if f.Kind != kindString && f.Kind != kindJSON {
		return false
	}
	n := strings.ToLower(f.JSON)
	return strings.Contains(n, "secret") || strings.Contains(n, "password")
}

// fileFlag is the name of the flag that reads a secret member from a file: the
// second source that keeps the value off argv (the spine convention for secrets
// is a file, as with _FILE settings). It is "" for a member that is not secret.
func (f argField) fileFlag() string {
	if !f.secret() {
		return ""
	}
	return f.Flag + "-file"
}

// stdinToken, as the value of a string flag, reads the value from standard
// input so that content (and secrets) need not be on argv.
const stdinToken = "-"

// Flags every op command has; an argument field must not use these names.
const (
	flagJSON     = "json"
	flagHelpName = "help"
)

var (
	targetType = reflect.TypeFor[ops.Target]()
	jsonUnmar  = reflect.TypeFor[json.Unmarshaler]()
)

// deriveFields lists the JSON members of t (a struct type) in declaration
// order, flattening embedded structs without a tag like encoding/json does.
// A name declared at several depths resolves to the shallowest one. It returns
// an error when two members map to one flag name or a flag name is reserved.
func deriveFields(t reflect.Type) ([]argField, error) {
	type cand struct {
		f     argField
		depth int
	}
	var all []cand
	var walk func(t reflect.Type, depth int, seen map[reflect.Type]bool)
	walk = func(t reflect.Type, depth int, seen map[reflect.Type]bool) {
		if seen[t] {
			return
		}
		seen[t] = true
		defer delete(seen, t)
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			tag := sf.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if sf.Anonymous && name == "" {
				ft := sf.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct && ft != targetType {
					walk(ft, depth+1, seen)
					continue
				}
			}
			if !sf.IsExported() {
				continue
			}
			if name == "" {
				name = sf.Name
			}
			all = append(all, cand{argField{JSON: name, Flag: flagName(name), Kind: classify(sf.Type), Type: sf.Type}, depth})
		}
	}
	walk(t, 0, map[reflect.Type]bool{})

	best := map[string]int{}
	for _, c := range all {
		if d, ok := best[c.f.JSON]; !ok || c.depth < d {
			best[c.f.JSON] = c.depth
		}
	}
	var out []argField
	taken := map[string]string{}
	done := map[string]bool{}
	for _, c := range all {
		if c.depth != best[c.f.JSON] || done[c.f.JSON] {
			continue
		}
		done[c.f.JSON] = true
		if c.f.Flag == flagJSON || c.f.Flag == flagHelpName {
			return nil, fmt.Errorf("argument %q maps to the reserved flag --%s", c.f.JSON, c.f.Flag)
		}
		if other, dup := taken[c.f.Flag]; dup {
			return nil, fmt.Errorf("arguments %q and %q map to the same flag --%s", other, c.f.JSON, c.f.Flag)
		}
		taken[c.f.Flag] = c.f.JSON
		out = append(out, c.f)
	}
	for _, f := range out {
		ff := f.fileFlag()
		if ff == "" {
			continue
		}
		if other, dup := taken[ff]; dup {
			return nil, fmt.Errorf("arguments %q and %q map to the same flag --%s", other, f.JSON, ff)
		}
		taken[ff] = f.JSON
	}
	return out, nil
}

// flagName turns a JSON member name into a flag name.
func flagName(json string) string {
	return strings.ReplaceAll(strings.ToLower(json), "_", "-")
}

// classify picks the flag kind for a Go type.
func classify(t reflect.Type) argKind {
	if t == targetType || (t.Kind() == reflect.Pointer && t.Elem() == targetType) {
		return kindTarget
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.String && reflect.PointerTo(t).Implements(jsonUnmar) && encodesAsString(t) {
		return kindStringLike
	}
	switch t.Kind() {
	case reflect.String:
		return kindString
	case reflect.Bool:
		return kindBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return kindInt
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return kindUint
	case reflect.Float32, reflect.Float64:
		return kindFloat
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String && !reflect.PointerTo(t).Implements(jsonUnmar) {
			return kindStringList
		}
	}
	return kindJSON
}

// encodesAsString reports whether the zero value of t encodes as a JSON string.
func encodesAsString(t reflect.Type) bool {
	b, err := json.Marshal(reflect.Zero(t).Interface())
	return err == nil && len(b) > 0 && b[0] == '"'
}

// flagHelp is the help text of the well-known argument names. Other names get
// a text made from the name.
var flagHelp = map[string]string{
	"request_id":    "request id, a UUIDv7 (default: a new one); a repeat of the same id within 10 minutes is answered from the Worker's cache and never delivers twice",
	"force":         "override FORCE guards in this call (only when you pass it)",
	"text":          "the `text`; \"-\" reads it from standard input",
	"task":          "the task `text`; \"-\" reads it from standard input",
	"keys":          "a tmux key name such as Enter or C-c; repeat the flag for more keys",
	"lines":         "number of scrollback `lines` (0: the default)",
	"limit":         "maximum number of results (0: the default)",
	"timeout_ms":    "stop waiting after this many `milliseconds` (0: the default)",
	"after_turn_id": "start after this pipe turn id (a UUIDv7)",
	"name":          "the `name`",
	"value":         "the new `value`; \"-\" reads it from standard input (for a secret setting the value is read from standard input only)",
	"cwd":           "starting `directory`",
	"argv":          "program and arguments to run without a shell; repeat the flag for each element",
	"choice":        "the option to choose: a decimal number is the 1-based position in the reviewed dialog (always the position, even if an option has a numeric label); otherwise an option label or key such as yes or no",
	"submit":        "press Enter after the text in the same request, on the same pane (default: no Enter); if the text went out and the Enter did not, the outcome is partial or unknown",
	"expect":        "the reviewed dialog as a `JSON` document {\"pane_id\",\"pid\",\"command\",\"session_created\",\"dialog_digest\"} copied from the pane object of session_snapshot or session_capture (--json); \"-\" reads it from standard input. The answer reaches only that dialog: if it changed, nothing is sent (pane_changed or dialog_changed) and --force does not override that",
}

const targetHelp = "target session as `[worker-id:]session[:window.pane]` (default: the current tmux session)"

func helpFor(op string, f argField) string {
	if d := ops.MemberDoc(op, f.JSON); d != "" {
		if f.Kind == kindString {
			d += ` "-" reads it from standard input.`
		}
		return d
	}
	if f.Kind == kindTarget {
		return targetHelp
	}
	if f.secret() {
		return strings.ReplaceAll(f.JSON, "_", " ") + " (a secret): never taken from the command line; pass \"-\" to read it from standard input, or use --" + f.fileFlag() + " (a trailing newline is removed)"
	}
	if h, ok := flagHelp[f.JSON]; ok {
		return h
	}
	switch f.Kind {
	case kindStringLike:
		return strings.ReplaceAll(f.JSON, "_", " ") + " as an RFC 3339 `timestamp`"
	case kindStringList:
		return strings.ReplaceAll(f.JSON, "_", " ") + "; repeat the flag for more values"
	case kindJSON:
		return strings.ReplaceAll(f.JSON, "_", " ") + " as a `JSON` document; \"-\" reads it from standard input"
	case kindString:
		return strings.ReplaceAll(f.JSON, "_", " ") + "; \"-\" reads it from standard input"
	}
	return strings.ReplaceAll(f.JSON, "_", " ")
}

// bindFlags registers one flag per field on fs.
func bindFlags(fs *pflag.FlagSet, op string, fields []argField) {
	for _, f := range fields {
		usage := helpFor(op, f)
		switch f.Kind {
		case kindBool:
			fs.Bool(f.Flag, false, usage)
		case kindInt:
			fs.Int64(f.Flag, 0, usage)
		case kindUint:
			fs.Uint64(f.Flag, 0, usage)
		case kindFloat:
			fs.Float64(f.Flag, 0, usage)
		case kindStringList:
			fs.StringArray(f.Flag, nil, usage)
		default: // string, string-like, target, JSON
			fs.String(f.Flag, "", usage)
		}
		if ff := f.fileFlag(); ff != "" {
			fs.String(ff, "", "read the "+strings.ReplaceAll(f.JSON, "_", " ")+" (a secret) from the file at `path`, so that it is not on the command line (a trailing newline is removed)")
		}
	}
}

// usageError is a problem with the command line: exit status 2.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, a ...any) error { return &usageError{fmt.Errorf(format, a...)} }

// maxStdinBytes bounds what a "-" value reads from standard input.
const maxStdinBytes = 1 << 20

// stdinOnce reads standard input at most once for a command.
type stdinOnce struct {
	r    io.Reader
	used bool
}

func (s *stdinOnce) read(ctx context.Context, flag string) (string, error) {
	if s.used {
		return "", usagef("--%s: standard input was already used by another flag", flag)
	}
	s.used = true
	// A read of a terminal or a pipe cannot be interrupted, so it runs on its
	// own goroutine and Ctrl-C or SIGTERM (the context) ends the wait. The
	// goroutine ends with the process or when the input ends.
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(s.r, maxStdinBytes+1))
		done <- result{b, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		return "", fmt.Errorf("read standard input for --%s: %w", flag, ctx.Err())
	}
	if res.err != nil {
		return "", fmt.Errorf("read standard input for --%s: %w", flag, res.err)
	}
	if len(res.b) > maxStdinBytes {
		return "", usagef("--%s: standard input is larger than %d bytes", flag, maxStdinBytes)
	}
	return string(res.b), nil
}

// collectArgs builds the JSON members of the request from the flags the user
// passed. The target member is left to the caller (see resolveTarget). A secret
// is accepted from standard input ("-") or from a file (--<flag>-file) only,
// never from the command line.
func collectArgs(ctx context.Context, cmd *cobra.Command, fields []argField, stdin *stdinOnce) (map[string]json.RawMessage, error) {
	members := map[string]json.RawMessage{}
	literal := map[string]bool{} // string members given on the command line
	fs := cmd.Flags()
	for _, f := range fields {
		if ff := f.fileFlag(); ff != "" && fs.Changed(ff) {
			if fs.Changed(f.Flag) {
				return nil, usagef("--%s and --%s both give the same value; use one of them", f.Flag, ff)
			}
			raw, err := secretFromFile(ctx, fs, f, stdin)
			if err != nil {
				return nil, err
			}
			members[f.JSON] = raw
			continue
		}
		if f.Kind == kindTarget || !fs.Changed(f.Flag) {
			continue
		}
		if f.Kind == kindString || f.Kind == kindJSON {
			if v, _ := fs.GetString(f.Flag); v != stdinToken {
				if f.secret() {
					return nil, usagef("--%s is a secret and is never taken from the command line; pass --%s - and give the value on standard input, or use --%s <path>", f.Flag, f.Flag, f.fileFlag())
				}
				literal[f.JSON] = true
			}
		}
		raw, err := flagValue(ctx, fs, f, stdin)
		if err != nil {
			return nil, err
		}
		members[f.JSON] = raw
	}
	// settings_set style requests: the value of a secret setting.
	var key string
	if json.Unmarshal(members["key"], &key) == nil {
		if st, ok := config.Default.Lookup(key); ok && st.Secret {
			if literal["value"] {
				return nil, usagef("--value: %q is a secret setting and its value is never taken from the command line; pass --value - and give the value on standard input", key)
			}
			// The value came from standard input: like every secret it
			// loses the newline that echo or an editor added.
			var v string
			if json.Unmarshal(members["value"], &v) == nil {
				members["value"], _ = json.Marshal(trimOneNewline(v))
			}
		}
	}
	return members, nil
}

// trimOneNewline removes a single trailing "\n" or "\r\n". A secret that is
// piped in or kept in a file normally ends with the newline of echo or an
// editor, which is not part of the secret. Only one is removed, so a secret
// that really ends in a line break can still be given with a second one.
func trimOneNewline(s string) string {
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		s = t
		return strings.TrimSuffix(s, "\r")
	}
	return s
}

// secretFromFile reads the secret member f from the file named by its file
// flag. The path "-" means standard input, like the main flag.
func secretFromFile(ctx context.Context, fs *pflag.FlagSet, f argField, stdin *stdinOnce) (json.RawMessage, error) {
	ff := f.fileFlag()
	path, err := fs.GetString(ff)
	if err != nil {
		return nil, usagef("--%s: %v", ff, err)
	}
	var s string
	switch path {
	case "":
		return nil, usagef("--%s: a file path is required", ff)
	case stdinToken:
		if s, err = stdin.read(ctx, ff); err != nil {
			return nil, err
		}
	default:
		fh, err := os.Open(path)
		if err != nil {
			return nil, usagef("--%s: %v", ff, err)
		}
		defer func() { _ = fh.Close() }()
		b, err := io.ReadAll(io.LimitReader(fh, maxStdinBytes+1))
		if err != nil {
			return nil, usagef("--%s: %v", ff, err)
		}
		if len(b) > maxStdinBytes {
			return nil, usagef("--%s: the file is larger than %d bytes", ff, maxStdinBytes)
		}
		s = string(b)
	}
	return jsonValue(f, trimOneNewline(s))
}

// jsonValue turns the text of a secret member into its JSON value: a string
// member is the string, a JSON member must be a JSON document.
func jsonValue(f argField, s string) (json.RawMessage, error) {
	if f.Kind == kindJSON {
		s = strings.TrimSpace(s)
		if !json.Valid([]byte(s)) {
			return nil, usagef("--%s: the value is not valid JSON", f.Flag)
		}
		return json.RawMessage(s), nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, usagef("--%s: %v", f.Flag, err)
	}
	return raw, nil
}

func flagValue(ctx context.Context, fs *pflag.FlagSet, f argField, stdin *stdinOnce) (json.RawMessage, error) {
	var v any
	var err error
	switch f.Kind {
	case kindBool:
		v, err = fs.GetBool(f.Flag)
	case kindInt:
		v, err = fs.GetInt64(f.Flag)
	case kindUint:
		v, err = fs.GetUint64(f.Flag)
	case kindFloat:
		v, err = fs.GetFloat64(f.Flag)
	case kindStringList:
		v, err = fs.GetStringArray(f.Flag)
	case kindString, kindStringLike, kindJSON:
		var s string
		if s, err = fs.GetString(f.Flag); err != nil {
			break
		}
		if s == stdinToken {
			if s, err = stdin.read(ctx, f.Flag); err != nil {
				return nil, err
			}
			if f.secret() {
				s = trimOneNewline(s)
			}
		}
		if f.Kind == kindJSON {
			s = strings.TrimSpace(s)
			if !json.Valid([]byte(s)) {
				return nil, usagef("--%s: the value is not valid JSON", f.Flag)
			}
			return json.RawMessage(s), nil
		}
		v = s
	}
	if err != nil {
		return nil, usagef("--%s: %v", f.Flag, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, usagef("--%s: %v", f.Flag, err)
	}
	return raw, nil
}

// validateArgs decodes the members into the op's argument type and runs its
// Validate method, so that a bad call fails before anything is sent. The
// request itself carries the members as given.
func validateArgs(op ops.Op, members map[string]json.RawMessage) (json.RawMessage, error) {
	raw, err := json.Marshal(members)
	if err != nil {
		return nil, err
	}
	target := op.NewArgs()
	if err := json.Unmarshal(raw, target); err != nil {
		return nil, usagef("invalid arguments: %v", err)
	}
	if v, ok := target.(ops.Validator); ok {
		if err := v.Validate(); err != nil {
			var ae *ops.ArgumentError
			if errors.As(err, &ae) {
				return nil, usagef("%s", ae.Error())
			}
			return nil, usagef("%v", err)
		}
	}
	return raw, nil
}
