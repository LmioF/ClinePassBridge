// 联网工具：在「模型映射」里按模型增删上游内置工具（vercel:*）。
// 服务端把 server_tools 规则折算成每个模型的实际工具集；保存时按整份视图
// 重写成逐模型规则，界面上看到的勾选就是请求里实际追加的工具。
const modelToolsView = { map: {}, catalog: [], draft: [], model: null, busy: false, loading: false, ready: false, revision: 0, error: '' };

function modelToolsLabel(id) {
  const count = (modelToolsView.map[id] || []).length;
  return count ? ` · ${count}` : '';
}

async function loadModelServerTools() {
  try {
    const data = await api('/models/server-tools');
    if (!data || !data.models || typeof data.models !== 'object' || Array.isArray(data.models) || !Array.isArray(data.catalog)) {
      throw new Error('工具配置响应不完整，请重试');
    }
    modelToolsView.map = data.models;
    modelToolsView.catalog = data.catalog;
    modelToolsView.error = '';
    if (Array.isArray(state.models) && state.models.length) renderModelRows();
    return true;
  } catch (error) {
    modelToolsView.error = error.message;
    return false;
  }
}

function modelToolsControls() {
  const locked = modelToolsView.busy || modelToolsView.loading || !modelToolsView.ready;
  $('saveModelTools').disabled = locked;
  $('modelToolsCustom').disabled = locked;
  for (const box of $('modelToolsList').querySelectorAll('input')) box.disabled = locked;
  $('retryModelTools').disabled = modelToolsView.busy || modelToolsView.loading;
  $('cancelModelTools').disabled = modelToolsView.busy;
  $('closeModelTools').disabled = modelToolsView.busy;
}

async function openModelTools(index) {
  if (modelToolsView.busy) return;
  const model = state.models[Number(index)];
  if (!model) return;
  const revision = ++modelToolsView.revision;
  modelToolsView.model = { id: model.id, upstream_id: model.upstream_id };
  modelToolsView.ready = false;
  modelToolsView.loading = true;
  modelToolsView.draft = [];
  $('modelToolsModel').textContent = `${model.id} → ${model.upstream_id}`;
  $('modelToolsList').innerHTML = '';
  $('modelToolsCustom').value = '';
  $('modelToolsState').textContent = '正在读取工具配置…';
  $('retryModelTools').hidden = true;
  modelToolsControls();
  if (!$('modelToolsDialog').open) $('modelToolsDialog').showModal();
  const loaded = await loadModelServerTools();
  if (revision !== modelToolsView.revision || !$('modelToolsDialog').open) return;
  modelToolsView.loading = false;
  if (!loaded) {
    $('modelToolsState').textContent = `读取失败：${modelToolsView.error}`;
    $('retryModelTools').hidden = false;
    modelToolsControls();
    return;
  }
  modelToolsView.ready = true;
  modelToolsView.draft = [...(modelToolsView.map[model.id] || [])];
  const chosen = new Set(modelToolsView.draft);
  const known = new Set(modelToolsView.catalog.map((item) => item.type));
  $('modelToolsList').innerHTML = modelToolsView.catalog.map((item) => `<label class="model-tool-option"><input type="checkbox" value="${esc(item.type)}"${chosen.has(item.type) ? ' checked' : ''}><span>${esc(item.label || item.type)}</span><span class="model-tool-note">${esc(item.note || '')}</span></label>`).join('');
  $('modelToolsCustom').value = [...chosen].filter((type) => !known.has(type)).join(', ');
  $('modelToolsState').textContent = chosen.size ? '' : '这个模型目前没有任何联网工具。';
  modelToolsControls();
}

async function saveModelTools() {
  const model = modelToolsView.model;
  if (!model || !modelToolsView.ready || modelToolsView.loading || modelToolsView.busy) return;
  if (!state.models.some((item) => item.id === model.id && item.upstream_id === model.upstream_id)) {
    modelToolsView.ready = false;
    $('modelToolsState').textContent = '模型已变更，请关闭后重新打开';
    modelToolsControls();
    return;
  }
  const seen = new Set();
  const tools = [];
  const add = (type) => {
    const trimmed = String(type || '').trim();
    if (trimmed && !seen.has(trimmed)) { seen.add(trimmed); tools.push(trimmed); }
  };
  for (const box of $('modelToolsList').querySelectorAll('input:checked')) add(box.value);
  for (const raw of $('modelToolsCustom').value.split(',')) add(raw);
  modelToolsView.busy = true;
  $('modelToolsState').textContent = '正在保存…';
  modelToolsControls();
  try {
    const data = await api('/models/server-tools', { method: 'PUT', body: { model: model.id, tools }, fullError: true });
    if (data && data.models) modelToolsView.map = data.models;
    if (data && Array.isArray(data.catalog) && data.catalog.length) modelToolsView.catalog = data.catalog;
    renderModelRows();
    $('modelToolsDialog').close();
    toast(data && data.message ? data.message : '联网工具已保存');
  } catch (error) {
    $('modelToolsState').textContent = `保存未完成：${error.message}`;
  } finally {
    modelToolsView.busy = false;
    modelToolsControls();
  }
}

$('modelList').addEventListener('click', (event) => {
  const button = event.target.closest('.model-tools');
  if (button) openModelTools(button.dataset.index);
});
$('cancelModelTools').addEventListener('click', () => { if (!modelToolsView.busy) $('modelToolsDialog').close(); });
$('closeModelTools').addEventListener('click', () => { if (!modelToolsView.busy) $('modelToolsDialog').close(); });
$('retryModelTools').addEventListener('click', () => {
  const model = modelToolsView.model;
  const index = state.models.findIndex((item) => item.id === model?.id && item.upstream_id === model?.upstream_id);
  if (index >= 0) return openModelTools(index);
});
$('modelToolsForm').addEventListener('submit', (event) => { event.preventDefault(); return saveModelTools(); });
$('modelToolsDialog').addEventListener('cancel', (event) => { if (modelToolsView.busy) event.preventDefault(); });
$('modelToolsDialog').addEventListener('close', () => {
  modelToolsView.revision++;
  modelToolsView.ready = false;
  modelToolsView.loading = false;
});
