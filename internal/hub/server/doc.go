// Package server wires the Minion Hub together: module kernel, TLS modes, public URL and health endpoints (AD-18).
//
// The module kernel (kernel.go, Story 3.1) is how Hub features plug in
// without editing a shared file. A feature adds one file, mod_<name>.go, whose
// init function calls [RegisterModule] with a name, an order and an init
// function. The kernel then
//
//  1. initialises the modules in ascending order (ties broken by name);
//  2. lets each module [Provide] a typed service that later modules
//     [Require]. A module must declare each service it provides with
//     [DeclaresService] when it registers: [Provide] refuses an undeclared
//     type, and registering a second declaration of the same type fails.
//     Startup therefore fails with an English error that names both the
//     requiring and the providing module when a service is missing, for
//     example when the provider has a higher order than the requirer;
//  3. runs the start hooks in module order and the stop hooks in reverse.
//
// The registry is an explicit value ([Registry]) so that tests build their
// own; [RegisterModule] uses the process-wide [DefaultRegistry].
//
// The HTTP server (server.go, Story 3.2) is one Hub run: [Server.Run] checks
// the data dir ([CheckDataDir]), loads or generates the TLS certificate
// (tls.go: ECDSA P-256 self-signed in <data>/tls, minimum TLS 1.3, SIGHUP
// reload), starts the modules and serves. The "core" module provides the
// services *Config, *Network (client IP, scheme and public URL behind trusted
// proxies, publicurl.go), *Health (/healthz and /readyz, health.go) and the
// root *http.ServeMux; the "store" module (mod_store.go) opens the database.
//
// Owner: Stories 3.1 and 3.2.
package server
