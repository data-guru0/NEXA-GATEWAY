const $ = (q, root = document) => root.querySelector(q);
const $$ = (q, root = document) => [...root.querySelectorAll(q)];
const state = { user: null, providers: [], routingProfiles: [], routingTargets: [], routingModels: {}, traces: [], traceTotal: 0, status: 'all', conversation: [], code: 'python', chartRange: '24h', chartFrom: '', chartTo: '', selectedTrace: null, traceView: 'readable', traceRequest: 0, abort: null, poll: null, headersDirty: false, recentTraces: [], traceList: [], users: [], apiKeys: [], prices: [], priceExpanded: false, stats: null, providerStats: {}, routingStats: null };

const api = async (path, options = {}) => {
  const response = await fetch(path, { credentials: 'same-origin', headers: { 'Content-Type': 'application/json', ...(options.headers || {}) }, ...options });
  if (response.status === 204) return null;
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data?.error?.message || `Request failed (${response.status})`);
  return data;
};
const esc = (value = '') => String(value).replace(/[&<>'"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[c]));
const uiIcon = (name, className = 'button-icon') => `<svg class="${className}" aria-hidden="true"><use href="#icon-${name}"/></svg>`;
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
  const codeBlock = () => `<pre><button class="code-copy" type="button">COPY</button><code>${esc(codeLines.join('\n'))}</code></pre>`;
  for (const line of lines) {
    if (/^```/.test(line.trim())) { flushParagraph(); closeList(); if (inCode) { html += codeBlock(); codeLines = []; } inCode = !inCode; continue; }
    if (inCode) { codeLines.push(line); continue; }
    if (!line.trim()) { flushParagraph(); closeList(); continue; }
    const heading = line.match(/^(#{1,4})\s+(.+)$/); if (heading) { flushParagraph(); closeList(); const level = heading[1].length + 2; html += `<h${level}>${inlineMarkdown(heading[2])}</h${level}>`; continue; }
    const unordered = line.match(/^\s*[-*]\s+(.+)$/); if (unordered) { flushParagraph(); if (list !== 'ul') { closeList(); html += '<ul>'; list = 'ul'; } html += `<li>${inlineMarkdown(unordered[1])}</li>`; continue; }
    const ordered = line.match(/^\s*\d+[.)]\s+(.+)$/); if (ordered) { flushParagraph(); if (list !== 'ol') { closeList(); html += '<ol>'; list = 'ol'; } html += `<li>${inlineMarkdown(ordered[1])}</li>`; continue; }
    const quote = line.match(/^>\s?(.+)$/); if (quote) { flushParagraph(); closeList(); html += `<blockquote>${inlineMarkdown(quote[1])}</blockquote>`; continue; }
    closeList(); paragraph.push(line.trim());
  }
  if (inCode) html += codeBlock(); flushParagraph(); closeList(); return html || '<p>—</p>';
}
const fmtNum = n => Intl.NumberFormat('en', { notation: n > 9999 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(n || 0);
const fmtTime = d => new Date(d).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const fmtDate = d => new Date(d).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
const fmtCost = n => n > 0 ? `$${n < .01 ? n.toFixed(6) : n.toFixed(3)}` : '—';
const initials = name => (name || 'N').split(/\s+/).map(x => x[0]).join('').slice(0, 2).toUpperCase();
const toast = (message, type = '') => { const el = document.createElement('div'); el.className = `toast ${type}`; el.textContent = message; $('#toast-stack').append(el); setTimeout(() => el.remove(), 4500); };
// Inline style attributes are blocked by the CSP, so percentage bars carry data-width and get it applied here.
const applyWidths = root => $$('[data-width]', root).forEach(el => { el.style.width = `${Math.min(100, Math.max(0, Number(el.dataset.width) || 0))}%`; });
const setError = (id, message = '') => { const el = $(id); el.textContent = message; el.classList.toggle('hidden', !message); };
const isAdmin = () => !!(state.user && (state.user.master || state.user.role === 'admin'));
const store = { get(key, fallback) { try { return JSON.parse(localStorage.getItem(key)) ?? fallback; } catch { return fallback; } }, set(key, value) { try { localStorage.setItem(key, JSON.stringify(value)); } catch {} } };
function markFresh(id) { const el = $(id); if (!el) return; el.dataset.updated = String(Date.now()); updateFreshness(); }
function updateFreshness() { $$('.freshness[data-updated]').forEach(el => { const seconds = Math.max(0, Math.floor((Date.now() - Number(el.dataset.updated)) / 1000)); el.textContent = seconds < 2 ? 'Updated now' : seconds < 60 ? `Updated ${seconds} s ago` : `Updated ${Math.floor(seconds / 60)} min ago`; }); }

// ask renders the shared dialog: a styled confirm, or a small form when fields are given.
function ask({ kicker = 'CONFIRM', title, message = '', fields = [], confirm = 'Confirm', danger = false }) {
  return new Promise(resolve => {
    const dialog = $('#ask-dialog'), form = $('#ask-form');
    $('#ask-kicker').textContent = kicker; $('#ask-title').textContent = title; $('#ask-message').textContent = message;
    $('#ask-fields').innerHTML = fields.map(f => `<label class="field"><span>${esc(f.label)}</span>${f.options
      ? `<select name="${esc(f.name)}">${f.options.map(([value, label]) => `<option value="${esc(value)}" ${value === f.value ? 'selected' : ''}>${esc(label)}</option>`).join('')}</select>`
      : `<input name="${esc(f.name)}" type="${f.type || 'text'}" ${f.minlength ? `minlength="${f.minlength}"` : ''} required placeholder="${esc(f.placeholder || '')}" value="${esc(f.value || '')}">`}</label>`).join('');
    $('#ask-confirm span').textContent = confirm; $('#ask-confirm').classList.toggle('danger', danger); setError('#ask-error');
    const done = value => { form.onsubmit = null; dialog.oncancel = null; $$('[data-ask=cancel]').forEach(b => { b.onclick = null; }); dialog.close(); resolve(value); };
    form.onsubmit = e => { e.preventDefault(); done(Object.fromEntries(new FormData(form))); };
    $$('[data-ask=cancel]').forEach(b => { b.onclick = () => done(null); });
    dialog.oncancel = e => { e.preventDefault(); done(null); };
    dialog.showModal(); $('#ask-fields input, #ask-fields select')?.focus();
  });
}
const confirmAction = (title, message, confirm = 'Confirm') => ask({ title, message, confirm, danger: true }).then(Boolean);

async function boot() {
  bindEvents(); setInterval(updateFreshness, 1000);
  $('#endpoint-host').textContent = location.host;
  const info = await api('/api/bootstrap').catch(() => null);
  if (info) { $('#version-label').textContent = `NEXA v${info.version}`; $('#sidebar-version').textContent = `v${info.version}`; }
  try { state.user = await api('/api/auth/me'); showApp(); } catch { showLogin(); }
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
  $('#provider-headers').addEventListener('input', () => { state.headersDirty = true; });
  $('#price-form').addEventListener('submit', savePrice);
  $('#price-search').addEventListener('input', renderPrices); $('#price-expand').addEventListener('click', () => { state.priceExpanded = !state.priceExpanded; renderPrices(); });
  $('#routing-form').addEventListener('submit', saveRoutingProfile); $('#new-routing-profile').addEventListener('click', () => openRoutingProfile()); $('#add-routing-target').addEventListener('click', () => addRoutingTarget());
  $('#route-preview-form').addEventListener('submit', previewRoute);
  $('#routing-name').addEventListener('input', () => { if (!$('#routing-id').value) $('#routing-slug').value = slugify($('#routing-name').value); });
  $$('input[name=provider-type]').forEach(x => x.addEventListener('change', providerTypeChanged));
  $('#provider-name').addEventListener('input', () => { if (!$('#provider-id').value) $('#provider-slug').value = slugify($('#provider-name').value); });
  $('#trace-refresh').addEventListener('click', () => loadTraces());
  let searchTimer;
  const debounced = () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => loadTraces(), 300); };
  $('#trace-search-form').addEventListener('submit', e => { e.preventDefault(); clearTimeout(searchTimer); loadTraces(); });
  $('#trace-search').addEventListener('input', e => { $('#trace-search-clear').classList.toggle('hidden', !e.target.value); debounced(); });
  $('#trace-search-clear').addEventListener('click', () => { clearTimeout(searchTimer); $('#trace-search').value = ''; $('#trace-search-clear').classList.add('hidden'); $('#trace-search').focus(); loadTraces(); });
  $('#trace-model').addEventListener('input', debounced);
  ['#trace-provider', '#trace-from', '#trace-to'].forEach(id => $(id).addEventListener('change', () => loadTraces()));
  $('#trace-filter-clear').addEventListener('click', () => { ['#trace-provider', '#trace-model', '#trace-from', '#trace-to'].forEach(id => { $(id).value = ''; }); loadTraces(); });
  $('#trace-more').addEventListener('click', () => loadTraces(true));
  $$('.filter-tabs [data-status]').forEach(x => x.addEventListener('click', () => { state.status = x.dataset.status; $$('.filter-tabs [data-status]').forEach(y => y.classList.toggle('active', y === x)); loadTraces(); }));
  $('#chart-range').addEventListener('change', e => selectChartRange(e.target.value));
  $('#custom-range').addEventListener('submit', applyCustomRange);
  $$('[data-trace-view]').forEach(x => x.addEventListener('click', () => { state.traceView = x.dataset.traceView; $$('[data-trace-view]').forEach(y => y.classList.toggle('active', y === x)); $('#trace-view-description').textContent = state.traceView === 'readable' ? 'Conversation formatted for reading' : 'Exact stored request and response structure'; renderTraceDetail(); }));
  $('#trace-previous').addEventListener('click', () => stepTrace(-1)); $('#trace-next').addEventListener('click', () => stepTrace(1));
  $('#play-provider').addEventListener('change', () => loadModels()); $('#reload-models').addEventListener('click', () => loadModels(true)); $('#play-model').addEventListener('change', updateChatRoute);
  $('#play-compare').addEventListener('change', toggleComparison); $('#compare-provider').addEventListener('change', () => loadCompareModels()); $('#reload-compare-models').addEventListener('click', () => loadCompareModels(true));
  $('#play-temperature').addEventListener('input', e => { $('#temperature-value').textContent = e.target.value; });
  $('#play-top-p').addEventListener('input', e => { $('#top-p-value').textContent = e.target.value; });
  $('#chat-form').addEventListener('submit', sendChat); $('#chat-input').addEventListener('keydown', e => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') $('#chat-form').requestSubmit(); });
  $('#clear-chat').addEventListener('click', clearChat);
  $('#play-system').addEventListener('change', saveConversation);
  ['#play-stream', '#play-json'].forEach(id => $(id).addEventListener('change', savePlaygroundPrefs));
  $('#play-json').addEventListener('change', updateJsonHint);
  $$('.code-tabs [data-code]').forEach(x => x.addEventListener('click', () => { state.code = x.dataset.code; $$('.code-tabs button').forEach(y => y.classList.toggle('active', x === y)); renderSDK(); }));
  $('#copy-sdk').addEventListener('click', () => copyText($('#sdk-code').textContent));
  $('#rotate-key').addEventListener('click', rotateKey); $('#copy-master-key').addEventListener('click', () => copyText($('#new-master-key').textContent));
  $('#change-password').addEventListener('click', changePassword);
  $('#api-key-form').addEventListener('submit', createApiKey);
  $('#user-search').addEventListener('input', renderUsers); $('#key-search').addEventListener('input', renderApiKeys);
  document.addEventListener('click', e => {
    const codeCopy = e.target.closest('.code-copy'); if (codeCopy) { copyText(codeCopy.nextElementSibling.textContent); return; }
    const copy = e.target.closest('[data-copy]'); if (copy) copyText(copy.dataset.copy);
    const snippet = e.target.closest('[data-copy-snippet]'); if (snippet) copyText(snippetFor('python', snippet.dataset.copySnippet));
    const edit = e.target.closest('[data-edit-provider]'); if (edit) openProvider(edit.dataset.editProvider);
    const toggle = e.target.closest('[data-toggle-provider]'); if (toggle) toggleProvider(toggle.dataset.toggleProvider);
    const del = e.target.closest('[data-delete-provider]'); if (del) deleteProvider(del.dataset.deleteProvider);
    const trace = e.target.closest('[data-trace-id]'); if (trace) openTrace(trace.dataset.traceId, trace.closest('#recent-traces') ? state.recentTraces : trace.closest('#trace-table') ? state.traces : null);
    const user = e.target.closest('[data-delete-user]'); if (user) deleteUser(user.dataset.deleteUser);
    const role = e.target.closest('[data-user-role]'); if (role) changeRole(role.dataset.userRole);
    const reset = e.target.closest('[data-user-password]'); if (reset) resetPassword(reset.dataset.userPassword);
    const revoke = e.target.closest('[data-revoke-key]'); if (revoke) revokeApiKey(revoke.dataset.revokeKey);
    const price = e.target.closest('[data-delete-price]'); if (price) deletePrice(price.dataset.deletePrice);
    const editRoute = e.target.closest('[data-edit-routing]'); if (editRoute) openRoutingProfile(editRoute.dataset.editRouting);
    const activateRoute = e.target.closest('[data-activate-routing]'); if (activateRoute) activateRoutingProfile(activateRoute.dataset.activateRouting);
    const checkRoute = e.target.closest('[data-check-routing]'); if (checkRoute) checkRoutingProfile(checkRoute.dataset.checkRouting, checkRoute);
    const deleteRoute = e.target.closest('[data-delete-routing]'); if (deleteRoute) deleteRoutingProfile(deleteRoute.dataset.deleteRouting);
    const removeTarget = e.target.closest('[data-remove-target]'); if (removeTarget) { state.routingTargets.splice(Number(removeTarget.dataset.removeTarget), 1); renderRoutingTargets(); }
  });
  document.addEventListener('keydown', e => { if ($('#trace-dialog').open && !e.target.matches?.('input,textarea,select')) { if (['ArrowLeft', 'ArrowUp'].includes(e.key)) { e.preventDefault(); stepTrace(-1); } if (['ArrowRight', 'ArrowDown'].includes(e.key)) { e.preventDefault(); stepTrace(1); } } if (e.key === 'Enter' && e.target.matches?.('tr[data-trace-id]')) openTrace(e.target.dataset.traceId, e.target.closest('#recent-traces') ? state.recentTraces : state.traces); });
}

async function login(event) {
  event.preventDefault(); setError('#login-error'); const masterMode = $('[data-login-mode].active').dataset.loginMode === 'master';
  const body = masterMode ? { MasterKey: $('#master-key').value } : { Username: $('#login-username').value, Password: $('#login-password').value };
  const button = $('#login-form .primary-action'); button.disabled = true;
  try { state.user = await api('/api/auth/login', { method: 'POST', body: JSON.stringify(body) }); showApp(); }
  catch (e) { setError('#login-error', e.message); }
  finally { button.disabled = false; }
}
async function logout() { await api('/api/auth/logout', { method: 'POST' }).catch(() => {}); state.user = null; clearInterval(state.poll); showLogin(); }
function showLogin() { $('#login-view').classList.remove('hidden'); $('#app-view').classList.add('hidden'); }
function showApp() {
  $('#login-view').classList.add('hidden'); $('#app-view').classList.remove('hidden');
  $('#user-name').textContent = state.user.username; $('#user-role').textContent = state.user.role.toUpperCase(); $('#user-initial').textContent = initials(state.user.username);
  document.body.classList.toggle('is-member', !isAdmin()); document.body.classList.toggle('not-master', !state.user.master);
  $('#change-password').classList.toggle('hidden', !!state.user.master);
  restoreConversation(); restorePlaygroundPrefs();
  route(); loadProviders(false); loadRoutingProfiles(false); loadStats(); if (isAdmin()) loadApiKeys(false);
}

function route() {
  if (!state.user) return; const name = (location.hash || '#overview').slice(1); const valid = ['overview', 'providers', 'routing', 'traces', 'playground', 'team']; const current = valid.includes(name) ? name : 'overview';
  $$('.page').forEach(x => x.classList.toggle('active', x.id === `page-${current}`)); $$('[data-route]').forEach(x => x.classList.toggle('active', x.dataset.route === current)); $('#page-crumb').textContent = current.toUpperCase();
  clearInterval(state.poll);
  if (current === 'overview') state.poll = setInterval(() => { if (!document.hidden) { loadStats(); loadRecent(); loadProviders(false); } }, 5000);
  loadRoute();
}
function loadRoute(force = false) {
  const current = $('.page.active')?.id.replace('page-', '');
  if (current === 'overview') { loadStats(); loadRecent(); loadProviders(false); }
  if (current === 'providers') { loadProviders(); loadPrices(); }
  if (current === 'routing') { loadProviders(false); loadRoutingStats(); loadRoutingProfiles(); }
  if (current === 'traces') { loadProviders(false); loadTraces(); }
  if (current === 'playground') { loadProviders(false, true); loadRoutingProfiles(false); }
  if (current === 'team') { loadUsers(); loadApiKeys(); }
  if (force) toast('Dashboard refreshed');
}
const rangeLabels = { '5m': 'PAST 5 MINUTES', '30m': 'PAST 30 MINUTES', '1h': 'PAST HOUR', '6h': 'PAST 6 HOURS', '24h': 'PAST 24 HOURS', '7d': 'PAST 7 DAYS', '30d': 'PAST 30 DAYS', custom: 'CUSTOM RANGE' };
function statsQuery() { const q = new URLSearchParams({ range: state.chartRange }); if (state.chartRange === 'custom') { q.set('from', state.chartFrom); q.set('to', state.chartTo); } return q; }
function selectChartRange(range) {
  state.chartRange = range; $('#chart-range').value = range; $('#custom-range').classList.toggle('hidden', range !== 'custom');
  if (range === 'custom') { if (!$('#chart-to').value) { const to = new Date(), from = new Date(to - 24 * 60 * 60 * 1000); $('#chart-to').value = localDateInput(to); $('#chart-from').value = localDateInput(from); } return; }
  loadStats();
}
function localDateInput(date) { const offset = date.getTimezoneOffset() * 60000; return new Date(date - offset).toISOString().slice(0, 16); }
function applyCustomRange(e) { e.preventDefault(); const from = new Date($('#chart-from').value), to = new Date($('#chart-to').value); if (!from.getTime() || !to.getTime() || from >= to) { toast('Choose a valid start and end time', 'error'); return; } state.chartFrom = from.toISOString(); state.chartTo = to.toISOString(); loadStats(); }
async function loadStats() {
  try {
    const x = await api(`/api/stats?${statsQuery()}`); state.stats = x; const count = x.requests ?? x.requests_24h;
    setMetric('#stat-requests', fmtNum(count)); $('#sidebar-requests').textContent = `${fmtNum(count)} requests / ${state.chartRange.toUpperCase()}`; $('#stat-range-label').textContent = state.chartRange.toUpperCase(); $('#traffic-period').textContent = `TRAFFIC / ${rangeLabels[state.chartRange]}`;
    setMetric('#stat-success', `${Math.round(x.success_rate)}<sup>%</sup>`, true); setMetric('#stat-tokens', fmtNum(x.tokens_24h)); setMetric('#stat-cost', x.cost_24h > 0 ? `$${x.cost_24h.toFixed(4)}` : '$0.00');
    setMetric('#stat-provider-latency', latencyLabel(x.avg_upstream_latency_ms)); setMetric('#stat-gateway-latency', latencyLabel(x.avg_gateway_latency_ms));
    $('#stat-latency-pct').textContent = count ? `end-to-end p50 ${latencyLabel(x.p50_latency_ms)} · p95 ${latencyLabel(x.p95_latency_ms)}` : 'end-to-end p50 — · p95 —';
    $('#success-caption').textContent = x.success_rate >= 99 ? 'All systems nominal' : x.success_rate >= 90 ? 'Minor errors detected' : 'Requires attention'; const mark = $('.status-mark', $('#success-caption').parentElement); mark.classList.toggle('good', x.success_rate >= 99); mark.classList.toggle('bad', x.success_rate < 99);
    renderMetricDelta('#requests-delta', count, x.previous?.requests); renderMetricDelta('#success-delta', x.success_rate, x.previous?.success_rate); renderMetricDelta('#tokens-delta', x.tokens_24h, x.previous?.tokens); renderMetricDelta('#cost-delta', x.cost_24h, x.previous?.cost, true); renderMetricDelta('#provider-latency-delta', x.avg_upstream_latency_ms, x.previous?.avg_upstream_latency_ms, true); renderMetricDelta('#gateway-latency-delta', x.avg_gateway_latency_ms, x.previous?.avg_gateway_latency_ms, true);
    renderTrend('#requests-spark', x.hourly, p => p.requests); renderTrend('#success-spark', x.hourly, p => p.requests ? (p.requests - p.errors) / p.requests * 100 : 100); renderTrend('#tokens-spark', x.hourly, p => p.tokens); renderTrend('#cost-spark', x.hourly, p => p.cost); renderTrend('#provider-latency-spark', x.hourly, p => p.upstream_latency_ms); renderTrend('#gateway-latency-spark', x.hourly, p => p.gateway_latency_ms);
    renderChart(x.hourly); renderBreakdowns(x); renderOverviewState(); markFresh('#overview-freshness');
  } catch (e) { toast(e.message, 'error'); }
}
function latencyLabel(value) { const number = Number(value || 0); if (number < 10) return `${number.toFixed(1)} ms`; return `${Math.round(number)} ms`; }
function durationLabel(value) { const number = Number(value || 0); return number >= 1000 ? `${(number / 1000).toFixed(2)} s` : `${Math.round(number)} ms`; }
function setMetric(selector, value, html = false) { const el = $(selector), previous = html ? el.innerHTML : el.textContent; if (previous === value) return; if (html) el.innerHTML = value; else el.textContent = value; el.classList.remove('number-change'); requestAnimationFrame(() => el.classList.add('number-change')); }
function renderMetricDelta(selector, current, previous, inverse = false) { const el = $(selector); current = Number(current || 0); previous = Number(previous || 0); if (!previous) { el.textContent = current ? 'New in this period' : 'No previous data'; el.className = 'metric-delta neutral'; return; } const change = (current - previous) / Math.abs(previous) * 100, up = change >= 0, improved = inverse ? !up : up; el.textContent = `${up ? '▲' : '▼'} ${Math.abs(change).toFixed(Math.abs(change) < 10 ? 1 : 0)}% vs previous`; el.className = `metric-delta ${improved ? 'positive' : 'negative'}`; }
function renderTrend(selector, points, value) { const el = $(selector), recent = (points || []).slice(-20), values = recent.map(value), min = Math.min(...values, 0), max = Math.max(...values, 1), span = Math.max(1, max - min), coords = values.map((v, i) => `${i / Math.max(1, values.length - 1) * 54},${17 - (v - min) / span * 15}`).join(' '); el.innerHTML = `<svg viewBox="0 0 54 19" preserveAspectRatio="none" aria-hidden="true"><polyline points="${coords}" fill="none" stroke="currentColor" stroke-width="2" vector-effect="non-scaling-stroke"/></svg>`; }
function ago(value) { const seconds = Math.max(0, Math.floor((Date.now() - new Date(value)) / 1000)); return seconds < 60 ? 'just now' : seconds < 3600 ? `${Math.floor(seconds / 60)} min ago` : `${Math.floor(seconds / 3600)} h ago`; }
function renderOverviewState() {
  const root = $('#gateway-status'); if (!root) return;
  const enabled = state.providers.filter(p => p.enabled), healthy = enabled.filter(p => p.check?.ok), failing = enabled.filter(p => p.check && !p.check.ok), checking = enabled.length - healthy.length - failing.length;
  const tone = !enabled.length ? 'idle' : failing.length === enabled.length ? 'down' : failing.length ? 'degraded' : checking ? 'checking' : 'ok';
  const headline = { ok: 'All systems operational', degraded: `${failing.length} provider${failing.length === 1 ? '' : 's'} failing`, down: 'All providers failing', checking: 'Checking providers…', idle: 'No providers connected' }[tone];
  const detail = { ok: `Every enabled provider answered its last health check. Traffic is flowing normally.`, degraded: `Requests to ${failing.map(p => p.name).join(', ')} will fail until the key or endpoint is fixed.`, down: 'No upstream provider is reachable — check keys and base URLs on the Providers page.', checking: 'Nexa verifies each provider on start-up and every 10 minutes.', idle: 'Add a provider to start routing traffic through Nexa.' }[tone];
  const lastCheck = enabled.map(p => p.check?.checked_at).filter(Boolean).sort().at(-1);
  const count = state.stats?.requests || 0, errorRate = count ? Math.max(0, 100 - Number(state.stats.success_rate || 0)) : 0, period = (rangeLabels[state.chartRange] || '').toLowerCase();
  const routes = enabled.slice(0, 5).map(p => { const status = p.check?.ok ? 'ok' : p.check ? 'bad' : 'wait'; return `<div class="status-route ${status}" title="${esc(p.check?.error || '')}"><span class="status-wire"><i></i><i></i></span><span class="status-node">${providerMark(p.type)}<b>${esc(p.name)}</b><em>${status === 'ok' ? 'Reachable' : status === 'bad' ? 'Failing' : 'Checking'}</em></span></div>`; }).join('') || '<p class="status-empty">No providers yet</p>';
  root.className = `status-card ${tone}`;
  root.innerHTML = `<div class="status-summary"><span class="status-beacon"><i></i></span><div><span class="status-kicker">GATEWAY STATUS</span><h2>${esc(headline)}</h2><p>${esc(detail)}</p></div></div><div class="status-map"><span class="status-core">N</span><div class="status-routes">${routes}</div></div><dl class="status-stats"><div><dt>Providers healthy</dt><dd>${healthy.length}<small> / ${enabled.length}</small></dd></div><div><dt>Failed requests</dt><dd>${count ? `${errorRate.toFixed(errorRate < 1 && errorRate > 0 ? 1 : 0)}%` : '—'}</dd><small>${count ? `of ${fmtNum(count)} · ${esc(period)}` : 'no traffic yet'}</small></div><p>${lastCheck ? `Health checked ${ago(lastCheck)}` : 'Awaiting first health check'}</p></dl>`;
}
function renderBreakdowns(x) { const providerMax = Math.max(1, ...(x.by_provider || []).map(v => v.requests)), costMax = Math.max(.000001, ...(x.by_model || []).map(v => v.cost)), row = (label, value, width, note) => `<div class="breakdown-row"><div><span title="${esc(label)}">${esc(label)}</span><b>${esc(value)}</b></div><i><span data-width="${Math.max(2, width)}"></span></i>${note ? `<small>${esc(note)}</small>` : ''}</div>`; $('#provider-breakdown').innerHTML = (x.by_provider || []).slice(0, 6).map(v => row(v.provider_name && v.provider_name !== '—' ? v.provider_name : 'Rejected before routing', fmtNum(v.requests), v.requests / providerMax * 100, `${v.requests ? (v.errors / v.requests * 100).toFixed(1) : 0}% errors`)).join('') || '<p class="breakdown-empty">No provider traffic yet.</p>'; $('#model-cost-breakdown').innerHTML = (x.by_model || []).slice().sort((a, b) => b.cost - a.cost).slice(0, 6).map(v => row(v.model || 'Unknown model', fmtCost(v.cost), v.cost / costMax * 100, v.provider_name)).join('') || '<p class="breakdown-empty">No priced model traffic yet.</p>'; const slow = (x.by_model || []).filter(v => v.p95_latency_ms > 0).sort((a, b) => b.p95_latency_ms - a.p95_latency_ms).slice(0, 6), slowMax = Math.max(1, ...slow.map(v => v.p95_latency_ms)); $('#slow-model-breakdown').innerHTML = slow.map(v => row(v.model || 'Unknown model', durationLabel(v.p95_latency_ms), v.p95_latency_ms / slowMax * 100, `${fmtNum(v.requests)} requests`)).join('') || '<p class="breakdown-empty">No latency samples yet.</p>'; applyWidths($('.breakdown-grid')); }
function renderChart(points) {
  if (!points?.length) { $('#traffic-chart').innerHTML = '<div class="chart-empty">No data in this time range.</div>'; return; }
  const w = 960, h = 240, left = 48, right = 12, top = 14, bottom = 34, plotH = h - top - bottom, rawMax = Math.max(1, ...points.map(x => Number(x.requests || 0))), power = Math.max(1, 10 ** Math.floor(Math.log10(rawMax))), max = Math.ceil(rawMax / power) * power, slot = (w - left - right) / points.length, barW = Math.max(3, Math.min(26, slot * .62)), labelEvery = Math.max(1, Math.ceil(points.length / 7)), y = value => top + plotH - Number(value || 0) / max * plotH;
  const ticks = [0, .25, .5, .75, 1].map(r => { const value = Math.round(max * r), yy = y(value); return `<line x1="${left}" x2="${w - right}" y1="${yy}" y2="${yy}"/><text x="${left - 10}" y="${yy + 4}" text-anchor="end">${fmtNum(value)}</text>`; }).join('');
  const bars = points.map((p, i) => { const requests = Number(p.requests || 0), errors = Math.min(requests, Number(p.errors || 0)), success = Math.max(0, requests - errors), x = left + i * slot + (slot - barW) / 2, successY = y(success), totalY = y(requests), base = top + plotH; return `<g><rect class="bar-success" x="${x}" y="${successY}" width="${barW}" height="${Math.max(0, base - successY)}"/><rect class="bar-error" x="${x}" y="${totalY}" width="${barW}" height="${Math.max(0, successY - totalY)}"/><rect class="chart-hit" data-index="${i}" x="${left + i * slot}" y="${top}" width="${Math.max(8, slot)}" height="${plotH}" fill="transparent"/>${i % labelEvery === 0 ? `<text class="chart-x-label" x="${x + barW / 2}" y="${h - 8}" text-anchor="middle">${esc(p.hour)}</text>` : ''}</g>`; }).join('');
  $('#traffic-chart').innerHTML = `<svg viewBox="0 0 ${w} ${h}" role="img" aria-label="Requests by time bucket with errors stacked on top"><g class="chart-grid">${ticks}</g>${bars}</svg><div class="chart-tooltip hidden"></div>`;
  const tooltip = $('.chart-tooltip', $('#traffic-chart')); $$('.chart-hit', $('#traffic-chart')).forEach(hit => { hit.addEventListener('pointerenter', showChartTooltip); hit.addEventListener('pointermove', showChartTooltip); hit.addEventListener('pointerleave', () => tooltip.classList.add('hidden')); });
  function showChartTooltip(e) { const p = points[Number(e.target.dataset.index)]; tooltip.innerHTML = `<b>${esc(new Date(p.timestamp).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }))}</b><span>Requests <strong>${p.requests}</strong></span><span>Errors <strong>${p.errors}</strong></span><span>Provider <strong>${latencyLabel(p.upstream_latency_ms)}</strong></span><span>Nexa <strong>${latencyLabel(p.gateway_latency_ms)}</strong></span>`; tooltip.classList.remove('hidden'); }
}
async function loadRecent() { try { const result = await api('/api/traces?limit=5'); state.traceTotal = result.total; state.recentTraces = result.data; $('#recent-traces').innerHTML = traceRows(result.data, false, true); renderOverviewState(); } catch {} }
function traceRows(rows, details = true, clickable = true, emptyMessage = 'No traces yet. Send a request in Playground to begin.') {
  const columns = details ? 9 : 7; if (!rows.length) return `<tr><td colspan="${columns}" class="empty-row">${esc(emptyMessage)}</td></tr>`;
  return rows.map(t => { const metadata = parseJSON(t.metadata) || {}, lane = metadata.lane || metadata.signals?.lane || metadata.signals?.complexity; return `<tr ${clickable ? `data-trace-id="${esc(t.id)}" tabindex="0"` : ''}><td><span class="status-pill ${t.status === 'success' ? '' : 'error'}">${esc(t.status)}${t.status_code && t.status !== 'success' ? ` · ${t.status_code}` : ''}</span></td><td><span class="route-tag">${esc(t.provider_name || 'Unresolved')}</span>${t.source === 'playground' ? '<small class="source-tag">LAB</small>' : ''}</td><td><span class="model-name">${esc(t.model || 'Unknown model')}</span>${t.stream ? '<small class="source-tag">SSE</small>' : ''}</td>${details ? `<td>${lane ? `<span class="decision-lane ${esc(lane)}">${esc(laneName(lane))}</span>` : '—'}</td><td>${esc(t.api_key_name || 'Master')}</td>` : ''}<td>${fmtNum(t.total_tokens)}</td><td>${durationLabel(t.latency_ms)}</td><td>${fmtCost(t.cost_usd)}</td><td>${details ? esc(fmtDate(t.created_at)) : esc(fmtTime(t.created_at))}</td></tr>`; }).join('');
}

async function loadProviders(render = true, select = false) {
  try {
    state.providers = await api('/api/providers');
    if (render && $('#page-providers').classList.contains('active')) { const metrics = await api('/api/stats?range=24h').catch(() => ({ by_provider: [] })); state.providerStats = Object.fromEntries((metrics.by_provider || []).map(v => [v.provider_id, v])); }
    if (render && $('#page-providers').classList.contains('active')) renderProviders();
    if (select || $('#page-playground').classList.contains('active')) renderProviderSelect();
    const filter = $('#trace-provider'), chosen = filter.value;
    filter.innerHTML = '<option value="">All providers</option>' + state.providers.map(p => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join(''); filter.value = chosen;
    renderOverviewState();
  } catch (e) { toast(e.message, 'error'); }
}
function providerLabel(type) { return ['compatible', 'custom'].includes(type) ? 'CUSTOM' : type.toUpperCase(); }
function providerMark(type) { const known = ['openai', 'groq', 'gemini', 'anthropic']; return `<svg class="brand-logo" aria-hidden="true"><use href="#logo-${known.includes(type) ? type : 'custom'}"/></svg>`; }
function checkBadge(check) { if (!check || check.error === 'paused') return ''; if (check.ok) return `<span class="check-badge ok" title="Verified ${esc(fmtDate(check.checked_at))}">KEY VERIFIED · ${check.models} MODELS</span>`; return `<span class="check-badge bad" title="${esc(check.error)}">CHECK FAILED · ${esc(check.error.slice(0, 60))}</span>`; }
function renderProviders() {
  const root = $('#provider-grid');
  if (!state.providers.length) { root.innerHTML = `<div class="empty-provider"><div class="welcome-glyph">N</div><h2>No routes connected</h2><p>Add an API provider to begin sending traffic through Nexa.</p><button class="primary-action compact" data-open-provider data-admin><span>Add first provider</span>${uiIcon('plus')}</button></div>`; $('[data-open-provider]', root).addEventListener('click', () => openProvider()); return; }
  root.innerHTML = state.providers.map(p => {
    const route = `${p.slug}/model-name`;
    const stateLabel = p.enabled ? 'ACTIVE' : 'PAUSED';
    const stateEl = isAdmin() ? `<button class="provider-state ${p.enabled ? '' : 'off'}" data-toggle-provider="${esc(p.id)}" title="${p.enabled ? 'Pause' : 'Resume'} this provider">${stateLabel}</button>` : `<span class="provider-state ${p.enabled ? '' : 'off'}">${stateLabel}</span>`;
    const metric = state.providerStats[p.id] || {}, errorRate = metric.requests ? metric.errors / metric.requests * 100 : 0;
    return `<article class="provider-card"><header><div class="provider-identity"><span class="provider-logo">${providerMark(p.type)}</span><div><h3>${esc(p.name)}</h3><span>${esc(providerLabel(p.type))}</span></div></div>${stateEl}</header>${checkBadge(p.check)}<div class="provider-metrics"><div><span>REQUESTS / 24H</span><b>${fmtNum(metric.requests)}</b></div><div><span>ERROR RATE</span><b>${errorRate.toFixed(errorRate < 1 ? 1 : 0)}%</b></div><div><span>P95</span><b>${metric.p95_latency_ms ? durationLabel(metric.p95_latency_ms) : '—'}</b></div></div><dl><div><dt>ROUTE TEMPLATE</dt><dd title="${esc(route)}">${esc(route)}</dd></div><div><dt>SECRET</dt><dd>${esc(p.masked_key)}</dd></div><div><dt>BASE URL</dt><dd title="${esc(p.base_url)}">${esc(p.base_url)}</dd></div>${p.header_names?.length ? `<div><dt>EXTRA HEADERS</dt><dd>${esc(p.header_names.join(', '))}</dd></div>` : ''}</dl><footer><button class="copy-route" data-copy="${esc(route)}">${uiIcon('copy')}Copy route</button><details class="card-menu"><summary aria-label="More provider actions">${uiIcon('more')}</summary><div><button data-copy-snippet="${esc(p.slug)}/YOUR_MODEL">Copy snippet</button><button data-edit-provider="${esc(p.id)}" data-admin>Edit provider</button><button class="danger-menu" data-delete-provider="${esc(p.id)}" data-admin>Remove provider</button></div></details></footer></article>`;
  }).join('');
}
const bases = { openai: 'https://api.openai.com/v1', groq: 'https://api.groq.com/openai/v1', gemini: 'https://generativelanguage.googleapis.com/v1beta/openai', anthropic: 'https://api.anthropic.com/v1', custom: '' };
function openProvider(id = '') {
  const p = state.providers.find(x => x.id === id); $('#provider-form').reset(); $('#provider-id').value = p?.id || ''; $('#provider-dialog-title').textContent = p ? 'Edit provider' : 'Add provider';
  const storedType = p?.type || 'openai', type = storedType === 'compatible' ? 'custom' : storedType; $(`input[name=provider-type][value="${type}"]`).checked = true;
  $('#provider-name').value = p?.name || ''; $('#provider-slug').value = p?.slug || ''; $('#provider-url').value = p?.base_url || bases[type]; $('#provider-key').value = ''; $('#provider-key').required = !p;
  $('#provider-key').placeholder = p ? `Keep existing (${p.masked_key})` : 'Stored with AES-256 encryption'; $('#provider-enabled').checked = p ? p.enabled : true;
  $('#provider-headers').value = ''; state.headersDirty = false; $('#provider-headers-status').textContent = p?.header_names?.length ? `SAVED: ${p.header_names.join(', ')}` : '';
  $('#provider-headers').placeholder = p?.header_names?.length ? `Keep saved headers (${p.header_names.join(', ')}) — type to replace them` : 'Optional, one per line — e.g. HTTP-Referer: https://myapp.example';
  updateProviderFields(type); setError('#provider-error'); $('#provider-dialog').showModal();
}
function updateProviderFields(type) { const custom = type === 'custom'; $('#provider-url').required = custom; $('#provider-url').placeholder = custom ? 'https://your-provider.example/v1' : 'Filled automatically'; $('#provider-url-note').textContent = custom ? 'Enter the OpenAI-compatible API base URL for your provider.' : 'Provider endpoint is filled automatically.'; }
function providerTypeChanged(e) { if (!$('#provider-id').value || $('#provider-url').value === '' || Object.values(bases).includes($('#provider-url').value)) $('#provider-url').value = bases[e.target.value]; const label = e.target.value === 'custom' ? 'Custom' : e.target.value[0].toUpperCase() + e.target.value.slice(1); if (!$('#provider-name').value) $('#provider-name').value = label; if (!$('#provider-slug').value) $('#provider-slug').value = e.target.value; updateProviderFields(e.target.value); }
function slugify(v) { return v.toLowerCase().trim().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, ''); }
function providerCheckToast(p, verb) {
  if (!p.enabled) toast(`Provider ${verb} (paused)`);
  else if (p.check?.ok) toast(`Provider ${verb} · key verified (${p.check.models} models)`);
  else toast(`Provider ${verb}, but the key check failed: ${p.check?.error || 'unknown error'}`, 'error');
}
async function saveProvider(e) {
  e.preventDefault(); const id = $('#provider-id').value;
  const body = { name: $('#provider-name').value, slug: $('#provider-slug').value, type: $('input[name=provider-type]:checked').value, base_url: $('#provider-url').value, api_key: $('#provider-key').value, enabled: $('#provider-enabled').checked };
  if (state.headersDirty || (!id && $('#provider-headers').value.trim())) body.extra_headers = $('#provider-headers').value;
  const button = $('#provider-form [type=submit]'); button.disabled = true;
  try { const p = await api(id ? `/api/providers/${id}` : '/api/providers', { method: id ? 'PUT' : 'POST', body: JSON.stringify(body) }); $('#provider-dialog').close(); providerCheckToast(p, id ? 'updated' : 'connected'); await loadProviders(); }
  catch (err) { setError('#provider-error', err.message); }
  finally { button.disabled = false; }
}
async function toggleProvider(id) {
  const p = state.providers.find(x => x.id === id); if (!p) return;
  try { const updated = await api(`/api/providers/${id}`, { method: 'PUT', body: JSON.stringify({ name: p.name, slug: p.slug, type: p.type, base_url: p.base_url, enabled: !p.enabled }) }); providerCheckToast(updated, updated.enabled ? 'resumed' : 'paused'); loadProviders(); }
  catch (e) { toast(e.message, 'error'); }
}
async function deleteProvider(id) { const p = state.providers.find(x => x.id === id); if (!await confirmAction(`Remove ${p?.name || 'this provider'}?`, 'Existing traces remain. Providers used by a smart routing profile must be removed from it first.', 'Remove')) return; try { await api(`/api/providers/${id}`, { method: 'DELETE' }); toast('Provider removed'); loadProviders(); } catch (e) { toast(e.message, 'error'); } }

async function loadPrices() {
  try {
    const x = await api('/api/prices'); const custom = new Set(x.custom.map(p => p.model.toLowerCase()));
    const used = new Set(); const metrics = await api('/api/stats?range=30d').catch(() => ({ by_model: [] })); (metrics.by_model || []).forEach(v => used.add(String(v.model).toLowerCase())); state.prices = [...x.custom.map(p => ({ ...p, source: 'custom', used: used.has(p.model.toLowerCase()) })), ...x.builtin.filter(p => !custom.has(p.model.toLowerCase())).map(p => ({ ...p, source: 'built-in', used: used.has(p.model.toLowerCase()) }))].sort((a, b) => Number(b.used) - Number(a.used) || Number(b.source === 'custom') - Number(a.source === 'custom') || a.model.localeCompare(b.model)); renderPrices();
  } catch (e) { toast(e.message, 'error'); }
}
function renderPrices() { const query = $('#price-search').value.trim().toLowerCase(); let rows = state.prices.filter(p => !query || p.model.toLowerCase().includes(query)); const total = rows.length; if (!query && !state.priceExpanded) rows = rows.slice(0, 12); $('#price-expand').classList.toggle('hidden', !!query || total <= 12); $('#price-expand').textContent = state.priceExpanded ? 'Show used first' : `Show all ${total}`; $('#price-table').innerHTML = rows.length ? rows.map(p => `<tr><td><span class="model-name">${esc(p.model)}</span>${p.used ? '<small class="used-model">USED</small>' : ''}</td><td>$${p.input}</td><td>$${p.output}</td><td><span class="source-tag ${p.source === 'custom' ? 'custom' : ''}">${p.source.toUpperCase()}</span></td><td>${p.source === 'custom' ? `<button class="detail-button" data-delete-price="${esc(p.model)}" data-admin title="Delete price" aria-label="Delete price">${uiIcon('trash')}</button>` : ''}</td></tr>`).join('') : '<tr><td colspan="5" class="empty-row">No matching model prices.</td></tr>'; }
async function savePrice(e) {
  e.preventDefault();
  try { await api('/api/prices', { method: 'PUT', body: JSON.stringify({ model: $('#price-model').value.trim(), input: Number($('#price-input').value), output: Number($('#price-output').value) }) }); $('#price-form').reset(); toast('Price saved · applies to new traces'); loadPrices(); }
  catch (err) { toast(err.message, 'error'); }
}
async function deletePrice(model) { try { await api(`/api/prices?model=${encodeURIComponent(model)}`, { method: 'DELETE' }); toast('Price removed'); loadPrices(); } catch (e) { toast(e.message, 'error'); } }

async function loadRoutingProfiles(render = true) {
  try {
    state.routingProfiles = await api('/api/routing/profiles');
    if (render && $('#page-routing').classList.contains('active')) renderRoutingProfiles();
    if ($('#page-playground').classList.contains('active')) renderProviderSelect();
    const preview = $('#preview-profile'), chosen = preview.value;
    preview.innerHTML = state.routingProfiles.length ? state.routingProfiles.map(p => `<option value="${esc(p.id)}">${esc(p.name)}${p.active ? ' · active' : ''}</option>`).join('') : '<option value="">Create a profile first</option>';
    if (chosen && state.routingProfiles.some(p => p.id === chosen)) preview.value = chosen;
  } catch (e) { toast(e.message, 'error'); }
}
async function loadRoutingStats() { try { state.routingStats = await api('/api/stats?range=7d'); if ($('#page-routing').classList.contains('active') && state.routingProfiles.length) renderRoutingProfiles(); } catch (e) { toast(e.message, 'error'); } }
const laneName = tier => ({ low: 'LIGHT', medium: 'MEDIUM', high: 'HEAVY' })[tier] || String(tier || '').toUpperCase();
function renderRoutingProfiles() {
  const root = $('#routing-profiles');
  if (!state.routingProfiles.length) { root.innerHTML = '<div class="routing-empty"><span class="panel-kicker">NO PROFILES YET</span><h2>Build your first decision ladder.</h2><p>Connect at least one provider, then map prompt difficulty to models with a fallback chain.</p></div>'; return; }
  root.innerHTML = state.routingProfiles.map(p => { const usage = (state.routingStats?.routing || []).filter(v => v.profile_slug === p.slug), total = usage.reduce((sum, v) => sum + v.requests, 0), counts = Object.fromEntries(usage.map(v => [v.lane, v.requests])); return `<article class="routing-profile-card ${p.active ? 'active' : ''}"><header><div><p>JEV / TYPESAFE · ${p.jev_key_configured ? 'KEY READY' : 'NO KEY — FALLBACK LANE ONLY'}</p><h3>${esc(p.name)}</h3><p>smart/${esc(p.slug)}</p></div><span class="${p.active ? 'profile-active' : 'profile-inactive'}">${p.active ? 'ACTIVE DEFAULT' : 'STANDBY'}</span></header><div class="profile-lanes">${['low', 'medium', 'high'].map(tier => { const target = p.targets.filter(t => t.tier === tier).sort((a, b) => a.priority - b.priority)[0], provider = state.providers.find(x => x.id === target?.provider_id), share = total ? Math.round((counts[tier] || 0) / total * 100) : 0; return `<div class="profile-lane ${tier}"><div><span>${laneName(tier)}</span><b>${share}% <small>7D SHARE</small></b></div><strong>${target ? `${esc(provider?.slug || 'missing')}/${esc(target.model)}` : 'Not configured'}</strong><i><span data-width="${share}"></span></i></div>`; }).join('<span class="lane-arrow">→</span>')}</div><div class="profile-route simplified"><div><span>SMART REQUESTS / 7D</span><b>${fmtNum(total)}</b></div><div><span>IF JEV FAILS</span><b>${laneName(p.fallback_lane)}</b></div><div><span>ESCALATE BELOW</span><b>${p.confidence_threshold ? `${Math.round(p.confidence_threshold * 100)}%` : 'OFF'}</b></div></div><footer>${p.active ? `<button data-copy-snippet="smart">COPY SNIPPET</button>` : `<button data-activate-routing="${esc(p.id)}" data-admin>SET ACTIVE</button>`}<button data-check-routing="${esc(p.id)}" data-admin>TEST JEV</button><button data-edit-routing="${esc(p.id)}" data-admin>EDIT</button><button data-delete-routing="${esc(p.id)}" data-admin>REMOVE</button></footer></article>`; }).join(''); applyWidths(root);
}
function laneDescription(tier) { return ({ low: 'Simple, short and straightforward prompts requiring light reasoning.', medium: 'Moderately complex prompts requiring several reasoning steps.', high: 'Complex, ambiguous or expert prompts requiring deep reasoning.' })[tier] || ''; }
function blankTarget(priority = 0) { const tier = ['low', 'medium', 'high'][Math.min(priority, 2)]; return { id: crypto.randomUUID?.() || `${Date.now()}-${priority}`, provider_id: state.providers.find(p => p.enabled)?.id || '', model: '', tier, description: laneDescription(tier), unsupported: [], input_cost_per_million: 0, output_cost_per_million: 0, priority, enabled: true }; }
function openRoutingProfile(id = '') {
  const p = state.routingProfiles.find(x => x.id === id); $('#routing-form').reset(); $('#routing-id').value = p?.id || ''; $('#routing-dialog-title').textContent = p ? 'Edit smart profile' : 'Create smart profile';
  $('#routing-name').value = p?.name || ''; $('#routing-slug').value = p?.slug || ''; $('#routing-jev-key').value = ''; $('#jev-key-status').textContent = p?.jev_key_configured ? 'KEY READY' : '';
  $('#routing-fallback').value = p?.fallback_lane || 'medium'; $('#routing-threshold').value = p ? p.confidence_threshold : 0.6; $('#routing-timeout').value = p ? Math.round(p.request_timeout_ms / 1000) : 120; $('#routing-retries').value = p ? p.max_retries : 1;
  state.routingTargets = structuredClone(p?.targets || [blankTarget(0)]); state.routingModels = {}; setError('#routing-error'); renderRoutingTargets(); $('#routing-dialog').showModal();
}
function providerOptions(selected) { return state.providers.filter(p => p.enabled).map(p => `<option value="${esc(p.id)}" ${p.id === selected ? 'selected' : ''}>${esc(p.name)} · ${esc(p.slug)}</option>`).join(''); }
function addRoutingTarget() { state.routingTargets.push(blankTarget(state.routingTargets.length)); renderRoutingTargets(); }
function renderRoutingTargets() {
  const capability = (t, name) => `<label class="check"><input type="checkbox" data-capability="${name}" ${(t.unsupported || []).includes(name) ? '' : 'checked'}> ${name}</label>`;
  $('#routing-targets').innerHTML = state.routingTargets.map((t, i) => `<div class="target-editor" data-target-index="${i}"><div class="lane-number"><span>${String(i + 1).padStart(2, '0')}</span><i></i></div><label><span>PROVIDER</span><select data-target-field="provider_id">${providerOptions(t.provider_id)}</select></label><label class="model-select-field"><span>MODEL ID <b data-model-count></b></span><select data-target-field="model" required><option value="${esc(t.model)}">${t.model ? esc(t.model) : 'Loading live models…'}</option></select></label><label><span>DIFFICULTY LANE</span><select data-target-field="tier"><option value="low" ${t.tier === 'low' ? 'selected' : ''}>LIGHT</option><option value="medium" ${t.tier === 'medium' ? 'selected' : ''}>MEDIUM</option><option value="high" ${t.tier === 'high' ? 'selected' : ''}>HEAVY</option></select></label><div class="lane-summary ${esc(t.tier)}"><b>${esc(laneName(t.tier))}</b><span>${esc(laneDescription(t.tier))}</span></div><button type="button" data-remove-target="${i}" title="Remove lane" aria-label="Remove lane">${uiIcon('trash')}</button><div class="lane-extra"><span>SUPPORTS</span>${capability(t, 'tools')}${capability(t, 'vision')}${capability(t, 'json')}<label class="price-input">IN $/1M<input type="number" min="0" step="0.0001" data-target-field="input_cost_per_million" value="${Number(t.input_cost_per_million || 0)}"></label><label class="price-input">OUT $/1M<input type="number" min="0" step="0.0001" data-target-field="output_cost_per_million" value="${Number(t.output_cost_per_million || 0)}"></label><small>0 = use model pricing table</small></div></div>`).join('');
  $$('.target-editor', $('#routing-targets')).forEach((row, index) => {
    const target = state.routingTargets[index];
    const provider = $('[data-target-field=provider_id]', row), model = $('[data-target-field=model]', row), tier = $('[data-target-field=tier]', row);
    provider.addEventListener('change', () => { target.provider_id = provider.value; target.model = ''; loadRoutingTargetModels(index, true); });
    model.addEventListener('change', () => { target.model = model.value; });
    tier.addEventListener('change', () => { target.tier = tier.value; target.description = laneDescription(tier.value); renderRoutingTargets(); });
    $$('[data-capability]', row).forEach(box => box.addEventListener('change', () => { target.unsupported = $$('[data-capability]', row).filter(x => !x.checked).map(x => x.dataset.capability); }));
    $$('.price-input input', row).forEach(input => input.addEventListener('input', () => { target[input.dataset.targetField] = Number(input.value || 0); }));
    loadRoutingTargetModels(index);
  });
}
async function loadRoutingTargetModels(index, force = false) {
  const target = state.routingTargets[index], row = $(`[data-target-index="${index}"]`, $('#routing-targets')); if (!target || !row) return;
  const select = $('[data-target-field=model]', row), count = $('[data-model-count]', row), providerID = target.provider_id;
  if (!providerID) { select.innerHTML = '<option value="">Choose a provider first</option>'; return; }
  select.disabled = true; select.innerHTML = '<option value="">Loading live models…</option>';
  try {
    if (force || !state.routingModels[providerID]) { const list = await api(`/api/providers/${providerID}/models?capability=chat`); state.routingModels[providerID] = [...new Set(list.map(x => x.id || x.name).filter(Boolean))].sort(); }
    if (state.routingTargets[index]?.provider_id !== providerID) return;
    const names = state.routingModels[providerID], saved = target.model && !names.includes(target.model) ? [target.model] : [];
    select.innerHTML = [...saved, ...names].map(x => `<option value="${esc(x)}">${esc(x)}${saved.includes(x) ? ' · SAVED' : ''}</option>`).join('') || '<option value="">No chat models returned</option>';
    if (target.model && [...select.options].some(x => x.value === target.model)) select.value = target.model; else target.model = select.value;
    count.textContent = `${names.length} LIVE`; select.disabled = false;
  } catch (err) { select.innerHTML = '<option value="">Could not load models</option>'; count.textContent = 'ERROR'; toast(err.message, 'error'); }
}
async function saveRoutingProfile(e) {
  e.preventDefault();
  if (!state.routingTargets.length) { setError('#routing-error', 'Add at least one model lane.'); return; }
  if (state.routingTargets.some(t => !t.provider_id || !t.model)) { setError('#routing-error', 'Choose a real provider model for every lane.'); return; }
  const id = $('#routing-id').value;
  const body = { name: $('#routing-name').value, slug: $('#routing-slug').value, jev_api_key: $('#routing-jev-key').value, fallback_lane: $('#routing-fallback').value, confidence_threshold: Number($('#routing-threshold').value), request_timeout_ms: Number($('#routing-timeout').value) * 1000, max_retries: Number($('#routing-retries').value),
    targets: state.routingTargets.map((t, i) => ({ id: t.id, provider_id: t.provider_id, model: t.model, tier: t.tier, description: laneDescription(t.tier), unsupported: t.unsupported || [], input_cost_per_million: Number(t.input_cost_per_million || 0), output_cost_per_million: Number(t.output_cost_per_million || 0), priority: i, enabled: true, context_window: 1000000, max_output_tokens: 1000000 })) };
  try { await api(id ? `/api/routing/profiles/${id}` : '/api/routing/profiles', { method: id ? 'PUT' : 'POST', body: JSON.stringify(body) }); $('#routing-dialog').close(); toast(id ? 'Routing profile updated' : 'Routing profile created'); loadRoutingProfiles(); }
  catch (err) { setError('#routing-error', err.message); }
}
async function activateRoutingProfile(id) { try { await api(`/api/routing/profiles/${id}/activate`, { method: 'POST', body: '{}' }); toast('Default smart route activated'); loadRoutingProfiles(); } catch (e) { toast(e.message, 'error'); } }
async function checkRoutingProfile(id, button) {
  button.disabled = true; button.textContent = 'TESTING…';
  try { const r = await api(`/api/routing/profiles/${id}/check`, { method: 'POST', body: '{}' }); if (r.ok) toast(`Jev OK · ${laneName(r.complexity)} · ${Math.round(r.confidence * 100)}% · ${r.latency_ms} ms`); else toast(`Jev check failed: ${r.error}`, 'error'); }
  catch (e) { toast(e.message, 'error'); }
  finally { button.disabled = false; button.textContent = 'TEST JEV'; }
}
async function deleteRoutingProfile(id) {
  const p = state.routingProfiles.find(x => x.id === id);
  const message = p?.active ? 'This is the ACTIVE default profile. Applications calling model "smart" will fail until another profile is activated.' : 'Applications calling this profile by slug will stop working.';
  if (!await confirmAction(`Remove routing profile ${p?.name || ''}?`, message, 'Remove')) return;
  try { await api(`/api/routing/profiles/${id}`, { method: 'DELETE' }); toast('Routing profile removed'); loadRoutingProfiles(); } catch (e) { toast(e.message, 'error'); }
}
async function previewRoute(e) {
  e.preventDefault(); const out = $('#route-preview-result'); const profileID = $('#preview-profile').value;
  if (!profileID) { toast('Create a routing profile first', 'error'); return; }
  const capabilities = [['tools', '#preview-tools'], ['vision', '#preview-vision']].filter(([, id]) => $(id).checked).map(([name]) => name);
  out.classList.remove('hidden'); out.innerHTML = '<p class="muted">Asking Jev…</p>';
  try {
    const d = await api('/api/routing/evaluate', { method: 'POST', body: JSON.stringify({ profile_id: profileID, prompt: $('#preview-prompt').value, max_output_tokens: 1024, capabilities }) });
    const s = d.signals, first = d.ranked[0];
    const byLane = Object.fromEntries(d.ranked.map(r => [r.target.tier, r])); out.innerHTML = `<div class="preview-head"><span class="decision-lane ${esc(s.lane)}">${esc(laneName(s.lane))}</span><p>Jev rated this <strong>${esc(laneName(s.complexity))}</strong> at <strong>${Math.round((s.confidence || 0) * 100)}%</strong> confidence${s.escalated ? ' — below the threshold, so Nexa escalated one lane' : ''}. First route: <strong>${esc(first.provider_slug)}/${esc(first.target.model)}</strong>.</p><small>${s.cached ? 'CACHED DECISION' : `JEV ${Math.round(s.jev_latency_ms || 0)} MS`}</small></div>${d.engine_error ? `<p class="form-error">${esc(d.engine_error)}</p>` : ''}<div class="preview-lanes">${['low', 'medium', 'high'].map(lane => { const r = byLane[lane]; return `<div class="${s.lane === lane ? 'selected' : ''}"><span>${laneName(lane)}</span><strong>${r ? `${esc(r.provider_slug)}/${esc(r.target.model)}` : 'Not configured'}</strong><small>${s.lane === lane ? 'SELECTED' : r ? esc(r.reasons.join(', ')) : '—'}</small></div>`; }).join('<i>→</i>')}</div><ol class="preview-ladder">${d.ranked.map(r => `<li><span>${esc(r.provider_slug)}/${esc(r.target.model)}</span><small>${esc(laneName(r.target.tier))} · ${esc(r.reasons.join(', '))}${r.healthy ? '' : ' · UNHEALTHY'}</small></li>`).join('')}</ol>`;
  } catch (err) { out.innerHTML = `<p class="form-error">${esc(err.message)}</p>`; }
}

function traceQuery(offset) {
  const q = new URLSearchParams({ limit: '100', offset: String(offset), status: state.status });
  const search = $('#trace-search').value.trim(); if (search) q.set('search', search);
  if ($('#trace-provider').value) q.set('provider', $('#trace-provider').value);
  if ($('#trace-model').value.trim()) q.set('model', $('#trace-model').value.trim());
  if ($('#trace-from').value) q.set('from', new Date($('#trace-from').value).toISOString());
  if ($('#trace-to').value) q.set('to', new Date($('#trace-to').value).toISOString());
  return q;
}
async function loadTraces(append = false) {
  const requestID = ++state.traceRequest, search = $('#trace-search').value.trim();
  $('.search-field').classList.add('searching');
  try {
    const result = await api(`/api/traces?${traceQuery(append ? state.traces.length : 0)}`); if (requestID !== state.traceRequest) return;
    state.traces = append ? [...state.traces, ...result.data] : result.data; state.traceTotal = result.total;
    $('#trace-total').textContent = fmtNum(result.total);
    $('#trace-table').innerHTML = traceRows(state.traces, true, true, search ? `No traces match “${search}”.` : 'No traces match these filters yet.');
    $('#trace-more').classList.toggle('hidden', state.traces.length >= result.total); $('#trace-more').textContent = `LOAD MORE · ${fmtNum(result.total - state.traces.length)} REMAINING`;
    markFresh('#trace-freshness');
  } catch (e) { if (requestID === state.traceRequest) toast(e.message, 'error'); }
  finally { if (requestID === state.traceRequest) $('.search-field').classList.remove('searching'); }
}
async function openTrace(id, list) {
  if (list !== undefined) state.traceList = list || [];
  try { state.selectedTrace = await api(`/api/traces/${id}`); state.traceView = 'readable'; $$('[data-trace-view]').forEach(x => x.classList.toggle('active', x.dataset.traceView === 'readable')); $('#trace-view-description').textContent = 'Conversation formatted for reading'; const t = state.selectedTrace, provider = t.provider_name && t.provider_name !== '—' ? t.provider_name : '', route = `${provider ? `${provider} / ` : ''}${t.model || 'Unknown model'}`; $('#trace-dialog-title').textContent = t.status === 'success' ? route : t.provider_id ? `Failed · ${route}` : `Rejected · ${route}`; updateTraceStepper(); renderTraceDetail(); if (!$('#trace-dialog').open) $('#trace-dialog').showModal(); } catch (e) { toast(e.message, 'error'); }
}
function updateTraceStepper() { const list = state.traceList, index = list.findIndex(t => t.id === state.selectedTrace?.id); $('#trace-nav').classList.toggle('hidden', index < 0 || list.length < 2); $('#trace-position').textContent = `${index + 1} / ${list.length}`; $('#trace-previous').disabled = index <= 0; $('#trace-next').disabled = index < 0 || index >= list.length - 1; }
function stepTrace(direction) { const list = state.traceList, index = list.findIndex(t => t.id === state.selectedTrace?.id), next = list[index + direction]; if (next) openTrace(next.id); }
function parseJSON(value) { try { return JSON.parse(value); } catch { return null; } }
function traceMessages(value) {
  const parsed = parseJSON(value); if (!Array.isArray(parsed)) return `<div class="plain-copy">${esc(value || 'No input captured.')}</div>`;
  return `<div class="message-stack">${parsed.map(message => { const role = String(message.role || 'message').toUpperCase(); const content = typeof message.content === 'string' ? message.content : JSON.stringify(message.content, null, 2); return `<article class="trace-message ${esc(role.toLowerCase())}"><span>${esc(role)}</span><div class="markdown-body">${role === 'ASSISTANT' ? renderMarkdown(content) : `<p>${esc(content || '—')}</p>`}</div></article>`; }).join('')}</div>`;
}
function traceResponse(value) { const parsed = parseJSON(value); const text = parsed?.error?.message || parsed?.choices?.[0]?.message?.content || value || 'No output captured.'; return `<div class="plain-copy markdown-body">${renderMarkdown(typeof text === 'string' ? text : JSON.stringify(text, null, 2))}</div>`; }
function jsonTraceValue(value, role) { const parsed = parseJSON(value); if (parsed !== null) return JSON.stringify(parsed, null, 2); return JSON.stringify({ role, content: value || '' }, null, 2); }
function traceRoutingDetail(t) {
  const m = parseJSON(t.metadata); if (!m?.smart_routing) return '';
  const s = m.signals || {}, lane = s.lane || s.complexity || 'medium', attempts = m.attempts || [];
  const selected = attempts.slice().reverse().find(a => a.status_code >= 200 && a.status_code < 300)?.route || `${t.provider_name}/${t.model}`, fallbacks = Math.max(0, attempts.length - 1);
  return `<section class="routing-trace"><header><div><span>SMART ROUTING DECISION · smart/${esc(m.profile_slug || '')}</span><b>${esc(selected)}</b></div><em>${Math.round((s.confidence || 0) * 100)}% CONFIDENCE</em></header><div class="routing-decision-copy"><span class="decision-lane ${esc(lane)}">${esc(laneName(lane))}</span><p>Jev rated this conversation <strong>${esc(laneName(s.complexity || lane))}</strong> with <strong>${Math.round((s.confidence || 0) * 100)}% confidence</strong>${s.escalated ? ', below the threshold, so Nexa escalated it one lane' : ''}, then routed it to <strong>${esc(selected)}</strong>.</p><small>JEV ENGINE${s.cached ? ' · CACHED' : s.jev_latency_ms ? ` · ${Math.round(s.jev_latency_ms)} MS` : ''}${fallbacks ? ` · ${fallbacks} FALLBACK ${fallbacks === 1 ? 'ATTEMPT' : 'ATTEMPTS'}` : ''}</small></div>${attempts.length > 1 ? `<div class="routing-trace-attempts">${attempts.map((a, i) => `<div class="${a.status_code >= 200 && a.status_code < 300 ? 'success' : 'failed'}"><i>${i + 1}</i><span>${esc(a.route)}${a.error ? ` — ${esc(a.error.slice(0, 80))}` : ''}</span><b>${a.status_code || 'NET'} · ${fmtNum(a.latency_ms)} MS</b></div>`).join('')}</div>` : ''}${m.engine_error ? `<p>${esc(m.engine_error)}</p>` : ''}</section>`;
}
function traceTimeline(t) { const metadata = parseJSON(t.metadata) || {}, signals = metadata.signals || {}, attempts = Array.isArray(metadata.attempts) ? metadata.attempts : [], total = Math.max(1, Number(t.latency_ms || 0)), stages = [{ label: 'Gateway work', detail: durationLabel(t.gateway_latency_ms), tone: 'gateway' }]; if (metadata.smart_routing) stages.push({ label: 'Jev decision', detail: `${laneName(signals.lane || signals.complexity)} · ${durationLabel(signals.jev_latency_ms)}`, tone: 'jev' }); attempts.forEach((attempt, index) => stages.push({ label: attempts.length > 1 ? `Attempt ${index + 1}` : 'Provider', detail: `${attempt.route || t.model} · ${attempt.status_code || 'NET'} · ${durationLabel(attempt.latency_ms)}`, tone: attempt.status_code >= 200 && attempt.status_code < 300 ? 'success' : 'failed' })); if (!attempts.length && (t.provider_id || t.upstream_latency_ms > 0)) stages.push({ label: 'Provider', detail: durationLabel(t.upstream_latency_ms), tone: t.status === 'success' ? 'success' : 'failed' }); if (t.ttft_ms) stages.push({ label: 'First token', detail: durationLabel(t.ttft_ms), tone: 'token' }); stages.push({ label: t.status === 'success' ? 'Done' : 'Rejected', detail: durationLabel(total), tone: t.status === 'success' ? 'success' : 'failed' }); return `<section class="trace-timeline"><header><div><span>REQUEST TIMELINE</span><h3>Gateway to completion</h3></div><small>${durationLabel(total)} TOTAL</small></header><div class="timeline-bar">${stages.map(stage => `<span class="${stage.tone}"></span>`).join('')}</div><div class="timeline-events">${stages.map(stage => `<div><i class="${stage.tone}"></i><span>${esc(stage.label)}</span><strong>${esc(stage.detail)}</strong></div>`).join('')}</div></section>`; }
function renderTraceDetail() {
  const t = state.selectedTrace; if (!t) return;
  const latencyDetail = t.upstream_latency_ms ? `${fmtNum(t.upstream_latency_ms)} ms provider + ${fmtNum(t.gateway_latency_ms)} ms Nexa` : `${fmtNum(t.latency_ms)} ms total`;
  const params = parseJSON(t.params) || {}, paramText = Object.entries(params).filter(([k]) => k !== 'stream_options').map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : v}`).join(' · ');
  const content = state.traceView === 'json'
    ? `<div class="trace-content"><section class="trace-block"><h3>FULL REQUEST / JSON</h3><pre>${esc(jsonTraceValue(t.request || t.prompt, 'user'))}</pre></section><section class="trace-block"><h3>OUTPUT / JSON</h3><pre>${esc(jsonTraceValue(t.response, 'assistant'))}</pre></section></div>`
    : `<div class="trace-content readable"><section class="trace-block"><h3>INPUT / CURRENT TURN</h3>${traceMessages(t.prompt)}${paramText ? `<p class="trace-params">${esc(paramText)}</p>` : ''}</section><section class="trace-block"><h3>OUTPUT / RESPONSE</h3>${traceResponse(t.response)}</section></div>`;
  const cell = (label, value, title = '') => `<div><span>${label}</span><b title="${esc(title)}">${value}</b></div>`;
  $('#trace-detail').innerHTML = `<div class="trace-id-line"><span>${esc(t.request_id || t.id)}</span><time>${esc(fmtDate(t.created_at))}</time></div><div class="trace-summary">${cell('OUTCOME', `${esc(t.status.toUpperCase())} / ${t.status_code}`)}${cell('TOKENS', `${fmtNum(t.input_tokens)} IN · ${fmtNum(t.output_tokens)} OUT`)}${cell('LATENCY', durationLabel(t.latency_ms), latencyDetail)}${cell('EST. COST', fmtCost(t.cost_usd))}${cell('FIRST TOKEN', t.ttft_ms ? durationLabel(t.ttft_ms) : t.stream ? '—' : 'NOT STREAMED')}${cell('FINISH', esc((t.finish_reason || '—').toUpperCase()))}${cell('CACHED · REASONING', `${fmtNum(t.cached_tokens)} · ${fmtNum(t.reasoning_tokens)}`)}${cell('CALLER', esc(`${(t.source || 'api').toUpperCase()} · ${t.api_key_name || '—'}`), `Request ID ${t.request_id || '—'}`)}</div>${traceTimeline(t)}${traceRoutingDetail(t)}${t.error ? `<div class="form-error">${esc(t.error)}</div>` : ''}${content}`;
}
function renderProviderSelect() {
  const select = $('#play-provider'), previous = select.value, smart = state.routingProfiles.length ? '<option value="__smart__">Nexa Smart Routing · AUTO</option>' : '';
  select.innerHTML = smart + state.providers.filter(p => p.enabled).map(p => `<option value="${esc(p.id)}">${esc(p.name)} · ${esc(p.slug)}</option>`).join('');
  if (previous && [...select.options].some(o => o.value === previous)) select.value = previous; if (!select.options.length) select.innerHTML = '<option value="">Add a provider first</option>'; const compare = $('#compare-provider'), compareValue = compare.value; compare.innerHTML = select.innerHTML; if (compareValue && [...compare.options].some(o => o.value === compareValue)) compare.value = compareValue; else if (compare.options.length > 1) compare.selectedIndex = 1; loadModels(); if ($('#play-compare').checked) loadCompareModels();
}
async function loadModels(refresh = false) {
  const id = $('#play-provider').value, model = $('#play-model'), row = model.closest('.model-input-row'); model.innerHTML = '<option>Fetching model catalog…</option>'; model.disabled = true; row.classList.add('loading');
  const done = () => { model.disabled = false; row.classList.remove('loading'); updateChatRoute(); };
  if (!id) { model.innerHTML = '<option value="">No provider available</option>'; done(); return; }
  if (id === '__smart__') { const active = state.routingProfiles.find(p => p.active); model.innerHTML = `${active ? '<option value="smart">smart · active default</option>' : ''}${state.routingProfiles.map(p => `<option value="smart/${esc(p.slug)}">smart/${esc(p.slug)} · ${esc(p.name)}</option>`).join('')}`; $('#model-count').textContent = `${state.routingProfiles.length} PROFILES`; done(); return; }
  try { const list = await api(`/api/providers/${id}/models?capability=chat${refresh ? '&refresh=1' : ''}`); const names = [...new Set(list.map(x => x.id || x.name).filter(Boolean))].sort(); model.innerHTML = names.map(x => `<option value="${esc(x)}">${esc(x)}</option>`).join('') || '<option value="">No chat models returned</option>'; $('#model-count').textContent = `${names.length} CHAT`; }
  catch (e) { model.innerHTML = '<option value="">Could not load models</option>'; toast(e.message, 'error'); }
  finally { done(); }
}
function toggleComparison() { const enabled = $('#play-compare').checked; $('#compare-controls').classList.toggle('hidden', !enabled); if (enabled) loadCompareModels(); }
async function loadCompareModels(refresh = false) { const id = $('#compare-provider').value, model = $('#compare-model'), row = model.closest('.model-input-row'); model.disabled = true; row.classList.add('loading'); model.innerHTML = '<option>Fetching model catalog…</option>'; const done = () => { model.disabled = false; row.classList.remove('loading'); }; if (!id) { model.innerHTML = '<option value="">No provider available</option>'; done(); return; } if (id === '__smart__') { const active = state.routingProfiles.find(p => p.active); model.innerHTML = `${active ? '<option value="smart">smart · active default</option>' : ''}${state.routingProfiles.map(p => `<option value="smart/${esc(p.slug)}">smart/${esc(p.slug)} · ${esc(p.name)}</option>`).join('')}`; done(); return; } try { const list = await api(`/api/providers/${id}/models?capability=chat${refresh ? '&refresh=1' : ''}`), names = [...new Set(list.map(x => x.id || x.name).filter(Boolean))].sort(); model.innerHTML = names.map(name => `<option value="${esc(name)}">${esc(name)}</option>`).join('') || '<option value="">No chat models returned</option>'; } catch (e) { model.innerHTML = '<option value="">Could not load models</option>'; toast(e.message, 'error'); } finally { done(); } }
function updateChatRoute() { const smart = $('#play-provider').value === '__smart__', p = state.providers.find(x => x.id === $('#play-provider').value), model = $('#play-model').value, reasoning = !smart && p?.type === 'openai' && isReasoningModel(model); $('#chat-route').textContent = smart ? (model || 'NO SMART PROFILE') : p && model ? `${p.slug}/${model}` : 'NO ROUTE SELECTED'; $('#play-temperature').disabled = reasoning; $('#play-temperature').title = reasoning ? 'This model controls its own sampling temperature.' : ''; }
function welcomeHTML() { return '<div class="chat-welcome"><div class="welcome-glyph">N</div><h2>Start a conversation</h2><p>Select a provider and model, then send a message. Requests appear automatically in Traces.</p></div>'; }
function clearChat() { if (state.abort) state.abort.abort(); state.conversation = []; $('#chat-messages').innerHTML = welcomeHTML(); $('#chat-metrics').textContent = 'READY'; saveConversation(); }
function saveConversation() { store.set('nexa.playground', { conversation: state.conversation, system: $('#play-system').value }); }
function updateJsonHint() { $('#json-hint').classList.toggle('hidden', !$('#play-json').checked); }
function savePlaygroundPrefs() { store.set('nexa.playground.prefs', { stream: $('#play-stream').checked, json: $('#play-json').checked }); }
function restorePlaygroundPrefs() { const prefs = store.get('nexa.playground.prefs', {}); if (typeof prefs.stream === 'boolean') $('#play-stream').checked = prefs.stream; if (typeof prefs.json === 'boolean') $('#play-json').checked = prefs.json; updateJsonHint(); }
// OpenAI and Groq reject json_object unless a message mentions JSON, so the playground adds the
// instruction only when the conversation does not already say it.
function withJsonInstruction(messages) {
  if (messages.some(m => /json/i.test(typeof m.content === 'string' ? m.content : JSON.stringify(m.content)))) return messages;
  const note = 'Respond with a single valid JSON object.';
  if (messages[0]?.role === 'system') return [{ ...messages[0], content: `${messages[0].content}\n\n${note}` }, ...messages.slice(1)];
  return [{ role: 'system', content: note }, ...messages];
}
function jsonReplyHTML(content) {
  try { const parsed = JSON.parse(content.trim()); return `${renderMarkdown('```json\n' + JSON.stringify(parsed, null, 2) + '\n```')}<span class="json-note ok">✓ VALID JSON</span>`; }
  catch { return `${renderMarkdown(content)}<span class="json-note bad">✕ NOT VALID JSON</span>`; }
}
function restoreConversation() { const saved = store.get('nexa.playground', null); if (!saved) return; state.conversation = Array.isArray(saved.conversation) ? saved.conversation : []; if (typeof saved.system === 'string') $('#play-system').value = saved.system; renderConversation(); }
function renderConversation() { $('#chat-messages').innerHTML = state.conversation.length ? '' : welcomeHTML(); state.conversation.forEach(m => addMessage(m.role, m.content)); }
function addMessage(role, content, loading = false) { $('.chat-welcome')?.remove(); const item = document.createElement('div'); item.className = `chat-message ${role}`; item.innerHTML = `<span class="role">${role === 'user' ? 'YOU' : 'NEXA'}</span><div class="bubble markdown-body ${loading ? 'typing' : ''}">${role === 'assistant' && !loading ? renderMarkdown(content) : esc(content)}</div>`; $('#chat-messages').append(item); $('#chat-messages').scrollTop = $('#chat-messages').scrollHeight; return item; }
function setMessageMeta(item, parts) { let meta = $('.message-meta', item); if (!meta) { meta = document.createElement('div'); meta.className = 'message-meta'; item.append(meta); } meta.innerHTML = parts.filter(Boolean).map(part => `<span>${esc(part)}</span>`).join(''); }
function setSending(sending) { const button = $('#chat-send'); button.classList.toggle('stop', sending); $('span', button).textContent = sending ? 'Stop' : 'Send'; $('.button-icon use', button).setAttribute('href', sending ? '#icon-stop' : '#icon-arrow'); }
function routeLabel(headers, fallback) {
  const routed = headers.get('X-Nexa-Routed-Model'); if (!routed) return fallback;
  const lane = headers.get('X-Nexa-Routing-Lane'), confidence = Number(headers.get('X-Nexa-Routing-Confidence') || 0);
  return `${fallback} → ${routed} · ${laneName(lane)} · ${Math.round(confidence * 100)}%`;
}
async function sendChat(e) {
  e.preventDefault();
  if (state.abort) { state.abort.abort(); return; }
  const input = $('#chat-input'), text = input.value.trim(), smart = $('#play-provider').value === '__smart__', p = state.providers.find(x => x.id === $('#play-provider').value), model = $('#play-model').value;
  if (!text || (!smart && !p) || !model) { toast('Choose a provider and model first', 'error'); return; }
  if ($('#play-compare').checked) { await sendComparison(text, smart, p, model); return; }
  input.value = ''; state.conversation.push({ role: 'user', content: text }); addMessage('user', text); saveConversation();
  const pending = addMessage('assistant', '', true), bubble = pending.querySelector('.bubble'), started = performance.now(), stream = $('#play-stream').checked;
  const messages = []; const system = $('#play-system').value.trim(); if (system) messages.push({ role: 'system', content: system }); messages.push(...state.conversation);
  const request = { model: smart ? model : `${p.slug}/${model}`, messages, max_completion_tokens: Number($('#play-max-tokens').value), stream };
  if (stream) request.stream_options = { include_usage: true };
  if (!$('#play-temperature').disabled) request.temperature = Number($('#play-temperature').value);
  if (Number($('#play-top-p').value) !== 1) request.top_p = Number($('#play-top-p').value);
  const stops = $('#play-stop').value.split(',').map(s => s.trim()).filter(Boolean); if (stops.length) request.stop = stops;
  const jsonMode = $('#play-json').checked;
  if (jsonMode) { request.response_format = { type: 'json_object' }; request.messages = withJsonInstruction(messages); }
  state.abort = new AbortController(); setSending(true); $('#chat-metrics').textContent = 'REQUEST IN FLIGHT…';
  let content = '', usage = null, ttft = 0, traceID = '', stopped = false, frame = 0;
  const paint = () => { frame = 0; bubble.innerHTML = renderMarkdown(content || '…'); $('#chat-messages').scrollTop = $('#chat-messages').scrollHeight; };
  try {
    const response = await fetch('/api/playground/chat', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request), signal: state.abort.signal });
    traceID = response.headers.get('X-Nexa-Trace-Id') || '';
    $('#chat-route').textContent = routeLabel(response.headers, request.model);
    if (!response.ok) { const data = await response.json().catch(() => ({})); throw new Error(data?.error?.message || `Request failed (${response.status})`); }
    bubble.classList.remove('typing');
    if (stream) {
      const reader = response.body.getReader(), decoder = new TextDecoder(); let buffer = '';
      for (;;) {
        const { value, done } = await reader.read(); if (done) break;
        buffer += decoder.decode(value, { stream: true }); const lines = buffer.split('\n'); buffer = lines.pop();
        for (const line of lines) {
          if (!line.startsWith('data:')) continue; const data = line.slice(5).trim(); if (data === '[DONE]') continue;
          const chunk = parseJSON(data); if (!chunk) continue;
          if (chunk.error) throw new Error(chunk.error.message || 'Stream error');
          const delta = chunk.choices?.[0]?.delta || {};
          if (delta.content) { if (!ttft) ttft = performance.now() - started; content += delta.content; if (!frame) frame = requestAnimationFrame(paint); }
          (delta.tool_calls || []).forEach(tc => { content += tc.function?.name ? `\n\n**Tool call:** \`${tc.function.name}\` ` : ''; content += tc.function?.arguments || ''; });
          if (chunk.usage) usage = chunk.usage;
        }
      }
    } else {
      const out = await response.json(); const raw = out?.choices?.[0]?.message; usage = out?.usage;
      content = typeof raw?.content === 'string' ? raw.content : raw?.tool_calls ? `**Tool calls**\n\`\`\`json\n${JSON.stringify(raw.tool_calls, null, 2)}\n\`\`\`` : JSON.stringify(out, null, 2);
    }
  } catch (err) {
    if (err.name === 'AbortError') stopped = true;
    else { if (frame) cancelAnimationFrame(frame); bubble.classList.remove('typing'); bubble.textContent = `Request failed: ${err.message}`; setMessageMeta(pending, [durationLabel(performance.now() - started), 'Failed']); $('#chat-metrics').innerHTML = `ERROR${traceID ? ` · <button class="metric-link" data-trace-id="${esc(traceID)}">Open trace</button>` : ''}`; state.abort = null; setSending(false); loadStats(); return; }
  }
  if (frame) cancelAnimationFrame(frame);
  bubble.classList.remove('typing'); bubble.innerHTML = jsonMode && content && !stopped ? jsonReplyHTML(content) : renderMarkdown(content + (stopped ? '\n\n*(stopped)*' : ''));
  if (content) { state.conversation.push({ role: 'assistant', content }); saveConversation(); }
  const parts = [`${Math.round(performance.now() - started)} MS`]; if (ttft) parts.push(`FIRST TOKEN ${Math.round(ttft)} MS`); if (usage) parts.push(`${fmtNum(usage.total_tokens)} TOKENS`); if (stopped) parts.push('STOPPED');
  setMessageMeta(pending, [durationLabel(performance.now() - started), ttft ? `${durationLabel(ttft)} to first token` : '', usage ? `${fmtNum(usage.total_tokens)} tokens` : '', stopped ? 'Stopped' : '']);
  $('#chat-metrics').innerHTML = `${esc(parts.join(' · '))}${traceID ? ` · <button class="metric-link" data-trace-id="${esc(traceID)}">Open trace</button>` : ''}`;
  state.abort = null; setSending(false); $('#chat-messages').scrollTop = $('#chat-messages').scrollHeight; loadStats();
  if (traceID) api(`/api/traces/${traceID}`).then(trace => setMessageMeta(pending, [durationLabel(trace.latency_ms), trace.ttft_ms ? `${durationLabel(trace.ttft_ms)} to first token` : '', `${fmtNum(trace.total_tokens)} tokens`, fmtCost(trace.cost_usd)])).catch(() => {});
}
async function sendComparison(text, smart, provider, model) { const secondSmart = $('#compare-provider').value === '__smart__', secondProvider = state.providers.find(x => x.id === $('#compare-provider').value), secondModel = $('#compare-model').value; if ((!secondSmart && !secondProvider) || !secondModel) { toast('Choose the second provider and model', 'error'); return; } const input = $('#chat-input'); input.value = ''; state.conversation.push({ role: 'user', content: text }); addMessage('user', text); saveConversation(); const messages = [], system = $('#play-system').value.trim(); if (system) messages.push({ role: 'system', content: system }); messages.push(...state.conversation); const routes = [smart ? model : `${provider.slug}/${model}`, secondSmart ? secondModel : `${secondProvider.slug}/${secondModel}`], shell = addComparisonMessage(routes); state.abort = new AbortController(); setSending(true); $('#chat-metrics').textContent = '2 REQUESTS IN FLIGHT…'; const call = async route => { const started = performance.now(), request = { model: route, messages, max_completion_tokens: Number($('#play-max-tokens').value), stream: false }, routeModel = route.split('/').at(-1); if (!String(route).startsWith('smart') && !isReasoningModel(routeModel)) request.temperature = Number($('#play-temperature').value); if (Number($('#play-top-p').value) !== 1) request.top_p = Number($('#play-top-p').value); const stops = $('#play-stop').value.split(',').map(v => v.trim()).filter(Boolean); if (stops.length) request.stop = stops; if ($('#play-json').checked) { request.response_format = { type: 'json_object' }; request.messages = withJsonInstruction(messages); } const response = await fetch('/api/playground/chat', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request), signal: state.abort.signal }), traceID = response.headers.get('X-Nexa-Trace-Id') || ''; if (!response.ok) { const data = await response.json().catch(() => ({})); throw new Error(data?.error?.message || `Request failed (${response.status})`); } const out = await response.json(), raw = out?.choices?.[0]?.message, content = typeof raw?.content === 'string' ? raw.content : JSON.stringify(raw?.tool_calls || out, null, 2); return { content, usage: out?.usage, traceID, elapsed: performance.now() - started, route: routeLabel(response.headers, route) }; }; const results = await Promise.allSettled(routes.map(call)); results.forEach((result, index) => renderComparisonResult(shell, index, result)); const first = results.find(result => result.status === 'fulfilled'); if (first) { state.conversation.push({ role: 'assistant', content: first.value.content }); saveConversation(); } $('#chat-metrics').textContent = `${results.filter(v => v.status === 'fulfilled').length} / 2 COMPLETE`; state.abort = null; setSending(false); loadStats(); }
function addComparisonMessage(routes) { $('.chat-welcome')?.remove(); const item = document.createElement('div'); item.className = 'comparison-message'; item.innerHTML = `<span class="role">COMPARE</span><div class="comparison-grid">${routes.map((route, index) => `<article data-comparison="${index}"><header><span>${esc(route)}</span></header><div class="bubble markdown-body typing"></div><footer>Request in flight…</footer></article>`).join('')}</div>`; $('#chat-messages').append(item); $('#chat-messages').scrollTop = $('#chat-messages').scrollHeight; return item; }
function renderComparisonResult(shell, index, result) { const card = $(`[data-comparison="${index}"]`, shell), bubble = $('.bubble', card), footer = $('footer', card); bubble.classList.remove('typing'); if (result.status === 'rejected') { bubble.textContent = `Request failed: ${result.reason.message}`; footer.textContent = 'FAILED'; return; } const value = result.value; $('header span', card).textContent = value.route; bubble.innerHTML = $('#play-json').checked ? jsonReplyHTML(value.content) : renderMarkdown(value.content); footer.textContent = `${durationLabel(value.elapsed)} · ${fmtNum(value.usage?.total_tokens)} tokens`; if (value.traceID) api(`/api/traces/${value.traceID}`).then(trace => { footer.innerHTML = `${durationLabel(trace.latency_ms)} · ${fmtNum(trace.total_tokens)} tokens · ${fmtCost(trace.cost_usd)} · <button class="metric-link" data-trace-id="${esc(value.traceID)}">Open trace</button>`; }).catch(() => {}); }
function isReasoningModel(model) { const id = model.toLowerCase(); return /^gpt-5/.test(id) || /^o[134]/.test(id); }

async function loadUsers() {
  try {
    state.users = await api('/api/users'); renderUsers(); renderSDK();
  } catch (e) { toast(e.message, 'error'); }
}
function renderUsers() { const query = $('#user-search').value.trim().toLowerCase(), users = state.users.filter(u => !query || u.username.toLowerCase().includes(query) || u.role.toLowerCase().includes(query)); $('#user-search').closest('label').classList.toggle('hidden', state.users.length <= 5 && !query); $('#users-list').innerHTML = users.length ? users.map(u => { const self = u.id === state.user.id; return `<div class="user-row"><span class="user-avatar">${esc(initials(u.username))}</span><div><h3>${esc(u.username)}${self ? ' <small>(you)</small>' : ''}</h3><p>CREATED ${esc(fmtDate(u.created_at).toUpperCase())}</p></div><span class="role-tag">${esc(u.role.toUpperCase())}</span><div class="user-actions">${self ? '' : `<button data-user-role="${esc(u.id)}" data-admin>ROLE</button><button data-user-password="${esc(u.id)}" data-admin>RESET PW</button><button class="delete-user" data-delete-user="${esc(u.id)}" data-admin title="Delete user" aria-label="Delete user">${uiIcon('trash')}</button>`}</div></div>`; }).join('') : `<div class="empty-row">${query ? 'No users match this search.' : 'No local users yet. Master access remains active.'}</div>`; }
async function saveUser(e) { e.preventDefault(); try { await api('/api/users', { method: 'POST', body: JSON.stringify({ Username: $('#new-username').value, Password: $('#new-password').value, Role: $('#new-role').value }) }); $('#user-dialog').close(); $('#user-form').reset(); toast('User created'); loadUsers(); } catch (err) { setError('#user-error', err.message); } }
async function deleteUser(id) { if (!await confirmAction('Delete this user?', 'All of their active sessions end immediately.', 'Delete')) return; try { await api(`/api/users/${id}`, { method: 'DELETE' }); toast('User removed'); loadUsers(); } catch (e) { toast(e.message, 'error'); } }
async function changeRole(id) {
  const u = state.users.find(x => x.id === id); if (!u) return;
  const values = await ask({ kicker: 'ACCESS / ROLE', title: `Role for ${u.username}`, fields: [{ name: 'role', label: 'ROLE', value: u.role, options: [['member', 'Member — view & playground'], ['admin', 'Administrator — full control']] }], confirm: 'Save role' });
  if (!values) return; try { await api(`/api/users/${id}`, { method: 'PUT', body: JSON.stringify({ role: values.role }) }); toast('Role updated'); loadUsers(); } catch (e) { toast(e.message, 'error'); }
}
async function resetPassword(id) {
  const u = state.users.find(x => x.id === id); if (!u) return;
  const values = await ask({ kicker: 'ACCESS / PASSWORD', title: `Reset password for ${u.username}`, message: 'Their existing sessions will be signed out.', fields: [{ name: 'password', label: 'NEW PASSWORD', type: 'password', minlength: 8, placeholder: 'At least 8 characters' }], confirm: 'Reset password' });
  if (!values) return; try { await api(`/api/users/${id}`, { method: 'PUT', body: JSON.stringify({ password: values.password }) }); toast('Password reset'); } catch (e) { toast(e.message, 'error'); }
}
async function changePassword() {
  const values = await ask({ kicker: 'YOUR ACCOUNT', title: 'Change my password', message: 'Your other sessions will be signed out.', fields: [{ name: 'current_password', label: 'CURRENT PASSWORD', type: 'password' }, { name: 'new_password', label: 'NEW PASSWORD', type: 'password', minlength: 8, placeholder: 'At least 8 characters' }], confirm: 'Change password' });
  if (!values) return; try { await api('/api/auth/password', { method: 'POST', body: JSON.stringify(values) }); toast('Password changed'); } catch (e) { toast(e.message, 'error'); }
}
async function loadApiKeys(render = true) {
  try {
    state.apiKeys = await api('/api/api-keys'); if (render) renderApiKeys(); renderOverviewState();
  } catch (e) { toast(e.message, 'error'); }
}
function renderApiKeys() { const query = $('#key-search').value.trim().toLowerCase(), keys = state.apiKeys.filter(k => !query || k.name.toLowerCase().includes(query) || k.prefix.toLowerCase().includes(query)); $('#key-search').closest('label').classList.toggle('hidden', state.apiKeys.length <= 5 && !query); $('#api-key-table').innerHTML = keys.length ? keys.map(k => `<tr><td><b>${esc(k.name)}</b></td><td><span class="model-name">${esc(k.prefix)}</span></td><td>${esc(fmtDate(k.created_at))}</td><td>${k.last_used_at ? esc(fmtDate(k.last_used_at)) : 'never'}</td><td><button class="detail-button" data-revoke-key="${esc(k.id)}" data-admin title="Revoke key" aria-label="Revoke key">${uiIcon('trash')}</button></td></tr>`).join('') : `<tr><td colspan="5" class="empty-row">${query ? 'No API keys match this search.' : 'No API keys yet. Create one per application instead of sharing the master key.'}</td></tr>`; }
async function createApiKey(e) {
  e.preventDefault();
  try { const out = await api('/api/api-keys', { method: 'POST', body: JSON.stringify({ name: $('#api-key-name').value }) }); $('#api-key-form').reset(); showSecret('New API key', 'Copy it now — Nexa stores only a hash. Use it as the api_key / Bearer token in your SDK. Revoke it here at any time.', out.key); loadApiKeys(); }
  catch (err) { toast(err.message, 'error'); }
}
async function revokeApiKey(id) { if (!await confirmAction('Revoke this API key?', 'Applications using it are rejected immediately.', 'Revoke')) return; try { await api(`/api/api-keys/${id}`, { method: 'DELETE' }); toast('API key revoked'); loadApiKeys(); } catch (e) { toast(e.message, 'error'); } }
function showSecret(title, note, value) { $('#key-dialog-title').textContent = title; $('#key-dialog-note').textContent = note; $('#new-master-key').textContent = value; $('#key-dialog').showModal(); }
async function rotateKey() {
  if (!await confirmAction('Rotate the master key now?', 'The previous key immediately stops authenticating API calls and other master sessions are signed out.', 'Rotate')) return;
  try { const out = await api('/api/master-key/rotate', { method: 'POST', body: '{}' }); showSecret('New master key', 'This value will not be shown again. The previous master key no longer works.', out.master_key); } catch (e) { toast(e.message, 'error'); }
}
function snippetFor(lang, model) {
  const origin = location.origin;
  const snippets = {
    python: `from openai import OpenAI\n\nclient = OpenAI(\n    api_key="YOUR_NEXA_API_KEY",\n    base_url="${origin}/v1"\n)\n\nresponse = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "Hello, Nexa!"}]\n)`,
    javascript: `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  apiKey: "YOUR_NEXA_API_KEY",\n  baseURL: "${origin}/v1"\n});\n\nconst response = await client.chat.completions.create({\n  model: "${model}",\n  messages: [{ role: "user", content: "Hello, Nexa!" }]\n});`,
    curl: `curl ${origin}/v1/chat/completions \\\n  -H "Authorization: Bearer YOUR_NEXA_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","messages":[{"role":"user","content":"Hello, Nexa!"}]}'`
  };
  return snippets[lang];
}
function renderSDK() { const p = state.providers.find(x => x.enabled); $('#sdk-code').textContent = snippetFor(state.code, p ? `${p.slug}/YOUR_MODEL` : 'provider-slug/YOUR_MODEL'); }
async function copyText(value) { if (!value) return; try { await navigator.clipboard.writeText(value); toast('Copied to clipboard'); } catch { toast('Could not access clipboard', 'error'); } }

boot();
