/**
 * ux10b-cross-rack-drag.test.tsx — UX-10b: cross-rack drag, single-rack layout, drop validation.
 *
 * Tests:
 *   1. Multi-rack render — two racks shown side-by-side; drag from rack-A fires PUT to rack-B.
 *   2. Single-rack render — one rack still works, no layout breakage.
 *   3. Validation — dropping a 4U node on a slot with only 2U remaining fires a toast, no PUT.
 */

import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, act, waitFor } from "@testing-library/react"
import * as React from "react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"

// ── Mocks ──────────────────────────────────────────────────────────────────────

// apiFetch — track calls; default to resolving undefined (204)
const mockApiFetch = vi.fn()
vi.mock("@/lib/api", () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}))

// toast — capture toasts
const mockToast = vi.fn()
vi.mock("@/hooks/use-toast", () => ({ toast: (...args: unknown[]) => mockToast(...args) }))

// @dnd-kit/core — minimal mock that captures the onDragEnd / onDragStart callbacks
// so tests can trigger drag events programmatically without pointer events in jsdom.
let capturedOnDragEnd: ((e: unknown) => void) | null = null
let capturedOnDragStart: ((e: unknown) => void) | null = null

vi.mock("@dnd-kit/core", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@dnd-kit/core")>()
  return {
    ...actual,
    DndContext: ({
      children,
      onDragEnd,
      onDragStart,
    }: {
      children: React.ReactNode
      onDragEnd: (e: unknown) => void
      onDragStart: (e: unknown) => void
    }) => {
      // Capture via ref-like assignment so the test can call them after render
      capturedOnDragEnd = onDragEnd
      capturedOnDragStart = onDragStart
      return <div data-testid="dnd-context">{children}</div>
    },
    DragOverlay: ({ children }: { children: React.ReactNode }) => (
      <div data-testid="drag-overlay">{children}</div>
    ),
    useDraggable: () => ({
      attributes: {},
      listeners: {},
      setNodeRef: () => {},
      transform: null,
      isDragging: false,
    }),
    useDroppable: ({ data }: { id: unknown; data: unknown }) => ({
      setNodeRef: () => {},
      isOver: false,
      data: { current: data },
    }),
    useSensor: () => ({}),
    useSensors: (...args: unknown[]) => args,
    PointerSensor: class {},
    closestCenter: () => null,
  }
})

// SectionErrorBoundary — passthrough
vi.mock("@/components/ErrorBoundary", () => ({
  SectionErrorBoundary: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}))

// ── Helpers ────────────────────────────────────────────────────────────────────

function makeQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: Infinity },
      mutations: { retry: false },
    },
  })
}

function makeRack(id: string, name: string, heightU: number, positions: {
  node_id: string; rack_id: string; slot_u: number; height_u: number
}[] = []) {
  return { id, name, height_u: heightU, positions }
}

function makeNode(id: string, hostname: string) {
  return { id, hostname, status: "active" }
}

// Pre-seed the query cache so the component renders immediately without fetching.
// apiFetch is mocked globally — the seedX functions bypass it by writing directly.
function seedRacks(qc: QueryClient, racks: ReturnType<typeof makeRack>[]) {
  qc.setQueryData(["racks"], { racks, total: racks.length })
}

function seedNodes(qc: QueryClient, nodes: ReturnType<typeof makeNode>[]) {
  qc.setQueryData(["nodes-health-dc"], { nodes })
  qc.setQueryData(["nodes-unassigned"], { nodes: [], total: 0 })
}

async function renderDatacenter(qc: QueryClient) {
  // Dynamic import ensures mocks are applied before the module is evaluated
  const { DatacenterPage } = await import("@/routes/datacenter")
  let result!: ReturnType<typeof render>
  await act(async () => {
    result = render(
      <QueryClientProvider client={qc}>
        <DatacenterPage />
      </QueryClientProvider>
    )
  })
  return result
}

// ── Tests ──────────────────────────────────────────────────────────────────────

describe("UX-10b: cross-rack drag + tile layout", () => {
  beforeEach(() => {
    vi.resetAllMocks()
    capturedOnDragEnd = null
    capturedOnDragStart = null
    // Default: mutations succeed silently
    mockApiFetch.mockResolvedValue(undefined)
  })

  it("renders two racks side-by-side in tile layout", async () => {
    const qc = makeQueryClient()
    seedRacks(qc, [makeRack("rack-a", "rack-a", 8), makeRack("rack-b", "rack-b", 8)])
    seedNodes(qc, [makeNode("n1", "node-1")])

    await renderDatacenter(qc)

    // Wait for racks to appear (after React Query moves out of pending state)
    await waitFor(() => {
      expect(screen.getAllByText(/rack-a/i).length).toBeGreaterThanOrEqual(1)
    })
    expect(screen.getAllByText(/rack-b/i).length).toBeGreaterThanOrEqual(1)
    // Single shared DndContext wrapping everything
    expect(screen.getByTestId("dnd-context")).toBeTruthy()
  })

  it("fires PUT to destination rack_id on cross-rack drag-drop", async () => {
    const qc = makeQueryClient()
    const nodeId = "node-1"
    seedRacks(qc, [
      makeRack("rack-a", "rack-a", 8, [
        { node_id: nodeId, rack_id: "rack-a", slot_u: 1, height_u: 1 },
      ]),
      makeRack("rack-b", "rack-b", 8),
    ])
    seedNodes(qc, [makeNode(nodeId, "compute-1")])

    await renderDatacenter(qc)

    // Verify DndContext callback was captured at render time
    expect(capturedOnDragEnd).not.toBeNull()

    // Fire drag start (sets activeDrag state)
    await act(async () => {
      capturedOnDragStart!({
        active: {
          id: `node-${nodeId}`,
          data: {
            current: {
              nodeId,
              rackId: "rack-a",
              slotU: 1,
              heightU: 1,
              fromUnassigned: false,
            },
          },
        },
      })
    })

    // Fire drag end — drop onto rack-b slot 3
    await act(async () => {
      capturedOnDragEnd!({
        active: {
          id: `node-${nodeId}`,
          data: {
            current: {
              nodeId,
              rackId: "rack-a",
              slotU: 1,
              heightU: 1,
              fromUnassigned: false,
            },
          },
        },
        over: {
          id: "slot-rack-b-3",
          data: { current: { rackId: "rack-b", slotU: 3 } },
        },
      })
    })

    // Wait for the async mutation to resolve and fire apiFetch
    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        `/api/v1/racks/rack-b/positions/${nodeId}`,
        expect.objectContaining({ method: "PUT" })
      )
    })

    // Verify slot_u in request body
    const callBody = JSON.parse(
      (mockApiFetch.mock.calls[0][1] as { body: string }).body
    )
    expect(callBody.slot_u).toBe(3)
  })

  it("works with a single rack — no cross-rack logic needed", async () => {
    const qc = makeQueryClient()
    const nodeId = "node-solo"
    seedRacks(qc, [
      makeRack("rack-only", "rack-only", 4, [
        { node_id: nodeId, rack_id: "rack-only", slot_u: 1, height_u: 1 },
      ]),
    ])
    seedNodes(qc, [makeNode(nodeId, "solo-node")])

    await renderDatacenter(qc)

    expect(screen.getAllByText(/rack-only/i).length).toBeGreaterThanOrEqual(1)
    expect(capturedOnDragEnd).not.toBeNull()

    // Within-rack drag: move from slot 1 to slot 2
    await act(async () => {
      capturedOnDragStart!({
        active: {
          id: `node-${nodeId}`,
          data: {
            current: {
              nodeId,
              rackId: "rack-only",
              slotU: 1,
              heightU: 1,
              fromUnassigned: false,
            },
          },
        },
      })
    })

    await act(async () => {
      capturedOnDragEnd!({
        active: {
          id: `node-${nodeId}`,
          data: {
            current: {
              nodeId,
              rackId: "rack-only",
              slotU: 1,
              heightU: 1,
              fromUnassigned: false,
            },
          },
        },
        over: {
          id: "slot-rack-only-2",
          data: { current: { rackId: "rack-only", slotU: 2 } },
        },
      })
    })

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        `/api/v1/racks/rack-only/positions/${nodeId}`,
        expect.objectContaining({ method: "PUT" })
      )
    })
  })

  it("shows toast and fires no PUT when 4U node drops on slot with 2U remaining", async () => {
    const qc = makeQueryClient()
    const blocker = "node-blocker"
    const mover = "node-mover"

    // rack-a: 4U total; slots 3-4 occupied by blocker (2U).
    // Dropping mover (4U) at slot 1 → needs slots 1-4 → conflicts at 3 and 4.
    seedRacks(qc, [
      makeRack("rack-a", "rack-a", 4, [
        { node_id: blocker, rack_id: "rack-a", slot_u: 3, height_u: 2 },
      ]),
      makeRack("rack-b", "rack-b", 8, [
        { node_id: mover, rack_id: "rack-b", slot_u: 1, height_u: 4 },
      ]),
    ])
    seedNodes(qc, [makeNode(blocker, "blocker"), makeNode(mover, "mover")])

    await renderDatacenter(qc)

    await act(async () => {
      capturedOnDragStart!({
        active: {
          id: `node-${mover}`,
          data: {
            current: {
              nodeId: mover,
              rackId: "rack-b",
              slotU: 1,
              heightU: 4,
              fromUnassigned: false,
            },
          },
        },
      })
    })

    // Drop mover (4U) onto rack-a slot 1 — slots 1-4, but slot 3+4 occupied by blocker
    await act(async () => {
      capturedOnDragEnd!({
        active: {
          id: `node-${mover}`,
          data: {
            current: {
              nodeId: mover,
              rackId: "rack-b",
              slotU: 1,
              heightU: 4,
              fromUnassigned: false,
            },
          },
        },
        over: {
          id: "slot-rack-a-1",
          data: { current: { rackId: "rack-a", slotU: 1 } },
        },
      })
    })

    // Toast fired with destructive variant
    expect(mockToast).toHaveBeenCalledWith(
      expect.objectContaining({ variant: "destructive" })
    )
    // No PUT should have fired
    expect(mockApiFetch).not.toHaveBeenCalled()
  })

  it("drag overlay shows hostname not ID slice (#247)", async () => {
    const qc = makeQueryClient()
    const nodeId = "cbf2c958-4172-47c3-9b0d-29caa4e21df4"
    seedRacks(qc, [
      makeRack("rack-a", "rack-a", 4, [
        { node_id: nodeId, rack_id: "rack-a", slot_u: 1, height_u: 1 },
      ]),
    ])
    seedNodes(qc, [makeNode(nodeId, "slurm-controller")])

    await renderDatacenter(qc)

    // Fire drag start — the overlay should now be visible
    await act(async () => {
      capturedOnDragStart!({
        active: {
          id: `node-${nodeId}`,
          data: {
            current: {
              nodeId,
              rackId: "rack-a",
              slotU: 1,
              heightU: 1,
              fromUnassigned: false,
            },
          },
        },
      })
    })

    // The drag overlay must contain the hostname
    const overlay = screen.getByTestId("drag-overlay")
    expect(overlay.textContent).toContain("slurm-controller")
    // Must NOT contain the bare ID prefix
    expect(overlay.textContent).not.toContain("cbf2c958")
  })
})
