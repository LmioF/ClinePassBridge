const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const root = path.join(__dirname, '../internal/bridge/ui');
const scheduling = fs.readFileSync(path.join(root, 'scheduling.js'), 'utf8');
const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const schedulingEnd = scheduling.indexOf("$('credentialList').addEventListener('change'");
const renderStart = html.indexOf('    function renderCredentials(');
const renderEnd = html.indexOf('    function usageViewVisible()', renderStart);
assert.ok(schedulingEnd > 0 && renderStart >= 0 && renderEnd > renderStart);

const at = day => `2030-01-${String(day).padStart(2, '0')}T00:00:00Z`;
const credential = (id, days = [], extra = {}) => ({
  id, label: id, enabled: true, quota_paused: true,
  quota_block: { limits: days.map(day => ({ resets_at: at(day) })) }, ...extra,
});
const normals = () => Array.from({ length: 12 }, (_, i) => credential(`normal-${i}`, [], {
  quota_paused: false, enabled: i !== 4, priority: 12 - i,
}));
const ids = items => Array.from(items, item => item.id);

function page(items) {
  const nodes = new Map();
  let builds = 0;
  const context = vm.createContext({
    credentialView: {
      items, page: 0, visible: [], usage: new Map(), pending: new Map(),
      queue: new Map(), expanded: new Set(),
    },
    CREDENTIAL_PAGE_SIZE: 12,
    authGate: { verified: true, paused: null },
    $: id => {
      if (!nodes.has(id)) nodes.set(id, { value: '', children: [], textContent: '' });
      return nodes.get(id);
    },
    esc: value => String(value ?? ''), shown: value => value,
    refreshEstimateDialog() {}, syncModelTestCredentials() {}, syncClearAffinityControls() {},
    syncCredentialCollapse() {}, refreshVisibleUsage() {},
    usageStatusHTML: () => '', usageHTML: () => '', usagePlanHTML: () => '', buttonIcon: () => '',
    api: async url => { assert.equal(url, '/scheduling'); return { accounts: structuredClone(context.accounts) }; },
    accounts: [],
  });
  vm.runInContext(scheduling.slice(0, schedulingEnd) + `
    globalThis.order = {
      resetAt: credentialQuotaResetAt, compare: compareCredentialDisplayOrder,
      load: loadCredentialScheduling,
      setAccounts: rows => { credentialScheduling.accounts = new Map(rows.map(a => [a.id, a])); }
    };
  `, context);
  vm.runInContext(html.slice(renderStart, renderEnd), context);
  const render = context.renderCredentials;
  context.renderCredentials = (...args) => { builds++; return render(...args); };
  return {
    context, nodes, ...context.order,
    render: (...args) => context.renderCredentials(...args),
    visible: () => ids(context.credentialView.visible),
    get builds() { return builds; },
  };
}

test('mixed credentials sort before the 12-row page boundary and focus uses display order without mutating input', () => {
  const normal = normals();
  const items = [credential('late', [9], { enabled: false }), ...normal.slice(0, 6),
    credential('early', [2]), ...normal.slice(6)];
  const original = items.slice();
  const view = page([]);
  view.render({ items });
  assert.deepEqual(view.visible(), ids(normal));
  assert.deepEqual(items, original);
  assert.deepEqual(ids(view.context.credentialView.items), ids(original));
  view.nodes.get('credentialSearch').value = 'normal';
  view.render(undefined, 'early');
  assert.equal(view.nodes.get('credentialSearch').value, '');
  assert.equal(view.context.credentialView.page, 1);
  assert.deepEqual(view.visible(), ['early', 'late']);
  view.render(undefined, 'normal-11');
  assert.equal(view.context.credentialView.page, 0);
  assert.deepEqual(view.visible(), ids(normal));
  assert.deepEqual(ids(view.context.credentialView.items), ids(original));
});

test('quota deadlines prefer the latest blocked snapshot, otherwise the last valid window; ties and unknowns stay stable', () => {
  const items = [credential('unknown-a'), credential('multi', [2, 5]),
    credential('tie-b', [3]), credential('latest', [9]), credential('fallback', [3]),
    credential('unknown-b'), credential('tie-a', [3])];
  items[1].quota_block.limits.push({ resets_at: 'invalid' });
  const view = page(items);
  view.setAccounts([
    { id: 'latest', quota_blocked: true, retry_at: at(1) },
    { id: 'fallback', quota_blocked: true, retry_at: 'invalid' },
    { id: 'unknown-b', quota_blocked: true, retry_at: 'invalid',
      host: { next_retry_after: at(1), credential_cooldown: true },
      model_cooldowns: [{ model: 'fixture', retry_at: at(1) }] },
    { id: 'nonquota', quota_blocked: false, retry_at: at(1) },
  ]);
  assert.equal(view.resetAt(items[1]), Date.parse(at(5)));
  assert.equal(view.resetAt(items[3]), Date.parse(at(1)));
  assert.equal(view.resetAt(items[4]), Date.parse(at(3)));
  assert.equal(view.resetAt(credential('nonquota', [8])), Date.parse(at(8)));
  assert.ok(Number.isNaN(view.resetAt(items[5])));
  assert.deepEqual(ids(items.slice().sort(view.compare)), [
    'latest', 'tie-b', 'fallback', 'tie-a', 'multi', 'unknown-a', 'unknown-b',
  ]);
});

test('scheduling refresh reorders even off-page deadlines and recovered accounts, rebuilding only when display order changes', async () => {
  const normal = normals();
  const items = [credential('a', [3]), credential('b', [5]), ...normal];
  const view = page(items);
  view.render();
  assert.equal(view.builds, 1);
  view.context.accounts = [
    { id: 'a', enabled: true, quota_blocked: true, retry_at: at(7) },
    { id: 'b', enabled: true, quota_blocked: true, retry_at: at(5) },
  ];
  await view.load();
  assert.equal(view.builds, 2, 'off-page order changes still rebuild the list');
  assert.deepEqual(view.visible(), ids(normal));
  assert.deepEqual(ids(items.slice().sort(view.compare)).slice(-2), ['b', 'a']);
  await view.load();
  assert.equal(view.builds, 2, 'unchanged snapshots preserve the existing list');
  view.context.accounts[0].retry_at = at(8);
  await view.load();
  assert.equal(view.builds, 2, 'a changed deadline without an order change needs no rebuild');
  view.context.accounts[1].quota_blocked = false;
  await view.load();
  assert.equal(view.builds, 3);
  assert.deepEqual(view.visible(), ['b', ...ids(normal).slice(0, 11)]);
  assert.equal(view.nodes.get('credentialState').textContent, '');
});
