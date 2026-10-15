import { expect, test } from 'vitest';
import { validateFiles, validateHint, validateProject } from './project-model';
import { actions, canHint } from './lifecycle';
import { buildEntries } from './activity-data';
import type { LogRecord, ProjectStatus } from './types';

test('hint validation uses normalized lifecycle states and rejects archived rounds', () => {
  const text = {text: 'Additional authorized scope'};
  for (const status of ['running', 'paused', 'pending'] satisfies ProjectStatus[]) {
    expect(validateHint(text, {status, files: []})).toBe('');
    expect(validateHint(text, {status, archived: true, files: []})).toContain('已结束');
  }
  for (const status of ['done', 'terminated'] satisfies ProjectStatus[]) {
    expect(validateHint(text, {status, files: []})).toContain('已结束');
  }
  expect(validateHint(text)).toContain('不存在');
});
test('forms enforce upload sizes, counts and every server-disallowed control character', () => {
  expect(validateFiles([{name: 'large.zip', size: 257 * 1048576}])).toContain('256');
  expect(validateFiles(Array.from({length: 33}, (_, i) => ({name: i + '.txt', size: 1})))).toContain('32');
  expect(validateFiles([{name: 'c', size: 1}], [{name: 'a', size: 256 * 1048576}, {name: 'b', size: 256 * 1048576}])).toContain('512');
  for (const name of ['.', '..', ...Array.from({length: 32}, (_, i) => 'client' + String.fromCharCode(0x80 + i) + '.apk')]) {
    const files = [new File(['apk'], name)];
    expect(validateProject({name: 'Client test', type: 'pentest', target: 'authorized', goal: 'evidence', files})).toContain('文件名');
    expect(validateHint({text: 'scope', files}, {status: 'running', files: []})).toContain('文件名');
  }
  expect(validateFiles([new File(['apk'], '客户端 v1.0.apk')])).toBe('');
});
test('lifecycle preserves historical read-only and pending-as-running behavior', () => {
  expect(actions({status: 'pending'})).toEqual(actions({status: 'running'})); expect(canHint({status: 'pending'})).toBe(true);
  for (const flag of [{archived: true}, {readOnly: true}]) { expect(actions({status: 'running', ...flag})).toEqual([]); expect(canHint({status: 'running', ...flag})).toBe(false); }
  expect(canHint({status: 'done'})).toBe(false); expect(actions({status: 'done'})).toEqual(['restart', 'delete']);
});
test('activity preserves evidence and Shanghai timestamps without inventing missing times', () => {
  const base: LogRecord = {id: 'x', title: 'Real event', body: 'evidence', time: '2026-09-23T16:05:09Z', phase: 'Execute', level: 'info', worker: '', source: 'state', node: {type: 'fact', id: 'proof'}, artifacts: [{path: 'file.txt', excerpt: 'actual'}]};
  const entries = buildEntries({logs: [base, {...base, id: 'missing', time: ''}, {...base, id: 'invalid', time: 'invalid'}]});
  expect(entries.map(entry => entry.time)).toEqual(['—', '—', '00:05:09']); expect(entries[2]?.key).toBe('fact:proof'); expect(entries[2]?.evidence).toBe('file.txt\nactual');
});
