// Package pipe implements the pipe broker and its store port for talking to
// agent sessions (AD-23, R26, R28). It is part of the shared kernel: the Hub
// is the broker in Hub mode and the Worker is the broker in Solo mode, both
// through this one package, so the limits are identical everywhere.
//
// The broker owns three things: the send limits (limits.go: 8 KiB per
// message, 1 message per second with a burst of 5, 120 per hour, per peer),
// the task budget (budget.go: 200 messages per peer per 24 hours by default)
// and the idempotent commit of turns (UUIDv7 turn_id, turn.ack "stored").
// A message the limits do not allow is rejected with an *Error and is never
// queued or delayed. The broker never sends automatic replies, never
// forwards, never delays and never replays: an ack means "stored".
//
// Persistence is behind the Store port (store.go); the Hub and Worker stores
// implement it with SQLite (Stories 7.3 and 7.4) and the tests use an
// in-memory implementation. Time comes from an injected clock.
//
// A kernel package imports no other kernel package, so this package defines
// its own Error and Code. The code strings are the snake_case codes declared
// in internal/proto/codes.go; callers convert an *Error with
// proto.NewError(proto.Code(e.Code), e.Message).
//
// Owner: Story 7.1
package pipe
