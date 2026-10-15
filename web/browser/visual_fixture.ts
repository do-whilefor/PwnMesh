import http from 'node:http';
import fs from 'node:fs/promises';
import path from 'node:path';
import {createRequire} from 'node:module';
import {fileURLToPath} from 'node:url';
import type {Page} from 'playwright';
import type {WorkspaceSnapshot, ProjectDTO, InputDTO, ExecutionDTO} from '../src/api/types.js';

export interface FixtureState extends WorkspaceSnapshot {
  graph: WorkspaceSnapshot['graph'] & {facts: NonNullable<WorkspaceSnapshot['graph']['facts']>; intents: NonNullable<WorkspaceSnapshot['graph']['intents']>; hints: NonNullable<WorkspaceSnapshot['graph']['hints']>};
  fact_relations: NonNullable<WorkspaceSnapshot['fact_relations']>;
}
export interface ProjectFixture {state: FixtureState; runs?: ExecutionDTO[]; inputs?: InputDTO[]; contents?: Record<string, string | Buffer>}
const require = createRequire(import.meta.url);

export async function launch() {
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright') as typeof import('playwright');
  return chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
}
export const projectPath = (id: string) => '/projects/' + encodeURIComponent(id);
export const fillCreate = async (page: Page,title: string,type='pentest') => {
  await page.locator('[data-create]').first().click();
  await page.locator('#project-name').fill(title);
  await page.locator('#project-target').fill('https://authorized.example.test');
  await page.locator('#project-goal').fill('核对授权边界并记录真实证据');
  await page.locator('input[name="type"][value="'+type+'"]').check();
};
export const stateFor = (p: ProjectDTO): FixtureState => ({graph:{project:p,facts:[{id:'origin',description:'https://authorized.example.test'},{id:'goal',description:'核对授权边界并记录真实证据'}],intents:[],hints:[]},revision:1,goals:[{id:'goal',condition:'核对授权边界并记录真实证据',status:'open'}],steps:[],fact_records:[{id:'origin',description:'https://authorized.example.test',status:'input'}],findings:[],fact_relations:[]});

// Serve the real bundle. API fixtures remain browser-local so this helper can
// also test a populated live service.
export async function staticServer() {
  const root = fileURLToPath(new URL('../../dist', import.meta.url));
  const contentTypes: Record<string, string> = {'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.png':'image/png','.woff2':'font/woff2'};
  const server = http.createServer(async (req,res) => {
    try {
      const pathname = decodeURIComponent(new URL(req.url || '/', 'http://local').pathname);
      const relative = pathname === '/' ? 'index.html' : pathname.startsWith('/static/') ? pathname.slice(8) : null;
      if (relative === null) throw new Error('Unknown route');
      const file = path.resolve(root,relative);
      if (!file.startsWith(root + path.sep)) throw new Error('Invalid path');
      const body = await fs.readFile(file);
      res.writeHead(200,{'Content-Type':(contentTypes[path.extname(file)]) || 'application/octet-stream'});
      res.end(body);
    } catch { res.writeHead(404);res.end(); }
  });
  await new Promise<void>(resolve => server.listen(0,'127.0.0.1',resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Static fixture has no TCP address');
  return {url:'http://127.0.0.1:' + address.port,close:() => new Promise<void>((resolve,reject) => server.close(error => error ? reject(error) : resolve()))};
}

const modelSettings = {protocol:'anthropic',base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',has_token:false,connection_mode:'direct',proxy_url:'http://host.docker.internal:7897',max_tokens:32768,request_timeout:180,reasoning_effort:'high'};
export async function interceptProject(page: Page,fixture: ProjectFixture,{unavailable = () => false}: {unavailable?: () => boolean} = {}) {
  const writes: string[]=[],reads: string[]=[],unexpected: string[]=[];
  await page.route('**/model-settings',route => route.fulfill({contentType:'application/json',body:JSON.stringify(modelSettings)}));
  await page.route('**/projects**',async route => {
    const request=route.request(),pathname=new URL(request.url()).pathname;
    if(request.method()!=='GET') {writes.push(request.method()+' '+pathname);return route.fulfill({status:405,contentType:'application/json',body:'{}'});}
    reads.push(pathname);
    const {state,runs=[],inputs=[]}=fixture, project=state.graph.project, base='/projects/'+project.id;
    let body: unknown;
    if(pathname==='/projects')body=[project];
    else if(pathname.startsWith(base+'/') && project.orchestration_version!==1)return route.fulfill({status:409,contentType:'application/json',body:JSON.stringify({detail:'Unsupported project protocol; create a new project to continue'})});
    else if(pathname===base+'/state') {if(unavailable())return route.abort('failed');body=state;}
    else if(pathname===base+'/state/events')body=[];
    else if(pathname===base+'/executions')body={items:runs,through:runs.length};
    else if(pathname===base+'/executions/changes')body={items:runs,deleted:[],through:runs.length,generation:project.generation||0};
    else if(pathname===base+'/identity')body={id:project.id,generation:project.generation||0};
    else if(pathname===base+'/inputs')body=inputs;
    else if(pathname===base+'/rounds')body={items:[],next_cursor:null};
    else if(pathname.startsWith(base+'/inputs/') && fixture.contents?.[pathname.slice((base+'/inputs/').length)] !== undefined) return route.fulfill({contentType:'application/octet-stream',body:fixture.contents[pathname.slice((base+'/inputs/').length)]});
    else {unexpected.push(pathname);return route.fulfill({status:404,body:'{}'});}
    return route.fulfill({contentType:'application/json',body:JSON.stringify(body)});
  });
  return {writes,reads,unexpected};
}
export const paint = (page: Page) => page.evaluate(async () => {await document.fonts.ready;await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));});
