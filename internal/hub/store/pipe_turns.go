package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/pipe"
)

// The turns a Worker stores answer deliveries and stay within quotas
// (0016_pipe_turn_quota.sql). This file is the part of the pipe store that
// decides that, in the transaction of the insert: a turn is checked against the
// delivery it answers and against the stored volume of its Worker, its owner
// and the Hub before anything is written, so that a refusal never costs a
// write.

// TurnRowOverhead is what a stored turn costs beyond its text: the row, its
// indexes and the position bookkeeping. It makes a flood of tiny turns count.
const TurnRowOverhead = 256

// TurnCost is the quota cost of a turn with a text of textBytes.
func TurnCost(textBytes int) int64 { return int64(textBytes) + TurnRowOverhead }

// TurnLimits are the storage quotas of the turns, as the sum of [TurnCost] and
// the number of turns held in the database (retention gives them back). A zero
// member is no limit.
type TurnLimits struct {
	WorkerCost, OwnerCost, GlobalCost    int64
	WorkerTurns, OwnerTurns, GlobalTurns int64
}

// TurnStatus is the answer of the store to a turn.
type TurnStatus int

// The statuses of [PipeStore.CheckTurn] and [PipeStore.PutDeliveredTurn].
const (
	// TurnStored: the turn is stored now (Put) or would be accepted (Check).
	TurnStored TurnStatus = iota
	// TurnDuplicate: the same turn (turn_id, Worker, session and delivery) is
	// stored already; the answer is the ack that was lost.
	TurnDuplicate
	// TurnIDTaken: the turn_id belongs to another turn.
	TurnIDTaken
	// TurnNoDelivery: the delivery does not exist for this Worker and session.
	TurnNoDelivery
	// TurnAnswered: another turn answered the delivery already.
	TurnAnswered
	// TurnQuotaWorker, TurnQuotaOwner and TurnQuotaGlobal: a storage quota is
	// used up; the turn is not stored and may be sent again later.
	TurnQuotaWorker
	TurnQuotaOwner
	TurnQuotaGlobal
)

// DeliveredTurn is a turn of an authenticated Worker that says which delivery
// it answers.
type DeliveredTurn struct {
	Turn pipe.Turn
	// MessageID is the id of the pipe_messages row the turn answers.
	MessageID int64
	// OwnerID is the owner of the Worker (for the owner quota); empty counts
	// for no owner.
	OwnerID string
	Limits  TurnLimits
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkTurn decides everything except the quotas.
func checkTurn(ctx context.Context, q rowQuerier, peer pipe.Peer, turnID string, messageID int64) (TurnStatus, error) {
	var (
		worker, session string
		msg             sql.NullInt64
	)
	err := q.QueryRowContext(ctx, `SELECT worker_id, session_name, message_id FROM pipe_turns WHERE turn_id = ?`, turnID).
		Scan(&worker, &session, &msg)
	switch {
	case err == nil:
		if worker == peer.WorkerID && session == peer.SessionName && msg.Valid && msg.Int64 == messageID {
			return TurnDuplicate, nil
		}
		return TurnIDTaken, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("store: read pipe turn: %w", err)
	}
	var one int
	err = q.QueryRowContext(ctx, `SELECT 1 FROM pipe_messages WHERE id = ? AND worker_id = ? AND session_name = ?`,
		messageID, peer.WorkerID, peer.SessionName).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TurnNoDelivery, nil
	case err != nil:
		return 0, fmt.Errorf("store: read pipe message: %w", err)
	}
	err = q.QueryRowContext(ctx, `SELECT 1 FROM pipe_turns WHERE message_id = ?`, messageID).Scan(&one)
	switch {
	case err == nil:
		return TurnAnswered, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("store: read pipe turn: %w", err)
	}
	return TurnStored, nil
}

// CheckTurn tells, without writing, what [PipeStore.PutDeliveredTurn] would do
// with a turn of peer that answers messageID, quotas aside. The Hub asks it
// first so that a refused or repeated turn costs no write and no quota.
func (p *PipeStore) CheckTurn(ctx context.Context, peer pipe.Peer, turnID string, messageID int64) (TurnStatus, error) {
	return checkTurn(ctx, p.s.db, peer, turnID, messageID)
}

// PutDeliveredTurn stores the turn when it answers a delivery of its Worker
// and the quotas have room, in one transaction: the check and the insert
// cannot be separated by another writer. A turn already stored under the same
// turn_id for the same delivery is TurnDuplicate (nothing changes).
func (p *PipeStore) PutDeliveredTurn(ctx context.Context, in DeliveredTurn) (TurnStatus, error) {
	t := in.Turn
	cost := TurnCost(len(t.Text))
	status := TurnStored
	err := p.s.Tx(ctx, func(tx *sql.Tx) error {
		st, err := checkTurn(ctx, tx, t.Peer, t.TurnID, in.MessageID)
		if err != nil {
			return err
		}
		if st != TurnStored {
			status = st
			return nil
		}
		type scope struct {
			name, key string
			cost, n   int64
			st        TurnStatus
		}
		for _, sc := range []scope{
			{"worker", t.Peer.WorkerID, in.Limits.WorkerCost, in.Limits.WorkerTurns, TurnQuotaWorker},
			{"owner", in.OwnerID, in.Limits.OwnerCost, in.Limits.OwnerTurns, TurnQuotaOwner},
			{"global", "", in.Limits.GlobalCost, in.Limits.GlobalTurns, TurnQuotaGlobal},
		} {
			if sc.name == "owner" && sc.key == "" {
				continue
			}
			if sc.cost <= 0 && sc.n <= 0 {
				continue
			}
			var turns, used int64
			err := tx.QueryRowContext(ctx, `SELECT turns, cost FROM pipe_turn_usage WHERE scope = ? AND key = ?`, sc.name, sc.key).Scan(&turns, &used)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: read pipe turn usage: %w", err)
			}
			if (sc.cost > 0 && used+cost > sc.cost) || (sc.n > 0 && turns+1 > sc.n) {
				status = sc.st
				return nil
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pipe_turns (turn_id, worker_id, session_name, text, at, message_id, owner_id, cost)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			t.TurnID, t.Peer.WorkerID, t.Peer.SessionName, t.Text, t.At.UnixMilli(), in.MessageID, in.OwnerID, cost); err != nil {
			return fmt.Errorf("store: put pipe turn: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return status, nil
}

// TurnUsage is the stored volume of one scope.
type TurnUsage struct {
	Turns, Cost int64
}

// TurnUsage returns the stored turns of a scope: "worker" (key the Worker id),
// "owner" (key the owner id) or "global" (empty key).
func (p *PipeStore) TurnUsage(ctx context.Context, scope, key string) (TurnUsage, error) {
	var u TurnUsage
	err := p.s.db.QueryRowContext(ctx, `SELECT turns, cost FROM pipe_turn_usage WHERE scope = ? AND key = ?`, scope, key).Scan(&u.Turns, &u.Cost)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TurnUsage{}, fmt.Errorf("store: read pipe turn usage: %w", err)
	}
	return u, nil
}

// Pressure is how much of the database and of its write-ahead log is in use
// now, which is not the same as how large their files are.
type Pressure struct {
	DBBytes, WALBytes int64
}

// walFrameHeader is the size of the header that SQLite writes in front of every
// page of the WAL.
const walFrameHeader = 24

// Pressure measures the capacity in use, so that a gate built on it recovers
// when the data is deleted and the log is checkpointed:
//
//   - DBBytes is the pages in use, (page_count - freelist_count) * page_size.
//     The free pages are reusable capacity: page_count alone only ever grows,
//     the file does not shrink without a vacuum, and a gate on it would stay
//     closed for good after one burst.
//   - WALBytes is the log that is not yet in the database. The WAL file keeps
//     its size after a checkpoint (it is rewritten from the start, not
//     truncated), so its size says nothing about what is outstanding. When the
//     file is not larger than walLimit it is what it is; when it is larger, a
//     PASSIVE checkpoint runs (it never blocks a reader or a writer and does the
//     work that is possible) and the frames it could not write back are counted:
//     a reader that pins the log keeps them outstanding, and that is real
//     pressure, while a log that is fully written back is none however large
//     the file is. The next write restarts the log from its beginning and the
//     journal_size_limit of the connection shrinks the file.
//
// It never vacuums and takes no lock of its own; the work of the checkpoint is
// bounded by the log.
func (p *PipeStore) Pressure(ctx context.Context, walLimit int64) (Pressure, error) {
	var pages, free, size int64
	for q, dst := range map[string]*int64{`PRAGMA page_count`: &pages, `PRAGMA freelist_count`: &free, `PRAGMA page_size`: &size} {
		if err := p.s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return Pressure{}, fmt.Errorf("store: read %s: %w", q, err)
		}
	}
	out := Pressure{DBBytes: max(0, pages-free) * size}
	fi, err := os.Stat(filepath.Join(p.s.dir, DBFile+"-wal"))
	if err != nil {
		return out, nil
	}
	out.WALBytes = fi.Size()
	if out.WALBytes > walLimit {
		var busy, frames, done int64
		if err := p.s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &frames, &done); err == nil && frames >= 0 && done >= 0 {
			out.WALBytes = max(0, frames-done) * (size + walFrameHeader)
		}
	}
	return out, nil
}
