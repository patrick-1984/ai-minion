package config

import (
	"fmt"
	"strings"
)

// Names of the Hub infrastructure enrollment settings (AD-7, R97).
const (
	KeyInfraDefaultOwner = "infra_default_owner"

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
	return HubInfra{InviteKeyFile: file, DefaultOwner: strings.TrimSpace(owner.Value)}, nil
}
