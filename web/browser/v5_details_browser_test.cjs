'use strict';
const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs/promises');
const path=require('node:path');
const {staticServer,interceptProject,paint}=require('./visual_fixture.cjs');

const properties=['display','position','fontFamily','fontSize','fontWeight','lineHeight','letterSpacing','color','backgroundColor','borderTopColor','borderTopWidth','borderRadius','paddingTop','paddingRight','paddingBottom','paddingLeft','gap'];
async function presentation(page,selectors) {
  return page.evaluate(({selectors,properties}) => Object.fromEntries(selectors.map(selector => {
    const element=document.querySelector(selector);if(!element)throw new Error('Missing visual reference: '+selector);
    const css=getComputedStyle(element),r=element.getBoundingClientRect();
    return [selector,{style:Object.fromEntries(properties.map(key=>[key,css[key]])),box:{x:r.x,y:r.y,width:r.width,height:r.height}}];
  })),{selectors,properties});
}
function compare(actual,reference,dimensions=['x','y','width','height']) {
  for(const [selector,expected] of Object.entries(reference)) {
    assert.deepEqual(actual[selector].style,expected.style,selector+' computed styles match the accepted v5 Demo');
    for(const key of dimensions) assert.ok(Math.abs(actual[selector].box[key]-expected.box[key])<=1,`${selector} ${key}: ${actual[selector].box[key]} vs Demo ${expected.box[key]}`);
  }
}
const workspace=['.sidebar','.brand-art','.create-button','.project-search','.project-heading','.title-row h1','.project-description','.project-body','.canvas-panel','.canvas-header','.view-tabs','.graph-stage','.canvas-footer','.activity-pane','.activity-heading','#activity-toolbar','.activity-search','#activity-search','.zoom-controls'];
const llm=['.llm-page','.llm-heading','.llm-heading h1','.llm-layout','.llm-form','.llm-providers','.llm-provider','.llm-connection','#llm-protocol','#llm-url','#llm-model','#llm-key','#llm-effort','#llm-connection-mode','.llm-aside','.llm-overview','.llm-diagram','.llm-test'];

test('production matches the actual v5 Demo at 1440 and 1920 pixels, including model direct and proxy modes', {
  timeout:90000,skip:process.env.PLAYWRIGHT_MODULE ? false : 'Set PLAYWRIGHT_MODULE for v5 visual browser coverage',
},async t => {
  const source=path.resolve(__dirname,'../../tmp/pwn-web-demo/v5/index.html');
  try{await fs.access(source);}catch{return t.skip('Optional v5 reference is not available; fixed visual-asset hashes still run in web/visual_assets_test.cjs');}
  const {chromium}=require(process.env.PLAYWRIGHT_MODULE),server=await staticServer();let browser;
  try {
    browser=await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE||undefined});
    const context=await browser.newContext({viewport:{width:1440,height:960},deviceScaleFactor:1,reducedMotion:'reduce'});
    const page=await context.newPage(),reference=await context.newPage(),errors=[],assetErrors=[];
    for(const current of [page,reference]){current.on('pageerror',error=>errors.push(error.message));current.on('response',response=>{if(response.status()>=400)assetErrors.push(response.url());});}
    const project={id:'atlas',title:'Atlas 平台安全评估',scenario:'pentest',status:'active',orchestration_version:1,generation:0,created_at:'2026-10-09T01:24:00Z'};
    const facts=[{id:'origin',description:'atlas.example.com',status:'input',created_at:project.created_at},{id:'goal',description:'验证核心业务接口的访问控制，追溯每一项发现的证据。',status:'input',created_at:project.created_at}];
    const fixture={state:{graph:{project,facts,intents:[],hints:[]},revision:1,goals:[{id:'goal',condition:facts[1].description,status:'open'}],steps:[],fact_records:facts,findings:[],fact_relations:[]}};
    const requests=await interceptProject(page,fixture);
    await Promise.all([page.goto(server.url+'/#project/atlas',{waitUntil:'networkidle'}),reference.goto(server.url+'/reference/v5/index.html#project/atlas',{waitUntil:'networkidle'})]);
    await page.locator('.canvas-panel').waitFor();await reference.locator('.canvas-panel').waitFor();
    const screenshots=path.resolve(__dirname,'../../tmp/v5-visual-comparison');await fs.mkdir(screenshots,{recursive:true});
    for(const width of [1440,1920]){
      await t.test('workspace '+width,async()=>{
        await Promise.all([page.setViewportSize({width,height:960}),reference.setViewportSize({width,height:960})]);await Promise.all([paint(page),paint(reference)]);
        compare(await presentation(page,workspace),await presentation(reference,workspace));
        const shapes=current=>current.locator('.create-button svg,.project-search svg,.view-tabs svg,.zoom-controls svg').evaluateAll(nodes=>nodes.map(node=>({viewBox:node.getAttribute('viewBox'),stroke:node.getAttribute('stroke-width'),shape:node.innerHTML})));
        assert.deepEqual(await shapes(page),await shapes(reference),'original Lucide icon geometry is preserved');
        assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
        await Promise.all([page.screenshot({path:path.join(screenshots,'production-workspace-'+width+'.png')}),reference.screenshot({path:path.join(screenshots,'demo-workspace-'+width+'.png')})]);
      });
    }
    await Promise.all([page.locator('#model-entry').click(),reference.locator('#model-entry').click()]);
    await page.locator('.llm-test:not(:disabled)').waitFor();
    for(const width of [1440,1920])for(const mode of ['direct','proxy']){
      await t.test('models '+mode+' '+width,async()=>{
        await Promise.all([page.setViewportSize({width,height:960}),reference.setViewportSize({width,height:960})]);
        await Promise.all([page.locator('#llm-connection-mode').selectOption(mode),reference.locator('#llm-connection-mode').selectOption(mode)]);
        await Promise.all([paint(page),paint(reference)]);
        const selectors=mode==='proxy'?llm.concat(['.llm-proxy-fields','#llm-proxy-protocol','#llm-proxy-host','#llm-proxy-port']):llm;
        // Service-specific key/proxy explanations change text wrapping and total
        // height. Columns, type, colors and control dimensions remain exact.
        compare(await presentation(page,selectors),await presentation(reference,selectors),['x','width']);
        const controls=selectors.filter(selector=>selector.startsWith('#'));
        const actual=await presentation(page,controls),expected=await presentation(reference,controls);
        for(const selector of controls)assert.equal(actual[selector].box.height,expected[selector].box.height,selector+' height');
        assert.equal(await page.locator('.llm-proxy-fields').isVisible(),mode==='proxy');
        if(mode==='proxy'){assert.equal(await page.locator('#llm-proxy-host').inputValue(),'host.docker.internal');assert.equal(await page.locator('#llm-proxy-port').inputValue(),'7897');}
        const providerImages=current=>current.locator('.llm-provider img').evaluateAll(nodes=>nodes.map(node=>({width:node.width,height:node.height,loaded:node.complete&&node.naturalWidth>0})));
        assert.deepEqual(await providerImages(page),await providerImages(reference));
        await Promise.all([page.screenshot({path:path.join(screenshots,'production-models-'+mode+'-'+width+'.png')}),reference.screenshot({path:path.join(screenshots,'demo-models-'+mode+'-'+width+'.png')})]);
      });
    }
    assert.deepEqual(requests.writes,[]);assert.deepEqual(requests.unexpected,[]);assert.deepEqual(errors,[]);assert.deepEqual(assetErrors,[]);
  }finally{if(browser)await browser.close();await server.close();}
});
