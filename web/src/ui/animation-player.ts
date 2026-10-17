/** One interruptible animation; bounded completion also covers suspended finish callbacks. */
export function animationPlayer() {
  let cancel: (() => void) | undefined;
  function stop() {const previous = cancel;cancel = undefined;previous?.();}
  function play(element: HTMLElement, frames: Keyframe[], duration: number) {
    stop();
    const animation = element.animate(frames,{duration,easing:'cubic-bezier(.22,1,.36,1)',fill:'both'});
    return new Promise<void>(resolve => {
      let settled = false;
      const finish = () => {if (!settled) {settled = true;clearTimeout(timer);resolve();}};
      const timer = setTimeout(finish,duration + 80);
      cancel = () => {animation.cancel();finish();};
      void animation.finished.then(finish,finish);
    });
  }
  return {play,stop};
}
