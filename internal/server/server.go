// Package server provides the clustr-serverd HTTP API built on chi.
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
	"github.com/sqoia-dev/clustr/pkg/api"
	"github.com/sqoia-dev/clustr/internal/config"
	"github.com/sqoia-dev/clustr/internal/db"
	"github.com/sqoia-dev/clustr/internal/image"
	ldapmodule "github.com/sqoia-dev/clustr/internal/ldap"
	networkmodule "github.com/sqoia-dev/clustr/internal/network"
	slurmmodule "github.com/sqoia-dev/clustr/internal/slurm"
	"github.com/sqoia-dev/clustr/internal/metrics"
	"github.com/sqoia-dev/clustr/internal/power"
	"github.com/sqoia-dev/clustr/internal/sysaccounts"
	ipmipower "github.com/sqoia-dev/clustr/internal/power/ipmi"
	proxmoxpower "github.com/sqoia-dev/clustr/internal/power/proxmox"
	"github.com/sqoia-dev/clustr/internal/reimage"
	"github.com/sqoia-dev/clustr/internal/notifications"
	"github.com/sqoia-dev/clustr/internal/alerts"
	"github.com/sqoia-dev/clustr/internal/allocation"
	"github.com/sqoia-dev/clustr/internal/selector"
	"github.com/sqoia-dev/clustr/internal/multicast"
	"github.com/sqoia-dev/clustr/internal/server/eventbus"
	"github.com/sqoia-dev/clustr/internal/server/handlers"
	portalhandler "github.com/sqoia-dev/clustr/internal/server/handlers/portal"
	webui "github.com/sqoia-dev/clustr/internal/server/web"
	"github.com/sqoia-dev/clustr/internal/webhook"
)

// BuildInfo holds build-time metadata injected via -ldflags.
type BuildInfo struct {
	Version   string
	CommitSHA string
	BuildTime string

	// Slurm bundle metadata compiled into the binary.
	SlurmVersion       string // e.g. "24.11.4"
	SlurmBundleVersion string // e.g. "v24.11.4-clustr5"
	SlurmBundleSHA256  string // bundle tarball SHA256 hex
}

// Server wraps the HTTP server and all its dependencies.
type Server struct {
	cfg                 config.ServerConfig
	db                  *db.DB
	audit               *db.AuditService
	broker              *LogBroker
	progress            *ProgressStore
	imageEvents         *ImageEventStore
	groupReimageEvents  *GroupReimageEventStore
	buildProgress       *BuildProgressStore
	shells              *image.ShellManager
	powerCache          *PowerCache
	powerRegistry       *power.Registry
	reimageOrchestrator *reimage.Orchestrator
	ldapMgr             *ldapmodule.Manager
	sysAccountsMgr      *sysaccounts.Manager
	networkMgr          *networkmodule.Manager
	slurmMgr            *slurmmodule.Manager
	clientdHub          *ClientdHub
	webhookDispatcher   *webhook.Dispatcher
	sessionSecret       []byte // HMAC key for browser session tokens
	router              chi.Router
	http                *http.Server
	logsHandler         *handlers.LogsHandler
	imgFactory          *image.Factory
	buildInfo           BuildInfo

	// notifier is the primary Notifier instance built by buildRouter and used by
	// StartBackgroundWorkers for the digest queue processor (E4).
	notifier *notifications.Notifier

	// flipBackFailureCount tracks verify-boot flipNodeToDiskFirst failures for
	// the /health endpoint (S4-9). Incremented atomically; read without lock for
	// health response since occasional skew is acceptable.
	flipBackFailureCount int64

	// dhcpLeaseLookup is set after construction by SetDHCPServer when the PXE
	// server is available. The NodesHandler captures it via a closure so it picks
	// up the live function even after the router is built.
	dhcpLeaseLookup func(mac string) net.IP

	// lastContentionRate caches the most recent SQLite write-contention rate (events/sec)
	// computed by the tech-trig evaluator between ticks. Used by T1 evaluation.
	// Stored as an atomicFloat64 (see tech_trig_worker.go) to avoid locking overhead.
	lastContentionRate atomicFloat64

	// alertSilences is the Sprint 24 #155 silence store.  Initialised alongside alertStore.
	alertSilences *alerts.SilenceStore

	// alertEngine is the #133 alert rule engine.  Initialised in buildRouter()
	// (after the notifier is available) and started in StartBackgroundWorkers().
	alertEngine      *alerts.Engine
	alertStore       *alerts.StateStore
	alertDispatcher  *alerts.Dispatcher

	// multicastScheduler is the Sprint 25 #157 UDPCast fleet-reimage scheduler.
	// Initialised in buildRouter() after the DB is available.
	multicastScheduler *multicast.Scheduler

	// eventBus is the UX-4 multiplexed event bus. One Bus instance fans events
	// from all internal producers (image lifecycle, node heartbeats, group
	// reimage, etc.) to all active GET /api/v1/events SSE subscribers.
	eventBus *eventbus.Bus

	// reconcileCache and reconcileMu support the image blob reconciler (#247).
	// reconcileCache maps imageID → cached reconcile result (mtime-keyed).
	// reconcileMu guards both reconcileCache and prevents concurrent reconcile
	// passes for the same image from overlapping.
	//
	// THREAD-SAFETY: reconcileCache (map[string]*reconCacheEntry) is ONLY ever
	// read or written while reconcileMu is held. No other goroutine may access
	// the map directly. This invariant is enforced by the single entry point
	// ReconcileImage which acquires reconcileMu before delegating to
	// reconcileImageLocked. evictReconcileCache() documents "caller must hold mu".
	reconcileMu    sync.Mutex
	reconcileCache map[string]*reconCacheEntry

	// systemHandler holds the handler built in buildRouter() so that
	// SetDHCPLeasesOnSystemHandler can wire DHCPLeases after buildRouter returns
	// (the PXE server is started after the HTTP server is built).
	systemHandler *handlers.SystemHandler
}

// buildProgressAdapter adapts *BuildProgressStore to image.BuildProgressReporter.
// The image package defines an interface with Start returning a BuildHandle interface;
// this adapter bridges the concrete server types to that interface.
type buildProgressAdapter struct {
	store *BuildProgressStore
}

// buildHandleAdapter wraps *BuildHandle (server) to satisfy image.BuildHandle.
type buildHandleAdapter struct {
	h *BuildHandle
}

func (a buildHandleAdapter) SetPhase(phase string)      { a.h.SetPhase(phase) }
func (a buildHandleAdapter) SetProgress(d, t int64)     { a.h.SetProgress(d, t) }
func (a buildHandleAdapter) AddSerialLine(line string)   { a.h.AddSerialLine(line) }
func (a buildHandleAdapter) AddStderrLine(line string)   { a.h.AddStderrLine(line) }
func (a buildHandleAdapter) Fail(msg string)             { a.h.Fail(msg) }
func (a buildHandleAdapter) Complete()                   { a.h.Complete() }

func (a buildProgressAdapter) Start(imageID string) image.BuildHandle {
	h := a.store.Start(imageID)
	return buildHandleAdapter{h: h}
}

// New creates a Server wired with the given config and database.
func New(cfg config.ServerConfig, database *db.DB, info BuildInfo) *Server {
	// Build the power provider registry and register all supported backends.
	registry := power.NewRegistry()
	ipmipower.Register(registry)
	proxmoxpower.Register(registry)

	reimageOrch := reimage.New(database, registry, log.Logger)

	shells := image.NewShellManager(database, cfg.ImageDir, log.Logger)
	buildProg := NewBuildProgressStore(cfg.ImageDir)

	// Resolve or generate the session HMAC secret.
	var secret []byte
	if cfg.SessionSecret != "" {
		secret = []byte(cfg.SessionSecret)
	} else {
		var err error
		secret, err = generateSessionSecret()
		if err != nil {
			log.Fatal().Err(err).Msg("server: failed to generate session secret")
		}
		log.Warn().Msg("CLUSTR_SESSION_SECRET not set — generated ephemeral session secret (sessions will not survive restarts)")
	}

	ldapMgr := ldapmodule.New(cfg, database)
	sysAccountsMgr := sysaccounts.New(database, ldapMgr.Allocator())
	networkMgr := networkmodule.New(database)

	// clientdHub must be created before SlurmManager so the hub reference is valid.
	hub := NewClientdHub()

	slurmMgr := slurmmodule.New(database, hub)
	// GAP-20: wire audit service into slurm manager for config change recording.
	slurmMgr.Audit = db.NewAuditService(database)

	// Wire the hub into the LDAP manager so Enable() can fanout CA cert +
	// sssd.conf pushes to enrolled nodes after a CA rotation (#109/#110).
	ldapMgr.SetHub(hub)
	// Wire staging DB into LDAP and network managers for two-stage commit (#154).
	ldapMgr.SetStagingDB(database)
	networkMgr.SetStagingDB(database)

	groupReimageEvents := NewGroupReimageEventStore()
	reimageOrch.Events = groupReimageEvents

	bus := eventbus.New()

	// UX-4: wire the event bus into existing stores so every Publish call also
	// fans the event to the multiplexed /api/v1/events SSE stream.
	imageEventsStore := NewImageEventStore()
	imageEventsStore.SetBus(bus)
	groupReimageEvents.SetBus(bus)

	logBroker := NewLogBroker()
	logBroker.SetBus(bus)

	s := &Server{
		cfg:                 cfg,
		db:                  database,
		audit:               db.NewAuditService(database),
		broker:              logBroker,
		progress:            NewProgressStore(),
		imageEvents:         imageEventsStore,
		groupReimageEvents:  groupReimageEvents,
		buildProgress:       buildProg,
		shells:              shells,
		powerCache:          NewPowerCache(15 * time.Second),
		powerRegistry:       registry,
		reimageOrchestrator: reimageOrch,
		ldapMgr:             ldapMgr,
		sysAccountsMgr:      sysAccountsMgr,
		networkMgr:          networkMgr,
		slurmMgr:            slurmMgr,
		clientdHub:          hub,
		webhookDispatcher:   webhook.New(database, log.Logger),
		sessionSecret:       secret,
		buildInfo:           info,
		eventBus:            bus,
		reconcileCache:      make(map[string]*reconCacheEntry), // #247: image blob reconciler cache
	}
	s.router = s.buildRouter()
	s.http = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

// NetworkManager returns the server's network module manager.
// Used by main to wire external callbacks (e.g. DHCP switch auto-discovery).
func (s *Server) NetworkManager() *networkmodule.Manager {
	return s.networkMgr
}

// SysAccountsManager returns the server's system accounts manager.
// Used by main to run the optional posixid reconciliation pass.
func (s *Server) SysAccountsManager() *sysaccounts.Manager {
	return s.sysAccountsMgr
}

// ClientdHub returns the server's clientd hub.
// Used by main to wire the posixid reconciliation pass.
func (s *Server) ClientdHub() *ClientdHub {
	return s.clientdHub
}

// SetDHCPLeaseLookup wires a DHCP lease lookup function so the registration
// handler can auto-populate node interfaces from DHCP-assigned IPs. Call this
// after server.New() when the PXE server is available. Safe to call with nil
// to disable the feature (e.g. when PXE is disabled).
func (s *Server) SetDHCPLeaseLookup(fn func(mac string) net.IP) {
	s.dhcpLeaseLookup = fn
}

// SetDHCPLeasesOnSystemHandler wires the DHCP lease source into the system
// handler's PXE-in-flight detection. Call this alongside SetDHCPLeaseLookup
// when the PXE server becomes available.  Safe to call with nil to leave
// pxe_in_flight empty (e.g. when PXE is disabled).
func (s *Server) SetDHCPLeasesOnSystemHandler(leases handlers.DHCPLeasesIface) {
	if s.systemHandler != nil {
		s.systemHandler.DHCPLeases = leases
	}
}

// lookupDHCPLease is the closure passed to NodesHandler. It delegates to the
// dhcpLeaseLookup field, which may be set after router construction.
func (s *Server) lookupDHCPLease(mac string) net.IP {
	if s.dhcpLeaseLookup == nil {
		return nil
	}
	return s.dhcpLeaseLookup(mac)
}

// StartBackgroundWorkers starts long-running background goroutines.
// Call this after New() and before ListenAndServe().
func (s *Server) StartBackgroundWorkers(ctx context.Context) {
	// Wire the server-lifetime context into the logs ingest handler so that
	// client disconnects (r.Context() cancellations) do not abort in-flight
	// SQLite log-batch transactions and silently drop deploy logs.
	s.logsHandler.ServerCtx = ctx
	// Wire shutdown context into the image factory so async build goroutines
	// are cancelled on graceful shutdown and the semaphore respects context.
	if s.imgFactory != nil {
		s.imgFactory.SetContext(ctx)
	}
	go s.reimageOrchestrator.Scheduler(ctx)
	go s.runLogPurger(ctx)
	go s.runAuditPurger(ctx)
	go s.runDiskSpaceMonitor(ctx)
	// ADR-0008: Post-reboot verification timeout scanner.
	go s.runVerifyTimeoutScanner(ctx)
	// S4-1: Prometheus gauge collector.
	go s.runMetricsCollector(ctx)
	// S4-3: Reimage-pending reaper — clears orphaned reimage_pending flags.
	go s.runReimagePendingReaper(ctx)
	// S4-4: Resume any group reimage jobs that were running before this process started.
	s.resumeRunningGroupReimageJobs(ctx)
	// LDAP module health checker.
	s.ldapMgr.StartBackgroundWorkers(ctx)
	// Slurm module health checker.
	s.slurmMgr.StartBackgroundWorkers(ctx)
	// E4: Digest queue processor — sends batched notification digests.
	if s.notifier != nil {
		go s.runDigestProcessor(ctx)
	}
	// F3: Allocation expiration scanner — sends warnings at 30/14/7 days.
	go s.runExpirationScanner(ctx)
	// H3: Auto-policy finalizer — closes the 24h undo window.
	go s.runAutoPolicyFinalizer(ctx)
	// M1: TECH-TRIG signal evaluator — 10-minute tick (Sprint M, v1.11.0).
	go s.runTechTrigEvaluator(ctx)
	// Sprint 22 #131: stats retention sweeper + Prometheus exposition cache.
	go s.runStatsRetentionSweeper(ctx)
	go s.runStatsPrometheusRefresher(ctx)
	// Sprint 22 #133: alert rule engine.  Start SMTP worker pool before engine.
	if s.alertDispatcher != nil {
		s.alertDispatcher.Start(ctx)
	}
	if s.alertEngine != nil {
		go s.alertEngine.Run(ctx)
	}
	// #250: Periodic image blob reconciler — default 6h, configurable via
	// CLUSTR_RECONCILE_INTERVAL (0 = disabled).
	go s.runImageReconciler(ctx, reconcileInterval())
	// #243: SELF-MON — control-plane host self-monitoring.
	go s.runSelfmon(ctx)
	// #250 Q2: api_keys expiration sweeper.
	// Reaps any api_keys row whose expires_at is in the past so the table doesn't
	// accumulate dead node-scope tokens (24h TTL each, minted on every PXE boot).
	go s.runAPIKeySweeper(ctx)
}

// runAPIKeySweeper ticks every 5 minutes and DELETEs api_keys rows whose
// expires_at is in the past. Lifecycle: started by StartBackgroundWorkers,
// cancelled when ctx is cancelled (graceful shutdown via the SIGTERM/SIGINT
// handler in cmd/clustr-serverd/main.go).  Issues only DELETEs against the
// shared *sql.DB, so no goroutine-local state to protect — modernc.org/sqlite
// serialises writes via the single-conn pool already (db.go:62).
func (s *Server) runAPIKeySweeper(ctx context.Context) {
	const interval = 5 * time.Minute
	log.Info().Str("interval", interval.String()).Msg("api-key sweeper: started")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("api-key sweeper: stopping")
			return
		case <-ticker.C:
			n, err := s.db.SweepExpiredAPIKeys(ctx, time.Now())
			if err != nil {
				log.Error().Err(err).Msg("api-key sweeper: failed")
				continue
			}
			if n > 0 {
				log.Info().Int64("rows", n).Msg("api-key sweeper: deleted expired rows")
			}
		}
	}
}

// runDigestProcessor polls the notification digest queue every hour and sends
// any entries whose scheduled_for time has passed. This is the delivery side
// of the digest mode scaffold introduced in Sprint E (E4, CF-11/CF-15).
func (s *Server) runDigestProcessor(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	log.Info().Msg("digest-processor: started (hourly poll)")
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("digest-processor: shutting down")
			return
		case <-ticker.C:
			s.flushDigestQueue(ctx)
		}
	}
}

// flushDigestQueue drains all due digest entries from the queue and sends them.
func (s *Server) flushDigestQueue(ctx context.Context) {
	entries, err := s.db.PollDigestQueue(ctx, time.Now())
	if err != nil {
		log.Error().Err(err).Msg("digest-processor: poll failed")
		return
	}
	if len(entries) == 0 {
		return
	}
	log.Info().Int("count", len(entries)).Msg("digest-processor: sending queued digests")

	// Group entries by recipient for batching.
	type digestBatch struct {
		email    string
		subjects []string
		bodies   []string
	}
	byEmail := map[string]*digestBatch{}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.ID)
		if byEmail[e.RecipientEmail] == nil {
			byEmail[e.RecipientEmail] = &digestBatch{email: e.RecipientEmail}
		}
		b := byEmail[e.RecipientEmail]
		b.subjects = append(b.subjects, e.Subject)
		b.bodies = append(b.bodies, e.BodyText)
	}

	// Send one digest email per recipient.
	for _, batch := range byEmail {
		subject := "clustr digest"
		if len(batch.subjects) == 1 {
			subject = batch.subjects[0]
		} else {
			subject = fmt.Sprintf("clustr digest (%d updates)", len(batch.subjects))
		}
		body := strings.Join(batch.bodies, "\n\n---\n\n")
		if err := s.notifier.Mailer.Send(ctx, []string{batch.email}, subject, body); err != nil {
			log.Error().Err(err).Str("email", batch.email).Msg("digest-processor: send failed")
			continue
		}
	}

	// Delete delivered entries.
	if err := s.db.DeleteDigestEntries(ctx, ids); err != nil {
		log.Error().Err(err).Msg("digest-processor: delete failed after send")
	}
}

// defaultFlipSemCap is the default max concurrent flipNodeToDiskFirst goroutines
// in the verify-boot timeout scanner (S4-6). Override with CLUSTR_FLIP_CONCURRENCY.
const defaultFlipSemCap = 5

// scannerFlipSemaphore returns a buffered channel used as a semaphore to bound
// the number of concurrent flipNodeToDiskFirst calls in the verify-boot scanner.
// A new channel is created on each tick cycle — cheap (just scanner goroutines,
// not a hot path) and avoids shared state between tick cycles.
func scannerFlipSemaphore() chan struct{} {
	cap := defaultFlipSemCap
	if v := os.Getenv("CLUSTR_FLIP_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cap = n
		}
	}
	return make(chan struct{}, cap)
}

// runVerifyTimeoutScanner ticks every 60 seconds and marks as timed-out any node
// that has deploy_completed_preboot_at set but no deploy_verified_booted_at within
// CLUSTR_VERIFY_TIMEOUT. ADR-0008.
func (s *Server) runVerifyTimeoutScanner(ctx context.Context) {
	timeout := s.cfg.VerifyTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute // safe default if config somehow zero
	}
	log.Info().Str("timeout", timeout.String()).Msg("verify-boot scanner: started — post-reboot verification timeout set to " + timeout.String())

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("verify-boot scanner: stopping")
			return
		case <-ticker.C:
			// Migration 054: fetch all awaiting-verification nodes (deploy_completed_preboot_at
			// <= now) so we can apply per-node timeout overrides in Go.
			// Nodes with a longer per-node override will not be timed-out by the global cutoff.
			now := time.Now()
			nodes, err := s.db.ListNodesAwaitingVerification(ctx, now)
			if err != nil {
				log.Error().Err(err).Msg("verify-boot scanner: ListNodesAwaitingVerification failed")
				continue
			}
			// S4-6: Fan out flipNodeToDiskFirst calls via goroutines with a bounded
			// semaphore (default 5 concurrent) to prevent the scanner from blocking
			// sequentially on simultaneous timeouts at 200-node clusters.
			flipSem := scannerFlipSemaphore()
			var wg sync.WaitGroup
			for _, n := range nodes {
				n := n // capture

				// Migration 054: compute effective timeout for this node.
				effectiveTimeout := timeout
				if n.VerifyTimeoutOverride != nil {
					if *n.VerifyTimeoutOverride == 0 {
						// Zero means disabled for this node — skip.
						continue
					}
					effectiveTimeout = time.Duration(*n.VerifyTimeoutOverride) * time.Second
				}
				// Only fire if the node has actually exceeded its effective timeout.
				if n.DeployCompletedPrebootAt == nil || now.Sub(*n.DeployCompletedPrebootAt) < effectiveTimeout {
					continue
				}

				wg.Add(1)
				flipSem <- struct{}{} // acquire slot
				go func() {
					defer func() { <-flipSem; wg.Done() }()

					if err := s.db.RecordVerifyTimeout(ctx, n.ID); err != nil {
						log.Error().Err(err).Str("node_id", n.ID).Str("hostname", n.Hostname).
							Msg("verify-boot scanner: RecordVerifyTimeout failed")
						return
					}
					// S4-2: fire verify_boot.timeout webhook.
					if s.webhookDispatcher != nil {
						s.webhookDispatcher.Dispatch(ctx, webhook.EventVerifyBootTimeout, webhook.Payload{
							NodeID:  n.ID,
							ImageID: n.BaseImageID,
						})
					}
					log.Warn().
						Str("node_id", n.ID).
						Str("hostname", n.Hostname).
						Str("timeout", effectiveTimeout.String()).
						Msgf("verify-boot scanner: node %s (%s) did not phone home within %s of deploy-complete — possible bootloader failure, kernel panic, or /etc/clustr/node-token not written correctly",
							n.ID, n.Hostname, effectiveTimeout)
					// Flip persistent boot order back to disk-first on deploy-timeout.
					// Prevents Proxmox VMs from being stuck PXE-first forever when the
					// deploy completes but the node never calls verify-boot.
					// Best-effort: errors are logged, not fatal. See docs/boot-architecture.md §10.
					if err := s.flipNodeToDiskFirst(ctx, n.ID); err != nil {
						log.Warn().Err(err).Str("node_id", n.ID).Str("hostname", n.Hostname).
							Msg("verify-boot scanner: FlipToDiskFirst failed on deploy-timeout (non-fatal)")
						// S4-9: track flip-back failures in Prometheus and health endpoint.
						metrics.FlipBackFailures.Inc()
						atomic.AddInt64(&s.flipBackFailureCount, 1)
					} else {
						log.Info().Str("node_id", n.ID).Str("hostname", n.Hostname).
							Msg("verify-boot scanner: persistent boot order flipped to disk-first after deploy-timeout")
					}
				}()
			}
			wg.Wait()
		}
	}
}

// flipNodeToDiskFirst resolves the power provider for nodeID and calls
// SetPersistentBootOrder([BootDisk, BootPXE]) to restore the disk-first
// persistent boot order after a successful deploy or deploy-timeout.
//
// On Proxmox this triggers an explicit stop+start (via the provider's
// SetPersistentBootOrder implementation) so the config change is committed.
// On IPMI this is a best-effort harmless reaffirmation of the one-shot
// override that was already consumed on the previous boot.
//
// Returns an error if the provider cannot be resolved or the call fails.
// Callers should treat errors as non-fatal warnings, not hard failures.
//
// See docs/boot-architecture.md §10.
func (s *Server) flipNodeToDiskFirst(ctx context.Context, nodeID string) error {
	node, err := s.db.GetNodeConfig(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("flipNodeToDiskFirst: load node %s: %w", nodeID, err)
	}

	var provCfg power.ProviderConfig
	switch {
	case node.PowerProvider != nil && node.PowerProvider.Type != "":
		provCfg = power.ProviderConfig{
			Type:   node.PowerProvider.Type,
			Fields: node.PowerProvider.Fields,
		}
	case node.BMC != nil && node.BMC.IPAddress != "":
		provCfg = power.ProviderConfig{
			Type: "ipmi",
			Fields: map[string]string{
				"host":     node.BMC.IPAddress,
				"username": node.BMC.Username,
				"password": node.BMC.Password,
			},
		}
	default:
		// No provider configured — nothing to flip. Bare-metal nodes with no
		// power provider use operator-managed BMC boot order; no clustr action needed.
		return nil
	}

	provider, err := s.powerRegistry.Create(provCfg)
	if err != nil {
		return fmt.Errorf("flipNodeToDiskFirst: resolve provider for node %s: %w", nodeID, err)
	}

	if err := provider.SetPersistentBootOrder(ctx, []power.BootDevice{power.BootDisk, power.BootPXE}); err != nil {
		if errors.Is(err, power.ErrNotSupported) {
			// Provider has no persistent-order concept — that's fine.
			return nil
		}
		return fmt.Errorf("flipNodeToDiskFirst: SetPersistentBootOrder for node %s: %w", nodeID, err)
	}
	return nil
}

// runLogPurger ticks every hour and applies two-pass log eviction (D2):
//
//  Pass 1 — TTL: delete rows older than CLUSTR_LOG_RETENTION (default 7d).
//  Pass 2 — per-node cap: for each node exceeding CLUSTR_LOG_MAX_ROWS_PER_NODE
//            (default 50000), delete the oldest rows until it is at the cap.
//
// Each cycle appends one row to node_logs_summary for audit purposes.
// Uses the server-lifetime context so it shuts down cleanly on SIGTERM.
func (s *Server) runLogPurger(ctx context.Context) {
	retention := 7 * 24 * time.Hour // default 7 days (D2: changed from 14d)
	if v := s.cfg.LogRetention; v != 0 {
		retention = v
	}
	maxRowsPerNode := int64(50000) // default 50K rows per node (D2)
	if v := s.cfg.LogMaxRowsPerNode; v != 0 {
		maxRowsPerNode = v
	}
	log.Info().
		Str("retention", retention.String()).
		Int64("max_rows_per_node", maxRowsPerNode).
		Msg("log purger: started")

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("log purger: stopping")
			return
		case <-ticker.C:
			// Pass 1: TTL eviction.
			olderThan := time.Now().Add(-retention)
			ttlRows, err := s.db.PurgeLogs(ctx, olderThan)
			if err != nil {
				log.Error().Err(err).Str("older_than", olderThan.Format(time.RFC3339)).
					Msg("log purger: TTL pass failed")
				continue
			}

			// Pass 2: per-node cap eviction.
			capRows, nodesAffected, err := s.db.PurgeLogsPerNodeCap(ctx, maxRowsPerNode)
			if err != nil {
				log.Error().Err(err).Int64("max_rows_per_node", maxRowsPerNode).
					Msg("log purger: per-node cap pass failed")
				// Don't skip summary; record what we have so far.
			}

			total := ttlRows + capRows
			log.Info().
				Int64("ttl_rows", ttlRows).
				Int64("cap_rows", capRows).
				Int64("total_rows", total).
				Int64("nodes_capped", nodesAffected).
				Str("retention", retention.String()).
				Int64("max_rows_per_node", maxRowsPerNode).
				Msg("log purger: purge complete")

			// Record summary event for audit trail.
			summary := db.LogPurgeSummaryRow{
				ID:            generatePurgeID(),
				PurgedAt:      time.Now().UTC(),
				TTLRows:       ttlRows,
				CapRows:       capRows,
				TotalRows:     total,
				RetentionSecs: int64(retention.Seconds()),
				MaxRowsCap:    maxRowsPerNode,
				NodeCount:     nodesAffected,
			}
			if serr := s.db.RecordLogPurgeSummary(ctx, summary); serr != nil {
				log.Warn().Err(serr).Msg("log purger: failed to record summary (non-fatal)")
			}
		}
	}
}

// generatePurgeID returns a short unique ID for a purge summary row.
func generatePurgeID() string {
	return fmt.Sprintf("purge-%d", time.Now().UnixNano())
}

// runAuditPurger ticks every hour and deletes audit_log rows older than
// CLUSTR_AUDIT_RETENTION (default 90 days, D13).
func (s *Server) runAuditPurger(ctx context.Context) {
	retention := 90 * 24 * time.Hour // default 90 days (D13)
	if v := s.cfg.AuditRetention; v != 0 {
		retention = v
	}
	log.Info().Str("retention", retention.String()).Msg("audit purger: started")

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("audit purger: stopping")
			return
		case <-ticker.C:
			olderThan := time.Now().Add(-retention)
			n, err := s.db.PurgeAuditLog(ctx, olderThan)
			if err != nil {
				log.Error().Err(err).Msg("audit purger: failed")
				continue
			}
			if n > 0 {
				log.Info().Int64("rows", n).Str("older_than", olderThan.Format(time.RFC3339)).
					Msg("audit purger: purged rows")
			}
		}
	}
}

// expirationWarnDays are the thresholds at which we send expiration warning emails.
var expirationWarnDays = []int{30, 14, 7}

// runExpirationScanner ticks once per day and sends expiration warning emails for
// node groups that have an expires_at set and are within 30, 14, or 7 days of
// expiring. Warnings are sent at most once per threshold per group.
// Sprint F (v1.5.0): F3 allocation expiration.
func (s *Server) runExpirationScanner(ctx context.Context) {
	log.Info().Msg("expiration scanner: started")
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	// Run immediately on startup so warnings aren't delayed on first deploy.
	s.scanExpirations(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("expiration scanner: stopping")
			return
		case <-ticker.C:
			s.scanExpirations(ctx)
		}
	}
}

func (s *Server) scanExpirations(ctx context.Context) {
	groups, err := s.db.ListGroupsWithExpiration(ctx)
	if err != nil {
		log.Error().Err(err).Msg("expiration scanner: list groups failed")
		return
	}
	now := time.Now().UTC()
	for _, g := range groups {
		daysUntilExpiry := int(g.ExpiresAt.Sub(now).Hours() / 24)
		// Check if already expired (fire once on the expiry day).
		if daysUntilExpiry < 0 {
			continue
		}
		for _, threshold := range expirationWarnDays {
			if daysUntilExpiry > threshold {
				continue
			}
			// Check if this threshold warning has already been sent.
			alreadySent := false
			for _, sent := range g.WarningSentDays {
				if sent == threshold {
					alreadySent = true
					break
				}
			}
			if alreadySent {
				continue
			}
			// Fetch member emails.
			emails, err := s.db.ListApprovedMemberEmails(ctx, g.GroupID)
			if err != nil {
				log.Error().Err(err).Str("group", g.GroupID).Msg("expiration scanner: get emails failed")
				continue
			}
			if len(emails) == 0 {
				log.Info().Str("group", g.GroupID).Int("days", threshold).
					Msg("expiration scanner: no recipient emails, skipping")
			} else if s.notifier != nil {
				s.notifier.NotifyExpirationWarning(ctx, emails, g.GroupName,
					g.ExpiresAt.Format("2006-01-02"), threshold)
			}
			// Record audit event and mark as sent regardless of email outcome
			// to prevent repeated attempts if SMTP is not configured.
			if s.audit != nil {
				s.audit.Record(ctx, "system", "clustr",
					db.AuditActionExpirationWarning,
					"node_group", g.GroupID, "",
					nil, map[string]interface{}{
						"group_name":     g.GroupName,
						"expires_at":     g.ExpiresAt.Format(time.RFC3339),
						"days_remaining": threshold,
						"recipients":     len(emails),
					})
			}
			if err := s.db.MarkExpirationWarningSent(ctx, g.GroupID, threshold); err != nil {
				log.Error().Err(err).Str("group", g.GroupID).Msg("expiration scanner: mark sent failed")
			}
		}
	}
}

// runAutoPolicyFinalizer ticks every hour and finalizes the 24-hour undo window
// for NodeGroups created by the auto-compute policy engine (H3, Sprint H).
// After 24 hours, the undo endpoint returns 409 for the group.
func (s *Server) runAutoPolicyFinalizer(ctx context.Context) {
	log.Info().Msg("auto-policy finalizer: started (hourly)")
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	s.finalizeAutoPolicies(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("auto-policy finalizer: stopping")
			return
		case <-ticker.C:
			s.finalizeAutoPolicies(ctx)
		}
	}
}

// finalizeAutoPolicies scans pending auto-compute groups and finalizes those
// whose 24-hour window has elapsed.
func (s *Server) finalizeAutoPolicies(ctx context.Context) {
	groups, err := s.db.ListPendingAutoComputeGroups(ctx)
	if err != nil {
		log.Error().Err(err).Msg("auto-policy finalizer: list pending failed")
		return
	}
	for _, g := range groups {
		if time.Since(g.CreatedAt) < 24*time.Hour {
			continue // window still open
		}
		if err := s.db.FinalizeAutoComputeState(ctx, g.GroupID); err != nil {
			log.Error().Err(err).Str("group_id", g.GroupID).
				Msg("auto-policy finalizer: finalize failed")
			continue
		}
		if s.audit != nil {
			s.audit.Record(ctx, "system", "system",
				"auto_allocation.window_closed", "node_group", g.GroupID, "",
				nil, map[string]string{"group_id": g.GroupID},
			)
		}
		log.Info().Str("group_id", g.GroupID).
			Msg("auto-policy finalizer: undo window closed")
	}
}

// diskSpaceThresholds are the disk usage fractions at which we warn / error / fatal.
const (
	diskWarnThreshold  = 0.80
	diskErrorThreshold = 0.90
	diskFatalThreshold = 0.95
)

// runDiskSpaceMonitor checks disk space on CLUSTR_IMAGE_DIR every 15 minutes
// and logs WARN at 80%, ERROR at 90%, FATAL+exit at 95% (S3-9).
func (s *Server) runDiskSpaceMonitor(ctx context.Context) {
	checkDisk := func() bool {
		pct, err := diskUsagePct(s.cfg.ImageDir)
		if err != nil {
			log.Warn().Err(err).Str("dir", s.cfg.ImageDir).Msg("disk monitor: could not check usage (non-fatal)")
			return true
		}
		switch {
		case pct >= diskFatalThreshold:
			log.Error().
				Str("dir", s.cfg.ImageDir).
				Str("usage", fmt.Sprintf("%.1f%%", pct*100)).
				Msg("disk space CRITICAL: image directory is ≥95% full — shutting down to prevent data corruption")
			return false // signal caller to exit
		case pct >= diskErrorThreshold:
			log.Error().
				Str("dir", s.cfg.ImageDir).
				Str("usage", fmt.Sprintf("%.1f%%", pct*100)).
				Msg("disk space ERROR: image directory is ≥90% full — free space immediately")
		case pct >= diskWarnThreshold:
			log.Warn().
				Str("dir", s.cfg.ImageDir).
				Str("usage", fmt.Sprintf("%.1f%%", pct*100)).
				Msg("disk space WARNING: image directory is ≥80% full")
		}
		return true
	}

	// Initial check at startup.
	if ok := checkDisk(); !ok {
		// Fatal — exit the process so systemd restarts it after space is freed.
		log.Fatal().Msg("disk space >95%: refusing to continue")
		return
	}

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ok := checkDisk(); !ok {
				log.Fatal().Msg("disk space >95%: shutting down")
				return
			}
		}
	}
}

// buildRouter constructs the chi router and registers all routes.
func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()

	// Global middleware stack.
	r.Use(panicRecovery)
	r.Use(securityHeadersMiddleware) // F1: CSP + X-Frame-Options + X-Content-Type-Options
	r.Use(corsMiddleware) // CORS before logging so preflight OPTIONS are handled cleanly
	r.Use(requestLogger)
	r.Use(chimiddleware.StripSlashes)
	r.Use(apiVersionHeader) // sets API-Version: v1 on all /api/v1/* responses

	if s.cfg.AuthDevMode {
		log.Warn().Msg("CLUSTR_AUTH_DEV_MODE=1 — authentication is DISABLED (dev mode only, never use in production)")
	}
	// apiKeyAuth is applied only to the /api/v1 subrouter below,
	// so that the embedded web UI at / and /ui/* is always accessible.

	// Build the auth handler — wire DB lookup and session sign/validate functions
	// so the handler doesn't import the server package (avoids circular import).
	authH := s.buildAuthHandler()

	// Derive public server URL from listen addr for boot script generation.
	// Use net.SplitHostPort to extract only the port from ListenAddr (which may
	// be "0.0.0.0:8080"), then combine it with the PXE ServerIP.
	_, port, splitErr := net.SplitHostPort(s.cfg.ListenAddr)
	if splitErr != nil {
		// ListenAddr had no port component — fall back to the raw value.
		port = s.cfg.ListenAddr
	}
	var serverURL string
	if s.cfg.PXE.ServerIP != "" {
		serverURL = "http://" + s.cfg.PXE.ServerIP + ":" + port
	} else {
		// Fallback: use localhost when PXE is not configured.
		serverURL = "http://localhost:" + port
	}

	// Handler instances.
	apiKeysH := s.buildAPIKeysHandler()
	usersH := s.buildUsersHandler()
	auditH := &handlers.AuditHandler{DB: s.db}
	// Audit service and actor-info closure are wired after getActorInfo is defined below.

	health := &handlers.HealthHandler{
		Version:          s.buildInfo.Version,
		CommitSHA:        s.buildInfo.CommitSHA,
		BuildTime:        s.buildInfo.BuildTime,
		DB:               s.db,
		BootDir:          s.cfg.PXE.BootDir,
		InitramfsPath:    s.cfg.PXE.BootDir + "/initramfs-clustr.img",
		FlipBackFailures: &s.flipBackFailureCount,
	}
	// getActorInfo extracts (actorID, actorLabel) from a request context.
	// actorID is users.id for session auth, api_keys.id for Bearer auth, or "".
	// actorLabel is "user:<id>" or "key:<label>" for display in audit log.
	getActorInfo := func(r *http.Request) (string, string) {
		if uid := userIDFromContext(r.Context()); uid != "" {
			return uid, "user:" + uid
		}
		if kid := keyIDFromContext(r.Context()); kid != "" {
			label := keyLabelFromContext(r.Context())
			if label == "" {
				label = kid
			}
			return kid, "key:" + label
		}
		return "", actorLabel(r.Context())
	}
	// Wire audit + actor info into handlers that need it.
	usersH.Audit = s.audit
	usersH.GetActorInfo = getActorInfo
	// GAP-20: wire audit into API key handler (create/revoke/rotate).
	apiKeysH.Audit = s.audit
	apiKeysH.GetActorInfo = getActorInfo
	// GAP-20: wire actor info into slurm manager so routes.go reads from the
	// correct context key (server middleware's ctxKeyKeyLabel, not a local type).
	s.slurmMgr.GetActorInfo = getActorInfo
	// PR4: wire the server URL and GPG key into the slurm manager so it can
	// resolve the "clustr-builtin" sentinel to a concrete /repo/<distro>-<arch>/
	// URL and inject the GPG key into node chroots at deploy time.
	s.slurmMgr.ServerURL = serverURL
	s.slurmMgr.GPGKeyBytes = GPGKeyBytes()

	images := &handlers.ImagesHandler{
		DB:                s.db,
		ImageDir:          s.cfg.ImageDir,
		Progress:          s.progress,
		Audit:             s.audit,
		GetActorInfo:      getActorInfo,
		WebhookDispatcher: s.webhookDispatcher,
		ImageEvents:       s.imageEvents,
		ImageReconciler:   s, // #251: reconcile endpoint — s implements ImageReconcilerIface
		// Factory wired below after imgFactory is initialised.
	}

	bundlesH := &handlers.BundlesHandler{
		DB:           s.db,
		Audit:        s.audit,
		GetActorInfo: getActorInfo,
	}

	// Sprint 4: TUS resumable upload handler (IMG-ISO-1..2).
	tusH := &handlers.TUSHandler{
		ImageDir:     s.cfg.ImageDir,
		DB:           s.db,
		Audit:        s.audit,
		ImageEvents:  s.imageEvents,
		GetActorInfo: getActorInfo,
	}
	tusH.StartGC()
	gpgKeysH := &handlers.GPGKeysHandler{
		DB: s.db,
		EmbeddedKeys: []handlers.EmbeddedGPGKey{
			{Owner: "clustr-release", ArmoredKey: GPGKeyBytes()},
			{Owner: "RPM-GPG-KEY-rocky-9", ArmoredKey: RockyKeyBytes()},
			{Owner: "RPM-GPG-KEY-EPEL-9", ArmoredKey: EPELKeyBytes()},
		},
	}
	nodes := &handlers.NodesHandler{
		DB:                s.db,
		Audit:             s.audit,
		GetActorInfo:      getActorInfo,
		FlipToDiskFirst:   s.flipNodeToDiskFirst,
		WebhookDispatcher: s.webhookDispatcher,
		LDAPNodeConfig: func(ctx context.Context) (*api.LDAPNodeConfig, error) {
			return s.ldapMgr.NodeConfig(ctx)
		},
		RecordNodeLDAPConfigured: func(ctx context.Context, nodeID, configHash string) error {
			return s.ldapMgr.RecordNodeConfigured(ctx, nodeID, configHash)
		},
		SystemAccountsConfig: func(ctx context.Context) (*api.SystemAccountsNodeConfig, error) {
			return s.sysAccountsMgr.NodeConfig(ctx)
		},
		NetworkConfig: func(ctx context.Context, groupID string) (*api.NetworkNodeConfig, error) {
			return s.networkMgr.NodeNetworkConfig(ctx, groupID)
		},
		SlurmNodeConfig: func(ctx context.Context, nodeID string) (*api.SlurmNodeConfig, error) {
			return s.slurmMgr.NodeConfig(ctx, nodeID)
		},
		SudoersNodeConfig: func(ctx context.Context) (*api.SudoersNodeConfig, error) {
			return s.ldapMgr.SudoersNodeConfig(ctx)
		},
		LookupDHCPLease: s.lookupDHCPLease,
		DHCPSubnetCIDR:  s.cfg.PXE.SubnetCIDR,
		ServerIP:        s.cfg.PXE.ServerIP,
	}
	nodeGroups := &handlers.NodeGroupsHandler{
		DB:                 s.db,
		Orchestrator:       s.reimageOrchestrator,
		Audit:              s.audit,
		GetActorInfo:       getActorInfo,
		GroupReimageEvents: s.groupReimageEvents,
	}
	layoutH := &handlers.LayoutHandler{DB: s.db}
	// Use NewFactory so the build semaphore is initialised (capacity from
	// CLUSTR_MAX_CONCURRENT_BUILDS, default 4). Context is wired later via
	// SetContext in StartBackgroundWorkers once the server-lifetime ctx exists.
	if s.imgFactory == nil {
		s.imgFactory = image.NewFactory(
			s.db,
			s.cfg.ImageDir,
			log.Logger,
			buildProgressAdapter{store: s.buildProgress},
			"",
		)
	}
	imgFactory := s.imgFactory
	// Wire the factory into the images handler now that imgFactory is initialised.
	images.Factory = imgFactory
	factory := &handlers.FactoryHandler{
		DB:       s.db,
		ImageDir: s.cfg.ImageDir,
		Factory:  imgFactory,
		Shells:   s.shells,
	}
	factory.Audit = s.audit
	factory.GetActorInfo = getActorInfo
	buildProgressH := &handlers.BuildProgressHandler{
		Store:    s.buildProgress,
		ImageDir: s.cfg.ImageDir,
	}
	resumeH := &handlers.ResumeHandler{
		DB:       s.db,
		ImageDir: s.cfg.ImageDir,
		Factory:  imgFactory,
	}
	initramfsH := &handlers.InitramfsHandler{
		DB:            s.db,
		ScriptPath:    "scripts/build-initramfs.sh", // ignored at runtime — script is embedded
		InitramfsPath: s.cfg.PXE.BootDir + "/initramfs-clustr.img",
		ClustrBinPath:  s.cfg.ClustrBinPath, // abs path to clustr CLI binary; defaults to /usr/local/bin/clustr
		ImageDir:      s.cfg.ImageDir,
		ImageEvents:   s.imageEvents,
	}
	// Prime the in-memory sha256 cache from the on-disk initramfs (if present).
	// Non-fatal: if the file does not yet exist the cache stays empty and the
	// live-entry guard in DeleteInitramfsHistory simply skips the check until
	// the first successful rebuild.
	initramfsH.InitLiveSHA256()
	systemH := &handlers.SystemHandler{
		Initramfs:      initramfsH,
		ImageBuilds:    s.buildProgress,
		Reimages:       s.db,
		DeployProgress: s.progress,
		Shells:         s.shells,
		// DHCPLeases is wired later via SetDHCPLeasesOnSystemHandler (PXE may
		// be disabled and is started after buildRouter returns).
	}
	s.systemHandler = systemH
	logs := &handlers.LogsHandler{DB: s.db, Broker: s.broker, Hub: s.clientdHub}
	s.logsHandler = logs
	progress := &handlers.ProgressHandler{Store: s.progress}

	// UX-4: multiplexed SSE endpoint — one connection per browser tab carrying all topics.
	eventsH := &handlers.EventsHandler{Bus: s.eventBus}
	ipmiH := &handlers.IPMIHandler{DB: s.db, Cache: s.powerCache, Registry: s.powerRegistry}
	powerH := &handlers.PowerHandler{DB: s.db, Registry: s.powerRegistry}
	nodeHealthH := &handlers.NodeHealthHandler{
		DB:  handlers.NewNodeHealthDBAdapter(s.db, selector.NewDBAdapter(s.db)),
		Hub: s.clientdHub,
	}
	reimageH := &handlers.ReimageHandler{
		DB:              s.db,
		Orchestrator:    s.reimageOrchestrator,
		Audit:           s.audit,
		ImageReconciler: s, // #248: pre-deploy guard — s implements ImageReconcilerIface
		GetActorLabel: func(r *http.Request) string {
			return actorLabel(r.Context())
		},
		GetActorInfo: getActorInfo,
	}
	boot := &handlers.BootHandler{
		BootDir:   s.cfg.PXE.BootDir,
		TFTPDir:   s.cfg.PXE.TFTPDir,
		ServerURL: serverURL,
		Version:   s.buildInfo.Version,
		DB:        s.db,
		MintNodeToken: func(nodeID string) (string, error) {
			return CreateNodeScopedKey(context.Background(), s.db, nodeID)
		},
	}

	// Sprint 25 #157 — UDPCast multicast fleet-reimage scheduler.
	// sender.Run is wired here; the udp-sender binary is operator-installed
	// (dnf install udpcast) at /usr/bin/udp-sender (or CLUSTR_UDPSENDER_PATH).
	realSender := multicast.NewSender()
	s.multicastScheduler = multicast.NewScheduler(
		s.db,
		multicast.MakeBlobSenderFunc(s.db, realSender),
		serverURL,
	)
	if err := s.multicastScheduler.Start(context.Background()); err != nil {
		log.Error().Err(err).Msg("multicast: scheduler Start failed (non-fatal, continuing)")
	}
	multicastH := &handlers.MulticastHandler{
		Scheduler: s.multicastScheduler,
		DB:        s.db,
	}

	// S4-1: Prometheus metrics endpoint — unauthenticated so scrapers can reach it
	// without managing API keys. Restrict at the network/reverse-proxy level if needed.
	r.Get("/metrics", (&handlers.MetricsHandler{}).ServeHTTP)

	// I4: pprof profiling endpoints — gated by admin session or API key.
	// Enable with CLUSTR_PPROF_ENABLED=true. Not exposed by default to reduce
	// the attack surface on production installs.
	if os.Getenv("CLUSTR_PPROF_ENABLED") == "true" {
		r.Route("/debug/pprof", func(r chi.Router) {
			r.Use(requireScope(true))
			r.Use(requireRole("admin"))
			r.HandleFunc("/", pprofIndex)
			r.HandleFunc("/cmdline", pprofCmdline)
			r.HandleFunc("/profile", pprofProfile)
			r.HandleFunc("/symbol", pprofSymbol)
			r.HandleFunc("/trace", pprofTrace)
			r.HandleFunc("/{name}", pprofHandler)
		})
		log.Info().Msg("pprof profiling endpoints enabled at /debug/pprof (admin only)")
	}

	// Embedded SPA — serves built web/dist assets with index.html fallback.
	// API routes registered above take precedence; this catches everything else.
	spaHandler, spaErr := webui.Handler()
	if spaErr != nil {
		log.Fatal().Err(spaErr).Msg("server: failed to load embedded web UI")
	}
	r.Handle("/*", spaHandler)

	// /repo/* — public, unauthenticated Slurm package repository served from
	// cfg.RepoDir.  Populated by "clustr-serverd bundle install".
	// Must be mounted BEFORE /api/v1 and outside apiKeyAuth so deployed nodes
	// can reach it without API credentials.
	//
	// Cache policy (per Richard's design doc §7.3):
	//   - repodata/* : max-age=300  (small metadata, safe to recheck frequently)
	//   - *.rpm      : max-age=86400, immutable  (content-addressed by version in name)
	//   - others     : no explicit Cache-Control (stdlib defaults)
	//
	// Byte-range requests and ETag are handled automatically by http.FileServer.
	repoFS := http.StripPrefix("/repo", http.FileServer(http.Dir(s.cfg.RepoDir)))
	r.Get("/repo/health", s.serveRepoHealth)
	// GPG public key for clustr-internal-repo is served from the DB (not disk) so
	// it is always in sync with what was generated by InitRepoGPGKey.  This specific
	// path must be registered BEFORE the wildcard /repo/* handler so chi routes it here
	// rather than falling through to the filesystem.
	if s.slurmMgr != nil {
		r.Get("/repo/clustr-internal-repo/RPM-GPG-KEY-clustr-internal-repo",
			s.slurmMgr.HandleRepoFile)
	}
	r.Handle("/repo/*", repoCacheMiddleware(repoFS))

	r.Route("/api/v1", func(r chi.Router) {
		// All /api/v1 routes: resolve the API key scope from the Bearer token
		// or the session cookie (ADR-0006). Public endpoints (boot files, node
		// register, logs) accept node-scope keys OR unauthenticated requests.
		r.Use(apiKeyAuth(s.db, s.cfg.AuthDevMode, s.sessionSecret, s.cfg.SessionSecure))

		// Auth endpoints — no scope required (login is pre-auth by definition).
		r.Get("/auth/status", authH.HandleStatus)
		r.Post("/auth/login", authH.HandleLogin)
		r.Post("/auth/logout", authH.HandleLogout)
		r.Get("/auth/me", authH.HandleMe)
		// set-password requires a valid session (even during forced-change flow).
		r.Post("/auth/set-password", authH.HandleSetPassword)

		// Readiness probe — unauthenticated so Docker Compose healthchecks, smoke tests,
		// and the README Quick Start can all call it without credentials. Returns 200
		// with JSON if healthy, 503 with reason map if not. (GAP-2)
		r.Get("/healthz/ready", health.ServeReady)

		// Active-jobs probe — unauthenticated so the autodeploy script can poll it
		// without a token to decide whether a clustr-serverd restart is safe.
		// Restart is safe when ALL fields in the response are empty arrays.
		r.Get("/system/active-jobs", systemH.GetActiveJobs)

		// ─── Researcher portal API (C1 — viewer role and above) ───────────────────
		// These routes are accessible by viewer, readonly, operator, and admin.
		// Admin-only management routes (/portal/config) are gated by requireRole("admin").
		portalH := s.buildPortalHandler()
		viewerMW := portalhandler.ViewerMiddleware(s.db, userIDFromContext)
		r.Group(func(r chi.Router) {
			r.Use(requireViewer())
			r.Use(viewerMW)
			r.Get("/portal/status", portalH.HandleStatus)
			r.Post("/portal/me/password", portalH.HandleChangePassword)
			r.Get("/portal/me/quota", portalH.HandleGetQuota)
			r.Get("/portal/partitions/status", portalH.HandleGetPartitions)
		})
		// Admin-only: portal config management.
		r.With(requireScope(true)).With(requireRole("admin")).Get("/portal/config", portalH.HandleGetConfig)
		r.With(requireScope(true)).With(requireRole("admin")).Put("/portal/config", portalH.HandleUpdateConfig)

		// ─── Sprint D — build mailer/notifier early so PI handler can use it ────────
		smtpCfgEarly := s.loadSMTPConfig()
		mailerEarly := notifications.NewSMTPMailer(smtpCfgEarly)
		notifierEarly := &notifications.Notifier{Mailer: mailerEarly, Audit: s.audit}
		// Store on the server for use by StartBackgroundWorkers (digest processor, E4).
		s.notifier = notifierEarly

		// ─── Sprint 22 #133: alert rule engine ───────────────────────────────────────
		{
			alertStore, err := alerts.NewStateStore(s.db)
			if err != nil {
				log.Error().Err(err).Msg("alerts: failed to init state store — alert engine disabled")
			} else {
				alertDispatcher := &alerts.Dispatcher{
					Webhook: s.webhookDispatcher,
					Mailer:  mailerEarly,
				}
				silenceStore := alerts.NewSilenceStore(s.db)
				s.alertStore = alertStore
				s.alertSilences = silenceStore
				s.alertDispatcher = alertDispatcher
				s.alertEngine = alerts.New("", NewStatsDBAdapter(s.db), s.db, alertStore, silenceStore, alertDispatcher)
			}
		}

		// ─── PI portal API (C.5 — pi role and admin) ──────────────────────────────
		// PI-scoped routes: PI can only access their own NodeGroups.
		// Admin can access all PI data. operator, readonly, viewer are blocked.
		piH := s.buildPIHandler()
		piH.Notifier = notifierEarly
		piMW := portalhandler.PIMiddleware(s.db, userIDFromContext, userRoleFromContext)
		r.Group(func(r chi.Router) {
			r.Use(requirePI())
			r.Use(piMW)
			// List owned NodeGroups.
			r.Get("/portal/pi/groups", piH.HandleListGroups)
			// Per-group utilization view (CF-02 partial).
			r.Get("/portal/pi/groups/{id}/utilization", piH.HandleGetGroupUtilization)
			// Member management (CF-08).
			r.Get("/portal/pi/groups/{id}/members", piH.HandleListMembers)
			r.Post("/portal/pi/groups/{id}/members", piH.HandleAddMember)
			r.Delete("/portal/pi/groups/{id}/members/{username}", piH.HandleRemoveMember)
			// Expansion requests (C5-3-3).
			r.Post("/portal/pi/groups/{id}/expansion-requests", piH.HandleRequestExpansion)
		})

		// Admin-only: PI request management ("Pending PI Requests" panel).
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/pi/member-requests", piH.HandleListPendingMemberRequests)
		r.With(requireScope(true)).With(requireRole("admin")).
			Post("/admin/pi/member-requests/{id}/resolve", piH.HandleResolveMemberRequest)
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/pi/expansion-requests", piH.HandleListPendingExpansionRequests)
		r.With(requireScope(true)).With(requireRole("admin")).
			Post("/admin/pi/expansion-requests/{id}/resolve", piH.HandleResolveExpansionRequest)

		// ─── Sprint D — Grant + Publication routes (PI + admin) ──────────────────
		// Grants CRUD on PI-owned NodeGroups.
		r.Group(func(r chi.Router) {
			r.Use(requirePI())
			r.Use(piMW)
			r.Get("/portal/pi/groups/{id}/grants", piH.HandleListGrants)
			r.Post("/portal/pi/groups/{id}/grants", piH.HandleCreateGrant)
			r.Put("/portal/pi/groups/{id}/grants/{grantID}", piH.HandleUpdateGrant)
			r.Delete("/portal/pi/groups/{id}/grants/{grantID}", piH.HandleDeleteGrant)
			// Publications CRUD on PI-owned NodeGroups.
			r.Get("/portal/pi/groups/{id}/publications", piH.HandleListPublications)
			r.Post("/portal/pi/groups/{id}/publications", piH.HandleCreatePublication)
			r.Put("/portal/pi/groups/{id}/publications/{pubID}", piH.HandleUpdatePublication)
			r.Delete("/portal/pi/groups/{id}/publications/{pubID}", piH.HandleDeletePublication)
			// DOI lookup — opt-in outbound call to CrossRef.
			r.Get("/portal/pi/publications/lookup", piH.HandleDOILookup)
			// Annual review responses.
			r.Get("/portal/pi/review-cycles", piH.HandleListPIReviewCycles)
			r.Post("/portal/pi/review-cycles/{cycleID}/groups/{groupID}/respond", piH.HandleSubmitReviewResponse)
		})

		// ─── Sprint G — PI manager delegation (G3 / CF-09) ───────────────────────
		// PI owner or admin can delegate/revoke managers for their NodeGroups.
		// Delegated managers have the same per-project rights as the PI but are
		// NOT the owner. Listed under PI portal middleware.
		managerDelegH := &portalhandler.ManagerDelegationHandler{
			DB:           s.db,
			Audit:        s.audit,
			Notifier:     notifierEarly,
			GetActorInfo: getActorInfo,
		}
		r.Group(func(r chi.Router) {
			r.Use(requirePI())
			r.Use(piMW)
			// Manager list/add/remove on a specific NodeGroup.
			r.Get("/portal/pi/groups/{id}/managers", managerDelegH.HandleListManagers)
			r.Post("/portal/pi/groups/{id}/managers", managerDelegH.HandleAddManager)
			r.Delete("/portal/pi/groups/{id}/managers/{userID}", managerDelegH.HandleRemoveManager)
			// List all groups where the caller is a delegated manager.
			r.Get("/portal/pi/managed-groups", managerDelegH.HandleListManagedGroups)
		})

		// ─── Sprint H — Auto-compute allocation policy engine (H1/H2/H3 / CF-29) ──
		autoPolicyH := s.buildAutoPolicyHandler(notifierEarly, getActorInfo)
		// PI: create a project (with optional auto_compute=true for wizard submit).
		r.Group(func(r chi.Router) {
			r.Use(requirePI())
			r.Use(piMW)
			r.Post("/projects", autoPolicyH.HandleCreateProject)
			// Onboarding wizard status (H2).
			r.Get("/portal/pi/onboarding-status", autoPolicyH.HandleGetOnboardingStatus)
			r.Post("/portal/pi/onboarding-complete", autoPolicyH.HandleCompleteOnboarding)
			// Undo auto-policy on a group the PI owns (H3).
			r.Post("/node-groups/{id}/undo-auto-policy", autoPolicyH.HandleUndoAutoPolicy)
			// Read undo window state for the PI portal banner.
			r.Get("/node-groups/{id}/auto-policy-state", autoPolicyH.HandleGetAutoPolicyState)
		})
		// Admin: undo on any group + read/write policy config.
		r.With(requireScope(true)).With(requireRole("admin")).
			Post("/admin/node-groups/{id}/undo-auto-policy", autoPolicyH.HandleUndoAutoPolicy)
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/auto-policy", autoPolicyH.HandleGetAutoPolicyConfig)
		r.With(requireScope(true)).With(requireRole("admin")).
			Put("/admin/auto-policy", autoPolicyH.HandleUpdateAutoPolicyConfig)
		// Read-only state available to admin (no PI middleware needed).
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/node-groups/{id}/auto-policy-state", autoPolicyH.HandleGetAutoPolicyState)

		// ─── Sprint D — Director portal API (director or admin) ──────────────────
		directorH := &portalhandler.DirectorHandler{DB: s.db, Audit: s.audit}
		directorMW := portalhandler.DirectorMiddleware(userIDFromContext)
		r.Group(func(r chi.Router) {
			r.Use(requireDirector())
			r.Use(directorMW)
			r.Get("/portal/director/summary", directorH.HandleSummary)
			r.Get("/portal/director/groups", directorH.HandleListGroups)
			r.Get("/portal/director/groups/{id}", directorH.HandleGetGroup)
			r.Get("/portal/director/export.csv", directorH.HandleExportCSV)
			r.Get("/portal/director/export-full.csv", directorH.HandleExportCSVFull)
			r.Get("/portal/director/review-cycles", directorH.HandleListReviewCycles)
			r.Get("/portal/director/review-cycles/{id}", directorH.HandleGetReviewCycle)
		})

		// ─── Sprint D — SMTP config + broadcast (admin only) ─────────────────────
		// Re-use the mailer built earlier for PI handler notifications.
		notifH := &handlers.NotificationsHandler{
			DB:                      s.db,
			Audit:                   s.audit,
			Mailer:                  mailerEarly,
			BroadcastRateLimitHours: 1,
		}
		r.With(requireScope(true)).With(requireRole("admin")).Get("/admin/smtp", notifH.HandleGetSMTP)
		r.With(requireScope(true)).With(requireRole("admin")).Put("/admin/smtp", notifH.HandleUpdateSMTP)
		r.With(requireScope(true)).With(requireRole("admin")).Post("/admin/smtp/test", notifH.HandleTestSMTP)
		r.With(requireScope(true)).With(requireRole("admin")).
			Post("/node-groups/{id}/broadcast", notifH.HandleBroadcast)

		// ─── Sprint D — Review cycle management (admin only) ──────────────────────
		r.With(requireScope(true)).With(requireRole("admin")).
			Post("/admin/review-cycles", portalhandler.HandleCreateReviewCycle(s.db, s.audit))
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/review-cycles", portalhandler.HandleListReviewCycles(s.db))
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/review-cycles/{id}", portalhandler.HandleGetReviewCycle(s.db))

		// ─── Sprint E — Allocation change requests (E1, CF-20) ───────────────────
		acrH := &portalhandler.AllocationChangeRequestHandler{
			DB:           s.db,
			Audit:        s.audit,
			Notifier:     notifierEarly,
			GetActorInfo: getActorInfo,
		}
		// PI routes for change requests.
		r.Group(func(r chi.Router) {
			r.Use(requirePI())
			r.Use(piMW)
			r.Get("/portal/pi/groups/{id}/change-requests", acrH.HandleListGroupChangeRequests)
			r.Post("/portal/pi/groups/{id}/change-requests", acrH.HandleCreateChangeRequest)
			r.Post("/portal/pi/change-requests/{reqID}/withdraw", acrH.HandleWithdrawChangeRequest)
		})
		// Admin queue + review.
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/change-requests", acrH.HandleAdminListChangeRequests)
		r.With(requireScope(true)).With(requireRole("admin")).
			Post("/admin/change-requests/{id}/review", acrH.HandleAdminReviewChangeRequest)

		// ─── Sprint E — Field of Science taxonomy (E2, CF-16) ────────────────────
		fosH := &portalhandler.FOSHandler{DB: s.db, Audit: s.audit}
		// Public (authenticated) — FOS picker for PI portal.
		r.Get("/fields-of-science", fosH.HandleListFOS)
		// PI: set FOS on owned group.
		r.Group(func(r chi.Router) {
			r.Use(requirePI())
			r.Use(piMW)
			r.Patch("/portal/pi/groups/{id}/field-of-science", fosH.HandleSetGroupFOS)
		})
		// Admin: manage the FOS list.
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/fields-of-science", fosH.HandleAdminListFOS)
		r.With(requireScope(true)).With(requireRole("admin")).
			Post("/admin/fields-of-science", fosH.HandleAdminCreateFOS)
		r.With(requireScope(true)).With(requireRole("admin")).
			Put("/admin/fields-of-science/{fosID}", fosH.HandleAdminUpdateFOS)
		// Director: FOS utilization breakdown.
		r.Group(func(r chi.Router) {
			r.Use(requireDirector())
			r.Use(directorMW)
			r.Get("/portal/director/fos-utilization", fosH.HandleDirectorFOSUtilization)
		})

		// ─── Sprint E — Per-attribute visibility policy (E3, CF-39) ──────────────
		visH := &portalhandler.AttributeVisibilityHandler{DB: s.db, Audit: s.audit}
		// PI + admin: view/set/delete project-level visibility overrides.
		r.Group(func(r chi.Router) {
			r.Use(requirePI())
			r.Use(piMW)
			r.Get("/portal/pi/groups/{id}/attribute-visibility", visH.HandleListGroupVisibility)
			r.Patch("/portal/pi/groups/{id}/attribute-visibility", visH.HandleSetGroupVisibility)
			r.Delete("/portal/pi/groups/{id}/attribute-visibility/{attr}", visH.HandleDeleteGroupVisibility)
		})
		// Admin: global defaults management.
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/attribute-visibility-defaults", visH.HandleListVisibilityDefaults)
		r.With(requireScope(true)).With(requireRole("admin")).
			Put("/admin/attribute-visibility-defaults/{attr}", visH.HandleUpdateVisibilityDefault)

		// ─── Sprint E — Per-user notification preferences (E4, CF-11/CF-15) ──────
		notifPrefsH := &handlers.NotificationPrefsHandler{
			DB:           s.db,
			Audit:        s.audit,
			GetActorInfo: getActorInfo,
		}
		// Any session-authenticated user can manage their own prefs.
		r.With(requireScope(true)).Get("/me/notification-prefs", notifPrefsH.HandleGetMyPrefs)
		r.With(requireScope(true)).Put("/me/notification-prefs/{event}", notifPrefsH.HandleSetMyPref)
		r.With(requireScope(true)).Post("/me/notification-prefs/reset", notifPrefsH.HandleResetMyPrefs)
		// Admin: view any user's prefs.
		r.With(requireScope(true)).With(requireRole("admin")).
			Get("/admin/users/{id}/notification-prefs", notifPrefsH.HandleAdminGetUserPrefs)

		// B2-2: Bootstrap status probe — unauthenticated. Returns whether the default
		// admin credentials hint should be shown on the login page. Safe to expose
		// publicly — returns only a boolean, no user data.
		r.Get("/auth/bootstrap-status", func(w http.ResponseWriter, r *http.Request) {
			complete := true
			count, err := s.db.CountUsers(r.Context())
			if err != nil || count == 0 {
				complete = false // fresh install — show the hint
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if complete {
				_, _ = w.Write([]byte(`{"bootstrap_complete":true}`))
			} else {
				_, _ = w.Write([]byte(`{"bootstrap_complete":false}`))
			}
		})

		// Fully public — no key required (PXE-booted nodes before any key is issued).
		r.Get("/boot/ipxe", boot.ServeIPXEScript)
		r.Get("/boot/vmlinuz", boot.ServeVMLinuz)
		r.Get("/boot/initramfs.img", boot.ServeInitramfs)
		r.Get("/boot/rescue.cpio.gz", boot.ServeRescueInitramfs)
		r.Get("/boot/extra/memtest", boot.ServeMemtest)
		r.Get("/boot/ipxe.efi", boot.ServeIPXEEFI)
		r.Get("/boot/undionly.kpxe", boot.ServeUndionlyKPXE)

		// JSON Schema + OpenAPI 3.1 (#161) — publicly readable, no auth required.
		schemaH := handlers.NewSchemaHandler()
		r.Get("/schemas/{type}", schemaH.GetTypeSchema)
		r.Get("/openapi.json", schemaH.GetOpenAPI)

		// Slurm artifact download — protected by HMAC-signed URL (token + expires query params)
		// generated by the server and pushed to nodes via slurm_binary_push. No API key
		// required because nodes receive the signed URL from the server and may not hold an
		// admin-scope key. The HMAC prevents unauthorized downloads.
		if s.slurmMgr != nil {
			r.Get("/slurm/builds/{build_id}/artifact", s.slurmMgr.ServeArtifact)
		}

		// Node-scope callbacks — accept both node and admin keys, or no key (legacy PXE nodes).
		r.Post("/nodes/register", nodes.RegisterNode)
		r.Post("/logs", logs.IngestLogs)
		// POST /deploy/progress is intentionally outside the admin-only group.
		// The deploy agent running in initramfs calls this endpoint using its
		// node-scoped API key (minted at PXE-serve time). Placing it inside
		// the admin group would require admin-scoped keys in the initramfs,
		// which violates the least-privilege design (node keys can only interact
		// with their own node's resources). GET paths for progress are inside the
		// admin group below — only operators read the aggregated progress stream.
		r.Post("/deploy/progress", progress.IngestProgress)

		// Deploy lifecycle callbacks — require node-scope auth where the key's bound
		// node_id must match the URL {id}. Admin keys also pass (for manual overrides).
		// These are intentionally outside the admin-only group so the deploy agent
		// running in initramfs can call them using its node-scoped key.
		r.With(requireNodeOwnership("id")).Post("/nodes/{id}/deploy-complete", nodes.DeployComplete)
		r.With(requireNodeOwnership("id")).Post("/nodes/{id}/deploy-failed", nodes.DeployFailed)

		// ADR-0008: Post-reboot verification phone-home endpoint.
		// Called by the deployed OS (via clustr-verify-boot.service systemd oneshot)
		// on first boot. Node-scoped token required; admin keys are NOT accepted here.
		// The node-scoped key written to /etc/clustr/node-token at finalize time is
		// the same one minted during PXE enrollment and is reused post-boot.
		r.With(requireNodeOwnership("id")).Post("/nodes/{id}/verify-boot", nodes.VerifyBoot)

		// flip-to-disk — called by the deploy agent (node-scoped key) after writing
		// the rootfs to signal the server to set next boot to disk and power-cycle.
		// Must be outside the admin-only group: UEFI nodes use a node-scoped key and
		// would get 403 if this were admin-only. Admin keys also pass (manual override).
		r.With(requireNodeOwnership("id")).Post("/nodes/{id}/power/flip-to-disk", powerH.FlipToDisk)

		// Self-read: allow a node-scoped key to read its own node record.
		// Used by the deploy agent's state verification loop after deploy-complete.
		// The chi router matches the most specific (longest) path first, so the
		// admin-only GET /nodes/{id} below still applies for admin keys; this route
		// is only reached by node-scoped keys (requireNodeOwnership allows both).
		r.With(requireNodeOwnership("id")).Get("/nodes/{id}/self", nodes.GetNode)

		// clustr-clientd WebSocket endpoint — node-scoped key required; the key's
		// bound node_id must match the {id} URL parameter (same as verify-boot).
		clientdH := &handlers.ClientdHandler{
			DB:     s.db,
			Hub:    s.clientdHub,
			Broker: s.broker,
			BiosDB: s.db,
			SudoersNodeConfig: func(ctx context.Context) (*api.SudoersNodeConfig, error) {
				return s.ldapMgr.SudoersNodeConfig(ctx)
			},
		}
		r.With(requireNodeOwnership("id")).Get("/nodes/{id}/clientd/ws", clientdH.HandleClientdWS)

		// Image fetch routes accessible by node-scoped keys (deploy agent reads its assigned image).
		// requireImageAccess handles both admin and node scopes; node keys may only access the
		// image currently assigned to their bound node. Must be outside the admin-only group.
		r.With(requireImageAccess("id", s.db)).Get("/images/{id}", images.GetImage)
		r.With(requireImageAccess("id", s.db)).Get("/images/{id}/blob", images.DownloadBlob)

		// C2-5: Dashboard anomaly card — accessible to readonly/operator/admin.
		// HTMX-aware: returns HTML partial when HX-Request: true.
		dashboardH := &handlers.DashboardHandler{DB: s.db}
		r.With(requireRole("readonly")).Get("/dashboard/anomalies", dashboardH.HandleAnomalies)

		// Admin-only routes — require admin scope.
		r.Group(func(r chi.Router) {
			r.Use(requireScope(true)) // admin scope required

			// API key management — admin role only (operators cannot manage API keys).
			r.With(requireRole("admin")).Get("/admin/api-keys", apiKeysH.HandleList)
			r.With(requireRole("admin")).Post("/admin/api-keys", apiKeysH.HandleCreate)
			r.With(requireRole("admin")).Delete("/admin/api-keys/{id}", apiKeysH.HandleRevoke)
			r.With(requireRole("admin")).Post("/admin/api-keys/{id}/rotate", apiKeysH.HandleRotate)

			// S4-2: Webhook subscription management — admin role only.
			webhooksH := &handlers.WebhooksHandler{DB: s.db}
			r.With(requireRole("admin")).Get("/admin/webhooks", webhooksH.HandleList)
			r.With(requireRole("admin")).Post("/admin/webhooks", webhooksH.HandleCreate)
			r.With(requireRole("admin")).Get("/admin/webhooks/{id}", webhooksH.HandleGet)
			r.With(requireRole("admin")).Put("/admin/webhooks/{id}", webhooksH.HandleUpdate)
			r.With(requireRole("admin")).Delete("/admin/webhooks/{id}", webhooksH.HandleDelete)
			r.With(requireRole("admin")).Get("/admin/webhooks/{id}/deliveries", webhooksH.HandleListDeliveries)

			// User management (ADR-0007) — admin role only (operator cannot manage users).
			// GET /admin/users includes group_ids for each user (S3-3).
			r.With(requireRole("admin")).Get("/admin/users", usersH.HandleListWithMemberships)
			r.With(requireRole("admin")).Post("/admin/users", usersH.HandleCreate)
			r.With(requireRole("admin")).Put("/admin/users/{id}", usersH.HandleUpdate)
			r.With(requireRole("admin")).Post("/admin/users/{id}/reset-password", usersH.HandleResetPassword)
			r.With(requireRole("admin")).Post("/admin/users/{id}/enable", usersH.HandleEnable)
			r.With(requireRole("admin")).Delete("/admin/users/{id}", usersH.HandleDelete)
			// GAP-21: /api/v1/users CRUD aliases — Sprint 3 docs and the walkthrough
			// expect these paths; /admin/users is the canonical path but /users also works.
			r.With(requireRole("admin")).Get("/users", usersH.HandleListWithMemberships)
			r.With(requireRole("admin")).Post("/users", usersH.HandleCreate)
			r.With(requireRole("admin")).Get("/users/{id}", usersH.HandleGetUser)
			r.With(requireRole("admin")).Put("/users/{id}", usersH.HandleUpdate)
			r.With(requireRole("admin")).Delete("/users/{id}", usersH.HandleDelete)
			// Group membership assignment (S3-3).
			r.With(requireRole("admin")).Get("/users/{id}/group-memberships", usersH.HandleGetGroupMemberships)
			r.With(requireRole("admin")).Put("/users/{id}/group-memberships", usersH.HandleSetGroupMemberships)

			// GPG keys — list is operator-accessible; import/delete are admin-only.
			r.Get("/gpg-keys", gpgKeysH.ListGPGKeys)
			r.With(requireRole("admin")).Post("/gpg-keys", gpgKeysH.ImportGPGKey)
			r.With(requireRole("admin")).Delete("/gpg-keys/{fingerprint}", gpgKeysH.DeleteGPGKey)

			// Audit log (S3-4) — admin only (operators and readonly cannot read audit log).
			r.With(requireRole("admin")).Get("/audit", auditH.HandleQuery)
			// ACT-DEL-1 (Sprint 4): single-record and bulk delete.
			r.With(requireRole("admin")).Delete("/audit/{id}", auditH.HandleDelete)
			r.With(requireRole("admin")).Delete("/audit", auditH.HandleBulkDelete)
			// F2: SIEM JSONL streaming export — admin only, rate-limited 1/min.
			r.With(requireRole("admin")).Get("/audit/export", auditH.HandleExport)

			// Health — liveness probe (existing).
			r.Get("/health", health.ServeHTTP)
			// #130: Cluster-wide node health summary (reachability + heartbeat).
			r.Get("/cluster/health", nodeHealthH.GetClusterHealth)

			// UX-4: multiplexed SSE endpoint. All authenticated sessions may connect;
			// the stream carries typed events for nodes, images, groups, and more.
			// Use ?topics=nodes,images to filter. One connection per browser tab.
			r.Get("/events", eventsH.ServeEvents)

			// Bundles — unified view of all slurm builds (DB) plus the built-in bundle.
			// GET is available to all authenticated sessions; DELETE requires admin role.
			r.Get("/bundles", bundlesH.ListBundles)
			r.With(requireRole("admin")).Delete("/bundles/{id}", bundlesH.DeleteBundle)

			// Images — mutating operations are admin-only.
			// GET /images/{id} and GET /images/{id}/blob are registered above with
			// requireImageAccess so node keys can also reach them.
			// SSE-1: image lifecycle event stream — must be registered before /images/{id}.
			r.Get("/images/events", images.StreamImageEvents)
			r.Get("/images", images.ListImages)
			r.Post("/images", images.CreateImage)
			// IMG-URL-1: download image from URL (Sprint 4).
			r.Post("/images/from-url", images.FromURL)
			// IMG-ISO-1..2: TUS resumable upload (Sprint 4).
			r.Options("/uploads/", tusH.Options)
			r.Post("/uploads/", tusH.Create)
			r.Head("/uploads/{id}", tusH.Head)
			r.Patch("/uploads/{id}", tusH.Patch)
			r.Delete("/uploads/{id}", tusH.TUSDelete)
			r.Post("/images/from-upload", tusH.FromUpload)
			r.Delete("/images/{id}", images.DeleteImage)
			r.Get("/images/{id}/status", images.GetImageStatus)
			r.Get("/images/{id}/disklayout", images.GetDiskLayout)
			r.Put("/images/{id}/disklayout", images.PutDiskLayout)
			r.Put("/images/{id}/install-instructions", images.PutInstallInstructions)
			r.Post("/images/{id}/blob", images.UploadBlob)
			r.Get("/images/{id}/metadata", images.GetImageMetadata)
			r.Put("/images/{id}/tags", images.UpdateImageTags)

			// Factory
			r.Get("/image-roles", factory.ListImageRoles)
			r.Post("/factory/pull", factory.Pull)
			r.Post("/factory/import", factory.Import)
			r.Post("/factory/import-path", factory.ImportPath)
			r.Post("/factory/import-iso", factory.ImportPath) // alias used by the web UI
			r.Post("/factory/capture", factory.Capture)
			r.Post("/factory/probe-iso", factory.ProbeISO)
			r.Post("/factory/build-from-iso", factory.BuildFromISO)
			// ISO-FS-1/2: list and register local files from the import dir.
			// Must be registered before /images/{id} to avoid chi wildcard match.
			r.Get("/images/local-files", factory.ListLocalFiles)
			r.Post("/images/from-local-file", factory.FromLocalFile)

			// ISO build observability — stream must come before plain snapshot route.
			r.Get("/images/{id}/build-progress/stream", buildProgressH.StreamBuildProgress)
			r.Get("/images/{id}/build-progress", buildProgressH.GetBuildProgress)
			r.Get("/images/{id}/build-log", buildProgressH.GetBuildLog)
			r.Get("/images/{id}/build-manifest", buildProgressH.GetBuildManifest)

			// Build resume (F2) — resume an interrupted build from last phase.
			r.Post("/images/{id}/resume", resumeH.ResumeImageBuild)
			// C3-5: cancel in-progress ISO build without deleting the image record.
			r.Post("/images/{id}/cancel", images.CancelBuild)
			// #251: Blob reconcile endpoint — inspect artifact integrity and optionally self-heal.
			r.Post("/images/{id}/reconcile", images.ReconcileImage)

			// System initramfs management (F1).
			r.Get("/system/initramfs", initramfsH.GetInitramfs)
			r.Post("/system/initramfs/rebuild", initramfsH.RebuildInitramfs)
			r.Delete("/system/initramfs/history/{id}", initramfsH.DeleteInitramfsHistory)

			// Sprint 4 INITRD-1..6: image-store initramfs build with SSE log streaming.
			r.Post("/initramfs/build", initramfsH.BuildInitramfsFromImage)
			r.Delete("/initramfs/builds/{id}", initramfsH.CancelInitramfsBuild)

			// Shell sessions
			r.Post("/images/{id}/shell-session", factory.OpenShellSession)
			r.Delete("/images/{id}/shell-session/{sid}", factory.CloseShellSession)
			r.Post("/images/{id}/shell-session/{sid}/exec", factory.ExecInSession)
			// wsTokenLift hoists ?token= into the Authorization header for
			// browser WS connections (browsers cannot set custom headers on
			// WS upgrade requests). HTTP endpoints never use this middleware.
			r.With(wsTokenLift).Get("/images/{id}/shell-session/{sid}/ws", factory.ShellWS)

			// Active deploy detection (for shell modal warning)
			r.Get("/images/{id}/active-deploys", factory.ActiveDeploys)

			// DHCP allocations — read-only view of MAC→IP mappings from node_configs.
			// No dnsmasq lease files are read; source of truth is the node table.
			dhcpH := &handlers.DHCPHandler{DB: s.db}
			r.Get("/dhcp/leases", dhcpH.ListLeases)

			// Nodes — by-mac must be before /{id} to avoid chi match ambiguity.
			// nodes/connected must be before nodes/{id} to avoid chi match ambiguity.
			r.Get("/nodes/by-mac/{mac}", nodes.GetNodeByMAC)
			r.Get("/nodes/connected", clientdH.GetConnectedNodes)
			r.Get("/nodes/unassigned", nodes.ListUnassignedNodes)
			r.Get("/nodes", nodes.ListNodes)
			r.Post("/nodes", nodes.CreateNode)
			// Sprint 4 BULK-1: batch create endpoint (must be before /{id}).
			r.Post("/nodes/batch", nodes.BatchCreateNodes)
			r.Get("/nodes/{id}", nodes.GetNode)
			// PUT, PATCH and DELETE require admin or group-scoped operator access.
			r.With(requireGroupAccess("id", s.db)).Put("/nodes/{id}", nodes.UpdateNode)
			// Sprint 4 EDIT-NODE-1: partial update endpoint.
			r.With(requireGroupAccess("id", s.db)).Patch("/nodes/{id}", nodes.PatchNode)
			r.With(requireGroupAccess("id", s.db)).Delete("/nodes/{id}", nodes.DeleteNode)

			// S5-12: Node config change history (admin-only audit trail).
			configHistoryH := &handlers.NodeConfigHistoryHandler{DB: s.db}
			r.With(requireRole("admin")).Get("/nodes/{id}/config-history", configHistoryH.HandleList)

			// Sprint 7 NODE-SUDO-1..3: per-node sudoer management.
			nodeSudoersH := &handlers.NodeSudoersHandler{
				DB:           s.db,
				Audit:        s.audit,
				GetActorInfo: getActorInfo,
				StagingDB:    s.db,
			}
			r.Get("/nodes/{id}/sudoers", nodeSudoersH.HandleList)
			r.With(requireRole("admin")).Post("/nodes/{id}/sudoers", nodeSudoersH.HandleAdd)
			r.With(requireRole("admin")).Delete("/nodes/{id}/sudoers/{uid}", nodeSudoersH.HandleRemove)
			r.With(requireRole("admin")).Post("/nodes/{id}/sudoers/sync", nodeSudoersH.HandleSync)

			// Sprint 7 NODE-SUDO-5: unified user search across local + LDAP.
			usersSearchH := &handlers.UsersSearchHandler{
				DB:      s.db,
				LDAPMgr: s.ldapMgr,
			}
			r.Get("/users/search", usersSearchH.HandleSearch)

			// clientd heartbeat — admin read of latest heartbeat data.
			r.Get("/nodes/{id}/heartbeat", clientdH.GetHeartbeat)

			// Config push — push a whitelisted config file to a live node.
			r.Put("/nodes/{id}/config-push", clientdH.ConfigPush)

			// Remote exec — run a whitelisted diagnostic command on a live node.
			r.Post("/nodes/{id}/exec", clientdH.ExecOnNode)

			// Sprint 22 #131: per-node stats query.
			statsH := &handlers.StatsHandler{DB: NewStatsDBAdapter(s.db)}
			r.Get("/nodes/{id}/stats", statsH.GetNodeStats)

			// #243: SELF-MON — control-plane host status endpoint.
			cpHandler := &handlers.ControlPlaneHandler{DB: handlers.NewControlPlaneDBAdapter(s.db)}
			r.Get("/control-plane", cpHandler.ServeHTTP)

			// Sprint 22 #133: alert rule engine — query active + recent alerts.
			if s.alertStore != nil {
				alertsH := &handlers.AlertsHandler{Store: s.alertStore}
				r.Get("/alerts", alertsH.HandleList)

				// Sprint 24 #155: alert silences CRUD.
				if s.alertSilences != nil {
					silH := &handlers.SilencesHandler{Store: s.alertSilences}
					r.Get("/alerts/silences", silH.HandleList)
					r.Post("/alerts/silences", silH.HandleCreate)
					r.Delete("/alerts/silences/{id}", silH.HandleDelete)
				}

				// Sprint 24 #155: alert rules listing (engine-loaded rules).
				// UX-9: PUT /api/v1/alerts/rules/{name} for in-UI rule editing.
				if s.alertEngine != nil {
					rulesH := &handlers.AlertRulesHandler{
						Engine:  s.alertEngine,
						StatsDB: handlers.NewAlertRulesDBAdapter(s.db.SQL()),
						// RuleWriter defaults to privhelper.RuleWrite when nil.
					}
					r.Get("/alerts/rules", rulesH.HandleList)
					r.Put("/alerts/rules/{name}", rulesH.HandleUpdate)
				}
			}

			// Batch operator exec (#126) — arbitrary command across a node selector,
			// streamed as SSE. Admin/operator scope only (runs arbitrary commands on nodes).
			execH := &handlers.ExecHandler{
				DB:  handlers.NewExecDBAdapter(selector.NewDBAdapter(s.db)),
				Hub: s.clientdHub,
			}
			r.Post("/exec", execH.HandleExec)

			// Batch file copy (#127) — push files to a node selector over clientd.
			cpH := &handlers.CpHandler{
				DB:  handlers.NewExecDBAdapter(selector.NewDBAdapter(s.db)),
				Hub: s.clientdHub,
			}
			r.Post("/cp", cpH.HandleCp)

			// Console broker (#128) — brokered server-side IPMI SOL or SSH PTY.
			// Admin/operator scope only (full terminal access to the node).
			// The operator connects via WebSocket; the server opens the upstream
			// (ipmitool sol activate or SSH PTY) and pipes bidirectionally.
			consoleH := &handlers.ConsoleHandler{
				DB: handlers.NewConsoleDBAdapter(s.db),
			}
			// wsTokenLift hoists ?token= into the Authorization header for
			// browser WS connections (browsers cannot set custom headers on
			// WS upgrade requests). HTTP endpoints never use this middleware.
			r.With(wsTokenLift).Get("/console/{node_id}", consoleH.HandleConsole)

			// BIOS handler declaration — referenced by both node-level and
			// top-level routes registered in the blocks below.
			biosH := &handlers.BiosHandler{DB: s.db}

			// Disk layout hierarchy — node-level overrides, group assignment,
			// hardware-aware recommendations, and validation.
			r.Get("/nodes/{id}/layout-recommendation", layoutH.GetLayoutRecommendation)
			r.Get("/nodes/{id}/effective-layout", layoutH.GetEffectiveLayout)
			r.Put("/nodes/{id}/layout-override", layoutH.SetNodeLayoutOverride)
			r.Post("/nodes/{id}/layout/validate", layoutH.ValidateLayout)
			r.Put("/nodes/{id}/group", layoutH.AssignNodeGroup)
			r.Get("/nodes/{id}/effective-mounts", layoutH.GetEffectiveMounts)

			// BIOS profile node bindings and live read (#159).
			r.Put("/nodes/{id}/bios-profile", biosH.AssignProfile)
			r.Delete("/nodes/{id}/bios-profile", biosH.DetachProfile)
			r.Get("/nodes/{id}/bios-profile", biosH.GetNodeProfile)
			r.Post("/nodes/{id}/bios/read", clientdH.ReadBiosOnNode)
			r.Post("/nodes/{id}/bios/apply", clientdH.BiosApplyOnNode)

			// Disk layout catalog (#146) — named, reusable layouts that can be
			// assigned to node groups (default) or individual nodes (override).
			diskLayoutsH := &handlers.DiskLayoutsHandler{
				DB:  s.db,
				Hub: s.clientdHub,
			}
			r.Post("/disk-layouts/capture/{node_id}", diskLayoutsH.CaptureLayout)
			r.Get("/disk-layouts", diskLayoutsH.ListLayouts)
			r.Get("/disk-layouts/{id}", diskLayoutsH.GetLayout)
			r.Put("/disk-layouts/{id}", diskLayoutsH.UpdateLayout)
			r.Delete("/disk-layouts/{id}", diskLayoutsH.DeleteLayout)

			// Boot menu entries (#160) — operator-defined iPXE menu items appended
			// to the disk-boot menu at PXE-serve time.
			bootEntriesH := &handlers.BootEntriesHandler{DB: s.db}
			r.Get("/boot-entries", bootEntriesH.ListBootEntries)
			r.Post("/boot-entries", bootEntriesH.CreateBootEntry)
			r.Get("/boot-entries/{id}", bootEntriesH.GetBootEntry)
			r.Put("/boot-entries/{id}", bootEntriesH.UpdateBootEntry)
			r.Delete("/boot-entries/{id}", bootEntriesH.DeleteBootEntry)

			// Sprint 25 #157 — UDPCast multicast fleet-reimage.
			// enqueue: node/operator enrolls in a multicast session.
			// wait:    node long-polls until the session fires or falls back.
			// outcome: node reports udp-receiver result after the transfer.
			r.Post("/multicast/enqueue", multicastH.Enqueue)
			r.Get("/multicast/sessions/{id}", multicastH.GetSession)
			r.Get("/multicast/sessions/{id}/wait", multicastH.Wait)
			r.Post("/multicast/sessions/{id}/members/{node_id}/outcome", multicastH.RecordOutcome)

			// BIOS profiles (#159) — vendor-agnostic BIOS settings management.
			r.Post("/bios-profiles", biosH.CreateProfile)
			r.Get("/bios-profiles", biosH.ListProfiles)
			r.Get("/bios-profiles/{id}", biosH.GetProfile)
			r.Put("/bios-profiles/{id}", biosH.UpdateProfile)
			r.Delete("/bios-profiles/{id}", biosH.DeleteProfile)
			r.Get("/bios/providers/{vendor}/verify", biosH.VerifyProvider)

			// Rack model (#149) — physical rack inventory and node U-slot assignments.
			racksH := &handlers.RacksHandler{DB: s.db}
			r.Get("/racks", racksH.ListRacks)
			r.Post("/racks", racksH.CreateRack)
			r.Get("/racks/{id}", racksH.GetRack)
			r.Put("/racks/{id}", racksH.UpdateRack)
			r.Delete("/racks/{id}", racksH.DeleteRack)
			// Legacy rack-position endpoints — deprecated in v0.11.0, removed in v0.12.0.
			// Sunset: Mon, 01 Dec 2026. Use PUT/DELETE /api/v1/nodes/{id}/placement instead.
			r.Put("/racks/{id}/positions/{node_id}", racksH.SetPosition)
			r.Delete("/racks/{id}/positions/{node_id}", racksH.DeletePosition)

			// Enclosures (#231 Sprint 31) — multi-node chassis (blade, twin, quad, half-width).
			enclosuresH := &handlers.EnclosuresHandler{DB: s.db}
			r.Get("/enclosure-types", enclosuresH.ListEnclosureTypes)
			r.Get("/racks/{rack_id}/enclosures", enclosuresH.ListEnclosuresForRack)
			r.Post("/racks/{rack_id}/enclosures", enclosuresH.CreateEnclosure)
			r.Get("/enclosures/{id}", enclosuresH.GetEnclosure)
			r.Put("/enclosures/{id}", enclosuresH.UpdateEnclosure)
			r.Delete("/enclosures/{id}", enclosuresH.DeleteEnclosure)
			r.Post("/enclosures/{id}/slots/{slot_index}", enclosuresH.SetSlot)
			r.Delete("/enclosures/{id}/slots/{slot_index}", enclosuresH.ClearSlot)

			// Unified placement endpoint (#231) — subsumes rack + enclosure placement.
			placementH := &handlers.PlacementHandler{DB: s.db}
			r.Put("/nodes/{node_id}/placement", placementH.SetPlacement)
			r.Delete("/nodes/{node_id}/placement", placementH.DeletePlacement)

			// Node groups — named sets of nodes sharing a disk layout override.
			r.Get("/node-groups", nodeGroups.ListNodeGroups)
			r.Post("/node-groups", nodeGroups.CreateNodeGroup)
			r.Get("/node-groups/{id}", nodeGroups.GetNodeGroup)
			r.Put("/node-groups/{id}", nodeGroups.UpdateNodeGroup)
			r.Delete("/node-groups/{id}", nodeGroups.DeleteNodeGroup)
			// Group membership management.
			r.Post("/node-groups/{id}/members", nodeGroups.AddGroupMembers)
			r.Delete("/node-groups/{id}/members/{node_id}", nodeGroups.RemoveGroupMember)
			// C5-1-2: PI ownership assignment (admin-only).
			r.With(requireRole("admin")).Put("/node-groups/{id}/pi", nodeGroups.SetNodeGroupPI)
			// F3: Allocation expiration — pi rank or higher can set/clear.
			// requireRole("pi") passes pi, operator, and admin through.
			r.With(requireRole("pi")).Put("/node-groups/{id}/expiration", nodeGroups.HandleSetExpiration)
			r.With(requireRole("pi")).Delete("/node-groups/{id}/expiration", nodeGroups.HandleClearExpiration)
			// G2 (CF-40): Per-NodeGroup LDAP group access restrictions.
			// Admin-only: controls which LDAP groups are allowed to use this partition.
			// GET returns the current list; PUT replaces it (pass [] to clear, = open access).
			r.With(requireRole("admin")).Get("/node-groups/{id}/ldap-restrictions", nodeGroups.GetNodeGroupRestrictions)
			r.With(requireRole("admin")).Put("/node-groups/{id}/ldap-restrictions", nodeGroups.SetNodeGroupRestrictions)
			// Rolling group reimage — requires admin or group-scoped operator access.
			r.With(requireGroupAccessByGroupID("id", s.db)).Post("/node-groups/{id}/reimage", nodeGroups.ReimageGroup)
			// Group reimage SSE event stream — GET /api/v1/node-groups/{id}/reimage/events?job_id=<jid>
			r.Get("/node-groups/{id}/reimage/events", nodeGroups.StreamGroupReimageEvents)
			// Group reimage job status polling.
			r.Get("/reimages/jobs/{jobID}", nodeGroups.GetGroupReimageJob)
			r.Post("/reimages/jobs/{jobID}/resume", nodeGroups.ResumeGroupReimageJob)

			// IPMI / power management — subpaths of /nodes/{id} must be
			// registered in the same chi group so the auth middleware applies.
			// Read-only power status and sensors are visible to all authenticated users.
			// State-changing power ops require admin or group-scoped operator access.
			r.Get("/nodes/{id}/power", ipmiH.GetPowerStatus)
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/power/on", ipmiH.PowerOn)
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/power/off", ipmiH.PowerOff)
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/power/cycle", ipmiH.PowerCycle)
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/power/reset", ipmiH.PowerReset)
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/power/pxe", ipmiH.SetBootPXE)
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/power/disk", ipmiH.SetBootDisk)
			r.Get("/nodes/{id}/sensors", ipmiH.GetSensors)
			// #129: SEL read and clear.
			r.Get("/nodes/{id}/sel", ipmiH.GetSEL)
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/sel/clear", ipmiH.ClearSEL)
			// #130: Per-node health summary.
			r.Get("/nodes/{id}/health", nodeHealthH.GetNodeHealth)
			// CFG-3: BMC config update (admin-only, typed confirm required).
			r.With(requireRole("admin")).Patch("/nodes/{id}/bmc", ipmiH.PatchBMC)
			// CFG-4: BMC connection test.
			r.Post("/nodes/{id}/bmc/test", ipmiH.TestBMC)

			// Reimage — queue, track and retry node reimages via the power provider.
			// Create requires group-scoped operator access.
			r.With(requireGroupAccess("id", s.db)).Post("/nodes/{id}/reimage", reimageH.Create)
			// S4-10: Cancel in-flight reimage by node ID (not reimage UUID).
			r.With(requireGroupAccess("id", s.db)).Delete("/nodes/{id}/reimage/active", reimageH.CancelActiveForNode)
			// GAP-11: GET active reimage — returns {} when none exists instead of empty body.
			r.Get("/nodes/{id}/reimage/active", reimageH.GetActiveForNode)
			r.Get("/nodes/{id}/reimage", reimageH.ListForNode)
			r.Get("/reimage/{id}", reimageH.Get)
			r.Delete("/reimage/{id}", reimageH.Cancel)
			// cancel-all-active must be registered before /{id}/retry so chi's
			// radix tree matches the literal segment before the wildcard.
			r.Post("/reimage/cancel-all-active", reimageH.CancelAllActive)
			r.Post("/reimage/{id}/retry", reimageH.Retry)
			r.Get("/reimages", reimageH.List)

			// Logs — stream must be registered before plain /logs.
			r.Get("/logs/stream", logs.StreamLogs)
			r.Get("/logs", logs.QueryLogs)

			// Deployment progress — stream must be registered before plain routes.
			r.Get("/deploy/progress/stream", progress.StreamProgress)
			r.Get("/deploy/progress/{mac}", progress.GetProgress)
			r.Get("/deploy/progress", progress.ListProgress)

			// LDAP module — admin-only management routes.
			r.With(requireRole("admin")).Group(func(r chi.Router) {
				ldapmodule.RegisterRoutes(r, s.ldapMgr)
				// Sudoers push — broadcasts the sudoers drop-in to all connected nodes.
				r.Post("/ldap/sudoers/push", clientdH.HandleSudoersPush)
			})

			// System Accounts module — admin-only management routes.
			r.With(requireRole("admin")).Group(func(r chi.Router) {
				sysaccounts.RegisterRoutes(r, s.sysAccountsMgr)
			})

			// Sprint 7: Identity surface — group overlays + specialty groups.
			identityGroupsH := &handlers.IdentityGroupsHandler{DB: s.db}
			r.With(requireRole("admin")).Get("/groups/{group_dn}/supplementary-members", identityGroupsH.HandleListOverlay)
			r.With(requireRole("admin")).Post("/groups/{group_dn}/supplementary-members", identityGroupsH.HandleAddOverlay)
			r.With(requireRole("admin")).Delete("/groups/{group_dn}/supplementary-members/{user_identifier}", identityGroupsH.HandleRemoveOverlay)
			r.With(requireRole("admin")).Get("/groups/specialty", identityGroupsH.HandleListSpecialty)
			r.With(requireRole("admin")).Post("/groups/specialty", identityGroupsH.HandleCreateSpecialty)
			r.With(requireRole("admin")).Patch("/groups/specialty/{id}", identityGroupsH.HandleUpdateSpecialty)
			r.With(requireRole("admin")).Delete("/groups/specialty/{id}", identityGroupsH.HandleDeleteSpecialty)
			r.With(requireRole("admin")).Post("/groups/specialty/{id}/members", identityGroupsH.HandleAddSpecialtyMember)
			r.With(requireRole("admin")).Delete("/groups/specialty/{id}/members/{uid}", identityGroupsH.HandleRemoveSpecialtyMember)

			// Network module — admin-only management routes.
			r.With(requireRole("admin")).Group(func(r chi.Router) {
				networkmodule.RegisterRoutes(r, s.networkMgr)
			})

			// Slurm module — admin-only management routes.
			r.With(requireRole("admin")).Group(func(r chi.Router) {
				slurmmodule.RegisterRoutes(r, s.slurmMgr)
			})

			// M1: TECH-TRIG monitoring — admin-only (Sprint M, v1.11.0).
			// D27 Bucket 2 signal dashboard: surface current metric values, thresholds,
			// and fired state for the four TECH-TRIG signals.
			techTrigH := &handlers.TechTriggersHandler{
				DB:           s.db,
				Audit:        s.audit,
				GetActorInfo: getActorInfo,
			}
			r.With(requireRole("admin")).Get("/admin/tech-triggers", techTrigH.HandleList)
			r.With(requireRole("admin")).Get("/admin/tech-triggers/history", techTrigH.HandleHistory)
			r.With(requireRole("admin")).Post("/admin/tech-triggers/{name}/reset", techTrigH.HandleReset)
			r.With(requireRole("admin")).Post("/admin/tech-triggers/{name}/signal", techTrigH.HandleSignal)

			// Sprint 24 #154: two-stage commit pending-changes API.
			changesH := s.buildChangesHandler(getActorInfo)
			// Count is readable by any authenticated role (badge poll).
			r.Get("/changes/count", changesH.HandleCount)
			r.Get("/changes/mode", changesH.HandleGetMode)
			r.With(requireRole("admin")).Get("/changes", changesH.HandleList)
			r.With(requireRole("admin")).Post("/changes", changesH.HandleStage)
			r.With(requireRole("admin")).Post("/changes/commit", changesH.HandleCommit)
			r.With(requireRole("admin")).Post("/changes/clear", changesH.HandleClear)
			r.With(requireRole("admin")).Put("/changes/mode/{surface}", changesH.HandleSetMode)
		})
	})

	return r
}

// buildPortalHandler constructs the portal.Handler with closures wired to
// the LDAP and Slurm managers.
func (s *Server) buildPortalHandler() *portalhandler.Handler {
	h := &portalhandler.Handler{
		DB: s.db,
	}

	// Wire LDAP user info fetcher.
	h.GetLDAPUser = func(ctx context.Context, uid string) (*portalhandler.LDAPUserInfo, error) {
		info, err := s.ldapMgr.GetPortalUserInfo(ctx, uid)
		if err != nil || info == nil {
			return nil, err
		}
		return &portalhandler.LDAPUserInfo{
			UID:         info.UID,
			DisplayName: info.DisplayName,
			Email:       info.Email,
			Groups:      info.Groups,
		}, nil
	}

	// Wire LDAP self-service password change.
	h.SetLDAPPassword = func(ctx context.Context, uid, currentPassword, newPassword string) error {
		return s.ldapMgr.ChangeOwnPassword(ctx, uid, currentPassword, newPassword)
	}

	// Wire storage quota fetcher.
	h.GetLDAPQuota = func(ctx context.Context, uid string) (*portalhandler.QuotaResponse, error) {
		cfg, err := s.db.GetPortalConfig(ctx)
		if err != nil {
			return &portalhandler.QuotaResponse{Configured: false}, nil
		}
		if cfg.LDAPQuotaUsedAttr == "" && cfg.LDAPQuotaLimitAttr == "" {
			// Also check env vars as override.
			usedAttr := os.Getenv("CLUSTR_LDAP_QUOTA_USED_ATTR")
			limitAttr := os.Getenv("CLUSTR_LDAP_QUOTA_LIMIT_ATTR")
			if usedAttr == "" && limitAttr == "" {
				return &portalhandler.QuotaResponse{Configured: false}, nil
			}
			cfg.LDAPQuotaUsedAttr = usedAttr
			cfg.LDAPQuotaLimitAttr = limitAttr
		}
		quota, err := s.ldapMgr.GetPortalQuota(ctx, uid, cfg.LDAPQuotaUsedAttr, cfg.LDAPQuotaLimitAttr)
		if err != nil || quota == nil {
			return &portalhandler.QuotaResponse{Configured: false}, nil
		}
		return &portalhandler.QuotaResponse{
			UsedBytes:  quota.UsedBytes,
			LimitBytes: quota.LimitBytes,
			UsedRaw:    quota.UsedRaw,
			LimitRaw:   quota.LimitRaw,
			Configured: quota.Configured,
		}, nil
	}

	// Wire Slurm partition status.
	h.GetPartitionStatus = func(ctx context.Context) ([]portalhandler.PartitionStatus, error) {
		partitions, err := s.slurmMgr.GetPartitionStatus(ctx)
		if err != nil || partitions == nil {
			return nil, err
		}
		out := make([]portalhandler.PartitionStatus, len(partitions))
		for i, p := range partitions {
			out[i] = portalhandler.PartitionStatus{
				Partition:      p.Partition,
				State:          p.State,
				TotalNodes:     p.TotalNodes,
				AvailableNodes: p.AvailableNodes,
			}
		}
		return out, nil
	}

	return h
}

// buildChangesHandler constructs the two-stage commit ChangesHandler (#154)
// with kind-specific CommitFns wired to the real immediate-apply code paths.
func (s *Server) buildChangesHandler(getActorInfo func(r *http.Request) (string, string)) *handlers.ChangesHandler {
	h := &handlers.ChangesHandler{
		DB:           s.db,
		Audit:        s.audit,
		GetActorInfo: getActorInfo,
		CommitFns: map[string]handlers.ChangesCommitFn{
			// ldap_user: replay the payload through ApplyUserCreate which wraps
			// WriteBind + ditClient.CreateUser without exposing the unexported ditClient.
			"ldap_user": func(ctx context.Context, c db.PendingChange) error {
				var req ldapmodule.CreateUserRequest
				if err := json.Unmarshal([]byte(c.Payload), &req); err != nil {
					return fmt.Errorf("ldap_user commit: decode payload: %w", err)
				}
				return s.ldapMgr.ApplyUserCreate(ctx, req)
			},
			// sudoers_rule: replay the payload by adding a sudoer to the node.
			"sudoers_rule": func(ctx context.Context, c db.PendingChange) error {
				var req struct {
					UserIdentifier string `json:"user_identifier"`
					Source         string `json:"source"`
					Commands       string `json:"commands"`
				}
				if err := json.Unmarshal([]byte(c.Payload), &req); err != nil {
					return fmt.Errorf("sudoers_rule commit: decode payload: %w", err)
				}
				if req.UserIdentifier == "" {
					return fmt.Errorf("sudoers_rule commit: user_identifier required in payload")
				}
				if req.Source == "" {
					req.Source = "local"
				}
				if req.Commands == "" {
					req.Commands = "ALL"
				}
				sudoer := db.NodeSudoer{
					NodeID:         c.Target,
					UserIdentifier: req.UserIdentifier,
					Source:         req.Source,
					Commands:       req.Commands,
				}
				return s.db.NodeSudoersAdd(ctx, sudoer)
			},
			// node_network: replay a network profile create or update.
			"node_network": func(ctx context.Context, c db.PendingChange) error {
				var p api.NetworkProfile
				if err := json.Unmarshal([]byte(c.Payload), &p); err != nil {
					return fmt.Errorf("node_network commit: decode payload: %w", err)
				}
				if c.Target == "new_profile" {
					_, err := s.networkMgr.CreateProfile(ctx, p)
					return err
				}
				_, err := s.networkMgr.UpdateProfile(ctx, c.Target, p)
				return err
			},
		},
	}
	return h
}

// buildPIHandler constructs the PI portal handler with LDAP closures.
func (s *Server) buildPIHandler() *portalhandler.PIHandler {
	h := &portalhandler.PIHandler{
		DB:    s.db,
		Audit: s.audit,
	}

	// Wire LDAP group membership helpers — best-effort, nil-safe.
	h.AddLDAPMember = func(ctx context.Context, groupName, username string) error {
		return s.ldapMgr.AddUserToGroup(ctx, username, groupName)
	}
	h.RemoveLDAPMember = func(ctx context.Context, groupName, username string) error {
		return s.ldapMgr.RemoveUserFromGroup(ctx, username, groupName)
	}

	return h
}

// buildAutoPolicyHandler constructs the AutoPolicyHandler for Sprint H.
func (s *Server) buildAutoPolicyHandler(notifier *notifications.Notifier, getActorInfo func(r *http.Request) (string, string)) *portalhandler.AutoPolicyHandler {
	engine := &allocation.Engine{
		DB:    s.db,
		Audit: s.audit,
	}

	// Wire LDAP sync hook (G1) — ensures a posixGroup exists for the NodeGroup.
	// EnsureProjectGroup is non-blocking; it queues on LDAP unavailability.
	// We need the group name so look it up, then call the plugin.
	engine.SyncLDAPGroup = func(ctx context.Context, groupID string) error {
		ng, err := s.db.GetNodeGroupFull(ctx, groupID)
		if err != nil {
			return fmt.Errorf("auto-policy ldap sync: get group: %w", err)
		}
		s.ldapMgr.EnsureProjectGroup(ctx, groupID, ng.Name)
		return nil
	}

	// Wire G2 restriction setter — noop for now (default is open / membership-based).
	// A future sprint can wire the node_group_restrictions insert here.
	engine.SetGroupRestriction = nil

	// Wire Slurm partition auto-assignment — nil if Slurm module not active.
	if s.slurmMgr != nil {
		engine.AddSlurmPartition = func(ctx context.Context, groupID, partitionName string) error {
			return s.slurmMgr.AddAutoPartition(ctx, groupID, partitionName)
		}
	}

	return &portalhandler.AutoPolicyHandler{
		DB:           s.db,
		Audit:        s.audit,
		Engine:       engine,
		Notifier:     notifier,
		GetActorInfo: getActorInfo,
	}
}

// loadSMTPConfig loads SMTP config from the DB (with env-var override at send time).
// This is used at server start to build the mailer; env vars are re-read on each
// send by SMTPMailer.
func (s *Server) loadSMTPConfig() notifications.SMTPConfig {
	cfg, err := s.db.GetSMTPConfig(context.Background())
	if err != nil {
		log.Warn().Err(err).Msg("smtp: failed to load config from DB; using defaults")
		return notifications.SMTPConfig{Port: 587, UseTLS: true}
	}
	return notifications.SMTPConfig{
		Host:     cfg.Host,
		Port:     cfg.Port,
		Username: cfg.Username,
		Password: cfg.Password,
		From:     cfg.From,
		UseTLS:   cfg.UseTLS,
		UseSSL:   cfg.UseSSL,
	}
}

// buildAuthHandler constructs the AuthHandler with closures that call into
// the server's DB and session-signing functions. This avoids the handlers
// package importing the server package (which would be circular).
func (s *Server) buildAuthHandler() *handlers.AuthHandler {
	const cookieName = "clustr_session"

	// Legacy API-key login (deprecated — removed in v1.1).
	loginWithKeyFn := func(rawKey string) (keyPrefix string, scope string, ok bool) {
		hashInput := rawKey
		for _, pfx := range []string{"clustr-admin-", "clustr-node-"} {
			if strings.HasPrefix(rawKey, pfx) {
				hashInput = strings.TrimPrefix(rawKey, pfx)
				break
			}
		}
		h := sha256.Sum256([]byte(hashInput))
		hashHex := fmt.Sprintf("%x", h)
		lookupResult, err := s.db.LookupAPIKey(context.Background(), hashHex)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", "", false
			}
			log.Error().Err(err).Msg("auth handler: db lookup failed")
			return "", "", false
		}
		if lookupResult.Scope != api.KeyScopeAdmin {
			return "", "", false
		}
		kid := hashInput
		if len(kid) > 8 {
			kid = kid[:8]
		}
		return kid, string(lookupResult.Scope), true
	}

	// Primary username+password login (ADR-0007).
	loginWithPasswordFn := func(username, password string) (userID, role string, mustChange bool, err error) {
		user, err := s.db.GetUserByUsername(context.Background(), username)
		if err != nil {
			// ErrUserNotFound → "invalid" (generic, prevents user enumeration).
			// Any other error is a real DB failure — surface it so the handler
			// can return 500 rather than masking infrastructure failures as 401.
			if errors.Is(err, db.ErrUserNotFound) {
				return "", "", false, fmt.Errorf("invalid")
			}
			return "", "", false, fmt.Errorf("db: %w", err)
		}
		if user.IsDisabled() {
			return "", "", false, fmt.Errorf("disabled")
		}
		if berr := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); berr != nil {
			return "", "", false, fmt.Errorf("invalid")
		}
		// Update last_login_at asynchronously — never block the login response.
		go func() { _ = s.db.SetLastLogin(context.Background(), user.ID) }()
		return user.ID, string(user.Role), user.MustChangePassword, nil
	}

	signForUserFn := func(userID, role string) (string, time.Time, error) {
		p := newSessionPayload(userID, role)
		token, err := signSessionToken(s.sessionSecret, p)
		if err != nil {
			return "", time.Time{}, err
		}
		return token, time.Unix(p.EXP, 0), nil
	}

	signForKeyFn := func(keyPrefix string) (string, time.Time, error) {
		p := newSessionPayloadForKey(keyPrefix)
		token, err := signSessionToken(s.sessionSecret, p)
		if err != nil {
			return "", time.Time{}, err
		}
		return token, time.Unix(p.EXP, 0), nil
	}

	validateFn := func(token string) (sub, role string, exp time.Time, needsReissue bool, newToken string, ok bool) {
		result, err := validateSessionToken(s.sessionSecret, token)
		if err != nil {
			return "", "", time.Time{}, false, "", false
		}
		reissued := ""
		actuallyReissued := false
		if result.needsReissue {
			slid := slideSessionPayload(result.payload)
			if t, serr := signSessionToken(s.sessionSecret, slid); serr == nil {
				reissued = t
				result.payload = slid
				actuallyReissued = true
			}
			// If sign failed, skip re-issue silently — the existing valid token
			// continues to work. Do NOT return needsReissue=true with an empty
			// newToken, which would cause HandleMe to overwrite the cookie with "".
		}
		return result.payload.Sub, result.payload.Role, time.Unix(result.payload.EXP, 0), actuallyReissued, reissued, true
	}

	setPasswordFn := func(userID, currentPassword, newPassword string) (string, time.Time, error) {
		user, err := s.db.GetUser(context.Background(), userID)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("user_not_found")
		}
		if berr := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(currentPassword)); berr != nil {
			return "", time.Time{}, fmt.Errorf("wrong_password")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("hash: %w", err)
		}
		if err := s.db.SetUserPassword(context.Background(), userID, string(hash), true); err != nil {
			return "", time.Time{}, err
		}
		// Issue a fresh session token with the same role.
		p := newSessionPayload(user.ID, string(user.Role))
		token, err := signSessionToken(s.sessionSecret, p)
		if err != nil {
			return "", time.Time{}, err
		}
		return token, time.Unix(p.EXP, 0), nil
	}

	// B1-4: GetUserGroups fetches group memberships for operator role scoping.
	getUserGroupsFn := func(userID string) ([]string, error) {
		return s.db.GetUserGroupMemberships(context.Background(), userID)
	}

	// C5-1-4: GetUsername returns the username for a user ID (PI portal display).
	getUsernameFn := func(userID string) (string, error) {
		user, err := s.db.GetUser(context.Background(), userID)
		if err != nil {
			return "", err
		}
		return user.Username, nil
	}

	// HasAdminUser returns true when at least one active admin user exists.
	// Used by GET /api/v1/auth/status for first-run detection (AUTH0-1).
	hasAdminUserFn := func() (bool, error) {
		n, err := s.db.CountActiveAdmins(context.Background())
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}

	// HasDefaultAdmin returns true when the "clustr" default admin exists.
	// Surfaced in /api/v1/auth/status as default_admin_present so the web UI
	// can warn operators who removed the default account via bootstrap-admin.
	hasDefaultAdminFn := func() (bool, error) {
		_, err := s.db.GetUserByUsername(context.Background(), "clustr")
		if err != nil {
			return false, nil //nolint:nilerr // not-found is not an error here
		}
		return true, nil
	}

	return &handlers.AuthHandler{
		HasAdminUser:    hasAdminUserFn,
		HasDefaultAdmin: hasDefaultAdminFn,
		LoginWithKey:      loginWithKeyFn,
		LoginWithPassword: loginWithPasswordFn,
		SignForUser:       signForUserFn,
		SignForKey:        signForKeyFn,
		Validate:          validateFn,
		SetPassword:       setPasswordFn,
		GetUserGroups:     getUserGroupsFn,
		GetUsername:       getUsernameFn,
		CookieName:        cookieName,
		Secure:            s.cfg.SessionSecure,
	}
}

// buildUsersHandler constructs the UsersHandler with a bcrypt helper closure.
func (s *Server) buildUsersHandler() *handlers.UsersHandler {
	return &handlers.UsersHandler{
		DB: s.db,
		HashPassword: func(plaintext string) (string, error) {
			h, err := bcrypt.GenerateFromPassword([]byte(plaintext), 12)
			if err != nil {
				return "", err
			}
			return string(h), nil
		},
	}
}

// buildAPIKeysHandler constructs the APIKeysHandler with closures that call into
// the server's DB and key-generation functions without causing circular imports.
//
// user_id ownership: web UI mints carry the session user's id (every web caller
// hits this through apiKeyAuth which populates ctxKeyUserID).  Bearer-token
// callers (admin keys hitting POST /admin/api-keys directly) carry the user_id
// of the key they authenticated with, which itself was minted by some user.
// Falls back to the bootstrap admin only as a last resort — that only fires when
// dev-mode auth is enabled (CLUSTR_AUTH_DEV_MODE=1), where there is no real
// caller identity at all.
func (s *Server) buildAPIKeysHandler() *handlers.APIKeysHandler {
	mintFn := func(r *http.Request, scope api.KeyScope, nodeID, label, createdBy string, expiresAt *time.Time) (string, string, string, error) {
		userID, err := s.resolveCallerUserID(r)
		if err != nil {
			return "", "", "", err
		}
		raw, err := generateRawKey()
		if err != nil {
			return "", "", "", err
		}
		keyHash := sha256Hex(raw)
		rec := db.APIKeyRecord{
			ID:        uuid.New().String(),
			Scope:     scope,
			NodeID:    nodeID,
			KeyHash:   keyHash,
			Label:     label,
			CreatedBy: createdBy,
			UserID:    userID,
			CreatedAt: time.Now(),
			ExpiresAt: expiresAt,
		}
		if err := s.db.CreateAPIKey(r.Context(), rec); err != nil {
			return "", "", "", fmt.Errorf("create api key: %w", err)
		}
		return raw, rec.ID, keyHash, nil
	}

	actorLabelFn := func(r *http.Request) string {
		return keyLabelFromContext(r.Context())
	}

	return &handlers.APIKeysHandler{
		DB:            s.db,
		MintKey:       mintFn,
		GetActorLabel: actorLabelFn,
	}
}

// resolveCallerUserID extracts the user_id associated with the current request.
// Resolution order:
//
//  1. Session cookie auth → ctxKeyUserID directly.
//  2. Bearer token auth → user_id of the api_keys row that auth'd the request.
//  3. Fallback (dev-mode, auto-register, or stale chains) → bootstrap admin.
//
// The fallback exists because pre-103 keys carried no user_id and the migration
// backfilled all of them to the bootstrap admin; we extend the same convention
// to in-flight requests where no chain identity is recoverable.
func (s *Server) resolveCallerUserID(r *http.Request) (string, error) {
	ctx := r.Context()
	if uid := userIDFromContext(ctx); uid != "" {
		return uid, nil
	}
	if kid := keyIDFromContext(ctx); kid != "" {
		if rec, err := s.db.GetAPIKey(ctx, kid); err == nil && rec.UserID != "" {
			return rec.UserID, nil
		}
	}
	return resolveBootstrapAdminID(ctx, s.db)
}

// repoCacheMiddleware wraps a repo file-server handler and sets Cache-Control
// headers appropriate for a yum/dnf repository:
//   - repodata/*  -> public, max-age=300   (metadata, recheck cheaply)
//   - *.rpm       -> public, max-age=86400, immutable  (content-addressed)
//   - everything else -> no extra header (stdlib default)
func repoCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/repodata/"):
			w.Header().Set("Cache-Control", "public, max-age=300")
		case strings.HasSuffix(path, ".rpm"):
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		}
		next.ServeHTTP(w, r)
	})
}

// installedBundleInfo is a JSON-serialisable record of a bundle that has been
// installed under a repo subdirectory.  Read from .installed-version files.
type installedBundleInfo struct {
	Distro        string `json:"distro"`
	Arch          string `json:"arch"`
	SlurmVersion  string `json:"slurm_version"`
	ClustrRelease string `json:"clustr_release"`
	InstalledAt   string `json:"installed_at"`
	BundleSHA256  string `json:"bundle_sha256"`
}

// serveRepoHealth returns a JSON object listing all installed bundles and all
// repo subdirectories present on disk (GAP-17: includes both el9-x86_64/ and
// el9-x86_64-deps/).
// GET /repo/health — public, no auth.
func (s *Server) serveRepoHealth(w http.ResponseWriter, r *http.Request) {
	type response struct {
		Installed []installedBundleInfo `json:"installed"`
		Subdirs   []string              `json:"subdirs"` // all repo subdirs present on disk
	}

	entries, err := os.ReadDir(s.cfg.RepoDir)
	if err != nil {
		// RepoDir doesn't exist yet (bundle not yet installed) — return empty list.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"installed":[],"subdirs":[]}`)
		return
	}

	var bundles []installedBundleInfo
	var subdirs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		subdirs = append(subdirs, entry.Name())

		versionFile := filepath.Join(s.cfg.RepoDir, entry.Name(), ".installed-version")
		data, err := os.ReadFile(versionFile)
		if err != nil {
			continue
		}
		var info installedBundleInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		bundles = append(bundles, info)
	}

	if bundles == nil {
		bundles = []installedBundleInfo{}
	}
	if subdirs == nil {
		subdirs = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(response{Installed: bundles, Subdirs: subdirs})
}

// Handler returns the underlying http.Handler for use in tests.
func (s *Server) Handler() http.Handler {
	return s.router
}

// ListenAndServe starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()

		// Give in-flight builds up to 120 seconds to finish naturally before
		// we force-cancel them. HTTP shutdown gets its own 5-second window on
		// top of that. Total wall-clock budget: 125 seconds.
		//
		// The systemd unit sets TimeoutStopSec=300 which comfortably covers
		// this window. Previously the 25s budget caused QEMU builds to be
		// interrupted mid-install when the autodeploy timer restarted the
		// service; 120s gives most OS package-download phases time to complete.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer drainCancel()

		log.Info().Msg("shutdown: waiting for in-flight builds to complete (up to 120s)")
		s.buildProgress.WaitForActive(drainCtx)

		// Any builds still active after the drain window are stuck — cancel them
		// so the DB record gets updated and the UI doesn't spin forever.
		s.buildProgress.CancelAllActive("server shutting down")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("graceful shutdown error")
		}
		if err := s.shells.CloseAll(); err != nil {
			log.Error().Err(err).Msg("shell session cleanup error")
		}
	}()

	log.Info().Str("addr", s.cfg.ListenAddr).Msg("server listening")
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}
