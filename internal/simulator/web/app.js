const els = Object.fromEntries([...document.querySelectorAll('[id]')].map(el => [el.id, el]));
let lastSpeedSent = 1;

async function api(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { 'Content-Type': 'application/json', ...(options.headers || {}) } });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
  return body;
}

function showError(error) {
  els.notice.textContent = error.message;
  setTimeout(() => { els.notice.textContent = ''; }, 5000);
}

function render(status) {
  const live = status.running && status.phase === 'running';
  const busy = status.running && !live;
  els.phase.textContent = status.phase.charAt(0).toUpperCase() + status.phase.slice(1);
  els['state-dot'].className = live ? 'live' : busy ? 'busy' : '';
  els.start.disabled = status.running;
  els.stop.disabled = !status.running;
  els.workers.textContent = status.workers;
  els.teams.textContent = status.teams;
  els.projects.textContent = status.projects;
  els.actions.textContent = status.stats.total_actions;
  els.errors.textContent = `${status.stats.errors} errors`;
  els.todo.textContent = status.tasks.todo || 0;
  els['in-progress'].textContent = status.tasks.in_progress || 0;
  els.done.textContent = status.tasks.done || 0;
  const taskTotal = (status.tasks.todo || 0) + (status.tasks.in_progress || 0) + (status.tasks.done || 0);
  els['task-total'].textContent = `${taskTotal} tasks`;
  els['effective-rate'].textContent = `${formatNumber(status.effective_actions_per_minute)} actions/min`;
  els.session.textContent = status.session ? `Session ${status.session}` : 'No active session';
  if (document.activeElement !== els.speed) els.speed.value = status.speed;
  els['speed-label'].textContent = `${formatNumber(status.speed)}×`;

  const actions = Object.entries(status.stats.actions).sort((a, b) => b[1] - a[1]);
  const max = Math.max(1, ...actions.map(([, value]) => value));
  els['action-mix'].innerHTML = actions.length ? actions.map(([name, value]) => `
    <div class="mix-row"><span>${label(name)}</span><div class="bar"><span style="width:${value / max * 100}%"></span></div><strong>${value}</strong></div>
  `).join('') : '<p class="empty">Start a workday to see traffic.</p>';

  els.activity.innerHTML = status.recent.length ? status.recent.map(event => `
    <div class="event ${event.success ? '' : 'failed'}"><time>${new Date(event.at).toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', second:'2-digit'})}</time><b>${label(event.action)}</b><span>${escapeHTML(event.detail)}</span></div>
  `).join('') : '<p class="empty">Synthetic activity will appear here.</p>';
}

function label(value) { return value.replaceAll('_', ' ').replace(/\b\w/g, x => x.toUpperCase()); }
function formatNumber(value) { return Number(value).toLocaleString(undefined, { maximumFractionDigits: 2 }); }
function escapeHTML(value) { return String(value).replace(/[&<>'"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c])); }

els.start.addEventListener('click', async () => { try { render(await api('/api/start', { method: 'POST' })); } catch (error) { showError(error); } });
els.stop.addEventListener('click', async () => { try { render(await api('/api/stop', { method: 'POST' })); } catch (error) { showError(error); } });
els.speed.addEventListener('input', () => { els['speed-label'].textContent = `${formatNumber(els.speed.value)}×`; els['effective-rate'].textContent = `${formatNumber(30 * Number(els.speed.value))} actions/min`; });
els.speed.addEventListener('change', async () => {
  const multiplier = Number(els.speed.value);
  if (multiplier === lastSpeedSent) return;
  try { const status = await api('/api/speed', { method: 'PATCH', body: JSON.stringify({ multiplier }) }); lastSpeedSent = multiplier; render(status); } catch (error) { showError(error); }
});

async function refresh() { try { const status = await api('/api/status'); lastSpeedSent = status.speed; render(status); } catch (error) { showError(error); } }
refresh();
setInterval(refresh, 1000);
