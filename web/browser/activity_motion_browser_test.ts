import test from 'node:test';
import assert from 'node:assert/strict';
import type {Page} from 'playwright';
import {interceptProject, launch, paint, stateFor, staticServer} from './visual_fixture.js';

const options = {timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for browser motion regression'};

async function transition(page: Page, selector: string) {
  const frames = await page.evaluate(async selector => {
    document.querySelector(selector)!.dispatchEvent(new MouseEvent('click',{bubbles:true}));
    const samples: {opacity: number; offset: number}[] = [], start = performance.now();
    do {
      await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
      const style = getComputedStyle(document.querySelector('.activity-pane')!);
      samples.push({opacity:Number(style.opacity),offset:new DOMMatrixReadOnly(style.transform).m42});
    } while (performance.now() - start < 450);
    return samples;
  },selector);
  assert.ok(frames.some(frame => frame.opacity > .01 && frame.opacity < .99),'activity changes include partial opacity');
  assert.ok(frames.some(frame => Math.abs(frame.offset) > .05),'activity changes include a small vertical offset');
  assert.deepEqual(frames.at(-1),{opacity:1,offset:0});
}

test('activity fades between logs, nodes and edges, interrupts cleanly, and preserves reading state on refresh',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}}), errors: string[] = [];
    page.setDefaultTimeout(5000); page.on('pageerror',error => errors.push(error.message));
    const project = {id:'activity-motion',title:'Activity motion fixture',status:'active',orchestration_version:1,generation:0};
    const state = stateFor(project);
    const first = {id:'first',description:'First selected fact',status:'valid',detail:{body:'First detail'}};
    const second = {id:'second',description:'Second selected fact',status:'valid',detail:{body:Array.from({length:60},(_,index) => 'Evidence line '+index).join('\n')}};
    state.graph.facts.push(first,second); state.fact_records.push(first,second);
    state.fact_relations.push({kind:'supports',source:'first',target:'second',reason:'Fixture relation'});
    const requests = await interceptProject(page,{state});
    await page.goto(server.url+'/#/project/'+project.id,{waitUntil:'networkidle'});
    await page.locator('[data-node-key="fact:first"]').waitFor(); await paint(page);
    await transition(page,'[data-node-key="fact:first"]');
    assert.equal(await page.locator('.inspector-title').textContent(),first.description);
    await transition(page,'[data-node-key="fact:second"]');
    assert.equal(await page.locator('.inspector-title').textContent(),second.description);
    await transition(page,'.graph-edge-hit');
    assert.equal(await page.locator('#activity-title').textContent(),'关系详情');
    await transition(page,'#clear-selection');
    assert.equal(await page.locator('.log-stream').isVisible(),true);

    await page.evaluate(async () => {
      for (const key of ['first','second','first','second','first','second']) {
        document.querySelector<HTMLElement>(`[data-node-key="fact:${key}"]`)!.click();
        await new Promise(resolve => setTimeout(resolve,20));
      }
      await new Promise(resolve => setTimeout(resolve,450));
    });
    assert.equal(await page.locator('.inspector-title').textContent(),second.description);
    assert.deepEqual(await page.locator('.activity-pane').evaluate((pane: HTMLElement) => ({inert:pane.inert,motion:pane.dataset.viewMotion || '',running:pane.getAnimations().filter(animation => animation.playState === 'running').length})),{inert:false,motion:'',running:0});

    await page.locator('[data-inspector-action="toggle"]').click();
    await page.waitForFunction(() => document.querySelector('[data-inspector-action="toggle"]')?.getAttribute('aria-expanded') === 'true');
    const savedScroll = await page.locator('#activity-content').evaluate(async element => {
      await new Promise(resolve => setTimeout(resolve,350)); element.scrollTop = 180; return element.scrollTop;
    });
    assert.ok(savedScroll > 0,'the expanded inspector is scrollable');
    second.detail.body += '\nFresh polling marker'; state.revision++;
    const refreshed = await page.evaluate(async () => {
      const pane = document.querySelector<HTMLElement>('.activity-pane')!, host = document.querySelector<HTMLElement>('#activity-content')!;
      let animated = false;
      const observer = new MutationObserver(() => {if (pane.dataset.viewMotion) animated = true;});
      observer.observe(pane,{attributes:true,attributeFilter:['data-view-motion']});
      document.dispatchEvent(new Event('visibilitychange'));
      const start = performance.now();
      while (!host.textContent?.includes('Fresh polling marker') && performance.now() - start < 3000) await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
      observer.disconnect();
      return {updated:host.textContent?.includes('Fresh polling marker'),scroll:host.scrollTop,expanded:document.querySelector('[data-inspector-action="toggle"]')?.getAttribute('aria-expanded'),animated};
    });
    assert.deepEqual(refreshed,{updated:true,scroll:savedScroll,expanded:'true',animated:false},'polling refreshes content without replaying motion or losing reading state');

    await page.evaluate(async () => {
      document.querySelector<HTMLElement>('[data-node-key="fact:first"]')!.click();
      await new Promise(resolve => setTimeout(resolve,20)); location.hash = '/llm';
    });
    await page.locator('.llm-page').waitFor(); await page.locator('.llm-back').click();
    await page.locator('.activity-pane').waitFor(); await paint(page);
    assert.equal(await page.locator('.activity-pane').evaluate((pane: HTMLElement) => pane.inert),false,'cached activity releases interrupted motion');
    await page.emulateMedia({reducedMotion:'reduce'});
    await page.locator('[data-node-key="fact:second"]').click(); await paint(page);
    assert.equal(await page.locator('.inspector-title').textContent(),second.description);
    assert.deepEqual(await page.locator('.activity-pane').evaluate((pane: HTMLElement) => ({inert:pane.inert,motion:pane.dataset.viewMotion || ''})),{inert:false,motion:''});
    assert.deepEqual(requests.writes,[]); assert.deepEqual(requests.unexpected,[]); assert.deepEqual(errors,[]);
  } finally {await browser.close(); await server.close();}
});
