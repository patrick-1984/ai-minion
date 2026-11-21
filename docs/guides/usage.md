# Usage and subscription limits

Back to the [README](../../README.md).

Open **Usage** in the Hub for subscription limits, accounts, session counters, Worker inventory and personal token buckets. A user sees their own registered Workers. Administrators can see domain telemetry. Each person's account labels, ordering and buckets stay private.

**Workers** shows whether a Worker is online, when it reported and which harnesses it can identify. Select **Scan installed harnesses** to check a registered Worker. Some fields depend on a configured native producer; **Unknown** means no usable observation. The Hub does not infer tokens, charges or remaining capacity from a terminal screen.

**Subscriptions** uses fresh observations from distinct subscription accounts. API-key usage is excluded from subscription capacity. At **Accounts**, see each allowance's scope, consumed share, reset, observation time and source. A five-hour window and a weekly window are separate. A pace label compares use against the elapsed part of a known window. Choose a collector Worker or a passive native session. The dedicated Codex reader does not start a conversation. Claude and Gemini native quota observations can become stale while a session is idle.

**Sessions** shows disclosed metadata, including start/end observations, folder, harness, model and available counters. **History** contains cumulative snapshots. Diagnostic events carry lifecycle types and timestamps. Prompts, task descriptions, commands, tool payloads and console output are outside the Usage report.

**Token buckets** organize either sessions or provider identities. Select individual members, all members on a Worker or all accessible members. A member can move to one bucket at a time within its kind or remain unassigned. This affects your view only.

## Register a Worker

An explicit enrollment option, `minion worker up --accept-usage`, accepts monitoring of all sessions on the selected Worker, including unshared sessions. A Worker may also review the pinned enrollment locally with `minion worker config usage` and accept with `minion worker config usage --accept`. New metadata-capable enrollments on a supporting Hub require this step before serving. Console sharing remains independently controlled. An update from a fully enrolled older Worker preserves its prior console permissions and shows pending or unknown statistics until acceptance.

Native source setup, provider translations, credential fingerprints, collector behavior, polling limits and the public API schema are documented in the [usage source reference](../../research/minion-usage/native-sources.md). See [Claude native integration](../../research/minion-usage/claude-integration.md) to configure Claude Code without replacing existing status-line behavior.
