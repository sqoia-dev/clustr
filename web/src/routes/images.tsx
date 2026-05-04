import * as React from "react"
import { useNavigate, useSearch } from "@tanstack/react-router"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { formatDistanceToNow } from "date-fns"
import { Search, ChevronUp, ChevronDown, ChevronsUpDown, Copy, Check, Plus, Trash2, AlertTriangle, Upload, Link, Layers, Terminal, FolderOpen, HardDrive, Pencil, Info, PackageCheck } from "lucide-react"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs"
import {
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "@/components/ui/table"
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from "@/components/ui/sheet"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Skeleton } from "@/components/ui/skeleton"
import { StatusDot } from "@/components/StatusDot"
import { apiFetch } from "@/lib/api"
import type { BaseImage, Bundle, ImageEvent, InstallInstruction, InstallInstructionOpcode, ListBundlesResponse, ListImagesResponse, ListLocalFilesResponse, LocalFileInfo, ReconcileResult } from "@/lib/types"
import { cn } from "@/lib/utils"
import { useEventSubscription } from "@/contexts/connection"
import { toast } from "@/hooks/use-toast"
import * as tus from "tus-js-client"
import { ImageShell } from "@/components/ImageShell"

interface ImageSearch {
  q?: string
  tab?: string
  sort?: string
  dir?: "asc" | "desc"
  addImage?: string
}

function imageStateLabel(status: string): string {
  switch (status) {
    case "ready": return "ready"
    case "building": return "building"
    case "error": return "error"
    case "archived": return "archived"
    case "interrupted": return "interrupted"
    case "corrupt": return "corrupt"
    case "blob_missing": return "blob missing"
    default: return status
  }
}

function imageState(status: string): "healthy" | "warning" | "error" | "neutral" | "pending" {
  switch (status) {
    case "ready": return "healthy"
    case "building": return "pending"
    case "error": return "error"
    case "archived": return "neutral"
    case "corrupt": return "error"
    case "blob_missing": return "warning"
    default: return "neutral"
  }
}

// ImageStatusBadge renders a coloured inline badge for non-ready statuses.
function ImageStatusBadge({ status }: { status: string }) {
  if (status === "ready") return null
  const colorClass =
    status === "corrupt" ? "bg-red-100 text-red-800 border border-red-300" :
    status === "blob_missing" ? "bg-yellow-100 text-yellow-800 border border-yellow-300" :
    status === "error" ? "bg-orange-100 text-orange-800 border border-orange-300" :
    "bg-gray-100 text-gray-700 border border-gray-300"
  return (
    <span className={`ml-2 inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium ${colorClass}`}>
      {imageStateLabel(status)}
    </span>
  )
}

// ReconcilePanel shows the detailed checks from a reconcile result.
function ReconcilePanel({ result, onClose }: { result: ReconcileResult; onClose: () => void }) {
  const outcomeColor =
    result.outcome === "ok" || result.outcome === "healed" || result.outcome === "re_finalized"
      ? "text-green-700" : "text-red-700"

  return (
    <div className="mt-3 p-3 bg-gray-50 border border-gray-200 rounded-md text-sm space-y-2">
      <div className="flex items-center justify-between">
        <span className="font-medium">Reconcile result</span>
        <button onClick={onClose} className="text-gray-400 hover:text-gray-600 text-xs">dismiss</button>
      </div>
      <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
        <span className="text-gray-500">Outcome</span>
        <span className={`font-mono font-medium ${outcomeColor}`}>{result.outcome}</span>
        <span className="text-gray-500">Status</span>
        <span className="font-mono">
          {result.previous_status !== result.new_status
            ? `${result.previous_status} → ${result.new_status}`
            : result.new_status}
        </span>
        {result.error_detail && <>
          <span className="text-gray-500">Detail</span>
          <span className="font-mono break-all">{result.error_detail}</span>
        </>}
        {result.checks && <>
          <span className="text-gray-500">Blob exists</span>
          <span className="font-mono">{String(result.checks.blob_exists)}</span>
          <span className="text-gray-500">SHA on disk</span>
          <span className="font-mono break-all">{result.checks.sha_on_disk || "—"}</span>
          <span className="text-gray-500">SHA in DB</span>
          <span className="font-mono break-all">{result.checks.sha_in_db || "—"}</span>
          {result.checks.sha_in_metadata && <>
            <span className="text-gray-500">SHA in metadata</span>
            <span className="font-mono break-all">{result.checks.sha_in_metadata}</span>
          </>}
          <span className="text-gray-500">Size on disk</span>
          <span className="font-mono">{result.checks.size_on_disk.toLocaleString()} B</span>
          <span className="text-gray-500">Size in DB</span>
          <span className="font-mono">{result.checks.size_in_db.toLocaleString()} B</span>
          <span className="text-gray-500">Path resolution</span>
          <span className="font-mono">{result.checks.blob_path_resolution}</span>
        </>}
        {result.actions_taken?.length > 0 && <>
          <span className="text-gray-500">Actions</span>
          <span className="font-mono">{result.actions_taken.join(", ")}</span>
        </>}
        {result.audit_id && <>
          <span className="text-gray-500">Audit ID</span>
          <span className="font-mono">{result.audit_id}</span>
        </>}
      </div>
    </div>
  )
}

function formatBytes(bytes: number): string {
  if (!bytes) return "—"
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`
}

export function ImagesPage() {
  const navigate = useNavigate()
  const search = useSearch({ strict: false }) as ImageSearch

  const q = search.q ?? ""
  const tab = search.tab ?? "base"
  const sortCol = search.sort ?? ""
  const sortDir = search.dir ?? "asc"
  const [advanced, setAdvanced] = React.useState(false)
  const [selectedImage, setSelectedImage] = React.useState<BaseImage | null>(null)
  const [addImageOpen, setAddImageOpen] = React.useState(false)
  const [buildInitramfsOpen, setBuildInitramfsOpen] = React.useState(false)
  // Bug #247: re-attach to in-progress build from the images table.
  const [viewProgressImage, setViewProgressImage] = React.useState<BaseImage | null>(null)

  // IMG-URL-6: auto-open AddImageSheet from URL param (used by Cmd-K "Add image from URL…").
  React.useEffect(() => {
    if (search.addImage === "1") {
      setAddImageOpen(true)
      navigate({
        to: "/images",
        search: { q: q || undefined, tab: tab === "base" ? undefined : tab, sort: sortCol || undefined, dir: sortDir === "asc" ? undefined : "desc", addImage: undefined },
        replace: true,
      })
    }
  }, [search.addImage]) // eslint-disable-line react-hooks/exhaustive-deps

  function updateSearch(patch: Partial<ImageSearch>) {
    navigate({
      to: "/images",
      search: {
        q: patch.q !== undefined ? patch.q : q || undefined,
        tab: patch.tab !== undefined ? patch.tab : tab === "base" ? undefined : tab,
        sort: patch.sort !== undefined ? patch.sort : sortCol || undefined,
        dir: patch.dir !== undefined ? patch.dir : sortDir === "asc" ? undefined : "desc",
        addImage: undefined,
      },
      replace: true,
    })
  }

  const queryClient = useQueryClient()
  const imageQueryKey = ["images", q, sortCol, sortDir]

  const { data, isLoading: imagesLoading, isError: imagesError } = useQuery<ListImagesResponse>({
    queryKey: imageQueryKey,
    queryFn: () => {
      const params = new URLSearchParams()
      if (q) params.set("search", q)
      if (sortCol) params.set("sort", sortCol)
      if (sortDir) params.set("dir", sortDir)
      return apiFetch<ListImagesResponse>(`/api/v1/images?${params}`)
    },
    // SSE-2: No polling — SSE events trigger targeted invalidation instead.
    staleTime: Infinity,
  })

  // UX-4: Subscribe to image lifecycle events via the multiplexed /api/v1/events stream.
  // Replaces the per-page useSSE("/api/v1/images/events") subscription.
  useEventSubscription<ImageEvent>("images", (event) => {
    if (event.kind === "image.deleted") {
      // Remove deleted image from cache immediately, then refetch list.
      queryClient.setQueryData<ListImagesResponse>(imageQueryKey, (old) => {
        if (!old) return old
        return { ...old, images: old.images.filter((img) => img.id !== event.id) }
      })
    } else if (event.image) {
      // Update or insert the changed image in the cached list.
      queryClient.setQueryData<ListImagesResponse>(imageQueryKey, (old) => {
        if (!old) {
          queryClient.invalidateQueries({ queryKey: imageQueryKey })
          return old
        }
        const exists = old.images.some((img) => img.id === event.id)
        if (exists) {
          return {
            ...old,
            images: old.images.map((img) =>
              img.id === event.id ? (event.image as BaseImage) : img
            ),
          }
        }
        // New image — prepend and bump total.
        return { ...old, images: [event.image as BaseImage, ...old.images], total: old.total + 1 }
      })
    }
  })

  const allImages = data?.images ?? []
  // Base Images: anything that is not an initramfs artifact.
  const baseImages = allImages.filter((img) => img.build_method !== "initramfs")
  // Initramfs: images built by the initramfs build pipeline.
  const initramfsImages = allImages.filter((img) => img.build_method === "initramfs")

  // Bundles — all slurm builds from the slurm_builds table (DB-backed).
  const { data: bundlesData } = useQuery<ListBundlesResponse>({
    queryKey: ["bundles"],
    queryFn: () => apiFetch<ListBundlesResponse>("/api/v1/bundles"),
    staleTime: 30_000,
  })
  const bundles = bundlesData?.bundles ?? []

  // Delete bundle state.
  const [deletingBundleId, setDeletingBundleId] = React.useState<string | null>(null)
  const [deleteConfirmBundle, setDeleteConfirmBundle] = React.useState<Bundle | null>(null)
  const [deleteConfirmName, setDeleteConfirmName] = React.useState("")
  const [deleteForce, setDeleteForce] = React.useState(false)

  const deleteBundleMutation = useMutation({
    mutationFn: ({ id, force }: { id: string; force: boolean }) =>
      apiFetch(`/api/v1/bundles/${id}${force ? "?force=true" : ""}`, { method: "DELETE" }),
    onMutate: ({ id }) => setDeletingBundleId(id),
    onSettled: () => {
      setDeletingBundleId(null)
      setDeleteConfirmBundle(null)
      setDeleteConfirmName("")
      setDeleteForce(false)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["bundles"] })
      toast({ title: "Bundle deleted" })
    },
    onError: (err: Error) => {
      toast({ title: "Delete failed", description: err.message, variant: "destructive" })
    },
  })

  function handleDeleteBundle(bundle: Bundle) {
    setDeleteConfirmBundle(bundle)
    setDeleteConfirmName("")
    setDeleteForce(false)
  }

  function handleDeleteConfirm() {
    if (!deleteConfirmBundle) return
    deleteBundleMutation.mutate({ id: deleteConfirmBundle.id, force: deleteForce })
  }

  function handleSort(col: string) {
    if (sortCol === col) {
      updateSearch({ dir: sortDir === "asc" ? "desc" : "asc" })
    } else {
      updateSearch({ sort: col, dir: "asc" })
    }
  }

  function SortIcon({ col }: { col: string }) {
    if (sortCol !== col) return <ChevronsUpDown className="h-3 w-3 opacity-40" />
    return sortDir === "asc" ? <ChevronUp className="h-3 w-3" /> : <ChevronDown className="h-3 w-3" />
  }

  function relativeTime(iso?: string) {
    if (!iso) return "—"
    try { return formatDistanceToNow(new Date(iso), { addSuffix: true }) } catch { return "—" }
  }

  return (
    <div className="flex flex-col h-full">
      {/* Toolbar */}
      <div className="flex items-center justify-between gap-3 border-b border-border px-6 py-3">
        <div className="relative w-72">
          <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            className="pl-8"
            placeholder="Search images..."
            value={q}
            onChange={(e) => updateSearch({ q: e.target.value || undefined })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" onClick={() => setAddImageOpen(true)}>
            <Plus className="h-4 w-4 mr-1" />
            Add Image
          </Button>
          {/* INITRD-3: Build Initramfs button — lives on the Initramfs tab */}
          {tab === "initramfs" && (
            <Button size="sm" variant="outline" onClick={() => setBuildInitramfsOpen(true)}>
              <Layers className="h-4 w-4 mr-1" />
              Build Initramfs
            </Button>
          )}
          <Button
            variant="outline"
            size="sm"
            onClick={() => setAdvanced((a) => !a)}
            className={cn(advanced && "bg-secondary")}
          >
            {advanced ? "Basic view" : "Advanced"}
          </Button>
        </div>
      </div>

      {/* Tabs */}
      <div className="flex-1 overflow-auto">
        <Tabs
          value={tab}
          onValueChange={(v) => updateSearch({ tab: v === "base" ? undefined : v })}
          className="flex flex-col h-full"
        >
          <div className="px-6 pt-3 border-b border-border shrink-0">
            <TabsList>
              <TabsTrigger value="base">Base Images ({baseImages.length})</TabsTrigger>
              <TabsTrigger value="bundles">Bundles ({bundles.length})</TabsTrigger>
              <TabsTrigger value="initramfs">Initramfs ({initramfsImages.length})</TabsTrigger>
            </TabsList>
          </div>

          {/* Base Images tab */}
          <TabsContent value="base" className="flex-1 overflow-auto mt-0">
            {imagesLoading ? (
              <div className="p-4 space-y-2">
                {Array.from({ length: 4 }).map((_, i) => (
                  <div key={i} className="h-10 w-full rounded bg-secondary/40 animate-pulse" />
                ))}
              </div>
            ) : imagesError ? (
              <div className="flex items-center justify-center h-40">
                <p className="text-sm text-destructive">Failed to load images. Reload to retry.</p>
              </div>
            ) : baseImages.length === 0 ? (
              <BaseImagesEmptyState onAddImage={() => setAddImageOpen(true)} />
            ) : (
              <ImageTable
                images={baseImages}
                advanced={advanced}
                onSelect={setSelectedImage}
                onViewProgress={setViewProgressImage}
                handleSort={handleSort}
                SortIcon={SortIcon}
                relativeTime={relativeTime}
              />
            )}
          </TabsContent>

          {/* Bundles tab — unified slurm build catalog */}
          <TabsContent value="bundles" className="flex-1 overflow-auto mt-0">
            <BundlesIntroCard />
            {bundles.length === 0 ? (
              <BundlesEmptyState />
            ) : (
              <BundlesTable
                bundles={bundles}
                onDelete={handleDeleteBundle}
                deletingId={deletingBundleId}
              />
            )}
          </TabsContent>

          {/* Initramfs tab — shows built initramfs artifacts */}
          <TabsContent value="initramfs" className="flex-1 overflow-auto mt-0">
            {imagesLoading ? (
              <div className="p-4 space-y-2">
                {Array.from({ length: 4 }).map((_, i) => (
                  <div key={i} className="h-10 w-full rounded bg-secondary/40 animate-pulse" />
                ))}
              </div>
            ) : imagesError ? (
              <div className="flex items-center justify-center h-40">
                <p className="text-sm text-destructive">Failed to load images. Reload to retry.</p>
              </div>
            ) : initramfsImages.length === 0 ? (
              <InitramfsEmptyState onBuild={() => setBuildInitramfsOpen(true)} />
            ) : (
              <ImageTable
                images={initramfsImages}
                advanced={advanced}
                onSelect={setSelectedImage}
                onViewProgress={setViewProgressImage}
                handleSort={handleSort}
                SortIcon={SortIcon}
                relativeTime={relativeTime}
              />
            )}
          </TabsContent>
        </Tabs>
      </div>

      {selectedImage && (
        <ImageSheet image={selectedImage} onClose={() => setSelectedImage(null)} relativeTime={relativeTime} />
      )}

      {/* Bug #247: re-attach to an in-progress build from the images table */}
      {viewProgressImage && (
        <Dialog open onOpenChange={(v) => { if (!v) setViewProgressImage(null) }}>
          <DialogContent className="max-w-lg">
            <DialogHeader>
              <DialogTitle className="flex items-center gap-2 text-sm">
                <Terminal className="h-4 w-4 text-muted-foreground" />
                Build progress — {viewProgressImage.name}
              </DialogTitle>
            </DialogHeader>
            <BuildProgressPanel
              imageId={viewProgressImage.id}
              url={viewProgressImage.source_url ?? ""}
              onClose={() => setViewProgressImage(null)}
            />
          </DialogContent>
        </Dialog>
      )}

      {/* IMG-URL-4 / IMG-ISO-4: Add Image sheet */}
      <AddImageSheet open={addImageOpen} onClose={() => setAddImageOpen(false)} />
      {/* INITRD-3..5: Build Initramfs sheet */}
      <BuildInitramfsSheet
        open={buildInitramfsOpen}
        onClose={() => setBuildInitramfsOpen(false)}
        images={allImages.filter((img) => img.status === "ready")}
      />

      {/* Bundle delete confirmation dialog */}
      <Dialog
        open={deleteConfirmBundle !== null}
        onOpenChange={(v) => {
          if (!v) {
            setDeleteConfirmBundle(null)
            setDeleteConfirmName("")
            setDeleteForce(false)
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete bundle</DialogTitle>
          </DialogHeader>
          <div className="space-y-4 pt-1">
            <div className="rounded-md border border-destructive/30 bg-destructive/5 p-3 flex items-start gap-2 text-sm text-destructive">
              <AlertTriangle className="h-4 w-4 shrink-0 mt-0.5" />
              <span>
                This removes the bundle record and its artifact files. This cannot be undone.
              </span>
            </div>

            {deleteConfirmBundle && deleteConfirmBundle.nodes_using > 0 && (
              <div className="rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-400 space-y-2">
                <div className="flex items-center gap-2 font-medium">
                  <AlertTriangle className="h-4 w-4 shrink-0" />
                  {deleteConfirmBundle.nodes_using} node{deleteConfirmBundle.nodes_using !== 1 ? "s" : ""} currently using this bundle
                </div>
                <label className="flex items-center gap-2 cursor-pointer">
                  <input
                    type="checkbox"
                    checked={deleteForce}
                    onChange={(e) => setDeleteForce(e.target.checked)}
                    className="rounded border-border"
                  />
                  <span>Force delete (nodes will lose version tracking)</span>
                </label>
              </div>
            )}

            <div className="space-y-1">
              <p className="text-sm text-muted-foreground">
                Type <code className="font-mono text-xs">{deleteConfirmBundle?.name}</code> to confirm:
              </p>
              <Input
                className="font-mono text-sm"
                placeholder={deleteConfirmBundle?.name ?? ""}
                value={deleteConfirmName}
                onChange={(e) => setDeleteConfirmName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && deleteConfirmName === deleteConfirmBundle?.name) {
                    if (!deleteConfirmBundle?.nodes_using || deleteForce) handleDeleteConfirm()
                  }
                }}
              />
            </div>

            <div className="flex gap-2 justify-end">
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  setDeleteConfirmBundle(null)
                  setDeleteConfirmName("")
                  setDeleteForce(false)
                }}
              >
                Cancel
              </Button>
              <Button
                variant="destructive"
                size="sm"
                disabled={
                  deleteConfirmName !== deleteConfirmBundle?.name ||
                  deleteBundleMutation.isPending ||
                  (!!deleteConfirmBundle?.nodes_using && !deleteForce)
                }
                onClick={handleDeleteConfirm}
              >
                {deleteBundleMutation.isPending ? "Deleting…" : "Delete bundle"}
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// ─── AddImageSheet ────────────────────────────────────────────────────────────
// Sprint 4: IMG-URL-4..5 (URL tab) + IMG-ISO-4..7 (Upload tab)

interface AddImageSheetProps {
  open: boolean
  onClose: () => void
}

export function AddImageSheet({ open, onClose }: AddImageSheetProps) {
  const [tab, setTab] = React.useState<"url" | "upload" | "filesystem">("url")

  function handleClose() {
    setTab("url")
    onClose()
  }

  return (
    <Sheet open={open} onOpenChange={(v) => !v && handleClose()}>
      <SheetContent side="right" className="w-full sm:max-w-lg overflow-y-auto">
        <SheetHeader>
          <SheetTitle>Add Image</SheetTitle>
          <SheetDescription>Download from a URL, upload an ISO, or use a file already on the server.</SheetDescription>
        </SheetHeader>
        <div className="mt-6">
          <Tabs value={tab} onValueChange={(v) => setTab(v as "url" | "upload" | "filesystem")}>
            <TabsList className="w-full">
              <TabsTrigger value="url" className="flex-1">
                <Link className="h-3.5 w-3.5 mr-1.5" />
                From URL
              </TabsTrigger>
              <TabsTrigger value="upload" className="flex-1">
                <Upload className="h-3.5 w-3.5 mr-1.5" />
                Upload ISO
              </TabsTrigger>
              <TabsTrigger value="filesystem" className="flex-1">
                <FolderOpen className="h-3.5 w-3.5 mr-1.5" />
                Server files
              </TabsTrigger>
            </TabsList>
            <TabsContent value="url" className="mt-4">
              <AddImageFromURL onSuccess={handleClose} />
            </TabsContent>
            <TabsContent value="upload" className="mt-4">
              <AddImageFromISO onSuccess={handleClose} />
            </TabsContent>
            <TabsContent value="filesystem" className="mt-4">
              <AddImageFromFilesystem onSuccess={handleClose} />
            </TabsContent>
          </Tabs>
        </div>
      </SheetContent>
    </Sheet>
  )
}

// ─── BuildProgressPanel ───────────────────────────────────────────────────────
// Live progress panel for ISO download + QEMU install via build-progress SSE.

interface BuildProgressState {
  phase: string
  bytesDown: number
  bytesTotal: number   // -1 when Content-Length was absent
  serialLines: string[]
  errorMsg: string
}

const DOWNLOAD_PHASES = new Set(["downloading_iso", "downloading"])
const BUILD_PHASES = new Set([
  "generating_config", "creating_disk", "launching_vm",
  "installing", "extracting", "scrubbing", "finalizing",
])

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

interface BuildProgressPanelProps {
  imageId: string
  /** Source URL displayed in the panel header. Optional when re-attaching from the images table. */
  url?: string
  onClose: () => void
}

function BuildProgressPanel({ imageId, url, onClose }: BuildProgressPanelProps) {
  const [ps, setPs] = React.useState<BuildProgressState>({
    phase: "downloading_iso",
    bytesDown: 0,
    bytesTotal: -1,
    serialLines: [],
    errorMsg: "",
  })

  // ETA estimation: keep a ring of (timestamp, bytesDown) samples.
  const etaSamplesRef = React.useRef<Array<{ ts: number; bytes: number }>>([])

  // Serial log: auto-scroll unless user has scrolled up.
  const logEndRef = React.useRef<HTMLDivElement | null>(null)
  const logContainerRef = React.useRef<HTMLDivElement | null>(null)
  const userScrolledRef = React.useRef(false)

  React.useEffect(() => {
    if (!userScrolledRef.current) {
      logEndRef.current?.scrollIntoView({ behavior: "smooth" })
    }
  }, [ps.serialLines])

  // Open the SSE stream for this image's build progress.
  React.useEffect(() => {
    const es = new EventSource(`/api/v1/images/${imageId}/build-progress/stream`, { withCredentials: true })

    // The server sends an initial "snapshot" event with the full BuildState,
    // then incremental "message" events (default event type) with BuildEvent.
    // On snapshot: REPLACE state entirely — the server's BuildState is the
    // authoritative current truth for reconnect-mid-build scenarios.
    // bytes_total=0 from the server means Content-Length was unknown; map it
    // to -1 (the web sentinel for "no total") so the progress bar renders
    // correctly instead of showing "0 B / [nothing]".
    es.addEventListener("snapshot", (e: MessageEvent) => {
      try {
        const snap = JSON.parse(e.data)
        setPs({
          phase:       snap.phase        || "downloading_iso",
          bytesDown:   snap.bytes_done   || 0,
          bytesTotal:  snap.bytes_total  > 0 ? snap.bytes_total : -1,
          serialLines: Array.isArray(snap.serial_tail) ? snap.serial_tail : [],
          errorMsg:    snap.error_message || "",
        })
      } catch { /* ignore malformed snapshot */ }
    })

    es.onmessage = (e: MessageEvent) => {
      try {
        const ev = JSON.parse(e.data)
        setPs((prev) => {
          const next = { ...prev }

          if (ev.phase) {
            next.phase = ev.phase
            if (ev.phase === "failed") {
              next.errorMsg = ev.error ?? "Build failed"
            }
          }
          if (ev.bytes_done !== undefined && ev.bytes_done > 0) {
            next.bytesDown = ev.bytes_done
            // Update ETA sample ring (keep last 10).
            const samples = etaSamplesRef.current
            samples.push({ ts: Date.now(), bytes: ev.bytes_done })
            if (samples.length > 10) samples.shift()
          }
          if (ev.bytes_total !== undefined) {
            next.bytesTotal = ev.bytes_total
          }
          if (ev.serial_line) {
            // Cap at 500 lines client-side to avoid unbounded memory.
            const lines = [...prev.serialLines, ev.serial_line]
            next.serialLines = lines.length > 500 ? lines.slice(lines.length - 500) : lines
          }
          return next
        })
      } catch { /* ignore malformed event */ }
    }

    es.onerror = () => {
      // SSE auto-reconnects; don't surface transient connection drops.
    }

    return () => { es.close() }
  }, [imageId])

  // Compute ETA from the last two rate samples.
  function etaString(): string | null {
    const samples = etaSamplesRef.current
    if (samples.length < 2 || ps.bytesTotal <= 0) return null
    const oldest = samples[0]
    const newest = samples[samples.length - 1]
    const elapsed = (newest.ts - oldest.ts) / 1000 // seconds
    if (elapsed <= 0) return null
    const rate = (newest.bytes - oldest.bytes) / elapsed // bytes/sec
    if (rate <= 0) return null
    const remaining = ps.bytesTotal - newest.bytes
    if (remaining <= 0) return null
    const secs = Math.round(remaining / rate)
    if (secs < 60) return `~${secs}s`
    if (secs < 3600) return `~${Math.round(secs / 60)}m`
    return `~${Math.round(secs / 3600)}h`
  }

  const pct = ps.bytesTotal > 0 ? Math.min(100, Math.round((ps.bytesDown / ps.bytesTotal) * 100)) : null
  const isDownloading = DOWNLOAD_PHASES.has(ps.phase)
  const isBuilding = BUILD_PHASES.has(ps.phase)
  const isFailed = ps.phase === "failed"
  const isDone = ps.phase === "complete"
  const eta = isDownloading ? etaString() : null

  return (
    <div className="space-y-3">
      <div className="rounded-md border border-border bg-card p-4 space-y-3">
        {/* Phase + status indicator */}
        <div className="flex items-center gap-2 text-sm">
          {isFailed ? (
            <span className="h-2 w-2 rounded-full bg-destructive shrink-0" />
          ) : isDone ? (
            <span className="h-2 w-2 rounded-full bg-green-500 shrink-0" />
          ) : (
            <span className="h-2 w-2 rounded-full bg-status-warning animate-pulse shrink-0" />
          )}
          <span className="font-medium">{phaseLabel(ps.phase)}</span>
        </div>

        {/* URL — omitted when re-attaching from the images table without a known source URL */}
        {url && <p className="text-xs text-muted-foreground font-mono break-all">{url}</p>}

        {/* Download progress bar */}
        {isDownloading && (
          <div className="space-y-1">
            <div className="h-1.5 rounded-full bg-secondary overflow-hidden">
              {pct !== null ? (
                <div
                  className="h-full bg-status-warning transition-[width] duration-300"
                  style={{ width: `${pct}%` }}
                />
              ) : (
                <div className="h-full bg-status-warning animate-pulse" style={{ width: "40%" }} />
              )}
            </div>
            <div className="flex items-center justify-between text-xs text-muted-foreground">
              <span>
                {fmtBytes(ps.bytesDown)}
                {ps.bytesTotal > 0 && ` / ${fmtBytes(ps.bytesTotal)}`}
                {pct !== null && ` (${pct}%)`}
              </span>
              {eta && <span>{eta} remaining</span>}
            </div>
          </div>
        )}

        {/* Indeterminate bar for non-download build phases */}
        {isBuilding && (
          <div className="h-1.5 rounded-full bg-secondary overflow-hidden">
            <div className="h-full bg-blue-500 animate-pulse" style={{ width: "70%" }} />
          </div>
        )}

        {/* Error message */}
        {isFailed && ps.errorMsg && (
          <p className="text-xs text-destructive font-mono break-all">{ps.errorMsg}</p>
        )}
      </div>

      {/* Serial log panel — shown during install phases */}
      {(isBuilding || isDone) && ps.serialLines.length > 0 && (
        <div className="rounded-md border border-border bg-black/90">
          <div className="px-3 py-1.5 border-b border-border/40 flex items-center gap-2">
            <Terminal className="h-3.5 w-3.5 text-muted-foreground" />
            <span className="text-xs text-muted-foreground font-medium">Serial console</span>
            <span className="ml-auto text-xs text-muted-foreground">{ps.serialLines.length} lines</span>
          </div>
          <div
            ref={logContainerRef}
            className="h-48 overflow-y-auto p-2 font-mono text-[10px] leading-relaxed text-green-400"
            onScroll={() => {
              const el = logContainerRef.current
              if (!el) return
              const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 32
              userScrolledRef.current = !atBottom
            }}
          >
            {ps.serialLines.map((line, i) => (
              <div key={i}>{line}</div>
            ))}
            <div ref={logEndRef} />
          </div>
        </div>
      )}

      <Button variant="ghost" size="sm" onClick={onClose} className="w-full">
        Close (continues in background)
      </Button>
    </div>
  )
}

// ─── AddImageFromURL ──────────────────────────────────────────────────────────
// IMG-URL-4..5

function AddImageFromURL({ onSuccess }: { onSuccess: () => void }) {
  const qc = useQueryClient()
  const [url, setUrl] = React.useState("")
  const [name, setName] = React.useState("")
  const [sha256, setSha256] = React.useState("")
  const [urlError, setUrlError] = React.useState("")
  const [progressImageId, setProgressImageId] = React.useState<string | null>(null)

  // Auto-suggest name from URL.
  React.useEffect(() => {
    if (!url) return
    try {
      const parts = url.split("/")
      const last = parts[parts.length - 1].split("?")[0]
      if (last && !name) setName(last)
    } catch { /* ignore */ }
  }, [url]) // eslint-disable-line react-hooks/exhaustive-deps

  const submitMutation = useMutation({
    mutationFn: () =>
      apiFetch<{ id: string; status: string }>("/api/v1/images/from-url", {
        method: "POST",
        body: JSON.stringify({
          url,
          name: name || undefined,
          expected_sha256: sha256 || undefined,
        }),
      }),
    onSuccess: (res) => {
      setProgressImageId(res.id)
      qc.invalidateQueries({ queryKey: ["images"] })
      toast({ title: "Download started", description: `${name || url} — downloading in background.` })
    },
    onError: (err) => {
      setUrlError(String(err))
    },
  })

  // UX-4: Watch for image download completion via the multiplexed /api/v1/events stream.
  // Replaces the per-page useSSE("/api/v1/images/events") subscription.
  useEventSubscription<ImageEvent>("images", (event) => {
    if (!progressImageId || event.id !== progressImageId) return
    if (event.kind === "image.finalized") {
      qc.invalidateQueries({ queryKey: ["images"] })
      toast({ title: "Download complete", description: name || url })
      onSuccess()
    } else if (event.image?.status === "error") {
      setUrlError(event.image.error_message || "Download failed")
      setProgressImageId(null)
    }
  })

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setUrlError("")
    if (!url) { setUrlError("URL is required"); return }
    if (!url.startsWith("http://") && !url.startsWith("https://")) {
      setUrlError("URL must use http or https")
      return
    }
    submitMutation.mutate()
  }

  // Show live progress panel once we have an image ID from the server.
  if (progressImageId) {
    return (
      <BuildProgressPanel
        imageId={progressImageId}
        url={url}
        onClose={onSuccess}
      />
    )
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <AddImageField label="URL *">
        <Input
          placeholder="https://example.com/image.iso"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          className={cn(urlError && "border-destructive")}
        />
        {urlError && <p className="text-xs text-destructive mt-1">{urlError}</p>}
      </AddImageField>
      <AddImageField label="Name (auto-filled from URL)">
        <Input
          placeholder="my-image"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </AddImageField>
      <AddImageField
        label="Expected SHA256 (optional)"
        hint="If provided, download fails if the computed hash doesn't match."
      >
        <Input
          placeholder="sha256hex…"
          value={sha256}
          onChange={(e) => setSha256(e.target.value)}
          className="font-mono text-xs"
        />
      </AddImageField>
      <div className="flex gap-2 pt-2">
        <Button type="submit" className="flex-1" disabled={submitMutation.isPending}>
          {submitMutation.isPending ? "Starting…" : "Download"}
        </Button>
        <Button type="button" variant="ghost" onClick={onSuccess}>Cancel</Button>
      </div>
    </form>
  )
}

// ─── AddImageFromISO ──────────────────────────────────────────────────────────
// IMG-ISO-4..7: TUS resumable upload

const TUS_UPLOAD_ENDPOINT = "/api/v1/uploads/"
const ISO_WARN_BYTES = 10 * 1024 * 1024 * 1024 // 10 GB
const HASH_MAX_BYTES = 2 * 1024 * 1024 * 1024 // 2 GB

function AddImageFromISO({ onSuccess }: { onSuccess: () => void }) {
  const qc = useQueryClient()
  const [file, setFile] = React.useState<File | null>(null)
  const [name, setName] = React.useState("")
  const [uploadProgress, setUploadProgress] = React.useState(0)
  const [uploading, setUploading] = React.useState(false)
  const [paused, setPaused] = React.useState(false)
  const [error, setError] = React.useState("")
  const [clientHash, setClientHash] = React.useState("")
  const [hashProgress, setHashProgress] = React.useState(0)
  const uploadRef = React.useRef<tus.Upload | null>(null)
  const inputRef = React.useRef<HTMLInputElement>(null)

  function handleFileChange(f: File) {
    setFile(f)
    setError("")
    setClientHash("")
    setHashProgress(0)
    if (!name) setName(f.name)
    // IMG-ISO-6: compute SHA256 client-side for <2GB files.
    if (f.size < HASH_MAX_BYTES) {
      computeFileSHA256(f, setHashProgress).then(setClientHash).catch(() => {/* skip */})
    }
  }

  async function computeFileSHA256(f: File, onProgress: (p: number) => void): Promise<string> {
    const buf = await f.slice(0, f.size).arrayBuffer()
    onProgress(50)
    const hashBuf = await crypto.subtle.digest("SHA-256", buf)
    onProgress(100)
    return Array.from(new Uint8Array(hashBuf)).map((b) => b.toString(16).padStart(2, "0")).join("")
  }

  function startUpload() {
    if (!file) return
    setError("")
    setUploading(true)
    setPaused(false)

    const upload = new tus.Upload(file, {
      endpoint: TUS_UPLOAD_ENDPOINT,
      retryDelays: [0, 1000, 3000, 5000],
      metadata: {
        filename: file.name,
        filetype: file.type || "application/octet-stream",
        name: name || file.name,
      },
      chunkSize: 10 * 1024 * 1024, // 10 MiB chunks
      onProgress(bytesSent, bytesTotal) {
        setUploadProgress(Math.round((bytesSent / bytesTotal) * 100))
      },
      onSuccess() {
        const uploadId = upload.url?.split("/").pop() ?? ""
        // Register the upload as an image.
        apiFetch<{ id: string }>("/api/v1/images/from-upload", {
          method: "POST",
          body: JSON.stringify({
            upload_id: uploadId,
            name: name || file.name,
            expected_sha256: clientHash || undefined,
          }),
        }).then((res) => {
          qc.invalidateQueries({ queryKey: ["images"] })
          toast({ title: "Upload complete", description: `${name || file.name} registered as image ${res.id.slice(0, 8)}` })
          onSuccess()
        }).catch((err) => {
          setError(String(err))
          setUploading(false)
        })
      },
      onError(err) {
        setError(String(err))
        setUploading(false)
        setPaused(false)
      },
    })
    uploadRef.current = upload
    upload.start()
  }

  function pauseUpload() {
    uploadRef.current?.abort()
    setPaused(true)
  }

  function resumeUpload() {
    if (!file) return
    setPaused(false)
    uploadRef.current?.start()
  }

  return (
    <div className="space-y-4">
      {/* IMG-ISO-7: warn on large files */}
      {file && file.size > ISO_WARN_BYTES && (
        <div className="flex items-start gap-2 rounded-md border border-status-warning/40 bg-status-warning/5 p-3 text-xs text-status-warning">
          <AlertTriangle className="h-4 w-4 shrink-0 mt-0.5" />
          <span>Large ISO (&gt;10 GB) — consider hosting it internally and using From URL instead.</span>
        </div>
      )}

      {/* Drop zone */}
      <div
        className={cn(
          "rounded-md border-2 border-dashed border-border p-8 text-center cursor-pointer hover:border-primary/50 transition-colors",
          file && "border-primary/30 bg-primary/5"
        )}
        onClick={() => inputRef.current?.click()}
        onDragOver={(e) => e.preventDefault()}
        onDrop={(e) => {
          e.preventDefault()
          const f = e.dataTransfer.files[0]
          if (f) handleFileChange(f)
        }}
        role="button"
        tabIndex={0}
        aria-label="Click or drag ISO file to upload"
        onKeyDown={(e) => e.key === "Enter" && inputRef.current?.click()}
      >
        <input
          ref={inputRef}
          type="file"
          className="hidden"
          accept=".iso,.img,.tar,.tar.gz,.tar.bz2,.tar.xz"
          onChange={(e) => { const f = e.target.files?.[0]; if (f) handleFileChange(f) }}
        />
        {file ? (
          <div className="space-y-1">
            <p className="text-sm font-medium">{file.name}</p>
            <p className="text-xs text-muted-foreground">{formatBytes(file.size)}</p>
            {hashProgress > 0 && hashProgress < 100 && (
              <p className="text-xs text-muted-foreground">Computing SHA256… {hashProgress}%</p>
            )}
            {clientHash && (
              <p className="text-xs text-muted-foreground font-mono">{clientHash.slice(0, 16)}…</p>
            )}
          </div>
        ) : (
          <div className="space-y-1">
            <Upload className="h-8 w-8 mx-auto text-muted-foreground" />
            <p className="text-sm text-muted-foreground">Drag and drop or click to select</p>
            <p className="text-xs text-muted-foreground">.iso, .img, .tar, .tar.gz, .tar.xz</p>
          </div>
        )}
      </div>

      {file && (
        <AddImageField label="Image name">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={file.name} />
        </AddImageField>
      )}

      {error && <p className="text-xs text-destructive">{error}</p>}

      {uploading && (
        <div className="space-y-2">
          <div className="flex items-center justify-between text-xs text-muted-foreground">
            <span>{paused ? "Paused" : "Uploading…"}</span>
            <span>{uploadProgress}%</span>
          </div>
          <div className="h-1.5 rounded-full bg-secondary overflow-hidden">
            <div
              className={cn("h-full bg-primary transition-all", paused && "opacity-50")}
              style={{ width: `${uploadProgress}%` }}
            />
          </div>
        </div>
      )}

      {file && (
        <div className="flex gap-2 pt-2">
          {!uploading || paused ? (
            <Button
              className="flex-1"
              onClick={paused ? resumeUpload : startUpload}
              disabled={!file}
            >
              {paused ? "Resume" : "Upload"}
            </Button>
          ) : (
            <Button variant="outline" className="flex-1" onClick={pauseUpload}>
              Pause
            </Button>
          )}
          <Button variant="ghost" onClick={onSuccess}>Cancel</Button>
        </div>
      )}
    </div>
  )
}

function AddImageField({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1">
      <label className="text-sm text-muted-foreground">{label}</label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

// ─── AddImageFromFilesystem ───────────────────────────────────────────────────
// ISO-FS-3..4: list and register files already on the server import dir.

function AddImageFromFilesystem({ onSuccess }: { onSuccess: () => void }) {
  const qc = useQueryClient()
  const [selectedFile, setSelectedFile] = React.useState<LocalFileInfo | null>(null)
  const [name, setName] = React.useState("")
  const [submitting, setSubmitting] = React.useState(false)
  const [error, setError] = React.useState("")

  const { data, isLoading, isError, refetch } = useQuery<ListLocalFilesResponse>({
    queryKey: ["images-local-files"],
    queryFn: () => apiFetch<ListLocalFilesResponse>("/api/v1/images/local-files"),
    staleTime: 15000,
  })

  const files = data?.files ?? []
  const importDir = data?.import_dir ?? "/var/lib/clustr/iso"

  function handleSelectFile(f: LocalFileInfo) {
    setSelectedFile(f)
    setName((prev) => prev || f.name.replace(/\.(iso|img|qcow2|raw)$/i, ""))
    setError("")
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!selectedFile) { setError("Select a file first"); return }
    if (!name.trim()) { setError("Name is required"); return }
    setSubmitting(true)
    setError("")
    try {
      await apiFetch<{ id: string }>("/api/v1/images/from-local-file", {
        method: "POST",
        body: JSON.stringify({ path: selectedFile.path, name: name.trim() }),
      })
      qc.invalidateQueries({ queryKey: ["images"] })
      toast({ title: "Image registered", description: `${name} added from server filesystem.` })
      onSuccess()
    } catch (err) {
      setError(String(err))
    } finally {
      setSubmitting(false)
    }
  }

  if (isLoading) {
    return (
      <div className="space-y-2 py-4">
        {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12 w-full" />)}
      </div>
    )
  }

  if (isError) {
    return (
      <div className="py-4 text-center space-y-2">
        <p className="text-sm text-destructive">Failed to list server files</p>
        <Button size="sm" variant="outline" onClick={() => refetch()}>Retry</Button>
      </div>
    )
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <div className="rounded-md border border-border bg-secondary/30 px-3 py-2 text-xs text-muted-foreground">
        <HardDrive className="h-3.5 w-3.5 inline mr-1.5 align-text-bottom" />
        Files in <code className="font-mono">{importDir}</code> — drop ISOs here to make them appear.
      </div>

      {files.length === 0 ? (
        <div className="py-6 text-center space-y-1">
          <p className="text-sm text-muted-foreground">No .iso, .img, .qcow2 or .raw files found</p>
          <p className="text-xs text-muted-foreground">Copy files to <code className="font-mono">{importDir}</code> on the server.</p>
          <Button size="sm" variant="outline" className="mt-2" onClick={() => refetch()}>Refresh</Button>
        </div>
      ) : (
        <div className="space-y-1 max-h-52 overflow-y-auto rounded-md border border-border">
          {files.map((f) => (
            <button
              key={f.path}
              type="button"
              className={cn(
                "w-full flex items-center gap-3 px-3 py-2.5 text-left hover:bg-secondary/40 transition-colors",
                selectedFile?.path === f.path && "bg-secondary"
              )}
              onClick={() => handleSelectFile(f)}
            >
              <FolderOpen className="h-4 w-4 text-muted-foreground shrink-0" />
              <div className="flex-1 min-w-0">
                <p className="text-sm font-mono truncate">{f.name}</p>
                <p className="text-xs text-muted-foreground">{formatBytes(f.size)} · {new Date(f.mtime).toLocaleDateString()}</p>
              </div>
              {selectedFile?.path === f.path && (
                <Check className="h-4 w-4 text-primary shrink-0" />
              )}
            </button>
          ))}
        </div>
      )}

      {selectedFile && (
        <AddImageField label="Image name *">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="rocky9-base"
          />
        </AddImageField>
      )}

      {error && <p className="text-xs text-destructive">{error}</p>}

      <div className="flex gap-2 pt-2">
        <Button type="submit" className="flex-1" disabled={!selectedFile || submitting}>
          {submitting ? "Registering…" : "Register Image"}
        </Button>
        <Button type="button" variant="ghost" onClick={onSuccess}>Cancel</Button>
      </div>
    </form>
  )
}

// ─── ImageTable ───────────────────────────────────────────────────────────────
// Shared table component for both Base Images and Initramfs tabs.

interface ImageTableProps {
  images: BaseImage[]
  advanced: boolean
  onSelect: (img: BaseImage) => void
  /** Bug #247: callback to re-attach the BuildProgressPanel for an in-progress build. */
  onViewProgress?: (img: BaseImage) => void
  handleSort: (col: string) => void
  SortIcon: (props: { col: string }) => React.ReactElement
  relativeTime: (iso?: string) => string
}

function ImageTable({ images, advanced, onSelect, onViewProgress, handleSort, SortIcon, relativeTime }: ImageTableProps) {
  return (
    <Table>
      <caption className="sr-only">Cluster images</caption>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">
            <button className="flex items-center gap-1 hover:text-foreground" onClick={() => handleSort("name")}>
              Name <SortIcon col="name" />
            </button>
          </TableHead>
          <TableHead scope="col">Status</TableHead>
          <TableHead scope="col">
            <button className="flex items-center gap-1 hover:text-foreground" onClick={() => handleSort("version")}>
              Version <SortIcon col="version" />
            </button>
          </TableHead>
          <TableHead scope="col">Size</TableHead>
          <TableHead scope="col">SHA256</TableHead>
          {advanced && <TableHead scope="col">OS / Arch</TableHead>}
          <TableHead scope="col">
            <button className="flex items-center gap-1 hover:text-foreground" onClick={() => handleSort("created_at")}>
              Created <SortIcon col="created_at" />
            </button>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {images.map((img) => (
          <TableRow key={img.id} className="cursor-pointer" onClick={() => onSelect(img)}>
            <TableCell>
              <div className="flex items-center gap-2">
                <span className="font-medium text-sm">{img.name}</span>
                <ImageStatusBadge status={img.status} />
                {/* Bug #247: "View progress" button for in-progress builds.
                    Clicking re-opens the BuildProgressPanel connected to the
                    same SSE stream — the stream sends a snapshot on connect so
                    re-attaching picks up current state immediately. */}
                {img.status === "building" && onViewProgress && (
                  <button
                    className="ml-1 inline-flex items-center gap-1 rounded border border-border bg-secondary/60 px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground hover:bg-secondary hover:text-foreground transition-colors"
                    title="Re-open build progress panel"
                    onClick={(e) => { e.stopPropagation(); onViewProgress(img) }}
                  >
                    <Terminal className="h-2.5 w-2.5" />
                    View progress
                  </button>
                )}
              </div>
            </TableCell>
            <TableCell>
              <StatusDot state={imageState(img.status)} label={imageStateLabel(img.status)} />
            </TableCell>
            <TableCell className="font-mono text-xs text-muted-foreground">
              {img.version || "—"}
            </TableCell>
            <TableCell className="text-xs text-muted-foreground">
              {formatBytes(img.size_bytes)}
            </TableCell>
            <TableCell className="font-mono text-xs text-muted-foreground">
              {img.checksum ? img.checksum.slice(0, 12) + "…" : "—"}
            </TableCell>
            {advanced && (
              <TableCell className="text-xs text-muted-foreground">
                {[img.os, img.arch].filter(Boolean).join(" / ") || "—"}
              </TableCell>
            )}
            <TableCell className="text-xs text-muted-foreground">
              {relativeTime(img.created_at)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

// ─── BundlesIntroCard ─────────────────────────────────────────────────────────
// Explains the slurm RPM catalog model so operators understand what they're
// looking at. Shown at the top of the Bundles tab regardless of content.

function BundlesIntroCard() {
  return (
    <div className="mx-6 mt-4 mb-3 rounded-md border border-border bg-card p-4 flex items-start gap-3">
      <Info className="h-4 w-4 text-muted-foreground shrink-0 mt-0.5" />
      <div className="space-y-1">
        <p className="text-sm font-medium">Slurm RPM catalog</p>
        <p className="text-xs text-muted-foreground leading-relaxed">
          Each entry here is a signed set of Slurm RPMs built by the clustr build pipeline and served
          from the cluster&apos;s internal yum repo (<code className="font-mono">clustr-internal-repo</code>).
          Nodes install slurm by running <code className="font-mono">dnf install</code> against that repo —
          no external network access required. The <strong>active</strong> bundle is what new nodes will
          receive. Non-active bundles can be deleted once all nodes have migrated off them.
        </p>
      </div>
    </div>
  )
}

// ─── BundlesTable ─────────────────────────────────────────────────────────────
// Unified table: all slurm builds from GET /api/v1/bundles (DB-backed).
// Each row shows status, signature, nodes using, last deployed, and a delete
// affordance for non-active builds.

interface BundlesTableProps {
  bundles: Bundle[]
  onDelete: (bundle: Bundle) => void
  deletingId: string | null
}

function BundleStatusBadge({ status, isActive }: { status: string; isActive: boolean }) {
  if (isActive) {
    return (
      <span className="inline-flex items-center gap-1 text-xs font-medium text-status-healthy">
        <span className="h-1.5 w-1.5 rounded-full bg-status-healthy" />
        active
      </span>
    )
  }
  if (status === "completed") {
    return <span className="text-xs text-muted-foreground">ready</span>
  }
  if (status === "running" || status === "building") {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-status-warning">
        <span className="h-1.5 w-1.5 rounded-full bg-status-warning animate-pulse" />
        building
      </span>
    )
  }
  if (status === "failed" || status === "error") {
    return <span className="text-xs text-status-error">failed</span>
  }
  return <span className="text-xs text-muted-foreground">{status}</span>
}

function SigStatusBadge({ sig }: { sig?: string }) {
  if (sig === "signed") {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-status-healthy">
        <PackageCheck className="h-3 w-3" />
        signed
      </span>
    )
  }
  if (sig === "unsigned") {
    return <span className="text-xs text-amber-600 dark:text-amber-400">unsigned</span>
  }
  return <span className="text-xs text-muted-foreground">—</span>
}

function KindBadge({ kind }: { kind: string }) {
  if (kind === "build") {
    return (
      <span className="inline-flex items-center rounded-sm border border-blue-500/30 bg-blue-500/5 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
        build pipeline
      </span>
    )
  }
  return <span className="text-xs text-muted-foreground">{kind}</span>
}

function BundlesTable({ bundles, onDelete, deletingId }: BundlesTableProps) {
  return (
    <Table>
      <caption className="sr-only">Slurm bundle catalog</caption>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">Name</TableHead>
          <TableHead scope="col">Version</TableHead>
          <TableHead scope="col">Kind</TableHead>
          <TableHead scope="col">Status</TableHead>
          <TableHead scope="col">Signature</TableHead>
          <TableHead scope="col">Nodes using</TableHead>
          <TableHead scope="col">Last deployed</TableHead>
          <TableHead scope="col">SHA256</TableHead>
          <TableHead scope="col" className="w-10" />
        </TableRow>
      </TableHeader>
      <TableBody>
        {bundles.map((b) => (
          <TableRow key={b.id}>
            <TableCell>
              <span className="font-medium text-sm font-mono">{b.name}</span>
            </TableCell>
            <TableCell className="font-mono text-xs text-muted-foreground">{b.slurm_version}</TableCell>
            <TableCell>
              <KindBadge kind={b.kind} />
            </TableCell>
            <TableCell>
              <BundleStatusBadge status={b.status} isActive={b.is_active} />
            </TableCell>
            <TableCell>
              <SigStatusBadge sig={b.sig_status} />
            </TableCell>
            <TableCell className="text-xs text-muted-foreground">
              {b.nodes_using > 0 ? b.nodes_using : "—"}
            </TableCell>
            <TableCell className="text-xs text-muted-foreground">
              {b.last_deployed_at
                ? formatDistanceToNow(new Date(b.last_deployed_at * 1000), { addSuffix: true })
                : "—"}
            </TableCell>
            <TableCell className="font-mono text-xs text-muted-foreground">
              {b.sha256 ? b.sha256.slice(0, 12) + "…" : "—"}
            </TableCell>
            <TableCell>
              {!b.is_active && (
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-7 w-7 text-muted-foreground hover:text-destructive"
                  disabled={deletingId === b.id}
                  onClick={() => onDelete(b)}
                  title="Delete bundle"
                >
                  {deletingId === b.id ? (
                    <span className="h-3.5 w-3.5 rounded-full border-2 border-current border-t-transparent animate-spin" />
                  ) : (
                    <Trash2 className="h-3.5 w-3.5" />
                  )}
                </Button>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

// ─── Empty states ─────────────────────────────────────────────────────────────

function BaseImagesEmptyState({ onAddImage }: { onAddImage?: () => void }) {
  const [copied, setCopied] = React.useState(false)
  const snippet = "clustr-serverd image upload --name myimage --version 1.0 /path/to/image.tar"

  function copy() {
    navigator.clipboard.writeText(snippet).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    })
  }

  return (
    <div className="flex flex-col items-center justify-center h-full min-h-64 gap-4 p-8 text-center">
      <div className="space-y-1">
        <h2 className="text-base font-semibold">No images yet</h2>
        <p className="text-sm text-muted-foreground">Upload a base image to get started:</p>
      </div>
      <div className="flex items-center gap-2 rounded-md border border-border bg-card px-3 py-2 max-w-xl">
        <code className="text-xs font-mono flex-1 text-left">{snippet}</code>
        <Button variant="ghost" size="icon" className="h-7 w-7 shrink-0" onClick={copy}>
          {copied ? <Check className="h-3.5 w-3.5 text-green-500" /> : <Copy className="h-3.5 w-3.5" />}
        </Button>
      </div>
      {onAddImage && (
        <Button onClick={onAddImage} size="sm">
          <Plus className="h-4 w-4 mr-1" />
          Add Image
        </Button>
      )}
    </div>
  )
}

function BundlesEmptyState() {
  const navigate = useNavigate()
  return (
    <div className="flex flex-col items-center justify-center h-full min-h-64 gap-4 p-8 text-center">
      <PackageCheck className="h-8 w-8 text-muted-foreground/40" />
      <div className="space-y-1">
        <h2 className="text-base font-semibold">No slurm bundles yet</h2>
        <p className="text-sm text-muted-foreground max-w-sm leading-relaxed">
          clustr&apos;s build pipeline turns slurm source into signed RPMs served via this cluster&apos;s
          internal yum repo. Build your first slurm version to populate this catalog — nodes will
          install from it via clientd-orchestrated dnf.
        </p>
      </div>
      <Button size="sm" onClick={() => navigate({ to: "/slurm" })}>
        Build slurm
      </Button>
    </div>
  )
}

function InitramfsEmptyState({ onBuild }: { onBuild: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center h-full min-h-64 gap-4 p-8 text-center">
      <div className="space-y-1">
        <h2 className="text-base font-semibold">No initramfs built yet</h2>
        <p className="text-sm text-muted-foreground">
          Click &apos;Build Initramfs&apos; above to create one from a base image and bundle.
        </p>
      </div>
      <Button size="sm" variant="outline" onClick={onBuild}>
        <Layers className="h-4 w-4 mr-1" />
        Build Initramfs
      </Button>
    </div>
  )
}

function ImageSheet({ image, onClose, relativeTime }: { image: BaseImage; onClose: () => void; relativeTime: (iso?: string) => string }) {
  const qc = useQueryClient()
  const [copiedSha, setCopiedSha] = React.useState(false)
  const [deleteExpanded, setDeleteExpanded] = React.useState(false)
  const [deleteConfirm, setDeleteConfirm] = React.useState("")
  const [deleteError, setDeleteError] = React.useState("")
  const [shellOpen, setShellOpen] = React.useState(false)

  // Reconcile state (#252)
  const [reconcileResult, setReconcileResult] = React.useState<ReconcileResult | null>(null)
  const reconcileMutation = useMutation({
    mutationFn: (forceReFinalize: boolean) =>
      apiFetch<ReconcileResult>(`/api/v1/images/${image.id}/reconcile`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ force_re_finalize: forceReFinalize }),
      }),
    onSuccess: (result) => {
      setReconcileResult(result)
      qc.invalidateQueries({ queryKey: ["images"] })
    },
    onError: (err) => {
      toast({ title: "Reconcile failed", description: String(err), variant: "destructive" })
    },
  })

  // Install Instructions edit state (#147)
  const [instrDialogOpen, setInstrDialogOpen] = React.useState(false)
  const [instrList, setInstrList] = React.useState<InstallInstruction[]>(image.install_instructions ?? [])
  const [instrEditIdx, setInstrEditIdx] = React.useState<number | null>(null)
  const [instrForm, setInstrForm] = React.useState<InstallInstruction>({ opcode: "overwrite", target: "", payload: "" })
  const [instrSaveError, setInstrSaveError] = React.useState("")

  const instrMutation = useMutation({
    mutationFn: (instructions: InstallInstruction[]) =>
      apiFetch<BaseImage>(`/api/v1/images/${image.id}/install-instructions`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ instructions }),
      }),
    onSuccess: (updated) => {
      qc.invalidateQueries({ queryKey: ["images"] })
      setInstrList(updated.install_instructions ?? [])
      setInstrDialogOpen(false)
      setInstrEditIdx(null)
      setInstrSaveError("")
      toast({ title: "Install instructions saved" })
    },
    onError: (err) => {
      setInstrSaveError(String(err))
    },
  })

  function openAddInstr() {
    setInstrForm({ opcode: "overwrite", target: "", payload: "" })
    setInstrEditIdx(null)
    setInstrDialogOpen(true)
    setInstrSaveError("")
  }

  function openEditInstr(idx: number) {
    setInstrForm({ ...instrList[idx] })
    setInstrEditIdx(idx)
    setInstrDialogOpen(true)
    setInstrSaveError("")
  }

  function saveInstr() {
    if (!instrForm.target.trim()) {
      setInstrSaveError("Target path is required")
      return
    }
    const updated = instrEditIdx === null
      ? [...instrList, instrForm]
      : instrList.map((it, i) => i === instrEditIdx ? instrForm : it)
    instrMutation.mutate(updated)
  }

  function removeInstr(idx: number) {
    const updated = instrList.filter((_, i) => i !== idx)
    instrMutation.mutate(updated)
  }

  function moveInstr(idx: number, dir: -1 | 1) {
    const updated = [...instrList]
    const swap = idx + dir
    if (swap < 0 || swap >= updated.length) return
    ;[updated[idx], updated[swap]] = [updated[swap], updated[idx]]
    instrMutation.mutate(updated)
  }

  function copySHA() {
    navigator.clipboard.writeText(image.checksum).then(() => {
      setCopiedSha(true)
      setTimeout(() => setCopiedSha(false), 2000)
    })
  }

  // IMG-DEL-2/4: optimistic delete with rollback on 409.
  const deleteMutation = useMutation({
    mutationFn: () =>
      apiFetch<void>(`/api/v1/images/${image.id}`, { method: "DELETE" }),
    onMutate: async () => {
      await qc.cancelQueries({ queryKey: ["images"] })
      const prev = qc.getQueryData<ListImagesResponse>(["images"])
      // Optimistic remove.
      qc.setQueryData<ListImagesResponse>(["images"], (old) => {
        if (!old) return old
        return { ...old, images: old.images.filter((img) => img.id !== image.id) }
      })
      return { prev }
    },
    onSuccess: () => {
      toast({ title: "Image deleted", description: image.name })
      onClose()
    },
    onError: (err, _v, ctx) => {
      // Rollback.
      if (ctx?.prev) qc.setQueryData(["images"], ctx.prev)
      const msg = String(err)
      if (msg.includes("image_in_use") || msg.includes("in use") || msg.includes("409")) {
        setDeleteError("Cannot delete: image is assigned to one or more nodes. Reimage them first.")
      } else {
        setDeleteError(msg)
      }
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ["images"] })
    },
  })

  function confirmDelete() {
    if (deleteConfirm !== image.name) return
    setDeleteError("")
    deleteMutation.mutate()
  }

  return (
    <Sheet open onOpenChange={(v) => !v && onClose()}>
      <SheetContent side="right" className="w-full sm:max-w-xl overflow-y-auto">
        <SheetHeader>
          <SheetTitle>{image.name}</SheetTitle>
          <SheetDescription>
            <StatusDot state={imageState(image.status)} label={imageStateLabel(image.status)} />
          </SheetDescription>
        </SheetHeader>

        <div className="mt-6 space-y-4">
          <Section title="Identity">
            <Row label="ID" value={image.id} mono />
            <Row label="Version" value={image.version || "—"} />
            <Row label="OS" value={image.os || "—"} />
            <Row label="Arch" value={image.arch || "—"} />
            <Row label="Format" value={image.format || "—"} />
            <Row label="Firmware" value={image.firmware || "—"} />
          </Section>

          <Section title="Content">
            <Row label="Size" value={formatBytes(image.size_bytes)} />
            <div className="flex items-start justify-between gap-4 text-sm">
              <span className="text-muted-foreground shrink-0">SHA256</span>
              <div className="flex items-center gap-1 min-w-0">
                <span className="font-mono text-xs break-all">{image.checksum || "—"}</span>
                {image.checksum && (
                  <Button variant="ghost" size="icon" className="h-5 w-5 shrink-0" onClick={copySHA}>
                    {copiedSha ? <Check className="h-3 w-3 text-green-500" /> : <Copy className="h-3 w-3" />}
                  </Button>
                )}
              </div>
            </div>
          </Section>

          <Section title="Lifecycle">
            <Row label="Created" value={relativeTime(image.created_at)} />
            <Row label="Finalized" value={relativeTime(image.finalized_at)} />
            {image.build_method && <Row label="Build method" value={image.build_method} />}
            {image.source_url && <Row label="Source URL" value={image.source_url} />}
          </Section>

          {image.tags?.length > 0 && (
            <Section title="Tags">
              <div className="flex flex-wrap gap-1.5">
                {image.tags.map((t) => (
                  <span key={t} className="rounded bg-secondary px-2 py-0.5 text-xs font-mono">{t}</span>
                ))}
              </div>
            </Section>
          )}

          {image.notes && (
            <Section title="Notes">
              <p className="text-sm text-muted-foreground">{image.notes}</p>
            </Section>
          )}

          {/* #252: Reconcile panel — shown for corrupt/blob_missing images */}
          {(image.status === "corrupt" || image.status === "blob_missing") && (
            <Section title="Blob Integrity">
              <div className="space-y-2">
                <div className="flex items-center gap-2">
                  <ImageStatusBadge status={image.status} />
                  <span className="text-xs text-muted-foreground">
                    {image.status === "corrupt"
                      ? "On-disk artifact disagrees with DB checksum and cannot be auto-healed."
                      : "Blob file is absent from disk. Restore from backup or delete this image."}
                  </span>
                </div>
                {image.error_message && (
                  <p className="text-xs font-mono text-muted-foreground break-all">{image.error_message}</p>
                )}
                <div className="flex gap-2 flex-wrap">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={reconcileMutation.isPending}
                    onClick={() => reconcileMutation.mutate(false)}
                  >
                    {reconcileMutation.isPending ? "Checking…" : "Recheck blob"}
                  </Button>
                  {image.status === "corrupt" && (
                    <Button
                      size="sm"
                      variant="destructive"
                      disabled={reconcileMutation.isPending}
                      onClick={() => {
                        if (window.confirm(`Force re-finalize ${image.id.slice(0, 8)}? This accepts the on-disk SHA as the new truth and rewrites metadata.json. Only proceed if you have inspected the tar.`)) {
                          reconcileMutation.mutate(true)
                        }
                      }}
                    >
                      Force re-finalize
                    </Button>
                  )}
                </div>
                {reconcileResult && (
                  <ReconcilePanel result={reconcileResult} onClose={() => setReconcileResult(null)} />
                )}
              </div>
            </Section>
          )}

          {/* Recheck button for ready images too (operator convenience) */}
          {image.status === "ready" && (
            <Section title="Blob Integrity">
              <div className="space-y-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={reconcileMutation.isPending}
                  onClick={() => reconcileMutation.mutate(false)}
                >
                  {reconcileMutation.isPending ? "Checking…" : "Recheck blob"}
                </Button>
                {reconcileResult && (
                  <ReconcilePanel result={reconcileResult} onClose={() => setReconcileResult(null)} />
                )}
              </div>
            </Section>
          )}

          {/* #147: Install Instructions section */}
          {image.status !== "archived" && (
            <Section title="Install Instructions">
              {instrList.length === 0 ? (
                <p className="text-xs text-muted-foreground">No instructions configured.</p>
              ) : (
                <div className="space-y-1.5">
                  {instrList.map((instr, i) => (
                    <div key={i} className="flex items-start gap-2 rounded border border-border bg-muted/30 px-2 py-1.5 text-xs">
                      <span className="shrink-0 rounded bg-secondary px-1.5 py-0.5 font-mono text-[10px]">{instr.opcode}</span>
                      <span className="min-w-0 flex-1 truncate font-mono text-muted-foreground" title={instr.target}>{instr.target}</span>
                      <span className="shrink-0 truncate max-w-[120px] text-muted-foreground/60" title={instr.payload}>{instr.payload.slice(0, 40)}{instr.payload.length > 40 ? "…" : ""}</span>
                      <div className="flex shrink-0 gap-0.5">
                        <Button variant="ghost" size="icon" className="h-5 w-5" onClick={() => moveInstr(i, -1)} disabled={i === 0 || instrMutation.isPending}>
                          <ChevronUp className="h-3 w-3" />
                        </Button>
                        <Button variant="ghost" size="icon" className="h-5 w-5" onClick={() => moveInstr(i, 1)} disabled={i === instrList.length - 1 || instrMutation.isPending}>
                          <ChevronDown className="h-3 w-3" />
                        </Button>
                        <Button variant="ghost" size="icon" className="h-5 w-5" onClick={() => openEditInstr(i)} disabled={instrMutation.isPending}>
                          <Pencil className="h-3 w-3" />
                        </Button>
                        <Button variant="ghost" size="icon" className="h-5 w-5 text-destructive" onClick={() => removeInstr(i)} disabled={instrMutation.isPending}>
                          <Trash2 className="h-3 w-3" />
                        </Button>
                      </div>
                    </div>
                  ))}
                </div>
              )}
              <Button variant="outline" size="sm" className="mt-2 w-full" onClick={openAddInstr} disabled={instrMutation.isPending}>
                <Plus className="h-3.5 w-3.5 mr-1.5" />
                Add instruction
              </Button>
            </Section>
          )}

          {/* #147: Edit instruction dialog */}
          <Dialog open={instrDialogOpen} onOpenChange={(v) => { if (!v) setInstrDialogOpen(false) }}>
            <DialogContent className="sm:max-w-lg">
              <DialogHeader>
                <DialogTitle>{instrEditIdx === null ? "Add instruction" : "Edit instruction"}</DialogTitle>
              </DialogHeader>
              <div className="space-y-4 py-2">
                <div className="space-y-1">
                  <label className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Opcode</label>
                  <div className="flex gap-3">
                    {(["overwrite", "modify", "script"] as InstallInstructionOpcode[]).map((op) => (
                      <label key={op} className="flex items-center gap-1.5 cursor-pointer text-sm">
                        <input
                          type="radio"
                          name="opcode"
                          value={op}
                          checked={instrForm.opcode === op}
                          onChange={() => setInstrForm((f) => ({ ...f, opcode: op }))}
                        />
                        <span className="font-mono text-xs">{op}</span>
                      </label>
                    ))}
                  </div>
                </div>
                <div className="space-y-1">
                  <label className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Target path</label>
                  <Input
                    className="font-mono text-xs"
                    placeholder="/etc/sysctl.conf"
                    value={instrForm.target}
                    onChange={(e) => setInstrForm((f) => ({ ...f, target: e.target.value }))}
                  />
                </div>
                <div className="space-y-1">
                  <label className="text-xs font-medium text-muted-foreground uppercase tracking-wider">
                    {instrForm.opcode === "modify" ? 'Payload (JSON: {"find": "<regex>", "replace": "<string>"})' : instrForm.opcode === "script" ? "Script (POSIX shell)" : "Payload (file content)"}
                  </label>
                  <textarea
                    className={cn(
                      "w-full rounded-md border border-input bg-background px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 font-mono text-xs resize-y",
                      instrForm.opcode === "script" ? "min-h-[160px]" : "min-h-[80px]"
                    )}
                    placeholder={instrForm.opcode === "modify" ? '{"find": "^kernel.panic.*", "replace": "kernel.panic = 10"}' : instrForm.opcode === "script" ? "#!/bin/sh\necho 'hello from chroot'" : "File content here..."}
                    value={instrForm.payload}
                    onChange={(e) => setInstrForm((f) => ({ ...f, payload: e.target.value }))}
                  />
                </div>
                {instrSaveError && <p className="text-xs text-destructive">{instrSaveError}</p>}
                <div className="flex gap-2 pt-1">
                  <Button
                    className="flex-1"
                    onClick={saveInstr}
                    disabled={instrMutation.isPending}
                  >
                    {instrMutation.isPending ? "Saving…" : "Save"}
                  </Button>
                  <Button variant="ghost" onClick={() => setInstrDialogOpen(false)}>Cancel</Button>
                </div>
              </div>
            </DialogContent>
          </Dialog>

          {image.error_message && (
            <Section title="Error">
              <p className="text-sm text-destructive font-mono text-xs">{image.error_message}</p>
            </Section>
          )}

          {/* SHELL-4..6: xterm.js shell drawer — renders outside the sheet stack to cover full viewport */}
          {shellOpen && <ImageShell image={image} onClose={() => setShellOpen(false)} />}

          {/* SHELL-4: shell button — opens xterm.js full-screen drawer (admin-only on server) */}
          {image.status === "ready" && (
            <div className="pt-4 border-t border-border">
              <Button
                variant="outline"
                size="sm"
                className="w-full"
                onClick={() => setShellOpen(true)}
              >
                <Terminal className="h-3.5 w-3.5 mr-1.5" />
                Open shell
              </Button>
            </div>
          )}

          {/* IMG-DEL-2: inline destructive delete with typed name confirm */}
          <div className="pt-4 border-t border-border space-y-3">
            {!deleteExpanded ? (
              <Button
                variant="outline"
                size="sm"
                className="text-destructive border-destructive/40 hover:bg-destructive/10 w-full"
                onClick={() => { setDeleteExpanded(true); setDeleteError("") }}
              >
                <Trash2 className="h-3.5 w-3.5 mr-1.5" />
                Delete image
              </Button>
            ) : (
              <div className="rounded-md border border-destructive/30 bg-destructive/5 p-4 space-y-3">
                <div className="flex items-center gap-2 text-sm font-medium text-destructive">
                  <AlertTriangle className="h-4 w-4 shrink-0" />
                  Delete image — this cannot be undone
                </div>
                {deleteError && (
                  <p className="text-xs text-destructive">{deleteError}</p>
                )}
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">
                    Type <code className="font-mono">{image.name}</code> to confirm:
                  </p>
                  <Input
                    className="font-mono text-xs"
                    placeholder={image.name}
                    value={deleteConfirm}
                    onChange={(e) => setDeleteConfirm(e.target.value)}
                  />
                </div>
                <div className="flex gap-2">
                  <Button
                    variant="destructive"
                    size="sm"
                    className="flex-1"
                    disabled={deleteConfirm !== image.name || deleteMutation.isPending}
                    onClick={confirmDelete}
                  >
                    {deleteMutation.isPending ? "Deleting…" : "Confirm delete"}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => { setDeleteExpanded(false); setDeleteConfirm(""); setDeleteError("") }}
                  >
                    Cancel
                  </Button>
                </div>
              </div>
            )}
          </div>
        </div>
      </SheetContent>
    </Sheet>
  )
}

// ─── BuildInitramfsSheet ──────────────────────────────────────────────────────
// INITRD-3..7: build initramfs from a base image with live SSE log streaming.

interface BuildInitramfsSheetProps {
  open: boolean
  onClose: () => void
  images: BaseImage[]
}

function BuildInitramfsSheet({ open, onClose, images }: BuildInitramfsSheetProps) {
  const qc = useQueryClient()
  const [baseImageId, setBaseImageId] = React.useState("")
  const [imgName, setImgName] = React.useState("")
  const [running, setRunning] = React.useState(false)
  const [logLines, setLogLines] = React.useState<string[]>([])
  const [doneImageId, setDoneImageId] = React.useState<string | null>(null)
  const [errorMsg, setErrorMsg] = React.useState("")
  const abortRef = React.useRef<AbortController | null>(null)
  const logEndRef = React.useRef<HTMLDivElement | null>(null)

  // Auto-scroll log panel (INITRD-4).
  React.useEffect(() => {
    logEndRef.current?.scrollIntoView({ behavior: "smooth" })
  }, [logLines])

  function handleClose() {
    if (running && abortRef.current) {
      abortRef.current.abort()
    }
    setRunning(false)
    setLogLines([])
    setDoneImageId(null)
    setErrorMsg("")
    setBaseImageId("")
    setImgName("")
    onClose()
  }

  async function handleBuild() {
    setRunning(true)
    setLogLines([])
    setDoneImageId(null)
    setErrorMsg("")

    const ctrl = new AbortController()
    abortRef.current = ctrl

    try {
      // INITRD-6: abort controller doubles as cancel signal.
      const res = await fetch("/api/v1/initramfs/build", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({
          base_image_id: baseImageId || undefined,
          name: imgName || undefined,
        }),
        signal: ctrl.signal,
      })
      if (!res.ok) {
        const txt = await res.text().catch(() => "unknown error")
        setErrorMsg(`Server error ${res.status}: ${txt}`)
        setRunning(false)
        return
      }

      const reader = res.body?.getReader()
      if (!reader) {
        setErrorMsg("No response body from server")
        setRunning(false)
        return
      }

      const decoder = new TextDecoder()
      let buf = ""

      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        const lines = buf.split("\n")
        buf = lines.pop() ?? ""
        for (const line of lines) {
          if (!line.startsWith("data: ")) continue
          try {
            const evt = JSON.parse(line.slice(6))
            if (evt.type === "log") {
              setLogLines((prev) => [...prev, evt.line])
            } else if (evt.type === "done") {
              setDoneImageId(evt.image_id)
              setRunning(false)
              qc.invalidateQueries({ queryKey: ["images"] })
              toast({ title: "Initramfs built", description: `Image ${evt.image_id?.slice(0, 8)} is ready.` })
            } else if (evt.type === "error") {
              setErrorMsg(evt.message ?? "Build failed")
              setRunning(false)
            }
          } catch {
            // non-JSON SSE line (keep-alives etc)
          }
        }
      }
    } catch (err: unknown) {
      if ((err as Error)?.name !== "AbortError") {
        setErrorMsg(String(err))
      }
      setRunning(false)
    }
  }

  async function handleCancel() {
    if (abortRef.current) abortRef.current.abort()
    // Also signal server-side cancel.
    try {
      await fetch("/api/v1/initramfs/builds/current", {
        method: "DELETE",
        credentials: "include",
      })
    } catch {
      // Best-effort
    }
    setRunning(false)
    setLogLines((prev) => [...prev, "(build cancelled by operator)"])
  }

  return (
    <Sheet open={open} onOpenChange={(v) => !v && handleClose()}>
      <SheetContent side="right" className="w-full sm:max-w-lg overflow-y-auto">
        <SheetHeader>
          <SheetTitle>Build Initramfs</SheetTitle>
          <SheetDescription>
            Build the PXE initramfs and register it as an image. The existing system initramfs will also be updated.
          </SheetDescription>
        </SheetHeader>
        <div className="mt-6 space-y-4">
          {!running && !doneImageId && (
            <>
              <div className="space-y-1">
                <label className="text-sm text-muted-foreground">Base Image (optional)</label>
                <select
                  className="w-full text-sm border border-border bg-background rounded-md px-3 py-1.5"
                  value={baseImageId}
                  onChange={(e) => setBaseImageId(e.target.value)}
                >
                  <option value="">Current system kernel (default)</option>
                  {images.map((img) => (
                    <option key={img.id} value={img.id}>
                      {img.name} {img.version} ({img.id.slice(0, 8)})
                    </option>
                  ))}
                </select>
                <p className="text-xs text-muted-foreground">
                  Selects which kernel modules to bundle. Leave blank to use the running kernel.
                </p>
              </div>
              <div className="space-y-1">
                <label className="text-sm text-muted-foreground">Name (optional)</label>
                <Input
                  placeholder="initramfs-custom"
                  value={imgName}
                  onChange={(e) => setImgName(e.target.value)}
                  className="text-sm"
                />
              </div>
              <Button className="w-full" onClick={handleBuild}>
                <Layers className="h-4 w-4 mr-2" />
                Start Build
              </Button>
            </>
          )}

          {/* Live log panel (INITRD-4) */}
          {(running || logLines.length > 0) && (
            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider">Build log</h3>
                {running && (
                  <Button variant="ghost" size="sm" className="h-6 px-2 text-xs text-destructive" onClick={handleCancel}>
                    Cancel
                  </Button>
                )}
              </div>
              <div className="rounded-md border border-border bg-card font-mono text-xs p-3 h-64 overflow-y-auto space-y-0.5">
                {logLines.map((line, i) => (
                  <div key={i} className="text-muted-foreground leading-relaxed">{line}</div>
                ))}
                {running && (
                  <div className="flex items-center gap-1.5 text-status-warning">
                    <span className="h-1.5 w-1.5 rounded-full bg-status-warning animate-pulse shrink-0" />
                    Building…
                  </div>
                )}
                <div ref={logEndRef} />
              </div>
            </div>
          )}

          {/* Error state */}
          {errorMsg && (
            <div className="rounded-md border border-destructive/30 bg-destructive/5 p-3 text-xs text-destructive flex items-start gap-2">
              <AlertTriangle className="h-3.5 w-3.5 shrink-0 mt-0.5" />
              {errorMsg}
            </div>
          )}

          {/* INITRD-5: success with "View image" link */}
          {doneImageId && (
            <div className="rounded-md border border-status-healthy/30 bg-status-healthy/5 p-3 text-sm space-y-2">
              <p className="font-medium text-status-healthy">Build complete</p>
              <p className="text-xs text-muted-foreground font-mono">{doneImageId}</p>
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  handleClose()
                }}
              >
                Close
              </Button>
            </div>
          )}
        </div>
      </SheetContent>
    </Sheet>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="space-y-2">
      <h3 className="text-xs font-medium text-muted-foreground uppercase tracking-wider">{title}</h3>
      <div className="space-y-1.5">{children}</div>
    </div>
  )
}

function Row({ label, value, mono }: { label: string; value?: string; mono?: boolean }) {
  return (
    <div className="flex items-start justify-between gap-4 text-sm">
      <span className="text-muted-foreground shrink-0">{label}</span>
      <span className={cn("text-right break-all", mono && "font-mono text-xs")}>{value ?? "—"}</span>
    </div>
  )
}
