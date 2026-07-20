const state = { projects: [], users: [], projectId: null };
const statuses = [
  { id: 'todo', label: 'To do' },
  { id: 'in_progress', label: 'In progress' },
  { id: 'done', label: 'Done' },
];

const projectSelect = document.querySelector('#project-select');
const description = document.querySelector('#project-description');
const board = document.querySelector('#board');
const notice = document.querySelector('#notice');

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
  return body;
}

function showError(error) {
  notice.textContent = error.message;
  window.setTimeout(() => { notice.textContent = ''; }, 5000);
}

async function loadProjects(preferredId) {
  state.projects = await api('/api/projects');
  projectSelect.innerHTML = state.projects.map(p => `<option value="${p.id}">${escapeHTML(p.name)} (${p.task_count})</option>`).join('');
  state.projectId = preferredId || state.projectId || state.projects[0]?.id || null;
  if (state.projectId) projectSelect.value = state.projectId;
  const project = state.projects.find(p => p.id === Number(state.projectId));
  description.textContent = project?.description || '';
  await loadBoard();
}

async function loadBoard() {
  if (!state.projectId) {
    board.innerHTML = '<p class="empty">Create a project to get started.</p>';
    return;
  }
  const tasks = await api(`/api/projects/${state.projectId}/tasks`);
  board.innerHTML = statuses.map(status => {
    const matching = tasks.filter(task => task.status === status.id);
    return `<article class="column"><h2>${status.label}<span class="count">${matching.length}</span></h2><div class="cards">${matching.map(taskCard).join('') || '<p class="empty">No tasks</p>'}</div></article>`;
  }).join('');
  board.querySelectorAll('[data-task-status]').forEach(select => {
    select.addEventListener('change', async () => {
      try {
        await api(`/api/tasks/${select.dataset.taskStatus}/status`, { method: 'PATCH', body: JSON.stringify({ status: select.value }) });
        await loadProjects(state.projectId);
      } catch (error) { showError(error); }
    });
  });
}

function taskCard(task) {
  const initials = task.assignee_name ? task.assignee_name.split(/\s+/).map(x => x[0]).slice(0, 2).join('') : '?';
  return `<section class="card"><h3>${escapeHTML(task.title)}</h3>${task.description ? `<p>${escapeHTML(task.description)}</p>` : ''}<footer><span class="avatar" title="${escapeHTML(task.assignee_name || 'Unassigned')}">${escapeHTML(initials)}</span><select aria-label="Status for ${escapeHTML(task.title)}" data-task-status="${task.id}">${statuses.map(s => `<option value="${s.id}" ${s.id === task.status ? 'selected' : ''}>${s.label}</option>`).join('')}</select></footer></section>`;
}

function escapeHTML(value) {
  return String(value).replace(/[&<>'"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[c]));
}

projectSelect.addEventListener('change', async () => {
  state.projectId = Number(projectSelect.value);
  const project = state.projects.find(p => p.id === state.projectId);
  description.textContent = project?.description || '';
  try { await loadBoard(); } catch (error) { showError(error); }
});

document.querySelector('#new-project-button').addEventListener('click', () => document.querySelector('#project-dialog').showModal());
document.querySelector('#new-task-button').addEventListener('click', () => {
  if (!state.projectId) return showError(new Error('Create a project first.'));
  document.querySelector('#task-dialog').showModal();
});
document.querySelectorAll('.cancel').forEach(button => button.addEventListener('click', () => button.closest('dialog').close()));

document.querySelector('#project-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = event.currentTarget;
  try {
    const project = await api('/api/projects', { method: 'POST', body: JSON.stringify(Object.fromEntries(new FormData(form))) });
    form.reset(); form.closest('dialog').close(); await loadProjects(project.id);
  } catch (error) { showError(error); }
});

document.querySelector('#task-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = event.currentTarget;
  const input = Object.fromEntries(new FormData(form));
  input.assignee_id = input.assignee_id ? Number(input.assignee_id) : null;
  try {
    await api(`/api/projects/${state.projectId}/tasks`, { method: 'POST', body: JSON.stringify(input) });
    form.reset(); form.closest('dialog').close(); await loadProjects(state.projectId);
  } catch (error) { showError(error); }
});

async function start() {
  try {
    state.users = await api('/api/users');
    const assignee = document.querySelector('[name="assignee_id"]');
    assignee.insertAdjacentHTML('beforeend', state.users.map(u => `<option value="${u.id}">${escapeHTML(u.name)}</option>`).join(''));
    await loadProjects();
  } catch (error) { showError(error); }
}

start();
