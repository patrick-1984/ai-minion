// Package audit records and reads the Hub audit log (AD-20).
//
// Security-relevant actions are written as one audit_events row each: actor,
// acting_as, action, target, outcome and a time, with no content. The package
// enforces "no content" structurally: every field is at most [MaxFieldLen]
// characters of printable UTF-8, so a message body, a screen or a secret
// cannot be passed in by mistake. Rejection errors name the field, never the
// value.
//
// Files:
//
//   - audit.go: [Log] with [Log.Record], [Log.RecordTx], [Log.List] and the
//     ops adapter [Log.ListOp].
//
// The package contains no SQL. It talks to a [Repository], which
// internal/hub/store implements (store/audit.go owns the table). Other
// domain packages call [Log.RecordTx] inside their own store.Tx so the audit
// row commits with the change it describes. Who may read the log is decided
// by the authorisation layer from the accepted action of the audit_list op
// (manage, admin only), not here.
//
// Owner: Story 3.3.
package audit
