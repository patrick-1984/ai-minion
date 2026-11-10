package agentdetect

import "strings"

// This file decides which program a process runs from its argv. The rule that
// keeps AD-12 safe: the arguments of a shell, interpreter or runner identify
// only the program it executes (the script operand, "-m <module>", the
// package of npx/uvx/pipx run/npm exec, the program of sudo/env). An operand
// of another subcommand or an argument of the script ("npm install -g
// @anthropic-ai/claude-code", "python -m pip install aider-chat", "node
// server.js claude") never identifies an agent. A child process that really
// is the agent is found through the process tree instead.

// matchArgs returns the agent that a process with this argv runs: the program
// itself, or the program or package it is given when the program is a shell
// or an interpreter ("bash -c claude", "node .../gemini", "python -m aider",
// "npx @google/gemini-cli", "sudo claude").
func (d *Detector) matchArgs(args []string) *Agent {
	if len(args) == 0 {
		return nil
	}
	prog := progName(args[0])
	if a := d.matchProgram(prog); a != nil {
		return a
	}
	rest := args[1:]
	switch {
	case shells[prog]:
		return d.matchShell(rest)
	case strings.HasPrefix(prog, "python"):
		return d.matchPython(rest)
	}
	switch prog {
	case "node", "nodejs":
		if i := operandIndex(rest, nodeFlags); i >= 0 {
			return d.matchToken(rest[i])
		}
	case "bun", "deno":
		return d.matchBunDeno(prog, rest)
	case "npx", "bunx":
		return d.matchRunPackage(rest, npxBoolFlags, npxValueFlags)
	case "uvx":
		return d.matchRunPackage(rest, uvxBoolFlags, uvxValueFlags)
	case "uv":
		return d.matchUV(rest)
	case "pipx":
		if i := firstOperand(rest, nil); i >= 0 && rest[i] == "run" {
			return d.matchRunPackage(rest[i+1:], pipxBoolFlags, pipxValueFlags)
		}
	case "npm", "pnpm", "yarn":
		if i := firstOperand(rest, nil); i >= 0 {
			switch rest[i] {
			case "exec", "x", "dlx":
				return d.matchRunPackage(rest[i+1:], npxBoolFlags, npxValueFlags)
			}
		}
	case "env":
		return d.matchWrapped(rest, envValueFlags)
	case "sudo":
		return d.matchWrapped(rest, sudoValueFlags)
	case "script":
		return d.matchScriptCmd(rest)
	}
	return nil
}

var (
	// Runner flags (npx, uvx, pipx, ...). A runner's argv is evidence only
	// when every flag before the operand is known: an unknown flag may take a
	// value that would be mistaken for the executable operand ("npx -w claude
	// bash", "uvx --with-editable /src/aider python"), so it makes the argv no
	// evidence at all and the live child process decides. The value flags
	// include the package selectors (-p/--package, --from, --spec): their
	// values are skipped, never matched.
	npxBoolFlags     = flagSet("-y", "--yes", "--no", "--no-install", "--ignore-existing", "-q", "--quiet", "--verbose", "--bun")
	npxValueFlags    = flagSet("-p", "--package", "--node-options", "--registry", "--cache", "--prefix", "--userconfig")
	uvxBoolFlags     = flagSet("--isolated", "--no-cache", "-n", "-q", "--quiet", "-v", "--verbose", "--offline", "--no-progress", "--native-tls", "--no-config", "--refresh", "--managed-python", "--no-managed-python", "--no-python-downloads", "--no-build", "--no-sources")
	uvxValueFlags    = flagSet("--from", "--with", "-w", "--python", "-p", "--with-requirements", "--index", "--index-url", "-i", "--default-index", "--extra-index-url", "--directory", "--project", "--env-file")
	pipxBoolFlags    = flagSet("--no-cache", "--verbose", "--quiet", "-v", "-q", "--pypackages")
	pipxValueFlags   = flagSet("--spec", "--python", "--pip-args", "--index-url")
	envValueFlags    = flagSet("-u", "--unset", "-C", "--chdir", "-S", "--split-string")
	sudoValueFlags   = flagSet("-u", "-g", "-h", "-p", "-C", "-D", "-r", "-t", "-T", "-U", "-R", "--user", "--group", "--host", "--prompt", "--chdir")
	pythonValueFlags = flagSet("-W", "-X", "--check-hash-based-pycs")
	shellValueFlags  = flagSet("-o", "+o", "--rcfile", "--init-file")
	scriptValueFlags = flagSet("-E", "--echo", "-I", "--log-in", "-O", "--log-out", "-B", "--log-io", "-T", "--log-timing", "-m", "--logging-format", "-t")
)

// flagSpec is the allow-list of a command that fails closed on unknown flags:
// bools never take the next token, values always do (unless written
// "--flag=value"). optional flags take a value only when it is attached with
// "=" (clap require_equals): bare they are bools, so they never swallow the
// next token. A flag in none of the lists, written bare or with "=", makes the
// argv before it no evidence: it may be "--eval=code" or take a value that
// would be mistaken for the operand.
//
// deno's --lock is deliberately in none of the lists: it is num_args(0..=1),
// so a bare "--lock" binds the next token unless that starts with "-" ("deno
// run --lock claude main.ts" locks to a file named claude). It is neither a
// bool nor an attached-only flag, and which reading holds depends on the next
// token, so every deno argv with --lock (bare or "--lock=x") fails closed and
// classifies none: a safe false negative, never a false positive.
type flagSpec struct {
	bools, values, optional map[string]bool
	boolPrefixes            []string
}

// isBoolPrefix reports whether name starts with one of the bool prefixes
// (deno permission flags); such a flag may carry an attached "=value".
func (s flagSpec) isBoolPrefix(name string) bool {
	for _, p := range s.boolPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// operandIndex returns the index of the first operand of args (the token
// after "--" included) or -1 when there is none or when a flag before it is
// not in spec (fail closed).
func operandIndex(args []string, spec flagSpec) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		}
		if len(a) > 1 && a[0] == '-' {
			name, _, hasValue := strings.Cut(a, "=")
			switch {
			case spec.values[name]:
				if !hasValue {
					i++
				}
			case spec.bools[name] && !hasValue:
			case spec.optional[name], spec.isBoolPrefix(name):
			default:
				return -1
			}
			continue
		}
		return i
	}
	return -1
}

var (
	nodeFlags = flagSpec{
		bools:    flagSet("--no-warnings", "--no-deprecation", "--trace-warnings", "--trace-deprecation", "--trace-uncaught", "--throw-deprecation", "--pending-deprecation", "--enable-source-maps", "--preserve-symlinks", "--preserve-symlinks-main", "--abort-on-uncaught-exception", "--expose-gc", "--expose_gc", "--harmony", "--use-strict", "--experimental-vm-modules", "--experimental-modules", "--experimental-strip-types", "--experimental-transform-types", "--experimental-require-module", "--experimental-wasm-modules", "--experimental-json-modules", "--experimental-top-level-await", "--experimental-fetch", "--no-experimental-fetch", "--no-experimental-global-navigator", "--frozen-intrinsics", "--jitless", "--prof", "--allow-natives-syntax", "--no-addons", "--no-network-family-autoselection", "--disallow-code-generation-from-strings", "--force-fips", "--enable-fips", "--openssl-legacy-provider", "--track-heap-objects", "--zero-fill-buffers", "--verify-base-objects"),
		optional: flagSet("--inspect", "--inspect-brk", "--inspect-wait", "--watch"),
		values:   flagSet("-r", "--require", "--import", "--loader", "--experimental-loader", "-C", "--conditions", "--env-file", "--title", "--redirect-warnings", "--input-type", "--max-old-space-size", "--max-semi-space-size", "--stack-size", "--inspect-port", "--disable-warning", "--report-dir", "--icu-data-dir", "--openssl-config", "--tls-cipher-list", "--unhandled-rejections", "--dns-result-order"),
	}
	bunFlags = flagSpec{
		bools:    flagSet("--bun", "-b", "--hot", "--silent", "--smol", "--no-install", "--prefer-offline", "--prefer-latest", "--if-present", "--no-clear-screen", "--no-addons", "--expose-gc", "--zero-fill-buffers", "--no-deprecation", "--throw-deprecation", "--no-summary", "--no-env-file", "--verbose"),
		optional: flagSet("--config", "-c", "--watch", "--inspect", "--inspect-brk", "--inspect-wait"),
		values:   flagSet("--cwd", "--env-file", "--preload", "-r", "--port", "--tsconfig-override", "--define", "-d", "--elide-lines", "--filter", "-F", "--shell", "--conditions", "--install", "--extension-order", "--main-fields", "--title", "--redirect-warnings", "--user-agent"),
	}
	denoFlags = flagSpec{
		bools:        flagSet("-A", "--allow-all", "--no-prompt", "--no-remote", "--no-npm", "--no-lock", "-q", "--quiet", "--cached-only", "--unstable"),
		optional:     flagSet("--no-check", "--watch", "--check", "--inspect", "--inspect-brk", "--inspect-wait", "--reload", "-r", "--env-file", "--node-modules-dir", "--v8-flags"),
		values:       flagSet("--config", "-c", "--import-map", "--cert", "--location", "--seed", "--tsconfig", "--v8-flag"),
		boolPrefixes: []string{"--allow-", "--deny-", "--unstable-"},
	}
	uvFlags = flagSpec{
		bools:  flagSet("--isolated", "--no-sync", "--frozen", "--locked", "--no-dev", "--all-extras", "--no-project", "--no-cache", "-n", "-q", "--quiet", "-v", "--verbose", "--offline", "--no-progress", "--native-tls", "--no-config", "--refresh", "--managed-python", "--no-managed-python", "--no-python-downloads", "--no-build", "--no-sources", "--active", "--exact", "--no-editable", "--all-packages", "--all-groups", "--no-default-groups", "--dev", "--only-dev", "--no-env-file", "--reinstall", "--no-binary", "--no-build-isolation", "--module", "-m", "--script", "-s", "--upgrade", "-U", "--no-install-project", "--no-install-workspace", "--no-install-local"),
		values: flagSet("--package", "--with-editable", "--extra", "--no-extra", "--no-group", "--group", "--with", "-w", "--python", "-p", "--project", "--directory", "--env-file", "--with-requirements", "--index", "--index-url", "-i", "--default-index", "--extra-index-url", "--only-group", "--exclude-newer", "--link-mode", "--config-file", "--cache-dir", "--color", "--keyring-provider", "--resolution", "--prerelease", "--refresh-package", "--reinstall-package", "--upgrade-package", "-P", "--no-binary-package", "--index-strategy", "--find-links", "-f", "--constraints", "-c", "--overrides", "--build-constraints", "--python-platform", "--no-install-package"),
	}
)

func flagSet(flags ...string) map[string]bool {
	m := make(map[string]bool, len(flags))
	for _, f := range flags {
		m[f] = true
	}
	return m
}

// firstOperand returns the index of the first operand of args: the first
// token that is not a flag and not the value of a flag in withValue, or the
// token after "--". It returns -1 when there is none.
func firstOperand(args []string, withValue map[string]bool) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return i + 1
			}
			return -1
		}
		if len(a) > 1 && (a[0] == '-' || a[0] == '+') {
			if withValue[a] {
				i++
			}
			continue
		}
		return i
	}
	return -1
}

// matchShell matches a shell's own program: the first word of its "-c"
// script, or its script file operand.
func (d *Detector) matchShell(args []string) *Agent {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case isCommandFlag(a):
			return d.matchScript(args[i+1:])
		case a == "--":
			if i+1 < len(args) {
				return d.matchToken(args[i+1])
			}
			return nil
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			if shellValueFlags[a] {
				i++
			}
		default:
			return d.matchToken(a)
		}
	}
	return nil
}

// isCommandFlag recognises "-c" and its combined forms such as "-lc" and "-ic".
func isCommandFlag(a string) bool {
	return len(a) >= 2 && a[0] == '-' && a[1] != '-' && strings.HasSuffix(a, "c") && strings.Trim(a[1:], "lic") == ""
}

// matchScript matches the first word of a "-c" script, skipping environment
// assignments and prefix commands. A script with more than one command
// ("claude; bash -i; true") matches nothing: the shell keeps that argv after
// the agent exits and would stay a permanent claim; the agent is then found
// as a live child process instead. A single command lives exactly as long as
// the shell that runs it.
func (d *Detector) matchScript(tokens []string) *Agent {
	joined := strings.Join(tokens, " ")
	if strings.ContainsAny(joined, ";&|\n`()") {
		return nil
	}
	words := strings.Fields(joined)
	for _, w := range words {
		w = strings.Trim(w, `"'`)
		if i := strings.IndexAny(w, ";&|"); i >= 0 {
			w = w[:i]
		}
		switch {
		case w == "":
			return nil
		case strings.Contains(w, "="):
			continue
		case w == "exec" || w == "command" || w == "nohup" || w == "env":
			continue
		}
		return d.matchToken(w)
	}
	return nil
}

// matchPython matches "python -m <module>" and "python <script>"; "-c" code
// and the arguments of a script match nothing.
func (d *Detector) matchPython(args []string) *Agent {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m":
			if i+1 < len(args) {
				return d.matchModule(args[i+1])
			}
			return nil
		case strings.HasPrefix(a, "-m") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			return d.matchModule(a[2:])
		case a == "--":
			if i+1 < len(args) {
				return d.matchToken(args[i+1])
			}
			return nil
		case len(a) > 1 && a[0] == '-':
			if pythonValueFlags[a] {
				i++
			}
			if strings.HasPrefix(a, "-c") && !strings.HasPrefix(a, "--") {
				return nil // inline code
			}
		default:
			return d.matchToken(a)
		}
	}
	return nil
}

// matchModule matches the top-level package of "-m <module>".
func (d *Detector) matchModule(m string) *Agent {
	if i := strings.IndexByte(m, '.'); i > 0 {
		m = m[:i]
	}
	return d.matchToken(m)
}

// matchBunDeno matches "bun <file>", "bun run <file>", "bun x <pkg>" and
// "deno run <file or npm: package>".
func (d *Detector) matchBunDeno(prog string, args []string) *Agent {
	spec := bunFlags
	if prog == "deno" {
		spec = denoFlags
	}
	i := operandIndex(args, spec)
	if i < 0 {
		return nil
	}
	switch args[i] {
	case "x":
		if prog == "bun" {
			return d.matchRunPackage(args[i+1:], npxBoolFlags, npxValueFlags)
		}
		return nil
	case "run":
		if j := operandIndex(args[i+1:], spec); j >= 0 {
			return d.matchToken(args[i+1+j])
		}
		return nil
	case "install", "add", "remove", "update", "upgrade", "test", "build", "init", "create", "pm", "link", "info", "eval", "repl", "compile", "cache", "fmt", "lint", "check":
		return nil
	}
	return d.matchToken(args[i])
}

// matchUV matches "uv run <program>" and "uv tool run <package>"; "uv pip
// install aider-chat" and the other subcommands match nothing.
func (d *Detector) matchUV(args []string) *Agent {
	i := operandIndex(args, uvFlags)
	if i < 0 {
		return nil
	}
	switch args[i] {
	case "run":
		if j := operandIndex(args[i+1:], uvFlags); j >= 0 {
			return d.matchArgs(args[i+1+j:])
		}
	case "tool":
		rest := args[i+1:]
		if j := operandIndex(rest, uvFlags); j >= 0 && rest[j] == "run" {
			return d.matchRunPackage(rest[j+1:], uvxBoolFlags, uvxValueFlags)
		}
	}
	return nil
}

// matchRunPackage matches the program a one-shot runner executes: the first
// operand (npx claude, uvx aider-chat, pipx run aider-chat), which is the
// executable and, without a package flag, also the package. The values of the
// package flags (npx -p, uvx --from, pipx --spec) only select the packages
// that are made available: they never prove that the executed program is the
// agent ("npx --package @anthropic-ai/claude-code bash" runs bash), so they
// are skipped, not matched. The runner fails closed: a flag before the
// operand that is in neither boolFlags nor valueFlags may take a value that
// would be mistaken for the operand, so the argv is no evidence at all (nil)
// and the live child process that the runner starts decides. Without an
// operand nothing is matched either. The arguments after the operand belong
// to the program and are ignored.
func (d *Detector) matchRunPackage(args []string, boolFlags, valueFlags map[string]bool) *Agent {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return d.matchPackage(args[i+1])
			}
			return nil
		}
		if len(a) > 1 && a[0] == '-' {
			name, _, hasValue := strings.Cut(a, "=")
			switch {
			case valueFlags[name]:
				if !hasValue {
					i++
				}
			case boolFlags[name] && !hasValue:
			default:
				return nil
			}
			continue
		}
		return d.matchPackage(a)
	}
	return nil
}

// matchPackage matches a package spec, ignoring a version suffix
// ("@google/gemini-cli@latest", "aider-chat==0.86").
func (d *Detector) matchPackage(t string) *Agent {
	if i := strings.Index(t, "=="); i > 0 {
		t = t[:i]
	}
	if i := strings.LastIndex(t, "@"); i > 0 {
		t = t[:i]
	}
	return d.matchToken(t)
}

// matchWrapped matches the program that env or sudo runs: skip their flags
// and environment assignments, then match the remaining argv like any other.
func (d *Detector) matchWrapped(args []string, withValue map[string]bool) *Agent {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return d.matchArgs(args[i+1:])
			}
			return nil
		case len(a) > 1 && a[0] == '-':
			if withValue[a] {
				i++
			}
		case strings.Contains(a, "="):
			// environment assignment
		default:
			return d.matchArgs(args[i:])
		}
	}
	return nil
}

// matchScriptCmd matches the command that script(1) runs: "-c <command>"
// (util-linux) or the command after the typescript file (BSD).
func (d *Detector) matchScriptCmd(args []string) *Agent {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--command":
			if i+1 < len(args) {
				return d.matchScript(args[i+1:])
			}
			return nil
		case strings.HasPrefix(a, "--command="):
			return d.matchScript([]string{strings.TrimPrefix(a, "--command=")})
		case a == "--":
			continue
		case len(a) > 1 && a[0] == '-':
			if scriptValueFlags[a] {
				i++
			}
		default:
			// BSD form: script [file [command ...]]
			if i+1 < len(args) {
				return d.matchArgs(args[i+1:])
			}
			return nil
		}
	}
	return nil
}
