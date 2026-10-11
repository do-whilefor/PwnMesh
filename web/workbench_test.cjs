'use strict';
const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const {Service,projectView,snapshotView}=require('./static/service.js');
const {APIError}=require('./static/api.js');
const model=require('./static/model.js');
const lifecycle=require('./static/lifecycle.js');
const stamp='2026-10-10T02:00:00Z';
const snapshot=(id='a',generation=0,revision=1)=>({graph:{project:{id,title:id,scenario:'pentest',status:'active',orchestration_version:1,generation,created_at:stamp},facts:[{id:'origin',description:'https://authorized.test',created_at:stamp},{id:'goal',description:'Find supported evidence',created_at:stamp}],intents:[],hints:[]},goals:[],steps:[],fact_records:[],findings:[],revision});
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
function fixture(states={a:snapshot()}){const calls=[];return {calls,async request(url,options={}){calls.push({url,options});if(url==='/projects')return Object.values(states).map(s=>s.graph.project);const [,id,suffix]=url.match(/^\/projects\/([^/]+)(.*)$/)||[];const state=states[id];if(suffix==='/state')return state;if(suffix==='/identity')return {id,generation:state.graph.project.generation};if(suffix==='/inputs')return [];if(suffix?.startsWith('/rounds'))return {items:[]};if(suffix?.startsWith('/state/events'))return [];throw new Error(url);},async projectExecutions(){return []}};}
test('production shell is the demo document structure with real network access and no fixture routes',()=>{const html=fs.readFileSync(path.join(__dirname,'static/index.html'),'utf8'),app=fs.readFileSync(path.join(__dirname,'static/app.js'),'utf8');assert.match(html,/<main id="main"><\/main>/);assert.match(html,/data-create/);assert.match(html,/project-menu-popover/);assert.match(html,/connect-src 'self'/);assert.doesNotMatch(html,/simulate-failure|workbench\.css|models\.css|#project\/atlas/);assert.doesNotMatch(app,/atlas\.example|local-.*randomUUID|模拟本次操作|lifecycle\.apply/);assert.match(app,/PwnLLMDemo\.render/);assert.match(app,/class="workspace-actions"/);});
test('project adapter maps server lifecycle without fabricating findings or times',()=>{const state=snapshot(),p=snapshotView(state,[],[],[]);assert.equal(p.name,'a');assert.equal(p.target,'https://authorized.test');assert.equal(p.goal,'Find supported evidence');assert.equal(p.status,'running');assert.equal(p.endedAt,null);assert.deepEqual(p.state.findings,[]);for(const [status,expected] of [['stopped','paused'],['completed','done'],['terminated','terminated']])assert.equal(projectView({...state.graph.project,status}).status,expected);});
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
  api.projectExecutions=async()=>[{id:'previous-round',generation:0}];
  await assert.rejects(service.refresh('a'),error=>error.status===409,'state generation '+stateGeneration);
  assert.equal(service.projects,before);assert.equal(service.projects[0].generation,0);assert.equal(service.events.has('a'),false);
 }
});
test('refresh accepts one stable round and treats missing legacy generations as round zero',async()=>{
 for(const generation of [undefined,0,4]){
  const state=snapshot('a',generation);if(generation===undefined)delete state.graph.project.generation;
  const api=fixture({a:state});api.projectExecutions=async()=>[{id:'current',generation:generation||0}];
  const result=await new Service(api).refresh('a');assert.equal(result.selected.generation,generation||0);assert.deepEqual(result.selected.runs.map(run=>run.id),['current']);
 }
});
test('failed refresh preserves the last accepted project state',async()=>{const api=fixture(),service=new Service(api);await service.refresh('a');const before=service.projects;api.request=()=>{throw new APIError('offline');};await assert.rejects(service.refresh('a'),/offline/);assert.equal(service.projects,before);});
test('refresh filters executions from old rounds and bounds event projection to state revision',async()=>{const state=snapshot('a',2,2),api=fixture({a:state}),old=api.request.bind(api);api.projectExecutions=async()=>[{id:'old',generation:1},{id:'current',generation:2}];api.request=(url,opts)=>url.includes('/state/events?')?[{revision:2,op:'event'},{revision:3,op:'event'}]:old(url,opts);const service=new Service(api),result=await service.refresh('a');assert.deepEqual(result.selected.runs.map(r=>r.id),['current']);assert.equal(result.selected.events.length,1);assert.equal(service.events.get('a').after,3);});
test('project creation retries only unfinished uploads on the same paused project',async()=>{const calls=[],files=[new File(['a'],'a.txt'),new File(['b'],'b.txt')];let fail=true;const service=new Service({async request(url,options){calls.push({url,options});return {project:{id:'created'}};},async uploadInput(base,file){calls.push({upload:file.name});if(file.name==='b.txt'&&fail){fail=false;throw new APIError('upload interrupted',503);}}});const values={name:'Test',type:'pentest',target:'authorized',goal:'evidence',files};await assert.rejects(service.create(values),error=>error.pendingCreate);assert.equal(calls[0].options.body.start_paused,true);assert.equal(await service.create(values),'created');assert.equal(calls.filter(c=>c.url==='/projects').length,1);assert.deepEqual(calls.filter(c=>c.upload).map(c=>c.upload),['a.txt','b.txt','b.txt']);assert.deepEqual(calls.at(-1).options.body,{status:'active'});});
test('ambiguous creation is never blindly retried',async()=>{let creates=0;const service=new Service({async request(){creates++;throw new APIError('lost connection',0);}}),values={name:'Test',type:'pentest',target:'authorized',goal:'evidence',files:[]};await assert.rejects(service.create(values),error=>error.uncertainCreate);await assert.rejects(service.create(values),/尚未确认/);assert.equal(creates,1);});
test('lifecycle operations use real endpoints and generation fencing',async()=>{const calls=[],service=new Service({async request(url,options){calls.push({url,options});}});for(const action of ['pause','resume','restart','terminate','delete'])await service.change('a/b',action,7);assert.equal(calls[0].url,'/projects/a%2Fb/status');assert.deepEqual(calls[0].options.body,{status:'stopped'});assert.deepEqual(calls[2].options.body,{expected_generation:7});assert.equal(calls.at(-1).options.method,'DELETE');});
test('supplement files and text use one atomic batch request',async()=>{const calls=[],file=new File(['bytes'],'request.http'),service=new Service({async uploadInputs(...args){calls.push(args);},async request(...args){calls.push(args);}});await service.hint('a',{text:'scope',files:[file]});assert.equal(calls.length,1);assert.equal(calls[0][1][0],file);assert.equal(calls[0][2],'scope');await service.hint('a',{text:'more scope',files:[]});assert.equal(calls[1][0],'/projects/a/hints');});
test('supplement retries reuse their key until the submitted project, text or file references change',async()=>{
 const calls=[],files=[new File(['apk'],'client.apk'),new File(['http'],'capture.http')];let fail=true;
 const service=new Service({async uploadInputs(...args){calls.push(args);if(fail)throw new APIError('response lost',0);return ['saved'];},async request(){return 'text saved';}});
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
 fail=true;await submit('a','scope',files);await service.hint('a',{text:'text only',files:[]});await submit('a','scope',files);assert.notEqual(calls[9][3],calls[8][3]);
});
test('pending supplement retries survive writes to another project and clear only their own project',async()=>{
 const calls=[],values={text:'scope',files:[new File(['apk'],'client.apk')]},lost=new Set(['/projects/a','/projects/b']);
 const service=new Service({async uploadInputs(...args){calls.push(args);if(lost.delete(args[0]))throw new APIError('response lost',0);return ['saved'];},async request(){return null;}});
 await assert.rejects(service.hint('a',values),/response lost/);await assert.rejects(service.hint('b',values),/response lost/);
 assert.equal(service.isHintRetry('a',values),true);assert.equal(service.isHintRetry('b',values),true);
 await service.hint('b',{text:'text only',files:[]});assert.equal(service.isHintRetry('a',values),true);assert.equal(service.isHintRetry('b',values),false);
 await service.hint('b',values);assert.notEqual(calls[2][3],calls[1][3]);assert.equal(service.isHintRetry('a',values),true);
 await service.change('b','delete');assert.equal(service.isHintRetry('a',values),true);
 await service.hint('a',values);assert.equal(calls[3][3],calls[0][3]);assert.equal(service.isHintRetry('a',values),false);
 lost.add('/projects/a');await assert.rejects(service.hint('a',values),/response lost/);assert.notEqual(calls[4][3],calls[3][3]);
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
