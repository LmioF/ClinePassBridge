const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const source = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/scheduling.js'), 'utf8');
const end = source.indexOf("$('credentialList').addEventListener('change'");
assert.ok(end > 0, 'scheduling functions must precede credential event handlers');

function scheduling(enabled, paused) {
  const item = { id: 'one', label: '测试账号', enabled, quota_paused: paused };
  const nodes = new Map();
  const toggleLabel = {};
  const toggle = { setAttribute() {}, closest: () => toggleLabel };
  nodes.set('.credential-enabled', toggle);
  for (const selector of ['.credential-quota-recovery', '.credential-schedule-badges', '.credential-schedule-detail', '.credential-toggle-label', '.reset-cooldown']) {
    nodes.set(selector, {});
  }
  const row = {
    dataset: { id: item.id },
    querySelector: selector => nodes.get(selector),
  };
  const list = { children: [row] };
  const state = {};
  const calls = [];
  const errors = [];
  const syncs = { models: 0, affinity: 0 };
  const account = { id: item.id, enabled, quota_blocked: paused };
  const context = vm.createContext({
    esc: value => String(value ?? ''),
    authGate: { verified: true, paused: null },
    credentialView: { items: [item] },
    $: id => id === 'credentialList' ? list : state,
    syncCredentialCollapse() {},
    syncModelTestCredentials: () => syncs.models++,
    syncClearAffinityControls: () => syncs.affinity++,
    toast: message => errors.push(message),
    api: async (url, options) => {
      calls.push({ url, options });
      if (url === '/scheduling') return { accounts: [{ ...account }] };
      assert.equal(url, '/scheduling/credential');
      account.enabled = options.body.enabled;
      return { id: item.id, enabled: account.enabled };
    },
  });
  vm.runInContext(source.slice(0, end) + `
    globalThis.view = {
      toggle: credentialToggleState, badges: credentialScheduleBadges,
      update: updateCredentialScheduling, change: changeCredentialScheduling,
      load: loadCredentialScheduling, pending: credentialScheduling.pending,
      paused: credentialQuotaPaused
    };
  `, context);
  return {
    ...context.view, item, row, nodes, account, calls, syncs, errors,
    render: () => context.view.update(),
  };
}

test('manual state alone controls checked; quota pause keeps its label and reset protection', () => {
  for (const enabled of [true, false]) for (const paused of [true, false]) {
    const view = scheduling(enabled, paused);
    const state = view.toggle(view.item);
    assert.equal(state.checked, enabled);
    assert.equal(state.disabled, false);
    assert.equal(state.label, !enabled ? '已停用' : paused ? '自动暂停' : '启用');
    assert.equal(view.badges(view.item).includes('人工停用'), !enabled);
    assert.equal(view.badges(view.item).includes('自动暂停'), paused);
    if (paused) {
      assert.match(state.title, /额度耗尽/);
      assert.match(state.title, enabled ? /当前为人工启用/ : /可切换为人工启用/);
      assert.match(state.title, enabled ? /额度复查确认恢复后/ : /切换后仍需等待额度恢复/);
    }
    view.render();
    assert.equal(view.nodes.get('.credential-enabled').checked, enabled);
    assert.equal(view.nodes.get('.reset-cooldown').disabled, !enabled || paused);
    view.pending.add(view.item.id);
    view.update();
    assert.equal(view.nodes.get('.credential-enabled').disabled, true);
    assert.equal(view.nodes.get('.reset-cooldown').disabled, true);
  }
});

test('paused accounts accept both manual toggle directions while reset sends no request', async () => {
  for (const enabled of [true, false]) {
    const view = scheduling(enabled, true);
    await view.change(view.item.id, 'reset');
    assert.equal(view.calls.length, 0);
    await view.change(view.item.id, 'toggle', !enabled);
    const writes = view.calls.filter(call => call.options?.method === 'PUT');
    assert.equal(writes.length, 1);
    assert.equal(writes[0].url, '/scheduling/credential');
    assert.equal(writes[0].options.body.id, view.item.id);
    assert.equal(writes[0].options.body.enabled, !enabled);
    assert.equal(view.item.enabled, !enabled);
    assert.equal(view.paused(view.item), true);
    assert.equal(view.nodes.get('.credential-enabled').checked, !enabled);
    assert.equal(view.nodes.get('.credential-enabled').disabled, false);
    assert.equal(view.nodes.get('.reset-cooldown').disabled, true);
    assert.equal(view.pending.size, 0);
    assert.deepEqual(view.errors, []);
    const count = view.calls.length;
    await view.change(view.item.id, 'reset');
    assert.equal(view.calls.length, count);
  }
});

test('pending blocks duplicate manual toggle requests during quota pause', async () => {
  const view = scheduling(false, true);
  view.pending.add(view.item.id);
  await view.change(view.item.id, 'toggle', true);
  assert.equal(view.calls.length, 0);
  assert.equal(view.item.enabled, false);
});

test('scheduling refresh synchronizes manual enabled changes even when quota pause is unchanged', async () => {
  for (const paused of [true, false]) {
    const view = scheduling(true, paused);
    view.account.enabled = false;
    await view.load();
    assert.equal(view.item.enabled, false);
    assert.equal(view.nodes.get('.credential-enabled').checked, false);
    assert.equal(view.nodes.get('.credential-enabled').disabled, false);
    assert.equal(view.syncs.models, 1);
    assert.equal(view.syncs.affinity, 1);
    await view.load();
    assert.equal(view.syncs.models, 1);
    assert.equal(view.syncs.affinity, 1);
    assert.deepEqual(view.errors, []);
  }
});
