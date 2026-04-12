package pxe

import (
	"fmt"
	"text/template"
	"bytes"
)

// iPXE boot script template.
// The ${mac} variable is expanded by iPXE itself at runtime.
//
// On UEFI systems, iPXE requires the initrd to be loaded with --name and then
// referenced by that name in the kernel command line via initrd=<name>. Without
// the --name form, the initrd is loaded into memory but the kernel never receives
// a reference to it, causing "VFS: Unable to mount root fs on unknown-block(0,0)".
//
// The kernel line must include initrd=initramfs.img so the Linux boot protocol
// handler in iPXE knows to pass the named image as the initrd to the kernel.
//
// This applies to both UEFI (ipxe.efi) and BIOS (undionly.kpxe) clients because
// the named-initrd form is the only reliably portable syntax across iPXE versions.
const bootScriptTemplate = `#!ipxe
set server-url {{.ServerURL}}
kernel ${server-url}/api/v1/boot/vmlinuz initrd=initramfs.img clonr.server=${server-url} clonr.mac=${mac} console=ttyS0,115200n8
initrd --name initramfs.img ${server-url}/api/v1/boot/initramfs.img
boot
`

var bootTmpl = template.Must(template.New("boot").Parse(bootScriptTemplate))

// bootScriptData holds template vars for the iPXE boot script.
type bootScriptData struct {
	ServerURL string
}

// GenerateBootScript renders the iPXE boot script for the given server URL.
// The MAC is left as an iPXE variable (${mac}) so iPXE fills it at runtime.
func GenerateBootScript(serverURL string) ([]byte, error) {
	data := bootScriptData{ServerURL: serverURL}
	var buf bytes.Buffer
	if err := bootTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("pxe/boot: render boot script: %w", err)
	}
	return buf.Bytes(), nil
}

// diskBootScriptTemplate is the iPXE response returned for nodes in NodeStateDeployed.
//
// iPXE's "exit" command surrenders control back to the BIOS/UEFI firmware, which
// then falls through to the next entry in the boot order — local disk. This
// requires PXE to be first in the BIOS boot order (set once during rack/stack)
// and is the canonical PXE-server-as-source-of-truth pattern (xCAT, Warewulf,
// Cobbler all work this way).
//
// The hostname comment is templated in so operators can confirm the correct node
// is receiving the disk-boot response in packet captures or iPXE serial output.
const diskBootScriptTemplate = `#!ipxe
echo Node {{.Hostname}} is deployed -- booting from local disk
exit
`

var diskBootTmpl = template.Must(template.New("diskboot").Parse(diskBootScriptTemplate))

// diskBootScriptData holds template vars for the disk boot script.
type diskBootScriptData struct {
	Hostname string
}

// GenerateDiskBootScript returns an iPXE script that hands control back to
// the BIOS/UEFI boot order (local disk). Used for nodes in NodeStateDeployed.
func GenerateDiskBootScript(hostname string) ([]byte, error) {
	data := diskBootScriptData{Hostname: hostname}
	var buf bytes.Buffer
	if err := diskBootTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("pxe/boot: render disk boot script: %w", err)
	}
	return buf.Bytes(), nil
}
