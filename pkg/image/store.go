package image

import "errors"

// ErrNotFound is returned when an image ID does not exist in the store.
var ErrNotFound = errors.New("image not found")

// Store defines the interface for image persistence. The default
// implementation uses SQLite via internal/db; alternative backends
// (e.g. remote registry) can satisfy this interface.
type Store interface {
	Save(img *Image) error
	Get(id string) (*Image, error)
	List() ([]*Image, error)
	Delete(id string) error
}
