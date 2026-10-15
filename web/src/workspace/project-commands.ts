import { APIError } from '../api/client';
import type { CreatedProjectDTO } from '../api/types';
import { validateProject } from './model/data';
import type { CreateProjectValues, HintValues, ProjectAction } from './model/types';
import { pathFor, ProjectResource } from './project-resource';

export class CommandError extends APIError {
  constructor(message: string, status = 0, readonly pendingCreate = false, readonly uncertainCreate = false) { super(message, status); this.name = 'CommandError'; }
}
interface PendingCreate { created: CreatedProjectDTO; files: File[]; uploaded: number }
interface PendingHint extends HintValues { key: string }
const asError = (error: unknown): Error => error instanceof Error ? error : new Error(String(error));
const statusOf = (error: unknown): number => error instanceof APIError ? error.status : -1;

/** Commands retain incomplete operations independently of dialog lifetimes. */
export class ProjectCommands {
  private submitting = false;
  private creation: PendingCreate | null = null;
  private uncertain = false;
  private readonly pendingHints = new Map<string, PendingHint>();
  constructor(private readonly resource: ProjectResource) {}
  get busy(): boolean { return this.submitting; }
  get pendingCreate(): Readonly<PendingCreate> | null { return this.creation; }
  get uncertainCreate(): boolean { return this.uncertain; }
  private async run<T>(work: () => Promise<T>): Promise<T> {
    if (this.submitting) throw new Error('正在处理上一项操作，请稍后重试。');
    this.submitting = true; this.resource.cancel();
    try { return await work(); } finally { this.submitting = false; }
  }
  async change(id: string, action: ProjectAction, generation: number): Promise<unknown> {
    return this.run(async () => {
      const base = pathFor(id), api = this.resource.api;
      let result: unknown;
      if (action === 'delete') {
        result = await api.request(base, {method: 'DELETE'}); this.resource.invalidate(id); this.pendingHints.delete(id);
      } else if (action === 'pause' || action === 'resume') {
        result = await api.request(base + '/status', {method: 'PUT', body: {status: action === 'pause' ? 'stopped' : 'active', expected_generation: generation}});
      } else {
        if (!['restart', 'terminate'].includes(action)) throw new Error('不支持的项目操作。');
        result = await api.request(base + '/' + action, {method: 'POST', body: {expected_generation: generation}});
        this.resource.invalidate(id);
      }
      if (action !== 'pause' && this.creation?.created.project.id === id) this.creation = null;
      return result;
    });
  }
  async create(values: CreateProjectValues): Promise<string> {
    return this.run(async () => {
      if (this.uncertain) throw new CommandError('上次创建结果尚未确认。请关闭窗口并在项目列表核对，刷新页面后再创建。', 0, false, true);
      const payload = validateProject({title: values.name, origin: values.target, goal: values.goal, scenario: values.type});
      const api = this.resource.api;
      if (this.creation) {
        const pending = this.creation;
        try {
          const identity = await api.request<{generation?: number}>(pathFor(pending.created.project.id) + '/identity');
          if ((identity.generation || 0) !== (pending.created.project.generation || 0)) throw new APIError('项目已进入新轮次。', 409);
        } catch (error) {
          if ([404, 409].includes(statusOf(error))) {
            this.creation = null;
            throw new CommandError(asError(error).message + ' 创建续传已取消，请在项目列表核对已保存的项目。', statusOf(error));
          }
          throw new CommandError(asError(error).message + '；无法确认项目轮次，请稍后重试。', statusOf(error), true);
        }
      }
      if (!this.creation) {
        let created: CreatedProjectDTO;
        try { created = await api.request<CreatedProjectDTO>('/projects', {method: 'POST', body: values.files.length ? {...payload, start_paused: true} : payload}); }
        catch (error) {
          if (statusOf(error) === 0) { this.uncertain = true; throw new CommandError(asError(error).message + '；创建结果未确认，请关闭窗口核对项目列表，避免重复创建。', 0, false, true); }
          throw error;
        }
        this.creation = {created, files: values.files.slice(), uploaded: 0};
      }
      const pending = this.creation, id = pending.created.project.id;
      try {
        for (; pending.uploaded < pending.files.length; pending.uploaded++) await api.uploadInput(pathFor(id), pending.files[pending.uploaded]!);
        if (pending.files.length) await api.request(pathFor(id) + '/status', {method: 'PUT', body: {status: 'active', expected_generation: pending.created.project.generation || 0}});
        this.creation = null; return id;
      } catch (error) {
        if ([404, 409].includes(statusOf(error))) {
          this.creation = null;
          throw new CommandError(asError(error).message + '；项目状态已变化，创建续传已取消，请核对项目列表。', statusOf(error));
        }
        throw new CommandError(asError(error).message + '；项目已保存，已导入材料保留。重试将继续上传并启动同一项目。', statusOf(error), true);
      }
    });
  }
  isHintRetry(id: string, values: HintValues): boolean {
    const pending = this.pendingHints.get(id);
    return !!pending && pending.text === values.text && pending.files.length === values.files.length && pending.files.every((file, index) => file === values.files[index]);
  }
  async hint(id: string, values: HintValues): Promise<unknown> {
    return this.run(async () => {
      let pending = this.pendingHints.get(id);
      if (!pending || !this.isHintRetry(id, values)) {
        const key = crypto.randomUUID?.() || Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('');
        pending = {text: values.text, files: values.files.slice(), key}; this.pendingHints.set(id, pending);
      }
      const result = await this.resource.api.uploadInputs(pathFor(id), values.files, values.text, pending.key);
      if (this.pendingHints.get(id) === pending) this.pendingHints.delete(id);
      return result;
    });
  }
}
