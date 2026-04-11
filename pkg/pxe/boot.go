package pxe

import (
	"fmt"
	"text/template"
	"bytes"
)

// iPXE boot script template.
// The ${mac} and ${next-server} variables are expanded by iPXE itself at runtime.
// We inject clonr.server at kernel command line level.
const bootScriptTemplate = `#!ipxe
set server-url {{.ServerURL}}
kernel ${server-url}/api/v1/boot/vmlinuz
initrd ${server-url}/api/v1/boot/initramfs.img
imgargs vmlinuz clonr.server=${server-url} clonr.mac=${mac} console=tty0
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
