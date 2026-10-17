// @vitest-environment happy-dom
import {flushPromises, mount} from '@vue/test-utils';
import {defineComponent, h, KeepAlive} from 'vue';
import {afterEach, beforeEach, expect, it, vi} from 'vitest';
import RecordReader from './RecordReader.vue';
import type {DetailRecord} from './detail';

const record = (title = 'Log record'): DetailRecord => ({title, kind: 'Log', body: 'Full content', meta: [], nodeKey: 'step:one'});
const settle = async () => {await vi.advanceTimersByTimeAsync(301); await flushPromises();};
function opener() {
  const button = document.createElement('button');
  document.body.append(button); button.focus(); return button;
}
beforeEach(() => {
  vi.useFakeTimers({toFake: ['setTimeout', 'clearTimeout', 'Date']});
  vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (this: HTMLDialogElement) {this.open = true;});
  vi.spyOn(HTMLDialogElement.prototype, 'close').mockImplementation(function (this: HTMLDialogElement) {this.open = false; this.dispatchEvent(new Event('close'));});
});
afterEach(() => {vi.useRealTimers(); document.body.replaceChildren();});

it('animates reader entry and Escape while retaining content and restoring the opener', async () => {
  const origin = opener(), wrapper = mount(RecordReader, {attachTo: document.body});
  try {
    await wrapper.vm.open(record());
    const dialog = wrapper.get<HTMLDialogElement>('dialog').element;
    expect(dialog.dataset.motion).toBe('enter'); expect(origin.getAttribute('aria-expanded')).toBe('true');
    await settle();
    const cancel = new Event('cancel', {cancelable: true}); dialog.dispatchEvent(cancel);
    expect(cancel.defaultPrevented).toBe(true); expect(dialog.open).toBe(true);
    expect(dialog.dataset.motion).toBe('leave'); expect(dialog.inert).toBe(true);
    expect(wrapper.get('.inspector-reader-content').text()).toContain('Full content');
    await settle();
    expect(dialog.open).toBe(false); expect(dialog.inert).toBe(false);
    expect(origin.getAttribute('aria-expanded')).toBe('false'); expect(document.activeElement).toBe(origin);
    expect(wrapper.emitted('closed')).toHaveLength(1);
  } finally {wrapper.unmount();}
});

it('reverses rapid closes and reopens without stale close events clearing the current reader', async () => {
  const origin = opener(), wrapper = mount(RecordReader, {attachTo: document.body});
  try {
    await wrapper.vm.open(record());
    const dialog = wrapper.get<HTMLDialogElement>('dialog').element;
    wrapper.get<HTMLButtonElement>('[data-inspector-action="close"]').element.focus();
    wrapper.vm.close(); await wrapper.vm.open(record('Latest record'));
    dialog.dispatchEvent(new Event('close'));
    expect(origin.getAttribute('aria-expanded')).toBe('true');
    await settle();
    expect(dialog.open).toBe(true); expect(dialog.inert).toBe(false);
    expect(wrapper.get('h2').text()).toBe('Latest record'); expect(wrapper.emitted('closed')).toBeUndefined();
    wrapper.vm.close(); wrapper.vm.close(); await settle();
    expect(dialog.open).toBe(false); expect(document.activeElement).toBe(origin);
    expect(wrapper.emitted('closed')).toHaveLength(1);
  } finally {wrapper.unmount();}
});

it('cancels pending asynchronous opens when closed or unmounted', async () => {
  const origin = opener(), wrapper = mount(RecordReader, {attachTo: document.body});
  const pending = wrapper.vm.open(record()); wrapper.vm.close(); await pending; await settle();
  expect(wrapper.get<HTMLDialogElement>('dialog').element.open).toBe(false);
  expect(origin.getAttribute('aria-expanded')).toBe('false'); expect(origin.classList.contains('is-reading')).toBe(false);
  const second = wrapper.vm.open(record()); wrapper.unmount(); await second; await settle();
  expect(HTMLDialogElement.prototype.showModal).not.toHaveBeenCalled();
  expect(origin.getAttribute('aria-expanded')).toBe('false');
});

it('clears the prior opener and closes node navigation immediately', async () => {
  const first = opener(), wrapper = mount(RecordReader, {attachTo: document.body});
  try {
    await wrapper.vm.open(record()); const second = opener(); await wrapper.vm.open(record('Second record'));
    expect(first.getAttribute('aria-expanded')).toBe('false'); expect(first.classList.contains('is-reading')).toBe(false);
    await wrapper.get('[data-inspector-action="locate"]').trigger('click');
    expect(wrapper.get<HTMLDialogElement>('dialog').element.open).toBe(false);
    expect(second.getAttribute('aria-expanded')).toBe('false');
    expect(wrapper.emitted('locate')).toEqual([['step:one']]);
    await settle(); expect(wrapper.emitted('closed')).toHaveLength(1);
  } finally {wrapper.unmount();}
});

it('dismisses by backdrop without motion when reduced motion is enabled', async () => {
  vi.spyOn(window, 'matchMedia').mockReturnValue({matches: true} as MediaQueryList);
  const origin = opener(), wrapper = mount(RecordReader, {attachTo: document.body});
  try {
    await wrapper.vm.open(record()); const dialog = wrapper.get<HTMLDialogElement>('dialog').element;
    expect(dialog.dataset.motion).toBeUndefined();
    dialog.dispatchEvent(new MouseEvent('click', {clientX: -1, clientY: -1, bubbles: true}));
    expect(dialog.open).toBe(false); expect(origin.getAttribute('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(origin);
  } finally {wrapper.unmount();}
});

it('closes a cached reader immediately on deactivation and invalidates pending entry', async () => {
  const origin = opener();
  const host = defineComponent({props: {active: Boolean}, setup: props => () => h(KeepAlive, {}, () => props.active ? h(RecordReader) : null)});
  const wrapper = mount(host, {attachTo: document.body, props: {active: true}});
  try {
    const reader = wrapper.getComponent(RecordReader); await reader.vm.open(record());
    const dialog = reader.get<HTMLDialogElement>('dialog').element;
    await wrapper.setProps({active: false});
    expect(dialog.open).toBe(false); expect(origin.getAttribute('aria-expanded')).toBe('false');
    await wrapper.setProps({active: true});
    const pending = reader.vm.open(record()); await wrapper.setProps({active: false}); await pending; await settle();
    expect(dialog.open).toBe(false); expect(origin.classList.contains('is-reading')).toBe(false);
  } finally {wrapper.unmount();}
});
