# Instructions for coding agents

These rules apply to every agent (Claude Code, Codex and others) that changes this repository. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the build, test and documentation rules; this file adds what agents must not skip.

## Review the visuals of every UI change

Any change that affects what a person sees (templates, CSS, client scripts, the tray, text printed to a terminal) is reviewed visually before it is reported as done:

- Run the changed build and open the changed pages or screens in a real browser (or desktop, for the tray). Sign in the way the environment allows.
- Take screenshots and look at them: at a wide desktop width, a laptop width (about 1024 px), a tablet width (about 768 px) and a phone width (about 390 px), in light and dark mode, with realistic data (long names, long values, empty and offline states).
- Check that nothing a person needs is cut, hidden, overlapped or misaligned, and that the page does not scroll sideways. Values that people compare by eye (fingerprints, key words, codes) must always be shown in full.
- Measurements in tests are not a substitute for looking at the result. Report what you looked at, at which sizes.

## Ship Hub-only fixes on the fast path

When a change touches only the Hub (web UI templates, CSS, client scripts, Hub-only page handlers, and their docs), deliver it quickly:

- Do not rebuild or update the Workers. Workers on the previous release keep working with the new Hub; update only the Hub.
- Skip the heavy gate run (race tests with tmux and end-to-end tags, cross builds, the install test, lint-negative, vulnerability scan). Run the tests of the changed Hub packages (for example `go test ./internal/hub/web/...`) and `make lint`, plus the visual review above.
- A change counts as Hub-only only if it touches nothing under `internal/worker/`, the Worker protocol, exporters, the CLI or installers. Split a mixed change: ship the Hub-only part on this fast path and the Worker part as its own release with the full gates.
- CI on the pull request still runs in full; the fast path saves the local gate run and the Worker rollout, not the review.

## Software feedback loop

At the start of a repository task, read any saved software lessons for this project. Use the private local
`ai-minion/software-lessons.md` file under `XDG_STATE_HOME` or `~/.local/state` on Unix. On Windows use
the same relative path under `LOCALAPPDATA`. A missing file means there are no saved lessons yet.

Record improvements to the Minion Hub, workers, installers, or operator workflow. Keep conversation transcripts, terminal output, worker identities, private logs, and credentials out of software lessons.

After a major change and at close-out, save generalized lessons that will help on a future task. Record
the verified symptom or gap, the useful change, and how it was checked. Deduplicate existing lessons.
Keep the file local and outside the repository. Share any relevant core-project fix or feature suggestion
with the user, including its expected benefit. A suggestion stays a proposal until the user asks to implement it.
