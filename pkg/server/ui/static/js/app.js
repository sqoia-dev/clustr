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
            if (parts.length === 3 && parts[2]) Pages.nodeDetail(parts[2]);
            else Pages.nodes();
        });
        Router.register('/nodes/*', (h)   => {
            const parts = h.split('/');
            Pages.nodeDetail(parts[2]);
        });
        Router.register('/logs',    ()    => Pages.logs());
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
        check();
        setInterval(check, 30000);
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
            const resp   = await API.images.list();
            const images = resp.images || [];

            App.render(`
                <div class="page-header">
                    <div>
                        <div class="page-title">Images</div>
                        <div class="page-subtitle">${images.length} image${images.length !== 1 ? 's' : ''} total</div>
                    </div>
                    <div class="flex gap-8">
                        <button class="btn btn-secondary" onclick="Pages.showImportISOModal()">Import ISO</button>
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

                ${images.length === 0
                    ? `<div class="card"><div class="card-body">${emptyState(
                        'No images yet',
                        'Pull your first image to get started.',
                        `<button class="btn btn-primary" onclick="Pages.showPullModal()">Pull Image</button>`
                    )}</div></div>`
                    : `<div class="image-grid">${images.map(img => this._imageCard(img)).join('')}</div>`
                }
            `);

            const hasBuilding = images.some(i => i.status === 'building');
            App.setAutoRefresh(() => Pages.images(), hasBuilding ? 5000 : 30000);

        } catch (e) {
            App.render(alertBox(`Failed to load images: ${e.message}`));
        }
    },

    _imageCard(img) {
        const statusClass = {
            ready:    'badge-ready',
            building: 'badge-building',
            error:    'badge-error',
            archived: 'badge-archived',
        }[img.status] || 'badge-archived';

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
        </div>`;
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
        const urlInput = document.getElementById('pull-url');
        const isoHint  = document.getElementById('pull-iso-hint');
        const fmtGroup  = document.getElementById('pull-format-group');
        const diskGroup = document.getElementById('pull-disk-group');
        const memGroup  = document.getElementById('pull-mem-group');
        const cpuGroup  = document.getElementById('pull-cpu-group');
        const ksGroup   = document.getElementById('pull-ks-group');
        const pullBtn   = document.getElementById('pull-btn');

        const applyISOMode = (isISO) => {
            isoHint.style.display  = isISO ? 'block' : 'none';
            fmtGroup.style.display  = isISO ? 'none'  : '';
            diskGroup.style.display = isISO ? ''      : 'none';
            memGroup.style.display  = isISO ? ''      : 'none';
            cpuGroup.style.display  = isISO ? ''      : 'none';
            if (ksGroup) ksGroup.style.display = isISO ? '' : 'none';
            pullBtn.textContent = isISO ? 'Build from ISO' : 'Pull Image';
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
                const body = {
                    url:              url,
                    name:             data.get('name'),
                    version:          data.get('version') || undefined,
                    os:               data.get('os')      || undefined,
                    arch:             data.get('arch')    || undefined,
                    disk_size_gb:     parseInt(data.get('disk_size_gb'), 10) || 0,
                    memory_mb:        parseInt(data.get('memory_mb'),    10) || 0,
                    cpus:             parseInt(data.get('cpus'),         10) || 0,
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
                ${img.status === 'building' ? `<div class="alert alert-info" style="margin-bottom:16px">Build in progress — auto-refreshing every 5 seconds.</div>` : ''}

                ${cardWrap('Image Details', `
                    <div class="card-body">
                        <div class="kv-grid">
                            <div class="kv-item"><div class="kv-key">ID</div><div class="kv-value">${escHtml(img.id)}</div></div>
                            <div class="kv-item"><div class="kv-key">Name</div><div class="kv-value">${escHtml(img.name)}</div></div>
                            <div class="kv-item"><div class="kv-key">Version</div><div class="kv-value">${escHtml(img.version || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">OS</div><div class="kv-value">${escHtml(img.os || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Arch</div><div class="kv-value">${escHtml(img.arch || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Format</div><div class="kv-value">${escHtml(img.format || '—')}</div></div>
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
                App.setAutoRefresh(() => Pages.imageDetail(id), 5000);
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
                    <button class="btn btn-primary" onclick='Pages.showNodeModal(null, ${JSON.stringify(JSON.stringify(images))})'>
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                            <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
                        </svg>
                        Add Node
                    </button>
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

    async nodeDetail(id) {
        App.render(loading('Loading node…'));
        try {
            const [node, imagesResp] = await Promise.all([
                API.nodes.get(id),
                API.images.list(),
            ]);
            const images = imagesResp.images || [];
            const img    = images.find(i => i.id === node.base_image_id);

            let hw = null;
            try {
                if (node.hardware_profile) {
                    hw = typeof node.hardware_profile === 'string'
                        ? JSON.parse(node.hardware_profile)
                        : node.hardware_profile;
                }
            } catch (_) {}

            const displayName = node.hostname || node.primary_mac;

            App.render(`
                <div class="breadcrumb">
                    <a href="#/nodes">Nodes</a>
                    <span class="breadcrumb-sep">/</span>
                    <span>${escHtml(displayName)}</span>
                </div>
                <div class="page-header">
                    <div style="display:flex;align-items:center;gap:12px">
                        <button class="detail-back-btn" onclick="Router.navigate('/nodes')">
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
                        ${(() => {
                            // Show "Capture this node" when the node has a reachable IP configured.
                            const iface = (node.interfaces || []).find(i => i.ip_address);
                            if (!iface) return '';
                            const ip = iface.ip_address.split('/')[0]; // strip CIDR suffix
                            const prefillHost = 'root@' + ip;
                            const prefillName = (node.hostname && node.hostname !== '(none)')
                                ? node.hostname.toLowerCase().replace(/[^a-z0-9-]/g, '-') + '-capture'
                                : '';
                            return '<button class="btn btn-secondary" onclick="Pages.showCaptureModal(' +
                                JSON.stringify(prefillHost) + ',' + JSON.stringify(prefillName) + ')">Capture this node</button>';
                        })()}
                        <button class="btn btn-secondary" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(node))}, ${JSON.stringify(JSON.stringify(images))})'>Edit</button>
                        <button class="btn btn-danger btn-sm" onclick="Pages.deleteNodeAndGoBack('${node.id}', '${escHtml(displayName)}')">Delete</button>
                    </div>
                </div>

                <div class="tab-bar">
                    <div class="tab active" onclick="Pages._switchTab(this, 'tab-overview')">Overview</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-hardware')">Hardware</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-bmc');Pages._onBMCTabOpen('${node.id}', ${!!(node.bmc || node.power_provider)})">Power / IPMI</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-disklayout');Pages._onDiskLayoutTabOpen('${node.id}')">Disk Layout</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-mounts');Pages._onMountsTabOpen('${node.id}')">Mounts</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-config')">Configuration</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-logs')">Logs</div>
                </div>

                <!-- Overview tab -->
                <div id="tab-overview" class="tab-panel active">
                    ${cardWrap('Node Details', `
                        <div class="card-body">
                            <div class="kv-grid">
                                <div class="kv-item"><div class="kv-key">ID</div><div class="kv-value">${escHtml(node.id)}</div></div>
                                <div class="kv-item"><div class="kv-key">Hostname</div><div class="kv-value">
                                    ${(node.hostname && node.hostname !== '(none)')
                                        ? escHtml(node.hostname) + (node.hostname_auto ? ' <span class="badge badge-neutral badge-sm" title="Auto-generated hostname">auto</span>' : '')
                                        : '<span class="text-dim" style="font-style:italic">Unassigned</span>'}
                                </div></div>
                                <div class="kv-item"><div class="kv-key">FQDN</div><div class="kv-value">${escHtml(node.fqdn || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Primary MAC</div><div class="kv-value">${escHtml(node.primary_mac)}</div></div>
                                <div class="kv-item"><div class="kv-key">Base Image</div><div class="kv-value">
                                    ${img
                                        ? `<a href="#/images/${img.id}">${escHtml(img.name)}</a> ${badge(img.status)}`
                                        : (node.base_image_id ? escHtml(node.base_image_id) : '—')}
                                </div></div>
                                <div class="kv-item"><div class="kv-key">Status</div><div class="kv-value">${nodeBadge(node)}</div></div>
                                <div class="kv-item"><div class="kv-key">Groups</div><div class="kv-value">
                                    ${(node.groups || []).map(g => `<span class="badge badge-neutral">${escHtml(g)}</span>`).join(' ') || '—'}
                                </div></div>
                                <div class="kv-item"><div class="kv-key">Kernel Args</div><div class="kv-value">${escHtml(node.kernel_args || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Created</div><div class="kv-value">${fmtDate(node.created_at)}</div></div>
                                <div class="kv-item"><div class="kv-key">Updated</div><div class="kv-value">${fmtDate(node.updated_at)}</div></div>
                            </div>
                        </div>`)}

                    ${node.interfaces && node.interfaces.length ? cardWrap('Network Interfaces', `
                        <div class="table-wrap"><table>
                            <thead><tr><th>Name</th><th>MAC</th><th>IP (CIDR)</th><th>Gateway</th><th>DNS</th><th>MTU</th></tr></thead>
                            <tbody>
                            ${node.interfaces.map(iface => `<tr>
                                <td class="mono">${escHtml(iface.name || '—')}</td>
                                <td class="mono dim">${escHtml(iface.mac_address || '—')}</td>
                                <td class="mono">${escHtml(iface.ip_address || '—')}</td>
                                <td class="mono dim">${escHtml(iface.gateway || '—')}</td>
                                <td class="mono dim">${(iface.dns || []).join(', ') || '—'}</td>
                                <td class="mono dim">${iface.mtu || '—'}</td>
                            </tr>`).join('')}
                            </tbody>
                        </table></div>`) : ''}
                </div>

                <!-- Hardware tab -->
                <div id="tab-hardware" class="tab-panel">
                    ${hw ? this._hardwareProfile(hw) : `<div class="card"><div class="card-body">${emptyState('No hardware profile', 'Hardware is discovered when a node registers via PXE boot.')}</div></div>`}
                </div>

                <!-- Power / IPMI tab — always rendered; content depends on provider config -->
                <div id="tab-bmc" class="tab-panel">
                    ${node.power_provider && node.power_provider.type ? `
                    ${cardWrap('Power Provider', `
                        <div class="card-body">
                            <div class="kv-grid" style="margin-bottom:12px">
                                <div class="kv-item">
                                    <div class="kv-key">Type</div>
                                    <div class="kv-value">
                                        <span class="badge badge-neutral">${escHtml(node.power_provider.type)}</span>
                                    </div>
                                </div>
                                ${node.power_provider.type === 'proxmox' && node.power_provider.fields ? `
                                <div class="kv-item"><div class="kv-key">API URL</div><div class="kv-value text-mono">${escHtml(node.power_provider.fields.api_url || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">PVE Node</div><div class="kv-value text-mono">${escHtml(node.power_provider.fields.node || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">VM ID</div><div class="kv-value text-mono">${escHtml(node.power_provider.fields.vmid || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Username</div><div class="kv-value text-mono">${escHtml(node.power_provider.fields.username || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Skip TLS</div><div class="kv-value">${node.power_provider.fields.insecure === 'true' ? 'Yes' : 'No'}</div></div>
                                ` : ''}
                            </div>
                            <div class="flex gap-8">
                                <button class="btn btn-secondary btn-sm" onclick="Pages._doFlipToDisk('${node.id}')">
                                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/></svg>
                                    Flip Next Boot → Disk
                                </button>
                                <button class="btn btn-danger btn-sm" onclick="Pages._doFlipToDisk('${node.id}', true)">
                                    <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24" style="width:13px;height:13px"><polyline points="23 4 23 10 17 10"/><path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/></svg>
                                    Flip → Disk + Reboot
                                </button>
                                <button class="btn btn-secondary btn-sm" style="margin-left:auto" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(node))}, ${JSON.stringify(JSON.stringify([]))})'>Edit Provider</button>
                            </div>
                            <div id="power-action-feedback" style="display:none;margin-top:10px" class="alert alert-info"></div>
                        </div>`,
                        ''
                    )}` : ''}

                    ${node.bmc && node.bmc.ip_address ? `
                    ${cardWrap('Power Status',
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
                    )}

                    ${cardWrap('Power Controls',
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
                            </div>
                            <div id="power-action-feedback" style="display:none;margin-top:10px" class="alert alert-info"></div>
                        </div>`,
                        ''
                    )}

                    ${cardWrap('BMC Information',
                        `<div class="card-body">
                            <div class="kv-grid">
                                <div class="kv-item"><div class="kv-key">IP Address</div><div class="kv-value text-mono">${escHtml(node.bmc.ip_address || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Netmask</div><div class="kv-value text-mono">${escHtml(node.bmc.netmask || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Gateway</div><div class="kv-value text-mono">${escHtml(node.bmc.gateway || '—')}</div></div>
                                <div class="kv-item"><div class="kv-key">Username</div><div class="kv-value text-mono">${escHtml(node.bmc.username || '—')}</div></div>
                            </div>
                            <div style="margin-top:12px">
                                <button class="btn btn-secondary btn-sm" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(node))}, ${JSON.stringify(JSON.stringify([]))})'>Edit BMC Config</button>
                            </div>
                        </div>`,
                        ''
                    )}

                    ${cardWrap('Sensor Readings',
                        `<div id="sensor-table-wrap"><div class="loading"><div class="spinner"></div>Loading sensors…</div></div>`,
                        `<button class="btn btn-secondary btn-sm" onclick="Pages._refreshSensors('${node.id}')">Refresh</button>`
                    )}`
                    : (!node.power_provider || !node.power_provider.type ? `<div class="card"><div class="card-body">${emptyState(
                        'No power management configured',
                        'Configure a power provider (Proxmox VE or IPMI/BMC) to enable remote power controls and auto boot-flip after deployment.',
                        `<button class="btn btn-primary btn-sm" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(node))}, ${JSON.stringify(JSON.stringify([]))})'>Configure Power</button>`
                    )}</div></div>` : '')}
                </div>

                <!-- Disk Layout tab -->
                <div id="tab-disklayout" class="tab-panel">
                    <div id="disklayout-content">
                        <div class="loading"><div class="spinner"></div>Loading layout…</div>
                    </div>
                </div>

                <!-- Mounts tab -->
                <div id="tab-mounts" class="tab-panel">
                    <div id="mounts-content">
                        <div class="loading"><div class="spinner"></div>Loading mounts…</div>
                    </div>
                </div>

                <!-- Configuration tab -->
                <div id="tab-config" class="tab-panel">
                    ${node.ssh_keys && node.ssh_keys.length ? cardWrap('SSH Public Keys', `
                        <div class="card-body">
                            ${node.ssh_keys.map((k, i) => `
                                <div style="margin-bottom:${i < node.ssh_keys.length - 1 ? '10px' : '0'}">
                                    <pre class="json-block" style="font-size:11px;user-select:all">${escHtml(k)}</pre>
                                </div>`).join('')}
                        </div>`) : ''}

                    ${Object.keys(node.custom_vars || {}).length ? cardWrap('Custom Variables', `
                        <div class="card-body">
                            <div class="kv-grid">
                            ${Object.entries(node.custom_vars).map(([k, v]) => `
                                <div class="kv-item"><div class="kv-key">${escHtml(k)}</div><div class="kv-value">${escHtml(v)}</div></div>`).join('')}
                            </div>
                        </div>`) : ''}

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

            // Load static logs for the node logs tab when it becomes active.
            document.querySelectorAll('.tab').forEach(tab => {
                tab.addEventListener('click', () => {
                    if (tab.textContent.trim() === 'Logs') {
                        Pages.loadNodeLogs(node.primary_mac);
                    }
                });
            });

            // Kick off initial power status fetch if any power management is configured.
            // This runs immediately so status is ready when the user opens the tab.
            if ((node.bmc && node.bmc.ip_address) || (node.power_provider && node.power_provider.type)) {
                Pages._refreshPowerStatus(node.id);
            }

        } catch (e) {
            App.render(alertBox(`Failed to load node: ${e.message}`));
        }
    },

    _switchTab(tabEl, panelId) {
        // Deactivate all tabs and panels.
        tabEl.closest('.tab-bar').querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
        document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
        tabEl.classList.add('active');
        const panel = document.getElementById(panelId);
        if (panel) panel.classList.add('active');
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
                API.request('GET', `/api/v1/nodes/${nodeId}/effective-layout`),
                API.request('GET', `/api/v1/nodes/${nodeId}/layout-recommendation`),
            ]);
            const effective = effectiveResp.status === 'fulfilled' ? effectiveResp.value : null;
            const rec = recResp.status === 'fulfilled' ? recResp.value : null;
            container.innerHTML = Pages._renderDiskLayoutTab(nodeId, effective, rec);
        } catch (e) {
            container.innerHTML = alertBox(`Failed to load disk layout: ${e.message}`);
        }
    },

    // _onMountsTabOpen fetches the effective-mounts response and renders it.
    async _onMountsTabOpen(nodeId) {
        const container = document.getElementById('mounts-content');
        if (!container) return;
        // Don't reload if already populated.
        if (container.dataset.loaded === nodeId) return;
        container.innerHTML = `<div class="loading"><div class="spinner"></div>Loading mounts…</div>`;
        try {
            const resp = await API.request('GET', `/api/v1/nodes/${nodeId}/effective-mounts`);
            container.innerHTML = Pages._renderEffectiveMountsTab(resp);
            container.dataset.loaded = nodeId;
        } catch (e) {
            container.innerHTML = alertBox(`Failed to load effective mounts: ${e.message}`);
        }
    },

    // _renderEffectiveMountsTab renders the merged fstab entry list for the Mounts tab.
    _renderEffectiveMountsTab(resp) {
        const mounts = (resp && resp.mounts) || [];

        const sourceLabel = (m) => {
            if (m.source === 'group') return `<span class="badge badge-neutral badge-sm" title="Inherited from group ${escHtml(m.group_id||'')}">group</span>`;
            return `<span class="badge badge-info badge-sm">node</span>`;
        };

        const mountsTable = mounts.length === 0
            ? emptyState('No additional mounts configured',
                'Use the Edit button to add shared storage mounts (NFS, Lustre, BeeGFS, CIFS…). They are appended to /etc/fstab during deployment.')
            : `<div class="table-wrap"><table>
                <thead><tr>
                    <th>Source</th>
                    <th>Mount Point</th>
                    <th>FS Type</th>
                    <th>Options</th>
                    <th>Auto-mkdir</th>
                    <th>Dump / Pass</th>
                    <th>Origin</th>
                    <th>Comment</th>
                </tr></thead>
                <tbody>
                ${mounts.map(m => `<tr>
                    <td class="mono">${escHtml(m.source||'—')}</td>
                    <td class="mono">${escHtml(m.mount_point||'—')}</td>
                    <td><span class="badge badge-neutral badge-sm">${escHtml(m.fs_type||'—')}</span></td>
                    <td class="mono dim" style="font-size:11px">${escHtml(m.options||'defaults')}</td>
                    <td style="text-align:center">${m.auto_mkdir ? '✓' : '—'}</td>
                    <td class="mono dim" style="text-align:center">${m.dump||0} / ${m.pass||0}</td>
                    <td>${sourceLabel(m)}</td>
                    <td class="dim" style="font-size:11px">${escHtml(m.comment||'—')}</td>
                </tr>`).join('')}
                </tbody>
            </table></div>`;

        return cardWrap('Effective Mounts',
            `<div class="card-body">
                <p style="margin:0 0 12px;color:var(--text-secondary);font-size:13px">
                    Merged result of group-level and node-level extra mounts.
                    These entries are appended to <code>/etc/fstab</code> after the base partition UUIDs are written.
                    <strong>Node entries override group entries</strong> when the mount point matches.
                </p>
                ${mountsTable}
            </div>`,
            ``
        );
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
                    <button class="btn btn-secondary btn-sm" onclick="Pages._showLayoutOverrideEditor('${nodeId}', ${JSON.stringify(JSON.stringify(effective.layout))})">
                        Edit Override
                    </button>
                    ${effective.source !== 'image' ? `<button class="btn btn-secondary btn-sm" onclick="Pages._clearLayoutOverride('${nodeId}')">Clear Override</button>` : ''}
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
                `<button class="btn btn-primary btn-sm" onclick="Pages._applyRecommendedLayout('${nodeId}', ${JSON.stringify(JSON.stringify(rec.layout))})">Apply Recommended Layout</button>`
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
            await API.request('PUT', `/api/v1/nodes/${nodeId}/layout-override`, { layout });
            Pages._onDiskLayoutTabOpen(nodeId);
        } catch (e) {
            alert(`Failed to apply layout: ${e.message}`);
        }
    },

    async _clearLayoutOverride(nodeId) {
        if (!confirm('Clear the node-level disk layout override? The group or image default will be used instead.')) return;
        try {
            await API.request('PUT', `/api/v1/nodes/${nodeId}/layout-override`, { clear_layout_override: true });
            Pages._onDiskLayoutTabOpen(nodeId);
        } catch (e) {
            alert(`Failed to clear override: ${e.message}`);
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
                        ${['xfs','ext4','vfat','swap','biosboot'].map(fs =>
                            `<option value="${fs}" ${p.filesystem===fs?'selected':''}>${fs}</option>`).join('')}
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
                    ['xfs','ext4','vfat','swap','biosboot'].map(fs =>
                        `<option value="${fs}" ${p.filesystem===fs?'selected':''}>${fs}</option>`).join('')
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
            await API.request('PUT', `/api/v1/nodes/${nodeId}/layout-override`, { layout: modal._layoutState });
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

// ─── Boot ─────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => App.init());
