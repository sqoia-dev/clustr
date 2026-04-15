package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/bootassets"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/pxe"
)

// BootHandler serves boot assets and dynamic iPXE scripts over HTTP.
// Boot files (vmlinuz, initramfs.img) are served from BootDir.
// iPXE chainload files (ipxe.efi, undionly.kpxe) are served from TFTPDir.
type BootHandler struct {
	// BootDir is the directory containing vmlinuz and initramfs.img.
	BootDir string
	// TFTPDir is the directory containing ipxe.efi and undionly.kpxe.
	TFTPDir string
	// ServerURL is the public URL of clonr-serverd (e.g. http://10.99.0.1:8080).
	// Used to generate the iPXE boot script.
	ServerURL string
	// DB is used to look up node state by MAC for PXE boot routing.
	// When nil the handler always returns the full boot script (safe default).
	DB *db.DB
	// MintNodeToken is called to generate a fresh node-scoped API key at PXE-serve
	// time. The returned raw key is embedded in the kernel cmdline as clonr.token.
	// When nil (e.g. in tests that don't need auth), an empty token is used.
	MintNodeToken func(nodeID string) (rawKey string, err error)
}

// ServeIPXEScript handles GET /api/v1/boot/ipxe.
//
// This is the PXE server's boot routing decision point. The DHCP handler sets
// the iPXE boot filename URL to:
//
//	http://<server>/api/v1/boot/ipxe?mac=${mac}
//
// iPXE expands ${mac} before fetching, so this handler receives the actual
// MAC address. It resolves the node state and returns one of:
//
//   - NodeStateDeployed: "#!ipxe\nexit\n" -- hands control back to BIOS/UEFI,
//     which falls through to local disk (the next boot order entry).
//
//   - All other states (Registered, Configured, ReimagePending, Failed, or
//     unknown MAC): the full clonr initramfs boot script, which causes the
//     node to run `clonr deploy --auto` and deploy or wait for assignment.
//
// For non-deployed nodes a fresh node-scoped API key is minted and embedded in
// the kernel cmdline as clonr.token=<key> so the deploy agent can authenticate
// against /images/{id} and /images/{id}/blob without an admin key.
//
// This is the canonical pattern used by xCAT, Warewulf, and Cobbler: the PXE
// server is the source of truth for what each node boots. No BMC SetNextBoot
// calls are needed for normal boot routing. PXE must be first in the BIOS boot
// order, set once during rack/stack and never changed.
func (h *BootHandler) ServeIPXEScript(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")

	// If we have a MAC and a DB, look up the node state and route the boot.
	if mac != "" && h.DB != nil {
		nodeCfg, err := h.DB.GetNodeConfigByMAC(r.Context(), mac)
		if err != nil && !errors.Is(err, api.ErrNotFound) {
			// DB error: log and fall through to the safe default (full boot script).
			// A transient DB error must never cause a node to boot from disk when
			// it should be reimaged -- fail open toward clonr deploy, not disk boot.
			log.Error().Err(err).Str("mac", mac).Msg("boot: lookup node by MAC")
		} else if err == nil {
			state := nodeCfg.State()
			log.Info().
				Str("mac", mac).
				Str("hostname", nodeCfg.Hostname).
				Str("state", string(state)).
				Msg("boot: PXE routing decision")

			if state == api.NodeStateDeployed {
				// Node is healthy and deployed -- tell iPXE to exit and boot from disk.
				script, genErr := pxe.GenerateDiskBootScript(nodeCfg.Hostname)
				if genErr != nil {
					log.Error().Err(genErr).Str("mac", mac).Msg("boot: generate disk boot script")
					http.Error(w, "failed to generate boot script", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(script)
				return
			}

			// Non-deployed node: mint a fresh node-scoped token for this deploy run.
			token := h.mintToken(r, nodeCfg.ID)
			script, genErr := pxe.GenerateBootScript(h.ServerURL, "clonr-node-"+token)
			if genErr != nil {
				log.Error().Err(genErr).Str("mac", mac).Msg("boot: generate boot script")
				http.Error(w, "failed to generate boot script", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(script)
			return
		}
		// Unknown MAC (ErrNotFound): node will self-register on boot. Serve script
		// without a token — the node has no ID yet and will register first, then
		// receive a token on its next PXE boot (triggered by the reimage flow).
	} else if mac == "" {
		log.Warn().Msg("boot: iPXE script requested without ?mac= -- returning full boot script")
	}

	// Default: return the full clonr initramfs boot script with no token.
	// Covers: unknown MACs and requests without a MAC parameter.
	script, err := pxe.GenerateBootScript(h.ServerURL, "")
	if err != nil {
		log.Error().Err(err).Msg("boot: generate iPXE script")
		http.Error(w, "failed to generate boot script", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(script)
}

// mintToken calls MintNodeToken if configured and logs failures. Returns the raw
// key (without the clonr-node- prefix) on success, or "" on failure/unconfigured.
// The caller prepends "clonr-node-" before embedding in the cmdline.
func (h *BootHandler) mintToken(r *http.Request, nodeID string) string {
	if h.MintNodeToken == nil || nodeID == "" {
		return ""
	}
	raw, err := h.MintNodeToken(nodeID)
	if err != nil {
		log.Error().Err(err).Str("node_id", nodeID).Msg("boot: failed to mint node-scoped token")
		return ""
	}
	return raw
}

// ServeVMLinuz handles GET /api/v1/boot/vmlinuz.
func (h *BootHandler) ServeVMLinuz(w http.ResponseWriter, r *http.Request) {
	h.serveFile(w, r, filepath.Join(h.BootDir, "vmlinuz"), "application/octet-stream")
}

// ServeInitramfs handles GET /api/v1/boot/initramfs.img.
func (h *BootHandler) ServeInitramfs(w http.ResponseWriter, r *http.Request) {
	h.serveFile(w, r, filepath.Join(h.BootDir, "initramfs.img"), "application/octet-stream")
}

// ServeIPXEEFI handles GET /api/v1/boot/ipxe.efi.
//
// Serves the embedded iPXE UEFI binary (x86-64) to OVMF/UEFI HTTP boot clients.
// This is the chainloader that UEFI HTTP boot downloads before executing the
// clonr boot script. It is intentionally served from an embedded binary so that
// the route works out-of-the-box without any on-disk file placement — the UEFI
// HTTP boot client hits 404 and loops forever if this route returns an error.
//
// The embedded binary takes precedence. A future operator override could be
// added by checking for an on-disk file in TFTPDir first, but for now the
// embedded binary is canonical and sufficient for x86-64 UEFI HTTP boot.
func (h *BootHandler) ServeIPXEEFI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/efi")
	http.ServeContent(w, r, "ipxe.efi", time.Time{}, bytes.NewReader(bootassets.IPXEEFI))
}

// ServeUndionlyKPXE handles GET /api/v1/boot/undionly.kpxe.
func (h *BootHandler) ServeUndionlyKPXE(w http.ResponseWriter, r *http.Request) {
	h.serveFile(w, r, filepath.Join(h.TFTPDir, "undionly.kpxe"), "application/octet-stream")
}

func (h *BootHandler) serveFile(w http.ResponseWriter, r *http.Request, path, contentType string) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Warn().Str("path", path).Msg("boot: file not found")
			writeError(w, api.ErrNotFound)
			return
		}
		log.Error().Err(err).Str("path", path).Msg("boot: open file")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}
