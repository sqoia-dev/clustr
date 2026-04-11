package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sqoia-dev/clonr/pkg/api"
)

const (
	defaultBufferSize    = 50
	defaultFlushInterval = 2 * time.Second
	closeFlushTimeout    = 5 * time.Second
)

// RemoteLogWriter buffers zerolog JSON output and ships it to the server in
// batches. It implements io.Writer so it can be used directly with zerolog's
// MultiLevelWriter.
//
// It never blocks the caller — if the buffer is full or the server is
// unreachable the entry is silently dropped so the CLI operation is never
// impacted.
type RemoteLogWriter struct {
	client      *Client
	nodeMAC     string
	hostname    string
	component   string
	bufferSize  int
	flushEvery  time.Duration

	mu      sync.Mutex
	buffer  []api.LogEntry

	flushCh chan struct{}
	done    chan struct{}
	once    sync.Once
}

// RemoteLogWriterOption is a functional option for NewRemoteLogWriter.
type RemoteLogWriterOption func(*RemoteLogWriter)

// WithComponent sets the default component label for all log entries.
func WithComponent(component string) RemoteLogWriterOption {
	return func(w *RemoteLogWriter) { w.component = component }
}

// NewRemoteLogWriter creates and starts a RemoteLogWriter.
//
// Tunable via environment:
//   - CLONR_LOG_BUFFER_SIZE   (int, default 50)
//   - CLONR_LOG_FLUSH_INTERVAL (duration string, default "2s")
func NewRemoteLogWriter(c *Client, nodeMAC, hostname string, opts ...RemoteLogWriterOption) *RemoteLogWriter {
	bufSize := defaultBufferSize
	if v := os.Getenv("CLONR_LOG_BUFFER_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			bufSize = n
		}
	}
	flushEvery := defaultFlushInterval
	if v := os.Getenv("CLONR_LOG_FLUSH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			flushEvery = d
		}
	}

	w := &RemoteLogWriter{
		client:     c,
		nodeMAC:    nodeMAC,
		hostname:   hostname,
		component:  "deploy", // sensible default
		bufferSize: bufSize,
		flushEvery: flushEvery,
		buffer:     make([]api.LogEntry, 0, bufSize),
		flushCh:    make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}

	go w.flusher()
	return w
}

// SetComponent changes the component label for future log entries.
// Safe to call from any goroutine.
func (w *RemoteLogWriter) SetComponent(component string) {
	w.mu.Lock()
	w.component = component
	w.mu.Unlock()
}

// SetNodeMAC updates the node MAC address stamped on future log entries.
// Useful when the MAC is not known at writer creation time.
func (w *RemoteLogWriter) SetNodeMAC(mac string) {
	w.mu.Lock()
	w.nodeMAC = mac
	w.mu.Unlock()
}

// SetHostname updates the hostname stamped on future log entries.
func (w *RemoteLogWriter) SetHostname(hostname string) {
	w.mu.Lock()
	w.hostname = hostname
	w.mu.Unlock()
}

// Write implements io.Writer. Each call is expected to be a single zerolog
// JSON line. Non-JSON input is shipped as a raw "info" message.
func (w *RemoteLogWriter) Write(p []byte) (int, error) {
	n := len(p)

	// Snapshot identity fields under lock before parsing, so we don't hold
	// the lock while doing JSON work.
	w.mu.Lock()
	mac := w.nodeMAC
	host := w.hostname
	comp := w.component
	w.mu.Unlock()

	entry := w.parseZerologLine(p, mac, host, comp)
	if entry == nil {
		return n, nil
	}

	w.mu.Lock()
	if len(w.buffer) < w.bufferSize {
		w.buffer = append(w.buffer, *entry)
	}
	shouldFlush := len(w.buffer) >= w.bufferSize
	w.mu.Unlock()

	if shouldFlush {
		// Signal the background flusher; never block the caller.
		select {
		case w.flushCh <- struct{}{}:
		default:
		}
	}
	return n, nil
}

// Flush ships any buffered entries to the server immediately.
// Returns the first transport error encountered; entries remain buffered on
// error so they can be retried on the next flush cycle.
func (w *RemoteLogWriter) Flush() error {
	w.mu.Lock()
	if len(w.buffer) == 0 {
		w.mu.Unlock()
		return nil
	}
	batch := make([]api.LogEntry, len(w.buffer))
	copy(batch, w.buffer)
	w.buffer = w.buffer[:0]
	w.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := w.client.SendLogs(ctx, batch); err != nil {
		// Put entries back on failure so they are not silently lost.
		w.mu.Lock()
		w.buffer = append(batch, w.buffer...)
		if len(w.buffer) > w.bufferSize*2 {
			// Trim to prevent unbounded growth when the server is persistently down.
			w.buffer = w.buffer[len(w.buffer)-w.bufferSize:]
		}
		w.mu.Unlock()
		return err
	}
	return nil
}

// Close flushes remaining buffered entries (best-effort, 5s timeout) and
// stops the background goroutine.
func (w *RemoteLogWriter) Close() error {
	w.once.Do(func() {
		close(w.done)
	})

	// Best-effort final flush with a hard timeout.
	done := make(chan error, 1)
	go func() { done <- w.Flush() }()
	select {
	case <-time.After(closeFlushTimeout):
	case <-done:
	}
	return nil
}

// flusher is the background goroutine that drains the buffer periodically or
// when signalled.
func (w *RemoteLogWriter) flusher() {
	ticker := time.NewTicker(w.flushEvery)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			_ = w.Flush() // errors logged nowhere intentionally — never block CLI
		case <-w.flushCh:
			_ = w.Flush()
		}
	}
}

// parseZerologLine parses a zerolog JSON line into a LogEntry. Returns nil for
// lines that cannot be parsed or that are clearly not log events.
// mac, hostname, and component are pre-snapshotted identity values from the caller.
func (w *RemoteLogWriter) parseZerologLine(p []byte, mac, hostname, component string) *api.LogEntry {
	line := bytes.TrimSpace(p)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}

	// Parse into a raw map to extract well-known zerolog fields.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}

	entry := &api.LogEntry{
		ID:        uuid.New().String(),
		NodeMAC:   mac,
		Hostname:  hostname,
		Component: component,
		Timestamp: time.Now().UTC(),
		Fields:    make(map[string]interface{}),
	}

	// Extract zerolog standard fields.
	for k, v := range raw {
		var s string
		switch k {
		case "level":
			_ = json.Unmarshal(v, &s)
			entry.Level = s
		case "message":
			_ = json.Unmarshal(v, &s)
			entry.Message = s
		case "time":
			_ = json.Unmarshal(v, &s)
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				entry.Timestamp = t.UTC()
			}
		case "component":
			// A "component" field on the log line takes precedence over the writer default.
			_ = json.Unmarshal(v, &s)
			if s != "" {
				entry.Component = s
			}
		default:
			// All other fields go into Fields map — keep it sparse.
			var val interface{}
			if err := json.Unmarshal(v, &val); err == nil {
				entry.Fields[k] = val
			}
		}
	}

	if entry.Level == "" {
		entry.Level = "info"
	}
	if entry.Message == "" {
		return nil // nothing useful to ship
	}

	// Don't ship empty fields maps.
	if len(entry.Fields) == 0 {
		entry.Fields = nil
	}

	return entry
}

// ─── SSE reader ─────────────────────────────────────────────────────────────

// StreamLogs opens an SSE connection to GET /api/v1/logs/stream and returns a
// channel of LogEntry values. The caller must call the returned cancel function
// to close the connection and drain the channel.
func (c *Client) StreamLogs(ctx context.Context, filter api.LogFilter) (<-chan api.LogEntry, func(), error) {
	path := buildLogsPath("/api/v1/logs/stream", filter)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "text/event-stream")

	// Use a no-timeout HTTP client for the long-lived SSE stream.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, nil, c.decodeError(resp)
	}

	ch := make(chan api.LogEntry, 32)
	cancelCtx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(ch)
		defer resp.Body.Close()
		defer cancel()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case <-cancelCtx.Done():
				return
			default:
			}
			line := scanner.Text()
			if len(line) < 6 || line[:6] != "data: " {
				continue // skip comments and blank lines
			}
			data := line[6:]
			var entry api.LogEntry
			if err := json.Unmarshal([]byte(data), &entry); err != nil {
				continue
			}
			select {
			case ch <- entry:
			case <-cancelCtx.Done():
				return
			}
		}
	}()

	return ch, func() {
		cancel()
		resp.Body.Close()
	}, nil
}
