const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');
const { test } = require('node:test');

const html = fs.readFileSync(path.join(__dirname, '../templates/index.html'), 'utf8');
const script = html.slice(html.indexOf('        const deviceSessionId ='), html.lastIndexOf('</script>'));

// Run the actual page lifecycle. Only browser APIs are replaced: EventSource
// tracks open connections, and timers let a queued retry run deterministically.
function page(hidden = false) {
    const windowEvents = new Map(), documentEvents = new Map(), timers = new Map();
    const streams = [], heartbeats = [];
    let nextTimer = 0;
    const document = { hidden, addEventListener: (name, fn) => documentEvents.set(name, fn) };
    const window = { crypto: webcrypto, addEventListener: (name, fn) => windowEvents.set(name, fn), close() {} };
    class EventSource {
        static CLOSED = 2;
        constructor(url) { this.url = url; this.readyState = 1; streams.push(this); }
        close() { this.readyState = 2; }
    }
    const context = vm.createContext({
        window, document, crypto: webcrypto, EventSource, navigator: { userAgent: 'Chrome', platform: 'Linux' },
        sessionStorage: { getItem() { return null; }, setItem() {} },
        setTimeout(fn) { const id = ++nextTimer; timers.set(id, fn); return id; },
        clearTimeout(id) { timers.delete(id); }, setInterval() {},
        fetch: async (url, options) => { heartbeats.push(JSON.parse(options.body)); return { ok: true, json: async () => ({ locked: false }) }; },
        location: { replace() {} }, console: { log() {} }
    });
    vm.runInContext(script, context);
    const emitWindow = (name, event = {}) => windowEvents.get(name)?.(event);
    return {
        streams, heartbeats, context,
        load() { emitWindow('load'); },
        visibility(value) { document.hidden = value; documentEvents.get('visibilitychange')?.(); },
        hide() { emitWindow('pagehide'); },
        restore() { emitWindow('pageshow', { persisted: true }); },
        retry() { const queued = [...timers.values()]; timers.clear(); for (const fn of queued) fn(); },
        active() { return streams.filter(stream => stream.readyState !== 2); }
    };
}

test('hidden page load leaves HTTP connection slots available', () => {
    const p = page(true); p.load();
    assert.equal(p.active().length, 0);
});

test('switching between four tabs leaves only the visible tab subscribed', () => {
    const pages = [];
    for (let i = 0; i < 4; i++) {
        if (pages.length) pages.at(-1).visibility(true);
        const p = page(); p.load(); pages.push(p);
    }
    assert.equal(pages.reduce((sum, p) => sum + p.active().length, 0), 2);
    pages.at(-1).visibility(true);
    pages[0].visibility(false);
    assert.equal(pages.reduce((sum, p) => sum + p.active().length, 0), 2);
});

test('visibility resumes both subscriptions without duplicating them', () => {
    const p = page(); p.load(); p.visibility(true); p.visibility(false); p.visibility(false);
    assert.equal(p.active().length, 2);
    assert.equal(p.active().filter(stream => stream.url.startsWith('/api/updates')).length, 1);
    assert.equal(p.active().filter(stream => stream.url.startsWith('/api/v1/device/events')).length, 1);
});

test('hidden page and pagehide cannot reopen streams through queued retries', () => {
    const p = page(); p.load();
    for (const stream of p.active()) stream.onerror();
    p.visibility(true); p.retry();
    assert.equal(p.active().length, 0);
    p.visibility(false);
    for (const stream of p.active()) stream.onerror();
    p.hide(); p.retry();
    assert.equal(p.active().length, 0);
    p.restore();
    assert.equal(p.active().length, 2);
});

test('hiding preserves content sequence for catch-up after visibility resumes', () => {
    const p = page(); p.load();
    vm.runInContext('lastEventSequence = 42', p.context);
    p.visibility(true); p.visibility(false);
    assert.ok(p.active().some(stream => stream.url === '/api/updates?since=42'));
});

test('device lock stops all streams and prevents later retry or visibility reconnect', () => {
    const p = page(); p.load();
    const device = p.active().find(stream => stream.url.startsWith('/api/v1/device/events'));
    device.onmessage({ data: '{"type":"lock"}' });
    p.retry(); p.visibility(true); p.visibility(false);
    assert.equal(p.active().length, 0);
});
