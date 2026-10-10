(function(root){'use strict';
  function buildState(project={}){return project.state||{graph:{project:project.raw||{},facts:[],intents:[],hints:[]},goals:[],steps:[],fact_records:[],findings:[],fact_relations:[],revision:0};}
  function create(host,project,options={}){const zoom=event=>options.onZoom?.(event.detail.zoom);host.addEventListener('graphzoom',zoom);const graph=new root.PwnMeshGraph(host,{onSelect:options.onSelect,onSelectEdge:options.onSelectEdge});graph.setState(buildState(project));const destroy=graph.destroy.bind(graph);graph.destroy=()=>{host.removeEventListener('graphzoom',zoom);destroy();};return graph;}
  root.PwnDemoGraph={buildState,create};
})(window);
