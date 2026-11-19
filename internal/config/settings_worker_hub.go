package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Names of the Worker's Hub settings (Story 4.4), for callers that look them
// up by key.
const (
	KeyServer      = "server"
	KeyHubCAFile   = "hub_ca_file"
	KeyHubInsecure = "hub_insecure"
	// KeyHubFingerprint is the fingerprint of the Hub this Worker may pair or
	// enroll with (v0.11.0). It is parsed by its callers: the kernel package
	// config does not import identity.
	KeyHubFingerprint = "hub_fingerprint"
	// KeyPairingDisplay says where a Worker that pairs by itself shows its
	// pairing code (tray W8a): on the console and in the log, or only in the
	// status of the local control socket.
	KeyPairingDisplay = "pairing_display"
)

// Values of the pairing_display setting.
const (
	// PairingDisplayConsole prints the pairing code in the console box and the
	// log (AD-20), as "minion worker pair" does. It is the default.
	PairingDisplayConsole = "console"
	// PairingDisplayStatus shows the code only in the Worker's status (the tray
	// reads it from "minion worker status --json"); the console and the log
	// say that a code is shown, without the code. "minion worker join <url>
	// --background" saves it.
	PairingDisplayStatus = "status"
)

func init() { declareWorkerHub(Default) }

// declareWorkerHub declares the settings that say where the Hub is and how
// plain HTTPS calls to it are verified. The flags are --server, --hub-ca and
// --hub-insecure; the environment names are MINION_SERVER, MINION_HUB_CA and
// MINION_HUB_INSECURE.
//
// The CA file setting keeps the key hub_ca_file (config file, panel, `config
// set`), but its environment name is MINION_HUB_CA and its flag --hub-ca, not
// MINION_HUB_CA_FILE and --hub-ca-file: the registry derives the flag from the
// environment name and forbids an environment name that ends in _FILE, which
// is reserved for the file form of secrets (AD-19, registry validation).
//
// hub_ca_file and hub_insecure apply only to plain HTTPS calls (the REST
// client, the CLI and the setup connectivity test). They never apply to the
// Worker's WebSocket to the Hub: that channel is authenticated by the AD-6
// handshake (pin, invitation fingerprint or pairing code), never by the TLS
// certificate, so it needs no flag (A-15).
func declareWorkerHub(r *Registry) {
	r.Declare(Setting{
		Key:   KeyServer,
		Scope: ScopeWorker,
		Help: "URL of the Minion Hub this Worker connects to, for example https://hub.example:8443 " +
			"(https, http, wss or ws; a path prefix for a reverse proxy is kept). Unset runs the Worker alone (Solo)",
	})
	r.Declare(Setting{
		Key:   KeyHubCAFile,
		Scope: ScopeWorker,
		Env:   "MINION_HUB_CA",
		Help: "PEM file with a private CA that signed the Hub's certificate, for plain HTTPS calls to the Hub only " +
			"(the Worker's own Hub connection never needs it)",
	})
	r.Declare(Setting{
		Key:   KeyHubInsecure,
		Scope: ScopeWorker,
		Help: "Skip the verification of the Hub's TLS certificate in plain HTTPS calls to the Hub only " +
			"(true or false; the Worker's own Hub connection never needs it)",
		Default: "false",
		Choices: []string{"true", "false"},
	})
	r.Declare(Setting{
		Key:          KeyHubFingerprint,
		Scope:        ScopeWorker,
		BlankIsUnset: true,
		Help: "Fingerprint of the Hub this Worker may pair or enroll with: six words or 52 characters, as " +
			"\"minion hub identity --words\" prints on the Hub host. Any other Hub is refused. On a paired Worker " +
			"it is compared with the paired Hub at every start",
	})
	r.Declare(Setting{
		Key:   KeyPairingDisplay,
		Scope: ScopeWorker,
		Help: "Where a Worker that pairs by itself shows its pairing code: console (the console and the log) or " +
			"status (only minion worker status, for the tray; the console and the log leave the code out)",
		Default: PairingDisplayConsole,
		Choices: []string{PairingDisplayConsole, PairingDisplayStatus},
	})
}

// ResolvePairingDisplay resolves pairing_display with res (a Worker
// resolver); nil res means the default.
func ResolvePairingDisplay(res *Resolver) (string, error) {
	if res == nil {
		return PairingDisplayConsole, nil
	}
	v, err := res.Resolve(KeyPairingDisplay)
	if err != nil {
		return "", err
	}
	return v.Value, nil
}

// WorkerHub is the resolved Hub configuration of a Worker run.
type WorkerHub struct {
	// Server is the Hub URL as configured, trimmed and validated. It is empty
	// when no Hub is configured: the Worker then runs alone (Solo).
	Server string
	// CAFile is the private CA file for plain HTTPS calls; empty when unset.
	CAFile string
	// Insecure turns off certificate verification for plain HTTPS calls.
	Insecure bool
	// Fingerprint is the expected Hub fingerprint as configured (trimmed,
	// not parsed here); empty when unset.
	Fingerprint string
}

// ErrInvalidServer is wrapped by the error [ValidateServerURL] returns.
var ErrInvalidServer = errors.New("invalid Hub URL")

// ResolveWorkerHub resolves the Hub settings with res (a Worker resolver) and
// validates them.
func ResolveWorkerHub(res *Resolver) (WorkerHub, error) {
	var wh WorkerHub
	server, err := res.Resolve(KeyServer)
	if err != nil {
		return wh, err
	}
	if wh.Server, err = ValidateServerURL(server.Value); err != nil {
		return wh, fmt.Errorf("setting %q (%s): %w", KeyServer, server.Source, err)
	}
	ca, err := res.Resolve(KeyHubCAFile)
	if err != nil {
		return wh, err
	}
	wh.CAFile = strings.TrimSpace(ca.Value)
	ins, err := res.Resolve(KeyHubInsecure)
	if err != nil {
		return wh, err
	}
	if wh.Insecure, err = strconv.ParseBool(ins.Value); err != nil {
		return wh, fmt.Errorf("setting %q (%s): invalid value %q, expected true or false", KeyHubInsecure, ins.Source, ins.Value)
	}
	fp, err := res.Resolve(KeyHubFingerprint)
	if err != nil {
		return wh, err
	}
	wh.Fingerprint = strings.TrimSpace(fp.Value)
	return wh, nil
}

// ValidateServerURL trims v and checks it is a usable Hub URL: scheme https,
// http, wss or ws, a host, and no user name or password. The empty string is
// valid and means "no Hub". The error never repeats v, because a mistyped URL
// may carry a credential.
func ValidateServerURL(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("%w: it is not a valid URL", ErrInvalidServer)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http", "wss", "ws":
	default:
		return "", fmt.Errorf("%w: it must start with https://, http://, wss:// or ws://", ErrInvalidServer)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%w: it has no host", ErrInvalidServer)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: it must not contain a user name or password", ErrInvalidServer)
	}
	return v, nil
}
