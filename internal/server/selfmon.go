package server

import (
	"context"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clustr/internal/db"
	"github.com/sqoia-dev/clustr/internal/metrics/collector"
)

const (
	// selfmonInterval is how often the control-plane self-monitoring collector runs.
	// Matches the clientd default of 30s so alert-engine windows see similar density.
	selfmonInterval = 30 * time.Second

	// selfmonHeartbeatPath is the file the selfmon goroutine touches every tick.
	// Monitored by clustr-selfmon-watchdog.service to detect a stalled collector.
	selfmonHeartbeatPath = "/run/clustr/selfmon.heartbeat"
)

// selfmonRestartWindow is the lookback for the serverd_restarts_delta_10m metric.
// 10 minutes at a 30s tick rate yields ~20 samples.
const selfmonRestartWindow = 10 * time.Minute

// runSelfmon is the control-plane self-monitoring goroutine.
// It collects host metrics every 30s and persists them as node_stats rows
// using the control-plane host ID as the node_id.
//
// The same MetricsIngest path (InsertStatsBatch) used by cluster nodes over
// WebSocket is used here, so the alert engine sees control-plane samples
// through the identical code path.
//
// WatchdogSec integration: the goroutine sends sd_notify(WATCHDOG=1) and
// touches the heartbeat file on every tick.  WatchdogSec=90 in the unit
// gives 3 ticks of margin before systemd kills the process.
//
// THREAD-SAFETY: runs in a single dedicated goroutine. The collector.Collector
// it owns must not be shared.  The RestartWindow is independently thread-safe.
func (s *Server) runSelfmon(ctx context.Context) {
	// Ensure the control-plane host row exists in the hosts table.
	cpHost, err := s.db.BootstrapControlPlaneHost(ctx)
	if err != nil {
		log.Error().Err(err).Msg("selfmon: failed to bootstrap control-plane host row — self-monitoring disabled")
		return
	}

	log.Info().
		Str("host_id", cpHost.ID).
		Str("hostname", cpHost.Hostname).
		Msg("selfmon: started")

	c := collector.New()

	// restartWin tracks NRestarts over a 10-minute rolling window so the alert
	// rule uses a delta (rate of restarts) rather than the monotonic cumulative
	// count.  On a fresh install, Delta() returns 0 for the first window.
	restartWin := collector.NewRestartWindow(selfmonRestartWindow)

	collect := func() {
		// Touch the heartbeat at the START of every tick so that a slow or
		// hung collector (e.g. collectSystemd blocking on dbus) does not cause
		// the meta-watchdog to fire a spurious WatchdogSec kill. The heartbeat
		// signals "this goroutine is scheduled and running", not "all metrics
		// collected successfully".
		collector.TouchHeartbeat(selfmonHeartbeatPath)

		// Send WATCHDOG=1 to systemd on every tick (every 30s).
		// WatchdogSec=90 in the unit gives 3 ticks of margin before the process
		// is killed. SdNotify returns (false, nil) when NOTIFY_SOCKET is not set
		// (i.e. running outside systemd — local dev, tests) so this is safe in
		// all environments.
		_, _ = daemon.SdNotify(false, daemon.SdNotifyWatchdog)

		tickCtx, cancel := context.WithTimeout(ctx, selfmonInterval/2)
		defer cancel()

		now := time.Now().UTC()
		samples := c.Collect(tickCtx)

		// Feed NRestarts into the rolling window and append the delta sample.
		// The raw serverd_restarts sample is kept for visibility; the rule
		// cp.serverd.restart.crit now targets serverd_restarts_delta_10m.
		for _, sp := range samples {
			if sp.Plugin == "systemd" && sp.Sensor == "serverd_restarts" {
				restartWin.Add(sp.Value, now)
				break
			}
		}
		samples = append(samples, collector.Sample{
			Plugin: "systemd",
			Sensor: "serverd_restarts_delta_10m",
			Value:  restartWin.Delta(),
			Unit:   "count",
			TS:     now,
		})

		// Append cert expiry samples.
		certPaths := []string{
			"/etc/clustr/tls/server.crt",
			"/etc/clustr/tls/ca.crt",
		}
		samples = append(samples, collector.CollectCertExpiry(certPaths, now)...)

		if len(samples) == 0 {
			return
		}

		// Convert to db.NodeStatRow using the control-plane host ID as node_id.
		rows := make([]db.NodeStatRow, 0, len(samples))
		for _, s := range samples {
			rows = append(rows, db.NodeStatRow{
				NodeID: cpHost.ID,
				Plugin: s.Plugin,
				Sensor: s.Sensor,
				Value:  s.Value,
				Unit:   s.Unit,
				Labels: s.Labels,
				TS:     s.TS,
			})
		}

		if err := s.db.InsertStatsBatch(tickCtx, rows); err != nil {
			log.Error().Err(err).
				Str("host_id", cpHost.ID).
				Int("samples", len(rows)).
				Msg("selfmon: InsertStatsBatch failed")
		}

		log.Debug().
			Str("host_id", cpHost.ID).
			Int("samples", len(rows)).
			Msg("selfmon: tick complete")
	}

	// Collect once immediately on startup.
	collect()

	ticker := time.NewTicker(selfmonInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("selfmon: stopped")
			return
		case <-ticker.C:
			collect()
		}
	}
}
