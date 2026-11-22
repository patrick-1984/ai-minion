# Usage and subscription limits

Back to the [README](../../README.md).

Open **Usage** in the Hub for subscription limits, accounts, session counters, Worker inventory and personal token buckets. A user sees their own registered Workers. Administrators can see domain telemetry. Each person's account labels, ordering and buckets stay private.

**Workers** shows whether a Worker is online, when it reported and which harnesses it can identify. Select **Scan installed harnesses** to check a registered Worker. Some fields depend on a configured native producer; **Unknown** means no usable observation. The Hub does not infer tokens, charges or remaining capacity from a terminal screen.

**Subscriptions** uses fresh observations from distinct subscription accounts. API-key usage is excluded from subscription capacity. At **Accounts**, see each allowance's scope, consumed share, reset, observation time and source. A five-hour window and a weekly window are separate. A pace label compares use against the elapsed part of a known window. Choose a collector Worker or a passive native session. The dedicated Codex reader does not start a conversation. Claude and Gemini native quota observations can become stale while a session is idle.

**Sessions** shows disclosed metadata, including start/end observations, folder, harness, model and available counters. **History** contains cumulative snapshots. Diagnostic events carry lifecycle types and timestamps. Prompts, task descriptions, commands, tool payloads and console output are outside the Usage report.

**Token buckets** organize either sessions or provider identities. Select individual members, all members on a Worker or all accessible members. A member can move to one bucket at a time within its kind or remain unassigned. This affects your view only.

## Show usage on your Workers (nominations)

A token bucket is private to you. **Nominating** a bucket publishes it: the bucket's label and its aggregate meters (percent, remaining amounts, resets, pace, coverage) leave the Hub and are copied to Workers, where other programs on that machine can draw a progress bar ([integrate with local programs](integrate-local.md)).

- **User nomination:** each user nominates at most one session bucket and one provider bucket of their own. They are shown on every approved Worker that user owns.
- **Domain nomination:** an administrator nominates one session bucket and one provider bucket for the whole Hub, from the administrator's own buckets. They are shown on every approved Worker. Administrators share these two slots: the last authorized change wins, and the admin list shows who set it.
- A Worker therefore receives up to four slots: `domain.session`, `domain.provider`, `user.session` and `user.provider`. Personal always means the Worker's Hub owner, also on an infrastructure Worker. A Worker that is still waiting for approval receives nothing.
- **Consent to republish:** the Hub asks you to confirm before publishing, names the data, the receiving Workers and, for a root Worker, the fact that every local account of that machine can read its copy. Withdrawing a nomination, deleting the bucket or losing the right to the domain slot removes it from the Workers within about a minute.
- **What Workers receive:** only the deliberately published label and the aggregate meters. Never member ids, session names, folders, account labels, key hints or other users' Worker ids. A meter without a known value has no value: nothing is estimated, and tokens left are never inferred from a percentage or from tokens spent.
- A preview shows exactly what your Workers receive: the Hub page, the `usage_readback` operation and the Hub-connected MCP tool. It does not need any Worker to be online.
- Nominating does not need the monitoring consent of a Worker: it is Hub data sent to the Worker, not a report from it.
- TODO(R5): confirm the delivery wording (acknowledged publication, retry) once the delivery ticket is merged.

## Register a Worker

An explicit enrollment option, `minion worker up --accept-usage`, accepts monitoring of all sessions on the selected Worker, including unshared sessions. A Worker may also review the pinned enrollment locally with `minion worker config usage` and accept with `minion worker config usage --accept`. New metadata-capable enrollments on a supporting Hub require this step before serving. Console sharing remains independently controlled. An update from a fully enrolled older Worker preserves its prior console permissions and shows pending or unknown statistics until acceptance.

Native source setup, provider translations, credential fingerprints, collector behavior, polling limits and the public API schema are documented in the [usage source reference](../../research/minion-usage/native-sources.md). See [Claude native integration](../../research/minion-usage/claude-integration.md) to configure Claude Code without replacing existing status-line behavior.
