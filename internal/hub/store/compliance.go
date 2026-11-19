package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CompliancePolicyRow is the one row of compliance_policy.
type CompliancePolicyRow struct {
	Version        int64
	Level          string
	Input          bool
	TrafficPayload bool
	DestLabel      string
	DestID         string
	TrafficID      string
	UpdatedAt      int64 // unix milliseconds
	UpdatedBy      string
}

// GetCompliancePolicy returns the stored policy; ok is false before the first
// one was written.
func (s *Store) GetCompliancePolicy(ctx context.Context) (CompliancePolicyRow, bool, error) {
	var r CompliancePolicyRow
	var in, tp int
	err := s.db.QueryRowContext(ctx,
		`SELECT version, level, input, traffic_payload, dest_label, dest_id, traffic_id, updated_at, updated_by FROM compliance_policy WHERE id = 1`).
		Scan(&r.Version, &r.Level, &in, &tp, &r.DestLabel, &r.DestID, &r.TrafficID, &r.UpdatedAt, &r.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return CompliancePolicyRow{}, false, nil
	}
	if err != nil {
		return CompliancePolicyRow{}, false, fmt.Errorf("store: read compliance policy: %w", err)
	}
	r.Input, r.TrafficPayload = in != 0, tp != 0
	return r, true, nil
}

// GetCompliancePolicyTx is GetCompliancePolicy inside a transaction.
func GetCompliancePolicyTx(ctx context.Context, tx *sql.Tx) (CompliancePolicyRow, bool, error) {
	var r CompliancePolicyRow
	var in, tp int
	err := tx.QueryRowContext(ctx,
		`SELECT version, level, input, traffic_payload, dest_label, dest_id, traffic_id, updated_at, updated_by FROM compliance_policy WHERE id = 1`).
		Scan(&r.Version, &r.Level, &in, &tp, &r.DestLabel, &r.DestID, &r.TrafficID, &r.UpdatedAt, &r.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return CompliancePolicyRow{}, false, nil
	}
	if err != nil {
		return CompliancePolicyRow{}, false, fmt.Errorf("store: read compliance policy: %w", err)
	}
	r.Input, r.TrafficPayload = in != 0, tp != 0
	return r, true, nil
}

// PutCompliancePolicyTx writes the policy row inside the caller's transaction.
func PutCompliancePolicyTx(ctx context.Context, ex Execer, r CompliancePolicyRow) error {
	_, err := ex.ExecContext(ctx,
		`INSERT INTO compliance_policy (id, version, level, input, traffic_payload, dest_label, dest_id, traffic_id, updated_at, updated_by)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET version = excluded.version, level = excluded.level, input = excluded.input,
		   traffic_payload = excluded.traffic_payload, dest_label = excluded.dest_label, dest_id = excluded.dest_id, traffic_id = excluded.traffic_id,
		   updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		r.Version, r.Level, boolToInt(r.Input), boolToInt(r.TrafficPayload), r.DestLabel, r.DestID, r.TrafficID, r.UpdatedAt, r.UpdatedBy)
	if err != nil {
		return fmt.Errorf("store: write compliance policy: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
