import { object, plain } from '@/ui/format';

export interface DetailRecord {
  title: string; kind: string; body: string; output?: string;
  meta: { label: string; value: string }[];
  evidence?: { title: string; body: string; path: string; reference?: string }[];
  nodeKey?: string; download?: { url: string; name: string };
}

export function selectionDetail(selected: unknown, kind: 'node' | 'edge', presentation?: { label: string; statusLabel: string }): DetailRecord {
  const item = object(selected), raw = object(item.raw), structured = object(raw.detail);
  const title = plain(item.label || item.title || item.description || item.id || '未命名记录');
  const body = plain(Object.keys(structured).length ? structured.body ?? structured.text ?? raw.detail : raw.detail || item.description || raw.description || raw.condition || raw.claim || title);
  const scope = plain(structured.scope || raw.scope);
  const items = structured.evidence ?? raw.evidence ?? item.evidence;
  const evidence = (Array.isArray(items) ? items : []).filter(value => value != null).map((value: unknown, index: number) => {
    const record = object(value);
    return typeof value === 'string' ? { title: `证据 ${String(index + 1).padStart(2, '0')}`, body: value, path: '' } : {
      title: plain(record.title || record.name || `证据 ${String(index + 1).padStart(2, '0')}`),
      body: plain(record.body ?? record.excerpt ?? record.description ?? value), path: plain(record.path || record.url),
      reference: [record.run_id, record.start_line != null ? `行 ${record.start_line}${record.end_line != null ? '–' + record.end_line : ''}` : ''].filter(Boolean).join(' · ')
    };
  });
  const meta = [{ label: '状态', value: presentation?.statusLabel || plain(item.statusLabel || item.status || '已记录') }];
  if (scope) meta.push({ label: '范围', value: scope });
  if (kind === 'edge') {
    const source = object(item.sourceNode), target = object(item.targetNode);
    meta.push({ label: '来源', value: plain(source.label || source.id || item.source) }, { label: '去向', value: plain(target.label || target.id || item.target) });
  }
  return { title, body, kind: kind === 'edge' ? '关系' : presentation?.label || '节点', meta, evidence,
    output: plain(structured.output ?? raw.execution_output ?? raw.output), nodeKey: kind === 'node' ? plain(item.key) : undefined };
}
