import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const go = readFileSync(new URL('../management.go', import.meta.url), 'utf8');
const script = go.match(/<script>([\s\S]*?)<\/script>/)[1];

function browser({ embedded = true, search = '?parent_origin=https://panel.test' } = {}) {
  const elements = new Map();
  const listeners = new Map();
  const stored = new Map([['reset-manager-management-key-v1', 'old-fixture']]);
  const sent = [];
  const redirects = [];
  let loads = 0;
  const parent = { postMessage: (...args) => sent.push(args) };
  const window = { addEventListener: (name, fn) => listeners.set(name, fn) };
  window.parent = embedded ? parent : window;
  const document = {
    readyState: 'loading', addEventListener() {},
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, { value: id === 'locale' ? 'en' : '', style: {}, hidden: false });
      return elements.get(id);
    },
    querySelector: () => document.getElementById('layout'),
  };
  const context = vm.createContext({ window, document, URL, URLSearchParams, console,
    location: { origin: 'https://proxy.test', pathname: '/gateway/v0/resource/plugins/reset-manager/status', search, replace: (url) => redirects.push(url) },
    localStorage: { getItem: (k) => stored.get(k), removeItem: (k) => stored.delete(k) },
    recordLoad: () => { loads++; },
  });
  vm.runInContext(script, context);
  vm.runInContext('loadStatus = async function () { recordLoad(); }', context);
  const response = (overrides = {}) => listeners.get('message')({
    source: parent, origin: 'https://panel.test',
    data: { type: 'cliproxy-plugin-session', pluginId: 'reset-manager',
      apiBase: 'https://proxy.test/gateway', managementKey: 'current-fixture' }, ...overrides,
  });
  return { context, elements, stored, sent, redirects, parent, response, loads: () => loads };
}

test('host session authenticates immediately without a second password or stored plugin key', () => {
  const page = browser();
  vm.runInContext('initializeHostSession()', page.context);
  assert.equal(page.sent[0][1], 'https://panel.test');
  page.response();
  assert.equal(vm.runInContext('authHeaders().authorization', page.context), 'Bearer current-fixture');
  assert.equal(page.elements.get('managementKey').value, '');
  assert.equal(page.elements.get('manualConnection').style.display, 'none');
  assert.equal(page.stored.has('reset-manager-management-key-v1'), false);
  assert.equal(page.loads(), 1);
});

test('ignores a different source, origin, plugin, server, or malformed response', () => {
  const page = browser();
  vm.runInContext('initializeHostSession()', page.context);
  page.response({ source: {} });
  page.response({ origin: 'https://evil.test' });
  for (const change of [{ pluginId: 'other' }, { apiBase: 'https://other.test' }, { apiBase: undefined }]) {
    page.response({ data: { type: 'cliproxy-plugin-session', pluginId: 'reset-manager',
      apiBase: 'https://proxy.test/gateway', managementKey: 'current-fixture', ...change } });
  }
  assert.equal(page.loads(), 0);
  assert.equal(vm.runInContext('currentManagementKey()', page.context), '');
});

test('host logout clears the in-memory credential and hides the account view', () => {
  const page = browser();
  vm.runInContext('initializeHostSession()', page.context);
  page.response();
  page.response({ data: { type: 'cliproxy-plugin-session', pluginId: 'reset-manager',
    apiBase: 'https://proxy.test/gateway', managementKey: '' } });
  assert.equal(vm.runInContext('currentManagementKey()', page.context), '');
  assert.equal(page.elements.get('layout').style.display, 'none');
  assert.throws(() => vm.runInContext('authHeaders()', page.context));
});

test('direct bookmarks use management login; explicit standalone mode remains available', () => {
  const page = browser({ embedded: false, search: '' });
  assert.equal(vm.runInContext('initializeHostSession()', page.context), true);
  assert.deepEqual(page.redirects, ['/gateway/management.html?plugin=reset-manager#/plugin-pages/reset-manager/0']);
  const standalone = browser({ embedded: false, search: '?standalone=1' });
  assert.equal(vm.runInContext('initializeHostSession()', standalone.context), false);
  assert.equal(standalone.redirects.length, 0);
});
