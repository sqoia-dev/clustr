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

function deployPhase(entry) {
    const msg  = (entry.message || '').toLowerCase();
    const comp = (entry.component || '').toLowerCase();
    if (msg.includes('complete') || msg.includes('success') || msg.includes('finali')) return 'complete';
    if (comp === 'efiboot' || msg.includes('bootloader')) return 'finalizing';
    if (comp === 'chroot' || msg.includes('chroot')) return 'finalizing';
    if (comp === 'deploy' && (msg.includes('extract') || msg.includes('rsync') || msg.includes('dd '))) return 'imaging';
    if (comp === 'deploy' && (msg.includes('partition') || msg.includes('mkfs') || msg.includes('format'))) return 'partitioning';
    if (comp === 'hardware' || msg.includes('discover')) return 'discovering';
    if (msg.includes('preflight') || msg.includes('validat')) return 'preflight';
    if (comp === 'deploy' || msg.includes('deploy')) return 'imaging';
    if (entry.level === 'error') return 'error';
    return 'in-progress';
}

const PHASE_ORDER = ['discovering', 'preflight', 'partitioning', 'imaging', 'finalizing', 'complete'];

const PHASE_BADGE = {
    complete:     'badge-ready',
    imaging:      'badge-info',
    partitioning: 'badge-info',
    finalizing:   'badge-info',
    discovering:  'badge-info',
    preflight:    'badge-info',
    'in-progress':'badge-info',
    error:        'badge-error',
    waiting:      'badge-neutral',
};

function phaseBadge(phase) {
    const cls = PHASE_BADGE[phase] || 'badge-neutral';
    return `<span class="badge ${cls}">${escHtml(phase)}</span>`;
}

function phaseProgress(phase) {
    const idx = PHASE_ORDER.indexOf(phase);
    if (idx < 0) return 0;
    return Math.round(((idx + 1) / PHASE_ORDER.length) * 100);
}

// ─── Pages ────────────────────────────────────────────────────────────────

const Pages = {

    // ── Dashboard ──────────────────────────────────────────────────────────

    async dashboard() {
        App.render(loading('Loading dashboard…'));

        try {
            const [imagesResp, nodesResp, logsResp] = await Promise.all([
                API.images.list(),
                API.nodes.list(),
                API.logs.query({ limit: '500' }),
            ]);

            const images = imagesResp.images || [];
            const nodes  = nodesResp.nodes   || [];
            const logs   = logsResp.logs     || [];

            const ready    = images.filter(i => i.status === 'ready').length;
            const building = images.filter(i => i.status === 'building').length;
            const errored  = images.filter(i => i.status === 'error').length;
            const configured = nodes.filter(n => n.base_image_id).length;

            const deployProgress = this._buildDeployProgress(logs);
            const recentActivity = this._buildRecentActivity(images, nodes, logs);

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
                        <div class="stat-value">${images.length}</div>
                        <div class="stat-sub">
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
                        <div class="stat-value">${nodes.length}</div>
                        <div class="stat-sub">${configured} configured · ${nodes.length - configured} unconfigured</div>
                    </div>
                    <div class="stat-card">
                        <div class="stat-icon stat-icon-amber">
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" viewBox="0 0 24 24">
                                <polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/>
                            </svg>
                        </div>
                        <div class="stat-label">Active Deployments</div>
                        <div class="stat-value">${deployProgress.filter(d => !d.stale && d.phase !== 'complete' && d.phase !== 'error').length}</div>
                        <div class="stat-sub">${deployProgress.filter(d => d.phase === 'complete').length} completed recently</div>
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
                    this._deployProgressTable(deployProgress))}

                <div style="display:grid;grid-template-columns:1fr 1fr;gap:20px;margin-bottom:20px">
                    ${cardWrap('Recent Images',
                        this._imagesTable(images.slice(0, 6)),
                        `<a href="#/images" class="btn btn-secondary btn-sm">View all</a>`)}
                    ${cardWrap('Recent Nodes',
                        this._nodesTable(nodes.slice(0, 6)),
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
                stream.onConnect(() => {
                    const ind = document.getElementById('dash-follow-ind');
                    if (ind) { ind.className = 'follow-indicator live'; ind.innerHTML = '<span class="follow-dot"></span>Live'; }
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

            App.setAutoRefresh(() => Pages.dashboard());

        } catch (e) {
            App.render(alertBox(`Failed to load dashboard: ${e.message}`));
        }
    },

    _buildDeployProgress(logs) {
        const STALE_MS = 30 * 60 * 1000; // 30 minutes
        const now = Date.now();
        const nodeMap = new Map();
        for (const entry of logs) {
            const key = entry.node_mac || entry.hostname || 'unknown';
            if (!nodeMap.has(key)) {
                nodeMap.set(key, {
                    key,
                    mac:      entry.node_mac || '—',
                    hostname: entry.hostname  || '',
                    phase:    'waiting',
                    lastTs:   entry.timestamp,
                    hasError: false,
                });
            }
            const state = nodeMap.get(key);
            const phase = deployPhase(entry);
            if (phase === 'error') {
                state.hasError = true;
            } else if (PHASE_ORDER.indexOf(phase) > PHASE_ORDER.indexOf(state.phase)) {
                state.phase = phase;
            }
            if (new Date(entry.timestamp) > new Date(state.lastTs)) {
                state.lastTs = entry.timestamp;
            }
        }
        return Array.from(nodeMap.values())
            .filter(s => s.phase !== 'waiting' || s.hasError)
            .map(s => {
                const age = now - new Date(s.lastTs).getTime();
                return {
                    ...s,
                    phase: s.hasError && s.phase !== 'complete' ? 'error' : s.phase,
                    stale: age > STALE_MS,
                };
            })
            .sort((a, b) => new Date(b.lastTs) - new Date(a.lastTs))
            .slice(0, 20);
    },

    _deployProgressTable(nodes) {
        const active = nodes.filter(n => !n.stale);
        if (!active.length) return emptyState('No active deployments');
        return `<div class="table-wrap"><table>
            <thead><tr>
                <th>Node</th><th>Phase</th><th>Progress</th><th>Last Activity</th>
            </tr></thead>
            <tbody>
            ${active.map(n => {
                const pct = phaseProgress(n.phase);
                const displayName = fmtHostname(n.hostname, n.mac);
                const barClass = n.phase === 'complete' ? 'complete' : n.phase === 'error' ? 'error' : '';
                return `<tr>
                    <td>${displayName}</td>
                    <td>${phaseBadge(n.phase)}</td>
                    <td>
                        <div style="display:flex;align-items:center;gap:8px">
                            <div class="progress-bar-wrap">
                                <div class="progress-bar-fill ${barClass}" style="width:${pct}%"></div>
                            </div>
                            <span class="text-dim text-sm">${pct}%</span>
                        </div>
                    </td>
                    <td class="text-dim text-sm">${fmtRelative(n.lastTs)}</td>
                </tr>`;
            }).join('')}
            </tbody>
        </table></div>`;
    },

    _buildRecentActivity(images, nodes, logs) {
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
                <tr class="clickable" onclick="Router.navigate('/images/${img.id}')">
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
                <tr class="clickable" onclick="Router.navigate('/nodes/${n.id}')">
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
            <div class="modal">
                <div class="modal-header">
                    <span class="modal-title">Pull Image</span>
                    <button class="modal-close" onclick="document.getElementById('pull-modal').remove()">×</button>
                </div>
                <div class="modal-body">
                    <form id="pull-form" onsubmit="Pages.submitPull(event)">
                        <div class="form-grid">
                            <div class="form-group" style="grid-column:1/-1">
                                <label>Image URL *</label>
                                <input type="url" name="url" placeholder="https://example.com/image.tar.gz" required>
                            </div>
                            <div class="form-group">
                                <label>Name *</label>
                                <input type="text" name="name" placeholder="rocky-9-hpc" required>
                            </div>
                            <div class="form-group">
                                <label>Version</label>
                                <input type="text" name="version" placeholder="9.3">
                            </div>
                            <div class="form-group">
                                <label>OS</label>
                                <input type="text" name="os" placeholder="rocky">
                            </div>
                            <div class="form-group">
                                <label>Arch</label>
                                <input type="text" name="arch" placeholder="x86_64">
                            </div>
                            <div class="form-group">
                                <label>Format</label>
                                <select name="format">
                                    <option value="filesystem">filesystem (tar)</option>
                                    <option value="block">block (raw/partclone)</option>
                                </select>
                            </div>
                            <div class="form-group" style="grid-column:1/-1">
                                <label>Notes</label>
                                <input type="text" name="notes" placeholder="Optional description">
                            </div>
                        </div>
                        <div id="pull-progress" style="display:none;margin-top:12px">
                            <div style="font-size:12px;color:var(--text-secondary);margin-bottom:6px">Submitting pull request…</div>
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
        overlay.querySelector('input[name="url"]').focus();
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
        const form = e.target;
        const btn  = document.getElementById('pull-btn');
        const res  = document.getElementById('pull-result');
        const prog = document.getElementById('pull-progress');
        const data = new FormData(form);

        btn.disabled = true;
        btn.textContent = 'Submitting…';
        if (prog) prog.style.display = 'block';
        res.innerHTML = '';

        try {
            const body = {
                url:     data.get('url'),
                name:    data.get('name'),
                version: data.get('version'),
                os:      data.get('os'),
                arch:    data.get('arch'),
                format:  data.get('format'),
                notes:   data.get('notes'),
                tags:    [],
            };
            const img = await API.factory.pull(body);
            if (prog) prog.style.display = 'none';
            res.innerHTML = alertBox(`Pull started: ${img.name} (${img.id}) — status: ${img.status}`, 'success');
            form.reset();
            App.setAutoRefresh(() => Pages.images(), 5000);
            setTimeout(() => {
                const modal = document.getElementById('pull-modal');
                if (modal) modal.remove();
                Pages.images();
            }, 1500);
        } catch (ex) {
            if (prog) prog.style.display = 'none';
            res.innerHTML = alertBox(`Pull failed: ${ex.message}`);
            btn.disabled = false;
            btn.textContent = 'Pull Image';
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
                            ? `<button class="btn btn-secondary" onclick="Pages.showShellHint('${escHtml(img.id)}')">Shell Access</button>`
                            : ''}
                        ${img.status !== 'archived'
                            ? `<button class="btn btn-danger btn-sm" onclick="Pages.archiveImage('${img.id}', '${escHtml(img.name)}')">Archive</button>`
                            : ''}
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
        const area = document.getElementById('shell-hint-area');
        if (!area) return;
        const cmd = `clonr shell ${id}`;
        area.innerHTML = cardWrap('Shell Access', `
            <div class="card-body">
                <p class="text-dim text-sm mb-12">Run this command on the clonr server to open an interactive shell inside the image:</p>
                <div class="copy-wrap">
                    <pre class="json-block" style="flex:1;margin:0;user-select:all">${escHtml(cmd)}</pre>
                    <button class="copy-btn" onclick="Pages._copyText('${escHtml(cmd)}', this)">Copy</button>
                </div>
            </div>`);
        area.scrollIntoView({ behavior: 'smooth' });
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

    async nodes() {
        App.render(loading('Loading nodes…'));
        try {
            const [nodesResp, imagesResp] = await Promise.all([
                API.nodes.list(),
                API.images.list(),
            ]);
            const nodes  = nodesResp.nodes  || [];
            const images = imagesResp.images || [];
            const imgMap = Object.fromEntries(images.map(i => [i.id, i]));

            App.render(`
                <div class="page-header">
                    <div>
                        <div class="page-title">Nodes</div>
                        <div class="page-subtitle">${nodes.length} node${nodes.length !== 1 ? 's' : ''} total</div>
                    </div>
                    <button class="btn btn-primary" onclick="Pages.showNodeModal(null, ${JSON.stringify(JSON.stringify(images))})">
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
                            <tbody>
                            ${nodes.map(n => {
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
                                return `<tr>
                                    <td>
                                        <a href="#/nodes/${n.id}" style="font-weight:500;color:var(--text-primary)">
                                            ${(n.hostname && n.hostname !== '(none)')
                                                ? escHtml(n.hostname)
                                                : `<span class="text-dim" style="font-style:italic">Unassigned</span>`}
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
                            }).join('')}
                            </tbody>
                        </table></div>`
                        : emptyState('No nodes', 'Add your first node using the button above',
                            `<button class="btn btn-primary" onclick="Pages.showNodeModal(null, ${JSON.stringify(JSON.stringify(images))})">Add Node</button>`)
                )}
            `);

            App.setAutoRefresh(() => Pages.nodes());

        } catch (e) {
            App.render(alertBox(`Failed to load nodes: ${e.message}`));
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
                                <label>Hostname *</label>
                                <input type="text" name="hostname" value="${isEdit ? escHtml(node.hostname) : ''}" required>
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

        const body = {
            hostname:      data.get('hostname'),
            fqdn:          data.get('fqdn'),
            primary_mac:   data.get('primary_mac'),
            base_image_id: data.get('base_image_id'),
            groups,
            ssh_keys:    sshKeys,
            kernel_args: data.get('kernel_args'),
            interfaces:  [],
            custom_vars: {},
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
                            <div class="page-title">
                                ${(node.hostname && node.hostname !== '(none)')
                                    ? escHtml(node.hostname)
                                    : `<span class="text-dim" style="font-style:italic">Unassigned</span>`}
                            </div>
                            <div class="page-subtitle text-mono">${escHtml(node.primary_mac)}</div>
                        </div>
                        ${nodeBadge(node)}
                    </div>
                    <div class="flex gap-8">
                        <button class="btn btn-secondary" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(node))}, ${JSON.stringify(JSON.stringify(images))})'>Edit</button>
                        <button class="btn btn-danger btn-sm" onclick="Pages.deleteNodeAndGoBack('${node.id}', '${escHtml(displayName)}')">Delete</button>
                    </div>
                </div>

                <div class="tab-bar">
                    <div class="tab active" onclick="Pages._switchTab(this, 'tab-overview')">Overview</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-hardware')">Hardware</div>
                    <div class="tab" onclick="Pages._switchTab(this, 'tab-bmc');Pages._onBMCTabOpen('${node.id}', ${!!node.bmc})">Power / IPMI</div>
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
                                    ${(node.hostname && node.hostname !== '(none)') ? escHtml(node.hostname) : '<span class="text-dim" style="font-style:italic">Unassigned</span>'}
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

                <!-- Power / IPMI tab — always rendered; content depends on BMC config -->
                <div id="tab-bmc" class="tab-panel">
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
                    : `<div class="card"><div class="card-body">${emptyState(
                        'BMC not configured',
                        'Add BMC credentials to this node to enable remote power management.',
                        `<button class="btn btn-primary btn-sm" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(node))}, ${JSON.stringify(JSON.stringify([]))})'>Configure BMC</button>`
                    )}</div></div>`}
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

            // Kick off initial power status fetch if BMC is configured.
            // This runs immediately so status is ready when the user opens the tab.
            if (node.bmc && node.bmc.ip_address) {
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

// ─── Boot ─────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => App.init());
