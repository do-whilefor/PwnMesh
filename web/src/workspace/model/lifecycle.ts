import type { ProjectAction } from './types';
const AVAILABLE: Readonly<Record<string, readonly ProjectAction[]>> = {
  running: ['pause', 'restart', 'terminate', 'delete'], paused: ['resume', 'restart', 'terminate', 'delete'],
  done: ['restart', 'delete'], terminated: ['restart', 'delete'],
};
type ProjectLifecycle = {status?: string; archived?: boolean; readOnly?: boolean};
const statusOf = (project?: ProjectLifecycle | null): string => project?.status === 'pending' ? 'running' : project?.status || '';
export function actions(project?: ProjectLifecycle | null): ProjectAction[] {
  if (project?.archived || project?.readOnly) return [];
  return Object.hasOwn(AVAILABLE, statusOf(project)) ? [...AVAILABLE[statusOf(project)]!] : [];
}
export function canHint(project?: ProjectLifecycle | null): boolean {
  return !project?.archived && !project?.readOnly && ['running', 'paused'].includes(statusOf(project));
}
