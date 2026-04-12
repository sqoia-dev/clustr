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
