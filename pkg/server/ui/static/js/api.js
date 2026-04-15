// api.js — thin fetch wrapper around the clonr-serverd REST API.
// All methods return parsed JSON or throw an Error with a message from the API.

const API = {
    BASE: '/api/v1',

    // _token returns the legacy localStorage key if present (backwards compat
    // for any CLI/scripted use that injected a Bearer token via the old modal).
    // Session-cookie auth does not need this — the browser sends the cookie automatically.
    _token() {
        try { return localStorage.getItem('clonr_admin_key') || ''; } catch (_) { return ''; }
    },

    _headers(extra = {}) {
        const h = { 'Content-Type': 'application/json', ...extra };
        const tok = this._token();
        if (tok) h['Authorization'] = `Bearer ${tok}`;
        return h;
    },

    // _redirectToLogin navigates to /login if not already there.
    _redirectToLogin() {
        if (window.location.pathname !== '/login') {
            window.location.href = '/login';
        }
    },

    async _parse(resp) {
        const ct = resp.headers.get('Content-Type') || '';
        if (!resp.ok) {
            // 401/403 — session expired or no auth. Redirect to login page.
            if (resp.status === 401 || resp.status === 403) {
                try { localStorage.removeItem('clonr_admin_key'); } catch (_) {}
                API._redirectToLogin();
                // Throw so callers don't proceed with undefined data.
                throw new Error('session expired — redirecting to login');
            }
            let msg = `HTTP ${resp.status}`;
            if (ct.includes('application/json')) {
                const body = await resp.json().catch(() => null);
                if (body && body.error) msg = body.error;
            }
            throw new Error(msg);
        }
        if (resp.status === 204) return null;
        if (ct.includes('application/json')) return resp.json();
        return null;
    },

    async get(path, params = {}) {
        const url = new URL(this.BASE + path, window.location.origin);
        Object.entries(params).forEach(([k, v]) => { if (v !== '' && v != null) url.searchParams.set(k, v); });
        const resp = await fetch(url.toString(), {
            headers: this._headers({ 'Content-Type': undefined }),
            credentials: 'same-origin',
        });
        return this._parse(resp);
    },

    async post(path, body) {
        const resp = await fetch(this.BASE + path, {
            method: 'POST',
            headers: this._headers(),
            body: JSON.stringify(body),
            credentials: 'same-origin',
        });
        return this._parse(resp);
    },

    async put(path, body) {
        const resp = await fetch(this.BASE + path, {
            method: 'PUT',
            headers: this._headers(),
            body: JSON.stringify(body),
            credentials: 'same-origin',
        });
        return this._parse(resp);
    },

    async del(path) {
        const resp = await fetch(this.BASE + path, {
            method: 'DELETE',
            headers: this._headers({ 'Content-Type': undefined }),
            credentials: 'same-origin',
        });
        return this._parse(resp);
    },

    // Generic request helper used by dynamic endpoints (layout, groups, etc.).
    async request(method, path, body) {
        const opts = {
            method,
            headers: method === 'GET' || method === 'DELETE'
                ? this._headers({ 'Content-Type': undefined })
                : this._headers(),
            credentials: 'same-origin',
        };
        if (body !== undefined && method !== 'GET' && method !== 'DELETE') {
            opts.body = JSON.stringify(body);
        }
        const resp = await fetch(this.BASE + path, opts);
        return this._parse(resp);
    },

    // Convenience methods.
    images: {
        list(status = '')           { return API.get('/images', status ? { status } : {}); },
        get(id)                     { return API.get(`/images/${id}`); },
        archive(id)                 { return API.del(`/images/${id}`); },
        // delete sends a real DELETE that removes blobs + DB record.
        // opts.force=true unassigns nodes and deletes anyway.
        delete(id, opts = {})       {
            const path = opts.force ? `/images/${id}?force=true` : `/images/${id}`;
            return API.del(path);
        },
        diskLayout(id)              { return API.get(`/images/${id}/disklayout`); },
        activeDeploys(id)           { return API.get(`/images/${id}/active-deploys`); },
        openShellSession(id)        { return API.post(`/images/${id}/shell-session`, {}); },
        closeShellSession(id, sid)  { return API.del(`/images/${id}/shell-session/${sid}`); },
        shellWsUrl(id, sid) {
            const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
            const tok = API._token();
            const base = `${proto}//${location.host}/api/v1/images/${id}/shell-session/${sid}/ws`;
            return tok ? `${base}?token=${encodeURIComponent(tok)}` : base;
        },
    },
    nodes: {
        list()                { return API.get('/nodes'); },
        get(id)               { return API.get(`/nodes/${id}`); },
        create(body)          { return API.post('/nodes', body); },
        update(id, body)      { return API.put(`/nodes/${id}`, body); },
        del(id)               { return API.del(`/nodes/${id}`); },
        power: {
            status(id)              { return API.get(`/nodes/${id}/power`); },
            on(id)                  { return API.post(`/nodes/${id}/power/on`); },
            off(id)                 { return API.post(`/nodes/${id}/power/off`); },
            cycle(id)               { return API.post(`/nodes/${id}/power/cycle`); },
            reset(id)               { return API.post(`/nodes/${id}/power/reset`); },
            pxeBoot(id)             { return API.post(`/nodes/${id}/power/pxe`); },
            diskBoot(id)            { return API.post(`/nodes/${id}/power/disk`); },
            // flipToDisk calls the provider-abstracted boot-flip endpoint.
            // When cycle=true the server also power-cycles the node after flipping.
            flipToDisk(id, cycle)   { return API.post(`/nodes/${id}/power/flip-to-disk${cycle ? '?cycle=true' : ''}`); },
        },
        sensors(id)           { return API.get(`/nodes/${id}/sensors`); },
    },
    logs: {
        query(params = {})    { return API.get('/logs', params); },
    },
    factory: {
        pull(body)            { return API.post('/factory/pull', body); },
        importISO(body)       { return API.post('/factory/import-iso', body); },
        capture(body)         { return API.post('/factory/capture', body); },
        // buildFromISO submits an installer ISO URL for automated VM-based install.
        // The server downloads the ISO, runs it in QEMU, captures the rootfs,
        // and returns a building BaseImage record. Poll GET /images/:id for status.
        buildFromISO(body)    { return API.post('/factory/build-from-iso', body); },

        // uploadISO — browser file upload with real progress.
        // file     : File object from <input type="file"> or drag-and-drop.
        // metadata : { name, version }
        // onProgress(pct, bytesLoaded, bytesTotal, speedBps, etaSecs)
        // Returns a Promise that resolves to the created BaseImage record.
        uploadISO(file, metadata, onProgress) {
            return new Promise((resolve, reject) => {
                const fd = new FormData();
                fd.append('file', file);
                fd.append('name', metadata.name || '');
                if (metadata.version) fd.append('version', metadata.version);

                const xhr = new XMLHttpRequest();
                let startTime = null;

                xhr.upload.addEventListener('loadstart', () => { startTime = Date.now(); });
                xhr.upload.addEventListener('progress', (e) => {
                    if (!e.lengthComputable || !onProgress) return;
                    const pct  = Math.round((e.loaded / e.total) * 100);
                    const secs = (Date.now() - startTime) / 1000 || 0.001;
                    const bps  = e.loaded / secs;
                    const eta  = bps > 0 ? (e.total - e.loaded) / bps : 0;
                    onProgress(pct, e.loaded, e.total, bps, eta);
                });

                xhr.addEventListener('load', () => {
                    if (xhr.status >= 200 && xhr.status < 300) {
                        try { resolve(JSON.parse(xhr.responseText)); }
                        catch { resolve(null); }
                    } else {
                        let msg = `HTTP ${xhr.status}`;
                        try {
                            const body = JSON.parse(xhr.responseText);
                            if (body && body.error) msg = body.error;
                        } catch {}
                        reject(new Error(msg));
                    }
                });
                xhr.addEventListener('error', () => reject(new Error('Network error during upload')));
                xhr.addEventListener('abort', () => reject(new Error('Upload cancelled')));

                const tok = API._token();
                xhr.open('POST', `${API.BASE}/factory/import`);
                if (tok) xhr.setRequestHeader('Authorization', `Bearer ${tok}`);
                xhr.send(fd);
            });
        },
    },
    buildProgress: {
        // get returns the current BuildState snapshot for an image.
        get(imageId)        { return API.get(`/images/${imageId}/build-progress`); },
        // sseUrl returns the URL for the SSE stream endpoint for a given image.
        sseUrl(imageId) {
            const tok = API._token();
            const base = `${window.location.origin}/api/v1/images/${imageId}/build-progress/stream`;
            return tok ? `${base}?token=${encodeURIComponent(tok)}` : base;
        },
        // buildLogUrl returns the URL for the full build log download.
        buildLogUrl(imageId) {
            const tok = API._token();
            const base = `${window.location.origin}/api/v1/images/${imageId}/build-log`;
            return tok ? `${base}?token=${encodeURIComponent(tok)}` : base;
        },
        // manifest returns the JSON build summary written after a completed build.
        manifest(imageId)   { return API.get(`/images/${imageId}/build-manifest`); },
    },
    imageRoles: {
        list()              { return API.get('/image-roles'); },
    },
    nodeGroups: {
        list()              { return API.get('/node-groups'); },
        get(id)             { return API.get(`/node-groups/${id}`); },
        create(body)        { return API.post('/node-groups', body); },
        update(id, body)    { return API.put(`/node-groups/${id}`, body); },
        del(id)             { return API.del(`/node-groups/${id}`); },
        // Group membership management.
        addMembers(id, nodeIds)  { return API.post(`/node-groups/${id}/members`, { node_ids: nodeIds }); },
        removeMember(id, nodeId) { return API.del(`/node-groups/${id}/members/${encodeURIComponent(nodeId)}`); },
        // Rolling group reimage.
        reimage(id, body)   { return API.post(`/node-groups/${id}/reimage`, body); },
        // Job status polling.
        jobStatus(jobId)    { return API.get(`/reimages/jobs/${encodeURIComponent(jobId)}`); },
        resumeJob(jobId)    { return API.post(`/reimages/jobs/${encodeURIComponent(jobId)}/resume`, {}); },
    },
    reimages: {
        // listForNode fetches reimage history for a single node.
        listForNode(nodeId)                 { return API.get(`/nodes/${nodeId}/reimage`); },
        // list fetches all reimage records with optional filters.
        list(params = {})                   { return API.get('/reimages', params); },
        get(id)                             { return API.get(`/reimage/${id}`); },
        cancel(id)                          { return API.del(`/reimage/${id}`); },
        retry(id)                           { return API.post(`/reimage/${id}/retry`, {}); },
    },
    health: {
        get()                 { return API.get('/health'); },
    },
    auth: {
        me()                  { return API.get('/auth/me'); },
    },
    apiKeys: {
        list()                { return API.get('/admin/api-keys'); },
        create(body)          { return API.post('/admin/api-keys', body); },
        revoke(id)            { return API.del(`/admin/api-keys/${id}`); },
        rotate(id)            { return API.post(`/admin/api-keys/${id}/rotate`, {}); },
    },
    users: {
        list()                        { return API.get('/admin/users'); },
        create(body)                  { return API.post('/admin/users', body); },
        update(id, body)              { return API.put(`/admin/users/${id}`, body); },
        resetPassword(id, password)   { return API.post(`/admin/users/${id}/reset-password`, { password }); },
        disable(id)                   { return API.del(`/admin/users/${id}`); },
    },
    system: {
        // initramfs — GET current status + history, POST to rebuild, DELETE history entry.
        initramfs()               { return API.get('/system/initramfs'); },
        rebuildInitramfs()        { return API.post('/system/initramfs/rebuild', {}); },
        deleteInitramfsHistory(id){ return API.del(`/system/initramfs/history/${encodeURIComponent(id)}`); },
    },
    resume: {
        // resume — POST to resume an interrupted image build.
        image(id)             { return API.post(`/images/${id}/resume`, {}); },
    },
    progress: {
        list()                { return API.get('/deploy/progress'); },
        get(mac)              { return API.get(`/deploy/progress/${encodeURIComponent(mac)}`); },

        // sseUrl returns the URL for the SSE stream endpoint.
        sseUrl() {
            const tok = API._token();
            const base = `${window.location.origin}/api/v1/deploy/progress/stream`;
            return tok ? `${base}?token=${encodeURIComponent(tok)}` : base;
        },
    },
};
