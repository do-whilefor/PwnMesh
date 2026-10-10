'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const {staticServer,interceptProject,paint}=require('./visual_fixture.cjs');

const created='2026-09-24T02:00:00Z';
const boardHead='浏览器折叠验收的黑板记录开头。',boardTail='黑板记录末尾：展开后应完整可见。';
const systemHead='浏览器折叠验收的系统记录开头。',systemTail='系统记录末尾：展开后应完整可见。';
const boardBody=boardHead+'\n'+'这是应折叠显示的详细证据内容。'.repeat(120)+'\n'+boardTail+' <script>literal-only</script>';
const systemBody=systemHead+'\n'+'这是应折叠显示的执行错误详情。'.repeat(120)+'\n'+systemTail;
function fixture(){
  const project={id:'ui-details-fixture',title:'日志与任务筛选浏览器验收',scenario:'ctf',status:'active',orchestration_version:1,generation:0,created_at:created};
  const facts=[{id:'origin',description:'受控的浏览器测试输入',status:'input'},{id:'long',description:boardBody,status:'valid'},{id:'short',description:'简短的证据记录',status:'valid'}].map(fact=>({...fact,created_at:created}));
  const steps=[{id:'done',description:'已经完成的任务',status:'completed',from:['origin'],result:'long',support_valid:true},{id:'running',description:'正在运行的任务',status:'running',from:['long'],depends_on:['done']},{id:'pending',description:'等待执行的任务',status:'blocked',from:['short'],depends_on:['running'],blocked_by:['running']},{id:'failed',description:'失败的任务',status:'failed',from:['origin']}].map(step=>({...step,goal_id:'goal',created_at:created}));
  const text='<script>window.previewExecuted=true</script>\nGET /orders HTTP/1.1';
  return {state:{graph:{project,facts,intents:[],hints:[]},revision:1,goals:[{id:'goal',condition:'验证日志折叠、布局和状态筛选',status:'open',created_at:created}],steps,fact_records:facts,findings:[],fact_relations:[]},
    runs:[{id:'long-system-run',project_id:project.id,generation:0,kind:'explore',intent:'failed',status:'failed',created_at:created,updated_at:created,result:{error:systemBody}}],
    inputs:[{id:'text',name:'request.http',size:Buffer.byteLength(text),path:'/workspace/inputs/request.http',sha256:'a'.repeat(64),created_at:created},{id:'binary',name:'client.apk',size:12,path:'/workspace/inputs/client.apk',sha256:'b'.repeat(64),created_at:created}],contents:{text,binary:Buffer.from([0,255,0])}};
}

test('v5 workbench reads real-shaped logs, materials and results and preserves live canvas interactions',{
  timeout:90000,skip:process.env.PWNMESH_WEB_URL||process.env.PLAYWRIGHT_MODULE?false:'Set PWNMESH_WEB_URL or PLAYWRIGHT_MODULE for workbench browser coverage',
},async t=>{
  const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright'),server=process.env.PWNMESH_WEB_URL?null:await staticServer();let browser;
  try{
    browser=await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE||undefined});
    const page=await browser.newPage({viewport:{width:1440,height:960},reducedMotion:'reduce'});page.setDefaultTimeout(12000);
    const data=fixture(),state=data.state,errors=[],assetErrors=[];let unavailable=false;
    page.on('pageerror',error=>errors.push(error.message));page.on('response',response=>{if(new URL(response.url()).pathname.startsWith('/static/')&&response.status()>=400)assetErrors.push(response.url());});
    const requests=await interceptProject(page,data,{unavailable:()=>unavailable});
    const base=process.env.PWNMESH_WEB_URL||server.url;
    await page.goto(new URL('/#project/'+state.graph.project.id,base).href,{waitUntil:'networkidle'});
    await page.waitForFunction(()=>document.querySelectorAll('#graph-host .graph-node').length===8);
    await page.locator('#add-hint:not(:disabled)').waitFor();await paint(page);
    const board=()=>page.locator('.timeline-entry[data-record="state:fact:long"]');
    const system=()=>page.locator('.timeline-entry[data-record="execution:long-system-run"]');
    const view=()=>page.evaluate(()=>({camera:document.querySelector('#graph-host .graph-world').style.transform,positions:[...document.querySelectorAll('#graph-host .graph-node')].map(node=>[node.dataset.nodeKey,node.style.left,node.style.top])}));
    const closeReader=async()=>{await page.locator('.inspector-reader[open] [data-inspector-action="close"]').click();await page.locator('.inspector-reader[open]').waitFor({state:'hidden'});};

    await t.test('long blackboard and execution logs fold, survive polling and expose safe full-text readers',async()=>{
      for(const [article,head,tail] of [[board(),boardHead,boardTail],[system(),systemHead,systemTail]]){
        assert.equal(await article.locator('details').getAttribute('open'),null);assert.match(await article.innerText(),new RegExp(head));assert.ok(!(await article.innerText()).includes(tail));
        await article.locator('details > summary').click();assert.ok(await article.locator('details').evaluate(node=>node.open));assert.ok((await article.innerText()).includes(tail));
      }
      assert.equal(await page.locator('.timeline-entry[data-record="state:fact:short"] details').count(),0);
      state.revision++;state.graph.hints.push({id:'poll-marker',content:'轮询新增记录',created_at:'2026-09-24T02:01:00Z',creator:'user'});
      await page.locator('.timeline-entry[data-record="hint:poll-marker"]').waitFor();
      assert.ok(await board().locator('details').evaluate(node=>node.open));assert.ok(await system().locator('details').evaluate(node=>node.open));
      await page.locator('#view-tab-result').click();await page.locator('#view-tab-graph').click();
      assert.ok(await board().locator('details').evaluate(node=>node.open));assert.ok(await system().locator('details').evaluate(node=>node.open));
      await page.locator('#activity-collapse-all').click();assert.equal(await page.locator('#activity-content details[open]').count(),0);
      await page.locator('#activity-search').fill(boardTail);await page.waitForFunction(()=>document.querySelectorAll('.timeline-entry').length===1);
      await board().locator('.log-title').click();assert.match(await page.locator('.inspector-reader[open] .inspector-reader-content').textContent(),/<script>literal-only<\/script>/);
      assert.equal(await page.locator('.inspector-reader[open] script').count(),0);await closeReader();await page.locator('#activity-clear-search').click();
    });

    await t.test('legend filters retain card positions and camera and reveal only connected visible edges',async()=>{
      const original=await view();
      for(const [status,count]of[['done',4],['running',1],['pending',2]]){
        const button=page.locator('[data-filter="'+status+'"]');await button.click();await paint(page);
        assert.equal(await button.getAttribute('aria-pressed'),'true');assert.equal(await page.locator('#graph-host .graph-node:visible').count(),count,status);
        const valid=await page.evaluate(()=>{
          const keys=new Set([...document.querySelectorAll('#graph-host .graph-node')].filter(node=>!node.hidden).map(node=>node.dataset.nodeKey));
          return [...document.querySelectorAll('#graph-host .graph-edge-hit')].every(hit=>{const[,source,target]=JSON.parse(hit.dataset.edgeKey.slice(5));return (getComputedStyle(hit.parentElement).display!=='none')===(keys.has(source)&&keys.has(target));});
        });assert.ok(valid);assert.deepEqual(await view(),original);
        await button.click();await paint(page);assert.equal(await page.locator('#graph-host .graph-node:visible').count(),8);assert.deepEqual(await view(),original);
      }
    });

    await t.test('dependency and node inspectors show the current relationship and safe complete evidence',async()=>{
      const edge=page.locator('.graph-edge-hit[aria-label="执行依赖：已经完成的任务 → 正在运行的任务，依赖已满足"]');assert.equal(await edge.count(),1);
      await edge.dispatchEvent('click');assert.match(await page.locator('.activity-pane .node-inspector').innerText(),/依赖已满足/);assert.match(await page.locator('.activity-pane .node-inspector').innerText(),/已经完成的任务/);await page.locator('#clear-selection').click();
      await page.locator('[data-node-key="fact:long"]').dispatchEvent('click');await page.locator('.activity-pane [data-inspector-action="read"]').click();
      assert.ok((await page.locator('.inspector-reader[open] .inspector-reader-content').textContent()).includes(boardTail));await closeReader();await page.locator('#clear-selection').click();
    });

    await t.test('polling errors disable mutations and recovery preserves the displayed graph',async()=>{
      const original=await view();unavailable=true;
      try{await page.waitForFunction(()=>document.querySelector('#add-hint')?.disabled);assert.ok((await page.locator('#toast').innerText()).trim());assert.deepEqual(await view(),original);}
      finally{unavailable=false;}
      await page.locator('#add-hint:not(:disabled)').waitFor();assert.deepEqual(await view(),original);
    });

    await t.test('materials open escaped previews and original downloads without fetching binary content',async()=>{
      await page.locator('#view-tab-materials').click();assert.equal(await page.locator('.material-row').count(),2);
      await page.locator('.material-row').filter({hasText:'request.http'}).click();
      assert.match(await page.locator('.inspector-reader[open] .inspector-reader-content').textContent(),/GET \/orders HTTP\/1.1/);
      assert.equal(await page.evaluate(()=>window.previewExecuted),undefined);assert.equal(await page.locator('.inspector-reader[open] script').count(),0);
      assert.match(await page.locator('.inspector-reader[open] a[download]').getAttribute('href'),/\/inputs\/text$/);await closeReader();
      await page.locator('.material-row').filter({hasText:'client.apk'}).focus();await page.keyboard.press('Enter');
      assert.match(await page.locator('.inspector-reader[open] .inspector-reader-content').textContent(),/二进制文件/);
      assert.equal(requests.reads.includes('/projects/'+state.graph.project.id+'/inputs/binary'),false);await closeReader();
      await page.locator('[data-content-read="input:target"]').click();assert.match(await page.locator('.inspector-reader[open] .inspector-reader-content').textContent(),/受控的浏览器测试输入/);await closeReader();
    });

    await t.test('new findings appear after polling, expose evidence and locate their canvas card',async()=>{
      await page.locator('#view-tab-result').click();assert.match(await page.locator('#project-view').innerText(),/暂无结果/);
      state.findings.push({id:'finding-one',claim:'受控的结果记录',scope:'/orders',status:'verified',sources:['long'],created_at:created});state.revision++;
      await page.locator('.result-card').waitFor();assert.match(await page.locator('.result-card').innerText(),/受控的结果记录/);
      await page.locator('[data-content-read="result:finding-one"]').click();assert.ok((await page.locator('.inspector-reader[open] .inspector-reader-content').textContent()).includes(boardTail));await closeReader();
      await page.locator('[data-focus="finding:finding-one"]').click();assert.equal(await page.locator('[data-node-key="finding:finding-one"]').getAttribute('aria-pressed'),'true');await page.locator('#clear-selection').click();
      await page.locator('#project-manage').click();assert.match(await page.locator('#project-dialog').innerText(),/受控的浏览器测试输入/);assert.equal(await page.locator('#project-dialog [data-action="pause"]').isEnabled(),true);await page.locator('[data-close="project-dialog"]').click();
    });

    await t.test('invalidated completion support updates selected details, card colors and result edges',async()=>{
      await page.locator('[data-node-key="fact:long"]').dispatchEvent('click');
      delete state.steps[0].support_valid;state.fact_records[1].support_invalid=true;state.steps[1].blocked_by=['done'];state.revision++;
      await page.waitForFunction(()=>document.querySelector('[data-node-key="step:done"]')?.classList.contains('invalid'));
      assert.match(await page.locator('.activity-pane .node-inspector').innerText(),/有效 · 支持失效/);
      const completed=page.locator('[data-node-key="step:done"]');assert.match(await completed.getAttribute('aria-label'),/已完成 · 完成结果支持失效/);
      const labels=await page.locator('.graph-edge-hit').evaluateAll(nodes=>nodes.filter(node=>node.dataset.edgeKey==='edge:'+JSON.stringify(['step_result','step:done','fact:long'])).map(node=>node.getAttribute('aria-label')));
      assert.equal(labels.length,1);assert.match(labels[0],/支持已失效$/);
      await completed.dispatchEvent('click');assert.match(await page.locator('.activity-pane .node-inspector').innerText(),/已完成 · 完成结果支持失效/);
      await page.locator('[data-filter="done"]').click();await paint(page);
      assert.equal(await completed.isVisible(),false);assert.equal(await page.locator('[data-node-key="fact:long"]').isVisible(),false);assert.equal(await page.locator('[data-node-key="fact:short"]').isVisible(),true);assert.equal(await page.locator('.activity-pane .node-inspector').count(),0);
    });

    await t.test('historical projects stay readable but hide unavailable mutations and reject an already-open hint form',async()=>{
      await page.locator('#add-hint').click();await page.locator('#hint-text').fill('A draft opened before the project became read-only');
      delete state.graph.project.orchestration_version;state.revision++;
      await page.waitForFunction(()=>document.querySelector('#add-hint')?.disabled);
      await page.locator('#hint-form [type="submit"]').click();assert.match(await page.locator('#hint-error').innerText(),/历史项目只读/);
      assert.equal(await page.locator('#hint-form [type="submit"]').isDisabled(),true);await page.locator('#hint-dialog [data-close="hint-dialog"]').first().click();
      assert.match(await page.locator('.project-heading .badge').innerText(),/历史项目.*只读/);assert.equal(await page.locator('.project-more').isDisabled(),true);
      await page.locator('#project-manage').click();assert.match(await page.locator('#project-dialog').innerText(),/历史项目只读/);assert.equal(await page.locator('#project-dialog [data-action]').count(),0);await page.locator('[data-close="project-dialog"]').click();
      await page.locator('#view-tab-materials').click();assert.equal(await page.locator('#add-materials').count(),0);assert.equal(await page.locator('.material-row').count(),2);
      await page.reload({waitUntil:'networkidle'});await page.locator('.project-heading .badge').filter({hasText:'只读'}).waitFor();assert.equal(await page.locator('#add-hint').isDisabled(),true);assert.equal(await page.locator('.project-more').isDisabled(),true);
    });

    assert.deepEqual(requests.writes,[],'detail checks never mutate the backing service');assert.deepEqual(requests.unexpected,[]);assert.deepEqual(assetErrors,[]);assert.deepEqual(errors,[]);
  }finally{if(browser)await browser.close();if(server)await server.close();}
});
