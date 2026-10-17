import {nextTick, onBeforeUnmount, onDeactivated, onMounted, shallowRef, watch, type Ref} from 'vue';

/** Swap one live view at the fade boundary; newer selections cancel older work. */
export function useViewMotion<T>(selected: Ref<T>, panel: Ref<HTMLElement | undefined>) {
  const displayed = shallowRef(selected.value);
  let version = 0, stop: (() => void) | undefined, preference: MediaQueryList | undefined;
  let pending = Promise.resolve();

  function clear() {
    stop?.(); stop = undefined;
    if (panel.value) {panel.value.inert = false; delete panel.value.dataset.viewMotion;}
  }
  function play(element: HTMLElement, frames: Keyframe[], duration: number) {
    stop?.();
    const motion = element.animate(frames, {duration, easing: 'cubic-bezier(.22,1,.36,1)', fill: 'both'});
    return new Promise<void>(resolve => {
      let settled = false;
      const finish = () => {if (!settled) {settled = true; clearTimeout(timer); resolve();}};
      const timer = setTimeout(finish, duration + 80);
      stop = () => {motion.cancel(); finish();};
      void motion.finished.then(finish, finish);
    });
  }
  function change(value: T) {
    const ticket = ++version, element = panel.value;
    const style = element && getComputedStyle(element);
    const from = {opacity: style?.opacity || '1', transform: style?.transform || 'none'};
    clear();
    if (!element || preference?.matches || typeof element.animate !== 'function') {
      displayed.value = value; pending = Promise.resolve(); return;
    }
    pending = (async () => {
      let entering: Keyframe = from;
      if (displayed.value !== value) {
        element.inert = true; element.dataset.viewMotion = 'leave';
        await play(element, [from, {opacity: 0, transform: 'translateY(-4px)'}], 110);
        if (ticket !== version) return;
        displayed.value = value;
        await nextTick();
        entering = {opacity: 0, transform: 'translateY(5px)'};
      }
      if (ticket !== version) return;
      element.inert = false; element.dataset.viewMotion = 'enter';
      await play(element, [entering, {opacity: 1, transform: 'none'}], 200);
      if (ticket === version) clear();
    })();
  }
  function reduce() {if (preference?.matches) change(selected.value);}
  function reset() {version++; clear(); displayed.value = selected.value; pending = Promise.resolve();}
  watch(selected, change, {flush: 'sync'});
  onMounted(() => {preference = window.matchMedia?.('(prefers-reduced-motion: reduce)'); preference?.addEventListener('change', reduce);});
  onDeactivated(reset);
  onBeforeUnmount(() => {reset(); preference?.removeEventListener('change', reduce);});
  return {displayed, ready: () => pending};
}
