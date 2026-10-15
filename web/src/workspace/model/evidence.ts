import type { EntityDTO } from '../../api/types';

/** Protocol 1 omits false support; legacy external feedback is a human input. */
export function stepSupportValid(step: Pick<EntityDTO, 'description' | 'support_valid'>, result: Pick<EntityDTO, 'legacy' | 'source_step_id'> | null | undefined, version: number | undefined): boolean | null {
  const feedback = step.description === 'external_feedback' && result?.legacy === true && !result.source_step_id;
  if (version === 1 && !feedback) return step.support_valid === true;
  return typeof step.support_valid === 'boolean' ? step.support_valid : null;
}

export function sourceSupport(record: Pick<EntityDTO, 'support_valid' | 'sources'>, facts: ReadonlyMap<string, EntityDTO>): boolean {
  if (typeof record.support_valid === 'boolean') return record.support_valid;
  const sources = (Array.isArray(record.sources) ? record.sources : []).filter(id => typeof id === 'string' && id);
  return sources.length > 0 && sources.every(id => {
    const fact = facts.get(id);
    return fact?.status === 'valid' && fact.support_invalid !== true;
  });
}
