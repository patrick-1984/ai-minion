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
// title forgets the name. Titles whose pane generation has no journal entry
// left are pruned.
func (s *Store) RecordHarnessTitle(ctx context.Context, grant string, e HarnessEvent, title string) error {
	if title != ops.SanitizeHarnessName(title) {
		return errors.New("unsanitized harness title")
	}
	return s.writeHarnessTitle(ctx, grant, e, func(tx *sql.Tx) error {
		var err error
		if title == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM harness_titles WHERE session=? AND pane=? AND created=? AND harness=? AND native=?`,
				e.Session, e.Pane, e.Created, e.Harness, e.NativeID)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO harness_titles(session,pane,created,harness,native,title,checked,at) VALUES(?,?,?,?,?,?,'',?)
 ON CONFLICT(session,pane,created,harness,native) DO UPDATE SET title=excluded.title,checked='',at=excluded.at`,
				e.Session, e.Pane, e.Created, e.Harness, e.NativeID, title, e.At.UnixNano())
		}
		return err
	})
}

// MarkHarnessTitleChecked records the digest (64 hex digits) of a name that
// the exporter could not confirm, keeping the name known before.
func (s *Store) MarkHarnessTitleChecked(ctx context.Context, grant string, e HarnessEvent, digest string) error {
	if !harnessDigest(digest) {
		return errors.New("invalid harness title digest")
	}
	return s.writeHarnessTitle(ctx, grant, e, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO harness_titles(session,pane,created,harness,native,title,checked,at) VALUES(?,?,?,?,?,'',?,?)
 ON CONFLICT(session,pane,created,harness,native) DO UPDATE SET checked=excluded.checked`,
			e.Session, e.Pane, e.Created, e.Harness, e.NativeID, digest, e.At.UnixNano())
		return err
	})
}

// writeHarnessTitle runs write under the monitoring grant, then prunes the
// names whose pane generation has no journal entry left.
func (s *Store) writeHarnessTitle(ctx context.Context, grant string, e HarnessEvent, write func(*sql.Tx) error) error {
	if grant == "" || !ops.ValidSessionName(e.Session) || len(e.Pane) > 16 || e.Pane == "" || e.Created <= 0 ||
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
	if err := write(tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_titles WHERE NOT EXISTS(SELECT 1 FROM harness_events e WHERE e.session=harness_titles.session
 AND e.pane=harness_titles.pane AND e.created=harness_titles.created AND e.harness=harness_titles.harness AND e.native=harness_titles.native)`); err != nil {
		return err
	}
	return tx.Commit()
}

// StoredHarnessTitle returns the kept name of one native session in one pane
// generation ("" for none) and the digest of the last name that could not be
// confirmed ("" for none).
func (s *Store) StoredHarnessTitle(ctx context.Context, session, pane, harness string, created int64, native string) (string, string, error) {
	var title, checked string
	err := s.db.QueryRowContext(ctx, `SELECT title,checked FROM harness_titles WHERE session=? AND pane=? AND created=? AND harness=? AND native=?`,
		session, pane, created, harness, native).Scan(&title, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return title, checked, err
}

// HarnessTitle returns the name of the native session that last reported in
// one pane generation, with the process id that reported it. It is empty when
// that session has no name, the process has ended, or monitoring is off. The
// caller confirms that the pane still runs that process.
func (s *Store) HarnessTitle(ctx context.Context, session, pane, harness string, created int64, now time.Time) (string, int, error) {
	var title, birth string
	var pid int
	err := s.db.QueryRowContext(ctx, `WITH latest AS (
      SELECT native,pid,birth FROM harness_events WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND at<=?5
      ORDER BY at DESC,seq DESC LIMIT 1
    )
    SELECT t.title,l.pid,l.birth FROM latest l JOIN harness_titles t
      ON t.session=?1 AND t.pane=?2 AND t.created=?3 AND t.harness=?4 AND t.native=l.native
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
