'use strict';

const state = {
  key: '',
  overview: null,
  accounts: [],
  models: [],
  config: null,
  logs: [],
  importStatus: null,
};

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

function esc(v) {
  return String(v == null ? '' : v)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function toast(msg, kind) {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast ' + (kind || '');
  el.classList.remove('hidden');
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.add('hidden'), 4200);
}

async function api(path, opts) {
  opts = opts || {};
  const headers = Object.assign({ 'content-type': 'application/json' }, opts.headers || {});
  if (state.key) headers['authorization'] = 'Bearer ' + state.key;
  const res = await fetch(path, Object.assign({ credentials: 'same-origin' }, opts, { headers }));
  if (res.status === 401) {
    showLogin();
    throw new Error('未登录');
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = { raw: text }; }
  if (!res.ok) {
    const msg = (data && data.error && (data.error.message || data.error)) || ('HTTP ' + res.status);
    throw new Error(typeof msg === 'string' ? msg : JSON.stringify(msg));
  }
  return data;
}

/* ------------------------------------------------------------------ login */

function showLogin() {
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  $('#login-key').focus();
}

function showApp() {
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
}

$('#login-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const key = $('#login-key').value.trim();
  $('#login-err').textContent = '';
  try {
    const res = await fetch('/panel/api/login', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ key }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error((data.error && data.error.message) || '登录失败');
    state.key = key;
    localStorage.setItem('capy2api_key', key);
    showApp();
    await refreshAll();
  } catch (e) {
    $('#login-err').textContent = e.message;
  }
});

$('#logout').addEventListener('click', async () => {
  state.key = '';
  localStorage.removeItem('capy2api_key');
  await fetch('/panel/api/logout', { method: 'POST', credentials: 'same-origin' }).catch(() => {});
  showLogin();
});

/* ------------------------------------------------------------------- tabs */

$$('#tabs button').forEach((btn) => {
  btn.addEventListener('click', () => {
    $$('#tabs button').forEach((b) => b.classList.toggle('active', b === btn));
    $$('.tab').forEach((t) => t.classList.add('hidden'));
    $('#tab-' + btn.dataset.tab).classList.remove('hidden');
    if (btn.dataset.tab === 'logs') loadLogs();
    if (btn.dataset.tab === 'models') { loadModels(); loadStats().catch((e) => toast(e.message, 'bad')); }
    if (btn.dataset.tab === 'keys') loadKeys().catch((e) => toast(e.message, 'bad'));
  });
});

/* --------------------------------------------------------------- overview */

function setText(sel, v) { const el = $(sel); if (el) el.textContent = v; }

async function loadOverview() {
  const data = await api('/panel/api/overview');
  state.overview = data;
  const o = data;
  const cards = [
    ['账号', o.accounts_total, o.accounts_ready + ' 可用'],
    ['执行模式', o.mode || 'thread', o.mode === 'thread' ? (o.template_jam || '自动选线程') : (o.project_id || '自动项目')],
    ['模型', o.models_total, 'ads 免费模型'],
    ['请求', o.requests.total, '成功 ' + o.requests.ok + ' / 失败 ' + o.requests.failed],
    ['平均耗时', (o.requests.avg_ms / 1000).toFixed(1) + 's', '端到端'],
    ['缓存命中', ((o.tokens.cache_hit_ratio || 0) * 100).toFixed(0) + '%', fmtTok(o.tokens.cache_read) + ' / ' + fmtTok(o.tokens.prompt)],
    ['运行时长', fmtDur(o.uptime_sec), o.version],
  ];
  $('#stat-cards').innerHTML = cards.map(([k, v, sub]) => `
    <div class="stat"><div class="k">${esc(k)}</div><div class="v">${esc(v)}<small>${esc(sub)}</small></div></div>
  `).join('');

  $('#connect-info').innerHTML = [
    ['Base URL', o.connect.openai_base],
    ['Anthropic Base', o.connect.anthropic_base],
    ['API Key', o.connect.api_key_hint],
    ['模型示例', o.connect.model_sample || '—'],
    ['模型列表', 'GET /v1/models'],
  ].map(([k, v]) => `<div class="k">${esc(k)}</div><div class="v mono">${esc(v)}</div>`).join('');

  const imp = o.capy_app || {};
  $('#import-info').innerHTML = [
    ['本机会话', imp.available ? '<span class="pill ok">可导入</span>' : '<span class="pill bad">未找到</span>'],
    ['应用进程', imp.running ? '<span class="pill ok">运行中</span>' : '<span class="pill warn">未运行</span>'],
    ['数据目录', imp.path || '—'],
  ].map(([k, v]) => `<div class="k">${esc(k)}</div><div class="v">${v}</div>`).join('');

  renderRecent(o.recent || []);
  loadContext().catch(() => {});
  loadConvInfo().catch(() => {});
}

async function loadConvInfo() {
  const d = await api('/panel/api/conversations');
  const items = d.conversations || [];
  const el = $('#conv-info');
  el.innerHTML = [
    ['自动清理', d.enabled ? '开启' : '关闭'],
    ['TTL', (d.ttl_sec || 0) + ' 秒'],
    ['当前会话线程', items.length + ' 条'],
  ].map(([k, v]) => `<div class="k">${esc(k)}</div><div class="v">${esc(String(v))}</div>`).join('');
}

$('#btn-cleanup-convs').addEventListener('click', async () => {
  try {
    const r = await api('/panel/api/conversations/cleanup', { method: 'POST', body: '{}' });
    toast('已清理 ' + (r.deleted || 0) + ' 条线程', 'ok');
    await loadConvInfo();
  } catch (e) {
    toast(e.message, 'bad');
  }
});

async function loadContext() {
  const d = await api('/panel/api/context');
  const el = $('#ctx-info');
  const last = d.last_turn || {};
  const ct = last.context_tokens || {};
  const daemon = d.daemon || {};
  el.innerHTML = [
    ['策略', esc(d.policy || '—')],
    ['设备', daemon.online ? '<span class="pill ok">在线</span>' : '<span class="pill bad">离线</span>'],
    ['system', (ct.system || 0) + ' tok'],
    ['tools', (ct.tools || 0) + ' tok'],
    ['mcp', (ct.mcp || 0) + ' tok'],
    ['skills / rules', (ct.skills || 0) + ' / ' + (ct.rules || 0) + ' tok'],
    ['本轮工具', esc((last.tools_used || []).join(', ') || '—')],
    ['记忆拦截', esc(((daemon.memory_attempts || []).join(', ')) || '无')],
  ].map(([k, v]) => `<div class="k">${esc(k)}</div><div class="v">${v}</div>`).join('');
}

function renderRecent(rows) {
  const t = $('#recent-table');
  if (!rows.length) { t.innerHTML = '<tr><td class="muted">还没有请求</td></tr>'; return; }
  t.innerHTML = `
    <thead><tr><th>时间</th><th>模型</th><th>状态</th><th>耗时</th><th>tokens</th><th>线程</th></tr></thead>
    <tbody>${rows.slice(0, 12).map((r) => `
      <tr>
        <td class="mono">${esc(shortTime(r.time))}</td>
        <td class="mono">${esc(r.model)}</td>
        <td>${statusPill(r.status, r.error)}</td>
        <td class="mono">${(r.duration_ms / 1000).toFixed(2)}s</td>
        <td class="mono">${r.prompt_tokens}/${r.output_tokens}<span class="muted"> ·cache ${r.cache_tokens || 0}</span></td>
        <td class="mono">${esc(r.jam_id || '—')}</td>
      </tr>`).join('')}</tbody>`;
}

function statusPill(status, err) {
  if (status === 'ok') return '<span class="pill ok">ok</span>';
  const title = err ? ' title="' + esc(err) + '"' : '';
  return '<span class="pill bad"' + title + '>失败</span>';
}

function shortTime(t) {
  if (!t) return '—';
  return String(t).replace('T', ' ').replace(/\..*/, '');
}

function fmtDur(sec) {
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h';
  return Math.floor(sec / 86400) + 'd';
}

/* --------------------------------------------------------------- accounts */

async function loadAccounts() {
  const data = await api('/panel/api/accounts');
  state.accounts = data.accounts || [];
  const t = $('#accounts-table');
  if (!state.accounts.length) {
    t.innerHTML = '<tr><td class="muted">还没有账号。点上面的「从 Capy.app 一键导入」，或在下方手动添加。</td></tr>';
    return;
  }
  t.innerHTML = `
    <thead><tr><th>名称</th><th>组织 / 设备</th><th>状态</th><th>最近使用</th><th>操作</th></tr></thead>
    <tbody>${state.accounts.map((a) => `
      <tr>
        <td>
          <div>${esc(a.name)}</div>
          <div class="muted mono">${esc(a.email || a.user_id || a.id)}</div>
        </td>
        <td class="mono">
          <div>${esc(a.org_id || '—')}</div>
          <div class="muted">${esc(a.device_id || '未绑定设备')}</div>
        </td>
        <td>${a.enabled ? accountPill(a.state, a.last_error) : '<span class="pill warn">已禁用</span>'}</td>
        <td class="mono muted">${esc(shortTime(a.last_used_at))}</td>
        <td>
          <button class="small" data-act="probe" data-id="${esc(a.id)}">校验</button>
          <button class="small" data-act="toggle" data-id="${esc(a.id)}" data-enabled="${a.enabled}">${a.enabled ? '禁用' : '启用'}</button>
          <button class="small danger" data-act="del" data-id="${esc(a.id)}">删除</button>
        </td>
      </tr>`).join('')}</tbody>`;

  $$('#accounts-table button').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const id = btn.dataset.id;
      try {
        if (btn.dataset.act === 'probe') {
          btn.disabled = true;
          const r = await api('/panel/api/accounts/' + id + '/probe', { method: 'POST', body: '{}' });
          toast('校验通过：' + (r.models || []).length + ' 个可用模型', 'ok');
          await Promise.all([loadAccounts(), loadOverview()]);
        } else if (btn.dataset.act === 'toggle') {
          await api('/panel/api/accounts/' + id, {
            method: 'PATCH',
            body: JSON.stringify({ enabled: btn.dataset.enabled !== 'true' }),
          });
          await loadAccounts();
        } else if (btn.dataset.act === 'del') {
          if (!confirm('删除这个账号？')) return;
          await api('/panel/api/accounts/' + id, { method: 'DELETE' });
          await Promise.all([loadAccounts(), loadOverview()]);
        }
      } catch (e) {
        toast(e.message, 'bad');
      } finally {
        btn.disabled = false;
      }
    });
  });
}

function accountPill(stateName, err) {
  if (stateName === 'ok') return '<span class="pill ok">正常</span>';
  if (stateName === 'cooldown') {
    return '<span class="pill warn" title="' + esc(err || '') + '">冷却中</span>';
  }
  if (stateName === 'invalid') {
    return '<span class="pill bad" title="' + esc(err || '') + '">失效</span>';
  }
  return '<span class="pill">未校验</span>';
}

let loginPoll = null;

async function pollBrowserLogin() {
  try {
    const s = await api('/panel/api/accounts/browser-login/status');
    const hint = $('#browser-login-hint');
    if (s.status === 'waiting') {
      hint.textContent = '等待你在浏览器里完成登录…（最多 5 分钟）';
      loginPoll = setTimeout(pollBrowserLogin, 2000);
      return;
    }
    if (s.status === 'ok') {
      hint.textContent = '登录成功：' + (s.name || '账号已导入');
      toast('登录成功：' + (s.name || ''), 'ok');
      clearTimeout(loginPoll);
      await Promise.all([loadAccounts(), loadOverview(), loadModels()]);
      return;
    }
    if (s.status === 'error') {
      hint.textContent = s.message || '登录失败';
      toast(s.message || '登录失败', 'bad');
      clearTimeout(loginPoll);
      return;
    }
    hint.textContent = '点「浏览器登录」会打开一个独立 Chrome 窗口，你在里面正常登录 Capy（Google/邮箱都行），登录完成后自动导入，不用装 App。';
  } catch (e) {
    toast(e.message, 'bad');
  }
}

$('#btn-browser-login').addEventListener('click', async () => {
  const btn = $('#btn-browser-login');
  btn.disabled = true;
  try {
    const r = await api('/panel/api/accounts/browser-login', { method: 'POST', body: '{}' });
    toast(r.status === 'waiting' ? '已打开浏览器，请完成登录' : '已启动', 'ok');
    clearTimeout(loginPoll);
    pollBrowserLogin();
  } catch (e) {
    toast(e.message, 'bad');
  } finally {
    btn.disabled = false;
  }
});

$('#btn-import').addEventListener('click', async () => {
  const btn = $('#btn-import');
  btn.disabled = true;
  btn.textContent = '正在读取…';
  try {
    const r = await api('/panel/api/accounts/import', { method: 'POST', body: '{}' });
    toast('已导入：' + (r.name || '账号') + '（' + (r.models || []).length + ' 个模型）', 'ok');
    await Promise.all([loadAccounts(), loadOverview(), loadModels()]);
  } catch (e) {
    toast(e.message, 'bad');
  } finally {
    btn.disabled = false;
    btn.textContent = '从 Capy.app 一键导入';
  }
});

$('#account-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = new FormData(ev.target);
  const body = {};
  for (const [k, v] of f.entries()) body[k] = String(v).trim();
  try {
    const r = await api('/panel/api/accounts', { method: 'POST', body: JSON.stringify(body) });
    toast('已添加：' + r.name, 'ok');
    ev.target.reset();
    await Promise.all([loadAccounts(), loadOverview()]);
  } catch (e) {
    toast(e.message, 'bad');
  }
});

/* ----------------------------------------------------------------- models */

async function loadModels() {
  const data = await api('/panel/api/models');
  state.models = data.models || [];
  const t = $('#models-table');
  if (!state.models.length) {
    t.innerHTML = '<tr><td class="muted">暂无可用模型（先导入账号并保证 Capy 桌面端在线）。</td></tr>';
    return;
  }
  t.innerHTML = `
    <thead><tr><th>模型 ID</th><th>名称</th><th>上下文</th><th>输出</th><th>推理档位</th><th>默认</th></tr></thead>
    <tbody>${state.models.map((m) => `
      <tr>
        <td class="mono">${esc(m.id)}</td>
        <td>${esc(m.name)}</td>
        <td class="mono">${m.limits ? fmtTok(m.limits.context) : '—'}</td>
        <td class="mono">${m.limits ? fmtTok(m.limits.output) : '—'}</td>
        <td class="mono">${esc((m.efforts || []).join(' / ') || '—')}</td>
        <td class="mono">${esc(m.default_effort || '—')}</td>
      </tr>`).join('')}</tbody>`;
}

async function loadStats() {
  const data = await api('/panel/api/stats');
  const rows = (data.models || []).slice().sort((a, b) => b.requests - a.requests);
  const t = $('#stats-table');
  if (!rows.length) { t.innerHTML = '<tr><td class="muted">还没有请求</td></tr>'; }
  else {
    t.innerHTML = `
      <thead><tr><th>模型</th><th>请求</th><th>失败</th><th>平均耗时</th><th>首字</th><th>速度</th><th>输入/输出</th><th>缓存命中</th></tr></thead>
      <tbody>${rows.map((r) => `
        <tr>
          <td class="mono">${esc(r.model)}</td>
          <td class="mono">${r.requests}${r.streaming ? ' <span class="pill">' + r.streaming + ' stream</span>' : ''}</td>
          <td class="mono">${r.failed ? '<span class="pill bad">' + r.failed + '</span>' : '0'}</td>
          <td class="mono">${(r.avg_latency_ms / 1000).toFixed(1)}s</td>
          <td class="mono">${r.avg_ttft_ms ? (r.avg_ttft_ms / 1000).toFixed(2) + 's' : '—'}</td>
          <td class="mono">${r.tokens_per_second ? r.tokens_per_second.toFixed(1) + ' t/s' : '—'}</td>
          <td class="mono">${fmtTok(r.prompt_tokens)} / ${fmtTok(r.completion_tokens)}</td>
          <td class="mono">${(r.cache_hit_ratio * 100).toFixed(1)}%</td>
        </tr>`).join('')}</tbody>`;
  }
  setText('#stats-since', '统计起点：' + (data.since || '—'));
}

function fmtTok(n) {
  if (!n) return '—';
  if (n >= 1000) return Math.round(n / 1000) + 'k';
  return String(n);
}

$('#btn-refresh-stats').addEventListener('click', () => loadStats().catch((e) => toast(e.message, 'bad')));

$('#btn-refresh-models').addEventListener('click', async () => {
  try {
    await api('/panel/api/models?refresh=1');
    await loadModels();
    toast('模型已刷新', 'ok');
  } catch (e) { toast(e.message, 'bad'); }
});

/* ------------------------------------------------------------------- keys */

async function loadKeys() {
  const data = await api('/panel/api/keys');
  const rows = data.keys || [];
  const t = $('#keys-table');
  // 账号下拉
  const accSel = $('#key-account');
  if (accSel && state.accounts.length) {
    accSel.innerHTML = '<option value="">共享池</option>' + state.accounts.map((a) =>
      `<option value="${esc(a.id)}">${esc(a.name)}</option>`).join('');
  }
  if (!rows.length) { t.innerHTML = '<tr><td class="muted">还没有子密钥</td></tr>'; return; }
  t.innerHTML = `
    <thead><tr><th>名称</th><th>Key</th><th>用量</th><th>配额</th><th>RPM</th><th>状态</th><th>操作</th></tr></thead>
    <tbody>${rows.map((k) => `
      <tr>
        <td>${esc(k.name)}<div class="muted mono">${esc(k.id)}</div></td>
        <td class="mono">${esc(k.key_hint)}</td>
        <td class="mono">${k.requests} 次<br>${fmtTok(k.prompt_tokens + k.completion_tokens)} tok</td>
        <td class="mono">${k.quota_requests ? k.quota_requests + ' 次' : '不限'} / ${k.quota_tokens ? fmtTok(k.quota_tokens) + ' tok' : '不限'}</td>
        <td class="mono">${k.rpm_limit || '不限'}</td>
        <td>${k.enabled ? '<span class="pill ok">启用</span>' : '<span class="pill bad">禁用</span>'}</td>
        <td>
          <button class="small" data-act="toggle" data-id="${esc(k.id)}" data-enabled="${k.enabled}">${k.enabled ? '禁用' : '启用'}</button>
          <button class="small" data-act="reset" data-id="${esc(k.id)}">清零</button>
          <button class="small danger" data-act="del" data-id="${esc(k.id)}">删除</button>
        </td>
      </tr>`).join('')}</tbody>`;
  $$('#keys-table button').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const id = btn.dataset.id;
      try {
        if (btn.dataset.act === 'toggle') {
          await api('/panel/api/keys/' + id, { method: 'PATCH', body: JSON.stringify({ enabled: btn.dataset.enabled !== 'true' }) });
        } else if (btn.dataset.act === 'reset') {
          await api('/panel/api/keys/' + id, { method: 'PATCH', body: JSON.stringify({ reset_usage: true }) });
        } else if (btn.dataset.act === 'del') {
          if (!confirm('删除这个密钥？')) return;
          await api('/panel/api/keys/' + id, { method: 'DELETE' });
        }
        await loadKeys();
      } catch (e) { toast(e.message, 'bad'); }
    });
  });
}

$('#key-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const body = {
    name: f.name.value.trim(),
    quota_requests: Number(f.quota_requests.value || 0),
    quota_tokens: Number(f.quota_tokens.value || 0),
    rpm_limit: Number(f.rpm_limit.value || 0),
    max_concurrent: Number(f.max_concurrent.value || 0),
    ttl_days: Number(f.ttl_days.value || 0),
    account_id: f.account_id.value,
    allowed_models: f.allowed_models.value.split(',').map((s) => s.trim()).filter(Boolean),
  };
  try {
    const r = await api('/panel/api/keys', { method: 'POST', body: JSON.stringify(body) });
    setText('#key-created', '明文密钥（只显示一次）：' + r.key);
    await loadKeys();
  } catch (e) { toast(e.message, 'bad'); }
});

/* ----------------------------------------------------------------- config */

async function loadConfig() {
  const data = await api('/panel/api/config');
  state.config = data.config;
  const f = $('#config-form');
  const c = data.config;
  f.api_key.value = c.api_key || '';
  f.mode.value = c.mode || 'thread';
  f.template_jam.value = c.template_jam || '';
  f.thread_mode.value = c.thread_mode || 'root';
  f.pool_size.value = c.pool_size || 2;
  f.device_policy.value = c.device_policy || 'text';
  f.queue_timeout_sec.value = c.queue_timeout_sec ?? 30;
  f.max_tool_steps.value = c.max_tool_steps || 12;
  f.conversation_ttl_sec.value = c.conversation_ttl_sec || 1800;
  f.project_id.value = c.project_id || '';
  f.reasoning.value = c.reasoning || '';
  f.turn_timeout_sec.value = c.turn_timeout_sec;
  f.poll_interval_ms.value = c.poll_interval_ms;
  f.max_concurrent.value = c.max_concurrent;
  f.delete_after.value = String(!!c.delete_after);
  f.typewriter.value = String(!!c.typewriter);
  f.realtime.value = String(c.realtime !== false);
  f.workspace_mode.value = c.workspace_mode || 'worktree';
  f.workspace_root.value = c.workspace_root || '';
  f.allowed_models.value = (c.allowed_models || []).join(', ');
  setText('#config-path', '配置文件：' + (data.path || 'config.json') + '（改端口/账号目录需重启进程）');
}

$('#config-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const body = {
    api_key: f.api_key.value.trim(),
    mode: f.mode.value,
    template_jam: f.template_jam.value.trim(),
    thread_mode: f.thread_mode.value,
    pool_size: Number(f.pool_size.value || 2),
    device_policy: f.device_policy.value,
    queue_timeout_sec: Number(f.queue_timeout_sec.value || 0),
    max_tool_steps: Number(f.max_tool_steps.value || 12),
    conversation_ttl_sec: Number(f.conversation_ttl_sec.value || 1800),
    project_id: f.project_id.value.trim(),
    reasoning: f.reasoning.value,
    turn_timeout_sec: Number(f.turn_timeout_sec.value),
    poll_interval_ms: Number(f.poll_interval_ms.value),
    max_concurrent: Number(f.max_concurrent.value),
    delete_after: f.delete_after.value === 'true',
    typewriter: f.typewriter.value === 'true',
    realtime: f.realtime.value === 'true',
    workspace_mode: f.workspace_mode.value,
    workspace_root: f.workspace_root.value.trim(),
    allowed_models: f.allowed_models.value.split(',').map((s) => s.trim()).filter(Boolean),
  };
  try {
    await api('/panel/api/config', { method: 'POST', body: JSON.stringify(body) });
    toast('已保存；api_key 变更立即生效', 'ok');
    await loadConfig();
  } catch (e) { toast(e.message, 'bad'); }
});

/* ------------------------------------------------------------------- logs */

async function loadLogs() {
  const data = await api('/panel/api/logs?limit=100');
  state.logs = data.logs || [];
  const t = $('#logs-table');
  if (!state.logs.length) { t.innerHTML = '<tr><td class="muted">暂无日志</td></tr>'; return; }
  t.innerHTML = `
    <thead><tr><th>时间</th><th>账号</th><th>模型</th><th>状态</th><th>耗时</th><th>tokens</th><th>线程 / 广告</th></tr></thead>
    <tbody>${state.logs.map((r) => `
      <tr>
        <td class="mono">${esc(shortTime(r.time))}</td>
        <td>${esc(r.account)}</td>
        <td class="mono">${esc(r.model)}</td>
        <td>${statusPill(r.status, r.error)}${r.stream ? ' <span class="pill">stream</span>' : ''}</td>
        <td class="mono">${(r.duration_ms / 1000).toFixed(2)}s</td>
        <td class="mono">${r.prompt_tokens}/${r.output_tokens}<span class="muted"> ·cache ${r.cache_tokens || 0}</span></td>
        <td class="mono">${esc(r.jam_id || '—')}${r.ad_brand ? ' · ' + esc(r.ad_brand) : ''}</td>
      </tr>`).join('')}</tbody>`;
}

$('#btn-refresh-logs').addEventListener('click', () => loadLogs().catch((e) => toast(e.message, 'bad')));

/* ------------------------------------------------------------------- boot */

async function refreshAll() {
  try {
    await Promise.all([loadOverview(), loadAccounts(), loadConfig()]);
    setText('#version', 'v' + (state.overview ? state.overview.version : 'dev'));
  } catch (e) {
    toast(e.message, 'bad');
  }
}

(async function boot() {
  const saved = localStorage.getItem('capy2api_key');
  if (saved) state.key = saved;
  try {
    await api('/panel/api/overview');
    showApp();
    await refreshAll();
  } catch (e) {
    showLogin();
  }
})();
