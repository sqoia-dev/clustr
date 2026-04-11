// Package image manages clonr image metadata and lifecycle.
package image

import "time"

// Status represents the current state of an image in the store.
type Status string

const (
	StatusPending  Status = "pending"
	StatusReady    Status = "ready"
	StatusFailed   Status = "failed"
	StatusArchived Status = "archived"
)

// Image is the metadata record for a stored node image.
type Image struct {
	ID          string
	Name        string
	Description string
	Status      Status
	SizeBytes   int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Tags        map[string]string
}
