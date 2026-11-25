# Minion Hub web UI: capability map (checklist for redesign verification)

Source: commit 308dafa (v0.12.0); the access pages (`/access`, `/access/grants`, `/access/why`, `/access/control`: nav row, rows 17 to 17c, the two POST rows, sections 2.17 to 2.17c, 3.15, 3.15a, the JS rows and their appendix entries) were rewritten at commit 53a708cf (v0.20.6 plus the branch `integration/access-h1`). Web UI root: `internal/hub/web/` (abbreviated `W/` below; templates `W/templates/`, JS `W/static/`).
Descriptive only. Line numbers refer to the files at the commit of their section; everything outside the access pages is still the v0.12.0 state and has not been re-read. "L" = line in the named template.

How the UI is wired (applies to every page):
- Pages and routes self-register in `init()` of `W/page_*.go` via `RegisterPage` / `RegisterRoute` / `RegisterNav` / `RegisterBanner` / `RegisterLoginProvider` (`W/web.go:194-207`). `Server.buildRoutes` (`W/web.go:534-558`) mounts `GET /login`, `GET /account/password`, every page (behind `gate`), every route (behind `gate`), a default `GET /{$}` home (`W/layout.html` default "content") only if no page claims it, and a catch-all 404 (`handleNotFound`, `W/page_login.go:123`).
- `gate(roles, exempt, next)` (`W/page_login.go:71-92`): no session -> GET: redirect to `/login?next=<path>` (HX request: `HX-Redirect`); non-GET non-HX: JSON 401 `unauthenticated`. `MustChangePassword` and not exempt -> redirect `/account/password`. Role not allowed -> 403 page "You are not allowed to open this page."
- Admin pages use `adminPageData`/`adminRoute` (`W/page_users.go:220-315`): `requireAdmin` returns 404 (not 403) for non-admins AND for an admin who is acting as another user (`sess.ActAs != nil`). Only `GET /admin` is registered with `Roles: admin` (so it answers 403).
- Non-admin-page handlers use `userPageData`/`userRoute` (`W/page_invitations.go:78-117`).
- Every HTML response: CSP `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-src ...; form-action 'self'` (`W/web.go:533-549`), `Cache-Control: no-store`. List pages (`/`, `/workers`, `/sessions`, `/workers/{worker}`) have `Inspector: true` (frame-src 'self'); `/s/{worker}/{session}` has `Embeddable: true` (`?embed=1` => compact layout, frame-ancestors 'self').
- Htmx fragment convention: forms use `hx-post` with `hx-target` = a `#...-result` div, `hx-swap="innerHTML"`, `hx-disabled-elt`. Errors come back as HTTP 200 `<p class="form-error" role="alert">` (`writeResultError`, `W/page_users.go:~322`); success of admin/user forms calls `done()` => flash cookie `minion_flash` + `HX-Refresh: true` (`W/page_users.go:~361-370`). CSRF: body has `hx-headers='{"X-Minion-CSRF": ...}'` (`layout.html:18`) and `<meta name="csrf-token">` (L7).
- Flash messages: cookie `minion_flash` (max 4 flashes, 300 runes each, 60 s), shown in `#flash-area` (`layout.go:115-256`, `layout.html:78`). Banners: `#banner-slot` (`layout.html:76`).

---

## 1. Navigation

### 1.1 Shell structure (`W/templates/layout.html`)
Three layouts selected by `View` (`layout.go:21-58`):

| Layout | Condition | Elements |
|---|---|---|
| Anonymous | not `SignedIn` | `layout.html:85-92`: top bar with brand link `/` ("Minion"), empty `#banner-slot`, `#flash-area` (flashes), `<main id=content>`. No nav, no theme picker, no sign out. Pages: only `/login` renders for anonymous (everything else redirects to login). |
| Signed-in full | `SignedIn` and not `Embed` | `layout.html:26-83`: left rail + top bar (below) |
| Embed (compact) | `SignedIn` and `Embed` (`?embed=1` on an Embeddable page = `/s/...`) | `layout.html:21-25`: icon sprite + `<main>` only; no rail/topbar/banners/flashes (embedded view never consumes the flash cookie, `layout.go:513-515`). Body gets `class="embed"` (L18). |

Common head (L1-17): `<title>{{Title}} - Minion`, csrf meta, htmx-config meta, color-scheme meta, favicon `/static/favicon.svg`, `/static/minion.css`, scripts `theme.js`, `htmx.min.js` (defer), `sse.js` (defer), `forms.js` (defer), `shell.js` (defer). Body attr `hx-ext="sse"`, skip link `#content` (L19), block "skiplinks" (L20; session page adds "Skip to session controls" when `Operate`, `session.html:39`).

### 1.2 Left rail (signed-in full layout, `layout.html:29-57`)
- Brand: `<a class="brand" href="/">` + "Minion" (L31). Mobile "Close menu" button `#menu-close` (L32).
- `<nav aria-label="Main" id="site-nav">` (L34): one block per `NavGroups` entry (L35-42). Group heading `<h2 class="nav-label" id="nav-{{ID}}">` is a link to `.Path` only when set, else plain text (L36). Links show icon + label; `aria-current="page"` on the active link (L38). A group with no visible entries is omitted (`layout.go:331-364`).
- Rail footer (L44-56): version block `data-build` "Minion {{Build.Version}}" (L45-46); if admin and a newer release is known: link `{{From}} -> {{To}}` to the release URL, `target=_blank` (L47). Identity link `<a class="identity" href="/account">` showing avatar initials, username, role label ("Administrator" / "User") with `aria-current` when on `/account` (L49-52). Sign-out form `data-json-action="/auth/logout" data-next="/login"` with button "Sign out" (L53-55).
- Build notice rules (`W/build_notice.go:~36-51`): version "dev" is shown as "dev (commit)"; the update notice only appears for role admin and while the session is NOT acting as another user (`layout.go:504-509` sets role to user while act-as); source = `SetUpdateSource`.

### 1.3 Top bar (`layout.html:60-75`)
- Menu button `#menu-button` (aria-expanded, aria-controls `site-rail`; visible below 960 px) (L61); scrim `#scrim` (L58).
- Breadcrumb `<nav class="crumbs" aria-label="Breadcrumb">` (L62-65): built by `crumbsFor` (`layout.go:378-401`): home page = only "Control center" (no link); otherwise "Control center" (/) > [for pages whose nav entry is in the admin group and `/admin` exists: "Administration" (/admin)] > page title. Pages whose Data implements `Crumbed` override: Worker page (`page_worker.go:128`: Control center > Workers (list URL incl. scope) > worker name), Session page (`page_session.go:382`: Control center > Sessions > session name), Admin overview (`page_account.go:132`: Control center > Administration), Hub identity (`page_identity.go:172`: Control center > Administration > Hub identity).
- Theme picker: `<select data-theme-select>` System / Light / Dark (L67-73). Also duplicated on `/account` (`account.html:186`).

### 1.4 Banners (`#banner-slot`, `layout.html:76-77`; sources registered via `RegisterBanner`)
| Banner | Shown to | Condition / text | Source |
|---|---|---|---|
| Act-as | any session with `ActAs` | warning "Acting as U (admin A) - N min left", with form `data-json-action="/api/v1/ops/act_as_stop"` button "Exit" (`ActAsStopURL = ops.RESTPrefix + OpActAsStop`) | `W/page_actas.go:16-40` |
| Hub identity status | admin only (`adminSession`) | warning: bootstrap B0/B1 "Set the Hub passphrase..."; B2 "no certificate yet"; danger when cert expired or `Warning=="error"`; warning when `Warning=="warning"` (expiry soon). No banner otherwise | `W/page_identity.go:96-117` |
Banner kinds: info/warning/danger (`banner-{{Kind}}`), optional action form (`BannerAction.URL` same-origin path, POSTs `{}` JSON via forms.js then reloads).

### 1.5 Navigation items (registered with `RegisterNav`; sorted by Order then Label, `web.go:391-396`)
Group heading labels: Workspace / Access / Administration (`layout.go:99-103`). The "Administration" heading links to `/admin` for admins (`layout.go:358-360`). Roles: "all" = every signed-in user (anonymous sees no nav at all). Act-as: nav is computed from `sess.Role` (`layout.go:302-364`), which stays admin while acting, so an admin keeps seeing admin items in the rail, but admin *pages* and admin POST routes answer 404 during act-as (`requireAdmin`).

| Group | Order | Label | URL | Icon | Role | Condition | Registered at |
|---|---|---|---|---|---|---|---|
| Workspace | 100 | Control center | `/` | i-home | all | - | `page_sessions.go:881` |
| Workspace | 110 | Workers | `/workers` | i-workers | all | - | `page_sessions.go:882` |
| Workspace | 120 | Sessions | `/sessions` | i-sessions | all | - | `page_sessions.go:883` |
| Access | 590 | Add worker | `/workers/add` | i-plus | all | - | `page_addworker.go:147` |
| Access | 600 | Invitations | `/invitations` | i-invite | all | - | `page_invitations.go:659` |
| Access | 610 | API keys | `/api-keys` | i-key | all | - | `page_apikeys.go:380` |
| Access | 615 | Access | `/access` | i-sessions | all | - | `page_access.go:281` |
| Access | 620 | Minion control | `/access/control` | i-sessions | admin | - | `page_minion_control.go:442` |
| Administration | 800 | Users | `/admin/users` | i-users | admin | - | `page_users.go:612` |
| Administration | 810 | Hub settings | `/admin/settings` | i-settings | admin | - | `page_settings.go:189` |
| Administration | 812 | Updates | `/admin/updates` | i-settings | admin | - | `page_updates.go:45` |
| Administration | 815 | Hub identity | `/admin/identity` | i-key | admin | - | `page_identity.go:221` |
| Administration | 820 | Identity & sign-in | `/admin/oidc` | i-shield | admin | - | `page_oidc.go:159` |
| Administration | 828 | Diagnostics | `/admin/diagnostics` | i-panel | admin | - | `page_diagnostics.go:88` |
| Administration | 830 | Audit | `/admin/audit` | i-audit | admin | - | `page_audit.go:147` |

Pages with NO nav entry (reachable only by link or URL):
- `/account` (via identity link in rail footer and layout), `/account/password` (via Account page button and forced redirect), `/admin` (via "Administration" heading, admin only), `/workers/{worker}`, `/s/{worker}/{session}`, `/admin/users/{id}`, `/access/grants` and `/access/why` (all users; reached by the `nav.access-nav` links "Who reaches what", "Grants", "Why?" on the three access pages), `/admin/session-control` (admin; no link from any template or admin overview card; only its own page links to `/access/control`; found via `grep session-control templates`).
- Admin overview cards (`/admin`, `page_account.go:136-145`): Users, Hub settings, Identity & sign-in, Hub identity, Minion control (`/access/control`), Updates, Diagnostics, Audit (8 cards; Session control is not among them).

### 1.6 Login page providers (anonymous)
`login.html:23-28`: one button per registered `LoginProvider` ("Sign in with {{Label}}", link to `/auth/oidc/{id}/login`). Providers are registered at start-up by `server/mod_oidc.go:204` for each valid OIDC config (`Order: 100`). No provider => the block is not rendered. OIDC off => no buttons.

### 1.7 Cross-page links appearing outside nav (conditions)
- Rail footer: update link (admin, newer release); identity link.
- Workers/Control center header "Add worker" button -> `/workers/add` (`workers.html:6`, `sessions.html:6`); empty state "Add worker" (`_lists.html` "no_workers").
- Workers header badge `#identity-countdown` -> `/admin/identity` when `IdentityNote` (admin only, certificate action needed; `page_identity.go:119-140`).
- Scope switch Mine / All users on `/`, `/workers`, `/sessions`, `/workers/{worker}`: only when `CanFilter` = admin and not acting as another user (`page_sessions.go:507-514`); URLs `?scope=all`.

---
## 2. Pages (one section per route)

Conventions: "Tpl" = file in `W/templates/`; `define X` = named block inside it (line = `grep -n 'define "X"'`, see section 2.0 index). Per-file line numbers of every action-bearing attribute are in Appendix A. "Roles" lists who can open the page; "gated" = how the server enforces it. Flash/ result text = what the user reads after an action.

### 2.0 Index of template blocks (file:line of each `{{define}}`)
account.html:1 content; addworker.html:1 join_created, :33 worker_added, :41 content; admin.html:1 content; apikeys.html:1 apikey_secret, :12 content; audit.html:1 content; change_password.html:1 content; debug.html:3 debug_panel; diagnostics.html:1 content; identity.html:1 content, :112 passphrase-policy, :121 proxy-confirm; invitations.html:1 invitation_key, :24 content; layout.html:96 default content (+ whole layout L1-95); login.html:1 content; minion_control.html:1 content, :48 control_row; oidc.html:1 oidc_fields, :37 content; sessioncontrol.html:1 content; session.html:1 panel, :25 result, :39 skiplinks, :41 content; sessionlist.html:1 content; sessions.html:1 dashboard, :33 content; settings.html:1 content; share.html:3 share_panel; updates.html:1 content; users.html:1 user_secret, :8 content, :11 user_list, :58 user_detail; worker.html:1 content; workers.html:1 content; `_lists.html`:1 worker_table, :9 session_table, :17 no_workers, :23 no_sessions, :29 no_match, :35 workers_results, :44 sessions_results, :52 worker_results; `_session_row.html`:1 worker_row, :28 session_row, :38 worker_pending_facts, :45 worker_approval_buttons; `_inspector.html`:1 inspector_kept, :11 inspector; `_pairing_requests.html`:1 pairing_requests, :3 pairing_requests_list; `_pipe_panel.html`:1 pipe_turns, :2 pipe_panel; `_session_panes.html`:1 session_details; `_icons.html`:1 icon, :2 icons (sprite: i-home, i-workers, i-sessions, i-plus, i-invite, i-key, i-users, i-settings, i-shield, i-audit, i-menu, i-close, i-panel, i-copy, i-pause, i-logout), :20 brandmark; `_screen.html`:1 screen (the `<pre id=screen class=term>` terminal).

### 2.1 Route table (all registered routes)

GET pages (22):

| # | GET path | Handler / data fn (registration) | Tpl | Roles / gating |
|---|---|---|---|---|
| 1 | `/login` | `Server.handleLogin` (`page_login.go:96`; `web.go:541`) | login.html | anonymous (signed-in user is redirected to `next`, or `/account/password` if must-change) |
| 2 | `/account/password` | `Server.handleChangePassword` (`page_login.go:110`; `web.go:542`) | change_password.html | signed-in (gate exempt from must-change) |
| 3 | `/` | `dashboardData` (`page_sessions.go:859`) | sessions.html | all signed-in |
| 4 | `/workers` | `listData(get, listWorkers)` (`page_sessions.go:865`) | workers.html | all |
| 5 | `/sessions` | `listData(get, listSessions)` (`page_sessions.go:871`) | sessionlist.html | all |
| 6 | `/workers/{worker}` | `workerData` (`page_worker.go:249`) | worker.html | all (404 for unknown/other owner's/other scope) |
| 7 | `/workers/add` | `userPageData(addWorkerData)` (`page_addworker.go:146`) | addworker.html | all |
| 8 | `/invitations` | `userPageData(invitationsData)` (`page_invitations.go:658`) | invitations.html | all (admin sees extras) |
| 9 | `/api-keys` | `userPageData(apiKeysData)` (`page_apikeys.go:379`) | apikeys.html | all |
| 10 | `/account` | `accountData` (`page_account.go:149`) | account.html | all |
| 11 | `/admin` | `adminOverviewData` (`page_account.go:151`) | admin.html | admin (`Roles` => 403 otherwise) |
| 12 | `/admin/users` | `adminPageData(usersListData)` (`page_users.go:610`) | users.html (`user_list`) | admin (404 otherwise/during act-as) |
| 13 | `/admin/users/{id}` | `adminPageData(userDetailData)` (`page_users.go:611`) | users.html (`user_detail`) | admin |
| 14 | `/admin/settings` | `settingsData` (`page_settings.go:188`) | settings.html | admin |
| 15 | `/admin/oidc` | `oidcData` (`page_oidc.go:158`) | oidc.html | admin |
| 16 | `/admin/identity` | `identityData` (`page_identity.go:220`) | identity.html | admin |
| 17 | `/access/control` | `controlData` (`page_minion_control.go:237`); `?check=<workerID>\|<session>` | minion_control.html | admin |
| 17a | `/access` | `accessData` (`page_access.go:163`), op `access_matrix` | access.html | all (`userPageData`) |
| 17b | `/access/grants` | `grantsData` (`page_access_grants.go:204`), op `grant_list` | access_grants.html (+ `_grant_row.html`) | all (`userPageData`) |
| 17c | `/access/why` | `accessWhyData` (`page_access.go:230`); `?source=<workerID>\|<session>&target=<workerID>\|<session>`; ops `access_matrix`, `access_explain` | access_why.html | all (`userPageData`) |
| 18 | `/admin/updates` | `updatesData` (`page_updates.go:44`) | updates.html | admin |
| 19 | `/admin/diagnostics` | `diagnosticsData` (`page_diagnostics.go:87`) | diagnostics.html | admin |
| 20 | `/admin/audit` | `auditData` (`page_audit.go:146`) | audit.html | admin |
| 21 | `/admin/session-control` | `sessionControlData` (`page_sessioncontrol.go:227`) | sessioncontrol.html | admin; NO nav item, no admin card |
| 22 | `/s/{worker}/{session}` | `sessionData` (`page_session.go:985`), `Embeddable` (`?embed=1`) | session.html | all (view-only unless `Operate`) |
| - | any other path | `handleNotFound` (`page_login.go:123`) -> default layout "content" (`layout.html:96`): h1 message ("That page does not exist.") and hint paragraph | layout.html | anonymous -> login redirect; signed-in 404; 403 text "Your account does not have access to this page. Open another page from the menu." |
| - | `GET /` fallback | `handleHome` (only if no page owns `/`) "Welcome, {{Username}}" "Use the menu to open a page." | layout.html | n/a (page 3 owns `/`) |

GET fragments / streams / scripts (12; not full pages):

| GET path | Handler | Purpose |
|---|---|---|
| `/live/dashboard` (`?scope=all`) | `registeredLiveHandler` (`page_sessions.go:862`) | SSE event `dashboard`, feeds `/` |
| `/live/workers` | `listLiveHandler(listWorkers)` (`:868`) | SSE for `/workers` |
| `/live/sessions` | `listLiveHandler(listSessions)` (`:874`) | SSE for `/sessions` |
| `/live/workers/{worker}` | `workerLiveHandler` (`page_worker.go:252`) | SSE for worker page |
| `/workers/pairing-requests` | `handlePairingRequests` (`page_pairing_requests.go:96`) | pairing requests list fragment (polled every 5 s); admin only (non-admin or failure => HTTP 286 stops polling) |
| `/onboarding.js` | `handleOnboardingScript` (`page_invitations.go:298`) | copy-button script (`data-copy`) |
| `/inspector/principal` | `handlePrincipal` (`page_inspector_context.go:149`) | JSON identity context for inspector |
| `/s/{worker}/{session}/context` | `handleSessionContext` (`:150`) | may-read check for inspector |
| `/s/{worker}/{session}/screen` | `handlePanel` (`page_session.go:718`) | screen panel fragment (live refresh) |
| `/s/{worker}/{session}/pipe/turns` | `handlePipeTurns` (`page_pipe.go:301`) | conversation turns fragment |
| `/w/{worker}/share` | `handleShareGet` (`page_share.go:139`) | share panel fragment |
| `/w/{worker}/debug` | `handleDebugGet` (`page_worker_debug.go:228`) | debug panel fragment |
| `/static/*` | `newStaticHandler` (`web.go:574`) | embedded static assets (ETag, no-cache) |

POST routes behind `gate` (36). "Result" = what answers the htmx call. U = any signed-in user (`userRoute`; the Hub op enforces ownership/admin), A = admin only (`adminRoute`, 404 otherwise), S = `sessionRoute` (resolves worker+session, view/operate rules).

| POST path | Handler | Role | Op called | Result |
|---|---|---|---|---|
| `/workers/add` | `handleAddWorker` (`page_addworker.go:148`) | U | `pairing_claim` | fragment `worker_added` or `form-error` |
| `/workers/add/join` | `handleAddWorkerJoin` (`page_addworker_join.go:236`) | U | `invitation_create` | fragment `join_created` or error |
| `/account/sessions` | `handleAccountSessions` (`page_account.go:150`) | U | `account_prefs_set` | result text; refused while acting as another user |
| `/workers/{worker}/update` | `handleWorkerUpdate` (`page_worker_update.go:130`) | U (UI shows button to admin only) | `worker_update` | result fragment |
| `/workers/update-all` | `handleWorkersUpdateAll` (`:131`) | U (UI: admin only) | `worker_update` per Worker | result fragment |
| `/workers/{worker}/approve` | `handleWorkerApprove` (`page_worker_approval.go:49`) | U | `worker_approve` | "The Worker is approved. It reconnects within moments and then receives operations." |
| `/workers/{worker}/remove` | `handleWorkerRemove` (`:50`) | U | `worker_remove` | "The Worker is removed." |
| `/workers/{worker}/repair-pairing` | `handleWorkerRepairPairing` (`:51`) | U | `worker_pairing_reissue` | "Pairing re-issued for 10 minutes. Run minion worker pair ..." |
| `/workers/pairing-requests/{pairing}/admit` | `handlePairingRequestAdmit` (`page_pairing_requests.go:97`) | U (fragment is admin-only) | `pairing_request_admit` | result in `#pairing-requests-result` |
| `/workers/pairing-requests/{pairing}/reject` | `handlePairingRequestReject` (`:98`) | U | `pairing_request_reject` | result |
| `/api-keys` | `handleAPIKeyCreate` (`page_apikeys.go:381`) | U | `api_key_create` | fragment `apikey_secret` or `form-error` |
| `/api-keys/{id}/revoke` | `handleAPIKeyRevoke` (`:382`) | U | `api_key_revoke` | "The API key is revoked." |
| `/invitations` | `handleInvitationCreate` (`page_invitations.go:660`) | U | `invitation_create` | fragment `invitation_key` or error |
| `/invitations/{id}/revoke` | `handleInvitationRevoke` (`:661`) | U | `invitation_revoke` | result |
| `/s/{worker}/{session}/text` | `handleSendText` (`page_session.go:847`) | S | `send_text` | `result` fragment + HX-Trigger events |
| `/s/{worker}/{session}/keys` | `handleSendKey` (`:878`) | S | `send_keys` | same |
| `/s/{worker}/{session}/task` | `handleSetTask` (`:896`) | S | `set_task` | same (adds `minion-task-saved`) |
| `/s/{worker}/{session}/answer` | `handleAnswer` (`:908`) | S | `answer` | same (dialog answer; `minion-dialog-stale` when dialog changed) |
| `/s/{worker}/{session}/pipe` | `handlePipeSend` (`page_pipe.go:302`) | S | `pipe_send` | same (`minion-pipe-refresh`) |
| `/w/{worker}/sessions` | `handleNewSession` (`page_session.go:957`) | S | `session_create` | HX-Redirect (or redirect 303) to the new session; or warning result |
| `/w/{worker}/share` | `handleShareSet` (`page_share.go:143`) | S | `share_set` | share panel fragment |
| `/w/{worker}/debug` | `handleDebugSet` (`page_worker_debug.go:184`) | S | `worker_debug` / `worker_logs` | debug panel fragment |
| `/admin/users` | `handleUserCreate` (`page_users.go:532`) | A | `user_create` | `user_secret` fragment (temporary password) |
| `/admin/users/{id}/reset-password` | `handleUserReset` (`:553`) | A | `user_reset_password` | `user_secret` fragment |
| `/admin/users/{id}/role` | `handleUserRole` (`:567`) | A | `user_set_role` | done() flash + reload |
| `/admin/users/{id}/deactivate` | `handleUserDeactivate` (`:577`) | A | `user_deactivate` | done() flash + reload |
| `/admin/users/{id}/act-as` | `handleActAsStart` (`:582`) | A | `act_as_start` | redirect/refresh into act-as |
| `/admin/users/{id}/identities/{identity}/detach` | `handleIdentityDetach` (`:600`) | A | `identity_detach` | result |
| `/admin/settings/{key}` | `handleSettingSet` (`page_settings.go:190`) | A | `settings_set` | "The setting K is saved." / "...is back to its default." / "Enter a value, or use Remove to clear the secret." |
| `/admin/settings-section` | `handleSettingsSection` (`page_settings.go`) | A | `settings_set` with `changes` | "The section S is saved." / "Nothing to save." |
| `/admin/oidc/providers` | `handleOIDCSave` (`page_oidc.go:97`) | A | `oidc_provider_set` | result |
| `/admin/oidc/providers/{id}/roles` | `handleOIDCRoles` (`:127`) | A | `role_rule_set` | result |
| `/admin/oidc/providers/{id}/delete` | `handleOIDCDelete` (`:116`) | A | `oidc_provider_set` (delete) | result |
| `/admin/updates/hub` | `handleHubUpdate` (`page_updates.go:46`) | A | `hub_update` | "Updating the Hub from X to Y. It restarts within a minute; reload this page then. ..." / "The Hub was not updated: R." |
| `/access/control/{action}` (user-managed, accept-all, master, bound-master, accept) | `handleControlChange` (`page_minion_control.go:275`, registered `:443`) | A | `control_set_worker` / `control_set_master` / `control_set_bound_master` / `control_set_accept` | re-rendered `control_row` + HX-Trigger `control-changed`; unknown action 404 |
| `/access/grants/{action}` (set-level, revoke, approve, add) | `handleGrantChange` (`page_access_grants.go:234`, registered `:340`); `add` goes to `handleGrantAdd` (`:296`) | U | `grant_set` (set-level, add) / `grant_revoke` / `grant_approve` (button only for an administrator on a suspended row); then `grant_list` to re-render | row actions: re-rendered `grant_row` with a status line ("Saved"; a gone grant as `tr.grant-gone` "Revoked"; HTTP 409: "Changed by someone else. This is the current state."); `add`: result in `#grant-add-result` + HX-Trigger `grants-changed` |
| `/admin/session-control` | `handleSessionControlSet` (`page_sessioncontrol.go:230`) | A | `session_control_set` | result |
| `/admin/session-control/remove` | `handleSessionControlRemove` (`:231`) | A | `session_control_set` (remove) | result |

Auth/REST endpoints used by the UI JS (outside `W/`, served by other modules): `POST /auth/login`, `POST /auth/logout` (`auth/local/login.go:270-271`), `POST /auth/reauth` (`:272`), `POST /auth/reauth/oidc` (`auth/oidc/reauth.go:36`), `GET /auth/oidc/{provider}/login`, `GET /auth/oidc/callback` (`auth/oidc/login.go:176`), `POST /api/v1/ops/password_change` (`PasswordChangeURL`, `page_login.go:33`), `POST /api/v1/ops/act_as_stop` (banner), `POST /api/v1/ops/hub_identity_{set_passphrase,reissue,rotate,change_passphrase}` (identity.js), `GET /api/v1/events` (SSE of the session view, `eventsPath`), `GET /api/v1/sessions` (`apiSessionsPath`).

---

### 2.2 `/login` (login.html)
- Purpose: sign in. Data `LoginData{Next, Providers}`; `next` from `?next=` filtered by `safeNext` (same-origin path else `/`).
- Controls (`login.html`): h1 "Sign in"; `<form method=post action="/auth/login" data-json-action="/auth/login" data-next="{{Next}}">` fields `username` (text, autocomplete username, required, autofocus; label "Username or e-mail"), `password` (required); `<p class="form-error" data-form-error hidden>`; submit "Sign in". Provider links `<a class="btn btn-secondary" href="{{Path}}">Sign in with {{Label}}</a>` inside `div.providers role=group aria-label="Other sign-in methods"` (only `{{if .Data.Providers}}`). `<noscript>` text "Signing in needs JavaScript in this browser." Without JS the form posts as a normal form (`method=post`, `/auth/login` accepts form or JSON, `auth/local/login.go:283`).
- JS: forms.js (JSON submit, error text into `[data-form-error]`, on success navigate to `data-next` if same-origin path, `user.must_change_password` => `/account/password`).
- Anonymous layout (no rail). Flash area present (e.g. messages after sign-out/redirect).

### 2.3 `/account/password` (change_password.html)
- Purpose: change own password; also forced when `MustChangePassword`.
- Elements: h1 "Change password"; `{{if .Data.MustChange}}` "You must choose a new password before you can use the Hub."; warning paragraph "changing your password revokes all your API keys..."; form `method=post action={{Action}}` (= `/api/v1/ops/password_change`) `data-json-action` same, `data-next="/"`, `data-request-id`, `data-match="new_password,confirm_password"`; fields `old_password`, `new_password`, `confirm_password` (all required, autocomplete current/new); `[data-form-error]`; submit "Change password"; `<noscript>`.
- JS: forms.js (adds `request_id` UUIDv7, checks match client-side: error "The two passwords do not match.", drops `confirm_password` from body).

### 2.4 `/` Control center (sessions.html; define `dashboard`, `content`)
- Purpose: overview of workers and sessions with live updates; inspector host.
- Header: h1 "Control center", p "Your workers and sessions, updated live."; `a.btn[href=/workers/add]` icon i-plus "Add worker".
- Scope switch `nav#scope-switch` (Mine / All users) only `{{if .Data.CanFilter}}` (admin, not acting as); `aria-current="true"` marks active; hrefs `MineURL`, `AllURL` (`/?scope=all`).
- `p#list-announce` (sr-only live), `div#list-result` (target of action results), `div#dashboard.stack[data-live-list][sse-connect={{LiveURL}}][sse-swap="dashboard"][hx-swap="innerHTML"]` renders `define dashboard`:
  - `.Notice` paragraph (e.g. "not served", `noticeNotServed`).
  - If `TotalWorkers`: `div.summary[data-summary]` three tiles: "{{Online}} of {{Total}} workers online", "{{TotalSessions}} sessions", "{{Waiting}} waiting for an answer". Then `section "Needs attention"` (`ul.attention`, max `maxAttention` items): per pending worker "Worker N is waiting for approval." + link "Review worker" (`/workers?worker=<id>[&scope=all]`); per offline non-pending worker "Worker N is offline." + "View worker"; per waiting session (item with `.Session`) a link with `data-inspect data-inspect-label data-control="open"` aria-label "Open session T" (`li` has `data-worker data-session data-key="a-<w>-<s>"`).
  - Empty state if no workers and no notice: block `no_workers` (h2 "No workers connected", text, button "Add worker").
  - Section "Workers" (+ link "All workers" `/workers`): `worker_table` (see 2.5) or empty table row "No workers connected."
  - Section "Sessions" (+ link "All sessions" `/sessions`): `session_table` or empty row "No sessions yet. A connected worker can host a new session."
- Inspector: `{{template "inspector_kept"}}` and `{{template "inspector" .Data.Context}}` (aside#inspector etc., see 4.9). Scripts: live-list.js, inspector.js.
- Live: SSE `GET /live/dashboard[?scope=all]`, event `dashboard` replaces `#dashboard` innerHTML; heartbeat 15 s (`defaultLiveHeartbeat`).

### 2.5 `/workers` (workers.html; blocks `workers_results`, `worker_table`, `worker_row`)
- Header: h1 "Workers", p "Machines that host terminal sessions. Search and filter the list; it updates live."; admin-only certificate badge `a.badge#identity-countdown[data-tone]` -> `/admin/identity` shown when `.Data.IdentityNote` (certificate expiring/expired; `workersCountdown`, admin session only); `a.btn[href=/workers/add]` "Add worker".
- Scope switch (as 2.4).
- Search form `form.toolbar method=get action=/workers role=search`: hidden `scope=all` (when scope all), hidden `worker` (when `Filter.Worker`); `q` (search, maxlength 128, placeholder "Name or owner"), `status` select (Any + Online/Offline), `kind` select, `owner` select (only when `.Data.Owners`, i.e. all-scope), button "Search", link "Clear filters" (`ClearURL`) when filter active. `p.filters-active > span.chip` "Worker: name" when `Filter.Worker`.
- `{{template "pairing_requests" .}}`: `div[data-pairing-requests-panel]` > `div#pairing-requests[hx-get=/workers/pairing-requests][hx-trigger="load, every 5s"][hx-swap=innerHTML]` + `div#pairing-requests-result`. Fragment `pairing_requests_list` (admin only; empty => nothing): section "Pairing requests", explanatory text, table (Name, Source address, Waiting since, Worker fingerprint (words + hex), Hub fingerprint it was shown, Pairing id, Actions) with per row two forms: Admit (`action=AdmitURL`, hidden `worker_fingerprint`, `hub_fingerprint`, `data-control=admit`, hx-confirm "Admit worker N from ADDR? Admit only if the Worker shows these words ...") and Reject (`RejectURL`, hidden `worker_fingerprint`, `data-control=reject`, class btn-danger, hx-confirm "Reject the pairing request of N ..."). Rows carry `data-pairing-request`.
- `p#list-announce`, `div#list-result`, `div#results[data-live-list][sse-connect={{LiveURL}} (/live/workers)]` renders `workers_results`:
  - `p.result-count[data-summary]` "N workers" (+ " shown" when filtered).
  - `.Notice`; if `PendingWorkers`: `p.notice-warning[data-pending-banner]` "N worker(s) is/are waiting for approval ... compare all six words of the fingerprint ...".
  - If `UpdatableWorkers` (admin only, Hub release known): form `action=/workers/update-all` (hx-post, hx-target `#list-result`, `data-update-all`, hx-confirm "Update N Workers to this Hub's release? Each installs the signed release and restarts; tmux sessions keep running."), hidden `scope=all` when `ScopeAll`, button "Update all outdated (N)" `data-control="update-all"`.
  - `worker_table` or `no_workers` (no workers at all) or `no_match` ("No match" empty state with "Clear filters" link).
- Worker table columns: Worker, [Owner when all-scope], Status, Kind, Sessions, Version, Actions. Row `worker_row` (`tr[data-worker][data-key=w-<id>][data-row-link=worker]`):
  - name link `a[data-control=name][data-worker-link]` -> `{{URL}}` (`/workers/<id>[?scope=all]`).
  - Status: badge "pending approval" (+ "connected") with `worker_pending_facts` (Fingerprint words+hex, Host name, Source address, Hub check text with badges "not verified"/"unknown", `data-hub-check`), else badge online/offline.
  - Kind badge `data-kind`.
  - Sessions: count link `a[data-control=sessions]` -> `SessionsURL` or 0; `{{if and .Disclose .Sessions}}` `details.worker-sessions[data-keep-open=ws-<id>]` summary `data-control=sessions-toggle` "Show sessions" listing session links (`data-control="session:<name>" data-inspect data-inspect-label`, state badge) and "All N sessions" link `data-control=sessions-all`.
  - Version: build (`data-build`) or "protocol X" or "-"; when `UpdateTo`: "-> vY" and (if `UpdateURL`) form `action=UpdateURL` (hx-confirm "Update N from A to B? The Worker installs the signed release and restarts; tmux sessions keep running.") button "Update" `data-control=update`; else `(UpdateBlock)` text `[data-update-block]`.
  - Actions: pending => `worker_approval_buttons`: if `ApproveURL`: Approve form (hidden `fingerprint`, `data-control=approve`, hx-confirm with fingerprint words/addr) and Remove form (`RemoveURL`, `data-control=remove`, btn-danger, hx-confirm "Remove worker N (fp)? It is told to forget this Hub and cannot connect again with its key."); else muted "Waiting for approval". Online => "New session" form (`action=NewURL` = `/w/<ref>/sessions`, `data-control=new-session`) or, when `NewSessionRefused`, a disabled button + help text `#nsr-<id>`; offline => muted "Offline". `SessionHostText` line "Sessions: ..." with `data-session-host`/`data-sessions-hidden`.
- Live: SSE `/live/workers[?scope=all]`.

### 2.6 `/sessions` (sessionlist.html; blocks `sessions_results`, `session_table`, `session_row`)
- Header h1 "Sessions", p "Terminal sessions on your workers. Search by name, task or worker; the list updates live."; button `a.btn-secondary[href=/workers]` "Start one from Workers".
- Scope switch; search form `method=get action=/sessions` fields `scope`(hidden), `worker`(hidden), `q` (placeholder "Name, task or worker"), `status` (Stable/Busy/Dialog/Unknown/worker-offline choices), `kind`, `owner` (all-scope only); "Search"; "Clear filters". Worker chip.
- `div#results[data-live-list][sse-connect=/live/sessions]` renders `sessions_results`: `p.result-count[data-summary]`, `.Notice`, `session_table` / `no_sessions` (h2 "No sessions yet", "Open workers" button) / `no_match`.
- Session row `tr[data-worker][data-session][data-key=s-<w>-<s>][data-row-link=session]`: name link `a[data-control=name][data-inspect][data-inspect-label]` -> `/s/<w>/<s>` (shows `Worker/` prefix); Name (the harness session name, `td.col-name[data-harness-name]`, wraps, no clamp or `tabindex`; `td.col-name.empty` when none); Task (or "No task"); Worker link `data-control=worker data-worker-link` (+offline badge) unless `HideWorker`; Owner (all-scope); Kind; State badge `data-state` alone; Activity `td.col-activity[data-state-reason]` (wraps); Source `td.col-source[data-state-source]`; Last event `td.col-last-event` (`<time>`, `data-sort-value` ISO with ms). Every column but Owner sorts. The table sits in `div.table-wrap.session-table-wrap` (a `container-type: inline-size` query container): when that box is below 1240px wide (1320px when the table has the Owner column, `table.session-table.has-owner`), whatever the window width, rail or inspector panel, the table stacks as cards with `data-label` labels; empty Name/Activity/Source/Last event cells (`.empty`) are hidden there, and the column header (with its sort buttons) is `visibility: hidden`, out of the Tab order; a phone (max-width 639px) keeps its sort bar.
- Inspector panel included; scripts live-list.js, inspector.js.

### 2.7 `/workers/{worker}` (worker.html; block `worker_results`)
- Header h1 = worker name; p "A worker and its sessions. The table updates live."; button "All workers" -> `ListURL`. Scope switch when `CanFilter` (`MineURL`/`AllURL`).
- `div#results.stack[data-live-list][sse-connect=/live/workers/{id}]` renders `worker_results`:
  - `Gone` => empty state "This worker is no longer in your list" (+ text).
  - Else `p.result-count[data-summary]`, `.Notice`, `section.worker-facts` `dl.facts-grid`: Status, Kind, Owner (if set), Version (build, "-> UpdateTo", `(UpdateBlock)`), Protocol, Sessions (session host text/`data-session-host`), Worker id `<code>`.
  - If the Worker reported `login_startup_outdated` (v0.20.6): `p.notice-warning[data-login-startup-outdated]` "Login startup is outdated — run `minion worker up` on this host to remove the 'Done' messages in new shells."
  - Pending: warning banner (`data-pending-banner`), `worker_pending_facts`, `worker_approval_buttons` (Approve / Remove, see 2.5).
  - `.Worker.RepairURL` (worker never connected): form `action=RepairURL` hx-confirm "Re-issue pairing for worker N? For 10 minutes ..." button "Re-issue pairing" `data-control=repair-pairing`, `data-key=worker-repair-<id>`.
  - `.Operate` (owner or admin): "New session" form (`NewURL`, `data-control=new-session`, `data-key=worker-actions-<id>`) or disabled variant with `NewSessionRefused` help (`data-hidden-refused`).
  - `RemoveURL` and not pending: `details.forget-worker[data-keep-open]` "Forget this Worker" with form `action=RemoveURL` hx-confirm "Forget worker N? It must be paired again to connect to this Hub." button "Forget (remove) this Worker" `data-control=forget` btn-danger.
  - Section "Sessions": link "In the session list" (`data-control=session-list`) when sessions exist; `session_table` (without Worker column, `OnWorker`) or empty state `[data-sessions-empty=<reason>]` with reason text variants: failed (session list error + "minion worker status"), waiting, none, offline, plus "Choose New session to start one." when `Operate`.
- Only when `.Data.ShareURL` (= Operate): `section#share-section` with `div#share-panel[hx-get=/w/{id}/share][hx-trigger=load]` ("Loading the sharing choice...") -> fragment `share_panel` (2.8a); `section[data-unasked-worker=<id>] hidden` "Sending" with checkbox `#unasked-worker` "Send without asking, in all sessions of this Worker" + `#unasked-worker-status` (revealed by send-unasked.js).
- Only when `.Data.DebugURL` (= Operate): `section#debug-section` with `div#debug-panel[hx-get=/w/{id}/debug][hx-trigger=load]` ("Loading the debug state...") -> fragment `debug_panel` (2.8b).
- Scripts: live-list.js, inspector.js, share-bulk.js, send-unasked.js; inspector included.

#### 2.7a Share panel fragment (share.html `share_panel`; `GET/POST /w/{worker}/share`)
Results list (`div.result`), optional `p[data-share-note]`; when `.Mode`: form `#share-form-<id>` `action=URL` hx-post target `#share-panel` `data-key=share-<id>`; help "Now: all sessions are shared, also new ones | no session is shared | only the chosen sessions are shared | nothing is shared until you choose"; fieldset (disabled when `.Locked`) radios `name=mode` values `all` (`data-control=share-all`), `select` (`share-select`), `none` (`share-none`); fieldset `[data-share-sessions]`: bulk buttons "All"/"None" (`data-share-bulk=all|none`, wrapper `[data-share-bulk-wrap]` hidden until JS) and table of sessions with checkbox `name=name value=<session>` (label "(not shared)" marker), or "The worker has no session."; submit "Apply sharing" `data-control=share-apply` (disabled when Locked). JS: share-bulk.js.

#### 2.7b Debug panel fragment (debug.html `debug_panel`; `GET/POST /w/{worker}/debug`)
Result lines; `p[data-debug-note]` + `pre[data-debug-command]` when worker disallows (`allow_debug_remote`); when `Available && Closed`: form with hidden `action=logs`, `lines`, button "Show debug state and recent log" (`data-control=debug-open`); when open: state text `[data-debug-state=on|off|unknown]` ("Debug logging is on/off ..., for N more. Level now: L."), form "Turn debug on"/"Extend debug" (hidden `enabled=1`, `lines`, number `minutes` 1-1440 default 60, `data-control=debug-on`), form "Turn debug off" (hidden `enabled=0`, `lines`; only if `State.Remote` or state unknown; `data-control=debug-off`), "Recent log" with form (hidden `action=logs`, select `lines` from `Choices`, button "Refresh" `data-control=debug-refresh`), `pre.debug-log[data-debug-log]` (or "(empty)") or `p[data-debug-log-error]`. All forms: `hx-post={{URL}}` `hx-target=#debug-panel`.

### 2.8 `/workers/add` (addworker.html)
- Header "Add worker", p "A worker is a machine that hosts terminal sessions. Choose how it joins your space."
- Panel 1 `[data-join-panel]` "Add a Worker in one step": form `method=post hx-post=/workers/add/join hx-target=#join-result` fields `name` (optional, maxlength `MaxName`), `ttl` select (`TTLs`; label adds "(at most 1 week)" unless `AdminTTL`), submit "Create the join command", `div#join-result`. `<noscript>`. If `.Data.HubFull`: `details[data-hub-fingerprint]` "Pairing with a code instead? Give the Worker this Hub's fingerprint" showing fingerprint words/hex, `pre#hub-join` (`minion worker join <url> --hub-fingerprint <fp>`), button `data-copy="#hub-join"`. `<script src="/onboarding.js">`.
- Result fragment `join_created` (`div.secret[data-join-created]`): "Join command ... created. It works once and expires: T.", `p[data-join-checks]` (hub fingerprint words/hex, server), "Copy it now..." warning, `input#join-token` readonly + button `data-copy="#join-token"` "Copy the token"; sections with `pre` + copy button: Linux/macOS `#join-unix`; Linux system service (sudo) `#join-system`; Windows PowerShell `#join-windows`; Docker `#join-docker`; Docker Compose `#join-compose`; separate settings `#join-expanded`. Link to `/invitations`.
- `div.method-grid`: panel "Pair with a code": form `hx-post=/workers/add hx-target=#pair-result` fields `code` (required, placeholder MN-ABCD-EFGH, maxlength 32), `name` (optional), submit "Add the Worker", `div#pair-result`; panel "Use an invitation" with button `a[href=/invitations]` "Create an invitation key".
- Result fragment `worker_added` (`notice-positive`): "N is now in your space.", warnings, text "It appears in your workers as pending approval ... approve it" (link `/workers`). Errors via `pairingErrorText` (invalid/expired code, too many attempts, worker offline, rate limited, already paired, not allowed).
- `{{template "pairing_requests" .}}` at the bottom (admin-only list, polled 5 s) so admin can admit/reject here too.

### 2.9 `/invitations` (invitations.html)
- Header "Invitation keys". `Notices` (upgrade notices) `p.notice-warning[data-invitation-notice]`; static note about pending approval.
- Create form `hx-post=/invitations hx-target=#create-result`: `mode` select (One-time / Reusable), `max_uses` number (min 1, max/placeholder when `MaxUsesLimit`), `ttl` select (label "(at most 1 week)" unless `Admin`), `name` (maxlength 64), `kind` select (private/infrastructure) only `{{if .Data.Admin}}`; submit "Create the key"; `div#create-result`. Result fragment `invitation_key` (`div.secret`): headline with mode/kind/limit/expiry; bound Hub fingerprint note `[data-hub-words]`; `input#invite-key` + `data-copy="#invite-key"`; `pre#invite-oneliner` (copy), `pre#invite-file` (copy), `pre#invite-compose` (copy); link back.
- List: h2 "Your invitation keys" / "All invitation keys" (`All`); admin toggle link `/invitations` vs `/invitations?all=1` (admin only). Table columns Name (+notice), Use, Kind, Uses, Expires (`<time>`), State, [Created by when All], Actions. Revoke form per row only `.CanRevoke` (`hx-post=/invitations/{id}/revoke`, hx-confirm "Revoke this invitation key? No further Worker can enroll with it.", button "Revoke [name]" btn-danger, `span#revoke-<id>`); else "none". Empty row "There are no invitation keys yet."
- Script `/onboarding.js`.

### 2.10 `/api-keys` (apikeys.html)
- Header "API keys". Create form `hx-post=/api-keys hx-target=#create-result`: `name` (required, max 64); fieldset "What the key may reach": checkboxes `name=resource` (choices `Resources`, with `Selected`, help text; `ResourceNote`); `session` text ("Only one session (optional)", max 128); fieldset "What the key may do": checkboxes `name=action` (`Actions`; manage only for admin); `bound_worker` select ("Call key for a Worker (optional)": "Not a call key" + `BoundWorkers`) only when `BoundWorkers`; `expires` select (`Expiries`); submit "Create the key"; `#create-result`. Result `apikey_secret` (`div.secret`): name, scope, expiry, `input#api-key` readonly + `data-copy="#api-key"`, usage text (`Authorization: Bearer`, `MINION_API_KEY_FILE`), link back "Back to the API keys".
- List "Your API keys": table Name, Key (hint), Reaches and may do (scope), Expires, Last used, State, Actions. Revoke form per row `.CanRevoke`: `hx-post=/api-keys/{id}/revoke`, hx-confirm "Revoke this API key? Tools that use it stop working at once.", `span#revoke-<id>`; else "none". Empty row "You have no API keys yet." Script `/onboarding.js`.
- Validation messages: `errKeyForm` (name length, "The manage action is available only for a key that reaches everything.", etc.).

### 2.11 `/account` (account.html)
Sections: Identity (`dl`: Name, Role, "Acting as" (when `ActingAs`: "(everything you do is audited with your own name)"), "Signed in until" `<time>` UTC when `ExpiresAt` non-zero); Password (button -> `/account/password`); API keys (button -> `/api-keys`); Sessions `#sessions`: explanatory text on hidden sessions; three states: (a) `Prefs` present and no `PrefsURL` (acting as another user): read-only help `[data-prefs-readonly]`; (b) `Prefs` and `PrefsURL`: form `action=PrefsURL hx-post hx-target=#sessions-result`, hidden `for_user`, checkbox `allow_hidden_sessions` (id `allow-hidden-sessions`) "Allow creating hidden sessions", help `#hidden-effective` (variants per Hub setting `hidden_sessions`), button "Save", `div#sessions-result`; (c) neither: "The Hub could not read your session preferences right now." Appearance: theme select `#theme-account[data-theme-select]`. Sign out: form `data-json-action=/auth/logout data-next=/login` button "Sign out". Server messages for the POST: "New sessions that would be hidden from your terminal are allowed." / "...are refused."; "While you act as another user, ..."; "This form was opened for another account ...".

### 2.12 `/admin` (admin.html)
`ul.link-cards`: one `a[href=Path]` card per area (icon, label, description), see list in 1.5. Page text "Areas for administrators. Every page checks your role on the server."

### 2.13 `/admin/users` and `/admin/users/{id}` (users.html)
List (`user_list`): h1 "Users"; create form `hx-post=/admin/users hx-target=#create-result` fields `username` (required, max 64), `email` (optional), `role` select (user/admin), "Create user"; result fragment `user_secret` ("Account X created." / "New temporary password for X.", `code#temporary-password`, "shown once", link "Back to the accounts"). Accounts table: Username, E-mail, Role (+ admin sources), State (active/deactivated), Password (none (single sign-on)/temporary/set), Created, Actions: link "Manage {{Username}}" -> `/admin/users/{id}`; empty row "There are no accounts."
Detail (`user_detail`): h1 username, link "All users"; sections: Account `dl` (E-mail, Role with "(admin through the identity provider)" / "(local admin)", State, Password). If active: Role form (`hx-post=/admin/users/{id}/role`, hidden `role` = `user` ("Remove the local admin role") or `admin` ("Make a local admin"), `#role-result`). If `CanReset`: Reset password form (`/reset-password`, hidden `username`, checkbox `keep_api_keys value=true` `#keep-api-keys`, hx-confirm "Reset the password of this account? ...", `#reset-result`). If active: Act as form (`/act-as`, select `minutes` from `ActAsChoices`, default 60, button "Start acting as X", `#actas-result`). Linked identities table (Provider, Issuer, Subject, E-mail, Detach form `hx-post=.../identities/{identity}/detach`, hx-confirm "Detach this identity? The user can no longer sign in with it.", `#detach-<id>`) or `IdentitiesNote`. If active: Deactivate form (`/deactivate`, hx-confirm "Deactivate this account?", btn-danger "Deactivate X", `#deactivate-result`).
Result behavior: role/deactivate use `done()` (flash + `HX-Refresh`); create and reset render the one-time password fragment.

### 2.14 `/admin/settings` (settings.html)
h1 "Hub settings". If sections: `div.settings-tools` with `nav.settings-jump` (anchor per section `#<ID>`) and filter input `#settings-filter` (JS: settings-filter.js; status `#settings-filter-status`); sections `#settings-sections > section.settings-list#<ID>` with heading and per-row `li[data-filter]` containing `form.srow hx-post=/admin/settings/{Key} hx-target=#result-{Key}`: key label + env var (`code Env`, `EnvFile` "or ... (a file that holds the value)"); value control: locked => disabled input (type password if secret) + `#why-<key>` locked text; else select (`Choices`), password input (`name=value` + hidden `secret=1`, placeholder `SecretState`) or text input (`name=value`); buttons `name=action value=set` "Save" and `name=action value=unset` ("Remove" for secrets, else "Use the default") unless locked; description `#d-<key>` Help; meta `#m-<key>` (Secret state, Source, "Applies without a restart."/"Applies after the Hub restarts.", "A changed value waits for the next restart."); `p.form-error` "The current value is not valid: ..." when `.Error`; `div#result-<key>`. Empty: "There are no settings." `<noscript>` text.

### 2.15 `/admin/oidc` Identity & sign-in (oidc.html; block `oidc_fields`)
h1 "Identity & sign-in". If `InsecureTLS`: `div#insecure-tls-warning` notice-danger with `InsecureTLSWarning`. For each provider `section`: h2 name (id); `LockedText` when not `Editable` (set by environment/settings); form `hx-post=/admin/oidc/providers hx-target=#save-<id>` hidden `id`, fields (`oidc_fields`): `name` (shown on login button), `issuer`, `client_id`, `client_secret` (password; placeholder "leave empty to keep the secret"/"not set"), `role_claim`, `scopes`, `ca_file`, checkbox `tls_insecure_skip_verify` ("Do not verify the TLS certificate ... (insecure)"), checkbox `enabled`; (inputs `disabled` when not Editable); submit "Save the provider" only when Editable; `p.form-error` "TLS verification is off for this provider." when insecure and enabled. "Role rules": form `hx-post=/admin/oidc/providers/{id}/roles` textareas `admin_values`, `user_values` (one value per line), submit "Save the role rules" (Editable), `#roles-<id>`. Delete form (Editable only) `hx-post=.../delete` hx-confirm "Delete this provider? People can no longer sign in with it." "Delete the provider". Empty: "No provider is configured." "Add a provider" panel: form `hx-post=/admin/oidc/providers hx-target=#save-new`: `id` (pattern `[a-z0-9][a-z0-9_\-]{0,31}`) + same fields (`NewProvider`), "Add the provider". Effect on login page: enabled providers appear as buttons (registered at start-up in `server/mod_oidc.go:204`).

### 2.16 `/admin/identity` Hub identity (identity.html; blocks `passphrase-policy`, `proxy-confirm`)
`div#identity-page[data-state]`. Sections: State (`dl`: Bootstrap state, Passphrase set/not, Workers can connect yes/no, Hub fingerprint (root key) words + hex + `[data-hub-fingerprint]` + hint `minion hub identity --words`, Operational key, Certificate serial, Valid from, Valid until + badge `#id-countdown[data-days]` ("N days left"/"expired", tone by `Warning`), Last change); migration note (`MigratedFrom06`); web lock notice `p#id-lock[data-until]` + `#id-lock-left` countdown when `WebLocked`. "Confirm it is you": messages for `?reauth=ok` ("Signed in again.") and `unavailable` (reauth_unavailable text); `p#id-fresh[data-fresh][data-until]` ("Your sign-in is fresh until HH:MM:SS UTC." / "Changes need a sign-in from the last five minutes."); form `#id-reauth-oidc[data-reauth=oidc]` button "Sign in again with the identity provider" when `Method=="oidc"`, else `#id-reauth-local[data-reauth=local]` with `password` field and "Confirm"; each has `[data-form-error]`. `p#id-result` status. Conditional operation forms (JSON ops via identity.js, `data-identity-op`): `CanSetPassphrase` -> `hub_identity_set_passphrase` fields `passphrase`(`data-count-passphrase`), `confirm`; `CanReissue` -> `hub_identity_reissue` fields `passphrase`, `validity_days` (1-3650, default 3650), `serial_floor` (in `<details>` Advanced); `CanRotate` -> `hub_identity_rotate` fields `passphrase`, `new_passphrase`, `confirm`, `word` (type ROTATE); `CanChange` -> `hub_identity_change_passphrase` fields `passphrase`, `new_passphrase`, `confirm`. Each form: `[data-form-error]` and `proxy-confirm` button `[data-confirm-proxy]` ("I am behind a trusted HTTPS reverse proxy - continue"; revealed on `tls_required`). `passphrase-policy` text block `[data-passphrase-policy]` with `[data-passphrase-count]`. `<noscript>`.

### 2.17 `/access/control` Minion control (minion_control.html; block `control_row`)
Page text explains master/slave sessions; `details.control-how` "How it works". Tools: `input#control-filter` ("Filter Workers"), checkbox `#control-only-masters`; `p#control-status`. Panel "Workers" table (`control-table`): per Worker row `tr#ctl-<id>[data-filter][data-masters]`: name/owner/online badge; User-managed form (`hx-post=/access/control/user-managed hx-trigger=change hx-target="closest tr" hx-swap=outerHTML`; hidden `worker_id`; checkbox `value=on` "People and API keys"); "Accepts control from": form `/access/control/accept-all` (checkbox "All Workers"); if not AcceptAll `details.control-pick` ("N chosen") with `input.pick-filter` and per other Worker form `/access/control/accept` (hidden `worker_id`, `source_worker_id`; checkbox); Master sessions: list of master forms `/access/control/master` (hidden `worker_id`, `session_name`, optional `legacy=1` with "limited" badge; checked checkbox) or "none (all slave sessions)"; `CanBeMaster`: `details` "Add a master session (N sessions)" with filter and checkbox forms, or help texts (offline / none to nominate); else "cannot be a master (thin Worker or older than 0.8.0)"; `.Error` form-error; `.Saved` "Saved: ..." with Undo form (`hx-post=/access/control/{Action}` hidden fields, button "Undo" `btn-link`). Panel "Matrix" `#ctl-matrix[hx-get=/access/control hx-select=#ctl-matrix hx-swap=outerHTML hx-trigger="control-changed from:body"]`: table (rows = master-hosting Workers, columns = target Workers) with per cell: "." (own Worker), badge "all" (target accepts all), or form `/access/control/accept` checkbox `name=value`; empty "No master session yet: nominate one in the Workers list above." Panel "Who can control this session?": `form[method=get action=/access/control][data-autosubmit]` select `name=check` (value `<workerID>|<session>`, grouped by Worker), noscript "Check" button; result `div.check-result` listing controllers and `Result.Note`; empty "No shared session is online: nothing to check." Script `minion-control.js`.

Since v0.18.0 the Worker row also carries the session-bound masters (`control_list` member `BoundMasters`, `page_minion_control.go:123`, `:200`): two forms `hx-post=/access/control/bound-master hx-target=#ctl-<id> hx-swap=outerHTML` (minion_control.html L73: an existing bound master with its state `active` / `waiting` / `offline` / `unsupported` / `retired` / `inactive`; L87: nominate a candidate that is not yet a master), handled by the action `bound-master` -> `control_set_bound_master` (`page_minion_control.go:313-321`). Removing a name-only master that an active bound master holds is refused by the operation (conflict); the page shows the refusal in the row.

### 2.17a `/access` Access: who reaches what (access.html; script `access.js`)
`div.page-head.access-wide` h1 "Who reaches what"; `nav.access-nav` (Who reaches what / Grants / Why?). `section#access-matrix[hx-get=/access hx-select=#access-matrix hx-swap=outerHTML]`: h2 "Matrix", "As of <time>", button `[data-control=refresh][hx-get=/access hx-target=#access-matrix]`, button `[data-access=print]`. Empty state: "No session reaches another Worker yet. ..." with a link to `/access/control`. Otherwise `input#access-filter`, legend (V View, M Message, C Control, star = via master, blocked, none, Claimed / Verified), `table.access-matrix` inside `div.access-scroll`: column groups per target Worker (owner, offline mark) with one column per target session; one `tbody` per source Worker, one `tr[data-filter]` per source session with the proof badge (Verified for a bound master, else Claimed); cells `td.access-ok` (glyph, link to `/access/why?...` unless masked), `td.access-blocked`, or none. The list `ul.access-list` (one line per source session with what it reaches) is the narrow-screen form of the same data. Help line: sessions on the same Worker are not governed here. Read-only: the page calls only `access_matrix`.

### 2.17b `/access/grants` Grants (access_grants.html; blocks `content`, `grant_add_form`; partial `_grant_row.html` block `grant_row`; script `access-grants.js`)
h1 "Access", text on the three levels; `nav.access-nav`. Toolbar `div.grant-tools`: `input#grant-filter`, checkboxes `#grant-only-between` ("Between users"), `#grant-only-off` ("Suspended or expired"), button `[data-grant-add]` (hidden until the script runs). `details#grant-add` holds the Add form (`form#grant-add-form[hx-post=/access/grants/add hx-target=#grant-add-result]`: source Worker and session, target Worker and session or all shared sessions, level, until, note); the script moves it into `dialog#grant-dialog`. `section#grant-panel`: `table.rtable.grant-table` with columns Source, Target, Granted, State, Decided by, Until, Actions; `tbody#grant-rows[hx-get=/access/grants hx-select=#grant-rows hx-trigger="grants-changed from:body"]`; `p#grant-none` "No grant matches the filter."; empty state `div#grant-list.empty-state`. Row `tr#grant-<id>` (`grant_row`): source session @ Worker (owner), target session or "all shared sessions of <Worker>", level, state badge, `granted_by` with the authority, until, and the forms `hx-post=/access/grants/set-level`, `/approve` (only `.IsSuspended` and `.Admin`), `/revoke`, each `hx-target="closest tr" hx-swap=outerHTML hx-sync="closest tr:replace"`. Sources offered to a non-administrator are their own Workers; targets are the Workers of their session list.

### 2.17c `/access/why` Why? (access_why.html; no script)
h1 "Why?", "This page changes nothing."; `nav.access-nav`. `form[method=get action=/access/why].access-why-form`: selects `source` and `target` (option values `<workerID>|<session>`, grouped by Worker, filled from the `access_matrix` sources and targets plus the pair asked for), button "Check". With both chosen the page calls `access_explain` and shows a summary line and the checks in order (caller, grant, authority, time, sharing, workers), each with its state mark and text; links to `/access` and `/access/grants`. No navigation entry.

### 2.18 `/admin/session-control` (sessioncontrol.html) (no nav entry)
Notice linking to `/access/control` ("Session control is now Access > Minion control ..."). Form `#sc-form hx-post=/admin/session-control hx-target=#create-result`: `worker` select (required), `session` text with `datalist#sc-sessions` (required, max 128), radios `name=targets` values `all` / `selected` (`#sc-mode-all`, `#sc-mode-selected`), fieldset `#sc-targets` checkboxes `name=target` values `worker:<id>` and `session:<id>:<name>`, note, "Save the grant". Grants table (Controller session, Targets, Created, Actions) with Remove form (`hx-post=/admin/session-control/remove`, hidden `worker`, `session`, hx-confirm "Remove this grant? The controller session loses access on its next call.", `span#remove-<i>`); empty row "There are no grants. No session can control other Workers."

### 2.19 `/admin/updates` (updates.html)
`{{with .Data.Check}}` panel "Hub": `dl.facts` Running (`[data-current]`), Channel, Newest in the channel (release link `target=_blank`, badge "newer" or "(up to date)" or "not checked yet"); `div#list-result`; if `Newer && SelfUpdate`: form `action=/admin/updates/hub hx-post hx-target=#list-result` hx-confirm "Update the Hub from X to Y? The Hub copies its data, restarts in a new container and goes back to X if the new release does not become ready." button "Update the Hub to Y" (`data-control=hub-update`); else if `Newer`: `p[data-hub-manual]` "The Hub cannot update itself here: <Reason>." + `p.help` Command. Panel "Workers": text pointing to `/workers?scope=all` (Update / Update all outdated).

### 2.20 `/admin/diagnostics` (diagnostics.html)
Two panels of `dl` rows `Label: Value`: "Hub" (`.Data.Hub`) and "This request" (`.Data.Request`), built by `diagnosticsData` from op `hub_diagnostics` (version, uptime, public URL, trusted proxies, connected Workers; peer, client IP, forwarding headers, effective HTTPS). No forms.

### 2.21 `/admin/audit` (audit.html)
Filter form `method=get action=/admin/audit`: `actor` ("user id, or key:id"), `action` (e.g. settings.set), `from`, `to` (date/time UTC), `limit` select (25/50/100/200), "Filter", "Clear" link. `p.form-error` for `.Data.Error`. Table (caption "Newest first. Times are UTC...") Time, Actor, Acting as, Action, Target, Outcome; empty row "No events match." (or "No events are listed." on error). Pager links "Newest events" (`FirstURL`) and "Older events" (`NextURL`, `rel=next`, cursor).

### 2.22 `/s/{worker}/{session}` Session (session.html; blocks `panel`, `result`, `skiplinks`; partials `_pipe_panel.html`, `_session_panes.html`, `_screen.html`)
- Root `div#session-view` with data attributes read by JS: `data-embed`, `data-context`, `data-events-url` (`/api/v1/events?all=true&worker=<id>`), `data-screen-url` (`<path>/screen`), `data-worker-id`, `data-session`, `data-online`, `data-target`.
- Header: h1 "{{Session}} on {{Worker}}"; `button#pane-toggle` ("Conversation and details" / "Details", only when panes exist and not embed; shown by JS); "New session on W" form (`action=NewURL` = `/w/<ref>/sessions`, hx-target `#session-result`) only when `Operate` and not embed, or disabled variant with `NewRefused` text `#new-refused`. Facts list: Worker + online/offline badge, Owner, Access ("You can send input"/"View only"), `li[data-unasked] hidden` checkbox `#unasked-session` "Send without asking" + `#unasked-note` (Operate only).
- Notices: `Notice` (offline: "This Worker is offline. The screen and the controls come back when it reconnects."); view-only notice when online and not Operate.
- Terminal card (`Online`): terminal bar `span#conn[data-conn]`, `span#age`, buttons `#pause-toggle` (Pause), `#copy-screen` (Copy screen), `#text-view` (Text view) (hidden until JS); `p#conn-note`, `p#conn-live`; block `panel`: `div#screen-panel` with `p#pane-status` (state badge, Kind, State, Size WxH, Captured time) + `pre#screen.term` (`screen` block). If a dialog is detected (`.Answer`): `section.answer` "The pane shows a dialog" with form `action=Answer.URL hx-post hx-target=#session-result data-write data-answer-form`, hidden reviewed fields (`expect_pane_id`, `expect_pid`, `expect_command`, `expect_session_created`, `expect_dialog_digest`, `page_session.go:302-306`), radios `name=choice`, text `choice_text` (`#answer-choice`, max 64, `data-draft`), submit "Send answer"; else `AnswerNote` notice.
- Dock (only `Operate`): `div#session-controls`; `div#session-unknown` (kept "delivery unknown" outcomes), `div#session-result` (results). Tabs `#dock-tabs` (shown by JS): `#tab-text`, `#tab-keys`, `#tab-task`, and `#tab-pipe` "Conversation" (only embed with conversation). Sections:
  - Text `#dock-text`: form `action=TextURL hx-post hx-sync="this:drop" data-write` textarea `#send-text name=text data-draft`; buttons `#send-primary name=enter value=1` "Send" and `#send-paste name=enter value=0` "Paste without Enter"; helper texts `#send-help`, `#send-note`, `#paste-help`.
  - Keys `#dock-keys`: form `action=KeysURL data-write data-keys` with one `button name=key value=<K>` per key: Send Enter (`Enter`), Esc (`Escape`), Interrupt (Ctrl-C) (`C-c`), Tab (`Tab`), Shift-Tab (`BTab`), Space (`Space`), Backspace (`BSpace`), Delete (`DC`), Up, Down, Left, Right (`sessionKeys`, `page_session.go:249-259`); keyboard surface `#remote-keys` (hidden until JS) with `#remote-keys-state`, `#remote-keys-leave`.
  - Task `#dock-task`: form `action=TaskURL` input `#task-text name=task value=<task> data-saved maxlength=1024`, "Save task", `#task-state` "Unsaved changes".
  - Conversation `#dock-pipe` (embed only, when registered conversation pane): `pipe_panel` (below).
- Result fragment `result` (`div.result.result-<kind>`): title "Label: Summary" + reason `<code>`, warnings `span.session-warning`, details "The text that was submitted" (`pre`), `.Force` form `data-force data-target` with hidden `force=1` + original fields and button "Send anyway to T" (btn-danger-solid) with note, result meta "target at HH:MM:SS UTC".
- Right pane (full page only): `aside#session-pane` with `#pane-close`, optional `.pane-tabs` (`#pane-tab-<id>`), `div#pane-view-<id>` bodies from registered panes: `conversation` (Order 50, `pipe_panel`) and `details` (Order 100, `session_details` from `_session_panes.html`: Session, Worker online/offline, Owner, Worker kind, "Your access", Task, `details` "Technical details" Worker id, Protocol); `div#pane-scrim`.
- `pipe_panel` (`_pipe_panel.html`): `div#pipe-chat[data-pipe-read][data-pipe-target]`; help text; notices `#pipe-unavailable`, `#pipe-withheld`; `ol#pipe-turns[role=log][data-latest]` of `pipe_turns` (`li.pipe-turn[data-turn][data-dir]`, notes `data-pipe-note=behind|older`, `li[data-pipe-walk]`, empty `[data-pipe-empty]` "No messages yet..."), `button#pipe-jump` "Jump to latest" + `#pipe-unread`; status `#pipe-live`, `#pipe-status`, `#pipe-live-state`; send form (when `Operate`) `#pipe-form action=SendURL (=/s/w/s/pipe) hx-target=#session-result hx-sync data-write data-pipe` textarea `#pipe-text name=text data-draft`, submit `#pipe-send` "Send message" (disabled+`data-unavailable` when not `Sendable`).
- `<noscript>` "Typing and live updates need JavaScript in this browser." Scripts: terminal-input.js, session-ui.js, send-unasked.js, pipe-chat.js (when `HasConversation`), frame-child.js (embed).
- Live: browser `EventSource` on `data-events-url` (events `screen_changed`, `state_changed`, `pipe_changed`, snapshots) triggers re-fetch of `data-screen-url`; HX-Trigger events from write routes: `minion-screen-refresh`, `minion-input-sent`, `minion-input-unknown`, `minion-dialog-stale`, `minion-task-saved`, `minion-pipe-refresh` (`page_session.go:750-806`).
- Embed (`?embed=1`): compact layout, `data-embed`, no New-session/ pane toggle, conversation tab inside dock, `frame-child.js` postMessage bridge.

---
## 3. Processes (user journeys)

Notation: `[page] element -> POST target`. Page/element names refer to section 2; "(U)" any user, "(A)" admin only. Unless stated, htmx forms are `method=post` with `hx-post`, send header `X-Minion-CSRF`, and show errors as `<p class="form-error">` in the `hx-target` div.

### 3.1 Login (local)
1. Anonymous opens any page -> `gate` redirects to `/login?next=<path+query>` (GET; HX requests get `HX-Redirect`; non-GET/non-HX get JSON 401). (`page_login.go:71-92`, `loginTarget` `:50`)
2. `[/login]` form `data-json-action=/auth/login data-next={{Next}}`: fields `username`, `password` -> `POST /auth/login` (JSON via forms.js; also works as plain form post).
3. Error: JSON error envelope `error.message` shown in `p[data-form-error]` (e.g. bad credentials); button re-enabled.
4. Success: forms.js navigates to `data-next` (same-origin path validated by `safeNext`/`sameOriginPath`, else reload); if response `user.must_change_password` -> `/account/password` regardless of `next`.
5. Signed-in user opening `/login` is redirected to `next` (or `/account/password` if must-change) (`handleLogin`).
6. Sign out: rail footer form (`layout.html` L53) or Account "Sign out" section (`account.html` L49): `data-json-action=/auth/logout data-next=/login` -> `POST /auth/logout`. shell.js intercepts (capture phase) when unsent drafts exist (`[data-draft]` with text, `[data-unresolved]`, `[data-frame-dirty]`): `window.confirm("You have text that was not sent, or a delivery that is still unknown. Sign out and discard it?")`.

### 3.2 Login (OIDC)
1. Precondition: at least one enabled valid OIDC provider (registered at start-up, `server/mod_oidc.go:204`); changes made in `/admin/oidc` become login buttons after provider re-registration/start (see 3.14).
2. `[/login]` provider link `a.btn.btn-secondary[href=/auth/oidc/{id}/login]` "Sign in with {{Label}}" -> `GET /auth/oidc/{provider}/login` (starts flow, binding cookie scoped to `/auth/oidc`) -> IdP -> `GET /auth/oidc/callback` -> session cookie -> redirect.
3. Roles from IdP claims per role rules (`/admin/oidc` "Role rules"). Account password does not exist for SSO-only users (Users page shows "none (single sign-on)"; Account page text "Accounts that sign in through an identity provider keep their password there").
4. Re-authentication for Hub identity changes uses `POST /auth/reauth/oidc` (3.13).

### 3.3 Forced / voluntary password change
1. User with `MustChangePassword` (temporary password from 3.14/Users): `gate` sends every page except `/account/password` there (`page_login.go:82-85`); `/login` also redirects there.
2. Voluntary: `/account` panel "Password" button `a.btn-secondary[href=/account/password]` "Change password".
3. `[/account/password]` form fields `old_password`, `new_password`, `confirm_password`; forms.js checks match ("The two passwords do not match."), adds `request_id` (UUIDv7), drops `confirm_password` -> `POST /api/v1/ops/password_change` (`PasswordChangeURL`).
4. Success -> navigate to `/`. Warning text on page: all API keys are revoked on change.

### 3.4 Add a worker
Entry points: nav "Add worker" (`/workers/add`), "Add worker" buttons on `/`, `/workers`, empty state `no_workers`, link in `join_created`/`worker_added` fragments.
**A. One-step join command (U)** (`/workers/add`, panel `[data-join-panel]`):
1. Optional `name`, choose `ttl` (non-admin max 1 week).
2. Submit "Create the join command" -> `POST /workers/add/join` (`hx-target=#join-result`) -> op `invitation_create` (one-time key bound to Hub fingerprint).
3. Fragment `join_created`: copy `input#join-token` (`data-copy=#join-token`) or one of the per-platform commands (`#join-unix`, `#join-system`, `#join-windows`, `#join-docker`, `#join-compose`, `#join-expanded`), each with its own `data-copy` button; all copy via `/onboarding.js`.
4. Run on the machine; new Worker enrolls as **pending**; approve (3.6). If the key is lost: revoke on `/invitations` and create again.
**B. Fingerprint-pinned pairing aid** (`details[data-hub-fingerprint]`): copy `minion worker join <hub> --hub-fingerprint <fp>` (`#hub-join`, `data-copy`), needs Worker >= v0.11.0.
**C. Pairing code** (`/workers/add`, panel "Pair with a code"):
1. On the machine: `minion worker pair` shows code `MN-ABCD-EFGH`.
2. Enter `code` (+ optional `name`) -> `POST /workers/add` (`hx-target=#pair-result`) -> op `pairing_claim`.
3. Success fragment `worker_added` ("N is now in your space", pending approval, warnings). Failures: invalid/expired code, attempts exceeded, worker offline, rate-limited, already paired, forbidden (`pairingErrorText`, `page_addworker.go`).
**D. Pairing requests (admin)** (`pairing_requests` partial on `/workers` and `/workers/add`): `hx-get /workers/pairing-requests` on load and every 5 s lists Workers waiting with a code (name, source address, since, Worker fingerprint words+hex, Hub fingerprint shown, pairing id). Admit: form -> `POST /workers/pairing-requests/{pairing}/admit` (hidden `worker_fingerprint`, `hub_fingerprint`; confirm text asks to compare six words of each) -> op `pairing_request_admit`; Reject -> `POST .../{pairing}/reject` (hidden `worker_fingerprint`) -> op `pairing_request_reject`. Results appear in `#pairing-requests-result`. Non-admin: fragment answers HTTP 286 so polling stops (panel stays empty).
**E. Reusable/infrastructure invitation key**: see 3.7; "Use an invitation" panel button -> `/invitations`.
**F. Repair pairing** (worker never connected): see 3.6.

### 3.5 Worker detail and session control
1. Open worker: link `a[data-control=name][data-worker-link]` in a row, or row click (`tr[data-row-link=worker]`, handled by inspector.js), or scope links (`?scope=all`, admin).
2. `/workers/{id}` live table (SSE) + facts. Owner/admin (`Operate`) additionally see: New session, Forget, Share panel, Sending switch, Debug panel.
3. New session: `form action=/w/<ref>/sessions` (`data-control=new-session`) -> `POST /w/{worker}/sessions` (op `session_create`) -> redirect (`HX-Redirect`, else 303) to `/s/<ref>/<newsession>`; if Worker did not name it: warning "The session was created, but the Worker did not name it. Look for it in the session list." If the Hub refuses hidden sessions: disabled button with `NewSessionRefused` help text (policy from Account page / Hub setting `hidden_sessions`).
4. Open/attach a session: session link (`data-inspect`) in lists: plain click -> inspector side pane (3.5b); ctrl/cmd/shift/middle click or no JS -> full page `/s/{worker}/{session}`; "Open full session" button in the inspector (`#inspector-open`).
5. Share sessions (`Operate`): panel `GET /w/{worker}/share` on load; radios `mode` all/select/none + checkboxes `name` + "Apply sharing" -> `POST /w/{worker}/share` (op `share_set`, `names` = sorted unique `name` values; select with no names is refused so JS switches the mode to none when nothing is ticked).
6. Close/forget: "Forget this Worker" (`details` then form) -> `POST /workers/{worker}/remove` (op `worker_remove`, hx-confirm). There is no "close session" button in the Hub UI; sessions end on the Worker.
7. Debug (`Operate`): panel open `POST /w/{worker}/debug` (`action=logs`, `lines`), on/off (`enabled=1|0`, `minutes`), refresh -> ops `worker_debug`, `worker_logs` (audited; refused unless Worker has `allow_debug_remote`).
8. Session workspace (`/s/{w}/{s}`), the main control surface:
   - Text: `POST /s/{w}/{s}/text` fields `text`, `enter=1|0` (op `send_text`; Enter in the box clicks `#send-primary`).
   - Keys: `POST .../keys` field `key` (op `send_keys`).
   - Task: `POST .../task` field `task` (op `set_task`).
   - Dialog answer: `POST .../answer` fields `choice` | `choice_text` + hidden `expect_*` reviewed tokens (op `answer`); stale dialog => `minion-dialog-stale`.
   - Pipe message (conversation): `POST .../pipe` field `text` (op `pipe_send`); read `GET .../pipe/turns`.
   - Guarded sends: a refused send that can be forced returns `result` with `.Force` form (hidden `force=1`, button "Send anyway to T", styled danger) which session-ui.js intercepts with a modal `<dialog>` naming the target (focus on Cancel; `[data-dialog-cancel]`); "Send without asking" (`#unasked-session`, `#unasked-worker`) makes send-unasked.js add `force=1` to forced-capable forms (stored in localStorage `minion.unasked.*`). Unknown delivery => entries kept in `#session-unknown`.
   - Live screen: EventSource on `/api/v1/events?all=true&worker=<id>`, refetch `GET /s/{w}/{s}/screen`; Pause (`#pause-toggle`), Copy screen (`#copy-screen`), Text view (`#text-view`); connection label `#conn`.
   - Pane: toggle `#pane-toggle` (Details/Conversation); width >= 1280 docked (state in localStorage `minion-pane`), narrower modal drawer.

#### 3.5b Inspector (list pages)
On `/`, `/workers`, `/sessions`, `/workers/{id}`: click on a `data-inspect` session link or a session row opens `aside#inspector` with iframe(s) of `/s/<w>/<s>?embed=1` (max 4 kept frames), tools "Open full session" (`#inspector-open`) and "Close" (`#inspector-close`), dialog `#inspector-capacity` when too many frames hold unsent input (Cancel `#inspector-capacity-cancel`), alert `#inspector-alert`, kept-input notice `#inspector-kept`/`#inspector-kept-list`. Identity check reads `GET /inspector/principal` and `GET /s/<w>/<s>/context`.

### 3.6 Worker approval, removal, repair (U for own workers, A for all; Hub op enforces)
1. New Worker appears **pending approval** (badge, `worker_pending_facts` fingerprint words/hex, host name, source address, hub check) in `/workers`, `/workers/{id}`, `/` attention list ("Worker N is waiting for approval." + "Review worker"), and the `p[data-pending-banner]` count.
2. Approve: form (hidden `fingerprint`) -> `POST /workers/{worker}/approve` (op `worker_approve`; confirm text "compare all six words"); success text "The Worker is approved. It reconnects within moments and then receives operations."
3. Remove (pending row) -> `POST /workers/{worker}/remove` (op `worker_remove`; confirm); "Forget" on an approved worker -> same endpoint.
4. Repair pairing (worker never connected; `RepairURL`): `POST /workers/{worker}/repair-pairing` (op `worker_pairing_reissue`, 10 min).
5. Results land in `#list-result` (live-list pages keep focus via `data-key`/`data-control`).

### 3.7 Invitations (U; admin extras)
1. `/invitations` create form -> `POST /invitations` fields `mode` (one-time/reusable), `max_uses` (reusable), `ttl`, `name`, `kind` (admin: private/infrastructure) -> op `invitation_create`.
2. Fragment `invitation_key`: copy key `#invite-key`; commands `#invite-oneliner`, `#invite-file`, `#invite-compose` (all `data-copy`).
3. List own keys (admin: `?all=1` toggle shows everyone's, adds "Created by" column). Revoke -> `POST /invitations/{id}/revoke` (op `invitation_revoke`, hx-confirm).
4. Keys limited by an upgrade show `Notices`/per-row notice. A Worker enrolling with a key waits for approval (3.6).

### 3.8 API keys (U)
1. `/api-keys` create form -> `POST /api-keys`: `name`, `resource` (many), `session` (optional single session; requires exactly one Worker resource), `action` (many; manage only for admin and only for all-reaching key), `bound_worker` (call key; only if `BoundWorkers`), `expires` -> op `api_key_create`.
2. Fragment `apikey_secret`: key shown once in `input#api-key` (`data-copy=#api-key`), usage hints.
3. Revoke -> `POST /api-keys/{id}/revoke` (op `api_key_revoke`, hx-confirm). Password change and admin password reset revoke keys (reset form has `keep_api_keys` checkbox).

### 3.9 Users administration (A)
1. List/create: `/admin/users` form `POST /admin/users` (`username`, `email`, `role`) -> op `user_create`; temp password fragment `user_secret` (shown once).
2. Manage: `/admin/users/{id}` (link "Manage X").
3. Reset password: `POST /admin/users/{id}/reset-password` (`username`, `keep_api_keys`) -> temp password fragment; user signed out everywhere.
4. Role: `POST /admin/users/{id}/role` (`role=admin|user`) local admin toggle.
5. Deactivate: `POST /admin/users/{id}/deactivate` (confirm).
6. Detach identity: `POST /admin/users/{id}/identities/{identity}/detach` (confirm).
7. Act as: `POST /admin/users/{id}/act-as` (`minutes`) -> banner "Acting as ..." with "Exit" (`POST /api/v1/ops/act_as_stop`); during act-as admin pages 404, scope switch hidden, hidden-session preference read-only, build update notice hidden, audit "Acting as" column records the pair.

### 3.10 Hub settings (A)
`/admin/settings` filter (`#settings-filter`), jump links; per key: Save (`action=set`, `value`, `secret=1` for secrets) or Use the default/Remove (`action=unset`) -> `POST /admin/settings/{key}` (op `settings_set`); env-locked rows are read-only; "applies after restart" noted per row. The section "Log export" (v0.14.0) is one form `form.settings-batch` with `v.<key>` per editable row, `s.<key>`=1 and a "Remove" checkbox `r.<key>` for secrets, and one "Save section" button -> `POST /admin/settings-section` (op `settings_set` with `changes`, all or nothing; result in `#result-<section id>`).

### 3.11 Hub identity (A)
1. State on `/admin/identity`; admin banner and Workers header badge (`#identity-countdown`) point here when action is needed.
2. Re-authenticate (changes need sign-in < 5 min): local -> `POST /auth/reauth` body `{password}`; OIDC -> `POST /auth/reauth/oidc` (redirects to IdP and back to `/admin/identity?reauth=ok` or `?reauth=unavailable`).
3. Operations (JSON, identity.js): set passphrase (`hub_identity_set_passphrase`), re-issue (`hub_identity_reissue`), rotate (`hub_identity_rotate`, type ROTATE), change passphrase (`hub_identity_change_passphrase`) -> `POST /api/v1/ops/<op>`; reload after 1.2 s on success; `tls_required` shows "I am behind a trusted HTTPS reverse proxy - continue" which resends with `confirm_trusted_proxy`; `reauth_required` message; `rate_limited` reload.

### 3.12 Updates (A)
1. `/admin/updates`: Hub running vs newest in channel (daily GitHub check); rail footer shows "vX -> vY" link for admins.
2. Hub update: if `Newer && SelfUpdate`: button -> `POST /admin/updates/hub` (confirm; op `hub_update`); otherwise manual instructions (`Reason`, `Command`).
3. Worker update: from `/workers` (row "Update" `POST /workers/{worker}/update` (confirm) and "Update all outdated (N)" `POST /workers/update-all` (confirm; `scope=all` hidden when all-scope)); `/workers/{id}` version cell; buttons shown only to admins for online, non-pending Workers with self-update feature and an older build, when Hub is a release build.

### 3.13 Audit (A)
`/admin/audit` GET filters `actor`, `action`, `from`, `to`, `limit`, cursor links "Newest events"/"Older events" (op `audit_list`).

### 3.14 Identity & sign-in / OIDC admin (A)
Add provider (`POST /admin/oidc/providers`, `id` + fields), edit (same endpoint, hidden `id`), role rules (`POST /admin/oidc/providers/{id}/roles`: `admin_values`, `user_values`), delete (`.../delete`, confirm). Env/settings-defined providers are read-only. Insecure TLS option raises `#insecure-tls-warning`.

### 3.15 Minion control (A)
All edits are single checkbox changes (`hx-trigger=change`) -> `POST /access/control/{user-managed|accept-all|master|accept}` returning the re-rendered row; "Saved: ..." with Undo; matrix refreshes on `control-changed`; "Who can control this session?" is a GET with `check=<worker>|<session>`. Legacy session-control grants: `/admin/session-control` (orphan page, A): `POST /admin/session-control`, `POST /admin/session-control/remove`.

Session-bound masters (v0.18.0): a button form per candidate or bound master -> `POST /access/control/bound-master` (`control_set_bound_master`), answer and refresh as the other actions.

### 3.15a Access: matrix, grants, why (U; the operations decide)
1. Read: nav "Access" -> `/access` (matrix, Refresh re-reads `access_matrix`, Print); a cell link or the tab "Why?" -> `/access/why?source=..&target=..` (`access_explain`).
2. Add a grant: `/access/grants`, "Add grant" (dialog, or the `details` block without scripts) -> `POST /access/grants/add` -> `grant_set`; the server refuses a session whose Worker differs from the chosen Worker; "7" / "30" days become an `expires_at`; success sets `HX-Trigger: grants-changed` and the list reloads.
3. Change a level: row select + button -> `POST /access/grants/set-level` (`grant_set` with `id` + `if_revision`). Revoke: `POST /access/grants/revoke` (`grant_revoke`). Approve a suspended grant (administrator): `POST /access/grants/approve` (`grant_approve`); a non-administrator gets 404 from the web layer.
4. Every row action re-renders the row from a fresh `grant_list`; a revision conflict (409) shows the current row with "Changed by someone else. This is the current state."

### 3.16 Diagnostics (A)
Read-only `/admin/diagnostics` (reverse-proxy check). No actions.

### 3.17 Debug (worker) (U owner/A)
See 3.5 step 7 and 2.7b.

### 3.18 Account (U)
`/account`: identity read-out; links to password and API keys; hidden-session preference (`POST /account/sessions` fields `for_user`, `allow_hidden_sessions=on`; refused while acting as another user); theme (`data-theme-select`, browser only); sign out.

### 3.19 Theme and menu (all signed-in)
Theme select in top bar and Account (`data-theme-select`) -> localStorage `minion-theme` (`system` removes it); menu open/close below 960 px (shell.js).

---
## 4. Client-side JS capabilities (`W/static/`)

Embedded by `//go:embed static/*.js static/*.css static/*.svg` (`web.go:30`), served at `/static/<file>` with ETag and `Cache-Control: no-cache`. Also served by route: `/onboarding.js` (inline const `onboardingScript`, `page_invitations.go:259-293`). CSS: `minion.css` (`.js` class, `html[data-theme]`); favicon `favicon.svg`.

| File | Loaded by | What it does | DOM hooks / attributes it relies on |
|---|---|---|---|
| `htmx.min.js` (vendored htmx 2.0.11) | layout (all pages) | Handles all `hx-*` attributes | `hx-get/post/target/swap/trigger/confirm/select/sync/disabled-elt`, `HX-Trigger/HX-Redirect/HX-Refresh` response headers; meta `htmx-config` (`includeIndicatorStyles:false`) |
| `sse.js` (htmx SSE extension) | layout | SSE for live lists | body `hx-ext="sse"`; `sse-connect`, `sse-swap` (and `data-` forms) |
| `theme.js` | layout head | Applies `localStorage minion-theme` before first paint, adds class `js` to `<html>`, reacts to `storage` event | `html[data-theme]` |
| `forms.js` | layout | JSON form submit with CSRF header; error display; navigation after success | `data-json-action`, `data-next`, `data-match` ("a,b" fields must be equal; b not sent), `data-request-id` (adds UUIDv7 `request_id`), `[data-form-error]`, `#flash-area` fallback (`#form-error-flash`), `meta[name=csrf-token]`; `user.must_change_password` => `/account/password` |
| `shell.js` | layout | Theme selects, mobile menu (focus trap, Escape, scrim), heading focus after internal navigation, unsent-draft guard (sign-out `confirm()` and `beforeunload`) | `[data-theme-select]`, `#menu-button`, `#site-rail`, `#scrim`, `#menu-close`, `.nav-list a[aria-current=page]`, `#content h1`, `body.embed`, `[data-draft]`, `[data-unresolved]`, `[data-frame-dirty]`, form `data-json-action="/auth/logout"`; localStorage `minion-theme` |
| `live-list.js` | `/`, `/workers`, `/sessions`, `/workers/{id}` | Preserves focus, scroll, open `<details>` across SSE swaps; announces summary changes | `[data-live-list]`, events `htmx:sseBeforeMessage`/`htmx:sseMessage`, `[data-key]`, `[data-control]`, `details[data-keep-open]`, `.table-wrap`, `[data-summary]`, `#list-announce`, `#content h1` |
| `inspector.js` (970 lines) | list pages | Session inspector pane with kept iframes (<=4), identity-context checks, row click-through, whole-row links, capacity dialog | `a[data-inspect]` + `data-inspect-label`, `tr[data-row-link]` (`worker`/`session`), `[data-worker][data-session]`, `[data-worker-link]`, `[data-key]`, `[data-control]`, `#inspector`, `#inspector-frames`, `#inspector-title`, `#inspector-status`, `#inspector-scrim`, `#inspector-open`, `#inspector-close`, `#inspector-kept`, `#inspector-kept-list`, `#inspector-capacity` (+`-list`, `-cancel`), `#inspector-alert`, `#inspect-layout`, `[data-modal-inert]`, `data-context` (on `#inspector`), `data-inspector-doc`, `data-frame-dirty`; fetches `/inspector/principal`, `/s/<w>/<s>/context`; postMessage with frames |
| `frame-child.js` | `/s/..?embed=1` | Frame side of inspector protocol (ready/state/close-request; query/suspend/resume); Escape asks host to close | `#session-view`, `data-worker-id`, `data-session`, `data-context`, `dialog[open]`, `data-inspector-doc`, event `minion-frame-control` |
| `terminal-input.js` (1255 lines) | `/s/...` | Session view controller: EventSource on events URL, fetch screen panel, terminal colour classes in a constructed stylesheet, write-guard (drafts, unknown deliveries kept in `#session-unknown`, pause/suspend/reconnect disable writes), Enter-to-send composer, remote keyboard surface, HX-Trigger event handling | `#session-view` (`data-events-url`, `data-screen-url`, `data-worker-id`, `data-session`, `data-online`, `data-target`), `#screen`, `#screen-panel`, `#pane-status`, `#conn`, `#conn-note`, `#conn-live`, `#age`, `#pause-toggle`, `#session-result`, `#session-unknown`, `#session-controls`, `#send-text`, `#send-primary`, `#send-paste`, `#send-note`, `#task-text`, `#tab-keys`, `#remote-keys`, `#remote-keys-state`, `#remote-keys-leave`, `#answer-choice`, `[data-answer-form]`, `[data-write]`, `[data-keys]` (`button[name=key]`), `[data-draft]`, `#pipe-text`, `#pipe-send`, `#pipe-note`; events `htmx:*`, HX-Trigger names `minion-screen-refresh`, `minion-input-sent`, `minion-input-unknown`, `minion-dialog-stale`; `data-unresolved`, `data-unknown*`, `data-gated`, `data-conn` |
| `session-ui.js` | `/s/...` | Dock tabs (arrow/Home/End keys), right pane (docked >=1280 px with localStorage `minion-pane`, modal drawer below), Copy screen, Text view, "Send anyway" modal confirmation, task "Unsaved changes", dialog answer exclusivity (radio vs typed) | `#dock-tabs`, `[role=tab]`, `.pane-tabs`, `#session-controls`, `#pane-toggle`, `#session-pane`, `#session-grid`, `#pane-close`, `#pane-scrim`, `#copy-screen`, `#text-view`, `#screen`, `#task-text` (`data-saved`), `#task-state`, `form[data-force]>button`, `[data-dialog-cancel]`, `#answer-choice`, `[data-answer-form]`; event `minion-task-saved`, `minion-pane`; `#site-rail` (inert handling) |
| `pipe-chat.js` | `/s/..` when `HasConversation` | Keeps conversation turn list current (GET `data-pipe-read`, coalesced reads, scroll-follow only at bottom, "N new" jump button, polite announcements) | `#pipe-chat` (`data-pipe-read`, `data-pipe-target`), `#pipe-turns` (`data-latest`), `[data-turn]`, `[data-pipe-walk]`, `data-pipe-behind`, `#pipe-jump`, `#pipe-unread`, `#pipe-live`, `#pipe-status`, `#pipe-live-state`; event `minion-pipe-refresh` |
| `send-unasked.js` | `/s/...`, `/workers/{id}` | "Send without asking": per-session and per-Worker opt-in stored in localStorage `minion.unasked.s.<worker>\|<session>` / `minion.unasked.w.<worker>`; adds `force=1` on `htmx:configRequest` for forced-capable forms | `#session-view`, `#unasked-session`, `#unasked-note`, `[data-unasked]` (hidden until JS), `[data-unasked-worker]`, `#unasked-worker`, `#unasked-worker-status`, `form[data-force]` |
| `share-bulk.js` | `/workers/{id}` | Share panel All/None buttons; any session checkbox change syncs radio `mode` (select/none); reveals bulk wrap after swaps | `[data-share-bulk]`, `[data-share-bulk-wrap]`, `input[name=name]`, `input[name=mode]`, event `htmx:afterSwap` |
| `settings-filter.js` | `/admin/settings` | Live filter rows by key/env/help; hides empty sections; status line | `#settings-filter`, `#settings-filter-status`, `#settings-sections section`, `li[data-filter]` |
| `minion-control.js` | `/access/control` | Filter Workers/matrix, "only Workers with master sessions", picker filters, auto-submit of the check select, reload on lost answer (sendError/timeout/5xx) | `#control-filter`, `#control-only-masters`, `#control-status`, `.control-table`, `.control-matrix`, `tr[data-filter][data-masters]`, `.pick-filter`, `li[data-filter]`, `form[data-autosubmit] select`, `.check-result` |
| `access.js` (11 lines) | `/access` | Print button (`window.print`), filter of the source rows and of the narrow list | `[data-access=print]`, `#access-filter`, `#access-matrix [data-filter]` |
| `access-grants.js` (81 lines) | `/access/grants` | Client-side filter (text, "Between users", "Suspended or expired"), moves the Add form into the dialog and opens it, closes it on `grants-changed`, limits the session selects to the chosen Worker | `#grant-filter`, `#grant-only-between`, `#grant-only-off`, `[data-grant-add]`, `[data-grant-cancel]`, `[data-grant-worker]`, `[data-filter]`, `#grant-dialog`, `#grant-add-form` |
| `identity.js` | `/admin/identity` | Reauth (local/OIDC), JSON identity ops, clears secrets, passphrase code-point counter, proxy confirm, lock countdown, fresh-sign-in timer | `form[data-reauth=local|oidc]`, `form[data-identity-op]`, `[data-confirm-proxy]`, `[data-count-passphrase]`, `[data-passphrase-count]`, `#id-result`, `#id-lock` (`data-until`), `#id-lock-left`, `#id-fresh` (`data-until`), `[data-form-error]`; posts `/auth/reauth`, `/auth/reauth/oidc`, `/api/v1/ops/<op>` |
| `/onboarding.js` (route) | `/workers/add`, `/api-keys`, `/invitations` | Delegated click handler for copy buttons: selects text, `navigator.clipboard.writeText`, button text "Copied" / "Press Ctrl+C to copy" | `[data-copy="#id"]`, optional `data-label` |

Browser storage keys: `minion-theme` (theme), `minion-pane` (session pane open/closed), `minion.unasked.w.<worker>`, `minion.unasked.s.<worker>|<session>`. Cookies: session cookie, CSRF (token in meta + header), `minion_flash` (flash messages), OIDC binding cookie.
Server-sent HX headers used: `HX-Trigger` (session result events, `control-changed`), `HX-Redirect` (login redirect for HX requests, new session), `HX-Refresh` (after `done()`), HTTP 286 (stop polling pairing requests).

---

## Appendix A. Template -> action-bearing attributes (diff-friendly)

Extracted mechanically from `W/templates/*.html` (regex over: `action`, `hx-get/post/target/swap/trigger/confirm/select/sync/disabled-elt`, `data-json-action`, `data-copy`, `data-next`, `data-match`, `data-request-id`, `data-identity-op`, `data-reauth`, `data-control`, `data-inspect`, `data-confirm-proxy`, `data-share-bulk`, `data-autosubmit`, `data-write`, `data-force`, `data-answer-form`, `data-keys`, `data-pipe`, `data-draft`, `sse-connect/sse-swap`, `data-live-list`, `name=`, `href=`/`src=` that are absolute paths or template expressions). `hx-confirm` text is truncated to 70 chars. Lines are per-file line numbers. Shorthand `{{...}}` = template expression kept verbatim. Note: `name=` on hidden inputs inside `{{range}}` shows the template expression.

### _icons.html (0 lines with action attributes)

- (none)

### _inspector.html (1 lines with action attributes)

- L17: href="/sessions"

### _lists.html (14 lines with action attributes)

- L20: href="/workers/add"
- L26: href="/workers"
- L32: href="{{.ClearURL}}"
- L38: action="/workers/update-all"; hx-post="/workers/update-all"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Update {{.UpdatableWorkers}} Workers to this Hub's release? Each insta..."
- L39: name="scope"
- L40: data-control="update-all"
- L69: action="{{.Worker.RepairURL}}"; hx-post="{{.Worker.RepairURL}}"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Re-issue pairing for worker {{.Worker.Name}}? For 10 minutes the next ..."
- L71: data-control="repair-pairing"
- L74: data-control="new-session"
- L76: action="{{.Worker.NewURL}}"; hx-post="{{.Worker.NewURL}}"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L77: data-control="new-session"
- L81: action="{{.Worker.RemoveURL}}"; hx-post="{{.Worker.RemoveURL}}"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Forget worker {{.Worker.Name}}? It must be paired again to connect to ..."
- L82: data-control="forget"
- L87: href="{{.SessionsURL}}"; data-control="session-list"

### _pairing_requests.html (8 lines with action attributes)

- L1: hx-get="/workers/pairing-requests"; hx-trigger="load, every 5s"; hx-swap="innerHTML"
- L15: action="{{.AdmitURL}}"; hx-post="{{.AdmitURL}}"; hx-target="#pairing-requests-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Admit worker {{.Name}} from {{.SourceAddr}}? Admit only if the Worker ..."
- L16: name="worker_fingerprint"
- L17: name="hub_fingerprint"
- L18: data-control="admit"
- L20: action="{{.RejectURL}}"; hx-post="{{.RejectURL}}"; hx-target="#pairing-requests-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Reject the pairing request of {{.Name}} from {{.SourceAddr}}? The Work..."
- L21: name="worker_fingerprint"
- L22: data-control="reject"

### _pipe_panel.html (2 lines with action attributes)

- L13: action="{{.SendURL}}"; hx-post="{{.SendURL}}"; hx-target="#session-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-sync="this:drop"; data-write
- L16: name="text"; data-draft

### _screen.html (0 lines with action attributes)

- (none)

### _session_panes.html (0 lines with action attributes)

- (none)

### _session_row.html (17 lines with action attributes)

- L2: href="{{.URL}}"; data-control="name"
- L6: href="{{.SessionsURL}}"; data-control="sessions"
- L8: data-control="sessions-toggle"
- L10: href="{{.Path}}"; data-control="session:{{.Name}}"; data-inspect
- L13: href="{{.SessionsURL}}"; data-control="sessions-all"
- L16: action="{{.UpdateURL}}"; hx-post="{{.UpdateURL}}"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Update {{.Name}} from {{.Build}} to {{.UpdateTo}}? The Worker installs..."
- L17: data-control="update"
- L20: data-control="new-session"
- L22: action="{{.NewURL}}"; hx-post="{{.NewURL}}"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L23: data-control="new-session"
- L29: href="{{.Path}}"; data-control="name"; data-inspect
- L31: href="{{.WorkerURL}}"; data-control="worker"
- L45: action="{{.ApproveURL}}"; hx-post="{{.ApproveURL}}"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Approve worker {{.Name}} with fingerprint {{if .FingerprintWords}}{{.F..."
- L46: name="fingerprint"
- L47: data-control="approve"
- L49: action="{{.RemoveURL}}"; hx-post="{{.RemoveURL}}"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Remove worker {{.Name}} ({{if .FingerprintWords}}{{.FingerprintWords}}..."
- L50: data-control="remove"

### _grant_row.html (10 lines with action attributes)

- L10: hx-post="/access/grants/set-level"; hx-target="closest tr"; hx-swap="outerHTML"; hx-sync="closest tr:replace"; hx-disabled-elt="find button"
- L11: name="id"; name="if_revision"
- L12: name="source_worker_id"; name="source_session"
- L13: name="target_worker_id"; name="target_session"
- L14: name="expires_at"; name="note"
- L16: name="level"
- L23: hx-post="/access/grants/approve"; hx-target="closest tr"; hx-swap="outerHTML"; hx-sync="closest tr:replace"; hx-disabled-elt="find button"
- L24: name="id"; name="if_revision"
- L27: hx-post="/access/grants/revoke"; hx-target="closest tr"; hx-swap="outerHTML"; hx-sync="closest tr:replace"; hx-disabled-elt="find button"
- L28: name="id"; name="if_revision"

### access.html (6 lines with action attributes)

- L4: href="/access"; href="/access/grants"; href="/access/why"
- L5: hx-get="/access"; hx-select="#access-matrix"; hx-swap="outerHTML"
- L8: hx-get="/access"; hx-target="#access-matrix"; hx-disabled-elt="this"
- L10: href="/access/control"
- L24: href="{{.Href}}"; href="{{.Href}}"
- L32: src="/static/access.js"

### access_grants.html (16 lines with action attributes)

- L4: href="/access"; href="/access/grants"; href="/access/why"
- L20: hx-get="/access/grants"; hx-select="#grant-rows"; hx-swap="outerHTML"; hx-trigger="grants-changed from:body"
- L22: hx-get="/access/grants"; hx-select="#grant-panel"; hx-target="#grant-panel"; hx-swap="outerHTML"; hx-trigger="grants-changed from:body"
- L27: src="/static/access-grants.js"
- L30: action="/access/grants/add"; hx-post="/access/grants/add"; hx-target="#grant-add-result"; hx-swap="innerHTML"; hx-disabled-elt="find button[type=submit]"
- L33: name="source_worker_id"
- L37: name="source_session"
- L43: name="target_worker_id"
- L47: name="target_session"
- L54: name="level"
- L55: name="level"
- L56: name="level"
- L61: name="until"
- L62: name="until"
- L63: name="until"
- L66: name="note"

### access_why.html (5 lines with action attributes)

- L4: href="/access"; href="/access/grants"; href="/access/why"
- L7: action="/access/why"
- L8: name="source"
- L10: name="target"
- L13: href="/access/grants"

### account.html (6 lines with action attributes)

- L16: href="/account/password"
- L21: href="/api-keys"
- L27: action="{{.Data.PrefsURL}}"; hx-post="{{.Data.PrefsURL}}"; hx-target="#sessions-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L28: name="for_user"
- L29: name="allow_hidden_sessions"
- L49: data-json-action="/auth/logout"; data-next="/login"

### addworker.html (18 lines with action attributes)

- L4: href="/invitations"
- L8: data-copy="#join-token"
- L13: data-copy="#join-unix"
- L17: data-copy="#join-system"
- L20: data-copy="#join-windows"
- L24: data-copy="#join-docker"
- L27: data-copy="#join-compose"
- L31: data-copy="#join-expanded"
- L38: href="/workers"
- L46: hx-post="/workers/add/join"; hx-target="#join-result"; hx-swap="innerHTML"; hx-disabled-elt="find button[type=submit]"
- L49: name="name"
- L54: name="ttl"
- L67: data-copy="#hub-join"
- L70: src="/onboarding.js"
- L76: hx-post="/workers/add"; hx-target="#pair-result"; hx-swap="innerHTML"; hx-disabled-elt="find button[type=submit]"
- L79: name="code"
- L84: name="name"
- L95: href="/invitations"

### admin.html (1 lines with action attributes)

- L4: href="{{.Path}}"

### apikeys.html (11 lines with action attributes)

- L7: data-copy="#api-key"
- L10: href="/api-keys"
- L17: hx-post="/api-keys"; hx-target="#create-result"; hx-swap="innerHTML"; hx-disabled-elt="find button[type=submit]"
- L20: name="name"
- L24: name="resource"
- L30: name="session"
- L35: name="action"
- L40: name="bound_worker"
- L49: name="expires"
- L71: hx-post="/api-keys/{{.ID}}/revoke"; hx-target="#revoke-{{.ID}}"; hx-swap="innerHTML"; hx-confirm="Revoke this API key? Tools that use it stop working at once."; hx-disabled-elt="find button"
- L82: src="/onboarding.js"

### audit.html (9 lines with action attributes)

- L3: action="/admin/audit"
- L6: name="actor"
- L10: name="action"
- L14: name="from"
- L18: name="to"
- L22: name="limit"
- L28: href="/admin/audit"
- L48: href="{{.Data.FirstURL}}"
- L49: href="{{.Data.NextURL}}"

### change_password.html (4 lines with action attributes)

- L6: action="{{.Data.Action}}"; data-json-action="{{.Data.Action}}"; data-next="/"; data-match="new_password,confirm_password"; data-request-id
- L10: name="old_password"
- L14: name="new_password"
- L18: name="confirm_password"

### debug.html (12 lines with action attributes)

- L7: action="{{.URL}}"; hx-post="{{.URL}}"; hx-target="#debug-panel"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L8: name="action"; name="lines"; data-control="debug-open"
- L14: action="{{.URL}}"; hx-post="{{.URL}}"; hx-target="#debug-panel"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L15: name="enabled"; name="lines"
- L16: name="minutes"
- L17: data-control="debug-on"
- L19: action="{{.URL}}"; hx-post="{{.URL}}"; hx-target="#debug-panel"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L20: name="enabled"; name="lines"
- L21: data-control="debug-off"
- L25: action="{{.URL}}"; hx-post="{{.URL}}"; hx-target="#debug-panel"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L26: name="action"; name="lines"
- L27: data-control="debug-refresh"

### diagnostics.html (0 lines with action attributes)

- (none)

### identity.html (21 lines with action attributes)

- L27: data-reauth="oidc"
- L32: data-reauth="local"
- L34: name="password"
- L48: data-identity-op="hub_identity_set_passphrase"
- L49: name="passphrase"
- L50: name="confirm"
- L62: data-identity-op="hub_identity_reissue"
- L63: name="passphrase"
- L64: name="validity_days"
- L66: name="serial_floor"
- L80: data-identity-op="hub_identity_rotate"
- L81: name="passphrase"
- L82: name="new_passphrase"
- L83: name="confirm"
- L84: name="word"
- L97: data-identity-op="hub_identity_change_passphrase"
- L98: name="passphrase"
- L99: name="new_passphrase"
- L100: name="confirm"
- L109: src="/static/identity.js"
- L121: data-confirm-proxy

### invitations.html (14 lines with action attributes)

- L9: data-copy="#invite-key"
- L14: data-copy="#invite-oneliner"
- L17: data-copy="#invite-file"
- L21: data-copy="#invite-compose"
- L22: href="/invitations"
- L31: hx-post="/invitations"; hx-target="#create-result"; hx-swap="innerHTML"; hx-disabled-elt="find button[type=submit]"
- L34: name="mode"
- L41: name="max_uses"
- L45: name="ttl"
- L52: name="name"
- L56: name="kind"
- L68: href="/invitations"; href="/invitations?all=1"
- L80: hx-post="/invitations/{{.ID}}/revoke"; hx-target="#revoke-{{.ID}}"; hx-swap="innerHTML"; hx-confirm="Revoke this invitation key? No further Worker can enroll with it."; hx-disabled-elt="find button"
- L91: src="/onboarding.js"

### layout.html (21 lines with action attributes)

- L5: name="viewport"
- L7: name="csrf-token"
- L8: name="htmx-config"
- L9: name="color-scheme"
- L10: href="/static/favicon.svg"
- L11: href="/static/minion.css"
- L12: src="/static/theme.js"
- L13: src="/static/htmx.min.js"
- L14: src="/static/sse.js"
- L15: src="/static/forms.js"
- L16: src="/static/shell.js"
- L31: href="/"
- L36: href="{{.Path}}"
- L38: href="{{.Path}}"
- L47: href="{{.URL}}"
- L49: href="/account"
- L53: data-json-action="/auth/logout"; data-next="/login"
- L63: href="{{.Path}}"
- L77: data-json-action="{{.URL}}"
- L86: href="/"
- L97: href="/"

### login.html (4 lines with action attributes)

- L4: action="/auth/login"; data-json-action="/auth/login"; data-next="{{.Data.Next}}"
- L8: name="username"
- L12: name="password"
- L18: href="{{.Path}}"

### minion_control.html (24 lines with action attributes)

- L25: hx-get="/access/control"; hx-select="#ctl-matrix"; hx-swap="outerHTML"; hx-trigger="control-changed from:body"
- L28: hx-get="/access/control"; hx-target="#ctl-matrix"; hx-disabled-elt="this"
- L33: hx-post="/access/control/accept"; hx-trigger="change"; hx-target="#ctl-{{$t.WorkerID}}"; hx-swap="outerHTML"; name="worker_id"; name="source_worker_id"; name="value"
- L40: action="/access/control"; data-autosubmit
- L41: name="check"
- L51: src="/static/minion-control.js"
- L56: hx-post="/access/control/user-managed"; hx-trigger="change"; hx-target="closest tr"; hx-swap="outerHTML"; name="worker_id"; name="value"
- L58: hx-post="/access/control/accept-all"; hx-trigger="change"; hx-target="closest tr"; hx-swap="outerHTML"; name="worker_id"; name="value"
- L64: hx-post="/access/control/accept"; hx-trigger="change"; hx-target="#ctl-{{$row.WorkerID}}"; hx-swap="outerHTML"; name="worker_id"; name="source_worker_id"; name="value"
- L73: hx-post="/access/control/bound-master"; hx-target="#ctl-{{$row.WorkerID}}"; hx-swap="outerHTML"
- L74: name="worker_id"
- L75: name="session_name"
- L76: name="incarnation_id"
- L77: name="if_generation"
- L78: name="value"
- L87: hx-post="/access/control/bound-master"; hx-target="#ctl-{{$row.WorkerID}}"; hx-swap="outerHTML"
- L88: name="worker_id"
- L89: name="session_name"
- L90: name="incarnation_id"
- L91: name="if_generation"
- L92: name="value"
- L100: hx-post="/access/control/master"; hx-trigger="change"; hx-target="#ctl-{{$row.WorkerID}}"; hx-swap="outerHTML"; name="worker_id"; name="session_name"; name="legacy"; name="value"
- L105: hx-post="/access/control/master"; hx-trigger="change"; hx-target="#ctl-{{$row.WorkerID}}"; hx-swap="outerHTML"; name="worker_id"; name="session_name"; name="value"
- L110: hx-post="/access/control/{{.Action}}"; hx-target="#ctl-{{index .Fields "; hx-swap="outerHTML"; name="{{$k}}"

### oidc.html (17 lines with action attributes)

- L4: name="name"
- L8: name="issuer"
- L12: name="client_id"
- L16: name="client_secret"
- L20: name="role_claim"
- L24: name="scopes"
- L28: name="ca_file"
- L31: name="tls_insecure_skip_verify"
- L34: name="enabled"
- L47: hx-post="/admin/oidc/providers"; hx-target="#save-{{.ID}}"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L48: name="id"
- L55: hx-post="/admin/oidc/providers/{{.ID}}/roles"; hx-target="#roles-{{.ID}}"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L58: name="admin_values"
- L62: name="user_values"
- L68: hx-post="/admin/oidc/providers/{{.ID}}/delete"; hx-target="#delete-{{.ID}}"; hx-swap="innerHTML"; hx-confirm="Delete this provider? People can no longer sign in with it."; hx-disabled-elt="find button"
- L78: hx-post="/admin/oidc/providers"; hx-target="#save-new"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L81: name="id"

### session.html (21 lines with action attributes)

- L9: action="{{.URL}}"; hx-post="{{.URL}}"; hx-target="#session-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; data-write
- L10: name="{{.Name}}"
- L13: name="choice"
- L18: name="choice_text"; data-draft
- L29: action="{{.Action}}"; hx-post="{{.Action}}"; hx-target="#session-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; data-write
- L30: name="{{.Name}}"
- L31: name="force"
- L52: action="{{.Data.NewURL}}"; hx-post="{{.Data.NewURL}}"; hx-target="#session-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L94: hx-sync="this:drop"
- L96: action="{{.Data.TextURL}}"; hx-post="{{.Data.TextURL}}"; hx-target="#session-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-sync="this:drop"; data-write
- L99: name="text"; data-draft
- L103: name="enter"
- L109: action="{{.Data.KeysURL}}"; hx-post="{{.Data.KeysURL}}"; hx-target="#session-result"; hx-swap="innerHTML"; hx-sync="this:drop"; data-write
- L111: name="key"
- L128: action="{{.Data.TaskURL}}"; hx-post="{{.Data.TaskURL}}"; hx-target="#session-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L131: name="task"
- L157: src="/static/terminal-input.js"
- L158: src="/static/session-ui.js"
- L159: src="/static/send-unasked.js"
- L160: src="/static/pipe-chat.js"
- L161: src="/static/frame-child.js"

### sessioncontrol.html (11 lines with action attributes)

- L2: href="/access/control"
- L7: hx-post="/admin/session-control"; hx-target="#create-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L10: name="worker"
- L18: name="session"
- L27: name="targets"
- L28: name="targets"
- L33: name="target"
- L35: name="target"
- L55: hx-post="/admin/session-control/remove"; hx-target="#remove-{{$i}}"; hx-swap="innerHTML"; hx-confirm="Remove this grant? The controller session loses access on its next cal..."; hx-disabled-elt="find button"
- L56: name="worker"
- L57: name="session"

### sessionlist.html (14 lines with action attributes)

- L7: href="/workers"
- L10: href="{{.Data.MineURL}}"
- L11: href="{{.Data.AllURL}}"
- L13: action="/sessions"
- L14: name="scope"
- L15: name="worker"
- L16: name="q"
- L17: name="status"
- L18: name="kind"
- L19: name="owner"
- L20: href="{{.Data.ClearURL}}"
- L24: sse-connect="{{.Data.LiveURL}}"; sse-swap="dashboard"; hx-swap="innerHTML"; data-live-list
- L28: src="/static/live-list.js"
- L29: src="/static/inspector.js"

### sessions.html (9 lines with action attributes)

- L12: href="{{.Link}}"; data-control="open"; data-inspect
- L18: href="/workers"
- L25: href="/sessions"
- L39: href="/workers/add"
- L42: href="{{.Data.MineURL}}"
- L43: href="{{.Data.AllURL}}"
- L47: sse-connect="{{.Data.LiveURL}}"; sse-swap="dashboard"; hx-swap="innerHTML"; data-live-list
- L51: src="/static/live-list.js"
- L52: src="/static/inspector.js"

### settings.html (8 lines with action attributes)

- L17: hx-post="/admin/settings/{{.Key}}"; hx-target="#result-{{.Key}}"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L24: name="value"
- L28: name="value"
- L29: name="secret"
- L30: name="value"
- L34: name="action"
- L35: name="action"
- L50: src="/static/settings-filter.js"

### share.html (7 lines with action attributes)

- L5: action="{{.URL}}"; hx-post="{{.URL}}"; hx-target="#share-panel"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L9: name="mode"; data-control="share-all"
- L10: name="mode"; data-control="share-select"
- L11: name="mode"; data-control="share-none"
- L15: data-share-bulk="all"; data-control="share-tick-all"; data-share-bulk="none"; data-control="share-tick-none"
- L21: name="name"
- L27: data-control="share-apply"

### updates.html (4 lines with action attributes)

- L9: href="{{.ReleaseURL}}"
- L12: action="/admin/updates/hub"; hx-post="/admin/updates/hub"; hx-target="#list-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"; hx-confirm="Update the Hub from {{.Current}} to {{.Latest}}? The Hub copies its da..."
- L13: data-control="hub-update"
- L22: href="/workers?scope=all"

### users.html (17 lines with action attributes)

- L6: href="/admin/users"
- L16: hx-post="/admin/users"; hx-target="#create-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L19: name="username"
- L23: name="email"
- L27: name="role"
- L49: href="/admin/users/{{.ID}}"
- L60: href="/admin/users"
- L75: hx-post="/admin/users/{{$id}}/role"; hx-target="#role-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L76: name="role"
- L78: name="role"
- L88: hx-post="/admin/users/{{$id}}/reset-password"; hx-target="#reset-result"; hx-swap="innerHTML"; hx-confirm="Reset the password of this account? The user is signed out everywhere ..."; hx-disabled-elt="find button"
- L89: name="username"
- L90: name="keep_api_keys"
- L100: hx-post="/admin/users/{{$id}}/act-as"; hx-target="#actas-result"; hx-swap="innerHTML"; hx-disabled-elt="find button"
- L103: name="minutes"
- L125: hx-post="/admin/users/{{$id}}/identities/{{.ID}}/detach"; hx-target="#detach-{{.ID}}"; hx-swap="innerHTML"; hx-confirm="Detach this identity? The user can no longer sign in with it."; hx-disabled-elt="find button"
- L140: hx-post="/admin/users/{{$id}}/deactivate"; hx-target="#deactivate-result"; hx-swap="innerHTML"; hx-confirm="Deactivate this account?"; hx-disabled-elt="find button"

### worker.html (10 lines with action attributes)

- L7: href="{{.Data.ListURL}}"
- L10: href="{{.Data.MineURL}}"
- L11: href="{{.Data.AllURL}}"
- L15: sse-connect="{{.Data.LiveURL}}"; sse-swap="dashboard"; hx-swap="innerHTML"; data-live-list
- L18: hx-get="{{.Data.ShareURL}}"; hx-trigger="load"; hx-swap="innerHTML"
- L28: hx-get="{{.Data.DebugURL}}"; hx-trigger="load"; hx-swap="innerHTML"
- L33: src="/static/live-list.js"
- L34: src="/static/inspector.js"
- L35: src="/static/share-bulk.js"
- L36: src="/static/send-unasked.js"

### workers.html (15 lines with action attributes)

- L6: href="/admin/identity"
- L7: href="/workers/add"
- L10: href="{{.Data.MineURL}}"
- L11: href="{{.Data.AllURL}}"
- L13: action="/workers"
- L14: name="scope"
- L15: name="worker"
- L16: name="q"
- L17: name="status"
- L18: name="kind"
- L19: name="owner"
- L20: href="{{.Data.ClearURL}}"
- L26: sse-connect="{{.Data.LiveURL}}"; sse-swap="dashboard"; hx-swap="innerHTML"; data-live-list
- L30: src="/static/live-list.js"
- L31: src="/static/inspector.js"


## Appendix B. Counts (for sanity checks)
- The counts below are the v0.12.0 state. Added since, by the access pages only: 3 GET pages (`/access`, `/access/grants`, `/access/why`), 1 POST route (`/access/grants/{action}`), 1 nav item (Access), 4 template files (`access.html`, `access_grants.html`, `access_why.html`, `_grant_row.html`), 2 static JS files (`access.js`, `access-grants.js`). Other pages added since v0.12.0 are not counted here.
- Registered GET pages: 22 (20 via `RegisterPage` + `/login` + `/account/password`); non-page GET routes: 12 (+ `/static/*`).
- POST routes behind the gate: 36 (table in 2.1). Plus auth/REST endpoints used by JS: `/auth/login`, `/auth/logout`, `/auth/reauth`, `/auth/reauth/oidc`, `/api/v1/ops/*`, `/api/v1/events`.
- Nav items: 14 (3 Workspace, 4 Access, 7 Administration); banners: 2 sources; login providers: dynamic.
- In `templates/*.html`: 65 `<form>` elements, 50 `hx-post`, 4 `hx-get` attributes (pairing requests, share, debug, control matrix; counts are attribute occurrences), 5 `data-json-action`, 13 `data-copy`, 16 `hx-confirm`; 32 template files (8 partials `_*.html`).
- Static JS: 16 files in `W/static/` (htmx.min.js, sse.js, theme.js, forms.js, shell.js, live-list.js, inspector.js, frame-child.js, terminal-input.js, session-ui.js, pipe-chat.js, send-unasked.js, share-bulk.js, settings-filter.js, minion-control.js, identity.js; plus `minion.css`, `favicon.svg`, `README.md` (not JS)) and the route-served `/onboarding.js`.
