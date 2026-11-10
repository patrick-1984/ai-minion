// Package mcp serves the operation catalog as MCP tools over stdio (AD-22). It
// is the only package that imports the MCP SDK (AD-1).
//
// One tool exists per Worker op of the catalog: the name is the op name, the
// description is the catalog summary, and the input schema is derived from the
// op's argument type (schema.go). Write ops are tools only when the server is
// built with AllowWrites (`minion mcp --allow-writes`, MINION_MCP_ALLOW_WRITES).
// A tool call runs through a client.Backend (the local control socket in Solo
// mode) and returns the op result as structured content; terminal screens were
// already sanitised by the Worker (AD-14). Pipe turns of a peer that the Trust
// policy does not trust are withheld from the model with a notice (R29).
//
// Owner: Story 2.10.
package mcp
