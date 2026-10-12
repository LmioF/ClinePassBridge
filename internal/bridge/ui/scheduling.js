// This module shares the console's authenticated API client and theme.
const schedulingView = { dirty: false, data: null, egress: null, loading: false, saving: false, editing: '', revision: 0 };
const schedulingTime = (value) => value ? new Date(value).toLocaleString() : '—';
function schedulingBadge(label, tone = '') { return `<span class="badge ${tone}">${esc(label)}</span>`; }
const credentialScheduling = { accounts: new Map(), pending: new Set(), loading: false, refreshQueued: false, revision: 0 };
function credentialSchedule(id) { return credentialScheduling.accounts.get(id); }
function credentialQuotaPaused(item) { return credentialSchedule(item.id)?.quota_blocked ?? !!item.quota_paused; }
function credentialToggleState(item) {
  const paused = credentialQuotaPaused(item), manualDisabled = item.enabled === false;
  return {
    checked: !manualDisabled && !paused,
    disabled: paused || credentialScheduling.pending.has(item.id),
    label: manualDisabled ? '已停用' : paused ? '自动暂停' : '启用',
    title: paused
      ? (manualDisabled ? '已人工停用且额度耗尽；额度恢复后仍保持人工停用' : '额度耗尽已自动暂停；额度恢复后自动参与调度')
      : '人工启停；额度耗尽时自动暂停'
  };
}
function credentialToggleHTML(item) {
  const state = credentialToggleState(item);
  return `<label class="credential-toggle" title="${esc(state.title)}"><span class="credential-toggle-label">${state.label}</span><input type="checkbox" role="switch" class="credential-enabled" data-id="${esc(item.id)}" aria-label="${esc(item.label || item.id)} ${state.label}" ${state.checked ? 'checked' : ''} ${state.disabled ? 'disabled' : ''}><span class="toggle-track" aria-hidden="true"></span></label>`;
}
function credentialScheduleBadges(item) {
  const a = credentialSchedule(item.id), badges = [];
  if (item.enabled === false) badges.push(schedulingBadge('人工停用'));
  if (credentialQuotaPaused(item)) badges.push(schedulingBadge('额度耗尽 · 自动暂停', 'bad'));
  if (a?.persistence_error) badges.push(schedulingBadge('状态保存失败', 'bad'));
  if (item.enabled !== false && !credentialQuotaPaused(item)) {
    if (!a) badges.push(schedulingBadge('调度状态待查询'));
    else if (a.host?.disabled) badges.push(schedulingBadge('宿主停用', 'warn'));
    else if (a.host?.unavailable || a.backoffs?.length) badges.push(schedulingBadge('冷却中', 'warn'));
    else badges.push(schedulingBadge('参与调度', 'good'));
  }
  return badges.join('');
}
function credentialScheduleDetail(item) {
  const a = credentialSchedule(item.id); if (!a) return '';
  const reason = [a.quota_reason || a.host?.message || '', a.persistence_error ? `状态保存失败：${a.persistence_error}` : ''].filter(Boolean).join(' · ');
  const retry = a.retry_at || a.host?.next_retry_after;
  return `${reason ? `<span class="subtext" title="${esc(reason)}">${esc(reason)}</span>` : ''}${retry ? `<span class="subtext">${a.quota_blocked ? '额度复查' : '冷却至'} ${esc(schedulingTime(retry))}</span>` : ''}${(a.backoffs || []).map(w => `<span class="subtext">${esc(w.model)} · 冷却至 ${esc(schedulingTime(w.retry_at))}</span>`).join('')}`;
}
function updateCredentialScheduling() {
  for (const row of $('credentialList').children) {
    const item = credentialView.items.find(a => a.id === row.dataset.id); if (!item) continue;
    row.classList.toggle('quota-paused', credentialQuotaPaused(item));
    row.querySelector('.credential-schedule-badges').innerHTML = credentialScheduleBadges(item);
    row.querySelector('.credential-schedule-detail').innerHTML = credentialScheduleDetail(item);
    const toggle = row.querySelector('.credential-enabled');
    const state = credentialToggleState(item);
    toggle.checked = state.checked;
    toggle.disabled = state.disabled;
    toggle.setAttribute('aria-label', `${item.label || item.id} ${state.label}`);
    toggle.closest('.credential-toggle').title = state.title;
    row.querySelector('.credential-toggle-label').textContent = state.label;
    row.querySelector('.reset-cooldown').disabled = item.enabled === false || state.disabled;
  }
}
async function loadCredentialScheduling() {
  if (!authGate.verified || authGate.paused) return;
  if (credentialScheduling.loading || credentialScheduling.pending.size) { credentialScheduling.refreshQueued = true; return; }
  credentialScheduling.refreshQueued = false;
  credentialScheduling.loading = true;
  const revision = credentialScheduling.revision;
  try {
    const data = await api('/scheduling');
    if (revision !== credentialScheduling.revision) return;
    credentialScheduling.accounts = new Map((data.accounts || []).map(a => [a.id, a]));
    for (const item of credentialView.items) {
      const current = credentialSchedule(item.id);
      if (current && !credentialScheduling.pending.has(item.id)) item.enabled = current.enabled;
    }
    updateCredentialScheduling();
    $('credentialState').textContent = '';
  } catch (error) { $('credentialState').textContent = `调度状态读取失败：${error.message}`; }
  finally {
    credentialScheduling.loading = false;
    if ((credentialScheduling.refreshQueued || revision !== credentialScheduling.revision) && !credentialScheduling.pending.size) loadCredentialScheduling();
  }
}
async function changeCredentialScheduling(id, action, enabled) {
  if (credentialScheduling.pending.has(id)) return;
  const item = credentialView.items.find(a => a.id === id);
  if (!item || credentialQuotaPaused(item)) { updateCredentialScheduling(); return; }
  credentialScheduling.revision++;
  credentialScheduling.pending.add(id); updateCredentialScheduling();
  try {
    if (action === 'toggle') {
      await api('/scheduling/credential', { method: 'PUT', body: { id, enabled } });
      const item = credentialView.items.find(a => a.id === id); if (item) item.enabled = enabled;
      syncModelTestCredentials(); syncClearAffinityControls();
    } else {
      const result = await api('/scheduling/reset', { method: 'POST', body: { id } });
      if (result.warning) toast(result.warning);
    }
  } catch (error) { toast(error.message); }
  finally { credentialScheduling.revision++; credentialScheduling.pending.delete(id); updateCredentialScheduling(); await loadCredentialScheduling(); }
}
$('credentialList').addEventListener('change', (event) => {
  const input = event.target.closest('.credential-enabled');
  if (input) changeCredentialScheduling(input.dataset.id, 'toggle', input.checked);
});
$('credentialList').addEventListener('click', (event) => {
  const button = event.target.closest('.reset-cooldown');
  if (button && !button.disabled) changeCredentialScheduling(button.dataset.id, 'reset');
});
setInterval(() => { if (usageViewVisible()) loadCredentialScheduling(); }, 15000);
function renderEgress(data) {
  schedulingView.egress = data;
  $('egressEnabled').checked = !!data.enabled;
  $('egressWait').value = data.wait_timeout_seconds ?? 5;
  $('egressList').innerHTML = (data.endpoints || []).map((item) => {
    const status = item.status || {};
    const badge = !item.enabled ? schedulingBadge('停用') : status.retry_at && new Date(status.retry_at) > new Date() ? schedulingBadge('冷却', 'warn') : schedulingBadge('已配置', 'good');
    return `<div class="egress-row"><div class="schedule-account-main"><strong>${esc(item.label || item.id)}</strong>${badge}<span class="subtext mono">${esc(item.proxy_url === 'direct' ? '直连' : (item.proxy_url || '代理已保存'))}</span></div><div class="egress-metrics"><span>最近 60 秒 <b>${esc(status.requests_last_minute ?? 0)} / ${esc(item.rpm)}</b></span><span>在飞 <b>${esc(status.inflight ?? 0)} / ${esc(item.max_inflight)}</b></span><span title="相同真实出口 IP 使用同一分组，共享速率和并发预算">出口组 <b>${esc(item.group || item.id)}</b></span>${status.retry_at ? `<span>恢复 ${esc(schedulingTime(status.retry_at))}</span>` : ''}</div><div class="row-actions"><button type="button" class="button compact" data-egress-edit="${esc(item.id)}">编辑</button><button type="button" class="button compact danger" data-egress-delete="${esc(item.id)}">移除</button></div></div>`;
  }).join('') || `<div class="empty">${data.enabled ? '出口池已开启，但尚未配置出口' : '尚未配置出口 · 当前沿用 CPA 原有代理设置'}</div>`;
}
async function loadEgress() {
  if (schedulingView.loading || schedulingView.saving) return;
  schedulingView.loading = true; $('refreshEgress').disabled = true;
  try {
    const egress = await api('/egress');
    if (!schedulingView.dirty) renderEgress(egress);
    $('egressState').textContent = '';
  } catch (error) { $('egressState').textContent = `读取失败：${error.message}`; }
  finally { schedulingView.loading = false; $('refreshEgress').disabled = false; }
}
async function updateEgress(endpoints) {
  if (schedulingView.saving || !schedulingView.egress) return false;
  schedulingView.saving = true;
  $('saveEgress').disabled = true;
  try {
    const data = await api('/egress', { method: 'PUT', body: { enabled: $('egressEnabled').checked, wait_timeout_seconds: Number($('egressWait').value), ip_cooldown_seconds: schedulingView.egress.ip_cooldown_seconds ?? 60, endpoints } });
    schedulingView.dirty = false; renderEgress(data); $('egressState').textContent = '出口设置已保存'; return true;
  } catch (error) { $('egressState').textContent = `保存失败：${error.message}`; return false; }
  finally { schedulingView.saving = false; $('saveEgress').disabled = false; }
}
function egressEditableRows() {
  return (schedulingView.egress?.endpoints || []).map(({ id, label, group, rpm, max_inflight, enabled }) => ({ id, label, group, rpm, max_inflight, enabled }));
}
function openEgressDialog(id = '') {
  schedulingView.editing = id;
  const item = schedulingView.egress?.endpoints?.find((row) => row.id === id);
  $('egressForm').reset(); $('egressDialogTitle').textContent = item ? '编辑出口' : '添加出口';
  $('egressLabel').value = item?.label || '';
  $('egressURL').value = ''; $('egressURL').required = !item;
  $('egressURL').placeholder = item ? '留空保留原代理地址及认证' : 'http://… · socks5://… · direct';
  $('egressGroup').value = item?.group || '';
  $('egressRPM').value = item?.rpm ?? 60;
  $('egressInflight').value = item?.max_inflight ?? 4;
  $('egressEndpointEnabled').checked = item?.enabled ?? true;
  $('egressDialogState').textContent = '';
  $('egressDialog').showModal(); $('egressLabel').focus();
}
$('egressTab').addEventListener('click', () => setTab('egress'));
$('openEgressGuide').addEventListener('click', () => $('egressGuideDialog').showModal());
$('closeEgressGuide').addEventListener('click', () => $('egressGuideDialog').close());
$('refreshEgress').addEventListener('click', loadEgress);
$('openEgress').addEventListener('click', () => openEgressDialog());
$('egressControls').addEventListener('input', () => { schedulingView.dirty = true; });
$('saveEgress').addEventListener('click', () => updateEgress(egressEditableRows()));
$('cancelEgress').addEventListener('click', () => $('egressDialog').close());
$('egressDialog').addEventListener('cancel', (event) => { if (schedulingView.saving) event.preventDefault(); });
$('egressDialog').addEventListener('close', () => { $('egressURL').value = ''; schedulingView.editing = ''; });
$('egressList').addEventListener('click', async (event) => {
  const edit = event.target.closest('[data-egress-edit]'); if (edit) return openEgressDialog(edit.dataset.egressEdit);
  const remove = event.target.closest('[data-egress-delete]'); if (!remove || !confirm('移除此出口？已开始的请求会继续完成。')) return;
  remove.disabled = true; await updateEgress(egressEditableRows().filter((row) => row.id !== remove.dataset.egressDelete)); remove.disabled = false;
});
$('egressForm').addEventListener('submit', async (event) => {
  event.preventDefault(); if (schedulingView.saving) return;
  const id = schedulingView.editing || `egress-${globalThis.crypto?.randomUUID?.() || `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`}`;
  const endpoint = { id, label: $('egressLabel').value.trim(), group: $('egressGroup').value.trim() || id, rpm: Number($('egressRPM').value), max_inflight: Number($('egressInflight').value), enabled: $('egressEndpointEnabled').checked };
  const address = $('egressURL').value.trim(); if (address) endpoint.proxy_url = address;
  const endpoints = egressEditableRows().filter((row) => row.id !== id); endpoints.push(endpoint);
  $('submitEgress').disabled = true; $('cancelEgress').disabled = true;
  try { if (await updateEgress(endpoints)) $('egressDialog').close(); else $('egressDialogState').textContent = $('egressState').textContent; }
  finally { $('submitEgress').disabled = false; $('cancelEgress').disabled = false; }
});
setInterval(() => { if (!document.hidden && !$('egressPanel').hidden && !$('egressDialog').open && !schedulingView.dirty && document.activeElement?.closest('#egressControls') == null) loadEgress(); }, 15000);
