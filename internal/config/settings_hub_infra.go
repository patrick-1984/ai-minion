package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Names of the Hub infrastructure enrollment settings (AD-7, R97).
const (
	KeyInfraDefaultOwner = "infra_default_owner"

	// KeyInfraInviteKeyDays is the lifetime in days of the infrastructure
	// invitation key file (MINION_INFRA_INVITE_KEY_FILE), counted from its
	// first load (F-02). The expiry is stored with the key and a reload never
	// changes it; 0 means no expiry (the Hub then warns at every start).
	KeyInfraInviteKeyDays = "infra_invite_key_days"
	// DefaultInfraInviteKeyDays is the default of KeyInfraInviteKeyDays.
	DefaultInfraInviteKeyDays = 30
	// MaxInfraInviteKeyDays bounds KeyInfraInviteKeyDays (ten years).
	MaxInfraInviteKeyDays = 3650

	// KeyInfraInviteKeyFile names the setting whose environment variable is
	// MINION_INFRA_INVITE_KEY_FILE. Like the TLS file paths it is not
	// declared in the registry: the registry reserves the "_FILE" suffix for
	// the path variable of a secret, and this setting holds a file path
	// itself. [ResolveHubInfra] reads it directly (environment, then the
	// panel key); it has no panel field.
	KeyInfraInviteKeyFile = "infra_invite_key_file"

	EnvInfraInviteKeyFile = "MINION_INFRA_INVITE_KEY_FILE"
)

func init() { declareHubInfra(Default) }

// declareHubInfra declares the settings of Hub-wide infrastructure
// enrollment.
func declareHubInfra(r *Registry) {
	r.Declare(Setting{
		Key:   KeyInfraDefaultOwner,
		Scope: ScopeHub,
		Help:  "User name (or e-mail address) that owns the Workers enrolled with the Hub-wide infrastructure invitation key; the seeded admin when empty",
	})
	r.Declare(Setting{
		Key: KeyInfraInviteKeyDays, Scope: ScopeHub, Default: strconv.Itoa(DefaultInfraInviteKeyDays),
		Help: "Days the infrastructure invitation key file stays valid from its first load (0 = no expiry, not recommended)",
	})
}

// HubInfra holds the resolved infrastructure enrollment settings.
type HubInfra struct {
	// InviteKeyFile is the path of the file with the Hub-wide infrastructure
	// invitation key (MINION_INFRA_INVITE_KEY_FILE); empty when none is
	// configured. The file holds the key, a secret: callers read it with
	// ReadSecretFile and never log it.
	InviteKeyFile string
	// DefaultOwner is infra_default_owner; empty means the seeded admin.
	DefaultOwner string
	// InviteKeyLifetime is the lifetime of the key file from its first load:
	// infra_invite_key_days days. Zero means no expiry.
	InviteKeyLifetime time.Duration
}

// ParseInfraInviteKeyDays parses infra_invite_key_days: a whole number of days
// from 0 to MaxInfraInviteKeyDays.
func ParseInfraInviteKeyDays(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 || n > MaxInfraInviteKeyDays {
		return 0, fmt.Errorf("config: %s must be a whole number of days from 0 to %d, got %q", KeyInfraInviteKeyDays, MaxInfraInviteKeyDays, v)
	}
	return n, nil
}

// ResolveHubInfra resolves the infrastructure enrollment settings through r (a
// Hub-role resolver). The key file path comes from
// MINION_INFRA_INVITE_KEY_FILE, read through r.LookupEnv, and otherwise from
// the panel key infra_invite_key_file when r has a Panel (environment before
// panel, as for every Hub setting).
func ResolveHubInfra(r *Resolver) (HubInfra, error) {
	if r.Role != ScopeHub {
		return HubInfra{}, fmt.Errorf("config: ResolveHubInfra needs a %q resolver, got %q", ScopeHub, r.Role)
	}
	owner, err := r.Resolve(KeyInfraDefaultOwner)
	if err != nil {
		return HubInfra{}, err
	}
	file, err := r.pathSetting(EnvInfraInviteKeyFile, KeyInfraInviteKeyFile)
	if err != nil {
		return HubInfra{}, err
	}
	days, err := r.Resolve(KeyInfraInviteKeyDays)
	if err != nil {
		return HubInfra{}, err
	}
	n, err := ParseInfraInviteKeyDays(days.Value)
	if err != nil {
		return HubInfra{}, fmt.Errorf("%w (from %s)", err, days.Source)
	}
	return HubInfra{
		InviteKeyFile: file, DefaultOwner: strings.TrimSpace(owner.Value),
		InviteKeyLifetime: time.Duration(n) * 24 * time.Hour,
	}, nil
}
