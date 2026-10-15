import type { Point, Camera } from './types';
export interface Bounds { left: number; top: number; right: number; bottom: number; width: number; height: number }
interface BoundsOptions { width?: number; height?: number; padding?: number }
export function contentBounds(positions: ReadonlyMap<string, Point>, options: BoundsOptions = {}): Bounds {
  const { width = 148, height = 78, padding = 32 } = options;
  if (![width, height, padding].every(value => Number.isFinite(value) && value >= 0)) throw new RangeError('Canvas dimensions must be finite and non-negative');
  if (!positions.size) return {left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0};
  let left = Infinity, top = Infinity, right = -Infinity, bottom = -Infinity;
  for (const point of positions.values()) {
    if (!point || !Number.isFinite(point.x) || !Number.isFinite(point.y)) throw new RangeError('Node coordinates must be finite');
    left = Math.min(left, point.x - padding);
    top = Math.min(top, point.y - padding);
    right = Math.max(right, point.x + width + padding);
    bottom = Math.max(bottom, point.y + height + padding);
  }
  return { left, top, right, bottom, width: right - left, height: bottom - top };
}
export function worldBounds(positions: ReadonlyMap<string, Point>, options: BoundsOptions & {baseWidth?: number; baseHeight?: number} = {}): Bounds {
  const {baseWidth = 1780, baseHeight = 1080} = options;
  if (![baseWidth, baseHeight].every(value => Number.isFinite(value) && value >= 0)) throw new RangeError('Canvas dimensions must be finite and non-negative');
  const content = contentBounds(positions, options);
  const left = Math.min(0, content.left), top = Math.min(0, content.top);
  const right = Math.max(baseWidth, content.right), bottom = Math.max(baseHeight, content.bottom);
  return {left, top, right, bottom, width: right - left, height: bottom - top};
}

export function fitTransform(bounds: Pick<Bounds, 'left' | 'top' | 'width' | 'height'>, viewportWidth: number, viewportHeight: number): Camera | null {
  if (!Number.isFinite(viewportWidth) || !Number.isFinite(viewportHeight) || viewportWidth <= 0 || viewportHeight <= 0) return null;
  if (![bounds.left, bounds.top, bounds.width, bounds.height].every(Number.isFinite) || bounds.width < 0 || bounds.height < 0) throw new RangeError('Canvas bounds must be finite with non-negative dimensions');
  const scale = Math.min(1.15, Math.max(1, viewportWidth - 48) / Math.max(1, bounds.width), Math.max(1, viewportHeight - 90) / Math.max(1, bounds.height));
  return {
    scale,
    tx: (viewportWidth - bounds.width * scale) / 2 - bounds.left * scale,
    ty: 40 + (viewportHeight - 90 - bounds.height * scale) / 2 - bounds.top * scale,
  };
}

export function edgePanVelocity(x: number, y: number, width: number, height: number): Point {
  function axis(coordinate: number, extent: number) {
    if (!Number.isFinite(coordinate) || !Number.isFinite(extent) || extent <= 0) return 0;
    const zone = Math.min(56, extent / 2);
    if (coordinate < zone) return 480 * Math.min(1, (zone - coordinate) / zone);
    if (coordinate > extent - zone) return -480 * Math.min(1, (coordinate - extent + zone) / zone);
    return 0;
  }
  return { x: axis(x, width), y: axis(y, height) };
}
