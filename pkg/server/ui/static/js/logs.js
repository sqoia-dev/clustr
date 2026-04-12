// logs.js — SSE log streaming and log line rendering.

class LogStream {
    constructor(container, filters = {}) {
        this.container = container;
        this.filters = filters;
        this.source = null;
        this.autoScroll = true;
        this._userScrolled = false;
        this._onConnect = null;
        this._onDisconnect = null;

        // Detect manual scroll-up to pause auto-scroll.
        this.container.addEventListener('scroll', () => {
            const atBottom = this.container.scrollHeight - this.container.scrollTop <= this.container.clientHeight + 40;
            if (!atBottom) {
                this._userScrolled = true;
                this.autoScroll = false;
            } else {
                this._userScrolled = false;
                this.autoScroll = true;
            }
        });
    }

    connect() {
        this.disconnect();

        const url = new URL('/api/v1/logs/stream', window.location.origin);
        const tok = document.querySelector('meta[name="clonr-token"]');
        if (tok && tok.content) url.searchParams.set('token', tok.content);

        // Apply filters as query params.
        const { mac, hostname, level, component } = this.filters;
        if (mac)       url.searchParams.set('mac', mac);
        if (hostname)  url.searchParams.set('hostname', hostname);
        if (level)     url.searchParams.set('level', level);
        if (component) url.searchParams.set('component', component);

        this.source = new EventSource(url.toString());

        this.source.onopen = () => {
            if (this._onConnect) this._onConnect();
        };

        this.source.onerror = () => {
            if (this._onDisconnect) this._onDisconnect();
            // Auto-reconnect after 3 seconds on error.
            if (this.source) {
                this.source.close();
                this.source = null;
                setTimeout(() => {
                    if (this._shouldReconnect) this.connect();
                }, 3000);
            }
        };

        this.source.onmessage = (evt) => {
            try {
                const entry = JSON.parse(evt.data);
                this.appendEntry(entry);
            } catch (_) {}
        };

        this._shouldReconnect = true;
    }

    disconnect() {
        this._shouldReconnect = false;
        if (this.source) {
            this.source.close();
            this.source = null;
            if (this._onDisconnect) this._onDisconnect();
        }
    }

    get connected() {
        return this.source !== null && this.source.readyState !== EventSource.CLOSED;
    }

    setFilters(filters) {
        this.filters = filters;
        if (this.connected) this.connect(); // reconnect with new filters
    }

    onConnect(fn)    { this._onConnect = fn; }
    onDisconnect(fn) { this._onDisconnect = fn; }

    setAutoScroll(enabled) {
        this.autoScroll = enabled;
        if (enabled) this._scrollToBottom();
    }

    appendEntry(entry) {
        const line = this._renderLine(entry);
        this.container.appendChild(line);

        // Keep buffer bounded to prevent unbounded memory growth.
        while (this.container.children.length > 2000) {
            this.container.removeChild(this.container.firstChild);
        }

        if (this.autoScroll) this._scrollToBottom();
    }

    // Render a batch of log entries (from REST query) without auto-scroll until done.
    loadEntries(entries) {
        this.container.innerHTML = '';
        entries.forEach(e => this.container.appendChild(this._renderLine(e)));
        this._scrollToBottom();
    }

    clear() {
        this.container.innerHTML = '';
    }

    _scrollToBottom() {
        this.container.scrollTop = this.container.scrollHeight;
    }

    _renderLine(entry) {
        const div = document.createElement('div');
        const level = (entry.level || 'info').toLowerCase();
        div.className = `log-line log-line-${level}`;

        const ts = new Date(entry.timestamp);
        const tsStr = ts.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });

        const levelStr = level.toUpperCase().padEnd(5);

        div.innerHTML =
            `<span class="log-ts">${escHtml(tsStr)}</span>` +
            `<span class="log-level log-level-${escHtml(level)}">${escHtml(levelStr)}</span>` +
            (entry.component ? `<span class="log-component">[${escHtml(entry.component)}]</span>` : '') +
            (entry.hostname  ? `<span class="log-host">[${escHtml(entry.hostname)}]</span>` : '') +
            `<span class="log-msg">${escHtml(entry.message)}</span>`;

        return div;
    }
}

function escHtml(str) {
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;');
}
