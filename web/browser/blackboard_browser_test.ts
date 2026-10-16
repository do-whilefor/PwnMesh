import test from 'node:test';
import assert from 'node:assert/strict';
import { interceptProject, launch, stateFor, staticServer } from './visual_fixture.js';

test('blackboard exposes discovery provenance, derivations and retained review state', {
  timeout: 30000, skip: process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for browser coverage',
}, async () => {
  const server = await staticServer();
  let browser;
  try {
    browser = await launch();
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}});
    page.setDefaultTimeout(5000);
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    const state = stateFor({id: 'blackboard', title: 'Blackboard evidence review', status: 'active', orchestration_version: 1, generation: 0});
    state.fact_records.push({id: 'raw', description: 'Response bytes', status: 'refuted'}, {id: 'computed', description: 'Computed observation', status: 'valid', derived_from: ['raw'], support_invalid: true});
    state.steps = [{id: 'inspect', description: 'Inspect endpoint', status: 'completed', from: ['origin'], result: 'computed', support_valid: false}];
    state.assets = [{id: 'a', kind: 'endpoint', value: 'https://example.test/account', method: 'GET'}, {id: 'b', kind: 'endpoint', value: 'https://example.test/profile', method: 'GET'}];
    state.asset_anchors = [{asset_id: 'a', node_kind: 'fact', node_id: 'computed', discovery_step_id: 'inspect'}, {asset_id: 'b', node_kind: 'candidate', node_id: 'note'}];
    state.candidates = [{id: 'note', claim: 'Producer judgment retained for review', status: 'verified', reason: 'Compare the retained observations', sources: ['computed']}];
    state.disputes = [{id: 'question', finding_id: 'finding', candidate_ids: ['note'], status: 'open', question: 'Independent review required', reason: 'Conflicting evidence', review_step_ids: ['review'], review_fact_ids: ['raw']}];
    const requests = await interceptProject(page, {state});
    await page.goto(server.url + '/#project/blackboard', {waitUntil: 'networkidle'});
    await page.locator('[data-node-key="fact:computed"]').dispatchEvent('click');
    assert.match(await page.locator('.activity-pane .node-inspector').innerText(), /Derived from/);
    assert.match(await page.locator('.activity-pane .node-inspector').innerText(), /Discovery via Step inspect/);
    const edges = await page.locator('.graph-edge-hit').evaluateAll(nodes => nodes.map(node => node.getAttribute('data-edge-key')));
    assert.ok(edges.includes('edge:' + JSON.stringify(['fact_derivation', 'fact:raw', 'fact:computed'])));
    await page.locator('#view-tab-blackboard').click();
    await page.getByLabel('Filter by asset').selectOption('a');
    assert.match(await page.locator('[data-record="fact:computed"]').innerText(), /Discovery via Step inspect/);
    assert.equal(await page.locator('[data-candidate="note"]').count(), 0);
    await page.getByLabel('Filter by asset').selectOption('b');
    assert.match(await page.locator('[data-dispute="question"]').innerText(), /Review Facts: raw/);
    state.candidates[0].reason = 'Refreshed retained observation'; state.revision++;
    await page.getByText('Refreshed retained observation', {exact: true}).waitFor();
    assert.equal(await page.getByLabel('Filter by asset').inputValue(), 'b');
    await page.locator('[data-candidate="note"] .record-title').click();
    assert.match(await page.locator('.inspector-reader[open]').innerText(), /Producer judgment/);
    assert.equal(await page.locator('.inspector-reader[open] [data-inspector-action="locate"]').count(), 0);
    await page.locator('.inspector-reader[open] [data-inspector-action="close"]').click();
    await page.locator('.inspector-reader[open]').waitFor({state: 'hidden'});
    await page.getByLabel('Filter by asset').selectOption('');
    if (process.env.PWNMESH_VISUAL_OUTPUT) await page.screenshot({path: process.env.PWNMESH_VISUAL_OUTPUT, fullPage: true});
    assert.deepEqual(requests.writes, []); assert.deepEqual(requests.unexpected, []); assert.deepEqual(errors, []);
  } finally { await browser?.close(); await server.close(); }
});
