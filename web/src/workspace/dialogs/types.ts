import type {ProjectAction, ProjectStatus} from '../model/types';
export interface ProjectOperation {id: string; action: ProjectAction; generation: number; status: ProjectStatus}
export const actionNames: Record<ProjectAction, string> = {pause: '暂停', resume: '继续', restart: '重启', terminate: '终止', delete: '删除'};
export const actionIcons: Record<ProjectAction, string> = {pause: 'pause', resume: 'play', restart: 'rotate-ccw', terminate: 'square', delete: 'trash-2'};
export function errorMessage(error: unknown, fallback: string): string {return error instanceof Error ? error.message : fallback;}
export function errorFlag(error: unknown, key: string): boolean {return typeof error === 'object' && error !== null && key in error && Reflect.get(error, key) === true;}
