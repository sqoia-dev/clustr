// Package bootassets embeds static boot-time binaries shipped with clonr-serverd.
// These are served over HTTP by the BootHandler to PXE/UEFI HTTP boot clients
// that need to chainload into iPXE before executing the clonr boot script.
package bootassets

import _ "embed"

// IPXEEFI is the iPXE UEFI binary for x86-64 (ipxe.efi).
// Sourced from iPXE v2.0.0 official release (ipxeboot.tar.gz, x86_64/ipxe.efi).
// SHA256: 868aa34057ff416ebf2fdfb5781de035e2c540477c04039198a9f8a9c6130034
//
//go:embed ipxe.efi
var IPXEEFI []byte
