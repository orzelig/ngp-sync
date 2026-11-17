// The download page's inline script, run for real against a stubbed fetch and
// a minimal DOM (ADR 0003). Node built-ins only: `node --test site/index.test.js`.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const MANIFEST_PATH = '/update/macos-arm64.json';
const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');

function scripts() {
  return [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi)];
}

function pageScript() {
  const found = scripts();
  assert.equal(found.length, 1, 'site/index.html has exactly one <script> element');
  assert.equal(found[0][1].trim(), '', 'the <script> element has no attributes (no src)');
  return found[0][2];
}

function fallback() {
  const a = html.match(/<a\b[^>]*\bid="dl"[^>]*>/);
  assert.ok(a, 'the page has an element with id="dl"');
  const href = a[0].match(/\bhref="([^"]*)"/);
  assert.ok(href, 'the dl element has an href');
  const ver = html.match(/<span\b[^>]*\bid="ver"[^>]*>([^<]*)<\/span>/);
  assert.ok(ver, 'the page has a span with id="ver"');
  return { href: href[1], version: ver[1] };
}

const settle = () => new Promise((resolve) => setImmediate(resolve));

// Runs the page's script with `fetchImpl` and returns what it left in the DOM.
async function run(fetchImpl) {
  const code = pageScript();
  const fb = fallback();
  const dl = { href: fb.href };
  const ver = { textContent: fb.version };
  const calls = [];
  const rejections = [];
  const onRejection = (reason) => rejections.push(reason);
  process.on('unhandledRejection', onRejection);
  try {
    const context = {
      fetch: (...args) => {
        calls.push(args);
        return fetchImpl(...args);
      },
      document: {
        getElementById: (id) => (id === 'dl' ? dl : id === 'ver' ? ver : null),
      },
    };
    vm.runInNewContext(code, context);
    await settle();
    await settle();
  } finally {
    process.off('unhandledRejection', onRejection);
  }
  assert.deepEqual(rejections, [], 'the script leaves no unhandled rejection');
  return { href: dl.href, version: ver.textContent, calls, fb };
}

const okJson = (body) => () =>
  Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });

const NEW_VERSION = '156.0.8078.79-ngp.30';
const NEW_URL =
  'https://github.com/orzelig/ngp-releases/releases/download/v156.0.8078.79-ngp.30/NGP.dmg';

function assertFallback(r) {
  assert.equal(r.href, r.fb.href);
  assert.equal(r.version, r.fb.version);
}

test('markup: one inline script, and a well-formed fallback link and version', () => {
  pageScript();
  const fb = fallback();
  assert.ok(
    fb.href.startsWith('https://github.com/orzelig/ngp-releases/releases/download/'),
    `fallback href ${fb.href}`,
  );
  assert.match(fb.version, /^\d+\.\d+\.\d+\.\d+-ngp\.\d+$/);
});

test('whole manifest: the link and version are the manifest\'s', async () => {
  const r = await run(
    okJson({ version: NEW_VERSION, url: NEW_URL, sha256: 'ab'.repeat(32), sig: 'c2ln' }),
  );
  assert.equal(r.href, NEW_URL);
  assert.equal(r.version, NEW_VERSION);
  assert.equal(r.calls.length, 1);
  assert.equal(r.calls[0][0], MANIFEST_PATH);
  assert.equal(r.calls[0][1].cache, 'no-store');
});

test('non-OK response keeps the fallback', async () => {
  let read = false;
  const r = await run(() =>
    Promise.resolve({
      ok: false,
      status: 404,
      json: () => {
        read = true;
        return Promise.resolve({ version: NEW_VERSION, url: NEW_URL });
      },
    }),
  );
  assertFallback(r);
  assert.equal(read, false, 'the body of a non-OK response is not used');
});

test('network failure keeps the fallback', async () => {
  assertFallback(await run(() => Promise.reject(new TypeError('network down'))));
});

test('a body that is not JSON keeps the fallback', async () => {
  assertFallback(
    await run(() =>
      Promise.resolve({ ok: true, status: 200, json: () => Promise.reject(new SyntaxError('bad')) }),
    ),
  );
});

for (const [name, body] of [
  ['null', null],
  ['an array', []],
  ['a string', 'manifest'],
]) {
  test(`JSON ${name} keeps the fallback`, async () => {
    assertFallback(await run(okJson(body)));
  });
}

for (const [name, body] of [
  ['url only', { url: NEW_URL }],
  ['version only', { version: NEW_VERSION }],
  ['empty url', { url: '', version: NEW_VERSION }],
  ['empty version', { url: NEW_URL, version: '' }],
  ['non-string version', { url: NEW_URL, version: 30 }],
  ['non-string url', { url: {}, version: NEW_VERSION }],
]) {
  test(`incomplete manifest (${name}) keeps the fallback and applies neither field`, async () => {
    assertFallback(await run(okJson(body)));
  });
}
