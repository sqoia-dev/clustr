/**
 * datacenter.tsx — UX-10b + Sprint 31 (#231): multi-rack tile layout with chassis enclosure support.
 *
 * Layout:
 *   - Header: [+ Add rack] button
 *   - Left sidebar: unassigned nodes panel (nodes with no rack assignment)
 *   - Tile row: all racks rendered side-by-side in a horizontally scrollable flex container
 *   - Single DndContext wraps sidebar + all rack tiles so cross-rack drag works
 *
 * Sprint 31 additions:
 *   - Rack tiles now render EnclosureBlock components alongside rack-direct NodeBlocks.
 *   - EnclosureBlock shows internal slot grid; each slot is a drop target.
 *   - "Add chassis" button in each rack header opens the AddEnclosureModal.
 *   - Enclosure drag targets dispatch to PUT /api/v1/nodes/{id}/placement (unified endpoint).
 *   - Rack-direct drag targets continue to use PUT /api/v1/racks/{id}/positions/{node_id} (deprecated v0.12.0).
 */
import * as React from "react"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  useSensor,
  useSensors,
  closestCenter,
  useDraggable,
  useDroppable,
} from "@dnd-kit/core"
import type { DragEndEvent, DragStartEvent } from "@dnd-kit/core"
import { CSS } from "@dnd-kit/utilities"
import {
  Building2, Plus, Power, PowerOff, RefreshCw,
  Loader2, XCircle, AlertTriangle, Server, Pencil, Check, X, Scissors, LogOut,
  Layers, Cpu,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { SectionErrorBoundary } from "@/components/ErrorBoundary"
import { toast } from "@/hooks/use-toast"
import { cn } from "@/lib/utils"

// ─── Types ────────────────────────────────────────────────────────────────────

interface NodeRackPosition {
  node_id: string
  rack_id?: string
  slot_u?: number
  height_u?: number
  enclosure_id?: string
  slot_index?: number
}

/** A position that is rack-direct (not enclosure-resident). All rack fields are required. */
interface RackDirectPosition extends NodeRackPosition {
  rack_id: string
  slot_u: number
  height_u: number
}

interface EnclosureSlot {
  slot_index: number
  node_id?: string
}

interface Enclosure {
  id: string
  rack_id: string
  rack_slot_u: number
  height_u: number
  type_id: string
  label?: string
  slots?: EnclosureSlot[]
}

interface EnclosureType {
  id: string
  display_name: string
  height_u: number
  slot_count: number
  orientation: string
  description: string
}

interface Rack {
  id: string
  name: string
  height_u: number
  positions?: NodeRackPosition[]
  enclosures?: Enclosure[]
}

interface ListRacksResponse {
  racks: Rack[]
  total: number
}

interface NodeHealth {
  id: string
  hostname: string
  status: string
}

interface ListNodesResponse {
  nodes: NodeHealth[]
}

// Lightweight unassigned node stub from GET /api/v1/nodes/unassigned
interface UnassignedNodeStub {
  id: string
  hostname: string
  status: string
}

interface ListUnassignedNodesResponse {
  nodes: UnassignedNodeStub[]
  total: number
}

// Drag data shapes
interface InRackDragData {
  nodeId: string
  rackId: string
  slotU: number
  heightU: number
  fromUnassigned: false
  fromEnclosure?: false
}

interface InEnclosureDragData {
  nodeId: string
  enclosureId: string
  slotIndex: number
  heightU: number
  fromUnassigned: false
  fromEnclosure: true
}

interface UnassignedDragData {
  nodeId: string
  heightU: number
  fromUnassigned: true
  rackId?: never
  slotU?: never
}

type DragData = InRackDragData | InEnclosureDragData | UnassignedDragData

// Drop target data
interface RackDropData {
  rackId: string
  slotU: number
  kind: "rack"
}

interface EnclosureDropData {
  enclosureId: string
  slotIndex: number
  kind: "enclosure"
}

type DropData = RackDropData | EnclosureDropData

// Cut-state for keyboard cut-paste accessibility
interface CutState {
  nodeId: string
  srcRackId: string
  slotU: number
  heightU: number
}

// ─── Constants ────────────────────────────────────────────────────────────────

const SLOT_HEIGHT_PX = 24   // px per 1U
const RACK_WIDTH_PX  = 280  // px wide
const HEIGHT_U_PRESETS = [1, 2, 4, 8]

// ─── Status helpers ───────────────────────────────────────────────────────────

const STATUS_COLOR: Record<string, string> = {
  active:       "bg-green-500",
  provisioning: "bg-amber-400",
  error:        "bg-red-500",
  offline:      "bg-gray-400",
}

function statusDot(status: string) {
  return (
    <span
      className={cn("inline-block h-2.5 w-2.5 rounded-full shrink-0", STATUS_COLOR[status] ?? "bg-gray-400")}
      aria-hidden
    />
  )
}

// ─── Shared drag context (passed via React context) ───────────────────────────

interface DragCtx {
  activeDrag: DragData | null
  cutState: CutState | null
  setCutState: (s: CutState | null) => void
  onSlotPaste: (rackId: string, slotU: number, rackHeightU: number, rackPositions: NodeRackPosition[]) => void
  onRemoveFromRack: (nodeId: string, rackId: string, hostname: string, rackName: string) => void
  allRackPositions: Map<string, NodeRackPosition[]>
  onEnclosureSlotDrop: (nodeId: string, enclosureId: string, slotIndex: number) => void
}

const DragContext = React.createContext<DragCtx>({
  activeDrag: null,
  cutState: null,
  setCutState: () => {},
  onSlotPaste: () => {},
  onRemoveFromRack: () => {},
  allRackPositions: new Map(),
  onEnclosureSlotDrop: () => {},
})

// ─── Draggable node block (already placed in rack) ────────────────────────────

function NodeBlock({
  pos,
  hostname,
  status,
  rackHeightU,
  rackName,
  occupiedSlots,
  onResize,
  onClick,
}: {
  pos: RackDirectPosition
  hostname: string
  status: string
  rackHeightU: number
  rackName: string
  occupiedSlots: Set<number>
  onResize: (nodeId: string, newHeightU: number) => void
  onClick: () => void
}) {
  const { activeDrag, cutState, setCutState, onRemoveFromRack } = React.useContext(DragContext)
  const isCut = cutState?.nodeId === pos.node_id

  const { attributes, listeners, setNodeRef, transform, isDragging } = useDraggable({
    id: `node-${pos.node_id}`,
    data: {
      nodeId: pos.node_id,
      rackId: pos.rack_id,
      slotU: pos.slot_u,
      heightU: pos.height_u,
      fromUnassigned: false,
    } satisfies InRackDragData,
  })

  // Resize popover state
  const [resizing, setResizing] = React.useState(false)
  const [newHeightInput, setNewHeightInput] = React.useState(String(pos.height_u))

  const style: React.CSSProperties = {
    transform: CSS.Translate.toString(transform ?? null),
    opacity: isDragging ? 0.4 : 1,
    height: "100%",
    width: "100%",
    cursor: "grab",
    touchAction: "none",
    userSelect: "none",
    position: "relative",
  }

  function handleResizeSave() {
    const val = parseInt(newHeightInput, 10)
    if (!val || val < 1) return
    // Validate: fits in rack from current slot_u
    const topSlot = pos.slot_u + val - 1
    if (topSlot > rackHeightU) {
      toast({ title: "Cannot resize", description: `Node would extend beyond rack top (U${rackHeightU})`, variant: "destructive" })
      return
    }
    // Check overlap: all slots [slot_u, slot_u+val-1] must be either this node or empty
    for (let u = pos.slot_u; u <= topSlot; u++) {
      if (occupiedSlots.has(u) && u > pos.slot_u + pos.height_u - 1) {
        toast({ title: "Cannot resize", description: `Slot U${u} is occupied by another node`, variant: "destructive" })
        return
      }
    }
    setResizing(false)
    onResize(pos.node_id, val)
  }

  // Keyboard handler: Cmd-X marks this node as "cut"
  function handleKeyDown(e: React.KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key === "x") {
      e.preventDefault()
      setCutState({
        nodeId: pos.node_id,
        srcRackId: pos.rack_id,
        slotU: pos.slot_u,
        heightU: pos.height_u,
      })
      toast({ title: `Cut: ${hostname}`, description: "Focus a slot and press Cmd-V to place" })
    }
  }

  // Show highlighted border if this node is the active drag or cut target
  const isActiveInDrag = activeDrag && !activeDrag.fromUnassigned && (activeDrag as InRackDragData).nodeId === pos.node_id

  return (
    <div
      ref={setNodeRef}
      style={style}
      {...listeners}
      {...attributes}
      role="button"
      tabIndex={0}
      aria-label={`Node ${hostname} at U${pos.slot_u}`}
      onKeyDown={handleKeyDown}
      className={cn(
        "flex items-center gap-2 rounded border bg-secondary px-2 text-xs font-mono hover:bg-secondary/70 focus:outline-none focus:ring-1 focus:ring-accent",
        isCut ? "border-amber-400 ring-1 ring-amber-400/60" : "border-border",
        isActiveInDrag && "opacity-40",
      )}
    >
      {statusDot(status)}
      <span className="truncate flex-1" onClick={(e) => { e.stopPropagation(); onClick() }}>{hostname}</span>
      <span className="text-muted-foreground shrink-0">{pos.height_u}U</span>
      {/* Cut indicator */}
      {isCut && <Scissors className="h-2.5 w-2.5 text-amber-400 shrink-0" />}
      {/* Edit-U button — stops propagation so it doesn't trigger drag */}
      <button
        className="shrink-0 rounded p-0.5 hover:bg-accent/40 focus:outline-none"
        title="Resize"
        onPointerDown={(e) => e.stopPropagation()}
        onClick={(e) => {
          e.stopPropagation()
          setNewHeightInput(String(pos.height_u))
          setResizing(true)
        }}
      >
        <Pencil className="h-2.5 w-2.5 text-muted-foreground" />
      </button>
      {/* Remove-from-rack button — stops propagation so it doesn't trigger drag */}
      <button
        className="shrink-0 rounded p-0.5 hover:bg-destructive/20 focus:outline-none"
        title={`Remove ${hostname} from ${rackName}`}
        onPointerDown={(e) => e.stopPropagation()}
        onClick={(e) => {
          e.stopPropagation()
          onRemoveFromRack(pos.node_id, pos.rack_id, hostname, rackName)
        }}
      >
        <LogOut className="h-2.5 w-2.5 text-muted-foreground hover:text-destructive" />
      </button>

      {/* Resize popover — rendered inline, positioned absolute relative to block */}
      {resizing && (
        <div
          className="absolute z-50 top-full left-0 mt-1 bg-popover border border-border rounded shadow-md p-2 flex items-center gap-1.5"
          style={{ minWidth: 160 }}
          onPointerDown={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
        >
          <span className="text-[10px] text-muted-foreground shrink-0">Height (U):</span>
          <Input
            type="number"
            min={1}
            max={rackHeightU}
            value={newHeightInput}
            onChange={e => setNewHeightInput(e.target.value)}
            onKeyDown={e => { if (e.key === "Enter") handleResizeSave(); if (e.key === "Escape") setResizing(false) }}
            className="h-6 w-14 text-xs px-1 font-mono"
            autoFocus
          />
          <button className="rounded p-0.5 hover:bg-accent/40" onClick={handleResizeSave} title="Save"><Check className="h-3 w-3 text-green-500" /></button>
          <button className="rounded p-0.5 hover:bg-accent/40" onClick={() => setResizing(false)} title="Cancel"><X className="h-3 w-3 text-muted-foreground" /></button>
        </div>
      )}
    </div>
  )
}

// ─── Draggable unassigned node row ────────────────────────────────────────────

function UnassignedNodeRow({ node }: { node: UnassignedNodeStub }) {
  const [heightU, setHeightU] = React.useState(1)

  const { attributes, listeners, setNodeRef, transform, isDragging } = useDraggable({
    id: `unassigned-${node.id}`,
    data: {
      nodeId: node.id,
      heightU,
      fromUnassigned: true,
    } satisfies UnassignedDragData,
  })

  const style: React.CSSProperties = {
    transform: CSS.Translate.toString(transform ?? null),
    opacity: isDragging ? 0.4 : 1,
    touchAction: "none",
    userSelect: "none",
  }

  return (
    <div
      ref={setNodeRef}
      style={style}
      className="flex items-center gap-2 rounded border border-border bg-secondary/50 px-2 py-1 text-xs font-mono cursor-grab hover:bg-secondary/80"
    >
      {/* Drag handle area — covers the icon + hostname */}
      <div
        className="flex items-center gap-1.5 flex-1 min-w-0"
        {...listeners}
        {...attributes}
      >
        {statusDot(node.status)}
        <span className="truncate">{node.hostname}</span>
      </div>
      {/* Height-U selector — stops drag propagation */}
      <div
        className="flex items-center gap-0.5 shrink-0"
        onPointerDown={e => e.stopPropagation()}
      >
        {HEIGHT_U_PRESETS.map(u => (
          <button
            key={u}
            onClick={e => { e.stopPropagation(); setHeightU(u) }}
            className={cn(
              "rounded px-1.5 py-1 text-[10px] leading-none border transition-colors font-sans",
              heightU === u
                ? "bg-accent text-accent-foreground border-accent"
                : "bg-secondary border-border text-foreground hover:bg-accent/30 hover:border-accent/60"
            )}
          >
            {u}U
          </button>
        ))}
      </div>
    </div>
  )
}

// ─── Droppable slot ───────────────────────────────────────────────────────────

function SlotDropZone({
  rackId,
  slotU,
  rackHeightU,
  occupiedByOthers,
  onPaste,
}: {
  rackId: string
  slotU: number
  rackHeightU: number
  occupiedByOthers: Set<number>
  onPaste: (rackId: string, slotU: number) => void
}) {
  const { activeDrag, cutState } = React.useContext(DragContext)
  const { setNodeRef, isOver } = useDroppable({
    id: `slot-${rackId}-${slotU}`,
    data: { rackId, slotU },
  })

  // Determine if this slot can accept the active drag
  const dragHeightU = activeDrag?.heightU ?? cutState?.heightU ?? 0
  let canAccept = false
  if (dragHeightU > 0) {
    const topSlot = slotU + dragHeightU - 1
    if (topSlot <= rackHeightU) {
      canAccept = true
      for (let u = slotU; u <= topSlot; u++) {
        if (occupiedByOthers.has(u)) {
          canAccept = false
          break
        }
      }
    }
  }

  const isDraggingOrCutting = activeDrag !== null || cutState !== null

  // Keyboard handler: Cmd-V pastes cut node here
  function handleKeyDown(e: React.KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key === "v" && cutState) {
      e.preventDefault()
      onPaste(rackId, slotU)
    }
  }

  return (
    <div
      ref={setNodeRef}
      tabIndex={cutState ? 0 : -1}
      role={cutState ? "button" : undefined}
      aria-label={cutState ? `Paste to U${slotU}` : undefined}
      onKeyDown={handleKeyDown}
      style={{ height: SLOT_HEIGHT_PX }}
      className={cn(
        "w-full border-b border-border/30 transition-colors focus:outline-none",
        isOver && canAccept && "bg-accent/30",
        isOver && !canAccept && "bg-destructive/20",
        isDraggingOrCutting && !canAccept && !isOver && "opacity-30",
        isDraggingOrCutting && canAccept && !isOver && "bg-accent/8",
        cutState && canAccept && "focus:ring-1 focus:ring-accent",
      )}
    />
  )
}

// ─── Enclosure slot drop zone ─────────────────────────────────────────────────

function EnclosureSlotDropZone({
  enclosureId,
  slotIndex,
  nodeId,
  hostname,
  status,
  onNodeClick,
  onEject,
}: {
  enclosureId: string
  slotIndex: number
  nodeId?: string
  hostname?: string
  status?: string
  onNodeClick?: (nodeId: string) => void
  /** Bug #247: eject/unassign the node from this slot back to the unassigned sidebar. */
  onEject?: (enclosureId: string, slotIndex: number, hostname: string) => void
}) {
  const { activeDrag } = React.useContext(DragContext)
  const { setNodeRef, isOver } = useDroppable({
    id: `enc-slot-${enclosureId}-${slotIndex}`,
    data: { enclosureId, slotIndex, kind: "enclosure" } satisfies EnclosureDropData,
  })

  const isDragging = activeDrag !== null
  const occupied = !!nodeId

  return (
    <div
      ref={setNodeRef}
      className={cn(
        "relative flex items-center rounded border text-[10px] font-mono px-1 transition-colors",
        "min-h-[22px] flex-1 min-w-0",
        occupied
          ? "border-border bg-secondary/80"
          : "border-dashed border-border/60 bg-card",
        isOver && !occupied && "bg-accent/30 border-accent",
        isOver && occupied && "bg-destructive/20",
        isDragging && !occupied && !isOver && "bg-accent/8",
      )}
    >
      {/* Slot index label — fixed width, doesn't flex */}
      <span className="text-muted-foreground/50 shrink-0 text-[9px] w-3 text-right mr-0.5">{slotIndex}</span>

      {occupied ? (
        /* Occupied slot: status dot + truncated hostname + eject button */
        <>
          <button
            className="flex items-center gap-1 flex-1 min-w-0 cursor-pointer hover:text-foreground text-left"
            onClick={() => nodeId && onNodeClick && onNodeClick(nodeId)}
            onPointerDown={e => e.stopPropagation()}
          >
            {statusDot(status ?? "offline")}
            <span className="truncate flex-1 min-w-0">{hostname ?? nodeId?.slice(0, 8)}</span>
          </button>
          {onEject && (
            <button
              className="shrink-0 rounded p-0.5 ml-0.5 hover:bg-destructive/20 focus:outline-none"
              title={`Eject ${hostname ?? nodeId?.slice(0, 8)} from slot ${slotIndex}`}
              onPointerDown={e => e.stopPropagation()}
              onClick={e => { e.stopPropagation(); onEject(enclosureId, slotIndex, hostname ?? nodeId?.slice(0, 8) ?? "") }}
            >
              <LogOut className="h-2 w-2 text-muted-foreground hover:text-destructive" />
            </button>
          )}
        </>
      ) : (
        /* Empty slot: placeholder text */
        <span className="text-muted-foreground/40 italic text-[9px] flex-1">
          {isDragging ? "Drop here" : "Empty"}
        </span>
      )}
    </div>
  )
}

// ─── EnclosureBlock ───────────────────────────────────────────────────────────

function EnclosureBlock({
  enclosure,
  nodes,
  onNodeClick,
  onDelete,
  onEjectSlot,
}: {
  enclosure: Enclosure
  nodes: Map<string, NodeHealth>
  onNodeClick: (nodeId: string) => void
  onDelete: (enclosureId: string, label: string) => void
  /** Bug #247: callback to eject (unassign) a node from a specific slot. */
  onEjectSlot?: (enclosureId: string, slotIndex: number, hostname: string) => void
}) {
  const et = enclosureTypeMeta[enclosure.type_id]
  const slotCount = et?.slot_count ?? (enclosure.slots?.length ?? 0)

  // Build grid style based on orientation.
  let gridCols = 1
  if (et?.orientation === "horizontal") gridCols = slotCount
  else if (et?.orientation === "grid_2x2" || et?.orientation === "grid_2x4") gridCols = 2

  const label = enclosure.label || et?.display_name || enclosure.type_id

  return (
    <div
      className="absolute left-0 right-0 border-2 border-border/70 rounded bg-card/80 overflow-hidden"
      style={{
        top: 0,
        height: "100%",
        zIndex: 5,
      }}
    >
      {/* Chassis header */}
      <div className="flex items-center justify-between gap-1 px-1 py-0.5 bg-muted/30 border-b border-border/40">
        <div className="flex items-center gap-1 min-w-0">
          <Layers className="h-2.5 w-2.5 text-muted-foreground shrink-0" />
          <span className="text-[9px] font-medium truncate text-muted-foreground">{label}</span>
        </div>
        <button
          className="shrink-0 rounded p-0.5 hover:bg-destructive/20"
          title={`Delete chassis ${label}`}
          onPointerDown={e => e.stopPropagation()}
          onClick={e => { e.stopPropagation(); onDelete(enclosure.id, label) }}
        >
          <X className="h-2 w-2 text-muted-foreground hover:text-destructive" />
        </button>
      </div>

      {/* Slot grid */}
      <div
        className="p-0.5 gap-0.5"
        style={{
          display: "grid",
          gridTemplateColumns: `repeat(${gridCols}, 1fr)`,
          height: "calc(100% - 20px)",
          alignContent: "start",
        }}
      >
        {(enclosure.slots ?? []).map(slot => {
          const node = slot.node_id ? nodes.get(slot.node_id) : undefined
          return (
            <EnclosureSlotDropZone
              key={slot.slot_index}
              enclosureId={enclosure.id}
              slotIndex={slot.slot_index}
              nodeId={slot.node_id}
              hostname={node?.hostname}
              status={node?.status}
              onNodeClick={onNodeClick}
              onEject={onEjectSlot}
            />
          )
        })}
      </div>
    </div>
  )
}

// Minimal catalog metadata mirrored from the backend for layout hints.
// The full catalog is fetched from GET /api/v1/enclosure-types.
const enclosureTypeMeta: Record<string, EnclosureType> = {}

// ─── Slot fit validation (reusable by drag + keyboard paste) ──────────────────

function validateDrop(params: {
  targetRackId: string
  targetSlotU: number
  dragHeightU: number
  sourceNodeId: string
  allRackPositions: Map<string, NodeRackPosition[]>
  racks: Rack[]
}): { ok: true } | { ok: false; reason: string } {
  const { targetRackId, targetSlotU, dragHeightU, sourceNodeId, allRackPositions, racks } = params
  const rack = racks.find(r => r.id === targetRackId)
  if (!rack) return { ok: false, reason: "Rack not found" }

  const topSlot = targetSlotU + dragHeightU - 1
  if (topSlot > rack.height_u) {
    return {
      ok: false,
      reason: `Rack ${rack.name} only has ${rack.height_u}U total — node would extend to U${topSlot}`,
    }
  }

  const positions = allRackPositions.get(targetRackId) ?? []
  const conflictSlots: number[] = []
  for (const pos of positions) {
    if (pos.node_id === sourceNodeId) continue
    // allRackPositions only contains rack-direct positions (ListPositionsByRack filters enclosure-resident nodes)
    const posHeightU = pos.height_u ?? 1
    const posSlotU = pos.slot_u ?? 1
    for (let i = 0; i < posHeightU; i++) {
      const u = posSlotU + i
      if (u >= targetSlotU && u <= topSlot) {
        conflictSlots.push(u)
      }
    }
  }
  if (conflictSlots.length > 0) {
    const slotRange = conflictSlots.length === 1
      ? `U${conflictSlots[0]}`
      : `U${conflictSlots[0]}-U${conflictSlots[conflictSlots.length - 1]}`
    return { ok: false, reason: `Slots ${slotRange} already occupied` }
  }

  return { ok: true }
}

// ─── Rack tile ────────────────────────────────────────────────────────────────

function RackTile({
  rack,
  nodes,
  onNodeClick,
  onResize,
  onAddEnclosure,
  onDeleteEnclosure,
  onEjectSlot,
}: {
  rack: Rack
  nodes: Map<string, NodeHealth>
  onNodeClick: (nodeId: string) => void
  onResize: (nodeId: string, rackId: string, newHeightU: number) => void
  onAddEnclosure: (rackId: string) => void
  onDeleteEnclosure: (enclosureId: string, label: string) => void
  onEjectSlot: (enclosureId: string, slotIndex: number, hostname: string) => void
}) {
  const [powerModal, setPowerModal] = React.useState<PowerAction | null>(null)
  const { onSlotPaste } = React.useContext(DragContext)
  const positions = rack.positions ?? []
  const enclosures = rack.enclosures ?? []

  // Build a set of all U slots occupied by rack-direct nodes or enclosures.
  const allOccupied = new Set<number>()
  for (const pos of positions) {
    for (let i = 0; i < (pos.height_u ?? 1); i++) {
      allOccupied.add((pos.slot_u ?? 0) + i)
    }
  }
  // Enclosure chassis also occupy U slots — treat them as blocked for drop zones.
  for (const enc of enclosures) {
    for (let i = 0; i < enc.height_u; i++) {
      allOccupied.add(enc.rack_slot_u + i)
    }
  }

  // Pre-compute node blocks (rack-direct only; enclosure-resident nodes are rendered by EnclosureBlock).
  const nodeBlocks = positions.map(pos => {
    const node = nodes.get(pos.node_id)
    const topUNum = (pos.slot_u ?? 0) + (pos.height_u ?? 1) - 1
    const rowIndex = rack.height_u - topUNum
    return { pos, node, rowIndex }
  })

  const hasNodes = positions.length > 0 || enclosures.some(e => (e.slots ?? []).some(s => s.node_id))

  return (
    <div
      className="shrink-0 flex flex-col gap-3"
      style={{ width: RACK_WIDTH_PX + 20 }}
    >
      {/* Rack header */}
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5 min-w-0">
          <Building2 className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
          <span className="text-sm font-medium truncate">{rack.name}</span>
          <span className="text-xs text-muted-foreground shrink-0">
            ({positions.length}/{rack.height_u}U
            {enclosures.length > 0 ? ` · ${enclosures.length} chassis` : ""})
          </span>
        </div>
        <Button
          variant="ghost"
          size="sm"
          className="h-5 text-[10px] gap-0.5 px-1.5 shrink-0"
          title="Add chassis"
          onClick={() => onAddEnclosure(rack.id)}
        >
          <Cpu className="h-2.5 w-2.5" />
          +Chassis
        </Button>
      </div>

      {/* Bulk power controls */}
      <div className="flex items-center gap-1 flex-wrap">
        <Button
          variant="outline"
          size="sm"
          className="h-6 text-[10px] gap-1 px-2"
          onClick={() => setPowerModal("on")}
          disabled={!hasNodes}
          title="Power on all"
        >
          <Power className="h-2.5 w-2.5 text-green-500" />
          On
        </Button>
        <Button
          variant="outline"
          size="sm"
          className="h-6 text-[10px] gap-1 px-2"
          onClick={() => setPowerModal("off")}
          disabled={!hasNodes}
          title="Power off all"
        >
          <PowerOff className="h-2.5 w-2.5 text-red-500" />
          Off
        </Button>
        <Button
          variant="outline"
          size="sm"
          className="h-6 text-[10px] gap-1 px-2"
          onClick={() => setPowerModal("cycle")}
          disabled={!hasNodes}
          title="Reboot all"
        >
          <RefreshCw className="h-2.5 w-2.5" />
          Reboot
        </Button>
      </div>

      {/* Rack chassis */}
      <div
        className="relative border-2 border-border rounded-md bg-card overflow-hidden"
        style={{ width: RACK_WIDTH_PX, height: rack.height_u * SLOT_HEIGHT_PX }}
        role="img"
        aria-label={`Rack ${rack.name}`}
      >
        {/* Empty rack hint */}
        {!hasNodes && (
          <div className="absolute inset-0 flex items-center justify-center pointer-events-none z-10">
            <p className="text-[10px] text-muted-foreground/50 text-center px-4">
              No nodes assigned yet<br />Drag from the sidebar<br />
              Or click +Chassis to add a multi-node enclosure
            </p>
          </div>
        )}

        {/* U labels + drop zones */}
        {Array.from({ length: rack.height_u }, (_, i) => {
          const uNum = rack.height_u - i  // U1 at bottom, highest U at top

          const occupiedByOthers = new Set<number>()
          for (const pos of positions) {
            for (let j = 0; j < (pos.height_u ?? 1); j++) {
              occupiedByOthers.add((pos.slot_u ?? 0) + j)
            }
          }
          // Also block enclosure U-ranges so rack-direct nodes can't be dropped there.
          for (const enc of enclosures) {
            for (let j = 0; j < enc.height_u; j++) {
              occupiedByOthers.add(enc.rack_slot_u + j)
            }
          }

          return (
            <div
              key={uNum}
              className="relative flex items-center"
              style={{ top: i * SLOT_HEIGHT_PX, left: 0, right: 0, height: SLOT_HEIGHT_PX, position: "absolute", width: "100%" }}
            >
              {/* U label */}
              <span className="w-8 text-right pr-1 text-[9px] text-muted-foreground select-none shrink-0 font-mono">
                {uNum}
              </span>
              {/* Drop zone for this slot */}
              <div className="flex-1 relative" style={{ height: SLOT_HEIGHT_PX }}>
                <SlotDropZone
                  rackId={rack.id}
                  slotU={uNum}
                  rackHeightU={rack.height_u}
                  occupiedByOthers={occupiedByOthers}
                  onPaste={(rId, sU) => onSlotPaste(rId, sU, rack.height_u, positions)}
                />
              </div>
            </div>
          )
        })}

        {/* Rack-direct node blocks — absolutely positioned over the drop zones */}
        {nodeBlocks.map(({ pos, node, rowIndex }) => (
          <div
            key={pos.node_id}
            style={{
              position: "absolute",
              top: rowIndex * SLOT_HEIGHT_PX,
              left: 34,
              right: 2,
              height: (pos.height_u ?? 1) * SLOT_HEIGHT_PX - 2,
              zIndex: 10,
            }}
          >
            <NodeBlock
              pos={pos as RackDirectPosition}
              hostname={node?.hostname ?? pos.node_id.slice(0, 8)}
              status={node?.status ?? "offline"}
              rackHeightU={rack.height_u}
              rackName={rack.name}
              occupiedSlots={allOccupied}
              onResize={(nodeId, newHeightU) => onResize(nodeId, rack.id, newHeightU)}
              onClick={() => onNodeClick(pos.node_id)}
            />
          </div>
        ))}

        {/* Enclosure blocks — rendered at their rack_slot_u position */}
        {enclosures.map(enc => {
          // Enclosure top row index: counted from the top of the rack.
          // rack_slot_u is the bottom-most U. The top U is rack_slot_u + height_u - 1.
          const topU = enc.rack_slot_u + enc.height_u - 1
          const rowIndex = rack.height_u - topU
          return (
            <div
              key={enc.id}
              style={{
                position: "absolute",
                top: rowIndex * SLOT_HEIGHT_PX,
                left: 34,
                right: 2,
                height: enc.height_u * SLOT_HEIGHT_PX - 2,
                zIndex: 8,
              }}
            >
              <EnclosureBlock
                enclosure={enc}
                nodes={nodes}
                onNodeClick={onNodeClick}
                onDelete={onDeleteEnclosure}
                onEjectSlot={onEjectSlot}
              />
            </div>
          )
        })}
      </div>

      {/* Bulk power modal */}
      {powerModal && (
        <BulkPowerModal
          rack={rack}
          action={powerModal}
          positions={positions}
          nodes={nodes}
          onClose={() => setPowerModal(null)}
        />
      )}

      {/* Keyboard hint */}
      <p className="text-[9px] text-muted-foreground">
        Cmd-X to cut a node · Cmd-V on a focused slot to paste
      </p>
    </div>
  )
}

// ─── Bulk power confirmation modal ────────────────────────────────────────────

type PowerAction = "on" | "off" | "cycle"

function BulkPowerModal({
  rack,
  action,
  positions,
  nodes,
  onClose,
}: {
  rack: Rack
  action: PowerAction
  positions: NodeRackPosition[]
  nodes: Map<string, NodeHealth>
  onClose: () => void
}) {
  const [confirm, setConfirm] = React.useState("")
  const [running, setRunning] = React.useState(false)
  const [results, setResults] = React.useState<Array<{ nodeId: string; ok: boolean; err?: string }>>([])
  const [done, setDone] = React.useState(false)

  const actionLabel: Record<PowerAction, string> = {
    on: "Power on all",
    off: "Power off all",
    cycle: "Reboot all",
  }
  const actionPath: Record<PowerAction, string> = { on: "on", off: "off", cycle: "cycle" }
  const expectedConfirm = rack.name

  async function handleExecute() {
    if (confirm !== expectedConfirm) return
    setRunning(true)
    const out: typeof results = []
    for (const pos of positions) {
      try {
        await apiFetch(`/api/v1/nodes/${pos.node_id}/power/${actionPath[action]}`, { method: "POST" })
        out.push({ nodeId: pos.node_id, ok: true })
      } catch (e) {
        out.push({ nodeId: pos.node_id, ok: false, err: (e as Error).message })
      }
    }
    setResults(out)
    setRunning(false)
    setDone(true)
  }

  return (
    <Dialog open onOpenChange={v => !v && onClose()}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-sm">
            <AlertTriangle className="h-4 w-4 text-destructive shrink-0" />
            {actionLabel[action]} — {rack.name}
          </DialogTitle>
        </DialogHeader>
        {!done ? (
          <div className="space-y-4">
            <p className="text-sm text-muted-foreground">
              This will send a <strong>{action}</strong> command to all {positions.length} node{positions.length !== 1 ? "s" : ""} in rack <strong>{rack.name}</strong>.
              Type the rack name to confirm.
            </p>
            <Input
              placeholder={rack.name}
              value={confirm}
              onChange={e => setConfirm(e.target.value)}
              className="font-mono"
              disabled={running}
              autoFocus
            />
            <div className="flex gap-2 justify-end">
              <Button variant="outline" onClick={onClose} disabled={running}>Cancel</Button>
              <Button
                variant="destructive"
                onClick={handleExecute}
                disabled={confirm !== expectedConfirm || running}
              >
                {running && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
                {actionLabel[action]}
              </Button>
            </div>
          </div>
        ) : (
          <div className="space-y-3">
            <p className="text-sm font-medium">Results:</p>
            <div className="space-y-1 max-h-48 overflow-auto">
              {results.map(r => {
                const node = nodes.get(r.nodeId)
                return (
                  <div key={r.nodeId} className="flex items-center gap-2 text-xs">
                    <span className={r.ok ? "text-green-500" : "text-red-500"}>{r.ok ? "✓" : "✗"}</span>
                    <span className="font-mono">{node?.hostname ?? r.nodeId.slice(0, 8)}</span>
                    {r.err && <span className="text-muted-foreground truncate">{r.err}</span>}
                  </div>
                )
              })}
            </div>
            <Button variant="outline" className="w-full" onClick={onClose}>Close</Button>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}

// ─── Add Rack modal ───────────────────────────────────────────────────────────

function AddRackModal({ onClose, onCreated }: { onClose: () => void; onCreated: (rack: Rack) => void }) {
  const [name, setName] = React.useState("")
  const [heightU, setHeightU] = React.useState("42")
  const qc = useQueryClient()

  const createMut = useMutation({
    mutationFn: () =>
      apiFetch<{ rack: Rack }>("/api/v1/racks", {
        method: "POST",
        body: JSON.stringify({ name: name.trim(), height_u: parseInt(heightU, 10) || 42 }),
      }),
    onSuccess: (data) => {
      toast({ title: `Rack "${data.rack.name}" created` })
      qc.invalidateQueries({ queryKey: ["racks"] })
      onCreated(data.rack)
    },
    onError: (e: Error) => {
      toast({ title: "Failed to create rack", description: e.message, variant: "destructive" })
    },
  })

  return (
    <Dialog open onOpenChange={v => !v && onClose()}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle>Create rack</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Rack name</label>
            <Input
              placeholder="e.g. rack-a"
              value={name}
              onChange={e => setName(e.target.value)}
              onKeyDown={e => e.key === "Enter" && name.trim() && createMut.mutate()}
              autoFocus
            />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Height (U)</label>
            <Input
              type="number"
              min={1}
              max={100}
              value={heightU}
              onChange={e => setHeightU(e.target.value)}
            />
          </div>
          <div className="flex gap-2 justify-end">
            <Button variant="outline" onClick={onClose} disabled={createMut.isPending}>Cancel</Button>
            <Button
              onClick={() => createMut.mutate()}
              disabled={!name.trim() || createMut.isPending}
            >
              {createMut.isPending && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
              Create
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// ─── Add Enclosure modal ──────────────────────────────────────────────────────

function AddEnclosureModal({
  rackId,
  onClose,
}: {
  rackId: string
  onClose: () => void
}) {
  const [typeId, setTypeId] = React.useState("")
  const [rackSlotU, setRackSlotU] = React.useState("1")
  const [label, setLabel] = React.useState("")
  const qc = useQueryClient()

  const typesQuery = useQuery<{ types: EnclosureType[]; total: number }>({
    queryKey: ["enclosure-types"],
    queryFn: () => apiFetch("/api/v1/enclosure-types"),
    staleTime: Infinity,
  })

  const types = typesQuery.data?.types ?? []

  const createMut = useMutation({
    mutationFn: () =>
      apiFetch<{ enclosure: Enclosure }>(`/api/v1/racks/${rackId}/enclosures`, {
        method: "POST",
        body: JSON.stringify({
          type_id: typeId,
          rack_slot_u: parseInt(rackSlotU, 10) || 1,
          label: label.trim() || undefined,
        }),
      }),
    onSuccess: (data) => {
      toast({ title: `Chassis "${data.enclosure.label || data.enclosure.type_id}" added` })
      qc.invalidateQueries({ queryKey: ["racks"] })
      onClose()
    },
    onError: (e: Error) => {
      toast({ title: "Failed to add chassis", description: e.message, variant: "destructive" })
    },
  })

  return (
    <Dialog open onOpenChange={v => !v && onClose()}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-sm">
            <Cpu className="h-4 w-4 text-muted-foreground" />
            Add chassis to rack
          </DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Chassis type</label>
            {typesQuery.isPending ? (
              <Skeleton className="h-8 w-full" />
            ) : (
              <select
                className="w-full rounded border border-border bg-background px-2 py-1.5 text-sm focus:outline-none focus:ring-1 focus:ring-ring"
                value={typeId}
                onChange={e => setTypeId(e.target.value)}
                autoFocus
              >
                <option value="">Select a chassis type…</option>
                {types.map(t => (
                  <option key={t.id} value={t.id}>
                    {t.display_name} — {t.slot_count} slots, {t.height_u}U
                  </option>
                ))}
              </select>
            )}
            {typeId && types.find(t => t.id === typeId)?.description && (
              <p className="text-[10px] text-muted-foreground mt-1">
                {types.find(t => t.id === typeId)!.description}
              </p>
            )}
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Rack slot (bottom U)</label>
            <Input
              type="number"
              min={1}
              value={rackSlotU}
              onChange={e => setRackSlotU(e.target.value)}
              placeholder="e.g. 1"
            />
          </div>
          <div>
            <label className="text-xs text-muted-foreground mb-1 block">Label (optional)</label>
            <Input
              placeholder="e.g. blade-chassis-01"
              value={label}
              onChange={e => setLabel(e.target.value)}
              onKeyDown={e => e.key === "Enter" && typeId && createMut.mutate()}
            />
          </div>
          <div className="flex gap-2 justify-end">
            <Button variant="outline" onClick={onClose} disabled={createMut.isPending}>Cancel</Button>
            <Button
              onClick={() => createMut.mutate()}
              disabled={!typeId || createMut.isPending}
            >
              {createMut.isPending && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
              Add chassis
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// ─── Delete enclosure confirmation modal ──────────────────────────────────────

function DeleteEnclosureModal({
  enclosureId,
  label,
  onClose,
}: {
  enclosureId: string
  label: string
  onClose: () => void
}) {
  const qc = useQueryClient()

  const deleteMut = useMutation({
    mutationFn: () =>
      apiFetch(`/api/v1/enclosures/${enclosureId}`, { method: "DELETE" }),
    onSuccess: () => {
      toast({ title: `Chassis "${label}" removed` })
      qc.invalidateQueries({ queryKey: ["racks"] })
      qc.invalidateQueries({ queryKey: ["nodes-unassigned"] })
      onClose()
    },
    onError: (e: Error) => {
      toast({ title: "Failed to delete chassis", description: e.message, variant: "destructive" })
    },
  })

  return (
    <Dialog open onOpenChange={v => !v && onClose()}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-sm">
            <AlertTriangle className="h-4 w-4 text-destructive" />
            Remove chassis
          </DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Remove chassis <strong className="font-mono text-foreground">{label}</strong>?
            All nodes in its slots will return to the Unassigned sidebar.
          </p>
          <div className="flex gap-2 justify-end">
            <Button variant="outline" onClick={onClose} disabled={deleteMut.isPending}>Cancel</Button>
            <Button variant="destructive" onClick={() => deleteMut.mutate()} disabled={deleteMut.isPending}>
              {deleteMut.isPending && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
              Remove chassis
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// ─── Unassign confirmation modal ─────────────────────────────────────────────

// Pending unassign confirmation shape
interface UnassignPending {
  nodeId: string
  rackId: string
  hostname: string
  rackName: string
}

function UnassignConfirmModal({
  pending,
  onConfirm,
  onClose,
  isPending,
}: {
  pending: UnassignPending
  onConfirm: () => void
  onClose: () => void
  isPending: boolean
}) {
  return (
    <Dialog open onOpenChange={v => !v && onClose()}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-sm">
            <LogOut className="h-4 w-4 text-muted-foreground shrink-0" />
            Remove from rack
          </DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Remove <strong className="font-mono text-foreground">{pending.hostname}</strong> from{" "}
            <strong className="font-mono text-foreground">{pending.rackName}</strong>?
            It will return to the Unassigned sidebar.
          </p>
          <div className="flex gap-2 justify-end">
            <Button variant="outline" onClick={onClose} disabled={isPending}>Cancel</Button>
            <Button onClick={onConfirm} disabled={isPending}>
              {isPending && <Loader2 className="h-4 w-4 animate-spin mr-2" />}
              Remove from rack
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// ─── Unassigned nodes sidebar ─────────────────────────────────────────────────

function UnassignedSidebar() {
  const query = useQuery<ListUnassignedNodesResponse>({
    queryKey: ["nodes-unassigned"],
    queryFn: () => apiFetch<ListUnassignedNodesResponse>("/api/v1/nodes/unassigned"),
    refetchInterval: 15_000,
  })

  const nodes = query.data?.nodes ?? []

  return (
    <div className="w-64 shrink-0 border-r border-border pr-4 space-y-2">
      <div className="flex items-center justify-between">
        <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Unassigned nodes</p>
        {query.isFetching && <Loader2 className="h-3 w-3 animate-spin text-muted-foreground" />}
      </div>

      {query.isPending ? (
        <div className="space-y-1">
          {[1, 2, 3].map(i => <Skeleton key={i} className="h-7 w-full" />)}
        </div>
      ) : query.isError ? (
        <div className="space-y-1">
          <p className="text-xs text-destructive">Failed to load unassigned nodes</p>
          <p className="text-[10px] text-muted-foreground font-mono break-all">
            {query.error instanceof Error ? query.error.message : String(query.error)}
          </p>
          <button
            onClick={() => query.refetch()}
            className="text-[10px] text-muted-foreground underline hover:text-foreground"
          >
            Retry
          </button>
        </div>
      ) : nodes.length === 0 ? (
        <p className="text-xs text-muted-foreground italic">All nodes are assigned to a rack.</p>
      ) : (
        <div className="space-y-1 max-h-[calc(100vh-200px)] overflow-y-auto">
          <p className="text-[10px] text-muted-foreground">Select U-size then drag onto a rack slot.</p>
          {nodes.map(n => (
            <UnassignedNodeRow key={n.id} node={n} />
          ))}
        </div>
      )}
    </div>
  )
}

// ─── Main page ────────────────────────────────────────────────────────────────

export function DatacenterPage() {
  const [addRackOpen, setAddRackOpen] = React.useState(false)
  const [addEnclosureRackId, setAddEnclosureRackId] = React.useState<string | null>(null)
  const [deleteEnclosurePending, setDeleteEnclosurePending] = React.useState<{ id: string; label: string } | null>(null)
  const [activeDrag, setActiveDrag] = React.useState<DragData | null>(null)
  const [cutState, setCutState] = React.useState<CutState | null>(null)
  const [unassignPending, setUnassignPending] = React.useState<UnassignPending | null>(null)

  // Fetch racks with both positions (rack-direct) and enclosures (chassis-resident).
  const racksQuery = useQuery<ListRacksResponse>({
    queryKey: ["racks"],
    queryFn: () => apiFetch<ListRacksResponse>("/api/v1/racks?include=positions,enclosures"),
    refetchInterval: 15_000,
  })

  const nodesQuery = useQuery<ListNodesResponse>({
    queryKey: ["nodes-health-dc"],
    queryFn: () => apiFetch<ListNodesResponse>("/api/v1/cluster/health"),
    refetchInterval: 15_000,
  })

  const qc = useQueryClient()

  const racks = racksQuery.data?.racks ?? []

  const nodeMap = React.useMemo(() => {
    const m = new Map<string, NodeHealth>()
    for (const n of (nodesQuery.data?.nodes ?? [])) {
      m.set(n.id, n)
    }
    return m
  }, [nodesQuery.data])

  // Build a flat map of rackId → positions for cross-rack validation
  const allRackPositions = React.useMemo(() => {
    const m = new Map<string, NodeRackPosition[]>()
    for (const rack of racks) {
      m.set(rack.id, rack.positions ?? [])
    }
    return m
  }, [racks])

  // Populate the static enclosureTypeMeta lookup from the first racks response
  // (to avoid an additional query; the catalog is also fetched in AddEnclosureModal).
  React.useEffect(() => {
    racks.forEach(rack => {
      (rack.enclosures ?? []).forEach(enc => {
        // Hydrate the type meta map from slot count embedded in the enclosure.
        if (!enclosureTypeMeta[enc.type_id]) {
          enclosureTypeMeta[enc.type_id] = {
            id: enc.type_id,
            display_name: enc.type_id,
            height_u: enc.height_u,
            slot_count: enc.slots?.length ?? 0,
            orientation: "horizontal",
            description: "",
          }
        }
      })
    })
  }, [racks])

  const setPositionMut = useMutation({
    mutationFn: ({ nodeId, rackId, slotU, heightU }: { nodeId: string; rackId: string; slotU: number; heightU: number }) =>
      apiFetch(`/api/v1/racks/${rackId}/positions/${nodeId}`, {
        method: "PUT",
        body: JSON.stringify({ slot_u: slotU, height_u: heightU }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["racks"] })
      qc.invalidateQueries({ queryKey: ["nodes-unassigned"] })
    },
    onError: (e: Error) => {
      toast({ title: "Failed to move node", description: e.message, variant: "destructive" })
    },
  })

  // Unified placement mutation — used for enclosure-slot drops.
  const placementMut = useMutation({
    mutationFn: ({ nodeId, enclosureId, slotIndex }: { nodeId: string; enclosureId: string; slotIndex: number }) =>
      apiFetch(`/api/v1/nodes/${nodeId}/placement`, {
        method: "PUT",
        body: JSON.stringify({ kind: "enclosure_slot", enclosure_id: enclosureId, slot_index: slotIndex }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["racks"] })
      qc.invalidateQueries({ queryKey: ["nodes-unassigned"] })
    },
    onError: (e: Error) => {
      toast({ title: "Failed to place node in enclosure slot", description: e.message, variant: "destructive" })
    },
  })

  const removeFromRackMut = useMutation({
    mutationFn: ({ rackId, nodeId }: { rackId: string; nodeId: string }) =>
      apiFetch(`/api/v1/racks/${rackId}/positions/${nodeId}`, { method: "DELETE" }),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: ["racks"] })
      qc.invalidateQueries({ queryKey: ["nodes-unassigned"] })
      const node = nodeMap.get(vars.nodeId)
      if (node && node.status === "provisioning") {
        toast({
          title: `Removed ${node.hostname} from rack`,
          description: "Active deploy will continue — rack assignment is physical placement only.",
        })
      }
    },
    onError: (e: Error) => {
      toast({ title: "Failed to remove node from rack", description: e.message, variant: "destructive" })
    },
  })

  // Bug #247: eject a node from an enclosure slot back to the unassigned sidebar.
  // Calls DELETE /api/v1/enclosures/:id/slots/:slot_index.
  const ejectSlotMut = useMutation({
    mutationFn: ({ enclosureId, slotIndex }: { enclosureId: string; slotIndex: number }) =>
      apiFetch(`/api/v1/enclosures/${enclosureId}/slots/${slotIndex}`, { method: "DELETE" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["racks"] })
      qc.invalidateQueries({ queryKey: ["nodes-unassigned"] })
    },
    onError: (e: Error) => {
      toast({ title: "Failed to eject node from slot", description: e.message, variant: "destructive" })
    },
  })

  function handleEjectSlot(enclosureId: string, slotIndex: number, hostname: string) {
    ejectSlotMut.mutate({ enclosureId, slotIndex })
    toast({ title: `Ejected ${hostname} from slot ${slotIndex}`, description: "Node returned to unassigned sidebar." })
  }

  function handleRemoveFromRack(nodeId: string, rackId: string, hostname: string, rackName: string) {
    setUnassignPending({ nodeId, rackId, hostname, rackName })
  }

  function handleEnclosureSlotDrop(nodeId: string, enclosureId: string, slotIndex: number) {
    placementMut.mutate({ nodeId, enclosureId, slotIndex })
  }

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }))

  function handleDragStart(e: DragStartEvent) {
    setActiveDrag((e.active.data.current as DragData) ?? null)
    setCutState(null)
  }

  function handleDragEnd(e: DragEndEvent) {
    setActiveDrag(null)
    const { active, over } = e
    if (!over) return

    const overData = over.data.current as DropData | undefined
    if (!overData) return

    const activeData = active.data.current as DragData | undefined
    if (!activeData) return

    // Enclosure slot drop.
    if (overData.kind === "enclosure") {
      placementMut.mutate({
        nodeId: activeData.nodeId,
        enclosureId: overData.enclosureId,
        slotIndex: overData.slotIndex,
      })
      return
    }

    // Rack-direct drop (existing path).
    const dstRackId = overData.rackId
    const dstSlotU = overData.slotU
    const dragHeightU = activeData.heightU
    const srcRackId = (activeData.fromUnassigned || (activeData as InEnclosureDragData).fromEnclosure)
      ? null
      : (activeData as InRackDragData).rackId

    const check = validateDrop({
      targetRackId: dstRackId,
      targetSlotU: dstSlotU,
      dragHeightU,
      sourceNodeId: activeData.nodeId,
      allRackPositions,
      racks,
    })
    if (!check.ok) {
      toast({ title: "Cannot place node", description: check.reason, variant: "destructive" })
      return
    }

    if (activeData.fromUnassigned || (activeData as InEnclosureDragData).fromEnclosure) {
      setPositionMut.mutate({ nodeId: activeData.nodeId, rackId: dstRackId, slotU: dstSlotU, heightU: dragHeightU })
    } else if (srcRackId !== dstRackId) {
      setPositionMut.mutate({ nodeId: activeData.nodeId, rackId: dstRackId, slotU: dstSlotU, heightU: dragHeightU })
    } else {
      if (dstSlotU === (activeData as InRackDragData).slotU) return
      setPositionMut.mutate({ nodeId: activeData.nodeId, rackId: dstRackId, slotU: dstSlotU, heightU: dragHeightU })
    }
  }

  function handleSlotPaste(dstRackId: string, dstSlotU: number, _rackHeightU: number, _rackPositions: NodeRackPosition[]) {
    if (!cutState) return

    const check = validateDrop({
      targetRackId: dstRackId,
      targetSlotU: dstSlotU,
      dragHeightU: cutState.heightU,
      sourceNodeId: cutState.nodeId,
      allRackPositions,
      racks,
    })
    if (!check.ok) {
      toast({ title: "Cannot place node", description: check.reason, variant: "destructive" })
      return
    }

    setPositionMut.mutate({
      nodeId: cutState.nodeId,
      rackId: dstRackId,
      slotU: dstSlotU,
      heightU: cutState.heightU,
    })
    setCutState(null)
  }

  const dragCtxValue: DragCtx = {
    activeDrag,
    cutState,
    setCutState,
    onSlotPaste: handleSlotPaste,
    onRemoveFromRack: handleRemoveFromRack,
    allRackPositions,
    onEnclosureSlotDrop: handleEnclosureSlotDrop,
  }

  const loading = racksQuery.isPending
  const error = racksQuery.isError

  return (
    <SectionErrorBoundary section="Datacenter">
      <div className="p-6 max-w-[1600px] mx-auto">
        {/* Header */}
        <div className="flex items-center justify-between mb-6">
          <div className="flex items-center gap-3">
            <Building2 className="h-5 w-5 text-muted-foreground" />
            <h1 className="text-lg font-semibold">Datacenter</h1>
            {racks.length > 0 && (
              <span className="text-xs text-muted-foreground">
                {racks.length} rack{racks.length !== 1 ? "s" : ""}
              </span>
            )}
          </div>
          <Button
            size="sm"
            className="gap-2 text-xs"
            onClick={() => setAddRackOpen(true)}
          >
            <Plus className="h-3.5 w-3.5" />
            Add rack
          </Button>
        </div>

        {loading ? (
          <div className="space-y-2">
            {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-10 w-full" />)}
          </div>
        ) : error ? (
          <div className="flex flex-col items-center py-12 text-muted-foreground gap-2">
            <XCircle className="h-8 w-8 opacity-40" />
            <p className="text-sm">Failed to load racks.</p>
          </div>
        ) : racks.length === 0 ? (
          /* Empty state — zero racks */
          <div className="flex flex-col items-center py-24 text-muted-foreground gap-4">
            <Building2 className="h-16 w-16 opacity-15" />
            <p className="text-lg font-medium text-foreground">No racks defined</p>
            <p className="text-sm text-center max-w-xs">
              Create your first rack to start mapping physical node positions.
            </p>
            <Button onClick={() => setAddRackOpen(true)} className="gap-2">
              <Plus className="h-4 w-4" />
              Create your first rack
            </Button>
          </div>
        ) : (
          /* Multi-rack tile layout — single DndContext spans sidebar + all tiles */
          <DragContext.Provider value={dragCtxValue}>
            <DndContext
              sensors={sensors}
              collisionDetection={closestCenter}
              onDragStart={handleDragStart}
              onDragEnd={handleDragEnd}
            >
              <div className="flex gap-6">
                {/* Unassigned nodes sidebar */}
                <UnassignedSidebar />

                {/* Rack tiles — horizontally scrollable */}
                <div className="flex-1 min-w-0 overflow-x-auto pb-4">
                  <div className="flex gap-6 min-w-max">
                    {racks.map((rack) => (
                      <RackTile
                        key={rack.id}
                        rack={rack}
                        nodes={nodeMap}
                        onNodeClick={(nodeId) => {
                          window.dispatchEvent(new CustomEvent("clustr:open-node", { detail: { nodeId } }))
                        }}
                        onResize={(nodeId, rackId, newHeightU) => {
                          const pos = (rack.positions ?? []).find(p => p.node_id === nodeId)
                          if (!pos) return
                          setPositionMut.mutate({ nodeId, rackId, slotU: pos.slot_u ?? 1, heightU: newHeightU })
                        }}
                        onAddEnclosure={(rackId) => setAddEnclosureRackId(rackId)}
                        onDeleteEnclosure={(encId, label) => setDeleteEnclosurePending({ id: encId, label })}
                        onEjectSlot={handleEjectSlot}
                      />
                    ))}
                  </div>
                </div>
              </div>

              {/* Drag overlay — follows cursor */}
              <DragOverlay>
                {activeDrag && (
                  <div
                    className="rounded border border-accent bg-secondary/90 px-3 text-xs font-mono shadow-lg"
                    style={{ height: activeDrag.heightU * SLOT_HEIGHT_PX - 2, display: "flex", alignItems: "center" }}
                  >
                    <Server className="h-3 w-3 mr-2 shrink-0" />
                    {activeDrag.heightU}U
                  </div>
                )}
              </DragOverlay>
            </DndContext>
          </DragContext.Provider>
        )}

        {addRackOpen && (
          <AddRackModal
            onClose={() => setAddRackOpen(false)}
            onCreated={() => setAddRackOpen(false)}
          />
        )}

        {addEnclosureRackId && (
          <AddEnclosureModal
            rackId={addEnclosureRackId}
            onClose={() => setAddEnclosureRackId(null)}
          />
        )}

        {deleteEnclosurePending && (
          <DeleteEnclosureModal
            enclosureId={deleteEnclosurePending.id}
            label={deleteEnclosurePending.label}
            onClose={() => setDeleteEnclosurePending(null)}
          />
        )}

        {unassignPending && (
          <UnassignConfirmModal
            pending={unassignPending}
            isPending={removeFromRackMut.isPending}
            onClose={() => setUnassignPending(null)}
            onConfirm={() => {
              removeFromRackMut.mutate(
                { rackId: unassignPending.rackId, nodeId: unassignPending.nodeId },
                { onSettled: () => setUnassignPending(null) },
              )
            }}
          />
        )}
      </div>
    </SectionErrorBoundary>
  )
}
