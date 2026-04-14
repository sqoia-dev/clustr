// app.js — clonr-serverd web UI. Hash-based SPA routing, no frameworks.

// ─── Router ───────────────────────────────────────────────────────────────

const Router = {
    _routes: {},
    _current: null,
    _refreshTimer: null,

    register(hash, handler) {
        this._routes[hash] = handler;
    },

    start() {
        window.addEventListener('hashchange', () => this._navigate());
        this._navigate();
    },

    navigate(hash) {
        window.location.hash = hash;
    },

    _navigate() {
        const hash = window.location.hash.replace(/^#/, '') || '/';
        // Stop any running auto-refresh from the previous page.
        if (this._refreshTimer) { clearInterval(this._refreshTimer); this._refreshTimer = null; }
        // Disconnect any active log stream.
        if (App._logStream) { App._logStream.disconnect(); App._logStream = null; }
        // Disconnect any active progress stream.
        if (App._progressStream) { App._progressStream.disconnect(); App._progressStream = null; }
        // Close any ISO build SSE stream and elapsed timer.
        if (Pages._isoBuildSSE) { Pages._isoBuildSSE.close(); Pages._isoBuildSSE = null; }
        if (Pages._isoBuildElapsedTimer) { clearInterval(Pages._isoBuildElapsedTimer); Pages._isoBuildElapsedTimer = null; }
        // Remove node detail page click listener for actions dropdown.
        if (Pages._closeActionsDropdownOnOutsideClick) {
            document.removeEventListener('click', Pages._closeActionsDropdownOnOutsideClick);
        }

        // Match exact or prefix.
        let handler = this._routes[hash];
        if (!handler) {
            for (const key of Object.keys(this._routes)) {
                if (hash.startsWith(key + '/')) { handler = this._routes[key + '/*']; break; }
            }
        }
        if (!handler) handler = this._routes['/'];

        // Update sidebar nav active state.
        document.querySelectorAll('.nav-item').forEach(a => {
            const href = a.getAttribute('href').replace(/^#/, '');
            a.classList.toggle('active', hash === href || (href !== '/' && hash.startsWith(href)));
        });

        this._current = hash;
        handler(hash);
    },
};

// ─── App state ────────────────────────────────────────────────────────────

const App = {
    _logStream: null,
    _mainEl: null,

    // Simple short-lived data cache to avoid redundant fetches across refresh cycles.
    // Structure: { key: { data, expiresAt } }
    _cache: {},

    _cacheGet(key) {
        const entry = this._cache[key];
        if (!entry || Date.now() > entry.expiresAt) return null;
        return entry.data;
    },

    _cacheSet(key, data, ttlMs = 2000) {
        this._cache[key] = { data, expiresAt: Date.now() + ttlMs };
    },

    // toast shows a transient notification at the bottom-right of the screen.
    // kind: "success" | "error" | "info" (default info)
    // Auto-dismisses after 4 seconds.
    toast(message, kind = 'info') {
        let container = document.getElementById('toast-container');
        if (!container) {
            container = document.createElement('div');
            container.id = 'toast-container';
            container.style.cssText = 'position:fixed;bottom:20px;right:20px;z-index:9999;display:flex;flex-direction:column;gap:8px;pointer-events:none';
            document.body.appendChild(container);
        }
        const toast = document.createElement('div');
        const colors = {
            success: { bg: '#10b981', icon: '✓' },
            error:   { bg: '#ef4444', icon: '✕' },
            info:    { bg: '#3b82f6', icon: 'ℹ' },
        };
        const c = colors[kind] || colors.info;
        toast.style.cssText = `background:${c.bg};color:white;padding:12px 16px;border-radius:8px;box-shadow:0 4px 12px rgba(0,0,0,0.15);font-size:14px;font-weight:500;min-width:280px;max-width:420px;display:flex;align-items:center;gap:10px;pointer-events:auto;animation:toastIn 0.2s ease-out`;
        toast.innerHTML = `<span style="font-size:18px;font-weight:bold">${c.icon}</span><span style="flex:1">${escHtml(message)}</span><span style="cursor:pointer;opacity:0.7;padding:0 4px" onclick="this.parentElement.remove()">×</span>`;
        container.appendChild(toast);
        setTimeout(() => {
            toast.style.animation = 'toastOut 0.2s ease-in forwards';
            setTimeout(() => toast.remove(), 200);
        }, 4000);
    },

    init() {
        this._mainEl = document.getElementById('main-content');
        this._initRoutes();
        this._watchHealth();
        Router.start();
    },

    _initRoutes() {
        Router.register('/',        ()    => Pages.dashboard());
        Router.register('/images',  (h)   => {
            const parts = h.split('/');
            if (parts.length === 3 && parts[2]) Pages.imageDetail(parts[2]);
            else Pages.images();
        });
        Router.register('/images/*', (h)  => {
            const parts = h.split('/');
            Pages.imageDetail(parts[2]);
        });
        Router.register('/nodes',   (h)   => {
            const parts = h.split('/');
            if (parts.length === 3 && parts[2] === 'groups') Pages.nodeGroups();
            else if (parts.length === 4 && parts[2] === 'groups' && parts[3]) Pages.nodeGroupDetail(parts[3]);
            else if (parts.length === 3 && parts[2]) Pages.nodeDetail(parts[2]);
            else Pages.nodes();
        });
        Router.register('/nodes/*', (h)   => {
            const parts = h.split('/');
            if (parts[2] === 'groups' && parts[3]) Pages.nodeGroupDetail(parts[3]);
            else if (parts[2] === 'groups') Pages.nodeGroups();
            else Pages.nodeDetail(parts[2]);
        });
        Router.register('/logs',    ()    => Pages.logs());
        Router.register('/settings', ()   => Pages.settings());
    },

    render(html) {
        this._mainEl.innerHTML = `<div class="page-enter">${html}</div>`;
    },

    setAutoRefresh(fn, intervalMs = 30000) {
        if (Router._refreshTimer) clearInterval(Router._refreshTimer);
        Router._refreshTimer = setInterval(fn, intervalMs);
    },

    _watchHealth() {
        const dot   = document.getElementById('health-dot');
        const label = document.getElementById('health-label');
        const check = async () => {
            try {
                await API.health.get();
                if (dot)   { dot.classList.remove('offline'); }
                if (label) { label.textContent = 'online'; }
            } catch (_) {
                if (dot)   { dot.classList.add('offline'); }
                if (label) { label.textContent = 'offline'; }
            }
        };

        // Session expiry warning: poll /auth/me every 60s; if TTL < 600s show banner.
        const checkSession = async () => {
            const banner = document.getElementById('session-expiry-banner');
            if (!banner) return;
            try {
                const me = await fetch('/api/v1/auth/me', { credentials: 'same-origin' });
                if (!me.ok) { banner.style.display = 'none'; return; }
                const data = await me.json();
                if (!data.expires_at) { banner.style.display = 'none'; return; }
                const expiresAt = new Date(data.expires_at);
                const ttlSecs = Math.floor((expiresAt - Date.now()) / 1000);
                if (ttlSecs < 600) {
                    const mins = Math.max(1, Math.ceil(ttlSecs / 60));
                    banner.textContent = `Session expires in ${mins} minute${mins === 1 ? '' : 's'} — click to extend`;
                    banner.style.display = 'block';
                } else {
                    banner.style.display = 'none';
                }
            } catch (_) {
                if (banner) banner.style.display = 'none';
            }
        };

        check();
        checkSession();
        setInterval(check, 30000);
        setInterval(checkSession, 60000);
    },
};

// ─── Shared UI helpers ────────────────────────────────────────────────────

function loading(msg = 'Loading…') {
    return `<div class="loading"><div class="spinner"></div>${escHtml(msg)}</div>`;
}

function alertBox(msg, type = 'error') {
    return `<div class="alert alert-${type}">${escHtml(msg)}</div>`;
}

function badge(status) {
    const cls = {
        ready:    'badge-ready',
        building: 'badge-building',
        error:    'badge-error',
        archived: 'badge-archived',
    }[status] || 'badge-archived';
    return `<span class="badge ${cls}">${escHtml(status)}</span>`;
}

function nodeBadge(node) {
    if (node._deployStatus === 'success') return `<span class="badge badge-deployed">Deployed</span>`;
    if (node._deployStatus === 'error')   return `<span class="badge badge-error">Failed</span>`;
    if (node.base_image_id)               return `<span class="badge badge-info">Configured</span>`;
    if (node.hardware_profile)            return `<span class="badge badge-warning">Registered</span>`;
    return `<span class="badge badge-neutral">Unconfigured</span>`;
}

function fmtBytes(bytes) {
    if (!bytes || bytes === 0) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0, n = bytes;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

function fmtDate(ts) {
    if (!ts) return '—';
    return new Date(ts).toLocaleString('en-US', { month: 'short', day: 'numeric', year: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
}

function fmtDateShort(ts) {
    if (!ts) return '—';
    return new Date(ts).toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
}

function fmtRelative(ts) {
    if (!ts) return '—';
    const diff = Date.now() - new Date(ts).getTime();
    const s = Math.floor(diff / 1000);
    if (s < 60)  return `${s}s ago`;
    const m = Math.floor(s / 60);
    if (m < 60)  return `${m}m ago`;
    const h = Math.floor(m / 60);
    if (h < 24)  return `${h}h ago`;
    return fmtDateShort(ts);
}

function cardWrap(title, body, actions = '') {
    return `
        <div class="card">
            <div class="card-header">
                <span class="card-title">${title}</span>
                <div class="flex gap-8">${actions}</div>
            </div>
            <div>${body}</div>
        </div>`;
}

function emptyState(title, sub = '', cta = '') {
    return `<div class="empty-state">
        <div class="empty-state-icon">
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                <circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/>
            </svg>
        </div>
        <div class="empty-state-title">${escHtml(title)}</div>
        ${sub ? `<div class="empty-state-text">${escHtml(sub)}</div>` : ''}
        ${cta ? cta : ''}
    </div>`;
}

// Returns HTML for a hostname cell: italic "Unassigned" + MAC below when hostname is
// absent, empty, null, or the literal server placeholder "(none)".
function fmtHostname(hostname, mac) {
    const isUnset = !hostname || hostname === '(none)';
    const macHtml = mac ? `<div class="text-dim text-sm text-mono">${escHtml(mac)}</div>` : '';
    if (isUnset) {
        return `<span class="text-muted" style="font-style:italic">Unassigned</span>${macHtml}`;
    }
    return `<span style="font-weight:600">${escHtml(hostname)}</span>${macHtml}`;
}

// ─── Deployment phase helpers ──────────────────────────────────────────────

const PHASE_BADGE = {
    complete:     'badge-ready',
    downloading:  'badge-info',
    extracting:   'badge-info',
    partitioning: 'badge-info',
    formatting:   'badge-info',
    finalizing:   'badge-info',
    preflight:    'badge-info',
    error:        'badge-error',
    waiting:      'badge-neutral',
};

function phaseBadge(phase) {
    const cls = PHASE_BADGE[phase] || 'badge-info';
    return `<span class="badge ${cls}">${escHtml(phase)}</span>`;
}

// fmtSpeed formats bytes/sec as a human-readable speed string.
function fmtSpeed(bps) {
    if (!bps || bps <= 0) return '—';
    if (bps >= 1024 * 1024) return `${(bps / (1024 * 1024)).toFixed(1)} MB/s`;
    if (bps >= 1024) return `${(bps / 1024).toFixed(1)} KB/s`;
    return `${bps} B/s`;
}

// fmtETA formats remaining seconds as mm:ss.
function fmtETA(secs) {
    if (!secs || secs <= 0) return '—';
    const m = Math.floor(secs / 60);
    const s = secs % 60;
    return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

// ─── Pages ────────────────────────────────────────────────────────────────

const Pages = {

    // ── Dashboard ──────────────────────────────────────────────────────────

    // _dashDeployMap persists the deployment map across refresh cycles so the
    // ProgressStream SSE updates are not lost when dashboardRefresh() runs.
    _dashDeployMap: null,

    async dashboard() {
        App.render(loading('Loading dashboard…'));

        try {
            const [imagesResp, nodesResp, progressEntries] = await Promise.all([
                API.images.list(),
                API.nodes.list(),
                API.progress.list().catch(() => []),
            ]);

            const images = imagesResp.images || [];
            const nodes  = nodesResp.nodes   || [];

            // Warm the cache so the first auto-refresh is instant.
            App._cacheSet('images', images);
            App._cacheSet('nodes', nodes);

            const ready    = images.filter(i => i.status === 'ready').length;
            const building = images.filter(i => i.status === 'building').length;
            const errored  = images.filter(i => i.status === 'error').length;
            const configured = nodes.filter(n => n.base_image_id).length;

            // Build a MAC → progress map for live updates. Stored on Pages so
            // dashboardRefresh() can reuse and mutate it without losing SSE state.
            const deployMap = new Map();
            (progressEntries || []).forEach(p => deployMap.set(p.node_mac, p));
            Pages._dashDeployMap = deployMap;

            const recentActivity = this._buildRecentActivity(images, nodes);

            App.render(`
                <div class="page-header">
                    <div>
                        <div class="page-title">Dashboard</div>
                        <div class="page-subtitle">System overview and active deployments</div>
                    </div>
                    <div class="flex gap-8">
                        <button class="btn btn-secondary" onclick="Router.navigate('/logs')">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/>
                            </svg>
                            View Logs
                        </button>
                        <button class="btn btn-primary" onclick="Router.navigate('/images')">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
                            </svg>
                            Pull Image
                        </button>
                    </div>
                </div>

                <div class="stats-grid">
                    <div class="stat-card">
                        <div class="stat-icon stat-icon-blue">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/>
                            </svg>
                        </div>
                        <div class="stat-label">Total Images</div>
                        <div class="stat-value" id="dash-images-count">${images.length}</div>
                        <div class="stat-sub" id="dash-images-sub">
                            <span class="text-success">${ready} ready</span>
                            ${building > 0 ? ` · <span class="text-accent">${building} building</span>` : ''}
                            ${errored  > 0 ? ` · <span class="text-error">${errored} error</span>` : ''}
                        </div>
                    </div>
                    <div class="stat-card">
                        <div class="stat-icon stat-icon-green">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <rect x="2" y="2" width="20" height="8" rx="2"/><rect x="2" y="14" width="20" height="8" rx="2"/>
                                <line x1="6" y1="6" x2="6.01" y2="6"/><line x1="6" y1="18" x2="6.01" y2="18"/>
                            </svg>
                        </div>
                        <div class="stat-label">Nodes</div>
                        <div class="stat-value" id="dash-nodes-count">${nodes.length}</div>
                        <div class="stat-sub" id="dash-nodes-sub">${configured} configured · ${nodes.length - configured} unconfigured</div>
                    </div>
                    <div class="stat-card">
                        <div class="stat-icon stat-icon-amber">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/>
                            </svg>
                        </div>
                        <div class="stat-label">Active Deployments</div>
                        <div class="stat-value" id="dash-active-count">${Array.from(deployMap.values()).filter(d => d.phase !== 'complete' && d.phase !== 'error').length}</div>
                        <div class="stat-sub" id="dash-complete-count">${Array.from(deployMap.values()).filter(d => d.phase === 'complete').length} completed recently</div>
                    </div>
                    <div class="stat-card">
                        <div class="stat-icon stat-icon-purple">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <path d="M12 2a10 10 0 1 0 10 10"/><polyline points="12 6 12 12 16 14"/>
                            </svg>
                        </div>
                        <div class="stat-label">System Health</div>
                        <div class="stat-value" style="font-size:18px;color:var(--success)">Online</div>
                        <div class="stat-sub">PXE · API · Logs</div>
                    </div>
                </div>

                ${cardWrap('Active Deployments',
                    `<div id="deploy-progress-container">${this._deployProgressTable(deployMap)}</div>`)}

                <div style="display:grid;grid-template-columns:1fr 1fr;gap:20px;margin-bottom:20px">
                    ${cardWrap('Recent Images',
                        `<div id="dash-recent-images-wrap">${this._imagesTable(images.slice(0, 6))}</div>`,
                        `<a href="#/images" class="btn btn-secondary btn-sm">View all</a>`)}
                    ${cardWrap('Recent Nodes',
                        `<div id="dash-recent-nodes-wrap">${this._nodesTable(nodes.slice(0, 6))}</div>`,
                        `<a href="#/nodes" class="btn btn-secondary btn-sm">View all</a>`)}
                </div>

                ${recentActivity.length > 0 ? cardWrap('Recent Activity',
                    this._activityTimeline(recentActivity),
                    '') : ''}

                ${cardWrap('Live Log Stream',
                    `<div id="dash-log-viewer" class="log-viewer"></div>`,
                    `<span class="follow-indicator" id="dash-follow-ind">
                        <span class="follow-dot"></span>connecting…
                    </span>`)}
            `);

            const viewer = document.getElementById('dash-log-viewer');
            if (viewer) {
                const stream = new LogStream(viewer);
                // Pre-load the last 50 log entries so the viewer shows history
                // before any new SSE events arrive.
                try {
                    const resp = await API.logs.query({ limit: 50 });
                    const entries = (resp.logs || []).reverse(); // oldest-first for natural scroll
                    if (entries.length > 0) {
                        stream.loadEntries(entries);
                    } else {
                        viewer.innerHTML = '<div class="empty-state" style="padding:20px"><div class="empty-state-text">Waiting for log events…</div></div>';
                    }
                } catch (_) {
                    viewer.innerHTML = '<div class="empty-state" style="padding:20px"><div class="empty-state-text">Waiting for log events…</div></div>';
                }
                stream.onConnect(() => {
                    const ind = document.getElementById('dash-follow-ind');
                    if (ind) { ind.className = 'follow-indicator live'; ind.innerHTML = '<span class="follow-dot"></span>Live'; }
                    // Clear empty-state placeholder if present
                    const es = viewer.querySelector('.empty-state');
                    if (es) es.remove();
                });
                stream.onDisconnect((permanent) => {
                    const ind = document.getElementById('dash-follow-ind');
                    if (permanent) {
                        if (ind) { ind.className = 'follow-indicator'; ind.innerHTML = '<span class="follow-dot"></span>unavailable'; }
                        const viewer = document.getElementById('dash-log-viewer');
                        if (viewer && !viewer.children.length) {
                            viewer.innerHTML = '<div class="empty-state" style="padding:30px"><div class="empty-state-text">Live log stream unavailable</div></div>';
                        }
                    } else {
                        if (ind) { ind.className = 'follow-indicator'; ind.innerHTML = '<span class="follow-dot"></span>Reconnecting…'; }
                    }
                });
                stream.connect();
                App._logStream = stream;
            }

            // ── Real-time deployment progress via SSE ──────────────────────
            App._progressStream = new ProgressStream(deployMap, () => {
                const container = document.getElementById('deploy-progress-container');
                if (container) container.innerHTML = Pages._deployProgressTable(deployMap);
                // Update stats counters.
                const activeCount = document.getElementById('dash-active-count');
                const completeCount = document.getElementById('dash-complete-count');
                if (activeCount) activeCount.textContent = Array.from(deployMap.values()).filter(d => d.phase !== 'complete' && d.phase !== 'error').length;
                if (completeCount) completeCount.textContent = Array.from(deployMap.values()).filter(d => d.phase === 'complete').length + ' completed recently';
            });
            App._progressStream.connect();

            // Incremental auto-refresh — updates data in-place, no DOM flicker.
            App.setAutoRefresh(() => Pages.dashboardRefresh());

        } catch (e) {
            App.render(alertBox(`Failed to load dashboard: ${e.message}`));
        }
    },

    // dashboardRefresh — called by the auto-refresh timer every 30 seconds.
    // Fetches fresh data and updates existing DOM elements in-place.
    // Never replaces the outer layout, the log stream, or any other stateful widget.
    async dashboardRefresh() {
        // Guard: if the dashboard DOM is gone (navigated away), do nothing.
        if (!document.getElementById('dash-images-count')) return;

        try {
            // Use cached data if still fresh (avoids thundering-herd on rapid refreshes).
            let images = App._cacheGet('images');
            let nodes  = App._cacheGet('nodes');

            const fetches = [];
            if (!images) fetches.push(API.images.list().then(r => { images = r.images || []; App._cacheSet('images', images); }));
            if (!nodes)  fetches.push(API.nodes.list().then(r  => { nodes  = r.nodes   || []; App._cacheSet('nodes',  nodes);  }));
            if (fetches.length) await Promise.all(fetches);

            // ── Stat cards ────────────────────────────────────────────────
            const ready      = images.filter(i => i.status === 'ready').length;
            const building   = images.filter(i => i.status === 'building').length;
            const errored    = images.filter(i => i.status === 'error').length;
            const configured = nodes.filter(n => n.base_image_id).length;

            const imagesCount = document.getElementById('dash-images-count');
            const imagesSub   = document.getElementById('dash-images-sub');
            const nodesCount  = document.getElementById('dash-nodes-count');
            const nodesSub    = document.getElementById('dash-nodes-sub');

            if (imagesCount) imagesCount.textContent = images.length;
            if (imagesSub) {
                let sub = `<span class="text-success">${ready} ready</span>`;
                if (building > 0) sub += ` · <span class="text-accent">${building} building</span>`;
                if (errored  > 0) sub += ` · <span class="text-error">${errored} error</span>`;
                imagesSub.innerHTML = sub;
            }
            if (nodesCount) nodesCount.textContent = nodes.length;
            if (nodesSub)   nodesSub.textContent   = `${configured} configured · ${nodes.length - configured} unconfigured`;

            // ── Recent Images table (diff rows, no full replace) ──────────
            const imagesWrap = document.getElementById('dash-recent-images-wrap');
            if (imagesWrap) {
                this._diffTable(imagesWrap, images.slice(0, 6), 'id', (img) => {
                    const tr = document.createElement('tr');
                    tr.className = 'clickable';
                    tr.dataset.key = img.id;
                    tr.onclick = () => Router.navigate(`/images/${img.id}`);
                    tr.innerHTML = `
                        <td>
                            <span style="font-weight:500">${escHtml(img.name)}</span>
                            ${img.version ? `<span class="text-dim text-sm"> v${escHtml(img.version)}</span>` : ''}
                        </td>
                        <td>
                            ${img.os   ? `<span class="badge badge-neutral badge-sm">${escHtml(img.os)}</span> ` : ''}
                            ${img.arch ? `<span class="badge badge-neutral badge-sm">${escHtml(img.arch)}</span>` : ''}
                            ${!img.os && !img.arch ? '<span class="text-dim">—</span>' : ''}
                        </td>
                        <td>${badge(img.status)}</td>
                        <td class="text-dim text-mono text-sm">${fmtBytes(img.size_bytes)}</td>`;
                    return tr;
                }, (tr, img) => {
                    // Update status badge and size in place — name/os/arch don't change.
                    const cells = tr.querySelectorAll('td');
                    if (cells[2]) cells[2].innerHTML = badge(img.status);
                    if (cells[3]) cells[3].textContent = fmtBytes(img.size_bytes);
                });
            }

            // ── Recent Nodes table (diff rows) ────────────────────────────
            const nodesWrap = document.getElementById('dash-recent-nodes-wrap');
            if (nodesWrap) {
                this._diffTable(nodesWrap, nodes.slice(0, 6), 'id', (n) => {
                    const tr = document.createElement('tr');
                    tr.className = 'clickable';
                    tr.dataset.key = n.id;
                    tr.onclick = () => Router.navigate(`/nodes/${n.id}`);
                    tr.innerHTML = `
                        <td>
                            ${(n.hostname && n.hostname !== '(none)')
                                ? `<span style="font-weight:500">${escHtml(n.hostname)}</span>`
                                : `<span class="text-dim" style="font-style:italic">Unassigned</span>`}
                            <div class="text-dim text-sm text-mono">${escHtml(n.primary_mac || '—')}</div>
                        </td>
                        <td>${nodeBadge(n)}</td>
                        <td class="text-dim text-sm">${fmtRelative(n.updated_at)}</td>`;
                    return tr;
                }, (tr, n) => {
                    const cells = tr.querySelectorAll('td');
                    if (cells[1]) cells[1].innerHTML = nodeBadge(n);
                    if (cells[2]) cells[2].textContent = fmtRelative(n.updated_at);
                });
            }

        } catch (_) {
            // Silently swallow refresh errors — the user is still on a functional page.
            // The next tick will retry automatically.
        }
    },

    // _diffTable reconciles the rows inside a table container (which holds a
    // .table-wrap > table > tbody) against a new data array.
    // - keyField: the property name used as the row identity key (matches data-key)
    // - createRow(item): returns a new <tr> element with data-key set
    // - updateRow(tr, item): mutates an existing <tr> in-place with fresh values
    _diffTable(container, newData, keyField, createRow, updateRow) {
        const tbody = container.querySelector('tbody');
        // If the table structure doesn't exist yet (e.g. was empty-state), rebuild fully.
        if (!tbody) {
            if (newData.length === 0) return; // leave empty-state as-is
            // Can't diff without a tbody — let the next full navigation render handle it.
            return;
        }

        const existing = new Map();
        tbody.querySelectorAll('[data-key]').forEach(el => existing.set(el.dataset.key, el));

        // Track insertion order so rows stay sorted consistently.
        for (const item of newData) {
            const key = String(item[keyField]);
            if (existing.has(key)) {
                updateRow(existing.get(key), item);
                existing.delete(key);
            } else {
                tbody.appendChild(createRow(item));
            }
        }

        // Remove rows that are no longer in the data set.
        for (const [, el] of existing) el.remove();
    },

    // _deployProgressTable renders the active deployments table from a MAC → DeployProgress map.
    _deployProgressTable(deployMap) {
        const entries = Array.from(deployMap.values())
            .sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at))
            .slice(0, 20);

        if (!entries.length) return emptyState('No active deployments');

        return `<div class="table-wrap"><table>
            <thead><tr>
                <th>Node</th><th>Phase</th><th>Progress</th><th>Speed</th><th>ETA</th><th>Updated</th>
            </tr></thead>
            <tbody>
            ${entries.map(p => {
                const pct = p.bytes_total > 0 ? Math.min(100, Math.round(p.bytes_done / p.bytes_total * 100)) : 0;
                const displayName = fmtHostname(p.hostname, p.node_mac);
                const barClass = p.phase === 'complete' ? 'complete' : p.phase === 'error' ? 'error' : '';

                let progressCell;
                if (p.phase === 'complete') {
                    progressCell = `<span style="color:var(--success);font-weight:600">&#10003; Done</span>`;
                } else if (p.phase === 'error') {
                    progressCell = `<span style="color:var(--error)" title="${escHtml(p.error || '')}">&#10007; Error${p.error ? ': ' + escHtml(p.error.slice(0, 60)) : ''}</span>`;
                } else if (p.bytes_total > 0) {
                    progressCell = `<div style="display:flex;align-items:center;gap:8px">
                        <div class="progress-bar-wrap" style="min-width:120px">
                            <div class="progress-bar-fill ${barClass}" style="width:${pct}%"></div>
                        </div>
                        <span class="text-dim text-sm" style="white-space:nowrap">${pct}% &nbsp;${fmtBytes(p.bytes_done)} / ${fmtBytes(p.bytes_total)}</span>
                    </div>`;
                } else {
                    progressCell = `<span class="text-dim text-sm">—</span>`;
                }

                return `<tr data-mac="${escHtml(p.node_mac)}">
                    <td>${displayName}</td>
                    <td>${phaseBadge(p.phase)}</td>
                    <td>${progressCell}</td>
                    <td class="text-dim text-sm">${fmtSpeed(p.speed_bps)}</td>
                    <td class="text-dim text-sm">${p.phase !== 'complete' && p.phase !== 'error' ? fmtETA(p.eta_seconds) : '—'}</td>
                    <td class="text-dim text-sm">${fmtRelative(p.updated_at)}</td>
                </tr>`;
            }).join('')}
            </tbody>
        </table></div>`;
    },

    _buildRecentActivity(images, nodes) {
        const items = [];
        images.slice(0, 4).forEach(img => {
            items.push({ type: 'image', ts: img.created_at, data: img });
        });
        nodes.slice(0, 4).forEach(n => {
            items.push({ type: 'node', ts: n.created_at, data: n });
        });
        return items.sort((a, b) => new Date(b.ts) - new Date(a.ts)).slice(0, 8);
    },

    _activityTimeline(items) {
        return `<div class="timeline">` + items.map(item => {
            if (item.type === 'image') {
                const img = item.data;
                return `<div class="timeline-item">
                    <div class="timeline-icon timeline-icon-blue">
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                            <polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/>
                        </svg>
                    </div>
                    <div class="timeline-body">
                        <div class="timeline-title">Image <a href="#/images/${img.id}" class="text-accent">${escHtml(img.name)}</a> ${badge(img.status)}</div>
                        <div class="timeline-ts">${fmtRelative(item.ts)}</div>
                    </div>
                </div>`;
            } else {
                const n = item.data;
                const hasHostname = n.hostname && n.hostname !== '(none)';
                const displayName = hasHostname ? n.hostname : n.primary_mac;
                const displayHtml = hasHostname
                    ? escHtml(n.hostname)
                    : `<span class="text-muted" style="font-style:italic">Unassigned</span>`;
                return `<div class="timeline-item">
                    <div class="timeline-icon timeline-icon-green">
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                            <rect x="2" y="2" width="20" height="8" rx="2"/><rect x="2" y="14" width="20" height="8" rx="2"/>
                            <line x1="6" y1="6" x2="6.01" y2="6"/><line x1="6" y1="18" x2="6.01" y2="18"/>
                        </svg>
                    </div>
                    <div class="timeline-body">
                        <div class="timeline-title">Node <a href="#/nodes/${n.id}" class="text-accent">${displayHtml}</a> registered ${nodeBadge(n)}</div>
                        <div class="timeline-ts">${fmtRelative(item.ts)}</div>
                    </div>
                </div>`;
            }
        }).join('') + `</div>`;
    },

    _imagesTable(images) {
        if (!images.length) return emptyState('No images yet', 'Pull an image from the Images page');
        return `<div class="table-wrap"><table>
            <thead><tr>
                <th>Name</th><th>OS / Arch</th><th>Status</th><th>Size</th>
            </tr></thead>
            <tbody>
            ${images.map(img => `
                <tr class="clickable" data-key="${escHtml(img.id)}" onclick="Router.navigate('/images/${img.id}')">
                    <td>
                        <span style="font-weight:500">${escHtml(img.name)}</span>
                        ${img.version ? `<span class="text-dim text-sm"> v${escHtml(img.version)}</span>` : ''}
                    </td>
                    <td>
                        ${img.os   ? `<span class="badge badge-neutral badge-sm">${escHtml(img.os)}</span> ` : ''}
                        ${img.arch ? `<span class="badge badge-neutral badge-sm">${escHtml(img.arch)}</span>` : ''}
                        ${!img.os && !img.arch ? '<span class="text-dim">—</span>' : ''}
                    </td>
                    <td>${badge(img.status)}</td>
                    <td class="text-dim text-mono text-sm">${fmtBytes(img.size_bytes)}</td>
                </tr>`).join('')}
            </tbody>
        </table></div>`;
    },

    _nodesTable(nodes) {
        if (!nodes.length) return emptyState('No nodes configured', 'Add a node from the Nodes page');
        return `<div class="table-wrap"><table>
            <thead><tr>
                <th>Host</th><th>Status</th><th>Updated</th>
            </tr></thead>
            <tbody>
            ${nodes.map(n => `
                <tr class="clickable" data-key="${escHtml(n.id)}" onclick="Router.navigate('/nodes/${n.id}')">
                    <td>
                        ${(n.hostname && n.hostname !== '(none)')
                            ? `<span style="font-weight:500">${escHtml(n.hostname)}</span>`
                            : `<span class="text-dim" style="font-style:italic">Unassigned</span>`}
                        <div class="text-dim text-sm text-mono">${escHtml(n.primary_mac || '—')}</div>
                    </td>
                    <td>${nodeBadge(n)}</td>
                    <td class="text-dim text-sm">${fmtRelative(n.updated_at)}</td>
                </tr>`).join('')}
            </tbody>
        </table></div>`;
    },

    // ── Images ─────────────────────────────────────────────────────────────

    async images() {
        App.render(loading('Loading images…'));
        try {
            const [resp, initramfsInfo] = await Promise.all([
                API.images.list(),
                API.system.initramfs().catch(() => null),
            ]);
            const images = resp.images || [];

            App.render(`
                <div class="page-header">
                    <div>
                        <div class="page-title">Images</div>
                        <div class="page-subtitle">${images.length} image${images.length !== 1 ? 's' : ''} total</div>
                    </div>
                    <div class="flex gap-8">
                        <button class="btn btn-secondary" onclick="Pages.showImportISOModal()">Import ISO</button>
                        <button class="btn btn-secondary" onclick="Pages.showBuildFromISOModal()">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <rect x="2" y="3" width="20" height="14" rx="2"/><polyline points="8 21 12 17 16 21"/>
                            </svg>
                            Build from ISO
                        </button>
                        <button class="btn btn-secondary" onclick="Pages.showCaptureModal()">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>
                            </svg>
                            Capture from Host
                        </button>
                        <button class="btn btn-primary" onclick="Pages.showPullModal()">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <line x1="12" y1="5" x2="12" y2="19"/><polyline points="19 12 12 19 5 12"/>
                            </svg>
                            Pull Image
                        </button>
                    </div>
                </div>

                ${this._initramfsCard(initramfsInfo)}

                ${images.length === 0
                    ? `<div class="card"><div class="card-body">${emptyState(
                        'No images yet',
                        'Pull your first image to get started.',
                        `<button class="btn btn-primary" onclick="Pages.showPullModal()">Pull Image</button>`
                    )}</div></div>`
                    : `<div class="image-grid">${images.map(img => this._imageCard(img)).join('')}</div>`
                }
            `);

            const hasBuilding = images.some(i => i.status === 'building' || i.status === 'interrupted');
            App.setAutoRefresh(() => Pages.images(), hasBuilding ? 5000 : 30000);

        } catch (e) {
            App.render(alertBox(`Failed to load images: ${e.message}`));
        }
    },

    // ── System initramfs card ───────────────────────────────────────────────

    _initramfsCard(info) {
        const sha = info && info.sha256 ? info.sha256.slice(0, 16) + '…' : 'not built';
        const size = info && info.size_bytes ? fmtBytes(info.size_bytes) : '—';
        const builtAt = info && info.build_time ? fmtRelative(info.build_time) : '—';
        const kernel = info && info.kernel_version ? escHtml(info.kernel_version) : '—';

        const historyRows = (info && info.history && info.history.length > 0)
            ? info.history.map(h => `<tr>
                <td class="text-mono text-sm">${escHtml(h.sha256 ? h.sha256.slice(0,16)+'…' : '—')}</td>
                <td class="text-sm">${escHtml(h.kernel_version || '—')}</td>
                <td class="text-sm">${fmtBytes(h.size_bytes)}</td>
                <td class="text-sm"><span class="badge ${h.outcome === 'success' ? 'badge-ready' : h.outcome === 'pending' ? 'badge-building' : 'badge-error'}">${escHtml(h.outcome)}</span></td>
                <td class="text-dim text-sm">${fmtRelative(h.started_at)}</td>
                <td class="text-dim text-sm">${escHtml(h.triggered_by_prefix || '—')}</td>
            </tr>`).join('')
            : `<tr><td colspan="6" class="text-dim text-sm" style="text-align:center;padding:12px">No rebuild history</td></tr>`;

        return `<div class="card" style="margin-bottom:20px;border-left:3px solid var(--accent)">
            <div class="card-header">
                <span class="card-title">System Initramfs</span>
                <div class="flex gap-8">
                    <button class="btn btn-secondary btn-sm" onclick="Pages.showRebuildInitramfsModal()">
                        Rebuild
                    </button>
                </div>
            </div>
            <div style="padding:0 20px 12px">
                <div style="display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin-bottom:16px">
                    <div>
                        <div class="text-dim text-sm">SHA256</div>
                        <div class="text-mono text-sm" style="margin-top:2px">${escHtml(sha)}</div>
                    </div>
                    <div>
                        <div class="text-dim text-sm">Size</div>
                        <div style="margin-top:2px">${size}</div>
                    </div>
                    <div>
                        <div class="text-dim text-sm">Built</div>
                        <div style="margin-top:2px">${builtAt}</div>
                    </div>
                    <div>
                        <div class="text-dim text-sm">Kernel</div>
                        <div style="margin-top:2px">${kernel}</div>
                    </div>
                </div>
                <div class="table-wrap">
                    <table style="font-size:13px">
                        <thead><tr>
                            <th>SHA256</th><th>Kernel</th><th>Size</th><th>Outcome</th><th>When</th><th>By</th>
                        </tr></thead>
                        <tbody>${historyRows}</tbody>
                    </table>
                </div>
            </div>
        </div>`;
    },

    showRebuildInitramfsModal() {
        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'rebuild-initramfs-modal';
        overlay.innerHTML = `
            <div class="modal" style="max-width:480px">
                <div class="modal-header">
                    <span class="modal-title">Rebuild System Initramfs</span>
                    <button class="modal-close" onclick="document.getElementById('rebuild-initramfs-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <p style="color:var(--text-secondary);margin-bottom:16px">
                        This will shell out to <code>scripts/build-initramfs.sh</code>, build a new initramfs image,
                        verify its sha256, and atomically replace the current one. The build takes 1–5 minutes.
                    </p>
                    <p style="color:var(--warning,#f59e0b);font-size:13px">
                        Rejected if any node has an active deployment in progress.
                    </p>
                    <div id="rebuild-log-pane" style="display:none;margin-top:16px;max-height:300px;overflow-y:auto;
                         background:var(--bg-tertiary,#0f1923);border-radius:6px;padding:12px;
                         font-family:monospace;font-size:12px;line-height:1.6;color:var(--text-secondary)">
                    </div>
                    <div id="rebuild-result" style="margin-top:12px;display:none"></div>
                </div>
                <div class="modal-footer" style="display:flex;gap:8px;justify-content:flex-end">
                    <button class="btn btn-secondary" onclick="document.getElementById('rebuild-initramfs-modal').remove()">Cancel</button>
                    <button class="btn btn-primary" id="rebuild-confirm-btn" onclick="Pages.confirmRebuildInitramfs()">Rebuild</button>
                </div>
            </div>`;
        document.body.appendChild(overlay);
    },

    async confirmRebuildInitramfs() {
        const btn = document.getElementById('rebuild-confirm-btn');
        const logPane = document.getElementById('rebuild-log-pane');
        const resultDiv = document.getElementById('rebuild-result');
        if (btn) { btn.disabled = true; btn.textContent = 'Building…'; }
        if (logPane) { logPane.style.display = 'block'; logPane.textContent = 'Starting rebuild…\n'; }

        try {
            const result = await API.system.rebuildInitramfs();
            if (logPane && result && result.log_lines) {
                logPane.textContent = result.log_lines.join('\n');
                logPane.scrollTop = logPane.scrollHeight;
            }
            if (resultDiv) {
                resultDiv.style.display = 'block';
                resultDiv.innerHTML = `<div class="alert alert-success" style="background:rgba(16,185,129,0.1);border:1px solid var(--success);border-radius:6px;padding:12px;color:var(--success)">
                    Rebuild complete. New sha256: <code>${escHtml((result && result.sha256 || '').slice(0,16))}…</code>
                </div>`;
            }
            if (btn) { btn.textContent = 'Done'; }
            // Refresh the page after a moment to show new hash.
            setTimeout(() => {
                const modal = document.getElementById('rebuild-initramfs-modal');
                if (modal) modal.remove();
                Pages.images();
            }, 2000);
        } catch (e) {
            if (resultDiv) {
                resultDiv.style.display = 'block';
                resultDiv.innerHTML = `<div class="alert alert-error" style="background:rgba(239,68,68,0.1);border:1px solid var(--error);border-radius:6px;padding:12px;color:var(--error)">Rebuild failed: ${escHtml(e.message)}</div>`;
            }
            if (btn) { btn.disabled = false; btn.textContent = 'Retry'; }
        }
    },

    // ── Image card with resume button ──────────────────────────────────────

    _imageCard(img) {
        const statusClass = {
            ready:       'badge-ready',
            building:    'badge-building',
            error:       'badge-error',
            archived:    'badge-archived',
            interrupted: 'badge-error',
        }[img.status] || 'badge-archived';

        const isResumable = img.status === 'interrupted' || img.status === 'error';
        const resumeBtn = isResumable
            ? `<button class="btn btn-secondary btn-sm" style="margin-top:8px;font-size:11px"
                onclick="event.stopPropagation();Pages.resumeImageBuild('${escHtml(img.id)}')">
                &#9654; Resume
               </button>`
            : '';

        return `<div class="image-card" onclick="Router.navigate('/images/${img.id}')">
            <div class="image-card-name" title="${escHtml(img.name)}">${escHtml(img.name)}</div>
            <div class="image-card-meta">
                <span class="badge ${statusClass}">${escHtml(img.status)}</span>
                ${img.os   ? `<span class="badge badge-neutral badge-sm">${escHtml(img.os)}</span>` : ''}
                ${img.arch ? `<span class="badge badge-neutral badge-sm">${escHtml(img.arch)}</span>` : ''}
                ${img.format ? `<span class="badge badge-neutral badge-sm">${escHtml(img.format)}</span>` : ''}
            </div>
            <div class="image-card-footer">
                <span class="text-mono">${fmtBytes(img.size_bytes)}</span>
                <span>${fmtRelative(img.created_at)}</span>
            </div>
            ${resumeBtn}
        </div>`;
    },

    async resumeImageBuild(imageId) {
        try {
            await API.resume.image(imageId);
            App.toast('Build resumed — polling for progress…', 'success');
            Pages.images();
        } catch (e) {
            App.toast(`Resume failed: ${e.message}`, 'error');
        }
    },

    showPullModal() {
        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'pull-modal';
        overlay.innerHTML = `
            <div class="modal" style="max-width:600px">
                <div class="modal-header">
                    <span class="modal-title">Pull Image</span>
                    <button class="modal-close" onclick="document.getElementById('pull-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <form id="pull-form" onsubmit="Pages.submitPull(event)">
                        <div class="form-grid">
                            <div class="form-group" style="grid-column:1/-1">
                                <label>Image URL *</label>
                                <input type="url" name="url" id="pull-url"
                                    placeholder="https://example.com/image.tar.gz  or  https://…/Rocky-10.1-x86_64-dvd1.iso"
                                    required>
                                <div id="pull-iso-hint" style="display:none;margin-top:6px;padding:8px 10px;
                                    background:var(--bg-tertiary,#1e2a3a);border-radius:6px;font-size:12px;
                                    color:var(--text-secondary)">
                                    Installer ISO detected — clonr will run the installer in a temporary
                                    QEMU VM and capture the result as a base image (5-30 min).
                                    KVM acceleration is used when available.
                                </div>
                            </div>
                            <div class="form-group">
                                <label>Name *</label>
                                <input type="text" name="name" placeholder="rocky-10-base" required>
                            </div>
                            <div class="form-group">
                                <label>Version</label>
                                <input type="text" name="version" placeholder="10.1">
                            </div>
                            <div class="form-group">
                                <label>OS</label>
                                <input type="text" name="os" placeholder="rocky">
                            </div>
                            <div class="form-group">
                                <label>Arch</label>
                                <input type="text" name="arch" placeholder="x86_64">
                            </div>
                            <!-- Standard (non-ISO) fields -->
                            <div class="form-group" id="pull-format-group">
                                <label>Format</label>
                                <select name="format">
                                    <option value="filesystem">filesystem (tar)</option>
                                    <option value="block">block (raw/partclone)</option>
                                </select>
                            </div>
                            <!-- ISO-installer fields (shown only for .iso URLs) -->
                            <div class="form-group" id="pull-disk-group" style="display:none">
                                <label>Disk Size (GB)</label>
                                <input type="number" name="disk_size_gb" value="20" min="10" max="500">
                            </div>
                            <div class="form-group" id="pull-mem-group" style="display:none">
                                <label>VM Memory (MB)</label>
                                <input type="number" name="memory_mb" value="2048" min="512" max="32768">
                            </div>
                            <div class="form-group" id="pull-cpu-group" style="display:none">
                                <label>VM CPUs</label>
                                <input type="number" name="cpus" value="2" min="1" max="16">
                            </div>
                            <!-- Role presets (ISO mode only) -->
                            <div style="grid-column:1/-1" id="pull-roles-group" style="display:none">
                                <div style="font-size:13px;font-weight:600;margin-bottom:8px;color:var(--text-primary)">Node Roles <span style="font-weight:400;font-size:12px;color:var(--text-secondary)">(select all that apply)</span></div>
                                <div id="pull-roles-list" style="display:grid;gap:6px;margin-bottom:10px">
                                    <div style="font-size:12px;color:var(--text-secondary);font-style:italic">Loading roles…</div>
                                </div>
                                <div id="pull-roles-preview" style="font-size:12px;color:var(--text-secondary);margin-bottom:10px"></div>
                                <label style="display:flex;align-items:flex-start;gap:8px;cursor:pointer;font-size:13px">
                                    <input type="checkbox" name="install_updates" id="pull-install-updates" style="margin-top:2px;flex-shrink:0">
                                    <span>
                                        <strong>Install OS updates during build</strong><br>
                                        <span style="font-size:12px;color:var(--text-secondary)">Adds ~5-10 min. The resulting image will not need immediate patching on deploy.</span>
                                    </span>
                                </label>
                            </div>
                            <div class="form-group" style="grid-column:1/-1" id="pull-ks-group" style="display:none">
                                <label>Custom Kickstart / Autoinstall</label>
                                <textarea name="custom_kickstart" rows="3"
                                    placeholder="Paste a custom kickstart or autoinstall config here (optional — leave blank to use the auto-generated template)"
                                    style="width:100%;font-family:monospace;font-size:12px;resize:vertical"></textarea>
                            </div>
                            <div class="form-group" style="grid-column:1/-1">
                                <label>Notes</label>
                                <input type="text" name="notes" placeholder="Optional description">
                            </div>
                        </div>
                        <div id="pull-progress" style="display:none;margin-top:12px">
                            <div style="font-size:12px;color:var(--text-secondary);margin-bottom:6px" id="pull-progress-label">Submitting…</div>
                            <div class="progress-bar-wrap" style="width:100%">
                                <div class="progress-bar-fill" style="width:60%;animation:indeterminate 1.5s ease infinite"></div>
                            </div>
                        </div>
                        <div id="pull-result"></div>
                        <div class="form-actions">
                            <button type="button" class="btn btn-secondary" onclick="document.getElementById('pull-modal').remove()">Cancel</button>
                            <button type="submit" class="btn btn-primary" id="pull-btn">Pull Image</button>
                        </div>
                    </form>
                </div>
            </div>`;
        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });

        // Detect ISO URL and toggle ISO-specific fields.
        const urlInput    = document.getElementById('pull-url');
        const isoHint     = document.getElementById('pull-iso-hint');
        const fmtGroup    = document.getElementById('pull-format-group');
        const diskGroup   = document.getElementById('pull-disk-group');
        const memGroup    = document.getElementById('pull-mem-group');
        const cpuGroup    = document.getElementById('pull-cpu-group');
        const rolesGroup  = document.getElementById('pull-roles-group');
        const ksGroup     = document.getElementById('pull-ks-group');
        const pullBtn     = document.getElementById('pull-btn');
        let rolesLoaded   = false;

        // _loadRoles fetches the role list from the server and renders the picker.
        // Called once on first ISO URL detection; subsequent toggles reuse the DOM.
        const _loadRoles = async () => {
            if (rolesLoaded) return;
            rolesLoaded = true;
            const list    = document.getElementById('pull-roles-list');
            const preview = document.getElementById('pull-roles-preview');
            try {
                const resp   = await API.imageRoles.list();
                const roles  = resp.roles || [];
                list.innerHTML = roles.map(r => `
                    <label style="display:flex;align-items:flex-start;gap:8px;cursor:pointer;
                                  padding:6px 8px;border-radius:6px;
                                  background:var(--bg-tertiary,#1e2a3a);font-size:13px"
                           title="${escHtml(r.notes || '')}">
                        <input type="checkbox" name="role_ids" value="${escHtml(r.id)}"
                               style="margin-top:2px;flex-shrink:0"
                               onchange="Pages._updateRolePreview()">
                        <span>
                            <strong>${escHtml(r.name)}</strong>
                            <span style="color:var(--text-secondary);font-size:12px;display:block">${escHtml(r.description)}</span>
                            ${r.notes ? `<span style="color:var(--text-secondary);font-size:11px;font-style:italic;display:block">${escHtml(r.notes)}</span>` : ''}
                        </span>
                    </label>`).join('');
                Pages._updateRolePreview();
            } catch (ex) {
                list.innerHTML = `<div style="font-size:12px;color:var(--text-secondary)">Could not load role presets: ${escHtml(ex.message)}</div>`;
            }
        };

        const applyISOMode = (isISO) => {
            isoHint.style.display   = isISO ? 'block' : 'none';
            fmtGroup.style.display  = isISO ? 'none'  : '';
            diskGroup.style.display = isISO ? ''      : 'none';
            memGroup.style.display  = isISO ? ''      : 'none';
            cpuGroup.style.display  = isISO ? ''      : 'none';
            if (rolesGroup) rolesGroup.style.display = isISO ? '' : 'none';
            if (ksGroup) ksGroup.style.display = isISO ? '' : 'none';
            pullBtn.textContent = isISO ? 'Build from ISO' : 'Pull Image';
            if (isISO) _loadRoles();
        };

        urlInput.addEventListener('input', () => {
            const val = urlInput.value.trim().toLowerCase().split('?')[0];
            applyISOMode(val.endsWith('.iso'));
            // Auto-fill name/os from filename when URL looks like an ISO.
            if (val.endsWith('.iso')) {
                Pages._autoFillFromISOUrl(urlInput.value);
            }
        });

        urlInput.focus();
    },

    // _autoFillFromISOUrl populates Name/Version/OS inputs from an ISO filename.
    _autoFillFromISOUrl(isoURL) {
        const form    = document.getElementById('pull-form');
        if (!form) return;
        const nameEl  = form.elements['name'];
        const verEl   = form.elements['version'];
        const osEl    = form.elements['os'];
        if (!nameEl || !verEl || !osEl) return;

        const base = isoURL.split('/').pop().split('?')[0].replace(/\.iso$/i, '');
        if (!nameEl.value) {
            nameEl.value = base.toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
        }
        if (!verEl.value) {
            const m = base.match(/(\d+\.\d+)/);
            if (m) verEl.value = m[1];
        }
        if (!osEl.value) {
            const lower = base.toLowerCase();
            if (lower.includes('rocky'))     osEl.value = 'rocky';
            else if (lower.includes('alma')) osEl.value = 'almalinux';
            else if (lower.includes('centos')) osEl.value = 'centos';
            else if (lower.includes('ubuntu')) osEl.value = 'ubuntu';
            else if (lower.includes('debian')) osEl.value = 'debian';
            else if (lower.includes('opensuse') || lower.includes('suse')) osEl.value = 'suse';
            else if (lower.includes('alpine')) osEl.value = 'alpine';
        }
    },

    // _updateRolePreview updates the "Selected: ..." line below the role picker.
    _updateRolePreview() {
        const form    = document.getElementById('pull-form');
        const preview = document.getElementById('pull-roles-preview');
        if (!form || !preview) return;
        const checked = [...form.querySelectorAll('input[name="role_ids"]:checked')];
        if (checked.length === 0) {
            preview.textContent = '';
            return;
        }
        const names = checked.map(cb => {
            const label = cb.closest('label');
            return label ? (label.querySelector('strong') || {}).textContent || cb.value : cb.value;
        });
        preview.textContent = 'Selected: ' + names.join(', ');
    },

    showImportISOModal() {
        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'iso-modal';
        overlay.innerHTML = `
            <div class="modal">
                <div class="modal-header">
                    <span class="modal-title">Upload Image File</span>
                    <button class="modal-close" onclick="document.getElementById('iso-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <form id="iso-form" onsubmit="Pages.submitImportISO(event)">
                        <div class="form-group" style="margin-bottom:16px">
                            <label>Image File</label>
                            <div class="upload-zone" id="iso-drop-zone">
                                <input type="file" id="iso-file-input" accept=".iso,.img,.qcow2,.tar.gz,.tgz,.raw">
                                <div class="upload-zone-icon">&#8686;</div>
                                <div class="upload-zone-label">
                                    Drop an ISO, qcow2, img, or tarball here<br>
                                    <span style="font-size:12px">or click to browse</span>
                                </div>
                                <div class="upload-zone-filename" id="iso-filename"></div>
                            </div>
                        </div>
                        <div class="form-grid">
                            <div class="form-group">
                                <label>Name *</label>
                                <input type="text" name="name" id="iso-name" placeholder="rocky-9-hpc" required>
                            </div>
                            <div class="form-group">
                                <label>Version</label>
                                <input type="text" name="version" id="iso-version" placeholder="9.3">
                            </div>
                        </div>
                        <div class="upload-progress-wrap" id="iso-progress-wrap" style="display:none">
                            <div class="upload-progress-bar-outer">
                                <div class="upload-progress-bar-inner" id="iso-progress-bar"></div>
                            </div>
                            <div class="upload-progress-meta">
                                <span id="iso-progress-pct">0%</span>
                                <span id="iso-progress-speed"></span>
                                <span id="iso-progress-eta"></span>
                            </div>
                        </div>
                        <div id="iso-result"></div>
                        <div class="form-actions">
                            <button type="button" class="btn btn-secondary" onclick="document.getElementById('iso-modal').remove()">Cancel</button>
                            <button type="submit" class="btn btn-primary" id="iso-btn">Upload &amp; Import</button>
                        </div>
                    </form>
                </div>
            </div>`;
        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });

        // Wire up file picker and drag-and-drop after the DOM is appended.
        const zone    = document.getElementById('iso-drop-zone');
        const input   = document.getElementById('iso-file-input');
        const fnLabel = document.getElementById('iso-filename');
        const nameEl  = document.getElementById('iso-name');
        const verEl   = document.getElementById('iso-version');

        const applyFile = (file) => {
            if (!file) return;
            fnLabel.textContent = `${file.name} (${fmtBytes(file.size)})`;
            // Auto-populate name/version from filename when the fields are still empty.
            if (!nameEl.value) {
                const base = file.name.replace(/\.(iso|img|qcow2|tar\.gz|tgz|raw)$/i, '');
                nameEl.value = base.toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
            }
            if (!verEl.value) {
                const m = file.name.match(/(\d+\.\d+)/);
                if (m) verEl.value = m[1];
            }
        };

        input.addEventListener('change', () => applyFile(input.files[0]));

        zone.addEventListener('dragover',  e => { e.preventDefault(); zone.classList.add('drag-over'); });
        zone.addEventListener('dragleave', () => zone.classList.remove('drag-over'));
        zone.addEventListener('drop', e => {
            e.preventDefault();
            zone.classList.remove('drag-over');
            const file = e.dataTransfer.files[0];
            if (file) {
                const dt = new DataTransfer();
                dt.items.add(file);
                input.files = dt.files;
                applyFile(file);
            }
        });
    },

    async submitPull(e) {
        e.preventDefault();
        const form  = e.target;
        const btn   = document.getElementById('pull-btn');
        const res   = document.getElementById('pull-result');
        const prog  = document.getElementById('pull-progress');
        const label = document.getElementById('pull-progress-label');
        const data  = new FormData(form);
        const url   = (data.get('url') || '').trim();
        const isISO = url.toLowerCase().split('?')[0].endsWith('.iso');

        btn.disabled = true;
        btn.textContent = 'Submitting…';
        if (prog) prog.style.display = 'block';
        if (label) label.textContent = isISO
            ? 'Starting ISO build (this can take 5-30 min — check the image status for progress)…'
            : 'Submitting pull request…';
        res.innerHTML = '';

        try {
            let img;
            if (isISO) {
                const roleIds = [...form.querySelectorAll('input[name="role_ids"]:checked')].map(cb => cb.value);
                const updatesEl = form.querySelector('input[name="install_updates"]');
                const body = {
                    url:              url,
                    name:             data.get('name'),
                    version:          data.get('version') || undefined,
                    os:               data.get('os')      || undefined,
                    arch:             data.get('arch')    || undefined,
                    disk_size_gb:     parseInt(data.get('disk_size_gb'), 10) || 0,
                    memory_mb:        parseInt(data.get('memory_mb'),    10) || 0,
                    cpus:             parseInt(data.get('cpus'),         10) || 0,
                    role_ids:         roleIds.length > 0 ? roleIds : undefined,
                    install_updates:  updatesEl ? updatesEl.checked : false,
                    custom_kickstart: data.get('custom_kickstart') || undefined,
                    notes:            data.get('notes')  || undefined,
                    tags:             [],
                };
                img = await API.factory.buildFromISO(body);
            } else {
                const body = {
                    url:     url,
                    name:    data.get('name'),
                    version: data.get('version'),
                    os:      data.get('os'),
                    arch:    data.get('arch'),
                    format:  data.get('format'),
                    notes:   data.get('notes'),
                    tags:    [],
                };
                img = await API.factory.pull(body);
            }
            if (prog) prog.style.display = 'none';
            const verb = isISO ? 'ISO build started' : 'Pull started';
            res.innerHTML = alertBox(`${verb}: ${escHtml(img.name)} (${img.id}) — status: ${img.status}`, 'success');
            form.reset();
            App.setAutoRefresh(() => Pages.images(), 5000);
            setTimeout(() => {
                const modal = document.getElementById('pull-modal');
                if (modal) modal.remove();
                Pages.images();
            }, 1500);
        } catch (ex) {
            if (prog) prog.style.display = 'none';
            res.innerHTML = alertBox(`${isISO ? 'ISO build' : 'Pull'} failed: ${ex.message}`);
            btn.disabled = false;
            btn.textContent = isISO ? 'Build from ISO' : 'Pull Image';
        }
    },

    async submitImportISO(e) {
        e.preventDefault();
        const form     = e.target;
        const btn      = document.getElementById('iso-btn');
        const res      = document.getElementById('iso-result');
        const input    = document.getElementById('iso-file-input');
        const progWrap = document.getElementById('iso-progress-wrap');
        const progBar  = document.getElementById('iso-progress-bar');
        const progPct  = document.getElementById('iso-progress-pct');
        const progSpd  = document.getElementById('iso-progress-speed');
        const progEta  = document.getElementById('iso-progress-eta');

        const file = input && input.files[0];
        if (!file) {
            res.innerHTML = alertBox('Please select a file to upload.');
            return;
        }

        const data = new FormData(form);
        btn.disabled = true;
        btn.textContent = 'Uploading…';
        res.innerHTML = '';
        if (progWrap) progWrap.style.display = 'block';

        const onProgress = (pct, loaded, total, bps, eta) => {
            if (!progBar) return;
            progBar.style.width  = `${pct}%`;
            progPct.textContent  = `${pct}%`;
            progSpd.textContent  = bps > 0 ? `${fmtBytes(Math.round(bps))}/s` : '';
            progEta.textContent  = eta > 0 ? `ETA ${Math.ceil(eta)}s` : '';
        };

        try {
            const img = await API.factory.uploadISO(file, {
                name:    data.get('name'),
                version: data.get('version'),
            }, onProgress);

            if (progBar) { progBar.style.width = '100%'; progBar.classList.add('complete'); }
            if (progPct) progPct.textContent = '100%';
            if (progEta) progEta.textContent = 'Done';

            res.innerHTML = alertBox(`Upload complete: ${img.name} (${img.id}) — processing in background`, 'success');
            App.setAutoRefresh(() => Pages.images(), 5000);
            setTimeout(() => {
                const modal = document.getElementById('iso-modal');
                if (modal) modal.remove();
                Pages.images();
            }, 2000);
        } catch (ex) {
            if (progBar) progBar.classList.add('error');
            res.innerHTML = alertBox(`Upload failed: ${ex.message}`);
            btn.disabled = false;
            btn.textContent = 'Upload & Import';
        }
    },

    async archiveImage(id, name) {
        if (!confirm(`Archive image "${name}"? This cannot be undone.`)) return;
        try {
            await API.images.archive(id);
            Pages.images();
        } catch (e) {
            alert(`Archive failed: ${e.message}`);
        }
    },

    // showDeleteImageModal — opens a confirmation modal for real image deletion.
    // When the image is in-use by nodes, shows them and offers a force-delete checkbox.
    async showDeleteImageModal(id, name) {
        // Pre-fetch to see if any nodes are using the image.
        let nodes = [];
        try {
            const resp = await API.get(`/nodes`, { base_image_id: id });
            nodes = (resp && resp.nodes) || [];
        } catch (_) {}

        const nodesHtml = nodes.length
            ? `<div style="margin:12px 0 8px;padding:10px 12px;background:var(--bg-secondary);border-radius:6px;border:1px solid var(--border)">
                <div style="font-size:12px;font-weight:600;color:var(--text-secondary);margin-bottom:6px">Nodes using this image:</div>
                ${nodes.map(n => `<div style="font-size:13px;font-family:var(--font-mono);padding:2px 0">${escHtml(n.hostname || n.primary_mac)}</div>`).join('')}
               </div>
               <label style="display:flex;align-items:center;gap:8px;cursor:pointer;margin:4px 0 12px">
                   <input type="checkbox" id="delete-image-force">
                   <span style="font-size:13px">Force delete — unassign ${nodes.length} node${nodes.length !== 1 ? 's' : ''} and delete anyway</span>
               </label>`
            : `<p style="margin:8px 0 16px;color:var(--text-secondary);font-size:13px">This will permanently remove the image and all associated files. This action cannot be undone.</p>`;

        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'delete-image-modal';
        overlay.innerHTML = `
            <div class="modal" style="max-width:480px">
                <div class="modal-header">
                    <span class="modal-title">Delete Image</span>
                    <button class="modal-close" onclick="document.getElementById('delete-image-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <p style="margin:0 0 4px;font-weight:600">${escHtml(name)}</p>
                    ${nodesHtml}
                    <div id="delete-image-error" style="display:none" class="form-error"></div>
                    <div class="form-actions" style="margin-top:0">
                        <button class="btn btn-secondary" onclick="document.getElementById('delete-image-modal').remove()">Cancel</button>
                        <button class="btn btn-danger" id="delete-image-confirm-btn" onclick="Pages._confirmDeleteImage('${id}')">Delete Permanently</button>
                    </div>
                </div>
            </div>`;
        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });
    },

    async _confirmDeleteImage(id) {
        const force = !!(document.getElementById('delete-image-force') || {}).checked;
        const btn = document.getElementById('delete-image-confirm-btn');
        const errEl = document.getElementById('delete-image-error');
        if (btn) { btn.disabled = true; btn.textContent = 'Deleting…'; }
        if (errEl) errEl.style.display = 'none';
        try {
            await API.images.delete(id, { force });
            const modal = document.getElementById('delete-image-modal');
            if (modal) modal.remove();
            Router.navigate('/images');
        } catch (e) {
            if (errEl) { errEl.textContent = e.message; errEl.style.display = 'block'; }
            if (btn) { btn.disabled = false; btn.textContent = 'Delete Permanently'; }
        }
    },

    // ── Capture from Host ──────────────────────────────────────────────────

    showCaptureModal(prefillHost = '', prefillName = '') {
        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'capture-modal';
        overlay.innerHTML = `
            <div class="modal" style="max-width:560px">
                <div class="modal-header">
                    <span class="modal-title">Capture from Host</span>
                    <button class="modal-close" onclick="document.getElementById('capture-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <div class="alert alert-info" style="margin-bottom:16px;font-size:12px">
                        The server will SSH to the source host and rsync its filesystem into a new image.
                        SSH host key verification is disabled — only use this on trusted golden nodes.
                    </div>
                    <form id="capture-form" onsubmit="Pages.submitCapture(event)">
                        <div class="form-group" style="margin-bottom:14px">
                            <label>Source Host * <span style="font-size:11px;color:var(--text-secondary)">(user@host or host)</span></label>
                            <input type="text" name="source_host" placeholder="root@192.168.1.10" value="${escHtml(prefillHost)}" required>
                        </div>
                        <div class="form-grid" style="margin-bottom:14px">
                            <div class="form-group">
                                <label>Name *</label>
                                <input type="text" name="name" placeholder="rocky9-golden" value="${escHtml(prefillName)}" required>
                            </div>
                            <div class="form-group">
                                <label>Version</label>
                                <input type="text" name="version" placeholder="1.0.0" value="1.0.0">
                            </div>
                            <div class="form-group">
                                <label>OS</label>
                                <input type="text" name="os" placeholder="Rocky Linux 9">
                            </div>
                            <div class="form-group">
                                <label>Arch</label>
                                <input type="text" name="arch" placeholder="x86_64" value="x86_64">
                            </div>
                            <div class="form-group">
                                <label>SSH Port</label>
                                <input type="number" name="ssh_port" value="22" min="1" max="65535">
                            </div>
                        </div>
                        <div style="margin-bottom:14px">
                            <label style="font-size:12px;font-weight:600;color:var(--text-secondary);margin-bottom:6px;display:block">SSH Authentication</label>
                            <div class="tab-bar" style="margin-bottom:10px">
                                <div class="tab active" id="ssh-tab-key" onclick="Pages._switchCaptureAuth('key')">Private Key (server path)</div>
                                <div class="tab" id="ssh-tab-pwd" onclick="Pages._switchCaptureAuth('pwd')">Password</div>
                            </div>
                            <div id="ssh-auth-key">
                                <div class="form-group">
                                    <label>Key Path <span style="font-size:11px;color:var(--text-secondary)">(absolute path on the server)</span></label>
                                    <input type="text" name="ssh_key_path" placeholder="/etc/clonr/keys/golden_key">
                                </div>
                            </div>
                            <div id="ssh-auth-pwd" style="display:none">
                                <div class="form-group">
                                    <label>Password <span style="font-size:11px;color:var(--text-secondary)">(requires sshpass on server)</span></label>
                                    <input type="password" name="ssh_password" autocomplete="off">
                                </div>
                            </div>
                        </div>
                        <div class="form-group" style="margin-bottom:14px">
                            <label>Extra Exclude Paths <span style="font-size:11px;color:var(--text-secondary)">(one per line, beyond defaults)</span></label>
                            <textarea name="exclude_paths" rows="3" placeholder="/opt/scratch&#10;/data/volatile"></textarea>
                        </div>
                        <div class="form-group" style="margin-bottom:14px">
                            <label>Notes</label>
                            <input type="text" name="notes" placeholder="Optional description">
                        </div>
                        <div id="capture-progress" style="display:none;margin-top:12px">
                            <div style="font-size:12px;color:var(--text-secondary);margin-bottom:6px">Submitting capture request…</div>
                            <div class="progress-bar-wrap" style="width:100%">
                                <div class="progress-bar-fill" style="width:60%;animation:indeterminate 1.5s ease infinite"></div>
                            </div>
                        </div>
                        <div id="capture-result"></div>
                        <div class="form-actions">
                            <button type="button" class="btn btn-secondary" onclick="document.getElementById('capture-modal').remove()">Cancel</button>
                            <button type="submit" class="btn btn-primary" id="capture-btn">Start Capture</button>
                        </div>
                    </form>
                </div>
            </div>`;
        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });
        const firstInput = overlay.querySelector('input[name="source_host"]');
        if (firstInput && !prefillHost) firstInput.focus();
    },

    _switchCaptureAuth(tab) {
        const keyDiv = document.getElementById('ssh-auth-key');
        const pwdDiv = document.getElementById('ssh-auth-pwd');
        const keyTab = document.getElementById('ssh-tab-key');
        const pwdTab = document.getElementById('ssh-tab-pwd');
        if (tab === 'key') {
            if (keyDiv) keyDiv.style.display = '';
            if (pwdDiv) pwdDiv.style.display = 'none';
            if (keyTab) keyTab.classList.add('active');
            if (pwdTab) pwdTab.classList.remove('active');
        } else {
            if (keyDiv) keyDiv.style.display = 'none';
            if (pwdDiv) pwdDiv.style.display = '';
            if (keyTab) keyTab.classList.remove('active');
            if (pwdTab) pwdTab.classList.add('active');
        }
    },

    async submitCapture(e) {
        e.preventDefault();
        const form = e.target;
        const btn  = document.getElementById('capture-btn');
        const res  = document.getElementById('capture-result');
        const prog = document.getElementById('capture-progress');
        const data = new FormData(form);

        btn.disabled = true;
        btn.textContent = 'Submitting…';
        if (prog) prog.style.display = 'block';
        res.innerHTML = '';

        const excludeRaw = (data.get('exclude_paths') || '').split('\n').map(s => s.trim()).filter(Boolean);

        try {
            const body = {
                source_host:  data.get('source_host'),
                ssh_user:     '',  // embedded in source_host if user@host form
                ssh_key_path: data.get('ssh_key_path') || '',
                ssh_password: data.get('ssh_password') || '',
                ssh_port:     parseInt(data.get('ssh_port') || '22', 10),
                name:         data.get('name'),
                version:      data.get('version') || '1.0.0',
                os:           data.get('os') || '',
                arch:         data.get('arch') || 'x86_64',
                exclude_paths: excludeRaw,
                notes:        data.get('notes') || '',
                tags:         [],
            };
            const img = await API.factory.capture(body);
            if (prog) prog.style.display = 'none';
            res.innerHTML = alertBox(
                `Capture started: ${img.name} (${img.id.substring(0, 8)}) — status: ${img.status}. ` +
                `The server is rsyncing from ${body.source_host} — this may take several minutes.`,
                'success'
            );
            App.setAutoRefresh(() => Pages.images(), 5000);
            setTimeout(() => {
                const modal = document.getElementById('capture-modal');
                if (modal) modal.remove();
                Pages.images();
            }, 2500);
        } catch (ex) {
            if (prog) prog.style.display = 'none';
            res.innerHTML = alertBox(`Capture failed: ${ex.message}`);
            btn.disabled = false;
            btn.textContent = 'Start Capture';
        }
    },

    // ── Image Detail ───────────────────────────────────────────────────────

    async imageDetail(id) {
        App.render(loading('Loading image…'));
        try {
            const img = await API.images.get(id);
            const tagsHtml = (img.tags || []).map(t => `<span class="badge badge-neutral">${escHtml(t)}</span>`).join(' ') || '—';

            App.render(`
                <div class="breadcrumb">
                    <a href="#/images">Images</a>
                    <span class="breadcrumb-sep">/</span>
                    <span>${escHtml(img.name)}</span>
                </div>
                <div class="page-header">
                    <div style="display:flex;align-items:center;gap:12px">
                        <button class="detail-back-btn" onclick="Router.navigate('/images')">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <polyline points="15 18 9 12 15 6"/>
                            </svg>
                            Back
                        </button>
                        <div>
                            <div class="page-title">${escHtml(img.name)}</div>
                            <div class="page-subtitle">${escHtml(img.id)}</div>
                        </div>
                        ${badge(img.status)}
                    </div>
                    <div class="flex gap-8">
                        ${img.status === 'ready'
                            ? `<button class="btn btn-secondary" onclick="Pages.openShellTerminal('${escHtml(img.id)}')">Shell Access</button>`
                            : ''}
                        <button class="btn btn-danger btn-sm" onclick="Pages.showDeleteImageModal('${img.id}', '${escHtml(img.name)}')">Delete Image</button>
                    </div>
                </div>

                ${img.error_message ? alertBox(`Error: ${img.error_message}`) : ''}
                ${img.status === 'building' ? Pages._isoBuildInProgress(img) : ''}

                ${cardWrap('Image Details', `
                    <div class="card-body">
                        <div class="kv-grid">
                            <div class="kv-item"><div class="kv-key">ID</div><div class="kv-value">${escHtml(img.id)}</div></div>
                            <div class="kv-item"><div class="kv-key">Name</div><div class="kv-value">${escHtml(img.name)}</div></div>
                            <div class="kv-item"><div class="kv-key">Version</div><div class="kv-value">${escHtml(img.version || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">OS</div><div class="kv-value">${escHtml(img.os || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Arch</div><div class="kv-value">${escHtml(img.arch || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Format</div><div class="kv-value">${escHtml(img.format || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Firmware</div><div class="kv-value">${img.firmware === 'bios' ? '<span class="badge badge-warning badge-sm">BIOS (legacy)</span>' : '<span class="badge badge-neutral badge-sm">UEFI</span>'}</div></div>
                            <div class="kv-item"><div class="kv-key">Size</div><div class="kv-value">${fmtBytes(img.size_bytes)}</div></div>
                            <div class="kv-item"><div class="kv-key">Checksum (sha256)</div><div class="kv-value" style="font-size:11px">${escHtml(img.checksum || '—')}</div></div>
                            <div class="kv-item" style="grid-column:1/-1"><div class="kv-key">Source URL</div>
                                <div class="kv-value" style="font-size:12px">${img.source_url
                                    ? `<a href="${escHtml(img.source_url)}" target="_blank" rel="noreferrer">${escHtml(img.source_url)}</a>`
                                    : '—'}</div>
                            </div>
                            <div class="kv-item"><div class="kv-key">Tags</div><div class="kv-value">${tagsHtml}</div></div>
                            <div class="kv-item"><div class="kv-key">Notes</div><div class="kv-value">${escHtml(img.notes || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Created</div><div class="kv-value">${fmtDate(img.created_at)}</div></div>
                            <div class="kv-item"><div class="kv-key">Finalized</div><div class="kv-value">${fmtDate(img.finalized_at)}</div></div>
                        </div>
                    </div>`)}

                ${img.disk_layout ? cardWrap('Disk Layout', `
                    <div class="card-body">
                        ${this._renderDiskLayout(img.disk_layout)}
                    </div>`) : ''}

                <div id="shell-hint-area"></div>
            `);

            if (img.status === 'building') {
                if (img.build_method === 'iso') {
                    // ISO build: subscribe to the SSE build progress stream for
                    // live phase, serial console, and byte progress updates.
                    Pages._startIsoBuildSSE(id);
                } else {
                    // Non-ISO build (pull, capture, import): fall back to polling.
                    App.setAutoRefresh(async () => {
                        try {
                            const fresh = await API.images.get(id);
                            if (fresh.status !== 'building') {
                                Pages.imageDetail(id);
                            }
                        } catch (_) {}
                    }, 5000);
                }
            }
        } catch (e) {
            App.render(alertBox(`Failed to load image: ${e.message}`));
        }
    },

    _renderDiskLayout(layout) {
        if (!layout) return '<pre class="json-block">null</pre>';
        if (typeof layout === 'object') {
            const disks = layout.disks || layout.Disks || [];
            if (disks.length > 0) {
                return disks.map(disk => {
                    const parts = disk.partitions || disk.Partitions || [];
                    const totalBytes = parts.reduce((s, p) => s + (p.size_bytes || p.Size || 0), 0) || 1;
                    const segColors = ['seg-boot', 'seg-root', 'seg-swap', 'seg-data', 'seg-other'];
                    const barHtml = parts.map((p, i) => {
                        const sz = p.size_bytes || p.Size || 0;
                        const pct = Math.max(3, Math.round((sz / totalBytes) * 100));
                        const label = p.name || p.Name || p.mountpoint || p.MountPoint || `p${i+1}`;
                        const cls = segColors[i % segColors.length];
                        return `<div class="${cls} disk-segment" style="flex:${pct}" title="${escHtml(label)}: ${fmtBytes(sz)}">
                            ${pct > 8 ? escHtml(label.replace('/dev/', '').replace('sda', '').replace('nvme0n1', '')) : ''}
                        </div>`;
                    }).join('');
                    return `<div style="margin-bottom:16px">
                        <div style="font-size:12px;font-family:var(--font-mono);font-weight:600;color:var(--text-secondary);margin-bottom:6px">
                            ${escHtml(disk.name || disk.Name || 'disk')} — ${fmtBytes(disk.size_bytes || disk.Size || 0)}
                        </div>
                        <div class="disk-bar">${barHtml || '<div class="seg-other disk-segment" style="flex:1">unpartitioned</div>'}</div>
                        <div style="display:flex;flex-wrap:wrap;gap:8px;margin-top:6px">
                            ${parts.map((p, i) => `<span style="display:inline-flex;align-items:center;gap:5px;font-size:11px;color:var(--text-secondary)">
                                <span style="width:10px;height:10px;border-radius:2px;display:inline-block" class="${segColors[i % segColors.length]}"></span>
                                ${escHtml(p.name || p.Name || p.mountpoint || p.MountPoint || `p${i+1}`)} (${fmtBytes(p.size_bytes || p.Size || 0)})
                            </span>`).join('')}
                        </div>
                    </div>`;
                }).join('') + `<details style="margin-top:12px">
                    <summary>Raw JSON</summary>
                    <pre class="json-block" style="margin:12px">${escHtml(JSON.stringify(layout, null, 2))}</pre>
                </details>`;
            }
        }
        return `<pre class="json-block">${escHtml(JSON.stringify(layout, null, 2))}</pre>`;
    },

    showShellHint(id) {
        // Legacy fallback — replaced by openShellTerminal.
        this.openShellTerminal(id);
    },

    // ── Browser Shell Terminal ─────────────────────────────────────────────

    _shellTerm: null,          // active xterm.js Terminal instance
    _shellWs: null,            // active WebSocket
    _shellSessionId: null,     // active session ID for cleanup
    _shellImageId: null,       // active image ID for cleanup

    async openShellTerminal(imageId) {
        // Create session on server first.
        let sess;
        try {
            sess = await API.images.openShellSession(imageId);
        } catch (e) {
            alert(`Failed to open shell session: ${e.message}`);
            return;
        }

        this._shellSessionId = sess.session_id;
        this._shellImageId = imageId;

        // Check for active deploys (for the warning banner).
        let activeCount = 0;
        try {
            const ad = await API.images.activeDeploys(imageId);
            activeCount = ad.active_count || 0;
        } catch (_) {}

        // Build modal HTML.
        const warnHtml = activeCount > 0
            ? `<div class="shell-modal-warn">
                &#9888; This image is currently being deployed to ${activeCount} node${activeCount !== 1 ? 's' : ''}.
                Shell access is safe (read-only race) but changes won\'t affect in-progress deployments.
               </div>`
            : '';

        const overlay = document.createElement('div');
        overlay.className = 'shell-modal-overlay';
        overlay.id = 'shell-modal-overlay';
        overlay.innerHTML = `
            <div class="shell-modal">
                <div class="shell-modal-header">
                    <div style="display:flex;align-items:center;gap:12px">
                        <div class="shell-modal-dots">
                            <div class="shell-modal-dot red"></div>
                            <div class="shell-modal-dot yellow"></div>
                            <div class="shell-modal-dot green"></div>
                        </div>
                        <span class="shell-modal-title">shell &mdash; ${escHtml(imageId)}</span>
                    </div>
                    <button class="shell-modal-close" onclick="Pages.closeShellTerminal()" title="Close terminal">&times;</button>
                </div>
                ${warnHtml}
                <div class="shell-modal-body">
                    <div id="shell-terminal-container"></div>
                </div>
                <div class="shell-status-bar">
                    <span class="shell-status-indicator connecting" id="shell-status-dot"></span>
                    <span id="shell-status-text">Connecting…</span>
                </div>
            </div>
        `;
        document.body.appendChild(overlay);

        // Close on overlay click (outside the modal box).
        overlay.addEventListener('click', (e) => {
            if (e.target === overlay) Pages.closeShellTerminal();
        });

        // Close on Escape.
        this._shellEscHandler = (e) => { if (e.key === 'Escape') Pages.closeShellTerminal(); };
        document.addEventListener('keydown', this._shellEscHandler);

        // Initialise xterm.js.
        const term = new Terminal({
            cursorBlink: true,
            theme: {
                background: '#0d1117',
                foreground: '#c9d1d9',
                cursor:     '#58a6ff',
                selectionBackground: 'rgba(88,166,255,0.3)',
            },
            fontFamily: 'JetBrains Mono, Fira Code, Cascadia Code, Consolas, monospace',
            fontSize: 13,
            lineHeight: 1.4,
            scrollback: 3000,
        });

        const fitAddon = new FitAddon.FitAddon();
        term.loadAddon(fitAddon);
        term.open(document.getElementById('shell-terminal-container'));
        fitAddon.fit();
        this._shellTerm = term;

        // Resize observer — fit terminal to container size.
        const ro = new ResizeObserver(() => {
            try { fitAddon.fit(); } catch (_) {}
            if (this._shellWs && this._shellWs.readyState === WebSocket.OPEN && term.cols && term.rows) {
                this._shellWs.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
            }
        });
        ro.observe(document.getElementById('shell-terminal-container'));
        this._shellRo = ro;

        // Open WebSocket.
        const wsUrl = API.images.shellWsUrl(imageId, sess.session_id);
        const ws = new WebSocket(wsUrl);
        this._shellWs = ws;

        ws.onopen = () => {
            this._setShellStatus('connected', 'Connected');
            // Send initial resize.
            if (term.cols && term.rows) {
                ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
            }
            term.focus();
        };

        ws.onmessage = (evt) => {
            try {
                const msg = JSON.parse(evt.data);
                if (msg.type === 'data') term.write(msg.data);
            } catch (_) {
                // Raw string fallback (shouldn't happen with our server).
                term.write(evt.data);
            }
        };

        ws.onerror = () => {
            this._setShellStatus('disconnected', 'Connection error');
            term.writeln('\r\n\x1b[31m[clonr] WebSocket error\x1b[0m');
        };

        ws.onclose = () => {
            this._setShellStatus('disconnected', 'Disconnected');
            term.writeln('\r\n\x1b[90m[clonr] Session closed\x1b[0m');
        };

        // Pipe keystrokes to WebSocket.
        term.onData((data) => {
            if (ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'data', data }));
            }
        });
    },

    closeShellTerminal() {
        // Kill WebSocket.
        if (this._shellWs) {
            this._shellWs.onclose = null; // suppress the "Disconnected" write
            this._shellWs.close();
            this._shellWs = null;
        }
        // Dispose terminal.
        if (this._shellTerm) {
            this._shellTerm.dispose();
            this._shellTerm = null;
        }
        // Stop resize observer.
        if (this._shellRo) {
            this._shellRo.disconnect();
            this._shellRo = null;
        }
        // Remove Escape listener.
        if (this._shellEscHandler) {
            document.removeEventListener('keydown', this._shellEscHandler);
            this._shellEscHandler = null;
        }
        // Remove modal.
        const overlay = document.getElementById('shell-modal-overlay');
        if (overlay) overlay.remove();

        // Close server-side session.
        if (this._shellImageId && this._shellSessionId) {
            const imgId = this._shellImageId;
            const sid   = this._shellSessionId;
            this._shellImageId = null;
            this._shellSessionId = null;
            API.images.closeShellSession(imgId, sid).catch(() => {});
        }
    },

    _setShellStatus(state, text) {
        const dot  = document.getElementById('shell-status-dot');
        const span = document.getElementById('shell-status-text');
        if (dot)  { dot.className = `shell-status-indicator ${state}`; }
        if (span) { span.textContent = text; }
    },

    _copyText(text, btn) {
        if (navigator.clipboard) {
            navigator.clipboard.writeText(text).then(() => {
                const orig = btn.textContent;
                btn.textContent = 'Copied!';
                btn.style.color = 'var(--success-text)';
                setTimeout(() => { btn.textContent = orig; btn.style.color = ''; }, 1500);
            }).catch(() => {});
        }
    },

    // ── Nodes ──────────────────────────────────────────────────────────────

    // _nodesImages caches the images list used for modal data across refresh cycles.
    _nodesImages: null,

    async nodes() {
        App.render(loading('Loading nodes…'));
        try {
            const [nodesResp, imagesResp] = await Promise.all([
                API.nodes.list(),
                API.images.list(),
            ]);
            const nodes  = nodesResp.nodes  || [];
            const images = imagesResp.images || [];

            // Cache for incremental refresh.
            App._cacheSet('nodes',  nodes);
            App._cacheSet('images', images);
            Pages._nodesImages = images;

            const imgMap = Object.fromEntries(images.map(i => [i.id, i]));

            App.render(`
                <div class="page-header">
                    <div>
                        <div class="page-title">Nodes</div>
                        <div class="page-subtitle" id="nodes-subtitle">${nodes.length} node${nodes.length !== 1 ? 's' : ''} total</div>
                    </div>
                    <div class="flex gap-8">
                        <button class="btn btn-secondary" onclick="Router.navigate('/nodes/groups')">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/>
                                <rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/>
                            </svg>
                            Manage Groups
                        </button>
                        <button class="btn btn-primary" onclick='Pages.showNodeModal(null, ${JSON.stringify(JSON.stringify(images))})'>
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
                            </svg>
                            Add Node
                        </button>
                    </div>
                </div>

                <div class="tab-bar" style="margin-bottom:20px">
                    <div class="tab active" onclick="Router.navigate('/nodes')">All Nodes</div>
                    <div class="tab" onclick="Router.navigate('/nodes/groups')">Groups</div>
                </div>

                ${cardWrap(`All Nodes`,
                    nodes.length
                        ? `<div class="table-wrap"><table>
                            <thead><tr>
                                <th>Host</th><th>Image</th><th>Status</th><th>Hardware</th><th>Groups</th><th>Updated</th><th>Actions</th>
                            </tr></thead>
                            <tbody id="nodes-tbody">
                            ${nodes.map(n => Pages._nodeRow(n, imgMap, images)).join('')}
                            </tbody>
                        </table></div>`
                        : `<div id="nodes-empty">${emptyState('No nodes', 'Add your first node using the button above',
                            `<button class="btn btn-primary" onclick='Pages.showNodeModal(null, ${JSON.stringify(JSON.stringify(images))})'>Add Node</button>`)}</div>`
                )}
            `);

            // Incremental auto-refresh — updates rows in-place without blowing away the DOM.
            App.setAutoRefresh(() => Pages.nodesRefresh());

        } catch (e) {
            App.render(alertBox(`Failed to load nodes: ${e.message}`));
        }
    },

    // _nodeRow renders a single <tr> string for the nodes table.
    _nodeRow(n, imgMap, images) {
        const img = imgMap[n.base_image_id];
        let hwChips = '';
        try {
            const hw = n.hardware_profile
                ? (typeof n.hardware_profile === 'string' ? JSON.parse(n.hardware_profile) : n.hardware_profile)
                : null;
            if (hw) {
                const chips = [];
                if (hw.CPUCount || hw.cpu_count) chips.push(`${hw.CPUCount || hw.cpu_count} CPU`);
                if (hw.MemoryBytes || hw.memory_bytes) chips.push(fmtBytes(hw.MemoryBytes || hw.memory_bytes) + ' RAM');
                if (hw.Disks && hw.Disks.length) chips.push(`${hw.Disks.length} disk${hw.Disks.length > 1 ? 's' : ''}`);
                if (hw.NICs && hw.NICs.length) chips.push(`${hw.NICs.length} NIC${hw.NICs.length > 1 ? 's' : ''}`);
                hwChips = `<div class="hw-chips">${chips.map(c => `<span class="hw-chip">${escHtml(c)}</span>`).join('')}</div>`;
            }
        } catch (_) {}
        const hostnameHtml = (n.hostname && n.hostname !== '(none)')
            ? `${escHtml(n.hostname)}${n.hostname_auto ? ' <span class="badge badge-neutral badge-sm" title="Auto-generated hostname">auto</span>' : ''}`
            : `<span class="text-dim" style="font-style:italic">Unassigned</span>`;
        return `<tr data-key="${escHtml(n.id)}">
            <td>
                <a href="#/nodes/${n.id}" style="font-weight:500;color:var(--text-primary)">
                    ${hostnameHtml}
                </a>
                <div class="text-dim text-sm text-mono">${escHtml(n.primary_mac || '—')}</div>
            </td>
            <td class="text-sm">
                ${img
                    ? `<a href="#/images/${img.id}">${escHtml(img.name)}</a>`
                    : (n.base_image_id ? `<span class="text-dim text-mono">${n.base_image_id.substring(0, 8)}…</span>` : '<span class="text-dim">—</span>')}
            </td>
            <td>${nodeBadge(n)}</td>
            <td>${hwChips || '<span class="text-dim text-sm">—</span>'}</td>
            <td>
                ${(n.groups || []).map(g => `<span class="badge badge-neutral badge-sm">${escHtml(g)}</span>`).join(' ') || '<span class="text-dim">—</span>'}
            </td>
            <td class="text-dim text-sm">${fmtRelative(n.updated_at)}</td>
            <td>
                <div class="flex gap-6">
                    <button class="btn btn-secondary btn-sm" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(n))}, ${JSON.stringify(JSON.stringify(images))})'>Edit</button>
                    <button class="btn btn-danger btn-sm" onclick="Pages.deleteNode('${n.id}', '${escHtml(n.hostname || n.primary_mac)}')">Delete</button>
                </div>
            </td>
        </tr>`;
    },

    // nodesRefresh — called by the auto-refresh timer. Updates the nodes table
    // in-place without replacing the full page layout or showing a loading state.
    async nodesRefresh() {
        const tbody = document.getElementById('nodes-tbody');
        if (!tbody) return; // navigated away, or table was empty on initial render

        try {
            let nodes  = App._cacheGet('nodes');
            let images = App._cacheGet('images') || Pages._nodesImages || [];

            const fetches = [];
            if (!nodes)  fetches.push(API.nodes.list().then(r  => { nodes  = r.nodes   || []; App._cacheSet('nodes',  nodes);  }));
            // Only re-fetch images if cache is cold — they change infrequently.
            if (!App._cacheGet('images')) fetches.push(API.images.list().then(r => { images = r.images || []; App._cacheSet('images', images); Pages._nodesImages = images; }));
            if (fetches.length) await Promise.all(fetches);

            const imgMap = Object.fromEntries(images.map(i => [i.id, i]));

            // Update subtitle.
            const subtitle = document.getElementById('nodes-subtitle');
            if (subtitle) subtitle.textContent = `${nodes.length} node${nodes.length !== 1 ? 's' : ''} total`;

            // Diff the tbody rows.
            const existing = new Map();
            tbody.querySelectorAll('[data-key]').forEach(el => existing.set(el.dataset.key, el));

            for (const n of nodes) {
                const key = n.id;
                if (existing.has(key)) {
                    // Update only the columns that can change between refreshes.
                    const tr   = existing.get(key);
                    const cells = tr.querySelectorAll('td');
                    if (cells[2]) cells[2].innerHTML = nodeBadge(n);
                    if (cells[5]) cells[5].textContent = fmtRelative(n.updated_at);
                    // Refresh action buttons with latest node JSON (hostname may have changed).
                    if (cells[6]) cells[6].innerHTML = `<div class="flex gap-6">
                        <button class="btn btn-secondary btn-sm" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(n))}, ${JSON.stringify(JSON.stringify(images))})'>Edit</button>
                        <button class="btn btn-danger btn-sm" onclick="Pages.deleteNode('${n.id}', '${escHtml(n.hostname || n.primary_mac)}')">Delete</button>
                    </div>`;
                    existing.delete(key);
                } else {
                    // New node appeared — insert a full row.
                    tbody.insertAdjacentHTML('beforeend', Pages._nodeRow(n, imgMap, images));
                }
            }

            // Remove rows for nodes that were deleted.
            for (const [, el] of existing) el.remove();

        } catch (_) {
            // Silently ignore refresh errors — next tick will retry.
        }
    },

    showNodeModal(nodeJson, imagesJson) {
        const node   = nodeJson   ? JSON.parse(nodeJson)   : null;
        const images = imagesJson ? JSON.parse(imagesJson) : [];
        const isEdit = !!node;

        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'node-modal';

        const imgOptions = images
            .filter(i => i.status === 'ready')
            .map(i => `<option value="${escHtml(i.id)}" ${node && node.base_image_id === i.id ? 'selected' : ''}>${escHtml(i.name)}${i.version ? ' (' + i.version + ')' : ''}</option>`)
            .join('');

        overlay.innerHTML = `
            <div class="modal">
                <div class="modal-header">
                    <span class="modal-title">${isEdit ? 'Edit Node' : 'Add Node'}</span>
                    <button class="modal-close" onclick="document.getElementById('node-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <form id="node-form" onsubmit="Pages.submitNode(event, ${isEdit ? `'${node.id}'` : 'null'})">
                        <div class="form-grid">
                            <div class="form-group">
                                <label>Hostname *
                                    ${isEdit && node.hostname_auto
                                        ? ' <span class="badge badge-neutral badge-sm" title="Auto-generated hostname">auto</span>'
                                        : ''}
                                </label>
                                <div style="display:flex;gap:6px;align-items:center">
                                    <input type="text" name="hostname" id="node-hostname-input"
                                        value="${isEdit ? escHtml(node.hostname) : ''}"
                                        placeholder="${isEdit && node.hostname_auto ? escHtml(node.hostname) + ' (auto-generated)' : ''}"
                                        style="flex:1" required>
                                    ${isEdit && node.hostname_auto
                                        ? `<button type="button" class="btn btn-secondary btn-sm"
                                               onclick="Pages._regenerateHostname('${escHtml(node.primary_mac)}')"
                                               title="Pick a new auto-generated hostname">Regenerate</button>`
                                        : ''}
                                </div>
                            </div>
                            <div class="form-group">
                                <label>FQDN</label>
                                <input type="text" name="fqdn" value="${isEdit ? escHtml(node.fqdn || '') : ''}">
                            </div>
                            <div class="form-group">
                                <label>Primary MAC *</label>
                                <input type="text" name="primary_mac" value="${isEdit ? escHtml(node.primary_mac) : ''}" placeholder="aa:bb:cc:dd:ee:ff" required>
                            </div>
                            <div class="form-group">
                                <label>Base Image</label>
                                <select name="base_image_id">
                                    <option value="">Select image…</option>
                                    ${imgOptions}
                                    ${!imgOptions ? `<option disabled>No ready images available</option>` : ''}
                                </select>
                                <div id="role-mismatch-warning" class="alert alert-warning" style="display:none;margin-top:8px;font-size:12px"></div>
                            </div>
                            <div class="form-group" style="grid-column:1/-1">
                                <label>Groups (comma-separated)</label>
                                <input type="text" name="groups" value="${isEdit ? escHtml((node.groups || []).join(', ')) : ''}" placeholder="compute, gpu, infiniband">
                            </div>
                            <div class="form-group" style="grid-column:1/-1">
                                <label>Kernel Args</label>
                                <input type="text" name="kernel_args" value="${isEdit ? escHtml(node.kernel_args || '') : ''}" placeholder="quiet splash">
                            </div>
                            <div class="form-group" style="grid-column:1/-1">
                                <label>SSH Public Keys (one per line)</label>
                                <textarea name="ssh_keys" rows="3" placeholder="ssh-ed25519 AAAA…">${isEdit ? escHtml((node.ssh_keys || []).join('\n')) : ''}</textarea>
                            </div>
                        </div>

                        <!-- Power Provider section -->
                        <div style="margin-top:20px;padding-top:16px;border-top:1px solid var(--border)">
                            <div style="font-weight:600;font-size:13px;margin-bottom:12px;color:var(--text-secondary)">Power Provider</div>
                            <div class="form-grid">
                                <div class="form-group" style="grid-column:1/-1">
                                    <label>Provider Type</label>
                                    <select name="power_provider_type" id="pp-type-select"
                                        onchange="Pages._onPowerProviderTypeChange(this.value)"
                                        value="${isEdit && node.power_provider ? escHtml(node.power_provider.type || '') : ''}">
                                        <option value="">None — no power management</option>
                                        <option value="ipmi" ${isEdit && node.power_provider && node.power_provider.type === 'ipmi' ? 'selected' : ''}>IPMI (uses BMC config)</option>
                                        <option value="proxmox" ${isEdit && node.power_provider && node.power_provider.type === 'proxmox' ? 'selected' : ''}>Proxmox VE</option>
                                    </select>
                                </div>
                            </div>
                            <!-- Proxmox fields — shown/hidden by JS -->
                            <div id="pp-proxmox-fields" style="display:${isEdit && node.power_provider && node.power_provider.type === 'proxmox' ? '' : 'none'}">
                                <div class="form-grid">
                                    <div class="form-group">
                                        <label>API URL</label>
                                        <input type="text" name="pp_api_url"
                                            value="${isEdit && node.power_provider && node.power_provider.fields ? escHtml(node.power_provider.fields.api_url || '') : ''}"
                                            placeholder="https://proxmox.example.com:8006">
                                    </div>
                                    <div class="form-group">
                                        <label>Node Name</label>
                                        <input type="text" name="pp_node"
                                            value="${isEdit && node.power_provider && node.power_provider.fields ? escHtml(node.power_provider.fields.node || '') : ''}"
                                            placeholder="pve">
                                    </div>
                                    <div class="form-group">
                                        <label>VM ID</label>
                                        <input type="text" name="pp_vmid"
                                            value="${isEdit && node.power_provider && node.power_provider.fields ? escHtml(node.power_provider.fields.vmid || '') : ''}"
                                            placeholder="202">
                                    </div>
                                    <div class="form-group">
                                        <label>Username</label>
                                        <input type="text" name="pp_username"
                                            value="${isEdit && node.power_provider && node.power_provider.fields ? escHtml(node.power_provider.fields.username || '') : ''}"
                                            placeholder="root@pam">
                                    </div>
                                    <div class="form-group">
                                        <label>Password</label>
                                        <input type="password" name="pp_password"
                                            placeholder="${isEdit && node.power_provider && node.power_provider.fields && node.power_provider.fields.password === '****' ? '(saved — leave blank to keep)' : 'Enter password'}">
                                    </div>
                                    <div class="form-group" style="display:flex;align-items:center;gap:8px;padding-top:22px">
                                        <input type="checkbox" name="pp_insecure" id="pp-insecure"
                                            ${isEdit && node.power_provider && node.power_provider.fields && node.power_provider.fields.insecure === 'true' ? 'checked' : ''}>
                                        <label for="pp-insecure" style="margin:0;font-weight:400">Skip TLS verification (self-signed certs)</label>
                                    </div>
                                </div>
                            </div>
                        </div>
                        <!-- End Power Provider section -->

                        <!-- Shared Mounts section -->
                        <div style="margin-top:20px;padding-top:16px;border-top:1px solid var(--border)">
                            <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px">
                                <div style="font-weight:600;font-size:13px;color:var(--text-secondary)">Additional Mounts (fstab)</div>
                                <div style="display:flex;gap:6px;align-items:center">
                                    <select id="mount-preset-select" onchange="Pages._applyMountPreset()" style="font-size:12px;padding:4px 6px">
                                        <option value="">Insert preset…</option>
                                        <option value="nfs-home">NFS home directory</option>
                                        <option value="lustre">Lustre scratch</option>
                                        <option value="beegfs">BeeGFS data</option>
                                        <option value="cifs">CIFS share (Windows)</option>
                                        <option value="bind">Bind mount</option>
                                        <option value="tmpfs">tmpfs for /tmp</option>
                                    </select>
                                    <button type="button" class="btn btn-secondary btn-sm" onclick="Pages._addMountRow()">+ Add Mount</button>
                                </div>
                            </div>
                            <div id="mounts-table-wrap">
                                <table id="mounts-table" style="width:100%;font-size:12px;border-collapse:collapse">
                                    <thead>
                                        <tr style="border-bottom:1px solid var(--border)">
                                            <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Source</th>
                                            <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Mount Point</th>
                                            <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">FS Type</th>
                                            <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Options</th>
                                            <th style="text-align:center;padding:4px 6px;font-weight:500;color:var(--text-secondary)" title="Auto-create the mount point directory">mkd</th>
                                            <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Comment</th>
                                            <th style="padding:4px"></th>
                                        </tr>
                                    </thead>
                                    <tbody id="mounts-tbody">
                                        ${(() => {
                                            const mounts = (isEdit && node.extra_mounts) ? node.extra_mounts : [];
                                            return mounts.map((m, i) => Pages._mountRowHTML(i, m)).join('');
                                        })()}
                                    </tbody>
                                </table>
                                ${(() => {
                                    const mounts = (isEdit && node.extra_mounts) ? node.extra_mounts : [];
                                    return mounts.length === 0 ? '<div id="mounts-empty" style="text-align:center;padding:12px;color:var(--text-dim);font-size:12px">No additional mounts configured</div>' : '';
                                })()}
                            </div>
                        </div>
                        <!-- End Shared Mounts section -->

                        <div id="node-form-result"></div>
                        <div class="form-actions">
                            <button type="button" class="btn btn-secondary" onclick="document.getElementById('node-modal').remove()">Cancel</button>
                            <button type="submit" class="btn btn-primary" id="node-submit-btn">${isEdit ? 'Save Changes' : 'Create Node'}</button>
                        </div>
                    </form>
                </div>
            </div>`;

        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });

        // Wire up role-mismatch warning when admin changes the base image selection.
        const imageSelect = overlay.querySelector('select[name="base_image_id"]');
        if (imageSelect && node) {
            const checkMismatch = () => Pages._checkRoleMismatch(imageSelect.value, node, images);
            imageSelect.addEventListener('change', checkMismatch);
            // Run once on open so pre-selected images are validated immediately.
            if (node.base_image_id) checkMismatch();
        }

        // Attach live-validation listeners after DOM is ready.
        const tbody = document.getElementById('mounts-tbody');
        if (tbody) {
            tbody.addEventListener('input', () => Pages._validateMountRows());
            tbody.addEventListener('change', (e) => {
                // When fs_type changes, suggest _netdev for network filesystems.
                if (e.target && e.target.name === 'mount_fs_type') {
                    Pages._onFSTypeChange(e.target);
                }
            });
        }
    },

    // _mountRowHTML builds the HTML for a single mount row in the fstab editor.
    _mountRowHTML(idx, m) {
        m = m || {};
        const networkFSTypes = ['nfs','nfs4','cifs','smbfs','beegfs','lustre','gpfs','9p'];
        const fsTypes = ['nfs','nfs4','cifs','beegfs','lustre','gpfs','xfs','ext4','bind','9p','tmpfs','vfat','ext3','smbfs'];
        const fsSelect = fsTypes.map(t =>
            `<option value="${t}"${m.fs_type === t ? ' selected' : ''}>${t}</option>`
        ).join('');
        return `<tr data-mount-idx="${idx}" style="border-bottom:1px solid var(--border)">
            <td style="padding:4px 3px">
                <input type="text" name="mount_source" value="${escHtml(m.source||'')}"
                    placeholder="nfs-server:/export/home" style="width:100%;min-width:120px;font-size:12px"
                    required>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="mount_point" value="${escHtml(m.mount_point||'')}"
                    placeholder="/home/shared" style="width:100%;min-width:100px;font-size:12px"
                    required pattern="/.+">
            </td>
            <td style="padding:4px 3px">
                <select name="mount_fs_type" style="font-size:12px;padding:2px 4px">
                    ${fsSelect}
                </select>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="mount_options" value="${escHtml(m.options||'')}"
                    placeholder="defaults,_netdev" style="width:100%;min-width:120px;font-size:12px">
            </td>
            <td style="padding:4px 3px;text-align:center">
                <input type="checkbox" name="mount_auto_mkdir" ${m.auto_mkdir !== false ? 'checked' : ''}>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="mount_comment" value="${escHtml(m.comment||'')}"
                    placeholder="optional note" style="width:100%;min-width:80px;font-size:12px">
            </td>
            <td style="padding:4px 3px">
                <button type="button" class="btn btn-danger btn-sm"
                    onclick="Pages._removeMountRow(this)" style="padding:2px 6px;font-size:11px">✕</button>
            </td>
        </tr>`;
    },

    // _addMountRow appends a blank mount row to the table.
    _addMountRow(preset) {
        const tbody = document.getElementById('mounts-tbody');
        const empty = document.getElementById('mounts-empty');
        if (!tbody) return;
        if (empty) empty.remove();
        const idx = tbody.querySelectorAll('tr').length;
        tbody.insertAdjacentHTML('beforeend', Pages._mountRowHTML(idx, preset || {}));
        Pages._validateMountRows();
    },

    // _removeMountRow removes the row containing the given button.
    _removeMountRow(btn) {
        const row = btn.closest('tr');
        if (row) row.remove();
        const tbody = document.getElementById('mounts-tbody');
        if (tbody && tbody.querySelectorAll('tr').length === 0) {
            const wrap = document.getElementById('mounts-table-wrap');
            if (wrap && !document.getElementById('mounts-empty')) {
                wrap.insertAdjacentHTML('beforeend',
                    '<div id="mounts-empty" style="text-align:center;padding:12px;color:var(--text-dim);font-size:12px">No additional mounts configured</div>');
            }
        }
        Pages._validateMountRows();
    },

    // _validateMountRows highlights rows with missing required fields.
    _validateMountRows() {
        const tbody = document.getElementById('mounts-tbody');
        if (!tbody) return;
        let hasErrors = false;
        tbody.querySelectorAll('tr').forEach(row => {
            const src = row.querySelector('[name="mount_source"]');
            const mp  = row.querySelector('[name="mount_point"]');
            let rowOk = true;
            if (src && !src.value.trim()) { src.style.border = '1px solid var(--error)'; rowOk = false; }
            else if (src) src.style.border = '';
            if (mp && (!mp.value.trim() || mp.value[0] !== '/')) {
                mp.style.border = '1px solid var(--error)'; rowOk = false;
            } else if (mp) mp.style.border = '';
            if (!rowOk) hasErrors = true;
        });
        const btn = document.getElementById('node-submit-btn');
        if (btn) btn.disabled = hasErrors;
    },

    // _onFSTypeChange auto-suggests _netdev for network filesystems when the
    // options field is empty.
    _onFSTypeChange(select) {
        const networkFS = ['nfs','nfs4','cifs','smbfs','beegfs','lustre','gpfs','9p'];
        const row = select.closest('tr');
        if (!row) return;
        const optsInput = row.querySelector('[name="mount_options"]');
        if (!optsInput || optsInput.value.trim()) return; // don't overwrite existing
        if (networkFS.includes(select.value)) {
            optsInput.value = 'defaults,_netdev';
        }
    },

    // _applyMountPreset inserts a preset row based on the dropdown selection.
    _applyMountPreset() {
        const sel = document.getElementById('mount-preset-select');
        if (!sel || !sel.value) return;
        const presets = {
            'nfs-home': { source: 'nfs-server:/export/home', mount_point: '/home/shared', fs_type: 'nfs4', options: 'defaults,_netdev,vers=4', auto_mkdir: true, comment: 'NFS home directory' },
            'lustre':   { source: 'mgs@tcp:/scratch',        mount_point: '/scratch',     fs_type: 'lustre', options: 'defaults,_netdev,flock', auto_mkdir: true, comment: 'Lustre scratch' },
            'beegfs':   { source: 'beegfs',                  mount_point: '/mnt/beegfs',  fs_type: 'beegfs', options: 'defaults,_netdev',       auto_mkdir: true, comment: 'BeeGFS data' },
            'cifs':     { source: '//winserver/share',       mount_point: '/mnt/share',   fs_type: 'cifs',   options: 'defaults,_netdev,vers=3.0,sec=ntlmssp', auto_mkdir: true, comment: 'CIFS (Windows) share' },
            'bind':     { source: '/data/src',               mount_point: '/data/dst',    fs_type: 'bind',   options: 'defaults,bind',           auto_mkdir: true, comment: 'Bind mount' },
            'tmpfs':    { source: 'tmpfs',                   mount_point: '/tmp',         fs_type: 'tmpfs',  options: 'defaults,size=4G,mode=1777', auto_mkdir: false, comment: 'tmpfs /tmp' },
        };
        const p = presets[sel.value];
        if (p) Pages._addMountRow(p);
        sel.value = ''; // reset dropdown
    },

    // _collectMounts reads all mount rows from the form and returns an array
    // of FstabEntry objects ready for the API body.
    _collectMounts() {
        const tbody = document.getElementById('mounts-tbody');
        if (!tbody) return [];
        const rows = tbody.querySelectorAll('tr');
        const mounts = [];
        rows.forEach(row => {
            const source    = (row.querySelector('[name="mount_source"]')?.value || '').trim();
            const mountPoint = (row.querySelector('[name="mount_point"]')?.value || '').trim();
            const fsType    = row.querySelector('[name="mount_fs_type"]')?.value || 'nfs';
            const options   = (row.querySelector('[name="mount_options"]')?.value || '').trim();
            const autoMkdir = row.querySelector('[name="mount_auto_mkdir"]')?.checked !== false;
            const comment   = (row.querySelector('[name="mount_comment"]')?.value || '').trim();
            if (!source || !mountPoint) return; // skip incomplete rows
            mounts.push({ source, mount_point: mountPoint, fs_type: fsType, options, auto_mkdir: autoMkdir, comment, dump: 0, pass: 0 });
        });
        return mounts;
    },

    // _onPowerProviderTypeChange shows/hides the Proxmox fields when the provider
    // type dropdown changes. Called by the onchange handler in the node edit modal.
    _onPowerProviderTypeChange(type) {
        const proxmoxFields = document.getElementById('pp-proxmox-fields');
        if (proxmoxFields) proxmoxFields.style.display = (type === 'proxmox') ? '' : 'none';
    },

    async submitNode(e, nodeId) {
        e.preventDefault();
        const form = e.target;
        const btn  = document.getElementById('node-submit-btn');
        const res  = document.getElementById('node-form-result');
        const data = new FormData(form);

        btn.disabled = true;
        btn.textContent = 'Saving…';
        res.innerHTML = '';

        const groups  = data.get('groups').split(',').map(g => g.trim()).filter(Boolean);
        const sshKeys = data.get('ssh_keys').split('\n').map(k => k.trim()).filter(Boolean);

        // Build power_provider from form fields.
        const ppType = data.get('power_provider_type') || '';
        let powerProvider = null;
        if (ppType === 'proxmox') {
            const fields = {
                api_url:  data.get('pp_api_url') || '',
                node:     data.get('pp_node') || '',
                vmid:     data.get('pp_vmid') || '',
                username: data.get('pp_username') || '',
                insecure: document.getElementById('pp-insecure') && document.getElementById('pp-insecure').checked ? 'true' : 'false',
            };
            // Only include password if the user typed something; blank means keep existing.
            const pw = data.get('pp_password');
            if (pw) fields.password = pw;
            powerProvider = { type: 'proxmox', fields };
        } else if (ppType === 'ipmi') {
            powerProvider = { type: 'ipmi', fields: {} };
        }

        const body = {
            hostname:       data.get('hostname'),
            fqdn:           data.get('fqdn'),
            primary_mac:    data.get('primary_mac'),
            base_image_id:  data.get('base_image_id'),
            groups,
            ssh_keys:       sshKeys,
            kernel_args:    data.get('kernel_args'),
            interfaces:     [],
            custom_vars:    {},
            power_provider: powerProvider,
            extra_mounts:   Pages._collectMounts(),
        };

        try {
            if (nodeId) {
                await API.nodes.update(nodeId, body);
            } else {
                await API.nodes.create(body);
            }
            document.getElementById('node-modal').remove();
            Pages.nodes();
        } catch (ex) {
            res.innerHTML = `<div class="form-error">${escHtml(ex.message)}</div>`;
            btn.disabled = false;
            btn.textContent = nodeId ? 'Save Changes' : 'Create Node';
        }
    },

    async deleteNode(id, name) {
        if (!confirm(`Delete node "${name}"? This cannot be undone.`)) return;
        try {
            await API.nodes.del(id);
            Pages.nodes();
        } catch (e) {
            alert(`Delete failed: ${e.message}`);
        }
    },

    // _regenerateHostname picks a new random 4-hex suffix and fills the hostname field.
    // Called by the Regenerate button in the node edit modal.
    _regenerateHostname(mac) {
        const input = document.getElementById('node-hostname-input');
        if (!input) return;
        // Generate a random 4-character hex suffix (not MAC-derived, so it's clearly new).
        const suffix = Math.floor(Math.random() * 0xffff).toString(16).padStart(4, '0');
        input.value = 'clonr-' + suffix;
        input.focus();
    },

    // ── Node Detail ────────────────────────────────────────────────────────

    // _nodeEditorState tracks per-tab dirty state for inline editing.
    // Structure: { tabId: { dirty: bool, original: {}, current: {} } }
    _nodeEditorState: {},

    // _nodeEditorNodeId is the node ID currently loaded in nodeDetail.
    _nodeEditorNodeId: null,

    async nodeDetail(id) {
        App.render(loading('Loading node…'));
        // Reset per-tab dirty state on page load.
        Pages._nodeEditorState = {};
        Pages._nodeEditorNodeId = id;

        try {
            const [node, imagesResp, nodeGroupsResp, reimagesResp] = await Promise.all([
                API.nodes.get(id),
                API.images.list(),
                API.nodeGroups.list().catch(() => ({ groups: [] })),
                API.reimages.listForNode(id).catch(() => ({ requests: [] })),
            ]);
            const images     = imagesResp.images || [];
            const nodeGroups = (nodeGroupsResp && (nodeGroupsResp.node_groups || nodeGroupsResp.groups)) || [];
            const img        = images.find(i => i.id === node.base_image_id);
            const reimageHistory = (reimagesResp && reimagesResp.requests) || [];

            let hw = null;
            try {
                if (node.hardware_profile) {
                    hw = typeof node.hardware_profile === 'string'
                        ? JSON.parse(node.hardware_profile)
                        : node.hardware_profile;
                }
            } catch (_) {}

            const displayName = node.hostname || node.primary_mac;

            // Build capture-this-node button HTML if node has a configured IP.
            let captureBtn = '';
            const iface = (node.interfaces || []).find(i => i.ip_address);
            if (iface) {
                const ip = iface.ip_address.split('/')[0];
                const prefillHost = 'root@' + ip;
                const prefillName = (node.hostname && node.hostname !== '(none)')
                    ? node.hostname.toLowerCase().replace(/[^a-z0-9-]/g, '-') + '-capture'
                    : '';
                captureBtn = '<button class="btn btn-secondary" onclick="Pages.showCaptureModal(' +
                    JSON.stringify(prefillHost) + ',' + JSON.stringify(prefillName) + ')">Capture this node</button>';
            }

            // Ready image options for Overview tab image selector.
            const imgOptions = images
                .filter(i => i.status === 'ready')
                .map(i => `<option value="${escHtml(i.id)}" ${node.base_image_id === i.id ? 'selected' : ''}>${escHtml(i.name)}${i.version ? ' (' + i.version + ')' : ''}</option>`)
                .join('');

            // Node group options for Overview tab.
            const groupOptions = nodeGroups
                .map(g => `<option value="${escHtml(g.id)}" ${node.group_id === g.id ? 'selected' : ''}>${escHtml(g.name)}</option>`)
                .join('');

            // Discovered NIC MACs for Network tab (for interface editor MAC dropdowns).
            const discoveredMACs = hw && hw.NICs ? hw.NICs.map(n => n.MAC || n.MACAddress).filter(Boolean) : [];
            const discoveredMACsJSON = JSON.stringify(discoveredMACs);

            App.render(`
                <div class="breadcrumb">
                    <a href="#/nodes">Nodes</a>
                    <span class="breadcrumb-sep">/</span>
                    <span>${escHtml(displayName)}</span>
                </div>
                <div class="page-header">
                    <div style="display:flex;align-items:center;gap:12px">
                        <button class="detail-back-btn" onclick="Pages._nodeDetailBack()">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <polyline points="15 18 9 12 15 6"/>
                            </svg>
                            Back
                        </button>
                        <div>
                            <div class="page-title" style="display:flex;align-items:center;gap:8px">
                                ${(node.hostname && node.hostname !== '(none)')
                                    ? escHtml(node.hostname)
                                    : `<span class="text-dim" style="font-style:italic">Unassigned</span>`}
                                ${node.hostname_auto ? `<span class="badge badge-neutral badge-sm" title="Auto-generated hostname">auto</span>` : ''}
                            </div>
                            <div class="page-subtitle text-mono">${escHtml(node.primary_mac)}</div>
                        </div>
                        ${nodeBadge(node)}
                    </div>
                    <div class="flex gap-8">
                        ${captureBtn}
                        <div class="actions-dropdown" id="node-actions-dropdown">
                            <button class="btn btn-secondary" onclick="Pages._toggleActionsDropdown()">
                                Actions
                                <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><polyline points="6 9 12 15 18 9"/></svg>
                            </button>
                            <div class="actions-dropdown-menu" id="node-actions-menu">
                                <button class="actions-dropdown-item" onclick="Pages._nodeActionsRediscover('${node.id}');Pages._toggleActionsDropdown()">Re-discover hardware</button>
                                <button class="actions-dropdown-item" onclick="Pages._nodeActionsTriggerReimage('${node.id}','${escHtml(displayName)}');Pages._toggleActionsDropdown()">Trigger reimage</button>
                                ${iface ? `<button class="actions-dropdown-item" onclick="Pages.showCaptureModal(${JSON.stringify('root@' + iface.ip_address.split('/')[0])},${JSON.stringify((node.hostname && node.hostname !== '(none)') ? node.hostname.toLowerCase().replace(/[^a-z0-9-]/g, '-') + '-capture' : '')});Pages._toggleActionsDropdown()">Capture as image</button>` : ''}
                                <div class="actions-dropdown-sep"></div>
                                <button class="actions-dropdown-item danger" onclick="Pages.deleteNodeAndGoBack('${node.id}', '${escHtml(displayName)}');Pages._toggleActionsDropdown()">Delete node</button>
                            </div>
                        </div>
                        <button class="btn btn-danger btn-sm" onclick="Pages.deleteNodeAndGoBack('${node.id}', '${escHtml(displayName)}')">Delete</button>
                    </div>
                </div>

                <div class="tab-bar" id="node-tab-bar">
                    <div class="tab active" id="node-tab-btn-overview" onclick="Pages._switchNodeTab(this, 'tab-overview', 'overview')">Overview</div>
                    <div class="tab" id="node-tab-btn-hardware" onclick="Pages._switchNodeTab(this, 'tab-hardware', 'hardware')">Hardware</div>
                    <div class="tab" id="node-tab-btn-network" onclick="Pages._switchNodeTab(this, 'tab-network', 'network')">Network</div>
                    <div class="tab" id="node-tab-btn-bmc" onclick="Pages._switchNodeTab(this, 'tab-bmc', 'bmc');Pages._onBMCTabOpen('${node.id}', ${!!(node.bmc || node.power_provider)})">Power / IPMI</div>
                    <div class="tab" id="node-tab-btn-disklayout" onclick="Pages._switchNodeTab(this, 'tab-disklayout', 'disklayout');Pages._onDiskLayoutTabOpen('${node.id}')">Disk Layout</div>
                    <div class="tab" id="node-tab-btn-mounts" onclick="Pages._switchNodeTab(this, 'tab-mounts', 'mounts');Pages._onMountsTabOpen('${node.id}')">Mounts</div>
                    <div class="tab" id="node-tab-btn-config" onclick="Pages._switchNodeTab(this, 'tab-config', 'config')">Configuration</div>
                    <div class="tab" id="node-tab-btn-logs" onclick="Pages._switchNodeTab(this, 'tab-logs', 'logs');Pages.loadNodeLogs('${escHtml(node.primary_mac)}')">Logs</div>
                </div>

                <!-- Overview tab — inline editable -->
                <div id="tab-overview" class="tab-panel active">
                    <div id="tab-save-bar-overview" class="tab-save-bar" style="display:none">
                        <span class="save-status modified" id="tab-save-status-overview">Unsaved changes</span>
                        <button class="btn btn-secondary btn-sm" onclick="Pages._tabRevert('overview')" id="tab-revert-overview">Revert</button>
                        <button class="btn btn-primary btn-sm" onclick="Pages._tabSaveOverview('${node.id}')" id="tab-save-overview">Save</button>
                    </div>
                    ${cardWrap('Node Details', `
                        <div class="card-body">
                            <div class="form-grid" style="margin-bottom:0">
                                <div class="form-group">
                                    <label>Hostname</label>
                                    <input type="text" id="ov-hostname" value="${escHtml(node.hostname || '')}"
                                        placeholder="clonr-node" pattern="^[a-zA-Z0-9][a-zA-Z0-9.-]*$"
                                        oninput="Pages._tabMarkDirty('overview')">
                                </div>
                                <div class="form-group">
                                    <label>FQDN</label>
                                    <input type="text" id="ov-fqdn" value="${escHtml(node.fqdn || '')}"
                                        placeholder="node.example.com"
                                        oninput="Pages._tabMarkDirty('overview')">
                                </div>
                                <div class="form-group">
                                    <label>Base Image</label>
                                    <select id="ov-base-image" onchange="Pages._tabMarkDirty('overview');Pages._checkRoleMismatchInline(this.value, ${JSON.stringify(node)}, ${JSON.stringify(images)})">
                                        <option value="">No image assigned</option>
                                        ${imgOptions}
                                    </select>
                                    <div id="ov-role-mismatch-warning" class="alert alert-warning" style="display:none;margin-top:6px;font-size:12px"></div>
                                </div>
                                <div class="form-group">
                                    <label>Node Group</label>
                                    <select id="ov-group-id" onchange="Pages._tabMarkDirty('overview')">
                                        <option value="">None</option>
                                        ${groupOptions}
                                    </select>
                                </div>
                                <div class="form-group" style="grid-column:1/-1">
                                    <label>Groups / Tags <span style="font-size:11px;color:var(--text-secondary)">(comma-separated)</span></label>
                                    <input type="text" id="ov-groups" value="${escHtml((node.groups || []).join(', '))}"
                                        placeholder="compute, gpu, infiniband"
                                        oninput="Pages._tabMarkDirty('overview')">
                                </div>
                                <div class="form-group" style="grid-column:1/-1">
                                    <label>Reimage Status</label>
                                    <div style="display:flex;align-items:center;gap:12px;padding:8px 0">
                                        ${node.reimage_pending
                                            ? `<span class="badge badge-warning">Reimage pending</span>
                                               <span class="text-dim" style="font-size:12px">Node will re-deploy on next PXE boot</span>`
                                            : `<span class="badge badge-neutral">Normal</span>
                                               <button type="button" class="btn btn-secondary btn-sm" onclick="Pages._nodeActionsTriggerReimage('${node.id}', '${escHtml(displayName)}')">Request Reimage</button>`}
                                    </div>
                                </div>
                            </div>
                        </div>`)}

                    ${cardWrap('Node Info', `
                        <div class="card-body">
                            <div class="kv-grid">
                                <div class="kv-item"><div class="kv-key">ID</div><div class="kv-value">${escHtml(node.id)}</div></div>
                                <div class="kv-item"><div class="kv-key">Primary MAC</div><div class="kv-value text-mono">${escHtml(node.primary_mac)}</div></div>
                                <div class="kv-item"><div class="kv-key">Status</div><div class="kv-value">${nodeBadge(node)}</div></div>
                                <div class="kv-item"><div class="kv-key">Current Image</div><div class="kv-value">
                                    ${img ? `<a href="#/images/${img.id}">${escHtml(img.name)}</a> ${badge(img.status)}` : (node.base_image_id ? escHtml(node.base_image_id) : '—')}
                                </div></div>
                                <div class="kv-item"><div class="kv-key">Node Group</div><div class="kv-value">
                                    ${node.group_id
                                        ? (() => { const g = nodeGroups.find(x => x.id === node.group_id); return g ? `<a href="#/nodes/groups/${g.id}">${escHtml(g.name)}</a>` : `<span class="text-mono text-dim text-sm">${escHtml(node.group_id)}</span>`; })()
                                        : '<span class="text-dim">—</span>'}
                                </div></div>
                                <div class="kv-item"><div class="kv-key">Last Deploy OK</div><div class="kv-value">${node.last_deploy_succeeded_at ? fmtDate(node.last_deploy_succeeded_at) : '—'}</div></div>
                                <div class="kv-item"><div class="kv-key">Last Deploy Failed</div><div class="kv-value">${node.last_deploy_failed_at ? fmtDate(node.last_deploy_failed_at) : '—'}</div></div>
                                <div class="kv-item"><div class="kv-key">Created</div><div class="kv-value">${fmtDate(node.created_at)}</div></div>
                                <div class="kv-item"><div class="kv-key">Updated</div><div class="kv-value">${fmtDate(node.updated_at)}</div></div>
                            </div>
                        </div>`)}

                    ${cardWrap('Reimage History', (() => {
                        const recent = reimageHistory.slice(0, 10);
                        if (recent.length === 0) {
                            return `<div class="card-body">${emptyState('No reimage history', 'Reimage requests appear here after the first deploy.')}</div>`;
                        }
                        const statusBadge = (s) => {
                            const cls = {
                                complete:    'badge-success',
                                succeeded:   'badge-success',
                                failed:      'badge-danger',
                                in_progress: 'badge-warning',
                                triggered:   'badge-warning',
                                pending:     'badge-neutral',
                                canceled:    'badge-neutral',
                            }[s] || 'badge-neutral';
                            return `<span class="badge ${cls}">${escHtml(s)}</span>`;
                        };
                        const rows = recent.map(r => {
                            const failDetail = r.status === 'failed' && (r.exit_code != null || r.phase)
                                ? `<div class="text-dim text-sm" style="margin-top:2px">exit&nbsp;${r.exit_code ?? '?'}&nbsp;(${escHtml(r.exit_name || r.phase || 'unknown')})</div>`
                                : '';
                            const errTip = r.error_message
                                ? `title="${escHtml(r.error_message)}"`
                                : '';
                            return `<tr>
                                <td class="text-mono text-sm" ${errTip}>${escHtml(r.id.slice(0,8))}</td>
                                <td>${statusBadge(r.status)}${failDetail}</td>
                                <td class="text-sm">${r.completed_at ? fmtDate(r.completed_at) : (r.created_at ? fmtDate(r.created_at) : '—')}</td>
                                <td class="text-sm text-dim">${escHtml(r.phase || '—')}</td>
                            </tr>`;
                        }).join('');
                        return `<div class="card-body" style="padding:0">
                            <table style="width:100%;border-collapse:collapse;font-size:13px">
                                <thead><tr style="border-bottom:1px solid var(--border)">
                                    <th style="padding:8px 16px;text-align:left;font-weight:500;color:var(--text-secondary)">ID</th>
                                    <th style="padding:8px 16px;text-align:left;font-weight:500;color:var(--text-secondary)">Status</th>
                                    <th style="padding:8px 16px;text-align:left;font-weight:500;color:var(--text-secondary)">When</th>
                                    <th style="padding:8px 16px;text-align:left;font-weight:500;color:var(--text-secondary)">Phase</th>
                                </tr></thead>
                                <tbody>${rows}</tbody>
                            </table>
                        </div>`;
                    })())}
                </div>

                <!-- Hardware tab — read-only display + re-discover action -->
                <div id="tab-hardware" class="tab-panel">
                    <div style="display:flex;align-items:center;gap:12px;margin-bottom:16px">
                        ${hw && hw.discovered_at ? `<span class="text-dim" style="font-size:12px">Last discovered: ${fmtRelative(hw.discovered_at)}</span>` : ''}
                        <button class="btn btn-secondary btn-sm" style="margin-left:auto" onclick="Pages._nodeActionsRediscover('${node.id}')">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><polyline points="23 4 23 10 17 10"/><path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/></svg>
                            Re-discover Hardware
                        </button>
                    </div>
                    ${hw ? this._hardwareProfile(hw) : `<div class="card"><div class="card-body">${emptyState('No hardware profile', 'Hardware is discovered when a node registers via PXE boot.')}</div></div>`}
                </div>

                <!-- Network tab — inline editable interface configs -->
                <div id="tab-network" class="tab-panel">
                    <div id="tab-save-bar-network" class="tab-save-bar" style="display:none">
                        <span class="save-status modified" id="tab-save-status-network">Unsaved changes</span>
                        <button class="btn btn-secondary btn-sm" onclick="Pages._tabRevert('network')" id="tab-revert-network">Revert</button>
                        <button class="btn btn-primary btn-sm" onclick="Pages._tabSaveNetwork('${node.id}')" id="tab-save-network">Save</button>
                    </div>
                    ${cardWrap('Network Interfaces', `
                        <div class="card-body">
                            <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px">
                                <span class="text-dim" style="font-size:12px">Configure logical interfaces. Discovered interfaces are shown read-only on the Hardware tab.</span>
                                <button type="button" class="btn btn-secondary btn-sm" data-macs="${escHtml(discoveredMACsJSON)}" onclick="Pages._netAddInterface(JSON.parse(this.dataset.macs))">+ Add Interface</button>
                            </div>
                            <div id="net-interfaces-list">
                                ${(node.interfaces || []).length === 0
                                    ? `<div id="net-empty" style="text-align:center;padding:16px;color:var(--text-dim);font-size:12px">No interfaces configured</div>`
                                    : (node.interfaces || []).map((iface, i) => Pages._netInterfaceRowHTML(i, iface, discoveredMACs)).join('')}
                            </div>
                        </div>`)}
                </div>

                <!-- Power / IPMI tab — power controls + inline provider editor -->
                <div id="tab-bmc" class="tab-panel">
                    ${(node.bmc && node.bmc.ip_address) || (node.power_provider && node.power_provider.type) ? `
                    ${node.bmc && node.bmc.ip_address ? cardWrap('Power Status',
                        `<div class="card-body">
                            <div id="power-status-panel" style="display:flex;align-items:center;gap:16px;margin-bottom:16px">
                                <div id="power-indicator" style="width:18px;height:18px;border-radius:50%;background:var(--border);flex-shrink:0"></div>
                                <div>
                                    <div id="power-label" style="font-weight:600;font-size:15px">Checking…</div>
                                    <div id="power-last-checked" class="text-dim text-sm"></div>
                                </div>
                                <button class="btn btn-secondary btn-sm" style="margin-left:auto" onclick="Pages._refreshPowerStatus('${node.id}')">Refresh</button>
                            </div>
                            <div id="power-error-msg" style="display:none" class="alert alert-error"></div>
                        </div>`,
                        ''
                    ) : ''}

                    ${node.bmc && node.bmc.ip_address ? cardWrap('Power Controls',
                        `<div class="card-body">
                            <div class="flex gap-8" style="flex-wrap:wrap;margin-bottom:8px">
                                <button id="btn-power-on"    class="btn btn-secondary btn-sm" onclick="Pages._doPowerAction('${node.id}', 'on')">Power On</button>
                                <button id="btn-power-off"   class="btn btn-danger btn-sm"    onclick="Pages._confirmPowerAction('${node.id}', 'off',   'Power Off', 'This will immediately cut power to the node.')">Power Off</button>
                                <button id="btn-power-cycle" class="btn btn-danger btn-sm"    onclick="Pages._confirmPowerAction('${node.id}', 'cycle', 'Power Cycle', 'This will hard-cycle the node (power off then on).')">Power Cycle</button>
                                <button id="btn-power-reset" class="btn btn-danger btn-sm"    onclick="Pages._confirmPowerAction('${node.id}', 'reset', 'Reset', 'This will issue a hard reset. The node will reboot immediately.')">Reset</button>
                            </div>
                            <div class="flex gap-8" style="flex-wrap:wrap">
                                <button class="btn btn-secondary btn-sm" onclick="Pages._confirmPowerAction('${node.id}', 'pxe',  'Boot to PXE', 'Sets next boot to PXE and power-cycles the node.')">
                                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z"/><line x1="4" y1="22" x2="4" y2="15"/></svg>
                                    PXE Boot
                                </button>
                                <button class="btn btn-secondary btn-sm" onclick="Pages._doPowerAction('${node.id}', 'disk')">
                                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
                                    Boot to Disk
                                </button>
                                <button class="btn btn-secondary btn-sm" onclick="Pages._doFlipToDisk('${node.id}')">
                                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
                                    Flip Next Boot → Disk
                                </button>
                                <button class="btn btn-danger btn-sm" onclick="Pages._doFlipToDisk('${node.id}', true)">
                                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><polyline points="23 4 23 10 17 10"/><path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/></svg>
                                    Flip → Disk + Reboot
                                </button>
                            </div>
                            <div id="power-action-feedback" style="display:none;margin-top:10px" class="alert alert-info"></div>
                        </div>`,
                        ''
                    ) : (node.power_provider && node.power_provider.type ? cardWrap('Power Actions',
                        `<div class="card-body">
                            <div class="flex gap-8">
                                <button class="btn btn-secondary btn-sm" onclick="Pages._doFlipToDisk('${node.id}')">Flip Next Boot → Disk</button>
                                <button class="btn btn-danger btn-sm" onclick="Pages._doFlipToDisk('${node.id}', true)">Flip → Disk + Reboot</button>
                            </div>
                            <div id="power-action-feedback" style="display:none;margin-top:10px" class="alert alert-info"></div>
                        </div>`, ''
                    ) : '')}

                    ${node.bmc && node.bmc.ip_address ? cardWrap('BMC Information',
                        `<div class="card-body">
                            <div class="kv-grid">
                                <div class="kv-item"><div class="kv-key">IP Address</div><div class="kv-value text-mono">${escHtml(node.bmc.ip_address || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Netmask</div><div class="kv-value text-mono">${escHtml(node.bmc.netmask || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Gateway</div><div class="kv-value text-mono">${escHtml(node.bmc.gateway || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Username</div><div class="kv-value text-mono">${escHtml(node.bmc.username || '—')}</div></div>
                            </div>
                        </div>`,
                        ''
                    ) : ''}

                    ${node.bmc && node.bmc.ip_address ? cardWrap('Sensor Readings',
                        `<div id="sensor-table-wrap"><div class="loading"><div class="spinner"></div>Loading sensors…</div></div>`,
                        `<button class="btn btn-secondary btn-sm" onclick="Pages._refreshSensors('${node.id}')">Refresh</button>`
                    ) : ''}
                    ` : ''}

                    <!-- Power Provider Configuration — always shown, inline editable -->
                    <div id="tab-save-bar-bmc" class="tab-save-bar" style="display:none">
                        <span class="save-status modified" id="tab-save-status-bmc">Unsaved changes</span>
                        <button class="btn btn-secondary btn-sm" onclick="Pages._tabRevert('bmc')" id="tab-revert-bmc">Revert</button>
                        <button class="btn btn-primary btn-sm" onclick="Pages._tabSavePower('${node.id}')" id="tab-save-bmc">Save</button>
                    </div>
                    ${cardWrap('Power Provider Configuration', `
                        <div class="card-body">
                            <div class="form-grid">
                                <div class="form-group" style="grid-column:1/-1">
                                    <label>Provider Type</label>
                                    <select id="pp-type" onchange="Pages._onPowerProviderInlineTypeChange(this.value);Pages._tabMarkDirty('bmc')">
                                        <option value="" ${!node.power_provider || !node.power_provider.type ? 'selected' : ''}>None — no power management</option>
                                        <option value="ipmi" ${node.power_provider && node.power_provider.type === 'ipmi' ? 'selected' : ''}>IPMI (uses BMC config)</option>
                                        <option value="proxmox" ${node.power_provider && node.power_provider.type === 'proxmox' ? 'selected' : ''}>Proxmox VE</option>
                                    </select>
                                </div>
                            </div>
                            <div id="pp-inline-ipmi-fields" style="display:${node.power_provider && node.power_provider.type === 'ipmi' ? '' : 'none'}">
                                <div class="form-grid">
                                    <div class="form-group">
                                        <label>BMC IP Address</label>
                                        <input type="text" id="pp-ipmi-ip"
                                            value="${escHtml(node.power_provider && node.power_provider.fields ? node.power_provider.fields.ip || '' : '')}"
                                            placeholder="192.168.1.100" oninput="Pages._tabMarkDirty('bmc')">
                                    </div>
                                    <div class="form-group">
                                        <label>Username</label>
                                        <input type="text" id="pp-ipmi-username"
                                            value="${escHtml(node.power_provider && node.power_provider.fields ? node.power_provider.fields.username || '' : '')}"
                                            placeholder="admin" oninput="Pages._tabMarkDirty('bmc')">
                                    </div>
                                    <div class="form-group">
                                        <label>Password <span style="font-size:11px;color:var(--text-secondary)">(blank = keep existing)</span></label>
                                        <input type="password" id="pp-ipmi-password" placeholder="••••••••" oninput="Pages._tabMarkDirty('bmc')" autocomplete="new-password">
                                    </div>
                                    <div class="form-group">
                                        <label>Channel</label>
                                        <input type="number" id="pp-ipmi-channel"
                                            value="${escHtml(node.power_provider && node.power_provider.fields ? node.power_provider.fields.channel || '1' : '1')}"
                                            min="1" max="15" oninput="Pages._tabMarkDirty('bmc')">
                                    </div>
                                </div>
                            </div>
                            <div id="pp-inline-proxmox-fields" style="display:${node.power_provider && node.power_provider.type === 'proxmox' ? '' : 'none'}">
                                <div class="form-grid">
                                    <div class="form-group">
                                        <label>API URL</label>
                                        <input type="text" id="pp-pve-url"
                                            value="${escHtml(node.power_provider && node.power_provider.fields ? node.power_provider.fields.api_url || '' : '')}"
                                            placeholder="https://proxmox.example.com:8006" oninput="Pages._tabMarkDirty('bmc')">
                                    </div>
                                    <div class="form-group">
                                        <label>PVE Node Name</label>
                                        <input type="text" id="pp-pve-node"
                                            value="${escHtml(node.power_provider && node.power_provider.fields ? node.power_provider.fields.node || '' : '')}"
                                            placeholder="pve" oninput="Pages._tabMarkDirty('bmc')">
                                    </div>
                                    <div class="form-group">
                                        <label>VM ID</label>
                                        <input type="text" id="pp-pve-vmid"
                                            value="${escHtml(node.power_provider && node.power_provider.fields ? node.power_provider.fields.vmid || '' : '')}"
                                            placeholder="202" oninput="Pages._tabMarkDirty('bmc')">
                                    </div>
                                    <div class="form-group">
                                        <label>Username</label>
                                        <input type="text" id="pp-pve-username"
                                            value="${escHtml(node.power_provider && node.power_provider.fields ? node.power_provider.fields.username || '' : '')}"
                                            placeholder="root@pam" oninput="Pages._tabMarkDirty('bmc')">
                                    </div>
                                    <div class="form-group">
                                        <label>Password <span style="font-size:11px;color:var(--text-secondary)">(blank = keep existing)</span></label>
                                        <input type="password" id="pp-pve-password" placeholder="••••••••" oninput="Pages._tabMarkDirty('bmc')" autocomplete="new-password">
                                    </div>
                                    <div class="form-group" style="display:flex;align-items:center;gap:8px;padding-top:22px">
                                        <input type="checkbox" id="pp-pve-insecure" ${node.power_provider && node.power_provider.fields && node.power_provider.fields.insecure === 'true' ? 'checked' : ''}
                                            onchange="Pages._tabMarkDirty('bmc')">
                                        <label for="pp-pve-insecure" style="margin:0;font-weight:400;cursor:pointer">Skip TLS verification (self-signed certs)</label>
                                    </div>
                                </div>
                            </div>
                        </div>`)}
                </div>

                <!-- Disk Layout tab — Richard's existing inline editor, untouched -->
                <div id="tab-disklayout" class="tab-panel">
                    <div id="disklayout-content">
                        <div class="loading"><div class="spinner"></div>Loading layout…</div>
                    </div>
                </div>

                <!-- Mounts tab — inline editable node-level mounts -->
                <div id="tab-mounts" class="tab-panel">
                    <div id="tab-save-bar-mounts" class="tab-save-bar" style="display:none">
                        <span class="save-status modified" id="tab-save-status-mounts">Unsaved changes</span>
                        <button class="btn btn-secondary btn-sm" onclick="Pages._tabRevert('mounts')" id="tab-revert-mounts">Revert</button>
                        <button class="btn btn-primary btn-sm" onclick="Pages._tabSaveMounts('${node.id}')" id="tab-save-mounts">Save</button>
                    </div>
                    <div id="mounts-content">
                        <div class="loading"><div class="spinner"></div>Loading mounts…</div>
                    </div>
                </div>

                <!-- Configuration tab — inline editable SSH keys, kernel args, custom vars -->
                <div id="tab-config" class="tab-panel">
                    <div id="tab-save-bar-config" class="tab-save-bar" style="display:none">
                        <span class="save-status modified" id="tab-save-status-config">Unsaved changes</span>
                        <button class="btn btn-secondary btn-sm" onclick="Pages._tabRevert('config')" id="tab-revert-config">Revert</button>
                        <button class="btn btn-primary btn-sm" onclick="Pages._tabSaveConfig('${node.id}')" id="tab-save-config">Save</button>
                    </div>
                    ${cardWrap('SSH Authorized Keys', `
                        <div class="card-body">
                            <div class="form-group" style="margin-bottom:0">
                                <label>One key per line</label>
                                <textarea id="cfg-ssh-keys" rows="6"
                                    placeholder="ssh-ed25519 AAAA…&#10;ssh-rsa AAAA…"
                                    oninput="Pages._tabMarkDirty('config')"
                                    style="font-family:var(--font-mono);font-size:12px">${escHtml((node.ssh_keys || []).join('\n'))}</textarea>
                                <div id="cfg-ssh-keys-error" style="display:none;color:var(--error);font-size:12px;margin-top:4px"></div>
                            </div>
                        </div>`)}

                    ${cardWrap('Kernel Arguments', `
                        <div class="card-body">
                            <div class="form-group" style="margin-bottom:0">
                                <label>Extra kernel cmdline args appended at boot</label>
                                <input type="text" id="cfg-kernel-args" value="${escHtml(node.kernel_args || '')}"
                                    placeholder="quiet splash"
                                    oninput="Pages._tabMarkDirty('config')">
                                <div id="cfg-kernel-args-error" style="display:none;color:var(--error);font-size:12px;margin-top:4px"></div>
                            </div>
                        </div>`)}

                    ${cardWrap('Custom Variables', `
                        <div class="card-body">
                            <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:10px">
                                <span class="text-dim" style="font-size:12px">Key/value pairs available as template variables during deployment</span>
                                <button type="button" class="btn btn-secondary btn-sm" onclick="Pages._cfgAddVar()">+ Add Variable</button>
                            </div>
                            <div id="cfg-vars-list">
                                ${Object.keys(node.custom_vars || {}).length === 0
                                    ? `<div id="cfg-vars-empty" style="text-align:center;padding:12px;color:var(--text-dim);font-size:12px">No custom variables</div>`
                                    : Object.entries(node.custom_vars || {}).map(([k, v], i) => Pages._cfgVarRowHTML(i, k, v)).join('')}
                            </div>
                        </div>`)}

                    ${cardWrap('Raw JSON', `<div class="card-body"><pre class="json-block">${escHtml(JSON.stringify(node, null, 2))}</pre></div>`)}
                </div>

                <!-- Logs tab -->
                <div id="tab-logs" class="tab-panel">
                    ${cardWrap(`Logs — ${escHtml(node.hostname || node.primary_mac)}`, `
                        <div class="log-filter-bar" style="border:none;box-shadow:none;border-bottom:1px solid var(--border);border-radius:0;padding:12px 16px">
                            <label class="toggle">
                                <input type="checkbox" id="node-follow-toggle" onchange="Pages.toggleNodeLogs(this.checked, '${escHtml(node.primary_mac)}')">
                                Live
                            </label>
                            <span class="follow-indicator" id="node-follow-ind">
                                <span class="follow-dot"></span>static
                            </span>
                        </div>
                        <div id="node-log-viewer" class="log-viewer tall"></div>`,
                        `<button class="btn btn-secondary btn-sm" onclick="Pages.loadNodeLogs('${escHtml(node.primary_mac)}')">Refresh</button>`)}
                </div>
            `);

            // Store original values for revert on each editable tab.
            Pages._nodeEditorState['overview'] = {
                dirty: false,
                original: {
                    hostname:      node.hostname || '',
                    fqdn:          node.fqdn || '',
                    base_image_id: node.base_image_id || '',
                    group_id:      node.group_id || '',
                    groups:        (node.groups || []).join(', '),
                },
            };
            Pages._nodeEditorState['bmc'] = {
                dirty: false,
                original: {
                    pp_type:          (node.power_provider && node.power_provider.type) || '',
                    pp_ipmi_ip:       (node.power_provider && node.power_provider.fields && node.power_provider.fields.ip) || '',
                    pp_ipmi_username: (node.power_provider && node.power_provider.fields && node.power_provider.fields.username) || '',
                    pp_ipmi_channel:  (node.power_provider && node.power_provider.fields && node.power_provider.fields.channel) || '1',
                    pp_pve_url:       (node.power_provider && node.power_provider.fields && node.power_provider.fields.api_url) || '',
                    pp_pve_node:      (node.power_provider && node.power_provider.fields && node.power_provider.fields.node) || '',
                    pp_pve_vmid:      (node.power_provider && node.power_provider.fields && node.power_provider.fields.vmid) || '',
                    pp_pve_username:  (node.power_provider && node.power_provider.fields && node.power_provider.fields.username) || '',
                    pp_pve_insecure:  !!(node.power_provider && node.power_provider.fields && node.power_provider.fields.insecure === 'true'),
                },
            };
            Pages._nodeEditorState['config'] = {
                dirty: false,
                original: {
                    ssh_keys:    (node.ssh_keys || []).join('\n'),
                    kernel_args: node.kernel_args || '',
                    custom_vars: Object.assign({}, node.custom_vars || {}),
                },
            };
            Pages._nodeEditorState['network'] = {
                dirty: false,
                original: {
                    interfaces: JSON.parse(JSON.stringify(node.interfaces || [])),
                },
            };
            Pages._nodeEditorState['mounts'] = {
                dirty: false,
                original: {
                    extra_mounts: JSON.parse(JSON.stringify(node.extra_mounts || [])),
                },
            };

            // Kick off initial power status fetch if any power management is configured.
            if ((node.bmc && node.bmc.ip_address) || (node.power_provider && node.power_provider.type)) {
                Pages._refreshPowerStatus(node.id);
            }

            // Close actions dropdown when clicking outside.
            document.addEventListener('click', Pages._closeActionsDropdownOnOutsideClick);

        } catch (e) {
            App.render(alertBox(`Failed to load node: ${e.message}`));
        }
    },

    // _closeActionsDropdownOnOutsideClick closes the actions dropdown when the user
    // clicks anywhere outside it. Bound as a document listener, removed on navigate.
    _closeActionsDropdownOnOutsideClick(e) {
        const dropdown = document.getElementById('node-actions-dropdown');
        if (dropdown && !dropdown.contains(e.target)) {
            const menu = document.getElementById('node-actions-menu');
            if (menu) menu.classList.remove('open');
        }
    },

    _toggleActionsDropdown() {
        const menu = document.getElementById('node-actions-menu');
        if (menu) menu.classList.toggle('open');
    },

    // _nodeDetailBack navigates back to /nodes, prompting if there are unsaved changes.
    _nodeDetailBack() {
        const dirtyTabs = Object.entries(Pages._nodeEditorState)
            .filter(([, s]) => s.dirty)
            .map(([tab]) => tab);
        if (dirtyTabs.length === 0) {
            Router.navigate('/nodes');
            return;
        }
        if (confirm(`You have unsaved changes on the ${dirtyTabs.join(', ')} tab(s). Leave without saving?`)) {
            Router.navigate('/nodes');
        }
    },

    // _switchNodeTab handles tab switching with unsaved-changes protection.
    _switchNodeTab(tabEl, panelId, tabKey) {
        const currentTabKey = Pages._nodeCurrentTab || 'overview';

        // Check if current tab is dirty.
        const currentState = Pages._nodeEditorState[currentTabKey];
        if (currentState && currentState.dirty) {
            // Show unsaved-changes dialog.
            Pages._showUnsavedChangesDialog(currentTabKey, () => {
                // Discard and continue.
                Pages._tabRevert(currentTabKey);
                Pages._doSwitchNodeTab(tabEl, panelId, tabKey);
            }, async () => {
                // Save and continue.
                const saved = await Pages._tabSaveByKey(currentTabKey, Pages._nodeEditorNodeId);
                if (saved) Pages._doSwitchNodeTab(tabEl, panelId, tabKey);
            });
            return;
        }

        Pages._doSwitchNodeTab(tabEl, panelId, tabKey);
    },

    _doSwitchNodeTab(tabEl, panelId, tabKey) {
        document.querySelectorAll('#node-tab-bar .tab').forEach(t => t.classList.remove('active'));
        document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
        tabEl.classList.add('active');
        const panel = document.getElementById(panelId);
        if (panel) panel.classList.add('active');
        Pages._nodeCurrentTab = tabKey;
    },

    // _switchTab is kept for non-node pages (image detail uses it via tab-bar).
    _switchTab(tabEl, panelId) {
        tabEl.closest('.tab-bar').querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
        document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
        tabEl.classList.add('active');
        const panel = document.getElementById(panelId);
        if (panel) panel.classList.add('active');
    },

    // _showUnsavedChangesDialog shows a confirm dialog for unsaved changes protection.
    // onDiscard — called when user clicks "Discard and continue"
    // onSaveAndContinue — called when user clicks "Save and continue"
    _showUnsavedChangesDialog(tabName, onDiscard, onSaveAndContinue) {
        const existing = document.getElementById('unsaved-changes-modal');
        if (existing) existing.remove();

        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'unsaved-changes-modal';
        overlay.innerHTML = `
            <div class="modal" style="max-width:440px">
                <div class="modal-header">
                    <span class="modal-title">Unsaved Changes</span>
                </div>
                <div class="modal-body">
                    <p style="margin:0 0 16px;color:var(--text-secondary);font-size:13px">
                        You have unsaved changes on the <strong>${escHtml(tabName)}</strong> tab.
                    </p>
                    <div class="form-actions" style="margin-top:0">
                        <button class="btn btn-secondary" id="ucd-cancel">Cancel</button>
                        <button class="btn btn-secondary" id="ucd-discard">Discard and continue</button>
                        <button class="btn btn-primary" id="ucd-save">Save and continue</button>
                    </div>
                </div>
            </div>`;
        document.body.appendChild(overlay);

        overlay.querySelector('#ucd-cancel').onclick  = () => overlay.remove();
        overlay.querySelector('#ucd-discard').onclick = () => { overlay.remove(); onDiscard(); };
        overlay.querySelector('#ucd-save').onclick    = () => { overlay.remove(); onSaveAndContinue(); };
    },

    // ── Tab dirty state tracking ───────────────────────────────────────────────

    _tabMarkDirty(tabKey) {
        const state = Pages._nodeEditorState[tabKey];
        if (!state) return;
        state.dirty = true;

        const saveBar    = document.getElementById(`tab-save-bar-${tabKey}`);
        const tabBtnEl   = document.getElementById(`node-tab-btn-${tabKey}`);
        if (saveBar)  saveBar.style.display = '';
        if (tabBtnEl) tabBtnEl.classList.add('tab-dirty');
    },

    _tabMarkClean(tabKey) {
        const state = Pages._nodeEditorState[tabKey];
        if (state) state.dirty = false;

        const saveBar    = document.getElementById(`tab-save-bar-${tabKey}`);
        const statusEl   = document.getElementById(`tab-save-status-${tabKey}`);
        const tabBtnEl   = document.getElementById(`node-tab-btn-${tabKey}`);
        if (saveBar)  saveBar.style.display = 'none';
        if (statusEl) { statusEl.textContent = 'Saved'; statusEl.className = 'save-status saved'; }
        if (tabBtnEl) tabBtnEl.classList.remove('tab-dirty');
    },

    _tabMarkSaving(tabKey) {
        const statusEl = document.getElementById(`tab-save-status-${tabKey}`);
        const saveBtn  = document.getElementById(`tab-save-${tabKey}`);
        if (statusEl) { statusEl.textContent = 'Saving…'; statusEl.className = 'save-status'; }
        if (saveBtn)  saveBtn.disabled = true;
    },

    _tabMarkError(tabKey, msg) {
        const statusEl = document.getElementById(`tab-save-status-${tabKey}`);
        const saveBtn  = document.getElementById(`tab-save-${tabKey}`);
        if (statusEl) { statusEl.textContent = msg; statusEl.className = 'save-status error'; }
        if (saveBtn)  { saveBtn.disabled = false; }
    },

    // _tabRevert resets the tab inputs back to original values without saving.
    _tabRevert(tabKey) {
        const state = Pages._nodeEditorState[tabKey];
        if (!state) return;
        const orig = state.original;

        if (tabKey === 'overview') {
            const h   = document.getElementById('ov-hostname');
            const f   = document.getElementById('ov-fqdn');
            const img = document.getElementById('ov-base-image');
            const grp = document.getElementById('ov-group-id');
            const gs  = document.getElementById('ov-groups');
            if (h)   h.value   = orig.hostname;
            if (f)   f.value   = orig.fqdn;
            if (img) img.value = orig.base_image_id;
            if (grp) grp.value = orig.group_id;
            if (gs)  gs.value  = orig.groups;
        } else if (tabKey === 'bmc') {
            const pt = document.getElementById('pp-type');
            if (pt) { pt.value = orig.pp_type; Pages._onPowerProviderInlineTypeChange(orig.pp_type); }
            const ipIp  = document.getElementById('pp-ipmi-ip');
            const ipUsr = document.getElementById('pp-ipmi-username');
            const ipCh  = document.getElementById('pp-ipmi-channel');
            if (ipIp)  ipIp.value  = orig.pp_ipmi_ip;
            if (ipUsr) ipUsr.value = orig.pp_ipmi_username;
            if (ipCh)  ipCh.value  = orig.pp_ipmi_channel;
            const pveUrl  = document.getElementById('pp-pve-url');
            const pveNode = document.getElementById('pp-pve-node');
            const pveVmid = document.getElementById('pp-pve-vmid');
            const pveUsr  = document.getElementById('pp-pve-username');
            const pveIns  = document.getElementById('pp-pve-insecure');
            if (pveUrl)  pveUrl.value   = orig.pp_pve_url;
            if (pveNode) pveNode.value  = orig.pp_pve_node;
            if (pveVmid) pveVmid.value  = orig.pp_pve_vmid;
            if (pveUsr)  pveUsr.value   = orig.pp_pve_username;
            if (pveIns)  pveIns.checked = orig.pp_pve_insecure;
        } else if (tabKey === 'config') {
            const keys = document.getElementById('cfg-ssh-keys');
            const krnl = document.getElementById('cfg-kernel-args');
            if (keys) keys.value = orig.ssh_keys;
            if (krnl) krnl.value = orig.kernel_args;
            // Re-render the custom vars list.
            const list = document.getElementById('cfg-vars-list');
            if (list) {
                if (Object.keys(orig.custom_vars).length === 0) {
                    list.innerHTML = `<div id="cfg-vars-empty" style="text-align:center;padding:12px;color:var(--text-dim);font-size:12px">No custom variables</div>`;
                } else {
                    list.innerHTML = Object.entries(orig.custom_vars).map(([k, v], i) => Pages._cfgVarRowHTML(i, k, v)).join('');
                }
            }
        } else if (tabKey === 'network') {
            const list = document.getElementById('net-interfaces-list');
            if (list) {
                if (orig.interfaces.length === 0) {
                    list.innerHTML = `<div id="net-empty" style="text-align:center;padding:16px;color:var(--text-dim);font-size:12px">No interfaces configured</div>`;
                } else {
                    list.innerHTML = orig.interfaces.map((iface, i) => Pages._netInterfaceRowHTML(i, iface, [])).join('');
                }
            }
        } else if (tabKey === 'mounts') {
            const tbody = document.getElementById('mounts-node-tbody');
            if (tbody) {
                tbody.innerHTML = orig.extra_mounts.map((m, i) => Pages._mountsNodeRowHTML(i, m)).join('');
                Pages._mountsUpdateEmpty();
            }
        }

        Pages._tabMarkClean(tabKey);
    },

    // _tabSaveByKey is a dispatch helper used by the unsaved-changes dialog.
    // Returns true on success, false on failure.
    async _tabSaveByKey(tabKey, nodeId) {
        try {
            if (tabKey === 'overview') await Pages._tabSaveOverview(nodeId);
            else if (tabKey === 'bmc')    await Pages._tabSavePower(nodeId);
            else if (tabKey === 'config') await Pages._tabSaveConfig(nodeId);
            else if (tabKey === 'network') await Pages._tabSaveNetwork(nodeId);
            else if (tabKey === 'mounts') await Pages._tabSaveMounts(nodeId);
            return true;
        } catch (_) {
            return false;
        }
    },

    // ── Per-tab save handlers ──────────────────────────────────────────────────

    async _tabSaveOverview(nodeId) {
        Pages._tabMarkSaving('overview');
        const saveBtn = document.getElementById('tab-save-overview');

        const hostname    = (document.getElementById('ov-hostname')?.value || '').trim();
        const fqdn        = (document.getElementById('ov-fqdn')?.value || '').trim();
        const baseImageId = document.getElementById('ov-base-image')?.value || '';
        const groupId     = document.getElementById('ov-group-id')?.value || '';
        const groupsRaw   = (document.getElementById('ov-groups')?.value || '');

        // Validate hostname.
        if (hostname && !/^[a-zA-Z0-9][a-zA-Z0-9.-]*$/.test(hostname)) {
            Pages._tabMarkError('overview', 'Invalid hostname format');
            if (saveBtn) saveBtn.disabled = false;
            return;
        }

        const groups = groupsRaw.split(',').map(g => g.trim()).filter(Boolean);

        try {
            // Fetch current node to get all fields we're not changing (the API requires full body).
            const existing = await API.nodes.get(nodeId);
            const body = {
                hostname:        hostname || existing.hostname,
                fqdn,
                primary_mac:     existing.primary_mac,
                base_image_id:   baseImageId,
                group_id:        groupId,
                groups,
                ssh_keys:        existing.ssh_keys || [],
                kernel_args:     existing.kernel_args || '',
                custom_vars:     existing.custom_vars || {},
                interfaces:      existing.interfaces || [],
                power_provider:  existing.power_provider || null,
                extra_mounts:    existing.extra_mounts || [],
                disk_layout_override: existing.disk_layout_override || null,
            };
            await API.nodes.update(nodeId, body);

            // Update original state so subsequent reverts work correctly.
            Pages._nodeEditorState['overview'].original = {
                hostname, fqdn, base_image_id: baseImageId, group_id: groupId, groups: groupsRaw,
            };
            Pages._tabMarkClean('overview');

            // Update the page title if hostname changed.
            const titleEl = document.querySelector('.page-title');
            if (titleEl && hostname) titleEl.textContent = hostname;
        } catch (e) {
            Pages._tabMarkError('overview', `Save failed: ${e.message}`);
            if (saveBtn) saveBtn.disabled = false;
        }
    },

    async _tabSavePower(nodeId) {
        Pages._tabMarkSaving('bmc');
        const saveBtn = document.getElementById('tab-save-bmc');

        const ppType = document.getElementById('pp-type')?.value || '';
        let powerProvider = null;

        if (ppType === 'ipmi') {
            const fields = {
                ip:       (document.getElementById('pp-ipmi-ip')?.value || '').trim(),
                username: (document.getElementById('pp-ipmi-username')?.value || '').trim(),
                channel:  (document.getElementById('pp-ipmi-channel')?.value || '1').trim(),
            };
            const pw = document.getElementById('pp-ipmi-password')?.value || '';
            if (pw) fields.password = pw;
            powerProvider = { type: 'ipmi', fields };
        } else if (ppType === 'proxmox') {
            const insecureEl = document.getElementById('pp-pve-insecure');
            const fields = {
                api_url:  (document.getElementById('pp-pve-url')?.value || '').trim(),
                node:     (document.getElementById('pp-pve-node')?.value || '').trim(),
                vmid:     (document.getElementById('pp-pve-vmid')?.value || '').trim(),
                username: (document.getElementById('pp-pve-username')?.value || '').trim(),
                insecure: (insecureEl && insecureEl.checked) ? 'true' : 'false',
            };
            const pw = document.getElementById('pp-pve-password')?.value || '';
            if (pw) fields.password = pw;
            powerProvider = { type: 'proxmox', fields };
        }

        try {
            const existing = await API.nodes.get(nodeId);
            const body = {
                hostname:        existing.hostname,
                primary_mac:     existing.primary_mac,
                fqdn:            existing.fqdn || '',
                base_image_id:   existing.base_image_id || '',
                group_id:        existing.group_id || '',
                groups:          existing.groups || [],
                ssh_keys:        existing.ssh_keys || [],
                kernel_args:     existing.kernel_args || '',
                custom_vars:     existing.custom_vars || {},
                interfaces:      existing.interfaces || [],
                power_provider:  powerProvider,
                extra_mounts:    existing.extra_mounts || [],
                disk_layout_override: existing.disk_layout_override || null,
            };
            await API.nodes.update(nodeId, body);

            // Clear the password inputs so they don't re-submit old values.
            const pwEl1 = document.getElementById('pp-ipmi-password');
            const pwEl2 = document.getElementById('pp-pve-password');
            if (pwEl1) pwEl1.value = '';
            if (pwEl2) pwEl2.value = '';

            Pages._nodeEditorState['bmc'].original.pp_type = ppType;
            Pages._tabMarkClean('bmc');

            // Re-fetch power status after provider change.
            if (powerProvider) setTimeout(() => Pages._refreshPowerStatus(nodeId), 500);
        } catch (e) {
            Pages._tabMarkError('bmc', `Save failed: ${e.message}`);
            if (saveBtn) saveBtn.disabled = false;
        }
    },

    async _tabSaveConfig(nodeId) {
        Pages._tabMarkSaving('config');
        const saveBtn = document.getElementById('tab-save-config');

        const sshKeysRaw  = document.getElementById('cfg-ssh-keys')?.value || '';
        const kernelArgs  = (document.getElementById('cfg-kernel-args')?.value || '').trim();
        const keysErrEl   = document.getElementById('cfg-ssh-keys-error');
        const krnlErrEl   = document.getElementById('cfg-kernel-args-error');

        if (keysErrEl) keysErrEl.style.display = 'none';
        if (krnlErrEl) krnlErrEl.style.display = 'none';

        // Validate SSH keys.
        const sshKeys = sshKeysRaw.split('\n').map(k => k.trim()).filter(Boolean);
        const invalidKeys = sshKeys.filter(k => !/^(ssh-rsa|ssh-ed25519|ecdsa-sha2-)/.test(k));
        if (invalidKeys.length) {
            if (keysErrEl) { keysErrEl.textContent = `Invalid key format: ${invalidKeys[0].substring(0, 40)}…`; keysErrEl.style.display = ''; }
            Pages._tabMarkError('config', 'Validation error');
            if (saveBtn) saveBtn.disabled = false;
            return;
        }

        // Validate kernel args — no shell metacharacters.
        if (/[;|`$()]/.test(kernelArgs)) {
            if (krnlErrEl) { krnlErrEl.textContent = 'Kernel args must not contain shell metacharacters (; | ` $ ())'; krnlErrEl.style.display = ''; }
            Pages._tabMarkError('config', 'Validation error');
            if (saveBtn) saveBtn.disabled = false;
            return;
        }

        // Collect custom vars from the editor rows.
        const customVars = {};
        document.querySelectorAll('#cfg-vars-list .cfg-var-row').forEach(row => {
            const k = (row.querySelector('.cfg-var-key')?.value || '').trim();
            const v = (row.querySelector('.cfg-var-val')?.value || '').trim();
            if (k) customVars[k] = v;
        });

        try {
            const existing = await API.nodes.get(nodeId);
            const body = {
                hostname:        existing.hostname,
                primary_mac:     existing.primary_mac,
                fqdn:            existing.fqdn || '',
                base_image_id:   existing.base_image_id || '',
                group_id:        existing.group_id || '',
                groups:          existing.groups || [],
                ssh_keys:        sshKeys,
                kernel_args:     kernelArgs,
                custom_vars:     customVars,
                interfaces:      existing.interfaces || [],
                power_provider:  existing.power_provider || null,
                extra_mounts:    existing.extra_mounts || [],
                disk_layout_override: existing.disk_layout_override || null,
            };
            await API.nodes.update(nodeId, body);

            Pages._nodeEditorState['config'].original = {
                ssh_keys:    sshKeys.join('\n'),
                kernel_args: kernelArgs,
                custom_vars: Object.assign({}, customVars),
            };
            Pages._tabMarkClean('config');
        } catch (e) {
            Pages._tabMarkError('config', `Save failed: ${e.message}`);
            if (saveBtn) saveBtn.disabled = false;
        }
    },

    async _tabSaveNetwork(nodeId) {
        Pages._tabMarkSaving('network');
        const saveBtn = document.getElementById('tab-save-network');

        // Collect interface rows.
        const interfaces = [];
        document.querySelectorAll('#net-interfaces-list .net-iface-row').forEach(row => {
            const mac  = row.querySelector('.net-iface-mac')?.value.trim() || '';
            const name = row.querySelector('.net-iface-name')?.value.trim() || '';
            const ip   = row.querySelector('.net-iface-ip')?.value.trim() || '';
            const gw   = row.querySelector('.net-iface-gw')?.value.trim() || '';
            const dns  = (row.querySelector('.net-iface-dns')?.value || '').split(',').map(s => s.trim()).filter(Boolean);
            const mtu  = parseInt(row.querySelector('.net-iface-mtu')?.value || '0', 10) || 0;
            const bond = row.querySelector('.net-iface-bond')?.value.trim() || '';
            if (mac || name || ip) {
                interfaces.push({ mac_address: mac, name, ip_address: ip, gateway: gw, dns, mtu: mtu || undefined, bond: bond || undefined });
            }
        });

        try {
            const existing = await API.nodes.get(nodeId);
            const body = {
                hostname:        existing.hostname,
                primary_mac:     existing.primary_mac,
                fqdn:            existing.fqdn || '',
                base_image_id:   existing.base_image_id || '',
                group_id:        existing.group_id || '',
                groups:          existing.groups || [],
                ssh_keys:        existing.ssh_keys || [],
                kernel_args:     existing.kernel_args || '',
                custom_vars:     existing.custom_vars || {},
                interfaces,
                power_provider:  existing.power_provider || null,
                extra_mounts:    existing.extra_mounts || [],
                disk_layout_override: existing.disk_layout_override || null,
            };
            await API.nodes.update(nodeId, body);

            Pages._nodeEditorState['network'].original = {
                interfaces: JSON.parse(JSON.stringify(interfaces)),
            };
            Pages._tabMarkClean('network');
        } catch (e) {
            Pages._tabMarkError('network', `Save failed: ${e.message}`);
            if (saveBtn) saveBtn.disabled = false;
        }
    },

    // ── Network tab helpers ───────────────────────────────────────────────────

    _netInterfaceRowHTML(idx, iface, discoveredMACs) {
        iface = iface || {};
        const macOptions = (discoveredMACs || [])
            .map(m => `<option value="${escHtml(m)}" ${iface.mac_address === m ? 'selected' : ''}>${escHtml(m)}</option>`)
            .join('');
        return `<div class="net-iface-row" data-idx="${idx}" style="border:1px solid var(--border);border-radius:6px;padding:12px;margin-bottom:10px">
            <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:10px">
                <span style="font-size:12px;font-weight:600;color:var(--text-secondary)">Interface ${idx + 1}</span>
                <button type="button" class="btn btn-danger btn-sm" style="padding:2px 8px;font-size:11px"
                    onclick="Pages._netRemoveInterface(this);Pages._tabMarkDirty('network')">Remove</button>
            </div>
            <div class="form-grid" style="margin-bottom:0">
                <div class="form-group">
                    <label>MAC Address</label>
                    ${discoveredMACs && discoveredMACs.length
                        ? `<select class="net-iface-mac" onchange="Pages._tabMarkDirty('network')">
                                <option value="">Custom / none</option>
                                ${macOptions}
                            </select>`
                        : `<input type="text" class="net-iface-mac" value="${escHtml(iface.mac_address || '')}"
                                placeholder="aa:bb:cc:dd:ee:ff" oninput="Pages._tabMarkDirty('network')">`
                    }
                </div>
                <div class="form-group">
                    <label>Interface Name</label>
                    <input type="text" class="net-iface-name" value="${escHtml(iface.name || '')}"
                        placeholder="eth0" oninput="Pages._tabMarkDirty('network')">
                </div>
                <div class="form-group">
                    <label>IP Address (CIDR)</label>
                    <input type="text" class="net-iface-ip" value="${escHtml(iface.ip_address || '')}"
                        placeholder="192.168.1.50/24" oninput="Pages._tabMarkDirty('network')">
                </div>
                <div class="form-group">
                    <label>Gateway</label>
                    <input type="text" class="net-iface-gw" value="${escHtml(iface.gateway || '')}"
                        placeholder="192.168.1.1" oninput="Pages._tabMarkDirty('network')">
                </div>
                <div class="form-group">
                    <label>DNS <span style="font-size:11px;color:var(--text-secondary)">(comma-separated)</span></label>
                    <input type="text" class="net-iface-dns" value="${escHtml((iface.dns || []).join(', '))}"
                        placeholder="8.8.8.8, 8.8.4.4" oninput="Pages._tabMarkDirty('network')">
                </div>
                <div class="form-group">
                    <label>MTU</label>
                    <input type="number" class="net-iface-mtu" value="${iface.mtu || ''}"
                        placeholder="1500" oninput="Pages._tabMarkDirty('network')">
                </div>
                <div class="form-group">
                    <label>Bond</label>
                    <input type="text" class="net-iface-bond" value="${escHtml(iface.bond || '')}"
                        placeholder="bond0" oninput="Pages._tabMarkDirty('network')">
                </div>
            </div>
        </div>`;
    },

    _netAddInterface(discoveredMACs) {
        const list = document.getElementById('net-interfaces-list');
        if (!list) return;
        const emptyEl = document.getElementById('net-empty');
        if (emptyEl) emptyEl.remove();

        const macs = Array.isArray(discoveredMACs) ? discoveredMACs : [];
        const idx = list.querySelectorAll('.net-iface-row').length;
        list.insertAdjacentHTML('beforeend', Pages._netInterfaceRowHTML(idx, {}, macs));
        Pages._tabMarkDirty('network');
    },

    _netRemoveInterface(btn) {
        const row = btn.closest('.net-iface-row');
        if (row) row.remove();
        const list = document.getElementById('net-interfaces-list');
        if (list && list.querySelectorAll('.net-iface-row').length === 0) {
            list.innerHTML = `<div id="net-empty" style="text-align:center;padding:16px;color:var(--text-dim);font-size:12px">No interfaces configured</div>`;
        }
    },

    // ── Configuration tab helpers ─────────────────────────────────────────────

    _cfgVarRowHTML(idx, key, value) {
        return `<div class="cfg-var-row" data-idx="${idx}" style="display:flex;gap:8px;align-items:center;margin-bottom:6px">
            <input type="text" class="cfg-var-key" value="${escHtml(key)}"
                placeholder="variable_name" style="flex:1" oninput="Pages._tabMarkDirty('config')">
            <input type="text" class="cfg-var-val" value="${escHtml(value)}"
                placeholder="value" style="flex:2" oninput="Pages._tabMarkDirty('config')">
            <button type="button" class="btn btn-danger btn-sm" style="padding:2px 8px;font-size:11px;flex-shrink:0"
                onclick="Pages._cfgRemoveVar(this);Pages._tabMarkDirty('config')">✕</button>
        </div>`;
    },

    _cfgAddVar() {
        const list = document.getElementById('cfg-vars-list');
        if (!list) return;
        const emptyEl = document.getElementById('cfg-vars-empty');
        if (emptyEl) emptyEl.remove();
        const idx = list.querySelectorAll('.cfg-var-row').length;
        list.insertAdjacentHTML('beforeend', Pages._cfgVarRowHTML(idx, '', ''));
        Pages._tabMarkDirty('config');
    },

    _cfgRemoveVar(btn) {
        const row = btn.closest('.cfg-var-row');
        if (row) row.remove();
        const list = document.getElementById('cfg-vars-list');
        if (list && list.querySelectorAll('.cfg-var-row').length === 0) {
            list.innerHTML = `<div id="cfg-vars-empty" style="text-align:center;padding:12px;color:var(--text-dim);font-size:12px">No custom variables</div>`;
        }
    },

    // ── Power Provider inline type change ─────────────────────────────────────

    _onPowerProviderInlineTypeChange(type) {
        const ipmiFields   = document.getElementById('pp-inline-ipmi-fields');
        const proxmoxFields = document.getElementById('pp-inline-proxmox-fields');
        if (ipmiFields)    ipmiFields.style.display    = (type === 'ipmi')    ? '' : 'none';
        if (proxmoxFields) proxmoxFields.style.display = (type === 'proxmox') ? '' : 'none';
    },

    // _checkRoleMismatchInline is the inline-editing version of _checkRoleMismatch.
    // Uses the #ov-role-mismatch-warning element in the overview tab form.
    _checkRoleMismatchInline(imageId, node, images) {
        const warnEl = document.getElementById('ov-role-mismatch-warning');
        if (!warnEl) return;
        if (!imageId) { warnEl.style.display = 'none'; return; }
        const img = (images || []).find(i => i.id === imageId);
        if (!img) { warnEl.style.display = 'none'; return; }
        const builtFor = img.built_for_roles || [];
        if (!builtFor.length) { warnEl.style.display = 'none'; return; }
        const roleKeywords = ['compute', 'gpu-compute', 'gpu', 'storage', 'head-node', 'management', 'minimal'];
        const nodeRoles = (node.groups || []).filter(g => roleKeywords.some(k => g.toLowerCase().includes(k)));
        const mismatched = nodeRoles.filter(g => !builtFor.some(r => g.toLowerCase().includes(r) || r.toLowerCase().includes(g)));
        if (mismatched.length) {
            warnEl.innerHTML = `Role mismatch: node has <strong>${escHtml(mismatched.join(', '))}</strong> but image built for <strong>${escHtml(builtFor.join(', '))}</strong>`;
            warnEl.style.display = '';
        } else {
            warnEl.style.display = 'none';
        }
    },

    // ── Node Actions dropdown ─────────────────────────────────────────────────

    async _nodeActionsRediscover(nodeId) {
        if (!confirm('Mark node for hardware re-discovery?\n\nThe node will need to PXE boot to re-register its hardware profile.\nThis does NOT wipe the disk.')) return;
        try {
            // Trigger a reimage so the node PXE-boots and re-registers its hardware profile.
            await API.request('POST', `/nodes/${nodeId}/reimage`, {});
            alert('Reimage requested. PXE-boot the node to re-discover hardware. After registration, cancel or skip deployment if you only want hardware discovery.');
        } catch (e) {
            alert(`Re-discover failed: ${e.message}`);
        }
    },

    async _nodeActionsTriggerReimage(nodeId, displayName) {
        if (!confirm(`Trigger reimage of "${displayName}"?\n\nThe node will re-deploy on next PXE boot.`)) return;
        try {
            await API.request('POST', `/nodes/${nodeId}/reimage`, {});
            alert('Reimage requested. The node will re-deploy on next PXE boot.');
            // Reload the page to show updated reimage_pending state.
            Pages.nodeDetail(nodeId);
        } catch (e) {
            alert(`Trigger reimage failed: ${e.message}`);
        }
    },

    async loadNodeLogs(mac) {
        const viewer = document.getElementById('node-log-viewer');
        if (!viewer) return;
        try {
            const resp = await API.logs.query({ mac, limit: '300' });
            const entries = resp.logs || [];
            if (!App._nodeLogStream) {
                App._nodeLogStream = new LogStream(viewer);
            }
            App._nodeLogStream.loadEntries(entries);
            if (!entries.length) {
                viewer.innerHTML = `<div class="empty-state" style="padding:30px">
                    <div class="empty-state-text">No log entries for this node</div>
                </div>`;
            }
        } catch (e) {
            viewer.innerHTML = `<div style="padding:12px;color:var(--error);font-size:12px">Error: ${escHtml(e.message)}</div>`;
        }
    },

    toggleNodeLogs(enabled, mac) {
        const viewer = document.getElementById('node-log-viewer');
        const ind    = document.getElementById('node-follow-ind');
        if (!viewer) return;

        if (enabled) {
            if (!App._nodeLogStream) App._nodeLogStream = new LogStream(viewer);
            App._nodeLogStream.setFilters({ mac });
            App._nodeLogStream.setAutoScroll(true);
            App._nodeLogStream.onConnect(() => {
                if (ind) { ind.className = 'follow-indicator live'; ind.innerHTML = '<span class="follow-dot"></span>Live'; }
            });
            App._nodeLogStream.onDisconnect((permanent) => {
                if (ind) {
                    ind.className = 'follow-indicator';
                    ind.innerHTML = permanent
                        ? '<span class="follow-dot"></span>unavailable'
                        : '<span class="follow-dot"></span>Reconnecting…';
                }
            });
            App._nodeLogStream.connect();
        } else {
            if (App._nodeLogStream) {
                App._nodeLogStream.disconnect();
                if (ind) { ind.className = 'follow-indicator'; ind.innerHTML = '<span class="follow-dot"></span>static'; }
            }
        }
    },

    // ── Power management ──────────────────────────────────────────────────────

    // _doFlipToDisk calls POST /nodes/:id/power/flip-to-disk via the provider.
    // When cycle=true the server also power-cycles the node after flipping.
    async _doFlipToDisk(nodeId, cycle) {
        const feedback = document.getElementById('power-action-feedback');
        const label = cycle ? 'Flip to Disk + Reboot' : 'Flip to Disk';
        if (feedback) { feedback.textContent = `${label}…`; feedback.style.display = ''; feedback.className = 'alert alert-info'; }
        try {
            await API.nodes.power.flipToDisk(nodeId, !!cycle);
            if (feedback) { feedback.textContent = `${label} command sent.`; feedback.className = 'alert alert-info'; }
            if (!cycle) setTimeout(() => Pages._refreshPowerStatus(nodeId), 2000);
        } catch (e) {
            if (feedback) { feedback.textContent = `Error: ${e.message}`; feedback.className = 'alert alert-error'; }
        }
    },

    // ── Disk Layout tab ───────────────────────────────────────────────────────

    // _onDiskLayoutTabOpen loads the effective layout and recommendation for the
    // disk layout editor tab. Called once when the tab is first opened.
    async _onDiskLayoutTabOpen(nodeId) {
        const container = document.getElementById('disklayout-content');
        if (!container) return;
        container.innerHTML = `<div class="loading"><div class="spinner"></div>Loading…</div>`;
        try {
            const [effectiveResp, recResp] = await Promise.allSettled([
                API.request('GET', `/nodes/${nodeId}/effective-layout`),
                API.request('GET', `/nodes/${nodeId}/layout-recommendation`),
            ]);
            const effective = effectiveResp.status === 'fulfilled' ? effectiveResp.value : null;
            const rec = recResp.status === 'fulfilled' ? recResp.value : null;
            container.innerHTML = Pages._renderDiskLayoutTab(nodeId, effective, rec);
        } catch (e) {
            container.innerHTML = alertBox(`Failed to load disk layout: ${e.message}`);
        }
    },

    // _onMountsTabOpen fetches both effective-mounts and node data, then renders the two-section editor.
    async _onMountsTabOpen(nodeId) {
        const container = document.getElementById('mounts-content');
        if (!container) return;
        // Don't reload if already populated and not dirty.
        const state = Pages._nodeEditorState['mounts'];
        if (container.dataset.loaded === nodeId && !(state && state.dirty)) return;
        container.innerHTML = `<div class="loading"><div class="spinner"></div>Loading mounts…</div>`;
        try {
            const [effectiveResp, node] = await Promise.all([
                API.request('GET', `/nodes/${nodeId}/effective-mounts`),
                API.nodes.get(nodeId),
            ]);
            // Sync editor state with fresh node data (in case another tab saved something).
            if (state && !state.dirty) {
                state.original = { extra_mounts: JSON.parse(JSON.stringify(node.extra_mounts || [])) };
            }
            container.innerHTML = Pages._renderEffectiveMountsTab(nodeId, effectiveResp, node);
            container.dataset.loaded = nodeId;
            // Wire up live validation on the editable tbody.
            const tbody = document.getElementById('mounts-node-tbody');
            if (tbody) {
                tbody.addEventListener('input', () => Pages._tabMarkDirty('mounts'));
                tbody.addEventListener('change', (e) => {
                    if (e.target && e.target.name === 'mount_fs_type') {
                        Pages._onFSTypeChange(e.target);
                    }
                    Pages._tabMarkDirty('mounts');
                });
            }
        } catch (e) {
            container.innerHTML = alertBox(`Failed to load mounts: ${e.message}`);
        }
    },

    // _renderEffectiveMountsTab renders a two-section layout:
    //   Section 1 — inherited group mounts (read-only)
    //   Section 2 — node-level mounts (inline editable)
    _renderEffectiveMountsTab(nodeId, resp, node) {
        const allMounts  = (resp && resp.mounts) || [];
        const groupId    = (resp && resp.group_id) || '';
        const groupMounts = allMounts.filter(m => m.source === 'group');
        const nodeMounts  = (node && node.extra_mounts) || [];

        // ── Section 1: Inherited from group ──────────────────────────────────
        const groupSection = (() => {
            if (groupMounts.length === 0) {
                const noGroupMsg = groupId
                    ? `<div class="text-dim" style="padding:12px;font-size:13px">No mounts defined on the assigned group.</div>`
                    : `<div class="text-dim" style="padding:12px;font-size:13px">Node is not assigned to a group.</div>`;
                return cardWrap('Inherited from Group',
                    `<div class="card-body">${noGroupMsg}</div>`);
            }
            const rows = groupMounts.map(m => `<tr>
                <td class="mono">${escHtml(m.source_device||m.source||'—')}</td>
                <td class="mono">${escHtml(m.mount_point||'—')}</td>
                <td><span class="badge badge-neutral badge-sm">${escHtml(m.fs_type||'—')}</span></td>
                <td class="mono dim" style="font-size:11px">${escHtml(m.options||'defaults')}</td>
                <td style="text-align:center">${m.auto_mkdir ? '✓' : '—'}</td>
                <td class="dim" style="font-size:11px">${escHtml(m.comment||'—')}</td>
            </tr>`).join('');
            return cardWrap('Inherited from Group',
                `<div class="card-body">
                    <p style="margin:0 0 10px;color:var(--text-secondary);font-size:12px">
                        These mounts come from the node's group and cannot be edited here.
                        Node-level entries with the same mount point will override the group entry.
                    </p>
                    <div class="table-wrap"><table>
                        <thead><tr>
                            <th>Source</th><th>Mount Point</th><th>FS Type</th>
                            <th>Options</th><th>Auto-mkdir</th><th>Comment</th>
                        </tr></thead>
                        <tbody>${rows}</tbody>
                    </table></div>
                </div>`);
        })();

        // ── Section 2: Node-level mounts (editable) ──────────────────────────
        const nodeRows = nodeMounts.map((m, i) => Pages._mountsNodeRowHTML(i, m)).join('');
        const emptyRow = nodeMounts.length === 0
            ? `<div id="mounts-node-empty" style="text-align:center;padding:16px;color:var(--text-dim);font-size:12px">No node-level mounts configured</div>`
            : '';

        const nodeSection = cardWrap('Node-Level Mounts',
            `<div class="card-body">
                <p style="margin:0 0 10px;color:var(--text-secondary);font-size:12px">
                    Added to <code>/etc/fstab</code> during deployment.
                    These entries override group entries when the mount point matches.
                </p>
                <div style="display:flex;align-items:center;gap:8px;margin-bottom:10px">
                    <select id="mounts-preset-select" class="form-select" style="font-size:12px;padding:4px 8px;width:auto">
                        <option value="">— Apply preset —</option>
                        <option value="nfs-home">NFS home</option>
                        <option value="lustre">Lustre scratch</option>
                        <option value="beegfs">BeeGFS data</option>
                        <option value="cifs">CIFS / Samba</option>
                        <option value="bind">Bind mount</option>
                        <option value="tmpfs">tmpfs</option>
                    </select>
                    <button type="button" class="btn btn-secondary btn-sm"
                        onclick="Pages._mountsApplyPreset()">Apply</button>
                    <button type="button" class="btn btn-secondary btn-sm"
                        onclick="Pages._mountsAddRow()">+ Add Mount</button>
                </div>
                <div id="mounts-node-table-wrap">
                    <div class="table-wrap" style="overflow-x:auto${nodeMounts.length === 0 ? ';display:none' : ''}">
                        <table id="mounts-node-table" style="width:100%;font-size:12px;border-collapse:collapse">
                            <thead><tr style="border-bottom:1px solid var(--border)">
                                <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Source</th>
                                <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Mount Point</th>
                                <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">FS Type</th>
                                <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Options</th>
                                <th style="text-align:center;padding:4px 6px;font-weight:500;color:var(--text-secondary)" title="Auto-create mount point directory">mkd</th>
                                <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Comment</th>
                                <th style="padding:4px"></th>
                            </tr></thead>
                            <tbody id="mounts-node-tbody">${nodeRows}</tbody>
                        </table>
                    </div>
                    ${emptyRow}
                </div>
            </div>`);

        return `${groupSection}${nodeSection}`;
    },

    // _mountsNodeRowHTML builds one editable row for a node-level mount entry.
    _mountsNodeRowHTML(idx, m) {
        m = m || {};
        const fsTypes = ['nfs','nfs4','cifs','beegfs','lustre','gpfs','xfs','ext4','bind','9p','tmpfs','vfat','ext3','smbfs'];
        const fsSelect = fsTypes.map(t =>
            `<option value="${t}"${m.fs_type === t ? ' selected' : ''}>${t}</option>`
        ).join('');
        return `<tr data-mount-idx="${idx}" style="border-bottom:1px solid var(--border)">
            <td style="padding:4px 3px">
                <input type="text" name="mount_source" value="${escHtml(m.source||'')}"
                    placeholder="nfs-server:/export/home" style="width:100%;min-width:130px;font-size:12px" required>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="mount_point" value="${escHtml(m.mount_point||'')}"
                    placeholder="/mnt/share" style="width:100%;min-width:110px;font-size:12px" required pattern="/.+">
            </td>
            <td style="padding:4px 3px">
                <select name="mount_fs_type" style="font-size:12px;padding:2px 4px">${fsSelect}</select>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="mount_options" value="${escHtml(m.options||'')}"
                    placeholder="defaults,_netdev" style="width:100%;min-width:130px;font-size:12px">
            </td>
            <td style="padding:4px 3px;text-align:center">
                <input type="checkbox" name="mount_auto_mkdir" ${m.auto_mkdir !== false ? 'checked' : ''}>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="mount_comment" value="${escHtml(m.comment||'')}"
                    placeholder="optional note" style="width:100%;min-width:80px;font-size:12px">
            </td>
            <td style="padding:4px 3px">
                <button type="button" class="btn btn-danger btn-sm"
                    onclick="Pages._mountsRemoveRow(this)" style="padding:2px 6px;font-size:11px">✕</button>
            </td>
        </tr>`;
    },

    // _mountsAddRow appends a blank (or preset) row to the node mounts table.
    _mountsAddRow(preset) {
        const tbody = document.getElementById('mounts-node-tbody');
        if (!tbody) return;
        const idx = tbody.querySelectorAll('tr').length;
        tbody.insertAdjacentHTML('beforeend', Pages._mountsNodeRowHTML(idx, preset || {}));
        Pages._mountsShowTable();
        Pages._tabMarkDirty('mounts');
    },

    // _mountsRemoveRow removes the row containing the given button.
    _mountsRemoveRow(btn) {
        const row = btn.closest('tr');
        if (row) row.remove();
        Pages._mountsUpdateEmpty();
        Pages._tabMarkDirty('mounts');
    },

    // _mountsApplyPreset reads the preset dropdown and inserts the preset row.
    _mountsApplyPreset() {
        const sel = document.getElementById('mounts-preset-select');
        if (!sel || !sel.value) return;
        const presets = {
            'nfs-home': { source: 'nfs-server:/export/home', mount_point: '/home/shared',  fs_type: 'nfs4',   options: 'defaults,_netdev,vers=4',              auto_mkdir: true,  comment: 'NFS home directory' },
            'lustre':   { source: 'mgs@tcp:/scratch',        mount_point: '/scratch',       fs_type: 'lustre', options: 'defaults,_netdev,flock',               auto_mkdir: true,  comment: 'Lustre scratch' },
            'beegfs':   { source: 'beegfs',                  mount_point: '/mnt/beegfs',    fs_type: 'beegfs', options: 'defaults,_netdev',                     auto_mkdir: true,  comment: 'BeeGFS data' },
            'cifs':     { source: '//winserver/share',        mount_point: '/mnt/share',     fs_type: 'cifs',   options: 'defaults,_netdev,vers=3.0,sec=ntlmssp',auto_mkdir: true,  comment: 'CIFS (Windows) share' },
            'bind':     { source: '/data/src',               mount_point: '/data/dst',      fs_type: 'bind',   options: 'defaults,bind',                        auto_mkdir: true,  comment: 'Bind mount' },
            'tmpfs':    { source: 'tmpfs',                   mount_point: '/tmp',           fs_type: 'tmpfs',  options: 'defaults,size=4G,mode=1777',           auto_mkdir: false, comment: 'tmpfs /tmp' },
        };
        const p = presets[sel.value];
        if (p) Pages._mountsAddRow(p);
        sel.value = '';
    },

    // _mountsShowTable reveals the table wrapper (used after first row is added).
    _mountsShowTable() {
        const wrap = document.getElementById('mounts-node-table-wrap');
        if (!wrap) return;
        const tableWrap = wrap.querySelector('.table-wrap');
        if (tableWrap) tableWrap.style.display = '';
        const empty = document.getElementById('mounts-node-empty');
        if (empty) empty.remove();
    },

    // _mountsUpdateEmpty shows the empty-state message when the tbody is empty,
    // and hides the table wrapper.
    _mountsUpdateEmpty() {
        const tbody = document.getElementById('mounts-node-tbody');
        const wrap  = document.getElementById('mounts-node-table-wrap');
        if (!tbody || !wrap) return;
        const hasRows = tbody.querySelectorAll('tr').length > 0;
        const tableWrap = wrap.querySelector('.table-wrap');
        if (tableWrap) tableWrap.style.display = hasRows ? '' : 'none';
        const existing = document.getElementById('mounts-node-empty');
        if (!hasRows && !existing) {
            wrap.insertAdjacentHTML('beforeend',
                `<div id="mounts-node-empty" style="text-align:center;padding:16px;color:var(--text-dim);font-size:12px">No node-level mounts configured</div>`);
        } else if (hasRows && existing) {
            existing.remove();
        }
    },

    // _mountsCollect reads all editable rows and returns an array of FstabEntry objects.
    // Returns null (with inline validation errors shown) if any row is invalid.
    _mountsCollect() {
        const tbody = document.getElementById('mounts-node-tbody');
        if (!tbody) return [];
        const fsTypeWhitelist = new Set(['nfs','nfs4','cifs','beegfs','lustre','gpfs','xfs','ext4','bind','9p','tmpfs','vfat','ext3','smbfs']);
        const mounts = [];
        let valid = true;
        tbody.querySelectorAll('tr').forEach(row => {
            const srcEl  = row.querySelector('[name="mount_source"]');
            const mpEl   = row.querySelector('[name="mount_point"]');
            const fsEl   = row.querySelector('[name="mount_fs_type"]');
            const optEl  = row.querySelector('[name="mount_options"]');
            const mkdEl  = row.querySelector('[name="mount_auto_mkdir"]');
            const cmtEl  = row.querySelector('[name="mount_comment"]');
            const source     = (srcEl?.value || '').trim();
            const mountPoint = (mpEl?.value || '').trim();
            const fsType     = fsEl?.value || 'nfs';
            const options    = (optEl?.value || '').trim();
            const autoMkdir  = mkdEl ? mkdEl.checked : true;
            const comment    = (cmtEl?.value || '').trim();

            // Validate required fields.
            if (!source) { if (srcEl) srcEl.style.border = '1px solid var(--error)'; valid = false; }
            else if (srcEl) srcEl.style.border = '';
            if (!mountPoint || mountPoint[0] !== '/') { if (mpEl) mpEl.style.border = '1px solid var(--error)'; valid = false; }
            else if (mpEl) mpEl.style.border = '';
            if (!fsTypeWhitelist.has(fsType)) { valid = false; return; }

            mounts.push({ source, mount_point: mountPoint, fs_type: fsType, options, auto_mkdir: autoMkdir, comment, dump: 0, pass: 0 });
        });
        return valid ? mounts : null;
    },

    // _tabSaveMounts saves the node-level mounts via PUT /nodes/:id.
    async _tabSaveMounts(nodeId) {
        Pages._tabMarkSaving('mounts');
        const saveBtn = document.getElementById('tab-save-mounts');

        const mounts = Pages._mountsCollect();
        if (mounts === null) {
            Pages._tabMarkError('mounts', 'Fix validation errors before saving');
            if (saveBtn) saveBtn.disabled = false;
            return;
        }

        try {
            const existing = await API.nodes.get(nodeId);
            const body = Object.assign({}, existing, { extra_mounts: mounts });
            await API.nodes.update(nodeId, body);

            // Update editor state so revert has the new baseline.
            const state = Pages._nodeEditorState['mounts'];
            if (state) state.original = { extra_mounts: JSON.parse(JSON.stringify(mounts)) };

            Pages._tabMarkClean('mounts');
            App.toast('Mounts saved', 'success');

            // Force tab to re-render on next open so effective view is fresh.
            const container = document.getElementById('mounts-content');
            if (container) delete container.dataset.loaded;
        } catch (e) {
            Pages._tabMarkError('mounts', `Save failed: ${e.message}`);
            if (saveBtn) saveBtn.disabled = false;
        }
    },

    _renderDiskLayoutTab(nodeId, effective, rec) {
        const sourceLabel = {
            node:  '<span class="badge badge-info">Node Override</span>',
            group: '<span class="badge badge-neutral">Group Override</span>',
            image: '<span class="badge badge-archived">Image Default</span>',
        };

        const layoutToTable = (layout) => {
            if (!layout || !layout.partitions || layout.partitions.length === 0) {
                return `<div class="text-dim" style="padding:12px">No partitions defined.</div>`;
            }
            const totalFixed = layout.partitions.reduce((s, p) => s + (p.size_bytes || 0), 0);
            // Visual bar: compute each partition's width as a % of total fixed space (or uniform if fill).
            const hasFill = layout.partitions.some(p => !p.size_bytes);
            const barParts = layout.partitions.map(p => {
                const pct = (hasFill || totalFixed === 0)
                    ? (p.size_bytes ? Math.round(p.size_bytes / (totalFixed || 1) * 80) : 20)
                    : Math.max(2, Math.round(p.size_bytes / totalFixed * 100));
                const colors = {
                    xfs: '#3b82f6', ext4: '#8b5cf6', vfat: '#10b981', swap: '#f59e0b',
                    biosboot: '#6b7280', bios_grub: '#6b7280',
                };
                const bg = colors[p.filesystem] || '#94a3b8';
                return `<div style="flex:${pct};background:${bg};min-width:24px;display:flex;align-items:center;justify-content:center;font-size:10px;color:#fff;overflow:hidden;white-space:nowrap;padding:0 4px" title="${escHtml(p.label||p.mountpoint||p.filesystem)}">${escHtml(p.label||p.mountpoint||'')}</div>`;
            }).join('');
            const bar = `<div style="display:flex;height:32px;border-radius:6px;overflow:hidden;margin-bottom:12px;border:1px solid var(--border)">${barParts}</div>`;

            const rows = layout.partitions.map((p, i) => {
                const sizeStr = p.size_bytes
                    ? fmtBytes(p.size_bytes)
                    : '<span class="badge badge-neutral" style="font-size:10px">fill</span>';
                return `<tr>
                    <td>${escHtml(p.label || '—')}</td>
                    <td>${sizeStr}</td>
                    <td><span class="badge badge-neutral" style="font-size:10px">${escHtml(p.filesystem || '—')}</span></td>
                    <td class="text-mono">${escHtml(p.mountpoint || '—')}</td>
                    <td class="text-dim">${(p.flags||[]).join(', ') || '—'}</td>
                    <td class="text-dim">${escHtml(p.device||'(auto)')}</td>
                </tr>`;
            }).join('');

            const bootloader = layout.bootloader
                ? `<div style="margin-top:8px;font-size:12px;color:var(--text-secondary)">Bootloader: <strong>${escHtml(layout.bootloader.type||'')} (${escHtml(layout.bootloader.target||'')})</strong></div>`
                : '';
            if (layout.target_device) {
                // show target device hint
            }

            return `
                ${bar}
                <div class="table-wrap"><table>
                    <thead><tr><th>Label</th><th>Size</th><th>Filesystem</th><th>Mountpoint</th><th>Flags</th><th>Device</th></tr></thead>
                    <tbody>${rows}</tbody>
                </table></div>
                ${bootloader}`;
        };

        let effectiveSection = '';
        if (effective) {
            const src = sourceLabel[effective.source] || `<span class="badge badge-neutral">${escHtml(effective.source)}</span>`;
            effectiveSection = cardWrap(
                `Current Effective Layout &nbsp;${src}`,
                `<div class="card-body">
                    ${layoutToTable(effective.layout)}
                </div>`,
                `<div class="flex gap-8">
                    <button class="btn btn-secondary btn-sm" onclick='Pages._showLayoutOverrideEditor(${JSON.stringify(nodeId)}, ${JSON.stringify(JSON.stringify(effective.layout))})'>
                        Edit Override
                    </button>
                    ${effective.source !== 'image' ? `<button class="btn btn-secondary btn-sm" onclick='Pages._clearLayoutOverride(${JSON.stringify(nodeId)})'>Clear Override</button>` : ''}
                </div>`
            );
        }

        let recSection = '';
        if (rec) {
            const warnings = (rec.warnings || []).map(w =>
                `<div class="alert alert-warning" style="margin:4px 0;font-size:12px">${escHtml(w)}</div>`).join('');
            recSection = cardWrap(
                'Recommended Layout',
                `<div class="card-body">
                    ${layoutToTable(rec.layout)}
                    ${warnings}
                    ${rec.reasoning ? `<details style="margin-top:12px"><summary style="cursor:pointer;font-size:12px;color:var(--text-secondary)">Reasoning</summary><pre style="font-size:11px;margin-top:8px;white-space:pre-wrap;color:var(--text-secondary)">${escHtml(rec.reasoning)}</pre></details>` : ''}
                </div>`,
                `<button class="btn btn-primary btn-sm" onclick='Pages._applyRecommendedLayout(${JSON.stringify(nodeId)}, ${JSON.stringify(JSON.stringify(rec.layout))})'>Apply Recommended Layout</button>`
            );
        } else if (rec === null) {
            recSection = cardWrap('Recommended Layout',
                `<div class="card-body">${emptyState('No recommendation available', 'Hardware profile not yet discovered (node must PXE-boot to register hardware).')}</div>`);
        }

        return effectiveSection + recSection;
    },

    async _applyRecommendedLayout(nodeId, layoutJSON) {
        const layout = JSON.parse(layoutJSON);
        if (!confirm('Apply the recommended disk layout as a node-level override? This will override the image/group default for this node only.')) return;
        try {
            await API.request('PUT', `/nodes/${nodeId}/layout-override`, { layout });
            App.toast('Recommended layout applied as node override', 'success');
            Pages._onDiskLayoutTabOpen(nodeId);
        } catch (e) {
            App.toast(`Failed to apply layout: ${e.message}`, 'error');
        }
    },

    async _clearLayoutOverride(nodeId) {
        if (!confirm('Clear the node-level disk layout override? The group or image default will be used instead.')) return;
        try {
            await API.request('PUT', `/nodes/${nodeId}/layout-override`, { clear_layout_override: true });
            App.toast('Node layout override cleared — using group or image default', 'success');
            Pages._onDiskLayoutTabOpen(nodeId);
        } catch (e) {
            App.toast(`Failed to clear override: ${e.message}`, 'error');
        }
    },

    _showLayoutOverrideEditor(nodeId, layoutJSON) {
        const layout = JSON.parse(layoutJSON);
        // Build an editable partition table in a modal.
        const rows = (layout.partitions || []).map((p, i) => `
            <tr>
                <td><input type="text" value="${escHtml(p.label||'')}" onchange="Pages._layoutEditorUpdate(${i},'label',this.value)" style="width:90px"></td>
                <td>
                    <input type="text" value="${p.size_bytes ? fmtBytes(p.size_bytes) : 'fill'}" onchange="Pages._layoutEditorParseSizeInput(${i},this.value)" style="width:80px" placeholder="e.g. 100GB or fill">
                </td>
                <td>
                    <select onchange="Pages._layoutEditorUpdate(${i},'filesystem',this.value)">
                        ${[['xfs','xfs'],['ext4','ext4'],['vfat','vfat (ESP/FAT)'],['swap','swap'],['biosboot','biosboot'],['','none (raw/LVM PV)']].map(([val,lbl]) =>
                            `<option value="${val}" ${(p.filesystem||'')===val?'selected':''}>${lbl}</option>`).join('')}
                    </select>
                </td>
                <td><input type="text" value="${escHtml(p.mountpoint||'')}" onchange="Pages._layoutEditorUpdate(${i},'mountpoint',this.value)" style="width:90px"></td>
                <td>
                    <button class="btn btn-danger btn-sm" onclick="Pages._layoutEditorRemoveRow(${i})" style="padding:2px 8px">✕</button>
                </td>
            </tr>`).join('');

        const overlay = document.createElement('div');
        overlay.id = 'layout-editor-modal';
        overlay.className = 'modal-overlay';
        overlay.innerHTML = `
            <div class="modal" style="max-width:720px;width:95vw">
                <div class="modal-header"><h2>Edit Disk Layout Override</h2></div>
                <div class="modal-body" style="padding:20px">
                    <div id="layout-editor-warnings" style="margin-bottom:10px"></div>
                    <div class="table-wrap">
                        <table id="layout-editor-table">
                            <thead><tr><th>Label</th><th>Size</th><th>Filesystem</th><th>Mountpoint</th><th></th></tr></thead>
                            <tbody id="layout-editor-tbody">${rows}</tbody>
                        </table>
                    </div>
                    <div style="margin-top:10px;display:flex;gap:8px">
                        <button class="btn btn-secondary btn-sm" onclick="Pages._layoutEditorAddRow()">Add Partition</button>
                        <button class="btn btn-secondary btn-sm" onclick="Pages._layoutEditorFillLast()">Set Last → Fill</button>
                    </div>
                    <div id="layout-editor-result" style="margin-top:10px"></div>
                    <div class="form-actions" style="margin-top:16px">
                        <button class="btn btn-secondary" onclick="document.getElementById('layout-editor-modal').remove()">Cancel</button>
                        <button class="btn btn-primary" id="layout-save-btn" onclick="Pages._layoutEditorSave('${nodeId}')">Save Override</button>
                    </div>
                </div>
            </div>`;
        // Store current layout state on the element.
        overlay._layoutState = JSON.parse(JSON.stringify(layout));
        overlay._nodeId = nodeId;
        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });
    },

    _getLayoutEditorModal() {
        return document.getElementById('layout-editor-modal');
    },

    _layoutEditorUpdate(idx, field, value) {
        const modal = this._getLayoutEditorModal();
        if (!modal) return;
        modal._layoutState.partitions[idx][field] = value;
        this._layoutEditorValidate(modal);
    },

    _layoutEditorParseSizeInput(idx, value) {
        const modal = this._getLayoutEditorModal();
        if (!modal) return;
        const trimmed = value.trim().toLowerCase();
        let bytes = 0;
        if (trimmed === 'fill' || trimmed === '0' || trimmed === '') {
            bytes = 0;
        } else {
            const match = trimmed.match(/^([\d.]+)\s*(mb|gb|tb|kb|b)?$/);
            if (match) {
                const n = parseFloat(match[1]);
                const unit = match[2] || 'b';
                const mult = {b:1, kb:1024, mb:1024**2, gb:1024**3, tb:1024**4};
                bytes = Math.round(n * (mult[unit]||1));
            }
        }
        modal._layoutState.partitions[idx].size_bytes = bytes;
        this._layoutEditorValidate(modal);
    },

    _layoutEditorRemoveRow(idx) {
        const modal = this._getLayoutEditorModal();
        if (!modal) return;
        modal._layoutState.partitions.splice(idx, 1);
        // Re-render the table body.
        this._layoutEditorRebuildRows(modal);
        this._layoutEditorValidate(modal);
    },

    _layoutEditorAddRow() {
        const modal = this._getLayoutEditorModal();
        if (!modal) return;
        modal._layoutState.partitions.push({ label: '', size_bytes: 0, filesystem: 'xfs', mountpoint: '' });
        this._layoutEditorRebuildRows(modal);
    },

    _layoutEditorFillLast() {
        const modal = this._getLayoutEditorModal();
        if (!modal || !modal._layoutState.partitions.length) return;
        const last = modal._layoutState.partitions[modal._layoutState.partitions.length - 1];
        last.size_bytes = 0;
        this._layoutEditorRebuildRows(modal);
    },

    _layoutEditorRebuildRows(modal) {
        const tbody = document.getElementById('layout-editor-tbody');
        if (!tbody) return;
        const parts = modal._layoutState.partitions;
        tbody.innerHTML = parts.map((p, i) => `
            <tr>
                <td><input type="text" value="${escHtml(p.label||'')}" onchange="Pages._layoutEditorUpdate(${i},'label',this.value)" style="width:90px"></td>
                <td><input type="text" value="${p.size_bytes ? fmtBytes(p.size_bytes) : 'fill'}" onchange="Pages._layoutEditorParseSizeInput(${i},this.value)" style="width:80px"></td>
                <td><select onchange="Pages._layoutEditorUpdate(${i},'filesystem',this.value)">${
                    [['xfs','xfs'],['ext4','ext4'],['vfat','vfat (ESP/FAT)'],['swap','swap'],['biosboot','biosboot'],['','none (raw/LVM PV)']].map(([val,lbl]) =>
                        `<option value="${val}" ${(p.filesystem||'')===val?'selected':''}>${lbl}</option>`).join('')
                }</select></td>
                <td><input type="text" value="${escHtml(p.mountpoint||'')}" onchange="Pages._layoutEditorUpdate(${i},'mountpoint',this.value)" style="width:90px"></td>
                <td><button class="btn btn-danger btn-sm" onclick="Pages._layoutEditorRemoveRow(${i})" style="padding:2px 8px">✕</button></td>
            </tr>`).join('');
        this._layoutEditorValidate(modal);
    },

    _layoutEditorValidate(modal) {
        const warningsEl = document.getElementById('layout-editor-warnings');
        const saveBtn = document.getElementById('layout-save-btn');
        if (!warningsEl || !modal) return;
        const parts = modal._layoutState.partitions;
        const errs = [];
        const hasRoot = parts.some(p => p.mountpoint === '/');
        if (!hasRoot) errs.push('Must have a / (root) partition');
        const fillCount = parts.filter(p => !p.size_bytes).length;
        if (fillCount > 1) errs.push('Only one partition may use "fill" (size_bytes = 0)');
        // ESP must be vfat — UEFI firmware cannot read other filesystem types.
        for (const p of parts) {
            const isESP = p.mountpoint === '/boot/efi' || (p.flags || []).includes('esp');
            const fs = (p.filesystem || '').toLowerCase();
            if (isESP && fs !== '' && fs !== 'vfat' && fs !== 'fat32' && fs !== 'fat') {
                errs.push(`ESP partition (${p.mountpoint || p.label || '/boot/efi'}) must use vfat — UEFI firmware cannot read "${p.filesystem}"`);
            }
            // Swap mountpoint must pair with swap filesystem.
            if (p.mountpoint === 'swap' && fs !== '' && fs !== 'swap') {
                errs.push(`Partition with mountpoint "swap" must use the swap filesystem, not "${p.filesystem}"`);
            }
        }
        warningsEl.innerHTML = errs.map(e => `<div class="alert alert-error" style="margin:2px 0;font-size:12px">${escHtml(e)}</div>`).join('');
        if (saveBtn) saveBtn.disabled = errs.length > 0;
    },

    async _layoutEditorSave(nodeId) {
        const modal = this._getLayoutEditorModal();
        if (!modal) return;
        const saveBtn = document.getElementById('layout-save-btn');
        const resultEl = document.getElementById('layout-editor-result');
        if (saveBtn) { saveBtn.disabled = true; saveBtn.textContent = 'Saving…'; }
        try {
            await API.request('PUT', `/nodes/${nodeId}/layout-override`, { layout: modal._layoutState });
            modal.remove();
            Pages._onDiskLayoutTabOpen(nodeId);
        } catch (e) {
            if (resultEl) resultEl.innerHTML = `<div class="alert alert-error">${escHtml(e.message)}</div>`;
            if (saveBtn) { saveBtn.disabled = false; saveBtn.textContent = 'Save Override'; }
        }
    },

    // _onBMCTabOpen is called when the user clicks the Power / IPMI tab.
    // Starts a 20-second auto-refresh for power status and a 30-second refresh
    // for sensors. Clears both when the user navigates away (Router stop/start).
    _onBMCTabOpen(nodeId, hasBMC) {
        if (!hasBMC) return;
        // Clear any existing IPMI timers.
        if (Pages._powerTimer)  { clearInterval(Pages._powerTimer);  Pages._powerTimer  = null; }
        if (Pages._sensorTimer) { clearInterval(Pages._sensorTimer); Pages._sensorTimer = null; }
        // Load sensors immediately, then every 30s.
        Pages._refreshSensors(nodeId);
        Pages._sensorTimer = setInterval(() => Pages._refreshSensors(nodeId), 30000);
        // Power status is already being fetched; start auto-refresh every 20s.
        Pages._powerTimer = setInterval(() => Pages._refreshPowerStatus(nodeId), 20000);
    },

    // _refreshPowerStatus fetches the current power status from the server and
    // updates the status indicator, label, and button disabled states.
    async _refreshPowerStatus(nodeId) {
        const indicator = document.getElementById('power-indicator');
        const label     = document.getElementById('power-label');
        const lastEl    = document.getElementById('power-last-checked');
        const errEl     = document.getElementById('power-error-msg');
        if (!indicator) return; // tab not visible

        try {
            const data = await API.nodes.power.status(nodeId);
            const status = data.status || 'unknown';

            // Colour-code the status indicator.
            const colours = { on: 'var(--success)', off: 'var(--text-dim)', unknown: '#f59e0b', error: 'var(--error)' };
            const labels  = { on: 'Power On', off: 'Power Off', unknown: 'Unknown', error: 'BMC Unreachable' };
            indicator.style.background = colours[status] || colours.unknown;
            if (label) label.textContent = labels[status] || status;
            if (lastEl && data.last_checked) {
                lastEl.textContent = 'Last checked ' + fmtRelative(data.last_checked);
            }
            if (errEl) {
                if (data.error) {
                    errEl.textContent = data.error;
                    errEl.style.display = '';
                } else {
                    errEl.style.display = 'none';
                }
            }
            // Disable buttons that don't make sense for the current state.
            const btnOn    = document.getElementById('btn-power-on');
            const btnOff   = document.getElementById('btn-power-off');
            const btnCycle = document.getElementById('btn-power-cycle');
            const btnReset = document.getElementById('btn-power-reset');
            if (btnOn)    btnOn.disabled    = (status === 'on');
            if (btnOff)   btnOff.disabled   = (status === 'off');
            if (btnCycle) btnCycle.disabled = (status === 'off');
            if (btnReset) btnReset.disabled = (status === 'off');
        } catch (e) {
            if (label) label.textContent = 'Error';
            if (errEl) { errEl.textContent = e.message; errEl.style.display = ''; }
        }
    },

    // _doPowerAction executes a power action without a confirmation dialog.
    // Used for non-destructive actions (power on, boot-to-disk).
    async _doPowerAction(nodeId, action) {
        const feedback = document.getElementById('power-action-feedback');
        if (feedback) { feedback.textContent = `Sending ${action}…`; feedback.style.display = ''; feedback.className = 'alert alert-info'; }
        try {
            const fn = {
                on: ()   => API.nodes.power.on(nodeId),
                disk: () => API.nodes.power.diskBoot(nodeId),
            }[action];
            if (!fn) return;
            await fn();
            if (feedback) { feedback.textContent = `${action} command sent.`; feedback.className = 'alert alert-info'; }
            // Refresh status after a short delay to let BMC process the command.
            setTimeout(() => Pages._refreshPowerStatus(nodeId), 2000);
        } catch (e) {
            if (feedback) { feedback.textContent = `Error: ${e.message}`; feedback.className = 'alert alert-error'; }
        }
    },

    // _confirmPowerAction shows a modal dialog before executing a destructive action.
    _confirmPowerAction(nodeId, action, title, description) {
        if (!confirm(`${title}\n\n${description}\n\nAre you sure?`)) return;
        const actionFns = {
            off:   () => API.nodes.power.off(nodeId),
            cycle: () => API.nodes.power.cycle(nodeId),
            reset: () => API.nodes.power.reset(nodeId),
            pxe:   () => API.nodes.power.pxeBoot(nodeId),
        };
        const fn = actionFns[action];
        if (!fn) return;
        const feedback = document.getElementById('power-action-feedback');
        if (feedback) { feedback.textContent = `Sending ${action} command…`; feedback.style.display = ''; feedback.className = 'alert alert-info'; }
        fn().then(() => {
            if (feedback) { feedback.textContent = `${title} command sent.`; }
            setTimeout(() => Pages._refreshPowerStatus(nodeId), 2000);
        }).catch(e => {
            if (feedback) { feedback.textContent = `Error: ${e.message}`; feedback.className = 'alert alert-error'; }
        });
    },

    // _refreshSensors fetches sensor readings and renders them in the sensor table.
    async _refreshSensors(nodeId) {
        const wrap = document.getElementById('sensor-table-wrap');
        if (!wrap) return;
        try {
            const data = await API.nodes.sensors(nodeId);
            const sensors = data.sensors || [];
            if (!sensors.length) {
                wrap.innerHTML = `<div style="padding:12px;color:var(--text-dim);font-size:13px">No sensor readings returned by BMC.</div>`;
                return;
            }
            const statusColour = { ok: 'var(--success)', warning: '#f59e0b', critical: 'var(--error)' };
            wrap.innerHTML = `<div class="table-wrap"><table>
                <thead><tr><th>Sensor</th><th>Value</th><th>Units</th><th>Status</th></tr></thead>
                <tbody>
                ${sensors.map(s => `<tr>
                    <td>${escHtml(s.name)}</td>
                    <td class="mono">${escHtml(s.value || '—')}</td>
                    <td class="text-dim">${escHtml(s.units || '—')}</td>
                    <td><span style="color:${statusColour[s.status] || 'inherit'};font-weight:500">${escHtml(s.status)}</span></td>
                </tr>`).join('')}
                </tbody>
            </table></div>`;
        } catch (e) {
            wrap.innerHTML = `<div style="padding:12px;color:var(--error);font-size:12px">Sensor read failed: ${escHtml(e.message)}</div>`;
        }
    },

    _hardwareProfile(hw) {
        const sections = [];

        if ((hw.Disks && hw.Disks.length) || (hw.MDArrays && hw.MDArrays.length)) {
            let diskHtml = '';

            if (hw.MDArrays && hw.MDArrays.length) {
                diskHtml += `<div style="margin-bottom:16px">
                    <div class="kv-key" style="margin-bottom:8px">Software RAID Arrays</div>
                    <div class="table-wrap"><table>
                        <thead><tr><th>Array</th><th>Level</th><th>State</th><th>Size</th><th>Chunk</th><th>Members</th><th>FS</th><th>Mountpoint</th></tr></thead>
                        <tbody>
                        ${hw.MDArrays.map(a => `<tr>
                            <td class="mono text-accent">${escHtml(a.name)}</td>
                            <td class="mono dim">${escHtml(a.level || '—')}</td>
                            <td>${this._raidStateBadge(a.state)}</td>
                            <td class="mono dim">${fmtBytes(a.size_bytes)}</td>
                            <td class="mono dim">${a.chunk_kb ? a.chunk_kb + 'K' : '—'}</td>
                            <td class="mono dim" style="white-space:normal">${(a.members || []).join(', ') || '—'}</td>
                            <td class="mono dim">${escHtml(a.filesystem || '—')}</td>
                            <td class="mono dim">${escHtml(a.mountpoint || '—')}</td>
                        </tr>`).join('')}
                        </tbody>
                    </table></div>
                </div>`;
            }

            if (hw.Disks && hw.Disks.length) {
                diskHtml += hw.Disks.map(d => {
                    const parts = d.Partitions || [];
                    const segColors = ['seg-boot', 'seg-root', 'seg-swap', 'seg-data', 'seg-other'];
                    const totalSz = d.Size || 1;
                    const barHtml = parts.map((p, i) => {
                        const pct = Math.max(3, Math.round(((p.Size || 0) / totalSz) * 100));
                        const cls = segColors[i % segColors.length];
                        return `<div class="${cls} disk-segment" style="flex:${pct}" title="${escHtml(p.Name || '')}: ${fmtBytes(p.Size)}">
                            ${pct > 8 ? escHtml(p.Name || '') : ''}
                        </div>`;
                    }).join('');

                    return `<div style="margin-bottom:16px">
                        <div style="font-size:12px;font-family:var(--font-mono);font-weight:600;color:var(--text-secondary);margin-bottom:6px">
                            /dev/${escHtml(d.Name)} — ${fmtBytes(d.Size)}
                            ${d.Model ? `<span style="font-weight:400"> (${escHtml(d.Model)})</span>` : ''}
                            <span class="badge badge-neutral badge-sm" style="margin-left:6px">${d.Rotational ? 'HDD' : 'SSD'}</span>
                            ${d.Transport ? `<span class="badge badge-neutral badge-sm">${escHtml(d.Transport)}</span>` : ''}
                        </div>
                        ${parts.length ? `<div class="disk-bar">${barHtml}</div>` : ''}
                    </div>`;
                }).join('');
            }

            sections.push(cardWrap('Disk Topology', `<div class="card-body">${diskHtml}</div>`));
        }

        if (hw.IBDevices && hw.IBDevices.length) {
            const ibHtml = hw.IBDevices.map(dev => `
                <div style="margin-bottom:16px;padding-bottom:16px;border-bottom:1px solid var(--border)">
                    <div class="text-mono text-accent" style="font-size:13px;font-weight:600;margin-bottom:10px">${escHtml(dev.Name)}</div>
                    <div class="kv-grid mb-12">
                        <div class="kv-item"><div class="kv-key">Board ID</div><div class="kv-value">${escHtml(dev.BoardID || '—')}</div></div>
                        <div class="kv-item"><div class="kv-key">Firmware</div><div class="kv-value">${escHtml(dev.FWVersion || '—')}</div></div>
                        <div class="kv-item"><div class="kv-key">Node GUID</div><div class="kv-value" style="font-size:11px">${escHtml(dev.NodeGUID || '—')}</div></div>
                    </div>
                    ${dev.Ports && dev.Ports.length ? `
                    <div class="table-wrap"><table>
                        <thead><tr><th>Port</th><th>State</th><th>Phys State</th><th>Rate</th><th>LID</th><th>Link Layer</th><th>GID</th></tr></thead>
                        <tbody>
                        ${dev.Ports.map(p => `<tr>
                            <td class="mono">${p.Number}</td>
                            <td>${this._ibStateBadge(p.State)}</td>
                            <td class="mono dim">${escHtml(p.PhysState || '—')}</td>
                            <td class="mono dim">${escHtml(p.Rate || '—')}</td>
                            <td class="mono dim">${escHtml(p.LID || '—')}</td>
                            <td class="mono dim">${escHtml(p.LinkLayer || '—')}</td>
                            <td class="mono dim" style="font-size:11px">${escHtml(p.GID || '—')}</td>
                        </tr>`).join('')}
                        </tbody>
                    </table></div>` : ''}
                </div>`).join('');
            sections.push(cardWrap('InfiniBand Devices', `<div class="card-body">${ibHtml}</div>`));
        }

        if (hw.NICs && hw.NICs.length) {
            const nicHtml = `<div class="table-wrap"><table>
                <thead><tr><th>Interface</th><th>MAC</th><th>Speed</th><th>Driver</th><th>State</th><th>IP (Runtime)</th></tr></thead>
                <tbody>
                ${hw.NICs.map(n => `<tr>
                    <td class="mono" style="color:var(--accent)">${escHtml(n.Name || '—')}</td>
                    <td class="mono dim">${escHtml(n.MAC || n.MACAddress || '—')}</td>
                    <td class="mono dim">${escHtml(n.Speed || '—')}</td>
                    <td class="mono dim">${escHtml(n.Driver || '—')}</td>
                    <td>${this._nicStateBadge(n.State || n.OperState)}</td>
                    <td class="mono dim">${(n.Addresses || []).join(', ') || '—'}</td>
                </tr>`).join('')}
                </tbody>
            </table></div>`;
            sections.push(cardWrap('NICs', `<div class="card-body">${nicHtml}</div>`));
        }

        if (hw.BMC) {
            const bmcHtml = `<div class="card-body"><div class="kv-grid">
                <div class="kv-item"><div class="kv-key">IP</div><div class="kv-value">${escHtml(hw.BMC.IPAddress || '—')}</div></div>
                <div class="kv-item"><div class="kv-key">Firmware</div><div class="kv-value">${escHtml(hw.BMC.FirmwareVersion || '—')}</div></div>
                <div class="kv-item"><div class="kv-key">Manufacturer</div><div class="kv-value">${escHtml(hw.BMC.Manufacturer || '—')}</div></div>
            </div></div>`;
            sections.push(cardWrap('BMC / IPMI (Discovered)', bmcHtml));
        }

        if (!sections.length) {
            return `<div class="card"><div class="card-body">${emptyState('No hardware data', 'Detailed hardware profile is populated when the node registers via PXE.')}</div></div>`;
        }

        return sections.join('');
    },

    _raidStateBadge(state) {
        const cls = { active: 'badge-ready', degraded: 'badge-warning', rebuilding: 'badge-info' }[state] || 'badge-neutral';
        return `<span class="badge ${cls}">${escHtml(state || '—')}</span>`;
    },

    _ibStateBadge(state) {
        const s = (state || '').toUpperCase();
        const cls = s === 'ACTIVE' ? 'badge-ready' : s === 'DOWN' ? 'badge-error' : 'badge-warning';
        return `<span class="badge ${cls}">${escHtml(state || '—')}</span>`;
    },

    _nicStateBadge(state) {
        const s = (state || '').toLowerCase();
        const cls = s === 'up' ? 'badge-ready' : s === 'down' ? 'badge-error' : 'badge-neutral';
        return `<span class="badge ${cls}">${escHtml(state || '—')}</span>`;
    },

    async deleteNodeAndGoBack(id, name) {
        if (!confirm(`Delete node "${name}"? This cannot be undone.`)) return;
        try {
            await API.nodes.del(id);
            Router.navigate('/nodes');
        } catch (e) {
            alert(`Delete failed: ${e.message}`);
        }
    },

    // ── Node Groups ───────────────────────────────────────────────────────────

    async nodeGroups() {
        App.render(loading('Loading node groups…'));
        try {
            const [groupsResp, nodesResp] = await Promise.all([
                API.nodeGroups.list(),
                API.nodes.list(),
            ]);
            const groups = (groupsResp && (groupsResp.groups || groupsResp.node_groups)) || [];
            const nodes  = (nodesResp  && nodesResp.nodes)        || [];

            // Count nodes per group.
            const nodeCountMap = {};
            nodes.forEach(n => { if (n.group_id) nodeCountMap[n.group_id] = (nodeCountMap[n.group_id] || 0) + 1; });

            const tbody = groups.map(g => {
                const nodeCount = nodeCountMap[g.id] || 0;
                const hasLayout = !!(g.disk_layout_override && g.disk_layout_override.partitions && g.disk_layout_override.partitions.length);
                const partCount = hasLayout ? g.disk_layout_override.partitions.length : 0;
                const mountCount = (g.extra_mounts || []).length;

                return `<tr data-key="${escHtml(g.id)}">
                    <td style="font-weight:600">
                        <a href="#/nodes/groups/${g.id}" style="color:var(--text-primary)">${escHtml(g.name)}</a>
                    </td>
                    <td class="text-dim text-sm">${escHtml(g.description || '—')}</td>
                    <td style="text-align:center">
                        ${nodeCount > 0
                            ? `<span class="badge badge-info">${nodeCount}</span>`
                            : `<span class="text-dim">0</span>`}
                    </td>
                    <td style="text-align:center">
                        ${hasLayout
                            ? `<span class="badge badge-ready" title="${partCount} partition${partCount !== 1 ? 's' : ''}">yes</span>`
                            : `<span class="text-dim">—</span>`}
                    </td>
                    <td style="text-align:center">
                        ${mountCount > 0
                            ? `<span class="badge badge-neutral">${mountCount}</span>`
                            : `<span class="text-dim">—</span>`}
                    </td>
                    <td class="text-dim text-sm">${fmtRelative(g.updated_at)}</td>
                    <td>
                        <div class="flex gap-6">
                            <button class="btn btn-secondary btn-sm"
                                onclick='Pages.showNodeGroupModal(${JSON.stringify(JSON.stringify(g))})'>Edit</button>
                            <button class="btn btn-danger btn-sm"
                                onclick="Pages.deleteNodeGroup('${escHtml(g.id)}', '${escHtml(g.name)}')">Delete</button>
                        </div>
                    </td>
                </tr>`;
            }).join('');

            const tableHtml = groups.length
                ? `<div class="table-wrap"><table>
                    <thead><tr>
                        <th>Name</th>
                        <th>Description</th>
                        <th style="text-align:center">Nodes</th>
                        <th style="text-align:center">Disk Layout Override</th>
                        <th style="text-align:center">Extra Mounts</th>
                        <th>Updated</th>
                        <th>Actions</th>
                    </tr></thead>
                    <tbody>${tbody}</tbody>
                </table></div>`
                : emptyState(
                    'No node groups yet',
                    'Groups let you share disk layouts, mounts, and config across nodes with similar roles.',
                    `<button class="btn btn-primary" onclick="Pages.showNodeGroupModal(null)">Create First Group</button>`
                );

            App.render(`
                <div class="page-header">
                    <div>
                        <div class="page-title">Node Groups</div>
                        <div class="page-subtitle">${groups.length} group${groups.length !== 1 ? 's' : ''} defined</div>
                    </div>
                    <button class="btn btn-primary" onclick="Pages.showNodeGroupModal(null)">
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                            <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
                        </svg>
                        Create Group
                    </button>
                </div>

                <div class="tab-bar" style="margin-bottom:20px">
                    <div class="tab" onclick="Router.navigate('/nodes')">All Nodes</div>
                    <div class="tab active" onclick="Router.navigate('/nodes/groups')">Groups</div>
                </div>

                ${cardWrap('All Groups', tableHtml)}
            `);
        } catch (e) {
            App.render(alertBox(`Failed to load node groups: ${e.message}`));
        }
    },

    // nodeGroupDetail shows a read-only detail page for a single group.
    async nodeGroupDetail(id) {
        App.render(loading('Loading group…'));
        try {
            const [group, nodesResp] = await Promise.all([
                API.nodeGroups.get(id),
                API.nodes.list(),
            ]);
            const nodes = ((nodesResp && nodesResp.nodes) || []).filter(n => n.group_id === id);
            const hasLayout = !!(group.disk_layout_override && group.disk_layout_override.partitions && group.disk_layout_override.partitions.length);
            const mounts = group.extra_mounts || [];

            const nodesHtml = nodes.length === 0
                ? `<div class="text-dim" style="padding:12px;font-size:13px">No nodes currently assigned to this group.</div>`
                : `<div class="table-wrap"><table>
                    <thead><tr><th>Hostname</th><th>MAC</th><th>Status</th><th>Updated</th></tr></thead>
                    <tbody>
                    ${nodes.map(n => `<tr>
                        <td><a href="#/nodes/${n.id}" style="font-weight:500;color:var(--text-primary)">${escHtml(n.hostname || '(unassigned)')}</a></td>
                        <td class="text-mono text-dim text-sm">${escHtml(n.primary_mac || '—')}</td>
                        <td>${nodeBadge(n)}</td>
                        <td class="text-dim text-sm">${fmtRelative(n.updated_at)}</td>
                    </tr>`).join('')}
                    </tbody>
                </table></div>`;

            const layoutHtml = hasLayout
                ? (() => {
                    const parts = group.disk_layout_override.partitions;
                    const rows = parts.map(p => `<tr>
                        <td>${escHtml(p.label || '—')}</td>
                        <td>${p.size_bytes ? fmtBytes(p.size_bytes) : '<span class="badge badge-neutral" style="font-size:10px">fill</span>'}</td>
                        <td><span class="badge badge-neutral" style="font-size:10px">${escHtml(p.filesystem || '—')}</span></td>
                        <td class="text-mono">${escHtml(p.mountpoint || '—')}</td>
                    </tr>`).join('');
                    return `<div class="table-wrap"><table>
                        <thead><tr><th>Label</th><th>Size</th><th>Filesystem</th><th>Mountpoint</th></tr></thead>
                        <tbody>${rows}</tbody>
                    </table></div>`;
                })()
                : `<div class="text-dim" style="padding:12px;font-size:13px">No disk layout override — nodes in this group use their image default.</div>`;

            const mountsHtml = mounts.length === 0
                ? `<div class="text-dim" style="padding:12px;font-size:13px">No extra mounts defined on this group.</div>`
                : `<div class="table-wrap"><table>
                    <thead><tr><th>Source</th><th>Mount Point</th><th>FS Type</th><th>Options</th><th>Auto-mkdir</th><th>Comment</th></tr></thead>
                    <tbody>
                    ${mounts.map(m => `<tr>
                        <td class="text-mono">${escHtml(m.source || '—')}</td>
                        <td class="text-mono">${escHtml(m.mount_point || '—')}</td>
                        <td><span class="badge badge-neutral badge-sm">${escHtml(m.fs_type || '—')}</span></td>
                        <td class="text-mono text-dim" style="font-size:11px">${escHtml(m.options || 'defaults')}</td>
                        <td style="text-align:center">${m.auto_mkdir ? '✓' : '—'}</td>
                        <td class="text-dim" style="font-size:11px">${escHtml(m.comment || '—')}</td>
                    </tr>`).join('')}
                    </tbody>
                </table></div>`;

            App.render(`
                <div class="breadcrumb">
                    <a href="#/nodes">Nodes</a>
                    <span class="breadcrumb-sep">/</span>
                    <a href="#/nodes/groups">Groups</a>
                    <span class="breadcrumb-sep">/</span>
                    <span>${escHtml(group.name)}</span>
                </div>
                <div class="page-header">
                    <div>
                        <div class="page-title">${escHtml(group.name)}</div>
                        ${group.description ? `<div class="page-subtitle">${escHtml(group.description)}</div>` : ''}
                    </div>
                    <div class="flex gap-8">
                        <button class="btn btn-secondary" onclick='Pages.showNodeGroupModal(${JSON.stringify(JSON.stringify(group))})'>Edit Group</button>
                        <button class="btn btn-danger btn-sm" onclick="Pages.deleteNodeGroup('${escHtml(group.id)}', '${escHtml(group.name)}')">Delete</button>
                    </div>
                </div>

                <div class="kv-grid mb-16" style="max-width:480px;margin-bottom:20px">
                    <div class="kv-item"><div class="kv-key">ID</div><div class="kv-value text-mono text-sm">${escHtml(group.id)}</div></div>
                    <div class="kv-item"><div class="kv-key">Nodes</div><div class="kv-value">${nodes.length}</div></div>
                    <div class="kv-item"><div class="kv-key">Created</div><div class="kv-value">${fmtDate(group.created_at)}</div></div>
                    <div class="kv-item"><div class="kv-key">Updated</div><div class="kv-value">${fmtDate(group.updated_at)}</div></div>
                </div>

                ${cardWrap('Nodes in this Group', `<div class="card-body">${nodesHtml}</div>`)}
                ${cardWrap('Disk Layout Override', `<div class="card-body">${layoutHtml}</div>`)}
                ${cardWrap('Extra Mounts', `<div class="card-body">${mountsHtml}</div>`)}
            `);
        } catch (e) {
            App.render(alertBox(`Failed to load group: ${e.message}`));
        }
    },

    // showNodeGroupModal opens the Create or Edit group modal.
    showNodeGroupModal(groupJSON) {
        const group = groupJSON ? JSON.parse(groupJSON) : null;
        const isEdit = !!group;
        const mounts = (group && group.extra_mounts) || [];
        const hasLayout = !!(group && group.disk_layout_override && group.disk_layout_override.partitions && group.disk_layout_override.partitions.length);

        const existingMountRows = mounts.map((m, i) => Pages._ngMountRowHTML(i, m)).join('');
        const existingLayoutRows = hasLayout
            ? group.disk_layout_override.partitions.map((p, i) => Pages._ngLayoutRowHTML(i, p)).join('')
            : '';

        const overlay = document.createElement('div');
        overlay.id = 'node-group-modal';
        overlay.className = 'modal-overlay';

        // Store state for the layout editor.
        overlay._layoutState = hasLayout
            ? JSON.parse(JSON.stringify(group.disk_layout_override))
            : { partitions: [] };

        overlay.innerHTML = `
            <div class="modal" style="max-width:760px;width:96vw;max-height:90vh;overflow-y:auto">
                <div class="modal-header">
                    <span class="modal-title">${isEdit ? 'Edit Group' : 'Create Node Group'}</span>
                    <button class="modal-close" onclick="document.getElementById('node-group-modal').remove()">×</button>
                </div>
                <div class="modal-body" style="padding:20px">
                    <div id="ng-form-result" style="margin-bottom:10px"></div>

                    <!-- Basic fields -->
                    <div class="form-grid" style="margin-bottom:16px">
                        <div class="form-group">
                            <label>Name <span style="color:var(--error)">*</span></label>
                            <input type="text" id="ng-name" value="${escHtml(group ? group.name : '')}"
                                placeholder="compute-nodes"
                                pattern="^[a-zA-Z0-9][a-zA-Z0-9_\\-]*$"
                                oninput="Pages._ngValidateName(this)"
                                required>
                            <div id="ng-name-hint" class="form-hint" style="min-height:16px"></div>
                        </div>
                        <div class="form-group">
                            <label>Description</label>
                            <input type="text" id="ng-description" value="${escHtml(group ? (group.description || '') : '')}"
                                placeholder="Standard CPU compute nodes">
                        </div>
                    </div>

                    <!-- Disk Layout Override -->
                    <details id="ng-layout-details" ${hasLayout ? 'open' : ''} style="margin-bottom:16px;border:1px solid var(--border);border-radius:6px">
                        <summary style="padding:10px 14px;cursor:pointer;font-weight:600;font-size:14px;user-select:none">
                            Disk Layout Override
                            <span style="font-weight:400;font-size:12px;color:var(--text-secondary);margin-left:8px">
                                ${hasLayout ? `${group.disk_layout_override.partitions.length} partition${group.disk_layout_override.partitions.length !== 1 ? 's' : ''} defined` : 'inherits from image'}
                            </span>
                        </summary>
                        <div style="padding:14px;border-top:1px solid var(--border)">
                            <div style="display:flex;align-items:center;gap:10px;margin-bottom:12px">
                                <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-weight:400">
                                    <input type="radio" name="ng-layout-mode" value="inherit"
                                        ${!hasLayout ? 'checked' : ''} onchange="Pages._ngLayoutModeChange(this.value)">
                                    Inherit from image (default)
                                </label>
                                <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-weight:400">
                                    <input type="radio" name="ng-layout-mode" value="custom"
                                        ${hasLayout ? 'checked' : ''} onchange="Pages._ngLayoutModeChange(this.value)">
                                    Use custom layout for this group
                                </label>
                            </div>
                            <div id="ng-layout-editor" style="display:${hasLayout ? '' : 'none'}">
                                <div id="ng-layout-warnings" style="margin-bottom:8px"></div>
                                <div class="table-wrap">
                                    <table id="ng-layout-table">
                                        <thead><tr>
                                            <th>Label</th><th>Size</th><th>Filesystem</th><th>Mountpoint</th><th></th>
                                        </tr></thead>
                                        <tbody id="ng-layout-tbody">${existingLayoutRows}</tbody>
                                    </table>
                                </div>
                                <div style="margin-top:8px;display:flex;gap:8px">
                                    <button type="button" class="btn btn-secondary btn-sm" onclick="Pages._ngLayoutAddRow()">Add Partition</button>
                                    <button type="button" class="btn btn-secondary btn-sm" onclick="Pages._ngLayoutFillLast()">Set Last → Fill</button>
                                </div>
                            </div>
                        </div>
                    </details>

                    <!-- Extra Mounts -->
                    <details id="ng-mounts-details" ${mounts.length > 0 ? 'open' : ''} style="margin-bottom:20px;border:1px solid var(--border);border-radius:6px">
                        <summary style="padding:10px 14px;cursor:pointer;font-weight:600;font-size:14px;user-select:none">
                            Extra Mounts
                            <span style="font-weight:400;font-size:12px;color:var(--text-secondary);margin-left:8px" id="ng-mounts-count">
                                ${mounts.length > 0 ? `${mounts.length} mount${mounts.length !== 1 ? 's' : ''} defined` : 'none'}
                            </span>
                        </summary>
                        <div style="padding:14px;border-top:1px solid var(--border)">
                            <div style="display:flex;align-items:center;gap:8px;margin-bottom:10px">
                                <select id="ng-mounts-preset" style="font-size:12px;padding:4px 8px;width:auto">
                                    <option value="">— Apply preset —</option>
                                    <option value="nfs-home">NFS home</option>
                                    <option value="lustre">Lustre scratch</option>
                                    <option value="beegfs">BeeGFS data</option>
                                    <option value="cifs">CIFS / Samba</option>
                                    <option value="bind">Bind mount</option>
                                    <option value="tmpfs">tmpfs</option>
                                </select>
                                <button type="button" class="btn btn-secondary btn-sm" onclick="Pages._ngMountsApplyPreset()">Apply</button>
                                <button type="button" class="btn btn-secondary btn-sm" onclick="Pages._ngMountsAddRow()">+ Add Mount</button>
                            </div>
                            <div id="ng-mounts-wrap">
                                ${mounts.length > 0 ? `<div class="table-wrap" style="overflow-x:auto"><table id="ng-mounts-table" style="width:100%;font-size:12px;border-collapse:collapse">
                                    <thead><tr style="border-bottom:1px solid var(--border)">
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Source</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Mount Point</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">FS Type</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Options</th>
                                        <th style="text-align:center;padding:4px 6px;font-weight:500;color:var(--text-secondary)" title="Auto-mkdir">mkd</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Comment</th>
                                        <th style="padding:4px"></th>
                                    </tr></thead>
                                    <tbody id="ng-mounts-tbody">${existingMountRows}</tbody>
                                </table></div>` : `<div class="table-wrap" style="overflow-x:auto;display:none"><table id="ng-mounts-table" style="width:100%;font-size:12px;border-collapse:collapse">
                                    <thead><tr style="border-bottom:1px solid var(--border)">
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Source</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Mount Point</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">FS Type</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Options</th>
                                        <th style="text-align:center;padding:4px 6px;font-weight:500;color:var(--text-secondary)" title="Auto-mkdir">mkd</th>
                                        <th style="text-align:left;padding:4px 6px;font-weight:500;color:var(--text-secondary)">Comment</th>
                                        <th style="padding:4px"></th>
                                    </tr></thead>
                                    <tbody id="ng-mounts-tbody"></tbody>
                                </table></div>
                                <div id="ng-mounts-empty" style="text-align:center;padding:16px;color:var(--text-dim);font-size:12px">No mounts configured</div>`}
                            </div>
                        </div>
                    </details>

                    <div class="form-actions">
                        <button class="btn btn-secondary" onclick="document.getElementById('node-group-modal').remove()">Cancel</button>
                        <button class="btn btn-primary" id="ng-save-btn"
                            onclick="Pages._ngSubmit(${isEdit ? `'${escHtml(group.id)}'` : 'null'})">
                            ${isEdit ? 'Save Changes' : 'Create Group'}
                        </button>
                    </div>
                </div>
            </div>`;

        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });

        // Initialise layout state reference on the overlay.
        // (already set above, but re-read after DOM insert for clarity)
        const modal = document.getElementById('node-group-modal');
        if (modal) modal._layoutState = overlay._layoutState;
    },

    _ngValidateName(input) {
        const hint = document.getElementById('ng-name-hint');
        if (!hint) return;
        const val = input.value;
        if (!val) { hint.textContent = ''; return; }
        if (!/^[a-zA-Z0-9][a-zA-Z0-9_\-]*$/.test(val)) {
            hint.style.color = 'var(--error)';
            hint.textContent = 'Use letters, digits, hyphens, or underscores only';
        } else {
            hint.style.color = 'var(--success)';
            hint.textContent = '';
        }
    },

    _ngLayoutModeChange(mode) {
        const editor = document.getElementById('ng-layout-editor');
        if (!editor) return;
        editor.style.display = mode === 'custom' ? '' : 'none';
    },

    _ngGetModal() {
        return document.getElementById('node-group-modal');
    },

    // ── Group layout editor ───────────────────────────────────────────────────

    _ngLayoutRowHTML(idx, p) {
        p = p || {};
        return `<tr>
            <td><input type="text" value="${escHtml(p.label||'')}"
                onchange="Pages._ngLayoutUpdate(${idx},'label',this.value)" style="width:90px"></td>
            <td><input type="text" value="${p.size_bytes ? fmtBytes(p.size_bytes) : 'fill'}"
                onchange="Pages._ngLayoutParseSize(${idx},this.value)" style="width:80px" placeholder="e.g. 100GB or fill"></td>
            <td><select onchange="Pages._ngLayoutUpdate(${idx},'filesystem',this.value)">
                ${[['xfs','xfs'],['ext4','ext4'],['vfat','vfat (ESP/FAT)'],['swap','swap'],['biosboot','biosboot'],['','none (raw/LVM PV)']].map(([val,lbl]) =>
                    `<option value="${val}" ${(p.filesystem||'')===val?'selected':''}>${lbl}</option>`).join('')}
            </select></td>
            <td><input type="text" value="${escHtml(p.mountpoint||'')}"
                onchange="Pages._ngLayoutUpdate(${idx},'mountpoint',this.value)" style="width:90px"></td>
            <td><button class="btn btn-danger btn-sm" onclick="Pages._ngLayoutRemoveRow(${idx})"
                style="padding:2px 8px">✕</button></td>
        </tr>`;
    },

    _ngLayoutUpdate(idx, field, value) {
        const modal = this._ngGetModal();
        if (!modal) return;
        modal._layoutState.partitions[idx][field] = value;
        this._ngLayoutValidate(modal);
    },

    _ngLayoutParseSize(idx, value) {
        const modal = this._ngGetModal();
        if (!modal) return;
        const trimmed = value.trim().toLowerCase();
        let bytes = 0;
        if (trimmed !== 'fill' && trimmed !== '0' && trimmed !== '') {
            const match = trimmed.match(/^([\d.]+)\s*(mb|gb|tb|kb|b)?$/);
            if (match) {
                const n = parseFloat(match[1]);
                const unit = match[2] || 'b';
                const mult = {b:1, kb:1024, mb:1024**2, gb:1024**3, tb:1024**4};
                bytes = Math.round(n * (mult[unit]||1));
            }
        }
        modal._layoutState.partitions[idx].size_bytes = bytes;
        this._ngLayoutValidate(modal);
    },

    _ngLayoutRemoveRow(idx) {
        const modal = this._ngGetModal();
        if (!modal) return;
        modal._layoutState.partitions.splice(idx, 1);
        this._ngLayoutRebuildRows(modal);
        this._ngLayoutValidate(modal);
    },

    _ngLayoutAddRow() {
        const modal = this._ngGetModal();
        if (!modal) return;
        if (!modal._layoutState) modal._layoutState = { partitions: [] };
        modal._layoutState.partitions.push({ label: '', size_bytes: 0, filesystem: 'xfs', mountpoint: '' });
        this._ngLayoutRebuildRows(modal);
    },

    _ngLayoutFillLast() {
        const modal = this._ngGetModal();
        if (!modal || !modal._layoutState || !modal._layoutState.partitions.length) return;
        modal._layoutState.partitions[modal._layoutState.partitions.length - 1].size_bytes = 0;
        this._ngLayoutRebuildRows(modal);
    },

    _ngLayoutRebuildRows(modal) {
        const tbody = document.getElementById('ng-layout-tbody');
        if (!tbody) return;
        tbody.innerHTML = (modal._layoutState.partitions || []).map((p, i) =>
            this._ngLayoutRowHTML(i, p)
        ).join('');
        this._ngLayoutValidate(modal);
    },

    _ngLayoutValidate(modal) {
        const warnEl  = document.getElementById('ng-layout-warnings');
        const saveBtn = document.getElementById('ng-save-btn');
        if (!warnEl || !modal || !modal._layoutState) return;
        const mode = document.querySelector('input[name="ng-layout-mode"]:checked');
        if (!mode || mode.value !== 'custom') { warnEl.innerHTML = ''; if (saveBtn) saveBtn.disabled = false; return; }
        const parts = modal._layoutState.partitions || [];
        const errs = [];
        if (!parts.some(p => p.mountpoint === '/')) errs.push('Must have a / (root) partition');
        if (parts.filter(p => !p.size_bytes).length > 1) errs.push('Only one partition may use "fill"');
        warnEl.innerHTML = errs.map(e => `<div class="alert alert-error" style="margin:2px 0;font-size:12px">${escHtml(e)}</div>`).join('');
        if (saveBtn) saveBtn.disabled = errs.length > 0;
    },

    // ── Group mounts editor ───────────────────────────────────────────────────

    _ngMountRowHTML(idx, m) {
        m = m || {};
        const fsTypes = ['nfs','nfs4','cifs','beegfs','lustre','gpfs','xfs','ext4','bind','9p','tmpfs','vfat','ext3','smbfs'];
        const fsSelect = fsTypes.map(t =>
            `<option value="${t}"${m.fs_type === t ? ' selected' : ''}>${t}</option>`
        ).join('');
        return `<tr data-ng-mount-idx="${idx}" style="border-bottom:1px solid var(--border)">
            <td style="padding:4px 3px">
                <input type="text" name="ng_mount_source" value="${escHtml(m.source||'')}"
                    placeholder="nfs-server:/export/home" style="width:100%;min-width:130px;font-size:12px" required>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="ng_mount_point" value="${escHtml(m.mount_point||'')}"
                    placeholder="/mnt/share" style="width:100%;min-width:110px;font-size:12px" required>
            </td>
            <td style="padding:4px 3px">
                <select name="ng_mount_fs_type" style="font-size:12px;padding:2px 4px">${fsSelect}</select>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="ng_mount_options" value="${escHtml(m.options||'')}"
                    placeholder="defaults,_netdev" style="width:100%;min-width:120px;font-size:12px">
            </td>
            <td style="padding:4px 3px;text-align:center">
                <input type="checkbox" name="ng_mount_auto_mkdir" ${m.auto_mkdir !== false ? 'checked' : ''}>
            </td>
            <td style="padding:4px 3px">
                <input type="text" name="ng_mount_comment" value="${escHtml(m.comment||'')}"
                    placeholder="optional note" style="width:100%;min-width:80px;font-size:12px">
            </td>
            <td style="padding:4px 3px">
                <button type="button" class="btn btn-danger btn-sm"
                    onclick="Pages._ngMountsRemoveRow(this)" style="padding:2px 6px;font-size:11px">✕</button>
            </td>
        </tr>`;
    },

    _ngMountsAddRow(preset) {
        const tbody = document.getElementById('ng-mounts-tbody');
        const wrap  = document.getElementById('ng-mounts-wrap');
        if (!tbody) return;
        const idx = tbody.querySelectorAll('tr').length;
        tbody.insertAdjacentHTML('beforeend', Pages._ngMountRowHTML(idx, preset || {}));
        // Show table if hidden.
        const tableWrap = wrap ? wrap.querySelector('.table-wrap') : null;
        if (tableWrap) tableWrap.style.display = '';
        const empty = document.getElementById('ng-mounts-empty');
        if (empty) empty.remove();
        this._ngUpdateMountsCount();
    },

    _ngMountsRemoveRow(btn) {
        const row = btn.closest('tr');
        if (row) row.remove();
        const tbody = document.getElementById('ng-mounts-tbody');
        const wrap  = document.getElementById('ng-mounts-wrap');
        if (tbody && tbody.querySelectorAll('tr').length === 0) {
            const tableWrap = wrap ? wrap.querySelector('.table-wrap') : null;
            if (tableWrap) tableWrap.style.display = 'none';
            if (wrap && !document.getElementById('ng-mounts-empty')) {
                wrap.insertAdjacentHTML('beforeend',
                    `<div id="ng-mounts-empty" style="text-align:center;padding:16px;color:var(--text-dim);font-size:12px">No mounts configured</div>`);
            }
        }
        this._ngUpdateMountsCount();
    },

    _ngMountsApplyPreset() {
        const sel = document.getElementById('ng-mounts-preset');
        if (!sel || !sel.value) return;
        const presets = {
            'nfs-home': { source: 'nfs-server:/export/home', mount_point: '/home/shared',  fs_type: 'nfs4',   options: 'defaults,_netdev,vers=4',               auto_mkdir: true,  comment: 'NFS home directory' },
            'lustre':   { source: 'mgs@tcp:/scratch',        mount_point: '/scratch',       fs_type: 'lustre', options: 'defaults,_netdev,flock',                auto_mkdir: true,  comment: 'Lustre scratch' },
            'beegfs':   { source: 'beegfs',                  mount_point: '/mnt/beegfs',    fs_type: 'beegfs', options: 'defaults,_netdev',                      auto_mkdir: true,  comment: 'BeeGFS data' },
            'cifs':     { source: '//winserver/share',        mount_point: '/mnt/share',     fs_type: 'cifs',   options: 'defaults,_netdev,vers=3.0,sec=ntlmssp', auto_mkdir: true,  comment: 'CIFS (Windows) share' },
            'bind':     { source: '/data/src',               mount_point: '/data/dst',      fs_type: 'bind',   options: 'defaults,bind',                         auto_mkdir: true,  comment: 'Bind mount' },
            'tmpfs':    { source: 'tmpfs',                   mount_point: '/tmp',           fs_type: 'tmpfs',  options: 'defaults,size=4G,mode=1777',            auto_mkdir: false, comment: 'tmpfs /tmp' },
        };
        const p = presets[sel.value];
        if (p) Pages._ngMountsAddRow(p);
        sel.value = '';
    },

    _ngUpdateMountsCount() {
        const count = document.getElementById('ng-mounts-count');
        if (!count) return;
        const tbody = document.getElementById('ng-mounts-tbody');
        const n = tbody ? tbody.querySelectorAll('tr').length : 0;
        count.textContent = n > 0 ? `${n} mount${n !== 1 ? 's' : ''} defined` : 'none';
    },

    _ngCollectMounts() {
        const tbody = document.getElementById('ng-mounts-tbody');
        if (!tbody) return [];
        const mounts = [];
        tbody.querySelectorAll('tr').forEach(row => {
            const source    = (row.querySelector('[name="ng_mount_source"]')?.value || '').trim();
            const mountPoint = (row.querySelector('[name="ng_mount_point"]')?.value || '').trim();
            const fsType    = row.querySelector('[name="ng_mount_fs_type"]')?.value || 'nfs';
            const options   = (row.querySelector('[name="ng_mount_options"]')?.value || '').trim();
            const autoMkdir = row.querySelector('[name="ng_mount_auto_mkdir"]')?.checked !== false;
            const comment   = (row.querySelector('[name="ng_mount_comment"]')?.value || '').trim();
            if (!source || !mountPoint) return;
            mounts.push({ source, mount_point: mountPoint, fs_type: fsType, options, auto_mkdir: autoMkdir, comment, dump: 0, pass: 0 });
        });
        return mounts;
    },

    // ── Group form submit ─────────────────────────────────────────────────────

    async _ngSubmit(groupId) {
        const nameEl   = document.getElementById('ng-name');
        const descEl   = document.getElementById('ng-description');
        const resultEl = document.getElementById('ng-form-result');
        const saveBtn  = document.getElementById('ng-save-btn');
        const modal    = this._ngGetModal();

        if (!nameEl || !modal) return;

        const name = (nameEl.value || '').trim();
        const desc = (descEl ? descEl.value : '').trim();

        if (!name) {
            if (resultEl) resultEl.innerHTML = `<div class="alert alert-error">Name is required</div>`;
            return;
        }
        if (!/^[a-zA-Z0-9][a-zA-Z0-9_\-]*$/.test(name)) {
            if (resultEl) resultEl.innerHTML = `<div class="alert alert-error">Name must contain only letters, digits, hyphens, and underscores</div>`;
            return;
        }

        // Determine layout override.
        const modeEl = document.querySelector('input[name="ng-layout-mode"]:checked');
        const useCustomLayout = modeEl && modeEl.value === 'custom';
        let diskLayoutOverride = null;
        if (useCustomLayout) {
            const parts = (modal._layoutState && modal._layoutState.partitions) || [];
            if (!parts.some(p => p.mountpoint === '/')) {
                if (resultEl) resultEl.innerHTML = `<div class="alert alert-error">Disk layout must include a root (/) partition</div>`;
                return;
            }
            if (parts.filter(p => !p.size_bytes).length > 1) {
                if (resultEl) resultEl.innerHTML = `<div class="alert alert-error">Only one partition may use "fill"</div>`;
                return;
            }
            diskLayoutOverride = { partitions: parts };
        }

        const extraMounts = this._ngCollectMounts();

        const body = {
            name,
            description: desc,
            disk_layout_override: diskLayoutOverride,
            extra_mounts: extraMounts,
        };

        if (saveBtn) { saveBtn.disabled = true; saveBtn.textContent = 'Saving…'; }
        if (resultEl) resultEl.innerHTML = '';

        try {
            if (groupId) {
                await API.nodeGroups.update(groupId, body);
                App.toast(`Group "${name}" updated`, 'success');
            } else {
                await API.nodeGroups.create(body);
                App.toast(`Group "${name}" created`, 'success');
            }
            document.getElementById('node-group-modal').remove();
            Pages.nodeGroups();
        } catch (e) {
            if (resultEl) resultEl.innerHTML = `<div class="alert alert-error">${escHtml(e.message)}</div>`;
            if (saveBtn) { saveBtn.disabled = false; saveBtn.textContent = groupId ? 'Save Changes' : 'Create Group'; }
        }
    },

    // deleteNodeGroup deletes a group after checking whether nodes are using it.
    async deleteNodeGroup(id, name) {
        // Fetch current nodes to see if any are using this group.
        let usingNodes = [];
        try {
            const resp = await API.nodes.list();
            usingNodes = ((resp && resp.nodes) || []).filter(n => n.group_id === id);
        } catch (_) {}

        let confirmed = false;
        if (usingNodes.length > 0) {
            const names = usingNodes.map(n => n.hostname || n.primary_mac).join(', ');
            confirmed = confirm(
                `${usingNodes.length} node${usingNodes.length !== 1 ? 's are' : ' is'} using this group: ${names}.\n\n` +
                `Deleting it will remove the group assignment from those nodes but keep them as standalone nodes.\n\nContinue?`
            );
        } else {
            confirmed = confirm(`Delete group "${name}"? This cannot be undone.`);
        }
        if (!confirmed) return;

        try {
            await API.nodeGroups.del(id);
            App.toast(`Group "${name}" deleted`, 'success');
            Pages.nodeGroups();
        } catch (e) {
            if (e.message && e.message.includes('409')) {
                alert(`Cannot delete group — it is still in use. Remove all node assignments first or contact the admin.`);
            } else {
                App.toast(`Delete failed: ${e.message}`, 'error');
            }
        }
    },

    // ── Build from ISO modal ─────────────────────────────────────────���─────

    // _isoDetectDistro parses common ISO URL patterns and returns
    // { distro, version, os, name } best-effort pre-fills.
    _isoDetectDistro(url) {
        const lower = url.toLowerCase();
        const base  = lower.split('?')[0].split('/').pop();

        // Version: grab first X.Y (or X.Y.Z) token from the filename.
        const verMatch = base.match(/[\-_](\d+\.\d+(?:\.\d+)?)/);
        const version  = verMatch ? verMatch[1] : '';

        if (lower.includes('rockylinux.org') || base.startsWith('rocky-')) {
            return { distro: 'rocky', os: 'Rocky Linux ' + (version || ''), version };
        }
        if (lower.includes('almalinux.org') || base.startsWith('almalinux-')) {
            return { distro: 'almalinux', os: 'AlmaLinux ' + (version || ''), version };
        }
        if (lower.includes('centos.org') || base.startsWith('centos-')) {
            return { distro: 'centos', os: 'CentOS ' + (version || ''), version };
        }
        if (lower.includes('ubuntu.com') || lower.includes('releases.ubuntu.com') || lower.includes('cdimage.ubuntu.com') || base.startsWith('ubuntu-')) {
            return { distro: 'ubuntu', os: 'Ubuntu ' + (version || ''), version };
        }
        if (lower.includes('debian.org') || base.startsWith('debian-')) {
            return { distro: 'debian', os: 'Debian ' + (version || ''), version };
        }
        if (lower.includes('opensuse.org') || lower.includes('suse.com') || base.startsWith('opensuse-') || base.startsWith('sle-')) {
            return { distro: 'suse', os: 'SUSE / openSUSE', version };
        }
        if (lower.includes('alpinelinux.org') || base.startsWith('alpine-')) {
            return { distro: 'alpine', os: 'Alpine Linux', version };
        }
        return { distro: '', os: '', version };
    },

    // _isoFormatHint returns a human-readable label for the auto-install format.
    _isoFormatHint(distro) {
        const fmts = {
            rocky: 'kickstart install', almalinux: 'kickstart install',
            centos: 'kickstart install', rhel: 'kickstart install',
            ubuntu: 'cloud-init autoinstall', debian: 'preseed install',
            suse: 'AutoYaST install', alpine: 'answers file',
        };
        return fmts[distro] || 'automated install';
    },

    // _countUniquePackages returns the number of unique packages across all
    // provided role objects (which have a package_count field from the API).
    // Since the server pre-computes per-role cross-distro unique counts, we
    // cannot trivially deduplicate across roles client-side without knowing the
    // actual package lists. We approximate by summing and noting the overlap.
    // The "Preview" line is best-effort; exact count is computed server-side.
    _countUniquePackages(selectedRoles) {
        // Use the max single-role count plus 30% of remaining as an approximation.
        if (!selectedRoles.length) return 0;
        const counts = selectedRoles.map(r => r.package_count).sort((a, b) => b - a);
        let total = counts[0];
        for (let i = 1; i < counts.length; i++) {
            total += Math.round(counts[i] * 0.6); // ~40% overlap heuristic
        }
        return total;
    },

    async showBuildFromISOModal(prefillUrl) {
        // Load roles in parallel with modal render.
        let roles = [];
        try {
            const resp = await API.imageRoles.list();
            roles = resp.roles || [];
        } catch (e) {
            console.warn('Failed to load image roles:', e.message);
        }

        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.id = 'build-iso-modal';

        const rolesHtml = roles.length
            ? roles.map(r => `
                <label class="role-card" data-role-id="${escHtml(r.id)}">
                    <div class="role-card-header">
                        <input type="checkbox" name="role_ids" value="${escHtml(r.id)}" onchange="Pages._onRoleToggle()">
                        <span class="role-card-name">${escHtml(r.name)}</span>
                        <span class="role-card-count">${r.package_count} pkgs</span>
                    </div>
                    <div class="role-card-desc">${escHtml(r.description)}</div>
                    ${r.notes ? `<div class="role-card-note">
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24" style="width:11px;height:11px;flex-shrink:0"><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
                        ${escHtml(r.notes)}
                    </div>` : ''}
                </label>`).join('')
            : '<div style="color:var(--text-secondary);font-size:13px;padding:12px 0">No roles available — build will use minimal install.</div>';

        // Store roles on overlay for access in event handlers.
        overlay._roles = roles;

        overlay.innerHTML = `
            <div class="modal modal-wide">
                <div class="modal-header">
                    <span class="modal-title">Build Image from ISO</span>
                    <button class="modal-close" onclick="document.getElementById('build-iso-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <form id="build-iso-form" onsubmit="Pages.submitBuildFromISO(event)">

                        <div class="form-group" style="margin-bottom:6px">
                            <label>ISO URL *</label>
                            <input type="url" id="build-iso-url" name="url"
                                placeholder="https://download.rockylinux.org/pub/rocky/10/isos/x86_64/Rocky-10.1-x86_64-dvd1.iso"
                                oninput="Pages._onISOUrlChange(this.value)"
                                required>
                            <div id="build-iso-url-hint" class="form-hint" style="min-height:18px"></div>
                        </div>

                        <div class="form-grid" style="margin-bottom:16px">
                            <div class="form-group">
                                <label>Name *</label>
                                <input type="text" name="name" id="build-iso-name" placeholder="rocky10-compute" required>
                            </div>
                            <div class="form-group">
                                <label>Version</label>
                                <input type="text" name="version" id="build-iso-version" placeholder="10.1">
                            </div>
                            <div class="form-group">
                                <label>OS</label>
                                <input type="text" name="os" id="build-iso-os" placeholder="Rocky Linux 10">
                            </div>
                        </div>

                        <div style="margin-bottom:16px">
                            <div style="font-size:12px;font-weight:600;color:var(--text-secondary);margin-bottom:8px;text-transform:uppercase;letter-spacing:0.5px">Node roles</div>
                            <div class="role-picker" id="build-iso-roles">
                                ${rolesHtml}
                            </div>
                            <div id="build-iso-role-preview" class="form-hint" style="margin-top:8px;min-height:18px"></div>
                        </div>

                        <div class="form-group" style="margin-bottom:16px">
                            <label style="font-size:12px;font-weight:600;color:var(--text-secondary);display:block;margin-bottom:8px;text-transform:uppercase;letter-spacing:0.5px">Firmware</label>
                            <div style="display:flex;gap:20px;align-items:center">
                                <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-weight:400">
                                    <input type="radio" name="firmware" value="uefi" id="build-iso-fw-uefi" checked>
                                    <span>UEFI <span class="badge badge-neutral badge-sm" style="margin-left:2px">default</span></span>
                                </label>
                                <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-weight:400">
                                    <input type="radio" name="firmware" value="bios" id="build-iso-fw-bios">
                                    <span>BIOS <span style="color:var(--text-secondary);font-size:11px">(legacy)</span></span>
                                </label>
                            </div>
                            <div class="form-hint">UEFI: OVMF + ESP partition. BIOS: SeaBIOS + biosboot GPT partition. Use BIOS for legacy HPC nodes without EFI firmware.</div>
                        </div>

                        <div class="form-group" style="margin-bottom:16px">
                            <label style="display:flex;align-items:center;gap:8px;cursor:pointer;font-weight:400">
                                <input type="checkbox" name="install_updates" id="build-iso-updates">
                                <span>Install OS updates during build</span>
                            </label>
                            <div class="form-hint">Adds ~5-10 min, produces a fully patched image</div>
                        </div>

                        <div style="margin-bottom:16px">
                            <div style="font-size:12px;font-weight:600;color:var(--text-secondary);margin-bottom:12px;text-transform:uppercase;letter-spacing:0.5px">VM Resources</div>
                            <div class="form-grid">
                                <div class="form-group">
                                    <label>Disk: <span id="build-iso-disk-val">20 GB</span></label>
                                    <input type="range" name="disk_size_gb" id="build-iso-disk"
                                        min="10" max="100" step="5" value="20"
                                        oninput="document.getElementById('build-iso-disk-val').textContent=this.value+' GB'">
                                </div>
                                <div class="form-group">
                                    <label>Memory: <span id="build-iso-mem-val">2 GB</span></label>
                                    <input type="range" name="memory_gb" id="build-iso-mem"
                                        min="1" max="8" step="1" value="2"
                                        oninput="document.getElementById('build-iso-mem-val').textContent=this.value+' GB'">
                                </div>
                                <div class="form-group">
                                    <label>CPUs: <span id="build-iso-cpu-val">2</span></label>
                                    <input type="range" name="cpus" id="build-iso-cpu"
                                        min="1" max="8" step="1" value="2"
                                        oninput="document.getElementById('build-iso-cpu-val').textContent=this.value">
                                </div>
                            </div>
                        </div>

                        <div style="margin-bottom:16px">
                            <div style="font-size:12px;font-weight:600;color:var(--text-secondary);margin-bottom:12px;text-transform:uppercase;letter-spacing:0.5px">Default Login (optional)</div>
                            <div class="form-hint" style="margin-bottom:10px">Creates a user account in the image with sudo/wheel access. Supported for RHEL-family (Rocky, Alma, RHEL) builds. Leave blank to use only the built-in root account.</div>
                            <div class="form-grid">
                                <div class="form-group">
                                    <label for="build-iso-username">Username</label>
                                    <input type="text" name="default_username" id="build-iso-username"
                                        placeholder="e.g. admin" autocomplete="off">
                                </div>
                                <div class="form-group">
                                    <label for="build-iso-password">Password</label>
                                    <input type="password" name="default_password" id="build-iso-password"
                                        autocomplete="new-password">
                                </div>
                            </div>
                        </div>

                        <details id="build-iso-advanced">
                            <summary style="font-size:12px;font-weight:600;color:var(--text-secondary);cursor:pointer;user-select:none;margin-bottom:10px">
                                Advanced: Custom Kickstart
                            </summary>
                            <div class="alert alert-warning" style="font-size:12px;margin-bottom:10px">
                                Overrides role-based package list. Use only if you need full control.
                            </div>
                            <div class="form-group">
                                <textarea name="custom_kickstart" id="build-iso-kickstart" rows="8"
                                    placeholder="# Paste your kickstart/autoinstall/preseed here...&#10;# Leave blank to use the auto-generated config from roles."
                                    style="font-family:var(--font-mono);font-size:12px"></textarea>
                            </div>
                        </details>

                        <div id="build-iso-result" style="margin-top:12px"></div>
                        <div class="form-actions">
                            <button type="button" class="btn btn-secondary" onclick="document.getElementById('build-iso-modal').remove()">Cancel</button>
                            <button type="submit" class="btn btn-primary" id="build-iso-btn">Build Image</button>
                        </div>
                    </form>
                </div>
            </div>`;

        document.body.appendChild(overlay);
        overlay.addEventListener('click', e => { if (e.target === overlay) overlay.remove(); });

        // Prefill the URL field if provided (e.g. retrying an interrupted build).
        if (prefillUrl) {
            const urlInput = overlay.querySelector('#build-iso-url');
            if (urlInput) {
                urlInput.value = prefillUrl;
                Pages._onISOUrlChange(prefillUrl);
            }
        }

        overlay.querySelector('#build-iso-url').focus();

        // Initial role preview state.
        Pages._onRoleToggle();
    },

    _onISOUrlChange(url) {
        const hintEl  = document.getElementById('build-iso-url-hint');
        const nameEl  = document.getElementById('build-iso-name');
        const verEl   = document.getElementById('build-iso-version');
        const osEl    = document.getElementById('build-iso-os');
        if (!hintEl) return;

        const det = Pages._isoDetectDistro(url);
        if (det.distro) {
            hintEl.textContent = 'Detected: ' + det.os + ' (' + Pages._isoFormatHint(det.distro) + ')';
            hintEl.style.color = 'var(--success)';
            if (nameEl && !nameEl.value && det.distro) {
                // Auto-generate a slug name from distro + version.
                const slug = (det.distro + (det.version ? det.version.replace(/\./g, '') : '')).toLowerCase();
                nameEl.value = slug;
            }
            if (verEl && !verEl.value && det.version) {
                verEl.value = det.version;
            }
            if (osEl && !osEl.value && det.os) {
                osEl.value = det.os;
            }
        } else {
            hintEl.textContent = url.toLowerCase().endsWith('.iso') ? 'ISO URL — distro could not be detected' : '';
            hintEl.style.color = 'var(--text-secondary)';
        }
    },

    _onRoleToggle() {
        const modal   = document.getElementById('build-iso-modal');
        const preview = document.getElementById('build-iso-role-preview');
        const btn     = document.getElementById('build-iso-btn');
        if (!preview || !modal) return;

        const checked  = [...document.querySelectorAll('#build-iso-roles input[type=checkbox]:checked')];
        const roleIds  = checked.map(cb => cb.value);
        const roles    = (modal._roles || []).filter(r => roleIds.includes(r.id));
        const hasKS    = !!(document.getElementById('build-iso-kickstart') || {}).value;

        if (roles.length) {
            const pkgEst = Pages._countUniquePackages(roles);
            preview.textContent = 'Preview: ~' + pkgEst + ' unique packages will be installed';
            preview.style.color = 'var(--accent)';
        } else if (hasKS) {
            preview.textContent = 'Using custom kickstart — role packages ignored';
            preview.style.color = 'var(--text-secondary)';
        } else {
            preview.textContent = 'No roles selected — minimal base install only';
            preview.style.color = 'var(--text-secondary)';
        }

        // Disable submit if no roles AND no custom kickstart.
        if (btn) btn.disabled = (roles.length === 0 && !hasKS);
    },

    async submitBuildFromISO(e) {
        e.preventDefault();
        const form   = e.target;
        const btn    = document.getElementById('build-iso-btn');
        const result = document.getElementById('build-iso-result');
        const data   = new FormData(form);

        btn.disabled  = true;
        btn.textContent = 'Submitting…';
        result.innerHTML = '';

        const roleIds = [...form.querySelectorAll('input[name="role_ids"]:checked')].map(cb => cb.value);

        const firmwareEl = form.querySelector('input[name="firmware"]:checked');
        const body = {
            url:              data.get('url'),
            name:             data.get('name'),
            version:          data.get('version') || '',
            os:               data.get('os') || '',
            disk_size_gb:     parseInt(data.get('disk_size_gb') || '20', 10),
            memory_mb:        parseInt(data.get('memory_gb') || '2', 10) * 1024,
            cpus:             parseInt(data.get('cpus') || '2', 10),
            role_ids:         roleIds,
            firmware:         firmwareEl ? firmwareEl.value : 'uefi',
            install_updates:  form.querySelector('input[name="install_updates"]').checked,
            custom_kickstart: data.get('custom_kickstart') || '',
            default_username: data.get('default_username') || undefined,
            default_password: data.get('default_password') || undefined,
        };

        try {
            const img = await API.factory.buildFromISO(body);
            result.innerHTML = alertBox(
                'Build started: ' + img.name + ' (' + img.id.substring(0, 8) + ') — status: ' + img.status +
                '. This may take 10-30 minutes.',
                'success'
            );
            App.setAutoRefresh(() => Pages.images(), 5000);
            setTimeout(() => {
                const modal = document.getElementById('build-iso-modal');
                if (modal) modal.remove();
                Router.navigate('/images/' + img.id);
            }, 1800);
        } catch (ex) {
            result.innerHTML = alertBox('Build failed: ' + ex.message);
            btn.disabled = false;
            btn.textContent = 'Build Image';
        }
    },

    // ── ISO build progress panel ───────────────────────────────────────────

    // _isoBuildInProgress returns the HTML for the inline build progress panel
    // shown on the image detail page when status=building.
    _isoBuildInProgress(img) {
        if (img.build_method !== 'iso') {
            return `<div class="alert alert-info" style="margin-bottom:16px">Build in progress — connecting to live stream…</div>`;
        }
        return `
            <div class="card iso-build-panel" style="margin-bottom:16px" id="iso-build-card">
                <div class="card-header">
                    <span class="card-title">Building ${escHtml(img.name)} from ISO</span>
                    <span class="badge badge-building" id="iso-build-badge">building</span>
                </div>
                <div class="card-body">
                    <div id="iso-build-interrupted-banner" style="display:none;background:var(--bg-warning,#fff3cd);border:1px solid var(--border-warning,#ffc107);border-radius:4px;padding:12px 14px;margin-bottom:14px;font-size:13px">
                        <strong>Build state not available</strong> — the build may have been interrupted by a server restart.
                        Check the build log for details, or delete this image and retry.
                        <div style="margin-top:8px;display:flex;gap:8px;align-items:center">
                            <a class="btn btn-secondary btn-sm" href="${escHtml(API.buildProgress.buildLogUrl(img.id))}" target="_blank" rel="noreferrer">View Build Log</a>
                            <button class="btn btn-danger btn-sm" onclick="Pages._deleteAndRetryBuild('${escHtml(img.id)}', ${JSON.stringify(img.source_url || '')})">Delete and Retry</button>
                        </div>
                    </div>
                    <div id="iso-build-phase" style="font-size:13px;margin-bottom:12px;color:var(--text-secondary)">
                        Phase: <span id="iso-build-phase-value" style="font-weight:600;color:var(--text-primary)">Connecting…</span>
                    </div>
                    <div class="progress-bar-wrap" style="margin-bottom:6px">
                        <div class="progress-bar-fill" id="iso-build-bar" style="width:5%;transition:width 0.4s ease"></div>
                    </div>
                    <div id="iso-build-bytes" style="font-size:11px;color:var(--text-secondary);margin-bottom:10px;min-height:16px"></div>
                    <div style="display:flex;gap:24px;font-size:12px;color:var(--text-secondary);margin-bottom:16px">
                        <span>Elapsed: <span id="iso-build-elapsed" class="text-mono">—</span></span>
                    </div>
                    <div style="font-size:11px;font-weight:600;color:var(--text-secondary);margin-bottom:6px;text-transform:uppercase;letter-spacing:0.5px">Serial console</div>
                    <div id="iso-serial-log" class="iso-serial-log log-viewer" style="height:240px;overflow-y:auto;font-family:var(--font-mono);font-size:11px;background:var(--bg-tertiary);border-radius:4px;padding:8px 10px;white-space:pre-wrap;word-break:break-all"></div>
                    <div style="margin-top:12px;display:flex;gap:8px;align-items:center">
                        <button class="btn btn-danger btn-sm" id="iso-cancel-btn" onclick="Pages._cancelIsoBuild('${escHtml(img.id)}')">Cancel Build</button>
                        <a class="btn btn-secondary btn-sm" href="${escHtml(API.buildProgress.buildLogUrl(img.id))}" target="_blank" rel="noreferrer">Download Full Log</a>
                    </div>
                </div>
            </div>`;
    },

    // _startIsoBuildSSE opens an SSE connection for an ISO build and wires all
    // UI updates. Call this once after the page HTML is rendered.
    _startIsoBuildSSE(imageId) {
        const sseUrl = API.buildProgress.sseUrl(imageId);
        const es = new EventSource(sseUrl);
        let userScrolled = false;
        let _elapsedTimer = null;

        const serialEl  = document.getElementById('iso-serial-log');
        const phaseEl   = document.getElementById('iso-build-phase-value');
        const barEl     = document.getElementById('iso-build-bar');
        const bytesEl   = document.getElementById('iso-build-bytes');
        const elapsedEl = document.getElementById('iso-build-elapsed');
        const badgeEl   = document.getElementById('iso-build-badge');

        if (serialEl) {
            serialEl.addEventListener('scroll', () => {
                const atBottom = serialEl.scrollHeight - serialEl.scrollTop - serialEl.clientHeight < 40;
                userScrolled = !atBottom;
            });
        }

        const _appendSerial = (line) => {
            if (!serialEl) return;
            const div = document.createElement('div');
            div.className = Pages._serialLineClass(line);
            div.textContent = line;
            serialEl.appendChild(div);
            // Trim to 500 visible lines to avoid runaway DOM growth.
            while (serialEl.children.length > 500) serialEl.removeChild(serialEl.firstChild);
            if (!userScrolled) serialEl.scrollTop = serialEl.scrollHeight;
        };

        const _applyPhase = (phase, elapsedMs) => {
            if (!phase) return;
            if (phaseEl) phaseEl.textContent = Pages._phaseLabel(phase);
            if (badgeEl) {
                badgeEl.className = 'badge ' + Pages._phaseBadgeClass(phase);
                badgeEl.textContent = phase.replace(/_/g, ' ');
            }
            if (barEl) {
                const pct = Pages._phasePercent(phase);
                barEl.style.width = pct + '%';
            }
            if (phase === 'complete' || phase === 'failed' || phase === 'canceled') {
                clearInterval(_elapsedTimer);
                es.close();
                setTimeout(() => Pages.imageDetail(imageId), 1800);
            }
        };

        const _applyProgress = (done, total) => {
            if (!barEl) return;
            if (total > 0) {
                const pct = Math.min(100, Math.round((done / total) * 100));
                if (bytesEl) bytesEl.textContent = `${fmtBytes(done)} / ${fmtBytes(total)}`;
                barEl.style.width = pct + '%';
            } else if (done > 0) {
                if (bytesEl) bytesEl.textContent = fmtBytes(done);
            }
        };

        const _startElapsedTimer = (startedAt) => {
            clearInterval(_elapsedTimer);
            // Derive the base time from the server-provided wall-clock timestamp so
            // the elapsed display is correct across page reloads and reconnects.
            const base = startedAt ? new Date(startedAt).getTime() : Date.now();
            _elapsedTimer = setInterval(() => {
                const secs = Math.floor((Date.now() - base) / 1000);
                if (elapsedEl) elapsedEl.textContent = fmtETA(secs);
            }, 1000);
        };

        // Initial snapshot event (sent immediately on connect by the server).
        es.addEventListener('snapshot', (e) => {
            try {
                const state = JSON.parse(e.data);
                _startElapsedTimer(state.started_at);
                _applyPhase(state.phase, state.elapsed_ms);
                _applyProgress(state.bytes_done, state.bytes_total);
                if (serialEl && Array.isArray(state.serial_tail)) {
                    serialEl.innerHTML = '';
                    state.serial_tail.forEach(_appendSerial);
                    if (!userScrolled) serialEl.scrollTop = serialEl.scrollHeight;
                }
            } catch (_) {}
        });

        // Incremental update events.
        es.onmessage = (e) => {
            try {
                const ev = JSON.parse(e.data);
                if (ev.phase)       _applyPhase(ev.phase, ev.elapsed_ms);
                if (ev.bytes_done)  _applyProgress(ev.bytes_done, ev.bytes_total);
                if (ev.serial_line) _appendSerial(ev.serial_line);
                if (ev.elapsed_ms && elapsedEl) {
                    elapsedEl.textContent = fmtETA(Math.floor(ev.elapsed_ms / 1000));
                }
            } catch (_) {}
        };

        // Track consecutive SSE errors to detect a dead build-progress endpoint.
        // EventSource auto-reconnects; after 3 failed attempts with no snapshot
        // received we surface the "interrupted" banner rather than spinning forever.
        let _sseErrorCount = 0;
        let _snapshotReceived = false;
        es.addEventListener('snapshot', () => { _snapshotReceived = true; _sseErrorCount = 0; });

        es.onerror = () => {
            clearInterval(_elapsedTimer);
            if (!_snapshotReceived) {
                _sseErrorCount++;
                if (_sseErrorCount >= 3) {
                    // The build-progress endpoint returned 404 or is unreachable.
                    // The image is still in "building" state but has no live goroutine.
                    es.close();
                    const banner = document.getElementById('iso-build-interrupted-banner');
                    const phaseDiv = document.getElementById('iso-build-phase');
                    if (banner) banner.style.display = '';
                    if (phaseDiv) phaseDiv.style.display = 'none';
                    const badgeEl2 = document.getElementById('iso-build-badge');
                    if (badgeEl2) {
                        badgeEl2.className = 'badge badge-error';
                        badgeEl2.textContent = 'interrupted';
                    }
                }
            }
        };

        Pages._isoBuildSSE = es;
        Pages._isoBuildElapsedTimer = _elapsedTimer;
    },

    _serialLineClass(line) {
        if (/kernel panic|BUG:|OOPS:|call trace/i.test(line)) return 'serial-line serial-panic';
        if (/\[\s*OK\s*\]/.test(line))                         return 'serial-line serial-ok';
        if (/warning|warn/i.test(line))                        return 'serial-line serial-warn';
        if (/error|fail|failed/i.test(line))                   return 'serial-line serial-error';
        return 'serial-line';
    },

    _phaseLabel(phase) {
        const labels = {
            downloading_iso:   'Downloading ISO',
            generating_config: 'Generating config',
            creating_disk:     'Creating disk',
            launching_vm:      'Launching VM',
            installing:        'Installing OS',
            extracting:        'Extracting rootfs',
            scrubbing:         'Scrubbing identity',
            finalizing:        'Finalizing',
            complete:          'Complete',
            failed:            'Failed',
            canceled:          'Canceled',
        };
        return labels[phase] || phase;
    },

    _phaseBadgeClass(phase) {
        if (phase === 'complete') return 'badge-success';
        if (phase === 'failed' || phase === 'canceled') return 'badge-error';
        return 'badge-building';
    },

    _phasePercent(phase) {
        const pcts = {
            downloading_iso:   10, generating_config: 20, creating_disk: 25,
            launching_vm: 30, installing: 60, extracting: 80,
            scrubbing: 90, finalizing: 95, complete: 100, failed: 100, canceled: 100,
        };
        return pcts[phase] || 5;
    },

    // _updateIsoBuildProgress is kept as a no-op; ISO builds now use SSE.
    _updateIsoBuildProgress() {},

    async _cancelIsoBuild(imageId) {
        if (!confirm('Cancel this build? The in-progress VM will be stopped and the image will remain in error state.')) return;
        try {
            if (Pages._isoBuildSSE) { Pages._isoBuildSSE.close(); Pages._isoBuildSSE = null; }
            clearInterval(Pages._isoBuildElapsedTimer);
            await API.images.delete(imageId);
            Router.navigate('/images');
        } catch (e) {
            alert('Cancel failed: ' + e.message);
        }
    },

    // _deleteAndRetryBuild deletes an interrupted image and reopens the Build from
    // ISO modal prefilled with the original source URL so the admin can retry
    // without having to retype anything.
    async _deleteAndRetryBuild(imageId, sourceUrl) {
        if (!confirm('Delete this image and open the Build from ISO modal to retry?')) return;
        try {
            if (Pages._isoBuildSSE) { Pages._isoBuildSSE.close(); Pages._isoBuildSSE = null; }
            clearInterval(Pages._isoBuildElapsedTimer);
            await API.images.delete(imageId);
            Router.navigate('/images');
            // Brief delay so the images page renders before opening the modal.
            setTimeout(() => Pages.showBuildFromISOModal(sourceUrl), 300);
        } catch (e) {
            alert('Delete failed: ' + e.message);
        }
    },

    // ── Role mismatch warning ───────────────────────────────────────────��──

    // _checkRoleMismatch is called when the admin changes the image selection on the
    // node edit modal. It reads the image's built_for_roles and compares against
    // the node's groups to surface a warning when there is a mismatch.
    _checkRoleMismatch(imageId, node, images) {
        const warnEl = document.getElementById('role-mismatch-warning');
        if (!warnEl) return;

        if (!imageId) { warnEl.style.display = 'none'; return; }

        const img = (images || []).find(i => i.id === imageId);
        if (!img) { warnEl.style.display = 'none'; return; }

        const builtFor = img.built_for_roles || [];
        if (!builtFor.length) { warnEl.style.display = 'none'; return; }

        // Compare the node's groups array against the image's built_for_roles.
        // A mismatch is when the node has a group that looks like a role ID
        // (compute, gpu-compute, storage, etc.) but it's not in the image's roles.
        const roleKeywords = ['compute', 'gpu-compute', 'gpu', 'storage', 'head-node', 'management', 'minimal'];
        const nodeRoles = (node.groups || []).filter(g => roleKeywords.some(k => g.toLowerCase().includes(k)));
        const mismatched = nodeRoles.filter(g => !builtFor.some(r => g.toLowerCase().includes(r) || r.toLowerCase().includes(g)));

        if (mismatched.length) {
            const nodeRoleStr  = mismatched.join(', ');
            const imageRoleStr = builtFor.join(', ');
            warnEl.innerHTML = `
                <div style="display:flex;gap:10px;align-items:flex-start">
                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24" style="width:18px;height:18px;flex-shrink:0;margin-top:2px;color:var(--warning)">
                        <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/>
                        <line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>
                    </svg>
                    <div>
                        <div style="font-weight:600;margin-bottom:2px">Role mismatch</div>
                        <div style="font-size:12px">This node has group <strong>${escHtml(nodeRoleStr)}</strong> but the image
                        <strong>${escHtml(img.name)}</strong> was built for <strong>${escHtml(imageRoleStr)}</strong>.
                        Required packages may not be present on the deployed node.</div>
                    </div>
                </div>`;
            warnEl.style.display = '';
        } else {
            warnEl.style.display = 'none';
        }
    },

    // ── Logs ───────────────────────────────────────────────────────────────

    async logs() {
        App.render(`
            <div class="page-header">
                <div>
                    <div class="page-title">Logs</div>
                    <div class="page-subtitle">Server-wide log stream with filters</div>
                </div>
            </div>

            <div class="log-filter-bar">
                <input id="lf-mac"       type="text"   placeholder="MAC address"   style="width:155px">
                <input id="lf-hostname"  type="text"   placeholder="Hostname"      style="width:130px">
                <select id="lf-level" style="width:130px">
                    <option value="">All levels</option>
                    <option value="debug">debug</option>
                    <option value="info">info</option>
                    <option value="warn">warn</option>
                    <option value="error">error</option>
                </select>
                <select id="lf-component" style="width:145px">
                    <option value="">All components</option>
                    <option value="hardware">hardware</option>
                    <option value="deploy">deploy</option>
                    <option value="chroot">chroot</option>
                    <option value="ipmi">ipmi</option>
                    <option value="efiboot">efiboot</option>
                    <option value="network">network</option>
                    <option value="rsync">rsync</option>
                    <option value="raid">raid</option>
                </select>
                <input id="lf-since" type="datetime-local" title="Since (local time)" style="width:185px">
                <button class="btn btn-secondary btn-sm" onclick="Pages.loadLogs()">Query</button>
                <button class="btn btn-secondary btn-sm" onclick="Pages.clearLogs()">Clear</button>
                <div class="follow-toggle-wrap">
                    <label class="toggle">
                        <input type="checkbox" id="follow-toggle" onchange="Pages.toggleFollow(this.checked)">
                        Live
                    </label>
                    <span class="follow-indicator" id="follow-ind">
                        <span class="follow-dot"></span>static
                    </span>
                </div>
            </div>

            <div id="log-viewer" class="log-viewer tall"></div>
        `);

        await Pages.loadLogs();
    },

    _logFilters() {
        const mac       = (document.getElementById('lf-mac')       || {}).value || '';
        const hostname  = (document.getElementById('lf-hostname')  || {}).value || '';
        const level     = (document.getElementById('lf-level')     || {}).value || '';
        const component = (document.getElementById('lf-component') || {}).value || '';
        const sinceEl   = document.getElementById('lf-since');
        let since = '';
        if (sinceEl && sinceEl.value) {
            since = new Date(sinceEl.value).toISOString();
        }
        return { mac, hostname, level, component, since, limit: '500' };
    },

    async loadLogs() {
        const viewer = document.getElementById('log-viewer');
        if (!viewer) return;

        const followToggle = document.getElementById('follow-toggle');
        if (App._logStream && followToggle && followToggle.checked) {
            App._logStream.setFilters(this._logFilters());
            return;
        }

        try {
            const params  = this._logFilters();
            const resp    = await API.logs.query(params);
            const entries = resp.logs || [];

            if (!App._logStream) {
                App._logStream = new LogStream(viewer);
            }
            App._logStream.loadEntries(entries);

            if (!entries.length) {
                viewer.innerHTML = `<div class="empty-state" style="padding:30px">
                    <div class="empty-state-text">No log entries match your filters</div>
                </div>`;
            }
        } catch (e) {
            if (viewer) viewer.innerHTML = `<div style="padding:12px;color:var(--error);font-size:12px;font-family:var(--font-mono)">Error: ${escHtml(e.message)}</div>`;
        }
    },

    clearLogs() {
        if (App._logStream) App._logStream.clear();
    },

    toggleFollow(enabled) {
        const viewer = document.getElementById('log-viewer');
        const ind    = document.getElementById('follow-ind');
        if (!viewer) return;

        if (enabled) {
            if (!App._logStream) App._logStream = new LogStream(viewer);
            App._logStream.setFilters(this._logFilters());
            App._logStream.setAutoScroll(true);
            App._logStream.onConnect(() => {
                if (ind) { ind.className = 'follow-indicator live'; ind.innerHTML = '<span class="follow-dot"></span>Live'; }
            });
            App._logStream.onDisconnect(() => {
                if (ind) { ind.className = 'follow-indicator'; ind.innerHTML = '<span class="follow-dot"></span>Reconnecting…'; }
            });
            App._logStream.connect();
        } else {
            if (App._logStream) {
                App._logStream.disconnect();
                if (ind) { ind.className = 'follow-indicator'; ind.innerHTML = '<span class="follow-dot"></span>static'; }
            }
        }
    },

    // ── Settings ───────────────────────────────────────────────────────────

    _settingsTab: 'api-keys', // tracks active tab

    async settings() {
        App.render(loading('Loading settings…'));
        await Pages._settingsRender(Pages._settingsTab);
    },

    async _settingsRender(tab) {
        Pages._settingsTab = tab;
        const tabs = ['api-keys', 'server-info', 'about'];
        const tabBar = tabs.map(t => {
            const active = t === tab ? 'style="border-bottom:2px solid var(--accent);color:var(--accent);"' : '';
            const label  = { 'api-keys': 'API Keys', 'server-info': 'Server Info', 'about': 'About' }[t];
            return `<button class="btn btn-ghost" ${active} onclick="Pages._settingsRender('${t}')">${label}</button>`;
        }).join('');

        let body = loading('Loading…');
        if (tab === 'api-keys') {
            body = await Pages._settingsAPIKeysTab();
        } else if (tab === 'server-info') {
            body = `<div class="card"><div class="card-header"><span class="card-title">Server Info</span></div><p class="text-secondary" style="padding:16px">Server information will appear here in a future update.</p></div>`;
        } else {
            body = `<div class="card"><div class="card-header"><span class="card-title">About clonr</span></div><p class="text-secondary" style="padding:16px">clonr — open-source node cloning and image management for HPC clusters.</p></div>`;
        }

        App.render(`
            <div class="page-header">
                <div>
                    <div class="page-title">Settings</div>
                    <div class="page-subtitle">Server and API key management</div>
                </div>
            </div>
            <div style="display:flex;gap:8px;margin-bottom:20px;border-bottom:1px solid var(--border);padding-bottom:0;">
                ${tabBar}
            </div>
            ${body}
        `);
    },

    async _settingsAPIKeysTab() {
        try {
            const resp = await API.apiKeys.list();
            const keys = (resp && resp.api_keys) ? resp.api_keys : [];

            const rows = keys.length === 0
                ? `<tr><td colspan="7" style="text-align:center;color:var(--text-secondary);padding:24px">No active API keys</td></tr>`
                : keys.map(k => {
                    const expires = k.expires_at ? fmtDate(k.expires_at) : '<span class="text-dim">never</span>';
                    const lastUsed = k.last_used_at ? fmtRelative(k.last_used_at) : '<span class="text-dim">never</span>';
                    const label = k.label || '<span class="text-dim">—</span>';
                    const scopeBadge = k.scope === 'admin'
                        ? `<span class="badge badge-info">admin</span>`
                        : `<span class="badge badge-neutral">node</span>`;
                    return `<tr>
                        <td class="text-mono text-sm">${escHtml(k.hash_prefix)}…</td>
                        <td>${scopeBadge}</td>
                        <td>${escHtml(k.label || '—')}</td>
                        <td class="text-sm text-secondary">${lastUsed}</td>
                        <td class="text-sm text-secondary">${expires}</td>
                        <td class="text-sm text-secondary">${escHtml(k.created_by || '—')}</td>
                        <td>
                            <div style="display:flex;gap:6px;">
                                <button class="btn btn-secondary btn-sm" onclick="Pages._settingsRotateKey('${k.id}')">Rotate</button>
                                <button class="btn btn-danger btn-sm" onclick="Pages._settingsRevokeKey('${k.id}', '${escHtml(k.label || k.hash_prefix)}')">Revoke</button>
                            </div>
                        </td>
                    </tr>`;
                }).join('');

            return `
                <div class="card">
                    <div class="card-header">
                        <span class="card-title">API Keys</span>
                        <button class="btn btn-primary btn-sm" onclick="Pages._settingsCreateKeyModal()">+ Create Key</button>
                    </div>
                    <table class="table">
                        <thead>
                            <tr>
                                <th>Hash Prefix</th><th>Scope</th><th>Label</th>
                                <th>Last Used</th><th>Expires</th><th>Created By</th><th>Actions</th>
                            </tr>
                        </thead>
                        <tbody>${rows}</tbody>
                    </table>
                </div>`;
        } catch (err) {
            return alertBox('Failed to load API keys: ' + err.message);
        }
    },

    _settingsCreateKeyModal() {
        const modal = document.createElement('div');
        modal.id = 'create-key-modal';
        modal.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.5);display:flex;align-items:center;justify-content:center;z-index:1000;';
        modal.innerHTML = `
            <div class="card" style="width:480px;max-width:95vw;">
                <div class="card-header">
                    <span class="card-title">Create API Key</span>
                    <button class="btn btn-ghost btn-sm" onclick="document.getElementById('create-key-modal').remove()">×</button>
                </div>
                <div style="padding:16px;display:flex;flex-direction:column;gap:12px;">
                    <label class="form-label">Scope
                        <select id="ckm-scope" class="form-input" style="margin-top:4px;">
                            <option value="admin">admin — full access</option>
                            <option value="node">node — deploy agent only</option>
                        </select>
                    </label>
                    <label class="form-label">Label (e.g. "ci-runner", "robert-laptop")
                        <input id="ckm-label" class="form-input" type="text" placeholder="ci-runner" style="margin-top:4px;">
                    </label>
                    <label class="form-label" id="ckm-nodeid-row" style="display:none;">Node ID (required for node scope)
                        <input id="ckm-nodeid" class="form-input" type="text" placeholder="node UUID" style="margin-top:4px;">
                    </label>
                    <label class="form-label">Expires (ISO8601, optional)
                        <input id="ckm-expires" class="form-input" type="datetime-local" style="margin-top:4px;">
                    </label>
                    <div style="display:flex;gap:8px;justify-content:flex-end;margin-top:8px;">
                        <button class="btn btn-secondary" onclick="document.getElementById('create-key-modal').remove()">Cancel</button>
                        <button class="btn btn-primary" onclick="Pages._settingsCreateKeySubmit()">Create</button>
                    </div>
                </div>
            </div>`;
        document.body.appendChild(modal);

        document.getElementById('ckm-scope').addEventListener('change', (e) => {
            document.getElementById('ckm-nodeid-row').style.display = e.target.value === 'node' ? '' : 'none';
        });
    },

    async _settingsCreateKeySubmit() {
        const scope    = document.getElementById('ckm-scope').value;
        const label    = document.getElementById('ckm-label').value.trim();
        const nodeID   = document.getElementById('ckm-nodeid').value.trim();
        const expiresV = document.getElementById('ckm-expires').value;

        if (scope === 'node' && !nodeID) {
            App.toast('Node ID is required for node-scoped keys', 'error');
            return;
        }

        let expiresAt = '';
        if (expiresV) {
            expiresAt = new Date(expiresV).toISOString();
        }

        try {
            const resp = await API.apiKeys.create({ scope, label, node_id: nodeID, expires_at: expiresAt });
            document.getElementById('create-key-modal').remove();
            Pages._settingsShowRawKey(resp.key, 'New API Key Created');
        } catch (err) {
            App.toast('Create failed: ' + err.message, 'error');
        }
    },

    async _settingsRotateKey(id) {
        if (!confirm('Rotate this key? The old key will stop working immediately.')) return;
        try {
            const resp = await API.apiKeys.rotate(id);
            Pages._settingsShowRawKey(resp.key, 'Key Rotated');
        } catch (err) {
            App.toast('Rotate failed: ' + err.message, 'error');
        }
    },

    async _settingsRevokeKey(id, label) {
        if (!confirm(`Revoke key "${label}"? This cannot be undone.`)) return;
        try {
            await API.apiKeys.revoke(id);
            App.toast('Key revoked', 'success');
            Pages._settingsRender('api-keys');
        } catch (err) {
            App.toast('Revoke failed: ' + err.message, 'error');
        }
    },

    _settingsShowRawKey(rawKey, title) {
        const modal = document.createElement('div');
        modal.id = 'rawkey-modal';
        modal.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);display:flex;align-items:center;justify-content:center;z-index:1001;';
        modal.innerHTML = `
            <div class="card" style="width:560px;max-width:95vw;">
                <div class="card-header">
                    <span class="card-title">${escHtml(title)}</span>
                </div>
                <div style="padding:16px;">
                    <div class="alert alert-warning" style="margin-bottom:12px;">
                        <strong>Save this key now.</strong> It will not be shown again.
                    </div>
                    <div style="background:var(--bg-primary);border:1px solid var(--border);border-radius:6px;padding:12px;font-family:var(--font-mono);font-size:13px;word-break:break-all;margin-bottom:12px;">
                        ${escHtml(rawKey)}
                    </div>
                    <div style="display:flex;gap:8px;justify-content:flex-end;">
                        <button class="btn btn-secondary" onclick="navigator.clipboard.writeText(${JSON.stringify(rawKey)}).then(()=>App.toast('Copied','success'))">Copy to clipboard</button>
                        <button class="btn btn-primary" onclick="document.getElementById('rawkey-modal').remove();Pages._settingsRender('api-keys')">Done</button>
                    </div>
                </div>
            </div>`;
        document.body.appendChild(modal);
    },
};

// ─── ProgressStream ───────────────────────────────────────────────────────
//
// Subscribes to /api/v1/deploy/progress/stream (SSE) and updates the shared
// deployMap (MAC → DeployProgress) on each event. Calls onUpdate() after each
// update so the caller can re-render only the affected part of the DOM.
//
// Completed or failed entries are removed from the map after 60 seconds so they
// don't accumulate in the table indefinitely.

class ProgressStream {
    constructor(deployMap, onUpdate) {
        this._map       = deployMap;    // Map<mac, DeployProgress>
        this._onUpdate  = onUpdate;     // () => void
        this._es        = null;
        this._timers    = new Map();    // mac → setTimeout handle
        this._stopped   = false;
    }

    connect() {
        if (this._stopped) return;
        const url = API.progress.sseUrl();
        this._es = new EventSource(url);

        this._es.onmessage = (e) => {
            let prog;
            try { prog = JSON.parse(e.data); } catch { return; }
            if (!prog || !prog.node_mac) return;

            const mac = prog.node_mac;
            this._map.set(mac, prog);

            // Cancel any pending removal for this node (phase may have changed).
            if (this._timers.has(mac)) {
                clearTimeout(this._timers.get(mac));
                this._timers.delete(mac);
            }

            // Schedule removal 60 seconds after the final state.
            if (prog.phase === 'complete' || prog.phase === 'error') {
                const t = setTimeout(() => {
                    this._map.delete(mac);
                    this._timers.delete(mac);
                    if (this._onUpdate) this._onUpdate();
                }, 60000);
                this._timers.set(mac, t);
            }

            if (this._onUpdate) this._onUpdate();
        };

        this._es.onerror = () => {
            if (this._stopped) return;
            // EventSource will automatically attempt to reconnect — no action needed.
        };
    }

    disconnect() {
        this._stopped = true;
        if (this._es) { this._es.close(); this._es = null; }
        this._timers.forEach(t => clearTimeout(t));
        this._timers.clear();
    }
}

// ─── Auth ─────────────────────────────────────────────────────────────────
//
// Auth manages the browser session (ADR-0006).
// The session is carried by an HttpOnly cookie set by POST /api/v1/auth/login.
// On 401/403 api.js redirects to /login — no modal needed.
// Auth.logout() calls POST /api/v1/auth/logout then redirects to /login.

const Auth = {
    async logout() {
        try {
            await fetch('/api/v1/auth/logout', {
                method: 'POST',
                credentials: 'same-origin',
            });
        } catch (_) {
            // Best-effort; redirect regardless.
        }
        try { localStorage.removeItem('clonr_admin_key'); } catch (_) {}
        window.location.href = '/login';
    },

    // extendSession re-validates the session (GET /auth/me with valid cookie slides it).
    async extendSession() {
        try {
            await fetch('/api/v1/auth/me', { credentials: 'same-origin' });
            const banner = document.getElementById('session-expiry-banner');
            if (banner) banner.style.display = 'none';
            App.toast('Session extended', 'success');
        } catch (_) {
            window.location.href = '/login';
        }
    },

    // boot verifies the session via GET /api/v1/auth/me.
    // Valid session → start the app.
    // No session / expired → redirect to /login.
    async boot() {
        try {
            const resp = await fetch('/api/v1/auth/me', {
                credentials: 'same-origin',
            });
            if (!resp.ok) {
                window.location.href = '/login';
                return;
            }
        } catch (_) {
            // Network error — still try to start the app; api.js will redirect on 401.
        }
        App.init();
    },
};

// ─── Boot ─────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => Auth.boot());
