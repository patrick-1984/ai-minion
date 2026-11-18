package cli

// The offer to trust a Hub's TLS key pin (v0.11.0, C6). A command on a
// terminal whose Hub presented a certificate this computer does not trust
// shows the key pin of that certificate and asks the user to type it after
// comparing it with `minion hub identity --words` on the Hub host. Only the
// exact pin is an answer (copy and enter, never y/N): it is saved for the
// Hub's origin in hub_tls_pins in the config file and the call is made once more with it. The
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
	// save stores pin in hub_tls_pins for the origin of the Hub URL hubURL,
	// keeping the pins of other Hubs, and returns the file it went to.
	save func(pin, hubURL string) (string, error)
}

func defaultHubPinOffer() *hubPinOffer {
	return &hubPinOffer{
		interactive: func(in io.Reader, out io.Writer) bool { return isTerminalStream(in) && isTerminalWriter(out) },
		save: func(pin, hubURL string) (string, error) {
			return saveHubPin(os.LookupEnv, pin, hubURL)
		},
	}
}

// saveHubPin adds pin for the origin of hubURL to hub_tls_pins in the client
// config file, under the config lock, and keeps the entries of other Hubs.
func saveHubPin(lookupEnv func(string) (string, bool), pin, hubURL string) (string, error) {
	origin, err := config.PinOrigin(hubURL)
	if err != nil {
		return "", err
	}
	dir, err := clientStateDir(lookupEnv)
	if err != nil {
		return "", err
	}
	return updateWorkerSettings(dir, func(file *config.ConfigFile) error {
		cur, _, _ := file.Lookup(config.KeyHubTLSPins)
		pins, err := config.ParseHubTLSPins(cur)
		if err != nil {
			return fmt.Errorf("%s in the config file: %w; fix or remove it first", config.KeyHubTLSPins, err)
		}
		pins[origin] = pin
		file.Set(config.KeyHubTLSPins, config.FormatHubTLSPins(pins))
		return nil
	})
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
		pin, ok := p.offer.ask(p.cmd, certErr, p.cfg.URL)
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
func (o *hubPinOffer) ask(cmd *cobra.Command, e *client.HubCertError, hubURL string) (string, bool) {
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
	path, err := o.save(e.Pin, hubURL)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cannot save %s: %v\n", config.KeyHubTLSPin, err)
		return "", false
	}
	origin, _ := config.PinOrigin(hubURL)
	_, _ = fmt.Fprintf(out, "Saved the pin for %s in %s (%s; it is used for this Hub only); trying again.\n", origin, path, config.KeyHubTLSPins)
	return e.Pin, true
}

// clientStateDir is the state directory whose config file holds the client
// settings.
func clientStateDir(lookupEnv func(string) (string, bool)) (string, error) {
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: lookupEnv}
	dir, err := res.Resolve(config.KeyStateDir)
	if err != nil {
		return "", err
	}
	if dir.Value == "" {
		return "", errors.New("the state directory is not known; set MINION_STATE_DIR")
	}
	return dir.Value, nil
}

// writeWorkerSettings writes settings to the Worker's config file in
// stateDir, in one write under the config lock, and returns the path of the
// file.
func writeWorkerSettings(stateDir string, kv map[string]string) (string, error) {
	return updateWorkerSettings(stateDir, func(file *config.ConfigFile) error {
		for k, v := range kv {
			file.Set(k, v)
		}
		return nil
	})
}

// updateWorkerSettings changes the Worker's config file in stateDir with
// change, in one read and write under the config lock, and returns the path
// of the file. Nothing is written when change fails.
func updateWorkerSettings(stateDir string, change func(*config.ConfigFile) error) (string, error) {
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
	if err := change(file); err != nil {
		return "", err
	}
	return file.Path(), file.Save()
}

// deleteWorkerSetting removes one setting from the Worker's config file in
// stateDir, under the config lock, and reports whether it was there. A
// missing config file is left alone.
func deleteWorkerSetting(stateDir, key string) (bool, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return false, err
	}
	path := filepath.Join(abs, workerConfigName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	release, err := lockConfigFile(filepath.Join(abs, configLockName))
	if err != nil {
		return false, err
	}
	defer release()
	file, err := config.LoadFile(path)
	if err != nil {
		return false, err
	}
	if _, ok, _ := file.Lookup(key); !ok {
		return false, nil
	}
	file.Delete(key)
	return true, file.Save()
}
