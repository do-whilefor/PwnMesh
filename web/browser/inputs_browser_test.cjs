'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const {staticServer,launch,fillCreate,stateFor,projectPath,interceptProject}=require('./visual_fixture.cjs');
test('production forms render accessible dialogs, retry uploads, and atomically supplement materials',{
 timeout:60000,skip:process.env.PLAYWRIGHT_MODULE?false:'Set PLAYWRIGHT_MODULE for isolated upload regression',
},async()=>{
 const server=await staticServer(),browser=await launch();
 try{
  const page=await browser.newPage({viewport:{width:1440,height:960}});page.setDefaultTimeout(12000);const errors=[],uploaded=[],inputs=[],writes=[],batchKeys=[];let project=null,creates=0,fail=true;
  await page.addInitScript(()=>Object.defineProperty(crypto,'randomUUID',{value:undefined}));
  page.on('pageerror',error=>errors.push(error.message));
  await page.route('**/projects**',async route=>{const req=route.request(),url=new URL(req.url()),pathname=url.pathname,method=req.method(),reply=(body,status=200)=>route.fulfill({status,contentType:'application/json',body:JSON.stringify(body)});
   if(method!=='GET')writes.push(method+' '+pathname);
   if(pathname==='/projects'&&method==='POST'){creates++;const data=req.postDataJSON();assert.equal(data.start_paused,true);project={id:'uploaded',title:data.title,status:'stopped',scenario:data.scenario,orchestration_version:1,generation:0,created_at:'2026-10-10T02:00:00Z'};return reply({project},201);}
   if(pathname==='/projects')return reply(project?[project]:[]);
   if(pathname.endsWith('/rounds'))return reply({items:[]});if(pathname.endsWith('/state/events'))return reply([]);if(pathname.endsWith('/state'))return reply(stateFor(project));if(pathname.endsWith('/identity'))return reply({id:project.id,generation:0});if(pathname.endsWith('/executions'))return reply({items:[],through:0});
   if(pathname.endsWith('/inputs')&&method==='GET')return reply(inputs);
   if(pathname.endsWith('/inputs')&&method==='POST'){
    const form=await new Response(req.postDataBuffer(),{headers:{'Content-Type':req.headers()['content-type']}}).formData(),file=form.get('file');uploaded.push(file.name);assert.deepEqual(Buffer.from(await file.arrayBuffer()),Buffer.from(file.name==='one.http'?'GET /one':[0,255,13,10]));
    if(file.name==='two.apk'&&fail){fail=false;return reply({detail:'Controlled failed upload'},503);}const input={id:String(inputs.length+1),name:file.name,size:file.size,path:'/workspace/inputs/'+file.name,sha256:'a'.repeat(64),created_at:project.created_at};inputs.push(input);return reply(input,201);
   }
   if(pathname.endsWith('/inputs/batch')){
    const form=await new Response(req.postDataBuffer(),{headers:{'Content-Type':req.headers()['content-type']}}).formData(),key=form.get('idempotency_key');
    assert.match(key,/^[a-f0-9]{32}$/);batchKeys.push(key);
    assert.equal(form.get('content'),batchKeys.length===3?'补充新的测试范围':'只使用授权测试环境');assert.equal(form.getAll('file').length,1);const file=form.get('file');assert.equal(await file.text(),'supplement bytes');
    if(batchKeys.length===1){inputs.push({id:'3',name:file.name,size:file.size,path:'/workspace/inputs/'+file.name,created_at:project.created_at,sha256:'b'.repeat(64)});return route.abort('failed');}
    if(batchKeys.length===2)assert.equal(key,batchKeys[0]);else assert.notEqual(key,batchKeys[0]);
    return reply([inputs.at(-1)],201);
   }
   if(pathname.endsWith('/status')){project.status=req.postDataJSON().status;return reply(project);}
   errors.push('Unexpected '+method+' '+pathname);return reply({},404);
  });
  await page.goto(server.url,{waitUntil:'networkidle'});
  assert.deepEqual(await page.locator('.view-tabs [role="tab"]').evaluateAll(tabs=>tabs.map(tab=>({disabled:tab.disabled,selected:tab.getAttribute('aria-selected')}))),[{disabled:true,selected:'true'},{disabled:true,selected:'false'},{disabled:true,selected:'false'}]);
  assert.deepEqual(await page.locator('.zoom-controls button').evaluateAll(buttons=>buttons.map(button=>({label:button.getAttribute('aria-label'),disabled:button.disabled}))),[
   {label:'缩小画布',disabled:true},{label:'放大画布',disabled:true},{label:'适应画布',disabled:true},{label:'按流程整理',disabled:true}
  ]);
  assert.equal(await page.locator('.legend button:disabled').count(),6);
  assert.deepEqual(await page.locator('.graph-trace-controls button').evaluateAll(buttons=>buttons.map(button=>({direction:button.dataset.trace,disabled:button.disabled,pressed:button.getAttribute('aria-pressed')}))),[
   {direction:'upstream',disabled:true,pressed:'true'},{direction:'downstream',disabled:true,pressed:'false'}
  ]);
  assert.match(await page.locator('.task-progress').textContent(),/0 \/ 0 个任务/);
  await fillCreate(page,'真实材料上传');
  assert.ok(await page.getByRole('dialog',{name:'新建项目',exact:true}).isVisible());
  assert.equal(await page.getByLabel('项目名称',{exact:true}).inputValue(),'真实材料上传');
  await page.locator('#project-files').setInputFiles({name:'client\u0085.apk',mimeType:'application/octet-stream',buffer:Buffer.from('apk')});
  assert.match(await page.locator('#create-error').textContent(),/文件名/);assert.equal(await page.locator('#selected-files li').count(),0);assert.equal(creates,0);
  await page.locator('#project-files').setInputFiles([{name:'one.http',mimeType:'text/plain',buffer:Buffer.from('GET /one')},{name:'two.apk',mimeType:'application/octet-stream',buffer:Buffer.from([0,255,13,10])}]);await page.locator('#create-form [type="submit"]').click();await page.waitForFunction(()=>document.getElementById('create-error').textContent.includes('Controlled failed upload'));assert.equal(creates,1);assert.equal(project.status,'stopped');assert.equal(await page.locator('#project-name').isDisabled(),true);assert.equal(await page.locator('#selected-files [data-remove-file]').first().isDisabled(),true);assert.equal(await page.locator('#selected-files li').count(),2);
  await page.locator('#create-form [type="submit"]').click();await page.locator('#create-dialog').waitFor({state:'hidden'});assert.equal(creates,1);assert.deepEqual(uploaded,['one.http','two.apk','two.apk']);assert.equal(project.status,'active');
  await page.locator('#add-hint').click();
  assert.ok(await page.getByRole('dialog',{name:'补充信息',exact:true}).isVisible());
  await page.getByLabel('补充内容',{exact:true}).fill('只使用授权测试环境');await page.locator('#hint-files').setInputFiles({name:'notes.txt',mimeType:'text/plain',buffer:Buffer.from('supplement bytes')});const before=writes.length;await page.locator('#hint-form [type="submit"]').click();
  await page.waitForFunction(()=>document.getElementById('hint-error').textContent.includes('无法连接'));assert.equal(await page.locator('#hint-selected-files li').count(),1);
  await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});assert.deepEqual(writes.slice(before),Array(2).fill('POST /projects/uploaded/inputs/batch'));
  await page.locator('#add-hint').click();await page.getByLabel('补充内容',{exact:true}).fill('补充新的测试范围');await page.locator('#hint-files').setInputFiles({name:'notes.txt',mimeType:'text/plain',buffer:Buffer.from('supplement bytes')});await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});assert.equal(batchKeys.length,3);
  await page.locator('[data-view="materials"]').click();await page.waitForFunction(()=>document.querySelectorAll('.material-row').length===3);assert.match(await page.locator('#project-view').textContent(),/notes\.txt/);assert.deepEqual(errors,[]);
 }finally{await browser.close();await server.close();}
});
test('a lost supplement response remains retryable after the last project file slot is filled',{
 timeout:45000,skip:process.env.PLAYWRIGHT_MODULE?false:'Set PLAYWRIGHT_MODULE for upload quota regression',
},async()=>{
 const server=await staticServer(),browser=await launch();
 try{
  const page=await browser.newPage({viewport:{width:1440,height:960}});page.setDefaultTimeout(10000);
  const project={id:'quota',title:'最后一个文件重试',scenario:'pentest',status:'active',orchestration_version:1,generation:0,created_at:'2026-10-10T02:00:00Z'},keys=[];
  const inputs=Array.from({length:31},(_,i)=>({id:String(i),name:'existing-'+i+'.txt',size:1,path:'/inputs/'+i,created_at:project.created_at,sha256:'a'.repeat(64)}));
  await interceptProject(page,{state:stateFor(project),inputs});
  await page.route('**/inputs/batch',async route=>{
   const req=route.request(),form=await new Response(req.postDataBuffer(),{headers:{'Content-Type':req.headers()['content-type']}}).formData();keys.push(form.get('idempotency_key'));
   if(keys.length===1){inputs.push({id:'last',name:'last.apk',size:4,path:'/inputs/last',created_at:project.created_at,sha256:'b'.repeat(64)});return route.abort('failed');}
   assert.equal(keys[1],keys[0]);return route.fulfill({status:201,contentType:'application/json',body:JSON.stringify([inputs.at(-1)])});
  });
  await page.goto(server.url,{waitUntil:'networkidle'});await page.locator('[data-view="materials"]').click();await page.locator('#add-hint').click();
  await page.locator('#hint-text').fill('测试最后一个文件');await page.locator('#hint-files').setInputFiles({name:'last.apk',mimeType:'application/octet-stream',buffer:Buffer.from('last')});await page.locator('#hint-form [type="submit"]').click();
  await page.waitForFunction(()=>document.getElementById('hint-error').textContent.includes('无法连接'));await page.waitForFunction(()=>document.querySelectorAll('.material-row').length===32);
  await page.locator('#hint-text').fill('这是新的补充');await page.locator('#hint-form [type="submit"]').click();await page.waitForFunction(()=>document.getElementById('hint-error').textContent.includes('32'));assert.equal(keys.length,1);await page.locator('#hint-text').fill('测试最后一个文件');
  await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});assert.equal(keys.length,2);assert.equal(inputs.length,32);
 }finally{await browser.close();await server.close();}
});
test('demo upload form preserves bytes and retries a lost supplement response through the real HTTP service',{
 timeout:90000,skip:process.env.PWNMESH_WEB_URL?false:'Set PWNMESH_WEB_URL to a dedicated empty service',
},async()=>{
 const base=new URL(process.env.PWNMESH_WEB_URL),request=async(path,options={})=>{const r=await fetch(new URL(path,base),options);assert.ok(r.ok,path+': '+r.status);return r;};assert.deepEqual(await(await request('/projects')).json(),[]);const browser=await launch();let id;
 try{
  const page=await browser.newPage({viewport:{width:1440,height:960}});await page.goto(base.href,{waitUntil:'networkidle'});await fillCreate(page,'真实上传字节验证');const bytes=Buffer.from([0,255,13,10,80,75]);
  await page.locator('#project-files').setInputFiles({name:'fixture.bin',mimeType:'application/octet-stream',buffer:bytes});await page.locator('#create-form [type="submit"]').click();await page.locator('#create-dialog').waitFor({state:'hidden'});id=await page.evaluate(()=>location.hash.slice(9));
  const inputs=await(await request(projectPath(id)+'/inputs')).json();assert.equal(inputs.length,1);const download=await request(projectPath(id)+'/inputs/'+inputs[0].id);assert.deepEqual(Buffer.from(await download.arrayBuffer()),bytes);
  await page.locator('[data-view="materials"]').click();await page.locator('[data-material="0"]').click();await page.locator('.inspector-reader[open]').waitFor();assert.equal(await page.locator('.inspector-reader[open] a[download]').count(),1);await page.locator('.inspector-reader[open] [data-inspector-action="close"]').click();
  const keys=[];await page.route('**/inputs/batch',async route=>{
   const req=route.request(),form=await new Response(req.postDataBuffer(),{headers:{'Content-Type':req.headers()['content-type']}}).formData();keys.push(form.get('idempotency_key'));
   const response=await route.fetch();assert.equal(response.status(),201);
   if(keys.length===1)return route.abort('failed');return route.fulfill({response});
  });
  await page.locator('#add-hint').click();await page.locator('#hint-text').fill('失去响应后仍只补充一次');await page.locator('#hint-files').setInputFiles({name:'client.apk',mimeType:'application/octet-stream',buffer:bytes});await page.locator('#hint-form [type="submit"]').click();
  await page.waitForFunction(()=>document.getElementById('hint-error').textContent.includes('无法连接'));const before=await(await request(projectPath(id)+'/state')).json();
  assert.equal(before.graph.hints.filter(hint=>hint.content==='失去响应后仍只补充一次').length,1);
  await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});const after=await(await request(projectPath(id)+'/state')).json();
  assert.equal(keys.length,2);assert.ok(keys[0]);assert.equal(keys[1],keys[0]);assert.equal(after.revision,before.revision);assert.equal(after.graph.hints.length,before.graph.hints.length);
  const saved=await(await request(projectPath(id)+'/inputs')).json();assert.equal(saved.length,2);const apk=saved.find(file=>file.name==='client.apk');assert.deepEqual(Buffer.from(await(await request(projectPath(id)+'/inputs/'+apk.id)).arrayBuffer()),bytes);
  await page.locator('#add-hint').click();await page.locator('#hint-text').fill('失去响应后仍只补充一次');await page.locator('#hint-files').setInputFiles({name:'client.apk',mimeType:'application/octet-stream',buffer:bytes});await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});
  const repeated=await(await request(projectPath(id)+'/state')).json();assert.equal(keys.length,3);assert.notEqual(keys[2],keys[0]);assert.equal(repeated.graph.hints.length,after.graph.hints.length+1);assert.equal(repeated.graph.hints.filter(hint=>hint.content==='失去响应后仍只补充一次').length,2);assert.ok(repeated.revision>after.revision);assert.equal((await(await request(projectPath(id)+'/inputs')).json()).length,2);
  await page.unroute('**/inputs/batch');let textRequests=0;
  await page.route(/\/(?:hints|inputs\/batch)$/,async route=>{const response=await route.fetch();assert.equal(response.status(),201);if(++textRequests===1)return route.abort('failed');return route.fulfill({response});});
  await page.locator('#add-hint').click();await page.locator('#hint-text').fill('纯文本补充丢失响应后也只提交一次');await page.locator('#hint-form [type="submit"]').click();
  await page.waitForFunction(()=>document.getElementById('hint-error').textContent.includes('无法连接'));const textBefore=await(await request(projectPath(id)+'/state')).json();assert.equal(textBefore.graph.hints.filter(hint=>hint.content==='纯文本补充丢失响应后也只提交一次').length,1);
  await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});const textAfter=await(await request(projectPath(id)+'/state')).json();
  assert.equal(textRequests,2);assert.equal(textAfter.revision,textBefore.revision);assert.equal(textAfter.graph.hints.length,textBefore.graph.hints.length);
 }finally{await browser.close();if(id)await request(projectPath(id),{method:'DELETE'});}
});

test('material readers ignore older responses and navigation to the model page',{
 timeout:45000,skip:process.env.PLAYWRIGHT_MODULE?false:'Set PLAYWRIGHT_MODULE for material reader race regression',
},async()=>{
 const server=await staticServer(),browser=await launch();
 try{
  const page=await browser.newPage({viewport:{width:1440,height:960}}),project={id:'reader',title:'材料读取隔离',scenario:'pentest',status:'active',orchestration_version:1,generation:0,created_at:'2026-10-10T02:00:00Z'};let slowRoute;
  const files=['slow','fast'].map(id=>({id,name:id+'.txt',size:12,path:'/inputs/'+id+'.txt',created_at:project.created_at,sha256:'a'.repeat(64)}));
  const requests=await interceptProject(page,{state:stateFor(project),inputs:files,contents:{fast:'fast actual'}});
  await page.route('**/inputs/slow',route=>{slowRoute=route;});
  await page.goto(server.url,{waitUntil:'networkidle'});await page.locator('[data-view="materials"]').click();
  const first=page.waitForRequest(req=>req.url().endsWith('/inputs/slow'));await page.locator('[data-material="0"]').click();await first;await page.locator('[data-material="1"]').click();await page.waitForFunction(()=>document.querySelector('.inspector-reader[open]')?.textContent.includes('fast actual'));
  const firstResponse=page.waitForResponse(res=>res.url().endsWith('/inputs/slow'));await slowRoute.fulfill({body:'slow actual',contentType:'application/octet-stream'});await firstResponse;await page.evaluate(()=>new Promise(requestAnimationFrame));assert.match(await page.locator('.inspector-reader[open]').textContent(),/fast actual/);
  await page.locator('.inspector-reader[open] [data-inspector-action="close"]').click();await page.locator('.inspector-reader[open]').waitFor({state:'hidden'});
  const second=page.waitForRequest(req=>req.url().endsWith('/inputs/slow'));await page.locator('[data-material="0"]').click();await second;await page.locator('#model-entry').click();await page.locator('.llm-page').waitFor();const secondResponse=page.waitForResponse(res=>res.url().endsWith('/inputs/slow'));await slowRoute.fulfill({body:'stale after navigation',contentType:'application/octet-stream'});await secondResponse;await page.evaluate(()=>new Promise(requestAnimationFrame));assert.equal(await page.locator('.inspector-reader[open]').count(),0);
  assert.deepEqual(requests.writes,[]);assert.deepEqual(requests.unexpected,[]);
 }finally{await browser.close();await server.close();}
});
