# Patches for the native Android build

`series` lists the patches the build applies to the tmux 3.8 source (zero fuzz,
`patch -p1 --fuzz=0`, hash-checked). It is empty on purpose.

## Provenance

The Termux recipe for tmux 3.8 was reviewed at commit
`0a98a0cc82f220c76b0f543e55d645c78bc79497` of `termux/termux-packages`
(`packages/tmux`). The hashes of the files are in `../versions.lock`. A strict
dry run (`patch --fuzz=0`) against the upstream 3.8 tarball rejected three
hunks, and the build shows that none of them is needed here:

| Termux file | What it does | Why it is not applied |
| --- | --- | --- |
| `configure.ac.patch` (hunk 1) | Drops the `/etc` default of `sysconfdir`. | Rejected on 3.8 and irrelevant: the Worker always passes `-f <fixed config>`, so no configuration file is searched. |
| `configure.ac.patch` (hunk 2) | Skips the run tests of `strtonum`, `reallocarray` and `recallocarray` (they cannot run when cross compiling). | Rejected on 3.8 and unneeded: `configure` falls back to the bundled `compat/` versions when cross compiling, which the build log shows ("working strtonum... no"). |
| `compat-setproctitle.c.patch` | Adds `#include <stdlib.h>` for `getprogname()`. | Rejected on 3.8; the 3.8 source compiles for API 29 without it. |
| `tmux.h.patch` | Moves the default socket directory into the Termux prefix. | The Worker always passes `-S <private socket>`, so the compiled-in default is never used. |

The Termux build also links `libandroid-support`, `libandroid-glob` and
`utf8proc`; those exist for Android versions older than the API floor of this
build (29) or are optional features (sixel, utf8proc). They are not used. Width
of wide characters therefore comes from bionic's `wcwidth`; check it on the
devices (Phase 3) before enabling utf8proc.

## Adding a patch

1. Make it against the pristine 3.8 tree: `diff -u` or `git format-patch`, one
   concern per file, with a header that states the origin (URL and commit) and
   why it is needed.
2. Check it with `patch -p1 --fuzz=0 --dry-run` on the pristine tree.
3. Put `<file> <sha256>` into `series`.
