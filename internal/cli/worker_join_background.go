package cli

// "minion worker join <url> --background" (tray tickets v3, W8a): pairing by
// code that the running Worker does, for the tray.
//
// The command saves the Hub address (and the expected Hub fingerprint) with
// pairing_display=status in the Worker's config file, records in
// <state>/join-pin.json whether it created the hub_fingerprint expectation
// (C11), and restarts the running Worker. The Worker then pairs by itself and
// shows its code only in its status ("minion worker status --json",
// pairing.code), never on its console or in its log: the tray shows it in a
// menu item. The command itself never prints the code.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// backgroundSavedText says what a background join did before its activation.
const backgroundSavedText = "the Hub address is saved for pairing in the background"

// backgroundJoinResult is the JSON result of a background join.
type backgroundJoinResult struct {
	joinResult
	// PairingDisplay is the pairing_display the Worker uses (status).
	PairingDisplay string `json:"pairing_display,omitempty"`
	// HubFingerprintCreated says whether this join created the
	// hub_fingerprint expectation (C11); absent when nothing was saved.
	HubFingerprintCreated *bool `json:"hub_fingerprint_created,omitempty"`
}

// runWorkerJoinBackground runs "worker join <url> --background" and ends it
// with the result contract (one JSON object with --json).
func runWorkerJoinBackground(cmd *cobra.Command, deps workerPairDeps, opt pairOptions, args []string) error {
	stdout, stderr := deps.stdout, deps.stderr
	if stdout == nil {
		stdout = cmd.OutOrStdout()
	}
	if stderr == nil {
		stderr = cmd.ErrOrStderr()
	}
	res, err := backgroundJoin(cmd, deps, opt, args)
	if err == nil {
		human := stdout
		if opt.asJSON {
			human = stderr
		}
		_, _ = fmt.Fprintf(human, "Saved the Hub address %s and restarted the Worker. It pairs in the background: its pairing code is shown "+
			"in the tray and by minion worker status, not on its console or in its log.\n", res.Hub)
	}
	return emitResult(stdout, stderr, opt.asJSON, res, err)
}

func backgroundJoin(cmd *cobra.Command, deps workerPairDeps, opt pairOptions, args []string) (backgroundJoinResult, error) {
	var res backgroundJoinResult
	switch {
	case len(args) == 1 && identity.LooksLikeJoinToken(args[0]):
		// Never echo it: the token holds an invitation key.
		return res, usagef("a join token is never an argument: " + joinHint)
	case len(args) != 1:
		return res, usagef("give the Hub address: minion worker join <url> --background")
	case opt.tokenFile != "" || opt.resume:
		return res, usagef("--background pairs with a code; it does not go with --token-file or --resume")
	case opt.newCode:
		return res, usagef("--background does not go with --new-code")
	case opt.acceptLoss < 0:
		return res, usagef("--accept-loss takes a number of sessions, 0 or more")
	}
	url, err := config.ValidateServerURL(args[0])
	if err != nil {
		return res, &usageError{err}
	}
	if url == "" {
		return res, usagef("give the Hub address, for example https://hub.example:8443")
	}
	res.Hub = safeURL(url)
	flags := overrideFlags{base: changedFlags{cmd: cmd, reg: config.Default}, key: config.KeyServer, value: url}
	r := &config.Resolver{Role: config.ScopeWorker, Flags: flags, LookupEnv: deps.lookupEnv}
	if deps.loadConfig != nil {
		file, err := loadConfigFile(r, deps.loadConfig)
		if err != nil {
			return res, err
		}
		r.File = file
	}
	core, err := config.ResolveWorkerCore(r)
	if err != nil {
		return res, err
	}
	res.StateDir = core.StateDir
	// A paused Worker talks to no Hub, and pairing is Hub traffic (tray W3c).
	if p, err := agent.ReadPause(core.StateDir); err != nil {
		return res, err
	} else if p.Active(time.Now()) {
		return res, newResultError(ExitPaused, "this Worker is paused, so it talks to no Hub; resume it first (minion worker resume), then run this again. Nothing was changed.")
	}
	ctx := cmdContext(cmd)
	paired, isPaired, err := readPairing(ctx, core.StateDir)
	if err != nil {
		return res, err
	}
	if isPaired {
		pe := agent.AlreadyPaired(paired)
		return res, &resultError{Exit: ExitError, Code: string(proto.CodeAlreadyPaired), Message: pe.Error() + "; nothing was changed", Err: pe}
	}

	// The expected Hub fingerprint: a flag or the environment, else what the
	// config file holds. A malformed one is an error, never "absent".
	fpSetting, err := r.Resolve(config.KeyHubFingerprint)
	if err != nil {
		return res, err
	}
	expect, err := agent.ParseHubExpectation(fpSetting.Value)
	if err != nil {
		return res, err
	}
	fileFp, _, err := fileValue(r.File, config.KeyHubFingerprint)
	if err != nil {
		return res, err
	}
	saveFp := ""
	if fpSetting.Source != config.SourceFile && !expect.IsZero() {
		if fileFp != "" {
			// A fingerprint provisioned in the file is a pin a join must not
			// replace.
			m, err := agent.ParseHubExpectation(fileFp)
			if err != nil {
				return res, fmt.Errorf("the config file: %w", err)
			}
			if m.String() != expect.String() {
				return res, newResultError(ExitError, fmt.Sprintf("the Hub fingerprint given (%s) differs from hub_fingerprint in the config file (%s); "+
					"nothing was changed. To pair with another Hub, run minion worker config unset hub_fingerprint first", expect.Words(), m.Words()))
			}
		}
		saveFp = expect.String()
	}

	// The pairing code must appear only in the status. A flag or an
	// environment variable that asks for the console would win again at the
	// restart: refuse instead of printing the code to a log.
	if pd, err := r.Resolve(config.KeyPairingDisplay); err != nil {
		return res, err
	} else if pd.Source != config.SourceFile && pd.Source != config.SourceDefault && pd.Value == config.PairingDisplayConsole {
		return res, newResultError(ExitError, "pairing_display=console is set by a flag or an environment variable ("+string(pd.Source)+
			"); a background pairing shows its code only in the status. Remove it, then run this again. Nothing was changed")
	}

	// The first pairing of a Worker that has no sharing choice needs one, as
	// the join with a token does; it is saved before anything else.
	share, err := r.Resolve(config.KeyShareExisting)
	if err != nil {
		return res, &usageError{err}
	}
	if share.Source == config.SourceFlag {
		if err := commitJoinShare(ctx, deps, core, share.Value); err != nil {
			return res, err
		}
	} else if needed, err := (workerSetupDeps{}).setupNeeded(ctx, r, core.StateDir, true); err != nil {
		return res, err
	} else if needed {
		return res, usagef("this Worker has no sharing choice yet: give --share-existing all|none|later (what the Hub may see of the tmux sessions). Nothing was changed")
	}

	pin, err := saveBackgroundJoin(core.StateDir, url, saveFp, time.Now())
	if err != nil {
		res.Config = joinFailed
		return res, fmt.Errorf("save the Hub address for pairing in the background: %w", err)
	}
	res.Config = joinSaved
	res.PairingDisplay = config.PairingDisplayStatus
	created := pin.HubFingerprintCreated
	res.HubFingerprintCreated = &created
	return res, activateJoin(cmd, deps, opt, core.StateDir, &res.joinResult, backgroundSavedText)
}

// saveBackgroundJoin saves server, hub_fingerprint (when fingerprint is not
// empty) and pairing_display=status in the Worker config file, and the C11
// provenance record, under the config lock that `minion worker config` holds.
func saveBackgroundJoin(stateDir, url, fingerprint string, now time.Time) (agent.JoinPin, error) {
	return saveJoinHub(stateDir, url, fingerprint, agent.JoinPinSourceBackground, now)
}

// saveJoinHub saves server and hub_fingerprint (when fingerprint is not
// empty) in the Worker config file together with the C11 provenance record
// of the join named by source, under the config lock that `minion worker
// config` holds. A background join also saves pairing_display=status and
// lifts an earlier cancel (W8b).
func saveJoinHub(stateDir, url, fingerprint, source string, now time.Time) (agent.JoinPin, error) {
	background := source == agent.JoinPinSourceBackground
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return agent.JoinPin{}, fmt.Errorf("cannot create the state directory %q: %w", stateDir, err)
	}
	release, err := lockConfigFile(filepath.Join(stateDir, configLockName))
	if err != nil {
		return agent.JoinPin{}, err
	}
	defer release()
	f, err := config.LoadFile(filepath.Join(stateDir, workerConfigName))
	if err != nil {
		return agent.JoinPin{}, err
	}
	fileServer, _, err := f.Lookup(config.KeyServer)
	if err != nil {
		return agent.JoinPin{}, err
	}
	fileFp, _, err := f.Lookup(config.KeyHubFingerprint)
	if err != nil {
		return agent.JoinPin{}, err
	}
	// A pin provisioned while this command ran (the caller checked an older
	// snapshot) is never replaced by another one.
	if cur := strings.TrimSpace(fileFp); fingerprint != "" && cur != "" && !sameFingerprint(cur, fingerprint) {
		return agent.JoinPin{}, fmt.Errorf("hub_fingerprint in the config file changed while this command ran (it is %s now); nothing was changed", strings.TrimSpace(fileFp))
	}
	// An unreadable record proves nothing about who set the values (ok is
	// false then): it is replaced by what this join knows.
	prev, hadPrev, _ := agent.ReadJoinPin(stateDir)
	pin := agent.NextJoinPin(prev, hadPrev, strings.TrimSpace(fileServer), strings.TrimSpace(fileFp), url, fingerprint, now)
	pin.Source = source
	f.Set(config.KeyServer, url)
	if fingerprint != "" {
		f.Set(config.KeyHubFingerprint, fingerprint)
	}
	if background {
		f.Set(config.KeyPairingDisplay, config.PairingDisplayStatus)
	}
	// The provenance record is written before the config file: a failure
	// between the two leaves a record that claims values the file does not
	// hold yet (a resume saves them), never values without a record (which a
	// later leave would take for the user's own).
	if err := agent.WriteJoinPin(stateDir, pin); err != nil {
		return agent.JoinPin{}, err
	}
	if err := f.Save(); err != nil {
		return agent.JoinPin{}, err
	}
	// A new join asks for the pairing that an earlier cancel stopped (W8b).
	if background {
		if err := agent.RemovePairCancel(stateDir); err != nil {
			return agent.JoinPin{}, err
		}
	}
	return pin, nil
}

// sameFingerprint compares two hub_fingerprint values in any accepted
// notation.
func sameFingerprint(a, b string) bool {
	ma, ea := agent.ParseHubExpectation(a)
	mb, eb := agent.ParseHubExpectation(b)
	if ea != nil || eb != nil {
		return a == b
	}
	return ma.String() == mb.String()
}
