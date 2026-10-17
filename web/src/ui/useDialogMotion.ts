import {onBeforeUnmount, type Ref} from 'vue';

export function useDialogMotion(dialog: Ref<HTMLDialogElement | undefined>) {
  let stop: ((cancelled?: boolean) => void) | undefined;
  let pendingClose: Promise<void> | undefined;
  const preference = window.matchMedia?.('(prefers-reduced-motion: reduce)');
  const onPreferenceChange = (event: MediaQueryListEvent) => {if (event.matches) stop?.(false);};
  preference?.addEventListener?.('change', onPreferenceChange);

  function transition(element: HTMLDialogElement, closing: boolean, from?: ReturnType<typeof snapshot>) {
    const duration = closing ? 160 : 220;
    element.style.setProperty('--dialog-backdrop-from', from?.backdrop || '0');
    element.dataset.motion = closing ? 'leave' : 'enter';
    element.inert = closing;
    const animation = element.animate([
      from?.panel || {opacity: 0, transform: 'translateY(8px) scale(.985)'},
      closing ? {opacity: 0, transform: 'translateY(6px) scale(.99)'} : {opacity: 1, transform: 'none'},
    ], {duration, easing: 'cubic-bezier(.2,.7,.2,1)', fill: 'both'});
    return new Promise<void>(resolve => {
      let settled = false;
      const finish = (cancelled = false) => {
        if (settled) return;
        settled = true; clearTimeout(timer);
        if (closing && !cancelled) element.close();
        animation.cancel(); delete element.dataset.motion; element.inert = false;
        element.style.removeProperty('--dialog-backdrop-from');
        stop = undefined; pendingClose = undefined; resolve();
      };
      // Also settle when a browser suspends animation callbacks in a background tab.
      const timer = setTimeout(() => finish(), duration + 80);
      stop = (cancelled = true) => finish(cancelled);
      void animation.finished.then(() => finish(), () => finish(true));
    });
  }

  function show() {
    const element = dialog.value;
    if (!element || (element.open && element.dataset.motion !== 'leave')) return;
    const from = element.open ? snapshot(element) : undefined;
    stop?.();
    if (!element.open) element.showModal();
    if (!preference?.matches && typeof element.animate === 'function') void transition(element, false, from);
  }

  function close(immediate = false): Promise<void> {
    const element = dialog.value;
    if (!element?.open) return Promise.resolve();
    if (!immediate && pendingClose) return pendingClose;
    const from = snapshot(element);
    stop?.();
    if (immediate || preference?.matches || typeof element.animate !== 'function') {
      element.close(); return Promise.resolve();
    }
    return pendingClose = transition(element, true, from);
  }

  onBeforeUnmount(() => {
    stop?.(); preference?.removeEventListener?.('change', onPreferenceChange);
  });
  return {show, close};
}

function snapshot(element: HTMLDialogElement) {
  const style = getComputedStyle(element);
  return {
    panel: {opacity: style.opacity || '1', transform: style.transform || 'none'},
    backdrop: getComputedStyle(element, '::backdrop').opacity || '1',
  };
}
