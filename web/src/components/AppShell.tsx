import * as React from "react"
import { Link, useRouterState, useNavigate } from "@tanstack/react-router"
import { Server, Image, Activity, Settings, ShieldCheck, Cpu, Building2, Bell, ChevronsLeft, ChevronsRight, Command as CmdIcon, Sun, Moon, LogOut, User, WifiOff, GitCommit, ChevronDown, ChevronRight, Trash2, Check, X, MonitorDot } from "lucide-react"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { CommandPalette } from "@/components/CommandPalette"
import { ErrorBoundary } from "@/components/ErrorBoundary"
import { useTheme } from "@/contexts/theme"
import { useConnection } from "@/contexts/connection"
import { useSession } from "@/contexts/auth"
import { apiFetch } from "@/lib/api"
import { cn } from "@/lib/utils"
import { toast } from "@/hooks/use-toast"

const navItems = [
  { label: "Nodes", path: "/nodes", icon: Server, active: true },
  { label: "Images", path: "/images", icon: Image, active: true },
  { label: "Slurm", path: "/slurm", icon: Cpu, active: true },
  { label: "Alerts", path: "/alerts", icon: Bell, active: true },
  { label: "Datacenter", path: "/datacenter", icon: Building2, active: true },
  { label: "Activity", path: "/activity", icon: Activity, active: true },
  { label: "Identity", path: "/identity", icon: ShieldCheck, active: true },
  { label: "Control Plane", path: "/control-plane", icon: MonitorDot, active: true },
  { label: "Settings", path: "/settings", icon: Settings, active: true },
]

const connectionConfig: Record<string, { color: string; label: string }> = {
  // UX-4 statuses
  connecting: { color: "bg-status-warning animate-pulse", label: "Connecting" },
  open: { color: "bg-status-healthy", label: "Connected" },
  reconnecting: { color: "bg-status-warning animate-pulse", label: "Reconnecting" },
  failed: { color: "bg-status-neutral", label: "Disconnected" },
  // Legacy statuses (kept for backward compat)
  connected: { color: "bg-status-healthy", label: "Connected" },
  disconnected: { color: "bg-status-neutral", label: "Disconnected" },
  paused: { color: "bg-status-error", label: "Live updates paused" },
}

// ─── Pending-changes types ────────────────────────────────────────────────────

interface PendingChange {
  id: string
  kind: string
  target: string
  payload: string // raw JSON string
  created_by: string
  created_at: number
}

interface CommitResult {
  id: string
  kind: string
  target: string
  success: boolean
  error?: string
}

const KIND_LABELS: Record<string, string> = {
  ldap_user: "LDAP user",
  sudoers_rule: "Sudoers rule",
  node_network: "Network profile",
}

// ─── Payload diff renderer ────────────────────────────────────────────────────

function PayloadDiff({ payload }: { payload: string }) {
  let parsed: Record<string, unknown>
  try {
    parsed = JSON.parse(payload) as Record<string, unknown>
  } catch {
    return <pre className="text-[10px] font-mono text-muted-foreground whitespace-pre-wrap break-all">{payload}</pre>
  }
  return (
    <div className="space-y-0.5">
      {Object.entries(parsed).map(([k, v]) => (
        <div key={k} className="flex gap-2 text-[10px] font-mono">
          <span className="text-green-400 shrink-0">+</span>
          <span className="text-muted-foreground shrink-0">{k}:</span>
          <span className="text-foreground break-all">{String(v)}</span>
        </div>
      ))}
    </div>
  )
}

// ─── Pending Changes drawer ───────────────────────────────────────────────────

function PendingChangesDrawer({
  open,
  onClose,
}: {
  open: boolean
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = React.useState<Set<string>>(new Set())

  const { data, isLoading } = useQuery<{ changes: PendingChange[]; total: number }>({
    queryKey: ["pending-changes"],
    queryFn: () => apiFetch("/api/v1/changes"),
    enabled: open,
    refetchInterval: open ? 5000 : false,
    staleTime: 0,
  })

  const commitAllMutation = useMutation({
    mutationFn: () =>
      apiFetch<{ results: CommitResult[] }>("/api/v1/changes/commit", { method: "POST", body: "{}" }),
    onSuccess: (res) => {
      const failed = res.results.filter((r) => !r.success)
      if (failed.length === 0) {
        toast({ title: `Committed ${res.results.length} change(s)` })
      } else {
        toast({
          variant: "destructive",
          title: `${failed.length} change(s) failed`,
          description: failed.map((f) => f.error ?? f.target).join("; "),
        })
      }
      qc.invalidateQueries({ queryKey: ["pending-changes"] })
      qc.invalidateQueries({ queryKey: ["pending-changes-count"] })
    },
    onError: (err) => toast({ variant: "destructive", title: "Commit failed", description: String(err) }),
  })

  const commitOneMutation = useMutation({
    mutationFn: (id: string) =>
      apiFetch<{ results: CommitResult[] }>("/api/v1/changes/commit", {
        method: "POST",
        body: JSON.stringify({ ids: [id] }),
      }),
    onSuccess: (res) => {
      const r = res.results[0]
      if (r?.success) {
        toast({ title: `Committed ${r.kind} on ${r.target}` })
      } else {
        toast({ variant: "destructive", title: "Commit failed", description: r?.error ?? "unknown" })
      }
      qc.invalidateQueries({ queryKey: ["pending-changes"] })
      qc.invalidateQueries({ queryKey: ["pending-changes-count"] })
    },
    onError: (err) => toast({ variant: "destructive", title: "Commit failed", description: String(err) }),
  })

  const clearAllMutation = useMutation({
    mutationFn: () =>
      apiFetch("/api/v1/changes/clear", { method: "POST", body: "{}" }),
    onSuccess: () => {
      toast({ title: "Cleared all pending changes" })
      qc.invalidateQueries({ queryKey: ["pending-changes"] })
      qc.invalidateQueries({ queryKey: ["pending-changes-count"] })
    },
    onError: (err) => toast({ variant: "destructive", title: "Clear failed", description: String(err) }),
  })

  const clearOneMutation = useMutation({
    mutationFn: (id: string) =>
      apiFetch("/api/v1/changes/clear", { method: "POST", body: JSON.stringify({ ids: [id] }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["pending-changes"] })
      qc.invalidateQueries({ queryKey: ["pending-changes-count"] })
    },
    onError: (err) => toast({ variant: "destructive", title: "Clear failed", description: String(err) }),
  })

  const changes = data?.changes ?? []

  function toggleExpand(id: string) {
    setExpanded((prev) => {
      const next = new Set(prev)
      next.has(id) ? next.delete(id) : next.add(id)
      return next
    })
  }

  if (!open) return null

  return (
    <>
      {/* Backdrop */}
      <div
        className="fixed inset-0 bg-black/40 z-40"
        onClick={onClose}
        aria-hidden="true"
      />
      {/* Drawer panel */}
      <div className="fixed right-0 top-0 h-full w-[480px] max-w-full bg-card border-l border-border z-50 flex flex-col shadow-xl">
        {/* Header */}
        <div className="flex items-center justify-between border-b border-border px-4 py-3 shrink-0">
          <h2 className="text-sm font-semibold flex items-center gap-2">
            <GitCommit className="h-4 w-4 text-muted-foreground" />
            Pending Changes {changes.length > 0 && <span className="text-muted-foreground">({changes.length})</span>}
          </h2>
          <Button variant="ghost" size="icon" className="h-7 w-7" onClick={onClose}>
            <X className="h-4 w-4" />
          </Button>
        </div>

        {/* Bulk actions */}
        {changes.length > 0 && (
          <div className="flex items-center gap-2 px-4 py-2 border-b border-border shrink-0">
            <Button
              size="sm"
              className="gap-1.5 text-xs h-7"
              onClick={() => commitAllMutation.mutate()}
              disabled={commitAllMutation.isPending}
            >
              <Check className="h-3 w-3" />
              {commitAllMutation.isPending ? "Committing..." : "Commit all"}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              className="gap-1.5 text-xs h-7 text-muted-foreground hover:text-destructive"
              onClick={() => clearAllMutation.mutate()}
              disabled={clearAllMutation.isPending}
            >
              <Trash2 className="h-3 w-3" />
              {clearAllMutation.isPending ? "Clearing..." : "Clear all"}
            </Button>
          </div>
        )}

        {/* Change list */}
        <div className="flex-1 overflow-y-auto">
          {isLoading && (
            <p className="text-sm text-muted-foreground p-4">Loading...</p>
          )}
          {!isLoading && changes.length === 0 && (
            <div className="flex flex-col items-center justify-center h-full gap-2 text-muted-foreground">
              <GitCommit className="h-8 w-8 opacity-20" />
              <p className="text-sm">No pending changes</p>
            </div>
          )}
          {changes.map((c) => {
            const isExpanded = expanded.has(c.id)
            return (
              <div key={c.id} className="border-b border-border/50">
                {/* Summary row */}
                <div className="flex items-center gap-2 px-4 py-2.5 hover:bg-secondary/20">
                  <button
                    className="flex items-center gap-2 flex-1 text-left min-w-0"
                    onClick={() => toggleExpand(c.id)}
                  >
                    {isExpanded ? (
                      <ChevronDown className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
                    ) : (
                      <ChevronRight className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
                    )}
                    <span className="text-xs rounded px-1.5 py-0.5 bg-secondary text-foreground shrink-0">
                      {KIND_LABELS[c.kind] ?? c.kind}
                    </span>
                    <span className="text-xs font-mono text-muted-foreground truncate">{c.target}</span>
                  </button>
                  <div className="flex items-center gap-1 shrink-0">
                    <Button
                      size="sm"
                      className="h-6 px-2 text-[11px] gap-1"
                      onClick={() => commitOneMutation.mutate(c.id)}
                      disabled={commitOneMutation.isPending}
                    >
                      <Check className="h-3 w-3" />
                      Commit
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 w-6 p-0 text-muted-foreground hover:text-destructive"
                      onClick={() => clearOneMutation.mutate(c.id)}
                      disabled={clearOneMutation.isPending}
                    >
                      <Trash2 className="h-3 w-3" />
                    </Button>
                  </div>
                </div>
                {/* Expanded diff */}
                {isExpanded && (
                  <div className="px-4 pb-3 pt-1 bg-secondary/10">
                    <PayloadDiff payload={c.payload} />
                    {c.created_by && (
                      <p className="text-[10px] text-muted-foreground mt-2">
                        Staged by {c.created_by}
                      </p>
                    )}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      </div>
    </>
  )
}

// ─── AppShell ─────────────────────────────────────────────────────────────────

// ─── Control-plane status strip ──────────────────────────────────────────────

interface CPStripStatus {
  overall_status: "healthy" | "degraded" | "critical"
  host: { hostname: string }
}

function ControlPlaneStrip() {
  const navigate = useNavigate()
  const { data } = useQuery<CPStripStatus>({
    queryKey: ["cp-strip-status"],
    queryFn: () => apiFetch("/api/v1/control-plane"),
    refetchInterval: 30_000,
    staleTime: 15_000,
    // Fail silently — the strip disappears if the endpoint isn't ready yet.
    retry: 0,
  })

  // Only show when status is not healthy (or when critical — non-dismissible).
  if (!data || data.overall_status === "healthy") return null

  const isCrit = data.overall_status === "critical"

  return (
    <div
      role="alert"
      className={cn(
        "flex items-center justify-between gap-3 px-4 py-1.5 text-xs shrink-0 cursor-pointer",
        isCrit
          ? "bg-destructive/15 border-b border-destructive/40 text-destructive"
          : "bg-amber-500/10 border-b border-amber-500/30 text-amber-400"
      )}
      onClick={() => navigate({ to: "/control-plane" })}
    >
      <span className="flex items-center gap-2">
        <span className={cn("h-1.5 w-1.5 rounded-full shrink-0", isCrit ? "bg-destructive" : "bg-amber-400")} />
        <span>
          Control plane{" "}
          <span className="font-semibold">{data.overall_status}</span>
          {data.host?.hostname && <span className="text-muted-foreground ml-1">— {data.host.hostname}</span>}
        </span>
      </span>
      <span className="underline underline-offset-2 hover:no-underline shrink-0">
        View details
      </span>
    </div>
  )
}

// ─── AppShell ─────────────────────────────────────────────────────────────────

export function AppShell({ children }: { children: React.ReactNode }) {
  const [collapsed, setCollapsed] = React.useState(false)
  const [paletteOpen, setPaletteOpen] = React.useState(false)
  const [userMenuOpen, setUserMenuOpen] = React.useState(false)
  const [changesDrawerOpen, setChangesDrawerOpen] = React.useState(false)
  const { theme, toggle } = useTheme()
  const { status, paused, retry } = useConnection()
  const { session, setUnauthed } = useSession()
  const routerState = useRouterState()
  const navigate = useNavigate()
  const currentPath = routerState.location.pathname

  const username =
    session.status === "authed" ? (session.user.username ?? session.user.sub) : ""

  // Poll pending-changes count every 10s for the badge.
  const { data: countData } = useQuery<{ count: number }>({
    queryKey: ["pending-changes-count"],
    queryFn: () => apiFetch("/api/v1/changes/count"),
    refetchInterval: 10000,
    staleTime: 5000,
  })
  const pendingCount = countData?.count ?? 0

  // Cmd-K + vim-style leader keys (g n/i/a/s)
  const gKeyPending = React.useRef(false)
  const gTimer = React.useRef<ReturnType<typeof setTimeout> | null>(null)

  React.useEffect(() => {
    function onKey(e: KeyboardEvent) {
      // Skip if focused on an input/textarea.
      const tag = (e.target as HTMLElement)?.tagName?.toLowerCase()
      const editable = (e.target as HTMLElement)?.isContentEditable
      if (tag === "input" || tag === "textarea" || tag === "select" || editable) return

      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault()
        setPaletteOpen(true)
        return
      }

      // Vim-style leader: g then n/i/a/s
      if (gKeyPending.current) {
        gKeyPending.current = false
        if (gTimer.current) clearTimeout(gTimer.current)
        switch (e.key) {
          case "n": navigate({ to: "/nodes", search: { q: undefined, status: undefined, sort: undefined, dir: undefined, openNode: undefined, reimage: undefined, addNode: undefined, deleteNode: undefined, tag: undefined, view: undefined, createGroup: undefined } }); break
          case "i": navigate({ to: "/images", search: { q: undefined, tab: undefined, sort: undefined, dir: undefined, addImage: undefined } }); break
          case "a": navigate({ to: "/activity", search: { q: undefined, kind: undefined } }); break
          case "s": navigate({ to: "/settings" }); break
          case "d": navigate({ to: "/identity" }); break
          case "l": navigate({ to: "/slurm" }); break
          case "r": navigate({ to: "/alerts" }); break
          case "c": navigate({ to: "/datacenter" }); break
        }
        return
      }

      if (e.key === "g" && !e.metaKey && !e.ctrlKey && !e.altKey) {
        gKeyPending.current = true
        gTimer.current = setTimeout(() => { gKeyPending.current = false }, 1000)
      }
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [navigate])

  // Close user menu on outside click.
  const userMenuRef = React.useRef<HTMLDivElement>(null)
  React.useEffect(() => {
    if (!userMenuOpen) return
    function onClick(e: MouseEvent) {
      if (userMenuRef.current && !userMenuRef.current.contains(e.target as Node)) {
        setUserMenuOpen(false)
      }
    }
    document.addEventListener("mousedown", onClick)
    return () => document.removeEventListener("mousedown", onClick)
  }, [userMenuOpen])

  async function handleLogout() {
    try {
      await apiFetch("/api/v1/auth/logout", { method: "POST" })
    } catch {
      // Ignore errors — clear local state regardless.
    }
    setUnauthed()
    setUserMenuOpen(false)
  }

  // POL-6: use "paused" config when SSE has been down for >30s.
  const conn = paused ? connectionConfig.paused : connectionConfig[status]

  return (
    <div className="flex flex-col h-full">
      {/* Control-plane status strip (non-dismissible, above everything) */}
      <ControlPlaneStrip />

      <div className="flex flex-1 bg-background text-foreground min-h-0">
      {/* A11Y-1: skip-to-main link */}
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:top-2 focus:left-2 focus:px-3 focus:py-1.5 focus:rounded focus:bg-primary focus:text-primary-foreground focus:text-sm"
      >
        Skip to main content
      </a>

      {/* Sidebar */}
      <aside
        className={cn(
          "flex flex-col border-r border-border bg-card transition-all duration-200",
          collapsed ? "w-14" : "w-52"
        )}
      >
        {/* Logo */}
        <div className={cn("flex h-14 items-center border-b border-border px-3", collapsed ? "justify-center" : "gap-2")}>
          <span className="inline-flex h-7 w-7 items-center justify-center rounded bg-primary text-primary-foreground text-xs font-bold shrink-0">
            C
          </span>
          {!collapsed && <span className="font-semibold text-sm">clustr</span>}
        </div>

        {/* Nav */}
        <nav className="flex flex-col gap-1 p-2 flex-1">
          {navItems.map((item) => {
            const isActive = currentPath.startsWith(item.path)
            const el = (
              <Link
                key={item.path}
                to={item.path}
                className={cn(
                  "flex items-center gap-3 rounded-md px-2 py-2 text-sm transition-colors",
                  isActive
                    ? "bg-secondary text-foreground"
                    : "text-muted-foreground hover:bg-secondary/50 hover:text-foreground"
                )}
              >
                <item.icon className="h-4 w-4 shrink-0" />
                {!collapsed && <span>{item.label}</span>}
              </Link>
            )
            if (collapsed) {
              return (
                <Tooltip key={item.path}>
                  <TooltipTrigger asChild>{el}</TooltipTrigger>
                  <TooltipContent side="right">{item.label}</TooltipContent>
                </Tooltip>
              )
            }
            return el
          })}
        </nav>

        {/* Collapse toggle */}
        <div className="border-t border-border p-2">
          <Button
            variant="ghost"
            size="icon"
            className="w-full"
            onClick={() => setCollapsed((c) => !c)}
          >
            {collapsed ? <ChevronsRight className="h-4 w-4" /> : <ChevronsLeft className="h-4 w-4" />}
          </Button>
        </div>
      </aside>

      {/* Main content */}
      <div className="flex flex-col flex-1 min-w-0">
        {/* Top bar */}
        <header className="flex h-14 items-center justify-between border-b border-border px-4 bg-card shrink-0">
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              className="gap-2 text-muted-foreground text-xs"
              onClick={() => setPaletteOpen(true)}
            >
              <CmdIcon className="h-3.5 w-3.5" />
              <span>Command</span>
              <kbd className="pointer-events-none ml-1 select-none rounded border border-border bg-muted px-1 text-[10px] font-mono">
                ⌘K
              </kbd>
            </Button>
          </div>

          <div className="flex items-center gap-3">
            {/* Pending Changes badge — only visible when count > 0 */}
            {pendingCount > 0 && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    variant="outline"
                    size="sm"
                    className="relative gap-1.5 text-xs h-8 border-amber-500/40 text-amber-400 hover:bg-amber-500/10"
                    onClick={() => setChangesDrawerOpen(true)}
                  >
                    <GitCommit className="h-3.5 w-3.5" />
                    <span>{pendingCount} pending</span>
                  </Button>
                </TooltipTrigger>
                <TooltipContent>Open pending changes</TooltipContent>
              </Tooltip>
            )}

            {/* Connection indicator */}
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <span className={cn("h-2 w-2 rounded-full shrink-0", conn.color)} />
              <span>{conn.label}</span>
            </div>

            {/* Theme toggle */}
            <Button variant="ghost" size="icon" onClick={toggle}>
              {theme === "dark" ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
            </Button>

            {/* User menu */}
            <div className="relative" ref={userMenuRef}>
              <Button
                variant="ghost"
                size="sm"
                className="gap-1.5 text-xs text-muted-foreground"
                onClick={() => setUserMenuOpen((v) => !v)}
              >
                <User className="h-3.5 w-3.5" />
                {username && <span className="max-w-24 truncate">{username}</span>}
              </Button>

              {userMenuOpen && (
                <div className="absolute right-0 top-full mt-1 w-40 rounded-md border border-border bg-card shadow-md z-50">
                  <button
                    className="flex w-full items-center gap-2 px-3 py-2 text-sm text-muted-foreground hover:bg-secondary/50 hover:text-foreground rounded-md"
                    onClick={handleLogout}
                  >
                    <LogOut className="h-3.5 w-3.5" />
                    Sign out
                  </button>
                </div>
              )}
            </div>
          </div>
        </header>

        {/* POL-6: SSE disconnect banner — shown after 30s of no connection */}
        {paused && (
          <div
            role="alert"
            className="flex items-center justify-between gap-3 px-4 py-2 bg-destructive/10 border-b border-destructive/30 text-sm text-destructive shrink-0"
          >
            <span className="flex items-center gap-2">
              <WifiOff className="h-4 w-4 shrink-0" aria-hidden="true" />
              Live updates paused. Check your network connection.
            </span>
            <button
              onClick={retry}
              className="underline underline-offset-2 hover:no-underline shrink-0 text-sm"
            >
              Retry
            </button>
          </div>
        )}

        {/* Page content */}
        <main id="main-content" className="flex-1 overflow-auto" tabIndex={-1}>
          <ErrorBoundary>
            {children}
          </ErrorBoundary>
        </main>
      </div>

      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />

      {/* Pending Changes drawer */}
      <PendingChangesDrawer
        open={changesDrawerOpen}
        onClose={() => setChangesDrawerOpen(false)}
      />
      </div>{/* end flex row (sidebar + main) */}
    </div>
  )
}
