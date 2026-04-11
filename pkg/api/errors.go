package api

import "errors"

// Sentinel errors returned by store operations. HTTP handlers map these to status codes.
var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrBadRequest = errors.New("bad request")
)
