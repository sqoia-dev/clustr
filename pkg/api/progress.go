package api

import "time"

// DeployProgress represents real-time progress of a deployment on a single node.
// It is POSTed by the clonr client to /api/v1/deploy/progress and streamed to
// the UI via Server-Sent Events at /api/v1/deploy/progress/stream.
type DeployProgress struct {
	NodeMAC    string    `json:"node_mac"`
	Hostname   string    `json:"hostname,omitempty"`
	Phase      string    `json:"phase"`       // "preflight", "partitioning", "formatting", "downloading", "extracting", "finalizing", "complete", "error"
	PhaseIndex int       `json:"phase_index"` // 1-based ordinal
	PhaseTotal int       `json:"phase_total"` // total phases (6)
	BytesDone  int64     `json:"bytes_done"`  // bytes processed in current phase
	BytesTotal int64     `json:"bytes_total"` // total bytes for current phase (0 if unknown)
	Speed      int64     `json:"speed_bps"`   // bytes/sec
	ETA        int       `json:"eta_seconds"` // estimated seconds remaining in phase
	Message    string    `json:"message,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	Error      string    `json:"error,omitempty"`
}
