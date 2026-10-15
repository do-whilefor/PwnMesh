// @vitest-environment happy-dom
import { test, vi, afterEach } from 'vitest';
import assert from 'node:assert/strict';
import { GraphController } from './controller';
import type { GraphRecord, GraphNode, GraphEdge, GraphLayout } from './types';
interface Fixture { graph: {project: GraphRecord}; goals: GraphRecord[]; steps: GraphRecord[]; fact_records: GraphRecord[]; findings: GraphRecord[]; fact_relations: GraphRecord[] }
afterEach(() => { vi.restoreAllMocks(); document.body.replaceChildren(); });
function harness(options: ConstructorParameters<typeof GraphController>[1] = {}) {
  const frames = new Map<number, FrameRequestCallback>(); let serial = 0;
  vi.spyOn(window, 'requestAnimationFrame').mockImplementation(fn => { frames.set(++serial, fn); return serial; });
  vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(id => { frames.delete(id); });
  const host = document.createElement('div'); document.body.append(host);
  const graph = new GraphController(host, options), capture = new Set<number>();
  Object.defineProperties(graph.viewport, {clientWidth: {value:800, writable:true}, clientHeight: {value:600, writable:true}});
  graph.viewport.getBoundingClientRect = () => new DOMRect(0, 0, graph.viewport.clientWidth, graph.viewport.clientHeight);
  graph.viewport.setPointerCapture = id => { capture.add(id); };
  graph.viewport.hasPointerCapture = id => capture.has(id);
  graph.viewport.releasePointerCapture = id => { capture.delete(id); };
  graph.viewportSize = graph.size();
  return {graph, host, document, window, frames, flush(time = 16) { const pending = [...frames]; frames.clear(); for (const [, fn] of pending) fn(time); }};
}
function pointer(graph: GraphController, type: string, data: PointerEventInit & {target?: EventTarget}) {
  const event = new PointerEvent(type, data);
  Object.defineProperty(event, 'target', {value:data.target ?? graph.viewport});
  return event;
}
function state(id = 'A', generation = 1): Fixture {
  return {graph: {project: {id, generation}}, fact_records: [{id: 'origin', description: '原始输入', status: 'input'}, {id: 'old', description: '旧事实', status: 'valid'}], steps: [{id: 'work', description: '任务', status: 'running', from: ['origin'], result: 'old'}], goals: [], findings: [], fact_relations: [{kind: 'refutes', source: 'origin', target: 'old', reason: 'original'}]};
}
test('repeated updates preserve DOM identity and manual positions, and merge one animation frame', () => {
  const h = harness(), graph = h.graph, value = state(); graph.setState(value); h.flush();
  const card = graph.cards.get('fact:origin')!; graph.positions.set('fact:origin', {x: -9900, y: 4200}); graph.zoomBy(.7);
  const camera = [graph.scale, graph.tx, graph.ty];
  value.fact_records.reverse(); value.fact_records.push({id: 'new', description: '新增', status: 'valid'});
  graph.setState(value); graph.setState(value); graph.setState(value);
  assert.equal(h.frames.size, 1); h.flush();
  assert.equal(graph.cards.get('fact:origin')!, card); assert.deepEqual(graph.positions.get('fact:origin')!, {x: -9900, y: 4200});
  assert.deepEqual([graph.scale, graph.tx, graph.ty], camera); assert.ok(graph.positions.has('fact:new'));
  graph.destroy();
});
test('switching projects restores exact camera and positions; new generations receive their own layout', () => {
  const h = harness(), graph = h.graph; graph.setState(state('A')); h.flush();
  graph.positions.set('fact:origin', {x: -444, y: 777}); graph.scale = .36; graph.tx = -555; graph.ty = 888; graph.transform();
  graph.setState(null); h.flush(); graph.setState(state('B')); h.flush(); graph.zoomBy(1.4);
  graph.setState(state('A')); h.flush();
  assert.deepEqual(graph.positions.get('fact:origin')!, {x: -444, y: 777}); assert.deepEqual([graph.scale, graph.tx, graph.ty], [.36, -555, 888]);
  graph.setState(state('A', 2)); h.flush(); assert.notDeepEqual(graph.positions.get('fact:origin')!, {x: -444, y: 777});
  assert.ok(graph.projectLayouts.has(JSON.stringify(['A', 1]))); assert.ok(graph.projectLayouts.has(JSON.stringify(['B', 1])));
  graph.destroy();
});

test('application-owned layouts survive graph remounts and remain scoped to the project generation', () => {
  const projectLayouts = new Map<string, GraphLayout>(), first = harness({projectLayouts});
  first.graph.setState(state('A')); first.flush();
  first.graph.positions.set('step:work', {x: 144, y: 288});
  first.graph.scale = .42; first.graph.tx = 72; first.graph.ty = -48;
  first.graph.destroy();
  const other = harness({projectLayouts}); other.graph.setState(state('B')); other.flush(); other.graph.destroy();
  const restored = harness({projectLayouts}); restored.graph.setState(state('A')); restored.flush();
  assert.deepEqual(restored.graph.positions.get('step:work')!, {x: 144, y: 288});
  assert.deepEqual([restored.graph.scale,restored.graph.tx,restored.graph.ty], [.42,72,-48]);
  restored.graph.setState(state('A', 2)); restored.flush();
  assert.notDeepEqual(restored.graph.positions.get('step:work')!, {x: 144, y: 288});
  assert.equal(projectLayouts.has(JSON.stringify(['A', 1])),true);
  assert.equal(projectLayouts.has(JSON.stringify(['B', 1])),true);
  restored.graph.destroy();
});

test('detaching a cached Vue subtree preserves the last visible viewport dimensions', () => {
  const projectLayouts = new Map<string, GraphLayout>(), first = harness({projectLayouts});
  first.graph.setState(state('A')); first.flush();
  first.graph.scale = .4; first.graph.tx = 50; first.graph.ty = -25; first.graph.transform();
  Object.assign(first.graph.viewport, {clientWidth:0, clientHeight:0});
  first.graph.resize(); first.graph.destroy();
  const restored = harness({projectLayouts}); restored.graph.setState(state('A')); restored.flush();
  assert.deepEqual([restored.graph.scale, restored.graph.tx, restored.graph.ty], [.4, 50, -25]);
  restored.graph.destroy();
});

test('viewing an archived round preserves the current round layout and camera across remounts', () => {
  const projectLayouts = new Map<string, GraphLayout>(), current = harness({projectLayouts});
  current.graph.setState(state('A', 2)); current.flush();
  current.graph.positions.set('step:work', {x: -620, y: 480});
  current.graph.scale = .55; current.graph.tx = 130; current.graph.ty = -260;
  current.graph.destroy();
  const archived = harness({projectLayouts}); archived.graph.setState(state('A', 1)); archived.flush();
  assert.notDeepEqual(archived.graph.positions.get('step:work')!, {x: -620, y: 480});
  archived.graph.destroy();
  const restored = harness({projectLayouts}); restored.graph.setState(state('A', 2)); restored.flush();
  assert.deepEqual(restored.graph.positions.get('step:work')!, {x: -620, y: 480});
  assert.deepEqual([restored.graph.scale, restored.graph.tx, restored.graph.ty], [.55, 130, -260]);
  restored.graph.destroy();
});
test('updated text invalidates no geometry but refreshes cached-edge labels, details and selection callbacks', () => {
  const selected: (GraphNode | null)[] = [], h = harness({onSelect: node => selected.push(node)}), graph = h.graph, value = state(); graph.setState(value); h.flush();
  const edge = graph.edges.find(item => item.kind === 'refutes')!, hit = graph.edgeElements.get(edge.id)!.hit, cache = graph.edgeRouteCache;
  graph.selectNode('fact:origin'); value.fact_records[0].description = '<img src=x onerror=alert(1)>'; value.fact_relations[0].reason = 'new explanation';
  graph.setState(value); h.flush();
  assert.equal(graph.edgeRouteCache, cache); assert.equal(graph.edgeElements.get(edge.id)!.hit, hit);
  assert.match(hit.getAttribute('aria-label')!, /<img/); assert.match(graph.edgeElements.get(edge.id)!.title.textContent!, /new explanation/);
  const card = graph.cards.get('fact:origin')!;
  assert.equal(card.parts.title.textContent, '<img src=x onerror=alert(1)>');
  assert.equal(card.getAttribute('aria-label')!, '起点：<img src=x onerror=alert(1)>，输入');
  assert.equal(card.title, '起点 · 输入\n<img src=x onerror=alert(1)>');
  assert.equal(selected.at(-1)!.description, '<img src=x onerror=alert(1)>');
  graph.project!.edgeIndex.get(edge.id)!.label = 'updated relation'; graph.scheduleDraw(); h.flush(); assert.equal(graph.edgeElements.get(edge.id)!.label.textContent, 'updated relation');
  graph.destroy();
});
test('dependency readiness refreshes selected details without replacing routes and respects endpoint filters', () => {
  const selected: (GraphEdge | null)[] = [], h = harness({onSelectEdge:edge => selected.push(edge)}), graph = h.graph, value = state();
  value.steps = [{id:'first',description:'前置任务',status:'running'}, {id:'next',description:'后续任务',status:'blocked',depends_on:['first'],blocked_by:['first']}];
  graph.setState(value); h.flush();
  const edge = graph.edges.find(edge => edge.kind === 'step_dependency')!, entry = graph.edgeElements.get(edge.id)!;
  const cache = graph.edgeRouteCache, positions = [...graph.positions], camera = [graph.scale,graph.tx,graph.ty];
  graph.selectEdge(edge); h.flush();
  assert.match(entry.hit.getAttribute('aria-label')!, /执行依赖：前置任务 → 后续任务，前置步骤运行中/);
  value.steps[0].status = 'completed'; value.steps[0].support_valid = true;
  value.steps[1].status = 'open'; value.steps[1].blocked_by = [];
  graph.setState(value); h.flush();
  assert.equal(selected.length, 2, 'an unchanged edge definition still refreshes after endpoint readiness changes');
  assert.equal(graph.edgeRouteCache, cache); assert.equal(graph.edgeElements.get(edge.id)!, entry);
  assert.equal(graph.getEdgeDetails(edge)!.status, 'done'); assert.match(entry.title.textContent!, /依赖已满足/);
  graph.setStatusFilter('pending'); h.flush();
  assert.equal(graph.cards.get('step:next')!.hidden, false); assert.equal(graph.cards.get('step:first')!.hidden, true);
  assert.equal(entry.group.style.display, 'none'); assert.equal(graph.selectedEdge, null); assert.equal(selected.at(-1)!, null);
  graph.setStatusFilter('all'); h.flush(); assert.equal(entry.group.style.display, '');
  assert.deepEqual(graph.positions, new Map(positions)); assert.deepEqual([graph.scale,graph.tx,graph.ty], camera);
  graph.selectEdge(edge); value.steps = value.steps.slice(1); graph.setState(value); h.flush();
  assert.equal(graph.edgeElements.has(edge.id), false); assert.equal(graph.selectedEdge, null);
  assert.equal((graph.diagnostics[0] as Extract<typeof graph.diagnostics[number], {kind:'missing_reference'}>).relation, 'step_dependency'); assert.equal(graph.warning.hidden, false);
  graph.destroy();
});
test('deleted selection and active drag release capture, stop edge-pan and notify application', () => {
  const selection: (GraphNode | null)[] = [], edges: (GraphEdge | null)[] = [], h = harness({onSelect: node => selection.push(node), onSelectEdge: edge => edges.push(edge)}), graph = h.graph, value = state(); graph.setState(value); h.flush();
  const previous = graph.project, removed = graph.edges.find(edge => edge.kind === 'refutes')!;
  graph.selectEdge(removed); h.flush();
  value.fact_relations = []; graph.setState(value); h.flush(); assert.equal(edges.at(-1)!, null); assert.equal(graph.selectedEdge, null);
  assert.equal(graph.getEdgeDetails(removed)!, null); assert.equal(graph.project!.edgeIndex.has(removed.id), false);
  assert.equal(graph.project!.incoming.get(removed.target)!.some(edge => edge.id === removed.id), false);
  assert.equal(graph.project!.outgoing.get(removed.source)!.some(edge => edge.id === removed.id), false);
  assert.equal(previous!.edgeIndex.get(removed.id)!, removed, 'refresh must not rewrite the preceding snapshot');
  graph.selectNode('fact:old'); const card = graph.cards.get('fact:old')!;
  graph.pointerDown(pointer(graph, 'pointerdown', {button: 0, target: card, pointerId: 7, clientX: 600, clientY: 300})); graph.pointerMove(pointer(graph, 'pointermove', {pointerId: 7, clientX: 799, clientY: 300}));
  assert.ok(graph.viewport.hasPointerCapture(7)); assert.notEqual(graph.panFrame, null);
  value.fact_records = value.fact_records.filter(fact => fact.id !== 'old'); value.steps = [];
  graph.setState(value); h.flush(); assert.equal(selection.at(-1)!, null); assert.equal(graph.drag, null); assert.equal(graph.panFrame, null); assert.ok(!graph.viewport.hasPointerCapture(7));
  for (const name of ['nodeIndex', 'incoming', 'outgoing'] as const) assert.equal(graph.project![name].has('fact:old'), false);
  assert.equal(graph.project!.edgeIndex.size, 0); assert.deepEqual(graph.project!.outgoing.get('fact:origin')!, []);
  graph.setState(null); h.flush();
  for (const name of ['nodeIndex', 'edgeIndex', 'incoming', 'outgoing'] as const) assert.equal(graph.project![name].size, 0);
  graph.destroy();
});
test('zoom during drag preserves world grab offset; resizing preserves camera center', () => {
  const h = harness(), graph = h.graph; graph.setState(state()); h.flush(); const card = graph.cards.get('fact:origin')!;
  graph.pointerDown(pointer(graph, 'pointerdown', {button: 0, target: card, pointerId: 1, clientX: 230, clientY: 200})); graph.pointerMove(pointer(graph, 'pointermove', {pointerId: 1, clientX: 400, clientY: 240}));
  const grab = [graph.drag!.grabX, graph.drag!.grabY]; graph.zoom(.72, 40, 60);
  const point = graph.positions.get('fact:origin')!;
  assert.ok(Math.abs((point.x + grab[0]) * graph.scale + graph.tx - 400) < 1e-7); assert.ok(Math.abs((point.y + grab[1]) * graph.scale + graph.ty - 240) < 1e-7);
  graph.cancelDrag(); const center = [(400 - graph.tx) / graph.scale, (300 - graph.ty) / graph.scale], scale = graph.scale;
  Object.assign(graph.viewport, {clientWidth:600, clientHeight:420}); graph.resize();
  assert.equal(graph.scale, scale); assert.deepEqual([(300 - graph.tx) / graph.scale, (210 - graph.ty) / graph.scale], center); graph.destroy();
});
test('blur, hidden document and destruction stop animation and remove every listener', () => {
  const h = harness(), graph = h.graph; graph.setState(state()); h.flush();
  const begin = () => { graph.pointerDown(pointer(graph, 'pointerdown', {button: 0, target: graph.cards.get('fact:origin')!, pointerId: 1, clientX: 200, clientY: 200})); graph.pointerMove(pointer(graph, 'pointermove', {pointerId: 1, clientX: 799, clientY: 300})); };
  begin(); h.window.dispatchEvent(new Event('blur')); assert.equal(graph.drag, null); assert.equal(graph.panFrame, null);
  begin(); Object.defineProperty(h.document, 'hidden', {value:true, configurable:true}); h.document.dispatchEvent(new Event('visibilitychange')); assert.equal(graph.drag, null);
  begin(); const disconnect = vi.spyOn(graph.resizeObserver!, 'disconnect');
  const removals = [h.window, h.document, graph.viewport, graph.svg, graph.nodeHost].map(target => vi.spyOn(target, 'removeEventListener'));
  const listenerCount = graph.listeners.length;
  graph.destroy(); graph.destroy(); h.flush();
  assert.equal(h.frames.size, 0); assert.equal(h.host.children.length, 0); assert.equal(disconnect.mock.calls.length, 1);
  assert.equal(removals.reduce((total, spy) => total + spy.mock.calls.length, 0), listenerCount);
  assert.equal(graph.listeners.length, 0);
  Object.defineProperty(h.document, 'hidden', {value:false, configurable:true});
});
test('separate graph instances have unique SVG marker identities', () => {
  const first = harness(), second = harness(); assert.notEqual(first.graph.instanceId, second.graph.instanceId);
  assert.notEqual(first.graph.defs.children[0].getAttribute('id')!, second.graph.defs.children[0].getAttribute('id')!); first.graph.destroy(); second.graph.destroy();
});

test('relation colors and arrowheads survive live status and selection changes', () => {
  const h = harness(), graph = h.graph, value = state(); graph.setState(value); h.flush();
  const input = graph.edges.find(edge => edge.kind === 'step_input')!;
  const output = graph.edges.find(edge => edge.kind === 'step_result')!;
  const entry = graph.edgeElements.get(input.id)!, outputEntry = graph.edgeElements.get(output.id)!;
  const color = entry.group.style.getPropertyValue('--edge-color'), marker = entry.path.getAttribute('marker-end')!;
  assert.ok(color); assert.notEqual(color, outputEntry.group.style.getPropertyValue('--edge-color'));
  assert.equal(entry.port.getAttribute('fill')!, color); assert.equal(entry.label.style.getPropertyValue('--edge-color'), color);
  assert.equal(entry.path.dataset.edgeKind, 'step_input'); assert.equal(entry.path.classList.contains('running'), true);
  const arrow = [...graph.defs.children].find(element => marker.includes(element.getAttribute('id')!))!.children[0];
  assert.equal(arrow.getAttribute('fill')!, 'none'); assert.equal(arrow.getAttribute('stroke')!, color);
  graph.selectEdge(input); h.flush();
  assert.equal(entry.path.getAttribute('marker-end')!, marker); assert.equal(entry.group.style.getPropertyValue('--edge-color'), color);
  assert.equal(entry.path.classList.contains('selected'), true);
  value.steps[0].status = 'completed'; value.steps[0].support_valid = true; graph.setState(value); h.flush();
  assert.equal(entry.path.classList.contains('done'), true); assert.equal(entry.path.getAttribute('marker-end')!, marker);
  assert.equal(entry.group.style.getPropertyValue('--edge-color'), color); graph.destroy();
});

test('cancelled work remains visible as cancelled and cannot return to the pending filter', () => {
  const h = harness(), graph = h.graph, value = state(); graph.setState(value); h.flush();
  const card = graph.cards.get('step:work')!, positions = new Map(graph.positions);
  value.steps[0].status = 'cancelled'; graph.setState(value); h.flush();
  assert.equal(graph.cards.get('step:work')!, card); assert.equal(card.dataset.status, 'cancelled');
  assert.ok(card.classList.contains('invalid')); assert.match(card.getAttribute('aria-label')!, /已取消/);
  assert.ok(graph.getEdgeDetails(graph.edges.find(edge => edge.kind === 'step_input')!)!.status === 'invalid');
  graph.setStatusFilter('pending'); h.flush(); assert.equal(card.hidden, true);
  graph.setStatusFilter('all'); h.flush(); assert.equal(card.hidden, false);
  assert.deepEqual(graph.positions, positions); graph.destroy();
});

test('lost Step support updates cards, result edges and completion filters without changing history or geometry', () => {
  const h = harness(), graph = h.graph, value = state();
  value.graph.project!.orchestration_version = 1;
  value.steps[0].status = 'completed'; value.steps[0].support_valid = true;
  graph.setState(value); h.flush(); graph.setStatusFilter('done'); h.flush();
  const card = graph.cards.get('step:work')!, fact = graph.cards.get('fact:old')!;
  const result = graph.edges.find(edge => edge.kind === 'step_result')!, edge = graph.edgeElements.get(result.id)!;
  const cache = graph.edgeRouteCache, positions = [...graph.positions], camera = [graph.scale,graph.tx,graph.ty];
  assert.equal(card.hidden, false); assert.equal(fact.hidden, false);
  delete value.steps[0].support_valid; value.fact_records[1].support_invalid = true;
  graph.setState(value); h.flush();
  assert.equal(card.hidden, true); assert.equal(fact.hidden, true);
  assert.ok(card.classList.contains('invalid')); assert.ok(fact.classList.contains('invalid'));
  assert.match(card.getAttribute('aria-label')!, /已完成 · 完成结果支持失效/);
  assert.match(fact.getAttribute('aria-label')!, /有效 · 支持失效/);
  assert.match(edge.hit.getAttribute('aria-label')!, /支持已失效/);
  assert.equal(edge.group.style.display, 'none');
  assert.equal(card.dataset.status, 'completed'); assert.equal(fact.dataset.status, 'valid');
  assert.equal(graph.edgeRouteCache, cache); assert.deepEqual(new Map(graph.positions), new Map(positions)); assert.deepEqual([graph.scale,graph.tx,graph.ty], camera);
  value.steps[0].support_valid = true; delete value.fact_records[1].support_invalid;
  graph.setState(value); h.flush();
  assert.equal(graph.cards.get('step:work')!, card); assert.equal(card.hidden, false); assert.equal(fact.hidden, false);
  assert.equal(edge.group.style.display, ''); graph.destroy();
});
test('the first real nodes arriving after an empty snapshot receive initial fit', () => {
  const h = harness(), graph = h.graph; graph.setState({graph: {project: {id: 'A', generation: 1}}}); h.flush();
  graph.setState(state()); assert.notEqual(graph.fitFrame, null); h.flush();
  assert.ok(graph.scale < 1); graph.destroy();
});

test('status filtering hides cards and disconnected edges without changing graph data, positions or camera', () => {
  const h = harness(), graph = h.graph, value = state();
  value.steps.push({id: 'queued', status: 'pending'}, {id: 'failed', status: 'failed'}, {id: 'paused', status: 'paused'}, {id: 'review', status: 'completed', invalid_sources: ['origin']});
  value.goals.push({id: 'unsupported', status: 'achieved', sources: ['old'], support_valid: false});
  graph.setState(value); h.flush(); graph.zoomBy(.8);
  const cards = [...graph.cards], positions = [...graph.positions].map(([key, point]) => [key, {...point}]), camera = [graph.scale, graph.tx, graph.ty];
  const visibleKeys = () => [...graph.cards].filter(([, card]) => !card.hidden).map(([key]) => key).sort();
  graph.setStatusFilter('done'); h.flush();
  assert.equal(graph.getStatusFilter(), 'done'); assert.equal(graph.getVisibleNodeCount(), 2); assert.equal(graph.nodes.length, 8);
  assert.deepEqual(visibleKeys(), ['fact:old', 'fact:origin']);
  for (const edge of graph.edges) {
    const expected = edge.kind === 'refutes' ? '' : 'none', entry = graph.edgeElements.get(edge.id)!;
    assert.equal(entry.group.style.display, expected); assert.equal(entry.label.style.display, expected);
  }
  graph.setStatusFilter('running'); h.flush(); assert.deepEqual(visibleKeys(), ['step:work']);
  graph.selectNode('step:work'); h.flush(); assert.deepEqual(visibleKeys(), ['step:work']);
  assert.equal(graph.cards.get('step:work')!.classList.contains('dim'), false);
  assert.equal(graph.cards.get('fact:origin')!.classList.contains('lineage'), true);
  assert.equal(graph.cards.get('fact:old')!.classList.contains('dim'), true);
  graph.setStatusFilter('pending'); h.flush(); assert.deepEqual(visibleKeys(), ['step:queued']);
  graph.setStatusFilter('all'); h.flush(); assert.equal(graph.getVisibleNodeCount(), 8);
  assert.ok([...graph.edgeElements.values()].every(entry => entry.group.style.display === '' && entry.label.style.display === ''));
  for (const [key, card] of cards) assert.equal(graph.cards.get(key)!, card);
  assert.deepEqual([...graph.positions], positions); assert.deepEqual([graph.scale, graph.tx, graph.ty], camera); graph.destroy();
});

test('filtering clears hidden selections and drag captures, and live status changes honor the active filter', () => {
  const nodes: (GraphNode | null)[] = [], edges: (GraphEdge | null)[] = [], h = harness({onSelect: node => nodes.push(node), onSelectEdge: edge => edges.push(edge)}), graph = h.graph, value = state();
  graph.setState(value); h.flush(); graph.selectNode('step:work');
  graph.pointerDown(pointer(graph, 'pointerdown', {button: 0, target: graph.cards.get('step:work')!, pointerId: 4, clientX: 200, clientY: 200}));
  graph.pointerMove(pointer(graph, 'pointermove', {pointerId: 4, clientX: 799, clientY: 300})); graph.setStatusFilter('done'); h.flush();
  assert.equal(graph.selected, null); assert.equal(nodes.at(-1)!, null); assert.equal(graph.drag, null); assert.equal(graph.panFrame, null); assert.ok(!graph.viewport.hasPointerCapture(4));
  graph.selectNode('step:work'); assert.equal(graph.selected, null);
  graph.selectEdge(graph.edges.find(edge => edge.kind === 'step_input')!); assert.equal(graph.selectedEdge, null);
  graph.selectEdge(graph.edges.find(edge => edge.kind === 'refutes')!); assert.ok(graph.selectedEdge);
  graph.setStatusFilter('running'); h.flush(); assert.equal(graph.selectedEdge, null); assert.equal(edges.at(-1)!, null);
  graph.selectNode('step:work');
  graph.pointerDown(pointer(graph, 'pointerdown', {button: 0, target: graph.cards.get('step:work')!, pointerId: 5, clientX: 200, clientY: 200}));
  value.steps[0].status = 'completed'; graph.setState(value); h.flush();
  assert.equal(graph.getStatusFilter(), 'running'); assert.equal(graph.selected, null); assert.equal(nodes.at(-1)!, null);
  assert.equal(graph.drag, null); assert.ok(!graph.viewport.hasPointerCapture(5)); assert.equal(graph.getVisibleNodeCount(), 0);
  assert.equal(graph.empty.hidden, false); assert.equal(graph.empty.textContent, '暂无运行中节点');
  value.steps[0].status = 'running'; graph.setState(value); h.flush();
  assert.equal(graph.getVisibleNodeCount(), 1); assert.equal(graph.empty.hidden, true); graph.destroy();
});

test('filtered fit uses visible nodes and empty filters preserve the camera; project and round changes reset filters', () => {
  const h = harness(), graph = h.graph; graph.setState(state()); h.flush();
  graph.positions.set('step:work', {x: 10, y: 20}); graph.positions.set('fact:origin', {x: 60000, y: 70000});
  graph.setStatusFilter('running'); h.flush(); graph.fit(); const filteredScale = graph.scale;
  graph.setStatusFilter('all'); graph.fit(); assert.ok(filteredScale > graph.scale * 10);
  graph.setStatusFilter('pending'); h.flush(); const camera = [graph.scale, graph.tx, graph.ty]; graph.fit();
  assert.deepEqual([graph.scale, graph.tx, graph.ty], camera); assert.equal(graph.pendingFit, false);
  graph.setState(state()); h.flush(); assert.equal(graph.getStatusFilter(), 'pending');
  graph.setState(state('A', 2)); h.flush(); assert.equal(graph.getStatusFilter(), 'all');
  graph.setStatusFilter('done'); graph.setState(state('B')); h.flush(); assert.equal(graph.getStatusFilter(), 'all');
  graph.setStatusFilter('running'); graph.setState(null); h.flush(); assert.equal(graph.getStatusFilter(), 'all'); graph.destroy();
});

test('fitting a filtered node is independent of its distance from the world origin', () => {
  const h = harness(), graph = h.graph; graph.setState(state()); h.flush();
  graph.setStatusFilter('running');
  for (const point of [{x: 10, y: 20}, {x: 60000, y: 70000}, {x: -60000, y: -70000}]) {
    graph.positions.set('step:work', point); graph.fit();
    assert.equal(graph.scale, 1.15);
    assert.ok(Math.abs((point.x + 74) * graph.scale + graph.tx - 400) < 1e-7);
    assert.ok(Math.abs((point.y + 39) * graph.scale + graph.ty - 295) < 1e-7);
  }
  graph.destroy();
});
