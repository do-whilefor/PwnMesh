import test from 'node:test';
import assert from 'node:assert/strict';
import type {Page} from 'playwright';
import {interceptProject, launch, paint, stateFor, staticServer} from './visual_fixture.js';
import type {ProjectFixture} from './visual_fixture.js';

const options = {timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for browser motion regression'};
const settings = {protocol:'anthropic',auth_mode:'api_key',base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',has_token:false,connection_mode:'direct',proxy_url:'',reasoning_effort:'max'};
type Change = {selector: string; value?: string};

function workspaceFixture(): ProjectFixture {
  const project = {id:'motion-fixture',title:'Workspace motion regression',scenario:'pentest',status:'active',orchestration_version:1,generation:0,created_at:'2026-10-17T00:00:00Z'};
  const state = stateFor(project);
  const fact = {id:'evidence',description:'Controlled evidence for the reader and canvas.',status:'valid',created_at:project.created_at};
  state.graph.facts.push(fact); state.fact_records.push(fact);
  state.steps.push({id:'verify',goal_id:'goal',description:'Verify controlled evidence',status:'completed',from:['origin'],result:fact.id,created_at:project.created_at});
  state.findings.push({id:'verified',claim:'Controlled verified finding',scope:'/fixture',status:'verified',sources:[fact.id],created_at:project.created_at});
  return {state,inputs:[{id:'attachment',name:'capture.bin',size:12,path:'/workspace/inputs/capture.bin',created_at:project.created_at}]};
}

type MotionFrame = {opacity: number; offset: number};
function eases(frames: MotionFrame[], label: string) {
  assert.ok(frames.some(frame => frame.opacity > .01 && frame.opacity < .99),`${label} includes partial opacity`);
  assert.ok(frames.some(frame => Math.abs(frame.offset) > .05),`${label} includes a small vertical offset`);
}

async function mockSettings(page: Page) {
  await page.route('**/model-settings',route => route.fulfill({contentType:'application/json',body:JSON.stringify(settings)}));
  await page.route('**/projects',route => route.fulfill({contentType:'application/json',body:'[]'}));
}

test('project menus fade both ways and rapid dismissal never leaves invisible actions active',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}}), errors: string[] = [];
    page.on('pageerror',error => errors.push(error.message));
    const fixture = workspaceFixture(); await interceptProject(page,fixture);
    await page.goto(server.url+'/#project/'+fixture.state.graph.project.id,{waitUntil:'networkidle'});
    const sample = (closing: boolean) => page.evaluate(async closing => {
      if (closing) document.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}));
      else document.querySelector<HTMLButtonElement>('[data-project-menu]')!.click();
      const frames: MotionFrame[] = [], start = performance.now();
      do {
        await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
        const menu = document.querySelector<HTMLElement>('#project-menu')!, style = getComputedStyle(menu);
        if (style.display !== 'none') frames.push({opacity:Number(style.opacity),offset:new DOMMatrixReadOnly(style.transform).m42});
      } while (performance.now() - start < 350);
      return frames;
    },closing);
    eases(await sample(false),'project menu opening');
    eases(await sample(true),'project menu closing');
    assert.equal(await page.locator('[data-project-menu]').evaluate(element => document.activeElement === element),true);
    await page.evaluate(async () => {
      const trigger = document.querySelector<HTMLButtonElement>('[data-project-menu]')!;
      for (let index = 0; index < 9; index++) {trigger.click();await new Promise(resolve => setTimeout(resolve,25));}
    });
    await page.waitForFunction(() => {
      const menu = document.querySelector<HTMLElement>('#project-menu')!;
      return !menu.inert && getComputedStyle(menu).opacity === '1' && !menu.getAnimations().length;
    });
    assert.equal(await page.locator('#project-menu [role="menuitem"]').first().evaluate(element => document.activeElement === element),true);
    await page.keyboard.press('Escape');
    assert.equal(await page.locator('#project-menu').evaluate(element => (element as HTMLElement).inert),true,'closing menu cannot receive clicks or keyboard focus');
    await page.locator('#project-menu').waitFor({state:'hidden'});
    await page.emulateMedia({reducedMotion:'reduce'});
    await sample(false); await sample(true);
    assert.equal(await page.locator('#project-menu').isVisible(),false);
    assert.deepEqual(errors,[]);
  } finally {await browser.close();await server.close();}
});

test('toast keeps its message throughout a soft entrance and exit',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}});
    await interceptProject(page,workspaceFixture());
    await page.goto(server.url+'/#project/motion-fixture',{waitUntil:'networkidle'});
    await page.locator('#view-tab-result').click(); await page.locator('#export-results').waitFor();
    const samples = await page.evaluate(async () => {
      document.querySelector<HTMLButtonElement>('#export-results')!.click();
      const frames: (MotionFrame & {elapsed:number; text:string})[] = [], start = performance.now();
      do {
        await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
        const toast = document.querySelector<HTMLElement>('#toast')!, style = getComputedStyle(toast);
        if (style.display !== 'none') frames.push({elapsed:performance.now()-start,text:toast.textContent || '',opacity:Number(style.opacity),offset:parseFloat(style.translate.split(' ')[1] || '0')});
      } while (performance.now() - start < 3600);
      return frames;
    });
    eases(samples.filter(frame => frame.elapsed < 300),'toast opening');
    eases(samples.filter(frame => frame.elapsed > 3150),'toast closing');
    assert.ok(samples.every(frame => frame.text.trim()),'message survives until the exit finishes');
    assert.equal(await page.locator('#toast').isVisible(),false);
  } finally {await browser.close();await server.close();}
});

// Trigger the change and sample layout in the same browser task, so Playwright's
// actionability waits cannot hide the intermediate frames being tested.
async function heights(page: Page, change: Change, checkAuthReveal = false): Promise<number[]> {
  const result = await page.evaluate(async ({selector,value,checkAuthReveal}) => {
    const form = document.querySelector<HTMLElement>('.llm-form')!;
    const samples = [form.getBoundingClientRect().height], start = performance.now();
    let authOverlap = 0;
    const target = document.querySelector<HTMLElement>(selector)!;
    if (value === undefined) target.click();
    else { (target as HTMLSelectElement).value = value; target.dispatchEvent(new Event('change',{bubbles:true})); }
    do {
      await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
      samples.push(form.getBoundingClientRect().height);
      if (checkAuthReveal) {
        const auth = document.querySelector<HTMLElement>('#llm-auth-mode')?.closest<HTMLElement>('.llm-field');
        const protocol = document.querySelector<HTMLElement>('.llm-protocol');
        if (auth && protocol && Number(getComputedStyle(auth).opacity) > .01) {
          const box = auth.getBoundingClientRect();
          const insets = getComputedStyle(auth).clipPath.match(/^inset\(([^)]+)\)/)?.[1]?.trim().split(/\s+/);
          const bottom = insets?.[2] || insets?.[0] || '0';
          const clipped = bottom.endsWith('%') ? parseFloat(bottom) * box.height / 100 : parseFloat(bottom);
          authOverlap = Math.max(authOverlap,box.bottom - clipped - protocol.getBoundingClientRect().top);
        }
      }
    } while (performance.now() - start < 500);
    return {samples,authOverlap};
  },{...change,checkAuthReveal});
  assert.ok(result.authOverlap <= 1,`the revealed auth row must stay above the moving protocol field (${result.authOverlap.toFixed(1)}px overlap)`);
  return result.samples;
}

function interpolates(samples: number[], label: string) {
  const first = samples[0]!, last = samples.at(-1)!;
  assert.ok(Math.abs(last - first) > 4,`${label} changes the form height`);
  const low = Math.min(first,last), high = Math.max(first,last);
  const middle = samples.filter(height => height > low + .5 && height < high - .5);
  assert.ok(new Set(middle.map(height => height.toFixed(1))).size >= 2,`${label} includes intermediate rendered heights instead of a jump`);
}

async function unclipped(page: Page) {
  const layout = await page.locator('.llm-form').evaluate(form => ({
    height:form.getBoundingClientRect().height,
    overflow:form.scrollHeight - form.clientHeight,
    bottom:form.getBoundingClientRect().bottom,
    actionsBottom:form.querySelector('.llm-actions')!.getBoundingClientRect().bottom,
    running:form.getAnimations({subtree:true}).filter(animation => animation.playState === 'running').length,
  }));
  assert.ok(layout.height > 0 && layout.overflow <= 2 && layout.actionsBottom <= layout.bottom,'the settled form contains every field and action');
  assert.equal(layout.running,0,'the form has no unfinished motion');
}

test('provider, subscription, and proxy changes interpolate layout and rapid changes settle cleanly',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1100}}), errors: string[] = [];
    page.on('pageerror',error => errors.push(error.message));
    await mockSettings(page);
    await page.goto(server.url+'/#/llm',{waitUntil:'networkidle'});
    await paint(page);
    interpolates(await heights(page,{selector:'[data-llm-provider="glm"]'},true),'DeepSeek to GLM');
    interpolates(await heights(page,{selector:'#llm-auth-mode',value:'glm_coding_plan'}),'GLM Coding Plan guidance');
    assert.equal(await page.locator('#llm-key').locator('xpath=../..').locator('.llm-field-hint').count(),1,'Coding Plan keeps its dedicated key guidance');
    const compact = await heights(page,{selector:'#llm-auth-mode',value:'api_key'});
    interpolates(compact,'API Key form compaction');
    const shrink = compact[0]! - compact.at(-1)!;
    assert.ok(shrink > 15 && shrink < 40,`removing the one-line key guidance naturally shrinks the form (${shrink.toFixed(1)}px)`);
    assert.equal(await page.locator('#llm-key').locator('xpath=../..').locator('.llm-field-hint').count(),0,'regular API Key has no hint or reserved hint row');
    interpolates(await heights(page,{selector:'#llm-connection-mode',value:'proxy'}),'proxy expansion');
    interpolates(await heights(page,{selector:'#llm-connection-mode',value:'direct'}),'proxy collapse');
    await page.locator('[data-llm-provider="kimi"]').click();
    await paint(page);
    await heights(page,{selector:'#llm-auth-mode',value:'kimi_oauth'});
    assert.equal(await page.locator('.llm-login-start').isVisible(),true);
    assert.equal(await page.locator('#llm-key').count(),0);
    await unclipped(page);

    await page.evaluate(async () => {
      const choose = (selector: string,value?: string) => {
        const target = document.querySelector<HTMLElement>(selector)!;
        if (value === undefined) target.click();
        else { (target as HTMLSelectElement).value = value; target.dispatchEvent(new Event('change',{bubbles:true})); }
      };
      for (const provider of ['glm','deepseek','kimi']) {
        choose(`[data-llm-provider="${provider}"]`);
        await new Promise(resolve => setTimeout(resolve,25));
      }
      choose('#llm-auth-mode','api_key');
      await new Promise(resolve => setTimeout(resolve,25));
      choose('#llm-auth-mode','kimi_oauth');
      choose('#llm-connection-mode','proxy');
      await new Promise(resolve => setTimeout(resolve,500));
    });
    assert.equal(await page.locator('#llm-auth-mode').inputValue(),'kimi_oauth');
    assert.equal(await page.locator('.llm-login-start').isVisible(),true);
    assert.equal(await page.locator('#llm-key').count(),0);
    assert.equal(await page.locator('.llm-proxy-fields').isVisible(),true);
    assert.equal(await page.locator('#llm-effort').inputValue(),'max');
    await unclipped(page);

    await page.locator('.llm-back').click();
    await page.locator('.project-page').waitFor();
    await page.locator('#model-entry').click();
    await page.locator('.llm-test').waitFor();
    await page.waitForFunction(() => document.querySelector<HTMLButtonElement>('.llm-test')?.disabled === false);
    assert.equal(await page.locator('.llm-page').count(),1,'returning to models leaves one active page');
    assert.deepEqual(errors,[]);
  } finally {await browser.close();await server.close();}
});

test('reduced motion settles model and workspace layout and dismisses dialogs without an animation delay',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1100},reducedMotion:'reduce'});
    await mockSettings(page);
    await page.goto(server.url+'/#/llm',{waitUntil:'networkidle'});
    const layout = await page.evaluate(async () => {
      document.querySelector<HTMLButtonElement>('[data-llm-provider="glm"]')!.click();
      const connection = document.querySelector<HTMLSelectElement>('#llm-connection-mode')!;
      connection.value = 'proxy'; connection.dispatchEvent(new Event('change',{bubbles:true}));
      await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
      const form = document.querySelector<HTMLElement>('.llm-form')!;
      return {auth:!!document.querySelector('#llm-auth-mode'),proxy:!document.querySelector<HTMLElement>('.llm-proxy-fields')!.hidden,running:form.getAnimations({subtree:true}).filter(animation => animation.playState === 'running').length};
    });
    assert.deepEqual(layout,{auth:true,proxy:true,running:0});
    await unclipped(page);
    await page.locator('.llm-back').click();
    await page.locator('[data-create]').first().click();
    const closed = await page.evaluate(() => {
      document.querySelector<HTMLButtonElement>('#create-dialog [data-close]')!.click();
      return !document.querySelector<HTMLDialogElement>('#create-dialog')!.open;
    });
    assert.equal(closed,true,'reduced motion closes immediately');
    const fixture = workspaceFixture();
    await interceptProject(page,fixture);
    await page.goto(server.url+'/#project/'+fixture.state.graph.project.id,{waitUntil:'networkidle'});
    await page.locator('[data-node-key="fact:evidence"]').waitFor();
    for (const view of ['materials','result','graph']) {
      const panel = await page.evaluate(async view => {
        document.querySelector<HTMLButtonElement>('#view-tab-'+view)!.click();
        await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
        const element = document.querySelector<HTMLElement>('#project-view')!, style = getComputedStyle(element);
        return {label:element.getAttribute('aria-labelledby'),inert:element.inert,motion:element.dataset.viewMotion || '',opacity:Number(style.opacity),offset:new DOMMatrixReadOnly(style.transform).m42,running:element.getAnimations().filter(animation => animation.playState === 'running').length};
      },view);
      assert.deepEqual(panel,{label:'view-tab-'+view,inert:false,motion:'',opacity:1,offset:0,running:0},'reduced motion switches workspace content by the next paint');
    }
  } finally {await browser.close();await server.close();}
});

test('dialogs ease in and out, restore focus on Escape, and stay open while submitting',options,async () => {
  const server = await staticServer(), browser = await launch();
  let release = () => {};
  const pending = new Promise<void>(resolve => {release = resolve;});
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1100}});
    await mockSettings(page);
    await page.route('**/projects',async route => {
      if (route.request().method() === 'POST') {await pending;return route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({detail:'Controlled submission failure'})});}
      return route.fulfill({contentType:'application/json',body:'[]'});
    });
    await page.goto(server.url,{waitUntil:'networkidle'});
    const sampleDialog = (close: boolean) => page.evaluate(async close => {
      const dialog = document.querySelector<HTMLDialogElement>('#create-dialog')!;
      const trigger = document.querySelector<HTMLButtonElement>(close ? '#create-dialog [data-close]' : '[data-create]')!;
      trigger.focus(); trigger.click();
      const samples: {open: boolean; opacity: number}[] = [], start = performance.now();
      do {
        await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
        samples.push({open:dialog.open,opacity:Number(getComputedStyle(dialog).opacity)});
      } while (performance.now() - start < 400);
      return samples;
    },close);
    const entering = await sampleDialog(false);
    assert.ok(entering.some(frame => frame.open && frame.opacity > 0 && frame.opacity < .99),'opening includes a partially visible dialog');
    assert.equal(entering.at(-1)!.opacity,1);
    const leaving = await sampleDialog(true);
    assert.ok(leaving.some(frame => frame.open && frame.opacity > 0 && frame.opacity < .99),'closing remains modal during the fade');
    assert.equal(leaving.at(-1)!.open,false);

    const opener = page.locator('[data-create]').first();
    await opener.click();
    await page.keyboard.press('Escape');
    await page.locator('#create-dialog').waitFor({state:'hidden'});
    assert.equal(await opener.evaluate(element => document.activeElement === element),true,'Escape returns keyboard focus to the opener');

    await opener.click();
    await page.locator('#project-name').fill('Motion regression');
    await page.locator('#project-target').fill('Controlled local fixture');
    await page.locator('#project-goal').fill('Verify dialog lifecycle');
    const submitted = page.waitForRequest(request => request.method() === 'POST' && new URL(request.url()).pathname === '/projects');
    await page.locator('#create-form [type="submit"]').click(); await submitted;
    await page.keyboard.press('Escape');
    assert.equal(await page.locator('#create-dialog').evaluate((dialog: HTMLDialogElement) => dialog.open),true);
    assert.equal(await page.locator('#create-dialog [data-close]').first().isDisabled(),true);
    release();
    await page.waitForFunction(() => document.querySelector<HTMLFormElement>('#create-form')?.getAttribute('aria-busy') === 'false');
    await page.keyboard.press('Escape');
    await page.locator('#create-dialog').waitFor({state:'hidden'});
    assert.equal(await opener.evaluate(element => document.activeElement === element),true);
  } finally {release();await browser.close();await server.close();}
});

test('workspace views ease between canvas, materials and results and rapid changes preserve canvas interaction',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}}), errors: string[] = [];
    page.setDefaultTimeout(5000); page.on('pageerror',error => errors.push(error.message));
    const fixture = workspaceFixture(), requests = await interceptProject(page,fixture);
    await page.goto(server.url+'/#project/'+fixture.state.graph.project.id,{waitUntil:'networkidle'});
    await page.locator('[data-node-key="fact:evidence"]').waitFor(); await paint(page);
    await page.locator('[data-node-key="fact:evidence"]').focus();
    await page.keyboard.press('ArrowRight'); await page.keyboard.press('ArrowDown');
    await page.locator('#zoom-in').click(); await paint(page);
    const canvas = () => page.evaluate(() => ({
      camera:document.querySelector<HTMLElement>('.graph-world')!.style.transform,
      positions:[...document.querySelectorAll<HTMLElement>('.graph-node')].map(node => [node.dataset.nodeKey,node.style.left,node.style.top]),
    }));
    const arranged = await canvas();
    for (const view of ['materials','result','graph']) {
      const frames = await page.evaluate(async view => {
        document.querySelector<HTMLButtonElement>('#view-tab-'+view)!.click();
        const samples: MotionFrame[] = [], start = performance.now();
        do {
          await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
          const panel = document.querySelector<HTMLElement>('#project-view')!;
          const style = getComputedStyle(panel);
          samples.push({opacity:Number(style.opacity),offset:new DOMMatrixReadOnly(style.transform).m42});
        } while (performance.now() - start < 500);
        return samples;
      },view);
      eases(frames,view+' view');
      assert.equal(frames.at(-1)!.opacity,1); assert.equal(frames.at(-1)!.offset,0);
      assert.equal(await page.locator('#view-tab-'+view).getAttribute('aria-selected'),'true');
    }
    assert.deepEqual(await canvas(),arranged,'tab transitions preserve the manually arranged canvas and camera');
    await page.evaluate(async () => {
      for (const view of ['materials','result','graph','materials','graph','result','graph']) {
        document.querySelector<HTMLButtonElement>('#view-tab-'+view)!.click();
        await new Promise(resolve => setTimeout(resolve,20));
      }
      await new Promise(resolve => setTimeout(resolve,500));
    });
    assert.equal(await page.locator('#project-view').count(),1,'only one settled panel remains after rapid changes');
    assert.equal(await page.locator('#view-tab-graph').getAttribute('aria-selected'),'true');
    assert.deepEqual(await canvas(),arranged);
    await page.locator('#zoom-out').click(); await paint(page);
    assert.notEqual((await canvas()).camera,arranged.camera,'the restored graph still responds to zoom controls');
    await page.locator('#view-tab-result').click();
    await page.locator('[data-focus="finding:verified"]').click();
    await page.waitForFunction(() => document.querySelector('[data-node-key="finding:verified"]')?.getAttribute('aria-pressed') === 'true');
    assert.equal(await page.locator('#view-tab-graph').getAttribute('aria-selected'),'true');
    await page.locator('#view-tab-result').click();
    await page.locator('[data-focus="finding:verified"]').waitFor();
    await page.evaluate(async () => {
      document.querySelector<HTMLButtonElement>('[data-focus="finding:verified"]')!.click();
      await new Promise(resolve => setTimeout(resolve,20));
      document.querySelector<HTMLButtonElement>('#view-tab-materials')!.click();
      await new Promise(resolve => setTimeout(resolve,500));
    });
    assert.equal(await page.locator('#view-tab-materials').getAttribute('aria-selected'),'true','a newer tab click supersedes pending result location');
    assert.equal(await page.locator('#project-view').getAttribute('aria-labelledby'),'view-tab-materials');
    assert.equal(await page.locator('#view-tab-materials').evaluate(element => document.activeElement === element),true,'superseded location cannot steal keyboard focus');
    assert.equal(await page.locator('#clear-selection').count(),0);
    assert.equal(await page.locator('.material-row').count(),1);
    assert.deepEqual(requests.writes,[]); assert.deepEqual(requests.unexpected,[]); assert.deepEqual(errors,[]);
  } finally {await browser.close();await server.close();}
});

test('log, material and result readers ease with their backdrop and rapid reopen never strands the modal',options,async () => {
  const server = await staticServer(), browser = await launch();
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1000}}), errors: string[] = [];
    page.setDefaultTimeout(5000); page.on('pageerror',error => errors.push(error.message));
    const fixture = workspaceFixture(), requests = await interceptProject(page,fixture);
    await page.goto(server.url+'/#project/'+fixture.state.graph.project.id,{waitUntil:'networkidle'});
    await page.locator('[data-node-key="fact:evidence"]').waitFor(); await paint(page);
    const log = '.timeline-entry[data-record="state:fact:evidence"] .log-title';
    const sample = (selector: string) => page.evaluate(async selector => {
      const opener = document.querySelector<HTMLElement>(selector)!;
      opener.focus(); opener.click();
      const samples: (MotionFrame & {open: boolean; backdrop: number})[] = [], start = performance.now();
      do {
        await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
        const dialog = document.querySelector<HTMLDialogElement>('.inspector-reader')!;
        const style = getComputedStyle(dialog);
        samples.push({open:dialog.open,opacity:Number(style.opacity),offset:new DOMMatrixReadOnly(style.transform).m42,backdrop:Number(getComputedStyle(dialog,'::backdrop').opacity)});
      } while (performance.now() - start < 400);
      return samples;
    },selector);
    for (const [view,opener] of [['graph',log],['materials','[data-material="0"]'],['result','[data-content-read="result:verified"]']] as const) {
      await page.locator('#view-tab-'+view).click();
      await page.locator(opener).waitFor();
      const entering = await sample(opener);
      eases(entering,view+' reader opening');
      assert.equal(entering.at(-1)!.open,true); assert.equal(entering.at(-1)!.opacity,1);
      const backdrop = entering.at(-1)!.backdrop;
      assert.ok(entering.some(frame => frame.backdrop > .01 && frame.backdrop < backdrop - .01),'reader backdrop fades in with the panel');
      const leaving = await sample('.inspector-reader[open] [data-inspector-action="close"]');
      eases(leaving.filter(frame => frame.open),view+' reader closing');
      assert.ok(leaving.some(frame => frame.open && frame.backdrop < backdrop - .01),'reader backdrop fades out before closing');
      assert.equal(leaving.at(-1)!.open,false);
      assert.equal(await page.locator(opener).evaluate(element => document.activeElement === element),true,'reader closure restores opener focus');
    }
    await page.locator('#view-tab-graph').click();
    await page.locator(log).click();
    const backdropJumps = await page.evaluate(async selector => {
      const dialog = document.querySelector<HTMLDialogElement>('.inspector-reader')!;
      const opacity = () => Number(getComputedStyle(dialog,'::backdrop').opacity);
      const jumps: number[] = [];
      for (let index = 0; index < 4; index++) {
        const beforeClose = opacity();
        document.querySelector<HTMLButtonElement>('.inspector-reader[open] [data-inspector-action="close"]')!.click();
        jumps.push(Math.abs(opacity() - beforeClose));
        await new Promise(resolve => setTimeout(resolve,25));
        const beforeOpen = opacity();
        document.querySelector<HTMLElement>(selector)!.click();
        await Promise.resolve(); await Promise.resolve();
        jumps.push(Math.abs(opacity() - beforeOpen));
        await new Promise(resolve => setTimeout(resolve,25));
      }
      await new Promise(resolve => setTimeout(resolve,400));
      return jumps;
    },log);
    assert.ok(backdropJumps.every(jump => jump < .08),`backdrop reversal preserves its rendered opacity: ${backdropJumps.join(', ')}`);
    assert.equal(await page.locator('.inspector-reader[open]').count(),1);
    assert.deepEqual(await page.locator('.inspector-reader[open]').evaluate((dialog: HTMLDialogElement) => ({inert:dialog.inert,running:dialog.getAnimations().filter(animation => animation.playState === 'running').length})),{inert:false,running:0});
    await page.keyboard.press('Escape');
    await page.locator('.inspector-reader[open]').waitFor({state:'hidden'});
    assert.equal(await page.locator('dialog[open]').count(),0,'rapid reopening leaves no modal backdrop behind');
    await page.locator('#zoom-in').click();
    assert.deepEqual(requests.writes,[]); assert.deepEqual(requests.unexpected,[]); assert.deepEqual(errors,[]);
  } finally {await browser.close();await server.close();}
});
