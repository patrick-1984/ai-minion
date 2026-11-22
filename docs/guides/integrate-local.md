# Read the Worker's status from another program

Other programs on the same machine can find out, without any credential, whether a Minion Worker runs for the current account or as root, which version it is, and (when the Hub nominated usage statistics) how much of a limit is left. This page is the contract. Back to the [README](../../README.md).

The contract has two documents:

- the **presence record** (`minion.presence/1`, schema [presence.schema.json](../schemas/presence.schema.json)): is a Worker running, which version, which scope;
- the **usage-meters document** (`minion.usage-meters/1`, schema [usage-meters.schema.json](../schemas/usage-meters.schema.json)): the statistics an owner or administrator deliberately published to Workers ([nominations](usage.md#show-usage-on-your-workers-nominations)). It is stored next to the presence record and is absent when nothing was nominated.

Example files are in [docs/schemas/fixtures](../schemas/fixtures).

## Quick answer

Use the command and let it do the verification:

```sh
minion worker discover --json                 # one JSON document: {"schema":"minion.worker-discovery/1","workers":[...]}
minion worker discover --json --usage         # each running Worker also carries its stored usage-meters document
```

The command looks at the Workers of this account (including those that use another state directory) and at root's, and verifies every record with the checks [below](#what-a-consumer-must-check). Add `--state-dir DIR` (or set `MINION_STATE_DIR`) to also look at one specific profile.

| Field of each entry in `workers` | Meaning |
| --- | --- |
| `instance`, `status`, `source` | the instance, one of `running`, `stale`, `stopped`, `unavailable`, and where the record was found (`private`, `public`, or `socket` for an older daemon that answers on its local socket and has no record) |
| `daemon_version`, `os_scope`, `pid`, `hub_origin` | as in the presence record (absent when unknown; `hub_origin` is absent for Solo) |
| `started_at`, `updated_at` | ISO 8601 UTC time, for example `2026-11-22T10:00:00.000Z` |
| `started_age_s`, `updated_age_s`, `stale_after_s` | seconds since each timestamp, and the staleness limit |
| `error` | why a record is `unavailable` (a short reason for a person, not for parsing) |
| `usage` | with `--usage`: the stored `minion.usage-meters/1` document of that Worker, when it has one |

The top level has `schema` (`minion.worker-discovery/1`) and, for an empty or doubtful result, a `note`. Exit status: `0` at least one Worker is running, `1` none is running, `2` the search failed, or none runs and some record could not be trusted (a running Worker may be hidden). `minion worker status --json` answers for one profile and has the same optional `usage` field.

If you cannot run the command (it is not installed, or you are in a language that prefers files), read the files as described below. Never read them with a plain `cat | jq` and trust the answer: see [what a consumer must check](#what-a-consumer-must-check).

## Where the records live

There is one file per daemon instance, named `<instance>.json` (the presence record) and `<instance>.usage.json` (the usage document). The instance is a random identifier of one daemon start; it matches `^[A-Za-z0-9-]{1,64}$`. Look at every file, not at one fixed name: a host can have several Workers.

| Worker runs as | Directory | Directory mode | File mode | Who can read |
| --- | --- | --- | --- | --- |
| A non-root user (login shell, user service, `minion-worker@<user>` system unit with `User=`, launchd agent) | `<state dir>/presence/` | 0700, owned by the user | 0600 | only that user |
| Root on Linux | `/run/minion-presence/` | 0755, owned by root | 0644 | every account on the host |
| Root on macOS | `/var/run/minion-presence/` | 0755, owned by root | 0644 | every account on the host |
| A user on Windows | `%LOCALAPPDATA%\minion\state\presence\` | protected DACL: the user and SYSTEM | same | only that user |

The default state directory is `$XDG_STATE_HOME/minion` (or `~/.local/state/minion`) on Linux, `~/Library/Application Support/minion` on macOS and `%LOCALAPPDATA%\minion\state` on Windows. Windows has no root Worker and no public directory.

A **relocated profile** (a Worker started with `--state-dir`, or with `MINION_STATE_DIR`) keeps its records in its own state directory. So that a program can still find it, the Worker writes a private pointer file `<default state dir>/presence-index/<hash>.json` (mode 0600, schema `minion.presence-index/1`, with a single absolute `dir`). A consumer follows each pointer only after the same checks it applies to a record. `minion worker discover` follows them for you.

The runtime directory `/run/minion` of the control socket is a different thing and is never used for records.

The public directories are written by root only. A user can neither plant nor change a file there, but a consumer still verifies ownership: see below.

## Presence record

| Field | Meaning |
| --- | --- |
| `schema` | always `minion.presence/1` |
| `instance` | identifier of this daemon start, equal to the file name |
| `daemon_version` | the version of the running daemon, for example `v0.19.0` (not the version of the CLI you ran) |
| `os_scope` | `user` (a non-root account) or `root` |
| `state` | `running` or `stopped` |
| `started_at`, `updated_at` | UTC unix milliseconds |
| `stale_after_s` | how old `updated_at` may be, in seconds (90) |
| `pid` | the daemon's process id, informative only |
| `hub_origin` | scheme, host and optional port of the Hub; absent for a Worker without a Hub (Solo) |

The record holds nothing else: no Hub owner, Worker name, key, key words, pairing code, session data or error text.

### Heartbeat, stale and the four statuses

The Worker rewrites its record every 30 seconds and on a change, by writing a temporary file and renaming it, so a reader sees the old or the new file, never half of one. A clean stop writes `state: "stopped"`; the version stays visible, which means "installed, not running". A crash leaves a `running` record that stops being updated. A reader derives one of four statuses:

| Status | Rule |
| --- | --- |
| `running` | `state` is `running` and `now - updated_at < stale_after_s` |
| `stale` | `state` is `running` and the age is `stale_after_s` or more (a crash, or a stuck Worker) |
| `stopped` | `state` is `stopped` |
| `unavailable` | no record, or it failed a check below; also an `updated_at` in the future |

"Running" is a recent-liveness signal, not a proof of health: it says the daemon wrote its record less than 90 seconds ago. It does not say the Hub link is up.

To know whether the Worker is **installed** for an account, look at the package or the `minion` executable; the presence record only describes a daemon that started.

## Usage-meters document

`<instance>.usage.json` is the document the Hub built and the Worker stored byte for byte (at most 32 KiB). It has `schema`, `generated_at` (UTC ms) and up to four `slots`:

| Slot (`scope.kind`) | What it is |
| --- | --- |
| `domain.session`, `domain.provider` | the pair an administrator nominated for the whole Hub |
| `user.session`, `user.provider` | the pair the Worker's Hub owner nominated |

Each slot has `slot{scope,kind,label}`, an optional `headline` (the `id` of the meter a progress bar should show: the lowest fresh remaining percentage), `omitted_groups` (window groups left out so that the slot stays within 16 meters) and `meters`.

Each meter is self-describing, so a consumer can draw it without knowing what it measures:

| Field | Meaning |
| --- | --- |
| `id`, `metric`, `label` | stable identity, public metric name such as `anthropic.general.5h`, display text |
| `unit` | `percent`, `tokens`, `requests`, `usd` or `ms` (time, in milliseconds) |
| `used`, `remaining`, `limit` | amounts in `unit`; present only when known. "Tokens left" is `remaining` with `unit` `tokens`; it is never computed from a percentage or from tokens spent |
| `used_percent`, `remaining_percent` | 0 to 100, present only when known |
| `resets_at`, `duration_ms` | earliest reset (UTC ms) and the length of the window |
| `pace` | `ahead`, `on_pace`, `below` or `unknown` |
| `observed_at`, `expires_at` | oldest source observation, and the time after which the meter is expired |
| `coverage` | `known`, `stale` and `unknown` account counts behind the meter |

Rules for consumers:

- An absent field means unknown. A `0` is an observed zero. Never treat a missing amount as zero.
- A meter at or after `expires_at` is expired: grey it out or hide it, and never show it as current. The same goes for a document whose slots are all expired, and for a document older than you accept by `generated_at`. The file keeps the last document while the Hub is away.
- If `coverage.stale` or `coverage.unknown` is not 0, the value covers only part of the accounts: say so.
- **New statistics appear as new meters** (a new `id`, `metric` and `unit`) or as new optional fields. Render `meters` generically, ignore fields you do not know, and a new statistic needs no change in your program. A breaking change raises the schema version to `/2`; reject a schema you do not know.
- The `headline` can be absent (no fresh comparable percentage); then show no bar.
- Who sees it: on a root Worker the document is in the public directory and every account on the host can read it, including the Hub owner's own pair. On a user Worker only that user can.

## What a consumer must check

A file in a directory you can list is not a file you can trust. A consumer must:

1. **Open once** and check the open handle (`fstat`), not the path. Checking a path and opening it again is a race. Open with no-follow semantics (`O_NOFOLLOW` on POSIX).
2. Require a **regular file**.
3. Require the **owner**: your own uid for a private file, uid 0 for a public one. A public file owned by anyone else is hostile; a private file in a directory you do not own is not yours.
4. Require the **mode**: a private file has no group or other bits (0600); a public file is exactly 0644. The same applies to the directory: private 0700 and yours; public owned by root and not writable by group or others.
5. **Bound the read**: at most 4 KiB for a presence record and 32 KiB for a usage document. Refuse a larger file.
6. Check the **schema**: the `schema` string, the types and the fields you use. Treat a file that fails as `unavailable`, not as "not installed".
7. Check the **age**: compute `now - updated_at`; a negative age (a timestamp in the future) is `unavailable`; compare with `stale_after_s`.
8. Enumerate the private and the public candidates **independently** and report the best status. A stale private file must not hide a fresh root file, and a parse failure is not an uninstalled Worker.

These are the rules of the `internal/worker/presence` reader that the CLI uses.

## Examples

### Shell: is it running, and which version

Use the CLI's discovery output, which has already done the checks above:

```sh
minion worker discover --json \
  | jq -r '[.workers[] | select(.status=="running")][0]
           | if . then "minion \(.daemon_version) running (\(.os_scope))" else "minion not running" end' \
  || echo "minion not installed or not readable"
```

### Python: the full verified read

This reads the presence records of the current account and of root on Linux or macOS, opens every file once, and checks the handle, owner, mode, size, schema and age. It follows no pointer file: add that if you need relocated profiles, or call the CLI.

```python
import json, os, re, stat, sys, time

PUBLIC = "/run/minion-presence" if sys.platform.startswith("linux") else "/var/run/minion-presence"
STATE = os.environ.get("XDG_STATE_HOME") or os.path.expanduser("~/.local/state")
PRIVATE = (os.path.join(STATE, "minion", "presence")
           if sys.platform.startswith("linux")
           else os.path.expanduser("~/Library/Application Support/minion/presence"))
INSTANCE = re.compile(r"^[A-Za-z0-9-]{1,64}$")
REQUIRED = ("schema", "instance", "daemon_version", "started_at", "updated_at",
            "stale_after_s", "state", "os_scope")


def read_bounded(dirpath, name, want_uid, want_mode, limit):
    dfd = os.open(dirpath, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        dst = os.fstat(dfd)
        if dst.st_uid != want_uid or dst.st_mode & 0o022 or (want_mode == 0o600 and dst.st_mode & 0o077):
            raise PermissionError("directory not trusted")
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=dfd)
    finally:
        os.close(dfd)
    with os.fdopen(fd, "rb") as f:
        st = os.fstat(f.fileno())                    # the open handle, not the path
        if not stat.S_ISREG(st.st_mode) or st.st_uid != want_uid:
            raise PermissionError("not a regular file of the expected owner")
        if stat.S_IMODE(st.st_mode) != want_mode and not (want_mode == 0o600 and not st.st_mode & 0o077):
            raise PermissionError("unexpected mode")
        data = f.read(limit + 1)
        if len(data) > limit:
            raise ValueError("too large")
        return data


def check(dirpath, name, want_uid, want_mode, want_scope):
    rec = json.loads(read_bounded(dirpath, name, want_uid, want_mode, 4096))
    if not isinstance(rec, dict) or rec.get("schema") != "minion.presence/1":
        raise ValueError("schema")
    if any(k not in rec for k in REQUIRED) or rec["os_scope"] != want_scope:
        raise ValueError("fields")
    if name != rec["instance"] + ".json" or rec["state"] not in ("running", "stopped"):
        raise ValueError("instance or state")
    age = time.time() * 1000 - rec["updated_at"]
    if age < 0:
        raise ValueError("timestamp in the future")
    if rec["state"] == "stopped":
        return "stopped", rec
    return ("running" if age < rec["stale_after_s"] * 1000 else "stale"), rec


def discover():
    found = []
    for dirpath, uid, mode, scope in ((PRIVATE, os.getuid(), 0o600, "user"), (PUBLIC, 0, 0o644, "root")):
        try:
            names = os.listdir(dirpath)
        except OSError:
            continue
        for n in names:
            if n.endswith(".json") and not n.endswith(".usage.json") and INSTANCE.match(n[:-5]):
                try:
                    found.append(check(dirpath, n, uid, mode, scope))
                except (OSError, ValueError):
                    pass          # unavailable: not the same as "not installed"
    order = {"running": 0, "stale": 1, "stopped": 2}
    found.sort(key=lambda t: (order[t[0]], -t[1]["updated_at"]))
    return found


if __name__ == "__main__":
    best = discover()
    print(f"{best[0][0]} {best[0][1]['daemon_version']} ({best[0][1]['os_scope']})" if best else "unavailable")
```

### tmux or status line: a progress bar of the headline remaining percentage

`minion-bar` is a small script you keep in your `PATH`. It takes the usage document of the best running Worker from the CLI and draws the headline meter. Labels are untrusted text: the script strips control characters before printing.

```sh
#!/bin/sh
# minion-bar: "Claude Max [######----] 62%"; prints nothing when no fresh headline exists.
minion worker discover --json --usage 2>/dev/null | jq -r --argjson now "$(date +%s)000" '
  [ .workers[] | select(.status=="running") | .usage.slots[]? | select(.headline != null) ] | .[0]
  | select(. != null) as $s
  | $s.meters[] | select(.id == $s.headline and .expires_at > $now and .remaining_percent != null)
  | (.remaining_percent / 10 | floor) as $n
  | "\($s.slot.label | gsub("[[:cntrl:]]"; "")) [" + ("#" * $n) + ("-" * (10 - $n)) + "] \(.remaining_percent | floor)%"'
```

In `~/.tmux.conf`: `set -g status-right '#(minion-bar)'`. For a time-based meter (`unit` `ms`) show `remaining / 60000` minutes instead of a percentage; the same loop works for any meter.

## Security notes

- Never trust a file you did not verify with the checks above. Do not read records from `/tmp`, from a directory another user can write, or through a symbolic link.
- Labels and any other text are plain, untrusted text from a Hub: strip control characters, limit the length, and never put them in a shell command, a terminal escape sequence or HTML without escaping.
- The records never contain: keys or tokens, the pairing code, key words, the Hub owner or Worker name, session names, folders, session counts, tmux socket paths, error text, member ids, account labels or key hints.
- The Hub can write only the usage document; the presence fields are written by the Worker. A Worker that the Hub did not approve receives no usage document.
- A public file means every local account can read it. See [Security and trust model](../security.md#the-public-presence-files-of-a-root-worker).
- Containers: a Worker in a container has its own mount namespace; its `/run` is not the host's, so a host program does not see it unless the directory is mounted. On Android the Worker's state is private to the app and no other app can read it.
