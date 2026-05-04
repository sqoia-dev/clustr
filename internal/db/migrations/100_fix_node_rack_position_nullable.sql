-- 100_fix_node_rack_position_nullable.sql (#246 v0.1.6)
--
-- Migration 096 recreated node_rack_position with `rack_id NOT NULL`. Migration
-- 098 (enclosure support) added enclosure_id/slot_index columns and the XOR
-- trigger, but did not relax the NOT NULL on rack_id, slot_u, and height_u.
--
-- Enclosure-resident nodes have rack_id = NULL, slot_u = NULL, height_u = NULL
-- (by XOR design) — so the NOT NULL constraints are incorrect and cause all
-- SetSlotOccupancy / SetNodePlacementEnclosure calls to fail with
-- "NOT NULL constraint failed: node_rack_position.rack_id".
--
-- Fix: recreate the table with the XOR-correct nullability.
-- The XOR triggers are dropped and recreated identically (SQLite does not allow
-- ALTER TABLE to modify constraints; full table recreation is required).

PRAGMA foreign_keys = OFF;

CREATE TABLE node_rack_position_v2 (
    -- Identity
    node_id     TEXT    PRIMARY KEY
                        REFERENCES node_configs(id) ON DELETE CASCADE,

    -- Rack-direct placement (set when enclosure_id IS NULL)
    rack_id     TEXT    REFERENCES racks(id) ON DELETE CASCADE,
    slot_u      INTEGER,
    height_u    INTEGER,

    -- Enclosure-resident placement (set when rack_id IS NULL)
    enclosure_id TEXT   REFERENCES enclosures(id) ON DELETE CASCADE,
    slot_index   INTEGER
);

INSERT INTO node_rack_position_v2
    SELECT node_id, rack_id, slot_u, height_u, enclosure_id, slot_index
    FROM node_rack_position;

-- Drop old triggers before dropping the table they reference.
DROP TRIGGER IF EXISTS node_rack_position_xor_parent_insert;
DROP TRIGGER IF EXISTS node_rack_position_xor_parent_update;

DROP TABLE node_rack_position;
ALTER TABLE node_rack_position_v2 RENAME TO node_rack_position;

-- Restore indices.
CREATE INDEX IF NOT EXISTS idx_node_rack_position_rack
    ON node_rack_position(rack_id);

CREATE INDEX IF NOT EXISTS idx_node_rack_position_enclosure
    ON node_rack_position(enclosure_id)
    WHERE enclosure_id IS NOT NULL;

-- Restore XOR trigger: INSERT (identical logic from 098_enclosures.sql).
CREATE TRIGGER node_rack_position_xor_parent_insert
BEFORE INSERT ON node_rack_position
BEGIN
    SELECT CASE
        WHEN (NEW.rack_id IS NOT NULL AND NEW.enclosure_id IS NOT NULL)
          OR (NEW.rack_id IS NULL     AND NEW.enclosure_id IS NULL)
        THEN RAISE(ABORT, 'node_rack_position: exactly one of rack_id/enclosure_id required')
    END;
    SELECT CASE
        WHEN NEW.enclosure_id IS NOT NULL AND NEW.slot_index IS NULL
        THEN RAISE(ABORT, 'node_rack_position: slot_index required when enclosure_id is set')
    END;
    SELECT CASE
        WHEN NEW.rack_id IS NOT NULL AND NEW.slot_u IS NULL
        THEN RAISE(ABORT, 'node_rack_position: slot_u required when rack_id is set')
    END;
END;

-- Restore XOR trigger: UPDATE (identical logic from 098_enclosures.sql).
CREATE TRIGGER node_rack_position_xor_parent_update
BEFORE UPDATE ON node_rack_position
BEGIN
    SELECT CASE
        WHEN (NEW.rack_id IS NOT NULL AND NEW.enclosure_id IS NOT NULL)
          OR (NEW.rack_id IS NULL     AND NEW.enclosure_id IS NULL)
        THEN RAISE(ABORT, 'node_rack_position: exactly one of rack_id/enclosure_id required')
    END;
    SELECT CASE
        WHEN NEW.enclosure_id IS NOT NULL AND NEW.slot_index IS NULL
        THEN RAISE(ABORT, 'node_rack_position: slot_index required when enclosure_id is set')
    END;
    SELECT CASE
        WHEN NEW.rack_id IS NOT NULL AND NEW.slot_u IS NULL
        THEN RAISE(ABORT, 'node_rack_position: slot_u required when rack_id is set')
    END;
END;

PRAGMA foreign_keys = ON;
