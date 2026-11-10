// Package web serves the embedded web UI: server-rendered pages, htmx and
// static assets (AD-21).
//
// Owner: Story 3.7.
//
// # Shape
//
// A [Server] is an [http.Handler] for everything that is not an API route. It
// owns the layout (navigation, a banner slot for Story 6.5's act-as banner and
// a flash area), the login page, the change-password page and the vendored
// static assets under /static/. Pages of other stories plug in through the
// registries in web.go ([RegisterPage], [RegisterNav], [RegisterRoute],
// [RegisterLoginProvider], [RegisterBanner]) from their own init functions,
// without editing the layout.
//
// # Rules
//
//   - Every value is rendered by html/template (AD-14). Nothing in this
//     package builds HTML by string concatenation or uses template.HTML.
//   - Pages are reachable only with a browser session. A session whose account
//     must change its password is sent to /account/password first.
//   - htmx requests carry the CSRF header through hx-headers on <body>; forms
//     that post JSON use static/forms.js, which sends the same header.
//   - Authorization is not decided here: the role filter of a page or a
//     navigation entry only hides what the caller could not use. Every action
//     is authorised again by the operation behind it (AD-10, AD-11).
package web
