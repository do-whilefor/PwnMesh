import {nextTick, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, shallowRef, watch, type Ref} from 'vue';
import {animationPlayer} from './animation-player';

/** Swap one live view at the fade boundary; newer selections cancel older work. */
export function useViewMotion<T>(selected: Ref<T>, panel: Ref<HTMLElement | undefined>) {
  const displayed = shallowRef(selected.value), leaving = ref(false);
  const {play,stop} = animationPlayer();
  let version = 0, active = true, preference: MediaQueryList | undefined;
  let pending = Promise.resolve();

  function clear() {
    stop(); leaving.value = false;
    if (panel.value) delete panel.value.dataset.viewMotion;
  }
  function change(value: T) {
    const ticket = ++version, element = panel.value;
    const style = element && getComputedStyle(element);
    const from = {opacity: style?.opacity || '1', transform: style?.transform || 'none'};
    clear();
    if (!active || !element || preference?.matches || typeof element.animate !== 'function') {
      displayed.value = value; pending = Promise.resolve(); return;
    }
    pending = (async () => {
      let entering: Keyframe = from;
      if (displayed.value !== value) {
        leaving.value = true; element.dataset.viewMotion = 'leave';
        await play(element, [from, {opacity: 0, transform: 'translateY(-4px)'}], 110);
        if (ticket !== version) return;
        displayed.value = value;
        await nextTick();
        entering = {opacity: 0, transform: 'translateY(5px)'};
      }
      if (ticket !== version) return;
      leaving.value = false; element.dataset.viewMotion = 'enter';
      await play(element, [entering, {opacity: 1, transform: 'none'}], 200);
      if (ticket === version) clear();
    })();
  }
  function reduce() {if (preference?.matches) change(selected.value);}
  function reset() {version++; clear(); displayed.value = selected.value; pending = Promise.resolve();}
  watch(selected, change, {flush: 'sync'});
  onMounted(() => {preference = window.matchMedia?.('(prefers-reduced-motion: reduce)'); preference?.addEventListener('change', reduce);});
  onActivated(() => {active = true; reset();});
  onDeactivated(() => {active = false; reset();});
  onBeforeUnmount(() => {active = false; reset(); preference?.removeEventListener('change', reduce);});
  return {displayed, leaving, ready: () => pending, reset};
}
