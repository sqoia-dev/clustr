package api

import (
	"testing"
)

// biosLayout is a minimal DiskLayout representing a BIOS boot configuration.
var biosLayout = DiskLayout{
	Partitions: []PartitionSpec{
		{Label: "biosboot", SizeBytes: 1 * 1024 * 1024, Filesystem: "biosboot", Flags: []string{"bios_grub"}},
		{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
		{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
	},
	Bootloader: Bootloader{Type: "grub2", Target: "i386-pc"},
}

// efiLayout is a minimal DiskLayout representing a UEFI boot configuration.
var efiLayout = DiskLayout{
	Partitions: []PartitionSpec{
		{Label: "esp", SizeBytes: 512 * 1024 * 1024, Filesystem: "vfat", MountPoint: "/boot/efi", Flags: []string{"esp", "boot"}},
		{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
		{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
	},
	Bootloader: Bootloader{Type: "grub2", Target: "x86_64-efi"},
}

// groupLayout is a different layout used for group-level override tests.
var groupLayout = DiskLayout{
	Partitions: []PartitionSpec{
		{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
	},
	Bootloader: Bootloader{Type: "grub2", Target: "i386-pc"},
}

// TestEffectiveLayout_NoOverride verifies that when no node or group override
// is set, the image's default layout is used and the source is reported as "image".
func TestEffectiveLayout_NoOverride(t *testing.T) {
	img := &BaseImage{DiskLayout: efiLayout}
	node := &NodeConfig{}

	got := node.EffectiveLayout(img, nil)
	if got.Bootloader.Target != efiLayout.Bootloader.Target {
		t.Errorf("EffectiveLayout: want image layout (x86_64-efi), got %q", got.Bootloader.Target)
	}
	if src := node.EffectiveLayoutSource(img, nil); src != "image" {
		t.Errorf("EffectiveLayoutSource: want \"image\", got %q", src)
	}
}

// TestEffectiveLayout_NodeOverride verifies that a node-level override beats
// both the group override and the image default.
func TestEffectiveLayout_NodeOverride(t *testing.T) {
	img := &BaseImage{DiskLayout: efiLayout}
	group := &NodeGroup{DiskLayoutOverride: &groupLayout}
	node := &NodeConfig{DiskLayoutOverride: &biosLayout}

	got := node.EffectiveLayout(img, group)
	if got.Bootloader.Target != biosLayout.Bootloader.Target {
		t.Errorf("EffectiveLayout: want node override (i386-pc), got %q", got.Bootloader.Target)
	}
	if src := node.EffectiveLayoutSource(img, group); src != "node" {
		t.Errorf("EffectiveLayoutSource: want \"node\", got %q", src)
	}
}

// TestEffectiveLayout_NodeOverride_NoGroup verifies node override wins when
// there is no group (nil group pointer).
func TestEffectiveLayout_NodeOverride_NoGroup(t *testing.T) {
	img := &BaseImage{DiskLayout: efiLayout}
	node := &NodeConfig{DiskLayoutOverride: &biosLayout}

	got := node.EffectiveLayout(img, nil)
	if got.Bootloader.Target != biosLayout.Bootloader.Target {
		t.Errorf("EffectiveLayout: want node override (i386-pc), got %q", got.Bootloader.Target)
	}
	if src := node.EffectiveLayoutSource(img, nil); src != "node" {
		t.Errorf("EffectiveLayoutSource: want \"node\", got %q", src)
	}
}

// TestEffectiveLayout_GroupOverride verifies that a group-level override beats
// the image default when no node-level override is set.
func TestEffectiveLayout_GroupOverride(t *testing.T) {
	img := &BaseImage{DiskLayout: efiLayout}
	group := &NodeGroup{DiskLayoutOverride: &groupLayout}
	node := &NodeConfig{} // no node override

	got := node.EffectiveLayout(img, group)
	if got.Bootloader.Target != groupLayout.Bootloader.Target {
		t.Errorf("EffectiveLayout: want group override (i386-pc), got %q", got.Bootloader.Target)
	}
	if src := node.EffectiveLayoutSource(img, group); src != "group" {
		t.Errorf("EffectiveLayoutSource: want \"group\", got %q", src)
	}
}

// TestEffectiveLayout_EmptyNodeOverride verifies that a nil DiskLayoutOverride
// on the node falls through to the group/image layer — it is not treated as an
// active override.
func TestEffectiveLayout_EmptyNodeOverride(t *testing.T) {
	img := &BaseImage{DiskLayout: efiLayout}
	node := &NodeConfig{DiskLayoutOverride: nil}

	got := node.EffectiveLayout(img, nil)
	if got.Bootloader.Target != efiLayout.Bootloader.Target {
		t.Errorf("EffectiveLayout: nil node override should fall through to image, got %q", got.Bootloader.Target)
	}
	if src := node.EffectiveLayoutSource(img, nil); src != "image" {
		t.Errorf("EffectiveLayoutSource: want \"image\" for nil node override, got %q", src)
	}
}

// TestEffectiveLayout_NilImg verifies that when the image is nil (no image
// assigned yet), EffectiveLayout returns a zero-value DiskLayout.
func TestEffectiveLayout_NilImg(t *testing.T) {
	node := &NodeConfig{}
	got := node.EffectiveLayout(nil, nil)
	if len(got.Partitions) != 0 {
		t.Errorf("EffectiveLayout with nil image: expected empty layout, got %+v", got)
	}
}
