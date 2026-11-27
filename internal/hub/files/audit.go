package files

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Audit outcomes of the admission; the end of a transfer is audited with its
// record state (delivered, partial, failed_not_sent, served, unknown).
const (
	AuditAdmitted = "admitted"
	AuditDenied   = "denied"
)

// AuditAction is the audit action of a transfer of direction: file.pull or
// file.push.
func AuditAction(direction string) string { return "file." + direction }

// Detail returns the audit detail of a record in the closed grammar
// (audit.ValidDetail): transfer id, direction, counts, outcome, reason,
// manifest digest and protected=off. It never names a path, a name or a
// token; a value that does not fit the grammar is left out.
func Detail(r store.FileTransferRow, outcome string) string {
	var b strings.Builder
	add := func(k, v string) {
		if v == "" || !audit.ValidDetail(k+"="+v) {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k + "=" + v)
	}
	add("transfer", r.ID)
	add("dir", r.Direction)
	if r.Kind != "" {
		add("kind", r.Kind)
	}
	if outcome != AuditAdmitted && outcome != AuditDenied {
		add("files", strconv.FormatInt(r.Files, 10))
		add("bytes", strconv.FormatInt(r.Bytes, 10))
		add("wire", strconv.FormatInt(r.WireBytes, 10))
	}
	add("outcome", outcome)
	if proto.Code(r.Reason).Known() {
		add("reason", r.Reason)
	}
	if proto.ValidManifestDigest(r.ManifestDigest) {
		add("manifest", r.ManifestDigest)
	}
	if r.Protected == "off" {
		add("protected", "off")
	}
	return b.String()
}

// record writes one audit event of a record; a failure is logged.
func (s *Service) record(ctx context.Context, r store.FileTransferRow, outcome string) {
	if s.cfg.Audit == nil {
		return
	}
	ev := audit.Event{
		Actor:    r.Principal,
		ActingAs: r.ActingAs,
		Action:   AuditAction(r.Direction),
		Target:   r.TargetWorkerID,
		Outcome:  outcome,
		Detail:   Detail(r, outcome),
	}
	if r.TargetSession != "" {
		ev.Target = r.TargetWorkerID + ":" + r.TargetSession
	}
	if r.SourceWorkerID != "" {
		ev.Source = r.SourceWorkerID
		if r.SourceSession != "" {
			ev.Source += ":" + r.SourceSession
		}
	}
	if len(r.GrantIDs) > 0 {
		ev.GrantID = strings.Join(r.GrantIDs, ",")
	} else if r.AcceptanceID != "" {
		ev.GrantID = r.AcceptanceID
	}
	if ev.Actor == "" {
		ev.Actor = "system"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.cfg.Audit.Record(ctx, ev); err != nil {
		s.log.Error("files: audit write failed", "action", ev.Action, "transfer_id", r.ID, "error", err.Error())
	}
}

// auditEnd records the end of a transfer with its record state.
func (s *Service) auditEnd(ctx context.Context, r store.FileTransferRow) {
	if ops.ValidFileState(r.State) {
		s.record(ctx, r, r.State)
	}
}
