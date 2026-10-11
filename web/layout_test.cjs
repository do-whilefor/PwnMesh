const test = require('node:test');
const assert = require('node:assert/strict');
const {flowLayout, mergePositions, NODE_WIDTH, NODE_HEIGHT} = require('./static/layout.js');

const node = (key, raw = {}) => ({key, id: key.split(':')[1], type: key.split(':')[0], raw});
const edge = (kind, source, target) => ({kind, source, target});
const byKey = result => [...result.positions].sort(([a], [b]) => a.localeCompare(b));
function assertSeparated(result, gapX = 96, gapY = 32) {
  const points = [...result.positions.values()];
  for (const point of points) {
    assert.ok(Number.isFinite(point.x) && Number.isFinite(point.y));
    assert.ok(point.x >= 0 && point.y >= 0 && point.x + NODE_WIDTH <= result.width && point.y + NODE_HEIGHT <= result.height);
  }
  for (let a = 0; a < points.length; a++) for (let b = a + 1; b < points.length; b++) {
    assert.ok(Math.abs(points[a].x - points[b].x) >= NODE_WIDTH + gapX || Math.abs(points[a].y - points[b].y) >= NODE_HEIGHT + gapY);
  }
}

test('flow order follows evidence, dependencies, observations and conclusion support', () => {
  const nodes = ['fact:origin', 'step:first', 'fact:result', 'fact:observed', 'step:next', 'fact:final', 'goal:goal', 'finding:proof'].map(key => node(key));
  const edges = [edge('step_input', 'fact:origin', 'step:first'), edge('step_result', 'step:first', 'fact:result'),
    edge('step_observation', 'step:first', 'fact:observed'), edge('step_input', 'fact:result', 'step:next'),
    edge('step_dependency', 'step:first', 'step:next'), edge('step_result', 'step:next', 'fact:final'),
    edge('goal_support', 'fact:final', 'goal:goal'), edge('finding_support', 'fact:final', 'finding:proof'),
    edge('goal_step', 'goal:goal', 'step:first'), edge('goal_step', 'goal:goal', 'step:next')];
  const result = flowLayout(nodes, edges);
  for (const relation of edges.filter(item => item.kind !== 'goal_step')) assert.ok(result.positions.get(relation.source).x < result.positions.get(relation.target).x, relation.kind);
  assert.equal(result.startKey, 'fact:origin'); assert.equal(result.endKey, 'goal:goal');
  assertSeparated(result);
  assert.deepEqual(byKey(flowLayout([...nodes].reverse(), [...edges].reverse())), byKey(result));
});

test('cycles occupy a finite layer and missing or corrective references do not impose flow', () => {
  const nodes = ['fact:origin', 'step:one', 'fact:cycle', 'step:next', 'fact:done'].map(key => node(key));
  const edges = [edge('step_input', 'fact:origin', 'step:one'), edge('step_result', 'step:one', 'fact:cycle'),
    edge('step_input', 'fact:cycle', 'step:one'), edge('step_input', 'fact:cycle', 'step:next'), edge('step_result', 'step:next', 'fact:done')];
  const result = flowLayout(nodes, edges);
  assert.equal(result.positions.get('step:one').x, result.positions.get('fact:cycle').x);
  assert.ok(result.positions.get('fact:origin').x < result.positions.get('step:one').x);
  assert.ok(result.positions.get('fact:cycle').x < result.positions.get('step:next').x);
  assertSeparated(result);
  assert.deepEqual(byKey(flowLayout(nodes, [...edges, edge('refutes', 'fact:done', 'fact:origin'), edge('step_input', 'missing', 'step:next'), null])), byKey(result));
});

test('goal ownership and parent hierarchy never collapse the supported execution chain', () => {
  const nodes = [node('goal:goal'), node('goal:child', {parent_id: 'goal'}), node('step:work'), node('fact:output')];
  const edges = [edge('parent', 'goal:goal', 'goal:child'), edge('goal_step', 'goal:child', 'step:work'),
    edge('step_result', 'step:work', 'fact:output'), edge('goal_support', 'fact:output', 'goal:child'), edge('goal_support', 'fact:output', 'goal:goal')];
  const result = flowLayout(nodes, edges);
  assert.ok(result.positions.get('step:work').x < result.positions.get('fact:output').x);
  assert.ok(result.positions.get('fact:output').x < result.positions.get('goal:child').x);
  assert.equal(result.positions.get('goal:child').x, result.positions.get('goal:goal').x);
  assertSeparated(result);
});

test('new projects place the isolated origin first and root goal last without inferred edges', () => {
  const nodes = ['fact:origin', 'fact:goal', 'goal:goal'].map(key => node(key));
  const result = flowLayout(nodes, []);
  const start = result.positions.get('fact:origin'), input = result.positions.get('fact:goal'), end = result.positions.get('goal:goal');
  assert.ok(start.x < input.x && input.x < end.x);
  assert.equal(start.y, input.y); assert.equal(input.y, end.y);
  assertSeparated(result);
  assert.deepEqual(byKey(flowLayout([...nodes].reverse(), [])), byKey(result));
  assertSeparated(flowLayout([node('fact:origin'), node('goal:goal')], []));
  assertSeparated(flowLayout([node('goal:goal')], []));
});

test('detached display anchors surround actual flow and compact orphan rows without changing ranks', () => {
  const chain = [node('step:work'), node('fact:output')];
  const edges = [edge('step_result', 'step:work', 'fact:output')];
  const baseline = flowLayout(chain, edges);
  const nodes = [...chain, node('fact:origin'), node('goal:goal'), ...Array.from({length: 90}, (_, index) => node('fact:orphan-' + index))];
  const result = flowLayout(nodes, [...edges, edge('goal_step', 'goal:goal', 'step:work')]);
  const start = result.positions.get('fact:origin'), end = result.positions.get('goal:goal');
  for (const [key, point] of result.positions) {
    if (key !== 'fact:origin') assert.ok(start.x < point.x);
    if (key !== 'goal:goal') assert.ok(point.x < end.x);
  }
  assert.equal(result.positions.get('fact:output').x - result.positions.get('step:work').x,
    baseline.positions.get('fact:output').x - baseline.positions.get('step:work').x);
  assertSeparated(result);
  assert.ok(result.width < 3000 && result.height < 3000);
});

test('dense layers and isolated nodes use nonoverlapping bounded placement', () => {
  const nodes = Array.from({length: 400}, (_, index) => node('fact:' + index));
  const edges = Array.from({length: 200}, (_, index) => edge('step_input', 'fact:0', 'fact:' + (index + 1)));
  const result = flowLayout(nodes, edges);
  assert.equal(result.positions.size, nodes.length); assertSeparated(result);
  const isolated = flowLayout(nodes, []);
  assertSeparated(isolated);
  assert.ok(isolated.width < 4000 && isolated.height < 4000, 'isolated nodes form a compact grid');
});

test('flow layout handles deep histories without recursive stack growth', () => {
  const nodes = Array.from({length: 12000}, (_, index) => node('step:' + index));
  const edges = nodes.slice(1).map((value, index) => edge('step_dependency', nodes[index].key, value.key));
  const result = flowLayout(nodes, edges);
  assert.equal(result.positions.size, nodes.length);
  assert.ok(result.positions.get(nodes[0].key).x < result.positions.get(nodes.at(-1).key).x);
});

test('flow placement remains compatible with cached manual positions and validates spacing', () => {
  const nodes = [node('fact:origin'), node('step:work')], result = flowLayout(nodes, [edge('step_input', 'fact:origin', 'step:work')]);
  result.positions.set('step:work', {x: -18000, y: 24000});
  const updated = mergePositions([...nodes, node('fact:new')], result.positions);
  assert.deepEqual(updated.positions.get('step:work'), {x: -18000, y: 24000});
  assert.deepEqual(updated.positions.get('fact:origin'), result.positions.get('fact:origin'));
  assert.equal(flowLayout([], []).positions.size, 0);
  assert.throws(() => flowLayout([node('fact:a'), node('fact:a')]), TypeError);
  assert.throws(() => flowLayout([{key: ''}]), TypeError);
  assert.throws(() => flowLayout(nodes, [], {gapY: -1}), RangeError);
  assert.throws(() => flowLayout(nodes, [], {padding: Infinity}), RangeError);
  assertSeparated(flowLayout(nodes, [], {gapX: 0, gapY: 0, padding: 0}), 0, 0);
});
