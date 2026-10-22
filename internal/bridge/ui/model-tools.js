// 联网工具：在「模型映射」里按模型增删上游内置工具（vercel:*）。
// 服务端把 server_tools 规则折算成每个模型的实际工具集；保存时按整份视图
// 重写成逐模型规则，界面上看到的勾选就是请求里实际追加的工具。
const modelToolsView = { map: {}, catalog: [], draft: [], index: -1, busy: false };

function modelToolsLabel(id) {
  const count = (modelToolsView.map[id] || []).length;
  return count ? ` · ${count}` : '';
}

async function loadModelServerTools() {
  try {
    const data = await api('/models/server-tools');
    modelToolsView.map = data && data.models ? data.models : {};
    modelToolsView.catalog = Array.isArray(data && data.catalog) ? data.catalog : [];
    if (Array.isArray(state.models) && state.models.length) renderModelRows();
  } catch (error) {
    modelToolsView.map = {};
  }
}

function openModelTools(index) {
  const model = state.models[Number(index)];
  if (!model) return;
  modelToolsView.index = Number(index);
  modelToolsView.draft = [...(modelToolsView.map[model.id] || [])];
  const chosen = new Set(modelToolsView.draft);
  const known = new Set(modelToolsView.catalog.map((item) => item.type));
  $('modelToolsModel').textContent = `${model.id} → ${model.upstream_id}`;
  $('modelToolsList').innerHTML = modelToolsView.catalog.map((item) => `<label class="model-tool-option"><input type="checkbox" value="${esc(item.type)}"${chosen.has(item.type) ? ' checked' : ''}><span>${esc(item.label || item.type)}</span><span class="model-tool-note">${esc(item.note || '')}</span></label>`).join('');
  $('modelToolsCustom').value = [...chosen].filter((type) => !known.has(type)).join(', ');
  $('modelToolsState').textContent = chosen.size ? '' : '这个模型目前没有任何联网工具。';
  $('modelToolsDialog').showModal();
}

async function saveModelTools() {
  const model = state.models[modelToolsView.index];
  if (!model || modelToolsView.busy) return;
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
  }
}

$('modelList').addEventListener('click', (event) => {
  const button = event.target.closest('.model-tools');
  if (button) openModelTools(button.dataset.index);
});
$('cancelModelTools').addEventListener('click', () => $('modelToolsDialog').close());
$('closeModelTools').addEventListener('click', () => $('modelToolsDialog').close());
$('saveModelTools').addEventListener('click', saveModelTools);
loadModelServerTools();
