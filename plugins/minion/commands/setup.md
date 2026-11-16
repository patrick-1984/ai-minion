---
description: Set this machine up for Minion - detect an installed minion, or install it, then start and pair a Worker (asks before every change)
argument-hint: "[hub URL]"
---

Set this machine up as a Minion Worker, following the minion skill (the same guide as `minion llm-help`).
Hub URL from the user, if given: $ARGUMENTS

1. Detect first and report what you find: run `minion version` and `minion worker status`. If minion is installed
   and the Worker is already paired and connected, say so and stop: nothing to set up; offer the next steps
   (sharing in the Hub, MCP, becoming a Minion master).
2. If minion is missing, show the one-line install command for this platform and run it only after the user
   agrees.
3. If no Hub URL was given, ask for it (or offer Solo mode without a Hub).
4. Ask whether the Worker should run as a service or in the foreground of a terminal, then run
   `minion worker up --share-existing none --server <hub URL>` (add `--foreground` for the terminal case; on
   Linux a system service needs sudo; ask before using sudo).
5. Get the pairing code and the Worker fingerprint at once: in the foreground they are in the
   `MINION WORKER PAIRING` box; for a service run `minion worker status` (repeat for up to 30 seconds until it
   shows the code). Tell the user: in the Hub open Workers > Add worker and enter the code; if the Hub asks to
   approve, compare the fingerprint.
6. Wait for the user, then run `minion worker status` again and report: paired to whom, connected, share mode.
   Remind them that nothing is shared until they choose on the Worker's page in the Hub (Sharing). Then, with their
   consent, run `minion worker config unset share_existing` so their Hub choice survives restarts.
7. Becoming a Minion master needs a master-session nomination and Workers that accept control from this one
   (Hub: Access > Minion control); tell the user where, do not try to grant it yourself.
