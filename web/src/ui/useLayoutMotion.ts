import {onBeforeUnmount, onBeforeUpdate, onMounted, onUpdated, type Ref} from 'vue';

const easing = 'cubic-bezier(.22, 1, .36, 1)';
type Position = {x: number; y: number; opacity: number; clipPath: string};

/** Animate a form's natural height and field positions without replacing focused inputs. */
export function useLayoutMotion(root: Ref<HTMLElement | undefined>, selector: string) {
  let previous: {height: number; positions: Map<HTMLElement, Position>} | undefined;
  let preference: MediaQueryList | undefined;
  let resize: Animation | undefined;
  const motions = new Map<HTMLElement, Animation>();
  let overflow: string | undefined;

  function stop() {
    resize?.cancel(); resize = undefined;
    for (const motion of motions.values()) motion.cancel();
    motions.clear();
    if (root.value && overflow !== undefined) root.value.style.overflow = overflow;
    overflow = undefined;
  }
  function enabled() {return !preference?.matches && typeof root.value?.animate === 'function';}
  function elements() {return [...root.value?.querySelectorAll<HTMLElement>(selector) || []];}
  function reduce() {if (preference?.matches) {stop(); previous = undefined;}}

  onMounted(() => {
    preference = window.matchMedia?.('(prefers-reduced-motion: reduce)');
    preference?.addEventListener('change', reduce);
  });
  onBeforeUpdate(() => {
    if (!root.value || !enabled()) {previous = undefined; return;}
    const box = root.value.getBoundingClientRect();
    previous = {height: box.height, positions: new Map()};
    for (const element of elements()) {
      const rect = element.getBoundingClientRect();
      if (rect.width && rect.height) {
        const style = getComputedStyle(element);
        previous.positions.set(element, {x: rect.left - box.left, y: rect.top - box.top, opacity: Number(style.opacity), clipPath: style.clipPath});
      }
    }
  });
  onUpdated(() => {
    const element = root.value, before = previous;
    previous = undefined;
    if (!element || !before || !enabled()) return;
    stop();
    const box = element.getBoundingClientRect();
    const fields = elements().map(field => ({field, rect: field.getBoundingClientRect()}));
    if (before.height > 0 && Math.abs(before.height - box.height) > .5) {
      overflow = element.style.overflow;
      element.style.overflow = 'clip';
      const animation = element.animate([{height: before.height + 'px'}, {height: box.height + 'px'}], {duration: 280, easing});
      resize = animation;
      animation.onfinish = () => {
        if (resize !== animation) return;
        resize = undefined;
        element.style.overflow = overflow || '';
        overflow = undefined;
      };
    }
    for (const {field, rect} of fields) {
      if (!rect.width || !rect.height) continue;
      const old = before.positions.get(field);
      const x = old ? old.x - (rect.left - box.left) : 0;
      const y = old ? old.y - (rect.top - box.top) : -4;
      if (old && Math.abs(x) < .5 && Math.abs(y) < .5 && old.opacity >= .99) continue;
      // Reveal inserted rows only as space opens beneath them, avoiding text overlap.
      const clipPath = old?.clipPath || 'inset(0 0 100% 0)';
      const animation = field.animate([
        {transform: `translate(${x}px, ${y}px)`, opacity: old?.opacity ?? 0, clipPath},
        {transform: 'translate(0, 0)', opacity: 1, clipPath: clipPath === 'none' ? 'none' : 'inset(0 0 0% 0)'},
      ], {duration: 280, easing});
      motions.set(field, animation);
      animation.onfinish = () => {if (motions.get(field) === animation) motions.delete(field);};
    }
  });
  onBeforeUnmount(() => {stop(); preference?.removeEventListener('change', reduce);});
}
