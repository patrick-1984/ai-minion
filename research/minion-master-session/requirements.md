# A77. Session-scoped Minion master integration

Discovery proposal, 2026-11-21. Canonical ask and release status are tracked outside the repository (A77). This wave starts from ai-minion `c8aaf50` and follows the recovery and Usage releases.

> “when minion worked is installed and starts to see sessions ... one ... nominated ... master ... expose skill/plugin for this one only session ... make most of the minion network (interact, give tasks etc ... very effective) ... only ... nominated ... only during duration ... skill/plugin files dropped in proper places ... depending ... harness ... and/or modified this session setting on fly ... once its master status ... removed ... removed”

The follow-up was “add that to this workload quye”. Research and fixture validation are authorized now. Work on the implementation and release follows queue order. Development uses fixture profiles and private terminal hosts.

## User experience

1. A person with the existing nomination permission selects a discovered session in **Access > Minion control**. The UI identifies the target session and reports whether its harness integration is active, waiting for a supported attachment, offline, or unsupported. A successful nomination and successful native attachment are distinct facts.
2. An attached master can find permitted Workers and sessions, inspect a target, submit an ordinary task to an eligible agent, and see the delivery outcome. The guide describes the shortest working sequence, the Hub consent rules, and how to handle failed, partial, or unknown delivery. A nomination with no consenting targets has an empty target list.
3. While nominated, the selected live session receives the integration. Two live sessions using the same user, working directory, and profile receive independent decisions. Activation can occur at a supported harness boundary such as the next turn. A running harness that was started without a supported attachment gets an explicit session-only setup path for a future deliberate launch or attach.
4. A person can revoke the nomination through the same control page. The Hub denies later master calls immediately at authorization. The integration withdraws its current tools, guidance, and Minion-owned artifacts on the supported boundary. Conversation text and past tool results can remain in native history.

## Acceptance evidence

| Case | Evidence required before release |
| --- | --- |
| Targeted live activation | Start two fixture sessions with the same user, working directory, and baseline profile. Show their catalogs and model inputs before nomination, after nominating one, and after revocation. Verify the second session never gets master tools, context, or credentials. Check the baseline settings and unrelated tools across both. |
| Effective permitted actions | A real Hub and two fixture Workers deliver benign task text to a consenting fixture agent as intended. Verify scoped discovery, read/preview, delivery outcome, and request-ID behavior. Show refusals for an unrelated Worker, a hidden or unshared target, missing consent, out-of-scope operations, and a master with no targets. |
| Authorization and revocation | Replay a stale tool invocation and a pre-revocation attachment after revocation or renewal failure. Verify denial, including when the display name and target list match a later nomination. Cover revoked target acceptance, key/fallback precedence, and cancellation or honest outcome for a call already admitted. |
| Incarnation identity | Replace a pane process, rename or recreate a terminal session, clear or resume a native conversation, or open a second pane under the same terminal name. A changed, ambiguous, or unverified incarnation stays inactive. Simulate recycled IDs and same-second recreation. Existing destination write protections continue to apply. |
| Lifecycle and recovery | Exercise repeated nomination/revocation, helper death, Worker and Hub restart, disconnect/reconnect, enrollment reset, owner reassignment, and expired leases. Verify cleanup only affects integration-owned files/processes, and that killed or offline clients cannot admit later calls. |
| Compatibility | Test supported harness versions and launch modes explicitly. An older Hub, Worker, or unsupported live harness shows the actual available behavior, with no downgrade to a different principal. Cover manually installed Minion plugins and configured API call keys in existing clients. |

The phrase “one only session” is interpreted as isolation of each selected session. Existing Minion control permits multiple separately nominated master sessions. Changing that policy needs a separate decision.
