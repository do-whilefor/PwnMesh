// @vitest-environment happy-dom
import {mount} from '@vue/test-utils';
import {defineComponent, h, KeepAlive, nextTick, ref} from 'vue';
import {afterEach, beforeEach, expect, it, vi} from 'vitest';
import {useViewMotion} from './useViewMotion';

beforeEach(() => {
  vi.useFakeTimers({toFake:['setTimeout','clearTimeout']});
  vi.spyOn(window,'matchMedia').mockReturnValue({matches:false,addEventListener() {},removeEventListener() {}} as unknown as MediaQueryList);
  vi.spyOn(HTMLElement.prototype,'animate').mockImplementation(() => ({cancel:vi.fn(),finished:new Promise(() => {})}) as unknown as Animation);
});
afterEach(() => {vi.useRealTimers();vi.restoreAllMocks();});

it('settles cached selections without starting hidden animations and returns with the latest view',async () => {
  const selected = ref('first');
  let motion!: ReturnType<typeof useViewMotion<string>>;
  const content = defineComponent({setup() {const panel = ref<HTMLElement>();motion = useViewMotion(selected,panel);return () => h('div',{ref:panel},motion.displayed.value);}});
  const host = defineComponent({props:{active:Boolean},setup:props => () => h(KeepAlive,{},() => props.active ? h(content) : null)});
  const wrapper = mount(host,{props:{active:true}});
  try {
    const baselineTimers = vi.getTimerCount();
    selected.value = 'second';await nextTick();expect(HTMLElement.prototype.animate).toHaveBeenCalledOnce();
    await wrapper.setProps({active:false});
    selected.value = 'latest';await nextTick();
    expect(HTMLElement.prototype.animate).toHaveBeenCalledOnce();
    await motion.ready();
    expect(motion.displayed.value).toBe('latest');expect(vi.getTimerCount()).toBe(baselineTimers);
    await wrapper.setProps({active:true});
    expect(wrapper.text()).toBe('latest');expect(HTMLElement.prototype.animate).toHaveBeenCalledOnce();
  } finally {wrapper.unmount();}
});
