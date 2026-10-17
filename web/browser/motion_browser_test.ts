import test from 'node:test';
import assert from 'node:assert/strict';
import type {Page} from 'playwright';
import {launch, paint, staticServer} from './visual_fixture.js';

const options = {timeout:30000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for browser motion regression'};
const settings = {protocol:'anthropic',auth_mode:'api_key',base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',has_token:false,connection_mode:'direct',proxy_url:'',reasoning_effort:'max'};
type Change = {selector: string; value?: string};

async function mockSettings(page: Page) {
  await page.route('**/model-settings',route => route.fulfill({contentType:'application/json',body:JSON.stringify(settings)}));
  await page.route('**/projects',route => route.fulfill({contentType:'application/json',body:'[]'}));
}

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
    interpolates(await heights(page,{selector:'#llm-connection-mode',value:'proxy'}),'proxy expansion');
    interpolates(await heights(page,{selector:'#llm-connection-mode',value:'direct'}),'proxy collapse');
    await page.locator('[data-llm-provider="kimi"]').click();
    await paint(page);
    interpolates(await heights(page,{selector:'#llm-auth-mode',value:'kimi_oauth'}),'Kimi subscription');

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

test('reduced motion settles model layout and dismisses dialogs without an animation delay',options,async () => {
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
