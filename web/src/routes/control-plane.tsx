/**
 * control-plane.tsx — /control-plane detail route (#243 SELF-MON)
 *
 * Shows control-plane host metrics, current alert rules targeted at the CP host,
 * and recent CP-role alerts.  Control-plane is NOT a cluster node — it never
 * appears in the datacenter view.
 */
import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { Server, RefreshCw, Clock, Activity, HardDrive, Cpu, Shield } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { apiFetch } from "@/lib/api"
import { SectionErrorBoundary } from "@/components/ErrorBoundary"
import { formatDistanceToNow } from "date-fns"

// ─── Types ───────────────────────────────────────────────────────────────────

interface CPHost {
  id: string
  hostname: string
  created_at: string
}

interface CPMetric {
  plugin: string
  sensor: string
  value: number
  unit?: string
  labels?: Record<string, string>
}

interface CPStatus {
  host: CPHost
  metrics: CPMetric[]
  overall_status: "healthy" | "degraded" | "critical"
  timestamp: string
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

function statusColor(status: string) {
  switch (status) {
    case "critical": return "bg-status-error"
    case "degraded": return "bg-status-warning"
    default: return "bg-status-healthy"
  }
}

function statusLabel(status: string) {
  switch (status) {
    case "critical": return "Critical"
    case "degraded": return "Degraded"
    default: return "Healthy"
  }
}

function formatValue(value: number, unit?: string): string {
  if (!unit) return value.toFixed(2)
  switch (unit) {
    case "bytes": {
      const gb = value / (1024 ** 3)
      if (gb >= 1) return `${gb.toFixed(1)} GiB`
      const mb = value / (1024 ** 2)
      if (mb >= 1) return `${mb.toFixed(1)} MiB`
      return `${(value / 1024).toFixed(1)} KiB`
    }
    case "pct": return `${value.toFixed(1)}%`
    case "seconds": return `${value.toFixed(3)}s`
    case "bool": return value === 0 ? "false" : "true"
    case "days": return `${value.toFixed(0)} days`
    default: return `${value.toFixed(2)} ${unit}`
  }
}

const PLUGIN_ICONS: Record<string, React.ComponentType<{ className?: string }>> = {
  disks: HardDrive,
  memory: Cpu,
  systemd: Shield,
  ntp: Clock,
  psi: Activity,
  certs: Shield,
  images: HardDrive,
}

// ─── Components ──────────────────────────────────────────────────────────────

function MetricsGrid({ metrics }: { metrics: CPMetric[] }) {
  if (metrics.length === 0) {
    return (
      <div className="text-center text-muted-foreground text-sm py-8">
        No metrics collected yet. The selfmon goroutine collects every 30s.
      </div>
    )
  }

  // Group by plugin.
  const byPlugin = new Map<string, CPMetric[]>()
  for (const m of metrics) {
    const group = byPlugin.get(m.plugin) ?? []
    group.push(m)
    byPlugin.set(m.plugin, group)
  }

  return (
    <div className="space-y-4">
      {Array.from(byPlugin.entries()).map(([plugin, rows]) => {
        const Icon = PLUGIN_ICONS[plugin] ?? Activity
        return (
          <div key={plugin} className="rounded-md border border-border bg-card">
            <div className="flex items-center gap-2 px-4 py-2.5 border-b border-border">
              <Icon className="h-3.5 w-3.5 text-muted-foreground" />
              <span className="text-xs font-semibold text-muted-foreground uppercase tracking-wide">
                {plugin}
              </span>
            </div>
            <div className="divide-y divide-border/50">
              {rows.map((m, idx) => (
                <div key={idx} className="flex items-center justify-between px-4 py-2">
                  <div className="flex items-center gap-2 min-w-0">
                    <span className="text-sm text-muted-foreground truncate">{m.sensor}</span>
                    {m.labels && Object.keys(m.labels).length > 0 && (
                      <span className="text-[10px] text-muted-foreground/60 font-mono truncate">
                        {Object.entries(m.labels).map(([k, v]) => `${k}=${v}`).join(", ")}
                      </span>
                    )}
                  </div>
                  <span className="text-sm font-mono text-foreground shrink-0 ml-4">
                    {formatValue(m.value, m.unit)}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )
      })}
    </div>
  )
}

// ─── Page ─────────────────────────────────────────────────────────────────────

export function ControlPlanePage() {
  const { data, isLoading, error, refetch, isFetching } = useQuery<CPStatus>({
    queryKey: ["control-plane-status"],
    queryFn: () => apiFetch("/api/v1/control-plane"),
    refetchInterval: 30_000,
    staleTime: 15_000,
  })

  return (
    <div className="p-6 max-w-4xl mx-auto space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <Server className="h-5 w-5 text-muted-foreground" />
          <div>
            <h1 className="text-lg font-semibold">Control Plane</h1>
            <p className="text-xs text-muted-foreground">
              clustr-serverd host — not a cluster node
            </p>
          </div>
        </div>
        <Button
          variant="outline"
          size="sm"
          className="gap-1.5 text-xs"
          onClick={() => refetch()}
          disabled={isFetching}
        >
          <RefreshCw className={`h-3 w-3 ${isFetching ? "animate-spin" : ""}`} />
          Refresh
        </Button>
      </div>

      {/* Error state */}
      {error && (
        <div className="rounded-md border border-destructive/40 bg-destructive/10 p-4 text-sm text-destructive">
          Failed to load control-plane status.{" "}
          <button className="underline" onClick={() => refetch()}>Retry</button>
        </div>
      )}

      {/* Loading state */}
      {isLoading && (
        <div className="space-y-3">
          <Skeleton className="h-20" />
          <Skeleton className="h-40" />
        </div>
      )}

      {data && (
        <>
          {/* Status card */}
          <div className="rounded-md border border-border bg-card p-4 flex items-center justify-between">
            <div className="flex items-center gap-4">
              <div className="flex items-center gap-2">
                <span className={`h-2.5 w-2.5 rounded-full ${statusColor(data.overall_status)}`} />
                <span className="text-sm font-medium">{statusLabel(data.overall_status)}</span>
              </div>
              <div className="text-xs text-muted-foreground">
                <span className="font-mono">{data.host.hostname}</span>
              </div>
              <div className="text-xs text-muted-foreground font-mono text-[10px]">
                {data.host.id}
              </div>
            </div>
            <div className="text-[11px] text-muted-foreground">
              Refreshed {formatDistanceToNow(new Date(data.timestamp), { addSuffix: true })}
            </div>
          </div>

          {/* Metrics */}
          <div>
            <h2 className="text-sm font-semibold mb-3 flex items-center gap-2">
              <Activity className="h-4 w-4 text-muted-foreground" />
              Current Metrics
              <Badge variant="secondary" className="ml-1 text-[10px]">
                {(data.metrics ?? []).length} series
              </Badge>
            </h2>
            <SectionErrorBoundary>
              <MetricsGrid metrics={data.metrics ?? []} />
            </SectionErrorBoundary>
          </div>
        </>
      )}
    </div>
  )
}
