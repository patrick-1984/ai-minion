// Package client is the Go client for the Hub REST API and the local control socket (AD-22).
//
// Every surface that calls an op (the CLI, `mn`, MCP) talks to a [Backend].
// [Local] is the backend over the Worker's local control socket (Story 2.8);
// [HubBackend] is the one over the Hub REST API (Story 6.9), and [Select]
// chooses between them: a configured Hub always wins and its failure is never
// answered by the local Worker.
//
// Files:
//
//   - client.go: the [Backend] interface and the errors callers test for;
//   - local.go: [Local], the backend over the local socket;
//   - hub.go: [HubBackend], POST /api/v1/ops/<name> with an API key from a
//     file, NDJSON streams, private-CA and explicit insecure TLS for this
//     client only, no redirects, no retries, bounded reads;
//   - select.go: [Select] and the "no Hub configured" hint of the local one.
//
// Owner: Story 2.8 (local socket); Story 6.9 (Hub REST).
package client
