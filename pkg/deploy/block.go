package deploy

// BlockEngine deploys images using block-level tools (dd, partclone).
// It produces bit-exact copies of disks or partitions, required when
// preserving bootloaders, partition UUIDs, or filesystem metadata exactly.
type BlockEngine struct {
	// Tool selects the block copy tool: "dd" or "partclone".
	Tool string
	// Compress enables on-the-fly gzip compression during transfer.
	Compress bool
}

// Deploy clones src block device to dst. Not yet implemented.
func (e *BlockEngine) Deploy(src, dst string) error {
	return ErrNotImplemented
}
