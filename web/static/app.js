'use strict';
(() => {
  const $ = s => document.querySelector(s), $$ = s => [...document.querySelectorAll(s)];
  const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const icon = name => '<i data-lucide="'+name+'" aria-hidden="true"></i>';
  const modelEntry = () => '<a id="model-entry" class="top-model-entry" href="#llm" data-nav="llm" title="模型接入">'+icon('plug')+'<span>模型接入</span><small id="model-summary" class="visually-hidden">未配置</small></a>';
  const paintIcons = () => lucide.createIcons({attrs:{'stroke-width':1.6}});
  const {fileSize} = PwnDemoModel;
  const lifecycle = PwnDemoLifecycle;
  const service = new PwnMeshService.Service(), projects = [];
  let roundReadVersion = 0, materialReadVersion = 0;
  let refreshTimer, refreshing = false, connected = false, lastSnapshot = '', pendingRefresh = false;
  const statusLabels = {running:'探索中',done:'已完成',paused:'已暂停',pending:'待开始',terminated:'已终止'};
  const types = {ctf:'CTF',audit:'代码审计',pentest:'渗透测试'};
  const actionNames = {pause:'暂停',resume:'继续',restart:'重启',terminate:'终止',delete:'删除'};
  const actionIcons = {pause:'pause',resume:'play',restart:'rotate-ccw',terminate:'square',delete:'trash-2'};
  let graph = null, selectedNode = null, selectedEdge = null, viewTab = 'graph', toastTimer, disposeLLM, disposeInspector;
  let selectedRun = 'current', menuProject = null, menuTrigger = null, command = null, mutating = false;
  let disposeActivity, disposeRecord, activityRecords = new Map(), activityOrigin = null;
  let llmReturn = null, returningFromLLM = false;
  const activityStates = new Map();
  function activityState() {
    const p = viewed(), key = p.id+':'+(p.generation || 0);
    if (!activityStates.has(key)) activityStates.set(key,{query:'',expanded:new Set(),scrollTop:0,revision:null});
    return activityStates.get(key);
  }
  const route = () => {
    const value = location.hash.slice(1);
    return value === 'llm' || projects.some(p => value === 'project/'+p.id) ? value : projects[0] ? 'project/'+projects[0].id : 'empty';
  };
  const current = () => projects.find(p => p.id === route().split('/')[1]) || projects[0];
  const viewed = () => selectedRun === 'current' ? current() : current()?.history.find(p => String(p.generation) === selectedRun) || current();
  const timeNow = () => new Intl.DateTimeFormat('zh-CN',{timeZone:'Asia/Shanghai',hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false}).format(new Date());
  const dateTime = value => value && Number.isFinite(Date.parse(value)) ? new Intl.DateTimeFormat('sv-SE',{timeZone:'Asia/Shanghai',year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hourCycle:'h23'}).format(new Date(value)) : '—';
  const projectTimes = p => '<div class="project-times" aria-label="项目时间范围"><span><time id="project-created-at" title="创建时间" datetime="'+esc(p.createdAt || '')+'">'+dateTime(p.createdAt)+'</time></span><span class="project-time-separator" aria-hidden="true">～</span><span><time id="project-ended-at" '+(p.endedAt ? 'title="结束时间" datetime="'+esc(p.endedAt)+'"' : 'title="尚未结束"')+'>'+dateTime(p.endedAt)+'</time></span></div>';
  const badge = p => '<span class="badge '+(p.archived ? 'paused' : p.status)+'"><i class="status-dot '+p.status+'"></i>'+(p.archived ? '已归档' : statusLabels[p.status])+'</span>';
  function toast(message) { $('#toast').textContent = message; $('#toast').hidden = false; clearTimeout(toastTimer); toastTimer = setTimeout(() => { $('#toast').hidden = true; }, 3200); }
  function closeMenu(restoreFocus = false) {
    $('#project-menu').hidden = true;
    menuTrigger?.setAttribute('aria-expanded','false');
    if (restoreFocus) menuTrigger?.focus();
    menuProject = null; menuTrigger = null;
  }
  function renderSidebar() {
    const search = $('#project-search').value.trim().toLowerCase();
    const list = projects.filter(p => (p.name+' '+p.target+' '+types[p.type]).toLowerCase().includes(search));
    $('#project-count').textContent = search ? list.length+'/'+projects.length : String(projects.length);
    $('#project-list').innerHTML = list.map(p => '<div class="project-row"><a class="project-item '+(route() === 'project/'+p.id ? 'selected' : '')+'" data-project-type="'+esc(p.type)+'" data-project-status="'+esc(p.status)+'" href="#project/'+p.id+'" '+(route() === 'project/'+p.id ? 'aria-current="page"' : '')+'><span class="project-copy"><strong title="'+esc(p.name)+'">'+esc(p.name)+'</strong><small class="project-meta"><span class="project-type">'+types[p.type]+'</span><span class="project-state"><i class="status-dot '+p.status+'"></i>'+statusLabels[p.status]+'</span></small></span></a><button class="icon-button project-more" data-project-menu="'+p.id+'" data-mutates aria-label="'+esc(p.name)+'的项目操作" aria-haspopup="menu" aria-expanded="false" title="项目操作">'+icon('ellipsis')+'</button></div>').join('') || '<div class="empty-state sidebar-empty">'+(projects.length ? '没有找到匹配的项目<button class="text-button" id="clear-search">清除搜索</button>' : '暂无项目')+'</div>';
    $$('[data-nav]').forEach(a => { const active = route() === a.dataset.nav; a.classList.toggle('active',active); if (active) a.setAttribute('aria-current','page'); else a.removeAttribute('aria-current'); });
    if ($('#model-summary')) $('#model-summary').textContent = PwnLLMDemo.summary();
    if ($('#model-entry')) $('#model-entry').title = '模型接入 · '+PwnLLMDemo.summary();
    const home = projects[0] ? '#project/'+projects[0].id : '#projects';
    $('.brand').href = home;
    updateBusy(); paintIcons();
  }
  function updateBusy() { $$('[data-mutates]').forEach(b => { b.disabled = mutating || !connected || b.dataset.unavailable === 'true'; }); }
  async function mutation(work) {
    if (mutating) throw new Error('正在处理上一项操作，请稍后重试。');
    mutating = true; clearTimeout(refreshTimer); service.cancel(); updateBusy();
    try { return await work(); }
    finally { mutating = false; updateBusy(); await refreshWorkspace(true); }
  }
  function projectFooter(p,state,completed) {
    return '<footer class="canvas-footer"><div class="legend" role="group" aria-label="按节点状态筛选">'+(viewTab === 'graph' ? [['done','已完成'],['running','运行中'],['pending','待执行']].map(([id,title]) => '<button data-filter="'+id+'" aria-pressed="false"><i class="legend-dot '+id+'"></i>'+title+'</button>').join('') : '')+'</div><div class="task-progress"><span>'+completed+' / '+state.steps.length+' 个任务</span><span class="progress-track"><i style="width:'+(state.steps.length ? completed/state.steps.length*100 : 0)+'%"></i></span></div></footer>';
  }
  function projectMarkup(p) {
    const c = current(), history = selectedRun !== 'current';
    const state = PwnDemoGraph.buildState(p), completed = state.steps.filter(s => s.status === 'completed').length;
    const actions = history ? '<button class="button secondary" id="return-current">'+icon('arrow-left')+'返回当前轮</button>' :
      '<button class="button secondary" id="add-hint" data-mutates data-unavailable="'+!lifecycle.canHint(c)+'" '+(!lifecycle.canHint(c) ? 'disabled title="重启后可补充信息"' : '')+'>'+icon('message-square-plus')+'补充信息</button>';
    return '<section class="project-page"><header class="project-heading"><div class="project-heading-copy"><div class="title-row"><h1 title="'+esc(p.name)+'">'+esc(p.name)+'</h1>'+badge(p)+'</div><div class="project-description"><div class="project-identity">'+icon(p.type === 'audit' ? 'code-2' : p.type === 'ctf' ? 'flag' : 'globe')+'<span>'+esc(types[p.type])+'</span><i class="meta-divider"></i><span class="target-summary" title="'+esc(p.target)+'">'+esc(p.target)+'</span></div>'+projectTimes(p)+'</div></div><div class="heading-actions">'+modelEntry()+'</div></header>'+
    '<div class="project-body"><section class="canvas-panel"><header class="canvas-header"><div class="view-tabs" role="tablist" aria-label="项目视图">'+[['graph','workflow','画布'],['materials','paperclip','材料'],['result','file-check-2','结果']].map(([id,i,title]) => '<button id="view-tab-'+id+'" data-view="'+id+'" role="tab" aria-selected="'+(viewTab === id)+'" aria-controls="project-view" tabindex="'+(viewTab === id ? '0' : '-1')+'" class="'+(viewTab === id ? 'active' : '')+'">'+icon(i)+title+(id === 'materials' ? '<small>'+p.files.length+'</small>' : '')+'</button>').join('')+'</div>'+
    '<div class="workspace-actions">'+((c.history?.length || 0) ? '<select id="run-select" aria-label="查看项目轮次"><option value="current">当前轮 · '+(c.generation+1)+'</option>'+[...c.history].reverse().map(h => '<option value="'+h.generation+'" '+(selectedRun === String(h.generation) ? 'selected' : '')+'>第 '+(h.generation+1)+' 轮 · 已归档</option>').join('')+'</select>' : '')+'<button class="button secondary" id="project-manage" aria-haspopup="dialog">'+icon('sliders-horizontal')+'项目管理</button>'+actions+'</div></header>'+
    '<div id="project-view" class="'+(viewTab === 'graph' ? 'graph-stage' : 'tab-content')+'" role="tabpanel" aria-labelledby="view-tab-'+viewTab+'">'+(viewTab === 'graph' ? '<div id="graph-host" class="graph-host"></div><div class="canvas-bottom"><div class="zoom-controls"><button class="icon-button" id="zoom-out" aria-label="缩小画布" title="缩小画布">'+icon('minus')+'</button><span id="zoom-label">100%</span><button class="icon-button" id="zoom-in" aria-label="放大画布" title="放大画布">'+icon('plus')+'</button><i></i><button class="icon-button" id="fit-graph" aria-label="适应画布" title="适应画布 · 0">'+icon('maximize')+'</button></div></div>' : viewTab === 'materials' ? materialsMarkup(p) : resultsMarkup(p,state))+'</div>'+projectFooter(p,state,completed)+'</section>'+
    '<aside class="activity-pane" aria-labelledby="activity-title"><header class="activity-heading"><h2>'+icon('notebook-text')+'<span id="activity-title">黑板日志</span></h2><span id="activity-match-count" role="status" aria-live="polite"></span><button id="activity-collapse-all" class="icon-button" title="收起全部展开的日志" aria-label="收起全部展开的日志" hidden>'+icon('list-collapse')+'</button></header><div id="activity-toolbar"></div><div id="activity-content" class="activity-content" role="region" aria-labelledby="activity-title" tabindex="0"></div><button id="activity-to-latest" title="返回最新记录" hidden>'+icon('arrow-up')+'最新记录</button></aside></div></section>';
  }
  function emptyWorkspaceMarkup() {
    return '<section class="project-page empty-project-page"><header class="project-heading"><div class="project-heading-copy empty-project-heading" aria-hidden="true"><div class="title-row"><h1>工作台</h1></div><div class="project-description"><span>尚未创建项目</span></div></div><div class="heading-actions">'+modelEntry()+'</div></header>'+
      '<div class="project-body"><section class="canvas-panel"><header class="canvas-header"><div class="view-tabs" role="tablist" aria-label="项目视图">'+[['graph','workflow','画布'],['materials','paperclip','材料'],['result','file-check-2','结果']].map(([id,i,title]) => '<button id="view-tab-'+id+'" role="tab" aria-selected="'+(id === 'graph')+'" aria-controls="project-view" class="'+(id === 'graph' ? 'active' : '')+'" disabled>'+icon(i)+title+(id === 'materials' ? '<small>0</small>' : '')+'</button>').join('')+'</div><div class="workspace-actions"><button class="button secondary" disabled>'+icon('sliders-horizontal')+'项目管理</button><button class="button secondary" disabled>'+icon('message-square-plus')+'补充信息</button></div></header>'+
      '<div id="project-view" class="graph-stage" role="tabpanel" aria-labelledby="view-tab-graph"><div id="graph-host" class="graph-host"></div><div class="canvas-empty-state"><h2>每一次探索，从一个目标开始。</h2><p>创建一个项目，把复杂的问题一步步展开。</p><button class="button primary" data-create>'+icon('plus')+'新建项目</button></div><div class="canvas-bottom"><div class="zoom-controls"><button class="icon-button" aria-label="缩小画布" disabled>'+icon('minus')+'</button><span>100%</span><button class="icon-button" aria-label="放大画布" disabled>'+icon('plus')+'</button><i></i><button class="icon-button" aria-label="适应画布" disabled>'+icon('maximize')+'</button></div></div></div>'+
      '<footer class="canvas-footer"><div class="legend" role="group" aria-label="按节点状态筛选">'+[['done','已完成'],['running','运行中'],['pending','待执行']].map(([id,title]) => '<button disabled><i class="legend-dot '+id+'"></i>'+title+'</button>').join('')+'</div><div class="task-progress"><span>0 / 0 个任务</span><span class="progress-track"><i style="width:0%"></i></span></div></footer></section>'+
      '<aside class="activity-pane" aria-labelledby="activity-title"><header class="activity-heading"><h2>'+icon('notebook-text')+'<span id="activity-title">黑板日志</span></h2><span id="activity-match-count">0 条记录</span></header><div id="activity-toolbar"><div class="activity-search"><label for="activity-search" class="visually-hidden">搜索黑板日志全文</label>'+icon('search')+'<input id="activity-search" type="search" placeholder="搜索日志全文" disabled></div></div><div id="activity-content" class="activity-content" role="region" aria-labelledby="activity-title" tabindex="0"></div></aside></div></section>';
  }
  function materialsMarkup(p) {
    return '<div class="section-heading"><h2 class="section-title">项目输入</h2></div><div class="project-inputs">'+[['target','起点 / 已知信息',p.target],['goal','终点 / 项目目标',p.goal]].map(([key,label,body]) => '<section class="content-card" data-content-key="'+key+'">'+contentHeader(label,'input:'+key)+'<p id="content-'+key+'" class="content-text is-collapsed">'+esc(body)+'</p></section>').join('')+'</div><div class="section-heading"><h2 class="section-title">材料</h2>'+(selectedRun === 'current' && lifecycle.canHint(p) ? '<button class="text-button" id="add-materials">'+icon('plus')+'添加材料</button>' : '')+'</div>'+
    (p.files.map((f,index) => '<div class="material-row" role="button" tabindex="0" data-material="'+index+'" aria-label="查看材料：'+esc(f.name)+'">'+icon('file-text')+'<span><strong title="'+esc(f.name)+'">'+esc(f.name)+'</strong><small>'+fileSize(f.size)+'</small></span></div>').join('') || '<div class="empty-state">'+icon('folder-open')+'暂无材料</div>');
  }
  function findingBody(f,state) {
    const fact = state.fact_records.find(r => r.id === f.sources?.[0]);
    const step = state.steps.find(s => s.id === fact?.source_step_id);
    return [f.support_valid === false ? '支持证据已失效，请核对来源。' : '',step?.detail?.body || fact?.description || f.reason || '',...(f.evidence || []).map(item => [item.path,item.excerpt].filter(Boolean).join('\n'))].filter(Boolean).join('\n\n');
  }
  function contentHeader(label,key) {
    const id = 'content-'+key.split(':').pop();
    return '<header class="content-heading"><h3>'+esc(label)+'</h3><div class="content-actions"><button class="text-button" data-content-toggle aria-expanded="false" aria-controls="'+id+'">展开全文'+icon('chevron-down')+'</button><button class="text-button" data-content-read="'+esc(key)+'" aria-haspopup="dialog">'+icon('maximize-2')+'放大阅读</button></div></header>';
  }
  function resultsMarkup(p,state) {
    const result = PwnMeshData.buildResult(state,p.logs || []);
    const summary = result.status === 'completed' ? '<article class="result-card content-card" data-content-key="completion">'+contentHeader('探索结论','completion:goal')+'<p id="content-goal" class="content-text is-collapsed">'+esc(result.summary)+'</p></article>' : result.status === 'unverified' ? '<article class="result-card"><p class="content-text">'+esc(result.notice)+'</p></article>' : '';
    return '<div class="section-heading"><h2 class="section-title">'+(p.status === 'done' ? '探索结论' : '阶段性发现')+'</h2>'+(state.findings.length || result.status === 'completed' ? '<button id="export-results" class="text-button">'+icon('download')+'导出记录</button>' : '')+'</div>'+
    (summary+state.findings.map(f => '<article class="result-card content-card" data-content-key="'+esc(f.id)+'">'+contentHeader('验证记录','result:'+f.id)+'<h3 class="result-title" title="'+esc(f.claim)+'">'+esc(f.claim)+'</h3><p class="result-scope">'+icon('code-2')+'<code>'+esc(f.scope)+'</code></p><p id="content-'+esc(f.id)+'" class="content-text is-collapsed">'+esc(findingBody(f,state))+'</p><button class="text-button result-locate" data-focus="finding:'+esc(f.id)+'">'+icon('locate-fixed')+'画布定位</button></article>').join('') || '<div class="empty-state">'+icon('file-search')+'暂无结果</div>');
  }
  function fitContentBodies() {
    $$('.content-card').forEach(card => {
      const body = card.querySelector('.content-text'), toggle = card.querySelector('[data-content-toggle]');
      const line = parseFloat(getComputedStyle(body).lineHeight);
      const expanded = toggle.getAttribute('aria-expanded') === 'true';
      toggle.hidden = !expanded && body.scrollHeight <= line*5+2;
    });
  }
  function readContent(key) {
    const p = viewed(), [kind,id] = key.split(':');
    if (kind === 'input') return openRecord({kind:'项目输入',title:id === 'target' ? '起点 / 已知信息' : '终点 / 项目目标',body:p[id],meta:[{label:'项目',value:p.name}]});
    const state = PwnDemoGraph.buildState(p);
    if (kind === 'completion') { const result=PwnMeshData.buildResult(state,p.logs || []); return openRecord({kind:'探索结论',title:'探索结论',body:result.summary || result.notice,meta:[{label:'项目',value:p.name}]}); }
    const finding = state.findings.find(f => f.id === id);
    if (finding) openRecord({kind:'探索结论',title:finding.claim,body:findingBody(finding,state),meta:[{label:'项目',value:p.name},{label:'范围',value:finding.scope}]},{onLocate:() => focusGraph('finding:'+finding.id)});
  }
  async function readMaterial(index) {
    const p=viewed(), file=p?.files[index], version=++materialReadVersion, page=route();if(!file)return;
    let body='二进制文件或超过 256 KiB 的材料，请下载原始文件查看。';
    if(PwnMeshInputPreview.textFile(file)) {
      const controller=new AbortController(), timer=setTimeout(()=>controller.abort(),15000);
      try{body=await PwnMeshInputPreview.read(PwnMeshInputPreview.downloadURL(p.id,file.id),{signal:controller.signal});}
      catch(error){body=error.name==='AbortError'?'读取超时，请下载原始文件查看。':error.message;}
      finally{clearTimeout(timer);}
    }
    if(version!==materialReadVersion||viewed()?.id!==p.id||viewed()?.generation!==p.generation||route()!==page)return;
    openRecord({kind:'项目材料',title:file.name,body,meta:[{label:'大小',value:fileSize(file.size)},{label:'导入时间',value:dateTime(file.created_at)},{label:'路径',value:file.path},{label:'SHA-256',value:file.sha256}]});
    const footer=$('.inspector-reader[open] .inspector-reader-footer');
    if(footer){const download=document.createElement('a');download.className='button secondary';download.href=PwnMeshInputPreview.downloadURL(p.id,file.id);download.download=file.name;download.innerHTML=icon('download')+'下载原始文件';footer.append(download);paintIcons();}
  }
  function marked(value,query) {
    const text = String(value || '');
    if (!query) return esc(text);
    let result = '', start = 0, at;
    const lower = text.toLowerCase(), needle = query.toLowerCase();
    while ((at = lower.indexOf(needle,start)) !== -1) {
      result += esc(text.slice(start,at))+'<mark>'+esc(text.slice(at,at+query.length))+'</mark>';
      start = at+query.length;
    }
    return result+esc(text.slice(start));
  }
  function logText(value,id,ui) {
    const text = String(value || ''), query = ui.query.trim();
    const hit = query ? text.toLowerCase().indexOf(query.toLowerCase()) : -1;
    const start = Math.max(0,hit-25), preview = (start ? '…' : '')+text.slice(start,start+260)+(start+260 < text.length ? '…' : '');
    const paragraphs = text.split(/\r?\n\s*\r?\n/).map(paragraph => '<p>'+marked(paragraph,query)+'</p>').join('');
    return '<details class="log-long-content" data-log-id="'+esc(id)+'" '+(ui.expanded.has(id) ? 'open' : '')+'><summary><span class="log-text-preview">'+marked(preview,query)+'</span><span class="log-expand"><span class="log-label-closed">展开全文</span><span class="log-label-open">收起全文</span>'+icon('chevron-down')+'<button class="log-read-sticky" data-log-read="'+esc(id)+'" title="在独立窗口中阅读日志全文">'+icon('maximize-2')+'放大阅读</button></span></summary><div class="log-body">'+paragraphs+'</div></details>';
  }
  function fitLogBodies(host,ui) {
    // Short records remain complete. Only content taller than four actual lines folds.
    host.querySelectorAll('.log-long-content').forEach(detail => {
      const body = detail.querySelector(':scope > .log-body'), probe = body.cloneNode(true);
      Object.assign(probe.style,{position:'absolute',visibility:'hidden',pointerEvents:'none',width:detail.getBoundingClientRect().width+'px',margin:'0'});
      detail.parentElement.append(probe);
      const height = probe.getBoundingClientRect().height, lineHeight = parseFloat(getComputedStyle(probe).lineHeight);
      probe.remove();
      if (height <= lineHeight*4+1) { ui.expanded.delete(detail.dataset.logId); detail.replaceWith(body); }
    });
  }
  function timelineEntry({id,phase,time,timestamp,title,body,evidence},ui) {
    const clock = '<time datetime="'+esc(timestamp || '')+'" title="'+esc(timestamp ? dateTime(timestamp) : time)+'">'+esc(time)+'</time>';
    const heading = '<div class="entry-title"><h3><button class="log-title" data-log-read="'+esc(id)+'" aria-label="阅读全文：'+esc(title)+'" aria-haspopup="dialog" title="查看完整日志"><span class="log-entry-title">'+marked(title,ui.query.trim())+'</span>'+icon('chevron-right')+'</button></h3></div>';
    const content = [body,evidence].filter(Boolean).filter((value,index,array) => array.indexOf(value) === index).join('\n\n');
    return '<article class="timeline-entry '+(phase === 'FINDING' ? 'is-finding' : '')+'" data-record="'+esc(id)+'" data-phase="'+esc(phase)+'"><div class="entry-headline">'+heading+'<div class="entry-meta">'+clock+'</div></div>'+(content ? logText(content,id,ui) : '')+'</article>';
  }
  function activityMarkup(entries,ui) {
    return entries.length ? '<div class="log-stream">'+entries.map(entry => timelineEntry(entry,ui)).join('')+'</div>' : '';
  }
  function renderActivity() {
    if (!viewed()) return;
    const host = $('#activity-content'); if (!host) return;
    disposeActivity?.(); disposeActivity = null;
    const p = viewed(), state = PwnDemoGraph.buildState(p);
    disposeInspector?.(); disposeInspector = null;
    const toolbar = $('#activity-toolbar'), latest = $('#activity-to-latest');
    $('#activity-title').textContent = selectedNode ? '节点详情' : selectedEdge ? '关系详情' : '黑板日志';
    toolbar.hidden = Boolean(selectedNode || selectedEdge); latest.hidden = true;
    $('#activity-match-count').hidden = toolbar.hidden;
    $('#activity-collapse-all').hidden = true;
    if (selectedNode || selectedEdge) {
      const nodeKey = selectedNode?.key;
      host.innerHTML = PwnDemoInspector.render({node:selectedNode,edge:selectedEdge,canLocate:Boolean(nodeKey)});
      paintIcons(); disposeInspector = PwnDemoInspector.bind(host,{toast,onLocate:nodeKey ? () => focusGraph(nodeKey) : undefined}); return;
    }
    const entries = PwnDemoActivity.buildEntries(p,state);
    const ui = activityState(), query = ui.query.trim().toLowerCase();
    const revision = JSON.stringify(entries.map(({id,timestamp,title,body,evidence}) => [id,timestamp,title,body,evidence]));
    if (ui.revision !== revision) { ui.revision = revision; ui.scrollTop = 0; }
    activityRecords = new Map(entries.map(e => [e.id,e]));
    const filtered = entries.filter(e => !query || [e.title,e.body,e.evidence].filter(Boolean).join('\n').toLowerCase().includes(query));
    $('#activity-match-count').textContent = filtered.length+' 条'+(query ? '匹配' : '记录');
    toolbar.innerHTML = '<div class="activity-search"><label for="activity-search" class="visually-hidden">搜索黑板日志全文</label>'+icon('search')+'<input id="activity-search" type="search" placeholder="搜索日志全文" value="'+esc(ui.query)+'" autocomplete="off"><button id="activity-clear-search" class="icon-button" aria-label="清除日志搜索" '+(!ui.query ? 'hidden' : '')+'>'+icon('x')+'</button></div>';
    host.innerHTML = activityMarkup(filtered,ui) || '<div class="empty-state log-empty">'+icon('search')+'没有匹配的记录<button class="text-button" id="activity-reset">清除筛选</button></div>';
    fitLogBodies(host,ui);
    const sync = () => {
      ui.scrollTop = host.scrollTop;
      host.querySelectorAll('details[data-log-id]').forEach(el => { if (el.open) ui.expanded.add(el.dataset.logId); else ui.expanded.delete(el.dataset.logId); });
      $('#activity-collapse-all').hidden = !host.querySelector('details[open]');
      latest.hidden = host.scrollTop < 100;
    };
    host.scrollTop = ui.scrollTop; sync();
    host.addEventListener('scroll',sync,{passive:true}); host.addEventListener('toggle',sync,true);
    disposeActivity = () => { sync(); host.removeEventListener('scroll',sync); host.removeEventListener('toggle',sync,true); };
    paintIcons();
  }
  function resetActivityScroll() {
    const ui = activityState();
    disposeActivity?.(); disposeActivity = null;
    ui.scrollTop = 0;
  }
  function collapseLog(detail) {
    const host = $('#activity-content'), entry = detail.closest('.timeline-entry');
    detail.open = false;
    host.scrollTop += entry.getBoundingClientRect().top-host.getBoundingClientRect().top-6;
    detail.querySelector('summary').focus({preventScroll:true});
  }
  function returnToActivity() {
    selectedNode = null; selectedEdge = null;
    if (graph) graph.selectNode(null); else renderActivity();
    const source = activityOrigin && $$('.log-title').find(button => button.dataset.logRead === activityOrigin);
    source?.focus({preventScroll:true}); activityOrigin = null;
  }
  function focusGraph(key,origin = null) {
    if (!graph) { viewTab = 'graph'; render(); }
    activityOrigin = origin;
    graph.setStatusFilter('all');
    $$('[data-filter]').forEach(el => { el.classList.remove('active'); el.setAttribute('aria-pressed','false'); });
    graph.selectNode(key); graph.focusNode(key); $('#clear-selection')?.focus({preventScroll:true});
  }
  function openRecord(record,options = {}) {
    materialReadVersion++;
    disposeRecord?.();
    const source = document.activeElement?.closest('[data-log-read]');
    source?.classList.add('is-reading'); source?.setAttribute('aria-expanded','true');
    disposeRecord = PwnDemoInspector.openRecord(record,{...options,toast,onClose:() => {
      source?.classList.remove('is-reading'); source?.setAttribute('aria-expanded','false');
      disposeRecord = null;
    }});
  }
  function projectInfo(p) {
    const dialog = $('#project-dialog');
    const controls = lifecycle.actions(p).filter(action => ['pause','resume','terminate'].includes(action));
    dialog.innerHTML = '<header class="dialog-header"><div><span class="dialog-eyebrow">项目资料与运行状态</span><h2 id="project-dialog-title">项目管理</h2></div><button class="icon-button" data-close="project-dialog" aria-label="关闭项目管理">'+icon('x')+'</button></header>'+
      '<div class="project-info-body" tabindex="0" role="region" aria-label="项目资料"><div class="project-info-identity"><h3>'+esc(p.name)+'</h3>'+badge(p)+'</div><dl class="project-info-meta">'+[['项目类型',types[p.type]],['材料',p.files.length+' 份'],['创建时间',dateTime(p.createdAt)],['结束时间',dateTime(p.endedAt)]].map(([label,value]) => '<div><dt>'+label+'</dt><dd>'+esc(value)+'</dd></div>').join('')+'</dl>'+[['起点 / 已知信息',p.target],['终点 / 项目目标',p.goal]].map(([label,value]) => '<section class="project-info-section"><h3>'+label+'</h3><p>'+esc(value)+'</p></section>').join('')+'</div>'+
      '<footer class="project-info-footer"><span>'+esc(p.archived ? '历史记录 · 仅供查看' : p.status === 'paused' ? '已暂停，现有记录已保留' : p.status === 'terminated' || p.status === 'done' ? '项目已结束，记录已保留' : '暂停可继续，终止保留记录')+'</span><div>'+controls.map(action => '<button class="button '+(action === 'terminate' ? 'danger-outline' : 'secondary')+'" data-action="'+action+'" data-action-project="'+esc(p.id)+'" data-mutates>'+icon(actionIcons[action])+actionNames[action]+'</button>').join('')+'</div></footer>';
    paintIcons(); updateBusy();
    if (!dialog.open) dialog.showModal();
  }
  function mountGraph(p) {
    graph = PwnDemoGraph.create($('#graph-host'),p,{
      onZoom: zoom => { if ($('#zoom-label')) $('#zoom-label').textContent = Math.round(zoom*100)+'%'; },
      onSelect: node => { selectedNode = node; selectedEdge = null; renderActivity(); },
      onSelectEdge: edge => { selectedEdge = edge && graph?.getEdgeDetails(edge.id); selectedNode = null; renderActivity(); }
    });
    $('#zoom-label').textContent = Math.round(graph.scale*100)+'%';
  }
  function render() {
    materialReadVersion++;
    closeMenu(); disposeRecord?.(); disposeActivity?.(); disposeActivity = null; disposeInspector?.(); disposeInspector = null; graph?.destroy(); graph = null; disposeLLM?.(); disposeLLM = null; selectedNode = null; selectedEdge = null;
    activityOrigin = null;
    const page = route(), p = viewed();
    $('.topbar').hidden = true;
    $('.topbar').innerHTML = '';
    if (page === 'llm') {
      const origin = projects.find(project => project.id === llmReturn?.id) || projects[0];
      $('#main').innerHTML = PwnLLMDemo.render({returnLabel:origin ? '返回项目' : '返回工作台',projectName:origin?.name || ''});
      disposeLLM = PwnLLMDemo.bind($('#main'),{onReturn:() => {
        if (!origin) { location.hash = 'projects'; return; }
        if (!llmReturn || llmReturn.id !== origin.id) llmReturn = {id:origin.id,view:'graph',run:'current',scrollTop:0};
        returningFromLLM = true; location.hash = 'project/'+origin.id;
      }});
    } else if (!p) {
      $('#main').innerHTML = emptyWorkspaceMarkup();
    } else {
      $('#main').innerHTML = projectMarkup(p); if (viewTab === 'graph') mountGraph(p); else fitContentBodies(); renderActivity();
    }
    renderSidebar(); paintIcons();
  }
  function openMenu(id,trigger) {
    if (menuProject === id && menuTrigger === trigger) { closeMenu(); return; }
    closeMenu();
    const p = projects.find(p => p.id === id); if (!p || mutating) return;
    menuProject = id; menuTrigger = trigger; trigger.setAttribute('aria-expanded','true');
    const menu = $('#project-menu');
    menu.innerHTML = lifecycle.actions(p).filter(action => ['restart','delete'].includes(action)).map(action => '<button role="menuitem" data-action="'+action+'" class="'+(action === 'delete' ? 'danger-item' : '')+'">'+icon(actionIcons[action])+actionNames[action]+'</button>').join('');
    menu.hidden = false; paintIcons();
    const box = trigger.getBoundingClientRect();
    menu.style.left = Math.max(8,Math.min(box.right-menu.offsetWidth,innerWidth-menu.offsetWidth-8))+'px';
    menu.style.top = Math.max(8,Math.min(box.bottom+6,innerHeight-menu.offsetHeight-8))+'px';
    menu.querySelector('button')?.focus();
  }
  function confirmAction(id,action) {
    const p = projects.find(p => p.id === id); if (!p || !lifecycle.actions(p).includes(action)) return;
    command = {id,action,generation:p.generation,status:p.status};
    $('#action-title').textContent = actionNames[action]+'项目';
    $('#action-project').textContent = p.name;
    $('#action-description').textContent = {restart:'归档当前轮，清空当前轮任务、执行记录与结论后重新开始。项目输入、材料、补充信息与历史轮次会保留。',terminate:'停止所有未完成任务并保留现有记录。目标不会标为完成，之后可重启。',delete:'删除这个项目及其全部历史记录，同时停止未完成任务。此操作无法撤销。'}[action];
    $('#confirm-action').textContent = actionNames[action]+'项目';
    $('#confirm-action').className = 'button '+(action === 'restart' ? 'primary' : 'danger-button');
    $('#action-error').textContent = '';
    $('#action-dialog').showModal();
  }
  async function performAction(id,action,{generation,status} = {}) {
    return mutation(async () => {
      roundReadVersion++;
      const p = projects.find(p => p.id === id);
      if (!p) throw new Error('项目已不存在，请关闭窗口并重新选择。');
      if (generation !== undefined && (generation !== p.generation || status !== p.status)) throw new Error('项目状态已变化，请关闭窗口后重新确认。');
      if (!lifecycle.actions(p).includes(action)) throw new Error('当前状态不支持此操作。');
      const pendingCreate = action === 'delete' && service.pendingCreate?.created.project.id === id;
      await service.change(id,action,generation ?? p.generation);
      if (pendingCreate) forms.resetCreate();
      if (action === 'delete') { graph?.forgetProject?.(id); if (current()?.id === id) location.hash = 'projects'; }
      if (current()?.id === id) { selectedRun = 'current'; if (action === 'restart') viewTab = 'graph'; }
      toast('项目已'+({pause:'暂停',resume:'继续',restart:'重启',terminate:'终止',delete:'删除'}[action]));
    });
  }
  async function refreshWorkspace(force = false, preferred) {
    clearTimeout(refreshTimer);
    if (mutating && !force) return;
    if (refreshing) { pendingRefresh = true; if (force) service.cancel(); return; }
    refreshing = true;
    const id = preferred ?? (location.hash.startsWith('#project/') ? location.hash.slice(9) : current()?.id || '');
    try {
      const before = current()?.id, result = await service.refresh(id); if (!result) return;
      projects.splice(0,projects.length,...result.projects); connected = true;
      const p = current(), signature = JSON.stringify([p?.id,p?.generation,p?.state,p?.logs,p?.files,p?.history.map(h=>[h.generation,h.archivedAt])]);
      if (route() !== 'llm' && ((!p && !$('.empty-project-page')) || before !== p?.id || !$('.project-page'))) render();
      else if (route() !== 'llm' && p && signature !== lastSnapshot) {
        if (selectedRun !== 'current' && !p.history.some(h=>String(h.generation)===selectedRun && h.state)) selectedRun = 'current';
        const shown = viewed(), holder = document.createElement('div'); holder.innerHTML = projectMarkup(shown);
        $('.project-heading')?.replaceWith(holder.querySelector('.project-heading'));
        $('.workspace-actions')?.replaceWith(holder.querySelector('.workspace-actions'));
        if (graph) graph.setState(PwnDemoGraph.buildState(shown));
        else {
          const container = $('#project-view'), top = container?.scrollTop || 0;
          const expanded = $$('.content-card').filter(card=>card.querySelector('[data-content-toggle]')?.getAttribute('aria-expanded')==='true').map(card=>card.dataset.contentKey);
          if (container) { container.innerHTML = viewTab === 'materials' ? materialsMarkup(shown) : resultsMarkup(shown,PwnDemoGraph.buildState(shown)); fitContentBodies(); expanded.forEach(key=>$$('.content-card').find(card=>card.dataset.contentKey===key)?.querySelector('[data-content-toggle]')?.click()); container.scrollTop = top; }
        }
        const state=PwnDemoGraph.buildState(shown), footer=document.createElement('div'); footer.innerHTML=projectFooter(shown,state,state.steps.filter(step=>step.status==='completed').length); $('.canvas-footer')?.replaceWith(footer.firstElementChild);
        const filter=graph?.getStatusFilter(); $$('[data-filter]').forEach(button=>{button.classList.toggle('active',button.dataset.filter===filter);button.setAttribute('aria-pressed',String(button.dataset.filter===filter));});
        const input=document.activeElement?.id==='activity-search'?document.activeElement:null, selection=input&&[input.selectionStart,input.selectionEnd]; renderActivity(); if(input&&$('#activity-search')){$('#activity-search').focus({preventScroll:true});$('#activity-search').setSelectionRange(...selection);}
      }
      lastSnapshot = signature; renderSidebar();
      if(p)try{localStorage.setItem('pwnmesh.selected-project',p.id);}catch{}
    } catch(error) { if (error.name !== 'AbortError' && !service.scope.controller?.signal.aborted) { connected = false; toast(error.message); updateBusy(); } }
    finally { refreshing = false; const retry=pendingRefresh; pendingRefresh=false;refreshTimer=setTimeout(()=>refreshWorkspace(true),retry?0:document.hidden?10000:2500); }
  }
  async function selectRound(value) {
    const p=current(),version=++roundReadVersion; if(!p)return;
    if(value==='current'){selectedRun=value;render();return;}
    try{await service.archive(p,Number(value));if(version!==roundReadVersion||current()?.id!==p.id||route()!=='project/'+p.id)return;selectedRun=value;render();$('#run-select')?.focus();}
    catch(error){if(version===roundReadVersion){toast(error.message);if($('#run-select'))$('#run-select').value=selectedRun;}}
  }
  document.addEventListener('visibilitychange',()=>{if(!document.hidden)refreshWorkspace(true);});
  window.addEventListener('pagehide',()=>{clearTimeout(refreshTimer);service.cancel();graph?.destroy();});
  window.addEventListener('pageshow',event=>{if(event.persisted){render();refreshWorkspace(true);}});
  const forms = PwnDemoForms.bind({
    getProject:id => projects.find(p => p.id === id), toast,
    onCreate:values => mutation(async () => {
      const id = await service.create(values); $('#project-search').value = ''; selectedRun = 'current'; viewTab = 'graph';
      location.hash = 'project/'+id; await refreshWorkspace(true,id); $('#project-list').scrollTop = 0; toast('项目已创建并开始');
    }),
    onHint:(id,values) => mutation(async () => {
      const p = projects.find(p => p.id === id); if (!p || !lifecycle.canHint(p)) throw new Error('项目已结束，重启后可补充信息。');
      await service.hint(id,values); if (current()?.id === id) selectedRun = 'current'; toast(p.status === 'paused' ? '补充已保存，继续后读取' : '补充已添加');
    })
  });
  function exportResults() {
    const p = viewed(), state = PwnDemoGraph.buildState(p);
    const body = '# '+p.name+'\n\nPwnMesh 项目记录 · 第 '+((p.generation || 0)+1)+' 轮\n\n'+(PwnMeshData.buildResult(state,p.logs || []).summary || '')+'\n\n'+state.findings.map(f => '## '+f.claim+'\n\n范围：'+f.scope+'\n\n'+findingBody(f,state)).join('\n\n');
    const url = URL.createObjectURL(new Blob([body],{type:'text/markdown;charset=utf-8'})), link = document.createElement('a');
    link.href = url; link.download = p.name.replace(/[<>:"/\\|?*\x00-\x1f]/g,'_').slice(0,100)+'-第'+((p.generation || 0)+1)+'轮.md';
    link.click(); setTimeout(() => URL.revokeObjectURL(url),1000); toast('记录已导出');
  }
  document.addEventListener('click',event => {
    if (event.target.closest('[data-nav="llm"]') && route().startsWith('project/')) {
      llmReturn = {id:current().id,view:viewTab,run:selectedRun,scrollTop:$('#project-view')?.scrollTop || 0,expanded:$$('.content-card').filter(card => card.querySelector('[data-content-toggle]')?.getAttribute('aria-expanded') === 'true').map(card => card.dataset.contentKey)};
    }
    const summary = event.target.closest('.log-long-content > summary');
    if (summary && event.target.closest('[data-log-read]')) event.preventDefault();
    else if (summary?.parentElement.open) { event.preventDefault(); collapseLog(summary.parentElement); return; }
    const material = event.target.closest('[data-material]'); if (material) { readMaterial(Number(material.dataset.material)); return; }
    const b = event.target.closest('button'); if (!b || b.disabled) return; const d = b.dataset;
    if ('contentToggle' in d) {
      const card = b.closest('.content-card'), body = card.querySelector('.content-text'), expanded = b.getAttribute('aria-expanded') !== 'true';
      b.setAttribute('aria-expanded',String(expanded)); body.classList.toggle('is-collapsed',!expanded);
      b.innerHTML = (expanded ? '收起全文' : '展开全文')+icon('chevron-down'); paintIcons();
      if (!expanded) { const container = $('#project-view'); container.scrollTop += card.getBoundingClientRect().top-container.getBoundingClientRect().top-16; b.focus({preventScroll:true}); }
      return;
    }
    if ('contentRead' in d) return readContent(d.contentRead);
    if ('projectMenu' in d) return openMenu(d.projectMenu,b);
    if (b.id === 'project-manage') return projectInfo(viewed());
    if ('logRead' in d) {
      const record = activityRecords.get(d.logRead); if (!record) return;
      return openRecord({title:record.title,kind:'黑板日志',body:[record.body,record.evidence].filter(Boolean).join('\n\n'),meta:[{label:'项目',value:viewed().name},{label:'时间',value:record.timestamp ? dateTime(record.timestamp) : record.time}]},{onLocate:record.key ? () => focusGraph(record.key,record.id) : undefined});
    }
    if (d.action) {
      const id = d.actionProject || menuProject; closeMenu();
      if (['pause','resume'].includes(d.action)) performAction(id,d.action).then(() => { if ($('#project-dialog').open) { projectInfo(projects.find(p => p.id === id)); $('#project-dialog [data-action]')?.focus({preventScroll:true}); } }).catch(e => toast(e.message));
      else { $('#project-dialog').close(); confirmAction(id,d.action); } return;
    }
    if ('create' in d) { closeMenu(); return forms.openCreate(); }
    if (d.close === 'action-dialog') { if (!mutating) $('#action-dialog').close(); return; }
    if (d.close === 'project-dialog') { if (!mutating) $('#project-dialog').close(); return; }
    if (d.view) { viewTab = d.view; render(); $('#view-tab-'+viewTab).focus(); return; }
    if (d.focus) return focusGraph(d.focus);
    if (d.filter && graph) { const next = graph.getStatusFilter() === d.filter ? 'all' : d.filter; graph.setStatusFilter(next); $$('[data-filter]').forEach(el => { el.classList.toggle('active',el.dataset.filter === next); el.setAttribute('aria-pressed',String(el.dataset.filter === next)); }); return; }
    if (b.id === 'clear-search') { $('#project-search').value = ''; renderSidebar(); $('#project-search').focus(); }
    if (b.id === 'zoom-in') graph?.zoom(1.18);
    if (b.id === 'zoom-out') graph?.zoom(1/1.18);
    if (b.id === 'fit-graph') graph?.fit();
    if (b.id === 'clear-selection') return returnToActivity();
    if (b.id === 'add-hint' || b.id === 'add-materials') forms.openHint(current().id);
    if (b.id === 'return-current') { roundReadVersion++; selectedRun = 'current'; render(); }
    if (b.id === 'activity-to-latest') $('#activity-content').scrollTop = 0;
    if (b.id === 'activity-clear-search' || b.id === 'activity-reset') {
      resetActivityScroll(); activityState().query = '';
      renderActivity(); $('#activity-search').focus();
    }
    if (b.id === 'activity-collapse-all') {
      resetActivityScroll(); activityState().expanded.clear(); renderActivity(); $('#activity-search').focus();
    }
    if (b.id === 'export-results') exportResults();
  });
  $('#project-search').addEventListener('input',() => { closeMenu(); renderSidebar(); });
  function applyActivitySearch(input) {
    if (!input.isConnected) return;
    const start = input.selectionStart, end = input.selectionEnd;
    resetActivityScroll(); activityState().query = input.value; renderActivity();
    const replacement = $('#activity-search'); replacement.focus({preventScroll:true}); replacement.setSelectionRange(start,end);
  }
  document.addEventListener('input',event => {
    if (event.target.id === 'activity-search' && !event.isComposing) applyActivitySearch(event.target);
  });
  document.addEventListener('compositionend',event => {
    if (event.target.id === 'activity-search') applyActivitySearch(event.target);
  });
  document.addEventListener('change',event => {
    if (event.target.id === 'run-select') selectRound(event.target.value);
  });
  $('#action-form').addEventListener('submit',async event => {
    event.preventDefault(); if (!command || mutating) return;
    const operation = {...command};
    const buttons = [...$('#action-dialog').querySelectorAll('button,input')];
    buttons.forEach(b => b.disabled = true); $('#confirm-action').textContent = '处理中…'; $('#action-error').textContent = '';
    try { await performAction(operation.id,operation.action,operation); $('#action-dialog').close(); }
    catch (e) { $('#action-error').textContent = e.message + ([0,409].includes(e.status) ? '；请关闭窗口，刷新后重新确认。' : ''); if ([0,409].includes(e.status)) command = null; }
    finally { buttons.forEach(b => b.disabled = false); $('#confirm-action').textContent = actionNames[operation.action]+'项目'; }
  });
  $('#action-dialog').addEventListener('cancel',event => { if (mutating) event.preventDefault(); });
  $('#action-dialog').addEventListener('close',() => { command = null; });
  $('#project-dialog').addEventListener('cancel',event => { if (mutating) event.preventDefault(); });
  $('#project-dialog').addEventListener('close',() => { if (!$('#action-dialog').open) $('#project-manage')?.focus({preventScroll:true}); });
  document.addEventListener('pointerdown',event => { if (!event.target.closest('#project-menu,[data-project-menu]')) closeMenu(); });
  let resizeTimer;
  window.addEventListener('resize',() => {
    closeMenu(); clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => {
      fitContentBodies();
      if (!$('#activity-content') || selectedNode || selectedEdge || $('dialog[open]')) return;
      const input = document.activeElement?.id === 'activity-search' ? document.activeElement : null;
      const selection = input && [input.selectionStart,input.selectionEnd];
      renderActivity();
      if (input) { $('#activity-search').focus({preventScroll:true}); $('#activity-search').setSelectionRange(...selection); }
    },150);
  });
  $('#project-list').addEventListener('scroll',() => closeMenu());
  document.addEventListener('keydown',event => {
    if(event.target.matches('[data-material]') && ['Enter',' '].includes(event.key)){event.preventDefault();readMaterial(Number(event.target.dataset.material));return;}
    if (event.target.id === 'activity-search' && event.key === 'Escape') {
      event.preventDefault(); resetActivityScroll(); activityState().query = ''; renderActivity(); $('#activity-search').focus(); return;
    }
    if (!$('#project-menu').hidden && ['ArrowDown','ArrowUp','Home','End','Escape','Tab'].includes(event.key)) {
      if (['Escape','Tab'].includes(event.key)) { closeMenu(event.key === 'Escape'); if (event.key === 'Escape') event.preventDefault(); return; }
      const items = [...$('#project-menu').querySelectorAll('button')], index = items.indexOf(document.activeElement);
      const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length-1 : (index+(event.key === 'ArrowDown' ? 1 : -1)+items.length)%items.length;
      event.preventDefault(); items[next]?.focus(); return;
    }
    const tab = event.target.closest('[role=tab]');
    if (tab && ['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) {
      const tabs = [...tab.parentElement.querySelectorAll('[role=tab]')], index = tabs.indexOf(tab);
      const next = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length-1 : (index+(event.key === 'ArrowRight' ? 1 : -1)+tabs.length)%tabs.length;
      event.preventDefault(); tabs[next].click(); document.getElementById(tabs[next].id)?.focus(); return;
    }
    if (event.target.closest('input,textarea,select,[contenteditable]') || $('dialog[open]') || event.ctrlKey || event.metaKey || event.altKey) return;
    if (event.key === 'n' || event.key === 'N') { event.preventDefault(); forms.openCreate(); }
    if (event.key === '/') { event.preventDefault(); $('#project-search').focus(); }
    if (event.key === 'Escape' && (selectedNode || selectedEdge)) returnToActivity();
  });
  window.addEventListener('hashchange',() => {
    roundReadVersion++;
    if (returningFromLLM && route() === 'project/'+llmReturn?.id) {
      returningFromLLM = false; selectedRun = llmReturn.run; viewTab = llmReturn.view; render();
      $$('.content-card').filter(card => llmReturn.expanded?.includes(card.dataset.contentKey)).forEach(card => card.querySelector('[data-content-toggle]')?.click());
      if ($('#project-view')) $('#project-view').scrollTop = llmReturn.scrollTop;
      $('#model-entry')?.focus({preventScroll:true});
    } else { returningFromLLM = false; selectedRun = 'current'; viewTab = 'graph'; render(); }
    refreshWorkspace(true,location.hash.startsWith('#project/') ? location.hash.slice(9) : current()?.id);
  });
  window.addEventListener('llm-updated',renderSidebar);
  try { if (!location.hash) { const id = new URLSearchParams(location.search).get('project') || localStorage.getItem('pwnmesh.selected-project'); if (id) history.replaceState(null,'','#project/'+id); } } catch {}
  render(); refreshWorkspace(true,location.hash.startsWith('#project/') ? location.hash.slice(9) : '');
})();
