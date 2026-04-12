package image

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/chroot"
	"github.com/sqoia-dev/clonr/pkg/db"
)

const (
	// maxSessions is the maximum number of concurrently open chroot sessions.
	maxSessions = 4
	// sessionTimeout is the idle duration after which a session is auto-closed.
	sessionTimeout = 30 * time.Minute
)

// ShellSession represents an active chroot environment for an image.
type ShellSession struct {
	ID       string
	ImageID  string
	RootDir  string
	Chroot   *chroot.Session
	Created  time.Time
	LastUsed time.Time
}

// ShellManager tracks all open ShellSessions and enforces concurrency limits.
// It is safe for concurrent use.
type ShellManager struct {
	Store    *db.DB
	ImageDir string
	Logger   zerolog.Logger

	mu       sync.Mutex
	sessions map[string]*ShellSession
}

// NewShellManager creates a ShellManager and starts the background reaper.
func NewShellManager(store *db.DB, imageDir string, logger zerolog.Logger) *ShellManager {
	m := &ShellManager{
		Store:    store,
		ImageDir: imageDir,
		Logger:   logger,
		sessions: make(map[string]*ShellSession),
	}
	go m.reapLoop()
	return m
}

// OpenSession creates and enters a chroot session for imageID.
// The image must have status "ready" or "building".
// Returns an error if the session limit is already reached.
func (m *ShellManager) OpenSession(ctx context.Context, imageID string) (*ShellSession, error) {
	img, err := m.Store.GetBaseImage(ctx, imageID)
	if err != nil {
		return nil, fmt.Errorf("shell: get image: %w", err)
	}
	if img.Status != api.ImageStatusReady {
		return nil, fmt.Errorf("shell: image %s has status %q — must be ready to open a shell session", imageID, img.Status)
	}

	rootDir := filepath.Join(m.ImageDir, imageID, "rootfs")

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.sessions) >= maxSessions {
		return nil, fmt.Errorf("shell: maximum concurrent sessions (%d) reached", maxSessions)
	}

	sess, err := chroot.NewSession(rootDir)
	if err != nil {
		return nil, fmt.Errorf("shell: create chroot session: %w", err)
	}
	if err := sess.Enter(); err != nil {
		return nil, fmt.Errorf("shell: enter chroot: %w", err)
	}

	now := time.Now()
	s := &ShellSession{
		ID:       uuid.New().String(),
		ImageID:  imageID,
		RootDir:  rootDir,
		Chroot:   sess,
		Created:  now,
		LastUsed: now,
	}
	m.sessions[s.ID] = s

	m.Logger.Info().
		Str("session_id", s.ID).
		Str("image_id", imageID).
		Str("rootdir", rootDir).
		Msg("shell: session opened")

	return s, nil
}

// CloseSession unmounts and removes the session identified by sessionID.
// It is safe to call on an already-closed session.
func (m *ShellManager) CloseSession(sessionID string) error {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if !ok {
		return nil
	}
	return m.closeSession(s)
}

func (m *ShellManager) closeSession(s *ShellSession) error {
	m.Logger.Info().Str("session_id", s.ID).Str("image_id", s.ImageID).Msg("shell: closing session")
	if err := s.Chroot.Close(); err != nil {
		return fmt.Errorf("shell: unmount chroot: %w", err)
	}
	return nil
}

// ExecInSession runs a command inside the named session's chroot and returns
// the combined stdout+stderr output.
func (m *ShellManager) ExecInSession(ctx context.Context, sessionID string, cmd string, args ...string) ([]byte, error) {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if ok {
		s.LastUsed = time.Now()
	}
	m.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("shell: session %s not found", sessionID)
	}

	out, err := s.Chroot.Exec(cmd, args...)
	if err != nil {
		return out, fmt.Errorf("shell: exec in session %s: %w", sessionID, err)
	}
	return out, nil
}

// InteractiveShell drops into an interactive bash shell inside sessionID,
// attaching stdin/stdout/stderr to the calling process's terminal.
func (m *ShellManager) InteractiveShell(sessionID string) error {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if ok {
		s.LastUsed = time.Now()
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("shell: session %s not found", sessionID)
	}
	return s.Chroot.Shell()
}

// CloseAll closes every open session. Called on server shutdown.
func (m *ShellManager) CloseAll() error {
	m.mu.Lock()
	sessions := make([]*ShellSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = make(map[string]*ShellSession)
	m.mu.Unlock()

	var firstErr error
	for _, s := range sessions {
		if err := m.closeSession(s); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ListSessions returns a snapshot of all open session IDs and their image IDs.
func (m *ShellManager) ListSessions() []ShellSessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ShellSessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, ShellSessionInfo{
			ID:       s.ID,
			ImageID:  s.ImageID,
			RootDir:  s.RootDir,
			Created:  s.Created,
			LastUsed: s.LastUsed,
		})
	}
	return out
}

// ShellSessionInfo is a read-only view of a session for API responses.
type ShellSessionInfo struct {
	ID       string    `json:"id"`
	ImageID  string    `json:"image_id"`
	RootDir  string    `json:"root_dir"`
	Created  time.Time `json:"created_at"`
	LastUsed time.Time `json:"last_used_at"`
}

// reapLoop periodically closes sessions that have been idle for sessionTimeout.
func (m *ShellManager) reapLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.reapIdle()
	}
}

func (m *ShellManager) reapIdle() {
	cutoff := time.Now().Add(-sessionTimeout)

	m.mu.Lock()
	var stale []*ShellSession
	for id, s := range m.sessions {
		if s.LastUsed.Before(cutoff) {
			stale = append(stale, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()

	for _, s := range stale {
		m.Logger.Info().
			Str("session_id", s.ID).
			Str("image_id", s.ImageID).
			Dur("idle", time.Since(s.LastUsed)).
			Msg("shell: reaping idle session")
		if err := m.closeSession(s); err != nil {
			m.Logger.Error().Err(err).Str("session_id", s.ID).Msg("shell: reap close error")
		}
	}
}
