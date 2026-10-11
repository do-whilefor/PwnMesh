(function (root, factory) {
  'use strict';
  const api = factory(root);
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.PwnDemoInspector = api;
})(typeof window === 'object' ? window : globalThis, function (root) {
  'use strict';
  const bindings = new WeakMap();
  const readerMotions = new WeakMap();
  let nextId = 0;
  const esc = value => String(value ?? '').replace(/[&<>"']/g, character => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[character]));
  const plain = value => {
    if (value == null) return '';
    if (typeof value === 'string') return value;
    if (typeof value === 'object') return JSON.stringify(value, null, 2);
    return String(value);
  };
  const icon = name => {
    const paths = {
      close: '<path d="m6 6 12 12M6 18 18 6"/>',
      back: '<path d="M19 12H5m6-6-6 6 6 6"/>',
      expand: '<path d="M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5"/>',
      copy: '<rect x="8" y="8" width="12" height="13" rx="2"/><path d="M16 8V5a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h3"/>',
      chevron: '<path d="m9 5 7 7-7 7"/>',
      output: '<path d="m4 6 5 5-5 5m9 2h7"/>',
      locate: '<circle cx="12" cy="12" r="6"/><circle cx="12" cy="12" r="2"/><path d="M12 2v4m0 12v4M2 12h4m12 0h4"/>',
      evidence: '<path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9Zm0 0v6h6M8 13h8M8 17h5"/>'
    };
    return `<svg class="inspector-icon" viewBox="0 0 24 24" aria-hidden="true">${paths[name] || paths.evidence}</svg>`;
  };
  function content({ node, edge, record } = {}) {
    const selected = node || edge || record;
    if (!selected) return null;
    const raw = record ? { detail: record } : selected.raw || {};
    const detail = raw.detail;
    const structured = detail && typeof detail === 'object' && !Array.isArray(detail) ? detail : null;
    const presentation = node && root.PwnMeshGraphView?.nodePresentation(node, 'goal:goal');
    const statuses = { running: '运行中', completed: '已完成', cancelled: '已取消', paused: '已暂停', achieved: '已达成', open: '待执行', pending: '待执行', input: '输入', valid: '有效', verified: '已验证', failed: '失败' };
    const title = plain(selected.label || selected.title || selected.description || selected.id || '未命名记录');
    const body = plain(structured ? structured.body ?? structured.text ?? detail : detail || selected.description || raw.description || raw.condition || raw.claim || title);
    const scope = plain(structured?.scope || raw.scope);
    const output = plain(structured?.output ?? raw.execution_output ?? raw.output);
    const evidenceItems = Array.isArray(structured?.evidence) ? structured.evidence : Array.isArray(raw.evidence) ? raw.evidence : Array.isArray(node?.evidence) ? node.evidence : [];
    const evidence = evidenceItems.filter(item => item != null).map((item, index) => typeof item === 'string' ? { title: `证据 ${String(index + 1).padStart(2, '0')}`, body: item, path: '' } : {
      title: plain(item.title || item.name || `证据 ${String(index + 1).padStart(2, '0')}`),
      body: plain(item.body ?? item.excerpt ?? item.description ?? item),
      path: plain(item.path || item.url || ''),
      reference: [item.run_id, item.start_line != null ? `行 ${item.start_line}${item.end_line != null ? '–' + item.end_line : ''}` : ''].filter(Boolean).join(' · ')
    });
    const meta = record ? (Array.isArray(record.meta) ? record.meta : []).filter(item => item != null).map(item => ({ label: plain(item.label), value: plain(item.value) })) : [{ label: '状态', value: presentation?.statusLabel || selected.statusLabel || statuses[selected.status] || selected.status || '已记录' }];
    if (scope) meta.push({ label: '范围', value: scope });
    if (edge) {
      meta.push({ label: '来源', value: plain(edge.sourceNode?.label || edge.sourceNode?.id || edge.source) });
      meta.push({ label: '去向', value: plain(edge.targetNode?.label || edge.targetNode?.id || edge.target) });
    }
    return { title, body, scope, output, evidence, meta, logRecord: record?.kind === '黑板日志', kind: record ? plain(record.kind || '日志') : node ? presentation?.label || ({ step: '任务', fact: '事实', goal: '目标', finding: '发现' }[node.type] || '节点') : '关系' };
  }
  const collapseSectionButton = label => `<button type="button" class="inspector-section-collapse" data-inspector-action="collapse-section">收起${label}${icon('chevron')}</button>`;
  function evidenceMarkup(item, index, reader = false) {
    const contents = `${item.path ? `<code class="inspector-evidence-path" ${reader ? 'data-inspector-copy-part' : ''}>${esc(item.path)}</code>` : ''}${item.reference ? `<p class="inspector-evidence-reference" ${reader ? 'data-inspector-copy-part' : ''}>${esc(item.reference)}</p>` : ''}<pre class="inspector-text" ${reader ? 'data-inspector-copy-part' : ''}>${esc(item.body)}</pre>`;
    return reader ? `<section class="inspector-reader-evidence"><h4 data-inspector-copy-part>${String(index + 1).padStart(2, '0')} · ${esc(item.title)}</h4>${contents}</section>` : `<details class="inspector-evidence-item"><summary><span>${String(index + 1).padStart(2, '0')}</span><strong>${esc(item.title)}</strong>${icon('chevron')}</summary><div>${contents}${collapseSectionButton('此证据')}</div></details>`;
  }
  function readerMarkup(value, id, canLocate = false) {
    const longTitle = value.title.length > 64 || /[\r\n]/.test(value.title);
    const showBody = value.body !== value.title;
    const metadata = value.meta.length ? `<div class="inspector-reader-meta">${value.meta.map(item => `<p data-inspector-copy-part><span>${esc(item.label)}：</span>${esc(item.value)}</p>`).join('')}</div>` : '';
    const locate = value.logRecord || canLocate ? `<button type="button" class="inspector-reader-locate" data-inspector-action="locate"${canLocate ? '' : ' disabled title="此日志没有对应节点"'}>${icon('locate')}定位节点</button>` : '';
    return `<dialog class="inspector-reader${value.logRecord ? ' log-record-reader' : ''}" aria-labelledby="${id}-reader-title"><header class="inspector-reader-header"><div><span>${esc(value.kind)}</span><h2 id="${id}-reader-title" title="${esc(value.title)}" ${longTitle ? '' : 'data-inspector-copy-part'}>${esc(value.title)}</h2></div><button type="button" class="icon-button" data-inspector-action="close" aria-label="关闭放大阅读">${icon('close')}</button></header><div class="inspector-reader-content" tabindex="0" role="region" aria-label="完整内容">${metadata}${longTitle ? `<section class="inspector-reader-full-title"><p data-inspector-copy-part>${esc(value.title)}</p></section>` : ''}${showBody ? `<section class="inspector-reader-body"><pre class="inspector-text" data-inspector-copy-part>${esc(value.body)}</pre></section>` : ''}${value.output ? `<section><h3 data-inspector-copy-part>执行输出</h3><pre class="inspector-text inspector-code" data-inspector-copy-part>${esc(value.output)}</pre></section>` : ''}${value.evidence.length ? `<section><h3 data-inspector-copy-part>关联证据 · ${value.evidence.length}</h3>${value.evidence.map((item, index) => evidenceMarkup(item, index, true)).join('')}</section>` : ''}</div><footer class="inspector-reader-footer"><button type="button" class="inspector-reader-top" data-inspector-action="top">${icon('back')}回到顶部</button>${locate}<button type="button" class="button secondary" data-inspector-action="copy">${icon('copy')}复制完整内容</button></footer></dialog>`;
  }
  function stopReaderMotion(reader) {
    const motion = readerMotions.get(reader);
    readerMotions.delete(reader);
    if (motion) { motion.onfinish = null; motion.cancel(); }
    reader.classList.toggle('is-closing', false);
  }
  function moveReader(reader, closing, finish) {
    stopReaderMotion(reader);
    const view = reader.ownerDocument?.defaultView || root;
    if (view.matchMedia?.('(prefers-reduced-motion: reduce)').matches || typeof reader.animate !== 'function') {
      finish?.();
      return;
    }
    const visible = { opacity: 1, transform: 'translateY(0) scale(1)' };
    const hidden = { opacity: 0, transform: `translateY(${closing ? 8 : 12}px) scale(.985)` };
    reader.classList.toggle('is-closing', closing);
    try {
      const motion = reader.animate(closing ? [visible, hidden] : [hidden, visible], {
        duration: closing ? 140 : 180,
        easing: closing ? 'cubic-bezier(.4, 0, 1, 1)' : 'cubic-bezier(.16, 1, .3, 1)',
        fill: 'both'
      });
      readerMotions.set(reader, motion);
      motion.onfinish = () => {
        if (readerMotions.get(reader) !== motion) return;
        stopReaderMotion(reader);
        finish?.();
      };
    } catch (_) {
      reader.classList.toggle('is-closing', false);
      finish?.();
    }
  }
  function showReader(reader) {
    if (reader.open) return;
    reader.showModal();
    moveReader(reader, false);
  }
  function closeReader(reader) {
    if (!reader.open || reader.classList.contains('is-closing')) return;
    moveReader(reader, true, () => { if (reader.open) reader.close(); });
  }
  function render(selection = {}) {
    const value = content(selection);
    if (!value) return '';
    const id = `inspector-${++nextId}`;
    const long = value.body.length > 280 || value.body.split('\n').length > 6;
    const showBody = value.body !== value.title || long;
    const metadata = `<dl class="inspector-meta">${value.meta.map(item => `<div${item.label === '范围' ? ' class="inspector-scope-row"' : item.label === '状态' ? ' class="inspector-status-row"' : ''}><dt>${esc(item.label)}</dt><dd title="${esc(item.value)}">${esc(item.value)}</dd></div>`).join('')}</dl>`;
    const expand = long ? `<button type="button" class="inspector-expand-text" data-inspector-action="toggle" aria-expanded="false" aria-controls="${id}-body">展开全文${icon('chevron')}</button>` : '';
    return `<section class="node-inspector"><div class="selection-header"><button type="button" class="inspector-back" id="clear-selection">${icon('back')}黑板日志</button></div><div class="inspector-heading"><h3 class="inspector-title" title="${esc(value.title)}">${esc(value.title)}</h3><div class="inspector-summary"><span class="badge">${esc(value.kind)}</span>${metadata}</div></div><div class="inspector-tools">${expand}<button type="button" data-inspector-action="read" aria-haspopup="dialog">${icon('expand')}放大阅读</button></div>${showBody ? `<section class="inspector-body-section"><pre id="${id}-body" class="inspector-text inspector-body-text${long ? ' is-collapsed' : ''}">${esc(value.body)}</pre>${long ? `<button type="button" class="inspector-expand-text inspector-body-collapse" data-inspector-action="collapse-body" aria-controls="${id}-body" hidden>收起全文${icon('chevron')}</button>` : ''}</section>` : ''}${value.output ? `<details class="inspector-fold"><summary>${icon('output')}<span>执行输出</span>${icon('chevron')}</summary><pre class="inspector-text inspector-code">${esc(value.output)}</pre>${collapseSectionButton('执行输出')}</details>` : ''}${value.evidence.length ? `<details class="inspector-fold inspector-evidence"><summary>${icon('evidence')}<span>关联证据</span><small>${String(value.evidence.length).padStart(2, '0')}</small>${icon('chevron')}</summary><div>${value.evidence.map((item, index) => evidenceMarkup(item, index)).join('')}</div>${collapseSectionButton('全部证据')}</details>` : ''}${readerMarkup(value, id, Boolean(selection.canLocate))}</section>`;
  }
  function bind(host, { toast, onLocate } = {}) {
    const panel = host.querySelector('.node-inspector');
    const previous = bindings.get(host);
    if (previous?.panel === panel) return previous.dispose;
    previous?.dispose();
    if (!panel) return () => {};
    const reader = panel.querySelector('.inspector-reader');
    const body = panel.querySelector('.inspector-body-text');
    const expandedButton = panel.querySelector('[data-inspector-action="toggle"]');
    const collapseBodyButton = panel.querySelector('[data-inspector-action="collapse-body"]');
    const readButton = panel.querySelector('[data-inspector-action="read"]');
    const readerContent = reader.querySelector('.inspector-reader-content');
    const doc = host.ownerDocument;
    let active = true;
    let copying = false;
    let navigating = false;
    host.scrollTop = 0;
    body?.classList.toggle('is-collapsed', Boolean(expandedButton));
    if (expandedButton) expandedButton.setAttribute('aria-expanded', 'false');
    if (collapseBodyButton) collapseBodyButton.hidden = true;
    panel.querySelectorAll('details').forEach(detail => { detail.open = false; });
    function fullText() { return [...reader.querySelectorAll('[data-inspector-copy-part]')].map(part => part.textContent).join('\n\n'); }
    async function copy() {
      if (copying) return;
      copying = true;
      const text = fullText();
      const buttons = [...panel.querySelectorAll('[data-inspector-action="copy"]')];
      buttons.forEach(button => { button.disabled = true; });
      let copied = false;
      try {
        const clipboard = doc.defaultView?.navigator?.clipboard;
        if (clipboard?.writeText) { await clipboard.writeText(text); copied = true; }
      } catch (_) { /* A denied clipboard permission falls back to local selection. */ }
      if (!copied && active && typeof doc.execCommand === 'function') {
        const input = doc.createElement('textarea');
        input.value = text;
        input.className = 'inspector-copy-buffer';
        input.setAttribute('aria-label', '待复制的完整内容');
        const previous = doc.activeElement;
        (reader.open ? reader : panel).append(input);
        try { input.select(); copied = doc.execCommand('copy'); } catch (_) { copied = false; }
        finally { input.remove(); if (previous?.isConnected) previous.focus(); }
      }
      if (active) {
        buttons.forEach(button => { button.disabled = false; });
        if (typeof toast === 'function') toast(copied ? '完整内容已复制' : '复制未完成，请在放大阅读中选择文本。');
      }
      copying = false;
    }
    function alignSection(section, focusTarget) {
      if (!section) return;
      const headerHeight = panel.querySelector('.selection-header')?.getBoundingClientRect().height || 0;
      const parentFold = section.parentElement?.closest?.('.inspector-fold[open]');
      const parentHeight = parentFold?.querySelector('summary')?.getBoundingClientRect().height || 0;
      const relativeTop = section.getBoundingClientRect().top - host.getBoundingClientRect().top - headerHeight - parentHeight;
      host.scrollTop = Math.max(0, host.scrollTop + relativeTop);
      focusTarget?.focus({ preventScroll: true });
    }
    function setExpanded(expanded) {
      if (!body || !expandedButton) return;
      expandedButton.setAttribute('aria-expanded', String(expanded));
      body.classList.toggle('is-collapsed', !expanded);
      expandedButton.innerHTML = (expanded ? '收起全文' : '展开全文') + icon('chevron');
      if (collapseBodyButton) collapseBodyButton.hidden = !expanded;
      if (!expanded) alignSection(panel.querySelector('.inspector-tools'), expandedButton);
    }
    function closeSection(section) {
      if (!section) return;
      section.open = false;
      alignSection(section, section.querySelector('summary'));
    }
    function click(event) {
      const button = event.target.closest('button[data-inspector-action]');
      if (!button) {
        const summary = event.target.closest('summary');
        const section = summary?.closest('details');
        if (section?.open && panel.contains(section)) { event.preventDefault(); closeSection(section); }
        return;
      }
      if (!panel.contains(button) || button.disabled) return;
      const action = button.dataset.inspectorAction;
      if (action === 'toggle') setExpanded(expandedButton.getAttribute('aria-expanded') !== 'true');
      else if (action === 'collapse-body') setExpanded(false);
      else if (action === 'collapse-section') closeSection(button.closest('details'));
      else if (action === 'top') readerContent.scrollTop = 0;
      else if (action === 'read') {
        navigating = false;
        showReader(reader);
        readerContent.scrollTop = 0;
      } else if (action === 'close') closeReader(reader);
      else if (action === 'copy') void copy();
      else if (action === 'locate' && typeof onLocate === 'function') {
        navigating = true;
        stopReaderMotion(reader);
        if (reader.open) reader.close();
        onLocate();
      }
    }
    function restoreFocus() {
      stopReaderMotion(reader);
      if (active && !navigating && readButton?.isConnected) readButton.focus({ preventScroll: true });
    }
    function cancel(event) { event.preventDefault(); closeReader(reader); }
    function backdrop(event) {
      if (event.target !== reader) return;
      const box = reader.getBoundingClientRect();
      if (event.clientX < box.left || event.clientX > box.right || event.clientY < box.top || event.clientY > box.bottom) closeReader(reader);
    }
    panel.addEventListener('click', click);
    reader.addEventListener('close', restoreFocus);
    reader.addEventListener('cancel', cancel);
    reader.addEventListener('click', backdrop);
    const dispose = () => {
      if (!active) return;
      active = false;
      panel.removeEventListener('click', click);
      reader.removeEventListener('close', restoreFocus);
      reader.removeEventListener('cancel', cancel);
      reader.removeEventListener('click', backdrop);
      stopReaderMotion(reader);
      if (reader.open) reader.close();
      reader.remove();
      if (bindings.get(host)?.dispose === dispose) bindings.delete(host);
    };
    bindings.set(host, { panel, dispose });
    return dispose;
  }
  function openRecord(record, { toast, onClose, onLocate } = {}) {
    const value = content({ record });
    if (!value) return () => {};
    const doc = root.document;
    if (!doc?.body) throw new Error('A document is required to open a record.');
    const previousFocus = doc.activeElement;
    const mount = doc.createElement('div');
    mount.className = 'inspector-record-mount';
    mount.innerHTML = `<section class="node-inspector">${readerMarkup(value, `inspector-${++nextId}`, typeof onLocate === 'function')}</section>`;
    doc.body.append(mount);
    const reader = mount.querySelector('.inspector-reader');
    const locateButton = typeof onLocate === 'function' ? reader.querySelector('[data-inspector-action="locate"]') : null;
    const unbind = bind(mount, { toast });
    let active = true;
    const dispose = () => {
      if (!active) return;
      active = false;
      reader.removeEventListener('close', dispose);
      locateButton?.removeEventListener('click', locate);
      unbind();
      mount.remove();
      if (previousFocus?.isConnected) previousFocus.focus({ preventScroll: true });
      if (typeof onClose === 'function') onClose();
    };
    function locate() {
      if (!active) return;
      dispose();
      onLocate();
    }
    locateButton?.addEventListener('click', locate);
    reader.addEventListener('close', dispose);
    try {
      showReader(reader);
      reader.querySelector('.inspector-reader-content').scrollTop = 0;
    } catch (error) { dispose(); throw error; }
    return dispose;
  }
  return Object.freeze({ render, bind, openRecord });
});
