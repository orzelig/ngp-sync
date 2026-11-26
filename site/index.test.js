// The download page's inline script, run for real against a stubbed fetch and
// a minimal DOM (ADR 0003); and, for every site/*.html, the design-token rules
// (ADR 0004). Node built-ins only: `node --test site/index.test.js`.
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

// ---------------------------------------------------------------------------
// The design tokens (ADR 0004): site/tokens.css is the one source of every
// colour, font size and font family on ngp.one, and every page uses it.

const SITE = __dirname;
const TOKENS = {
  '--ngp-bg': '#000000',
  '--ngp-text': '#FFFFFF',
  '--ngp-text-2': '#C3C9D4',
  '--ngp-text-3': '#A1A1A6',
  '--ngp-text-4': '#8A8A90',
  '--ngp-hairline': 'rgba(255, 255, 255, 0.14)',
  '--ngp-glass-1': 'rgba(16, 16, 18, 0.92)',
  '--ngp-glass-2': 'rgba(22, 22, 24, 0.92)',
  '--ngp-glass-3': 'rgba(34, 34, 38, 0.92)',
  '--ngp-urgent': '#FFB4A0',
  '--ngp-urgent-soft': '#E3BFB4',
  '--ngp-ok': '#7DDB9C',
  '--ngp-accent': '#9CC8FF',
  '--ngp-brand-hi': '#FF8A2A',
  '--ngp-brand-lo': '#EE5A05',
  '--ngp-brand-ring': '#FFFFFF',
  '--ngp-brand-core': '#13B45A',
  '--ngp-size-40': '40px',
  '--ngp-size-28': '28px',
  '--ngp-size-20': '20px',
  '--ngp-size-17': '17px',
  '--ngp-size-15': '15px',
  '--ngp-size-14': '14px',
  '--ngp-size-12': '12px',
  '--ngp-size-11': '11px',
  '--ngp-font': '-apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif',
};

// CSS named colours, less the keywords a page may use.
const NAMED_COLOURS = new Set(
  `aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue
  blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk
  crimson cyan darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki
  darkmagenta darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen
  darkslateblue darkslategray darkslategrey darkturquoise darkviolet deeppink deepskyblue
  dimgray dimgrey dodgerblue firebrick floralwhite forestgreen fuchsia gainsboro ghostwhite
  gold goldenrod gray green greenyellow grey honeydew hotpink indianred indigo ivory khaki
  lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral lightcyan
  lightgoldenrodyellow lightgray lightgreen lightgrey lightpink lightsalmon lightseagreen
  lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime limegreen linen
  magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple mediumseagreen
  mediumslateblue mediumspringgreen mediumturquoise mediumvioletred midnightblue mintcream
  mistyrose moccasin navajowhite navy oldlace olive olivedrab orange orangered orchid
  palegoldenrod palegreen paleturquoise palevioletred papayawhip peachpuff peru pink plum
  powderblue purple rebeccapurple red rosybrown royalblue saddlebrown salmon sandybrown
  seagreen seashell sienna silver skyblue slateblue slategray slategrey snow springgreen
  steelblue tan teal thistle tomato turquoise violet wheat white whitesmoke yellow
  yellowgreen canvas canvastext linktext visitedtext activetext buttonface buttontext
  buttonborder field fieldtext highlight highlighttext mark marktext graytext`.split(/\s+/),
);
const COLOUR_LITERAL = /#[0-9a-f]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(/i;

function hasColour(value) {
  if (COLOUR_LITERAL.test(value)) return true;
  const words = value.replace(/var\([^)]*\)/g, ' ').replace(/url\([^)]*\)/g, ' ').toLowerCase();
  return words.split(/[^a-z-]+/).some((w) => NAMED_COLOURS.has(w));
}

const stripComments = (css) => css.replace(/\/\*[\s\S]*?\*\//g, '');

function readTokens() {
  const css = stripComments(fs.readFileSync(path.join(SITE, 'tokens.css'), 'utf8'));
  const m = css.match(/^\s*:root\s*\{([^{}]*)\}\s*$/);
  assert.ok(m, 'tokens.css is exactly one :root rule and nothing else');
  const decls = {};
  for (const d of m[1].split(';')) {
    if (!d.trim()) continue;
    const i = d.indexOf(':');
    const name = d.slice(0, i).trim();
    assert.ok(name.startsWith('--ngp-'), `tokens.css declares only --ngp-* properties, not ${name}`);
    assert.ok(!(name in decls), `${name} is declared once`);
    decls[name] = d.slice(i + 1).trim();
  }
  return decls;
}

function pages() {
  const names = fs.readdirSync(SITE).filter((f) => f.endsWith('.html')).sort();
  assert.ok(names.includes('index.html'), 'the page list is read from site/');
  return names.map((name) => ({ name, src: fs.readFileSync(path.join(SITE, name), 'utf8') }));
}

function styleBlocks(src) {
  return [...src.matchAll(/<style\b[^>]*>([\s\S]*?)<\/style\s*>/gi)].map((m) => stripComments(m[1]));
}

// Every declaration in a stylesheet, from the innermost rule bodies (so those
// inside @media too).
function declarations(css) {
  const out = [];
  for (const body of css.matchAll(/\{([^{}]*)\}/g)) {
    for (const d of body[1].split(';')) {
      const i = d.indexOf(':');
      if (i < 0) continue;
      out.push({ prop: d.slice(0, i).trim().toLowerCase(), value: d.slice(i + 1).trim() });
    }
  }
  return out;
}

test('tokens.css: one :root rule holding exactly the design3 tokens', () => {
  assert.deepEqual(readTokens(), TOKENS);
});

test('tokens.css: no animation, transition or colour-scheme media query', () => {
  const css = fs.readFileSync(path.join(SITE, 'tokens.css'), 'utf8');
  assert.doesNotMatch(css, /transition|animation|@keyframes|prefers-color-scheme/i);
});

for (const { name, src } of pages()) {
  test(`${name}: links /tokens.css once, before its <style>`, () => {
    const links = [...src.matchAll(/<link\b[^>]*\bhref="\/tokens\.css"[^>]*>/gi)];
    assert.equal(links.length, 1, 'exactly one link to /tokens.css');
    assert.match(links[0][0], /\brel="stylesheet"/);
    const style = src.search(/<style\b/i);
    assert.ok(style < 0 || links[0].index < style, 'the tokens link comes before <style>');
  });

  test(`${name}: uses only tokens that tokens.css defines`, () => {
    const defined = readTokens();
    const used = [...src.matchAll(/var\(\s*(--[\w-]+)/g)].map((m) => m[1]);
    assert.ok(used.length > 0, 'the page uses the tokens');
    for (const v of used) assert.ok(v in defined, `${v} is defined in tokens.css`);
  });

  test(`${name}: hard-codes no colour`, () => {
    for (const css of styleBlocks(src)) {
      for (const { prop, value } of declarations(css)) {
        assert.ok(!hasColour(value), `${prop}: ${value} has a colour literal`);
      }
    }
    for (const m of src.matchAll(/\s(style|fill|stroke|stop-color|color|bgcolor)="([^"]*)"/gi)) {
      assert.ok(!hasColour(m[2]), `${m[1]}="${m[2]}" has a colour literal`);
    }
  });

  test(`${name}: every font size and family is a token`, () => {
    for (const css of styleBlocks(src)) {
      for (const { prop, value } of declarations(css)) {
        assert.notEqual(prop, 'font', 'no font shorthand');
        if (prop === 'font-size' || prop === 'font-family') {
          assert.match(value, /^var\(--ngp-[\w-]+\)$/, `${prop}: ${value}`);
        }
      }
    }
  });

  test(`${name}: always dark, on --ngp-bg, with no animation`, () => {
    assert.doesNotMatch(src, /transition|animation|@keyframes|prefers-color-scheme/i);
    const meta = src.match(/<meta\b[^>]*\bname="theme-color"[^>]*\bcontent="([^"]*)"/i);
    assert.ok(meta, 'the page has a theme-color meta');
    assert.equal(meta[1].toLowerCase(), readTokens()['--ngp-bg'].toLowerCase());
    const css = styleBlocks(src).join('\n');
    assert.match(
      css,
      /(?:^|[\s,}])(?:html|body)\s*\{[^}]*\bbackground(?:-color)?\s*:\s*var\(--ngp-bg\)/,
      'html or body has background: var(--ngp-bg)',
    );
  });
}

// WCAG 2 contrast from the token values, glass layers composited over --ngp-bg.
function rgba(value) {
  let m = value.match(/^#([0-9a-f]{6})$/i);
  if (m) {
    const n = parseInt(m[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1];
  }
  m = value.match(/^rgba\(\s*(\d+),\s*(\d+),\s*(\d+),\s*([\d.]+)\s*\)$/);
  assert.ok(m, `colour ${value} parses`);
  return [+m[1], +m[2], +m[3], +m[4]];
}
const over = ([r, g, b, a], [R, G, B]) => [r * a + R * (1 - a), g * a + G * (1 - a), b * a + B * (1 - a), 1];
function luminance([r, g, b]) {
  const lin = (c) => {
    const s = c / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}
function contrast(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

test('contrast: every text token is at least 4.5:1 on the background and on each glass layer', () => {
  const t = readTokens();
  const bg = rgba(t['--ngp-bg']);
  const surfaces = {
    '--ngp-bg': bg,
    '--ngp-glass-1': over(rgba(t['--ngp-glass-1']), bg),
    '--ngp-glass-2': over(rgba(t['--ngp-glass-2']), bg),
    '--ngp-glass-3': over(rgba(t['--ngp-glass-3']), bg),
  };
  for (const text of ['--ngp-text', '--ngp-text-2', '--ngp-text-3', '--ngp-text-4', '--ngp-accent']) {
    for (const [surface, colour] of Object.entries(surfaces)) {
      const ratio = contrast(rgba(t[text]), colour);
      assert.ok(ratio >= 4.5, `${text} on ${surface} is ${ratio.toFixed(2)}:1`);
    }
  }
  const onAccent = contrast(bg, rgba(t['--ngp-accent']));
  assert.ok(onAccent >= 4.5, `--ngp-bg on --ngp-accent is ${onAccent.toFixed(2)}:1`);
});

// ngp-sync#14: the privacy policy says what NGP Phone shares when it joins.
test('privacy.html: says what NGP Phone shares when it joins the account', () => {
  const src = fs.readFileSync(path.join(SITE, 'privacy.html'), 'utf8');
  const item = [...src.matchAll(/<li>([\s\S]*?)<\/li>/g)].map((m) => m[1].replace(/\s+/g, ' '))
    .find((li) => li.includes('NGP Phone'));
  assert.ok(item, 'a "What we collect" item names NGP Phone');
  for (const phrase of ['device name', 'sign-in tokens', 'turn on sync on the phone',
    'reads your synced data from sync.ngp.one', 'sends its device info']) {
    assert.ok(item.includes(phrase), `the NGP Phone item says "${phrase}"`);
  }
});
