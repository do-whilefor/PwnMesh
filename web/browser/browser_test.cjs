'use strict';
const test=require('node:test'),assert=require('node:assert/strict');
const {launch,projectPath,fillCreate}=require('./demo_harness.cjs');
test('demo workbench creates real projects and persists stop, resume, terminate, restart and delete',{
 timeout:180000,skip:process.env.PWNMESH_WEB_URL?false:'Set PWNMESH_WEB_URL to a dedicated empty service',
},async()=>{
 const base=new URL(process.env.PWNMESH_WEB_URL),created=new Set();
 const request=async(path,options={})=>{const r=await fetch(new URL(path,base),options);assert.ok(r.ok,path+': '+r.status);return r.status===204?null:r.json();};
 assert.deepEqual(await request('/projects'),[],'use a dedicated empty database');
 const browser=await launch();
 try{
  const page=await browser.newPage({viewport:{width:1440,height:960}});page.setDefaultTimeout(15000);const errors=[];page.on('pageerror',error=>errors.push(error.message));
  await page.goto(base.href,{waitUntil:'networkidle'});assert.equal(await page.locator('#model-entry').count(),1,'initial workspace exposes model connection');
  for(const type of ['ctf','audit','pentest']){
   const title='Demo真实生命周期 '+type;await fillCreate(page,title,type);await page.locator('#create-form [type="submit"]').click();await page.locator('#create-dialog').waitFor({state:'hidden'});await page.waitForFunction(title=>document.querySelector('.project-heading h1')?.textContent===title,title);
   const id=await page.evaluate(()=>location.hash.slice(9));created.add(id);const actual=await request(projectPath(id));assert.equal(actual.project.scenario,type);assert.equal(actual.project.status,'active');assert.match(await page.locator('#project-created-at').textContent(),/^\d{4}-\d{2}-\d{2} /);
  }
  const id=[...created].at(-1),status=async value=>{await page.waitForFunction(value=>document.querySelector('.project-heading .badge')?.classList.contains(value),value);};
  await page.locator('#project-manage').click();await page.locator('#project-dialog [data-action="pause"]').click();await status('paused');assert.equal((await request(projectPath(id))).project.status,'stopped');
  await page.locator('#project-dialog [data-action="resume"]').click();await status('running');assert.equal((await request(projectPath(id))).project.status,'active');
  await page.locator('#project-dialog [data-action="terminate"]').click();await page.locator('#confirm-action').click();await page.locator('#action-dialog').waitFor({state:'hidden'});await status('terminated');assert.equal((await request(projectPath(id))).project.status,'terminated');
  const row=page.locator('.project-row').filter({has:page.locator('a[href="#project/'+id+'"]')});await row.locator('.project-more').click();await page.locator('#project-menu [data-action="restart"]').click();await page.locator('#confirm-action').click();await page.locator('#action-dialog').waitFor({state:'hidden'});await status('running');assert.equal((await request(projectPath(id))).project.generation,1);
  await page.reload({waitUntil:'networkidle'});await page.waitForFunction(id=>document.querySelector('.project-item[aria-current="page"]')?.getAttribute('href')==='#project/'+id,id);assert.equal((await request(projectPath(id))).project.generation,1);
  await page.locator('#add-hint').click();await page.locator('#hint-text').fill('浏览器提交的真实补充说明');await page.locator('#hint-form [type="submit"]').click();await page.locator('#hint-dialog').waitFor({state:'hidden'});assert.ok(JSON.stringify(await request(projectPath(id))).includes('浏览器提交的真实补充说明'));
  await page.locator('[data-view="materials"]').click();assert.match(await page.locator('#project-view').textContent(),/authorized\.example\.test/);await page.locator('[data-view="result"]').click();assert.match(await page.locator('#project-view').textContent(),/暂无结果/);
  await page.locator('[data-view="graph"]').click();assert.ok(await page.locator('.graph-node').count()>0);
  for(const target of [...created]){
   const targetRow=page.locator('.project-row').filter({has:page.locator('a[href="#project/'+target+'"]')});await targetRow.locator('.project-more').click();await page.locator('#project-menu [data-action="delete"]').click();await page.locator('#confirm-action').click();await page.locator('#action-dialog').waitFor({state:'hidden'});await targetRow.waitFor({state:'detached'});created.delete(target);
  }
  assert.deepEqual(await request('/projects'),[]);assert.deepEqual(errors,[]);
 }finally{await browser.close();for(const id of created)await request(projectPath(id),{method:'DELETE'});}
});
