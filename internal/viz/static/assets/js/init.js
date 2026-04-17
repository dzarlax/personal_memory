// Router + bootstrap. Loaded last so all view-module functions are defined.

const VIEWS = ['overview', 'duplicates', 'forgotten', 'timeline', 'graph', 'documents'];

function parseTabFromPath() {
  const path = window.location.pathname;
  const rel = path.startsWith(BASE) ? path.slice(BASE.length) : path;
  const segments = rel.split('/').filter(Boolean);
  const tab = segments[0] || 'overview';
  return VIEWS.includes(tab) ? tab : 'overview';
}

function isTabAvailable(name) {
  const el = document.querySelector(`.tab-nav-item[data-view="${name}"]`);
  return el && el.style.display !== 'none';
}

function activateTab(name, pushHistory = true) {
  if (!isTabAvailable(name)) name = 'overview';

  document.querySelectorAll('.tab-nav-item').forEach(t => t.classList.remove('active'));
  document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
  const tab = document.querySelector(`.tab-nav-item[data-view="${name}"]`);
  if (tab) tab.classList.add('active');
  const view = document.getElementById(name + '-view');
  if (view) view.classList.add('active');

  // Lazy-load per-tab heavy content once.
  if (name === 'duplicates' && !document.querySelector('.dup-pair')) loadDuplicates();
  if (name === 'timeline' && !timeline) loadTimeline();
  if (name === 'graph' && !network) loadGraph();

  if (pushHistory) {
    const targetPath = `${BASE}/${name}`;
    if (window.location.pathname !== targetPath) {
      window.history.pushState({ tab: name }, '', targetPath);
    }
  }
}

// Tab clicks switch view AND update URL. Anchors have real hrefs for
// progressive enhancement (Cmd+click opens in new tab), so intercept
// the default nav for in-page routing.
document.querySelectorAll('.tab-nav-item').forEach(tab => {
  tab.addEventListener('click', (e) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey) return; // let the browser handle
    e.preventDefault();
    activateTab(tab.dataset.view);
  });
});

// Browser back/forward.
window.addEventListener('popstate', () => activateTab(parseTabFromPath(), false));

// Initial bootstrap. Activate the target tab synchronously so the page
// renders immediately; data fetches happen in parallel and fill the views
// as they resolve.
activateTab(parseTabFromPath(), false);
loadFacts();
loadDocuments();
