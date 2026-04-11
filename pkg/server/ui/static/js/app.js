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
            // Try prefix match (for detail routes like /images/abc123).
            for (const key of Object.keys(this._routes)) {
                if (hash.startsWith(key + '/')) { handler = this._routes[key + '/*']; break; }
            }
        }
        if (!handler) handler = this._routes['/'];

        // Update nav active state.
        document.querySelectorAll('nav a').forEach(a => {
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
            if (parts.length === 3) Pages.imageDetail(parts[2]);
            else Pages.images();
        });
        Router.register('/images/*', (h)  => {
            const parts = h.split('/');
            Pages.imageDetail(parts[2]);
        });
        Router.register('/nodes',   (h)   => {
            const parts = h.split('/');
            if (parts.length === 3) Pages.nodeDetail(parts[2]);
            else Pages.nodes();
        });
        Router.register('/nodes/*', (h)   => {
            const parts = h.split('/');
            Pages.nodeDetail(parts[2]);
        });
        Router.register('/logs',    ()    => Pages.logs());
    },

    render(html) {
        this._mainEl.innerHTML = html;
    },

    setAutoRefresh(fn, intervalMs = 30000) {
        if (Router._refreshTimer) clearInterval(Router._refreshTimer);
        Router._refreshTimer = setInterval(fn, intervalMs);
    },

    _watchHealth() {
        const dot = document.getElementById('health-dot');
        const label = document.getElementById('health-label');
        const check = async () => {
            try {
                await API.health.get();
                dot.style.background = 'var(--success)';
                label.textContent = 'online';
            } catch (_) {
                dot.style.background = 'var(--error)';
                label.textContent = 'offline';
            }
        };
        check();
        setInterval(check, 30000);
    },
};

// ─── Shared UI helpers ────────────────────────────────────────────────────

function loading(msg = 'Loading…') {
    return `<div class="loading"><div class="spinner"></div>${msg}</div>`;
}

function alertBox(msg, type = 'error') {
    return `<div class="alert alert-${type}">${escHtml(msg)}</div>`;
}

// Keep backward-compat alias (some inline handlers may still call alert()).
function alert(msg, type = 'error') { return alertBox(msg, type); }

function badge(status) {
    const cls = {
        ready: 'badge-ready',
        building: 'badge-building',
        error: 'badge-error',
        archived: 'badge-archived',
    }[status] || 'badge-archived';
    return `<span class="badge ${cls}">${escHtml(status)}</span>`;
}

// Node status badge — derived from hardware_profile / base_image_id / deploy events.
function nodeBadge(node) {
    if (node._deployStatus === 'success') return `<span class="badge badge-deployed">Deployed</span>`;
    if (node._deployStatus === 'error')   return `<span class="badge badge-error">Failed</span>`;
    if (node.base_image_id) return `<span class="badge badge-building">Configured</span>`;
    if (node.hardware_profile) return `<span class="badge badge-warning">Registered</span>`;
    return `<span class="badge badge-archived">Unconfigured</span>`;
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

function emptyState(text, sub = '') {
    return `<div class="empty-state">
        <div class="empty-state-icon">○</div>
        <div class="empty-state-text">${text}</div>
        ${sub ? `<div class="empty-state-sub">${sub}</div>` : ''}
    </div>`;
}

// ─── Deployment phase helpers ──────────────────────────────────────────────

// Deploy phase extraction from log component/message.
function deployPhase(entry) {
    const msg = (entry.message || '').toLowerCase();
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

function phaseBadge(phase) {
    const colors = {
        complete: 'var(--success)',
        imaging: 'var(--info)',
        partitioning: 'var(--info)',
        finalizing: 'var(--info)',
        discovering: 'var(--info)',
        preflight: 'var(--info)',
        'in-progress': 'var(--info)',
        error: 'var(--error)',
        waiting: 'var(--text-secondary)',
    };
    const color = colors[phase] || 'var(--text-secondary)';
    return `<span style="color:${color};font-family:var(--font-mono);font-size:11px;font-weight:600">${escHtml(phase)}</span>`;
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

            // Group recent logs by node identifier for deployment progress panel.
            const deployProgress = this._buildDeployProgress(logs);

            App.render(`
                <div class="stats-grid">
                    <div class="stat-card">
                        <div class="stat-label">Total Images</div>
                        <div class="stat-value">${images.length}</div>
                        <div class="stat-sub text-success">${ready} ready</div>
                    </div>
                    <div class="stat-card">
                        <div class="stat-label">Building</div>
                        <div class="stat-value" style="color:var(--info)">${building}</div>
                        <div class="stat-sub">${errored} error</div>
                    </div>
                    <div class="stat-card">
                        <div class="stat-label">Nodes</div>
                        <div class="stat-value">${nodes.length}</div>
                        <div class="stat-sub">${nodes.filter(n => n.base_image_id).length} configured</div>
                    </div>
                    <div class="stat-card">
                        <div class="stat-label">Images — Error</div>
                        <div class="stat-value" style="color:${errored > 0 ? 'var(--error)' : 'var(--text-secondary)'}">${errored}</div>
                        <div class="stat-sub">${errored > 0 ? 'needs attention' : 'all clear'}</div>
                    </div>
                </div>

                ${deployProgress.length > 0 ? cardWrap('Active Deployments', this._deployProgressTable(deployProgress)) : ''}

                ${cardWrap('Recent Images', this._imagesTable(images.slice(0, 10)),
                    `<a href="#/images" class="btn btn-secondary btn-sm">View all</a>`)}

                ${cardWrap('Recent Nodes', this._nodesTable(nodes.slice(0, 8)),
                    `<a href="#/nodes" class="btn btn-secondary btn-sm">View all</a>`)}

                ${cardWrap('Live Log Stream',
                    `<div id="dash-log-viewer" class="log-viewer"></div>`,
                    `<span class="follow-indicator live" id="dash-follow-ind">
                        <span class="follow-dot"></span>following
                    </span>`)}
            `);

            // Wire up live log stream on dashboard.
            const viewer = document.getElementById('dash-log-viewer');
            if (viewer) {
                const stream = new LogStream(viewer);
                stream.onConnect(() => {
                    const ind = document.getElementById('dash-follow-ind');
                    if (ind) { ind.className = 'follow-indicator live'; ind.innerHTML = '<span class="follow-dot"></span>following'; }
                });
                stream.onDisconnect(() => {
                    const ind = document.getElementById('dash-follow-ind');
                    if (ind) { ind.className = 'follow-indicator'; ind.textContent = 'disconnected'; }
                });
                stream.connect();
                App._logStream = stream;
            }

            // Auto-refresh stats every 30 seconds.
            App.setAutoRefresh(() => Pages.dashboard());

        } catch (e) {
            App.render(alertBox(`Failed to load dashboard: ${e.message}`));
        }
    },

    // Build a per-node deployment progress summary from recent logs.
    _buildDeployProgress(logs) {
        const nodeMap = new Map(); // key: mac or hostname

        for (const entry of logs) {
            const key = entry.node_mac || entry.hostname || 'unknown';
            if (!nodeMap.has(key)) {
                nodeMap.set(key, {
                    key,
                    mac: entry.node_mac || '—',
                    hostname: entry.hostname || '—',
                    phase: 'waiting',
                    lastTs: entry.timestamp,
                    hasError: false,
                });
            }
            const state = nodeMap.get(key);
            const phase = deployPhase(entry);
            // Advance to the latest phase seen.
            if (phase === 'error') {
                state.hasError = true;
            } else if (PHASE_ORDER.indexOf(phase) > PHASE_ORDER.indexOf(state.phase)) {
                state.phase = phase;
            }
            if (new Date(entry.timestamp) > new Date(state.lastTs)) {
                state.lastTs = entry.timestamp;
            }
        }

        // Only surface nodes that have deploy-related activity (exclude "waiting").
        return Array.from(nodeMap.values())
            .filter(s => s.phase !== 'waiting' || s.hasError)
            .map(s => ({ ...s, phase: s.hasError && s.phase !== 'complete' ? 'error' : s.phase }))
            .sort((a, b) => new Date(b.lastTs) - new Date(a.lastTs))
            .slice(0, 20);
    },

    _deployProgressTable(nodes) {
        if (!nodes.length) return emptyState('No active deployments');
        return `<div class="table-wrap"><table>
            <thead><tr>
                <th>Hostname</th><th>MAC</th><th>Phase</th><th>Last Activity</th>
            </tr></thead>
            <tbody>
            ${nodes.map(n => `
                <tr>
                    <td class="text-accent">${escHtml(n.hostname)}</td>
                    <td class="mono dim">${escHtml(n.mac)}</td>
                    <td>${phaseBadge(n.phase)}</td>
                    <td class="dim">${fmtDateShort(n.lastTs)}</td>
                </tr>`).join('')}
            </tbody>
        </table></div>`;
    },

    _imagesTable(images) {
        if (!images.length) return emptyState('No images yet', 'Pull an image from the Images page');
        return `<div class="table-wrap"><table>
            <thead><tr>
                <th>Name</th><th>Version</th><th>OS / Arch</th><th>Format</th>
                <th>Status</th><th>Size</th><th>Created</th>
            </tr></thead>
            <tbody>
            ${images.map(img => `
                <tr class="clickable" onclick="Router.navigate('/images/${img.id}')">
                    <td class="text-accent">${escHtml(img.name)}</td>
                    <td class="mono dim">${escHtml(img.version || '—')}</td>
                    <td class="mono dim">${escHtml([img.os, img.arch].filter(Boolean).join(' / ') || '—')}</td>
                    <td class="mono dim">${escHtml(img.format || '—')}</td>
                    <td>${badge(img.status)}</td>
                    <td class="mono dim">${fmtBytes(img.size_bytes)}</td>
                    <td class="dim">${fmtDateShort(img.created_at)}</td>
                </tr>`).join('')}
            </tbody>
        </table></div>`;
    },

    _nodesTable(nodes) {
        if (!nodes.length) return emptyState('No nodes configured', 'Add a node from the Nodes page');
        return `<div class="table-wrap"><table>
            <thead><tr>
                <th>Hostname</th><th>MAC</th><th>FQDN</th><th>Status</th><th>Groups</th><th>Updated</th>
            </tr></thead>
            <tbody>
            ${nodes.map(n => `
                <tr class="clickable" onclick="Router.navigate('/nodes/${n.id}')">
                    <td class="text-accent">${escHtml(n.hostname)}</td>
                    <td class="mono dim">${escHtml(n.primary_mac || '—')}</td>
                    <td class="mono dim truncate">${escHtml(n.fqdn || '—')}</td>
                    <td>${nodeBadge(n)}</td>
                    <td class="dim">${(n.groups || []).map(g => `<span class="badge badge-archived">${escHtml(g)}</span>`).join(' ') || '—'}</td>
                    <td class="dim">${fmtDateShort(n.updated_at)}</td>
                </tr>`).join('')}
            </tbody>
        </table></div>`;
    },

    // ── Images ─────────────────────────────────────────────────────────────

    async images() {
        App.render(loading('Loading images…'));
        try {
            const resp = await API.images.list();
            const images = resp.images || [];

            App.render(`
                <div class="page-title-row">
                    <span class="page-title">Images</span>
                </div>

                ${cardWrap('Pull Image', this._pullForm(), '')}

                ${cardWrap('Import ISO', this._importISOForm(), '')}

                ${cardWrap(`All Images <span class="text-dim" style="font-size:12px;font-weight:normal">${images.length} total</span>`,
                    images.length
                        ? `<div class="table-wrap"><table>
                            <thead><tr>
                                <th>Name</th><th>Version</th><th>OS</th><th>Arch</th><th>Format</th>
                                <th>Status</th><th>Size</th><th>Checksum</th><th>Created</th><th>Actions</th>
                            </tr></thead>
                            <tbody>
                            ${images.map(img => `
                                <tr>
                                    <td><a href="#/images/${img.id}" class="text-accent">${escHtml(img.name)}</a></td>
                                    <td class="mono dim">${escHtml(img.version || '—')}</td>
                                    <td class="mono dim">${escHtml(img.os || '—')}</td>
                                    <td class="mono dim">${escHtml(img.arch || '—')}</td>
                                    <td class="mono dim">${escHtml(img.format || '—')}</td>
                                    <td>${badge(img.status)}</td>
                                    <td class="mono dim">${fmtBytes(img.size_bytes)}</td>
                                    <td class="mono dim truncate" title="${escHtml(img.checksum || '')}">${img.checksum ? img.checksum.substring(0, 12) + '…' : '—'}</td>
                                    <td class="dim">${fmtDateShort(img.created_at)}</td>
                                    <td>
                                        ${img.status !== 'archived'
                                            ? `<button class="btn btn-danger btn-sm" onclick="Pages.archiveImage('${img.id}', '${escHtml(img.name)}')">Archive</button>`
                                            : '<span class="dim" style="font-size:11px">archived</span>'}
                                    </td>
                                </tr>`).join('')}
                            </tbody>
                        </table></div>`
                        : emptyState('No images', 'Pull an image using the form above')
                )}
            `);

            // Poll for any building images and refresh automatically.
            const hasBuilding = images.some(i => i.status === 'building');
            App.setAutoRefresh(() => Pages.images(), hasBuilding ? 5000 : 30000);

        } catch (e) {
            App.render(alertBox(`Failed to load images: ${e.message}`));
        }
    },

    _pullForm() {
        return `<div class="card-body">
            <form id="pull-form" onsubmit="Pages.submitPull(event)">
                <div class="form-grid">
                    <div class="form-group" style="grid-column: 1/-1">
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
                    <div class="form-group">
                        <label>Notes</label>
                        <input type="text" name="notes" placeholder="Optional description">
                    </div>
                </div>
                <div id="pull-result"></div>
                <div class="form-actions">
                    <button type="submit" class="btn btn-primary" id="pull-btn">Pull Image</button>
                </div>
            </form>
        </div>`;
    },

    _importISOForm() {
        return `<div class="card-body">
            <p style="font-size:12px;color:var(--text-secondary);margin-bottom:12px">
                Import an ISO file that is already present on the server host. Provide the absolute path to the file.
            </p>
            <form id="iso-form" onsubmit="Pages.submitImportISO(event)">
                <div class="form-grid">
                    <div class="form-group" style="grid-column:1/-1">
                        <label>Server-side ISO Path *</label>
                        <input type="text" name="path" placeholder="/srv/images/rocky-9.3-x86_64.iso" required>
                    </div>
                    <div class="form-group">
                        <label>Name *</label>
                        <input type="text" name="name" placeholder="rocky-9-hpc" required>
                    </div>
                    <div class="form-group">
                        <label>Version</label>
                        <input type="text" name="version" placeholder="9.3">
                    </div>
                </div>
                <div id="iso-result"></div>
                <div class="form-actions">
                    <button type="submit" class="btn btn-secondary" id="iso-btn">Import ISO</button>
                </div>
            </form>
        </div>`;
    },

    async submitPull(e) {
        e.preventDefault();
        const form = e.target;
        const btn  = document.getElementById('pull-btn');
        const res  = document.getElementById('pull-result');
        const data = new FormData(form);

        btn.disabled = true;
        btn.textContent = 'Submitting…';
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
            res.innerHTML = alertBox(`Pull started: ${img.name} (${img.id}) — status: ${img.status}`, 'success');
            form.reset();
            // Poll every 5 seconds while building.
            App.setAutoRefresh(() => Pages.images(), 5000);
            setTimeout(() => Pages.images(), 1500);
        } catch (ex) {
            res.innerHTML = alertBox(`Pull failed: ${ex.message}`);
        } finally {
            btn.disabled = false;
            btn.textContent = 'Pull Image';
        }
    },

    async submitImportISO(e) {
        e.preventDefault();
        const form = e.target;
        const btn  = document.getElementById('iso-btn');
        const res  = document.getElementById('iso-result');
        const data = new FormData(form);

        btn.disabled = true;
        btn.textContent = 'Importing…';
        res.innerHTML = '';

        try {
            const body = {
                path:    data.get('path'),
                name:    data.get('name'),
                version: data.get('version'),
            };
            const img = await API.factory.importISO(body);
            res.innerHTML = alertBox(`Import started: ${img.name} (${img.id})`, 'success');
            form.reset();
            setTimeout(() => Pages.images(), 1500);
        } catch (ex) {
            res.innerHTML = alertBox(`Import failed: ${ex.message}`);
        } finally {
            btn.disabled = false;
            btn.textContent = 'Import ISO';
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

            const diskLayoutJson = JSON.stringify(img.disk_layout, null, 2);
            const tagsHtml = (img.tags || []).map(t => `<span class="badge badge-archived">${escHtml(t)}</span>`).join(' ') || '—';

            // Shell command hint for admins.
            const shellCmd = `clonr shell ${img.id}`;

            App.render(`
                <div class="detail-header">
                    <button class="detail-back" onclick="Router.navigate('/images')">← Images</button>
                    <span class="detail-title">${escHtml(img.name)}</span>
                    ${badge(img.status)}
                </div>

                ${img.error_message ? alertBox(`Error: ${img.error_message}`) : ''}

                ${img.status === 'building' ? `
                    <div class="alert alert-info" style="margin-bottom:12px">
                        Build in progress… This page will auto-refresh every 5 seconds.
                    </div>` : ''}

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
                            <div class="kv-item"><div class="kv-key">Source URL</div><div class="kv-value" style="font-size:11px">${img.source_url ? `<a href="${escHtml(img.source_url)}" target="_blank" rel="noreferrer">${escHtml(img.source_url)}</a>` : '—'}</div></div>
                            <div class="kv-item"><div class="kv-key">Tags</div><div class="kv-value">${tagsHtml}</div></div>
                            <div class="kv-item"><div class="kv-key">Notes</div><div class="kv-value">${escHtml(img.notes || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Created</div><div class="kv-value">${fmtDate(img.created_at)}</div></div>
                            <div class="kv-item"><div class="kv-key">Finalized</div><div class="kv-value">${fmtDate(img.finalized_at)}</div></div>
                        </div>
                    </div>
                `, `
                    ${img.status === 'ready' ? `
                        <button class="btn btn-secondary btn-sm" onclick="Pages.showShellHint('${escHtml(img.id)}')" title="Show shell access command">Shell</button>
                    ` : ''}
                    ${img.status !== 'archived'
                        ? `<button class="btn btn-danger btn-sm" onclick="Pages.archiveImage('${img.id}', '${escHtml(img.name)}')">Archive</button>`
                        : ''}
                `)}

                ${cardWrap('Disk Layout', `<div class="card-body"><pre class="json-block">${escHtml(diskLayoutJson)}</pre></div>`)}

                <div id="shell-hint-area"></div>
            `);

            // Auto-refresh for building images.
            if (img.status === 'building') {
                App.setAutoRefresh(() => Pages.imageDetail(id), 5000);
            }
        } catch (e) {
            App.render(alertBox(`Failed to load image: ${e.message}`));
        }
    },

    showShellHint(id) {
        const area = document.getElementById('shell-hint-area');
        if (!area) return;
        area.innerHTML = cardWrap('Shell Access', `
            <div class="card-body">
                <p style="font-size:12px;color:var(--text-secondary);margin-bottom:8px">
                    Run this command on the clonr server to open an interactive shell inside the image:
                </p>
                <pre class="json-block" style="user-select:all">clonr shell ${escHtml(id)}</pre>
            </div>`);
        area.scrollIntoView({ behavior: 'smooth' });
    },

    // ── Nodes ──────────────────────────────────────────────────────────────

    async nodes() {
        App.render(loading('Loading nodes…'));
        try {
            const [nodesResp, imagesResp] = await Promise.all([
                API.nodes.list(),
                API.images.list(),
            ]);
            const nodes  = nodesResp.nodes   || [];
            const images = imagesResp.images  || [];
            const imgMap = Object.fromEntries(images.map(i => [i.id, i]));

            App.render(`
                <div class="page-title-row">
                    <span class="page-title">Nodes</span>
                    <button class="btn btn-primary btn-sm" onclick="Pages.showNodeModal(null, ${JSON.stringify(JSON.stringify(images))})">+ Add Node</button>
                </div>

                ${cardWrap(`All Nodes <span class="text-dim" style="font-size:12px;font-weight:normal">${nodes.length} total</span>`,
                    nodes.length
                        ? `<div class="table-wrap"><table>
                            <thead><tr>
                                <th>Hostname</th><th>FQDN</th><th>Primary MAC</th><th>Image</th>
                                <th>Status</th><th>Groups</th><th>Updated</th><th>Actions</th>
                            </tr></thead>
                            <tbody>
                            ${nodes.map(n => {
                                const img = imgMap[n.base_image_id];
                                return `<tr>
                                    <td><a href="#/nodes/${n.id}" class="text-accent">${escHtml(n.hostname)}</a></td>
                                    <td class="mono dim">${escHtml(n.fqdn || '—')}</td>
                                    <td class="mono dim">${escHtml(n.primary_mac)}</td>
                                    <td class="dim">${img ? `<a href="#/images/${img.id}">${escHtml(img.name)}</a>` : (n.base_image_id ? `<span class="text-dim">${escHtml(n.base_image_id.substring(0, 8))}…</span>` : '—')}</td>
                                    <td>${nodeBadge(n)}</td>
                                    <td class="dim">${(n.groups || []).map(g => `<span class="badge badge-archived">${escHtml(g)}</span>`).join(' ') || '—'}</td>
                                    <td class="dim">${fmtDateShort(n.updated_at)}</td>
                                    <td style="display:flex;gap:4px;flex-wrap:wrap">
                                        <button class="btn btn-secondary btn-sm" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(n))}, ${JSON.stringify(JSON.stringify(images))})'>Edit</button>
                                        <button class="btn btn-danger btn-sm" onclick="Pages.deleteNode('${n.id}', '${escHtml(n.hostname)}')">Delete</button>
                                    </td>
                                </tr>`;
                            }).join('')}
                            </tbody>
                        </table></div>`
                        : emptyState('No nodes', 'Add a node using the button above')
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
            .map(i => `<option value="${escHtml(i.id)}" ${node && node.base_image_id === i.id ? 'selected' : ''}>${escHtml(i.name)} ${i.version ? '(' + i.version + ')' : ''}</option>`)
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
                                <label>Base Image *</label>
                                <select name="base_image_id" required>
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

        // Close on backdrop click.
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

        const groups = data.get('groups')
            .split(',').map(g => g.trim()).filter(Boolean);
        const sshKeys = data.get('ssh_keys')
            .split('\n').map(k => k.trim()).filter(Boolean);

        const body = {
            hostname:     data.get('hostname'),
            fqdn:         data.get('fqdn'),
            primary_mac:  data.get('primary_mac'),
            base_image_id: data.get('base_image_id'),
            groups,
            ssh_keys: sshKeys,
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

    async deleteNode(id, hostname) {
        if (!confirm(`Delete node "${hostname}"? This cannot be undone.`)) return;
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

            const nodeJson = JSON.stringify(node, null, 2);

            // Parse hardware profile if present.
            let hw = null;
            try {
                if (node.hardware_profile) {
                    hw = typeof node.hardware_profile === 'string'
                        ? JSON.parse(node.hardware_profile)
                        : node.hardware_profile;
                }
            } catch (_) {}

            App.render(`
                <div class="detail-header">
                    <button class="detail-back" onclick="Router.navigate('/nodes')">← Nodes</button>
                    <span class="detail-title">${escHtml(node.hostname)}</span>
                    <span class="mono dim" style="font-size:12px">${escHtml(node.primary_mac)}</span>
                    ${nodeBadge(node)}
                </div>

                ${cardWrap('Node Details', `
                    <div class="card-body">
                        <div class="kv-grid">
                            <div class="kv-item"><div class="kv-key">ID</div><div class="kv-value">${escHtml(node.id)}</div></div>
                            <div class="kv-item"><div class="kv-key">Hostname</div><div class="kv-value">${escHtml(node.hostname)}</div></div>
                            <div class="kv-item"><div class="kv-key">FQDN</div><div class="kv-value">${escHtml(node.fqdn || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Primary MAC</div><div class="kv-value">${escHtml(node.primary_mac)}</div></div>
                            <div class="kv-item"><div class="kv-key">Base Image</div><div class="kv-value">${img ? `<a href="#/images/${img.id}">${escHtml(img.name)}</a> ${badge(img.status)}` : (node.base_image_id ? escHtml(node.base_image_id) : '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Groups</div><div class="kv-value">${(node.groups || []).map(g => `<span class="badge badge-archived">${escHtml(g)}</span>`).join(' ') || '—'}</div></div>
                            <div class="kv-item"><div class="kv-key">Kernel Args</div><div class="kv-value">${escHtml(node.kernel_args || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Created</div><div class="kv-value">${fmtDate(node.created_at)}</div></div>
                            <div class="kv-item"><div class="kv-key">Updated</div><div class="kv-value">${fmtDate(node.updated_at)}</div></div>
                        </div>
                    </div>
                `, `<button class="btn btn-secondary btn-sm" onclick='Pages.showNodeModal(${JSON.stringify(JSON.stringify(node))}, ${JSON.stringify(JSON.stringify(images))})'>Edit</button>
                    <button class="btn btn-danger btn-sm" onclick="Pages.deleteNodeAndGoBack('${node.id}', '${escHtml(node.hostname)}')">Delete</button>`)}

                ${node.ssh_keys && node.ssh_keys.length ? cardWrap('SSH Keys', `
                    <div class="card-body">
                        ${node.ssh_keys.map(k => `<div class="json-block" style="font-size:11px;margin-bottom:6px">${escHtml(k)}</div>`).join('')}
                    </div>`) : ''}

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

                ${node.ib_config && node.ib_config.length ? cardWrap('InfiniBand Config', `
                    <div class="table-wrap"><table>
                        <thead><tr><th>Device</th><th>PKeys</th><th>IPoIB Mode</th><th>IP</th><th>MTU</th></tr></thead>
                        <tbody>
                        ${node.ib_config.map(ib => `<tr>
                            <td class="mono">${escHtml(ib.device_name)}</td>
                            <td class="mono dim">${(ib.pkeys || []).join(', ') || '—'}</td>
                            <td class="mono dim">${escHtml(ib.ipoib_mode || '—')}</td>
                            <td class="mono dim">${escHtml(ib.ip_address || '—')}</td>
                            <td class="mono dim">${ib.mtu || '—'}</td>
                        </tr>`).join('')}
                        </tbody>
                    </table></div>`) : ''}

                ${node.bmc ? cardWrap('BMC / IPMI', `
                    <div class="card-body">
                        <div class="kv-grid">
                            <div class="kv-item"><div class="kv-key">IP Address</div><div class="kv-value">${escHtml(node.bmc.ip_address || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Netmask</div><div class="kv-value">${escHtml(node.bmc.netmask || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Gateway</div><div class="kv-value">${escHtml(node.bmc.gateway || '—')}</div></div>
                            <div class="kv-item"><div class="kv-key">Username</div><div class="kv-value">${escHtml(node.bmc.username || '—')}</div></div>
                        </div>
                        ${this._ipmiControls(node)}
                    </div>`) : ''}

                ${hw ? this._hardwareProfile(hw) : ''}

                ${Object.keys(node.custom_vars || {}).length ? cardWrap('Custom Variables', `
                    <div class="card-body">
                        <div class="kv-grid">
                        ${Object.entries(node.custom_vars).map(([k, v]) => `
                            <div class="kv-item"><div class="kv-key">${escHtml(k)}</div><div class="kv-value">${escHtml(v)}</div></div>`).join('')}
                        </div>
                    </div>`) : ''}

                ${cardWrap('Raw JSON', `<div class="card-body"><pre class="json-block">${escHtml(nodeJson)}</pre></div>`)}
            `);
        } catch (e) {
            App.render(alertBox(`Failed to load node: ${e.message}`));
        }
    },

    // IPMI control panel — shows copy-able CLI commands since the web server
    // typically doesn't have direct BMC network access.
    _ipmiControls(node) {
        if (!node.bmc || !node.bmc.ip_address) return '';
        const bmcIP = node.bmc.ip_address;
        const user  = node.bmc.username || 'admin';

        const commands = [
            { label: 'Power On',    cmd: `ipmitool -H ${bmcIP} -U ${user} -P <password> power on` },
            { label: 'Power Off',   cmd: `ipmitool -H ${bmcIP} -U ${user} -P <password> power off` },
            { label: 'Power Cycle', cmd: `ipmitool -H ${bmcIP} -U ${user} -P <password> power cycle` },
            { label: 'Reset',       cmd: `ipmitool -H ${bmcIP} -U ${user} -P <password> power reset` },
            { label: 'PXE Boot',    cmd: `ipmitool -H ${bmcIP} -U ${user} -P <password> chassis bootdev pxe && ipmitool -H ${bmcIP} -U ${user} -P <password> power cycle` },
            { label: 'Sensors',     cmd: `ipmitool -H ${bmcIP} -U ${user} -P <password> sdr list` },
        ];

        return `
            <div style="margin-top:16px;border-top:1px solid var(--border);padding-top:12px">
                <div style="font-size:11px;color:var(--text-secondary);text-transform:uppercase;letter-spacing:0.6px;font-weight:600;margin-bottom:10px">IPMI Controls</div>
                <p style="font-size:12px;color:var(--text-secondary);margin-bottom:10px">
                    Run these commands from a host with BMC network access. Click to copy.
                </p>
                <div style="display:flex;flex-wrap:wrap;gap:6px;margin-bottom:10px">
                    ${commands.map(c => `
                        <button class="btn btn-secondary btn-sm" onclick="Pages.copyCmd('${escHtml(c.cmd)}')" title="${escHtml(c.cmd)}">
                            ${escHtml(c.label)}
                        </button>`).join('')}
                </div>
                <div id="ipmi-cmd-display" style="display:none">
                    <pre class="json-block" id="ipmi-cmd-text" style="margin-top:8px;user-select:all"></pre>
                </div>
            </div>`;
    },

    copyCmd(cmd) {
        const display = document.getElementById('ipmi-cmd-display');
        const text    = document.getElementById('ipmi-cmd-text');
        if (display && text) {
            text.textContent = cmd;
            display.style.display = 'block';
        }
        if (navigator.clipboard) {
            navigator.clipboard.writeText(cmd).catch(() => {});
        }
    },

    // Render the hardware profile card if hw data is present.
    _hardwareProfile(hw) {
        const sections = [];

        // Disk topology — disks with partitions, RAID arrays.
        if ((hw.Disks && hw.Disks.length) || (hw.MDArrays && hw.MDArrays.length)) {
            let diskHtml = '';

            if (hw.MDArrays && hw.MDArrays.length) {
                diskHtml += `<div style="margin-bottom:12px">
                    <div style="font-size:11px;color:var(--text-secondary);text-transform:uppercase;margin-bottom:6px;font-weight:600">Software RAID Arrays</div>
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
                diskHtml += `<div style="font-size:11px;color:var(--text-secondary);text-transform:uppercase;margin-bottom:6px;font-weight:600">Physical Disks</div>
                <div class="table-wrap"><table>
                    <thead><tr><th>Device</th><th>Model</th><th>Size</th><th>Transport</th><th>Type</th><th>Partitions</th></tr></thead>
                    <tbody>
                    ${hw.Disks.map(d => `<tr>
                        <td class="mono text-accent">/dev/${escHtml(d.Name)}</td>
                        <td class="dim">${escHtml(d.Model || '—')}</td>
                        <td class="mono dim">${fmtBytes(d.Size)}</td>
                        <td class="mono dim">${escHtml(d.Transport || '—')}</td>
                        <td class="mono dim">${d.Rotational ? 'HDD' : 'SSD/NVMe'}</td>
                        <td class="mono dim">${(d.Partitions || []).map(p =>
                            `<span title="${escHtml(p.MountPoint || p.FSType || '')}">${escHtml(p.Name)} ${fmtBytes(p.Size)}</span>`
                        ).join(', ') || '—'}</td>
                    </tr>`).join('')}
                    </tbody>
                </table></div>`;
            }

            sections.push(cardWrap('Disk Topology', `<div class="card-body">${diskHtml}</div>`));
        }

        // InfiniBand devices.
        if (hw.IBDevices && hw.IBDevices.length) {
            const ibHtml = hw.IBDevices.map(dev => `
                <div style="margin-bottom:16px;padding-bottom:16px;border-bottom:1px solid var(--border)">
                    <div style="font-family:var(--font-mono);font-size:13px;color:var(--accent);margin-bottom:8px">${escHtml(dev.Name)}</div>
                    <div class="kv-grid" style="margin-bottom:8px">
                        <div class="kv-item"><div class="kv-key">Board ID</div><div class="kv-value">${escHtml(dev.BoardID || '—')}</div></div>
                        <div class="kv-item"><div class="kv-key">Firmware</div><div class="kv-value">${escHtml(dev.FWVersion || '—')}</div></div>
                        <div class="kv-item"><div class="kv-key">Node GUID</div><div class="kv-value mono" style="font-size:11px">${escHtml(dev.NodeGUID || '—')}</div></div>
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

        // NIC details.
        if (hw.NICs && hw.NICs.length) {
            const nicHtml = `<div class="table-wrap"><table>
                <thead><tr><th>Interface</th><th>MAC</th><th>Speed</th><th>Driver</th><th>State</th><th>IP (Runtime)</th></tr></thead>
                <tbody>
                ${hw.NICs.map(n => `<tr>
                    <td class="mono text-accent">${escHtml(n.Name || '—')}</td>
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

        // BMC/IPMI from hardware profile (discovered via ipmitool at register time).
        if (hw.BMC) {
            const bmcHtml = `<div class="card-body"><div class="kv-grid">
                <div class="kv-item"><div class="kv-key">IP</div><div class="kv-value">${escHtml(hw.BMC.IPAddress || '—')}</div></div>
                <div class="kv-item"><div class="kv-key">Firmware</div><div class="kv-value">${escHtml(hw.BMC.FirmwareVersion || '—')}</div></div>
                <div class="kv-item"><div class="kv-key">Manufacturer</div><div class="kv-value">${escHtml(hw.BMC.Manufacturer || '—')}</div></div>
            </div></div>`;
            sections.push(cardWrap('BMC / IPMI (Discovered)', bmcHtml));
        }

        return sections.join('');
    },

    _raidStateBadge(state) {
        const colors = {
            active: 'var(--success)',
            degraded: 'var(--warning)',
            rebuilding: 'var(--info)',
        };
        const color = colors[state] || 'var(--text-secondary)';
        return `<span style="color:${color};font-family:var(--font-mono);font-size:11px;font-weight:600">${escHtml(state || '—')}</span>`;
    },

    _ibStateBadge(state) {
        const s = (state || '').toUpperCase();
        const color = s === 'ACTIVE' ? 'var(--success)' : s === 'DOWN' ? 'var(--error)' : 'var(--warning)';
        return `<span style="color:${color};font-family:var(--font-mono);font-size:11px;font-weight:600">${escHtml(state || '—')}</span>`;
    },

    _nicStateBadge(state) {
        const s = (state || '').toLowerCase();
        const color = s === 'up' ? 'var(--success)' : s === 'down' ? 'var(--error)' : 'var(--text-secondary)';
        return `<span style="color:${color};font-family:var(--font-mono);font-size:11px;font-weight:600">${escHtml(state || '—')}</span>`;
    },

    async deleteNodeAndGoBack(id, hostname) {
        if (!confirm(`Delete node "${hostname}"? This cannot be undone.`)) return;
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
            <div class="page-title-row">
                <span class="page-title">Logs</span>
            </div>

            <div class="log-filter-bar">
                <input id="lf-mac"       type="text"  placeholder="Filter: MAC"       style="width:160px">
                <input id="lf-hostname"  type="text"  placeholder="Filter: Hostname"  style="width:140px">
                <select id="lf-level">
                    <option value="">All levels</option>
                    <option value="debug">debug</option>
                    <option value="info">info</option>
                    <option value="warn">warn</option>
                    <option value="error">error</option>
                </select>
                <select id="lf-component">
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
                <input id="lf-since" type="datetime-local" title="Since (local time)">
                <button class="btn btn-secondary btn-sm" onclick="Pages.loadLogs()">Query</button>
                <button class="btn btn-secondary btn-sm" onclick="Pages.clearLogs()">Clear</button>
                <span style="flex:1"></span>
                <div class="log-toolbar">
                    <label class="toggle">
                        <input type="checkbox" id="follow-toggle" onchange="Pages.toggleFollow(this.checked)">
                        Follow live
                    </label>
                    <span class="follow-indicator" id="follow-ind">
                        <span class="follow-dot"></span>disconnected
                    </span>
                </div>
            </div>

            <div id="log-viewer" class="log-viewer tall"></div>
        `);

        // Load recent logs on open.
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

        // If stream is active, just update its filters instead.
        const followToggle = document.getElementById('follow-toggle');
        if (App._logStream && followToggle && followToggle.checked) {
            App._logStream.setFilters(this._logFilters());
            return;
        }

        try {
            const params = this._logFilters();
            const resp = await API.logs.query(params);
            const entries = resp.logs || [];

            if (!App._logStream) {
                const stream = new LogStream(viewer);
                App._logStream = stream;
            }
            App._logStream.loadEntries(entries);

            if (!entries.length) {
                viewer.innerHTML = '<div class="empty-state" style="padding:30px"><div class="empty-state-text">No log entries match your filters</div></div>';
            }
        } catch (e) {
            if (viewer) viewer.innerHTML = `<div style="padding:12px;color:var(--error);font-family:var(--font-mono);font-size:12px">Error: ${escHtml(e.message)}</div>`;
        }
    },

    clearLogs() {
        if (App._logStream) App._logStream.clear();
    },

    toggleFollow(enabled) {
        const viewer  = document.getElementById('log-viewer');
        const ind     = document.getElementById('follow-ind');
        if (!viewer) return;

        if (enabled) {
            if (!App._logStream) {
                const stream = new LogStream(viewer);
                App._logStream = stream;
            }
            App._logStream.setFilters(this._logFilters());
            App._logStream.setAutoScroll(true);
            App._logStream.onConnect(() => {
                if (ind) { ind.className = 'follow-indicator live'; ind.innerHTML = '<span class="follow-dot"></span>following'; }
            });
            App._logStream.onDisconnect(() => {
                if (ind) { ind.className = 'follow-indicator'; ind.innerHTML = '<span class="follow-dot"></span>disconnected'; }
            });
            App._logStream.connect();
        } else {
            if (App._logStream) {
                App._logStream.disconnect();
                if (ind) { ind.className = 'follow-indicator'; ind.innerHTML = '<span class="follow-dot"></span>disconnected'; }
            }
        }
    },
};

// ─── Boot ─────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => App.init());
