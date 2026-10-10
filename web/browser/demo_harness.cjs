'use strict';
const {staticServer:assetServer}=require('./visual_fixture.cjs');
async function launch(){const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');return chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE||undefined});}
const projectPath=id=>'/projects/'+encodeURIComponent(id);
const fillCreate=async(page,title,type='pentest')=>{await page.locator('[data-create]').first().click();await page.locator('#project-name').fill(title);await page.locator('#project-target').fill('https://authorized.example.test');await page.locator('#project-goal').fill('核对授权边界并记录真实证据');await page.locator('input[name="type"][value="'+type+'"]').check();};
const stateFor=p=>({graph:{project:p,facts:[{id:'origin',description:'https://authorized.example.test'},{id:'goal',description:'核对授权边界并记录真实证据'}],intents:[],hints:[]},revision:1,goals:[{id:'goal',condition:'核对授权边界并记录真实证据',status:'open'}],steps:[],fact_records:[{id:'origin',description:'https://authorized.example.test',status:'input'}],findings:[],fact_relations:[]});
module.exports={assetServer,launch,projectPath,fillCreate,stateFor};
