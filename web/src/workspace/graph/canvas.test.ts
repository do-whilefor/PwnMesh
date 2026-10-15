import { test } from 'vitest';
import assert from 'node:assert/strict';
import { contentBounds, worldBounds, fitTransform, edgePanVelocity } from './canvas';
test('content fitting excludes empty origin space while world extents retain it', () => {
  for (const point of [{x: 60000, y: 70000}, {x: -60000, y: -70000}]) {
    const positions = new Map([['node', point]]), options = {padding: 96, baseWidth: 0, baseHeight: 0};
    const content = contentBounds(positions, options), world = worldBounds(positions, options);
    assert.equal(content.width, 148 + 192); assert.equal(content.height, 78 + 192);
    assert.ok(world.left <= 0 && world.right >= 0 && world.top <= 0 && world.bottom >= 0);
    assert.ok(world.width > content.width && world.height > content.height);
  }
  assert.deepEqual(contentBounds(new Map()), {left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0});
});
test('fit includes far and negative positions without limiting world bounds', () => {
  const positions = new Map([['left', {x: -100000, y: -300000}], ['right', {x: 800000, y: 600000}]]);
  const bounds = worldBounds(positions, {baseWidth: 0, baseHeight: 0});
  assert.ok(bounds.left < -100000); assert.ok(bounds.top < -300000);
  const camera = fitTransform(bounds, 750, 600)!;
  assert.ok(camera.scale > 0 && camera.scale < .001);
  assert.ok(bounds.left * camera.scale + camera.tx >= 0);
  assert.ok(bounds.right * camera.scale + camera.tx <= 750);
  assert.ok(bounds.top * camera.scale + camera.ty >= 0);
  assert.ok(bounds.bottom * camera.scale + camera.ty <= 600);
});
test('four-edge auto pan stops in center and continues outside viewport', () => {
  assert.ok(edgePanVelocity(0, 300, 700, 600).x > 0);
  assert.ok(edgePanVelocity(700, 300, 700, 600).x < 0);
  assert.ok(edgePanVelocity(350, 0, 700, 600).y > 0);
  assert.ok(edgePanVelocity(350, 600, 700, 600).y < 0);
  assert.deepEqual(edgePanVelocity(350, 300, 700, 600), {x: 0, y: 0});
  assert.equal(edgePanVelocity(-500, 300, 700, 600).x, 480);
});
test('zero-sized and empty viewports do not yield infinite camera transforms', () => {
  const bounds = worldBounds(new Map(), {baseWidth: 0, baseHeight: 0});
  assert.equal(fitTransform(bounds, 0, 200), null);
  assert.ok(Object.values(fitTransform(bounds, 100, 100)!).every(Number.isFinite));
});
