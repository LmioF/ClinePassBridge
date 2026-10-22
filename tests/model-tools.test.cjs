const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const root = path.join(__dirname, '../internal/bridge/ui');
const source = fs.readFileSync(path.join(root, 'model-tools.js'), 'utf8');
const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const loadConfigStart = html.indexOf('    async function loadConfig()');
const loadConfigEnd = html.indexOf('    let tabRevision = 0;', loadConfigStart);
assert.ok(loadConfigStart >= 0 && loadConfigEnd > loadConfigStart);
const loadConfig = html.slice(loadConfigStart, loadConfigEnd);
const clone = value => JSON.parse(JSON.stringify(value));

function page() {
  const nodes = new Map();
  const state = { models: [
    { id: 'alpha', upstream_id: 'cline-pass/alpha' },
    { id: 'beta', upstream_id: 'cline-pass/beta' },
  ] };
  const server = {
    authenticated: true, failTools: false, writes: [], toolReads: 0,
    models: { alpha: ['vercel:exa_search'], beta: [] },
    catalog: [{ type: 'vercel:exa_search', label: 'Exa', note: '搜索' }],
  };
  function node(id) {
    if (!nodes.has(id)) {
      const listeners = new Map();
      nodes.set(id, {
        value: '', textContent: '', hidden: true, disabled: false, open: false,
        boxes: [],
        set innerHTML(value) {
          this.html = value;
          this.boxes = [...value.matchAll(/<input type="checkbox" value="([^"]+)"( checked)?>/g)]
            .map(([, value, checked]) => ({ value, checked: !!checked, disabled: false }));
        },
        get innerHTML() { return this.html || ''; },
        addEventListener(type, callback) {
          if (!listeners.has(type)) listeners.set(type, []);
          listeners.get(type).push(callback);
        },
        async emit(type, event = {}) {
          await Promise.all((listeners.get(type) || []).map(callback => callback(event)));
        },
        querySelectorAll(selector) {
          return selector === 'input:checked' ? this.boxes.filter(box => box.checked) : this.boxes;
        },
        showModal() { this.open = true; },
        close() { this.open = false; return this.emit('close'); },
      });
    }
    return nodes.get(id);
  }
  const context = vm.createContext({
    $: node, state, esc: value => String(value), toast() {},
    renderModelRows() {}, renderModels(data) { state.models = clone(data.models); },
    resetModelForm() {}, renderCredentials() {}, loadCredentialScheduling() {},
    globalError() {}, updateProviderPolicyHint() {},
    present: value => value !== undefined && value !== null && value !== '',
    policyAutosave: {}, dropdowns: { get: () => ({ sync() {} }) },
    api: async (url, options = {}) => {
      if (url === '/models/server-tools') server.toolReads++;
      if (!server.authenticated) throw new Error('请输入管理密钥');
      if (url === '/models/server-tools') {
        if (server.failTools) throw new Error('工具服务暂时不可用');
        if (options.method === 'PUT') {
          server.writes.push(clone(options.body));
          server.models[options.body.model] = clone(options.body.tools);
        }
        return clone({ models: server.models, catalog: server.catalog });
      }
      if (url === '/config') return {};
      if (url === '/models') return { models: clone(state.models) };
      if (url === '/credentials') return { items: [] };
      throw new Error(`unexpected API path: ${url}`);
    },
  });
  vm.runInContext(source + '\n' + loadConfig, context);
  return {
    node, state, server,
    open: index => context.openModelTools(index),
    save: () => context.saveModelTools(),
    load: () => context.loadConfig(),
    draft: () => clone(vm.runInContext('modelToolsView.draft', context)),
  };
}

test('manual login reloads tool choices after the first authentication failure', async () => {
  const view = page();
  view.server.authenticated = false;
  await view.open(0);
  assert.equal(view.node('saveModelTools').disabled, true);
  assert.match(view.node('modelToolsState').textContent, /读取失败.*管理密钥/);
  await view.node('modelToolsDialog').close();

  view.server.authenticated = true;
  await view.load();
  await view.open(0);
  assert.deepEqual(view.draft(), ['vercel:exa_search']);
  assert.equal(view.node('modelToolsList').boxes[0].checked, true);
  assert.equal(view.node('saveModelTools').disabled, false);
});

test('failed reads block writes and can be retried without clearing existing tools', async () => {
  const view = page();
  await view.open(0);
  await view.node('modelToolsDialog').close();
  view.server.failTools = true;
  await view.open(0);
  assert.equal(view.node('saveModelTools').disabled, true);
  assert.equal(view.node('modelToolsCustom').disabled, true);
  assert.equal(view.node('retryModelTools').hidden, false);
  await view.save();
  assert.deepEqual(view.server.writes, []);
  assert.deepEqual(view.server.models.alpha, ['vercel:exa_search']);

  view.server.failTools = false;
  await view.node('retryModelTools').emit('click');
  assert.equal(view.node('saveModelTools').disabled, false);
  assert.deepEqual(view.draft(), ['vercel:exa_search']);
});

test('form submission saves the opened model even if model rows reorder', async () => {
  const view = page();
  await view.open(0);
  view.state.models.reverse();
  let prevented = false;
  await view.node('modelToolsForm').emit('submit', { preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.deepEqual(view.server.writes, [{ model: 'alpha', tools: ['vercel:exa_search'] }]);
  assert.equal(view.node('modelToolsDialog').open, false);
  assert.equal(view.node('saveModelTools').disabled, true);

  await view.open(0);
  view.state.models[0].upstream_id = 'cline-pass/replaced';
  await view.save();
  assert.equal(view.server.writes.length, 1);
  assert.equal(view.node('saveModelTools').disabled, true);
  assert.match(view.node('modelToolsState').textContent, /模型已变更/);
});
