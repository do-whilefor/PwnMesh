(function (root, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.PwnDemoLifecycle = api;
})(typeof window === 'object' ? window : globalThis, function () {
  'use strict';
  const AVAILABLE = Object.freeze({
    running: ['pause', 'restart', 'terminate', 'delete'],
    paused: ['resume', 'restart', 'terminate', 'delete'],
    done: ['restart', 'delete'],
    terminated: ['restart', 'delete']
  });
  const statusOf = project => project?.status === 'pending' ? 'running' : project?.status;
  const generationOf = project => Number.isSafeInteger(project?.generation) && project.generation >= 0 ? project.generation : 0;
  function actions(project) {
    if (project?.archived) return [];
    const status = statusOf(project);
    return Object.hasOwn(AVAILABLE, status) ? [...AVAILABLE[status]] : [];
  }
  function canHint(project) { return !project?.archived && ['running', 'paused'].includes(statusOf(project)); }
  return Object.freeze({ actions, canHint });
});
