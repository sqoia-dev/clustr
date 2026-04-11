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
    },
    logs: {
        query(params = {})    { return API.get('/logs', params); },
    },
    factory: {
        pull(body)            { return API.post('/factory/pull', body); },
        importISO(body)       { return API.post('/factory/import-iso', body); },
    },
    health: {
        get()                 { return API.get('/health'); },
    },
};
