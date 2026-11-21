package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/spf13/cobra"
)

func usageSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open local usage credential file")
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("use a private regular file for a usage credential")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return nil, errors.New("unexpected usage credential file size")
	}
	return bytes.TrimRight(b, "\r\n"), nil
}

func newUsageRecordCmd(deps workerConfigDeps) *cobra.Command {
	var opts agent.NativeUsageOptions
	var accountID, profile, apiKeyFile, secretFile, endpoint, observedAt string
	cmd := &cobra.Command{Use: "record <format>", Short: "Record a native metadata observation supplied on standard input", Long: "Record numeric usage and lifecycle metadata from a configured native status producer. Supported formats are claude-statusline, claude-hook, codex-token-usage, gemini-result, gemini-quota and deepseek-response.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ce, err := deps.env(cmd)
		if err != nil {
			return err
		}
		defer ce.release()
		st, err := store.OpenContext(cmd.Context(), ce.stateDir)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		enrollment, err := st.LoadEnrollment(cmd.Context())
		if err != nil {
			return errors.New("usage recording requires an enrolled Worker")
		}
		allowed, err := st.UsageRegistration(cmd.Context(), enrollment.WorkerID)
		if err != nil || !allowed {
			return errors.New("review and accept monitoring with minion worker config usage before recording")
		}
		if opts.Session == "" {
			pane := os.Getenv("TMUX_PANE")
			if regexp.MustCompile(`^%[0-9]{1,10}$`).MatchString(pane) {
				sub, cancel := context.WithTimeout(cmd.Context(), time.Second)
				got, e := exec.CommandContext(sub, "tmux", "display-message", "-p", "-t", pane, "#{session_name}").Output()
				cancel()
				if e == nil && len(got) < 258 {
					opts.Session = strings.TrimSuffix(string(got), "\n")
				}
			}
		}
		now := time.Now()
		opts.Format = args[0]
		opts.Provider = ops.UsageProvider(opts.Provider)
		if observedAt != "" {
			t, e := time.Parse(time.RFC3339Nano, observedAt)
			if e != nil {
				return errors.New("observed-at must be an RFC3339 timestamp")
			}
			opts.ObservedAt = t
		}
		if profile != "" {
			if accountID != "" || len(profile) > 80 || strings.TrimSpace(opts.Provider) == "" {
				return errors.New("use one named subscription profile and its provider")
			}
			accountID = ops.UsageID("operator-subscription-profile", hex.EncodeToString(enrollment.HubPub), opts.Provider, profile)
		}
		if apiKeyFile != "" || secretFile != "" || endpoint != "" {
			if accountID != "" || endpoint == "" {
				return errors.New("API attribution requires the actual endpoint and no subscription account")
			}
			var key []byte
			if apiKeyFile != "" {
				key, err = usageSecretFile(apiKeyFile)
				if err != nil {
					return err
				}
			} else {
				// This is the explicitly selected producer's own environment.
				// Never inspect another process or the harness credential store.
				name := map[string]string{"Anthropic": "ANTHROPIC_API_KEY", "OpenAI": "OPENAI_API_KEY", "DeepSeek": "DEEPSEEK_API_KEY", "OpenRouter": "OPENROUTER_API_KEY"}[opts.Provider]
				if name == "" {
					return errors.New("use an explicit credential file for this provider")
				}
				key = []byte(os.Getenv(name))
			}
			var a ops.UsageAccount
			if secretFile != "" {
				secret, err := usageSecretFile(secretFile)
				if err != nil {
					return err
				}
				a, err = agent.APIUsageAccount(opts.Provider, endpoint, string(key), secret, now.UnixMilli())
				if err != nil {
					return err
				}
			} else {
				a, err = agent.NativeAPIUsageAccount(opts.Provider, endpoint, string(key), hex.EncodeToString(enrollment.HubPub), now.UnixMilli())
				if err != nil {
					return err
				}
			}
			opts.Account = &a
		} else if accountID != "" {
			if strings.TrimSpace(opts.Provider) == "" {
				return errors.New("name the provider of this subscription")
			}
			label := opts.Provider + " subscription"
			if profile != "" {
				label = profile
			}
			opts.Account = &ops.UsageAccount{ID: accountID, Provider: opts.Provider, Label: label, AuthMode: "subscription", Source: "local-attribution", ObservedAt: now.UnixMilli()}
		}
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), (256<<10)+1))
		if err != nil {
			return errors.New("cannot read native usage document")
		}
		r, err := agent.ParseNativeUsage(raw, opts, now)
		if err != nil {
			return err
		}
		return st.SaveNativeUsage(cmd.Context(), enrollment, r, now)
	}}
	addNamedSettingFlags(cmd, deps.reg(), config.KeyStateDir)
	f := cmd.Flags()
	f.StringVar(&opts.Session, "session", "", "Worker terminal session name")
	f.StringVar(&opts.NativeID, "native-session", "", "Native session identifier for formats without one")
	f.StringVar(&opts.Model, "model", "", "Model reported by the producer")
	f.StringVar(&opts.Harness, "harness", "", "Harness reporting the metric, such as opencode")
	f.StringVar(&opts.Provider, "provider", "", "Provider reported by the producer")
	f.StringVar(&observedAt, "observed-at", "", "Original RFC3339 observation time supplied by the native producer")
	f.StringVar(&opts.EventID, "event-id", "", "Producer's stable event identifier, for idempotent replay")
	f.StringVar(&profile, "subscription-profile", "", "Explicit subscription profile label, shared across the Workers using that account")
	f.StringVar(&accountID, "account", "", "Subscription account fingerprint from Usage")
	f.StringVar(&endpoint, "endpoint", "", "API endpoint used by this producer")
	f.StringVar(&apiKeyFile, "api-key-file", "", "Private file containing the credential actually used")
	f.StringVar(&secretFile, "fingerprint-secret-file", "", "Private file containing a shared random domain fingerprint secret of at least 32 bytes")
	return cmd
}
