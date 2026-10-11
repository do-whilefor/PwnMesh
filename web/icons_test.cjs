'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const crypto = require('node:crypto');
const {ICONS, exportName, sourceFile, buildSubset} = require('./build-icons.cjs');
const bundle = fs.readFileSync(path.join(__dirname, 'static/lucide.min.js'), 'utf8');
const svgNamespace = 'http://www.w3.org/2000/svg';

// DOM operations used by the upstream renderer. Browser acceptance covers the
// real document; these fixtures inspect every glyph and attribute deterministically.
class Element {
  constructor(tag, namespaceURI = null) {
    this.tag = tag; this.namespaceURI = namespaceURI; this.attrs = new Map(); this.children = []; this.parentNode = null;
    if (tag === 'template') this.content = new Element('#fragment');
  }
  get attributes() { return [...this.attrs].map(([name, value]) => ({name, value})); }
  setAttribute(name, value) { this.attrs.set(name, String(value)); }
  getAttribute(name) { return this.attrs.get(name) ?? null; }
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; }
  replaceChild(next, previous) {
    const index = this.children.indexOf(previous);
    assert.notEqual(index, -1);
    this.children[index] = next; next.parentNode = this; previous.parentNode = null; return previous;
  }
  querySelectorAll(selector) {
    const attribute = selector.match(/^\[([^\]]+)\]$/)?.[1], matches = [];
    for (const child of this.children) {
      if (attribute ? child.getAttribute(attribute) !== null : child.tag === selector) matches.push(child);
      matches.push(...child.querySelectorAll(selector));
    }
    return matches;
  }
  get outerHTML() { return `<${this.tag}>`; }
}
function harness(source = bundle) {
  const document = new Element('#document'), warnings = [];
  document.createElementNS = (namespace, tag) => new Element(tag, namespace);
  const context = {document, console: {warn: message => warnings.push(message)}};
  vm.runInNewContext(source, context);
  return {document, warnings, lucide: context.lucide};
}
function placeholder(root, name, attrs = {}) {
  const element = new Element('i');
  if (name !== null) element.setAttribute('data-lucide', name);
  for (const [key, value] of Object.entries(attrs)) element.setAttribute(key, value);
  return root.appendChild(element);
}
const snapshot = element => [element.tag, Object.fromEntries(element.attrs), element.children.map(snapshot)];
const metadata = glyph => JSON.parse(JSON.stringify(glyph));

test('all selected glyphs and aliases exactly preserve Lucide 1.8.0 metadata', () => {
  const {lucide} = harness(), keys = ICONS.map(exportName);
  assert.equal(ICONS.length, 42);
  assert.deepEqual(Object.keys(lucide.icons), keys);
  assert.equal(Object.getPrototypeOf(lucide.icons), null);
  assert.equal(Object.isFrozen(lucide.icons), true);
  // Fingerprint of these 42 named glyphs in the pristine, checksum-locked UMD.
  assert.equal(crypto.createHash('sha256').update(JSON.stringify(lucide.icons)).digest('hex'),
    '30a603d00fa36aee227e7003ac85f51e1237be30560528b394f4711c4abe7c52');
  for (const key of keys) assert.equal(lucide[key], lucide.icons[key]);
  assert.equal(Buffer.byteLength(bundle) < 12000, true, 'do not reintroduce the full icon catalog');
});

test('literal and dynamic application icon choices are included in the subset', () => {
  const required = new Set();
  const text = file => fs.readFileSync(path.join(__dirname, 'static', file), 'utf8');
  for (const file of ['app.js', 'forms.js', 'llm.js', 'index.html']) {
    const source = text(file);
    for (const match of source.matchAll(/data-lucide="([a-z0-9-]+)"/g)) required.add(match[1]);
    // Includes all branches of icon(condition ? 'name' : 'other-name').
    for (const call of source.matchAll(/\bicon\(([^)\n]*)\)/g)) {
      if (!call[1].includes("'")) {
        assert.ok(['i', 'name', 'actionIcons[action]', 'type.icon'].includes(call[1]),
          `Inspect the icon mapping for new dynamic argument: ${file}: ${call[1]}`);
      }
      for (const name of call[1].matchAll(/'([a-z0-9-]+)'/g)) {
        if (!['audit', 'ctf'].includes(name[1])) required.add(name[1]);
      }
    }
    for (const expression of source.matchAll(/data-lucide="\$\{([^}]+)\}"/g)) {
      for (const name of expression[1].matchAll(/'([a-z0-9-]+)'/g)) {
        if (!['success', 'error', 'pending'].includes(name[1])) required.add(name[1]);
      }
    }
  }
  for (const type of require('./static/model.js').PROJECT_TYPES) required.add(type.icon);
  const app = text('app.js'), actions = app.match(/const actionIcons = \{([^}]+)\}/);
  assert.ok(actions, 'inspect dynamic action icons when their mapping changes');
  for (const match of actions[1].matchAll(/:'([^']+)'/g)) required.add(match[1]);
  const tabs = [...app.matchAll(/\['(?:graph|materials|result)','([^']+)'/g)];
  assert.ok(tabs.length >= 3, 'inspect dynamic tab icons when their mapping changes');
  tabs.forEach(match => required.add(match[1]));
  const zoom = [...app.matchAll(/\['(?:zoom-out|zoom-in|fit-graph|arrange-graph|upstream|downstream)','([^']+)'/g)];
  assert.equal(zoom.length, 6, 'inspect dynamic graph control icons when their mapping changes');
  zoom.forEach(match => required.add(match[1]));
  for (const name of required) assert.ok(ICONS.includes(name), `Missing application icon: ${name}`);
});

test('every icon renders the original geometry, namespace and default attributes', () => {
  const {document, lucide, warnings} = harness();
  ICONS.forEach(name => placeholder(document, name));
  lucide.createIcons({attrs: {'stroke-width': 1.6}});
  document.children.forEach((svg, index) => {
    const name = ICONS[index];
    assert.equal(svg.tag, 'svg'); assert.equal(svg.namespaceURI, svgNamespace);
    assert.equal(svg.getAttribute('viewBox'), '0 0 24 24');
    assert.equal(svg.getAttribute('stroke-width'), '1.6');
    assert.equal(svg.getAttribute('aria-hidden'), 'true');
    assert.equal(svg.getAttribute('class'), `lucide lucide-${name}`);
    const glyph = metadata(lucide.icons[exportName(name)]);
    assert.deepEqual(svg.children.map(child => [child.tag, Object.fromEntries(child.attrs)]), glyph);
    assert.ok(svg.children.every(child => child.namespaceURI === svgNamespace));
  });
  assert.deepEqual(warnings, []);
});

test('attributes, accessible names and class merging survive repeated rendering', () => {
  const {document, lucide} = harness();
  placeholder(document, 'plus', {id: 'add', class: 'local shared', 'aria-label': 'Add item', 'stroke-width': 3, width: 18, 'data-action': 'create'});
  placeholder(document, 'eye', {role: 'img', title: 'Visibility'});
  placeholder(document, 'x', {'aria-hidden': false});
  const options = {attrs: {class: ['shared', 'global'], 'stroke-width': 1.6, height: 20}};
  lucide.createIcons(options);
  const [add, eye, close] = document.children;
  assert.equal(add.getAttribute('class'), 'lucide lucide-plus local shared global');
  for (const [key, value] of Object.entries({id: 'add', 'aria-label': 'Add item', 'stroke-width': '3', width: '18', height: '20', 'data-action': 'create'})) assert.equal(add.getAttribute(key), value);
  assert.equal(add.getAttribute('aria-hidden'), null);
  assert.equal(eye.getAttribute('aria-hidden'), null);
  assert.equal(eye.getAttribute('title'), 'Visibility');
  assert.equal(close.getAttribute('aria-hidden'), 'false');
  const before = document.children.map(snapshot);
  lucide.createIcons(options);
  assert.deepEqual(document.children.map(snapshot), before);
});

test('custom roots, attribute names and icon sets retain upstream behavior', () => {
  const {document, lucide} = harness(), root = document.appendChild(new Element('section'));
  const outside = placeholder(document, 'plus');
  placeholder(root, null, {'data-symbol': 'custom-shape', class: 'original'});
  const custom = [['g', {id: 'group'}, [['path', {d: 'M0 0L4 4'}]]]];
  lucide.createIcons({root, nameAttr: 'data-symbol', icons: {CustomShape: custom}, attrs: {class: 'extra'}});
  assert.equal(outside.tag, 'i');
  const svg = root.children[0];
  assert.equal(svg.getAttribute('data-symbol'), 'custom-shape');
  assert.equal(svg.getAttribute('data-lucide'), 'custom-shape');
  assert.equal(svg.getAttribute('class'), 'lucide lucide-custom-shape original extra');
  assert.deepEqual(snapshot(svg.children[0]), ['g', {id: 'group'}, [['path', {d: 'M0 0L4 4'}, []]]]);
  assert.equal(lucide.createElement(custom, {width: 32}).getAttribute('width'), '32');
});

test('nested templates and deprecated icon-name placeholders remain supported', () => {
  const {document, lucide, warnings} = harness(), template = document.appendChild(new Element('template'));
  placeholder(template.content, 'eye-off');
  const nested = template.content.appendChild(new Element('template'));
  placeholder(nested.content, 'check');
  placeholder(document, null, {'icon-name': 'arrow-left'});
  lucide.createIcons();
  assert.equal(template.content.children[0].tag, 'i');
  assert.equal(document.children[1].tag, 'svg');
  assert.equal(document.children[1].getAttribute('data-lucide'), 'arrow-left');
  assert.match(warnings[0], /deprecated icon-name/);
  lucide.createIcons({inTemplates: true});
  assert.equal(template.content.children[0].tag, 'svg');
  assert.equal(nested.content.children[0].tag, 'svg');
});

test('unknown icons are retained and empty custom icon maps fail explicitly', () => {
  const {document, lucide, warnings} = harness(), unknown = placeholder(document, 'missing-icon');
  const inherited = placeholder(document, 'constructor');
  lucide.createIcons();
  assert.equal(document.children[0], unknown); assert.equal(document.children[1], inherited);
  assert.equal(warnings.length, 2);
  assert.throws(() => lucide.createIcons({icons: {}}), /Please provide an icons object/);
});

test('regeneration rejects altered or unsupported upstream code before evaluation', () => {
  assert.throws(() => buildSubset(Buffer.from('throw new Error("must not execute")')), /checksum mismatch/);
  assert.throws(() => buildSubset(Buffer.from(bundle)), /checksum mismatch/);
});

test('regeneration and DOM output match the pristine upstream bundle', {
  skip: process.env.LUCIDE_UPSTREAM ? false : 'Optional: set LUCIDE_UPSTREAM to the pristine lucide@1.8.0 package or UMD file'
}, () => {
  const upstream = fs.readFileSync(sourceFile(process.env.LUCIDE_UPSTREAM));
  assert.equal(buildSubset(upstream), bundle);
  const render = source => {
    const {document, lucide, warnings} = harness(source);
    ICONS.forEach((name, index) => placeholder(document, name, {class: 'original shared', 'aria-label': `Icon ${index}`, width: 18}));
    const options = {attrs: {class: ['shared', 'global'], 'stroke-width': 1.6}};
    lucide.createIcons(options); lucide.createIcons(options);
    return {tree: document.children.map(snapshot), warnings};
  };
  assert.deepEqual(render(bundle), render(upstream.toString('utf8')));
});
