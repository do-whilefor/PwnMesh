import test from 'node:test';
import assert from 'node:assert/strict';
import type {FixtureState} from './visual_fixture.js';
import {staticServer,launch,interceptProject,paint} from './visual_fixture.js';

// Exercise the production Vue workbench through its public controls and HTTP reads.
// Graph algorithms and controller lifecycle contracts have focused Vitest coverage.
test('embedded canvas handles live graph changes and unrestricted pointer movement', {
  skip: process.env.PWNMESH_WEB_URL || process.env.PLAYWRIGHT_MODULE ? false : 'Set PWNMESH_WEB_URL or PLAYWRIGHT_MODULE to load the canvas assets', timeout:90000
}, async t => {
  const browser = await launch(); t.after(() => browser.close());
  const server = process.env.PWNMESH_WEB_URL ? null : await staticServer();
  if (server) t.after(() => server.close());
  const page = await browser.newPage({viewport:{width:1440,height:960},reducedMotion:'reduce'});
  page.setDefaultTimeout(10000);
  const errors: string[] = []; page.on('pageerror',error => errors.push(error.message));
  const state: FixtureState = {
    graph:{project:{id:'browser-graph',title:'Graph acceptance',generation:0,status:'active',scenario:'pentest',orchestration_version:1,created_at:'2026-10-10T02:00:00Z'},
      facts:[{id:'origin',description:'https://authorized.example.test'},{id:'goal',description:'Controlled graph evidence'}],intents:[],hints:[]},
    revision:1, goals:[{id:'goal',condition:'真实目标',status:'open'}],
    steps:Array.from({length:60},(_,i) => ({id:'s'+i,description:'任务 '+i,goal_id:'goal',from:['origin'],result:'f'+i,status:'completed',support_valid:true})),
    fact_records:[{id:'origin',description:'项目输入',status:'input'},...Array.from({length:60},(_,i) => ({id:'f'+i,description:'证据 '+i,status:'valid'}))],
    findings:[],fact_relations:[{kind:'refutes',source:'f0',target:'f1',reason:'第一条说明'},
      {kind:'narrows',source:'f0',target:'f1',reason:'另一种关系'},
      {kind:'supersedes',source:'f1',target:'f0',reason:'有环'},
      {kind:'refutes',source:'missing',target:'f1',reason:'缺失引用'}]
  };
  const requests = await interceptProject(page,{state});
  let firstStateRead: number | undefined;
  page.on('response',response => { if (new URL(response.url()).pathname.endsWith('/state')) firstStateRead ??= Date.now(); });
  await page.goto(new URL('/#project/browser-graph',process.env.PWNMESH_WEB_URL || server!.url).href);
  await page.waitForFunction(() => document.querySelectorAll<HTMLElement>('#graph-host .graph-node').length === 122);
  assert.ok(firstStateRead, 'the initial state response was observed');
  const initialDuration = Date.now() - firstStateRead;
  assert.ok(initialDuration < 3000, `122-node render after state response took ${initialDuration}ms`);
  await paint(page);
  const snapshot = () => page.evaluate(() => {
    const host = document.querySelector<HTMLElement>('#graph-host')!, viewport = host.querySelector<HTMLElement>('.graph-viewport')!;
    const transform = new DOMMatrix(getComputedStyle(host.querySelector<HTMLElement>('.graph-world')!).transform);
    return {positions:[...host.querySelectorAll<HTMLElement>('.graph-node')].map(node => [node.dataset.nodeKey,{x:parseFloat(node.style.left),y:parseFloat(node.style.top)}] as const),
      camera:[transform.a,transform.e,transform.f],width:viewport.clientWidth,height:viewport.clientHeight};
  });
  const initial = await snapshot();
  const presentation = await page.evaluate(() => {
    const card = (key: string) => {
      const element = document.querySelector<HTMLElement>(`[data-node-key="${key}"]`)!, style = getComputedStyle(element);
      return {width:element.offsetWidth,height:element.offsetHeight,background:style.backgroundColor};
    };
    const edge = (kind: string) => {
      const element = document.querySelector<SVGElement>(`#graph-host .graph-edge[data-edge-kind="${kind}"]`)!;
      const marker = document.getElementById(element.getAttribute('marker-end')!.slice(5,-1))!;
      return {stroke:getComputedStyle(element).stroke,marker:marker.firstElementChild!.getAttribute('stroke'),fill:marker.firstElementChild!.getAttribute('fill')};
    };
    return {start:card('fact:origin'),fact:card('fact:f0'),task:card('step:s0'),goal:card('goal:goal'),input:edge('step_input'),output:edge('step_result'),refutes:edge('refutes')};
  });
  for (const kind of ['start','fact','task','goal'] as const) { assert.equal(presentation[kind].width,148); assert.equal(presentation[kind].height,78); }
  assert.equal(presentation.start.background,'rgb(52, 73, 96)');
  assert.equal(presentation.fact.background,'rgb(243, 248, 255)');
  assert.equal(presentation.task.background,'rgb(247, 244, 255)');
  assert.equal(presentation.goal.background,'rgb(237, 248, 244)');
  assert.equal(new Set((['input','output','refutes'] as const).map(kind=>presentation[kind].stroke)).size,3);
  assert.equal(presentation.input.fill,'none'); assert.equal(presentation.input.marker,'#6b89b2');

  const node = page.locator('#graph-host [data-node-key="fact:f0"]'), originalElement = await node.elementHandle();
  assert.ok(originalElement);
  state.fact_records.reverse(); state.steps.reverse();
  state.fact_records.find(f=>f.id==='f0')!.description = '变更标题，不重排';
  state.fact_relations[0].reason = '更新后的关系说明'; state.revision++;
  await page.waitForFunction(() => document.querySelector<HTMLElement>('[data-node-key="fact:f0"]')!.textContent?.includes('变更标题'));
  await paint(page);
  const update = await snapshot();
  assert.deepEqual(new Map(update.positions),new Map(initial.positions)); assert.deepEqual(update.camera,initial.camera);
  assert.equal(await originalElement.evaluate(element=>element===document.querySelector<HTMLElement>('[data-node-key="fact:f0"]')!),true);
  await page.locator('#graph-host .graph-edge-hit[data-edge-kind="refutes"]').first().dispatchEvent('click');
  await page.waitForFunction(() => document.querySelector<HTMLElement>('.node-inspector')?.textContent?.includes('更新后的关系说明'));
  await page.locator('#clear-selection').click();
  assert.equal(await page.locator('#graph-host .graph-edge-hit').count(),183);

  await page.locator('[data-filter="done"]').click(); await paint(page);
  const filtered = await snapshot();
  assert.deepEqual(new Map(filtered.positions),new Map(initial.positions)); assert.deepEqual(filtered.camera,initial.camera);
  assert.equal(await page.locator('#graph-host .graph-node:visible').count(),121);
  assert.equal(await page.locator('#graph-host .graph-edge-hit:visible').count(),123);
  await page.locator('.graph-viewport').focus(); await page.keyboard.press('Tab');
  assert.ok(await page.evaluate(()=>(document.activeElement as HTMLElement).dataset.edgeKey),'visible edges remain keyboard reachable');
  await page.locator('[data-filter="pending"]').click(); await paint(page);
  assert.equal(await page.locator('#graph-host .graph-node:visible').count(),1);
  assert.equal(await page.locator('#graph-host .graph-edge-hit:visible').count(),0);
  await page.locator('.graph-viewport').focus(); await page.keyboard.press('0'); await paint(page);
  const viewport = await page.locator('.graph-viewport').boundingBox(), goal = await page.locator('[data-node-key="goal:goal"]').boundingBox();
  assert.ok(viewport); assert.ok(goal);
  assert.ok(goal.x>=viewport.x && goal.y>=viewport.y && goal.x+goal.width<=viewport.x+viewport.width && goal.y+goal.height<=viewport.y+viewport.height);
  await page.keyboard.press('Tab');
  assert.equal(await page.evaluate(()=>(document.activeElement as HTMLElement).dataset.nodeKey),'goal:goal');
  await page.locator('[data-filter="running"]').click(); await paint(page);
  assert.equal(await page.locator('#graph-host .graph-node:visible').count(),0);
  state.steps.find(step=>step.id==='s0')!.status='running'; state.revision++;
  await page.waitForFunction(()=>!document.querySelector<HTMLElement>('[data-node-key="step:s0"]')!.hidden);
  assert.equal(await page.locator('#graph-host .graph-node:visible').count(),1);
  assert.equal(await page.locator('#graph-host .graph-edge-hit:visible').count(),0);
  state.steps.find(step=>step.id==='s0')!.status='completed'; state.revision++;
  await page.waitForFunction(()=>document.querySelector<HTMLElement>('[data-node-key="step:s0"]')!.dataset.status==='completed');
  await page.locator('[data-filter="running"]').click(); await paint(page);
  assert.equal(await page.locator('#graph-host .graph-node:visible').count(),122);

  // Move using the real keyboard handler; negative coordinates remain valid.
  await node.evaluate(element => { for(let i=0;i<400;i++) element.dispatchEvent(new KeyboardEvent('keydown',{key:'ArrowLeft',bubbles:true})); });
  await paint(page); assert.ok(new Map((await snapshot()).positions).get('fact:f0')!.x<0);
  await page.locator('#fit-graph').click(); await paint(page);
  const far = await node.boundingBox(), area = await page.locator('.graph-viewport').boundingBox();
  assert.ok(far); assert.ok(area);
  assert.ok(far.x>=area.x && far.y>=area.y && far.x+far.width<=area.x+area.width+1 && far.y+far.height<=area.y+area.height+1);
  await page.mouse.move(far.x+far.width/2,far.y+far.height/2);
  // Inspect a readable card before dragging, as a user would on a dense graph.
  // The whole-graph fit makes cards smaller than one frame of edge-pan travel.
  for (let i=0;i<3;i++) { await page.mouse.wheel(0,-150); await paint(page); }
  const enlarged = await node.boundingBox(); assert.ok(enlarged);
  await page.mouse.move(enlarged.x+enlarged.width/2,enlarged.y+enlarged.height/2); await page.mouse.down();
  await page.mouse.move(area.x+8,area.y+8,{steps:8});
  const before = await snapshot(); await page.waitForTimeout(220); const moving = await snapshot();
  assert.ok(moving.camera[1]>before.camera[1] && moving.camera[2]>before.camera[2],'top/left edge pan continues without pointer events');
  assert.ok(new Map(moving.positions).get('fact:f0')!.x<new Map(before.positions).get('fact:f0')!.x);
  await page.mouse.wheel(0,-100); await page.waitForTimeout(50);
  const grabbed = await node.boundingBox(); assert.ok(grabbed);
  assert.ok(grabbed.x<=area.x+9 && grabbed.x+grabbed.width>=area.x+7 && grabbed.y<=area.y+9 && grabbed.y+grabbed.height>=area.y+7,'zoom retains the pointer grab: '+JSON.stringify({grabbed,area,camera:(await snapshot()).camera}));
  await page.mouse.up(); const stopped=(await snapshot()).camera; await page.waitForTimeout(130);
  assert.deepEqual((await snapshot()).camera,stopped);
  await page.locator('#fit-graph').click(); await paint(page);
  const secondGrab=await node.boundingBox(); assert.ok(secondGrab);
  await page.mouse.move(secondGrab.x+secondGrab.width/2,secondGrab.y+secondGrab.height/2); await page.mouse.down();
  await page.mouse.move(area.x+area.width-8,area.y+area.height-8,{steps:8});
  const rightBefore=(await snapshot()).camera; await page.waitForTimeout(220); const rightAfter=(await snapshot()).camera;
  assert.ok(rightAfter[1]<rightBefore[1] && rightAfter[2]<rightBefore[2],'right/bottom edge pan continues');
  await page.evaluate(()=>window.dispatchEvent(new Event('blur'))); await page.mouse.up();
  const cancelled=(await snapshot()).camera; await page.waitForTimeout(130); assert.deepEqual((await snapshot()).camera,cancelled);
  const center=await snapshot();
  await page.setViewportSize({width:1280,height:800}); await paint(page); const resized=await snapshot();
  assert.equal(resized.camera[0],center.camera[0]);
  const worldCenter=(value: Awaited<ReturnType<typeof snapshot>>)=>[(value.width/2-value.camera[1])/value.camera[0],(value.height/2-value.camera[2])/value.camera[0]];
  worldCenter(center).forEach((coordinate,index)=>assert.ok(Math.abs(coordinate-worldCenter(resized)[index])<.01,'resize keeps world center'));
  await page.setViewportSize({width:1440,height:960}); await paint(page);
  const saved=await snapshot();
  for (const view of ['materials','graph']) {
    await page.locator('[data-view="'+view+'"]').click();
    await page.locator('#project-view[aria-labelledby="view-tab-'+view+'"]:not([inert])').waitFor();
  }
  await paint(page);
  const restored=await snapshot(); assert.deepEqual(new Map(restored.positions),new Map(saved.positions)); assert.deepEqual(restored.camera,saved.camera);
  await page.locator('[data-filter="done"]').click(); await node.dispatchEvent('click'); await paint(page);
  const beforeSettings=await snapshot();
  await page.locator('#model-entry').click(); await page.locator('.llm-page').waitFor();
  assert.equal(await page.locator('#graph-host').count(),0);
  await page.locator('[data-llm-return]').click(); await page.locator('#graph-host').waitFor(); await paint(page);
  const afterSettings=await snapshot();
  assert.deepEqual(new Map(afterSettings.positions),new Map(beforeSettings.positions)); assert.deepEqual(afterSettings.camera,beforeSettings.camera);
  assert.equal(await page.locator('[data-filter="done"]').getAttribute('aria-pressed'),'true');
  assert.equal(await node.getAttribute('aria-pressed'),'true');
  await page.locator('#clear-selection').click(); await page.locator('[data-filter="done"]').click();
  state.fact_records=state.fact_records.filter(f=>f.id!=='f0'); state.revision++;
  await page.waitForFunction(()=>!document.querySelector<HTMLElement>('[data-node-key="fact:f0"]')!);
  state.graph.project.generation=1; state.fact_records.find(f=>f.id==='origin')!.description='新的轮次'; state.revision++;
  await page.waitForFunction(()=>document.querySelector<HTMLElement>('[data-node-key="fact:origin"]')!.textContent?.includes('新的轮次')); await paint(page);
  assert.notDeepEqual((await snapshot()).camera,saved.camera);
  assert.deepEqual(errors,[]); assert.deepEqual(requests.writes,[]); assert.deepEqual(requests.unexpected,[]);
  t.diagnostic(`122 nodes / 183 edges; first accepted state rendered in ${initialDuration} ms`);
});
