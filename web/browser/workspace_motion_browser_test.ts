import test from 'node:test';
import assert from 'node:assert/strict';
import type {Page} from 'playwright';
import {launch, paint, stateFor, staticServer} from './visual_fixture.js';

const options = {timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for browser motion regression'};
type Change = {route: string} | {round: string};
async function sample(page: Page, change: Change) {
  return page.evaluate(async change => {
    if ('route' in change) location.hash = '/project/' + change.route;
    else {const select = document.querySelector<HTMLSelectElement>('#run-select')!; select.value = change.round; select.dispatchEvent(new Event('change',{bubbles:true}));}
    const samples: {title: string; opacity: number; offset: number}[] = [], start = performance.now();
    do {
      await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
      const panel = document.querySelector('.project-content')!, style = getComputedStyle(panel);
      samples.push({title:panel.querySelector('h1')!.textContent || '',opacity:Number(style.opacity),offset:new DOMMatrixReadOnly(style.transform).m42});
    } while (performance.now() - start < 600);
    return samples;
  },change);
}
function fades(samples: Awaited<ReturnType<typeof sample>>, outgoing: string, incoming: string) {
  for (const title of [outgoing,incoming]) assert.ok(samples.some(frame => frame.title === title && frame.opacity > .01 && frame.opacity < .99),title + ' participates in the fade');
  assert.ok(samples.some(frame => Math.abs(frame.offset) > .05),'project changes include a small offset');
  assert.deepEqual(samples.at(-1),{title:incoming,opacity:1,offset:0});
}

test('project and round navigation retains outgoing content, reverses quickly, and preserves cached canvas state',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}}), errors: string[] = [], writes: string[] = [], unexpected: string[] = [];
    page.setDefaultTimeout(5000); page.on('pageerror',error => errors.push(error.message));
    const projects = ['first','second','third'].map(id => ({id,title:id + ' project',status:'active',orchestration_version:1,generation:1}));
    const states = new Map(projects.map(project => [project.id,stateFor(project)]));
    const archives = new Map(projects.map(project => [project.id,Buffer.from(JSON.stringify(stateFor({...project,title:project.id + ' archive',generation:0})))]));
    await page.route('**/model-settings',route => route.fulfill({contentType:'application/json',body:JSON.stringify({protocol:'anthropic',auth_mode:'api_key',base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',has_token:false,connection_mode:'direct',proxy_url:'',reasoning_effort:'max'})}));
    await page.route('**/projects**',route => {
      const request = route.request(), path = new URL(request.url()).pathname, id = path.split('/')[2], state = states.get(id || ''), archive = archives.get(id || '');
      if (request.method() !== 'GET') {writes.push(request.method() + ' ' + path); return route.fulfill({status:405,body:'{}'});}
      let body: unknown;
      if (path === '/projects') body = projects;
      else if (!state) {unexpected.push(path); return route.fulfill({status:404,body:'{}'});}
      else if (path.endsWith('/rounds/0/entries')) body = {items:[{cursor:0,kind:'state',bytes:archive!.length}]};
      else if (path.endsWith('/rounds/0/entries/0')) body = {encoding:'base64',offset:0,total_bytes:archive!.length,data:archive!.toString('base64')};
      else if (path.endsWith('/rounds')) body = {items:[{generation:0,archived_at:'2026-10-16T00:00:00Z'}]};
      else if (path.endsWith('/state')) body = state;
      else if (path.endsWith('/identity')) body = {id,generation:1};
      else if (path.endsWith('/executions/changes')) body = {items:[],deleted:[],through:0,generation:1};
      else if (path.endsWith('/state/events') || path.endsWith('/inputs')) body = [];
      else {unexpected.push(path); return route.fulfill({status:404,body:'{}'});}
      return route.fulfill({contentType:'application/json',body:JSON.stringify(body)});
    });
    await page.goto(server.url+'/#/project/first',{waitUntil:'networkidle'}); await page.locator('[data-node-key="fact:origin"]').waitFor();
    await page.waitForFunction(() => !document.querySelector<HTMLElement>('.project-content')?.dataset.viewMotion);
    await page.locator('#zoom-in').click(); await paint(page);
    const camera = await page.locator('.graph-world').evaluate((element: HTMLElement) => element.style.transform);
    fades(await sample(page,{route:'second'}),'first project','second project');
    const preserved = await page.evaluate(async () => {
      const original = document.querySelector('#graph-host');
      for (const id of ['third','first','second','third','first']) {location.hash = '/project/'+id; await new Promise(resolve => setTimeout(resolve,40));}
      await new Promise(resolve => setTimeout(resolve,600));
      return {sameCanvas:original === document.querySelector('#graph-host'),title:document.querySelector('.project-content h1')?.textContent};
    });
    assert.deepEqual(preserved,{sameCanvas:true,title:'first project'});
    assert.equal(await page.locator('.graph-world').evaluate((element: HTMLElement) => element.style.transform),camera,'returning to a project restores its arranged camera');
    fades(await sample(page,{round:'0'}),'first project','first archive');
    const finalRound = await page.evaluate(async () => {
      for (const round of ['current','0','current','0','current']) {
        const select = document.querySelector<HTMLSelectElement>('#run-select')!;
        if (select.closest<HTMLElement>('[inert]')) throw new Error('Round navigation must remain interactive during its fade');
        select.value = round; select.dispatchEvent(new Event('change',{bubbles:true})); await new Promise(resolve => setTimeout(resolve,20));
      }
      await new Promise(resolve => setTimeout(resolve,500));
      return {title:document.querySelector('.project-content h1')?.textContent,round:document.querySelector<HTMLSelectElement>('#run-select')?.value};
    });
    assert.deepEqual(finalRound,{title:'first project',round:'current'});
    projects[0]!.title = 'First refreshed'; states.get('first')!.revision++;
    await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
    await page.waitForFunction(() => document.querySelector('.project-content h1')?.textContent === 'First refreshed');
    assert.equal(await page.locator('.project-content').getAttribute('data-view-motion'),null,'ordinary polling does not replay navigation');
    await page.evaluate(async () => {location.hash = '/project/second'; await new Promise(resolve => setTimeout(resolve,40)); document.querySelector<HTMLElement>('#model-entry')!.click();});
    await page.locator('.llm-page').waitFor(); await page.locator('.llm-back').click(); await page.locator('.project-content').waitFor();
    await page.waitForFunction(() => !document.querySelector<HTMLElement>('.project-content')?.dataset.viewMotion);
    assert.deepEqual(await page.locator('.project-content').evaluate((element: HTMLElement) => ({inert:element.inert,opacity:Number(getComputedStyle(element).opacity)})),{inert:false,opacity:1});
    assert.deepEqual(writes,[]); assert.deepEqual(unexpected,[]); assert.deepEqual(errors,[]);
  } finally {await browser.close(); await server.close();}
});
