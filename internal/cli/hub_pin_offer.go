package cli

// The offer to trust a Hub's TLS key pin (v0.11.0, C6). A command on a
// terminal whose Hub presented a certificate this computer does not trust
// shows the key pin of that certificate and asks the user to type it after
// comparing it with `minion hub identity --words` on the Hub host. Only the
// exact pin is an answer (copy and enter, never y/N): it is saved as
// hub_tls_pin in the config file and the call is made once more with it. The
// certificate error came before anything was sent, so the second attempt
// cannot run an operation twice.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
)

// hubPinOffer are the seams of the offer that tests replace.
type hubPinOffer struct {
	// interactive reports whether the user can be asked: standard input and
	// standard output are terminals.
	interactive func(in io.Reader, out io.Writer) bool
	// save stores pin as hub_tls_pin and returns the file it went to.
	save func(pin string) (string, error)
}

func defaultHubPinOffer() *hubPinOffer {
	return &hubPinOffer{
		interactive: func(in io.Reader, out io.Writer) bool { return isTerminalStream(in) && isTerminalWriter(out) },
		save:        func(pin string) (string, error) { return saveClientSetting(os.LookupEnv, config.KeyHubTLSPin, pin) },
	}
}

// offerPin wraps the Hub backend b of the settings cfg so that a certificate
// error of the first call may be answered by trusting the key pin. It returns
// b unchanged when there is nothing to offer: no seams, or a client that
// already says how to trust the Hub (a CA file, a pin or the insecure
// switch).
func (o *hubPinOffer) wrap(cmd *cobra.Command, cfg config.ClientHub, b client.Backend) client.Backend {
	if o == nil || cfg.CAFile != "" || cfg.Insecure || cfg.TLSPin != "" {
		return b
	}
	return &pinOfferBackend{backend: b, cmd: cmd, cfg: cfg, offer: o}
}

type pinOfferBackend struct {
	cmd   *cobra.Command
	cfg   config.ClientHub
	offer *hubPinOffer
	once  sync.Once

	mu      sync.Mutex
	backend client.Backend // the pinned backend once the pin was saved
}

func (p *pinOfferBackend) current() client.Backend {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.backend
}

func (p *pinOfferBackend) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	raw, err := p.current().Call(ctx, op, args)
	if nb := p.retry(err); nb != nil {
		return nb.Call(ctx, op, args)
	}
	return raw, err
}

func (p *pinOfferBackend) Stream(ctx context.Context, op string, args any, onProgress func(json.RawMessage) error) (json.RawMessage, error) {
	raw, err := p.current().Stream(ctx, op, args, onProgress)
	if nb := p.retry(err); nb != nil {
		return nb.Stream(ctx, op, args, onProgress)
	}
	return raw, err
}

// retry asks once per command and returns the pinned backend to call again
// with, or nil.
func (p *pinOfferBackend) retry(err error) client.Backend {
	var certErr *client.HubCertError
	if !errors.As(err, &certErr) || certErr.Expected != nil || certErr.Pin == "" {
		return nil
	}
	var nb client.Backend
	p.once.Do(func() {
		pin, ok := p.offer.ask(p.cmd, certErr)
		if !ok {
			return
		}
		cfg := p.cfg
		cfg.TLSPin = pin
		b, err := hubBackend(cfg, p.cmd.ErrOrStderr())
		if err != nil {
			_, _ = fmt.Fprintf(p.cmd.ErrOrStderr(), "cannot use the saved pin: %v\n", err)
			return
		}
		nb = b
		p.mu.Lock()
		p.backend = b // later calls of this command use the pin too
		p.mu.Unlock()
	})
	return nb
}

// ask shows the pin and saves it when the user typed exactly that pin. It
// reports whether the pin was saved.
func (o *hubPinOffer) ask(cmd *cobra.Command, e *client.HubCertError) (string, bool) {
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	if o.interactive == nil || !o.interactive(in, out) {
		return "", false
	}
	_, _ = fmt.Fprintf(out, "The Hub's certificate is not trusted by this computer (%s). Its key pin is %s; "+
		"compare it with `minion hub identity --words` on the Hub host. Type the pin to trust it, or anything else to stop: ",
		e.Reason, e.Pin)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if strings.TrimSpace(line) != e.Pin {
		_, _ = fmt.Fprintln(out, "Not trusted; nothing was saved.")
		return "", false
	}
	path, err := o.save(e.Pin)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cannot save %s: %v\n", config.KeyHubTLSPin, err)
		return "", false
	}
	_, _ = fmt.Fprintf(out, "Saved %s in %s; trying again.\n", config.KeyHubTLSPin, path)
	return e.Pin, true
}

// saveClientSetting writes one setting to the config file of the Worker's
// state directory, the file the client settings are read from, under the
// config lock. It returns the path of the file.
func saveClientSetting(lookupEnv func(string) (string, bool), key, value string) (string, error) {
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: lookupEnv}
	dir, err := res.Resolve(config.KeyStateDir)
	if err != nil {
		return "", err
	}
	if dir.Value == "" {
		return "", errors.New("the state directory is not known; set MINION_STATE_DIR")
	}
	return writeWorkerSetting(dir.Value, key, value)
}

// writeWorkerSetting writes one setting to the Worker's config file in
// stateDir, under the config lock, and returns the path of the file.
func writeWorkerSetting(stateDir, key, value string) (string, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	release, err := lockConfigFile(filepath.Join(abs, configLockName))
	if err != nil {
		return "", err
	}
	defer release()
	file, err := config.LoadFile(filepath.Join(abs, workerConfigName))
	if err != nil {
		return "", err
	}
	file.Set(key, value)
	return file.Path(), file.Save()
}
