(function(root,factory){
  const common = typeof module === 'object' && module.exports;
  const api = factory(common ? require('./api.js') : root.PwnMeshAPI, common ? require('./data.js') : root.PwnMeshData);
  if(common) module.exports=api; else root.PwnMeshService=api;
})(typeof window === 'object' ? window : globalThis,function(API,Data){
  'use strict';
  const pathFor = id => '/projects/' + encodeURIComponent(id);
  const statuses = {active:'running',stopped:'paused',completed:'done',terminated:'terminated'};
  function projectView(raw,previous={}) {
    return {...previous,id:raw.id,name:raw.title,type:raw.scenario || '',status:statuses[raw.status] || 'pending',readOnly:raw.orchestration_version !== 1,generation:raw.generation || 0,createdAt:raw.created_at,endedAt:raw.terminated_at || previous.endedAt || null,target:previous.target || '',goal:previous.goal || '',files:previous.files || [],history:previous.history || [],state:previous.state || null,logs:previous.logs || [],raw};
  }
  function snapshotView(state,events=[],runs=[],files=[],previous={}) {
    const project=projectView(state.graph.project,previous), facts=state.graph.facts || [];
    const timing=Data.projectTiming(state,runs);
    return {...project,state,logs:Data.buildLogs(state,events,runs),runs,events,files,target:facts.find(f=>f.id==='origin')?.description || '',goal:facts.find(f=>f.id==='goal')?.description || state.goals?.find(g=>g.id==='goal')?.condition || '',startedAt:timing.startedAt,endedAt:timing.endedAt || null};
  }
  async function readArchiveEntry(api, base, entry) {
    if (entry.bytes > 32 * 1024 * 1024) throw new Error('单条历史记录超过 32 MiB，无法在页面完整读取。');
    const bytes = new Uint8Array(entry.bytes);
    let offset = 0;
    for (;;) {
      const chunk = await api.request(base + '/entries/' + entry.cursor + '?offset=' + offset + '&limit=32768');
      if (chunk.encoding !== 'base64' || chunk.offset !== offset || chunk.total_bytes !== entry.bytes) throw new Error('历史记录分块不一致');
      const raw = atob(chunk.data), end = offset + raw.length;
      const complete = chunk.next_offset == null;
      const invalidBoundary = complete ? end !== bytes.length :
        !raw.length || chunk.next_offset !== end || end >= bytes.length;
      if (invalidBoundary) throw new Error('历史记录分块边界无效');
      for (let i = 0; i < raw.length; i++) bytes[offset + i] = raw.charCodeAt(i);
      if (complete) break;
      offset = end;
    }
    // Decode after joining bytes: a chunk can split a UTF-8 character.
    return JSON.parse(new TextDecoder('utf-8', {fatal:true}).decode(bytes));
  }
  class Service {
    constructor(api=new API.Client()){this.api=api;this.scope=new API.RequestScope();this.projects=[];this.events=new Map();this.views=new Map();this.pendingCreate=null;this.uncertainCreate=false;this.pendingHints=new Map();}
    cancel(){this.scope.cancel();}
    async pages(path, options = {}, cursor = -1) {
      const items = [];
      for (;;) {
        const page = await this.api.request(path + '?limit=100&cursor=' + cursor, options);
        if (!Array.isArray(page.items)) throw new Error('历史记录分页格式无效');
        items.push(...page.items);
        if (page.next_cursor == null) return items;
        if (!Number.isSafeInteger(page.next_cursor) || page.next_cursor <= cursor) throw new Error('历史记录分页边界无效');
        cursor = page.next_cursor;
      }
    }
    async refresh(preferred='') {
      const request=this.scope.begin(), options={signal:request.signal};
      const list=await this.api.request('/projects',options); if(!this.scope.current(request.version))return null;
      const projects=list.map(raw=>projectView(raw,this.projects.find(p=>p.id===raw.id))).sort((a,b)=>String(b.createdAt).localeCompare(String(a.createdAt))||b.id.localeCompare(a.id));
      const selected=projects.find(p=>p.id===preferred)||projects[0];
      if(selected){
        const base=pathFor(selected.id), previous=this.views.get(selected.id), summary=JSON.stringify(selected.raw);
        const sameRound=previous?.generation===selected.generation;
        // Terminal projects usually remain stable, but late execution cleanup
        // can still append events without changing the project list summary.
        const terminal=['done','terminated'].includes(selected.status), stableTerminal=sameRound&&terminal&&previous.summary===summary;
        const [loadedState,changes,rounds]=await Promise.all([
          stableTerminal?previous.state:this.api.request(base+'/state',options),
          this.api.executionChanges(base,sameRound?previous.executionRevision:0,selected.generation,options).catch(error=>{
            if(error.status===409&&this.scope.current(request.version)){this.views.delete(selected.id);this.events.delete(selected.id);}throw error;
          }),
          sameRound?previous.rounds:this.pages(base+'/rounds',options)
        ]);
        if(!this.scope.current(request.version))return null;
        const state=terminal&&(changes.items.length||changes.deleted.length)?await this.api.request(base+'/state',options):loadedState;
        if(!this.scope.current(request.version))return null;
        const generation=state.graph.project.generation||0;
        const files=sameRound&&state.revision===previous.state.revision?previous.files:await this.api.request(base+'/inputs',options);
        if(!this.scope.current(request.version))return null;
        const runsByID=new Map((sameRound?previous.runs:[]).map(run=>[run.id,run]));
        for(const id of changes.deleted)runsByID.delete(id);
        for(const run of changes.items)if((run.generation||0)===generation)runsByID.set(run.id,run);
        const runs=[...runsByID.values()];
        let cache=this.events.get(selected.id)||{generation,revision:0,after:0,items:[],caughtUp:false};
        if(cache.generation!==generation||state.revision<cache.revision)cache={generation,revision:0,after:0,items:[],caughtUp:false};
        let after=cache.after,caughtUp=cache.caughtUp;const batch=[];
        if(!caughtUp||state.revision!==cache.revision){
          caughtUp=false;
          for(let page=0;page<5;page++){const items=await this.api.request(base+'/state/events?after='+after,options);if(!items.length){caughtUp=true;break;}const next=Math.max(...items.map(e=>e.revision));if(next<=after){caughtUp=true;break;}batch.push(...items);after=next;if(items.length<1000){caughtUp=true;break;}}
        }
        const identity=await this.api.request(base+'/identity',options);if(!this.scope.current(request.version))return null;
        // Fence both sides of the parallel reads: state can already be from a
        // new round while execution and archive pages still describe the old one.
        if(selected.generation!==generation||(identity.generation||0)!==generation){this.events.delete(selected.id);this.views.delete(selected.id);throw new API.APIError('项目已进入新轮次，正在重新同步。',409);}
        cache={generation,revision:state.revision,after,caughtUp,items:batch.length?cache.items.concat(batch):cache.items};this.events.set(selected.id,cache);
        if(!stableTerminal||changes.items.length||changes.deleted.length||batch.length)Object.assign(selected,snapshotView(state,cache.items.filter(e=>e.revision<=state.revision),runs,files,selected));
        selected.history=rounds.map(round=>{const previous=selected.history.find(p=>p.generation===round.generation);return Object.assign(previous||{status:'paused'},{id:selected.id,name:selected.name,type:selected.type,generation:round.generation,archived:true,archivedAt:round.archived_at,endedAt:previous?.endedAt || round.archived_at,files:previous?.files||[],filesKnown:previous?.filesKnown===true,history:[]});});
        this.views.set(selected.id,{generation,summary,state,runs,files,rounds,executionRevision:changes.through});
      }
      if(!this.scope.current(request.version))return null;for(const id of this.views.keys())if(!projects.some(project=>project.id===id)){this.views.delete(id);this.events.delete(id);}this.projects=projects;return {projects,selected};
    }
    async change(id,action,generation){
      const base=pathFor(id);this.cancel();
      if(action==='delete'){await this.api.request(base,{method:'DELETE'});this.events.delete(id);this.views.delete(id);this.pendingHints.delete(id);if(this.pendingCreate?.created.project.id===id)this.pendingCreate=null;return;}
      if(action==='pause'||action==='resume')return this.api.request(base+'/status',{method:'PUT',body:{status:action==='pause'?'stopped':'active',expected_generation:generation}});
      if(!['restart','terminate'].includes(action))throw new Error('不支持的项目操作。');
      const result=await this.api.request(base+'/'+action,{method:'POST',body:{expected_generation:generation}});this.events.delete(id);this.views.delete(id);return result;
    }
    async create(values){
      if(this.uncertainCreate)throw new Error('上次创建结果尚未确认。请关闭窗口并在项目列表核对，刷新页面后再创建。');
      const payload=Data.validateProject({title:values.name,origin:values.target,goal:values.goal,scenario:values.type});
      if(!this.pendingCreate){
        let created;try{created=await this.api.request('/projects',{method:'POST',body:values.files.length?{...payload,start_paused:true}:payload});}
        catch(error){if(error.status===0){this.uncertainCreate=true;error.message+='；创建结果未确认，请关闭窗口核对项目列表，避免重复创建。';error.uncertainCreate=true;}throw error;}
        this.pendingCreate={created,files:values.files.slice(),uploaded:0};
      }
      const pending=this.pendingCreate,id=pending.created.project.id;
      try{
        for(;pending.uploaded<pending.files.length;pending.uploaded++)await this.api.uploadInput(pathFor(id),pending.files[pending.uploaded]);
        if(pending.files.length)await this.api.request(pathFor(id)+'/status',{method:'PUT',body:{status:'active',expected_generation:pending.created.project.generation||0}});
        this.pendingCreate=null;return id;
      }catch(error){error.pendingCreate=true;error.message+='；项目已保存，已导入材料保留。重试将继续上传并启动同一项目。';throw error;}
    }
    isHintRetry(id,values){const pending=this.pendingHints.get(id);return !!pending&&pending.text===values.text&&pending.files.length===values.files.length&&pending.files.every((file,index)=>file===values.files[index]);}
    async hint(id,values){
      let pending=this.pendingHints.get(id);
      if(!this.isHintRetry(id,values)){
        const key=crypto.randomUUID?.()||Array.from(crypto.getRandomValues(new Uint8Array(16)),byte=>byte.toString(16).padStart(2,'0')).join('');
        pending={text:values.text,files:values.files.slice(),key};this.pendingHints.set(id,pending);
      }
      const result=await this.api.uploadInputs(pathFor(id),values.files,values.text,pending.key);
      if(this.pendingHints.get(id)===pending)this.pendingHints.delete(id);
      return result;
    }
    async archive(project, generation) {
      const base = pathFor(project.id) + '/rounds/' + generation;
      const summary = project.history.find(p => p.generation === generation);
      if (!summary) throw new Error('此历史轮次不存在。');
      if (summary.state) return summary;
      const entries = await this.pages(base + '/entries', {}, 0);
      const records = {state:[],event:[],execution:[],inputs:[]};
      for (const entry of entries) {
        if (!Object.hasOwn(records, entry.kind)) continue;
        records[entry.kind].push(await readArchiveEntry(this.api, base, entry));
      }
      if (!records.state.length) throw new Error('历史轮次缺少状态快照。');
      if (records.inputs.length > 1 || (records.inputs.length === 1 && !Array.isArray(records.inputs[0]))) throw new Error('历史材料清单格式无效。');
      const filesKnown = records.inputs.length === 1;
      const snapshot = snapshotView(records.state[0], records.event, records.execution, filesKnown ? records.inputs[0] : [], summary);
      const value = {...snapshot,archived:true,filesKnown,endedAt:snapshot.endedAt || summary.archivedAt,history:[]};
      Object.assign(summary, value);
      const latest = this.projects.find(p => p.id === project.id)?.history.find(p => p.generation === generation);
      if (latest && latest !== summary) Object.assign(latest, value);
      return latest || summary;
    }
  }
  return {Service,projectView,snapshotView};
});
