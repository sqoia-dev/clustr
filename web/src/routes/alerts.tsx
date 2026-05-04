/**
 * alerts.tsx — Sprint 24 #155 Alerts surface.
 *
 * Three tabs:
 *   Active   — firing alerts, auto-refresh 5s, per-row silence + rule drill-down
 *   Silenced — active alerts with an associated silence
 *   History  — resolved alerts in last 24h (default), date range picker
 *
 * Per-rule drill-down: modal with rule detail + editable YAML editor (react-simple-code-editor + prismjs).
 * UX-9: PUT /api/v1/alerts/rules/{name} wired to Save button.
 */
import * as React from "react"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { formatDistanceToNow, format } from "date-fns"
import {
  Bell, CheckCircle2, XCircle, AlertCircle, Info,
  VolumeX, Eye, RefreshCw, Loader2, X,
  ChevronDown, Filter, Save,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs"
import { Skeleton } from "@/components/ui/skeleton"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { apiFetch } from "@/lib/api"
import { SectionErrorBoundary } from "@/components/ErrorBoundary"
import { toast } from "@/hooks/use-toast"
import { cn } from "@/lib/utils"
import Editor from "react-simple-code-editor"
import Prism from "prismjs"
import "prismjs/components/prism-yaml"

// ─── Types ───────────────────────────────────────────────────────────────────

interface Alert {
  id: number
  rule_name: string
  node_id: string
  sensor: string
  labels?: Record<string, string>
  severity: string
  state: string
  fired_at: string
  resolved_at?: string
  last_value: number
  threshold_op: string
  threshold_val: number
}

interface AlertsResponse {
  active: Alert[]
  recent: Alert[]
}

interface Silence {
  id: string
  rule_name: string
  node_id?: string
  expires_at: number
  created_at: number
  created_by?: string
}

interface SilencesResponse {
  silences: Silence[]
}

interface Rule {
  name: string
  description?: string
  plugin: string
  sensor: string
  labels?: Record<string, string>
  threshold: { op: string; value: number }
  duration_seconds: number
  severity: string
  notify: { webhook: boolean; email?: string[] }
}

interface RulesResponse {
  rules: Rule[]
  total: number
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

const SEVERITY_CONFIG: Record<string, { color: string; icon: React.ElementType; label: string }> = {
  critical: { color: "bg-status-error text-white",   icon: XCircle,       label: "Critical" },
  warn:     { color: "bg-status-warning text-black",  icon: AlertCircle,   label: "Warning"  },
  info:     { color: "bg-status-neutral text-white",  icon: Info,          label: "Info"     },
}

function SeverityPill({ severity }: { severity: string }) {
  const cfg = SEVERITY_CONFIG[severity] ?? { color: "bg-muted text-foreground", icon: Bell, label: severity }
  const Icon = cfg.icon
  return (
    <span className={cn("inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium", cfg.color)}>
      <Icon className="h-3 w-3" aria-hidden />
      {cfg.label}
    </span>
  )
}

function reltime(iso: string) {
  try {
    return formatDistanceToNow(new Date(iso), { addSuffix: true })
  } catch {
    return iso
  }
}

function silenceLabel(sil: Silence) {
  if (sil.expires_at === -1) return "forever"
  return format(new Date(sil.expires_at * 1000), "MMM d HH:mm")
}

function ruleToYaml(rule: Rule): string {
  const lines: string[] = [
    `name: ${rule.name}`,
  ]
  if (rule.description) lines.push(`description: ${rule.description}`)
  lines.push(`plugin: ${rule.plugin}`)
  lines.push(`sensor: ${rule.sensor}`)
  if (rule.labels && Object.keys(rule.labels).length > 0) {
    lines.push("labels:")
    for (const [k, v] of Object.entries(rule.labels)) {
      lines.push(`  ${k}: "${v}"`)
    }
  }
  lines.push("threshold:")
  lines.push(`  op: "${rule.threshold.op}"`)
  lines.push(`  value: ${rule.threshold.value}`)
  if (rule.duration_seconds > 0) {
    lines.push(`duration: ${rule.duration_seconds}s`)
  }
  lines.push(`severity: ${rule.severity}`)
  lines.push("notify:")
  lines.push(`  webhook: ${rule.notify.webhook}`)
  if (rule.notify.email && rule.notify.email.length > 0) {
    lines.push(`  email: [${rule.notify.email.map(e => `"${e}"`).join(", ")}]`)
  }
  return lines.join("\n")
}

// ─── Silence dropdown ────────────────────────────────────────────────────────

const SILENCE_DURATIONS = [
  { label: "1 hour",   value: "1h"      },
  { label: "4 hours",  value: "4h"      },
  { label: "24 hours", value: "24h"     },
  { label: "Forever",  value: "forever" },
]

function SilenceButton({ ruleName, nodeId, onDone }: { ruleName: string; nodeId: string; onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const ref = React.useRef<HTMLDivElement>(null)
  const qc = useQueryClient()

  const silenceMut = useMutation({
    mutationFn: (duration: string) =>
      apiFetch("/api/v1/alerts/silences", {
        method: "POST",
        body: JSON.stringify({ rule_name: ruleName, node_id: nodeId, duration }),
      }),
    onSuccess: () => {
      toast({ title: "Alert silenced" })
      qc.invalidateQueries({ queryKey: ["alerts"] })
      qc.invalidateQueries({ queryKey: ["alert-silences"] })
      setOpen(false)
      onDone()
    },
    onError: (e: Error) => {
      toast({ title: "Failed to silence", description: e.message, variant: "destructive" })
    },
  })

  React.useEffect(() => {
    if (!open) return
    function onDown(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener("mousedown", onDown)
    return () => document.removeEventListener("mousedown", onDown)
  }, [open])

  return (
    <div className="relative inline-block" ref={ref}>
      <Button
        variant="outline"
        size="sm"
        className="gap-1 h-7 text-xs"
        onClick={() => setOpen(v => !v)}
        disabled={silenceMut.isPending}
      >
        <VolumeX className="h-3 w-3" />
        Silence
        <ChevronDown className="h-3 w-3" />
      </Button>
      {open && (
        <div className="absolute right-0 top-full mt-1 z-50 w-32 rounded-md border border-border bg-card shadow-md">
          {SILENCE_DURATIONS.map(d => (
            <button
              key={d.value}
              className="flex w-full items-center px-3 py-1.5 text-xs hover:bg-secondary/50 rounded-md"
              onClick={() => silenceMut.mutate(d.value)}
              disabled={silenceMut.isPending}
            >
              {d.label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

// ─── Rule drill-down modal ────────────────────────────────────────────────────

function RuleModal({ rule, onClose }: { rule: Rule; onClose: () => void }) {
  const qc = useQueryClient()
  const [yamlValue, setYamlValue] = React.useState(() => ruleToYaml(rule))
  const isDirty = yamlValue !== ruleToYaml(rule)

  const saveMut = useMutation({
    mutationFn: () =>
      apiFetch(`/api/v1/alerts/rules/${encodeURIComponent(rule.name)}`, {
        method: "PUT",
        body: JSON.stringify({ yaml: yamlValue }),
      }),
    onSuccess: () => {
      toast({ title: "Rule saved" })
      qc.invalidateQueries({ queryKey: ["alert-rules"] })
      onClose()
    },
    onError: (e: Error) => {
      toast({ title: "Save failed", description: e.message, variant: "destructive" })
    },
  })

  return (
    <Dialog open onOpenChange={v => !v && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle className="font-mono text-sm">{rule.name}</DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          {rule.description && (
            <p className="text-sm text-muted-foreground">{rule.description}</p>
          )}
          <div className="grid grid-cols-2 gap-3 text-sm">
            <div>
              <span className="text-muted-foreground">Plugin</span>
              <p className="font-mono">{rule.plugin}</p>
            </div>
            <div>
              <span className="text-muted-foreground">Sensor</span>
              <p className="font-mono">{rule.sensor}</p>
            </div>
            <div>
              <span className="text-muted-foreground">Threshold</span>
              <p className="font-mono">{rule.threshold.op} {rule.threshold.value}</p>
            </div>
            <div>
              <span className="text-muted-foreground">Severity</span>
              <SeverityPill severity={rule.severity} />
            </div>
            {rule.duration_seconds > 0 && (
              <div>
                <span className="text-muted-foreground">Duration</span>
                <p>{rule.duration_seconds}s</p>
              </div>
            )}
            <div>
              <span className="text-muted-foreground">Notify</span>
              <p>
                {[
                  rule.notify.webhook ? "webhook" : null,
                  ...(rule.notify.email ?? []),
                ].filter(Boolean).join(", ") || "—"}
              </p>
            </div>
          </div>
          <div>
            <p className="text-xs text-muted-foreground mb-1">Rule YAML</p>
            <div className="rounded-md border border-border bg-muted p-3 overflow-auto max-h-64 font-mono text-xs">
              <Editor
                value={yamlValue}
                onValueChange={setYamlValue}
                highlight={code => Prism.highlight(code, Prism.languages.yaml, "yaml")}
                padding={0}
                style={{ fontFamily: "var(--font-mono, monospace)", fontSize: 12, lineHeight: 1.6 }}
              />
            </div>
          </div>
          <div className="flex justify-end gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saveMut.isPending}>
              Cancel
            </Button>
            <Button
              size="sm"
              className="gap-1.5"
              onClick={() => saveMut.mutate()}
              disabled={!isDirty || saveMut.isPending}
            >
              {saveMut.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <Save className="h-3.5 w-3.5" />
              )}
              Save
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

// ─── Filter toolbar ───────────────────────────────────────────────────────────

interface Filters {
  node: string
  severities: string[]
  rule: string
}

const ALL_SEVERITIES = ["critical", "warn", "info"]

function FilterBar({
  filters,
  onChange,
  rules,
}: {
  filters: Filters
  onChange: (f: Filters) => void
  rules: Rule[]
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 pb-3">
      <div className="flex items-center gap-1 text-xs text-muted-foreground">
        <Filter className="h-3.5 w-3.5" />
        Filters:
      </div>
      {/* Severity multi-select */}
      <div className="flex gap-1">
        {ALL_SEVERITIES.map(sev => {
          const active = filters.severities.includes(sev)
          return (
            <button
              key={sev}
              onClick={() => {
                const next = active
                  ? filters.severities.filter(s => s !== sev)
                  : [...filters.severities, sev]
                onChange({ ...filters, severities: next })
              }}
              className={cn(
                "rounded-full px-2 py-0.5 text-xs border transition-colors",
                active
                  ? "border-accent text-foreground bg-secondary"
                  : "border-border text-muted-foreground hover:border-accent"
              )}
            >
              {sev}
            </button>
          )
        })}
      </div>
      {/* Node text filter */}
      <Input
        className="h-7 w-44 text-xs"
        placeholder="Filter by node…"
        value={filters.node}
        onChange={e => onChange({ ...filters, node: e.target.value })}
      />
      {/* Rule dropdown */}
      {rules.length > 0 && (
        <select
          className="h-7 rounded-md border border-border bg-background text-xs px-2 text-foreground"
          value={filters.rule}
          onChange={e => onChange({ ...filters, rule: e.target.value })}
        >
          <option value="">All rules</option>
          {rules.map(r => (
            <option key={r.name} value={r.name}>{r.name}</option>
          ))}
        </select>
      )}
      {(filters.node || filters.severities.length > 0 || filters.rule) && (
        <button
          className="text-xs text-muted-foreground hover:text-foreground underline"
          onClick={() => onChange({ node: "", severities: [], rule: "" })}
        >
          Clear
        </button>
      )}
    </div>
  )
}

// ─── Alert table ──────────────────────────────────────────────────────────────

function applyFilters(alerts: Alert[], filters: Filters): Alert[] {
  return alerts.filter(a => {
    if (filters.severities.length > 0 && !filters.severities.includes(a.severity)) return false
    if (filters.node && !a.node_id.toLowerCase().includes(filters.node.toLowerCase())) return false
    if (filters.rule && a.rule_name !== filters.rule) return false
    return true
  })
}

function AlertTable({
  alerts,
  silences,
  rules,
  showSilenceExpiry,
  showResolved,
  onViewRule,
  onRefetch,
}: {
  alerts: Alert[]
  silences: Silence[]
  rules: Rule[]
  showSilenceExpiry?: boolean
  showResolved?: boolean
  onViewRule: (name: string) => void
  onRefetch: () => void
}) {
  const [filters, setFilters] = React.useState<Filters>({ node: "", severities: [], rule: "" })
  const qc = useQueryClient()

  const unsilenceMut = useMutation({
    mutationFn: (silenceId: string) =>
      apiFetch(`/api/v1/alerts/silences/${silenceId}`, { method: "DELETE" }),
    onSuccess: () => {
      toast({ title: "Silence removed" })
      qc.invalidateQueries({ queryKey: ["alerts"] })
      qc.invalidateQueries({ queryKey: ["alert-silences"] })
    },
    onError: (e: Error) => {
      toast({ title: "Failed to unsilence", description: e.message, variant: "destructive" })
    },
  })

  function getSilenceForAlert(a: Alert): Silence | undefined {
    return silences.find(
      s => s.rule_name === a.rule_name && (!s.node_id || s.node_id === a.node_id)
    )
  }

  const filtered = applyFilters(alerts, filters)

  return (
    <div className="space-y-2">
      <FilterBar filters={filters} onChange={setFilters} rules={rules} />
      {filtered.length === 0 ? (
        <div className="flex flex-col items-center justify-center py-16 text-muted-foreground">
          <CheckCircle2 className="h-10 w-10 mb-3 opacity-30" />
          <p className="text-sm">No alerts match the current filters.</p>
        </div>
      ) : (
        <div className="rounded-md border border-border overflow-visible">
          <table className="w-full text-sm overflow-hidden">
            <thead className="bg-muted/50 border-b border-border">
              <tr>
                <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Severity</th>
                <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Rule</th>
                <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Node</th>
                <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Sensor</th>
                <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Value</th>
                <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Fired</th>
                {showSilenceExpiry && (
                  <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Silence expires</th>
                )}
                {showResolved && (
                  <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Resolved</th>
                )}
                <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Actions</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map(a => {
                const silence = getSilenceForAlert(a)
                return (
                  <tr key={a.id} className="border-b border-border last:border-0 hover:bg-muted/30">
                    <td className="px-3 py-2">
                      <SeverityPill severity={a.severity} />
                    </td>
                    <td className="px-3 py-2 font-mono text-xs">{a.rule_name}</td>
                    <td className="px-3 py-2 font-mono text-xs truncate max-w-[140px]" title={a.node_id}>
                      {a.node_id.slice(0, 8)}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs">{a.sensor}</td>
                    <td className="px-3 py-2 font-mono text-xs">
                      {a.threshold_op} {a.threshold_val} (got {a.last_value.toFixed(2)})
                    </td>
                    <td className="px-3 py-2 text-xs text-muted-foreground">{reltime(a.fired_at)}</td>
                    {showSilenceExpiry && (
                      <td className="px-3 py-2 text-xs text-muted-foreground">
                        {silence ? silenceLabel(silence) : "—"}
                      </td>
                    )}
                    {showResolved && (
                      <td className="px-3 py-2 text-xs text-muted-foreground">
                        {a.resolved_at ? reltime(a.resolved_at) : "—"}
                      </td>
                    )}
                    <td className="px-3 py-2">
                      <div className="flex items-center gap-2">
                        {!showSilenceExpiry && !showResolved && (
                          <SilenceButton
                            ruleName={a.rule_name}
                            nodeId={a.node_id}
                            onDone={onRefetch}
                          />
                        )}
                        {showSilenceExpiry && silence && (
                          <Button
                            variant="outline"
                            size="sm"
                            className="h-7 text-xs gap-1"
                            onClick={() => unsilenceMut.mutate(silence.id)}
                            disabled={unsilenceMut.isPending}
                          >
                            <X className="h-3 w-3" />
                            Unsilence
                          </Button>
                        )}
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-7 text-xs gap-1"
                          onClick={() => onViewRule(a.rule_name)}
                        >
                          <Eye className="h-3 w-3" />
                          View rule
                        </Button>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

// ─── History tab ──────────────────────────────────────────────────────────────

function HistoryTab({ silences, rules, onViewRule }: { silences: Silence[]; rules: Rule[]; onViewRule: (name: string) => void }) {
  const [since, setSince] = React.useState<string>(() => {
    const d = new Date()
    d.setDate(d.getDate() - 1)
    return d.toISOString().slice(0, 10)
  })
  const [until, setUntil] = React.useState<string>(() => new Date().toISOString().slice(0, 10))

  // Build query params for history.
  const query = useQuery<AlertsResponse>({
    queryKey: ["alerts-history", since, until],
    queryFn: () =>
      apiFetch<AlertsResponse>(`/api/v1/alerts?state=resolved`),
    refetchInterval: 30_000,
  })

  // Client-side date filter on top of the resolved results.
  const sinceTs = new Date(since).getTime()
  const untilTs = new Date(until).getTime() + 86_400_000 // end of day
  const filtered = (query.data?.recent ?? []).filter(a => {
    if (!a.resolved_at) return false
    const t = new Date(a.resolved_at).getTime()
    return t >= sinceTs && t <= untilTs
  })

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <label className="text-xs text-muted-foreground">From</label>
        <input
          type="date"
          value={since}
          onChange={e => setSince(e.target.value)}
          className="h-7 rounded-md border border-border bg-background text-xs px-2 text-foreground"
        />
        <label className="text-xs text-muted-foreground">To</label>
        <input
          type="date"
          value={until}
          onChange={e => setUntil(e.target.value)}
          className="h-7 rounded-md border border-border bg-background text-xs px-2 text-foreground"
        />
      </div>
      {query.isPending ? (
        <div className="space-y-2">{Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-9 w-full" />)}</div>
      ) : query.isError ? (
        <div className="text-sm text-destructive">Failed to load history.</div>
      ) : (
        <AlertTable
          alerts={filtered}
          silences={silences}
          rules={rules}
          showResolved
          onViewRule={onViewRule}
          onRefetch={() => query.refetch()}
        />
      )}
    </div>
  )
}

// ─── Test exports ─────────────────────────────────────────────────────────────
// Exported for unit testing only.  Do not use in production code.
export { SilenceButton as _SilenceButton }

// ─── Main page ────────────────────────────────────────────────────────────────

export function AlertsPage() {
  const [viewRule, setViewRule] = React.useState<string | null>(null)

  const alertsQuery = useQuery<AlertsResponse>({
    queryKey: ["alerts"],
    queryFn: () => apiFetch<AlertsResponse>("/api/v1/alerts"),
    refetchInterval: 5_000,
  })

  const silencesQuery = useQuery<SilencesResponse>({
    queryKey: ["alert-silences"],
    queryFn: () => apiFetch<SilencesResponse>("/api/v1/alerts/silences"),
    refetchInterval: 10_000,
  })

  const rulesQuery = useQuery<RulesResponse>({
    queryKey: ["alert-rules"],
    queryFn: () => apiFetch<RulesResponse>("/api/v1/alerts/rules"),
    staleTime: 60_000,
  })

  const active = alertsQuery.data?.active ?? []
  const silences = silencesQuery.data?.silences ?? []
  const rules = rulesQuery.data?.rules ?? []

  // Silenced alerts: active alerts that have a matching silence.
  const silencedAlerts = active.filter(a =>
    silences.some(s => s.rule_name === a.rule_name && (!s.node_id || s.node_id === a.node_id))
  )
  const firingAlerts = active.filter(a => !silencedAlerts.includes(a))

  const selectedRule = rules.find(r => r.name === viewRule) ?? null

  return (
    <SectionErrorBoundary section="Alerts">
      <div className="p-6 max-w-7xl mx-auto">
        {/* Header */}
        <div className="flex items-center justify-between mb-6">
          <div className="flex items-center gap-3">
            <Bell className="h-5 w-5 text-muted-foreground" />
            <h1 className="text-lg font-semibold">Alerts</h1>
            {alertsQuery.isPending ? (
              <Skeleton className="h-5 w-8 rounded-full" />
            ) : (
              firingAlerts.length > 0 && (
                <Badge variant="destructive" className="text-xs">
                  {firingAlerts.length} firing
                </Badge>
              )
            )}
          </div>
          <Button
            variant="ghost"
            size="sm"
            className="gap-2 text-xs"
            onClick={() => {
              alertsQuery.refetch()
              silencesQuery.refetch()
            }}
            disabled={alertsQuery.isFetching}
          >
            <RefreshCw className={cn("h-3.5 w-3.5", alertsQuery.isFetching && "animate-spin")} />
            {alertsQuery.isFetching ? "Refreshing…" : "Refresh"}
          </Button>
        </div>

        <Tabs defaultValue="active">
          <TabsList className="mb-4">
            <TabsTrigger value="active">
              Active
              {firingAlerts.length > 0 && (
                <span className="ml-1.5 rounded-full bg-status-error text-white text-xs px-1.5 py-0.5 leading-none">
                  {firingAlerts.length}
                </span>
              )}
            </TabsTrigger>
            <TabsTrigger value="silenced">
              Silenced
              {silencedAlerts.length > 0 && (
                <span className="ml-1.5 rounded-full bg-muted text-foreground text-xs px-1.5 py-0.5 leading-none">
                  {silencedAlerts.length}
                </span>
              )}
            </TabsTrigger>
            <TabsTrigger value="history">History</TabsTrigger>
          </TabsList>

          {/* Active */}
          <TabsContent value="active">
            {alertsQuery.isPending ? (
              <div className="space-y-2">{Array.from({ length: 5 }).map((_, i) => <Skeleton key={i} className="h-9 w-full" />)}</div>
            ) : alertsQuery.isError ? (
              <div className="flex flex-col items-center py-12 text-muted-foreground gap-2">
                <XCircle className="h-8 w-8 opacity-40" />
                <p className="text-sm">Failed to load alerts. Is the alert engine running?</p>
              </div>
            ) : firingAlerts.length === 0 ? (
              <div className="flex flex-col items-center py-16 text-muted-foreground gap-3">
                <CheckCircle2 className="h-12 w-12 opacity-20" />
                <p className="text-sm font-medium">No active alerts</p>
                <p className="text-xs opacity-60">The cluster looks healthy.</p>
              </div>
            ) : (
              <AlertTable
                alerts={firingAlerts}
                silences={silences}
                rules={rules}
                onViewRule={setViewRule}
                onRefetch={() => alertsQuery.refetch()}
              />
            )}
          </TabsContent>

          {/* Silenced */}
          <TabsContent value="silenced">
            {silencedAlerts.length === 0 ? (
              <div className="flex flex-col items-center py-16 text-muted-foreground gap-3">
                <VolumeX className="h-12 w-12 opacity-20" />
                <p className="text-sm">No silenced alerts.</p>
              </div>
            ) : (
              <AlertTable
                alerts={silencedAlerts}
                silences={silences}
                rules={rules}
                showSilenceExpiry
                onViewRule={setViewRule}
                onRefetch={() => alertsQuery.refetch()}
              />
            )}
          </TabsContent>

          {/* History */}
          <TabsContent value="history">
            <HistoryTab silences={silences} rules={rules} onViewRule={setViewRule} />
          </TabsContent>
        </Tabs>

        {/* Rule drill-down modal */}
        {selectedRule && (
          <RuleModal rule={selectedRule} onClose={() => setViewRule(null)} />
        )}
        {viewRule && !selectedRule && (
          <Dialog open onOpenChange={v => !v && setViewRule(null)}>
            <DialogContent>
              <DialogHeader><DialogTitle>Rule: {viewRule}</DialogTitle></DialogHeader>
              <div className="flex items-center gap-2 text-sm text-muted-foreground py-4">
                <Loader2 className="h-4 w-4 animate-spin" />
                {rulesQuery.isPending ? "Loading rule…" : `Rule "${viewRule}" not found in loaded rules. It may have been removed from rules.d/.`}
              </div>
            </DialogContent>
          </Dialog>
        )}
      </div>
    </SectionErrorBoundary>
  )
}
