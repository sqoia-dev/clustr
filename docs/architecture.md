# clonr Architecture Design

## Project Context & Irreversibility Assessment

Before the structure: the decisions that will be painful to change later are (1) the image metadata schema and how base vs. node-specific config is split, (2) the storage layout on disk, and (3) the REST API contract between server and client. Everything else — logging library, CLI flag names, Makefile structure — is reversible. This document treats those three areas with the most rigor.

---

## Directory Layout

```
clonr/
├── cmd/
│   ├── clonr/                    # Static CLI binary (CGO_ENABLED=0)
│   │   └── main.go
│   └── clonr-serverd/            # Management server
│       └── main.go
├── pkg/
│   ├── api/                      # Shared API types (request/response structs, used by both sides)
│   │   ├── types.go
│   │   └── errors.go
│   ├── hardware/                 # Hardware discovery engine
│   │   ├── discovery.go          # Orchestrator: runs all sub-collectors
│   │   ├── cpu.go                # /proc/cpuinfo parser
│   │   ├── memory.go             # /proc/meminfo parser
│   │   ├── disk.go               # lsblk JSON parser + /sys/block/* walker
│   │   ├── network.go            # /sys/class/net/* + ip link JSON parser
│   │   ├── bios.go               # dmidecode integration (optional, graceful fallback)
│   │   └── types.go              # HardwareProfile, DiskDevice, NetworkInterface structs
│   ├── image/                    # Image lifecycle management (server-side)
│   │   ├── store.go              # ImageStore: CRUD over SQLite + blob filesystem
│   │   ├── factory.go            # ImageFactory: pull/import/build pipeline
│   │   ├── chroot.go             # Chroot lifecycle: mount, enter, cleanup
│   │   ├── capture.go            # Disk capture: rsync + partclone orchestration
│   │   ├── disklayout.go         # DiskLayout parsing, validation, serialization
│   │   └── types.go              # Image, BaseImage, NodeConfig, DiskLayout structs
│   ├── deploy/                   # Deployment engine (CLI-side, runs on target node)
│   │   ├── deploy.go             # Orchestrator
│   │   ├── rsync.go              # File-based deployment via rsync
│   │   ├── block.go              # Block-based deployment via dd/partclone
│   │   ├── multicast.go          # udpcast/mcast receiver
│   │   └── efiboot.go            # EFI boot entry repair via efibootmgr
│   ├── db/                       # Database layer
│   │   ├── db.go                 # SQLite open, migrate, connection pool
│   │   ├── migrations/           # Embedded SQL migration files
│   │   │   ├── 001_initial.sql
│   │   │   └── 002_node_config.sql
│   │   └── queries.go            # Typed query wrappers
│   ├── server/                   # HTTP server (clonr-serverd)
│   │   ├── server.go             # Router setup (net/http + chi)
│   │   ├── middleware.go         # Auth, logging, recovery
│   │   ├── handlers/
│   │   │   ├── images.go
│   │   │   ├── factory.go
│   │   │   ├── nodes.go
│   │   │   └── blobs.go          # Binary blob streaming (large file upload/download)
│   │   └── ui/                   # Embedded web UI (Go embed)
│   │       ├── embed.go
│   │       └── static/           # HTML/CSS/JS (minimal, no framework)
│   ├── client/                   # HTTP client for clonr CLI → server communication
│   │   ├── client.go
│   │   └── transport.go          # Retry, timeout, TLS config
│   └── config/                   # Config file loading for both binaries
│       ├── server.go             # ServerConfig struct + YAML/env loading
│       └── client.go             # ClientConfig struct (server URL, auth token)
├── internal/                     # Not exported — implementation details
│   └── proc/                     # Low-level /proc and /sys readers
│       ├── reader.go
│       └── sysfs.go
├── test/
│   ├── fixtures/                 # Test hardware output fixtures (real lsblk JSON samples)
│   │   ├── lsblk_nvme.json
│   │   ├── lsblk_sata_raid.json
│   │   └── cpuinfo_dual_socket.txt
│   ├── integration/              # Integration tests requiring root/loop devices
│   │   └── chroot_test.go
│   └── e2e/                      # End-to-end: boot a QEMU VM, deploy image
│       └── deploy_test.go
├── scripts/
│   ├── build-initramfs.sh        # Build minimal Rocky initramfs with clonr embedded
│   └── ci-test.sh
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

---

## Package Boundaries and Key Interfaces

### pkg/hardware

The hardware layer is structured as a collector pattern. Each sub-collector is independently testable with fixture files. The orchestrator calls them in parallel and merges results.

```go
// pkg/hardware/discovery.go

// Collector is implemented by each hardware sub-package.
// Collect must be safe to call in a goroutine and must not
// require any external process to be running (pure /proc + /sys reads).
type Collector interface {
    Collect(ctx context.Context) error
    Result() interface{}
}

// HardwareProfile is the complete hardware snapshot of a node.
// This is what gets sent to the server during enrollment and capture.
type HardwareProfile struct {
    Hostname   string             `json:"hostname"`
    Timestamp  time.Time          `json:"timestamp"`
    CPU        CPUInfo            `json:"cpu"`
    Memory     MemoryInfo         `json:"memory"`
    Disks      []DiskDevice       `json:"disks"`
    Interfaces []NetworkInterface `json:"interfaces"`
    BIOS       *BIOSInfo          `json:"bios,omitempty"` // nil if dmidecode unavailable
}

// Discover runs all collectors concurrently and returns a complete HardwareProfile.
// Any collector failure is non-fatal and is recorded in the returned error slice.
func Discover(ctx context.Context) (HardwareProfile, []error)
```

```go
// pkg/hardware/disk.go

type DiskDevice struct {
    Name        string      `json:"name"`         // "nvme0n1", "sda"
    Path        string      `json:"path"`         // "/dev/nvme0n1"
    SizeBytes   int64       `json:"size_bytes"`
    Type        DiskType    `json:"type"`         // NVMe, SATA, SAS, RAID
    Model       string      `json:"model"`
    Serial      string      `json:"serial"`
    Rotational  bool        `json:"rotational"`
    Partitions  []Partition `json:"partitions"`
    WWN         string      `json:"wwn,omitempty"`
}

type Partition struct {
    Name       string `json:"name"`
    SizeBytes  int64  `json:"size_bytes"`
    Filesystem string `json:"filesystem"` // "xfs", "ext4", "vfat", "swap"
    MountPoint string `json:"mountpoint,omitempty"`
    UUID       string `json:"uuid,omitempty"`
    Label      string `json:"label,omitempty"`
}

// DiskType drives which capture method is selected during image creation.
type DiskType string
const (
    DiskTypeNVMe DiskType = "nvme"
    DiskTypeSATA DiskType = "sata"
    DiskTypeSAS  DiskType = "sas"
    DiskTypeRAID DiskType = "raid" // Software RAID, md devices
)
```

### pkg/image — The Core Data Model

This is the most irreversible package. The split between BaseImage and NodeConfig is the architectural load-bearing wall.

```go
// pkg/image/types.go

// BaseImage represents a deployable OS image, stripped of any
// node-specific identity. It is immutable once finalized.
// Mutating a BaseImage requires creating a new version.
type BaseImage struct {
    ID          string        `json:"id"`           // UUIDv4
    Name        string        `json:"name"`         // "rocky9-hpc-base"
    Version     string        `json:"version"`      // "1.0.3"
    OS          string        `json:"os"`           // "Rocky Linux 9.3"
    Arch        string        `json:"arch"`         // "x86_64", "aarch64"
    Status      ImageStatus   `json:"status"`       // building, ready, error, archived
    Format      ImageFormat   `json:"format"`       // filesystem, block
    SizeBytes   int64         `json:"size_bytes"`
    Checksum    string        `json:"checksum"`     // sha256 of the blob
    BlobPath    string        `json:"-"`            // server-local path, never sent to client
    DiskLayout  DiskLayout    `json:"disk_layout"`  // partition schema, not node MACs/IPs
    Tags        []string      `json:"tags"`
    CreatedAt   time.Time     `json:"created_at"`
    FinalizedAt *time.Time    `json:"finalized_at,omitempty"`
    SourceURL   string        `json:"source_url,omitempty"`  // original pull URL or ""
    Notes       string        `json:"notes"`
}

// NodeConfig holds everything that makes a deployed image specific to one
// physical node. Applied at deployment time by the clonr CLI on the target.
// Never baked into the BaseImage blob.
type NodeConfig struct {
    ID           string            `json:"id"`           // UUIDv4
    Hostname     string            `json:"hostname"`
    FQDN         string            `json:"fqdn"`
    PrimaryMAC   string            `json:"primary_mac"`
    Interfaces   []InterfaceConfig `json:"interfaces"`   // static IP assignments
    SSHKeys      []string          `json:"ssh_keys"`     // authorized public keys
    KernelArgs   string            `json:"kernel_args"`  // extra grub args
    Groups       []string          `json:"groups"`       // slurm partitions, roles
    CustomVars   map[string]string `json:"custom_vars"`  // freeform for hooks
    BaseImageID  string            `json:"base_image_id"`
    CreatedAt    time.Time         `json:"created_at"`
    UpdatedAt    time.Time         `json:"updated_at"`
}

type InterfaceConfig struct {
    MACAddress  string   `json:"mac_address"`
    Name        string   `json:"name"`        // "eth0", "ens3"
    IPAddress   string   `json:"ip_address"`  // CIDR notation: "192.168.1.50/24"
    Gateway     string   `json:"gateway,omitempty"`
    DNS         []string `json:"dns,omitempty"`
    MTU         int      `json:"mtu,omitempty"`
    Bond        string   `json:"bond,omitempty"` // bond device name if applicable
}

// DiskLayout describes the partition schema that must exist on the target
// for a deployment to proceed. It is part of BaseImage, not NodeConfig.
// The actual device paths (/dev/nvme0n1 vs /dev/sda) are resolved at
// deploy time by matching size and type constraints.
type DiskLayout struct {
    Partitions []PartitionSpec `json:"partitions"`
    Bootloader Bootloader      `json:"bootloader"`
}

type PartitionSpec struct {
    Label      string   `json:"label"`       // "boot", "root", "swap", "data"
    SizeBytes  int64    `json:"size_bytes"`  // 0 = fill remaining
    Filesystem string   `json:"filesystem"`
    MountPoint string   `json:"mountpoint"`
    Flags      []string `json:"flags"`       // ["boot", "esp"]
    MinBytes   int64    `json:"min_bytes"`   // minimum acceptable disk to match
}

type Bootloader struct {
    Type   string `json:"type"`   // "grub2", "systemd-boot"
    Target string `json:"target"` // "x86_64-efi", "i386-pc"
}
```

### pkg/image — ImageStore Interface

```go
// pkg/image/store.go

// ImageStore is the persistence boundary. The SQLite implementation
// lives here; a future S3-backed implementation would satisfy the same interface.
type ImageStore interface {
    // BaseImage operations
    CreateBaseImage(ctx context.Context, img BaseImage) error
    GetBaseImage(ctx context.Context, id string) (BaseImage, error)
    ListBaseImages(ctx context.Context, filter ImageFilter) ([]BaseImage, error)
    UpdateBaseImageStatus(ctx context.Context, id string, status ImageStatus, errMsg string) error
    FinalizeBaseImage(ctx context.Context, id string, sizeBytes int64, checksum string) error
    ArchiveBaseImage(ctx context.Context, id string) error

    // NodeConfig operations
    CreateNodeConfig(ctx context.Context, cfg NodeConfig) error
    GetNodeConfig(ctx context.Context, id string) (NodeConfig, error)
    GetNodeConfigByMAC(ctx context.Context, mac string) (NodeConfig, error)
    ListNodeConfigs(ctx context.Context, baseImageID string) ([]NodeConfig, error)
    UpdateNodeConfig(ctx context.Context, cfg NodeConfig) error
    DeleteNodeConfig(ctx context.Context, id string) error

    // Blob path management (server-internal)
    GetBlobPath(ctx context.Context, imageID string) (string, error)
    AllocateBlobPath(ctx context.Context, imageID string) (string, error)
}
```

### pkg/image — Chroot Safety Model

The chroot lifecycle is where silent failures cause corrupted images. The mount/unmount ordering is strict: bind mounts must be unmounted in reverse order, and cleanup must happen even on panic via defer chains.

```go
// pkg/image/chroot.go

// ChrootSession represents a mounted chroot environment.
// Obtain via NewChrootSession; always defer session.Close().
type ChrootSession struct {
    rootDir    string
    mounts     []string    // ordered list of mounted paths, unmounted in reverse
    closed     bool
}

// MountOrder defines the mandatory bind mount sequence.
// These must be unmounted in reverse: devpts, dev, sys, proc.
var MountOrder = []MountSpec{
    {Source: "proc",     Target: "proc",    FSType: "proc",  Flags: ""},
    {Source: "sys",      Target: "sys",     FSType: "sysfs", Flags: ""},
    {Source: "/dev",     Target: "dev",     FSType: "",      Flags: "bind"},
    {Source: "/dev/pts", Target: "dev/pts", FSType: "",      Flags: "bind"},
}

// NewChrootSession mounts proc/sys/dev in order and returns a session.
// On any mount failure, already-mounted filesystems are unmounted before
// returning the error. This function requires root privileges.
func NewChrootSession(ctx context.Context, rootDir string) (*ChrootSession, error)

// RunInChroot executes a command inside the chroot.
func (s *ChrootSession) RunInChroot(ctx context.Context, argv []string, env []string) error

// Shell drops an interactive shell into the chroot (clonr shell <image>).
func (s *ChrootSession) Shell(ctx context.Context) error

// InjectFile writes content to a path relative to the chroot root.
func (s *ChrootSession) InjectFile(relPath string, content []byte, mode os.FileMode) error

// Close unmounts all bind mounts in reverse order. Safe to call multiple times.
func (s *ChrootSession) Close() error
```

### pkg/deploy

```go
// pkg/deploy/deploy.go

// Deployer is the interface implemented by rsync and block deployers.
type Deployer interface {
    Preflight(ctx context.Context, layout DiskLayout, hw HardwareProfile) error
    Deploy(ctx context.Context, opts DeployOpts, progress ProgressFunc) error
    Finalize(ctx context.Context, cfg NodeConfig, mountRoot string) error
}

type DeployOpts struct {
    ImageID     string
    ServerURL   string
    AuthToken   string
    TargetDisk  string      // resolved by Preflight from DiskLayout
    NodeConfig  NodeConfig
}

type ProgressFunc func(bytesWritten, totalBytes int64, phase string)
```

---

## SQLite Schema

```sql
-- migrations/001_initial.sql

CREATE TABLE base_images (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    version         TEXT NOT NULL,
    os              TEXT NOT NULL,
    arch            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'building',
    format          TEXT NOT NULL,
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    checksum        TEXT NOT NULL DEFAULT '',
    blob_path       TEXT NOT NULL DEFAULT '',
    disk_layout     TEXT NOT NULL DEFAULT '{}',
    tags            TEXT NOT NULL DEFAULT '[]',
    source_url      TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    error_message   TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    finalized_at    INTEGER
);

CREATE INDEX idx_base_images_status ON base_images(status);
CREATE INDEX idx_base_images_name ON base_images(name, version);

CREATE TABLE node_configs (
    id              TEXT PRIMARY KEY,
    hostname        TEXT NOT NULL,
    fqdn            TEXT NOT NULL,
    primary_mac     TEXT NOT NULL,
    interfaces      TEXT NOT NULL DEFAULT '[]',
    ssh_keys        TEXT NOT NULL DEFAULT '[]',
    kernel_args     TEXT NOT NULL DEFAULT '',
    groups          TEXT NOT NULL DEFAULT '[]',
    custom_vars     TEXT NOT NULL DEFAULT '{}',
    base_image_id   TEXT NOT NULL REFERENCES base_images(id),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE UNIQUE INDEX idx_node_configs_mac ON node_configs(primary_mac);
CREATE INDEX idx_node_configs_base_image ON node_configs(base_image_id);
CREATE INDEX idx_node_configs_hostname ON node_configs(hostname);

CREATE TABLE build_jobs (
    id              TEXT PRIMARY KEY,
    image_id        TEXT NOT NULL REFERENCES base_images(id),
    job_type        TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'queued',
    source_url      TEXT NOT NULL DEFAULT '',
    log_path        TEXT NOT NULL DEFAULT '',
    started_at      INTEGER,
    completed_at    INTEGER,
    created_at      INTEGER NOT NULL
);

CREATE INDEX idx_build_jobs_image ON build_jobs(image_id);
CREATE INDEX idx_build_jobs_status ON build_jobs(status);
```

```sql
-- migrations/002_node_config.sql

CREATE TABLE deploy_events (
    id              TEXT PRIMARY KEY,
    node_config_id  TEXT NOT NULL REFERENCES node_configs(id),
    image_id        TEXT NOT NULL REFERENCES base_images(id),
    triggered_by    TEXT NOT NULL DEFAULT 'cli',
    status          TEXT NOT NULL,
    error_message   TEXT NOT NULL DEFAULT '',
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    deployed_at     INTEGER NOT NULL
);
```

---

## REST API Surface (clonr-serverd)

All endpoints under `/api/v1/`. Auth via `Authorization: Bearer <token>` header.

### Images

```
GET    /api/v1/images                     # List images (?status=ready&os=Rocky)
POST   /api/v1/images                     # Create image record
GET    /api/v1/images/:id                 # Get image details
DELETE /api/v1/images/:id                 # Archive image

GET    /api/v1/images/:id/status          # Poll build status
GET    /api/v1/images/:id/log             # Stream build log (SSE)

GET    /api/v1/images/:id/blob            # Download image blob (range-request)
POST   /api/v1/images/:id/blob            # Upload image blob

GET    /api/v1/images/:id/disklayout      # Get disk layout JSON
PUT    /api/v1/images/:id/disklayout      # Replace disk layout JSON
```

### Image Factory

```
POST   /api/v1/factory/pull               # Pull from URL
POST   /api/v1/factory/import             # Import uploaded ISO
POST   /api/v1/factory/capture            # Register captured image
POST   /api/v1/images/:id/shell-session   # Allocate chroot shell (returns websocket URL)
GET    /api/v1/images/:id/shell-session/:sid  # WebSocket for interactive shell
DELETE /api/v1/images/:id/shell-session/:sid  # Terminate shell session
```

### Node Configs

```
GET    /api/v1/nodes                      # List node configs
POST   /api/v1/nodes                      # Create node config
GET    /api/v1/nodes/:id                  # Get node config
PUT    /api/v1/nodes/:id                  # Replace node config
DELETE /api/v1/nodes/:id                  # Delete node config
GET    /api/v1/nodes/by-mac/:mac          # Lookup by MAC (PXE/CLI boot)
```

### Deploy & System

```
POST   /api/v1/deploy/events              # CLI posts deployment result
GET    /api/v1/deploy/events              # Deployment history
GET    /api/v1/health                     # Health check
GET    /api/v1/config                     # Server config (non-secret)
```

---

## Image Lifecycle: Ingest → Store → Customize → Deploy

```
INGEST                    STORE                  CUSTOMIZE             DEPLOY
──────                    ─────                  ─────────             ──────
pull-image URL            AllocateBlobPath()     NewChrootSession()    clonr CLI boots on node
     │                         │                      │                      │
     ▼                         ▼                      ▼                      ▼
HTTP download             /images/<id>/          mount proc/sys/dev    HardwareProfile.Discover()
(streaming, resume)       blob.tar.gz OR         in order                    │
     │                    blob.img                    │                      ▼
     ▼                         │                      ▼               GET /nodes/by-mac/:mac
checksum verify           SQLite row:            RunInChroot(dnf)      → NodeConfig
     │                    status=building             │                      │
     ▼                         │                      ▼                      ▼
FinalizeBaseImage()       blob written           InjectFile(ssh_keys)  GET /images/:id/blob
status=ready              status=ready                │                (streaming, range)
                                                      ▼                      │
                                               session.Close()               ▼
                                               (unmount reverse)      Deployer.Preflight()
                                                                       disk size/arch check
                                                                             │
                                                                             ▼
                                                                       Deployer.Deploy()
                                                                       (rsync or partclone)
                                                                             │
                                                                             ▼
                                                                       Deployer.Finalize()
                                                                       apply NodeConfig
                                                                             │
                                                                             ▼
                                                                       fix-efiboot (if needed)
                                                                       POST /deploy/events
```

---

## CLI Subcommand Routing

```
clonr
├── image
│   ├── new        → POST /api/v1/images
│   ├── pull       → POST /api/v1/factory/pull
│   ├── import     → POST /api/v1/factory/import
│   ├── list       → GET /api/v1/images
│   ├── details    → GET /api/v1/images/:id
│   └── archive    → DELETE /api/v1/images/:id
├── shell          → WebSocket to /api/v1/images/:id/shell-session/:sid
├── fs             → PUT /api/v1/images/:id + chroot RunInChroot on server
├── disklayout
│   ├── get        → GET /api/v1/images/:id/disklayout
│   └── upload     → PUT /api/v1/images/:id/disklayout
├── node
│   ├── list       → GET /api/v1/nodes
│   ├── config     → GET /api/v1/nodes/:id or by-mac
│   └── set        → PUT /api/v1/nodes/:id
├── hardware       → Local: runs Discover(), prints HardwareProfile JSON
├── identify       → Blink NIC LED via ethtool
├── bios           → dmidecode + IPMI
├── deploy         → Full deployment: Preflight → Deploy → Finalize
├── multicast      → udpcast receiver mode
└── fix-efiboot    → Repair EFI boot entries
```

---

## Key Architectural Decisions

1. **BaseImage vs NodeConfig separation enforced at API level.** One image serves 200 nodes. Node identity is never baked into blobs.

2. **Pure-Go SQLite (`modernc.org/sqlite`).** Keeps both binaries buildable with CGO_ENABLED=0. ~10-15% slower writes, irrelevant for metadata.

3. **chi over Gin for HTTP router.** Composes with standard net/http middleware. No custom context type friction.

4. **Chroot shell via WebSocket, not SSH.** Inherits existing TLS + token auth. No key management overhead.

5. **Async build jobs with SSE log streaming.** Correct pattern for 1-10 minute operations (image pulls, ISO imports).

6. **No auth system at v1.** Single pre-shared API token. HPC environments are air-gapped and operator-administered. RBAC deferred until real requirement exists.

7. **No ORM.** Hand-written SQL queries in typed wrappers. Schema is simple enough that an ORM adds complexity without benefit.
