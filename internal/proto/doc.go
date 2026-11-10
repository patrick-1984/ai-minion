// Package proto holds the wire frames, protocol version and the shared error
// codes (AD-2, AD-3, AD-4, AD-18). It is part of the shared kernel and imports
// nothing from the rest of the module.
//
// The package is split by concern:
//
//   - frame.go: the length-prefixed JSON frame codec (WriteFrame, ReadFrame)
//     and its size limits.
//   - types.go: frame type names, stream kind prefixes and the Error value
//     carried by wire error frames and API error envelopes.
//   - version.go: the protocol version, ranges and negotiation (AD-4).
//   - codes.go: the one list of error and reason codes, their descriptions and
//     their HTTP status mapping. Later stories only append to it.
//
// Owner: Story 1.2.
package proto
