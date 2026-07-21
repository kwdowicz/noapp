const state = {
  projects: [], users: [], projectId: null,
  auth: { config: null, accessToken: '', refreshToken: '', idToken: '', expiresAt: 0, claims: null, idClaims: null },
};
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
  await refreshTokenIfNeeded();
  const response = await fetch(path, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${state.auth.accessToken}`,
      ...(options.headers || {}),
    },
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
  const disabled = canEdit() ? '' : ' disabled title="Viewer access is read-only"';
  return `<section class="card"><h3>${escapeHTML(task.title)}</h3>${task.description ? `<p>${escapeHTML(task.description)}</p>` : ''}<footer><span class="avatar" title="${escapeHTML(task.assignee_name || 'Unassigned')}">${escapeHTML(initials)}</span><select aria-label="Status for ${escapeHTML(task.title)}" data-task-status="${task.id}"${disabled}>${statuses.map(s => `<option value="${s.id}" ${s.id === task.status ? 'selected' : ''}>${s.label}</option>`).join('')}</select></footer></section>`;
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
document.querySelector('#logout-button').addEventListener('click', logout);

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
    await initializeAuthentication();
    renderIdentity();
    state.users = await api('/api/users');
    const assignee = document.querySelector('[name="assignee_id"]');
    assignee.insertAdjacentHTML('beforeend', state.users.map(u => `<option value="${u.id}">${escapeHTML(u.name)}</option>`).join(''));
    await loadProjects();
  } catch (error) { showError(error); }
}

start();

async function initializeAuthentication() {
  const configResponse = await fetch('/api/auth/config');
  if (!configResponse.ok) throw new Error('Could not load authentication configuration');
  state.auth.config = await configResponse.json();

  const params = new URLSearchParams(window.location.search);
  if (params.has('error')) {
    throw new Error(params.get('error_description') || params.get('error'));
  }
  if (!params.has('code')) {
    await beginLogin();
    return new Promise(() => {});
  }

  const pending = JSON.parse(sessionStorage.getItem('noapp-oidc-pending') || 'null');
  sessionStorage.removeItem('noapp-oidc-pending');
  if (!pending || pending.state !== params.get('state')) {
    throw new Error('Login state validation failed');
  }
  const tokens = await tokenRequest({
    grant_type: 'authorization_code',
    client_id: state.auth.config.client_id,
    code: params.get('code'),
    redirect_uri: redirectURI(),
    code_verifier: pending.verifier,
  });
  applyTokens(tokens);
  if (state.auth.idClaims?.nonce !== pending.nonce) throw new Error('Login nonce validation failed');
  window.history.replaceState({}, document.title, '/');
}

async function beginLogin() {
  const verifier = randomValue(64);
  const pending = { verifier, state: randomValue(32), nonce: randomValue(32) };
  sessionStorage.setItem('noapp-oidc-pending', JSON.stringify(pending));
  const challengeBytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier));
  const challenge = base64URL(new Uint8Array(challengeBytes));
  const url = new URL(`${state.auth.config.issuer}/protocol/openid-connect/auth`);
  url.search = new URLSearchParams({
    client_id: state.auth.config.client_id,
    redirect_uri: redirectURI(),
    response_type: 'code',
    scope: 'openid profile email',
    code_challenge: challenge,
    code_challenge_method: 'S256',
    state: pending.state,
    nonce: pending.nonce,
  });
  window.location.assign(url);
}

async function refreshTokenIfNeeded() {
  if (state.auth.expiresAt - Date.now() > 30000) return;
  if (!state.auth.refreshToken) {
    await beginLogin();
    return new Promise(() => {});
  }
  try {
    applyTokens(await tokenRequest({
      grant_type: 'refresh_token',
      client_id: state.auth.config.client_id,
      refresh_token: state.auth.refreshToken,
    }));
  } catch (_) {
    await beginLogin();
    return new Promise(() => {});
  }
}

async function tokenRequest(values) {
  const response = await fetch(`${state.auth.config.issuer}/protocol/openid-connect/token`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams(values),
  });
  const result = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(result.error_description || 'Token exchange failed');
  return result;
}

function applyTokens(tokens) {
  state.auth.accessToken = tokens.access_token;
  state.auth.refreshToken = tokens.refresh_token || state.auth.refreshToken;
  state.auth.idToken = tokens.id_token || state.auth.idToken;
  state.auth.expiresAt = Date.now() + (tokens.expires_in * 1000);
  state.auth.claims = parseJWT(tokens.access_token);
  state.auth.idClaims = parseJWT(state.auth.idToken);
}

function parseJWT(token) {
  let encoded = token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/');
  encoded += '='.repeat((4 - (encoded.length % 4)) % 4);
  const json = decodeURIComponent(atob(encoded).split('').map(char => `%${char.charCodeAt(0).toString(16).padStart(2, '0')}`).join(''));
  return JSON.parse(json);
}

function canEdit() {
  return state.auth.claims?.realm_access?.roles?.includes('noapp-editor');
}

function renderIdentity() {
  const name = state.auth.claims.preferred_username || state.auth.claims.sub;
  const role = canEdit() ? 'editor' : 'viewer';
  document.querySelector('#signed-in-user').textContent = `${name} · ${role}`;
  document.querySelector('#new-project-button').hidden = !canEdit();
  document.querySelector('#new-task-button').hidden = !canEdit();
}

function logout() {
  const url = new URL(`${state.auth.config.issuer}/protocol/openid-connect/logout`);
  url.search = new URLSearchParams({ id_token_hint: state.auth.idToken, post_logout_redirect_uri: redirectURI() });
  window.location.assign(url);
}

function redirectURI() {
  return `${window.location.origin}/`;
}

function randomValue(bytes) {
  const value = new Uint8Array(bytes);
  crypto.getRandomValues(value);
  return base64URL(value);
}

function base64URL(value) {
  return btoa(String.fromCharCode(...value)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
