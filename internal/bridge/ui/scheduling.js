// This module shares the console's authenticated API client and theme.
const schedulingView = { dirty: false, data: null, egress: null, loading: false, saving: false, editing: '', revision: 0 };
const schedulingTime = (value) => value ? new Date(value).toLocaleString() : '—';
function schedulingBadge(label, tone = '') { return `<span class="badge ${tone}">${esc(label)}</span>`; }
function renderScheduling(data) {
  schedulingView.data = data;
  const counts = data.summary || {};
  $('scheduleSummary').innerHTML = [
    ['参与调度', counts.available ?? 0], ['额度暂停', counts.quota_blocked ?? 0],
    ['本地拦截（留存日志）', counts.local_rejected ?? 0], ['上游 429（留存日志）', counts.upstream_429 ?? 0]
  ].map(([label, value]) => `<div class="summary-item"><div class="summary-label">${label}</div><div class="summary-value">${esc(value)}</div></div>`).join('');
  $('quotaAutoPause').checked = data.quota_auto_pause !== false;
  $('scheduleHostStatus').textContent = data.host?.available ? '已连接 CPA 调度状态' : '当前宿主未提供调度状态';
  $('scheduleHostStatus').title = data.host?.message || 'CPA 选号前的拒绝不计入插件请求日志。';
  const delay = data.host?.local_delay_supported;
  $('scheduleDelayStatus').textContent = delay ? '本地 429 · 5 秒延迟' : '插件本地 429 · 5 秒延迟';
  $('scheduleDelayStatus').title = (delay ? '已支持 CPA 选号前拒绝与插件本地退避。' : 'CPA 选号前的拒绝需要配套宿主支持；插件常规功能不依赖此扩展。') + (data.host?.request_cancel_observable ? '已支持客户端取消观察。' : '当前宿主按插件超时或停止信号结束等待。');
  $('scheduleAccounts').innerHTML = (data.accounts || []).map((account) => {
    let badge = schedulingBadge('可用', 'good');
    if (account.enabled === false) badge = schedulingBadge('手动停用');
    else if (account.quota_blocked) badge = schedulingBadge('额度暂停', 'bad');
    else if (account.host?.disabled) badge = schedulingBadge('宿主停用', 'warn');
    else if (account.host?.unavailable || account.backoffs?.length) badge = schedulingBadge('冷却中', 'warn');
    const reason = [account.quota_reason || account.host?.message || '', account.persistence_error ? `状态保存失败：${account.persistence_error}` : ''].filter(Boolean).join(' · ');
    const retry = account.retry_at || account.host?.next_retry_after;
    const windows = (account.backoffs || []).map((item) => `<div class="subtext">${esc(item.model)} · ${esc(schedulingTime(item.retry_at))}</div>`).join('');
    return `<div class="schedule-account"><div class="schedule-account-main"><strong>${esc(account.label || account.id)}</strong>${badge}<span class="subtext">优先级 ${esc(account.priority || 0)}</span></div><div class="schedule-account-detail">${reason ? `<span class="subtext" title="${esc(reason)}">${esc(reason)}</span>` : ''}${retry ? `<span class="subtext">复查 / 恢复 ${esc(schedulingTime(retry))}</span>` : ''}${windows}</div><div class="row-actions"><button type="button" class="button compact" data-schedule-action="refresh" data-id="${esc(account.id)}">复查用量</button><button type="button" class="button compact" data-schedule-action="toggle" data-id="${esc(account.id)}" data-enabled="${account.enabled === false}">${account.enabled === false ? '启用' : '停用'}</button><button type="button" class="button compact" data-schedule-action="reset" data-id="${esc(account.id)}" ${account.quota_blocked || account.enabled === false ? 'disabled' : ''} title="清除插件退避并尝试清除 CPA 冷却，不改变上游额度">清除冷却</button></div></div>`;
  }).join('') || '<div class="empty">添加凭据后，这里会显示调度状态。</div>';
  $('scheduleUpdated').textContent = `更新于 ${schedulingTime(data.observed_at)}`;
}
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
async function loadScheduling() {
  if (schedulingView.loading || schedulingView.saving) return;
  schedulingView.loading = true;
  $('refreshScheduling').disabled = true;
  try {
    const [schedule, egress] = await Promise.all([api('/scheduling'), api('/egress')]);
    renderScheduling(schedule); if (!schedulingView.dirty) renderEgress(egress);
    $('scheduleState').textContent = '';
  } catch (error) { $('scheduleState').textContent = `读取失败：${error.message}`; }
  finally { schedulingView.loading = false; $('refreshScheduling').disabled = false; }
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
$('schedulingTab').addEventListener('click', () => setTab('scheduling'));
$('refreshScheduling').addEventListener('click', loadScheduling);
$('quotaAutoPause').addEventListener('change', async () => {
  const input = $('quotaAutoPause'); input.disabled = true;
  try { await api('/scheduling', { method: 'PUT', body: { quota_auto_pause: input.checked } }); toast('额度策略已保存'); }
  catch (error) { input.checked = !input.checked; toast(error.message); }
  finally { input.disabled = false; loadScheduling(); }
});
$('scheduleAccounts').addEventListener('click', async (event) => {
  const button = event.target.closest('[data-schedule-action]'); if (!button) return;
  button.disabled = true;
  try {
    if (button.dataset.scheduleAction === 'refresh') await api('/credentials/usage', { query: { id: button.dataset.id, refresh: '1' } });
    else if (button.dataset.scheduleAction === 'toggle') await api('/scheduling/credential', { method: 'PUT', body: { id: button.dataset.id, enabled: button.dataset.enabled === 'true' } });
    else { const result = await api('/scheduling/reset', { method: 'POST', body: { id: button.dataset.id } }); if (result.warning) toast(result.warning); }
    await loadScheduling();
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; }
});
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
setInterval(() => { if (!document.hidden && !$('schedulingPanel').hidden && !$('egressDialog').open && !schedulingView.dirty && document.activeElement?.closest('#egressControls') == null) loadScheduling(); }, 15000);
