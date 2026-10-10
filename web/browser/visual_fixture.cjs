'use strict';
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');

// Serve the real bundle. API fixtures remain browser-local so this helper can
// also test a populated live service.
async function staticServer() {
  const root = path.resolve(__dirname,'../static');
  const server = http.createServer(async (req,res) => {
    try {
      const pathname = decodeURIComponent(new URL(req.url,'http://local').pathname);
      const relative = pathname === '/' ? 'index.html' : pathname.startsWith('/static/') ? pathname.slice(8) : null;
      if (relative === null) throw new Error('Unknown route');
      const file = path.resolve(root,relative);
      if (!file.startsWith(root + path.sep)) throw new Error('Invalid path');
      const body = await fs.readFile(file);
      res.writeHead(200,{'Content-Type':({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.png':'image/png','.woff2':'font/woff2'})[path.extname(file)] || 'application/octet-stream'});
      res.end(body);
    } catch { res.writeHead(404);res.end(); }
  });
  await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
  return {url:'http://127.0.0.1:' + server.address().port,close:() => new Promise(resolve => server.close(resolve))};
}

const modelSettings = {protocol:'anthropic',base_url:'https://api.deepseek.com/anthropic',model:'deepseek-flash',has_token:false,connection_mode:'direct',proxy_url:'http://host.docker.internal:7897',max_tokens:32768,request_timeout:180,reasoning_effort:'high'};
async function interceptProject(page,fixture,{unavailable = () => false} = {}) {
  const writes=[],reads=[],unexpected=[];
  await page.route('**/model-settings',route => route.fulfill({contentType:'application/json',body:JSON.stringify(modelSettings)}));
  await page.route('**/projects**',async route => {
    const request=route.request(),pathname=new URL(request.url()).pathname;
    if(request.method()!=='GET') {writes.push(request.method()+' '+pathname);return route.fulfill({status:405,contentType:'application/json',body:'{}'});}
    reads.push(pathname);
    const {state,runs=[],inputs=[]}=fixture, project=state.graph.project, base='/projects/'+project.id;
    let body;
    if(pathname==='/projects')body=[project];
    else if(pathname===base+'/state') {if(unavailable())return route.abort('failed');body=state;}
    else if(pathname===base+'/state/events')body=[];
    else if(pathname===base+'/executions')body={items:runs,through:runs.length};
    else if(pathname===base+'/identity')body={id:project.id,generation:project.generation||0};
    else if(pathname===base+'/inputs')body=inputs;
    else if(pathname===base+'/rounds')body={items:[],next_cursor:null};
    else if(pathname.startsWith(base+'/inputs/') && fixture.contents?.[pathname.slice((base+'/inputs/').length)] !== undefined) return route.fulfill({contentType:'application/octet-stream',body:fixture.contents[pathname.slice((base+'/inputs/').length)]});
    else {unexpected.push(pathname);return route.fulfill({status:404,body:'{}'});}
    return route.fulfill({contentType:'application/json',body:JSON.stringify(body)});
  });
  return {writes,reads,unexpected};
}
const paint = page => page.evaluate(async () => {await document.fonts.ready;await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));});
module.exports={staticServer,interceptProject,paint};
