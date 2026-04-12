package pxe

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pin/tftp/v3"
	"github.com/rs/zerolog/log"
)

// TFTPServer serves PXE boot files (ipxe.efi, undionly.kpxe) via TFTP.
type TFTPServer struct {
	tftpDir string
	server  *tftp.Server
}

// newTFTPServer creates a TFTPServer backed by the given directory.
func newTFTPServer(tftpDir string) *TFTPServer {
	return &TFTPServer{tftpDir: tftpDir}
}

// Start begins listening for TFTP requests on :69 (standard TFTP port).
// This requires CAP_NET_BIND_SERVICE or root privileges.
func (t *TFTPServer) Start(ctx context.Context) error {
	srv := tftp.NewServer(t.readHandler, nil)
	// SetTimeout requires a time.Duration value. The unit is nanoseconds, so
	// passing a bare integer (e.g. 5) sets a 5-nanosecond timeout which causes
	// immediate timeouts. Always use time.Second multiplier.
	srv.SetTimeout(5 * time.Second)
	// EnableSinglePort makes the server use port 69 for all DATA/ACK traffic
	// (instead of random ephemeral ports). This is required for UEFI/OVMF
	// clients which send ACKs back to the original port 69, not to the
	// ephemeral port used in standard TFTP. Without this, OVMF UEFI PXE
	// receives the first DATA block but its ACK never reaches the server
	// because the server is listening on a random port, causing i/o timeout.
	srv.EnableSinglePort()
	// Disable MTU-based blocksize negotiation — let the library honor the
	// client-requested blksize (OVMF requests 1468). Forcing 512 blocks causes
	// OVMF to abort with ERROR code=8 on first RRQ.
	srv.SetBlockSizeNegotiation(false)
	t.server = srv

	log.Info().Str("dir", t.tftpDir).Msg("TFTP server listening on :69")

	go func() {
		<-ctx.Done()
		srv.Shutdown()
	}()

	if err := srv.ListenAndServe(":69"); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("pxe/tftp: serve: %w", err)
	}
	return nil
}

// readHandler handles an incoming TFTP read request for a boot file.
func (t *TFTPServer) readHandler(filename string, rf io.ReaderFrom) error {
	// Sanitize: strip directory traversal and absolute paths.
	clean := filepath.Base(filepath.Clean(filename))
	fullPath := filepath.Join(t.tftpDir, clean)

	f, err := os.Open(fullPath)
	if err != nil {
		log.Warn().Str("file", clean).Err(err).Msg("TFTP: file not found")
		return fmt.Errorf("file not found: %s", clean)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err == nil {
		// Provide the file size so the client can display progress.
		type sizer interface {
			SetSize(int64)
		}
		if s, ok := rf.(sizer); ok {
			s.SetSize(stat.Size())
		}
	}

	n, err := rf.ReadFrom(f)
	if err != nil {
		log.Error().Str("file", clean).Err(err).Msg("TFTP: send error")
		return err
	}

	log.Info().Str("file", clean).Int64("bytes", n).Msg("TFTP: sent")
	return nil
}
