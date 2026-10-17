import test from 'node:test';
import assert from 'node:assert/strict';
import type {Page} from 'playwright';
import {interceptProject, launch, paint, stateFor, staticServer} from './visual_fixture.js';

const options = {timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for disclosure motion regression'};
const longText = Array.from({length:20},(_, index) => `Line ${index + 1}: controlled disclosure content with enough text to wrap.`).join('\n');

async function sample(page: Page, selector: string, trigger: string) {
  return page.evaluate(async ({selector,trigger}) => {
    const element = document.querySelector<HTMLElement>(selector)!;
    const read = () => {const style = getComputedStyle(element); return {height:element.getBoundingClientRect().height,opacity:Number(style.opacity),offset:new DOMMatrixReadOnly(style.transform).m42};};
    const frames = [read()], start = performance.now();
    document.querySelector<HTMLElement>(trigger)!.click();
    do {await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));frames.push(read());} while (performance.now() - start < 550);
    return frames;
  },{selector,trigger});
}
function smooth(frames: Awaited<ReturnType<typeof sample>>, label: string) {
  const first = frames[0]!.height,last = frames.at(-1)!.height,low = Math.min(first,last),high = Math.max(first,last);
  assert.ok(high - low > 10,`${label} changes disclosure height`);
  const middle = frames.filter(frame => frame.height > low + .25 && frame.height < high - .25);
  assert.ok(new Set(middle.map(frame => frame.height.toFixed(1))).size >= 2,`${label} interpolates height: ${frames.map(frame => frame.height.toFixed(1)).join(', ')}`);
  assert.ok(frames.some(frame => frame.opacity > .01 && frame.opacity < .99),`${label} fades`);
  assert.ok(frames.some(frame => Math.abs(frame.offset) > .1),`${label} has gentle displacement`);
}

test('text cards, logs, and nested inspector disclosures ease in both directions and survive reversals',options,async () => {
  const server = await staticServer(),browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1100}}),errors: string[] = [];
    page.on('pageerror',error => errors.push(error.message));
    const project = {id:'disclosure-fixture',title:'Disclosure motion',status:'active',orchestration_version:1,generation:0,created_at:'2026-10-17T00:00:00Z'};
    const state = stateFor(project);
    state.graph.facts[0]!.description = longText;
    const fact = {id:'long',description:longText,status:'valid',created_at:project.created_at,detail:{body:longText,output:longText,evidence:[{path:'/evidence.txt',excerpt:longText}]}};
    state.graph.facts.push(fact);state.fact_records.push(fact);
    const requests = await interceptProject(page,{state,inputs:[{id:'text',name:'request.http',size:120,created_at:project.created_at}]});
    await page.goto(server.url+'/#/project/'+project.id,{waitUntil:'networkidle'});await paint(page);
    const log = '[data-record="state:fact:long"] details';
    for (const direction of ['open','close']) smooth(await sample(page,log,log+' > summary'),'log '+direction);
    await page.locator('#view-tab-materials').click();
    await page.locator('[data-content-key="target"] .content-text').waitFor();
    await page.waitForFunction(() => !document.querySelector('[data-view-motion]'));
    const card = '[data-content-key="target"]';
    for (const direction of ['open','close']) smooth(await sample(page,card+' .content-text',card+' [data-content-toggle]'),'card '+direction);
    await page.locator('#view-tab-graph').click();
    await page.locator('[data-node-key="fact:long"]').click();
    await page.locator('.activity-pane .node-inspector').waitFor();
    await page.waitForFunction(() => !document.querySelector('[data-view-motion]'));
    for (const direction of ['open','close']) smooth(await sample(page,'.inspector-body-section','[data-inspector-action="toggle"]'),'inspector body '+direction);
    const output = '.inspector-fold:not(.inspector-evidence)';
    for (const direction of ['open','close']) smooth(await sample(page,output,output+' > summary'),'inspector output '+direction);
    await sample(page,output,output+' > summary');
    await page.locator(output+' [data-inspector-action="collapse-section"]').scrollIntoViewIfNeeded();
    smooth(await sample(page,output,output+' [data-inspector-action="collapse-section"]'),'inspector footer collapse');
    assert.equal(await page.locator(output+' > summary').evaluate(summary => {
      const host = summary.closest('#activity-content')!.getBoundingClientRect(),box = summary.getBoundingClientRect();
      const header = document.querySelector('.activity-pane .selection-header')!.getBoundingClientRect();
      return box.top >= header.bottom - 1 && box.bottom <= host.bottom + 1 && document.activeElement === summary;
    }),true,'collapsing from the bottom returns focus and scroll to the visible summary');
    const evidence = '.inspector-evidence';
    smooth(await sample(page,evidence,evidence+' > summary'),'evidence group');
    const item = '.inspector-evidence-item';
    for (const direction of ['open','close']) smooth(await sample(page,item,item+' > summary'),'nested evidence '+direction);
    for (const selector of [output,evidence,item]) {
      // Open the evidence parent before exercising its nested disclosure.
      if (selector === item) {await page.locator(evidence+' > summary').click();await page.waitForTimeout(450);}
      await page.evaluate(async selector => {
        const summary = document.querySelector<HTMLElement>(selector+' > summary')!;
        for (let index=0;index<7;index++) {summary.click();await new Promise(resolve => setTimeout(resolve,35));}
        await new Promise(resolve => setTimeout(resolve,500));
      },selector);
      assert.equal(await page.locator(selector).evaluate(node => node.getAnimations({subtree:true}).filter(animation => animation.playState === 'running').length),0,'rapid toggles settle');
      assert.equal(await page.locator(selector).evaluate(node => getComputedStyle(node).opacity),'1');
      assert.equal(await page.locator(selector).evaluate(node => (node as HTMLElement).style.height),'');
      assert.equal(await page.locator(selector).evaluate(node => (node as HTMLDetailsElement).open === (node.querySelector('summary')!.getAttribute('aria-expanded') === 'true')),true,'rendered state matches the latest toggle');
    }
    await page.emulateMedia({reducedMotion:'reduce'});
    const reduced = await page.locator(output).evaluate(async element => {
      const details = element as HTMLDetailsElement,before = details.open;
      details.querySelector<HTMLElement>('summary')!.click();await Promise.resolve();await Promise.resolve();
      return {changed:details.open !== before,running:details.getAnimations({subtree:true}).filter(animation => animation.playState === 'running').length};
    });
    assert.deepEqual(reduced,{changed:true,running:0});
    await page.emulateMedia({reducedMotion:'no-preference'});
    await page.locator('#add-hint').click();
    await page.waitForFunction(() => !document.querySelector('dialog[data-motion]'));
    for (const direction of ['open','close']) smooth(await sample(page,'#hint-imported','#hint-imported > summary'),'imported materials '+direction);
    assert.deepEqual(errors,[]);assert.deepEqual(requests.writes,[]);assert.deepEqual(requests.unexpected,[]);
  } finally {await browser.close();await server.close();}
});
