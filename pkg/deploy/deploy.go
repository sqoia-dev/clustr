// Package deploy provides deployment engines for pushing images to target nodes.
// Supported engines: rsync (filesystem-level), dd/partclone (block-level).
package deploy

import "errors"

// ErrNotImplemented is returned by engine stubs pending full implementation.
var ErrNotImplemented = errors.New("not implemented")

// Engine is the interface all deployment backends must satisfy.
type Engine interface {
	// Deploy pushes the image at src to the target node/disk at dst.
	Deploy(src, dst string) error
}
