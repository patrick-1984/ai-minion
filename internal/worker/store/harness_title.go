package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// HarnessTitleObservation is what [Store.ObserveHarnessTitle] tells the
// exporter: whether the name it observed must be confirmed in the transcript,
// and for that search the digest of the name last searched for and the
// transcript offset searched so far.
type HarnessTitleObservation struct {
	Verify  bool
	Checked string
	Scanned int64
}

// ObserveHarnessTitle records that the status line reported want (sanitized,
// "" for none) for one native session in one pane generation at e.At, the
// time the exporter observed it. The binding fields of e are checked as for
// [Store.RecordHarnessEvent]; Kind, Entity and Registry are ignored. Every
// observation advances the session's observation fence, clears and repeats
// included, unless a newer one was recorded already (then nothing changes):
//
//   - "" clears the name;
//   - the name the transcript confirmed last is shown again at once;
//   - any other name clears the name shown until the transcript confirms it
//     ([Store.ConfirmHarnessTitle]); Verify is then true.
//
// The search state (checked, scanned) is kept across observations.
func (s *Store) ObserveHarnessTitle(ctx context.Context, grant string, e HarnessEvent, want string) (HarnessTitleObservation, error) {
	if want != ops.SanitizeHarnessName(want) {
		return HarnessTitleObservation{}, errors.New("unsanitized harness title")
	}
	var out HarnessTitleObservation
	err := s.harnessTitleTx(ctx, grant, e, func(tx *sql.Tx) error {
		var verified, checked string
		var scanned, at int64
		err := tx.QueryRowContext(ctx, `SELECT verified,checked,scanned,at FROM harness_titles WHERE session=? AND pane=? AND created=? AND harness=? AND native=?`,
			e.Session, e.Pane, e.Created, e.Harness, e.NativeID).Scan(&verified, &checked, &scanned, &at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case at > e.At.UnixNano():
			return nil // a newer observation was recorded: this one is stale
		}
		title := ""
		if want != "" && want == verified {
			title = want
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO harness_titles(session,pane,created,harness,native,title,at) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(session,pane,created,harness,native) DO UPDATE SET title=excluded.title,at=excluded.at WHERE excluded.at>=harness_titles.at`,
			e.Session, e.Pane, e.Created, e.Harness, e.NativeID, title, e.At.UnixNano()); err != nil {
			return err
		}
		out = HarnessTitleObservation{Verify: want != "" && title == "", Checked: checked, Scanned: scanned}
		return nil
	})
	return out, err
}

// ConfirmHarnessTitle records that the transcript confirmed title, the name
// observed at e.At, as given by the user. It is shown only while that
// observation is still the session's latest one: a newer observation (another
// name, a clear) recorded meanwhile wins. The confirmation itself is kept, so
// a later repeat of the name is shown without a new search.
func (s *Store) ConfirmHarnessTitle(ctx context.Context, grant string, e HarnessEvent, title string) error {
	if title == "" || title != ops.SanitizeHarnessName(title) {
		return errors.New("unsanitized harness title")
	}
	return s.harnessTitleTx(ctx, grant, e, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE harness_titles SET verified=?6, title=CASE WHEN at=?7 THEN ?6 ELSE title END
 WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND native=?5 AND at>=?7`,
			e.Session, e.Pane, e.Created, e.Harness, e.NativeID, title, e.At.UnixNano())
		return err
	})
}

// MarkHarnessTitleChecked records how far the transcript was searched for the
// name observed at e.At without confirming it: digest (64 hex digits, it
// covers the name and the transcript file) and scanned (the offset searched
// so far) let the exporter search only what the transcript gains later. The
// search state of a newer observation is not replaced, except to advance the
// same search further.
func (s *Store) MarkHarnessTitleChecked(ctx context.Context, grant string, e HarnessEvent, digest string, scanned int64) error {
	if !harnessDigest(digest) || scanned < 0 {
		return errors.New("invalid harness title digest")
	}
	return s.harnessTitleTx(ctx, grant, e, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE harness_titles SET checked=?6, scanned=?7
 WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND native=?5 AND (at=?8 OR (checked=?6 AND scanned<?7))`,
			e.Session, e.Pane, e.Created, e.Harness, e.NativeID, digest, scanned, e.At.UnixNano())
		return err
	})
}

// harnessTitleTx runs write under the monitoring grant in one transaction,
// then prunes the names whose pane generation has no journal entry left.
func (s *Store) harnessTitleTx(ctx context.Context, grant string, e HarnessEvent, write func(*sql.Tx) error) error {
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
	if err := write(tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_titles WHERE NOT EXISTS(SELECT 1 FROM harness_events e WHERE e.session=harness_titles.session
 AND e.pane=harness_titles.pane AND e.created=harness_titles.created AND e.harness=harness_titles.harness AND e.native=harness_titles.native)`); err != nil {
		return err
	}
	return tx.Commit()
}

// HarnessTitle returns the name of the native session the pane's harness
// process runs now in one pane generation, with that process id. The session
// is chosen like [Store.NativeHarnessState] chooses it: the latest session
// start of the process that reported last fences off the conversations before
// it, so a delayed hook of the conversation before /clear cannot bring back its
// name. A conversation that ended (its session_end after its latest start)
// never has a name: when /clear ended the old conversation and the start of
// the new one was missed, the conversation that reported last is used, and a
// delayed callback of the ended one shows nothing. It is empty when that
// session has no name, the process has ended, or monitoring is off. The caller
// confirms that the pane still runs that process.
func (s *Store) HarnessTitle(ctx context.Context, session, pane, harness string, created int64, now time.Time) (string, int, error) {
	var title, birth string
	var pid int
	err := s.db.QueryRowContext(ctx, `WITH events AS (
      SELECT native,kind,pid,birth,at,seq FROM harness_events WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND at<=?5
    ), latest AS (
      SELECT native,pid,birth FROM events ORDER BY at DESC,seq DESC LIMIT 1
    ), ended AS (
      SELECT DISTINCT x.native FROM events x WHERE x.kind='session_end' AND NOT EXISTS(SELECT 1 FROM events y
        WHERE y.native=x.native AND y.kind='session_start' AND (y.at>x.at OR (y.at=x.at AND y.seq>x.seq)))
    ), generation AS (
      SELECT e.native FROM events e JOIN latest l USING(pid,birth) WHERE e.kind='session_start'
      ORDER BY e.at DESC,e.seq DESC LIMIT 1
    ), chosen AS (
      SELECT CASE WHEN g.native IS NOT NULL AND g.native NOT IN (SELECT native FROM ended) THEN g.native
        WHEN l.native NOT IN (SELECT native FROM ended) THEN l.native END AS native
      FROM latest l LEFT JOIN generation g ON TRUE
    )
    SELECT t.title,l.pid,l.birth FROM latest l JOIN chosen c JOIN harness_titles t
      ON t.session=?1 AND t.pane=?2 AND t.created=?3 AND t.harness=?4 AND t.native=c.native
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
