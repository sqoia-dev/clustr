// Package api defines the shared wire types used by clonr-serverd and the clonr CLI.
// All JSON field names here are authoritative — the REST API contract.
package api

import (
	"encoding/json"
	"fmt"
	"time"
)

// KeyScope defines the access level of an API key.
type KeyScope string

const (
	KeyScopeAdmin KeyScope = "admin" // full access to all admin routes
	KeyScopeNode  KeyScope = "node"  // limited: register, deploy-complete, logs ingest
)

// ImageStatus represents the lifecycle state of a BaseImage.
type ImageStatus string

const (
	ImageStatusBuilding ImageStatus = "building"
	ImageStatusReady    ImageStatus = "ready"
	ImageStatusError    ImageStatus = "error"
	ImageStatusArchived ImageStatus = "archived"
)

// ImageFormat describes how the image blob is stored on disk.
type ImageFormat string

const (
	ImageFormatFilesystem ImageFormat = "filesystem" // tar archive of a root filesystem
	ImageFormatBlock      ImageFormat = "block"      // raw block device image (partclone/dd)
)

// ImageFirmware identifies the firmware interface the image was built for.
// Allowed values: "uefi" (default, OVMF/EDK2) and "bios" (legacy SeaBIOS / i386-pc GRUB).
type ImageFirmware string

const (
	// FirmwareUEFI is the default — OVMF pflash drives in QEMU, efibootmgr on deploy.
	FirmwareUEFI ImageFirmware = "uefi"
	// FirmwareBIOS targets legacy BIOS nodes: SeaBIOS in the installer VM,
	// grub2-install --target=i386-pc at deploy time. GPT+biosboot partition is used
	// so disks >2 TB are supported.
	FirmwareBIOS ImageFirmware = "bios"
)

// FstabEntry describes a single mount to add to /etc/fstab during finalization.
// Entries are stored on NodeConfig and NodeGroup; the effective list is the
// group entries merged with node entries (node overrides group by mount point).
type FstabEntry struct {
	Source     string `json:"source"`              // e.g. "nfs-server:/export/home"
	MountPoint string `json:"mount_point"`         // e.g. "/home/shared"
	FSType     string `json:"fs_type"`             // "nfs", "nfs4", "cifs", "lustre", …
	Options    string `json:"options"`             // "defaults,_netdev,vers=4"
	Dump       int    `json:"dump"`                // usually 0
	Pass       int    `json:"pass"`                // usually 0 for network mounts
	AutoMkdir  bool   `json:"auto_mkdir"`          // create mount point if missing
	Comment    string `json:"comment,omitempty"`   // human-readable note
}

// NodeGroup is a named set of nodes that share a disk layout override and other
// configuration. Nodes may optionally belong to a group; when they do, the
// group's DiskLayoutOverride takes precedence over the image default but is
// overridden by a node-level DiskLayoutOverride.
type NodeGroup struct {
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	DiskLayoutOverride *DiskLayout  `json:"disk_layout_override,omitempty"` // nil = use image default
	ExtraMounts        []FstabEntry `json:"extra_mounts,omitempty"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
}

// DiskLayout describes the partition schema expected on a target node.
// It is part of BaseImage — never per-node.
type DiskLayout struct {
	// RAIDArrays defines software RAID arrays to create before partitioning.
	// Arrays are created first; PartitionSpec.Device may reference an array name
	// (e.g. "md0") to partition on top of a RAID array instead of a raw disk.
	RAIDArrays  []RAIDSpec      `json:"raid_arrays,omitempty"`
	Partitions  []PartitionSpec `json:"partitions"`
	Bootloader  Bootloader      `json:"bootloader"`
	// TargetDevice is an optional operator hint specifying the preferred kernel
	// device name (e.g. "nvme0n1") to deploy to. When set, selectTargetDisk
	// will prefer this device over automatic selection heuristics.
	TargetDevice string          `json:"target_device,omitempty"`
}

// RAIDSpec describes a software RAID array to create during deployment.
type RAIDSpec struct {
	// Name is the md device name, e.g. "md0".
	Name    string   `json:"name"`
	// Level is the RAID level: "raid0", "raid1", "raid5", "raid6", "raid10".
	Level   string   `json:"level"`
	// Members lists the member devices by kernel name (e.g. "sda", "sdb") or
	// by size-based selector (e.g. "smallest-2" = the two smallest disks).
	Members []string `json:"members"`
	// ChunkKB is the chunk size in KiB. When 0, mdadm picks the default for
	// the RAID level (typically 512K for raid0/5/6/10, unused for raid1).
	ChunkKB int      `json:"chunk_kb,omitempty"`
	// Spare is the number of hot spare devices to include in the array.
	Spare   int      `json:"spare,omitempty"`
}

// PartitionSpec describes a single partition within a DiskLayout.
type PartitionSpec struct {
	// Device is the target block device for this partition. If empty, the
	// deployer uses the automatically selected target disk. If set to an md
	// device name (e.g. "md0"), the partition is created on that RAID array.
	Device     string   `json:"device,omitempty"`
	Label      string   `json:"label"`
	SizeBytes  int64    `json:"size_bytes"`  // 0 = fill remaining
	Filesystem string   `json:"filesystem"`  // "xfs", "ext4", "vfat", "swap"
	MountPoint string   `json:"mountpoint"`
	Flags      []string `json:"flags"`      // ["boot", "esp"]
	MinBytes   int64    `json:"min_bytes"`  // minimum disk size to satisfy this layout
}

// Bootloader specifies which bootloader is used and its target platform.
type Bootloader struct {
	Type   string `json:"type"`   // "grub2", "systemd-boot"
	Target string `json:"target"` // "x86_64-efi", "i386-pc"
}

// BaseImage is a deployable OS image, stripped of all node-specific identity.
// It is immutable once finalized (Status == ImageStatusReady).
type BaseImage struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	OS           string        `json:"os"`
	Arch         string        `json:"arch"`
	Status       ImageStatus   `json:"status"`
	Format       ImageFormat   `json:"format"`
	// Firmware identifies the firmware interface this image was built for.
	// "uefi" (default) or "bios" (legacy). Existing images without this field
	// stored default to "uefi" via the DB column DEFAULT.
	Firmware     ImageFirmware `json:"firmware"`
	SizeBytes    int64         `json:"size_bytes"`
	Checksum     string        `json:"checksum"`     // sha256 hex of the blob
	DiskLayout   DiskLayout    `json:"disk_layout"`
	Tags         []string      `json:"tags"`
	SourceURL    string        `json:"source_url,omitempty"`
	Notes        string        `json:"notes"`
	ErrorMessage string        `json:"error_message,omitempty"`
	// BuildMethod identifies how the image was created: "pull", "import", "capture", "iso".
	// Used by the UI to decide which detail view to show (e.g. build progress panel).
	BuildMethod  string        `json:"build_method,omitempty"`
	// BuiltForRoles holds the HPC role IDs that were selected when the image was
	// built via the Build from ISO flow. Used by the node-assignment UI to warn
	// when a node's role tag doesn't match the image's built-for roles.
	BuiltForRoles []string     `json:"built_for_roles,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	FinalizedAt  *time.Time    `json:"finalized_at,omitempty"`
}

// InterfaceConfig holds the static network configuration for one NIC on a node.
type InterfaceConfig struct {
	MACAddress string   `json:"mac_address"`
	Name       string   `json:"name"`       // "eth0", "ens3"
	IPAddress  string   `json:"ip_address"` // CIDR: "192.168.1.50/24"
	Gateway    string   `json:"gateway,omitempty"`
	DNS        []string `json:"dns,omitempty"`
	MTU        int      `json:"mtu,omitempty"`
	Bond       string   `json:"bond,omitempty"`
}

// BMCNodeConfig holds IPMI/BMC network and credential configuration applied
// during node finalization. The password field is write-only — it is applied
// on the node itself and is never returned by the API.
type BMCNodeConfig struct {
	IPAddress string `json:"ip_address"`
	Netmask   string `json:"netmask"`
	Gateway   string `json:"gateway"`
	Username  string `json:"username"`
	Password  string `json:"password"` // applied during finalize, never returned by API
}

// IBInterfaceConfig holds per-device InfiniBand / IPoIB configuration applied
// during node finalization.
type IBInterfaceConfig struct {
	DeviceName string   `json:"device_name"`        // e.g. "mlx5_0"
	PKeys      []string `json:"pkeys"`              // partition keys, e.g. ["0x8001"]
	IPoIBMode  string   `json:"ipoib_mode"`         // "connected" or "datagram"
	IPAddress  string   `json:"ip_address,omitempty"` // IPoIB IP in CIDR notation
	MTU        int      `json:"mtu,omitempty"`      // typically 65520 for connected mode
}

// PowerProviderConfig holds the type and backend-specific fields for a node's
// power management provider. The "type" field selects the backend ("ipmi",
// "proxmox", …); "fields" carries backend-specific key/value pairs.
//
// Security: Fields may contain credentials. Always call Sanitize() before
// returning this struct in an API response.
type PowerProviderConfig struct {
	Type   string            `json:"type"`
	Fields map[string]string `json:"fields"`
}

// sensitiveFields lists the key names whose values are redacted by Sanitize.
var sensitiveFields = []string{
	"password", "token_secret", "secret", "api_key", "api_secret",
}

// Sanitize returns a copy of c with credential fields replaced by "****".
// Always call this before including a PowerProviderConfig in an API response.
func (c *PowerProviderConfig) Sanitize() *PowerProviderConfig {
	if c == nil {
		return nil
	}
	out := &PowerProviderConfig{
		Type:   c.Type,
		Fields: make(map[string]string, len(c.Fields)),
	}
	for k, v := range c.Fields {
		out.Fields[k] = v
	}
	for _, name := range sensitiveFields {
		if _, ok := out.Fields[name]; ok {
			out.Fields[name] = "****"
		}
	}
	return out
}

// NodeState enumerates the lifecycle states of a NodeConfig.
// The state is derived from existing fields via NodeConfig.State() rather than
// stored as a separate column, so it cannot drift from the underlying data.
type NodeState string

const (
	// NodeStateRegistered: node has PXE-booted and self-registered but no image
	// has been assigned yet. The node is idle, waiting for admin action.
	NodeStateRegistered NodeState = "registered"

	// NodeStateConfigured: a base image has been assigned but the node has not
	// yet run a successful deployment. Next PXE boot will trigger a deploy.
	NodeStateConfigured NodeState = "configured"

	// NodeStateDeploying: reserved for future use when a deploy is actively
	// in-flight and the server can observe it via progress callbacks.
	NodeStateDeploying NodeState = "deploying"

	// NodeStateDeployed: the most recent deploy succeeded and reimage_pending is
	// false. The PXE handler returns "exit" so the node boots from local disk.
	NodeStateDeployed NodeState = "deployed"

	// NodeStateReimagePending: admin has requested a reimage. The next PXE boot
	// will trigger a fresh deploy regardless of prior deployment state.
	NodeStateReimagePending NodeState = "reimage_pending"

	// NodeStateFailed: the most recent deploy failed and no successful deploy has
	// occurred since. Needs admin attention.
	NodeStateFailed NodeState = "failed"
)

// NodeConfig holds everything that makes a deployed image specific to one
// physical node. Applied at deploy time — never baked into the BaseImage blob.
type NodeConfig struct {
	ID              string               `json:"id"`
	Hostname        string               `json:"hostname"`
	HostnameAuto    bool                 `json:"hostname_auto"`
	FQDN            string               `json:"fqdn"`
	PrimaryMAC      string               `json:"primary_mac"`
	Interfaces      []InterfaceConfig    `json:"interfaces"`
	SSHKeys         []string             `json:"ssh_keys"`
	KernelArgs      string               `json:"kernel_args"`
	Groups          []string             `json:"groups"`
	CustomVars      map[string]string    `json:"custom_vars"`
	BaseImageID     string               `json:"base_image_id,omitempty"`
	BMC             *BMCNodeConfig       `json:"bmc,omitempty"`
	IBConfig        []IBInterfaceConfig  `json:"ib_config,omitempty"`
	// PowerProvider selects the power management backend for this node.
	// If nil, the server falls back to legacy BMC-based IPMI when BMC is set.
	PowerProvider   *PowerProviderConfig `json:"power_provider,omitempty"`
	// GroupID optionally links this node to a NodeGroup. When set, the group's
	// DiskLayoutOverride is consulted during layout resolution if the node has
	// no node-level override.
	GroupID         string               `json:"group_id,omitempty"`
	// DiskLayoutOverride, when non-nil, completely replaces the image's disk
	// layout for this specific node. Takes highest priority in resolution.
	DiskLayoutOverride *DiskLayout       `json:"disk_layout_override,omitempty"`
	// ExtraMounts holds additional /etc/fstab entries written during finalization.
	// The effective list is group mounts merged with node mounts; use
	// EffectiveExtraMounts to resolve. Stored as node-level on NodeConfig only
	// after server-side merging for the deploy path.
	ExtraMounts        []FstabEntry      `json:"extra_mounts,omitempty"`
	// ReimagePending is set to true by the reimage orchestrator after it fires
	// PowerCycle. The PXE boot handler returns the full clonr initramfs boot
	// script while this flag is set, causing the node to deploy fresh.
	// Cleared by the deploy-complete callback once deployment finalizes.
	ReimagePending  bool                 `json:"reimage_pending,omitempty"`
	// LastDeploySucceededAt is the Unix timestamp of the most recent successful
	// deployment finalize. Used by State() to determine NodeStateDeployed.
	LastDeploySucceededAt *time.Time `json:"last_deploy_succeeded_at,omitempty"`
	// LastDeployFailedAt is the Unix timestamp of the most recent failed deploy.
	// Used by State() to determine NodeStateFailed.
	LastDeployFailedAt *time.Time `json:"last_deploy_failed_at,omitempty"`
	// HardwareProfile is the raw hardware discovery JSON from the node.
	// Populated on auto-registration; nil when node was created manually.
	HardwareProfile json.RawMessage      `json:"hardware_profile,omitempty"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

// State derives the current lifecycle state of this node from its stored fields.
// This is the canonical way to determine what the PXE boot handler should return.
//
// Priority order (highest to lowest):
//  1. ReimagePending — always overrides everything else.
//  2. LastDeployFailedAt after LastDeploySucceededAt — node is in error.
//  3. LastDeploySucceededAt set — node is deployed and healthy.
//  4. BaseImageID set — node is configured but never deployed.
//  5. Otherwise — node is registered but has no image.
func (n *NodeConfig) State() NodeState {
	if n.ReimagePending {
		return NodeStateReimagePending
	}
	if n.LastDeployFailedAt != nil {
		if n.LastDeploySucceededAt == nil || n.LastDeployFailedAt.After(*n.LastDeploySucceededAt) {
			return NodeStateFailed
		}
	}
	if n.LastDeploySucceededAt != nil {
		return NodeStateDeployed
	}
	if n.BaseImageID != "" {
		return NodeStateConfigured
	}
	return NodeStateRegistered
}

// EffectiveLayout resolves the disk layout that will be used when deploying
// this node, following the three-level priority hierarchy:
//
//  1. Node-level override (highest) — DiskLayoutOverride on this NodeConfig.
//  2. Group-level override — DiskLayoutOverride on the NodeGroup, if any.
//  3. Image default (lowest) — DiskLayout on the BaseImage.
//
// Pass group=nil when the node is not in a group or the group has no override.
func (n *NodeConfig) EffectiveLayout(img *BaseImage, group *NodeGroup) DiskLayout {
	if n.DiskLayoutOverride != nil {
		return *n.DiskLayoutOverride
	}
	if group != nil && group.DiskLayoutOverride != nil {
		return *group.DiskLayoutOverride
	}
	if img != nil {
		return img.DiskLayout
	}
	return DiskLayout{}
}

// EffectiveLayoutSource returns a human-readable label describing which level
// of the hierarchy provided the effective layout: "node", "group", or "image".
func (n *NodeConfig) EffectiveLayoutSource(img *BaseImage, group *NodeGroup) string {
	if n.DiskLayoutOverride != nil {
		return "node"
	}
	if group != nil && group.DiskLayoutOverride != nil {
		return "group"
	}
	return "image"
}

// EffectiveExtraMounts returns the merged fstab entries for this node.
// Group entries form the base; node entries override by mount point or append.
// Pass group=nil when the node is not in a group.
func (n *NodeConfig) EffectiveExtraMounts(group *NodeGroup) []FstabEntry {
	result := []FstabEntry{}
	seen := map[string]int{}

	if group != nil {
		for _, m := range group.ExtraMounts {
			seen[m.MountPoint] = len(result)
			result = append(result, m)
		}
	}
	for _, m := range n.ExtraMounts {
		if idx, exists := seen[m.MountPoint]; exists {
			result[idx] = m // node overrides group for this mount point
		} else {
			result = append(result, m)
		}
	}
	return result
}

// allowedFSTypes is the whitelist of supported filesystem types for FstabEntry.
var allowedFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true,
	"beegfs": true, "lustre": true, "xfs": true, "ext4": true,
	"ext3": true, "vfat": true, "tmpfs": true, "bind": true,
	"9p": true, "gpfs": true,
}

// forbiddenMountPoints lists paths that must never be used as extra mount points.
var forbiddenMountPoints = map[string]bool{
	"/": true, "/boot": true, "/proc": true, "/sys": true, "/dev": true, "/run": true,
}

// networkFSTypes lists filesystem types that require network access at mount
// time and should carry the _netdev option so systemd waits for the network.
var networkFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true,
	"beegfs": true, "lustre": true, "gpfs": true, "9p": true,
}

// ValidateFstabEntry checks that e is safe to write into /etc/fstab.
// Returns a non-nil error describing the first problem found.
func ValidateFstabEntry(e FstabEntry) error {
	if e.Source == "" {
		return fmt.Errorf("fstab entry source must not be empty")
	}
	if e.MountPoint == "" || e.MountPoint[0] != '/' {
		return fmt.Errorf("fstab entry mount_point %q must be an absolute path", e.MountPoint)
	}
	if forbiddenMountPoints[e.MountPoint] {
		return fmt.Errorf("fstab entry mount_point %q is a reserved system path and cannot be overridden", e.MountPoint)
	}
	if !allowedFSTypes[e.FSType] {
		return fmt.Errorf("fstab entry fs_type %q is not in the allowed list", e.FSType)
	}
	return nil
}

// IsNetworkFS reports whether fsType requires network connectivity at mount time.
func IsNetworkFS(fsType string) bool {
	return networkFSTypes[fsType]
}

// --- Request types ---

// CreateImageRequest is the body for POST /api/v1/images.
type CreateImageRequest struct {
	Name       string      `json:"name"`
	Version    string      `json:"version"`
	OS         string      `json:"os"`
	Arch       string      `json:"arch"`
	Format     ImageFormat `json:"format"`
	DiskLayout DiskLayout  `json:"disk_layout"`
	Tags       []string    `json:"tags"`
	SourceURL  string      `json:"source_url,omitempty"`
	Notes      string      `json:"notes"`
}

// PullRequest is the body for POST /api/v1/factory/pull.
type PullRequest struct {
	URL        string      `json:"url"`
	Name       string      `json:"name"`
	Version    string      `json:"version"`
	OS         string      `json:"os"`
	Arch       string      `json:"arch"`
	Format     ImageFormat `json:"format"`
	DiskLayout DiskLayout  `json:"disk_layout"`
	Tags       []string    `json:"tags"`
	Notes      string      `json:"notes"`
}

// CreateNodeConfigRequest is the body for POST /api/v1/nodes.
type CreateNodeConfigRequest struct {
	Hostname    string            `json:"hostname"`
	FQDN        string            `json:"fqdn"`
	PrimaryMAC  string            `json:"primary_mac"`
	Interfaces  []InterfaceConfig `json:"interfaces"`
	SSHKeys     []string          `json:"ssh_keys"`
	KernelArgs  string            `json:"kernel_args"`
	Groups      []string          `json:"groups"`
	CustomVars  map[string]string `json:"custom_vars"`
	BaseImageID string            `json:"base_image_id"`
}

// UpdateNodeConfigRequest is the body for PUT /api/v1/nodes/:id.
type UpdateNodeConfigRequest struct {
	Hostname           string               `json:"hostname"`
	FQDN               string               `json:"fqdn"`
	PrimaryMAC         string               `json:"primary_mac"`
	Interfaces         []InterfaceConfig    `json:"interfaces"`
	SSHKeys            []string             `json:"ssh_keys"`
	KernelArgs         string               `json:"kernel_args"`
	Groups             []string             `json:"groups"`
	CustomVars         map[string]string    `json:"custom_vars"`
	BaseImageID        string               `json:"base_image_id"`
	PowerProvider      *PowerProviderConfig `json:"power_provider,omitempty"`
	GroupID            string               `json:"group_id,omitempty"`
	// DiskLayoutOverride, when non-nil, replaces the image/group disk layout for
	// this node. Send null or omit to clear a previously set override.
	DiskLayoutOverride *DiskLayout          `json:"disk_layout_override,omitempty"`
	// ClearLayoutOverride, when true, explicitly removes any node-level override.
	// Use this instead of sending an empty DiskLayoutOverride, which is ambiguous.
	ClearLayoutOverride bool                `json:"clear_layout_override,omitempty"`
	// ExtraMounts replaces the node-level extra fstab entries. Send an empty
	// slice to clear all node-level mounts (group mounts are unaffected).
	ExtraMounts         []FstabEntry        `json:"extra_mounts,omitempty"`
}

// ─── Node group request types ─────────────────────────────────────────────────

// CreateNodeGroupRequest is the body for POST /api/v1/node-groups.
type CreateNodeGroupRequest struct {
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	DiskLayoutOverride *DiskLayout  `json:"disk_layout_override,omitempty"`
	ExtraMounts        []FstabEntry `json:"extra_mounts,omitempty"`
}

// UpdateNodeGroupRequest is the body for PUT /api/v1/node-groups/:id.
type UpdateNodeGroupRequest struct {
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	DiskLayoutOverride *DiskLayout  `json:"disk_layout_override,omitempty"`
	ClearLayoutOverride bool        `json:"clear_layout_override,omitempty"`
	// ExtraMounts replaces the group-level extra fstab entries.
	ExtraMounts         []FstabEntry `json:"extra_mounts,omitempty"`
}

// AssignGroupRequest is the body for PUT /api/v1/nodes/:id/group.
type AssignGroupRequest struct {
	// GroupID is the group to assign. Empty string removes the node from its
	// current group (equivalent to DELETE).
	GroupID string `json:"group_id"`
}

// ─── Node group response types ────────────────────────────────────────────────

// ListNodeGroupsResponse wraps the node groups list.
type ListNodeGroupsResponse struct {
	Groups []NodeGroup `json:"groups"`
	Total  int         `json:"total"`
}

// ─── Layout recommendation types ─────────────────────────────────────────────

// LayoutRecommendation is the response from GET /api/v1/nodes/:id/layout-recommendation.
// It contains a suggested DiskLayout derived from hardware discovery and the
// reasoning behind each decision so admins can evaluate it before applying.
type LayoutRecommendation struct {
	Layout    DiskLayout `json:"layout"`
	Reasoning string     `json:"reasoning"`
	Warnings  []string   `json:"warnings,omitempty"`
}

// LayoutValidationRequest is the body for POST /api/v1/nodes/:id/layout/validate.
type LayoutValidationRequest struct {
	Layout DiskLayout `json:"layout"`
}

// LayoutValidationResponse is returned by the validation endpoint.
type LayoutValidationResponse struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// EffectiveLayoutResponse is returned by GET /api/v1/nodes/:id/effective-layout.
type EffectiveLayoutResponse struct {
	Layout DiskLayout `json:"layout"`
	Source string     `json:"source"` // "node", "group", or "image"
	GroupID string    `json:"group_id,omitempty"`
	ImageID string    `json:"image_id,omitempty"`
}

// EffectiveMountsResponse is returned by GET /api/v1/nodes/:id/effective-mounts.
// It shows the merge result along with where each entry originates.
type EffectiveMountEntry struct {
	FstabEntry
	Source  string `json:"source"`             // "node" or "group"
	GroupID string `json:"group_id,omitempty"` // set when source == "group"
}

type EffectiveMountsResponse struct {
	Mounts  []EffectiveMountEntry `json:"mounts"`
	NodeID  string                `json:"node_id"`
	GroupID string                `json:"group_id,omitempty"`
}

// --- Response types ---

// ErrorResponse is the standard error envelope returned on 4xx/5xx.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// ListImagesResponse wraps the images list.
type ListImagesResponse struct {
	Images []BaseImage `json:"images"`
	Total  int         `json:"total"`
}

// ListNodesResponse wraps the node configs list.
type ListNodesResponse struct {
	Nodes []NodeConfig `json:"nodes"`
	Total int          `json:"total"`
}

// HealthResponse is returned by GET /api/v1/health.
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

// ImageInUseResponse is returned with 409 Conflict when a DELETE /api/v1/images/:id
// is rejected because nodes have the image assigned.
type ImageInUseResponse struct {
	Error string       `json:"error"`
	Code  string       `json:"code"`
	Nodes []NodeConfig `json:"nodes"`
}

// ─── Log types ───────────────────────────────────────────────────────────────

// LogEntry is a single structured log event shipped from a CLI client.
type LogEntry struct {
	ID        string                 `json:"id"`
	NodeMAC   string                 `json:"node_mac"`
	Hostname  string                 `json:"hostname,omitempty"`
	Level     string                 `json:"level"`     // "debug", "info", "warn", "error"
	Component string                 `json:"component"` // "hardware", "deploy", "chroot", "ipmi", "efiboot"
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
}

// LogFilter specifies query constraints for log retrieval.
type LogFilter struct {
	NodeMAC   string
	Hostname  string
	Level     string
	Component string
	Since     *time.Time
	Limit     int
}

// ListLogsResponse wraps a log query result.
type ListLogsResponse struct {
	Logs  []LogEntry `json:"logs"`
	Total int        `json:"total"`
}

// ─── PXE / auto-registration types ───────────────────────────────────────────

// RegisterRequest is the body for POST /api/v1/nodes/register.
// Sent by the clonr client on first PXE boot to register itself with the server.
type RegisterRequest struct {
	// HardwareProfile is the raw JSON from hardware.Discover().
	HardwareProfile json.RawMessage `json:"hardware_profile"`
}

// RegisterResponse is the response body for POST /api/v1/nodes/register.
type RegisterResponse struct {
	NodeConfig *NodeConfig `json:"node_config"`
	// Action tells the client what to do next:
	//   "deploy"  — an image has been assigned; proceed with deployment.
	//   "wait"    — no image assigned yet; poll GET /api/v1/nodes/by-mac/:mac every 30s.
	//   "capture" — admin wants to capture this node's image (future).
	Action string `json:"action"`
	// DryRun, when true, instructs the deploy client to execute the full PXE
	// boot sequence (disk selection, partitioning decisions, etc.) but skip the
	// actual disk wipe and filesystem operations. Set when the triggering
	// reimage request had dry_run=true.
	DryRun bool `json:"dry_run,omitempty"`
}

// ─── Factory request types ────────────────────────────────────────────────────

// ImportISORequest is the JSON metadata posted alongside a multipart ISO upload.
type ImportISORequest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CaptureRequest is the body for POST /api/v1/factory/capture.
type CaptureRequest struct {
	// SourceHost is the SSH-reachable hostname or IP of the node to capture.
	SourceHost   string   `json:"source_host"`
	SSHUser      string   `json:"ssh_user,omitempty"`
	SSHPassword  string   `json:"ssh_password,omitempty"` // write-only, never returned
	SSHKeyPath   string   `json:"ssh_key_path,omitempty"`
	SSHPort      int      `json:"ssh_port,omitempty"` // defaults to 22 when zero
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Tags         []string `json:"tags"`
	Notes        string   `json:"notes"`
	ExcludePaths []string `json:"exclude_paths,omitempty"` // rsync --exclude patterns
}

// BuildFromISORequest is the body for POST /api/v1/factory/build-from-iso.
// It instructs clonr to download an installer ISO, run it in a temporary QEMU
// VM with an auto-generated kickstart/autoinstall config, and capture the
// installed OS as a deployable BaseImage.
//
// The build runs asynchronously and can take 5-30 minutes. Poll
// GET /api/v1/images/:id for status transitions: building → ready | error.
type BuildFromISORequest struct {
	// URL is the HTTP(S) URL of the installer ISO. Required.
	// Example: "https://download.rockylinux.org/pub/rocky/10/isos/x86_64/Rocky-10.1-x86_64-dvd1.iso"
	URL string `json:"url"`

	// Name is the human-readable name for the resulting BaseImage. Required.
	Name string `json:"name"`

	// Version is the image version string, e.g. "10.1". Optional.
	Version string `json:"version,omitempty"`

	// OS is a short OS identifier, e.g. "rocky", "ubuntu". Optional — auto-detected
	// from URL when empty.
	OS string `json:"os,omitempty"`

	// Arch is the CPU architecture, e.g. "x86_64". Optional.
	Arch string `json:"arch,omitempty"`

	// Distro explicitly specifies the distribution family when auto-detection
	// is unreliable. Valid values: "rocky", "almalinux", "centos", "rhel",
	// "ubuntu", "debian", "suse", "alpine". Optional — auto-detected from URL.
	Distro string `json:"distro,omitempty"`

	// DiskSizeGB is the size in GiB of the blank disk presented to the installer.
	// Default: 20. Minimum: 10. The installed rootfs will be smaller.
	DiskSizeGB int `json:"disk_size_gb,omitempty"`

	// MemoryMB is the RAM in MiB allocated to the installer VM. Default: 2048.
	MemoryMB int `json:"memory_mb,omitempty"`

	// CPUs is the number of virtual CPUs for the installer VM. Default: 2.
	CPUs int `json:"cpus,omitempty"`

	// RoleIDs is the list of HPC node role preset IDs to include in the build.
	// Each role ID corresponds to a Role returned by GET /api/v1/image-roles.
	// The role package lists are merged and written into the kickstart %packages
	// stanza. Ignored when CustomKickstart is non-empty.
	RoleIDs []string `json:"role_ids,omitempty"`

	// InstallUpdates, when true, appends a %post section that runs the distro's
	// package manager update command (dnf update -y / apt-get upgrade -y).
	// Adds 5-10 minutes to the build but produces a fully patched image.
	InstallUpdates bool `json:"install_updates,omitempty"`

	// CustomKickstart, when non-empty, overrides the auto-generated
	// kickstart/autoinstall config with admin-supplied content.
	// Only respected for RHEL-family distros (Rocky, Alma, CentOS, RHEL).
	// For other distros, this field is silently ignored.
	CustomKickstart string `json:"custom_kickstart,omitempty"`

	// DefaultUsername, when non-empty, creates a named user in the installed OS
	// with sudo/wheel access. Supported for RHEL-family (Rocky, Alma, CentOS, RHEL)
	// kickstart builds. Silently ignored for other distros.
	DefaultUsername string `json:"default_username,omitempty"`

	// DefaultPassword is the plaintext password for DefaultUsername and for the
	// root account. It is hashed server-side before being written to the installer
	// config; it is never stored or logged in plaintext.
	// When omitted, the root account uses a fixed per-build hash and no user
	// directive is emitted.
	DefaultPassword string `json:"default_password,omitempty"`

	// Firmware selects the firmware mode for the installer VM and resulting image.
	// Allowed values: "uefi" (default) and "bios" (legacy SeaBIOS). When empty,
	// "uefi" is assumed for backward compatibility.
	// - "uefi": OVMF pflash drives are passed to QEMU; ESP partition is created;
	//   efibootmgr is used during finalization.
	// - "bios": SeaBIOS (-bios flag) is used; a biosboot GPT partition is created;
	//   grub2-install --target=i386-pc is run during finalization.
	Firmware string `json:"firmware,omitempty"`

	// Tags is an optional list of string tags attached to the resulting image.
	Tags []string `json:"tags,omitempty"`

	// Notes is a free-text description stored on the resulting image.
	Notes string `json:"notes,omitempty"`
}

// ImageRoleResponse is the wire type for a single HPC role preset returned by
// GET /api/v1/image-roles. It is the read-only, UI-facing projection of the
// internal isoinstaller.Role type.
type ImageRoleResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	PackageCount int    `json:"package_count"` // unique packages across all supported distros
	Notes        string `json:"notes,omitempty"`
}

// ListImageRolesResponse wraps the role list returned by GET /api/v1/image-roles.
type ListImageRolesResponse struct {
	Roles []ImageRoleResponse `json:"roles"`
	Total int                 `json:"total"`
}

// ─── Shell session types ──────────────────────────────────────────────────────

// ShellSessionResponse is returned when a session is opened.
type ShellSessionResponse struct {
	SessionID string `json:"session_id"`
	ImageID   string `json:"image_id"`
	RootDir   string `json:"root_dir"`
}

// ExecRequest is the body for POST /api/v1/images/:id/shell-session/:sid/exec.
type ExecRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ExecResponse is returned by the exec endpoint.
type ExecResponse struct {
	Output string `json:"output"`
}

// ─── Reimage types ────────────────────────────────────────────────────────────

// ReimageStatus enumerates valid states for a ReimageRequest.
type ReimageStatus string

const (
	ReimageStatusPending    ReimageStatus = "pending"
	ReimageStatusTriggered  ReimageStatus = "triggered"
	ReimageStatusInProgress ReimageStatus = "in_progress"
	ReimageStatusComplete   ReimageStatus = "complete"
	ReimageStatusFailed     ReimageStatus = "failed"
	ReimageStatusCanceled   ReimageStatus = "canceled"
)

// IsTerminal reports whether s is a terminal state (no further transitions).
func (s ReimageStatus) IsTerminal() bool {
	switch s {
	case ReimageStatusComplete, ReimageStatusFailed, ReimageStatusCanceled:
		return true
	}
	return false
}

// ReimageRequest is the server-side record for a reimage lifecycle.
type ReimageRequest struct {
	ID           string        `json:"id"`
	NodeID       string        `json:"node_id"`
	ImageID      string        `json:"image_id"`
	Status       ReimageStatus `json:"status"`
	ScheduledAt  *time.Time    `json:"scheduled_at,omitempty"`
	TriggeredAt  *time.Time    `json:"triggered_at,omitempty"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	CompletedAt  *time.Time    `json:"completed_at,omitempty"`
	ErrorMessage string        `json:"error_message,omitempty"`
	RequestedBy  string        `json:"requested_by"`
	DryRun       bool          `json:"dry_run,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
}

// CreateReimageRequest is the body for POST /api/v1/nodes/:id/reimage.
type CreateReimageRequest struct {
	// ImageID is the base image to deploy. If empty the node's currently
	// assigned base_image_id is used.
	ImageID     string     `json:"image_id,omitempty"`
	// ScheduledAt, when non-nil, defers the reimage. nil = immediate.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	// DryRun sets next boot to PXE and power-cycles but does not wipe the disk.
	DryRun      bool       `json:"dry_run,omitempty"`
	// Force skips the image-ready and active-reimage pre-checks.
	Force       bool       `json:"force,omitempty"`
}

// ListReimagesResponse wraps the reimage history list.
type ListReimagesResponse struct {
	Requests []ReimageRequest `json:"requests"`
	Total    int              `json:"total"`
}

// ─── ISO build progress types ─────────────────────────────────────────────────

// BuildPhase is a named step in the ISO build pipeline.
type BuildPhase = string

const (
	BuildPhaseDownloadingISO   = "downloading_iso"
	BuildPhaseGeneratingConfig = "generating_config"
	BuildPhaseCreatingDisk     = "creating_disk"
	BuildPhaseLaunchingVM      = "launching_vm"
	BuildPhaseInstalling       = "installing"
	BuildPhaseExtracting       = "extracting"
	BuildPhaseScrubbing        = "scrubbing"
	BuildPhaseFinalizing       = "finalizing"
	BuildPhaseComplete         = "complete"
	BuildPhaseFailed           = "failed"
	BuildPhaseCanceled         = "canceled"
)

// BuildState is a snapshot of the current progress for one ISO build job.
// Returned by GET /api/v1/images/:id/build-progress.
type BuildState struct {
	ImageID      string    `json:"image_id"`
	Phase        string    `json:"phase"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	BytesTotal   int64     `json:"bytes_total"`
	BytesDone    int64     `json:"bytes_done"`
	ElapsedMS    int64     `json:"elapsed_ms"`
	ErrorMessage string    `json:"error_message,omitempty"`
	// SerialTail holds up to 100 recent lines from the QEMU serial console.
	SerialTail []string `json:"serial_tail,omitempty"`
	// QEMUStderr holds up to 50 recent lines from QEMU's own stderr output.
	QEMUStderr []string `json:"qemu_stderr,omitempty"`
}

// BuildEvent is one SSE message sent to subscribers of the build progress stream.
// It carries either a full state snapshot (on initial connect) or an incremental
// update (phase change, serial line, progress tick).
type BuildEvent struct {
	ImageID    string `json:"image_id"`
	Phase      string `json:"phase,omitempty"`
	SerialLine string `json:"serial_line,omitempty"` // non-empty = append-only line event
	StderrLine string `json:"stderr_line,omitempty"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
	BytesDone  int64  `json:"bytes_done,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}
