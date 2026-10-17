// @vitest-environment happy-dom
import {mount} from '@vue/test-utils';
import {defineComponent, h, ref} from 'vue';
import {afterEach, beforeEach, expect, it, vi} from 'vitest';
import {useDialogMotion} from './useDialogMotion';

let backdropOpacity = '1', panelOpacity = '1';
let preference: MediaQueryList;
let animations: {finish: () => void; cancel: ReturnType<typeof vi.fn>; frames: Keyframe[]}[];

beforeEach(() => {
  vi.useFakeTimers();
  backdropOpacity = panelOpacity = '1'; animations = [];
  preference = Object.assign(new EventTarget(), {matches: false}) as MediaQueryList;
  vi.spyOn(window, 'matchMedia').mockReturnValue(preference);
  vi.spyOn(globalThis, 'getComputedStyle').mockImplementation((_element, pseudo) => ({
    opacity: pseudo === '::backdrop' ? backdropOpacity : panelOpacity,
    transform: 'matrix(1, 0, 0, 1, 0, 3)',
  }) as CSSStyleDeclaration);
  vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (this: HTMLDialogElement) {this.open = true;});
  vi.spyOn(HTMLDialogElement.prototype, 'close').mockImplementation(function (this: HTMLDialogElement) {this.open = false;});
  vi.spyOn(HTMLElement.prototype, 'animate').mockImplementation(frames => {
    let finish = () => {}, reject = () => {};
    const finished = new Promise<void>((resolve, fail) => {finish = resolve; reject = fail;});
    const cancel = vi.fn(() => reject());
    animations.push({finish, cancel, frames: frames as Keyframe[]});
    return {finished, cancel} as unknown as Animation;
  });
});
afterEach(() => {vi.useRealTimers(); document.body.replaceChildren();});

function setup() {
  let motion!: ReturnType<typeof useDialogMotion>;
  const wrapper = mount(defineComponent({setup() {
    const dialog = ref<HTMLDialogElement>(); motion = useDialogMotion(dialog);
    return () => h('dialog', {ref: dialog});
  }}));
  return {wrapper, motion, dialog: wrapper.get('dialog').element as HTMLDialogElement};
}
function reduceMotion() {
  Object.assign(preference, {matches: true});
  preference.dispatchEvent(Object.assign(new Event('change'), {matches: true}));
}

it('continues both panel and backdrop opacity when opening or closing is interrupted', () => {
  const {wrapper, motion, dialog} = setup();
  try {
    motion.show();
    expect(dialog.style.getPropertyValue('--dialog-backdrop-from')).toBe('0');
    panelOpacity = '.45'; backdropOpacity = '.35'; void motion.close();
    expect(animations.at(-1)?.frames[0].opacity).toBe('.45');
    expect(dialog.style.getPropertyValue('--dialog-backdrop-from')).toBe('.35');
    panelOpacity = '.25'; backdropOpacity = '.2'; motion.show();
    expect(animations.at(-1)?.frames[0].opacity).toBe('.25');
    expect(dialog.style.getPropertyValue('--dialog-backdrop-from')).toBe('.2');
    expect(dialog.inert).toBe(false);
  } finally {wrapper.unmount();}
});

it('keeps repeated close requests pending until the active exit finishes', async () => {
  const {wrapper, motion, dialog} = setup();
  try {
    motion.show();
    const first = motion.close(), second = motion.close(), settled = vi.fn();
    second.then(settled);
    expect(second).toBe(first);
    await Promise.resolve(); expect(settled).not.toHaveBeenCalled();
    animations.at(-1)?.finish(); await first;
    expect(dialog.open).toBe(false); expect(dialog.inert).toBe(false);
    expect(settled).toHaveBeenCalledOnce();
  } finally {wrapper.unmount();}
});

it.each([false, true])('settles the active animation when reduced motion is enabled (closing: %s)', async closing => {
  const {wrapper, motion, dialog} = setup();
  try {
    motion.show();
    const pending = closing ? motion.close() : Promise.resolve();
    reduceMotion();
    expect(dialog.open).toBe(!closing);
    expect(dialog.dataset.motion).toBeUndefined();
    expect(dialog.inert).toBe(false);
    expect(animations.at(-1)?.cancel).toHaveBeenCalledOnce();
    expect(vi.getTimerCount()).toBe(0);
    await pending;
  } finally {wrapper.unmount();}
});

it('reopens safely and removes pending animation work and preference listeners on unmount', async () => {
  const removeListener = vi.spyOn(preference, 'removeEventListener');
  const {wrapper, motion, dialog} = setup();
  motion.show(); const pending = motion.close(); motion.show(); await pending;
  await vi.advanceTimersByTimeAsync(301);
  expect(dialog.open).toBe(true); expect(dialog.inert).toBe(false);
  void motion.close(); wrapper.unmount();
  expect(vi.getTimerCount()).toBe(0);
  expect(dialog.dataset.motion).toBeUndefined();
  expect(dialog.style.getPropertyValue('--dialog-backdrop-from')).toBe('');
  expect(removeListener).toHaveBeenCalledWith('change', expect.any(Function));
});
