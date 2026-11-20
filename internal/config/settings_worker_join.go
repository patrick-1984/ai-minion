package config

// KeyJoin is the key of the Worker's join token setting (v0.12.0): one string
// from the Hub's "Add worker" page that holds the Hub URL, the full Hub
// fingerprint and a one-time invitation key. The CLI expands it into server,
// hub_fingerprint and invite_key before anything else resolves them; the
// token is parsed by the CLI, because the kernel package config does not
// import identity.
const KeyJoin = "join"

func init() { declareWorkerJoin(Default) }

// declareWorkerJoin declares join: flag --join, environment MINION_JOIN or
// MINION_JOIN_FILE.
//
// It is a secret (it carries an invitation key) with two declared exceptions,
// for the same reason as the blueprints it follows (the Docker Swarm join
// token, the kubeadm bootstrap token, the edge agent token of Arcane, the
// one-off setup keys of Tailscale and NetBird): the secret works once and
// expires within hours, and it is useless once the Worker has enrolled.
//
//   - ArgvAllowedWithWarning preserves the original --join <token> form.
//   - PlainEnvAllowed preserves older container setups using MINION_JOIN.
//
// The Hub generates commands with a file descriptor, temporary file or stdin
// so enrollment credentials stay out of process arguments and container settings.
//
// The config file is never a source of the token: the CLI resolves it from
// the flag and the environment only, and `minion worker config` refuses it.
func declareWorkerJoin(r *Registry) {
	r.Declare(Setting{
		Key:    KeyJoin,
		Scope:  ScopeWorker,
		Secret: true,
		Help: "Join token (mnj1-...) from the Hub's Add worker page: the Hub URL, the full Hub fingerprint and a " +
			"one-time invitation key in one string. It sets --server, --hub-fingerprint and --invite-key; giving " +
			"one of them with a different value is an error. Also MINION_JOIN_FILE",
		PlainEnvAllowed:        true,
		ArgvAllowedWithWarning: true,
	})
}
