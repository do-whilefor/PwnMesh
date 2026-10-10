(function(root,factory){const api=factory();if(typeof module==='object'&&module.exports)module.exports=api;else root.PwnDemoActivity=api;})(typeof window==='object'?window:globalThis,function(){
  'use strict';
  const formatTime=value=>value&&Number.isFinite(Date.parse(value))?new Intl.DateTimeFormat('en-GB',{timeZone:'Asia/Shanghai',hour:'2-digit',minute:'2-digit',second:'2-digit',hourCycle:'h23'}).format(new Date(value)):'—';
  function buildEntries(project={}){return (project.logs||[]).map(log=>({id:log.id,title:log.title,body:[log.body,log.code].filter(Boolean).join('\n\n'),evidence:(log.artifacts||[]).map(item=>[item.path,item.excerpt].filter(Boolean).join('\n')).join('\n\n'),phase:log.node?.type==='finding'?'FINDING':log.level==='error'?'ERROR':log.phase||'WORKSPACE',timestamp:log.time,time:formatTime(log.time),key:log.node?log.node.type+':'+log.node.id:null,level:log.level})).reverse();}
  return {buildEntries};
});
