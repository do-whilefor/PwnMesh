import {nextTick, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, watch, type Ref} from 'vue';

/** Keep the current text until it fades, then ease to the disclosure's natural height. */
export function useDisclosureMotion(expanded: Readonly<Ref<boolean>>, panel: Ref<HTMLElement | undefined>) {
  const displayed = ref(expanded.value);
  let version = 0, stop: (() => void) | undefined, preference: MediaQueryList | undefined;
  let restore: (() => void) | undefined;
  let active = true;

  function clear() {stop?.();stop = undefined;restore?.();restore = undefined;}
  function reset() {version++;clear();displayed.value = expanded.value;}
  function play(element: HTMLElement, frames: Keyframe[], duration: number) {
    stop?.();
    const animation = element.animate(frames,{duration,easing:'cubic-bezier(.22,1,.36,1)',fill:'both'});
    return new Promise<void>(resolve => {
      let done = false;
      const finish = () => {if (!done) {done = true;clearTimeout(timer);resolve();}};
      const timer = setTimeout(finish,duration + 80);
      stop = () => {animation.cancel();finish();};
      void animation.finished.then(finish,finish);
    });
  }
  async function change(value: boolean) {
    const ticket = ++version,element = panel.value;
    if (!active || !element || preference?.matches || typeof element.animate !== 'function') {clear();displayed.value = value;return;}
    const height = element.getBoundingClientRect().height,style = getComputedStyle(element);
    let from: Keyframe = {opacity:style.opacity,transform:style.transform};
    clear();
    const savedHeight = element.style.height,savedOverflow = element.style.overflow;
    restore = () => {element.style.height = savedHeight;element.style.overflow = savedOverflow;};
    element.style.height = height + 'px';element.style.overflow = 'clip';
    if (displayed.value !== value) {
      await play(element,[from,{opacity:0,transform:'translateY(-3px)'}],85);
      if (ticket !== version) return;
      displayed.value = value;
      await nextTick();
      if (ticket !== version) return;
      from = {opacity:0,transform:'translateY(3px)'};
    }
    stop?.();stop = undefined;
    element.style.height = savedHeight;
    const target = element.getBoundingClientRect().height;
    element.style.height = height + 'px';
    await play(element,[{...from,height:height + 'px'},{opacity:1,transform:'none',height:target + 'px'}],220);
    if (ticket === version) clear();
  }
  function reduce() {if (preference?.matches) reset();}
  watch(expanded,value => void change(value),{flush:'sync'});
  onMounted(() => {preference = window.matchMedia?.('(prefers-reduced-motion: reduce)');preference?.addEventListener?.('change',reduce);});
  onActivated(() => {active = true;reset();});
  onDeactivated(() => {active = false;reset();});
  onBeforeUnmount(() => {active = false;reset();preference?.removeEventListener?.('change',reduce);});
  return {displayed,reset};
}
