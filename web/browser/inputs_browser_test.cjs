'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const {assetServer,launch,fillCreate,stateFor,projectPath}=require('./demo_harness.cjs');
test('demo forms upload raw files, retry a paused project, and atomically supplement materials',{
 timeout:60000,skip:process.env.PLAYWRIGHT_MODULE?false:'Set PLAYWRIGHT_MODULE for isolated upload regression',
},async()=>{
 const server=await assetServer(),browser=await launch();
 try{
  const page=await browser.newPage({viewport:{width:1440,height:960}});page.setDefaultTimeout(12000);const errors=[],uploaded=[],inputs=[],writes=[];let project=null,creates=0,fail=true;
  page.on('pageerror',error=>errors.push(error.message));
  await page.route('**/projects**',async route=>{const req=route.request(),url=new URL(req.url()),pathname=url.pathname,method=req.method(),reply=(body,status=200)=>route.fulfill({status,contentType:'application/json',body:JSON.stringify(body)});
   if(method!=='GET')writes.push(method+' '+pathname);
   if(pathname==='/projects'&&method==='POST'){creates++;const data=req.postDataJSON();assert.equal(data.start_paused,true);project={id:'uploaded',title:data.title,status:'stopped',scenario:data.scenario,generation:0,created_at:'2026-10-10T02:00:00Z'};return reply({project},201);}
   if(pathname==='/projects')return reply(project?[project]:[]);
   if(pathname.endsWith('/rounds'))return reply({items:[]});if(pathname.endsWith('/state/events'))return reply([]);if(pathname.endsWith('/state'))return reply(stateFor(project));if(pathname.endsWith('/identity'))return reply({id:project.id,generation:0});if(pathname.endsWith('/executions'))return reply({items:[],through:0});
   if(pathname.endsWith('/inputs')&&method==='GET')return reply(inputs);
   if(pathname.endsWith('/inputs')&&method==='POST'){
    const form=await new Response(req.postDataBuffer(),{headers:{'Content-Type':req.headers()['content-type']}}).formData(),file=form.get('file');uploaded.push(file.name);assert.deepEqual(Buffer.from(await file.arrayBuffer()),Buffer.from(file.name==='one.http'?'GET /one':[0,255,13,10]));
    if(file.name==='two.apk'&&fail){fail=false;return reply({detail:'Controlled failed upload'},503);}const input={id:String(inputs.length+1),name:file.name,size:file.size,path:'/workspace/inputs/'+file.name,sha256:'a'.repeat(64),created_at:project.created_at};inputs.push(input);return reply(input,201);
   }
   if(pathname.endsWith('/inputs/batch')){const form=await new Response(req.postDataBuffer(),{headers:{'Content-Type':req.headers()['content-type']}}).formData();assert.equal(form.get('content'),'只使用授权测试环境');assert.equal(form.getAll('file').length,1);const file=form.get('file');assert.equal(await file.text(),'supplement bytes');inputs.push({id:'3',name:file.name,size:file.size,path:'/workspace/inputs/'+file.name,created_at:project.created_at,sha256:'b'.repeat(64)});return reply([inputs.at(-1)],201);}
   if(pathname.endsWith('/status')){project.status=req.postDataJSON().status;return reply(project);}
   errors.push('Unexpected '+method+' '+pathname);return reply({},404);
  });
  await page.goto(server.url,{waitUntil:'networkidle'});await fillCreate(page,'真实材料上传');await page.locator('#project-files').setInputFiles([{name:'one.http',mimeType:'text/plain',buffer:Buffer.from('GET /one')},{name:'two.apk',mimeType:'application/octet-stream',buffer:Buffer.from([0,255,13,10])}]);await page.locator('#create-form [type="submit"]').click();await page.waitForFunction(()=>document.getElementById('create-error').textContent.includes('Controlled failed upload'));assert.equal(creates,1);assert.equal(project.status,'stopped');assert.equal(await page.locator('#project-name').isDisabled(),true);assert.equal(await page.locator('#selected-files [data-remove-file]').first().isDisabled(),true);assert.equal(await page.locator('#selected-files li').count(),2);
  await page.locator('#create-form [type="submit"]').click();await page.locator('#create-dialog').waitFor({state:'hidden'});assert.equal(creates,1);assert.deepEqual(uploaded,['one.http','two.apk','two.apk']);assert.equal(project.status,'active');
  await page.locator('#add-hint').click();await page.locator('#hint-text').fill('只使用授权测试环境');await page.locator('#hint-files').setInputFiles({name:'notes.txt',mimeType:'text/plain',buffer:Buffer.from('supplement bytes')});const before=writes.length;await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});assert.deepEqual(writes.slice(before),['POST /projects/uploaded/inputs/batch']);
  await page.locator('[data-view="materials"]').click();await page.waitForFunction(()=>document.querySelectorAll('.material-row').length===3);assert.match(await page.locator('#project-view').textContent(),/notes\.txt/);assert.deepEqual(errors,[]);
 }finally{await browser.close();await server.close();}
});
test('demo upload form preserves bytes through the real HTTP service',{
 timeout:90000,skip:process.env.PWNMESH_WEB_URL?false:'Set PWNMESH_WEB_URL to a dedicated empty service',
},async()=>{
 const base=new URL(process.env.PWNMESH_WEB_URL),request=async(path,options={})=>{const r=await fetch(new URL(path,base),options);assert.ok(r.ok,path+': '+r.status);return r;};assert.deepEqual(await(await request('/projects')).json(),[]);const browser=await launch();let id;
 try{const page=await browser.newPage({viewport:{width:1440,height:960}});await page.goto(base.href,{waitUntil:'networkidle'});await fillCreate(page,'真实上传字节验证');const bytes=Buffer.from([0,255,13,10,80,75]);await page.locator('#project-files').setInputFiles({name:'fixture.bin',mimeType:'application/octet-stream',buffer:bytes});await page.locator('#create-form [type="submit"]').click();await page.locator('#create-dialog').waitFor({state:'hidden'});id=await page.evaluate(()=>location.hash.slice(9));const inputs=await(await request(projectPath(id)+'/inputs')).json();assert.equal(inputs.length,1);const download=await request(projectPath(id)+'/inputs/'+inputs[0].id);assert.deepEqual(Buffer.from(await download.arrayBuffer()),bytes);await page.locator('[data-view="materials"]').click();await page.locator('[data-material="0"]').click();await page.locator('.inspector-reader[open]').waitFor();assert.equal(await page.locator('.inspector-reader[open] a[download]').count(),1);}finally{await browser.close();if(id)await request(projectPath(id),{method:'DELETE'});}
});

test('material readers ignore older responses and navigation to the model page',{
 timeout:45000,skip:process.env.PLAYWRIGHT_MODULE?false:'Set PLAYWRIGHT_MODULE for material reader race regression',
},async()=>{
 const server=await assetServer(),browser=await launch();
 try{
  const page=await browser.newPage({viewport:{width:1440,height:960}}),project={id:'reader',title:'材料读取隔离',scenario:'pentest',status:'active',generation:0,created_at:'2026-10-10T02:00:00Z'};let slowRoute;
  const files=['slow','fast'].map(id=>({id,name:id+'.txt',size:12,path:'/inputs/'+id+'.txt',created_at:project.created_at,sha256:'a'.repeat(64)}));
  await page.route('**/model-settings',route=>route.fulfill({contentType:'application/json',body:JSON.stringify({base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',has_token:false,connection_mode:'direct',reasoning_effort:'high'})}));
  await page.route('**/projects**',route=>{const pathname=new URL(route.request().url()).pathname,reply=body=>route.fulfill({contentType:'application/json',body:JSON.stringify(body)});if(pathname==='/projects')return reply([project]);if(pathname.endsWith('/inputs/slow')){slowRoute=route;return;}if(pathname.endsWith('/inputs/fast'))return route.fulfill({body:'fast actual',contentType:'application/octet-stream'});if(pathname.endsWith('/inputs'))return reply(files);if(pathname.endsWith('/state/events'))return reply([]);if(pathname.endsWith('/state'))return reply(stateFor(project));if(pathname.endsWith('/identity'))return reply({id:project.id,generation:0});if(pathname.endsWith('/executions'))return reply({items:[],through:0});if(pathname.endsWith('/rounds'))return reply({items:[]});return reply({});});
  await page.goto(server.url,{waitUntil:'networkidle'});await page.locator('[data-view="materials"]').click();
  const first=page.waitForRequest(req=>req.url().endsWith('/inputs/slow'));await page.locator('[data-material="0"]').click();await first;await page.locator('[data-material="1"]').click();await page.waitForFunction(()=>document.querySelector('.inspector-reader[open]')?.textContent.includes('fast actual'));
  const firstResponse=page.waitForResponse(res=>res.url().endsWith('/inputs/slow'));await slowRoute.fulfill({body:'slow actual',contentType:'application/octet-stream'});await firstResponse;await page.evaluate(()=>new Promise(requestAnimationFrame));assert.match(await page.locator('.inspector-reader[open]').textContent(),/fast actual/);
  await page.locator('.inspector-reader[open] [data-inspector-action="close"]').click();await page.locator('.inspector-reader[open]').waitFor({state:'hidden'});
  const second=page.waitForRequest(req=>req.url().endsWith('/inputs/slow'));await page.locator('[data-material="0"]').click();await second;await page.locator('#model-entry').click();await page.locator('.llm-page').waitFor();const secondResponse=page.waitForResponse(res=>res.url().endsWith('/inputs/slow'));await slowRoute.fulfill({body:'stale after navigation',contentType:'application/octet-stream'});await secondResponse;await page.evaluate(()=>new Promise(requestAnimationFrame));assert.equal(await page.locator('.inspector-reader[open]').count(),0);
 }finally{await browser.close();await server.close();}
});
