const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const html = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/index.html'), 'utf8');
function section(start, end) {
  const from = html.indexOf(start);
  const to = html.indexOf(end, from);
  assert.ok(from >= 0 && to > from, `UI section is missing: ${start}`);
  return html.slice(from, to);
}
const escape = value => String(value ?? '').replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
function formatter() {
  const context = vm.createContext({
    esc: escape, number: value => String(value),
    datum: (label, value) => `<div>${escape(label)}: ${escape(value)}</div>`,
  });
  vm.runInContext(section('    function networkToolName(', '    function logMarkup('), context);
  return context;
}

test('network badges and details appear only for positive successful calls', () => {
  const view = formatter();
  const supplied = { server_tools: ['vercel:exa_search'] };
  const zero = { ...supplied, server_tool_calls_reported: true, server_tool_calls: {} };
  for (const row of [{}, supplied, zero, { ...zero, server_tool_calls: { exa_search: 0 } }]) {
    assert.equal(view.networkLogBadge(row), '');
    assert.equal(view.networkLogDetails(row), '');
  }

  const actual = { ...supplied, server_tool_calls_reported: true, server_tool_calls: { exa_search: 2 } };
  assert.match(view.networkLogBadge(actual), /good/);
  assert.match(view.networkLogBadge(actual), /搜索 · Exa ×2/);
  assert.match(view.networkLogDetails(actual), /提供的搜索工具: Exa/);
  assert.match(view.networkLogDetails(actual), /成功搜索调用: Exa ×2/);
  assert.equal(view.networkLogBadge({ ...actual, upstream_skipped: true }), '');
  assert.equal(view.networkLogDetails({ ...actual, upstream_skipped: true }), '');
});

test('multi-tool details show partial retry reporting and escape metadata names', () => {
  const view = formatter();
  const row = {
    server_tools: ['vercel:exa_search', 'vercel:tako_search'],
    server_tool_calls_reported: true,
    server_tool_calls: { exa_search: 2, tako_search: 1 },
    attempts: [
      { server_tools: ['vercel:exa_search'] },
      { server_tools: ['vercel:exa_search'], server_tool_calls_reported: true, server_tool_calls: { exa_search: 2, tako_search: 1 } },
    ],
  };
  const badge = view.networkLogBadge(row);
  assert.match(badge, /搜索 · 3 次/);
  assert.match(badge, /Exa ×2、Tako ×1/);
  assert.match(badge, /1 次上游尝试未报告/);
  assert.match(view.networkLogDetails(row), /部分尝试未报告/);
  const escaped = view.networkLogBadge({ server_tool_calls: { '<img onerror="alert(1)">': 1 } });
  assert.doesNotMatch(escaped, /<img/);
  assert.match(escaped, /&lt;img/);
});

test('network filter resets pagination and sends the filter to the log API', async () => {
  const nodes = new Map();
  const calls = [];
  function node(id) {
    if (!nodes.has(id)) nodes.set(id, {
      checked: false, value: '', listeners: {},
      addEventListener(type, callback) { this.listeners[type] = callback; },
    });
    return nodes.get(id);
  }
  const context = vm.createContext({
    $: node, PAGE_SIZE: 50, AbortController,
    state: { page: 4, providers: new Set() },
    syncLogsRefreshButton() {}, renderProviderOptions() {}, globalError() {},
    number: value => String(value), esc: escape, actualProvider: () => '',
    logMarkup: () => '',
    api: async (url, options) => {
      calls.push({ url, query: options.query });
      return { items: [], total: 0, offset: 0 };
    },
  });
  const handler = html.match(/^    \$\('networkOnly'\)\.addEventListener\('change'.+$/m);
  assert.ok(handler, 'network filter change handler is missing');
  vm.runInContext(section('    async function loadLogs()', '    function syncLogsRefreshButton()') + '\n' + handler[0], context);
  node('networkOnly').checked = true;
  node('networkOnly').listeners.change();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(context.state.page, 0);
  assert.equal(calls[0].url, '/logs');
  assert.equal(calls[0].query.network, 'used');
  assert.equal(calls[0].query.offset, 0);
  node('networkOnly').checked = false;
  node('networkOnly').listeners.change();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(calls[1].query.network, '');
});
