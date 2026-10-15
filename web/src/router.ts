import { createRouter, createWebHashHistory } from 'vue-router';
import WorkspacePage from './workspace/WorkspacePage.vue';
import ModelSettingsPage from './model-settings/ModelSettingsPage.vue';

if (!location.hash) {
  try { const id = new URLSearchParams(location.search).get('project') || localStorage.getItem('pwnmesh.selected-project'); if (id) history.replaceState(history.state, '', location.pathname + location.search + '#/project/' + encodeURIComponent(id)); } catch { /* Navigation remains available when storage is disabled. */ }
}
function normalizeHash() {
  const hash = location.hash.slice(1);
  if (hash && !hash.startsWith('/')) history.replaceState(history.state, '', location.pathname + location.search + '#/' + hash);
}
normalizeHash();
window.addEventListener('hashchange', normalizeHash);
export const router = createRouter({ history: createWebHashHistory(), routes: [
  { path: '/projects', name: 'projects', component: WorkspacePage },
  { path: '/project/:id', name: 'project', component: WorkspacePage },
  { path: '/llm', name: 'models', component: ModelSettingsPage },
  { path: '/:pathMatch(.*)*', redirect: '/projects' }
] });
