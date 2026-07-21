const baseURL = process.env.NOAPP_URL || 'http://localhost:8080';
const tokenURL = process.env.NOAPP_TOKEN_URL || 'http://localhost:8082/realms/noapp/protocol/openid-connect/token';

async function requestToken(clientId, clientSecret) {
  const response = await fetch(tokenURL, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ grant_type: 'client_credentials', client_id: clientId, client_secret: clientSecret }),
  });
  if (!response.ok) throw new Error(`Token request for ${clientId} failed: ${response.status}`);
  return (await response.json()).access_token;
}

const primary = {
  name: process.env.NOAPP_CLIENT_ID || 'noapp-cli',
  token: await requestToken(process.env.NOAPP_CLIENT_ID || 'noapp-cli', process.env.NOAPP_CLIENT_SECRET || 'noapp-cli-dev-secret'),
};
const observer = {
  name: process.env.NOAPP_OBSERVER_CLIENT_ID || 'noapp-simulator',
  token: await requestToken(process.env.NOAPP_OBSERVER_CLIENT_ID || 'noapp-simulator', process.env.NOAPP_OBSERVER_CLIENT_SECRET || 'noapp-simulator-dev-secret'),
};

async function api(path, options = {}) {
  const response = await fetch(`${baseURL}${path}`, {
    ...options,
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${primary.token}`, ...(options.headers || {}) },
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `API request failed: ${response.status}`);
  return body;
}

const projects = await api('/api/projects');
if (!projects.length) throw new Error('The smoke test requires at least one project');
const projectId = projects[0].id;
const tasks = await api(`/api/projects/${projectId}/tasks`);
if (!tasks.length) throw new Error('The smoke test requires at least one task');
const original = tasks[0];
const changedStatus = original.status === 'done' ? 'todo' : 'done';
const websocketURL = new URL('/api/realtime', baseURL.replace(/^http/, 'ws'));

await new Promise((resolve, reject) => {
  const clients = [primary, observer].map(identity => ({ ...identity, socket: new WebSocket(websocketURL), subscribed: false, changed: false, restored: false }));
  let changeStarted = false;
  let restoreStarted = false;
  let finished = false;
  const timeout = setTimeout(() => finish(new Error('Timed out waiting for two-client realtime task events')), 20000);

  function finish(error) {
    if (finished) return;
    finished = true;
    clearTimeout(timeout);
    clients.forEach(client => client.socket.close(error ? 1011 : 1000, error ? 'smoke test failed' : 'smoke test complete'));
    if (error) reject(error); else resolve();
  }

  async function changeStatus(status) {
    await api(`/api/tasks/${original.id}/status`, { method: 'PATCH', body: JSON.stringify({ status }) });
  }

  for (const client of clients) {
    client.socket.addEventListener('open', () => {
      client.socket.send(JSON.stringify({ type: 'authenticate', access_token: client.token }));
    });
    client.socket.addEventListener('message', async messageEvent => {
      try {
        const message = JSON.parse(messageEvent.data);
        if (message.type === 'ready') {
          client.socket.send(JSON.stringify({ type: 'subscribe', project_id: projectId }));
          return;
        }
        if (message.type === 'subscribed') {
          client.subscribed = true;
          if (!changeStarted && clients.every(candidate => candidate.subscribed)) {
            changeStarted = true;
            await changeStatus(changedStatus);
          }
          return;
        }
        if (message.event_type !== 'task.status_changed' || message.task_id !== original.id) return;
        if (message.task.status === changedStatus) {
          client.changed = true;
          console.log(`${client.name} observed change ${message.event_id}: task ${message.task_id} -> ${message.task.status} (version ${message.task_version})`);
          if (!restoreStarted && clients.every(candidate => candidate.changed)) {
            restoreStarted = true;
            await changeStatus(original.status);
          }
        } else if (restoreStarted && message.task.status === original.status) {
          client.restored = true;
          console.log(`${client.name} observed restore ${message.event_id}: task ${message.task_id} -> ${message.task.status} (version ${message.task_version})`);
          if (clients.every(candidate => candidate.restored)) finish();
        }
      } catch (error) {
        finish(error);
      }
    });
    client.socket.addEventListener('error', () => finish(new Error(`${client.name} WebSocket connection failed`)));
  }
});

console.log('Two-client realtime smoke test passed; the original task status was restored.');
