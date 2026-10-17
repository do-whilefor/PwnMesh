// @vitest-environment happy-dom
import {flushPromises, mount} from '@vue/test-utils';
import {defineComponent, h, KeepAlive} from 'vue';
import {afterEach, beforeEach, expect, it, vi} from 'vitest';
import InspectorFold from '../workspace/details/InspectorFold.vue';
import SelectionInspector from '../workspace/details/SelectionInspector.vue';
import LogEntry from '../workspace/activity/LogEntry.vue';
import ProjectForms from '../workspace/dialogs/ProjectForms.vue';
import {projectView} from '../workspace/project-resource';

let preference: {matches: boolean; addEventListener: ReturnType<typeof vi.fn>; removeEventListener: ReturnType<typeof vi.fn>};
const cancellations: ReturnType<typeof vi.fn>[] = [];
beforeEach(() => {
  vi.useFakeTimers({toFake:['setTimeout','clearTimeout']});cancellations.length = 0;
  preference = {matches:false,addEventListener:vi.fn(),removeEventListener:vi.fn()};
  vi.spyOn(window,'matchMedia').mockReturnValue(preference as unknown as MediaQueryList);
  vi.spyOn(HTMLElement.prototype,'animate').mockImplementation(() => {
    const cancel = vi.fn();cancellations.push(cancel);
    // A lost finish event must still settle via the bounded fallback.
    return {cancel,finished:new Promise(() => {})} as unknown as Animation;
  });
});
afterEach(() => {vi.useRealTimers();vi.restoreAllMocks();vi.unstubAllGlobals();document.body.replaceChildren();});
const settle = async () => {await vi.advanceTimersByTimeAsync(500);await flushPromises();};

it('retains outgoing details until the fade boundary and latest rapid toggle wins',async () => {
  const wrapper = mount(InspectorFold,{slots:{summary:'Summary',default:'Full text'}});
  try {
    const details = wrapper.get<HTMLDetailsElement>('details').element;
    await wrapper.get('summary').trigger('click');
    expect(details.open).toBe(false);expect(wrapper.get('summary').attributes('aria-expanded')).toBe('true');
    await settle();expect(details.open).toBe(true);
    await wrapper.get('summary').trigger('click');
    expect(details.open).toBe(true);
    await vi.advanceTimersByTimeAsync(30);await wrapper.get('summary').trigger('click');
    await settle();expect(details.open).toBe(true);expect(details.style.height).toBe('');expect(details.style.overflow).toBe('');
    await wrapper.get('summary').trigger('click');await settle();expect(details.open).toBe(false);
    expect(cancellations.every(cancel => cancel.mock.calls.length)).toBe(true);expect(details.style.height).toBe('');
  } finally {wrapper.unmount();}
});

it('clears pending height and fade on reduced motion, cache deactivation, and unmount',async () => {
  const host = defineComponent({props:{active:Boolean},setup:props => () => h(KeepAlive,{},() => props.active ? h(InspectorFold,{}, {summary:() => 'Summary',default:() => 'Body'}) : null)});
  const wrapper = mount(host,{props:{active:true}});
  const details = wrapper.get<HTMLDetailsElement>('details').element;
  await wrapper.get('summary').trigger('click');
  preference.matches = true;preference.addEventListener.mock.calls[0]![1]();await flushPromises();
  expect(details.open).toBe(true);expect(details.style.height).toBe('');expect(vi.getTimerCount()).toBe(0);
  preference.matches = false;
  await wrapper.get('summary').trigger('click');await wrapper.setProps({active:false});
  expect(details.open).toBe(false);expect(details.style.overflow).toBe('');expect(vi.getTimerCount()).toBe(0);
  await wrapper.setProps({active:true});await wrapper.get('summary').trigger('click');wrapper.unmount();await flushPromises();
  expect(details.style.height).toBe('');expect(vi.getTimerCount()).toBe(0);expect(preference.removeEventListener).toHaveBeenCalled();
});

it('preserves inspector disclosure state on refresh and resets it for a new selection',async () => {
  preference.matches = true;
  const record = {title:'Selection',kind:'Fact',body:'Full text\n'.repeat(30),output:'Execution output',meta:[]};
  const wrapper = mount(SelectionInspector,{props:{record,recordKey:'fact:one'}});
  try {
    await wrapper.get('[data-inspector-action="toggle"]').trigger('click');await wrapper.get('summary').trigger('click');
    await wrapper.setProps({record:{...record,output:'Refreshed output'}});
    expect(wrapper.get<HTMLDetailsElement>('details').element.open).toBe(true);expect(wrapper.get('[data-inspector-action="toggle"]').attributes('aria-expanded')).toBe('true');
    await wrapper.setProps({recordKey:'fact:two'});
    expect(wrapper.get<HTMLDetailsElement>('details').element.open).toBe(false);expect(wrapper.get('[data-inspector-action="toggle"]').attributes('aria-expanded')).toBe('false');
  } finally {wrapper.unmount();}
});

it('remeasures log wrapping on width and content changes, not every animated height frame',async () => {
  let resize: ResizeObserverCallback = () => {};
  vi.stubGlobal('ResizeObserver',class {constructor(callback: ResizeObserverCallback) {resize = callback;}observe() {}disconnect() {}});
  const clone = vi.spyOn(Node.prototype,'cloneNode');
  const measurements = () => clone.mock.contexts.filter(node => node instanceof HTMLElement && node.classList.contains('log-body')).length;
  const entry = {id:'log',phase:'FACT',time:'12:00',title:'Log',body:'Content\n'.repeat(20),evidence:'',key:null};
  const wrapper = mount(LogEntry,{props:{entry,expanded:false,query:''}});
  try {
    await flushPromises();
    const frame = async (width: number,height: number) => {resize([{contentRect:{width,height}} as ResizeObserverEntry],{} as ResizeObserver);await flushPromises();};
    await frame(240,120);const measured = measurements();
    await frame(240,200);await frame(240,300);expect(measurements()).toBe(measured);
    await frame(250,300);expect(measurements()).toBe(measured + 1);
    await wrapper.setProps({entry:{...entry,body:entry.body+'Updated'}});await flushPromises();expect(measurements()).toBe(measured + 2);
  } finally {wrapper.unmount();}
});

it('keeps imported materials intact and resets a pending disclosure when hints reopen',async () => {
  vi.spyOn(HTMLDialogElement.prototype,'showModal').mockImplementation(function (this: HTMLDialogElement) {this.open = true;});
  vi.spyOn(HTMLDialogElement.prototype,'close').mockImplementation(function (this: HTMLDialogElement) {this.open = false;this.dispatchEvent(new Event('close'));});
  const project = projectView({id:'project',title:'Project',status:'active',orchestration_version:1});
  project.files = [{id:'capture',name:'capture.txt',size:12}];
  const wrapper = mount(ProjectForms,{props:{getProject:() => project,create:async () => {},hint:async () => {}}});
  try {
    await wrapper.vm.openHint(project.id);await settle();
    const imported = wrapper.get<HTMLDetailsElement>('#hint-imported').element;
    await wrapper.get('#hint-imported > summary').trigger('click');await settle();expect(imported.open).toBe(true);
    await wrapper.get('#hint-imported > summary').trigger('click');
    await wrapper.vm.openHint(project.id);await settle();
    expect(imported.open).toBe(false);expect(imported.style.height).toBe('');expect(wrapper.get('#hint-imported-count').text()).toBe('1');
    expect(wrapper.get('#hint-imported-files').text()).toContain('capture.txt');
    await wrapper.get('#hint-imported > summary').trigger('click');
    wrapper.get('#hint-dialog').element.dispatchEvent(new Event('close'));await settle();
    expect(imported.open).toBe(true);
  } finally {wrapper.unmount();}
});
