(function () {
  'use strict';
  const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const icon = name => `<i data-lucide="${name}" aria-hidden="true"></i>`;
  const scenarios = {
    ctf: { name: '例如：Web 题目分析', target: '题目描述、题目地址或已知线索', goal: '例如：定位漏洞并获取 Flag' },
    pentest: { name: '例如：核心业务 API 渗透测试', target: '目标地址、测试范围或已知线索', goal: '需要验证的问题，以及判断完成的标准' },
    audit: { name: '例如：核心服务代码审计', target: '仓库或代码路径、分支与已知问题', goal: '需要审查的模块，以及判断完成的标准' }
  };
  let activeBinding;

  function bind({ getProject, onCreate, onHint, toast = () => {} }) {
    if (![getProject, onCreate, onHint].every(callback => typeof callback === 'function')) throw new TypeError('Project form callbacks are required.');
    activeBinding?.destroy();
    const { validateProject, validateHint, validateFiles, isProjectClosed, createFormDrafts, PROJECT_TYPES, fileSize } = window.PwnDemoModel;
    const hintUnavailable = project => project?.readOnly ? '历史项目只读，请新建项目继续。' : isProjectClosed(project) ? '项目已结束，无法补充信息。' : '';
    const drafts = createFormDrafts();
    const $ = selector => document.querySelector(selector);
    const createDialog = $('#create-dialog'), hintDialog = $('#hint-dialog');
    const createForm = $('#create-form'), hintForm = $('#hint-form');
    if (!createDialog || !hintDialog || !createForm || !hintForm) throw new Error('Project form dialogs are missing.');
    const controller = new AbortController(), options = { signal: controller.signal };
    let createBusy = false, hintBusy = false, hintProjectId = null, createLocked = false, createUncertain = false;
    const icons = () => window.lucide?.createIcons({ attrs: { 'stroke-width': 1.6 } });
    const wait = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));
    const metadata = files => Array.from(files || []);
    const shortcut = '<span class="forms-shortcut">Ctrl / ⌘ + Enter</span>';
    const upload = (prefix, listId) => `<div class="forms-material-label"><label class="field-label" for="${prefix}-files">材料 <span>可选</span></label><span id="${prefix === 'project' ? 'create' : 'hint'}-file-count" class="forms-file-count"></span></div><label class="forms-upload" for="${prefix}-files">${icon('paperclip')}<span>选择文件</span><small>上传原始文件</small><input id="${prefix}-files" type="file" multiple aria-describedby="${prefix}-file-limits"></label><p class="forms-file-limits" id="${prefix}-file-limits">最多 32 个 · 单个 ≤ 256 MiB · 总计 ≤ 512 MiB</p><ul id="${listId}" class="file-list forms-files"></ul>`;

    createDialog.classList.add('forms-dialog');
    hintDialog.classList.add('forms-dialog');
    createForm.noValidate = true;
    hintForm.noValidate = true;
    createForm.innerHTML = `<header class="dialog-header"><div class="forms-branded-heading"><span class="forms-brand"><img src="/static/brand.png" alt="PwnMesh"></span><h2 id="create-title">新建项目</h2></div><button type="button" class="icon-button" data-close="create-dialog" aria-label="关闭创建项目">${icon('x')}</button></header>
      <div class="type-choices forms-type-choices" role="radiogroup" aria-label="项目类型">${PROJECT_TYPES.map(type => `<label><input type="radio" name="type" value="${type.value}"${type.value === 'pentest' ? ' checked' : ''}><span>${icon(type.icon)}${type.label}</span></label>`).join('')}</div>
      <label class="field-label" for="project-name">项目名称</label><input id="project-name" name="name" maxlength="200" required autocomplete="off">
      <label class="field-label" for="project-target">起点 / 已知信息</label><textarea id="project-target" name="target" rows="2" maxlength="32768" required></textarea>
      <label class="field-label" for="project-goal">终点 / 项目目标</label><textarea id="project-goal" name="goal" rows="2" maxlength="32768" required></textarea>
      ${upload('project', 'selected-files')}<p id="create-error" class="form-error" role="alert"></p><footer class="dialog-footer">${shortcut}<button type="button" class="button secondary" data-close="create-dialog">取消</button><button type="submit" class="button primary">创建并开始</button></footer>`;
    hintForm.innerHTML = `<header class="dialog-header"><div><h2 id="hint-title">补充信息</h2><p id="hint-project-name" class="forms-project-name"></p></div><button type="button" class="icon-button" data-close="hint-dialog" aria-label="关闭补充信息">${icon('x')}</button></header>
      <p id="hint-status" class="forms-status" role="status" hidden></p><label class="field-label" for="hint-text">补充内容</label><textarea id="hint-text" name="text" rows="4" maxlength="32768" placeholder="补充线索、测试范围或需要关注的问题…"></textarea>
      <details id="hint-imported" class="forms-imported" hidden><summary><span>已导入材料</span><span id="hint-imported-count"></span>${icon('chevron-down')}</summary><ul id="hint-imported-files" class="file-list forms-files"></ul></details>
      ${upload('hint', 'hint-selected-files')}<p class="form-error" id="hint-error" role="alert"></p><footer class="dialog-footer">${shortcut}<button type="button" class="button secondary" data-close="hint-dialog">取消</button><button type="submit" class="button primary">添加</button></footer>`;

    const errors = { create: $('#create-error'), hint: $('#hint-error') };
    function report(kind, message = '') {
      errors[kind].textContent = message;
      (kind === 'create' ? createForm : hintForm).querySelectorAll('[aria-invalid]').forEach(field => field.removeAttribute('aria-invalid'));
    }
    function refreshScenario() {
      const type = createForm.elements.namedItem('type').value;
      const scenario = scenarios[type] || scenarios.pentest;
      $('#project-name').placeholder = scenario.name;
      $('#project-target').placeholder = scenario.target;
      $('#project-goal').placeholder = scenario.goal;
    }
    function captureCreate() {
      const previous = drafts.getCreate();
      drafts.setCreate({ name: $('#project-name').value, target: $('#project-target').value, goal: $('#project-goal').value, type: createForm.elements.namedItem('type').value, files: previous.files });
    }
    function captureHint() {
      if (hintProjectId === null) return;
      const draft = drafts.getHint(hintProjectId);
      drafts.setHint(hintProjectId, { text: $('#hint-text').value, files: draft.files });
    }
    function list(files, kind, imported = false) {
      return files.map((file, index) => `<li>${icon('file')}<span title="${esc(file.name)}">${esc(file.name)}</span><small>${fileSize(file.size)}</small>${imported ? '' : `<button type="button" class="icon-button" ${kind === 'create' ? 'data-remove-file' : 'data-remove-hint-file'}="${index}" aria-label="移除 ${esc(file.name)}">${icon('x')}</button>`}</li>`).join('');
    }
    function renderCreateFiles() {
      const files = drafts.getCreate().files;
      $('#selected-files').innerHTML = list(files, 'create');
      $('#selected-files').querySelectorAll('[data-remove-file]').forEach(button => { button.disabled = createBusy || createLocked || createUncertain; });
      $('#create-file-count').textContent = `${files.length} / 32`;
      icons();
    }
    function renderHintFiles() {
      const project = getProject(hintProjectId), imported = project?.files || [];
      const files = drafts.getHint(hintProjectId).files;
      $('#hint-selected-files').innerHTML = list(files, 'hint');
      $('#hint-imported-files').innerHTML = list(imported, 'hint', true);
      $('#hint-imported-count').textContent = String(imported.length);
      $('#hint-imported').hidden = !imported.length;
      $('#hint-file-count').textContent = `${imported.length + files.length} / 32（含已导入）`;
      icons();
    }
    function restoreCreate() {
      const draft = drafts.getCreate();
      for (const name of ['name', 'target', 'goal']) $(`#project-${name}`).value = draft[name];
      createForm.elements.namedItem('type').value = draft.type;
      $('#project-files').value = '';
      refreshScenario();
      renderCreateFiles();
    }
    function hintStatus(project) {
      const status = $('#hint-status');
      status.hidden = project?.status !== 'paused' && !hintUnavailable(project);
      status.textContent = hintUnavailable(project) || (project?.status === 'paused' ? '项目已暂停，补充内容将在继续后使用。' : '');
      hintForm.querySelector('[type="submit"]').disabled = hintBusy || !!hintUnavailable(project);
    }
    function pending(kind, busy) {
      const form = kind === 'create' ? createForm : hintForm;
      form.setAttribute('aria-busy', String(busy));
      form.querySelectorAll('input,textarea,select,button').forEach(control => { control.disabled = busy; });
      if (kind === 'create' && !busy) { if (createLocked) form.querySelectorAll('input,textarea,[data-remove-file]').forEach(control => { control.disabled = true; }); if (createUncertain) form.querySelector('[type="submit"]').disabled = true; }
      form.querySelector('[type="submit"]').textContent = busy ? kind === 'create' ? '创建中…' : '添加中…' : kind === 'create' ? '创建并开始' : '添加';
      if (kind === 'hint' && !busy && hintProjectId !== null) hintStatus(getProject(hintProjectId));
    }
    function openCreate() {
      if (createBusy || hintBusy) { toast('正在提交，请稍候。'); return false; }
      if (hintDialog.open) { captureHint(); hintDialog.close(); }
      if (createDialog.open) { $('#project-name').focus(); return true; }
      restoreCreate();
      createDialog.showModal();
      $('#project-name').focus();
      return true;
    }
    function openHint(id) {
      if (createBusy || hintBusy) { toast('正在提交，请稍候。'); return false; }
      const project = getProject(id);
      if (!project || hintUnavailable(project)) { toast(project ? hintUnavailable(project) : '项目不存在或已移除。'); return false; }
      if (createDialog.open) { captureCreate(); createDialog.close(); }
      if (hintProjectId !== null) captureHint();
      hintProjectId = id;
      const draft = drafts.getHint(id);
      $('#hint-text').value = draft.text;
      $('#hint-files').value = '';
      $('#hint-project-name').textContent = project.name;
      $('#hint-imported').open = false;
      report('hint');
      hintStatus(project);
      renderHintFiles();
      if (!hintDialog.open) hintDialog.showModal();
      $('#hint-text').focus();
      return true;
    }

    createForm.addEventListener('input', () => { if (!createBusy) { captureCreate(); report('create'); } }, options);
    createForm.addEventListener('change', event => { if (event.target.name === 'type' && !createBusy) { captureCreate(); refreshScenario(); report('create'); } }, options);
    hintForm.addEventListener('input', event => { if (event.target.id === 'hint-text' && !hintBusy) { captureHint(); report('hint'); } }, options);
    for (const kind of ['create', 'hint']) {
      const input = $(kind === 'create' ? '#project-files' : '#hint-files');
      input.addEventListener('change', () => {
        if (kind === 'create' ? createBusy : hintBusy) return;
        const incoming = metadata(input.files); input.value = '';
        if (!incoming.length) return;
        const draft = kind === 'create' ? drafts.getCreate() : drafts.getHint(hintProjectId);
        const next = [...draft.files, ...incoming];
        const project = kind === 'hint' ? getProject(hintProjectId) : null;
        const error = kind === 'hint' && (!project || hintUnavailable(project)) ? hintUnavailable(project) || '项目不存在或已移除。' : validateFiles(next, project?.files || []);
        report(kind, error);
        if (error) { input.setAttribute('aria-invalid', 'true'); return; }
        draft.files = next;
        if (kind === 'create') { drafts.setCreate(draft); renderCreateFiles(); }
        else { drafts.setHint(hintProjectId, draft); renderHintFiles(); }
      }, options);
    }
    for (const [kind, dialog, form] of [['create', createDialog, createForm], ['hint', hintDialog, hintForm]]) {
      dialog.addEventListener('click', event => {
        const button = event.target.closest('button');
        if (!button) return;
        if (button.dataset.close === dialog.id) { event.stopPropagation(); if (!(kind === 'create' ? createBusy : hintBusy)) dialog.close(); return; }
        const attribute = kind === 'create' ? 'data-remove-file' : 'data-remove-hint-file';
        if (!button.hasAttribute(attribute)) return;
        event.stopPropagation();
        if (kind === 'create' ? createBusy || createLocked || createUncertain : hintBusy) return;
        const draft = kind === 'create' ? drafts.getCreate() : drafts.getHint(hintProjectId);
        const index = Number(button.getAttribute(attribute));
        if (!Number.isInteger(index) || index < 0 || index >= draft.files.length) return;
        draft.files.splice(index, 1); report(kind);
        if (kind === 'create') { drafts.setCreate(draft); renderCreateFiles(); }
        else { drafts.setHint(hintProjectId, draft); renderHintFiles(); }
      }, options);
      dialog.addEventListener('cancel', event => { if (kind === 'create' ? createBusy : hintBusy) event.preventDefault(); }, options);
      dialog.addEventListener('close', () => { if (kind === 'create') captureCreate(); else captureHint(); }, options);
      form.addEventListener('keydown', event => {
        if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
          event.preventDefault(); event.stopPropagation();
          if (!event.repeat && !(kind === 'create' ? createBusy : hintBusy)) form.requestSubmit();
        }
      }, options);
    }
    createForm.addEventListener('submit', async event => {
      event.preventDefault();
      if (createBusy || createUncertain) return;
      captureCreate();
      const draft = drafts.getCreate(), values = { ...draft, name: draft.name.trim(), target: draft.target.trim(), goal: draft.goal.trim() };
      const error = validateProject(values); report('create', error);
      if (error) {
        const key = !values.name || values.name.length > 200 ? 'name' : !values.target || values.target.length > 32768 ? 'target' : !values.goal || values.goal.length > 32768 ? 'goal' : 'files';
        $(`#project-${key}`).setAttribute('aria-invalid', 'true'); $(`#project-${key}`).focus(); return;
      }
      createBusy = true; pending('create', true);
      const started = Date.now();
      try {
        await onCreate(values);
        await wait(Math.max(0, 300 - (Date.now() - started)));
        createLocked = false; createUncertain = false; drafts.clearCreate(); restoreCreate(); report('create');
        createDialog.close();
      } catch (error) {
        await wait(Math.max(0, 300 - (Date.now() - started)));
        createLocked = !!error?.pendingCreate; createUncertain = !!error?.uncertainCreate;
        report('create', error?.message || '创建失败，请重试。');
      } finally { createBusy = false; pending('create', false); }
    }, options);
    hintForm.addEventListener('submit', async event => {
      event.preventDefault();
      if (hintBusy || hintProjectId === null) return;
      captureHint();
      const id = hintProjectId, draft = drafts.getHint(id), values = { text: draft.text.trim(), files: draft.files };
      const project = getProject(id), error = hintUnavailable(project) || validateHint(values, project); report('hint', error);
      hintStatus(project);
      if (error) { if (!values.text && !values.files.length) { $('#hint-text').setAttribute('aria-invalid', 'true'); $('#hint-text').focus(); } return; }
      hintBusy = true; pending('hint', true);
      const started = Date.now();
      try {
        await onHint(id, values);
        await wait(Math.max(0, 300 - (Date.now() - started)));
        drafts.clearHint(id); $('#hint-text').value = ''; $('#hint-files').value = '';
        renderHintFiles(); report('hint'); hintProjectId = null;
        hintDialog.close();
      } catch (error) {
        await wait(Math.max(0, 300 - (Date.now() - started)));
        report('hint', error?.message || '添加失败，请重试。');
      } finally { hintBusy = false; pending('hint', false); }
    }, options);
    restoreCreate(); icons();
    const resetCreate = () => { createLocked = false; createUncertain = false; drafts.clearCreate(); restoreCreate(); pending('create', false); report('create'); };
    const api = { openCreate, openHint, resetCreate, destroy: () => controller.abort() };
    activeBinding = api;
    return api;
  }
  window.PwnDemoForms = { bind };
})();
