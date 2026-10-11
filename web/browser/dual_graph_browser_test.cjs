'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const fs = require('node:fs/promises');
const {staticServer, interceptProject, paint, launch} = require('./visual_fixture.cjs');

test('graph provenance, flow controls and execution inspection stay linked across viewports', {
  timeout:90000, skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for graph browser coverage'
}, async t => {
  const server = await staticServer(); t.after(() => server.close());
  const browser = await launch(); t.after(() => browser.close());
  const page = await browser.newPage({viewport:{width:1440,height:960},reducedMotion:'reduce'});
  page.setDefaultTimeout(12000);
  const errors = [], time = '2026-10-11T01:00:00Z';
  page.on('pageerror', error => errors.push(error.message));
  const project = {id:'dual-graph',title:'Evidence and execution review',scenario:'audit',status:'active',orchestration_version:1,generation:1,created_at:time};
  const facts = [
    {id:'origin',description:'Authorized local repository',status:'input'},
    {id:'observation',description:'Initial observation from the first attempt',status:'valid',source_step_id:'scan',run_id:'docker@attempt-1'},
    {id:'result',description:'Retained scan result',status:'valid',source_step_id:'scan',run_id:'docker@attempt-2'},
    {id:'invalid',description:'Corrected premise',status:'refuted'}
  ];
  const state = {graph:{project,facts,intents:[],hints:[]},revision:1,
    goals:[{id:'goal',condition:'Review the retained evidence',status:'open'}],fact_records:facts,
    steps:[{id:'scan',description:'Inspect source files',status:'running',from:['origin'],goal_id:'goal'},
      {id:'review',description:'Review the observations',status:'blocked',depends_on:['scan'],blocked_by:['scan'],from:['observation'],goal_id:'goal'},
      {id:'pause',description:'Paused follow-up',status:'paused',from:['origin']}],
    findings:[],fact_relations:[]};
  const run = (id,status,extra={}) => ({id,project_id:project.id,generation:1,kind:'explore',backend:'docker',intent:'scan',status,created_at:time,updated_at:time,...extra});
  const fixture = {state,runs:[run('attempt-1','failed',{result:{error:'First attempt failed <script>literal evidence</script>'}}),run('attempt-2','running'),run('old-round','succeeded',{generation:0})]};
  const requests = await interceptProject(page,fixture);
  await page.goto(server.url + '/#project/' + project.id,{waitUntil:'networkidle'});
  await page.waitForFunction(() => document.querySelectorAll('#graph-host .graph-node').length === 8);
  await paint(page);
  assert.equal(await page.locator('.canvas-bottom button').count(),await page.locator('.canvas-bottom button svg.lucide').count(),'every canvas control renders its bundled icon');
  const node = key => page.locator('[data-node-key="' + key + '"]');
  await node('step:scan').click();
  assert.equal(await page.locator('#activity-content [data-execution]').count(),2);
  assert.equal(await page.locator('[data-execution="old-round"]').count(),0);
  await page.locator('[data-trace="downstream"]').click();
  assert.equal(await page.locator('[data-trace="downstream"]').getAttribute('aria-pressed'),'true');
  await page.waitForFunction(() => document.querySelector('[data-node-key="fact:observation"]').classList.contains('lineage'));
  await page.locator('#activity-content [data-focus="fact:observation"]').click();
  assert.equal(await page.locator('#activity-content [data-execution]').count(),1);
  await page.locator('[data-execution="attempt-1"]').click();
  assert.match(await page.locator('.inspector-record-mount .inspector-reader-content').textContent(),/First attempt failed <script>literal evidence<\/script>/);
  assert.equal(await page.locator('.inspector-record-mount script').count(),0);
  await page.locator('.inspector-record-mount [data-inspector-action="close"]').click();
  await page.locator('.inspector-record-mount').waitFor({state:'detached'});
  for (const [filter,key] of [['blocked','step:review'],['invalid','fact:invalid'],['paused','step:pause']]) {
    await page.locator('[data-filter="'+filter+'"]').click();
    await page.waitForFunction(key => [...document.querySelectorAll('#graph-host .graph-node')].filter(n => !n.hidden).map(n => n.dataset.nodeKey).join() === key,key);
    await page.locator('[data-filter="'+filter+'"]').click();
  }
  await page.locator('#arrange-graph').click();
  await paint(page);
  const geometry = await page.evaluate(() => Object.fromEntries([...document.querySelectorAll('#graph-host .graph-node')].map(n => [n.dataset.nodeKey,{x:parseFloat(n.style.left),y:parseFloat(n.style.top)}])));
  assert.ok(geometry['fact:origin'].x < geometry['step:scan'].x);
  assert.ok(geometry['step:scan'].x < geometry['fact:observation'].x);
  assert.ok(geometry['fact:observation'].x < geometry['step:review'].x);
  await node('step:scan').click();
  const edgeKey = 'edge:' + JSON.stringify(['step_observation','step:scan','fact:observation']);
  await page.locator('.graph-edge-hit').evaluateAll((nodes,key) => nodes.find(n => n.dataset.edgeKey === key).focus(),edgeKey);
  await page.keyboard.press('Enter');
  assert.equal(await page.locator('#activity-content [data-focus]').count(),2);
  await page.locator('#activity-content [data-focus="step:scan"]').click();

  for (const width of [1440,1920,390]) {
    await page.setViewportSize({width,height:960});
    await page.locator('#fit-graph').click();
    await paint(page);
    const overflow = await page.evaluate(() => {
      const controls = document.querySelector('.canvas-bottom'), footer = document.querySelector('.canvas-footer');
      const rect = el => {const b=el.getBoundingClientRect();return {left:b.left,right:b.right,top:b.top,bottom:b.bottom};};
      return {page:document.documentElement.scrollWidth > innerWidth, controls:controls.scrollWidth > controls.clientWidth,
        footer:footer.scrollWidth > footer.clientWidth,zoom:rect(document.querySelector('.zoom-controls')),trace:rect(document.querySelector('.graph-trace-controls'))};
    });
    assert.equal(overflow.page,false,'page width at '+width);
    assert.equal(overflow.controls,false,'canvas controls at '+width);
    assert.equal(overflow.footer,false,'status controls at '+width);
    assert.ok(overflow.zoom.right <= overflow.trace.left || overflow.zoom.bottom <= overflow.trace.top,'tool groups do not overlap');
    if (process.env.PWNMESH_SCREENSHOT_DIR) {
      await fs.mkdir(process.env.PWNMESH_SCREENSHOT_DIR,{recursive:true});
      await page.screenshot({path:path.join(process.env.PWNMESH_SCREENSHOT_DIR,'dual-graph-'+width+'.png'),fullPage:true});
    }
  }
  // Polling updates the selected inspector and converts only the accepted result edge.
  state.steps[0].status = 'completed'; state.steps[0].result = 'result'; state.steps[0].support_valid = true;
  state.revision++;
  fixture.runs[1].status = 'succeeded'; fixture.runs[1].updated_at = '2026-10-11T01:01:00Z';
  fixture.runs[1].result = {text:JSON.stringify({description:'Confirmed scan outcome'})};
  await page.waitForFunction(() => document.querySelector('[data-node-key="step:scan"]').dataset.status === 'completed');
  assert.equal(await page.locator('#activity-content [data-execution]').count(),2);
  assert.equal(await page.locator('.graph-edge-hit[data-edge-kind="step_result"]').count(),1);
  assert.equal(await page.locator('.graph-edge-hit[data-edge-kind="step_observation"]').count(),1);
  await page.locator('[data-execution="attempt-2"]').click();
  assert.match(await page.locator('.inspector-record-mount .inspector-reader-content').textContent(),/Confirmed scan outcome/);
  assert.deepEqual(errors,[]); assert.deepEqual(requests.writes,[]); assert.deepEqual(requests.unexpected,[]);
});
