package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"go.yaml.in/yaml/v2"

	"github.com/sqoia-dev/clustr/internal/db"
)

const (
	// engineTickInterval is how often the engine evaluates all rules.
	engineTickInterval = 60 * time.Second

	// ruleDirReloadInterval is how often the engine polls rules.d for mtime changes.
	ruleDirReloadInterval = 60 * time.Second

	// defaultRulesDir is the default location for YAML rule files.
	defaultRulesDir = "/etc/clustr/rules.d"
)

// StatsQuerier abstracts node_stats queries used by the engine.
// Satisfied by *db.DB directly via the adapter in the server package.
type StatsQuerier interface {
	QueryNodeStats(ctx context.Context, p db.QueryNodeStatsParams) ([]db.NodeStatRow, bool, error)
}

// NodeLister returns all registered node IDs.
// Used for the cluster-nodes-offline meta-rule.
type NodeLister interface {
	ListNodeConfigs(ctx context.Context, baseImageID string) ([]interface{ GetID() string }, error)
}

// HeartbeatChecker returns the last heartbeat time for a node.
type HeartbeatChecker interface {
	GetLastSeen(ctx context.Context, nodeID string) (time.Time, bool, error)
}

// Engine is the alert rule evaluator. Create it with New() and call Run()
// inside a goroutine; cancel the context to stop it.
//
// THREAD-SAFETY: Engine is NOT safe for concurrent use. All methods assume the
// single-Run-goroutine invariant (see Engine.Run). External callers must not
// invoke Engine methods from other goroutines. State accessed by Engine methods
// is reached through StateStore, which has its own thread-safety contract.
type Engine struct {
	rulesDir string
	stats    StatsQuerier
	nodeDB   *db.DB // for last_seen_at queries (offline node rule)
	store    *StateStore
	silences *SilenceStore // may be nil if silences table not available
	dispatcher *Dispatcher

	mu    sync.RWMutex // protects rules and ruleMtimes
	rules []*Rule
	// tracks mtime per file so we only reload when something changed
	ruleMtimes map[string]time.Time
}

// New creates an Engine.
//
//   - rulesDir: path to the directory containing *.yml rule files (e.g. /etc/clustr/rules.d).
//     If empty, defaults to defaultRulesDir.
//   - stats: the node_stats query backend.
//   - nodeDB: the full *db.DB, used for per-node last_seen_at queries.
//   - store: the alert state store.
//   - silences: the silence store; may be nil (silences are skipped).
//   - dispatcher: the outbound notification dispatcher.
func New(rulesDir string, stats StatsQuerier, nodeDB *db.DB, store *StateStore, silences *SilenceStore, dispatcher *Dispatcher) *Engine {
	if rulesDir == "" {
		rulesDir = defaultRulesDir
	}
	return &Engine{
		rulesDir:   rulesDir,
		stats:      stats,
		nodeDB:     nodeDB,
		store:      store,
		silences:   silences,
		dispatcher: dispatcher,
		ruleMtimes: make(map[string]time.Time),
	}
}

// isSilenced returns true when a current silence covers (ruleName, nodeID).
// Returns false on any DB error so we never suppress alerts due to query failures.
func (e *Engine) isSilenced(ctx context.Context, ruleName, nodeID string) bool {
	if e.silences == nil {
		return false
	}
	ok, err := e.silences.IsSilenced(ctx, ruleName, nodeID)
	if err != nil {
		log.Warn().Err(err).Str("rule", ruleName).Str("node_id", nodeID).
			Msg("alerts: silence check failed — treating as not silenced")
		return false
	}
	return ok
}

// Run starts the engine loop.  Blocks until ctx is cancelled.
// Call in a dedicated goroutine: go engine.Run(ctx).
func (e *Engine) Run(ctx context.Context) {
	log.Info().Str("rules_dir", e.rulesDir).Msg("alerts: engine started")

	// Load rules once immediately on start.
	e.reloadRulesIfChanged()

	tick := time.NewTicker(engineTickInterval)
	reloadTick := time.NewTicker(ruleDirReloadInterval)
	defer tick.Stop()
	defer reloadTick.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("alerts: engine stopped")
			return
		case <-reloadTick.C:
			e.reloadRulesIfChanged()
		case <-tick.C:
			e.evaluate(ctx)
		}
	}
}

// Reload forces an immediate rule reload.  Safe to call from any goroutine
// (e.g. from a SIGHUP handler).
func (e *Engine) Reload() {
	e.reloadRulesIfChanged()
}

// Rules returns a snapshot of the currently loaded rules (copy).
func (e *Engine) Rules() []*Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// ─── rule loading ─────────────────────────────────────────────────────────────

// reloadRulesIfChanged scans rulesDir and reloads rule files only when at
// least one mtime has changed since the last load.  Malformed files are logged
// and skipped; they do not crash the engine.
func (e *Engine) reloadRulesIfChanged() {
	entries, err := os.ReadDir(e.rulesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn().Err(err).Str("dir", e.rulesDir).Msg("alerts: cannot read rules dir")
		}
		// dir missing is not an error — engine runs with zero rules.
		return
	}

	// Build a map of current file→mtime for *.yml files.
	current := make(map[string]time.Time)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		current[filepath.Join(e.rulesDir, name)] = info.ModTime()
	}

	// Quick mtime comparison.
	e.mu.RLock()
	changed := len(current) != len(e.ruleMtimes)
	if !changed {
		for path, mt := range current {
			if prev, ok := e.ruleMtimes[path]; !ok || !prev.Equal(mt) {
				changed = true
				break
			}
		}
	}
	e.mu.RUnlock()

	if !changed {
		return
	}

	log.Info().Str("dir", e.rulesDir).Msg("alerts: reloading rules")
	var loaded []*Rule
	names := make(map[string]struct{})

	// Sort file paths for deterministic load order.
	paths := make([]string, 0, len(current))
	for p := range current {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		rules, err := loadRuleFile(path)
		if err != nil {
			log.Error().Err(err).Str("file", path).Msg("alerts: skipping malformed rule file")
			continue
		}
		for _, rule := range rules {
			if _, dup := names[rule.Name]; dup {
				log.Error().Str("rule", rule.Name).Str("file", path).
					Msg("alerts: duplicate rule name — skipping")
				continue
			}
			names[rule.Name] = struct{}{}
			loaded = append(loaded, rule)
		}
	}

	e.mu.Lock()
	e.rules = loaded
	e.ruleMtimes = current
	e.mu.Unlock()

	log.Info().Int("count", len(loaded)).Msg("alerts: rules loaded")
}

// loadRuleFile parses one or more YAML rule documents from a single file.
// Multi-document YAML (--- separator) is supported so that large rule sets like
// control-plane.yaml can live in a single file without one-file-per-rule bloat.
// Single-document files (the legacy format) are parsed identically.
func loadRuleFile(path string) ([]*Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Split into individual YAML documents on "---" separators, then parse each.
	// This is more reliable than using the Decoder for detecting truly empty docs.
	docs := splitYAMLDocs(data)
	var rules []*Rule
	for _, doc := range docs {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue // skip blank documents (trailing --- etc.)
		}
		var r Rule
		if err := yaml.Unmarshal(doc, &r); err != nil {
			return nil, err
		}
		r.sourceFile = path
		if err := r.Validate(); err != nil {
			return nil, err
		}
		rules = append(rules, &r)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	return rules, nil
}

// splitYAMLDocs splits a multi-document YAML byte slice on "---" document
// separators.  The separator must appear on its own line.
func splitYAMLDocs(data []byte) [][]byte {
	var docs [][]byte
	sep := []byte("\n---")
	// Handle leading "---" at the start of the file.
	trimmed := bytes.TrimPrefix(data, []byte("---\n"))
	parts := bytes.Split(append([]byte("\n"), trimmed...), sep)
	for _, p := range parts {
		// Strip the leading newline we injected above.
		p = bytes.TrimPrefix(p, []byte("\n"))
		docs = append(docs, p)
	}
	return docs
}

// ─── evaluation ───────────────────────────────────────────────────────────────

// evaluate runs one full evaluation tick: check all rules against all nodes.
func (e *Engine) evaluate(ctx context.Context) {
	e.mu.RLock()
	rules := make([]*Rule, len(e.rules))
	copy(rules, e.rules)
	e.mu.RUnlock()

	if len(rules) == 0 {
		return
	}

	for _, r := range rules {
		e.evaluateRule(ctx, r)
	}
}

// evaluateRule evaluates a single rule against node_stats.
func (e *Engine) evaluateRule(ctx context.Context, r *Rule) {
	// cluster-nodes-offline is a meta-rule that checks node presence, not stats.
	// It only applies to cluster_node role.
	if r.Plugin == "_meta" && r.Sensor == "node_offline" {
		if r.AppliesToRole(HostRoleClusterNode) {
			e.evaluateOfflineRule(ctx, r)
		}
		return
	}

	// Determine the time window.  duration=0 means "any sample in the last tick".
	window := r.Duration
	if window < engineTickInterval {
		window = engineTickInterval
	}
	since := time.Now().Add(-window)
	until := time.Now()

	// We query without specifying node_id (empty = all nodes) — but QueryNodeStats
	// requires a node_id.  To evaluate across all nodes we need to get all nodes
	// and query per-node.  Use a dedicated query.
	e.evaluateRuleAllNodes(ctx, r, since, until)
}

// evaluateRuleAllNodes queries node_stats for all hosts that have data for
// (plugin, sensor) in the window, filtered by the rule's host_role selector,
// and runs the threshold check per-group.
func (e *Engine) evaluateRuleAllNodes(ctx context.Context, r *Rule, since, until time.Time) {
	// Build the query that filters node_ids by the rule's host role.
	// - cluster_node: node_id must appear in node_configs (cluster nodes only)
	// - control_plane: node_id must appear in hosts WHERE role='control_plane'
	// - any: no role filter
	var (
		nodeRowsQuery string
		nodeRowsArgs  []interface{}
	)
	switch r.EffectiveHostRole() {
	case HostRoleControlPlane:
		nodeRowsQuery = `SELECT DISTINCT ns.node_id FROM node_stats ns
		 INNER JOIN hosts h ON h.id = ns.node_id AND h.role = 'control_plane'
		 WHERE ns.plugin = ? AND ns.sensor = ? AND ns.ts >= ? AND ns.ts <= ?`
		nodeRowsArgs = []interface{}{r.Plugin, r.Sensor, since.Unix(), until.Unix()}
	case HostRoleAny:
		nodeRowsQuery = `SELECT DISTINCT node_id FROM node_stats
		 WHERE plugin = ? AND sensor = ? AND ts >= ? AND ts <= ?`
		nodeRowsArgs = []interface{}{r.Plugin, r.Sensor, since.Unix(), until.Unix()}
	default: // cluster_node
		nodeRowsQuery = `SELECT DISTINCT ns.node_id FROM node_stats ns
		 INNER JOIN node_configs nc ON nc.id = ns.node_id
		 WHERE ns.plugin = ? AND ns.sensor = ? AND ns.ts >= ? AND ns.ts <= ?`
		nodeRowsArgs = []interface{}{r.Plugin, r.Sensor, since.Unix(), until.Unix()}
	}

	type nodeEntry struct{ id string }
	nodeRows, err := e.nodeDB.SQL().QueryContext(ctx, nodeRowsQuery, nodeRowsArgs...)
	if err != nil {
		log.Warn().Err(err).Str("rule", r.Name).Msg("alerts: query node list failed")
		return
	}
	var nodeIDs []string
	for nodeRows.Next() {
		var id string
		if err := nodeRows.Scan(&id); err != nil {
			continue
		}
		nodeIDs = append(nodeIDs, id)
	}
	nodeRows.Close()

	for _, nodeID := range nodeIDs {
		rows, _, err := e.stats.QueryNodeStats(ctx, db.QueryNodeStatsParams{
			NodeID: nodeID,
			Plugin: r.Plugin,
			Sensor: r.Sensor,
			Since:  since,
			Until:  until,
			Limit:  10000,
		})
		if err != nil {
			log.Warn().Err(err).Str("rule", r.Name).Str("node_id", nodeID).
				Msg("alerts: stats query failed")
			continue
		}
		if len(rows) == 0 {
			continue
		}

		// Group rows by label-tuple.
		groups := groupByLabels(rows)
		for labelsJSON, groupRows := range groups {
			// Parse the label map from the group key.
			var labels map[string]string
			if labelsJSON != "" {
				_ = json.Unmarshal([]byte(labelsJSON), &labels)
			}

			// Skip groups whose labels don't match the rule's label patterns.
			if !r.MatchesLabels(labels) {
				continue
			}

			// Check if ALL samples in the window satisfy the threshold.
			allSatisfy := true
			lastValue := groupRows[len(groupRows)-1].Value
			for _, row := range groupRows {
				if !r.Threshold.Evaluate(row.Value) {
					allSatisfy = false
					break
				}
			}

			key := alertStateKey{
				ruleName:   r.Name,
				nodeID:     nodeID,
				sensor:     r.Sensor,
				labelsJSON: labelsJSON,
			}

			if allSatisfy {
				if !e.store.IsActive(key) {
					// Transition to firing.
					alert, err := e.store.Fire(ctx, r, nodeID, labels, lastValue)
					if err != nil {
						log.Error().Err(err).Str("rule", r.Name).Str("node_id", nodeID).
							Msg("alerts: fire failed")
						continue
					}
					log.Info().Str("rule", r.Name).Str("node_id", nodeID).
						Float64("value", lastValue).Msg("alerts: FIRING")
					if !e.isSilenced(ctx, r.Name, nodeID) {
						e.dispatcher.Fire(ctx, r, alert)
					}
				} else {
					// Still firing — update the last_value but don't re-dispatch.
					e.store.UpdateLastValue(ctx, key, lastValue)
				}
			} else {
				if e.store.IsActive(key) {
					// Transition to resolved.
					alert, err := e.store.Resolve(ctx, key, lastValue)
					if err != nil {
						log.Error().Err(err).Str("rule", r.Name).Str("node_id", nodeID).
							Msg("alerts: resolve failed")
						continue
					}
					if alert != nil {
						log.Info().Str("rule", r.Name).Str("node_id", nodeID).
							Float64("value", lastValue).Msg("alerts: RESOLVED")
						e.dispatcher.Resolve(ctx, r, alert)
					}
				}
			}
		}
	}

	// Also check for active alerts on nodes that no longer have data in the
	// window (data gap = resolved by absence).  Only do this if the duration
	// window has passed.
	e.resolveStaleAlerts(ctx, r, nodeIDs)
}

// resolveStaleAlerts resolves active alerts for nodes that no longer appear
// in the active node set within the evaluation window.
func (e *Engine) resolveStaleAlerts(ctx context.Context, r *Rule, activeNodeIDs []string) {
	activeSet := make(map[string]struct{}, len(activeNodeIDs))
	for _, id := range activeNodeIDs {
		activeSet[id] = struct{}{}
	}

	// Iterate over a snapshot of active keys to avoid modifying the map while
	// ranging over it inside Resolve.
	type candidate struct {
		key    alertStateKey
		nodeID string
	}
	var toResolve []candidate
	for _, key := range e.store.ActiveKeys() {
		if key.ruleName != r.Name {
			continue
		}
		if _, inWindow := activeSet[key.nodeID]; !inWindow {
			toResolve = append(toResolve, candidate{key: key, nodeID: key.nodeID})
		}
	}
	for _, c := range toResolve {
		alert, err := e.store.Resolve(ctx, c.key, 0)
		if err != nil {
			log.Error().Err(err).Str("rule", r.Name).Str("node_id", c.nodeID).
				Msg("alerts: stale resolve failed")
			continue
		}
		if alert != nil {
			log.Info().Str("rule", r.Name).Str("node_id", c.nodeID).
				Msg("alerts: RESOLVED (data gap)")
			e.dispatcher.Resolve(ctx, r, alert)
		}
	}
}

// evaluateOfflineRule handles the cluster-nodes-offline meta-rule.
// It fires when any registered node has not pushed a sample in the last
// duration window (using last_seen_at from node_configs).
func (e *Engine) evaluateOfflineRule(ctx context.Context, r *Rule) {
	window := r.Duration
	if window <= 0 {
		window = 120 * time.Second
	}
	cutoff := time.Now().Add(-window).Unix()

	// Find nodes whose last_seen_at is older than the window.
	rows, err := e.nodeDB.SQL().QueryContext(ctx,
		`SELECT id FROM node_configs WHERE last_seen_at IS NOT NULL AND last_seen_at < ?`,
		cutoff)
	if err != nil {
		log.Warn().Err(err).Str("rule", r.Name).Msg("alerts: offline rule: query failed")
		return
	}
	var offlineIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		offlineIDs = append(offlineIDs, id)
	}
	rows.Close()

	// Check nodes that also have a last_seen_at = NULL (never seen) as offline.
	nullRows, err := e.nodeDB.SQL().QueryContext(ctx,
		`SELECT id FROM node_configs WHERE last_seen_at IS NULL`)
	if err == nil {
		for nullRows.Next() {
			var id string
			if err := nullRows.Scan(&id); err == nil {
				offlineIDs = append(offlineIDs, id)
			}
		}
		nullRows.Close()
	}

	// Build the set of currently offline nodes.
	offlineSet := make(map[string]struct{}, len(offlineIDs))
	for _, id := range offlineIDs {
		offlineSet[id] = struct{}{}
	}

	// Fire for newly offline nodes.
	for _, nodeID := range offlineIDs {
		key := alertStateKey{
			ruleName:   r.Name,
			nodeID:     nodeID,
			sensor:     r.Sensor,
			labelsJSON: "",
		}
		if !e.store.IsActive(key) {
			alert, err := e.store.Fire(ctx, r, nodeID, nil, 0)
			if err != nil {
				log.Error().Err(err).Str("rule", r.Name).Str("node_id", nodeID).
					Msg("alerts: offline fire failed")
				continue
			}
			log.Info().Str("rule", r.Name).Str("node_id", nodeID).
				Msg("alerts: node OFFLINE — FIRING")
			if !e.isSilenced(ctx, r.Name, nodeID) {
				e.dispatcher.Fire(ctx, r, alert)
			}
		}
	}

	// Resolve for nodes that have come back online.
	type candidate struct{ key alertStateKey }
	var toResolve []candidate
	for _, key := range e.store.ActiveKeys() {
		if key.ruleName != r.Name {
			continue
		}
		if _, stillOffline := offlineSet[key.nodeID]; !stillOffline {
			toResolve = append(toResolve, candidate{key: key})
		}
	}
	for _, c := range toResolve {
		alert, err := e.store.Resolve(ctx, c.key, 0)
		if err != nil {
			log.Error().Err(err).Str("rule", r.Name).Str("node_id", c.key.nodeID).
				Msg("alerts: offline resolve failed")
			continue
		}
		if alert != nil {
			log.Info().Str("rule", r.Name).Str("node_id", c.key.nodeID).
				Msg("alerts: node ONLINE — RESOLVED")
			e.dispatcher.Resolve(ctx, r, alert)
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// groupByLabels groups stats rows by their canonical labels_json key.
// The key is "" for rows with no labels.
func groupByLabels(rows []db.NodeStatRow) map[string][]db.NodeStatRow {
	out := make(map[string][]db.NodeStatRow)
	for _, row := range rows {
		key := labelsToJSON(row.Labels)
		out[key] = append(out[key], row)
	}
	return out
}
