'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const {Service,projectView,snapshotView}=require('./static/service.js');
const {APIError}=require('./static/api.js');
const model=require('./static/model.js');
const lifecycle=require('./static/lifecycle.js');
const stamp='2026-10-10T02:00:00Z';
const snapshot=(id='a',generation=0,revision=1)=>({graph:{project:{id,title:id,scenario:'pentest',status:'active',orchestration_version:1,generation,created_at:stamp},facts:[{id:'origin',description:'https://authorized.test',created_at:stamp},{id:'goal',description:'Find supported evidence',created_at:stamp}],intents:[],hints:[]},goals:[],steps:[],fact_records:[],findings:[],revision});
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
// Keep refresh behavior deterministic without loading browser geometry or timers.
function workspaceHarness(project,run='current'){
 const vm=require('node:vm'),nodes=new Map(),node=()=>({textContent:'',innerHTML:'',hidden:false,scrollTop:0,querySelector(){return null;},querySelectorAll(){return [];},replaceWith(){}});
 for(const selector of ['#main','#project-menu','#view-tab-materials small','.project-page'])nodes.set(selector,node());
 const badge=nodes.get('#view-tab-materials small'),main=nodes.get('#main');
 Object.defineProperty(main,'innerHTML',{set(value){this.markup=value;const count=value.match(/id="view-tab-materials"[^]*?<small>([^<]*)<\/small>/);if(count)badge.textContent=count[1];},get(){return this.markup;}});
 const service={scope:{},async refresh(){return {projects:[project]};}};
 const context={document:{hidden:false,activeElement:null,querySelector:selector=>nodes.get(selector)||null,querySelectorAll:()=>[],createElement:node},location:{hash:'#project/'+project.id},PwnDemoModel:model,PwnDemoLifecycle:lifecycle,PwnMeshService:{Service:function(){return service;}},lucide:{createIcons(){}},setTimeout(){},clearTimeout(){},localStorage:{setItem(){}}};
 const source=fs.readFileSync(path.join(__dirname,'static/app.js'),'utf8');
 vm.runInNewContext(source.slice(0,source.indexOf("  document.addEventListener('visibilitychange'"))+`
   renderSidebar=()=>{};renderActivity=()=>{};mountGraph=()=>{graph={setState(){},getStatusFilter(){},destroy(){}};};
   globalThis.app={projects,activityStates,activityRecords,activityState,render,refreshWorkspace,setRun(value){selectedRun=value;}};
 })();`,context);
 context.app.projects.push(project);context.app.setRun(run);context.app.render();
 return {app:context.app,service,badge};
}
function fixture(states={a:snapshot()}){const calls=[];return {calls,async request(url,options={}){calls.push({url,options});if(url==='/projects')return Object.values(states).map(s=>s.graph.project);const [,id,suffix]=url.match(/^\/projects\/([^/]+)(.*)$/)||[];const state=states[id];if(suffix==='/state')return state;if(suffix==='/identity')return {id,generation:state.graph.project.generation};if(suffix==='/inputs')return [];if(suffix?.startsWith('/rounds'))return {items:[]};if(suffix?.startsWith('/state/events'))return [];throw new Error(url);},async executionChanges(){return {items:[],deleted:[],through:0}}};}
test('workspace refresh updates the material badge when the file list changes',async()=>{
 const project=snapshotView(snapshot()),h=workspaceHarness(project);
 assert.equal(h.badge.textContent,'0');
 h.service.refresh=async()=>({projects:[{...project,files:[{id:'new',name:'new.txt'}]}]});
 await h.app.refreshWorkspace();
 assert.equal(h.badge.textContent,'1');
});
test('workspace refresh keeps the selected archive material count and unknown manifests',async()=>{
 for(const filesKnown of [true,false]){
  const project=snapshotView(snapshot('a',1)),archived={...snapshotView(snapshot()),filesKnown};project.history=[archived];
  const h=workspaceHarness(project,'0');
  h.service.refresh=async()=>({projects:[{...project,files:[{id:'current-only'}]}]});
  await h.app.refreshWorkspace();assert.equal(h.badge.textContent,filesKnown?'0':'?');
 }
});
test('workspace refresh releases deleted project activity while preserving live round state',async()=>{
 const project=snapshotView(snapshot('a',1)),h=workspaceHarness(project),live=h.app.activityState();live.query='keep';
 h.app.activityStates.set('deleted:0',{projectId:'deleted',query:'old',revision:'old body'});
 h.app.activityStates.set('a:0',{projectId:'a',query:'archive'});
 await h.app.refreshWorkspace();
 assert.equal(h.app.activityStates.has('deleted:0'),false);assert.equal(h.app.activityState(),live);assert.equal(h.app.activityStates.has('a:0'),true);
 h.app.activityRecords.set('old',{body:'deleted project body'});h.service.refresh=async()=>({projects:[]});
 await h.app.refreshWorkspace();
 assert.equal(h.app.activityStates.size,0);assert.equal(h.app.activityRecords.size,0);
});
test('production shell is the demo document structure with real network access and no fixture routes',()=>{const html=fs.readFileSync(path.join(__dirname,'static/index.html'),'utf8'),app=fs.readFileSync(path.join(__dirname,'static/app.js'),'utf8');assert.match(html,/<main id="main"><\/main>/);assert.match(html,/data-create/);assert.match(html,/project-menu-popover/);assert.match(html,/connect-src 'self'/);assert.doesNotMatch(html,/simulate-failure|workbench\.css|models\.css|#project\/atlas/);assert.doesNotMatch(app,/atlas\.example|local-.*randomUUID|模拟本次操作|lifecycle\.apply/);assert.match(app,/PwnLLMDemo\.render/);assert.match(app,/class="workspace-actions"/);});
test('project adapter maps server lifecycle without fabricating findings or times',()=>{const state=snapshot(),p=snapshotView(state,[],[],[]);assert.equal(p.name,'a');assert.equal(p.target,'https://authorized.test');assert.equal(p.goal,'Find supported evidence');assert.equal(p.status,'running');assert.equal(p.endedAt,null);assert.deepEqual(p.state.findings,[]);for(const [status,expected] of [['stopped','paused'],['completed','done'],['terminated','terminated']])assert.equal(projectView({...state.graph.project,status}).status,expected);});

test('project and archived snapshot adapters preserve unspecified and explicit scenarios',()=>{
 for(const scenario of [undefined,'','ctf','audit','pentest']){
  const state=snapshot();if(scenario===undefined)delete state.graph.project.scenario;else state.graph.project.scenario=scenario;
  const raw=structuredClone(state.graph.project),previous={type:'pentest',archived:true};
  assert.equal(projectView(state.graph.project,previous).type,scenario||'');
  assert.equal(snapshotView(state,[],[],[],previous).type,scenario||'');
  assert.deepEqual(state.graph.project,raw,'display projection must not change the server scenario');
 }
});

test('refresh clears a cached pentest label for an unspecified API project and its round summaries',async()=>{
 const state=snapshot('a',1);delete state.graph.project.scenario;
 const api=fixture({a:state}),request=api.request.bind(api);
 api.request=(url,options)=>url.includes('/rounds?')?{items:[{generation:0,archived_at:stamp}]}:request(url,options);
 const service=new Service(api);service.projects=[{id:'a',type:'pentest',history:[]}];
 const {projects,selected}=await service.refresh('a');
 assert.equal(projects[0].type,'');assert.equal(selected.type,'');assert.equal(selected.history[0].type,'');
});
test('historical projects are read-only when the server omits or explicitly sends protocol zero',()=>{
 for(const version of [undefined,0])for(const status of ['active','stopped','completed','terminated']){
  const raw={...snapshot().graph.project,status};if(version===undefined)delete raw.orchestration_version;else raw.orchestration_version=version;
  const p=projectView(raw);assert.equal(p.readOnly,true);assert.deepEqual(lifecycle.actions(p),[]);assert.equal(lifecycle.canHint(p),false);
 }
 const current=projectView(snapshot().graph.project,{readOnly:true});assert.equal(current.readOnly,false);assert.deepEqual(lifecycle.actions(current),['pause','restart','terminate','delete']);assert.equal(lifecycle.canHint(current),true);
 assert.deepEqual(lifecycle.actions({...current,archived:true}),[]);assert.equal(lifecycle.canHint({...current,archived:true}),false);
});
test('refresh fetches real materials, round summaries and generation-fenced state',async()=>{const api=fixture(),service=new Service(api);const value=await service.refresh('a');assert.equal(value.selected.id,'a');assert.ok(api.calls.some(c=>c.url==='/projects/a/inputs'));assert.ok(api.calls.some(c=>c.url.startsWith('/projects/a/rounds?')));assert.ok(api.calls.some(c=>c.url==='/projects/a/identity'));});
test('late project responses cannot replace the latest selection even if transport ignores abort',async()=>{const api=fixture({a:snapshot(),b:snapshot('b')}),old=api.request.bind(api),gate=deferred();api.request=(url,opts)=>url==='/projects/a/state'?gate.promise:old(url,opts);const service=new Service(api),first=service.refresh('a');await new Promise(r=>setImmediate(r));await service.refresh('b');gate.resolve(snapshot());assert.equal(await first,null);assert.equal(service.projects.find(p=>p.id==='b').state.graph.project.id,'b');});
test('generation conflict refuses mixed current and previous round data',async()=>{const api=fixture(),old=api.request.bind(api);api.request=(url,opts)=>url.endsWith('/identity')?{id:'a',generation:1}:old(url,opts);const service=new Service(api);await assert.rejects(service.refresh('a'),error=>error.status===409);assert.deepEqual(service.projects,[]);});
test('refresh rejects restarts on either side of the parallel state read without replacing the last snapshot',async()=>{
 for(const stateGeneration of [0,1]){
  const api=fixture(),service=new Service(api);await service.refresh('a');const before=service.projects,original=api.request.bind(api);
  // The list and round summaries were read before restart; the state request
  // may finish on either side, and the final identity sees the new round.
  api.request=(url,options)=>url==='/projects/a/state'?snapshot('a',stateGeneration):url==='/projects/a/identity'?{id:'a',generation:1}:original(url,options);
  api.executionChanges=async()=>({items:[{id:'previous-round',generation:0}],deleted:[],through:1});
  await assert.rejects(service.refresh('a'),error=>error.status===409,'state generation '+stateGeneration);
  assert.equal(service.projects,before);assert.equal(service.projects[0].generation,0);assert.equal(service.events.has('a'),false);
 }
});
test('refresh accepts one stable round and treats missing legacy generations as round zero',async()=>{
 for(const generation of [undefined,0,4]){
  const state=snapshot('a',generation);if(generation===undefined)delete state.graph.project.generation;
  const api=fixture({a:state});api.executionChanges=async()=>({items:[{id:'current',generation:generation||0}],deleted:[],through:1});
  const result=await new Service(api).refresh('a');assert.equal(result.selected.generation,generation||0);assert.deepEqual(result.selected.runs.map(run=>run.id),['current']);
 }
});
test('failed refresh preserves the last accepted project state',async()=>{const api=fixture(),service=new Service(api);await service.refresh('a');const before=service.projects;api.request=()=>{throw new APIError('offline');};await assert.rejects(service.refresh('a'),/offline/);assert.equal(service.projects,before);});
test('unchanged completed projects reuse snapshots and histories but still observe late execution results',async()=>{
 const state=snapshot();state.graph.project.status='completed';const api=fixture({a:state}),changes=[];
 let finish=false;
 api.executionChanges=async(base,after)=>{changes.push(after);return after===0?{items:[{id:'planner',generation:0,status:'running'}],deleted:[],through:1}:finish?{items:[{id:'planner',generation:0,status:'succeeded',result:{text:'Final answer'}}],deleted:[],through:2}:{items:[],deleted:[],through:1};};
 const service=new Service(api);const first=(await service.refresh('a')).selected;api.calls.length=0;
 const second=(await service.refresh('a')).selected;
 assert.deepEqual(api.calls.map(call=>call.url),['/projects','/projects/a/identity']);
 assert.equal(second.state,first.state);assert.equal(second.logs,first.logs);assert.equal(second.files,first.files);
 finish=true;const third=(await service.refresh('a')).selected;
 assert.equal(third.runs.length,1);assert.equal(third.runs[0].status,'succeeded');assert.equal(third.runs[0].result.text,'Final answer');assert.deepEqual(changes,[0,1,1]);
});

test('late terminal execution changes refresh state and events once',async()=>{
 const states={a:snapshot('a',0,6)};states.a.graph.project.status='completed';
 const api=fixture(states),request=api.request.bind(api);let cancelled=false;
 api.executionChanges=async(base,after)=>({items:after===(cancelled?2:1)?[]:[{id:'auxiliary',generation:0,status:cancelled?'cancelled':'running'}],deleted:[],through:cancelled?2:1});
 api.request=(url,options)=>url.includes('/state/events?')?(cancelled?[{revision:7,op:'execution_failed',id:'auxiliary',payload:{reason:'Cancellation confirmed'}}]:[]):request(url,options);
 const service=new Service(api);await service.refresh('a');
 states.a={...states.a,revision:7};cancelled=true;api.calls.length=0;
 const updated=(await service.refresh('a')).selected;
 assert.equal(updated.state.revision,7);assert.equal(updated.events.at(-1).revision,7);assert.equal(updated.runs[0].status,'cancelled');
 assert.ok(api.calls.some(call=>call.url==='/projects/a/state'));
 api.calls.length=0;await service.refresh('a');assert.deepEqual(api.calls.map(call=>call.url),['/projects','/projects/a/identity']);
});

test('first terminal refresh observes execution changes committed after its parallel state read',async t=>{
 for(const status of ['completed','terminated'])for(const cachedActive of [false,true])await t.test(status+(cachedActive?' after active view':' on initial load'),async()=>{
  const states={a:snapshot('a',0,6)},api=fixture(states),request=api.request.bind(api),service=new Service(api);
  if(cachedActive)await service.refresh('a');
  states.a={...states.a,graph:{...states.a.graph,project:{...states.a.graph.project,status}}};
  let changed=false;
  api.request=(url,options)=>url.includes('/state/events?')?(changed?[{revision:7,op:'execution_failed',id:'auxiliary',payload:{reason:'Cancellation confirmed'}}]:[]):request(url,options);
  api.executionChanges=async()=>{
   if(changed)return {items:[],deleted:[],through:2};
   // The parallel state response already owns revision 6; the execution update
   // advances both its cursor and the graph before that refresh completes.
   changed=true;states.a={...states.a,revision:7};
   return {items:[{id:'auxiliary',generation:0,status:'cancelled'}],deleted:[],through:2};
  };
  const updated=(await service.refresh('a')).selected;
  assert.equal(updated.state.revision,7);assert.equal(updated.events.at(-1).revision,7);assert.equal(updated.runs[0].status,'cancelled');
  api.calls.length=0;const unchanged=(await service.refresh('a')).selected;
  assert.equal(unchanged.state.revision,7);assert.equal(unchanged.events.at(-1).revision,7);
  assert.deepEqual(api.calls.map(call=>call.url),['/projects','/projects/a/identity']);
 });
});

test('a terminal state reload still rejects a concurrent project restart',async()=>{
 const state=snapshot('a',0,6);state.graph.project.status='completed';
 const api=fixture({a:state}),request=api.request.bind(api);let restarted=false;
 api.executionChanges=async()=>({items:restarted?[{id:'late',generation:0,status:'cancelled'}]:[],deleted:[],through:restarted?2:1});
 api.request=(url,options)=>restarted&&url==='/projects/a/state'?snapshot('a',1,0):request(url,options);
 const service=new Service(api);await service.refresh('a');const accepted=service.projects;
 restarted=true;await assert.rejects(service.refresh('a'),error=>error.status===409);
 assert.equal(service.projects,accepted);assert.equal(service.views.has('a'),false);
});
test('active refresh merges changed existing executions and deletions without reloading round or input history',async()=>{
 const state=snapshot(),api=fixture({a:state}),changes=[];let turn=0;
 api.executionChanges=async(base,after)=>{changes.push(after);return ++turn===1?{items:[{id:'running',generation:0,status:'running'},{id:'old',generation:0,status:'failed'}],deleted:[],through:2}:{items:[{id:'running',generation:0,status:'succeeded'}],deleted:['old'],through:4};};
 const service=new Service(api);await service.refresh('a');api.calls.length=0;
 const next=(await service.refresh('a')).selected;
 assert.deepEqual(next.runs.map(run=>[run.id,run.status]),[['running','succeeded']]);assert.deepEqual(changes,[0,2]);
 assert.ok(api.calls.some(call=>call.url==='/projects/a/state'));
 assert.ok(!api.calls.some(call=>call.url.includes('/rounds?')||call.url.endsWith('/inputs')||call.url.includes('/state/events?')));
});
test('a restart resets execution and input caches while preserving the response generation fence',async()=>{
 const states={a:snapshot()},api=fixture(states),revisions=[];
 api.executionChanges=async(base,after,generation)=>{revisions.push(after);return {items:[{id:'run-'+generation,generation,status:'running'}],deleted:[],through:generation+1};};
 const service=new Service(api);await service.refresh('a');states.a=snapshot('a',1);api.calls.length=0;
 const next=(await service.refresh('a')).selected;
 assert.deepEqual(revisions,[0,0]);assert.deepEqual(next.runs.map(run=>run.id),['run-1']);
 assert.ok(api.calls.some(call=>call.url.includes('/rounds?')));assert.ok(api.calls.some(call=>call.url.endsWith('/inputs')));
});
test('a reset execution cursor invalidates the cache and recovers on the next refresh',async()=>{
 const api=fixture(),seen=[];let reset=false;
 api.executionChanges=async(base,after)=>{seen.push(after);if(reset&&after)throw new APIError('cursor reset',409);return {items:[],deleted:[],through:reset?1:7};};
 const service=new Service(api);await service.refresh('a');reset=true;
 await assert.rejects(service.refresh('a'),error=>error.status===409);
 await service.refresh('a');assert.deepEqual(seen,[0,7,0]);
});
test('refresh filters executions from old rounds and bounds event projection to state revision',async()=>{const state=snapshot('a',2,2),api=fixture({a:state}),old=api.request.bind(api);api.executionChanges=async()=>({items:[{id:'old',generation:1},{id:'current',generation:2}],deleted:[],through:2});api.request=(url,opts)=>url.includes('/state/events?')?[{revision:2,op:'event'},{revision:3,op:'event'}]:old(url,opts);const service=new Service(api),result=await service.refresh('a');assert.deepEqual(result.selected.runs.map(r=>r.id),['current']);assert.equal(result.selected.events.length,1);assert.equal(service.events.get('a').after,3);});

test('terminal refresh resumes its bounded event backlog before reusing the caught-up view',async()=>{
 const state=snapshot('a',0,5002);state.graph.project.status='completed';
 const api=fixture({a:state}),request=api.request.bind(api),cursors=[];
 const events=Array.from({length:5002},(_,index)=>({revision:index+1,op:'checkpoint'}));
 api.request=(url,options)=>{
  if(!url.includes('/state/events?'))return request(url,options);
  const after=Number(new URL(url,'http://local').searchParams.get('after'));cursors.push(after);
  return events.slice(after,after+1000);
 };
 const service=new Service(api),first=(await service.refresh('a')).selected,partial=service.events.get('a');
 assert.deepEqual(cursors,[0,1000,2000,3000,4000]);
 assert.equal(first.events.length,5000);assert.equal(partial.after,5000);assert.equal(partial.caughtUp,false);
 const second=(await service.refresh('a')).selected;
 assert.deepEqual(cursors,[0,1000,2000,3000,4000,5000]);
 assert.equal(second.events.length,5002);assert.equal(service.events.get('a').caughtUp,true);
 assert.equal(partial.items.length,5000,'catch-up must not mutate the previously accepted cache');
 assert.equal(first.events.length,5000,'the previous view must retain its event projection');
 const third=(await service.refresh('a')).selected;
 assert.equal(cursors.length,6);assert.equal(third.logs,second.logs);
});

test('failed event catch-up retries from the last accepted cursor without publishing partial pages',async()=>{
 const states={a:snapshot()},api=fixture(states),request=api.request.bind(api),cursors=[];
 const events=Array.from({length:1002},(_,index)=>({revision:index+1,op:'checkpoint'}));
 let fail=false;
 api.request=(url,options)=>{
  if(!url.includes('/state/events?'))return request(url,options);
  const after=Number(new URL(url,'http://local').searchParams.get('after'));cursors.push(after);
  assert.equal(options.signal instanceof AbortSignal,true);
  if(fail&&after===1001)throw new APIError('event read interrupted',503);
  return events.slice(after,Math.min(after+1000,states.a.revision));
 };
 const service=new Service(api);await service.refresh('a');
 const accepted=service.events.get('a'),projects=service.projects;
 states.a=snapshot('a',0,1002);fail=true;
 await assert.rejects(service.refresh('a'),/event read interrupted/);
 assert.equal(service.projects,projects);assert.equal(service.events.get('a'),accepted);
 assert.equal(accepted.after,1);assert.equal(accepted.items.length,1);
 fail=false;const result=await service.refresh('a');
 assert.deepEqual(cursors,[0,1,1001,1,1001]);
 assert.deepEqual(result.selected.events.map(event=>event.revision),events.map(event=>event.revision));
});

test('event caches restart from zero after a revision rollback or a new round',async t=>{
 for(const generation of [0,1])await t.test('generation '+generation,async()=>{
  const states={a:snapshot('a',0,4)},api=fixture(states),request=api.request.bind(api),cursors=[];
  api.request=(url,options)=>{
   if(!url.includes('/state/events?'))return request(url,options);
   cursors.push(Number(new URL(url,'http://local').searchParams.get('after')));
   return [{revision:states.a.revision,op:'checkpoint',id:'round-'+states.a.graph.project.generation}];
  };
  const service=new Service(api);await service.refresh('a');
  const previous=service.events.get('a'),revision=generation===0?2:5;states.a=snapshot('a',generation,revision);
  const result=await service.refresh('a');
  assert.deepEqual(cursors,[0,0]);assert.deepEqual(result.selected.events.map(event=>event.revision),[revision]);
  assert.equal(service.events.get('a').generation,generation);assert.equal(previous.items[0].revision,4);
 });
});

test('project creation retries only unfinished uploads on the same paused project',async()=>{const calls=[],files=[new File(['a'],'a.txt'),new File(['b'],'b.txt')];let fail=true;const service=new Service({async request(url,options){calls.push({url,options});return {project:{id:'created'}};},async uploadInput(base,file){calls.push({upload:file.name});if(file.name==='b.txt'&&fail){fail=false;throw new APIError('upload interrupted',503);}}});const values={name:'Test',type:'pentest',target:'authorized',goal:'evidence',files};await assert.rejects(service.create(values),error=>error.pendingCreate);assert.equal(calls[0].options.body.start_paused,true);assert.equal(await service.create(values),'created');assert.equal(calls.filter(c=>c.url==='/projects').length,1);assert.deepEqual(calls.filter(c=>c.upload).map(c=>c.upload),['a.txt','b.txt','b.txt']);assert.deepEqual(calls.at(-1).options.body,{status:'active',expected_generation:0});});
test('ambiguous creation is never blindly retried',async()=>{let creates=0;const service=new Service({async request(){creates++;throw new APIError('lost connection',0);}}),values={name:'Test',type:'pentest',target:'authorized',goal:'evidence',files:[]};await assert.rejects(service.create(values),error=>error.uncertainCreate);await assert.rejects(service.create(values),/尚未确认/);assert.equal(creates,1);});
test('lifecycle operations use real endpoints and generation fencing',async()=>{const calls=[],service=new Service({async request(url,options){calls.push({url,options});}});for(const action of ['pause','resume','restart','terminate','delete'])await service.change('a/b',action,7);assert.equal(calls[0].url,'/projects/a%2Fb/status');assert.deepEqual(calls[0].options.body,{status:'stopped',expected_generation:7});assert.deepEqual(calls[1].options.body,{status:'active',expected_generation:7});assert.deepEqual(calls[2].options.body,{expected_generation:7});assert.equal(calls.at(-1).options.method,'DELETE');});
test('supplement files and text use one atomic batch request',async()=>{const calls=[],file=new File(['bytes'],'request.http'),service=new Service({async uploadInputs(...args){calls.push(args);}});await service.hint('a',{text:'scope',files:[file]});assert.equal(calls.length,1);assert.equal(calls[0][1][0],file);assert.equal(calls[0][2],'scope');await service.hint('a',{text:'more scope',files:[]});assert.equal(calls[1][0],'/projects/a');assert.deepEqual(calls[1][1],[]);assert.equal(calls[1][2],'more scope');assert.ok(calls[1][3]);});
test('supplement retries reuse their key until the submitted project, text or file references change',async()=>{
 const calls=[],files=[new File(['apk'],'client.apk'),new File(['http'],'capture.http')];let fail=true;
 const service=new Service({async uploadInputs(...args){calls.push(args);if(fail)throw new APIError('response lost',0);return ['saved'];}});
 const submit=async(id,text,items)=>assert.rejects(service.hint(id,{text,files:items}),/response lost/);
 await submit('a','scope',files);await submit('a','scope',files.slice());
 assert.match(calls[0][3],/^[a-f0-9-]{32,36}$/);assert.equal(calls[1][3],calls[0][3]);
 assert.equal(service.isHintRetry('a',{text:'scope',files:files.slice()}),true);assert.equal(service.isHintRetry('a',{text:'changed scope',files}),false);assert.equal(service.isHintRetry('b',{text:'scope',files}),false);assert.equal(service.isHintRetry('a',{text:'scope',files:[new File(['apk'],'client.apk'),files[1]]}),false);
 await submit('a','changed scope',files);assert.notEqual(calls[2][3],calls[1][3]);
 await submit('a','changed scope',files.slice().reverse());assert.notEqual(calls[3][3],calls[2][3]);
 const replacement=[new File(['http'],'capture.http'),files[0]];
 await submit('a','changed scope',replacement);assert.notEqual(calls[4][3],calls[3][3]);
 await submit('b','changed scope',replacement);assert.notEqual(calls[5][3],calls[4][3]);
 fail=false;assert.deepEqual(await service.hint('b',{text:'changed scope',files:replacement}),['saved']);assert.equal(calls[6][3],calls[5][3]);
 assert.equal(service.isHintRetry('b',{text:'changed scope',files:replacement}),false);
 await service.hint('b',{text:'changed scope',files:replacement});assert.notEqual(calls[7][3],calls[6][3]);
 fail=true;await submit('a','scope',files);await submit('a','text only',[]);await submit('a','text only',[]);assert.equal(calls[10][3],calls[9][3]);assert.equal(service.isHintRetry('a',{text:'text only',files:[]}),true);await submit('a','scope',files);assert.notEqual(calls[11][3],calls[8][3]);
});
test('pending supplement retries survive writes to another project and clear only their own project',async()=>{
 const calls=[],values={text:'scope',files:[new File(['apk'],'client.apk')]},lost=new Set(['/projects/a','/projects/b']);
 const service=new Service({async uploadInputs(...args){calls.push(args);if(lost.delete(args[0]))throw new APIError('response lost',0);return ['saved'];},async request(){return null;}});
 await assert.rejects(service.hint('a',values),/response lost/);await assert.rejects(service.hint('b',values),/response lost/);
 assert.equal(service.isHintRetry('a',values),true);assert.equal(service.isHintRetry('b',values),true);
 await service.hint('b',{text:'text only',files:[]});assert.equal(service.isHintRetry('a',values),true);assert.equal(service.isHintRetry('b',values),false);
 await service.hint('b',values);assert.notEqual(calls[3][3],calls[1][3]);assert.equal(service.isHintRetry('a',values),true);
 await service.change('b','delete');assert.equal(service.isHintRetry('a',values),true);
 await service.hint('a',values);assert.equal(calls[4][3],calls[0][3]);assert.equal(service.isHintRetry('a',values),false);
 lost.add('/projects/a');await assert.rejects(service.hint('a',values),/response lost/);assert.notEqual(calls[5][3],calls[4][3]);
 await service.change('a','delete');assert.equal(service.isHintRetry('a',values),false);
});
test('form drafts retain actual File objects, isolate file arrays and validate total uploaded bytes',()=>{
 const file=new File(['raw bytes'],'request.http'),drafts=model.createFormDrafts();
 const createFiles=[file];drafts.setCreate({name:'Test',type:'pentest',target:'scope',goal:'evidence',files:createFiles});createFiles.length=0;
 const create=drafts.getCreate();assert.equal(create.files[0],file);create.files.length=0;assert.equal(drafts.getCreate().files[0],file);
 const hintFiles=[file];drafts.setHint('a',{text:'Additional scope',files:hintFiles});hintFiles.length=0;
 const hint=drafts.getHint('a');assert.equal(hint.files[0],file);hint.files.length=0;assert.equal(drafts.getHint('a').files[0],file);
 assert.match(model.validateFiles([{name:'large.zip',size:257*1048576}]),/256/);assert.match(model.validateFiles(Array.from({length:33},(_,i)=>({name:i+'.txt',size:1}))),/32/);
});
test('file validation rejects every server-disallowed control character before creating a project',()=>{
 for(const name of ['.','..',...Array.from({length:32},(_,i)=>'client'+String.fromCharCode(0x80+i)+'.apk')]) {
  const files=[new File(['apk'],name)], values={name:'Client test',type:'pentest',target:'authorized',goal:'evidence',files};
  assert.match(model.validateFiles(files),/文件名/,JSON.stringify(name));
  assert.match(model.validateProject(values),/文件名/);
  assert.match(model.validateHint({text:'scope',files},{status:'running',files:[]}),/文件名/);
 }
 assert.equal(model.validateFiles([new File(['apk'],'客户端 v1.0.apk')]),'');
});
test('real log projection preserves timestamps, evidence and visible phase labels',()=>{
 const activity=require('./static/activity-data.js'),p={logs:[
  {id:'x',title:'Real event',body:'evidence',time:stamp,node:{type:'fact',id:'proof'},artifacts:[{path:'file.txt',excerpt:'actual'}]},
  {id:'error',level:'error',phase:'Execute'},
  {id:'finding',node:{type:'finding',id:'result'},level:'error'},
  {id:'model',phase:'Model'}
 ]};
 const entries=activity.buildEntries(p),entry=entries.find(item=>item.id==='x');
 assert.equal(entry.timestamp,stamp);assert.equal(entry.key,'fact:proof');assert.match(entry.evidence,/actual/);
 assert.deepEqual(entries.map(item=>[item.id,item.phase]),[['model','Model'],['finding','FINDING'],['error','ERROR'],['x','WORKSPACE']]);
 assert.deepEqual(activity.buildEntries({}),[]);
});

test('archive hydration survives a concurrent live refresh without losing the selected round',async()=>{
 const oldState=snapshot('a',0),liveState=snapshot('a',1),api=fixture({a:liveState}),original=api.request.bind(api),gate=deferred();
 const bytes=Buffer.from(JSON.stringify(oldState));
 api.request=(url,options)=>{if(url==='/projects/a/rounds?limit=100&cursor=-1')return {items:[{generation:0,archived_at:'2026-10-10T03:00:00Z'}]};if(url==='/projects/a/rounds/0/entries?limit=100&cursor=0')return {items:[{cursor:1,kind:'state',id:'state',bytes:bytes.length}]};if(url.startsWith('/projects/a/rounds/0/entries/1?'))return gate.promise;return original(url,options);};
 const service=new Service(api);await service.refresh('a');const before=service.projects[0],loading=service.archive(before,0);await new Promise(resolve=>setImmediate(resolve));await service.refresh('a');
 gate.resolve({encoding:'base64',offset:0,total_bytes:bytes.length,data:bytes.toString('base64')});await loading;
 assert.equal(service.projects[0].history[0].state.graph.project.generation,0);assert.equal(service.projects[0].history[0].archived,true);assert.equal(service.projects[0].state.graph.project.generation,1);
});
test('archived UTF-8 state chunks join without corrupting a split multibyte character',async()=>{
 const state=snapshot('a',0);state.graph.project.title='中文项目';const bytes=Buffer.from(JSON.stringify(state)),split=bytes.indexOf(Buffer.from('中文'))+1;
 const summary={id:'a',generation:0,archivedAt:stamp,files:[]},project={id:'a',history:[summary]};
 const service=new Service({async request(url){if(url.endsWith('/entries?limit=100&cursor=0'))return {items:[{cursor:1,kind:'state',bytes:bytes.length}]};const offset=Number(new URL(url,'http://local').searchParams.get('offset'));return {encoding:'base64',offset,total_bytes:bytes.length,data:bytes.subarray(offset,offset?bytes.length:split).toString('base64'),...(offset?{}:{next_offset:split})};}});service.projects=[project];const archived=await service.archive(project,0);assert.equal(archived.name,'中文项目');
});

test('archive hydration reads all entry pages, skips unknown kinds and reuses the hydrated round',async()=>{
 const bytes=Buffer.from(JSON.stringify(snapshot())),calls=[];
 const summary={id:'a',generation:0,archivedAt:stamp,files:[]},project={id:'a',history:[summary]};
 const base='/projects/a/rounds/0/entries';
 const service=new Service({async request(url){
  calls.push(url);
  if(url===base+'?limit=100&cursor=0')return {items:[{cursor:1,kind:'future',bytes:64*1024*1024}],next_cursor:1};
  if(url===base+'?limit=100&cursor=1')return {items:[{cursor:2,kind:'state',bytes:bytes.length}]};
  assert.equal(url,base+'/2?offset=0&limit=32768','unknown entry kinds must not be downloaded');
  return {encoding:'base64',offset:0,total_bytes:bytes.length,data:bytes.toString('base64')};
 }});
 service.projects=[project];
 const archived=await service.archive(project,0);
 assert.equal(archived,summary);
 assert.equal(archived.state.graph.project.generation,0);
 assert.equal(archived.filesKnown,false);
 assert.equal(await service.archive(project,0),archived);
 assert.deepEqual(calls,[base+'?limit=100&cursor=0',base+'?limit=100&cursor=1',base+'/2?offset=0&limit=32768']);
});

test('archive hydration rejects oversized records before downloading their contents',async()=>{
 const summary={id:'a',generation:0,archivedAt:stamp,files:[]},project={id:'a',history:[summary]},calls=[];
 const service=new Service({async request(url){
  calls.push(url);
  return {items:[{cursor:1,kind:'state',bytes:32*1024*1024+1}]};
 }});
 await assert.rejects(service.archive(project,0),/32 MiB/);
 assert.deepEqual(calls,['/projects/a/rounds/0/entries?limit=100&cursor=0']);
 assert.equal(summary.state,undefined);
});

test('archive hydration rejects invalid chunk boundaries',async t=>{
 const bytes=Buffer.from(JSON.stringify(snapshot()));
 const cases=[
  {name:'oversized',body:Buffer.concat([bytes,Buffer.from('extra')])},
  {name:'overlapping',body:bytes.subarray(0,bytes.length-1),nextOffset:1},
  {name:'truncated',body:bytes.subarray(0,bytes.length-1)},
  {name:'empty continuation',body:Buffer.alloc(0),nextOffset:1},
  {name:'continuation at end',body:bytes,nextOffset:bytes.length},
 ];
 for(const {name,body,nextOffset} of cases)await t.test(name,async()=>{
  const summary={id:'a',generation:0,archivedAt:stamp,files:[]},project={id:'a',history:[summary]};
  const service=new Service({async request(url){
   if(url.endsWith('/entries?limit=100&cursor=0'))return {items:[{cursor:1,kind:'state',bytes:bytes.length}]};
   const offset=Number(new URL(url,'http://local').searchParams.get('offset'));
   return {encoding:'base64',offset,total_bytes:bytes.length,data:body.toString('base64'),...(nextOffset===undefined?{}:{next_offset:nextOffset})};
  }});
  await assert.rejects(service.archive(project,0),/历史记录分块边界无效/);
  assert.equal(summary.state,undefined,'rejected bytes must not populate the archive cache');
 });
});

test('archive materials come from the retained manifest and survive later refreshes',async()=>{
 const oldState=snapshot('a',0),api=fixture({a:snapshot('a',1)}),request=api.request.bind(api);
 const original={id:'before',name:'before.txt',created_at:stamp},later={id:'after',name:'after.txt',created_at:stamp};
 const records=[['state',oldState],['inputs',[original]]].map(([kind,value],index)=>({kind,cursor:index+1,bytes:Buffer.from(JSON.stringify(value))}));
 api.request=(url,options)=>{
  if(url==='/projects/a/inputs')return [original,later];
  if(url==='/projects/a/rounds?limit=100&cursor=-1')return {items:[{generation:0,archived_at:stamp}]};
  if(url==='/projects/a/rounds/0/entries?limit=100&cursor=0')return {items:records.map(record=>({kind:record.kind,cursor:record.cursor,bytes:record.bytes.length}))};
  const match=url.match(/\/rounds\/0\/entries\/(\d+)\?/);if(match){const bytes=records[Number(match[1])-1].bytes;return {encoding:'base64',offset:0,total_bytes:bytes.length,data:bytes.toString('base64')};}
  return request(url,options);
 };
 const service=new Service(api);await service.refresh('a');
 assert.deepEqual(service.projects[0].history[0].files,[],'unhydrated history must not guess file membership');
 const archived=await service.archive(service.projects[0],0);assert.deepEqual(archived.files,[original]);assert.equal(archived.filesKnown,true);
 await service.refresh('a');assert.deepEqual(service.projects[0].history[0].files,[original]);
});

test('legacy archives distinguish an unavailable manifest from an empty manifest',async()=>{
 for(const manifest of [undefined,[]]){
  const state=snapshot(),records=[['state',state],...(manifest===undefined?[]:[['inputs',manifest]])].map(([kind,value],index)=>({kind,cursor:index+1,bytes:Buffer.from(JSON.stringify(value))}));
  const project={id:'a',history:[{id:'a',generation:0,archivedAt:stamp,files:[{id:'unproven'}]}]};
  const service=new Service({async request(url){if(url.endsWith('/entries?limit=100&cursor=0'))return {items:records.map(record=>({kind:record.kind,cursor:record.cursor,bytes:record.bytes.length}))};const record=records[Number(url.match(/\/entries\/(\d+)\?/)[1])-1];return {encoding:'base64',offset:0,total_bytes:record.bytes.length,data:record.bytes.toString('base64')};}});
  service.projects=[project];const archived=await service.archive(project,0);assert.deepEqual(archived.files,[]);assert.equal(archived.filesKnown,manifest!==undefined);
 }
 const app=fs.readFileSync(path.join(__dirname,'static/app.js'),'utf8');assert.match(app,/filesKnown === false/);assert.match(app,/此历史轮次未保存材料清单/);
});

test('archived rounds retain their real completion or termination time across live refreshes',async()=>{
 const ended='2026-10-10T02:30:00Z',archivedAt='2026-10-11T03:00:00Z';
 for(const status of ['completed','terminated','active']){
  const oldState=snapshot('a',0),liveState=snapshot('a',1);oldState.graph.project.status=status;
  if(status==='terminated')oldState.graph.project.terminated_at=ended;
  if(status==='completed'){
   oldState.goals=[{id:'goal',status:'achieved'}];
   oldState.graph.intents=[{id:'finish',to:'goal',concluded_at:ended}];
  }
  const bytes=Buffer.from(JSON.stringify(oldState)),api=fixture({a:liveState}),original=api.request.bind(api);
  api.request=(url,options)=>{
   if(url==='/projects/a/rounds?limit=100&cursor=-1')return {items:[{generation:0,archived_at:archivedAt}]};
   if(url==='/projects/a/rounds/0/entries?limit=100&cursor=0')return {items:[{cursor:1,kind:'state',bytes:bytes.length}]};
   if(url.startsWith('/projects/a/rounds/0/entries/1?'))return {encoding:'base64',offset:0,total_bytes:bytes.length,data:bytes.toString('base64')};
   return original(url,options);
  };
  const service=new Service(api);await service.refresh('a');const round=await service.archive(service.projects[0],0);
  const expected=status==='active'?archivedAt:new Date(ended).toISOString();
  assert.equal(round.endedAt,expected,status+' hydration');assert.equal(round.archivedAt,archivedAt);
  await service.refresh('a');assert.equal(service.projects[0].history[0].endedAt,expected,status+' refresh');
 }
});
