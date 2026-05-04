/**
 * build-progress-panel.test.ts — UX: ISO build progress panel
 *
 * Covers the pure state-reduction logic extracted from BuildProgressPanel:
 *   - applySnapshot: REPLACES state from a BuildState snapshot (reconnect path)
 *   - applyBuildEvent: updates state correctly for each BuildEvent field
 *   - phaseLabel: human-readable labels for each build phase string
 *   - fmtBytes: byte formatter edge cases
 *   - pct computation: correct percentage from (bytesDown, bytesTotal)
 *   - ETA estimation from rate samples
 */

import { describe, it, expect } from "vitest"

// ─── Types mirrored from BuildProgressPanel ───────────────────────────────────

interface BuildProgressState {
  phase: string
  bytesDown: number
  bytesTotal: number
  serialLines: string[]
  errorMsg: string
}

interface BuildEvent {
  phase?: string
  bytes_done?: number
  bytes_total?: number
  serial_line?: string
  error?: string
}

// BuildSnapshot mirrors the api.BuildState JSON shape sent by the server's
// "snapshot" SSE event (all fields present, zero-valued when not set).
interface BuildSnapshot {
  phase?: string
  bytes_done?: number
  bytes_total?: number     // 0 when Content-Length was absent (server zero value)
  serial_tail?: string[]
  error_message?: string
}

// ─── Logic under test (extracted from BuildProgressPanel) ────────────────────

const INITIAL_STATE: BuildProgressState = {
  phase: "downloading_iso",
  bytesDown: 0,
  bytesTotal: -1,
  serialLines: [],
  errorMsg: "",
}

/**
 * applySnapshot REPLACES the panel state with the server's BuildState snapshot.
 * Called when the SSE "snapshot" event arrives (initial connect or reconnect).
 *
 * Mirrors the snapshot event handler in BuildProgressPanel:
 *   - bytes_total=0 from Go means "unknown Content-Length"; map to -1 (web sentinel).
 *   - Full replacement semantics: no stale incremental state leaks in.
 */
function applySnapshot(snap: BuildSnapshot): BuildProgressState {
  return {
    phase:       snap.phase       || "downloading_iso",
    bytesDown:   snap.bytes_done  || 0,
    bytesTotal:  (snap.bytes_total ?? 0) > 0 ? (snap.bytes_total as number) : -1,
    serialLines: Array.isArray(snap.serial_tail) ? snap.serial_tail : [],
    errorMsg:    snap.error_message || "",
  }
}

/**
 * applyBuildEvent applies a single BuildEvent to produce the next state.
 * Mirrors the setPs updater in BuildProgressPanel.
 */
function applyBuildEvent(
  prev: BuildProgressState,
  ev: BuildEvent,
  maxSerialLines = 500,
): BuildProgressState {
  const next = { ...prev }

  if (ev.phase) {
    next.phase = ev.phase
    if (ev.phase === "failed") {
      next.errorMsg = ev.error ?? "Build failed"
    }
  }
  if (ev.bytes_done !== undefined && ev.bytes_done > 0) {
    next.bytesDown = ev.bytes_done
  }
  if (ev.bytes_total !== undefined) {
    next.bytesTotal = ev.bytes_total
  }
  if (ev.serial_line) {
    const lines = [...prev.serialLines, ev.serial_line]
    next.serialLines = lines.length > maxSerialLines ? lines.slice(lines.length - maxSerialLines) : lines
  }
  return next
}

function phaseLabel(phase: string): string {
  switch (phase) {
    case "downloading_iso": case "downloading": return "Downloading ISO…"
    case "generating_config": return "Generating installer config…"
    case "creating_disk":     return "Creating virtual disk…"
    case "launching_vm":      return "Launching installer VM…"
    case "installing":        return "Installing OS (Anaconda)…"
    case "extracting":        return "Extracting root filesystem…"
    case "scrubbing":         return "Scrubbing node identity…"
    case "finalizing":        return "Finalizing image…"
    case "complete":          return "Complete"
    case "failed":            return "Failed"
    default:                  return phase ? phase.replace(/_/g, " ") : "Working…"
  }
}

function fmtBytes(n: number): string {
  if (n < 0) return "?"
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`
}

function computePct(bytesDown: number, bytesTotal: number): number | null {
  if (bytesTotal <= 0) return null
  return Math.min(100, Math.round((bytesDown / bytesTotal) * 100))
}

/**
 * estimateETA computes remaining time in seconds given a sample ring.
 * Returns null when insufficient data or rate is zero/negative.
 */
function estimateETA(
  samples: Array<{ ts: number; bytes: number }>,
  bytesTotal: number,
): number | null {
  if (samples.length < 2 || bytesTotal <= 0) return null
  const oldest = samples[0]
  const newest = samples[samples.length - 1]
  const elapsed = (newest.ts - oldest.ts) / 1000
  if (elapsed <= 0) return null
  const rate = (newest.bytes - oldest.bytes) / elapsed
  if (rate <= 0) return null
  const remaining = bytesTotal - newest.bytes
  if (remaining <= 0) return null
  return Math.round(remaining / rate)
}

// ─── Tests ───────────────────────────────────────────────────────────────────

// ─── applySnapshot — reconnect-mid-build coverage (#248) ─────────────────────

describe("applySnapshot — full state replacement on reconnect", () => {
  it("should replace state with mid-build snapshot (bytes_done + bytes_total populated)", () => {
    // Simulate: component mounts mid-download; server sends snapshot with current progress.
    const snap: BuildSnapshot = {
      phase:      "downloading_iso",
      bytes_done:  500 * 1024 * 1024,
      bytes_total: 2 * 1024 * 1024 * 1024,
      serial_tail: [],
    }
    const state = applySnapshot(snap)
    expect(state.phase).toBe("downloading_iso")
    expect(state.bytesDown).toBe(500 * 1024 * 1024)
    expect(state.bytesTotal).toBe(2 * 1024 * 1024 * 1024)
    expect(state.serialLines).toHaveLength(0)
    expect(state.errorMsg).toBe("")
  })

  it("should map bytes_total=0 to -1 (Go zero value means unknown Content-Length)", () => {
    // This is the root cause of the '0 B' bug: Go serializes int64=0 as 0,
    // but the web uses -1 as the sentinel for 'no content-length'.
    const snap: BuildSnapshot = {
      phase:      "downloading_iso",
      bytes_done:  100 * 1024 * 1024,
      bytes_total: 0,   // server sent zero — no Content-Length header
    }
    const state = applySnapshot(snap)
    expect(state.bytesTotal).toBe(-1)
    // bytesDown should still reflect the actual progress
    expect(state.bytesDown).toBe(100 * 1024 * 1024)
  })

  it("should replace state when reconnecting during install phase with serial lines", () => {
    // Simulate reconnect during Anaconda install: snapshot has serial lines.
    const serialLines = ["anaconda started", "fetching packages", "installing kernel"]
    const snap: BuildSnapshot = {
      phase:      "installing",
      bytes_done:  0,
      bytes_total: 0,
      serial_tail: serialLines,
    }
    const state = applySnapshot(snap)
    expect(state.phase).toBe("installing")
    expect(state.serialLines).toEqual(serialLines)
    expect(state.serialLines).toHaveLength(3)
  })

  it("should reflect terminal failed state on reconnect", () => {
    // Reconnecting to a build that already failed while the panel was closed.
    const snap: BuildSnapshot = {
      phase:         "failed",
      bytes_done:    200 * 1024 * 1024,
      bytes_total:   2 * 1024 * 1024 * 1024,
      error_message: "QEMU exited with code 1",
    }
    const state = applySnapshot(snap)
    expect(state.phase).toBe("failed")
    expect(state.errorMsg).toBe("QEMU exited with code 1")
  })

  it("should reflect terminal complete state on reconnect", () => {
    const snap: BuildSnapshot = {
      phase:      "complete",
      bytes_done:  2 * 1024 * 1024 * 1024,
      bytes_total: 2 * 1024 * 1024 * 1024,
    }
    const state = applySnapshot(snap)
    expect(state.phase).toBe("complete")
    expect(state.bytesDown).toBe(2 * 1024 * 1024 * 1024)
    expect(state.bytesTotal).toBe(2 * 1024 * 1024 * 1024)
  })

  it("should not carry over stale incremental state after reconnect", () => {
    // Simulate: user had panel open during download (500MB), closed it,
    // then re-opened mid-install. Stale bytesDown must not persist.
    // The snapshot is the fresh source of truth — full replacement.
    const stalePrev: BuildProgressState = {
      phase:       "downloading_iso",
      bytesDown:   500 * 1024 * 1024,
      bytesTotal:  2 * 1024 * 1024 * 1024,
      serialLines: ["stale line"],
      errorMsg:    "",
    }
    const snap: BuildSnapshot = {
      phase:      "installing",
      bytes_done:  0,
      bytes_total: 0,
      serial_tail: ["anaconda started"],
    }
    // applySnapshot is a pure replacement — stalePrev is not an input.
    const state = applySnapshot(snap)
    // The stale bytesDown from the closed panel does NOT carry over.
    expect(state.bytesDown).toBe(0)
    expect(state.bytesTotal).toBe(-1)
    expect(state.phase).toBe("installing")
    expect(state.serialLines).toEqual(["anaconda started"])
    // Verify stalePrev is unchanged (applySnapshot is pure).
    expect(stalePrev.bytesDown).toBe(500 * 1024 * 1024)
  })

  it("should handle missing serial_tail gracefully (null or absent)", () => {
    const snap: BuildSnapshot = { phase: "creating_disk" }
    const state = applySnapshot(snap)
    expect(state.serialLines).toEqual([])
  })
})

describe("applyBuildEvent — phase transitions", () => {
  it("should update phase on phase event", () => {
    const next = applyBuildEvent(INITIAL_STATE, { phase: "installing" })
    expect(next.phase).toBe("installing")
  })

  it("should set errorMsg when phase is failed", () => {
    const next = applyBuildEvent(INITIAL_STATE, { phase: "failed", error: "OOM" })
    expect(next.phase).toBe("failed")
    expect(next.errorMsg).toBe("OOM")
  })

  it("should use default error message when error field is absent on failure", () => {
    const next = applyBuildEvent(INITIAL_STATE, { phase: "failed" })
    expect(next.errorMsg).toBe("Build failed")
  })

  it("should not change phase when phase field is absent", () => {
    const next = applyBuildEvent(INITIAL_STATE, { bytes_done: 1024 })
    expect(next.phase).toBe("downloading_iso")
  })
})

describe("applyBuildEvent — progress updates", () => {
  it("should update bytesDown from bytes_done", () => {
    const next = applyBuildEvent(INITIAL_STATE, { bytes_done: 5 * 1024 * 1024 })
    expect(next.bytesDown).toBe(5 * 1024 * 1024)
  })

  it("should update bytesTotal from bytes_total", () => {
    const next = applyBuildEvent(INITIAL_STATE, { bytes_total: 2 * 1024 * 1024 * 1024 })
    expect(next.bytesTotal).toBe(2 * 1024 * 1024 * 1024)
  })

  it("should not reset bytesDown to 0 (bytes_done=0 is ignored)", () => {
    const withBytes = applyBuildEvent(INITIAL_STATE, { bytes_done: 100 * 1024 * 1024 })
    const next = applyBuildEvent(withBytes, { bytes_done: 0 })
    // bytes_done=0 is ignored per the guard (ev.bytes_done > 0)
    expect(next.bytesDown).toBe(100 * 1024 * 1024)
  })

  it("should accumulate progress monotonically", () => {
    let s = INITIAL_STATE
    s = applyBuildEvent(s, { bytes_done: 100 * 1024 * 1024, bytes_total: 1024 * 1024 * 1024 })
    s = applyBuildEvent(s, { bytes_done: 200 * 1024 * 1024 })
    s = applyBuildEvent(s, { bytes_done: 512 * 1024 * 1024 })
    expect(s.bytesDown).toBe(512 * 1024 * 1024)
  })
})

describe("applyBuildEvent — serial lines", () => {
  it("should append a serial line", () => {
    const next = applyBuildEvent(INITIAL_STATE, { serial_line: "anaconda started" })
    expect(next.serialLines).toHaveLength(1)
    expect(next.serialLines[0]).toBe("anaconda started")
  })

  it("should append multiple serial lines in order", () => {
    let s = INITIAL_STATE
    s = applyBuildEvent(s, { serial_line: "line 1" })
    s = applyBuildEvent(s, { serial_line: "line 2" })
    s = applyBuildEvent(s, { serial_line: "line 3" })
    expect(s.serialLines).toHaveLength(3)
    expect(s.serialLines[2]).toBe("line 3")
  })

  it("should cap serial lines at maxSerialLines", () => {
    let s = INITIAL_STATE
    for (let i = 0; i < 510; i++) {
      s = applyBuildEvent(s, { serial_line: `line ${i}` }, 500)
    }
    expect(s.serialLines).toHaveLength(500)
    // Should be the last 500 lines.
    expect(s.serialLines[0]).toBe("line 10")
    expect(s.serialLines[499]).toBe("line 509")
  })

  it("should not add empty serial_line", () => {
    // An event without serial_line is a no-op for the lines array.
    const next = applyBuildEvent(INITIAL_STATE, { phase: "installing" })
    expect(next.serialLines).toHaveLength(0)
  })
})

describe("phaseLabel", () => {
  it("should return human-readable label for all defined phases", () => {
    const cases: [string, string][] = [
      ["downloading_iso",   "Downloading ISO…"],
      ["downloading",       "Downloading ISO…"],
      ["generating_config", "Generating installer config…"],
      ["creating_disk",     "Creating virtual disk…"],
      ["launching_vm",      "Launching installer VM…"],
      ["installing",        "Installing OS (Anaconda)…"],
      ["extracting",        "Extracting root filesystem…"],
      ["scrubbing",         "Scrubbing node identity…"],
      ["finalizing",        "Finalizing image…"],
      ["complete",          "Complete"],
      ["failed",            "Failed"],
    ]
    for (const [phase, want] of cases) {
      expect(phaseLabel(phase)).toBe(want)
    }
  })

  it("should replace underscores with spaces for unknown phases", () => {
    expect(phaseLabel("some_unknown_phase")).toBe("some unknown phase")
  })

  it("should return 'Working…' for empty phase string", () => {
    expect(phaseLabel("")).toBe("Working…")
  })
})

describe("fmtBytes", () => {
  it("should return '?' for negative values", () => {
    expect(fmtBytes(-1)).toBe("?")
  })

  it("should format bytes below 1 KB as B", () => {
    expect(fmtBytes(512)).toBe("512 B")
  })

  it("should format KB range", () => {
    expect(fmtBytes(1024)).toBe("1.0 KB")
    expect(fmtBytes(1536)).toBe("1.5 KB")
  })

  it("should format MB range", () => {
    expect(fmtBytes(1024 * 1024)).toBe("1.0 MB")
    expect(fmtBytes(500 * 1024 * 1024)).toBe("500.0 MB")
  })

  it("should format GB range", () => {
    expect(fmtBytes(2 * 1024 * 1024 * 1024)).toBe("2.00 GB")
    expect(fmtBytes(1.5 * 1024 * 1024 * 1024)).toBe("1.50 GB")
  })
})

describe("computePct", () => {
  it("should return null when bytesTotal is 0", () => {
    expect(computePct(0, 0)).toBeNull()
  })

  it("should return null when bytesTotal is negative (no Content-Length)", () => {
    expect(computePct(500 * 1024 * 1024, -1)).toBeNull()
  })

  it("should compute 0% at start", () => {
    expect(computePct(0, 1 * 1024 * 1024 * 1024)).toBe(0)
  })

  it("should compute 50% at half-way", () => {
    expect(computePct(512 * 1024 * 1024, 1024 * 1024 * 1024)).toBe(50)
  })

  it("should compute 100% at completion", () => {
    expect(computePct(1024 * 1024 * 1024, 1024 * 1024 * 1024)).toBe(100)
  })

  it("should cap at 100% when done exceeds total (can happen with race)", () => {
    expect(computePct(1025 * 1024 * 1024, 1024 * 1024 * 1024)).toBe(100)
  })

  it("should round to nearest integer", () => {
    // 1/3 of 3 GB = 33%
    expect(computePct(1 * 1024 * 1024 * 1024, 3 * 1024 * 1024 * 1024)).toBe(33)
  })
})

describe("estimateETA", () => {
  it("should return null with fewer than 2 samples", () => {
    expect(estimateETA([], 1024)).toBeNull()
    expect(estimateETA([{ ts: 0, bytes: 0 }], 1024)).toBeNull()
  })

  it("should return null when bytesTotal is 0 or negative", () => {
    const samples = [{ ts: 0, bytes: 0 }, { ts: 1000, bytes: 100 }]
    expect(estimateETA(samples, 0)).toBeNull()
    expect(estimateETA(samples, -1)).toBeNull()
  })

  it("should estimate ETA correctly given constant rate", () => {
    // Rate: 100 bytes/sec. Downloaded 100 bytes in 1 sec. Total: 1000 bytes.
    // Remaining: 900 bytes at 100 B/s = 9 seconds.
    const samples = [
      { ts: 0, bytes: 0 },
      { ts: 1000, bytes: 100 },
    ]
    const eta = estimateETA(samples, 1000)
    expect(eta).toBe(9)
  })

  it("should use the oldest and newest samples for rate estimation", () => {
    // 500 bytes/sec averaged over 2 seconds: 0→100 (1s), 100→1100 (2s).
    // Rate from oldest (0,0) to newest (2000,1100) = 1100/2 = 550 B/s.
    // Remaining: 1000000 - 1100 = 998900 at 550 B/s ≈ 1816 secs.
    const samples = [
      { ts: 0,    bytes: 0 },
      { ts: 1000, bytes: 100 },
      { ts: 2000, bytes: 1100 },
    ]
    const eta = estimateETA(samples, 1000000)
    expect(eta).toBe(1816)
  })

  it("should return null when nothing remaining (already done)", () => {
    const samples = [
      { ts: 0,    bytes: 0 },
      { ts: 1000, bytes: 1000 },
    ]
    // bytes_total equals newest.bytes — no remaining.
    expect(estimateETA(samples, 1000)).toBeNull()
  })
})
