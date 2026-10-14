(function(root,factory){
  const view = factory();
  if (typeof module === 'object' && module.exports) module.exports = view;
  else root.PwnMeshActivityView = view;
})(typeof globalThis !== 'undefined' ? globalThis : this,function(){
  'use strict';
  const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const icon = name => '<i data-lucide="'+name+'" aria-hidden="true"></i>';
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
  function fitBodies(host,ui) {
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
  function timelineEntry({id,phase,time,timestamp,title,body,evidence},ui,formatTime) {
    const clock = '<time datetime="'+esc(timestamp || '')+'" title="'+esc(timestamp ? formatTime(timestamp) : time)+'">'+esc(time)+'</time>';
    const heading = '<div class="entry-title"><h3><button class="log-title" data-log-read="'+esc(id)+'" aria-label="阅读全文：'+esc(title)+'" aria-haspopup="dialog" title="查看完整日志"><span class="log-entry-title">'+marked(title,ui.query.trim())+'</span>'+icon('chevron-right')+'</button></h3></div>';
    const content = [body,evidence].filter(Boolean).filter((value,index,array) => array.indexOf(value) === index).join('\n\n');
    return '<article class="timeline-entry '+(phase === 'FINDING' ? 'is-finding' : '')+'" data-record="'+esc(id)+'" data-phase="'+esc(phase)+'"><div class="entry-headline">'+heading+'<div class="entry-meta">'+clock+'</div></div>'+(content ? logText(content,id,ui) : '')+'</article>';
  }
  function render(entries,ui,formatTime) {
    return entries.length ? '<div class="log-stream">'+entries.map(entry => timelineEntry(entry,ui,formatTime)).join('')+'</div>' : '';
  }
  return {render,fitBodies};
});
