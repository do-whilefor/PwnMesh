const test = require('node:test');
const assert = require('node:assert/strict');
const data = require('./static/data.js');

function fixture() {
  return {revision:4,graph:{project:{id:'p',status:'active',generation:1,restarted_at:'2026-09-24T00:00:00Z'},
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

test('historical finding and goal events retain invalidated transitive evidence', () => {
  const state = fixture();
  state.fact_records[0].support_invalid = true;
  state.findings[0].support_valid = false;
  state.goals[0].support_valid = false;
  const events = ['finding','goal'].flatMap((op,index) => {
    const record = op === 'finding' ? state.findings[0] : state.goals[0];
    // Persisted Goal/Finding events explicitly contain support_valid. The log
    // adapter refreshes old events against the current evidence snapshot.
    return [1,2].map(version => ({revision:index * 2 + version,op,id:record.id,
      result:{...record,support_valid:false}}));
  });
  const logs = data.buildLogs(state,events).filter(log => log.source === 'event');
  assert.equal(logs.length,4);
  for (const log of logs) {
    assert.equal(log.supportValid,false,log.id);
    assert.equal(log.level,'warning',log.id);
    assert.match(log.title,/证据失效/);
  }
  assert.equal(state.fact_records[0].status,'valid','support invalidation preserves the recorded fact status');
});

test('Step and Fact logs preserve recorded success while exposing current support invalidation', () => {
  const state = fixture();
  state.graph.project.orchestration_version = 1;
  state.steps = [{id:'work',description:'Inspect endpoint',status:'completed',result:'f',from:['origin']}];
  state.fact_records[0].support_invalid = true;
  const events = [
    {revision:1,op:'step',id:'work',result:{...state.steps[0],support_valid:true}},
    {revision:2,op:'fact',id:'f',result:{...state.fact_records[0],support_invalid:false}}
  ];
  for (const inputEvents of [[],events]) {
    const logs = data.buildLogs(state,inputEvents);
    for (const [type,id,status] of [['step','work','completed'],['fact','f','valid']]) {
      const records = logs.filter(log => log.node?.type === type && log.node.id === id);
      assert.equal(records.length,1,'current projected support must not duplicate the latest event');
      assert.equal(records[0].status,status);
      assert.equal(records[0].supportValid,false);
      assert.equal(records[0].level,'warning');
      assert.match(records[0].title,/支持失效/);
    }
  }
  assert.equal(state.steps[0].support_valid,undefined,'projection does not mutate state');
  assert.equal(events[0].result.support_valid,true,'projection does not rewrite stored events');
});

test('Step log support follows live graph protocol and exempts legacy human feedback', () => {
  const state = fixture();
  state.steps = [{id:'work',description:'Inspect',status:'completed',result:'f'}];
  const stepLog = () => data.buildLogs(state).find(log => log.node?.type === 'step');
  assert.doesNotMatch(stepLog().title,/支持失效/,'legacy snapshots do not compute Step support');
  state.graph.project.orchestration_version = 1;
  assert.match(stepLog().title,/支持失效/);
  state.steps[0].support_valid = true;
  assert.doesNotMatch(stepLog().title,/支持失效/);
  delete state.steps[0].support_valid;
  state.steps[0].description = 'external_feedback';
  state.fact_records[0].legacy = true;
  assert.doesNotMatch(stepLog().title,/支持失效/,'human input does not claim Worker success');
});

test('Fact logs retain default statuses for snapshots that omit the status field', () => {
  const state = fixture();
  state.fact_records = [{id:'origin'}, {id:'goal'}, {id:'observation'}];
  const facts = data.buildLogs(state).filter(log => log.node?.type === 'fact');
  assert.deepEqual(Object.fromEntries(facts.map(log => [log.node.id,log.status])),
    {origin:'input',goal:'input',observation:'valid'});
});
