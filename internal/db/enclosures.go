// Package db — enclosures.go implements persistence for multi-node chassis
// enclosures (Sprint 31, #231, Path A).
//
// THREAD-SAFETY: All methods are safe for concurrent use. The underlying
// sql.DB serialises writers (MaxOpenConns=1, WAL mode). No in-process map
// is held by this file, so no additional mutex is required.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sqoia-dev/clustr/internal/enclosures"
	"github.com/sqoia-dev/clustr/pkg/api"
)

// ─── enclosures CRUD ──────────────────────────────────────────────────────────

// CreateEnclosure inserts a new enclosure row.
// Validates: type_id exists in catalog; chassis fits in rack; no rack-position overlap.
func (db *DB) CreateEnclosure(ctx context.Context, e api.Enclosure) error {
	// Validate type_id.
	et, ok := enclosures.Get(e.TypeID)
	if !ok {
		return fmt.Errorf("%w: unknown enclosure type %q", api.ErrBadRequest, e.TypeID)
	}

	// Validate rack exists and chassis fits within it.
	rack, err := db.GetRack(ctx, e.RackID)
	if err != nil {
		return err
	}
	if err := validateEnclosureFitsInRack(e.RackSlotU, et.HeightU, rack.HeightU); err != nil {
		return err
	}

	// Check for overlap with other enclosures or rack-direct nodes.
	if err := db.checkEnclosureOverlap(ctx, e.RackID, "", e.RackSlotU, et.HeightU); err != nil {
		return err
	}

	_, dbErr := db.sql.ExecContext(ctx, `
		INSERT INTO enclosures (id, rack_id, rack_slot_u, height_u, type_id, label, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, e.ID, e.RackID, e.RackSlotU, et.HeightU, e.TypeID, toNullString(e.Label),
		e.CreatedAt.Unix(), e.UpdatedAt.Unix())
	if dbErr != nil {
		return fmt.Errorf("db: create enclosure: %w", dbErr)
	}
	return nil
}

// GetEnclosure returns an enclosure by ID including its slot occupancy.
func (db *DB) GetEnclosure(ctx context.Context, id string) (api.Enclosure, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, rack_id, rack_slot_u, height_u, type_id, label, created_at, updated_at
		FROM enclosures WHERE id = ?
	`, id)
	enc, err := scanEnclosure(row)
	if err != nil {
		return api.Enclosure{}, err
	}
	slots, err := db.ListSlotsByEnclosure(ctx, id)
	if err != nil {
		return api.Enclosure{}, err
	}
	enc.Slots = slots
	return enc, nil
}

// ListEnclosuresByRack returns all enclosures in the given rack, each with slot occupancy.
//
// Implementation note: SQLite with MaxOpenConns=1 serialises all statements on a
// single connection. Calling ListSlotsByEnclosure (which opens two queries of its
// own) while the outer enclosure cursor is still open triggers "cannot start a
// transaction within a transaction" / context-cancelled panics. The fix is the
// standard two-pass pattern: materialise all enclosure rows first, explicitly close
// the cursor, then populate slot occupancy in a second pass.
func (db *DB) ListEnclosuresByRack(ctx context.Context, rackID string) ([]api.Enclosure, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, rack_id, rack_slot_u, height_u, type_id, label, created_at, updated_at
		FROM enclosures WHERE rack_id = ?
		ORDER BY rack_slot_u ASC
	`, rackID)
	if err != nil {
		return nil, fmt.Errorf("db: list enclosures by rack: %w", err)
	}

	// Pass 1: materialise all enclosure rows before opening any nested queries.
	var out []api.Enclosure
	for rows.Next() {
		enc, sErr := scanEnclosure(rows)
		if sErr != nil {
			rows.Close()
			return nil, sErr
		}
		out = append(out, enc)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("db: list enclosures by rack scan: %w", err)
	}
	rows.Close() // explicit close BEFORE the per-enclosure slot queries

	// Pass 2: populate slot occupancy now that the outer cursor is fully closed.
	for i := range out {
		slots, sErr := db.ListSlotsByEnclosure(ctx, out[i].ID)
		if sErr != nil {
			return nil, sErr
		}
		out[i].Slots = slots
	}
	return out, nil
}

// UpdateEnclosure updates the label and/or rack_slot_u of an enclosure.
// type_id and height_u are immutable after creation (changing them would require
// validating existing slot occupancy against the new slot count, which is a
// future feature).
func (db *DB) UpdateEnclosure(ctx context.Context, id, label string, rackSlotU int) error {
	enc, err := db.GetEnclosure(ctx, id)
	if err != nil {
		return err
	}

	newLabel := enc.Label
	if label != "" {
		newLabel = label
	}
	newRackSlotU := enc.RackSlotU
	if rackSlotU > 0 {
		// Validate new position.
		rack, rErr := db.GetRack(ctx, enc.RackID)
		if rErr != nil {
			return rErr
		}
		et, _ := enclosures.Get(enc.TypeID)
		if err := validateEnclosureFitsInRack(rackSlotU, et.HeightU, rack.HeightU); err != nil {
			return err
		}
		if err := db.checkEnclosureOverlap(ctx, enc.RackID, id, rackSlotU, et.HeightU); err != nil {
			return err
		}
		newRackSlotU = rackSlotU
	}

	res, dbErr := db.sql.ExecContext(ctx, `
		UPDATE enclosures SET label = ?, rack_slot_u = ?, updated_at = ? WHERE id = ?
	`, toNullString(newLabel), newRackSlotU, time.Now().Unix(), id)
	if dbErr != nil {
		return fmt.Errorf("db: update enclosure: %w", dbErr)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return api.ErrNotFound
	}
	return nil
}

// DeleteEnclosure removes an enclosure. The ON DELETE CASCADE on
// node_rack_position.enclosure_id clears all slot occupancy rows automatically.
func (db *DB) DeleteEnclosure(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM enclosures WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete enclosure: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return api.ErrNotFound
	}
	return nil
}

// ─── slot occupancy ───────────────────────────────────────────────────────────

// SetSlotOccupancy places a node in a specific enclosure slot.
// Validates: enclosure exists; slot_index is in [1, catalog.SlotCount]; slot not already occupied.
// Atomically clears any prior placement of the same node (rack-direct or enclosure).
func (db *DB) SetSlotOccupancy(ctx context.Context, enclosureID string, slotIndex int, nodeID string) error {
	enc, err := db.GetEnclosure(ctx, enclosureID)
	if err != nil {
		return err
	}
	et, ok := enclosures.Get(enc.TypeID)
	if !ok {
		return fmt.Errorf("db: enclosure type %q not in catalog (data inconsistency)", enc.TypeID)
	}
	if slotIndex < 1 || slotIndex > et.SlotCount {
		return fmt.Errorf("%w: slot_index %d out of range [1, %d] for enclosure type %q",
			api.ErrBadRequest, slotIndex, et.SlotCount, enc.TypeID)
	}

	// Check the slot is not already occupied by another node.
	var occupant sql.NullString
	err = db.sql.QueryRowContext(ctx, `
		SELECT node_id FROM node_rack_position
		WHERE enclosure_id = ? AND slot_index = ?
	`, enclosureID, slotIndex).Scan(&occupant)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("db: check slot occupancy: %w", err)
	}
	if occupant.Valid && occupant.String != nodeID {
		return fmt.Errorf("%w: slot %d of enclosure %s is occupied by node %s",
			api.ErrConflict, slotIndex, enclosureID, occupant.String)
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Remove any existing placement for this node.
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_rack_position WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("db: clear prior placement: %w", err)
	}

	// Insert the enclosure-resident row (rack_id and slot_u are NULL here).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_rack_position (node_id, rack_id, slot_u, height_u, enclosure_id, slot_index)
		VALUES (?, NULL, NULL, NULL, ?, ?)
	`, nodeID, enclosureID, slotIndex); err != nil {
		return fmt.Errorf("db: set slot occupancy: %w", err)
	}

	return tx.Commit()
}

// ClearSlotOccupancy removes the node currently in a given slot.
// Returns ErrNotFound if the slot is already empty.
func (db *DB) ClearSlotOccupancy(ctx context.Context, enclosureID string, slotIndex int) error {
	res, err := db.sql.ExecContext(ctx, `
		DELETE FROM node_rack_position
		WHERE enclosure_id = ? AND slot_index = ?
	`, enclosureID, slotIndex)
	if err != nil {
		return fmt.Errorf("db: clear slot occupancy: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return api.ErrNotFound
	}
	return nil
}

// ListSlotsByEnclosure returns the full slot occupancy for an enclosure.
// Unoccupied slots are included (NodeID = "").
func (db *DB) ListSlotsByEnclosure(ctx context.Context, enclosureID string) ([]api.EnclosureSlot, error) {
	enc, err := db.getEnclosureRaw(ctx, enclosureID)
	if err != nil {
		return nil, err
	}
	et, ok := enclosures.Get(enc.TypeID)
	if !ok {
		return nil, fmt.Errorf("db: enclosure type %q not in catalog", enc.TypeID)
	}

	// Fetch all occupied slots.
	rows, err := db.sql.QueryContext(ctx, `
		SELECT slot_index, node_id FROM node_rack_position
		WHERE enclosure_id = ?
		ORDER BY slot_index ASC
	`, enclosureID)
	if err != nil {
		return nil, fmt.Errorf("db: list slots by enclosure: %w", err)
	}
	defer rows.Close()

	occupied := make(map[int]string)
	for rows.Next() {
		var idx int
		var nid string
		if err := rows.Scan(&idx, &nid); err != nil {
			return nil, fmt.Errorf("db: scan slot occupancy: %w", err)
		}
		occupied[idx] = nid
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Build the full ordered slot list (1..SlotCount).
	slots := make([]api.EnclosureSlot, et.SlotCount)
	for i := range slots {
		slots[i] = api.EnclosureSlot{
			SlotIndex: i + 1,
			NodeID:    occupied[i+1],
		}
	}
	return slots, nil
}

// ─── selector support ─────────────────────────────────────────────────────────

// ListNodeIDsByEnclosureLabels returns all node IDs for nodes placed in
// enclosures whose label matches any of the given labels.
// Used by the selector to resolve --chassis.
func (db *DB) ListNodeIDsByEnclosureLabels(ctx context.Context, labels []string) ([]string, error) {
	if len(labels) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(labels))
	args := make([]interface{}, len(labels))
	for i, l := range labels {
		placeholders[i] = "?"
		args[i] = l
	}

	query := `
		SELECT nrp.node_id
		FROM node_rack_position nrp
		JOIN enclosures e ON e.id = nrp.enclosure_id
		WHERE e.label IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY nrp.node_id ASC
	`
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list nodes by enclosure labels: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan node id by enclosure: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ─── unified placement ────────────────────────────────────────────────────────

// SetNodePlacementRack atomically moves a node to a rack-direct slot.
// Clears any existing placement (rack-direct or enclosure-resident) first.
func (db *DB) SetNodePlacementRack(ctx context.Context, nodeID, rackID string, slotU, heightU int) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM node_rack_position WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("db: clear prior placement: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_rack_position (node_id, rack_id, slot_u, height_u, enclosure_id, slot_index)
		VALUES (?, ?, ?, ?, NULL, NULL)
	`, nodeID, rackID, slotU, heightU); err != nil {
		return fmt.Errorf("db: set rack placement: %w", err)
	}

	return tx.Commit()
}

// SetNodePlacementEnclosure atomically moves a node to an enclosure slot.
// Validates slot bounds and slot-occupancy conflict inside the transaction.
func (db *DB) SetNodePlacementEnclosure(ctx context.Context, nodeID, enclosureID string, slotIndex int) error {
	// Pre-validate before taking the transaction to give a clean error message.
	enc, err := db.GetEnclosure(ctx, enclosureID)
	if err != nil {
		return err
	}
	et, ok := enclosures.Get(enc.TypeID)
	if !ok {
		return fmt.Errorf("db: enclosure type %q not in catalog", enc.TypeID)
	}
	if slotIndex < 1 || slotIndex > et.SlotCount {
		return fmt.Errorf("%w: slot_index %d out of range [1, %d]",
			api.ErrBadRequest, slotIndex, et.SlotCount)
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Conflict check: is another node already in that slot?
	var occupant sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT node_id FROM node_rack_position
		WHERE enclosure_id = ? AND slot_index = ?
	`, enclosureID, slotIndex).Scan(&occupant)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("db: check slot occupancy: %w", err)
	}
	if occupant.Valid && occupant.String != nodeID {
		return fmt.Errorf("%w: slot %d of enclosure %s is already occupied",
			api.ErrConflict, slotIndex, enclosureID)
	}

	// Clear prior placement.
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_rack_position WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("db: clear prior placement: %w", err)
	}

	// Insert enclosure-resident row.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_rack_position (node_id, rack_id, slot_u, height_u, enclosure_id, slot_index)
		VALUES (?, NULL, NULL, NULL, ?, ?)
	`, nodeID, enclosureID, slotIndex); err != nil {
		return fmt.Errorf("db: set enclosure placement: %w", err)
	}

	return tx.Commit()
}

// ClearNodePlacement removes a node from any placement (rack or enclosure).
// Returns ErrNotFound if the node has no current placement.
func (db *DB) ClearNodePlacement(ctx context.Context, nodeID string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM node_rack_position WHERE node_id = ?`, nodeID)
	if err != nil {
		return fmt.Errorf("db: clear node placement: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return api.ErrNotFound
	}
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// validateEnclosureFitsInRack returns an error if [slotU, slotU+heightU-1] overflows the rack.
func validateEnclosureFitsInRack(rackSlotU, heightU, rackHeightU int) error {
	if rackSlotU < 1 {
		return fmt.Errorf("%w: rack_slot_u must be >= 1", api.ErrBadRequest)
	}
	top := rackSlotU + heightU - 1
	if top > rackHeightU {
		return fmt.Errorf("%w: enclosure extends to U%d but rack only has %dU",
			api.ErrBadRequest, top, rackHeightU)
	}
	return nil
}

// checkEnclosureOverlap returns an error if [rackSlotU, rackSlotU+heightU-1] overlaps
// any existing enclosure (excluding excludeID) or any rack-direct node in the rack.
func (db *DB) checkEnclosureOverlap(ctx context.Context, rackID, excludeID string, rackSlotU, heightU int) error {
	top := rackSlotU + heightU - 1

	// Check against other enclosures.
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, rack_slot_u, height_u FROM enclosures WHERE rack_id = ?
	`, rackID)
	if err != nil {
		return fmt.Errorf("db: overlap check (enclosures): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eid string
		var eu, eh int
		if err := rows.Scan(&eid, &eu, &eh); err != nil {
			return fmt.Errorf("db: scan enclosure for overlap: %w", err)
		}
		if eid == excludeID {
			continue
		}
		eTop := eu + eh - 1
		if rackSlotU <= eTop && eu <= top {
			return fmt.Errorf("%w: chassis would overlap enclosure %s at U%d–U%d",
				api.ErrConflict, eid, eu, eTop)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Check against rack-direct nodes.
	nodeRows, err := db.sql.QueryContext(ctx, `
		SELECT slot_u, height_u FROM node_rack_position WHERE rack_id = ?
	`, rackID)
	if err != nil {
		return fmt.Errorf("db: overlap check (nodes): %w", err)
	}
	defer nodeRows.Close()
	for nodeRows.Next() {
		var nu, nh int
		if err := nodeRows.Scan(&nu, &nh); err != nil {
			return fmt.Errorf("db: scan node for overlap: %w", err)
		}
		nTop := nu + nh - 1
		if rackSlotU <= nTop && nu <= top {
			return fmt.Errorf("%w: chassis would overlap rack-direct node at U%d–U%d",
				api.ErrConflict, nu, nTop)
		}
	}
	return nodeRows.Err()
}

// getEnclosureRaw fetches an enclosure row without populating Slots.
// Used internally to avoid the recursive call in ListSlotsByEnclosure.
func (db *DB) getEnclosureRaw(ctx context.Context, id string) (api.Enclosure, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, rack_id, rack_slot_u, height_u, type_id, label, created_at, updated_at
		FROM enclosures WHERE id = ?
	`, id)
	return scanEnclosure(row)
}

type enclosureScanner interface {
	Scan(dest ...any) error
}

func scanEnclosure(s enclosureScanner) (api.Enclosure, error) {
	var (
		e             api.Enclosure
		label         sql.NullString
		createdAtUnix int64
		updatedAtUnix int64
	)
	err := s.Scan(&e.ID, &e.RackID, &e.RackSlotU, &e.HeightU, &e.TypeID, &label,
		&createdAtUnix, &updatedAtUnix)
	if err == sql.ErrNoRows {
		return api.Enclosure{}, api.ErrNotFound
	}
	if err != nil {
		return api.Enclosure{}, fmt.Errorf("db: scan enclosure: %w", err)
	}
	e.Label = label.String
	e.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	e.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
	return e, nil
}

// toNullString converts an empty Go string to sql.NullString{Valid:false}.
// Used for the enclosures.label nullable column.
func toNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
