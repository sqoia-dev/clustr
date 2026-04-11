// Package db provides the SQLite persistence layer for clonr.
package db

import (
	"database/sql"
	"fmt"
)

// DB wraps a sql.DB with clonr-specific helpers.
type DB struct {
	sql *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies all
// pending migrations.
func Open(path string) (*DB, error) {
	// sqlite3 driver is registered by the importing binary via a blank import.
	// We do not import it here to keep this package driver-agnostic and testable.
	sqlDB, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}
	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("db: migrate: %w", err)
	}
	return db, nil
}

// Close closes the underlying database connection.
func (db *DB) Close() error {
	return db.sql.Close()
}
