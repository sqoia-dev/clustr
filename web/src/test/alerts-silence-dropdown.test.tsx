/**
 * alerts-silence-dropdown.test.tsx — Bug #248 regression guard.
 *
 * Verifies that clicking the Silence button renders the duration dropdown in
 * the DOM (i.e. it is not clipped by overflow-hidden on the table wrapper).
 *
 * Root cause: the table container div had `overflow-hidden` which clips
 * absolutely-positioned children.  The fix changed it to `overflow-visible`.
 * This test guards against reintroducing `overflow-hidden` on that element.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest"
import { render, screen, fireEvent } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { _SilenceButton as SilenceButton } from "../routes/alerts"

// ─── Helpers ─────────────────────────────────────────────────────────────────

function makeQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
}

function renderSilenceButton() {
  const qc = makeQueryClient()
  return render(
    <QueryClientProvider client={qc}>
      <SilenceButton
        ruleName="cp.serverd.restart.crit"
        nodeId="node-abc123"
        onDone={vi.fn()}
      />
    </QueryClientProvider>
  )
}

// ─── Tests ───────────────────────────────────────────────────────────────────

describe("SilenceButton dropdown", () => {
  beforeEach(() => {
    vi.resetAllMocks()
    // Stub fetch so mutation calls don't throw unhandled errors.
    vi.stubGlobal("fetch", vi.fn(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        headers: { get: () => "application/json" },
        text: () => Promise.resolve("{}"),
      })
    ))
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it("should render the Silence button", () => {
    renderSilenceButton()
    expect(screen.getByRole("button", { name: /silence/i })).toBeTruthy()
  })

  it("should show duration options in the DOM after clicking Silence", () => {
    renderSilenceButton()

    const btn = screen.getByRole("button", { name: /silence/i })
    fireEvent.click(btn)

    // All four duration options must be present and visible.
    expect(screen.getByText("1 hour")).toBeTruthy()
    expect(screen.getByText("4 hours")).toBeTruthy()
    expect(screen.getByText("24 hours")).toBeTruthy()
    expect(screen.getByText("Forever")).toBeTruthy()
  })

  it("should close the dropdown after clicking outside", () => {
    renderSilenceButton()

    const btn = screen.getByRole("button", { name: /silence/i })
    fireEvent.click(btn)

    // Dropdown open — options visible.
    expect(screen.getByText("1 hour")).toBeTruthy()

    // Click outside the dropdown.
    fireEvent.mouseDown(document.body)

    // Options must be gone.
    expect(screen.queryByText("1 hour")).toBeNull()
  })
})
