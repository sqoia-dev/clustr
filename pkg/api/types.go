// Package api defines the shared wire types used by clonr-serverd and the clonr CLI.
// All JSON field names here are authoritative — the REST API contract.
package api

import (
	"encoding/json"
	"time"
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
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Version      string      `json:"version"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	Status       ImageStatus `json:"status"`
	Format       ImageFormat `json:"format"`
	SizeBytes    int64       `json:"size_bytes"`
	Checksum     string      `json:"checksum"`     // sha256 hex of the blob
	DiskLayout   DiskLayout  `json:"disk_layout"`
	Tags         []string    `json:"tags"`
	SourceURL    string      `json:"source_url,omitempty"`
	Notes        string      `json:"notes"`
	ErrorMessage string      `json:"error_message,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	FinalizedAt  *time.Time  `json:"finalized_at,omitempty"`
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

// NodeConfig holds everything that makes a deployed image specific to one
// physical node. Applied at deploy time — never baked into the BaseImage blob.
type NodeConfig struct {
	ID              string              `json:"id"`
	Hostname        string              `json:"hostname"`
	FQDN            string              `json:"fqdn"`
	PrimaryMAC      string              `json:"primary_mac"`
	Interfaces      []InterfaceConfig   `json:"interfaces"`
	SSHKeys         []string            `json:"ssh_keys"`
	KernelArgs      string              `json:"kernel_args"`
	Groups          []string            `json:"groups"`
	CustomVars      map[string]string   `json:"custom_vars"`
	BaseImageID     string              `json:"base_image_id,omitempty"`
	BMC             *BMCNodeConfig      `json:"bmc,omitempty"`
	IBConfig        []IBInterfaceConfig `json:"ib_config,omitempty"`
	// HardwareProfile is the raw hardware discovery JSON from the node.
	// Populated on auto-registration; nil when node was created manually.
	HardwareProfile json.RawMessage     `json:"hardware_profile,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
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
}

// ─── Factory request types ────────────────────────────────────────────────────

// ImportISORequest is the JSON metadata posted alongside a multipart ISO upload.
type ImportISORequest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CaptureRequest is the body for POST /api/v1/factory/capture.
type CaptureRequest struct {
	Source  string   `json:"source"`   // rsync source: "user@host:/" or local path
	Name    string   `json:"name"`
	Version string   `json:"version"`
	OS      string   `json:"os"`
	Arch    string   `json:"arch"`
	Tags    []string `json:"tags"`
	Notes   string   `json:"notes"`
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
