# Vendored assets

These files are embedded into the Hub binary (`go:embed`); nothing is fetched
at run time and there is no npm build (AD-21).

| File | Source | SHA-256 |
| --- | --- | --- |
| `htmx.min.js` | htmx 2.0.11, `dist/htmx.min.js` (BSD 0-Clause) | `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717` |
| `sse.js` | htmx-ext-sse 2.2.2, `sse.js` (BSD 0-Clause) | `83eca6fa0611fe2b0bf1700b424b88b5eced38ef448ef9760a2ea08fbc875611` |

`minion.css` (the tokens, themes and components of the workbench shell), `favicon.svg` (a plain hand-written SVG icon, linked
from the layout) and the scripts below are written for Minion. `theme.js` runs in the head and applies the theme choice
before the first paint; `shell.js` is the theme picker, the mobile Menu and the unsent-draft guard; `live-list.js` keeps
focus, scroll and a polite summary across the live update of a list page; `session-ui.js` is the interface of the session
workspace (dock tabs, right pane, "Send anyway" confirmation). `inspector.js` is the controller of the session inspector on the control center, Workers and Sessions pages (a contextual right pane with the compact session view in a same-origin frame, a bounded frame cache, one guarded read of the sign-in, messages that carry flags and counts only); `frame-child.js` is the frame's half, loaded only by the compact view (`?embed=1`). `inspector.js` also handles the whole-row click of the session and Workers tables (Story 5.7). The design is in `docs/design/workbench/`. `forms.js` is the only
script of the shell: it submits forms that carry `data-json-action` as JSON
with the CSRF header (login, logout, change password). A form with
`data-request-id` also gets a fresh UUIDv7 `request_id` in its body, which every
write operation requires (change password).

`terminal-input.js` is loaded only by the session
view (`/s/<worker>/<session>`, Story 5.5): it refreshes the screen panel on
Hub events, shows the connection state, pauses the screen, settles the answer of every write by the form that issued it,
reports a write whose answer never arrived as unknown (never sending it again), sends the Text composer on a plain Enter (once, as the Send button does; Shift+Enter is a line break), runs the explicit keyboard box of the Keys tab (one named key per press, only while the box has the focus), and defines the terminal colour classes in
a constructed style sheet (the Content-Security-Policy forbids inline styles). It is tested against a
DOM stub (`testdata/session_live_test.js`); the shell scripts are tested in `testdata/workbench_ui_test.js`.
