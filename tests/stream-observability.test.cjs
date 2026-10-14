const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

const root = path.join(__dirname, '../internal/bridge/ui');
const escape = value => String(value ?? '').replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));

function scheduling() {
  const source = fs.readFileSync(path.join(root, 'scheduling.js'), 'utf8');
  const context = vm.createContext({ esc: escape, Date, Map });
  vm.runInContext(source.slice(0, source.indexOf('function updateCredentialScheduling()')) + '\nglobalThis.view = { badges: credentialScheduleBadges, detail: credentialScheduleDetail, set: rows => { credentialScheduling.accounts = new Map(rows.map(a => [a.id, a])); } };', context);
  return context.view;
}

test('card identifies model cooldown without presenting account cooldown', () => {
  const view = scheduling();
  const deadline = new Date(Date.now() + 3600000).toISOString();
  view.set([{ id: 'one', host: { unavailable: true, model_states_available: true }, model_cooldowns: [{ model: 'flash', retry_at: deadline, message: 'TransferEncodingError: <payload>' }] }]);
  const item = { id: 'one', enabled: true };
  assert.match(view.badges(item), /模型冷却/);
  assert.doesNotMatch(view.badges(item), /账号冷却|冷却中/);
  assert.match(view.detail(item), /flash · 冷却至/);
  assert.match(view.detail(item), /上游响应截断/);
  assert.doesNotMatch(view.detail(item), /<payload>/);
  view.set([{ id: 'one', host: { unavailable: true, model_states_available: true }, model_cooldowns: [{ model: 'flash', retry_at: new Date(0).toISOString() }] }]);
  assert.match(view.badges(item), /参与调度/);
  view.set([{ id: 'one', host: { model_states_available: true }, model_cooldowns: [
    { model: 'alias-a', upstream_model: 'shared', retry_at: deadline },
    { model: 'alias-b', upstream_model: 'shared', retry_at: new Date(Date.now() + 7200000).toISOString() },
  ] }]);
  assert.match(view.detail(item), /alias-a · 冷却至/);
  assert.match(view.detail(item), /alias-b · 冷却至/);
  view.set([{ id: 'one', host: { model_states_available: true, credential_blocked: true } }]);
  assert.match(view.badges(item), /鉴权失效/);
  view.set([{ id: 'one', quota_blocked: true, quota_reason: '每周额度耗尽', host: { disabled: true } }]);
  assert.match(view.badges(item), /额度耗尽 · 自动暂停/);
});

test('missing usage is distinct from explicitly reported zero usage', () => {
  const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
  const start = html.indexOf('    function logUsageReported(');
  const end = html.indexOf('    function actualProvider(', start);
  const context = vm.createContext({ number: value => String(value), Number });
  vm.runInContext(html.slice(start, end), context);
  const missing = { prompt_tokens: 0, completion_tokens: 0, prompt_usage_reported: false, completion_usage_reported: false };
  assert.equal(context.logTokenValue(missing, 'prompt_tokens'), '未报告');
  assert.equal(context.logTokenValue(missing, 'completion_tokens'), '未报告');
  assert.equal(context.logTokenValue({ ...missing, prompt_usage_reported: true, completion_usage_reported: true }, 'completion_tokens'), '0');
  assert.equal(context.logTokenValue({ prompt_tokens: 0, completion_tokens: 0, status: 502 }, 'completion_tokens'), '未报告');
  assert.equal(context.logTokenValue({ ...missing, upstream_skipped: true }, 'completion_tokens'), '0');
});
