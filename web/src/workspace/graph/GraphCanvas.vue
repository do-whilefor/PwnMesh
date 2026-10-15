<script setup lang="ts">
import { onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, watch } from 'vue';
import type { WorkspaceSnapshot } from '../../api/types';
import { GraphController } from './controller';
import type { EdgeSelection, GraphEdgeDetails, GraphLayout, GraphNode, NodeSelection } from './types';

const props = defineProps<{ state: WorkspaceSnapshot | null; projectLayouts?: Map<string, GraphLayout> }>();
const emit = defineEmits<{
  'select-node': [node: GraphNode | null];
  'select-edge': [edge: GraphEdgeDetails | null];
  zoom: [value: number];
}>();
const host = ref<HTMLDivElement>();
const localLayouts = new Map<string, GraphLayout>();
let controller: GraphController | undefined;
let retained: { key: string; node: string | null; edge: string | null; filter: string } | undefined;
const onZoom = (event: Event) => emit('zoom', (event as CustomEvent<{zoom: number}>).detail.zoom);

function activate() {
  if (!host.value || controller) return;
  host.value.addEventListener('graphzoom', onZoom);
  controller = new GraphController(host.value, {
    projectLayouts: props.projectLayouts ?? localLayouts,
    onSelect: node => emit('select-node', node),
    onSelectEdge: edge => emit('select-edge', controller?.getEdgeDetails(edge) ?? null),
  });
  controller.setState(props.state);
  if (retained?.key === controller.cacheKey()) {
    controller.setStatusFilter(retained.filter);
    if (retained.node) controller.selectNode(retained.node);
    else if (retained.edge) controller.selectEdge(retained.edge);
    if (!controller.selected && !controller.selectedEdge) controller.clearSelection();
  } else if (retained) {
    emit('select-node', null);
    emit('select-edge', null);
  }
}
function deactivate() {
  if (!controller) return;
  retained = {key:controller.cacheKey(), node:controller.selected, edge:controller.selectedEdge, filter:controller.getStatusFilter()};
  host.value?.removeEventListener('graphzoom', onZoom);
  controller.destroy();
  controller = undefined;
}
onMounted(activate);
onActivated(activate);
onDeactivated(deactivate);
onBeforeUnmount(deactivate);
watch(() => props.state, state => controller?.setState(state));

defineExpose({
  focusNode: (selection: NodeSelection) => controller?.focusNode(selection),
  selectNode: (selection: NodeSelection) => controller?.selectNode(selection),
  clearSelection: () => controller?.clearSelection(),
  setStatusFilter: (status: string) => controller?.setStatusFilter(status),
  getStatusFilter: () => controller?.getStatusFilter() ?? 'all',
  zoomBy: (factor: number) => controller?.zoomBy(factor),
  fit: () => controller?.fit(),
  getEdgeDetails: (edge: EdgeSelection) => controller?.getEdgeDetails(edge) ?? null,
});
</script>

<template>
  <div id="graph-host" ref="host" class="graph-host"></div>
</template>
