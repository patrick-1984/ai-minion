package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Names of the settings of the Hub client: the command line (minion, mn)
// and minion mcp when they call a Hub over its REST API instead of the local
// control socket (AD-22, Story 6.9).
const (
	// KeyHubURL is the base URL of the Hub (MINION_HUB_URL, --hub-url).
	KeyHubURL = "hub_url"
	// KeyAPIKey is the API key. It exists only as a secret: the key is read
	// from the file named by MINION_API_KEY_FILE and from nowhere else.
	KeyAPIKey = "api_key"
	// KeyHubWorker is the Worker a Hub call is routed to when the arguments
	// name none (MINION_HUB_WORKER, --hub-worker).
	KeyHubWorker = "hub_worker"
	// KeyHubTLSPin is the TLS key pin (or pins) of the Hub
	// (MINION_HUB_TLS_PIN, --hub-tls-pin). When set it replaces the
	// verification of the certificate chain for this client.
	KeyHubTLSPin = "hub_tls_pin"
	// KeyHubTLSPins are the TLS key pins that the interactive offer saved in
	// the config file, one entry per Hub origin (scheme://host:port, see
	// [PinOrigin]): "<origin>=<pin>[;<pin>]", entries separated by spaces.
	// The entry of the origin in use applies; a pin trusted for one Hub never
	// replaces or serves another one.
	KeyHubTLSPins = "hub_tls_pins"
)

// APIKeyFileEnv is the environment variable that names the file of the API key.
const APIKeyFileEnv = "MINION_API_KEY_FILE"

// Client is the registry of the Hub client's settings. It is separate from
// [Default] on purpose: these settings belong to the callers of a Hub, not to
// a Worker or the Hub itself, so "minion worker run" and "minion worker config"
// neither gain a flag for them nor resolve them. A resolver reads it with
// Role [ScopeWorker]: flag, then environment, then the config file, then the
// default.
var Client = NewRegistry()

func init() { declareClient(Client) }

// declareClient declares the client settings. hub_ca_file and hub_insecure
// share the keys, environment names and meaning of the Worker's settings of
// the same name (they apply to plain HTTPS calls to the Hub only), so one
// MINION_HUB_CA serves both. The flag of the CA file is --hub-ca-file (the
// registry cannot derive it: an environment name must not end in _FILE).
func declareClient(r *Registry) {
	r.Declare(Setting{
		Key:   KeyHubURL,
		Scope: ScopeWorker,
		Help: "URL of the Minion Hub to call over its REST API, for example https://hub.example:8443 " +
			"(a path prefix for a reverse proxy is kept). When set, commands and minion mcp use the Hub " +
			"and never the local Worker; it needs " + APIKeyFileEnv,
	})
	r.Declare(Setting{
		Key:    KeyAPIKey,
		Scope:  ScopeWorker,
		Secret: true,
		Help: "API key (mn_...) for the Hub. It is read only from the file named by " + APIKeyFileEnv +
			"; it is never accepted on the command line, in a plain environment variable or in the config file",
	})
	r.Declare(Setting{
		Key:   KeyHubCAFile,
		Scope: ScopeWorker,
		Env:   "MINION_HUB_CA",
		Help:  "PEM file with the private CA that signed the Hub's certificate (flag --hub-ca-file); replaces the system roots for this client only",
	})
	r.Declare(Setting{
		Key:   KeyHubInsecure,
		Scope: ScopeWorker,
		Help: "Skip the verification of the Hub's TLS certificate for this client only (true or false). " +
			"Anyone on the network path can then read the API key; prefer --hub-ca-file",
		Default: "false",
		Choices: []string{"true", "false"},
	})
	r.Declare(Setting{
		Key:   KeyHubTLSPin,
		Scope: ScopeWorker,
		Help: "TLS key pin of the Hub, sha256//<base64> as curl --pinnedpubkey takes it; several are separated by \";\". " +
			"When set, this client accepts exactly a Hub certificate with that key, whoever signed it, instead of verifying " +
			"the certificate chain. Take the value from the \"TLS key pin\" line of \"minion hub identity --words\" on the Hub host",
	})
	r.Declare(Setting{
		Key:   KeyHubTLSPins,
		Scope: ScopeWorker,
		Help: "TLS key pins saved when you trust a Hub's key at the prompt, one entry per Hub origin: " +
			"<scheme>://<host>:<port>=<pin>[;<pin>], entries separated by spaces. The entry of the Hub in use applies " +
			"(hub_tls_pin given by flag or environment comes first); a pin saved for one Hub is never used for another",
	})
	r.Declare(Setting{
		Key:   KeyHubWorker,
		Scope: ScopeWorker,
		Help: "Id (UUIDv7) of the Worker that runs the operations whose arguments name none, such as sessions_list and " +
			"session_create; a Worker name is not accepted here (the Hub's REST API takes only an id in worker_id). " +
			"A target of the form <worker-id>:<session> names its own Worker",
	})
}

// ClientHub is the resolved Hub configuration of a client run.
type ClientHub struct {
	// URL is the Hub's base URL, trimmed and validated. It is empty when no
	// Hub is configured.
	URL string
	// APIKeyFile is the path from MINION_API_KEY_FILE; empty when unset. The
	// key itself is read from it for each request, never here.
	APIKeyFile string
	// CAFile is the private CA file; empty when unset.
	CAFile string
	// Insecure turns the certificate verification off for this client.
	Insecure bool
	// TLSPin is the TLS key pin setting as given (one or more sha256//...
	// separated by ";"); empty when unset. The client parses it.
	TLSPin string
	// Worker is the id (UUIDv7) of the default Worker of target-less Worker
	// operations; empty when none is set. It is never a name.
	Worker string
}

// Configured reports whether the user asked for a Hub: a Hub URL or a key file
// is set. A half configuration is an error for the caller to report (it must
// never fall back to the local Worker).
func (c ClientHub) Configured() bool { return c.URL != "" || c.APIKeyFile != "" }

// ErrInvalidHubURL is wrapped by the error [ValidateHubURL] returns.
var ErrInvalidHubURL = errors.New("invalid Hub URL")

// hubKeyShapeRE matches text shaped like a Minion API key, in any letter case.
var hubKeyShapeRE = regexp.MustCompile(`(?i)mn_[a-z0-9]{16,}`)

// LooksLikeKey reports whether s (also percent-decoded) holds text shaped like
// an API key. A Hub URL that does would put a credential into a request path,
// a log line or a message, so it is refused without being repeated.
func LooksLikeKey(s string) bool {
	if hubKeyShapeRE.MatchString(s) {
		return true
	}
	d, err := url.PathUnescape(s)
	return err == nil && d != s && hubKeyShapeRE.MatchString(d)
}

// ErrAPIKeyNotAccepted is returned when the API key is offered in a way that
// leaks it: a plain environment variable or the config file.
var ErrAPIKeyNotAccepted = errors.New("the API key must be given only as a file")

// ValidateHubURL trims v and checks it is a usable base URL for the REST
// client: http or https, a host, no user name or password, no query and no
// fragment. The empty string is valid and means "no Hub". The error never
// repeats v, because a mistyped URL may carry a credential. (The refusal of
// plain http to a host that is not this machine is the client's policy, see
// internal/client.)
func ValidateHubURL(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("%w: it is not a valid URL", ErrInvalidHubURL)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http":
	default:
		return "", fmt.Errorf("%w: it must start with https:// or http://", ErrInvalidHubURL)
	}
	switch {
	case u.Hostname() == "":
		return "", fmt.Errorf("%w: it has no host", ErrInvalidHubURL)
	case u.User != nil:
		return "", fmt.Errorf("%w: it must not contain a user name or password; the API key goes in the file named by %s", ErrInvalidHubURL, APIKeyFileEnv)
	case u.RawQuery != "" || u.Fragment != "" || strings.Contains(v, "?") || strings.Contains(v, "#"):
		return "", fmt.Errorf("%w: it must not contain a query or a fragment", ErrInvalidHubURL)
	case LooksLikeKey(v):
		return "", fmt.Errorf("%w: it contains something that looks like an API key; the key goes only in the file named by %s", ErrInvalidHubURL, APIKeyFileEnv)
	}
	return v, nil
}

// ResolveClientHub resolves the client settings with res (a [ScopeWorker]
// resolver over [Client]) and validates them. The API key is not read: only the
// path of its file is returned.
//
// The key is accepted from the file named by MINION_API_KEY_FILE only. The
// plain MINION_API_KEY and a key stored in the config file are errors, because
// both are routes by which a secret ends up in process listings, shell history
// or backups. The key has no flag at all.
func ResolveClientHub(res *Resolver) (ClientHub, error) {
	var c ClientHub
	reg := res.registry()
	if _, ok := reg.Lookup(KeyHubURL); !ok {
		return c, errors.New("config: the client settings are not declared in this registry")
	}
	hubURL, err := res.Resolve(KeyHubURL)
	if err != nil {
		return c, err
	}
	if c.URL, err = ValidateHubURL(hubURL.Value); err != nil {
		return c, fmt.Errorf("setting %q (%s): %w", KeyHubURL, hubURL.Source, err)
	}
	key, _ := reg.Lookup(KeyAPIKey)
	if v, ok := res.lookupEnv(key.EnvName()); ok && v != "" {
		return c, fmt.Errorf("%w: %s is not accepted; put the key in a file and set %s to its path", ErrAPIKeyNotAccepted, key.EnvName(), APIKeyFileEnv)
	}
	if res.File != nil {
		if _, ok, _ := res.File.Lookup(KeyAPIKey); ok {
			return c, fmt.Errorf("%w: it is not accepted in the config file; put the key in a file and set %s to its path", ErrAPIKeyNotAccepted, APIKeyFileEnv)
		}
	}
	if p, ok := res.lookupEnv(key.FileEnvName()); ok {
		c.APIKeyFile = strings.TrimSpace(p)
	}
	ca, err := res.Resolve(KeyHubCAFile)
	if err != nil {
		return c, err
	}
	c.CAFile = strings.TrimSpace(ca.Value)
	ins, err := res.Resolve(KeyHubInsecure)
	if err != nil {
		return c, err
	}
	if c.Insecure, err = strconv.ParseBool(ins.Value); err != nil {
		return c, fmt.Errorf("setting %q (%s): invalid value %q, expected true or false", KeyHubInsecure, ins.Source, ins.Value)
	}
	pin, err := res.Resolve(KeyHubTLSPin)
	if err != nil {
		return c, err
	}
	// Only trimmed here: the kernel package config does not import identity;
	// client.NewHub refuses a malformed pin before anything is sent.
	c.TLSPin = strings.TrimSpace(pin.Value)
	if c.TLSPin == "" || pin.Source == SourceFile {
		// A pin saved for this Hub's origin applies; one given by flag or
		// environment comes first.
		pins, err := resolveHubTLSPins(res)
		if err != nil {
			return c, err
		}
		if c.URL != "" && len(pins) > 0 {
			origin, err := PinOrigin(c.URL)
			if err != nil {
				return c, fmt.Errorf("setting %q (%s): %w", KeyHubURL, hubURL.Source, err)
			}
			if p, ok := pins[origin]; ok {
				c.TLSPin = p
			}
		}
	}
	w, err := res.Resolve(KeyHubWorker)
	if err != nil {
		return c, err
	}
	// Only trimmed here: the client (client.NewHub) refuses a value that is not
	// a Worker id before anything is sent, and says so without repeating it.
	c.Worker = strings.TrimSpace(w.Value)
	return c, nil
}

func resolveHubTLSPins(res *Resolver) (map[string]string, error) {
	saved, err := res.Resolve(KeyHubTLSPins)
	if err != nil {
		return nil, err
	}
	pins, err := ParseHubTLSPins(saved.Value)
	if err != nil {
		return nil, fmt.Errorf("setting %q (%s): %w", KeyHubTLSPins, saved.Source, err)
	}
	if saved.Source != SourceEnv || res.File == nil {
		return pins, nil
	}
	raw, _, err := res.File.Lookup(KeyHubTLSPins)
	if err != nil {
		return nil, fmt.Errorf("setting %q (%s): %w", KeyHubTLSPins, SourceFile, err)
	}
	filePins, err := ParseHubTLSPins(raw)
	if err != nil {
		return nil, fmt.Errorf("setting %q (%s): %w", KeyHubTLSPins, SourceFile, err)
	}
	maps.Copy(filePins, pins)
	return filePins, nil
}

// PinOrigin is the origin a saved TLS pin belongs to: scheme://host:port
// with a lower-case ASCII host and numeric port (443 for https, 80 for http).
// Every path of one origin reaches the same TLS endpoint.
func PinOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("not a Hub URL with a host")
	}
	return pinOrigin(u)
}

func pinOrigin(u *url.URL) (string, error) {
	host := u.Hostname()
	if host == "" || u.User != nil {
		return "", errors.New("not a Hub URL with a host and no credentials")
	}
	scheme := strings.ToLower(u.Scheme)
	var port uint64
	switch scheme {
	case "https":
		port = 443
	case "http":
		port = 80
	default:
		return "", errors.New("the Hub URL must start with https:// or http://")
	}
	if explicit := u.Port(); explicit != "" {
		n, err := strconv.ParseUint(explicit, 10, 16)
		if err != nil {
			return "", errors.New("the Hub URL has an invalid port")
		}
		port = n
	}
	if _, err := netip.ParseAddr(host); err != nil {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil || host == "" {
			return "", errors.New("the Hub URL has an invalid host")
		}
	}
	return (&url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(strings.ToLower(host), strconv.FormatUint(port, 10)),
	}).String(), nil
}

// ParseHubTLSPins reads the value of hub_tls_pins: entries
// "<origin>=<pins>" separated by white space. A malformed entry is an error,
// never skipped: a pin that silently disappears would fall back to the
// certificate check.
func ParseHubTLSPins(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range strings.Fields(v) {
		origin, pins, ok := strings.Cut(e, "=")
		if !ok || pins == "" {
			return nil, errors.New("an entry is not <scheme>://<host>:<port>=<pin>")
		}
		u, err := url.Parse(origin)
		if err != nil || u.Path != "" || u.ForceQuery || u.RawQuery != "" || strings.Contains(origin, "#") {
			return nil, errors.New("an entry does not start with an origin <scheme>://<host>:<port>")
		}
		norm, err := pinOrigin(u)
		if err != nil {
			return nil, err
		}
		if _, dup := out[norm]; dup {
			return nil, errors.New("two entries name the origin " + norm)
		}
		out[norm] = pins
	}
	return out, nil
}

// FormatHubTLSPins is the value of hub_tls_pins for pins (origin to pins),
// sorted by origin.
func FormatHubTLSPins(pins map[string]string) string {
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+pins[k])
	}
	return strings.Join(parts, " ")
}
