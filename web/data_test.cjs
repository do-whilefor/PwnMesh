const test = require('node:test');
const assert = require('node:assert/strict');
const data = require('./static/data.js');

function fixture(status = 'active') {
  return {revision:4,graph:{project:{id:'p',status,generation:1,restarted_at:'2026-09-24T00:00:00Z'},
    intents:[{id:'done',to:'goal',from:['f'],description:'已核验目标',concluded_at:'2026-09-24T00:05:00Z'}]},
    goals:[{id:'goal',status:'achieved',sources:['f'],support_valid:true}],
    fact_records:[{id:'f',status:'valid'}],steps:[],
    findings:[{id:'finding',claim:'已验证发现',status:'verified',sources:['f'],support_valid:true}]};
}

test('Web creation selects orchestration for all scenarios and omits obsolete bootstrap options', () => {
  for (const scenario of ['ctf','pentest','audit']) {
    assert.deepEqual(data.validateProject({title:' 项目 ',origin:'输入',goal:'目标',scenario,bootstrap_enabled:true,orchestration_version:0}),
      {title:'项目',origin:'输入',goal:'目标',scenario,orchestration_version:1});
  }
  assert.throws(() => data.validateProject({title:'x'.repeat(201),origin:'x',goal:'y',scenario:'ctf'}));
  assert.throws(() => data.validateProject({title:'x',origin:'',goal:'y',scenario:'ctf'}));
});

test('activity time is Shanghai calendar time, with no invented fallback timestamp', () => {
  const activity = require('./static/activity-data.js');
  const entries = activity.buildEntries({logs:[{time:null},{time:'invalid'},{time:'2026-09-23T16:05:09Z'}]});
  assert.deepEqual(entries.map(entry => entry.time),['00:05:09','—','—']);
  assert.equal(entries[0].timestamp,'2026-09-23T16:05:09Z');
});

test('only persisted completion with valid root support becomes a project result', () => {
  const state = fixture();
  assert.equal(data.buildResult(state).status,'pending');
  state.graph.project.status = 'terminated';
  assert.equal(data.buildResult(state).status,'terminated');
  assert.equal(data.buildResult(state).summary,'');
  state.graph.project.status = 'completed';
  assert.equal(data.buildResult(state).summary,'已核验目标');
  state.goals[0].support_valid = false;
  assert.equal(data.buildResult(state).status,'unverified');
  assert.equal(data.buildResult(state).summary,'');
  state.goals[0].support_valid = true;
  state.graph.intents[0].concluded_at = '2026-09-23T23:59:59Z';
  assert.equal(data.buildResult(state).status,'unverified');
});

test('candidate findings, invalid support, rejected and truncated output never become successful conclusions', () => {
  const state = fixture();
  state.findings = [{id:'a',claim:'候选',status:'candidate',sources:[]},
    {id:'b',claim:'旧结论',status:'verified',sources:['f'],support_valid:false}];
  const executions = ['failed','rejected','succeeded'].map((status,i) => ({id:String(i),project_id:'p',generation:1,
    status,kind:'reason',result:{text:JSON.stringify({complete:{description:'不要当成完成'}}),truncated:i===2}}));
  const result = data.buildResult(state,data.buildLogs(state,[],executions));
  assert.equal(result.status,'pending');
  assert.deepEqual(result.conclusions,[]);
  assert.equal(result.truncated,true);
  assert.match(result.notice,/截断/);
  assert.equal(result.findings[0].statusLabel,'待验证');
  assert.match(result.findings[1].statusLabel,/证据失效/);
});

test('execution conclusions remain separate from project completion and never include old rounds or reasoning', () => {
  const state = fixture();
  const executions = [{id:'old',generation:0,status:'succeeded',kind:'explore',result:{text:'{"description":"旧轮"}'}},
    {id:'current',generation:1,status:'succeeded',kind:'explore',result:{text:'{"description":"观察结果","reasoning":"内部推理"}'}}];
  const logs = data.buildLogs(state,[],executions);
  const result = data.buildResult(state,logs);
  assert.equal(result.status,'pending');
  assert.equal(result.conclusions.length,1);
  assert.equal(result.conclusions[0].body,'观察结果');
  assert.equal(result.conclusions[0],logs.find(log => log.source === 'execution' && log.kind === 'model'),'result reuses the accepted log projection');
  const executionLogs = logs.filter(log => log.source === 'execution');
  assert.doesNotMatch(JSON.stringify(executionLogs),/内部推理|旧轮/);
});

test('activity logs identify real execution phases without inferring an upstream component', () => {
  const state = fixture();
  const logs = data.buildLogs(state,[],[{id:'run',generation:1,status:'failed',kind:'reason',
    updated_at:'2026-09-24T00:01:00Z',result:{error:'HTTP 503: execution failed'}}]);
  const error = logs.find(log => log.runId === 'run');
  assert.equal(error.phase,'Decide');
  assert.equal(error.level,'error');
  assert.equal(error.time,'2026-09-24T00:01:00Z');
  assert.match(error.body,/HTTP 503: execution failed/);
  assert.equal(logs.some(log => log.phase === 'LLM'),false);
});
