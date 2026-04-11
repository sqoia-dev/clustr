package deploy

// RsyncEngine deploys images using rsync for filesystem-level replication.
// It preserves permissions, extended attributes, and ACLs, making it suitable
// for OS cloning where bit-exact block copies are not required.
type RsyncEngine struct {
	// BandwidthKBps limits rsync bandwidth in KB/s. 0 means unlimited.
	BandwidthKBps int
	// ExcludePatterns is a list of rsync --exclude patterns (e.g. "/proc/*").
	ExcludePatterns []string
}

// Deploy runs rsync from src to dst. Not yet implemented.
func (e *RsyncEngine) Deploy(src, dst string) error {
	return ErrNotImplemented
}
