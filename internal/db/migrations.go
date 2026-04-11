package db

import "fmt"

// migrate applies the current schema to the database. It is idempotent due to
// IF NOT EXISTS clauses. A future version will use numbered migrations with a
// schema_version table for proper up/down migration support.
func (db *DB) migrate() error {
	if _, err := db.sql.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
