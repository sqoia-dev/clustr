package server

import (
	"context"
	"time"

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

// runSelfmon is the control-plane self-monitoring goroutine.
// It collects host metrics every 30s and persists them as node_stats rows
// using the control-plane host ID as the node_id.
//
// The same MetricsIngest path (InsertStatsBatch) used by cluster nodes over
// WebSocket is used here, so the alert engine sees control-plane samples
// through the identical code path.
//
// WatchdogSec integration: the goroutine touches the heartbeat file on every
// successful tick so that systemd's watchdog (WatchdogSec=30) and the
// clustr-selfmon-watchdog.timer can both detect a hung collector.
//
// THREAD-SAFETY: runs in a single dedicated goroutine. The collector.Collector
// it owns must not be shared.
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

	collect := func() {
		// Touch the heartbeat at the START of every tick so that a slow or
		// hung collector (e.g. collectSystemd blocking on dbus) does not cause
		// the meta-watchdog to fire a spurious WatchdogSec kill. The heartbeat
		// signals "this goroutine is scheduled and running", not "all metrics
		// collected successfully".
		collector.TouchHeartbeat(selfmonHeartbeatPath)

		tickCtx, cancel := context.WithTimeout(ctx, selfmonInterval/2)
		defer cancel()

		samples := c.Collect(tickCtx)

		// Append cert expiry samples.
		certPaths := []string{
			"/etc/clustr/tls/server.crt",
			"/etc/clustr/tls/ca.crt",
		}
		samples = append(samples, collector.CollectCertExpiry(certPaths, time.Now().UTC())...)

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
