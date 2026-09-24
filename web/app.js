const $ = (q, root = document) => root.querySelector(q);
const $$ = (q, root = document) => [...root.querySelectorAll(q)];
const state = { user: null, providers: [], traces: [], status: 'all', conversation: [], code: 'python', chartRange: '24h', chartFrom: '', chartTo: '', selectedTrace: null, traceView: 'readable', traceRequest: 0 };

const api = async (path, options = {}) => {
  const response = await fetch(path, { credentials: 'same-origin', headers: { 'Content-Type': 'application/json', ...(options.headers || {}) }, ...options });
  if (response.status === 204) return null;
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data?.error?.message || `Request failed (${response.status})`);
  return data;
};
const esc = (value = '') => String(value).replace(/[&<>'"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[c]));
function inlineMarkdown(value) {
  const code = []; let text = esc(value);
  text = text.replace(/`([^`]+)`/g, (_, content) => { const token = `@@NEXACODE${code.length}@@`; code.push(`<code>${content}</code>`); return token; });
  text = text.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>').replace(/__([^_]+)__/g, '<strong>$1</strong>').replace(/\*([^*]+)\*/g, '<em>$1</em>');
  code.forEach((item, index) => { text = text.replace(`@@NEXACODE${index}@@`, item); }); return text;
}
function renderMarkdown(value) {
  const lines = String(value ?? '').replace(/\r\n/g, '\n').split('\n'); let html = '', paragraph = [], list = '', inCode = false, codeLines = [];
  const flushParagraph = () => { if (paragraph.length) { html += `<p>${inlineMarkdown(paragraph.join(' '))}</p>`; paragraph = []; } };
  const closeList = () => { if (list) { html += `</${list}>`; list = ''; } };
  for (const line of lines) {
    if (/^```/.test(line.trim())) { flushParagraph(); closeList(); if (inCode) { html += `<pre><code>${esc(codeLines.join('\n'))}</code></pre>`; codeLines = []; } inCode = !inCode; continue; }
    if (inCode) { codeLines.push(line); continue; }
    if (!line.trim()) { flushParagraph(); closeList(); continue; }
    const heading = line.match(/^(#{1,4})\s+(.+)$/); if (heading) { flushParagraph(); closeList(); const level = heading[1].length + 2; html += `<h${level}>${inlineMarkdown(heading[2])}</h${level}>`; continue; }
    const unordered = line.match(/^\s*[-*]\s+(.+)$/); if (unordered) { flushParagraph(); if (list !== 'ul') { closeList(); html += '<ul>'; list = 'ul'; } html += `<li>${inlineMarkdown(unordered[1])}</li>`; continue; }
    const ordered = line.match(/^\s*\d+[.)]\s+(.+)$/); if (ordered) { flushParagraph(); if (list !== 'ol') { closeList(); html += '<ol>'; list = 'ol'; } html += `<li>${inlineMarkdown(ordered[1])}</li>`; continue; }
    const quote = line.match(/^>\s?(.+)$/); if (quote) { flushParagraph(); closeList(); html += `<blockquote>${inlineMarkdown(quote[1])}</blockquote>`; continue; }
    closeList(); paragraph.push(line.trim());
  }
  if (inCode) html += `<pre><code>${esc(codeLines.join('\n'))}</code></pre>`; flushParagraph(); closeList(); return html || '<p>—</p>';
}
const fmtNum = n => Intl.NumberFormat('en', { notation: n > 9999 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(n || 0);
const fmtTime = d => new Date(d).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const fmtDate = d => new Date(d).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
const fmtCost = n => n > 0 ? `$${n < .01 ? n.toFixed(6) : n.toFixed(3)}` : '—';
const initials = name => (name || 'N').split(/\s+/).map(x => x[0]).join('').slice(0, 2).toUpperCase();
const toast = (message, type = '') => { const el = document.createElement('div'); el.className = `toast ${type}`; el.textContent = message; $('#toast-stack').append(el); setTimeout(() => el.remove(), 3500); };
const setError = (id, message = '') => { const el = $(id); el.textContent = message; el.classList.toggle('hidden', !message); };

async function boot() {
  bindEvents();
  const info = await api('/api/bootstrap').catch(() => null);
  if (info) $('#version-label').textContent = `NEXA v${info.version}`;
  try { state.user = await api('/api/auth/me'); showApp(); } catch { showLogin(); }
  setInterval(updateClock, 1000); updateClock();
}

function bindEvents() {
  $$('#login-form [data-login-mode]').forEach(button => button.addEventListener('click', () => {
    $$('#login-form [data-login-mode]').forEach(x => x.classList.toggle('active', x === button));
    const master = button.dataset.loginMode === 'master';
    $('#master-fields').classList.toggle('hidden', !master); $('#user-fields').classList.toggle('hidden', master);
    $('#master-key').required = master; $('#login-username').required = !master; $('#login-password').required = !master;
  }));
  $$('[data-reveal]').forEach(button => button.addEventListener('click', () => { const input = $(`#${button.dataset.reveal}`); input.type = input.type === 'password' ? 'text' : 'password'; button.textContent = input.type === 'password' ? 'SHOW' : 'HIDE'; }));
  $('#login-form').addEventListener('submit', login);
  $('#logout-button').addEventListener('click', logout);
  $$('[data-route]').forEach(a => a.addEventListener('click', () => { if (innerWidth < 761) $('.sidebar').classList.remove('open'); }));
  window.addEventListener('hashchange', route); $('#menu-toggle').addEventListener('click', () => $('.sidebar').classList.toggle('open'));
  $('#refresh-button').addEventListener('click', () => loadRoute(true));
  $$('[data-open-provider]').forEach(x => x.addEventListener('click', () => openProvider()));
  $$('[data-open-user]').forEach(x => x.addEventListener('click', () => $('#user-dialog').showModal()));
  $$('[data-close-dialog]').forEach(x => x.addEventListener('click', () => x.closest('dialog').close()));
  $('#provider-form').addEventListener('submit', saveProvider); $('#user-form').addEventListener('submit', saveUser);
  $$('input[name=provider-type]').forEach(x => x.addEventListener('change', providerTypeChanged));
  $('#provider-name').addEventListener('input', () => { if (!$('#provider-id').value) $('#provider-slug').value = slugify($('#provider-name').value); });
  $('#trace-refresh').addEventListener('click', loadTraces);
  let searchTimer;
  $('#trace-search-form').addEventListener('submit', e => { e.preventDefault(); clearTimeout(searchTimer); loadTraces(); });
  $('#trace-search').addEventListener('input', e => { clearTimeout(searchTimer); $('#trace-search-clear').classList.toggle('hidden', !e.target.value); searchTimer = setTimeout(loadTraces, 300); });
  $('#trace-search-clear').addEventListener('click', () => { clearTimeout(searchTimer); $('#trace-search').value = ''; $('#trace-search-clear').classList.add('hidden'); $('#trace-search').focus(); loadTraces(); });
  $$('.filter-tabs [data-status]').forEach(x => x.addEventListener('click', () => { state.status = x.dataset.status; $$('.filter-tabs [data-status]').forEach(y => y.classList.toggle('active', y === x)); loadTraces(); }));
  $('#chart-range').addEventListener('change', e => selectChartRange(e.target.value));
  $('#custom-range').addEventListener('submit', applyCustomRange);
  $$('[data-trace-view]').forEach(x => x.addEventListener('click', () => { state.traceView = x.dataset.traceView; $$('[data-trace-view]').forEach(y => y.classList.toggle('active', y === x)); $('#trace-view-description').textContent = state.traceView === 'readable' ? 'Conversation formatted for reading' : 'Exact stored request and response structure'; renderTraceDetail(); }));
  $('#play-provider').addEventListener('change', loadModels); $('#reload-models').addEventListener('click', loadModels); $('#play-model').addEventListener('change', updateChatRoute);
  $('#play-temperature').addEventListener('input', e => $('#temperature-value').textContent = e.target.value);
  $('#chat-form').addEventListener('submit', sendChat); $('#chat-input').addEventListener('keydown', e => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') $('#chat-form').requestSubmit(); });
  $('#clear-chat').addEventListener('click', clearChat);
  $$('.code-tabs [data-code]').forEach(x => x.addEventListener('click', () => { state.code = x.dataset.code; $$('.code-tabs button').forEach(y => y.classList.toggle('active', x === y)); renderSDK(); }));
  $('#copy-sdk').addEventListener('click', () => copyText($('#sdk-code').textContent));
  $('#rotate-key').addEventListener('click', rotateKey); $('#copy-master-key').addEventListener('click', () => copyText($('#new-master-key').textContent));
  document.addEventListener('click', e => {
    const copy = e.target.closest('[data-copy]'); if (copy) copyText(copy.dataset.copy);
    const edit = e.target.closest('[data-edit-provider]'); if (edit) openProvider(edit.dataset.editProvider);
    const del = e.target.closest('[data-delete-provider]'); if (del) deleteProvider(del.dataset.deleteProvider);
    const trace = e.target.closest('[data-trace-id]'); if (trace) openTrace(trace.dataset.traceId);
    const user = e.target.closest('[data-delete-user]'); if (user) deleteUser(user.dataset.deleteUser);
  });
}

async function login(event) {
  event.preventDefault(); setError('#login-error'); const masterMode = $('[data-login-mode].active').dataset.loginMode === 'master';
  const body = masterMode ? { MasterKey: $('#master-key').value } : { Username: $('#login-username').value, Password: $('#login-password').value };
  const button = $('#login-form .primary-action'); button.disabled = true;
  try { state.user = await api('/api/auth/login', { method: 'POST', body: JSON.stringify(body) }); showApp(); }
  catch (e) { setError('#login-error', e.message); }
  finally { button.disabled = false; }
}
async function logout() { await api('/api/auth/logout', { method: 'POST' }).catch(() => {}); state.user = null; showLogin(); }
function showLogin() { $('#login-view').classList.remove('hidden'); $('#app-view').classList.add('hidden'); }
function showApp() {
  $('#login-view').classList.add('hidden'); $('#app-view').classList.remove('hidden');
  $('#user-name').textContent = state.user.username; $('#user-role').textContent = state.user.role.toUpperCase(); $('#user-initial').textContent = initials(state.user.username);
  route(); loadProviders(false); loadStats();
}

function route() {
  if (!state.user) return; const name = (location.hash || '#overview').slice(1); const valid = ['overview', 'providers', 'traces', 'playground', 'team']; const current = valid.includes(name) ? name : 'overview';
  $$('.page').forEach(x => x.classList.toggle('active', x.id === `page-${current}`)); $$('[data-route]').forEach(x => x.classList.toggle('active', x.dataset.route === current)); $('#page-crumb').textContent = current.toUpperCase();
  loadRoute();
}
function loadRoute(force = false) { const current = $('.page.active')?.id.replace('page-', ''); if (current === 'overview') { loadStats(); loadRecent(); } if (current === 'providers') loadProviders(); if (current === 'traces') loadTraces(); if (current === 'playground') loadProviders(false, true); if (current === 'team') loadUsers(); if (force) toast('Dashboard refreshed'); }
function updateClock() { const now = new Date(); $('#clock-time').textContent = now.toLocaleTimeString([], { hour12: false }); $('#clock-zone').textContent = Intl.DateTimeFormat().resolvedOptions().timeZone.split('/').pop().toUpperCase(); }

const rangeLabels = { '5m': 'PAST 5 MINUTES', '30m': 'PAST 30 MINUTES', '1h': 'PAST HOUR', '6h': 'PAST 6 HOURS', '24h': 'PAST 24 HOURS', '7d': 'PAST 7 DAYS', '30d': 'PAST 30 DAYS', custom: 'CUSTOM RANGE' };
function statsQuery() { const q = new URLSearchParams({ range: state.chartRange }); if (state.chartRange === 'custom') { q.set('from', state.chartFrom); q.set('to', state.chartTo); } return q; }
function selectChartRange(range) {
  state.chartRange = range; $('#chart-range').value = range; $('#custom-range').classList.toggle('hidden', range !== 'custom');
  if (range === 'custom') { if (!$('#chart-to').value) { const to = new Date(), from = new Date(to - 24*60*60*1000); $('#chart-to').value = localDateInput(to); $('#chart-from').value = localDateInput(from); } return; }
  loadStats();
}
function localDateInput(date) { const offset = date.getTimezoneOffset() * 60000; return new Date(date - offset).toISOString().slice(0, 16); }
function applyCustomRange(e) { e.preventDefault(); const from = new Date($('#chart-from').value), to = new Date($('#chart-to').value); if (!from.getTime() || !to.getTime() || from >= to) { toast('Choose a valid start and end time', 'error'); return; } state.chartFrom = from.toISOString(); state.chartTo = to.toISOString(); loadStats(); }
async function loadStats() {
  try {
    const x = await api(`/api/stats?${statsQuery()}`); const count = x.requests ?? x.requests_24h; $('#stat-requests').textContent = fmtNum(count); $('#sidebar-requests').textContent = `${fmtNum(count)} requests / ${state.chartRange.toUpperCase()}`; $('#stat-range-label').textContent = state.chartRange.toUpperCase(); $('#traffic-period').textContent = `TRAFFIC / ${rangeLabels[state.chartRange]}`; $('#stat-success').innerHTML = `${Math.round(x.success_rate)}<sup>%</sup>`; $('#stat-tokens').textContent = fmtNum(x.tokens_24h); $('#stat-cost').textContent = x.cost_24h > 0 ? `$${x.cost_24h.toFixed(4)}` : '$0.00'; $('#stat-provider-latency').textContent = latencyLabel(x.avg_upstream_latency_ms); $('#stat-gateway-latency').textContent = latencyLabel(x.avg_gateway_latency_ms); $('#success-caption').textContent = x.success_rate >= 99 ? 'All systems nominal' : x.success_rate >= 90 ? 'Minor errors detected' : 'Requires attention'; renderSpark(x.hourly); renderChart(x.hourly);
  } catch (e) { toast(e.message, 'error'); }
}
function latencyLabel(value) { const number = Number(value || 0); if (number < 10) return `${number.toFixed(1)} ms`; return `${Math.round(number)} ms`; }
function renderSpark(points) { const recent = points.slice(-12), max = Math.max(1, ...recent.map(x => x.requests)); $('#requests-spark').innerHTML = `<svg width="42" height="15" viewBox="0 0 42 15">${recent.map((x, i) => { const h = Math.max(2, x.requests / max * 15); return `<rect x="${i * 3.5}" y="${15 - h}" width="2" height="${h}" fill="currentColor"/>`; }).join('')}</svg>`; }
function renderChart(points) {
  if (!points?.length) { $('#traffic-chart').innerHTML = '<div class="chart-empty">No data in this time range.</div>'; return; }
  const w = 900, h = 220, pad = 15, max = Math.max(1, ...points.map(x => x.requests)), step = (w - pad * 2) / Math.max(1, points.length - 1);
  const coords = points.map((x, i) => [pad + i * step, h - 25 - (x.requests / max) * (h - 55)]); const error = points.map((x, i) => [pad + i * step, h - 25 - (x.errors / max) * (h - 55)]);
  const path = a => a.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' '); const labelEvery = Math.max(1, Math.ceil(points.length / 7));
  const area = `${path(coords)} L${coords.at(-1)[0]},${h-25} L${coords[0][0]},${h-25} Z`;
  $('#traffic-chart').innerHTML = `<svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img" aria-label="Interactive request velocity chart"><g stroke="#ddd9" stroke-width="1">${[0,1,2,3].map(i => `<line x1="${pad}" x2="${w-pad}" y1="${20+i*50}" y2="${20+i*50}"/>`).join('')}</g><path d="${area}" fill="#d9ff4329"/><path d="${path(coords)}" fill="none" stroke="#11130f" stroke-width="3" vector-effect="non-scaling-stroke"/><path d="${path(error)}" fill="none" stroke="#ff6847" stroke-width="2" vector-effect="non-scaling-stroke"/>${coords.map((p,i) => i%labelEvery===0 ? `<text x="${p[0]}" y="${h-3}" text-anchor="middle" font-size="8" font-family="monospace" fill="#777">${esc(points[i].hour)}</text>`:'').join('')}${coords.map((p,i) => `<circle cx="${p[0]}" cy="${p[1]}" r="4" fill="#d9ff43" stroke="#11130f" stroke-width="2"/><rect class="chart-hit" data-index="${i}" x="${Math.max(0,p[0]-step/2)}" y="0" width="${Math.max(12,step)}" height="${h-20}" fill="transparent"/>`).join('')}</svg><div class="chart-tooltip hidden"></div>`;
  const tooltip = $('.chart-tooltip', $('#traffic-chart')); $$('.chart-hit', $('#traffic-chart')).forEach(hit => { hit.addEventListener('pointerenter', showChartTooltip); hit.addEventListener('pointermove', showChartTooltip); hit.addEventListener('pointerleave', () => tooltip.classList.add('hidden')); });
  function showChartTooltip(e) { const p = points[Number(e.target.dataset.index)]; tooltip.innerHTML = `<b>${esc(new Date(p.timestamp).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }))}</b><span>Requests <strong>${p.requests}</strong></span><span>Errors <strong>${p.errors}</strong></span><span>Provider <strong>${latencyLabel(p.upstream_latency_ms)}</strong></span><span>Nexa <strong>${latencyLabel(p.gateway_latency_ms)}</strong></span>`; tooltip.classList.remove('hidden'); }
}
async function loadRecent() { try { const result = await api('/api/traces?limit=5'); $('#recent-traces').innerHTML = traceRows(result.data, false, true); } catch {} }
function traceRows(rows, details = true, clickable = true, emptyMessage = 'No traces yet. Send a request in Playground to begin.') {
  if (!rows.length) return `<tr><td colspan="8" class="empty-row">${esc(emptyMessage)}</td></tr>`;
  return rows.map(t => `<tr ${clickable ? `data-trace-id="${esc(t.id)}"` : ''}><td><span class="status-pill ${t.status === 'success' ? '' : 'error'}">${esc(t.status)}</span></td><td><span class="route-tag">${esc(t.provider_name)}</span></td><td><span class="model-name">${esc(t.model)}</span></td><td>${fmtNum(t.total_tokens)}</td><td>${fmtNum(t.latency_ms)} ms</td><td>${fmtCost(t.cost_usd)}</td><td>${fmtTime(t.created_at)}</td>${details ? '<td><button class="detail-button">↗</button></td>' : ''}</tr>`).join('');
}

async function loadProviders(render = true, select = false) {
  try { state.providers = await api('/api/providers'); if (render && $('#page-providers').classList.contains('active')) renderProviders(); if (select || $('#page-playground').classList.contains('active')) renderProviderSelect(); }
  catch (e) { toast(e.message, 'error'); }
}
function providerInitial(type) { return ({ openai: 'OA', groq: 'GQ', gemini: 'GM', anthropic: 'AN', compatible: 'CU', custom: 'CU' })[type] || 'AI'; }
function providerLabel(type) { return ['compatible', 'custom'].includes(type) ? 'CUSTOM' : type.toUpperCase(); }
function renderProviders() {
  const root = $('#provider-grid');
  if (!state.providers.length) { root.innerHTML = `<div class="empty-provider"><div class="welcome-glyph">N</div><h2>No routes connected</h2><p>Add an API provider to begin sending traffic through Nexa.</p><button class="primary-action compact" data-open-provider><span>Add first provider</span><b>＋</b></button></div>`; $('[data-open-provider]', root).addEventListener('click', () => openProvider()); return; }
  root.innerHTML = state.providers.map(p => { const route = `${p.slug}/model-name`; return `<article class="provider-card"><header><div class="provider-identity"><span class="provider-logo">${providerInitial(p.type)}</span><div><h3>${esc(p.name)}</h3><span>${esc(providerLabel(p.type))}</span></div></div><span class="provider-state ${p.enabled ? '' : 'off'}">${p.enabled ? 'ACTIVE' : 'PAUSED'}</span></header><dl><div><dt>ROUTE TEMPLATE</dt><dd title="${esc(route)}">${esc(route)}</dd></div><div><dt>SECRET</dt><dd>${esc(p.masked_key)}</dd></div><div><dt>BASE URL</dt><dd title="${esc(p.base_url)}">${esc(p.base_url)}</dd></div></dl><footer class="${p.enabled ? 'with-copy' : ''}">${p.enabled ? `<button class="copy-route" data-copy="${esc(route)}">COPY ROUTE</button>` : ''}<button data-edit-provider="${esc(p.id)}">EDIT ROUTE</button><button data-delete-provider="${esc(p.id)}">REMOVE</button></footer></article>`; }).join('');
}
const bases = { openai: 'https://api.openai.com/v1', groq: 'https://api.groq.com/openai/v1', gemini: 'https://generativelanguage.googleapis.com/v1beta/openai', anthropic: 'https://api.anthropic.com/v1', custom: '' };
function openProvider(id = '') {
  const p = state.providers.find(x => x.id === id); $('#provider-form').reset(); $('#provider-id').value = p?.id || ''; $('#provider-dialog-title').textContent = p ? 'Edit provider' : 'Add provider';
  const storedType = p?.type || 'openai', type = storedType === 'compatible' ? 'custom' : storedType; $(`input[name=provider-type][value="${type}"]`).checked = true; $('#provider-name').value = p?.name || ''; $('#provider-slug').value = p?.slug || ''; $('#provider-url').value = p?.base_url || bases[type]; $('#provider-key').value = ''; $('#provider-key').required = !p; $('#provider-key').placeholder = p ? `Keep existing (${p.masked_key})` : 'Stored with AES-256 encryption'; $('#provider-enabled').checked = p ? p.enabled : true; updateProviderFields(type); setError('#provider-error'); $('#provider-dialog').showModal();
}
function updateProviderFields(type) { const custom = type === 'custom'; $('#provider-url').required = custom; $('#provider-url').placeholder = custom ? 'https://your-provider.example/v1' : 'Filled automatically'; $('#provider-url-note').textContent = custom ? 'Enter the OpenAI-compatible API base URL for your provider.' : 'Provider endpoint is filled automatically.'; }
function providerTypeChanged(e) { if (!$('#provider-id').value || $('#provider-url').value === '' || Object.values(bases).includes($('#provider-url').value)) $('#provider-url').value = bases[e.target.value]; const label = e.target.value === 'custom' ? 'Custom' : e.target.value[0].toUpperCase() + e.target.value.slice(1); if (!$('#provider-name').value) $('#provider-name').value = label; if (!$('#provider-slug').value) $('#provider-slug').value = e.target.value; updateProviderFields(e.target.value); }
function slugify(v) { return v.toLowerCase().trim().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, ''); }
async function saveProvider(e) {
  e.preventDefault(); const id = $('#provider-id').value; const body = { name: $('#provider-name').value, slug: $('#provider-slug').value, type: $('input[name=provider-type]:checked').value, base_url: $('#provider-url').value, api_key: $('#provider-key').value, enabled: $('#provider-enabled').checked };
  try { await api(id ? `/api/providers/${id}` : '/api/providers', { method: id ? 'PUT' : 'POST', body: JSON.stringify(body) }); $('#provider-dialog').close(); toast(id ? 'Provider updated' : 'Provider connected'); await loadProviders(); }
  catch (err) { setError('#provider-error', err.message); }
}
async function deleteProvider(id) { const p = state.providers.find(x => x.id === id); if (!confirm(`Remove ${p?.name || 'this provider'}? Existing traces will remain.`)) return; try { await api(`/api/providers/${id}`, { method: 'DELETE' }); toast('Provider removed'); loadProviders(); } catch (e) { toast(e.message, 'error'); } }

async function loadTraces() {
  const requestID = ++state.traceRequest, search = $('#trace-search').value.trim();
  const q = new URLSearchParams({ limit: '100', status: state.status }); if (search) q.set('search', search);
  $('.search-field').classList.add('searching');
  try {
    const result = await api(`/api/traces?${q}`); if (requestID !== state.traceRequest) return;
    state.traces = result.data; $('#trace-total').textContent = fmtNum(result.total); $('#trace-table').innerHTML = traceRows(result.data, true, true, search ? `No traces match “${search}”.` : 'No traces yet. Send a request in Playground to begin.');
  } catch (e) { if (requestID === state.traceRequest) toast(e.message, 'error'); }
  finally { if (requestID === state.traceRequest) $('.search-field').classList.remove('searching'); }
}
async function openTrace(id) {
  try { state.selectedTrace = await api(`/api/traces/${id}`); state.traceView = 'readable'; $$('[data-trace-view]').forEach(x => x.classList.toggle('active', x.dataset.traceView === 'readable')); $('#trace-view-description').textContent = 'Conversation formatted for reading'; $('#trace-dialog-title').textContent = `${state.selectedTrace.provider_name} / ${state.selectedTrace.model}`; renderTraceDetail(); $('#trace-dialog').showModal(); } catch (e) { toast(e.message, 'error'); }
}
function parseJSON(value) { try { return JSON.parse(value); } catch { return null; } }
function traceMessages(value) {
  const parsed = parseJSON(value); if (!Array.isArray(parsed)) return `<div class="plain-copy">${esc(value || 'No input captured.')}</div>`;
  return `<div class="message-stack">${parsed.map(message => { const role = String(message.role || 'message').toUpperCase(); const content = typeof message.content === 'string' ? message.content : JSON.stringify(message.content, null, 2); return `<article class="trace-message ${esc(role.toLowerCase())}"><span>${esc(role)}</span><div class="markdown-body">${role === 'ASSISTANT' ? renderMarkdown(content) : `<p>${esc(content || '—')}</p>`}</div></article>`; }).join('')}</div>`;
}
function traceResponse(value) { const parsed = parseJSON(value); const text = parsed?.error?.message || parsed?.choices?.[0]?.message?.content || value || 'No output captured.'; return `<div class="plain-copy markdown-body">${renderMarkdown(typeof text === 'string' ? text : JSON.stringify(text, null, 2))}</div>`; }
function jsonTraceValue(value, role) { const parsed = parseJSON(value); if (parsed !== null) return JSON.stringify(parsed, null, 2); return JSON.stringify({ role, content: value || '' }, null, 2); }
function renderTraceDetail() {
  const t = state.selectedTrace; if (!t) return; const latencyDetail = t.upstream_latency_ms ? `${fmtNum(t.upstream_latency_ms)} ms provider + ${fmtNum(t.gateway_latency_ms)} ms Nexa` : `${fmtNum(t.latency_ms)} ms total`;
  const content = state.traceView === 'json' ? `<div class="trace-content"><section class="trace-block"><h3>INPUT / JSON</h3><pre>${esc(jsonTraceValue(t.prompt, 'user'))}</pre></section><section class="trace-block"><h3>OUTPUT / JSON</h3><pre>${esc(jsonTraceValue(t.response, 'assistant'))}</pre></section></div>` : `<div class="trace-content readable"><section class="trace-block"><h3>INPUT / CONVERSATION</h3>${traceMessages(t.prompt)}</section><section class="trace-block"><h3>OUTPUT / RESPONSE</h3>${traceResponse(t.response)}</section></div>`;
  $('#trace-detail').innerHTML = `<div class="trace-summary"><div><span>OUTCOME</span><b>${esc(t.status.toUpperCase())} / ${t.status_code}</b></div><div><span>TOKENS</span><b>${fmtNum(t.input_tokens)} IN · ${fmtNum(t.output_tokens)} OUT</b></div><div><span>LATENCY</span><b title="${esc(latencyDetail)}">${fmtNum(t.latency_ms)} MS</b></div><div><span>EST. COST</span><b>${fmtCost(t.cost_usd)}</b></div></div>${t.error ? `<div class="form-error">${esc(t.error)}</div>` : ''}${content}`;
}

function renderProviderSelect() {
  const select = $('#play-provider'), previous = select.value; select.innerHTML = state.providers.filter(p => p.enabled).map(p => `<option value="${esc(p.id)}">${esc(p.name)} · ${esc(p.slug)}</option>`).join('');
  if (previous && state.providers.some(p => p.id === previous)) select.value = previous; if (!select.options.length) select.innerHTML = '<option value="">Add a provider first</option>'; loadModels();
}
async function loadModels() {
  const id = $('#play-provider').value, model = $('#play-model'); model.innerHTML = '<option>Loading live models…</option>'; model.disabled = true;
  if (!id) { model.innerHTML = '<option value="">No provider available</option>'; updateChatRoute(); return; }
  try { const list = await api(`/api/providers/${id}/models?capability=chat`); const names = [...new Set(list.map(x => x.id || x.name).filter(Boolean))].sort(); model.innerHTML = names.map(x => `<option value="${esc(x)}">${esc(x)}</option>`).join('') || '<option value="">No chat models returned</option>'; $('#model-count').textContent = `${names.length} CHAT`; }
  catch (e) { model.innerHTML = `<option value="">Could not load models</option>`; toast(e.message, 'error'); }
  finally { model.disabled = false; updateChatRoute(); }
}
function updateChatRoute() { const p = state.providers.find(x => x.id === $('#play-provider').value), model = $('#play-model').value, reasoning = isReasoningModel(model); $('#chat-route').textContent = p && model ? `${p.slug}/${model}` : 'NO ROUTE SELECTED'; $('#play-temperature').disabled = reasoning; $('#play-temperature').title = reasoning ? 'This model controls its own sampling temperature.' : ''; }
function clearChat() { state.conversation = []; $('#chat-messages').innerHTML = `<div class="chat-welcome"><div class="welcome-glyph">N</div><h2>Test the route.</h2><p>Select a provider and model, then send a message. This is a real request and will appear in Traces.</p></div>`; $('#chat-metrics').textContent = 'READY'; }
function addMessage(role, content, loading = false) { $('.chat-welcome')?.remove(); const item = document.createElement('div'); item.className = `chat-message ${role}`; item.innerHTML = `<span class="role">${role === 'user' ? 'YOU' : 'NEXA'}</span><div class="bubble markdown-body ${loading ? 'typing' : ''}">${role === 'assistant' && !loading ? renderMarkdown(content) : esc(content)}</div>`; $('#chat-messages').append(item); $('#chat-messages').scrollTop = $('#chat-messages').scrollHeight; return item; }
async function sendChat(e) {
  e.preventDefault(); const input = $('#chat-input'), text = input.value.trim(), p = state.providers.find(x => x.id === $('#play-provider').value), model = $('#play-model').value; if (!text || !p || !model) { toast('Choose a provider and model first', 'error'); return; }
  input.value = ''; state.conversation.push({ role: 'user', content: text }); addMessage('user', text); const pending = addMessage('assistant', '', true); const started = performance.now(); $('#chat-form button').disabled = true; $('#chat-metrics').textContent = 'REQUEST IN FLIGHT…';
  const messages = []; const system = $('#play-system').value.trim(); if (system) messages.push({ role: 'system', content: system }); messages.push(...state.conversation);
  try { const request = { model: `${p.slug}/${model}`, messages, max_completion_tokens: Number($('#play-max-tokens').value), stream: false }; if (!isReasoningModel(model)) request.temperature = Number($('#play-temperature').value); const out = await api('/api/playground/chat', { method: 'POST', body: JSON.stringify(request) }); const rawContent = out?.choices?.[0]?.message?.content ?? JSON.stringify(out, null, 2), content = typeof rawContent === 'string' ? rawContent : JSON.stringify(rawContent, null, 2); pending.querySelector('.bubble').classList.remove('typing'); pending.querySelector('.bubble').innerHTML = renderMarkdown(content); state.conversation.push({ role: 'assistant', content }); $('#chat-metrics').textContent = `${Math.round(performance.now() - started)} MS · ${fmtNum(out?.usage?.total_tokens)} TOKENS`; loadStats(); }
  catch (err) { pending.querySelector('.bubble').classList.remove('typing'); pending.querySelector('.bubble').textContent = `Request failed: ${err.message}`; $('#chat-metrics').textContent = 'ERROR'; }
  finally { $('#chat-form button').disabled = false; $('#chat-messages').scrollTop = $('#chat-messages').scrollHeight; }
}
function isReasoningModel(model) { const id = model.toLowerCase(); return /^gpt-5/.test(id) || /^o[134]/.test(id) || id.includes('gpt-oss'); }

async function loadUsers() { try { const users = await api('/api/users'); $('#users-list').innerHTML = users.length ? users.map(u => `<div class="user-row"><span class="user-avatar">${esc(initials(u.username))}</span><div><h3>${esc(u.username)}</h3><p>CREATED ${esc(fmtDate(u.created_at).toUpperCase())}</p></div><span class="role-tag">${esc(u.role.toUpperCase())}</span><button class="delete-user" data-delete-user="${esc(u.id)}" title="Delete user">×</button></div>`).join('') : '<div class="empty-row">No local users yet. Master access remains active.</div>'; renderSDK(); } catch (e) { toast(e.message, 'error'); } }
async function saveUser(e) { e.preventDefault(); try { await api('/api/users', { method: 'POST', body: JSON.stringify({ Username: $('#new-username').value, Password: $('#new-password').value, Role: $('#new-role').value }) }); $('#user-dialog').close(); $('#user-form').reset(); toast('User created'); loadUsers(); } catch (err) { setError('#user-error', err.message); } }
async function deleteUser(id) { if (!confirm('Delete this user and all of their active sessions?')) return; try { await api(`/api/users/${id}`, { method: 'DELETE' }); toast('User removed'); loadUsers(); } catch (e) { toast(e.message, 'error'); } }
async function rotateKey() { if (!confirm('Rotate the master key now? The previous key will immediately stop authenticating API calls.')) return; try { const out = await api('/api/master-key/rotate', { method: 'POST', body: '{}' }); $('#new-master-key').textContent = out.master_key; $('#key-dialog').showModal(); } catch (e) { toast(e.message, 'error'); } }
function renderSDK() {
  const p = state.providers.find(x => x.enabled), model = p ? `${p.slug}/YOUR_MODEL` : 'provider-slug/YOUR_MODEL'; const origin = location.origin;
  const snippets = {
    python: `from openai import OpenAI\n\nclient = OpenAI(\n    api_key="YOUR_NEXA_MASTER_KEY",\n    base_url="${origin}/v1"\n)\n\nresponse = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "Hello, Nexa!"}]\n)`,
    javascript: `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  apiKey: "YOUR_NEXA_MASTER_KEY",\n  baseURL: "${origin}/v1"\n});\n\nconst response = await client.chat.completions.create({\n  model: "${model}",\n  messages: [{ role: "user", content: "Hello, Nexa!" }]\n});`,
    curl: `curl ${origin}/v1/chat/completions \\\n  -H "Authorization: Bearer YOUR_NEXA_MASTER_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","messages":[{"role":"user","content":"Hello, Nexa!"}]}'`
  }; $('#sdk-code').textContent = snippets[state.code];
}
async function copyText(value) { try { await navigator.clipboard.writeText(value); toast('Copied to clipboard'); } catch { toast('Could not access clipboard', 'error'); } }

boot();
