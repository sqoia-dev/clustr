// Package server provides the clonr-serverd HTTP API built on chi.
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/image"
	"github.com/sqoia-dev/clonr/pkg/power"
	ipmipower "github.com/sqoia-dev/clonr/pkg/power/ipmi"
	proxmoxpower "github.com/sqoia-dev/clonr/pkg/power/proxmox"
	"github.com/sqoia-dev/clonr/pkg/reimage"
	"github.com/sqoia-dev/clonr/pkg/server/handlers"
	"github.com/sqoia-dev/clonr/pkg/server/ui"
)

// Server wraps the HTTP server and all its dependencies.
type Server struct {
	cfg                 config.ServerConfig
	db                  *db.DB
	broker              *LogBroker
	progress            *ProgressStore
	buildProgress       *BuildProgressStore
	shells              *image.ShellManager
	powerCache          *PowerCache
	powerRegistry       *power.Registry
	reimageOrchestrator *reimage.Orchestrator
	sessionSecret       []byte // HMAC key for browser session tokens
	router              chi.Router
	http                *http.Server
	logsHandler         *handlers.LogsHandler
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
func New(cfg config.ServerConfig, database *db.DB) *Server {
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
		log.Warn().Msg("CLONR_SESSION_SECRET not set — generated ephemeral session secret (sessions will not survive restarts)")
	}

	s := &Server{
		cfg:                 cfg,
		db:                  database,
		broker:              NewLogBroker(),
		progress:            NewProgressStore(),
		buildProgress:       buildProg,
		shells:              shells,
		powerCache:          NewPowerCache(15 * time.Second),
		powerRegistry:       registry,
		reimageOrchestrator: reimageOrch,
		sessionSecret:       secret,
	}
	s.router = s.buildRouter()
	s.http = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: s.router,
	}
	return s
}

// StartBackgroundWorkers starts long-running background goroutines.
// Call this after New() and before ListenAndServe().
func (s *Server) StartBackgroundWorkers(ctx context.Context) {
	// Wire the server-lifetime context into the logs ingest handler so that
	// client disconnects (r.Context() cancellations) do not abort in-flight
	// SQLite log-batch transactions and silently drop deploy logs.
	s.logsHandler.ServerCtx = ctx
	go s.reimageOrchestrator.Scheduler(ctx)
	go s.runLogPurger(ctx)
	// ADR-0008: Post-reboot verification timeout scanner.
	go s.runVerifyTimeoutScanner(ctx)
}

// runVerifyTimeoutScanner ticks every 60 seconds and marks as timed-out any node
// that has deploy_completed_preboot_at set but no deploy_verified_booted_at within
// CLONR_VERIFY_TIMEOUT. ADR-0008.
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
			cutoff := time.Now().Add(-timeout)
			nodes, err := s.db.ListNodesAwaitingVerification(ctx, cutoff)
			if err != nil {
				log.Error().Err(err).Msg("verify-boot scanner: ListNodesAwaitingVerification failed")
				continue
			}
			for _, n := range nodes {
				if err := s.db.RecordVerifyTimeout(ctx, n.ID); err != nil {
					log.Error().Err(err).Str("node_id", n.ID).Str("hostname", n.Hostname).
						Msg("verify-boot scanner: RecordVerifyTimeout failed")
					continue
				}
				log.Warn().
					Str("node_id", n.ID).
					Str("hostname", n.Hostname).
					Str("timeout", timeout.String()).
					Msgf("verify-boot scanner: node %s (%s) did not phone home within %s of deploy-complete — possible bootloader failure, kernel panic, or /etc/clonr/node-token not written correctly",
						n.ID, n.Hostname, timeout)
			}
		}
	}
}

// runLogPurger ticks every hour and deletes log entries older than the
// configured retention window. Retention is read from CLONR_LOG_RETENTION
// (Go duration string, e.g. "336h" for 14 days); defaults to 336h (14d).
// Uses the server-lifetime context so it shuts down cleanly on SIGTERM.
func (s *Server) runLogPurger(ctx context.Context) {
	retention := 14 * 24 * time.Hour // default 14 days
	if v := s.cfg.LogRetention; v != 0 {
		retention = v
	}
	log.Info().Str("retention", retention.String()).Msg("log purger: started")

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("log purger: stopping")
			return
		case <-ticker.C:
			olderThan := time.Now().Add(-retention)
			n, err := s.db.PurgeLogs(ctx, olderThan)
			if err != nil {
				log.Error().Err(err).Str("older_than", olderThan.Format(time.RFC3339)).
					Msg("log purger: PurgeLogs failed")
			} else {
				log.Info().Int64("rows_purged", n).Str("older_than", olderThan.Format(time.RFC3339)).
					Str("retention", retention.String()).Msg("log purger: purge complete")
			}
		}
	}
}

// buildRouter constructs the chi router and registers all routes.
func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()

	// Global middleware stack.
	r.Use(panicRecovery)
	r.Use(requestLogger)
	r.Use(chimiddleware.StripSlashes)
	r.Use(apiVersionHeader) // sets API-Version: v1 on all /api/v1/* responses

	if s.cfg.AuthDevMode {
		log.Warn().Msg("CLONR_AUTH_DEV_MODE=1 — authentication is DISABLED (dev mode only, never use in production)")
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

	health := &handlers.HealthHandler{Version: "dev"}
	images := &handlers.ImagesHandler{DB: s.db, ImageDir: s.cfg.ImageDir, Progress: s.progress}
	nodes := &handlers.NodesHandler{DB: s.db}
	nodeGroups := &handlers.NodeGroupsHandler{DB: s.db, Orchestrator: s.reimageOrchestrator}
	layoutH := &handlers.LayoutHandler{DB: s.db}
	imgFactory := &image.Factory{
		Store:         s.db,
		ImageDir:      s.cfg.ImageDir,
		Logger:        log.Logger,
		BuildProgress: buildProgressAdapter{store: s.buildProgress},
	}
	factory := &handlers.FactoryHandler{
		DB:       s.db,
		ImageDir: s.cfg.ImageDir,
		Factory:  imgFactory,
		Shells:   s.shells,
	}
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
		InitramfsPath: s.cfg.PXE.BootDir + "/initramfs-clonr.img",
		ClonrBinPath:  s.cfg.ClonrBinPath, // abs path to clonr CLI binary; defaults to /usr/local/bin/clonr
	}
	// Prime the in-memory sha256 cache from the on-disk initramfs (if present).
	// Non-fatal: if the file does not yet exist the cache stays empty and the
	// live-entry guard in DeleteInitramfsHistory simply skips the check until
	// the first successful rebuild.
	initramfsH.InitLiveSHA256()
	logs := &handlers.LogsHandler{DB: s.db, Broker: s.broker}
	s.logsHandler = logs
	progress := &handlers.ProgressHandler{Store: s.progress}
	ipmiH := &handlers.IPMIHandler{DB: s.db, Cache: s.powerCache, Registry: s.powerRegistry}
	powerH := &handlers.PowerHandler{DB: s.db, Registry: s.powerRegistry}
	reimageH := &handlers.ReimageHandler{DB: s.db, Orchestrator: s.reimageOrchestrator}
	boot := &handlers.BootHandler{
		BootDir:   s.cfg.PXE.BootDir,
		TFTPDir:   s.cfg.PXE.TFTPDir,
		ServerURL: serverURL,
		DB:        s.db,
		MintNodeToken: func(nodeID string) (string, error) {
			return CreateNodeScopedKey(context.Background(), s.db, nodeID)
		},
	}

	// Embedded web UI — served without bearer auth.
	// The UI JavaScript talks to /api/v1 which enforces auth when a token is set.
	staticFS, _ := fs.Sub(ui.StaticFiles, "static")
	fileServer := http.FileServer(http.FS(staticFS))
	r.Handle("/ui/*", http.StripPrefix("/ui", fileServer))
	r.Get("/", serveIndex(staticFS))
	// /login — dedicated login page (served from same static FS as the main UI).
	r.Get("/login", serveLoginPage(staticFS))

	r.Route("/api/v1", func(r chi.Router) {
		// All /api/v1 routes: resolve the API key scope from the Bearer token
		// or the session cookie (ADR-0006). Public endpoints (boot files, node
		// register, logs) accept node-scope keys OR unauthenticated requests.
		r.Use(apiKeyAuth(s.db, s.cfg.AuthDevMode, s.sessionSecret, s.cfg.SessionSecure))

		// Auth endpoints — no scope required (login is pre-auth by definition).
		r.Post("/auth/login", authH.HandleLogin)
		r.Post("/auth/logout", authH.HandleLogout)
		r.Get("/auth/me", authH.HandleMe)

		// Fully public — no key required (PXE-booted nodes before any key is issued).
		r.Get("/boot/ipxe", boot.ServeIPXEScript)
		r.Get("/boot/vmlinuz", boot.ServeVMLinuz)
		r.Get("/boot/initramfs.img", boot.ServeInitramfs)
		r.Get("/boot/ipxe.efi", boot.ServeIPXEEFI)
		r.Get("/boot/undionly.kpxe", boot.ServeUndionlyKPXE)

		// Node-scope callbacks — accept both node and admin keys, or no key (legacy PXE nodes).
		r.Post("/nodes/register", nodes.RegisterNode)
		r.Post("/logs", logs.IngestLogs)
		r.Post("/deploy/progress", progress.IngestProgress)

		// Deploy lifecycle callbacks — require node-scope auth where the key's bound
		// node_id must match the URL {id}. Admin keys also pass (for manual overrides).
		// These are intentionally outside the admin-only group so the deploy agent
		// running in initramfs can call them using its node-scoped key.
		r.With(requireNodeOwnership("id")).Post("/nodes/{id}/deploy-complete", nodes.DeployComplete)
		r.With(requireNodeOwnership("id")).Post("/nodes/{id}/deploy-failed", nodes.DeployFailed)

		// ADR-0008: Post-reboot verification phone-home endpoint.
		// Called by the deployed OS (via clonr-verify-boot.service systemd oneshot)
		// on first boot. Node-scoped token required; admin keys are NOT accepted here.
		// The node-scoped key written to /etc/clonr/node-token at finalize time is
		// the same one minted during PXE enrollment and is reused post-boot.
		r.With(requireNodeOwnership("id")).Post("/nodes/{id}/verify-boot", nodes.VerifyBoot)

		// Self-read: allow a node-scoped key to read its own node record.
		// Used by the deploy agent's state verification loop after deploy-complete.
		// The chi router matches the most specific (longest) path first, so the
		// admin-only GET /nodes/{id} below still applies for admin keys; this route
		// is only reached by node-scoped keys (requireNodeOwnership allows both).
		r.With(requireNodeOwnership("id")).Get("/nodes/{id}/self", nodes.GetNode)

		// Image fetch routes accessible by node-scoped keys (deploy agent reads its assigned image).
		// requireImageAccess handles both admin and node scopes; node keys may only access the
		// image currently assigned to their bound node. Must be outside the admin-only group.
		r.With(requireImageAccess("id", s.db)).Get("/images/{id}", images.GetImage)
		r.With(requireImageAccess("id", s.db)).Get("/images/{id}/blob", images.DownloadBlob)

		// Admin-only routes — require admin scope.
		r.Group(func(r chi.Router) {
			r.Use(requireScope(true)) // admin scope required

			// API key management — admin can create, list, revoke, and rotate keys.
			r.Get("/admin/api-keys", apiKeysH.HandleList)
			r.Post("/admin/api-keys", apiKeysH.HandleCreate)
			r.Delete("/admin/api-keys/{id}", apiKeysH.HandleRevoke)
			r.Post("/admin/api-keys/{id}/rotate", apiKeysH.HandleRotate)

			// Health
			r.Get("/health", health.ServeHTTP)

			// Images — mutating operations are admin-only.
			// GET /images/{id} and GET /images/{id}/blob are registered above with
			// requireImageAccess so node keys can also reach them.
			r.Get("/images", images.ListImages)
			r.Post("/images", images.CreateImage)
			r.Delete("/images/{id}", images.DeleteImage)
			r.Get("/images/{id}/status", images.GetImageStatus)
			r.Get("/images/{id}/disklayout", images.GetDiskLayout)
			r.Put("/images/{id}/disklayout", images.PutDiskLayout)
			r.Post("/images/{id}/blob", images.UploadBlob)
			r.Get("/images/{id}/metadata", images.GetImageMetadata)

			// Factory
			r.Get("/image-roles", factory.ListImageRoles)
			r.Post("/factory/pull", factory.Pull)
			r.Post("/factory/import", factory.Import)
			r.Post("/factory/import-path", factory.ImportPath)
			r.Post("/factory/import-iso", factory.ImportPath) // alias used by the web UI
			r.Post("/factory/capture", factory.Capture)
			r.Post("/factory/build-from-iso", factory.BuildFromISO)

			// ISO build observability — stream must come before plain snapshot route.
			r.Get("/images/{id}/build-progress/stream", buildProgressH.StreamBuildProgress)
			r.Get("/images/{id}/build-progress", buildProgressH.GetBuildProgress)
			r.Get("/images/{id}/build-log", buildProgressH.GetBuildLog)
			r.Get("/images/{id}/build-manifest", buildProgressH.GetBuildManifest)

			// Build resume (F2) — resume an interrupted build from last phase.
			r.Post("/images/{id}/resume", resumeH.ResumeImageBuild)

			// System initramfs management (F1).
			r.Get("/system/initramfs", initramfsH.GetInitramfs)
			r.Post("/system/initramfs/rebuild", initramfsH.RebuildInitramfs)
			r.Delete("/system/initramfs/history/{id}", initramfsH.DeleteInitramfsHistory)

			// Shell sessions
			r.Post("/images/{id}/shell-session", factory.OpenShellSession)
			r.Delete("/images/{id}/shell-session/{sid}", factory.CloseShellSession)
			r.Post("/images/{id}/shell-session/{sid}/exec", factory.ExecInSession)
			r.Get("/images/{id}/shell-session/{sid}/ws", factory.ShellWS)

			// Active deploy detection (for shell modal warning)
			r.Get("/images/{id}/active-deploys", factory.ActiveDeploys)

			// Nodes — by-mac must be before /{id} to avoid chi match ambiguity.
			r.Get("/nodes/by-mac/{mac}", nodes.GetNodeByMAC)
			r.Get("/nodes", nodes.ListNodes)
			r.Post("/nodes", nodes.CreateNode)
			r.Get("/nodes/{id}", nodes.GetNode)
			r.Put("/nodes/{id}", nodes.UpdateNode)
			r.Delete("/nodes/{id}", nodes.DeleteNode)

			// Disk layout hierarchy — node-level overrides, group assignment,
			// hardware-aware recommendations, and validation.
			r.Get("/nodes/{id}/layout-recommendation", layoutH.GetLayoutRecommendation)
			r.Get("/nodes/{id}/effective-layout", layoutH.GetEffectiveLayout)
			r.Put("/nodes/{id}/layout-override", layoutH.SetNodeLayoutOverride)
			r.Post("/nodes/{id}/layout/validate", layoutH.ValidateLayout)
			r.Put("/nodes/{id}/group", layoutH.AssignNodeGroup)
			r.Get("/nodes/{id}/effective-mounts", layoutH.GetEffectiveMounts)

			// Node groups — named sets of nodes sharing a disk layout override.
			r.Get("/node-groups", nodeGroups.ListNodeGroups)
			r.Post("/node-groups", nodeGroups.CreateNodeGroup)
			r.Get("/node-groups/{id}", nodeGroups.GetNodeGroup)
			r.Put("/node-groups/{id}", nodeGroups.UpdateNodeGroup)
			r.Delete("/node-groups/{id}", nodeGroups.DeleteNodeGroup)
			// Group membership management.
			r.Post("/node-groups/{id}/members", nodeGroups.AddGroupMembers)
			r.Delete("/node-groups/{id}/members/{node_id}", nodeGroups.RemoveGroupMember)
			// Rolling group reimage.
			r.Post("/node-groups/{id}/reimage", nodeGroups.ReimageGroup)
			// Group reimage job status polling.
			r.Get("/reimages/jobs/{jobID}", nodeGroups.GetGroupReimageJob)
			r.Post("/reimages/jobs/{jobID}/resume", nodeGroups.ResumeGroupReimageJob)

			// IPMI / power management — subpaths of /nodes/{id} must be
			// registered in the same chi group so the auth middleware applies.
			r.Get("/nodes/{id}/power", ipmiH.GetPowerStatus)
			r.Post("/nodes/{id}/power/on", ipmiH.PowerOn)
			r.Post("/nodes/{id}/power/off", ipmiH.PowerOff)
			r.Post("/nodes/{id}/power/cycle", ipmiH.PowerCycle)
			r.Post("/nodes/{id}/power/reset", ipmiH.PowerReset)
			r.Post("/nodes/{id}/power/pxe", ipmiH.SetBootPXE)
			r.Post("/nodes/{id}/power/disk", ipmiH.SetBootDisk)
			r.Post("/nodes/{id}/power/flip-to-disk", powerH.FlipToDisk)
			r.Get("/nodes/{id}/sensors", ipmiH.GetSensors)

			// Reimage — queue, track and retry node reimages via the power provider.
			r.Post("/nodes/{id}/reimage", reimageH.Create)
			r.Get("/nodes/{id}/reimage", reimageH.ListForNode)
			r.Get("/reimage/{id}", reimageH.Get)
			r.Delete("/reimage/{id}", reimageH.Cancel)
			r.Post("/reimage/{id}/retry", reimageH.Retry)
			r.Get("/reimages", reimageH.List)

			// Logs — stream must be registered before plain /logs.
			r.Get("/logs/stream", logs.StreamLogs)
			r.Get("/logs", logs.QueryLogs)

			// Deployment progress — stream must be registered before plain routes.
			r.Get("/deploy/progress/stream", progress.StreamProgress)
			r.Get("/deploy/progress/{mac}", progress.GetProgress)
			r.Get("/deploy/progress", progress.ListProgress)
		})
	})

	return r
}

// serveLoginPage serves login.html from the embedded static FS.
func serveLoginPage(staticFS fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := staticFS.Open("login.html")
		if err != nil {
			http.Error(w, "login page not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "login page not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "login.html", stat.ModTime(), f.(io.ReadSeeker))
	}
}

// buildAuthHandler constructs the AuthHandler with closures that call into
// the server's DB and session-signing functions. This avoids the handlers
// package importing the server package (which would be circular).
func (s *Server) buildAuthHandler() *handlers.AuthHandler {
	const cookieName = "clonr_session"

	loginFn := func(rawKey string) (keyPrefix string, scope string, ok bool) {
		// Strip typed prefix before hashing, same as apiKeyAuth middleware.
		hashInput := rawKey
		for _, pfx := range []string{"clonr-admin-", "clonr-node-"} {
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
		// Login only allows admin-scope keys.
		if lookupResult.Scope != api.KeyScopeAdmin {
			return "", "", false
		}
		sc := lookupResult.Scope
		kid := hashInput
		if len(kid) > 8 {
			kid = kid[:8]
		}
		return kid, string(sc), true
	}

	signFn := func(keyPrefix string) (string, time.Time, error) {
		p := newSessionPayload(keyPrefix)
		token, err := signSessionToken(s.sessionSecret, p)
		if err != nil {
			return "", time.Time{}, err
		}
		return token, time.Unix(p.EXP, 0), nil
	}

	validateFn := func(token string) (scope string, exp time.Time, needsReissue bool, newToken string, ok bool) {
		result, err := validateSessionToken(s.sessionSecret, token)
		if err != nil {
			return "", time.Time{}, false, "", false
		}
		reissued := ""
		if result.needsReissue {
			slid := slideSessionPayload(result.payload)
			if t, serr := signSessionToken(s.sessionSecret, slid); serr == nil {
				reissued = t
				result.payload = slid
			}
		}
		return result.payload.Scope, time.Unix(result.payload.EXP, 0), result.needsReissue, reissued, true
	}

	return &handlers.AuthHandler{
		Login:      loginFn,
		Sign:       signFn,
		Validate:   validateFn,
		CookieName: cookieName,
		Secure:     s.cfg.SessionSecure,
	}
}

// buildAPIKeysHandler constructs the APIKeysHandler with closures that call into
// the server's DB and key-generation functions without causing circular imports.
func (s *Server) buildAPIKeysHandler() *handlers.APIKeysHandler {
	mintFn := func(r *http.Request, scope api.KeyScope, nodeID, label, createdBy string, expiresAt *time.Time) (string, string, string, error) {
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

// serveIndex serves index.html from the embedded static FS.
func serveIndex(staticFS fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := staticFS.Open("index.html")
		if err != nil {
			http.Error(w, "UI not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "UI not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", stat.ModTime(), f.(io.ReadSeeker))
	}
}

// Handler returns the underlying http.Handler for use in tests.
func (s *Server) Handler() http.Handler {
	return s.router
}

// ListenAndServe starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()

		// Give in-flight builds up to 25 seconds to finish naturally before
		// we force-cancel them. HTTP shutdown gets its own 5-second window on
		// top of that. Total wall-clock budget: 30 seconds.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer drainCancel()

		log.Info().Msg("shutdown: waiting for in-flight builds to complete (up to 25s)")
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
