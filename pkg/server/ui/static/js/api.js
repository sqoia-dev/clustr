// api.js — thin fetch wrapper around the clonr-serverd REST API.
// All methods return parsed JSON or throw an Error with a message from the API.

const API = {
    BASE: '/api/v1',

    // Read the auth token from the page meta tag (set by server if CLONR_AUTH_TOKEN is configured).
    _token() {
        const meta = document.querySelector('meta[name="clonr-token"]');
        return meta ? meta.content : '';
    },

    _headers(extra = {}) {
        const h = { 'Content-Type': 'application/json', ...extra };
        const tok = this._token();
        if (tok) h['Authorization'] = `Bearer ${tok}`;
        return h;
    },

    async _parse(resp) {
        const ct = resp.headers.get('Content-Type') || '';
        if (!resp.ok) {
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
        const resp = await fetch(url.toString(), { headers: this._headers({ 'Content-Type': undefined }) });
        return this._parse(resp);
    },

    async post(path, body) {
        const resp = await fetch(this.BASE + path, {
            method: 'POST',
            headers: this._headers(),
            body: JSON.stringify(body),
        });
        return this._parse(resp);
    },

    async put(path, body) {
        const resp = await fetch(this.BASE + path, {
            method: 'PUT',
            headers: this._headers(),
            body: JSON.stringify(body),
        });
        return this._parse(resp);
    },

    async del(path) {
        const resp = await fetch(this.BASE + path, {
            method: 'DELETE',
            headers: this._headers({ 'Content-Type': undefined }),
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
    imageRoles: {
        list()              { return API.get('/image-roles'); },
    },
    nodeGroups: {
        list()              { return API.get('/node-groups'); },
        get(id)             { return API.get(`/node-groups/${id}`); },
        create(body)        { return API.post('/node-groups', body); },
        update(id, body)    { return API.put(`/node-groups/${id}`, body); },
        del(id)             { return API.del(`/node-groups/${id}`); },
    },
    health: {
        get()                 { return API.get('/health'); },
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
