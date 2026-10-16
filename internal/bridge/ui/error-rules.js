// Uses the console's authenticated API client and shared dropdowns.
const errorRulesView = { loaded: false, busy: false, dirty: false, defaults: [], sync: null };
const errorRuleActions = [
  ['stop', '停止'], ['stop-and-cooldown', '停止并冷却'],
  ['continue', '继续'], ['continue-and-cooldown', '继续并冷却'],
];
let errorRuleSequence = 0;
$('openErrorRuleHelp').addEventListener('click', () => $('errorRuleHelpDialog').showModal());

function errorRuleExplanation(rule) {
  const texts = rule.conditions.filter((condition) => condition.type === 'contains').map((condition) => condition.text);
  const known = [
    [500, 'empty response content', '空响应', '上游未返回有效响应内容。'],
    [429, '[clinepassbridge:team_tpm_limit]', '上游共享团队限流', '上游共享团队的 token 速率达到上限；切换账号仍可能命中同一限制。'],
    [429, '[clinepassbridge:quota_block]', '账号额度阻断', '账号被额度保护暂停调度；可继续尝试其他额度可用的账号。'],
    [429, '[clinepassbridge:egress_unavailable]', '代理池无可用出口', '代理池暂无可用出口，或出口的并发、速率预算已满。'],
    [429, '[clinepassbridge:ip_rate_limit]', '出口 IP 限流', '当前出口 IP 触发上游频率限制；同一出口上的其他账号也可能受影响。'],
  ].filter(([status, marker]) => Number(rule.status) === status && texts.includes(marker));
  if (known.length) return { name: known.map((item) => item[2]).join(' / '), detail: known.map((item) => item[3]).join(' ') };
  const status = Number(rule.status);
  const generic = {
    400: ['请求参数错误', '请求格式或参数被上游拒绝；具体原因由匹配条件区分。'],
    401: ['身份验证失败', '凭据无效、过期或认证信息缺失；具体原因由匹配条件区分。'],
    403: ['访问被拒绝', '上游拒绝访问，可能涉及权限或风控；具体原因由匹配条件区分。'],
    404: ['资源不存在', '上游未找到请求的模型、接口或资源。'],
    408: ['请求超时', '请求未在上游允许的时间内完成。'],
    429: ['请求受限', '通常表示频率、并发或额度受限；请用匹配条件区分具体原因。'],
    500: ['服务内部错误', '服务处理请求时出错；请用匹配条件区分具体原因。'],
    502: ['网关响应异常', '网关未收到有效的上游响应；具体原因由匹配条件区分。'],
    503: ['服务暂不可用', '服务暂时没有可用容量或候选账号；具体原因由匹配条件区分。'],
    504: ['网关超时', '网关等待上游响应超时。'],
  }[status];
  return generic ? { name: generic[0], detail: generic[1] } : { name: `HTTP ${rule.status || '—'}`, detail: '按此状态码和匹配条件识别错误，再执行所选动作。' };
}

function updateErrorRuleExplanations() {
  const draft = readErrorRuleDraft();
  for (const [index, card] of [...$('errorRulesList').querySelectorAll('.error-rule-card')].entries()) {
    const explanation = errorRuleExplanation(draft[index]);
    card.querySelector('.error-rule-name').textContent = `规则 ${index + 1} · ${explanation.name}`;
    card.querySelector('.error-rule-description').textContent = explanation.detail;
  }
}

function errorRulesControls() {
  const locked = errorRulesView.busy || !errorRulesView.loaded;
  for (const field of $('errorRulesForm').querySelectorAll('input, textarea, select, button')) {
    field.disabled = locked || field.dataset.unavailable === 'true';
  }
  $('closeErrorRules').disabled = errorRulesView.busy;
  $('retryErrorRules').disabled = locked || errorRulesView.dirty;
  $('retryErrorRules').title = errorRulesView.dirty ? '请先保存编辑中的规则' : '将已保存规则重新同步到所有账号';
  for (const field of $('errorRulesList').querySelectorAll('select')) dropdowns.get(field.id)?.sync();
}

function errorRuleSelect(id, value, options, label, className) {
  return `<select class="control ${className}" id="${id}" aria-label="${esc(label)}">${options.map(([key, text]) => `<option value="${key}"${key === value ? ' selected' : ''}>${text}</option>`).join('')}</select>`;
}

function readErrorRuleDraft() {
  return [...$('errorRulesList').querySelectorAll('.error-rule-card')].map((card) => ({
    status: card.querySelector('.error-rule-status').value,
    action: card.querySelector('.error-rule-action').value,
    conditions: [...card.querySelectorAll('.error-rule-condition')].map((row) => ({
      type: row.querySelector('.error-rule-kind').value,
      text: row.querySelector('textarea').value,
    })),
  }));
}

function toErrorRuleDraft(rules) {
  return rules.map((rule) => ({ status: rule.status, action: rule.action, conditions: [
    ...(rule.match || []).map((text) => ({ type: 'contains', text })),
    ...(rule['match-regexr'] || []).map((text) => ({ type: 'regex', text })),
  ] }));
}

function renderErrorRuleDraft(draft) {
  const list = $('errorRulesList');
  for (const field of list.querySelectorAll('select')) {
    dropdowns.get(field.id)?.destroy();
    dropdowns.delete(field.id);
  }
  list.innerHTML = draft.length ? draft.map((rule, index) => {
    const id = ++errorRuleSequence;
    const conditionHTML = rule.conditions.map((condition, position) => {
      const conditionID = `error-condition-${id}-${position}`;
      return `<div class="error-rule-condition" data-condition="${position}">
        <div class="field condition-kind-field"><label for="${conditionID}-kind">匹配类型</label>${errorRuleSelect(`${conditionID}-kind`, condition.type, [['contains', '包含文本'], ['regex', '正则表达式']], `规则 ${index + 1} 条件 ${position + 1} 匹配类型`, 'error-rule-kind')}</div>
        <div class="field"><label for="${conditionID}-text" title="同一规则内任意条件命中即可；正则使用 Go 语法">匹配条件</label><textarea class="control" id="${conditionID}-text" rows="1" maxlength="4096" required aria-label="规则 ${index + 1} 条件 ${position + 1}">${esc(condition.text)}</textarea></div>
        <button class="button compact danger" type="button" data-rule-command="remove-condition" style="margin-top:22px" aria-label="删除规则 ${index + 1} 条件 ${position + 1}">${buttonIcon('trash')}删除</button>
      </div>`;
    }).join('');
    return `<section class="error-rule-card" data-rule="${index}" aria-label="规则 ${index + 1}">
      <div class="error-rule-head"><strong class="error-rule-name">规则 ${index + 1}</strong><div class="row-actions">
        <button class="button compact icon-button" type="button" data-rule-command="up" data-unavailable="${index === 0}" aria-label="上移规则 ${index + 1}" title="上移">${buttonIcon('up')}</button>
        <button class="button compact icon-button" type="button" data-rule-command="down" data-unavailable="${index === draft.length - 1}" aria-label="下移规则 ${index + 1}" title="下移">${buttonIcon('down')}</button>
        <button class="button compact danger" type="button" data-rule-command="remove" aria-label="删除规则 ${index + 1}">${buttonIcon('trash')}删除</button>
      </div></div>
      <p class="error-rule-description"></p>
      <div class="error-rule-fields"><div class="field"><label for="error-status-${id}">状态码</label><input class="control error-rule-status" id="error-status-${id}" type="number" min="100" max="599" step="1" required value="${esc(rule.status)}"></div>
      <div class="field"><label for="error-action-${id}" title="停止：向客户端返回错误；继续：尝试后续候选。并冷却：暂时排除失败的凭据或模型。">动作</label>${errorRuleSelect(`error-action-${id}`, rule.action, errorRuleActions, `规则 ${index + 1} 动作`, 'error-rule-action')}</div></div>
      <div class="error-rule-conditions">${conditionHTML}</div>
      <button class="button compact error-rule-add" type="button" data-rule-command="add-condition">${buttonIcon('plus')}新增条件</button>
    </section>`;
  }).join('') : '<div class="empty">暂无规则。保存后清除所有账号的单凭据规则，交由 CPA 继承规则或默认错误处理。</div>';
  for (const field of list.querySelectorAll('select')) {
    dropdowns.set(field.id, createDropdown(field, () => [...field.options].map((option) => ({ value: option.value, label: option.textContent }))));
  }
  updateErrorRuleExplanations();
  errorRulesControls();
}

function errorRulesChanged() {
  updateErrorRuleExplanations();
  errorRulesView.dirty = true;
  $('errorRulesState').textContent = '有未保存的修改';
  setButtonLabel('closeErrorRules', '放弃修改');
  errorRulesControls();
}

function showErrorRuleSync(sync, saved = false) {
  errorRulesView.sync = sync;
  const failed = sync?.failed || [];
  $('errorRulesFailures').hidden = !failed.length;
  $('retryErrorRules').hidden = !failed.length;
  $('errorRulesFailures').innerHTML = failed.length ? `以下账号同步失败<ul>${failed.map((item) => `<li>${esc(item.label || item.id)}：${esc(item.error || '请重试')}</li>`).join('')}</ul>` : '';
  $('errorRulesState').textContent = sync ? `${saved ? '规则已保存 · ' : ''}已同步 ${sync.succeeded ?? 0} / ${sync.total ?? 0} 个账号${failed.length ? `，${failed.length} 个待重试` : ''}` : '';
}

function acceptErrorRuleResponse(data, saved = false) {
  errorRulesView.loaded = true;
  errorRulesView.dirty = false;
  errorRulesView.defaults = data.defaults || errorRulesView.defaults;
  renderErrorRuleDraft(toErrorRuleDraft(data.rules || []));
  showErrorRuleSync(data.sync, saved);
  setButtonLabel('closeErrorRules', '关闭');
}

$('openErrorRules').addEventListener('click', async () => {
  if (errorRulesView.busy) return;
  errorRulesView.busy = true;
  errorRulesView.loaded = false;
  errorRulesView.dirty = false;
  renderErrorRuleDraft([]);
  $('errorRulesList').innerHTML = '<div class="empty">正在读取规则…</div>';
  $('errorRulesState').textContent = '';
  $('errorRulesFailures').hidden = true;
  $('retryErrorRules').hidden = true;
  setButtonLabel('closeErrorRules', '关闭');
  $('errorRulesDialog').showModal();
  try { acceptErrorRuleResponse(await api('/error-rules')); }
  catch (error) { $('errorRulesList').innerHTML = ''; $('errorRulesState').textContent = `读取失败：${error.message}`; }
  finally { errorRulesView.busy = false; errorRulesControls(); }
});

$('closeErrorRules').addEventListener('click', () => { if (!errorRulesView.busy) $('errorRulesDialog').close(); });
$('errorRulesDialog').addEventListener('cancel', (event) => { if (errorRulesView.busy) event.preventDefault(); });
$('errorRulesList').addEventListener('input', errorRulesChanged);
$('errorRulesList').addEventListener('change', errorRulesChanged);
$('errorRulesList').addEventListener('click', (event) => {
  const button = event.target.closest('[data-rule-command]');
  if (!button || errorRulesView.busy) return;
  const draft = readErrorRuleDraft(), index = Number(button.closest('[data-rule]').dataset.rule);
  const command = button.dataset.ruleCommand;
  if (command === 'remove') draft.splice(index, 1);
  else if (command === 'up' && index > 0) [draft[index - 1], draft[index]] = [draft[index], draft[index - 1]];
  else if (command === 'down' && index < draft.length - 1) [draft[index + 1], draft[index]] = [draft[index], draft[index + 1]];
  else if (command === 'add-condition') draft[index].conditions.push({ type: 'contains', text: '' });
  else if (command === 'remove-condition') draft[index].conditions.splice(Number(button.closest('[data-condition]').dataset.condition), 1);
  errorRulesChanged();
  renderErrorRuleDraft(draft);
});
$('addErrorRule').addEventListener('click', () => {
  const draft = readErrorRuleDraft();
  draft.push({ status: 429, action: 'continue-and-cooldown', conditions: [{ type: 'contains', text: '' }] });
  errorRulesChanged();
  renderErrorRuleDraft(draft);
  $('errorRulesList').lastElementChild?.querySelector('input')?.focus();
});
$('resetErrorRules').addEventListener('click', () => {
  errorRulesChanged();
  renderErrorRuleDraft(toErrorRuleDraft(errorRulesView.defaults));
  $('errorRulesState').textContent = '已恢复默认规则，保存后应用到所有账号';
});
$('errorRulesForm').addEventListener('submit', async (event) => {
  event.preventDefault();
  if (errorRulesView.busy || !errorRulesView.loaded) return;
  const draft = readErrorRuleDraft();
  if (draft.some((rule) => !rule.conditions.length || rule.conditions.some((condition) => !condition.text.trim()))) {
    $('errorRulesState').textContent = '每条规则至少需要一个非空匹配条件';
    return;
  }
  const rules = draft.map((rule) => ({ status: Number(rule.status), action: rule.action,
    match: rule.conditions.filter((condition) => condition.type === 'contains').map((condition) => condition.text),
    'match-regexr': rule.conditions.filter((condition) => condition.type === 'regex').map((condition) => condition.text),
  }));
  errorRulesView.busy = true;
  errorRulesControls();
  $('errorRulesState').textContent = '正在保存并同步所有账号…';
  try { acceptErrorRuleResponse(await api('/error-rules', { method: 'PUT', body: { rules }, fullError: true }), true); }
  catch (error) { $('errorRulesState').textContent = `保存未完成：${error.message}`; }
  finally { errorRulesView.busy = false; errorRulesControls(); }
});
$('retryErrorRules').addEventListener('click', async () => {
  if (errorRulesView.busy || errorRulesView.dirty) return;
  errorRulesView.busy = true;
  errorRulesControls();
  $('errorRulesState').textContent = '正在重新同步…';
  try { acceptErrorRuleResponse(await api('/error-rules/sync', { method: 'POST', body: {}, fullError: true }), true); }
  catch (error) { $('errorRulesState').textContent = `同步未完成：${error.message}`; }
  finally { errorRulesView.busy = false; errorRulesControls(); }
});
