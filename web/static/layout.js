(function (root, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.PwnMeshLayout = api;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';
  const NODE_WIDTH = 148, NODE_HEIGHT = 78;
  function randomFrom(seed) {
    let state = 2166136261;
    for (const character of String(seed)) state = Math.imul(state ^ character.charCodeAt(0), 16777619);
    return () => {
      state = (state + 0x6d2b79f5) | 0;
      let value = Math.imul(state ^ (state >>> 15), state | 1);
      value ^= value + Math.imul(value ^ (value >>> 7), value | 61);
      return ((value ^ (value >>> 14)) >>> 0) / 4294967296;
    };
  }
  const nodeKey = node => node.key || node.id;
  function anchors(nodes) {
    const startKey = nodes.some(node => nodeKey(node) === 'fact:origin') ? 'fact:origin' : null;
    const roots = nodes.filter(node => node.type === 'goal' && !node.raw?.parent_id).sort((a, b) => nodeKey(a).localeCompare(nodeKey(b)));
    // fact:goal is an input, not an inferred terminal answer.
    const endKey = roots.find(node => node.id === 'goal')?.key || (roots.length ? nodeKey(roots[0]) : null);
    return {startKey, endKey};
  }
  // Placement is independent of relationships: cycles, corrective edges and
  // missing references cannot turn layout into a failed topological sort.
  // Existing coordinates are copied verbatim; only new IDs get a free slot.
  function mergePositions(nodes, previous = new Map(), options = {}) {
    const gapX = options.gapX ?? 24, gapY = options.gapY ?? 24, padding = options.padding ?? 48;
    if (![gapX, gapY, padding].every(value => Number.isFinite(value) && value >= 0)) throw new RangeError('Layout spacing must be non-negative');
    const ordered = [...nodes].sort((a, b) => nodeKey(a).localeCompare(nodeKey(b)));
    const keys = ordered.map(nodeKey);
    if (keys.some(key => typeof key !== 'string' || !key) || new Set(keys).size !== keys.length) throw new TypeError('Node keys must be unique non-empty strings');
    const scale = Math.sqrt(Math.max(1, nodes.length) / 34);
    const width = Math.ceil(Math.max(940, 1780 * scale)), height = Math.ceil(Math.max(640, 1080 * scale));
    const positions = new Map();
    for (const key of keys) {
      const point = previous.get(key);
      if (point && Number.isFinite(point.x) && Number.isFinite(point.y)) positions.set(key, {...point});
    }
    const random = randomFrom(options.seed ?? keys.join('|'));
    const {startKey, endKey} = anchors(nodes);
    const occupied = candidate => [...positions.values()].some(point => candidate.x < point.x + NODE_WIDTH + gapX && candidate.x + NODE_WIDTH + gapX > point.x && candidate.y < point.y + NODE_HEIGHT + gapY && candidate.y + NODE_HEIGHT + gapY > point.y);
    const anchorPoints = new Map([[startKey, {x: padding, y: (height - NODE_HEIGHT) / 2}], [endKey, {x: width - padding - NODE_WIDTH, y: (height - NODE_HEIGHT) / 2}]]);
    for (const key of [startKey, endKey]) {
      if (key && !positions.has(key) && !occupied(anchorPoints.get(key))) positions.set(key, anchorPoints.get(key));
    }
    for (const key of keys) {
      if (positions.has(key)) continue;
      let candidate;
      for (let attempt = 0; attempt < 500; attempt++) {
        candidate = {x: Math.round((padding + random() * Math.max(1, width - padding * 2 - NODE_WIDTH)) * 10) / 10, y: Math.round((padding + random() * Math.max(1, height - padding * 2 - NODE_HEIGHT)) * 10) / 10};
        if (!occupied(candidate)) break;
        candidate = null;
      }
      // A bounded random search always has a finite fallback outside the cloud.
      // It neither moves existing/manual positions nor clamps world coordinates.
      if (!candidate) {
        const right = Math.max(width, ...[...positions.values()].map(point => point.x + NODE_WIDTH + gapX));
        candidate = {x: right + gapX, y: padding + random() * height};
      }
      positions.set(key, candidate);
    }
    return {positions, width, height, startKey, endKey};
  }
  function layout(nodes, edges = [], options = {}) { return mergePositions(nodes, new Map(), options); }
  function flowLayout(nodes, edges = [], options = {}) {
    const gapX = options.gapX ?? 96, gapY = options.gapY ?? 32, padding = options.padding ?? 48;
    if (![gapX, gapY, padding].every(value => Number.isFinite(value) && value >= 0)) throw new RangeError('Layout spacing must be non-negative');
    const keys = nodes.map(nodeKey);
    if (keys.some(key => typeof key !== 'string' || !key) || new Set(keys).size !== keys.length) throw new TypeError('Node keys must be unique non-empty strings');
    keys.sort();
    const outgoing = new Map(keys.map(key => [key, new Set()])), incoming = new Map(keys.map(key => [key, new Set()]));
    const neighbors = new Map(keys.map(key => [key, new Set()]));
    const flowKinds = new Set(['step_input', 'step_result', 'step_observation', 'step_dependency', 'goal_support', 'finding_support']);
    for (const edge of edges) {
      if (!edge || !outgoing.has(edge.source) || !outgoing.has(edge.target)) continue;
      if (flowKinds.has(edge.kind)) {
        outgoing.get(edge.source).add(edge.target); incoming.get(edge.target).add(edge.source);
      }
      // Ownership and Goal decomposition can group rows, but are not evidence flow.
      if (flowKinds.has(edge.kind) || edge.kind === 'goal_step' || edge.kind === 'parent') {
        neighbors.get(edge.source).add(edge.target); neighbors.get(edge.target).add(edge.source);
      }
    }
    for (const graph of [outgoing, incoming, neighbors]) for (const [key, values] of graph) graph.set(key, [...values].sort());

    // Condense cycles with iterative DFS so long histories cannot exhaust the stack.
    const visited = new Set(), finished = [];
    for (const key of keys) {
      if (visited.has(key)) continue;
      visited.add(key);
      const stack = [[key, 0]];
      while (stack.length) {
        const frame = stack[stack.length - 1], next = outgoing.get(frame[0])[frame[1]++];
        if (next === undefined) { finished.push(frame[0]); stack.pop(); }
        else if (!visited.has(next)) { visited.add(next); stack.push([next, 0]); }
      }
    }
    const componentOf = new Map(), components = [];
    for (const key of finished.reverse()) {
      if (componentOf.has(key)) continue;
      const id = components.length, members = [], stack = [key];
      componentOf.set(key, id);
      while (stack.length) {
        const current = stack.pop(); members.push(current);
        for (const source of incoming.get(current)) if (!componentOf.has(source)) { componentOf.set(source, id); stack.push(source); }
      }
      components.push({members, next: new Set(), pending: 0, rank: 0});
    }
    for (const [source, targets] of outgoing) for (const target of targets) {
      const from = componentOf.get(source), to = componentOf.get(target);
      if (from !== to && !components[from].next.has(to)) { components[from].next.add(to); components[to].pending++; }
    }
    const ready = components.flatMap((component, id) => component.pending === 0 ? [id] : []);
    for (let cursor = 0; cursor < ready.length; cursor++) {
      const component = components[ready[cursor]];
      for (const next of component.next) {
        const target = components[next]; target.rank = Math.max(target.rank, component.rank + 1);
        if (--target.pending === 0) ready.push(next);
      }
    }
    const layers = [], isolated = [];
    for (const key of keys) {
      if (!incoming.get(key).length && !outgoing.get(key).length) { isolated.push(key); continue; }
      const rank = components[componentOf.get(key)].rank;
      (layers[rank] ||= []).push(key);
    }
    const rows = new Map();
    const updateRows = layer => layer.forEach((key, row) => rows.set(key, row - (layer.length - 1) / 2));
    layers.forEach(updateRows);
    // A fixed number of barycenter sweeps reduces crossings without an unbounded solver.
    for (let pass = 0; pass < 4; pass++) {
      const ordered = pass % 2 ? [...layers].reverse() : layers;
      for (const layer of ordered) {
        const score = new Map(layer.map(key => {
          const related = neighbors.get(key).filter(other => rows.has(other) && componentOf.get(other) !== componentOf.get(key));
          return [key, related.length ? related.reduce((sum, other) => sum + rows.get(other), 0) / related.length : rows.get(key)];
        }));
        layer.sort((a, b) => score.get(a) - score.get(b) || a.localeCompare(b)); updateRows(layer);
      }
    }
    const {startKey, endKey} = anchors(nodes);
    const detachedStart = isolated.includes(startKey), detachedEnd = isolated.includes(endKey);
    const orphans = isolated.filter(key => key !== startKey && key !== endKey);
    const positions = new Map(), stepX = NODE_WIDTH + gapX, stepY = NODE_HEIGHT + gapY;
    const firstColumn = detachedStart ? 1 : 0;
    const rowCount = layers.reduce((max, layer) => Math.max(max, layer.length), 0);
    layers.forEach((layer, column) => layer.forEach((key, row) => positions.set(key, {
      x: padding + (firstColumn + column) * stepX, y: padding + ((rowCount - layer.length) / 2 + row) * stepY
    })));
    const columns = Math.max(layers.length, Math.ceil(Math.sqrt(orphans.length * stepY / stepX)));
    const isolatedTop = padding + (rowCount ? rowCount * stepY + gapY : 0);
    orphans.forEach((key, index) => positions.set(key, {x: padding + (firstColumn + index % columns) * stepX, y: isolatedTop + Math.floor(index / columns) * stepY}));
    // Detached anchors orient a new project without fabricating causal edges.
    const anchorRows = rowCount || (columns ? Math.ceil(orphans.length / columns) : 1);
    const anchorY = padding + (anchorRows - 1) * stepY / 2;
    if (detachedStart) positions.set(startKey, {x: padding, y: anchorY});
    if (detachedEnd) positions.set(endKey, {x: padding + (firstColumn + columns) * stepX, y: anchorY});
    let width = padding * 2, height = padding * 2;
    for (const point of positions.values()) { width = Math.max(width, point.x + NODE_WIDTH + padding); height = Math.max(height, point.y + NODE_HEIGHT + padding); }
    return {positions, width, height, startKey, endKey};
  }
  return {layout, mergePositions, flowLayout, anchors, NODE_WIDTH, NODE_HEIGHT};
});
