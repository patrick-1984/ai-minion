# Instructions for coding agents

These rules apply to every agent (Claude Code, Codex and others) that changes this repository. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the build, test and documentation rules; this file adds what agents must not skip.

## Review the visuals of every UI change

Any change that affects what a person sees (templates, CSS, client scripts, the tray, text printed to a terminal) is reviewed visually before it is reported as done:

- Run the changed build and open the changed pages or screens in a real browser (or desktop, for the tray). Sign in the way the environment allows.
- Take screenshots and look at them: at a wide desktop width, a laptop width (about 1024 px), a tablet width (about 768 px) and a phone width (about 390 px), in light and dark mode, with realistic data (long names, long values, empty and offline states).
- Check that nothing a person needs is cut, hidden, overlapped or misaligned, and that the page does not scroll sideways. Values that people compare by eye (fingerprints, key words, codes) must always be shown in full.
- Measurements in tests are not a substitute for looking at the result. Report what you looked at, at which sizes.
