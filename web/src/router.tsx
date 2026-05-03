import { createRouter, createRoute, createRootRoute, redirect, Outlet } from "@tanstack/react-router"
import { AppShell } from "@/components/AppShell"
import { LoginPage } from "@/routes/login"
import { SetupPage } from "@/routes/setup"
import { SetPasswordPage } from "@/routes/set-password"
import { NodesPage } from "@/routes/nodes"
import { ImagesPage } from "@/routes/images"
import { ActivityPage } from "@/routes/activity"
import { SettingsPage } from "@/routes/settings"
import { IdentityPage } from "@/routes/identity"
import { SlurmPage } from "@/routes/slurm"
import { AlertsPage } from "@/routes/alerts"
import { DatacenterPage } from "@/routes/datacenter"
import { ControlPlanePage } from "@/routes/control-plane"
import { SessionGate } from "@/components/SessionGate"

const rootRoute = createRootRoute({
  component: Outlet,
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: LoginPage,
  validateSearch: (search: Record<string, unknown>) => ({
    firstrun: typeof search.firstrun === "string" ? search.firstrun : undefined,
  }),
})

const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  component: SetupPage,
})

const setPasswordRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/set-password",
  component: SetPasswordPage,
})

// The protected layout wraps all authenticated routes in SessionGate, which
// redirects to /login when unauthed, /setup when setup_required, and
// /set-password when force_password_change cookie is present.
const protectedLayout = createRoute({
  getParentRoute: () => rootRoute,
  id: "protected",
  component: () => (
    <SessionGate>
      <AppShell>
        <Outlet />
      </AppShell>
    </SessionGate>
  ),
})

const indexRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/nodes", search: { q: undefined, status: undefined, sort: undefined, dir: undefined, openNode: undefined, reimage: undefined, addNode: undefined, deleteNode: undefined, tag: undefined, view: undefined, createGroup: undefined } })
  },
})

const nodesRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/nodes",
  component: NodesPage,
  validateSearch: (search: Record<string, unknown>) => ({
    q: typeof search.q === "string" ? search.q : undefined,
    status: typeof search.status === "string" ? search.status : undefined,
    sort: typeof search.sort === "string" ? search.sort : undefined,
    dir:
      search.dir === "asc" || search.dir === "desc"
        ? (search.dir as "asc" | "desc")
        : undefined,
    // PAL-2-2: open a specific node's detail sheet (with optional reimage panel)
    openNode: typeof search.openNode === "string" ? search.openNode : undefined,
    reimage: typeof search.reimage === "string" ? search.reimage : undefined,
    // NODE-CREATE-5: open AddNode sheet from Cmd-K
    addNode: typeof search.addNode === "string" ? search.addNode : undefined,
    // NODE-DEL-4: open node detail sheet with delete confirm pre-expanded (Cmd-K "Delete node…")
    deleteNode: typeof search.deleteNode === "string" ? search.deleteNode : undefined,
    // TAG-4: one or more key:value tag filters (AND semantics). Serialised as
    // repeated ?tag= params; TanStack Router coerces a single value to a string
    // and multiple values to an array — normalise both cases to string[].
    tag: Array.isArray(search.tag)
      ? (search.tag as string[]).filter((v) => typeof v === "string")
      : typeof search.tag === "string"
        ? [search.tag]
        : undefined,
    // GRP-2: toggle between "nodes" and "groups" view
    view:
      search.view === "groups"
        ? ("groups" as const)
        : undefined,
    // GRP-5: open create group sheet from Cmd-K
    createGroup: typeof search.createGroup === "string" ? search.createGroup : undefined,
  }),
})

const imagesRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/images",
  component: ImagesPage,
  validateSearch: (search: Record<string, unknown>) => ({
    q: typeof search.q === "string" ? search.q : undefined,
    tab: typeof search.tab === "string" ? search.tab : undefined,
    sort: typeof search.sort === "string" ? search.sort : undefined,
    dir: search.dir === "asc" || search.dir === "desc" ? (search.dir as "asc" | "desc") : undefined,
    // IMG-URL-6: open AddImageSheet from Cmd-K
    addImage: typeof search.addImage === "string" ? search.addImage : undefined,
  }),
})

const activityRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/activity",
  component: ActivityPage,
  validateSearch: (search: Record<string, unknown>) => ({
    q: typeof search.q === "string" ? search.q : undefined,
    kind: typeof search.kind === "string" ? search.kind : undefined,
  }),
})

const settingsRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/settings",
  component: SettingsPage,
})

const identityRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/identity",
  component: IdentityPage,
})

const slurmRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/slurm",
  component: SlurmPage,
})

const alertsRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/alerts",
  component: AlertsPage,
})

const datacenterRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/datacenter",
  component: DatacenterPage,
})

const controlPlaneRoute = createRoute({
  getParentRoute: () => protectedLayout,
  path: "/control-plane",
  component: ControlPlanePage,
})

const routeTree = rootRoute.addChildren([
  loginRoute,
  setupRoute,
  setPasswordRoute,
  protectedLayout.addChildren([
    indexRoute,
    nodesRoute,
    imagesRoute,
    activityRoute,
    settingsRoute,
    identityRoute,
    slurmRoute,
    alertsRoute,
    datacenterRoute,
    controlPlaneRoute,
  ]),
])

export const router = createRouter({
  routeTree,
  defaultPreload: "intent",
})

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router
  }
}
