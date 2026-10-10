(function(root,factory){
  const common = typeof module === 'object' && module.exports;
  const api = factory(common ? require('./api.js') : root.PwnMeshAPI, common ? require('./data.js') : root.PwnMeshData);
  if(common) module.exports=api; else root.PwnMeshService=api;
})(typeof window === 'object' ? window : globalThis,function(API,Data){
  'use strict';
  const pathFor = id => '/projects/' + encodeURIComponent(id);
  const statuses = {active:'running',stopped:'paused',completed:'done',terminated:'terminated'};
  function projectView(raw,previous={}) {
    return {...previous,id:raw.id,name:raw.title,type:raw.scenario || 'pentest',status:statuses[raw.status] || 'pending',readOnly:raw.orchestration_version !== 1,generation:raw.generation || 0,createdAt:raw.created_at,endedAt:raw.terminated_at || previous.endedAt || null,target:previous.target || '',goal:previous.goal || '',files:previous.files || [],history:previous.history || [],state:previous.state || null,logs:previous.logs || [],raw};
  }
  function snapshotView(state,events=[],runs=[],files=[],previous={}) {
    const project=projectView(state.graph.project,previous), facts=state.graph.facts || [];
    const timing=Data.projectTiming(state,runs);
    return {...project,state,logs:Data.buildLogs(state,events,runs),runs,events,files,target:facts.find(f=>f.id==='origin')?.description || '',goal:facts.find(f=>f.id==='goal')?.description || state.goals?.find(g=>g.id==='goal')?.condition || '',startedAt:timing.startedAt,endedAt:timing.endedAt || null};
  }
  class Service {
    constructor(api=new API.Client()){this.api=api;this.scope=new API.RequestScope();this.projects=[];this.events=new Map();this.pendingCreate=null;this.uncertainCreate=false;}
    cancel(){this.scope.cancel();}
    async pages(path,options={},cursor=-1){const items=[];for(;;){const page=await this.api.request(path+'?limit=100&cursor='+cursor,options);if(!Array.isArray(page.items))throw new Error('历史记录分页格式无效');items.push(...page.items);if(page.next_cursor==null)return items;if(!Number.isSafeInteger(page.next_cursor)||page.next_cursor<=cursor)throw new Error('历史记录分页边界无效');cursor=page.next_cursor;}}
    async refresh(preferred='') {
      const request=this.scope.begin(), options={signal:request.signal};
      const list=await this.api.request('/projects',options); if(!this.scope.current(request.version))return null;
      const projects=list.map(raw=>projectView(raw,this.projects.find(p=>p.id===raw.id))).sort((a,b)=>String(b.createdAt).localeCompare(String(a.createdAt))||b.id.localeCompare(a.id));
      const selected=projects.find(p=>p.id===preferred)||projects[0];
      if(selected){
        const base=pathFor(selected.id);
        const [state,runs,files,rounds]=await Promise.all([this.api.request(base+'/state',options),this.api.projectExecutions(base,options),this.api.request(base+'/inputs',options),this.pages(base+'/rounds',options)]);
        if(!this.scope.current(request.version))return null;
        const generation=state.graph.project.generation||0;
        let cache=this.events.get(selected.id)||{generation,revision:0,after:0,items:[]};
        if(cache.generation!==generation||state.revision<cache.revision)cache={generation,revision:0,after:0,items:[]};
        let after=cache.after;const batch=[];
        for(let page=0;page<5;page++){const items=await this.api.request(base+'/state/events?after='+after,options);if(!items.length)break;const next=Math.max(...items.map(e=>e.revision));if(next<=after)break;batch.push(...items);after=next;if(items.length<1000)break;}
        const identity=await this.api.request(base+'/identity',options);if(!this.scope.current(request.version))return null;
        if((identity.generation||0)!==generation){this.events.delete(selected.id);throw new API.APIError('项目已进入新轮次，正在重新同步。',409);}
        cache={generation,revision:state.revision,after,items:cache.items.concat(batch)};this.events.set(selected.id,cache);
        Object.assign(selected,snapshotView(state,cache.items.filter(e=>e.revision<=state.revision),runs.filter(r=>(r.generation||0)===generation),files,selected));
        selected.history=rounds.map(round=>{const previous=selected.history.find(p=>p.generation===round.generation);return Object.assign(previous||{status:'paused'},{id:selected.id,name:selected.name,type:selected.type,generation:round.generation,archived:true,archivedAt:round.archived_at,endedAt:round.archived_at,files:files.filter(f=>f.created_at<=round.archived_at),history:[]});});
      }
      if(!this.scope.current(request.version))return null;this.projects=projects;return {projects,selected};
    }
    async change(id,action,generation){
      const base=pathFor(id);this.cancel();
      if(action==='delete'){await this.api.request(base,{method:'DELETE'});this.events.delete(id);if(this.pendingCreate?.created.project.id===id)this.pendingCreate=null;return;}
      if(action==='pause'||action==='resume')return this.api.request(base+'/status',{method:'PUT',body:{status:action==='pause'?'stopped':'active'}});
      if(!['restart','terminate'].includes(action))throw new Error('不支持的项目操作。');
      const result=await this.api.request(base+'/'+action,{method:'POST',body:{expected_generation:generation}});this.events.delete(id);return result;
    }
    async create(values){
      if(this.uncertainCreate)throw new Error('上次创建结果尚未确认。请关闭窗口并在项目列表核对，刷新页面后再创建。');
      const payload=Data.validateProject({title:values.name,origin:values.target,goal:values.goal,scenario:values.type});
      if(!this.pendingCreate){
        let created;try{created=await this.api.request('/projects',{method:'POST',body:values.files.length?{...payload,start_paused:true}:payload});}
        catch(error){if(error.status===0){this.uncertainCreate=true;error.message+='；创建结果未确认，请关闭窗口核对项目列表，避免重复创建。';error.uncertainCreate=true;}throw error;}
        this.pendingCreate={created,payload,files:values.files.slice(),uploaded:0};
      }
      const pending=this.pendingCreate,id=pending.created.project.id;
      try{
        for(;pending.uploaded<pending.files.length;pending.uploaded++)await this.api.uploadInput(pathFor(id),pending.files[pending.uploaded]);
        if(pending.files.length)await this.api.request(pathFor(id)+'/status',{method:'PUT',body:{status:'active'}});
        this.pendingCreate=null;return id;
      }catch(error){error.pendingCreate=true;error.message+='；项目已保存，已导入材料保留。重试将继续上传并启动同一项目。';throw error;}
    }
    async hint(id,values){if(values.files.length)return this.api.uploadInputs(pathFor(id),values.files,values.text);return this.api.request(pathFor(id)+'/hints',{method:'POST',body:{content:values.text,creator:'user'}});}
    async archive(project,generation){
      const base=pathFor(project.id)+'/rounds/'+generation, summary=project.history.find(p=>p.generation===generation);
      if(!summary)throw new Error('此历史轮次不存在。');if(summary.state)return summary;
      const entries=await this.pages(base+'/entries',{},0), records={state:[],event:[],execution:[]};
      for(const entry of entries){if(!Object.hasOwn(records,entry.kind))continue;if(entry.bytes>32*1024*1024)throw new Error('单条历史记录超过 32 MiB，无法在页面完整读取。');
        const bytes=new Uint8Array(entry.bytes);let offset=0;
        for(;;){const chunk=await this.api.request(base+'/entries/'+entry.cursor+'?offset='+offset+'&limit=32768');if(chunk.encoding!=='base64'||chunk.offset!==offset||chunk.total_bytes!==entry.bytes)throw new Error('历史记录分块不一致');const raw=atob(chunk.data);for(let i=0;i<raw.length;i++)bytes[offset+i]=raw.charCodeAt(i);if(chunk.next_offset==null)break;if(chunk.next_offset<=offset)throw new Error('历史记录分块边界无效');offset=chunk.next_offset;}
        records[entry.kind].push(JSON.parse(new TextDecoder('utf-8',{fatal:true}).decode(bytes)));
      }
      if(!records.state.length)throw new Error('历史轮次缺少状态快照。');
      const value={...snapshotView(records.state[0],records.event,records.execution,summary.files,summary),archived:true,endedAt:summary.archivedAt,history:[]};Object.assign(summary,value);const latest=this.projects.find(p=>p.id===project.id)?.history.find(p=>p.generation===generation);if(latest&&latest!==summary)Object.assign(latest,value);return latest||summary;
    }
  }
  return {Service,projectView,snapshotView,pathFor};
});
