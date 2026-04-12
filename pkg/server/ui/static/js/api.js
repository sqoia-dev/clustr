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

    // Convenience methods.
    images: {
        list(status = '')     { return API.get('/images', status ? { status } : {}); },
        get(id)               { return API.get(`/images/${id}`); },
        archive(id)           { return API.del(`/images/${id}`); },
        diskLayout(id)        { return API.get(`/images/${id}/disklayout`); },
    },
    nodes: {
        list()                { return API.get('/nodes'); },
        get(id)               { return API.get(`/nodes/${id}`); },
        create(body)          { return API.post('/nodes', body); },
        update(id, body)      { return API.put(`/nodes/${id}`, body); },
        del(id)               { return API.del(`/nodes/${id}`); },
        power: {
            status(id)        { return API.get(`/nodes/${id}/power`); },
            on(id)            { return API.post(`/nodes/${id}/power/on`); },
            off(id)           { return API.post(`/nodes/${id}/power/off`); },
            cycle(id)         { return API.post(`/nodes/${id}/power/cycle`); },
            reset(id)         { return API.post(`/nodes/${id}/power/reset`); },
            pxeBoot(id)       { return API.post(`/nodes/${id}/power/pxe`); },
            diskBoot(id)      { return API.post(`/nodes/${id}/power/disk`); },
        },
        sensors(id)           { return API.get(`/nodes/${id}/sensors`); },
    },
    logs: {
        query(params = {})    { return API.get('/logs', params); },
    },
    factory: {
        pull(body)            { return API.post('/factory/pull', body); },
        importISO(body)       { return API.post('/factory/import-iso', body); },

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
    health: {
        get()                 { return API.get('/health'); },
    },
};
