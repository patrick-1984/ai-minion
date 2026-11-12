package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/adminsock"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The commands of `minion hub identity` that act on the running Hub (contract
// docs/architecture/proposals/hub-identity-v2.md, section 8.3). They ask for
// the passphrase on the terminal with echo off and refuse to run when standard
// input is not a terminal: there is no flag, environment variable or file for
// the passphrase. The passphrase goes to the Hub over the local admin socket
// (internal/hub/adminsock), never over the network or a reverse proxy. The
// Hub takes the actor from the kernel credentials of the socket peer and keeps
// a failure counter, lockout and hourly allowance of its own for the commands.

// Exit statuses of the identity commands, besides ExitOK (0), ExitError (1) and
// ExitUsage (2).
const (
	// ExitIdentityConflict: the state of the Hub does not allow the command,
	// or the change could not be confirmed and must be repeated.
	ExitIdentityConflict = 3
	// ExitPassphraseInvalid: the passphrase was not accepted.
	ExitPassphraseInvalid = 5
	// ExitIdentityLimited: locked after failures, or the hourly allowance is used up.
	ExitIdentityLimited = 6
	// ExitHubUnreachable: no running Hub answers on the admin socket.
	ExitHubUnreachable = 7
)

// identityDeps are the seams of the identity commands that tests replace.
type identityDeps struct {
	lookupEnv func(string) (string, bool)
	// isTerminal reports whether standard input is a terminal.
	isTerminal func() bool
	// readSecret prompts on the terminal and reads one line with echo off.
	readSecret func(prompt string) (string, error)
	// readLine prompts and reads one visible line (the word ROTATE).
	readLine func(prompt string) (string, error)
	// dial connects to the admin socket.
	dial func(ctx context.Context, path string) (net.Conn, error)
	// checkSocket verifies that the socket file is the Hub's own before a
	// passphrase is sent to it.
	checkSocket func(path string) error
	timeout     time.Duration
}

func defaultIdentityDeps() identityDeps {
	return identityDeps{
		isTerminal:  func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }, //nolint:gosec // a file descriptor fits int
		readSecret:  ttySecret,
		readLine:    ttyLine,
		dial:        dialSocket,
		checkSocket: checkSocketFile,
		timeout:     2 * time.Minute,
	}
}

// ExitInterrupted is the exit status when Ctrl+C or SIGTERM ended a prompt.
const ExitInterrupted = 130

// errInterrupted: a signal ended the prompt; the terminal state is restored.
var errInterrupted = errors.New("interrupted")

// readCancellable runs read in a goroutine and waits for it or for a signal.
// On a signal it calls restore (the saved terminal state, so echo is back
// on), writes a newline and returns errInterrupted; the goroutine that still
// waits in the terminal read ends with the process.
func readCancellable(sigs <-chan os.Signal, restore func(), read func() (string, error)) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := read()
		ch <- result{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-sigs:
		if restore != nil {
			restore()
		}
		_, _ = fmt.Fprintln(os.Stderr)
		return "", errInterrupted
	}
}

func ttySecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd()) //nolint:gosec // a file descriptor fits int
	_, _ = fmt.Fprint(os.Stderr, prompt)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var restore func()
	if st, err := term.GetState(fd); err == nil {
		restore = func() { _ = term.Restore(fd, st) }
	}
	s, err := readCancellable(sigs, restore, func() (string, error) {
		b, err := term.ReadPassword(fd)
		if err != nil {
			return "", err
		}
		s := string(b)
		for i := range b {
			b[i] = 0
		}
		return s, nil
	})
	if err == nil || !errors.Is(err, errInterrupted) {
		_, _ = fmt.Fprintln(os.Stderr)
	}
	return s, err
}

func ttyLine(prompt string) (string, error) {
	_, _ = fmt.Fprint(os.Stderr, prompt)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return readCancellable(sigs, nil, func() (string, error) {
		var line []byte
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if n == 1 {
				if buf[0] == '\n' {
					break
				}
				line = append(line, buf[0])
			}
			if err != nil {
				if errors.Is(err, io.EOF) && len(line) > 0 {
					break
				}
				return "", err
			}
		}
		return strings.TrimSpace(string(line)), nil
	})
}

func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// socketPath resolves the admin socket of the data directory.
func (d identityDeps) socketPath() (string, error) {
	res := &config.Resolver{Role: config.ScopeHub, LookupEnv: d.lookupEnv}
	dir, err := res.Resolve(config.KeyDataDir)
	if err != nil {
		return "", err
	}
	return adminsock.Path(dir.Value), nil
}

func addIdentityCommands(identity *cobra.Command, deps identityDeps) {
	add := func(use, short, long string, aliases []string, flags func(*cobra.Command) func() ops.IssueMembers, run func(c *identityRun, opts ops.IssueMembers) error) {
		cmd := &cobra.Command{
			Use: use, Short: short, Long: long, Aliases: aliases, Args: cobra.NoArgs,
			SilenceUsage: true, SilenceErrors: true,
		}
		cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
		var issue func() ops.IssueMembers
		if flags != nil {
			issue = flags(cmd)
		}
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			var opts ops.IssueMembers
			if issue != nil {
				opts = issue()
			}
			return run(&identityRun{cmd: cmd, deps: deps}, opts)
		}
		identity.AddCommand(cmd)
	}
	issueFlags := func(cmd *cobra.Command) func() ops.IssueMembers {
		days := cmd.Flags().Int("validity", 0, "certificate validity in days, 1 to 400 (default 180)")
		floor := cmd.Flags().Int64("serial-floor", 0, "the new serial must be greater than this value")
		far := cmd.Flags().Bool("allow-far-floor", false, "allow a serial floor more than 366 days ahead")
		return func() ops.IssueMembers {
			return ops.IssueMembers{ValidityDays: *days, SerialFloor: *floor, AllowFarFloor: *far}
		}
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the state of the Hub identity (needs the running Hub)",
		Long: `Show the state of the Hub identity: bootstrap state, root fingerprint, the
certificate serial and validity, the expiry warning, and the passphrase
lockout of the web and of the commands. It asks the running Hub over its local
admin socket and needs no passphrase.`,
		Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
	}
	status.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	asJSON := status.Flags().Bool("json", false, "print the status as JSON")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		return (&identityRun{cmd: cmd, deps: deps}).status(*asJSON)
	}
	identity.AddCommand(status)

	add("set-passphrase", "Set the Hub passphrase and issue the first certificate",
		`Seal the Hub root under a passphrase and issue the first certificate. Only while
no sealed root exists (a new Hub, or one migrated from 0.6); otherwise use
reissue. The passphrase is asked on the terminal, twice. Choose a long one and
keep it safe: it cannot be recovered.`, nil, issueFlags,
		func(c *identityRun, opts ops.IssueMembers) error {
			if err := c.needTTY("set-passphrase"); err != nil {
				return err
			}
			pass, err := c.secret("New passphrase: ")
			if err != nil {
				return err
			}
			conf, err := c.secret("Repeat the passphrase: ")
			if err != nil {
				return err
			}
			return c.operate(adminsock.PathSetPassphrase, ops.HubIdentitySetPassphraseArgs{Passphrase: pass, Confirm: conf, IssueMembers: opts})
		})
	add("reissue", "Re-issue the Hub certificate under the same root",
		`Issue a new operational key and certificate under the same root. All Worker
sessions close and reconnect. The root, its fingerprint, the pins and the
invitations are untouched. The passphrase is asked on the terminal.`, nil, issueFlags,
		func(c *identityRun, opts ops.IssueMembers) error {
			if err := c.needTTY("reissue"); err != nil {
				return err
			}
			pass, err := c.secret("Passphrase: ")
			if err != nil {
				return err
			}
			return c.operate(adminsock.PathReissue, ops.HubIdentityReissueArgs{Passphrase: pass, IssueMembers: opts})
		})
	add("rotate-master", "Rotate the master key (the Hub root)",
		`Create a new root under a new passphrase. The transition is signed by the old
root, a new certificate is issued, and all invitations are revoked. Workers
follow the chain at their next connect; none is re-paired. It asks for the
current passphrase, then the new one twice, then the word ROTATE.`, []string{"rotate"}, issueFlags,
		func(c *identityRun, opts ops.IssueMembers) error {
			if err := c.needTTY("rotate-master"); err != nil {
				return err
			}
			cur, err := c.secret("Current passphrase: ")
			if err != nil {
				return err
			}
			next, err := c.secret("New passphrase: ")
			if err != nil {
				return err
			}
			conf, err := c.secret("Repeat the new passphrase: ")
			if err != nil {
				return err
			}
			word, err := c.deps.readLine("Type ROTATE to confirm: ")
			if errors.Is(err, errInterrupted) {
				return &exitCodeError{ExitInterrupted}
			}
			if err != nil {
				return &usageError{errors.New("the confirmation could not be read")}
			}
			return c.operate(adminsock.PathRotateMaster, ops.HubIdentityRotateArgs{Passphrase: cur, NewPassphrase: next, Confirm: conf, Word: word, IssueMembers: opts})
		})
	add("change-passphrase", "Change the Hub passphrase",
		`Seal the same root under a new passphrase. No certificate, key, fingerprint or
Worker session changes. Backups made earlier still open with the old passphrase.
It asks for the current passphrase, then the new one twice.`, nil, nil,
		func(c *identityRun, _ ops.IssueMembers) error {
			if err := c.needTTY("change-passphrase"); err != nil {
				return err
			}
			cur, err := c.secret("Current passphrase: ")
			if err != nil {
				return err
			}
			next, err := c.secret("New passphrase: ")
			if err != nil {
				return err
			}
			conf, err := c.secret("Repeat the new passphrase: ")
			if err != nil {
				return err
			}
			return c.operate(adminsock.PathChangePassphrase, ops.HubIdentityChangePassphraseArgs{Passphrase: cur, NewPassphrase: next, Confirm: conf})
		})
}

// identityRun is one run of an identity command.
type identityRun struct {
	cmd  *cobra.Command
	deps identityDeps
}

// needTTY refuses to run when standard input is not a terminal.
func (c *identityRun) needTTY(name string) error {
	if c.deps.isTerminal != nil && c.deps.isTerminal() {
		return nil
	}
	return &usageError{fmt.Errorf("minion hub identity %s asks for the passphrase on a terminal, but standard input is not a terminal; there is no flag, environment variable or file for the passphrase", name)}
}

func (c *identityRun) secret(prompt string) (string, error) {
	s, err := c.deps.readSecret(prompt)
	if errors.Is(err, errInterrupted) {
		return "", &exitCodeError{ExitInterrupted}
	}
	if err != nil {
		return "", &usageError{errors.New("the passphrase could not be read from the terminal")}
	}
	return s, nil
}

// api is the error envelope of the Hub.
type identityAPIError struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// call sends one request to the admin socket and decodes the answer into out.
func (c *identityRun) call(method, path string, body any, out any) error {
	sock, err := c.deps.socketPath()
	if err != nil {
		return err
	}
	if c.deps.checkSocket != nil {
		if err := c.deps.checkSocket(sock); err != nil {
			return c.unreachable(sock, err)
		}
	}
	ctx, cancel := context.WithTimeout(c.cmd.Context(), c.deps.timeout)
	defer cancel()
	tr := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return c.deps.dial(ctx, sock) },
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://hub"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return c.unreachable(sock, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("the Hub's answer could not be read")
	}
	if resp.StatusCode/100 != 2 {
		return c.fail(raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("the Hub's answer could not be understood")
	}
	return nil
}

func (c *identityRun) unreachable(sock string, err error) error {
	_, _ = fmt.Fprintf(c.cmd.ErrOrStderr(), "Error: no running Hub answers on %s (%s). Start the Hub with the same MINION_DATA_DIR.\n", sock, safeNetText(err))
	return &exitCodeError{ExitHubUnreachable}
}

// safeNetText names the class of a connection failure without a path.
func safeNetText(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "no socket"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return "not reachable"
}

// fail prints the refusal of the Hub and returns the exit status.
func (c *identityRun) fail(raw []byte) error {
	var env identityAPIError
	if json.Unmarshal(raw, &env) != nil || env.Error.Code == "" {
		return errors.New("the Hub refused the request")
	}
	msg := env.Error.Message
	if at, ok := env.Error.Details["retry_at"].(string); ok && at != "" {
		msg += " (" + at + ")"
	}
	w := c.cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "Error: %s\n", sanitizeLine(msg))
	switch env.Error.Code {
	case "passphrase_invalid":
		return &exitCodeError{ExitPassphraseInvalid}
	case "rate_limited":
		return &exitCodeError{ExitIdentityLimited}
	case "conflict":
		return &exitCodeError{ExitIdentityConflict}
	case "invalid_argument":
		return &exitCodeError{ExitUsage}
	}
	return &exitCodeError{ExitError}
}

func sanitizeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func (c *identityRun) operate(path string, args any) error {
	var res ops.HubIdentityResult
	if err := c.call(http.MethodPost, path, args, &res); err != nil {
		return err
	}
	w := c.cmd.OutOrStdout()
	switch res.Kind {
	case "passphrase_change":
		_, _ = fmt.Fprintln(w, "The passphrase was changed. The root, the certificate and the Worker sessions are unchanged.")
	case "passphrase_set":
		_, _ = fmt.Fprintf(w, "The passphrase is set and the first certificate is issued.\n")
	case "rotate":
		_, _ = fmt.Fprintf(w, "The master key was rotated.\n")
	default:
		_, _ = fmt.Fprintf(w, "The certificate was re-issued.\n")
	}
	if res.Kind != "passphrase_change" {
		_, _ = fmt.Fprintf(w, "Root fingerprint:  %s\nCertificate serial: %d\nValid until:       %s\n",
			res.RootFingerprint, res.Serial, res.NotAfter.Time().Format("2006-01-02 15:04 UTC"))
	}
	for _, wn := range res.Warnings {
		_, _ = fmt.Fprintf(w, "Note: %s\n", sanitizeLine(wn.Message))
	}
	return nil
}

func (c *identityRun) status(asJSON bool) error {
	var st ops.HubIdentityStatusResult
	if err := c.call(http.MethodGet, adminsock.PathStatus, nil, &st); err != nil {
		return err
	}
	w := c.cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(w)
		return enc.Encode(st)
	}
	_, _ = fmt.Fprintf(w, "State:              %s\n", st.StateLabel)
	_, _ = fmt.Fprintf(w, "Passphrase:         %s\n", map[bool]string{true: "set", false: "not set yet"}[st.PassphraseSet])
	_, _ = fmt.Fprintf(w, "Workers can connect: %s\n", map[bool]string{true: "yes", false: "no"}[st.Serving])
	if st.RootFingerprint != "" {
		_, _ = fmt.Fprintf(w, "Root fingerprint:   %s\n", st.RootFingerprint)
	}
	if st.OpFingerprint != "" {
		_, _ = fmt.Fprintf(w, "Operational key:    %s\n", st.OpFingerprint)
	}
	if st.NotAfter != nil {
		_, _ = fmt.Fprintf(w, "Certificate serial: %d\n", st.Serial)
		_, _ = fmt.Fprintf(w, "Valid from:         %s\n", st.NotBefore.Time().Format("2006-01-02 15:04 UTC"))
		_, _ = fmt.Fprintf(w, "Valid until:        %s (%d days left)\n", st.NotAfter.Time().Format("2006-01-02 15:04 UTC"), st.DaysLeft)
	}
	if st.Warning != "" {
		_, _ = fmt.Fprintf(w, "Warning:            %s\n", st.Warning)
	}
	if st.LastOp != "" && st.LastOpAt != nil {
		_, _ = fmt.Fprintf(w, "Last change:        %s at %s\n", st.LastOp, st.LastOpAt.Time().Format("2006-01-02 15:04 UTC"))
	}
	for _, sc := range []struct {
		name string
		s    ops.IdentityScopeStatus
	}{{"web", st.Web}, {"commands", st.CLI}} {
		if sc.s.Locked {
			_, _ = fmt.Fprintf(w, "Lockout (%s): locked, %d seconds left\n", sc.name, sc.s.RetryAfterSecs)
		} else {
			_, _ = fmt.Fprintf(w, "Lockout (%s): none (%d failures, %d attempts this hour)\n", sc.name, sc.s.Failures, sc.s.AttemptsThisHour)
		}
	}
	return nil
}
