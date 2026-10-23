const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const html = fs.readFileSync(path.join(__dirname, '../internal/bridge/ui/index.html'), 'utf8');
function section(start, end) {
  const from = html.indexOf(start);
  const to = html.indexOf(end, from);
  assert.ok(from >= 0 && to > from, `missing UI section: ${start}`);
  return html.slice(from, to);
}
function declaration(name) {
  const line = html.match(new RegExp(`^    const ${name} = .+;$`, 'm'));
  assert.ok(line, `missing UI declaration: ${name}`);
  return line[0];
}
const code = [
  declaration('pinAutosave'), declaration('policyAutosave'), declaration('modelKey'),
  section('    function renderModels(', '    async function saveModels('),
  section('    const parseProviders =', '    function providerProbeHTML('),
  section("    $('pinModelList').addEventListener('input'", '    function syncModelTestCredentials('),
  section("    $('providerPolicy').addEventListener('change'", '    function selectedReasoningModel('),
  'globalThis.flushModels = flushPinModels; globalThis.flushPolicy = flushProviderPolicy;',
].join('\n');

const clone = value => JSON.parse(JSON.stringify(value));
const key = model => JSON.stringify([model.id, model.upstream_id]);
const fixtures = () => [
  { id: 'a', upstream_id: 'cline-pass/a', providers: ['old-a'], reasoning_mapping: { enabled: true, rules: { low: 'medium' } } },
  { id: 'b', upstream_id: 'cline-pass/b', providers: ['old-b'], reasoning_mapping: { enabled: false, rules: {} } },
  { id: 'untouched', upstream_id: 'cline-pass/untouched', providers: ['existing'], reasoning_mapping: { enabled: true, rules: { high: 'max' } } },
];
const settle = () => new Promise(resolve => setImmediate(resolve));
function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function page(intercept = async () => {}) {
  const initial = fixtures();
  const nodes = new Map();
  const inputs = new Map();
  const calls = [];
  const timers = new Map();
  let now = 0;
  let timerID = 0;
  let active = 0;
  let maxActive = 0;
  const server = { models: clone(initial), provider_policy: 'config', log_retention: 731 };
  const state = {
    models: clone(initial), config: clone(server), providers: new Set(),
    modelBusy: false, fetchingModels: false, candidateBusy: false, editingModelIndex: -1,
  };
  function node(id) {
    if (!nodes.has(id)) {
      const listeners = new Map();
      nodes.set(id, {
        value: '', textContent: '', dataset: {}, hidden: true, open: false,
        addEventListener(type, handler) {
          if (!listeners.has(type)) listeners.set(type, []);
          listeners.get(type).push(handler);
        },
        emit(type, event = {}) {
          return Promise.all((listeners.get(type) || []).map(handler => handler({ target: this, ...event })));
        },
        querySelectorAll() { return []; },
      });
    }
    return nodes.get(id);
  }
  function buildInputs() {
    inputs.clear();
    for (const model of state.models) {
      const badge = { textContent: '', classList: { toggle() {} } };
      const row = { querySelector: () => badge };
      inputs.set(model.id, {
        dataset: { modelKey: key(model) }, value: model.providers.join(', '),
        selectionStart: 0, selectionEnd: 0,
        closest: () => row,
        matches: selector => selector === 'input[data-model-key]',
      });
      inputs.set(`${model.id}#has`, {
        dataset: { modelKey: key(model), field: 'capabilities' }, value: (model.capabilities || []).join(', '),
        selectionStart: 0, selectionEnd: 0,
        closest: () => row,
        matches: selector => selector === 'input[data-model-key]',
      });
    }
  }
  buildInputs();
  node('pinModelList').querySelectorAll = selector => selector === 'input[data-model-key]' ? [...inputs.values()] : [];
  node('providerPolicy').value = server.provider_policy;
  const document = { activeElement: null };
  const context = vm.createContext({
    state, document, reasoningMappingBusy: false, $: node,
    present: value => value !== undefined && value !== null && value !== '',
    renderProviderOptions() {}, renderModelRows() {}, renderReasoningModels() {},
    resetModelForm() {}, renderCandidateList() {}, syncProviderProbeControls() {}, syncPinModelTests() {},
    renderPinModelRows: buildInputs,
    setTimeout(callback, delay) {
      const id = ++timerID;
      timers.set(id, { callback, at: now + delay });
      return id;
    },
    clearTimeout: id => timers.delete(id),
    api: async (url, options) => {
      const request = { url, method: options?.method, body: clone(options?.body) };
      calls.push(request);
      active++;
      maxActive = Math.max(maxActive, active);
      try {
        await intercept(request, calls.length);
        assert.equal(request.method, 'PUT');
        if (url === '/models') {
          server.models = clone(request.body.models);
          return { models: clone(server.models) };
        }
        assert.equal(url, '/config');
        Object.assign(server, request.body);
        return clone(server);
      } finally { active--; }
    },
  });
  vm.runInContext(code, context);
  return {
    state, nodes, inputs, calls, initial, server,
    get maxActive() { return maxActive; },
    async advance(milliseconds) {
      const target = now + milliseconds;
      for (;;) {
        const next = [...timers].filter(([, timer]) => timer.at <= target).sort((a, b) => a[1].at - b[1].at)[0];
        if (!next) break;
        timers.delete(next[0]);
        now = next[1].at;
        next[1].callback();
        await settle();
      }
      now = target;
      await settle();
    },
    edit(id, value) {
      const input = inputs.get(id);
      input.value = value;
      document.activeElement = input;
      return node('pinModelList').emit('input', { target: input });
    },
    choosePolicy(value) {
      node('providerPolicy').value = value;
      return node('providerPolicy').emit('change');
    },
    retry: () => node('retryPinModels').emit('click'),
    flushModels: () => context.flushModels(),
    flushPolicy: () => context.flushPolicy(),
  };
}

test('白名单防抖合并保存，保留模型其他属性并将清空值保存为 []', async () => {
  const view = page();
  await view.edit('a', 'Vendor-A');
  await view.advance(300);
  await view.edit('a', 'Vendor-A, vendor-a Other');
  await view.edit('b', '');
  await view.advance(649);
  assert.equal(view.calls.length, 0);
  await view.advance(1);
  assert.equal(view.calls.length, 1);
  const expected = clone(view.initial);
  expected[0].providers = ['vendor-a', 'other'];
  expected[1].providers = [];
  assert.deepEqual(view.calls[0].body.models, expected);
  assert.deepEqual(view.server.models, expected);
  assert.deepEqual(clone(view.state.models), expected);
  assert.equal(view.nodes.get('pinModelsState').dataset.phase, 'saved');
});

test('保存挂起期间继续修改两个模型，旧响应保留输入且后续写入串行完成', async () => {
  const gates = [deferred(), deferred()];
  const view = page((request, index) => gates[index - 1].promise);
  await view.edit('a', 'first-a');
  await view.advance(650);
  assert.equal(view.calls.length, 1);
  const inputA = view.inputs.get('a');
  const inputB = view.inputs.get('b');
  await view.edit('a', 'latest-a');
  await view.edit('b', 'latest-b');
  inputB.selectionStart = 4;
  inputB.selectionEnd = 4;
  await view.advance(650);
  assert.equal(view.calls.length, 1, '前一请求完成前保持单个写入');
  gates[0].resolve();
  await settle();
  assert.equal(view.calls.length, 2);
  assert.equal(view.inputs.get('a'), inputA);
  assert.equal(view.inputs.get('b'), inputB);
  assert.equal(inputA.value, 'latest-a');
  assert.equal(inputB.value, 'latest-b');
  assert.equal(inputB.selectionStart, 4);
  assert.deepEqual(view.calls[1].body.models.map(model => model.providers), [['latest-a'], ['latest-b'], ['existing']]);
  gates[1].resolve();
  assert.equal(await view.flushModels(), true);
  assert.equal(view.maxActive, 1);
  assert.deepEqual(view.server.models.map(model => model.providers), [['latest-a'], ['latest-b'], ['existing']]);
  assert.equal(inputA.value, 'latest-a');
  assert.equal(inputB.value, 'latest-b');
});

test('一次保存失败保留输入，点击重试后保存原草稿', async () => {
  const view = page(async (request, index) => {
    if (index === 1) throw new Error('fixture write failure');
  });
  await view.edit('a', 'retry-provider');
  await view.advance(650);
  assert.equal(view.calls.length, 1);
  assert.equal(view.inputs.get('a').value, 'retry-provider');
  assert.equal(view.nodes.get('pinModelsState').dataset.phase, 'error');
  assert.equal(view.nodes.get('retryPinModels').hidden, false);
  assert.deepEqual(view.server.models, view.initial);
  await view.retry();
  assert.equal(view.calls.length, 2);
  assert.deepEqual(view.calls[1].body, view.calls[0].body);
  assert.deepEqual(view.server.models[0].providers, ['retry-provider']);
  assert.equal(view.nodes.get('pinModelsState').dataset.phase, 'saved');
  assert.equal(view.nodes.get('retryPinModels').hidden, true);
});

test('快速切换策略串行保存，旧响应保留当前选择且最终值胜出', async () => {
  const gates = [deferred(), deferred(), deferred()];
  const view = page((request, index) => gates[index - 1].promise);
  await view.choosePolicy('client');
  await view.choosePolicy('config');
  await view.choosePolicy('client');
  await view.choosePolicy('config');
  assert.equal(view.calls.length, 1);
  gates[0].resolve();
  await settle();
  assert.equal(view.calls.length, 2);
  assert.equal(view.nodes.get('providerPolicy').value, 'config');
  await view.choosePolicy('client');
  assert.equal(view.calls.length, 2);
  gates[1].resolve();
  await settle();
  assert.equal(view.calls.length, 3);
  assert.equal(view.nodes.get('providerPolicy').value, 'client');
  gates[2].resolve();
  assert.equal(await view.flushPolicy(), true);
  assert.deepEqual(view.calls.map(call => call.body), [
    { provider_policy: 'client' }, { provider_policy: 'config' }, { provider_policy: 'client' },
  ]);
  assert.equal(view.maxActive, 1);
  assert.equal(view.server.provider_policy, 'client');
  assert.equal(view.state.config.provider_policy, 'client');
  assert.equal(view.state.config.log_retention, 731);
  assert.equal(view.nodes.get('providerPolicyState').dataset.phase, 'saved');
});

test('能力过滤与白名单共用自动保存，只给被修改的模型写入 capabilities', async () => {
  const view = page();
  await view.edit('a#has', 'Vision implicit-caching, vision');
  await view.edit('b', 'vendor-b');
  await view.advance(650);
  assert.equal(view.calls.length, 1);
  const expected = clone(view.initial);
  expected[0].capabilities = ['vision', 'implicit-caching'];
  expected[1].providers = ['vendor-b'];
  assert.deepEqual(view.calls[0].body.models, expected);
  assert.deepEqual(clone(view.state.models), expected);
  assert.equal(view.inputs.get('a#has').value, 'vision, implicit-caching');
  assert.equal(view.nodes.get('pinModelsState').dataset.phase, 'saved');

  await view.edit('a#has', '');
  await view.advance(650);
  assert.equal(view.calls.length, 2);
  assert.deepEqual(view.calls[1].body.models[0].capabilities, [], '清空能力过滤保存为 []');
});
