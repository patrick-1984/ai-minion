package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// RecordHarnessTitle keeps the user-given name of one native harness session
// in one pane generation. The binding fields of e are checked as for
// [Store.RecordHarnessEvent]; Kind, Entity and Registry are ignored. An empty
// title forgets the name. Writes are ordered by e.At, the time the exporter
// observed the name: an older observation whose callback finishes last does
// not overwrite a newer one (a clear included). Titles whose pane generation
// has no journal entry left are pruned.
func (s *Store) RecordHarnessTitle(ctx context.Context, grant string, e HarnessEvent, title string) error {
	if title != ops.SanitizeHarnessName(title) {
		return errors.New("unsanitized harness title")
	}
	return s.writeHarnessTitle(ctx, grant, e, title, "", 0)
}

// MarkHarnessTitleChecked records that the name the status line reports now
// could not be confirmed as given by the user: the session shows no name, and
// digest (64 hex digits, it covers the name and the transcript file) with
// scanned (the transcript offset searched so far) let the exporter search only
// what the transcript gains later. Ordered by e.At like
// [Store.RecordHarnessTitle].
func (s *Store) MarkHarnessTitleChecked(ctx context.Context, grant string, e HarnessEvent, digest string, scanned int64) error {
	if !harnessDigest(digest) || scanned < 0 {
		return errors.New("invalid harness title digest")
	}
	return s.writeHarnessTitle(ctx, grant, e, "", digest, scanned)
}

// writeHarnessTitle writes one row under the monitoring grant, unless a newer
// observation was written already, then prunes the
// names whose pane generation has no journal entry left.
func (s *Store) writeHarnessTitle(ctx context.Context, grant string, e HarnessEvent, title, checked string, scanned int64) error {
	if e.At.IsZero() || grant == "" || !ops.ValidSessionName(e.Session) || len(e.Pane) > 16 || e.Pane == "" || e.Created <= 0 ||
		(e.Harness != "codex" && e.Harness != "claude") || !harnessDigest(e.NativeID) {
		return errors.New("invalid harness title binding")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var granted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hub_enrollment WHERE id=1 AND usage_policy=1 AND worker_id<>'' AND owner<>''
 AND hex(hub_pub)||':'||worker_id||':'||owner=?)`, grant).Scan(&granted); err != nil {
		return err
	}
	if !granted {
		return errors.New("harness monitoring grant expired")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO harness_titles(session,pane,created,harness,native,title,checked,scanned,at) VALUES(?,?,?,?,?,?,?,?,?)
 ON CONFLICT(session,pane,created,harness,native) DO UPDATE SET title=excluded.title,checked=excluded.checked,scanned=excluded.scanned,at=excluded.at
 WHERE excluded.at>=harness_titles.at`,
		e.Session, e.Pane, e.Created, e.Harness, e.NativeID, title, checked, scanned, e.At.UnixNano()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_titles WHERE NOT EXISTS(SELECT 1 FROM harness_events e WHERE e.session=harness_titles.session
 AND e.pane=harness_titles.pane AND e.created=harness_titles.created AND e.harness=harness_titles.harness AND e.native=harness_titles.native)`); err != nil {
		return err
	}
	return tx.Commit()
}

// HarnessTitleCheck is what the store keeps of one native session's name: the
// confirmed name ("" for none), and for a name not confirmed (yet) the digest
// that identifies it and the transcript offset searched so far.
type HarnessTitleCheck struct {
	Title   string
	Checked string
	Scanned int64
}

// StoredHarnessTitle returns what is kept of the name of one native session in
// one pane generation (the zero value for nothing).
func (s *Store) StoredHarnessTitle(ctx context.Context, session, pane, harness string, created int64, native string) (HarnessTitleCheck, error) {
	var c HarnessTitleCheck
	err := s.db.QueryRowContext(ctx, `SELECT title,checked,scanned FROM harness_titles WHERE session=? AND pane=? AND created=? AND harness=? AND native=?`,
		session, pane, created, harness, native).Scan(&c.Title, &c.Checked, &c.Scanned)
	if errors.Is(err, sql.ErrNoRows) {
		return HarnessTitleCheck{}, nil
	}
	return c, err
}

// HarnessTitle returns the name of the native session the pane's harness
// process runs now in one pane generation, with that process id. The session
// is chosen like [Store.NativeHarnessState] chooses it: the latest session
// start of the process that reported last fences off the conversations before
// it, so a delayed hook of the conversation before /clear cannot bring back its
// name. It is empty when that session has no name, the process has ended, or
// monitoring is off. The caller confirms that the pane still runs that process.
func (s *Store) HarnessTitle(ctx context.Context, session, pane, harness string, created int64, now time.Time) (string, int, error) {
	var title, birth string
	var pid int
	err := s.db.QueryRowContext(ctx, `WITH latest AS (
      SELECT native,pid,birth FROM harness_events WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND at<=?5
      ORDER BY at DESC,seq DESC LIMIT 1
    ), generation AS (
      SELECT e.native FROM harness_events e JOIN latest l USING(pid,birth)
      WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND kind='session_start' AND at<=?5
      ORDER BY at DESC,seq DESC LIMIT 1
    )
    SELECT t.title,l.pid,l.birth FROM latest l LEFT JOIN generation g ON TRUE JOIN harness_titles t
      ON t.session=?1 AND t.pane=?2 AND t.created=?3 AND t.harness=?4 AND t.native=coalesce(g.native,l.native)
      WHERE t.title<>'' AND EXISTS(SELECT 1 FROM hub_enrollment WHERE id=1 AND usage_policy=1)`,
		session, pane, created, harness, now.UnixNano()).Scan(&title, &pid, &birth)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if pid <= 0 || agentdetect.ProcessBirth(pid) != birth {
		return "", 0, nil
	}
	return title, pid, nil
}
