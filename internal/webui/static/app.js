const BASE = (typeof window !== "undefined" && window.__BASE__) ||
  (typeof location !== "undefined" && location.pathname.startsWith("/codearts") ? "/codearts" : "") ||
  "";

function cookiePath() {
  return BASE || "/";
}

// admin_session 是 HttpOnly，JS 改不动它 —— 真正让会话失效的只有服务端的
// /api/logout。这里保留一个显式提示，避免有人以为清理过 cookie 就等于退出。
function clearSessionCookies() {
  /* no-op by design: server-side logout invalidates the session */
}

let csrfToken = "";

// 自动刷新定时器单例：init() 会被重复调用（登录成功、重连），
// 不持有句柄的话每次登录都多挂一个 5 秒定时器，且 401 之后仍在跑。
let _refreshTimer = null;

function stopAutoRefresh() {
  if (_refreshTimer) {
    clearInterval(_refreshTimer);
    _refreshTimer = null;
  }
}

async function loadCSRF() {
  try {
    const res = await fetch(BASE + "/api/csrf", { credentials: "same-origin" });
    const data = await res.json().catch(() => ({}));
    if (res.ok) csrfToken = data.csrf_token || "";
  } catch (_) {
    csrfToken = "";
  }
}

async function api(url, options = {}) {
  const headers = { "Content-Type": "application/json", ...(options.headers || {}) };
  if (options.method && options.method !== "GET" && options.method !== "HEAD" && csrfToken) {
    headers["X-CSRF-Token"] = csrfToken;
  }
  let res;
  try {
    res = await fetch(BASE + url, {
      credentials: "same-origin",
      ...options,
      headers,
      // 后端卡死时不能让 UI 无限等待——15s 后自己收回来。
      signal: options.signal || AbortSignal.timeout(15000),
    });
  } catch (err) {
    if (err && (err.name === "TimeoutError" || err.name === "AbortError")) {
      throw new Error("请求超时");
    }
    throw err;
  }
  if (res.status === 401) {
    // login itself can 401 on bad password — keep message, don't loop-clear mid-login
    if (url !== "/api/login") {
      stopAutoRefresh();
      showLogin();
      throw new Error("unauthorized");
    }
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// openExternal 只放行 http(s) 且一律 noopener：授权地址来自后端 JSON，
// 不校验 scheme 就是 javascript:/data: 注入面；不带 noopener 则第三方授权页
// 能通过 window.opener 反向操作本面板（reverse tabnabbing）。
function openExternal(url) {
  if (!url) return;
  let u;
  try {
    u = new URL(url, location.origin);
  } catch (_) {
    return toast("授权地址非法");
  }
  if (u.protocol !== "https:" && u.protocol !== "http:") {
    return toast("授权地址协议不合法：" + u.protocol);
  }
  window.open(u.href, "_blank", "noopener,noreferrer");
}

function $(id) {
  return document.getElementById(id);
}

function toast(msg) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.remove("show"), 2800);
}

function esc(s) {
  return String(s ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function showLogin() {
  $("login-page").classList.remove("hidden");
  $("app").classList.add("hidden");
}

function showApp() {
  $("login-page").classList.add("hidden");
  $("app").classList.remove("hidden");
}

function badge(status) {
  const s = String(status || "").toLowerCase();
  let cls = "bb";
  if (["registered", "active", "success", "ok", "token_ok", "activated", "ready"].includes(s)) cls = "bg";
  else if (["quota_exhausted"].includes(s)) cls = "bo"; // 透支中：仍可调用
  else if (["failed", "error", "dead", "expired", "disabled"].includes(s)) cls = "br";
  else if (["pending", "running", "manual", "imported", "no_cred", "token_expired", "unknown"].includes(s)) cls = "bo";
  return `<span class="badge ${cls}">${esc(gateLabel(status) || status || "—")}</span>`;
}

function formatLastError(raw) {
  const s = String(raw || "").trim();
  if (!s) return "";
  if (/APIG\.0203|APIG\.0201|Backend timeout|upstream timeout|stream cut/i.test(s)) {
    return "华为网关超时或思考中断（已自动换号续流）";
  }
  if (/00001041|并发会话|session busy/i.test(s)) {
    return "并发会话已满（已自动换号）";
  }
  if (/context canceled|client canceled/i.test(s)) {
    return "请求已取消";
  }
  try {
    const o = JSON.parse(s);
    const code = o.error_code || o.code || "";
    const msg = o.error_msg || o.err_msg || o.message || "";
    if (code || msg) return formatLastError(`${code} ${msg}`.trim()) || `${code} ${msg}`.trim();
  } catch (_) {}
  return s.length > 72 ? s.slice(0, 72) + "…" : s;
}

function gateLabel(g) {
  const map = {
    ready: "可调用",
    quota_exhausted: "透支中",
    expired: "已到期",
    disabled: "已禁用",
    no_cred: "无凭证",
    token_expired: "票过期",
    unknown: "未知",
  };
  return map[g] || g;
}

function fmtTime(t) {
  if (!t) return "—";
  const d = new Date(t);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return "—";
  return d.toLocaleString();
}

function expireText(q) {
  if (!q) return "上游未返回";
  const raw = (q.expire_time || "").trim();
  if (!raw) return "上游未返回";
  return esc(raw);
}

let _acctFilter = "all";
let _allAccountsCache = null; // 全池（国际+国内），供总览快览与 Key 选号复用
let _acctSearch = "";
let _accountsCache = [];

// —— generic click-to-sort table headers ——
const _sortState = {};

function compareSort(a, b) {
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a ?? "").localeCompare(String(b ?? ""), "zh");
}

function applySort(tableId, list, keyFn) {
  const st = _sortState[tableId];
  if (!st || !st.key) return list;
  const dir = st.dir === "desc" ? -1 : 1;
  return [...list].sort((a, b) => compareSort(keyFn(st.key, a), keyFn(st.key, b)) * dir);
}

function bindSortableHeaders(tableId, keyFn, rerender) {
  const table = $(tableId);
  if (!table) return;
  table.querySelectorAll("thead th[data-sort]").forEach((th) => {
    th.classList.add("sortable");
    th.addEventListener("click", () => {
      const key = th.dataset.sort;
      const st = _sortState[tableId] || (_sortState[tableId] = {});
      st.dir = st.key === key && st.dir === "asc" ? "desc" : "asc";
      st.key = key;
      table.querySelectorAll("thead th[data-sort]").forEach((h) => h.classList.remove("sort-asc", "sort-desc"));
      th.classList.add(st.dir === "asc" ? "sort-asc" : "sort-desc");
      rerender();
    });
  });
}

/** Draws a donut chart into an <svg id=svgId> from [{label,value,color}]. */
function drawDonut(svgId, segments, opts = {}) {
  const svg = $(svgId);
  if (!svg) return;
  const size = opts.size || 120;
  const stroke = opts.stroke || 14;
  const r = (size - stroke) / 2;
  const c = size / 2;
  const circumference = 2 * Math.PI * r;
  const total = segments.reduce((s, x) => s + Math.max(0, Number(x.value) || 0), 0);
  if (!total) {
    svg.setAttribute("viewBox", `0 0 ${size} ${size}`);
    svg.innerHTML =
      `<circle cx="${c}" cy="${c}" r="${r}" fill="none" stroke="var(--border)" stroke-width="${stroke}"/>` +
      `<text x="${c}" y="${c + 4}" text-anchor="middle" class="donut-sub">暂无数据</text>`;
    return;
  }
  let offset = 0;
  let paths = "";
  segments.forEach((seg) => {
    const v = Math.max(0, Number(seg.value) || 0);
    if (!v) return;
    const frac = v / total;
    const len = frac * circumference;
    paths +=
      `<circle cx="${c}" cy="${c}" r="${r}" fill="none" stroke="${seg.color}" stroke-width="${stroke}" ` +
      `stroke-dasharray="${len.toFixed(2)} ${(circumference - len).toFixed(2)}" stroke-dashoffset="${(-offset).toFixed(2)}" ` +
      `transform="rotate(-90 ${c} ${c})"><title>${esc(seg.label)}: ${esc(String(v))}</title></circle>`;
    offset += len;
  });
  svg.setAttribute("viewBox", `0 0 ${size} ${size}`);
  svg.innerHTML =
    paths +
    `<text x="${c}" y="${c - 4}" text-anchor="middle" class="donut-total">${esc(String(total))}</text>` +
    `<text x="${c}" y="${c + 14}" text-anchor="middle" class="donut-sub">${esc(opts.centerLabel || "总数")}</text>`;
}

function goPage(name) {
  document.querySelectorAll(".nav a").forEach((a) => a.classList.toggle("active", a.dataset.page === name));
  document.querySelectorAll(".page").forEach((p) => p.classList.toggle("active", p.id === "p-" + name));
  if (name === "overview") loadDashboard();
  if (name === "accounts") {
    loadAccounts();
    loadDashboard();
  }
  if (name === "models") loadModels();
  if (name === "cn") loadCN();
  if (name === "oauth") fillOAuthAccounts();
  if (name === "proxies") loadProxies();
  if (name === "apikeys") {
    loadAccounts().then(() => {
      fillKeyAccountPicker();
      loadAPIKeys();
    });
  }
}

function fmtTokens(n) {
  const v = Number(n);
  if (!Number.isFinite(v)) return "—";
  const abs = Math.abs(v);
  if (abs >= 1e8) return (v / 1e8).toFixed(2).replace(/\.00$/, "") + "亿";
  if (abs >= 1e4) return (v / 1e4).toFixed(v % 1e4 === 0 ? 0 : 1) + "万";
  return String(Math.round(v));
}

/** Exact token count with thousand separators, e.g. 5,000,000 */
function fmtTokensExact(n) {
  const v = Number(n);
  if (!Number.isFinite(v)) return "—";
  return Math.round(v).toLocaleString("en-US");
}

/**
 * Used/total cell: same unit for both sides, exact numbers in title tooltip.
 * Avoids confusing mixes like "206 / 500万".
 */
function usageCell(q) {
  if (!q) return `<span class="muted">—</span>`;
  const used = Number(q.used_tokens || 0);
  const total = Number(q.package_tokens || 0);
  const remain = remainOf(q);
  if (!total && !used && !remain) return `<span class="muted">—</span>`;
  // 上游快照时间：数字来自 CodeArts 配额接口，每 30 分钟自动同步 / 手动刷新都会覆盖校准。
  const fetched = q.fetched_at ? new Date(q.fetched_at) : null;
  const fresh = fetched && !Number.isNaN(fetched.getTime()) && fetched.getFullYear() >= 2000
    ? ` · 上游快照 ${fmtMMDDHHmm(fetched)}（${relAgo(fetched)}）`
    : "";
  const exact = `已用 ${fmtTokensExact(used)} · 总量 ${fmtTokensExact(total)} · 剩余 ${fmtTokensExact(remain)}${fresh}`;
  // Always show exact digits so 已用/总量 never mix units (e.g. 206 vs 500万).
  const usedText = fmtTokensExact(used);
  const totalText = fmtTokensExact(total);
  const remainText = fmtTokensExact(remain);
  const pct = total > 0 ? Math.min(100, Math.max(0, (used / total) * 100)) : 0;
  const compact =
    total >= 1e4
      ? `<div class="quota-hint">${esc(fmtTokens(used))} / ${esc(fmtTokens(total))} · 剩 ${esc(fmtTokens(remain))}</div>`
      : "";
  return `<div class="quota-cell" title="${esc(exact)}">
    <div class="quota-line"><span class="ql">已用</span><b>${esc(usedText)}</b></div>
    <div class="quota-line"><span class="ql">总量</span><b>${esc(totalText)}</b></div>
    <div class="quota-line"><span class="ql">剩余</span><b class="remain">${esc(remainText)}</b></div>
    ${compact}
    <div class="quota-bar"><i style="width:${pct.toFixed(1)}%"></i></div>
  </div>`;
}

function remainOf(q) {
  if (!q) return 0;
  let r = Number(q.remain_tokens || 0);
  const pkg = Number(q.package_tokens || 0);
  const used = Number(q.used_tokens || 0);
  if ((!Number.isFinite(r) || r <= 0) && pkg > 0) r = Math.max(0, pkg - used);
  if (pkg > 0 && used >= 0 && (r <= 0 || Math.abs(pkg - used - r) > 1)) {
    // Prefer package - used when remain looks stale/missing.
    r = Math.max(0, pkg - used);
  }
  return r;
}

/** Short countdown for a future timestamp, e.g. 3h20m后 / 2d后. */
function relShort(t) {
  const ms = t.getTime() - Date.now();
  if (!Number.isFinite(ms)) return "";
  if (ms <= 0) return "已到";
  const m = Math.floor(ms / 60000);
  if (m < 1) return "即将";
  if (m < 60) return `${m}m后`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h${m % 60 ? `${m % 60}m` : ""}后`;
  return `${Math.floor(h / 24)}d后`;
}

/** Short "ago" text for a past timestamp, e.g. 3h前 / 12d前. */
function relAgo(t) {
  const ms = Date.now() - t.getTime();
  if (!Number.isFinite(ms) || ms < 0) return "";
  const m = Math.floor(ms / 60000);
  if (m < 60) return `${m}m前`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h前`;
  return `${Math.floor(h / 24)}d前`;
}

function fmtMMDDHHmm(t) {
  const p2 = (n) => String(n).padStart(2, "0");
  return `${p2(t.getMonth() + 1)}-${p2(t.getDate())} ${p2(t.getHours())}:${p2(t.getMinutes())}`;
}

/**
 * 额度重置信息 —— 重置时间以上游为准：
 * - upstream  上游 payload 带显式 reset 字段，直接展示；
 * - expire    套餐周期，以上游下发的到期时间结束；
 * - estimated 上游没给具体时间，按每天/每月 00:00 (UTC+8) 估算；
 * - observed  上游完全不下发时间时，用额度计数回滚观测到的重置记录。
 */
function resetInfo(q) {
  if (!q) return null;
  const at = q.next_reset_at ? new Date(q.next_reset_at) : null;
  if (at && !Number.isNaN(at.getTime()) && at.getFullYear() >= 2000) {
    const src = q.reset_source || "";
    const tag = src === "upstream" ? "上游" : src === "expire" ? "到期" : "预计";
    const cls = src === "upstream" ? "reset-upstream" : "reset-estimated";
    // 上游下发的有效期截止是纯日期（start_date ~ end_date），原样展示。
    const when = src === "expire" && q.end_date ? q.end_date : fmtMMDDHHmm(at);
    const win = q.start_date && q.end_date ? `（上游有效期 ${q.start_date} ~ ${q.end_date}）` : "";
    return { txt: `${tag} ${when} (${relShort(at)})`, cls, title: (q.reset_note || "") + win };
  }
  const last = q.last_reset_at ? new Date(q.last_reset_at) : null;
  if (last && !Number.isNaN(last.getTime()) && last.getFullYear() >= 2000) {
    const n = Number(q.reset_count || 0);
    const days = Number(q.reset_interval_hours || 0) / 24;
    const cyc = days > 0 ? ` · 观测${days >= 10 ? days.toFixed(0) : days.toFixed(1)}d` : "";
    const cnt = n > 1 ? ` · 第${n}次` : "";
    return {
      txt: `上次重置 ${fmtMMDDHHmm(last)} (${relAgo(last)})${cyc}`,
      cls: "reset-observed",
      title: `上游未下发重置时间，重置由上游决定；此处为额度计数回滚观测${cnt}${q.reset_note ? " · " + q.reset_note : ""}`,
    };
  }
  if (q.period) {
    return {
      txt: "上游未下发",
      cls: "reset-estimated",
      title: "上游 Free/套餐包未下发到期或重置时间；下次刷新观测到重置后会自动显示（重置以上游为准）",
    };
  }
  return null;
}

function keyUsageCell(k) {
  const reqs = Number(k?.request_count || 0);
  const inn = Number(k?.prompt_tokens || 0);
  const out = Number(k?.completion_tokens || 0);
  const flying = Number(k?.in_flight || 0);
  const total = inn + out;
  if (!reqs && !total && !flying) return `<span class="muted">—</span>`;
  const exact = `在途 ${fmtTokensExact(flying)} · 请求 ${fmtTokensExact(reqs)} · 输入 ${fmtTokensExact(inn)} · 输出 ${fmtTokensExact(out)} · 合计 ${fmtTokensExact(total)}`;
  return `<div class="quota-cell" title="${esc(exact)}">
    <div class="quota-line"><span class="ql">输入</span><b>${esc(fmtTokensExact(inn))}</b></div>
    <div class="quota-line"><span class="ql">输出</span><b>${esc(fmtTokensExact(out))}</b></div>
    <div class="quota-line"><span class="ql">合计</span><b class="remain">${esc(fmtTokensExact(total))}</b></div>
    <div class="quota-hint">请求 ${esc(fmtTokensExact(reqs))} · 在途 ${esc(fmtTokensExact(flying))}</div>
  </div>`;
}

function quotaText(a) {
  const q = a?.quota;
  if (!q || (!q.package_tokens && !q.remain_tokens && !q.used_tokens)) return "—";
  return `${q.package_name || "套餐"} · 已用 ${fmtTokensExact(q.used_tokens || 0)} / 总量 ${fmtTokensExact(q.package_tokens || 0)} · 剩 ${fmtTokensExact(remainOf(q))}`;
}

let _autoRefreshPaused = false;

// toggleAutoRefresh pauses/resumes the 5s overview auto-refresh so a long task
// can be observed without the panel repainting mid-stream.
function toggleAutoRefresh() {
  _autoRefreshPaused = !_autoRefreshPaused;
  const btn = $("pause-refresh-btn");
  if (btn) btn.textContent = _autoRefreshPaused ? "▶ 恢复刷新" : "⏸ 暂停刷新";
  const live = $("live-indicator");
  if (live && _autoRefreshPaused) live.textContent = "已暂停";
  if (!_autoRefreshPaused) refreshAll(false);
}

async function init() {
  await loadCSRF();
  $("base-url").textContent = location.origin + BASE + "/v1";
  $("api-sample").textContent =
    `curl ${location.origin}${BASE}/v1/models\n\n` +
    `curl ${location.origin}${BASE}/v1/chat/completions \\\n` +
    `  -H "Content-Type: application/json" \\\n` +
    `  -d '{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}]}'`;
  try {
    await api("/api/dashboard");
    showApp();
    refreshAll(true);
    // Auto-refresh every 5s — lightweight, no quota refresh.
    stopAutoRefresh();
    _refreshTimer = setInterval(() => {
      // Paused by user (observing a long task) — skip this tick.
      if (_autoRefreshPaused) return;
      // Don't refresh while user is interacting (typing, modal open, batch select).
      if (document.activeElement?.tagName === "INPUT" ||
          document.activeElement?.tagName === "TEXTAREA" ||
          document.activeElement?.tagName === "SELECT") return;
      // Preserve scroll positions.
      const miniScroll = $("acct-mini")?.scrollTop || 0;
      const tblScroll = document.querySelector("#p-accounts .tbl-wrap")?.scrollTop || 0;
      refreshAll(false).then(() => {
        // Restore scroll positions after re-render.
        const m = $("acct-mini"); if (m) m.scrollTop = miniScroll;
        const t = document.querySelector("#p-accounts .tbl-wrap"); if (t) t.scrollTop = tblScroll;
        // Live indicator (in-flight / timestamp) is set inside loadDashboard.
      });
    }, 5000);
  } catch (e) {
    // 只有真正未鉴权才回登录页。后端 500 / 反代 502 / 断网一律如实报错，
    // 否则运维现场会被误导成"密码错了"。
    stopAutoRefresh();
    if (e && e.message === "unauthorized") {
      showLogin();
    } else {
      const box = $("login-error");
      if (box) box.textContent = "服务不可用：" + (e && e.message ? e.message : "未知错误");
      showLogin();
    }
  }
}

async function refreshAll(refreshQuota = false) {
  // Paint cached quota first. Waiting on 31 Huawei insight calls leaves
  // the overview/account table blank after every refresh.
  // allSettled：一个接口超时不该让另一张表也跟着空掉。
  const [dashRes, acctRes] = await Promise.allSettled([loadDashboard(), loadAccounts()]);
  const failed = [dashRes, acctRes].find((r) => r.status === "rejected");
  if (failed && failed.reason && failed.reason.message !== "unauthorized") {
    toast("刷新失败：" + failed.reason.message);
  }
  if (document.querySelector("#p-cn")?.classList.contains("active")) loadCN();
  if (!refreshQuota) return;
  try {
    const list = _accountsCache || [];
    await Promise.allSettled(
      list
        .filter((a) => a.channel !== "cn" && a.credentials && a.credentials.access_key_id)
        .map((a) => api(`/api/accounts/${a.id}/insight/refresh`, { method: "POST", body: "{}" }))
    );
    await Promise.all([loadDashboard(), loadAccounts()]);
  } catch (_) {}
}

async function loadDashboard() {
  const d = await api("/api/dashboard");
  const stick = d.session_stick || {};
  const pc = d.prompt_cache || {};
  const rs = d.request_stats || {};
  const live = d.live || {};
  const today = rs.today || {};
  const stickRate = pc.stick_hit_rate != null ? pc.stick_hit_rate : stick.hit_rate;
  let stickPct = "0%";
  if (stickRate != null) {
    stickPct = (stickRate * 100).toFixed(1) + "%";
  }

  const inflight = Number(live.in_flight || 0);
  const liveBadge = $("live-indicator");
  if (liveBadge) {
    liveBadge.innerHTML = inflight > 0
      ? `<span class="inflight-dot"></span>在途 ${inflight}`
      : new Date().toLocaleTimeString();
  }

  const cs = d.channel_stats || {};
  const cnS = cs.cn || {}, intlS = cs.intl || {};
  $("ov-sub").textContent =
    `账号 ${d.accounts || 0} · 可调用 ${d.callable || 0} · 透支中 ${d.quota_exhausted || 0} · 在途 ${inflight} · 粘滞 ${stickPct}` +
    ` · 国际 ${intlS.callable || 0}/${intlS.accounts || 0} · 国内码道 ${cnS.callable || 0}/${cnS.accounts || 0}` +
    ` · 更新 ${new Date(d.time || Date.now()).toLocaleString()}`;
  if ($("acct-sub")) {
    $("acct-sub").textContent =
      `导入 txt / 手动添加 / Token 刷新 · 共 ${d.accounts || 0} 个账号全部列出`;
  }

  const inTok = today.prompt_tokens ?? 0;
  const outTok = today.completion_tokens ?? 0;
  const usedTok = d.used_tokens ?? 0;
  const pkgTok = d.package_tokens ?? 0;
  const cards = [
    { icon: "👥", bg: "rgba(78,168,255,.12)", val: d.accounts ?? 0, key: "账号总数", trend: `可调用 ${d.callable ?? 0}` },
    { icon: "✅", bg: "rgba(63,185,80,.12)", val: d.callable ?? 0, key: "可调用", trend: `透支中 ${d.quota_exhausted ?? 0}` },
    { icon: "🚀", bg: "rgba(163,113,247,.12)", val: today.requests ?? 0, key: "今日请求", trend: `错误 ${today.errors ?? 0}` },
    { icon: "⚡", bg: "rgba(210,153,34,.12)", val: fmtTokens(d.remain_tokens ?? 0), key: "剩余额度", trend: `已用 ${fmtTokens(usedTok)} / ${fmtTokens(pkgTok)}` },
  ];
  $("stats").innerHTML = cards
    .map(
      (c) =>
        `<div class="stat"><div class="icon" style="background:${c.bg}">${c.icon}</div>` +
        `<div class="val">${esc(c.val)}</div><div class="key">${esc(c.key)}</div>` +
        `<div class="trend">${esc(c.trend || "")}</div></div>`
    )
    .join("");

  const ph = d.pool_health || {};
  const rel = d.reliability || {};
  const abortPct = ((rel.today_abort_rate || 0) * 100).toFixed(1) + "%";
  const cacheNote = pc.upstream_supported ? "上游可缓存" : "上游无 prefix-cache";
  const localHit = ((pc.local_hit_rate || 0) * 100).toFixed(1) + "%";
  const health = [
    { icon: "🟢", bg: "rgba(63,185,80,.12)", val: ph.ready ?? 0, key: "Ready", trend: "有额度可调用", cls: "stat-ready" },
    { icon: "🟡", bg: "rgba(210,153,34,.12)", val: ph.depleted ?? 0, key: "透支", trend: "仍可调，首字节慢", cls: "stat-depleted" },
    { icon: "🔴", bg: "rgba(248,81,73,.12)", val: ph.disabled ?? 0, key: "禁用", trend: `半开 ${ph.circuit_half ?? 0}`, cls: "stat-disabled" },
    { icon: "📡", bg: "rgba(78,168,255,.12)", val: abortPct, key: "客户端掉线率", trend: `今日 ${rel.today_client_aborts ?? 0} 次` },
    { icon: "⏱", bg: "rgba(210,153,34,.12)", val: `${rel.today_header_wait_p95 ?? 0}ms`, key: "首字节 P95", trend: `换号 ${rel.today_rotations ?? 0}` },
    { icon: "🧯", bg: "rgba(248,81,73,.12)", val: ph.circuit_open ?? 0, key: "熔断开数", trend: `半开 ${ph.circuit_half ?? 0}`, cls: "stat-disabled" },
    { icon: "💾", bg: "rgba(125,211,252,.12)", val: localHit, key: "缓存命中率", trend: cacheNote },
  ];
  // 渠道健康：国际版 / 国内码道各自的 ready / 透支 / 账号数
  {
    const cnS = (d.channel_stats || {}).cn || {};
    const intlS = (d.channel_stats || {}).intl || {};
    health.push({
      icon: "🌐", bg: "rgba(78,168,255,.12)",
      val: `${intlS.ready || 0}/${intlS.accounts || 0}`, key: "国际版 Ready",
      trend: `透支 ${intlS.depleted || 0} · 禁用 ${intlS.disabled || 0}`,
    });
    health.push({
      icon: "🀄", bg: "rgba(210,153,34,.12)",
      val: `${cnS.ready || 0}/${cnS.accounts || 0}`, key: "国内码道 Ready",
      trend: `透支 ${cnS.depleted || 0} · 禁用 ${cnS.disabled || 0}`,
    });
  }
  if ($("health-stats")) {
    $("health-stats").innerHTML = health
      .map(
        (c) =>
          `<div class="stat ${c.cls || ""}"><div class="icon" style="background:${c.bg}">${c.icon}</div>` +
          `<div class="val">${esc(c.val)}</div><div class="key">${esc(c.key)}</div>` +
          `<div class="trend">${esc(c.trend || "")}</div></div>`
      )
      .join("");
  }

  const healthSeg = [
    { label: "Ready", value: ph.ready || 0, color: "var(--green)" },
    { label: "透支", value: ph.depleted || 0, color: "var(--orange)" },
    { label: "禁用", value: ph.disabled || 0, color: "var(--red)" },
  ];
  drawDonut("chart-health-donut", healthSeg, { centerLabel: "账号" });
  if ($("health-legend")) {
    $("health-legend").innerHTML = healthSeg
      .map((x) => `<span><span class="dot" style="background:${x.color}"></span>${esc(x.label)}<b>${esc(x.value)}</b></span>`)
      .join("");
  }

  const keyUsage = d.key_usage || [];
  if ($("key-usage-card") && $("key-usage-strip")) {
    if (!keyUsage.length) {
      $("key-usage-card").style.display = "none";
    } else {
      $("key-usage-card").style.display = "";
      $("key-usage-strip").innerHTML = keyUsage
        .map((k) => {
          const flying = Number(k.in_flight || 0);
          return `<div class="usage-item"><div class="uk">${esc(k.name || k.id || "key")}</div>` +
            `<div class="uv">${esc(flying)}</div><div class="uh">在途 · 请求 ${esc(fmtTokensExact(k.request_count || 0))}</div></div>`;
        })
        .join("");
    }
  }

  if ($("usage-strip")) {
    $("usage-strip").innerHTML = [
      { k: "今日输入 token", v: fmtTokensExact(inTok), h: fmtTokens(inTok) },
      { k: "今日输出 token", v: fmtTokensExact(outTok), h: fmtTokens(outTok) },
      { k: "套餐已用", v: fmtTokensExact(usedTok), h: `总量 ${fmtTokensExact(pkgTok)}` },
      { k: "套餐剩余", v: fmtTokensExact(d.remain_tokens ?? 0), h: `P95 ${today.latency_p95_ms ?? 0}ms` },
    ]
      .map(
        (x) =>
          `<div class="usage-item"><div class="uk">${esc(x.k)}</div>` +
          `<div class="uv">${esc(x.v)}</div><div class="uh">${esc(x.h)}</div></div>`
      )
      .join("");
  }

  if ($("proto-strip")) {
    const chatN = today.proto_chat ?? 0;
    const msgN = today.proto_messages ?? 0;
    const respN = today.proto_responses ?? 0;
    const slowN = today.slow_count ?? 0;
    const totN = chatN + msgN + respN;
    const pct = (n) => (totN > 0 ? ((n / totN) * 100).toFixed(0) + "%" : "0%");
    $("proto-strip").innerHTML = [
      { k: "OpenAI chat", v: fmtTokensExact(chatN), h: pct(chatN) },
      { k: "Claude messages", v: fmtTokensExact(msgN), h: pct(msgN) },
      { k: "Responses", v: fmtTokensExact(respN), h: pct(respN) },
      { k: "慢请求 (>3s)", v: fmtTokensExact(slowN), h: `占 ${totN > 0 ? ((slowN / totN) * 100).toFixed(1) : 0}%` },
    ]
      .map(
        (x) =>
          `<div class="usage-item"><div class="uk">${esc(x.k)}</div>` +
          `<div class="uv">${esc(x.v)}</div><div class="uh">${esc(x.h)}</div></div>`
      )
      .join("");
    drawDonut(
      "chart-proto-donut",
      [
        { label: "OpenAI chat", value: chatN, color: "var(--accent)" },
        { label: "Claude messages", value: msgN, color: "#7dd3fc" },
        { label: "Responses", value: respN, color: "var(--violet)" },
      ],
      { centerLabel: "请求" }
    );
  }

  const tl = (rs.timeline || []).map((p) => ({
    label: p.label || "",
    requests: p.requests || 0,
    errors: p.errors || 0,
    promptTokens: p.prompt_tokens || 0,
    completionTokens: p.completion_tokens || 0,
  }));
  drawTimeline(tl);
}

function drawTimeline(tl) {
  const svg = $("chart-timeline");
  if (!svg) return;
  if (!tl.length) {
    svg.innerHTML = "";
    return;
  }
  const W = 800,
    H = 220,
    padL = 36,
    padB = 26,
    padT = 10;
  const iw = W - padL - 8,
    ih = H - padB - padT;
  const maxReq = Math.max(1, ...tl.map((p) => p.requests));
  const maxTok = Math.max(1, ...tl.map((p) => p.promptTokens + p.completionTokens));
  const total = tl.length;
  const bw = iw / total;
  let grid = "";
  for (let g = 0; g <= 4; g++) {
    const y = padT + (ih * g) / 4;
    grid += `<line class="chart-grid" x1="${padL}" y1="${y}" x2="${W - 8}" y2="${y}"/>`;
  }
  let labels = "";
  tl.forEach((p, i) => {
    if (i % Math.max(1, Math.floor(total / 6)) === 0) {
      labels += `<text class="chart-label" x="${padL + bw * i + bw / 2}" y="${H - 8}" text-anchor="middle">${esc(p.label)}</text>`;
    }
  });
  let bars = "";
  tl.forEach((p, i) => {
    const x = padL + bw * i + bw * 0.18;
    const bx = bw * 0.64;
    const h1 = Math.max(p.requests ? 2 : 0, (ih * p.requests) / maxReq);
    const inH = Math.max(p.promptTokens ? 2 : 0, (ih * p.promptTokens) / maxTok);
    const outH = Math.max(p.completionTokens ? 2 : 0, (ih * p.completionTokens) / maxTok);
    if (h1)
      bars += `<rect x="${x}" y="${padT + ih - h1}" width="${bx * 0.28}" height="${h1}" rx="2" fill="var(--accent)" opacity="0.75"><title>${esc(p.label)} 请求 ${p.requests}</title></rect>`;
    if (inH)
      bars += `<rect x="${x + bx * 0.32}" y="${padT + ih - inH}" width="${bx * 0.28}" height="${inH}" rx="2" fill="#7dd3fc" opacity="0.85"><title>${esc(p.label)} 输入 ${fmtTokensExact(p.promptTokens)}</title></rect>`;
    if (outH)
      bars += `<rect x="${x + bx * 0.64}" y="${padT + ih - outH}" width="${bx * 0.28}" height="${outH}" rx="2" fill="var(--green)" opacity="0.75"><title>${esc(p.label)} 输出 ${fmtTokensExact(p.completionTokens)}</title></rect>`;
    if (p.errors > 0) {
      const he = Math.max(2, (ih * p.errors) / maxReq);
      bars += `<rect x="${x + bx * 0.85}" y="${padT + ih - he}" width="${bx * 0.15}" height="${he}" rx="2" fill="var(--red)"><title>${esc(p.label)} 错误 ${p.errors}</title></rect>`;
    }
  });
  svg.innerHTML =
    `<line class="chart-grid" x1="${padL}" y1="${padT}" x2="${padL}" y2="${padT + ih}"/>` +
    `<line class="chart-grid" x1="${padL}" y1="${padT + ih}" x2="${W - 8}" y2="${padT + ih}"/>` +
    grid +
    bars +
    labels;
}

const _selAccounts = new Set();
const _selProxies = new Set();
const _selAPIKeys = new Set();

function syncSelCount(kind) {
  if (kind === "acct") {
    const n = _selAccounts.size;
    if ($("acct-sel-count")) $("acct-sel-count").textContent = `已选 ${n}`;
    if ($("acct-check-all")) {
      const boxes = document.querySelectorAll("#accounts-table .row-check");
      $("acct-check-all").checked = boxes.length > 0 && n === boxes.length;
    }
  } else if (kind === "proxy") {
    const n = _selProxies.size;
    if ($("proxy-sel-count")) $("proxy-sel-count").textContent = `已选 ${n}`;
    if ($("proxy-check-all")) {
      const boxes = document.querySelectorAll("#proxies-table .row-check");
      $("proxy-check-all").checked = boxes.length > 0 && n === boxes.length;
    }
  } else if (kind === "apikey") {
    const n = _selAPIKeys.size;
    if ($("apikey-sel-count")) $("apikey-sel-count").textContent = `已选 ${n}`;
    if ($("apikey-check-all")) {
      const boxes = document.querySelectorAll("#apikeys-table .row-check");
      $("apikey-check-all").checked = boxes.length > 0 && n === boxes.length;
    }
  }
}

function proxyOptionsHTML(selected) {
  const items = window._proxies || [];
  const cur = selected || "";
  let html = `<option value="">直连（不绑定）</option>`;
  for (const p of items) {
    const id = p.id || "";
    const label = p.name || p.address || id;
    html += `<option value="${esc(id)}"${id === cur ? " selected" : ""}>${esc(label)}</option>`;
  }
  return html;
}

function fillAcctBatchProxySelect() {
  const sel = $("acct-batch-proxy");
  if (!sel) return;
  const cur = sel.value;
  sel.innerHTML = proxyOptionsHTML(cur);
  sel.value = cur;
}

async function setAccountProxy(id, proxy_id) {
  try {
    await api(`/api/accounts/${id}/proxy`, { method: "PUT", body: JSON.stringify({ proxy_id: proxy_id || "" }) });
    const a = (_accountsCache || []).find((x) => x.id === id);
    if (a) a.proxy_id = proxy_id || "";
    toast(proxy_id ? "已绑定代理" : "已设为直连");
  } catch (e) {
    toast(e.message);
    loadAccounts();
  }
}

async function loadAccounts() {
  // 账户池 = 国际版专属视图；国内码道账号在「国内码道」页管理。
  const [list, all] = await Promise.all([
    api("/api/accounts?channel=intl"),
    api("/api/accounts").catch(() => null),
  ]);
  _accountsCache = list || [];
  if (all) _allAccountsCache = all;
  if (!window._proxies) {
    try {
      const d = await api("/api/proxies");
      window._proxies = d.items || [];
    } catch (_) {
      window._proxies = [];
    }
  }
  renderAccounts();
}


function acctSortValue(key, a) {
  const q = a.quota || {};
  switch (key) {
    case "email":
      return (a.email || "").toLowerCase();
    case "gate":
      return a.call_gate || (a.disabled ? "disabled" : "unknown");
    case "remain":
      return remainOf(q);
    case "expire":
      return q.expire_time || "";
    case "reset":
      if (q.next_reset_at) return new Date(q.next_reset_at).getTime();
      return q.last_reset_at ? new Date(q.last_reset_at).getTime() : 0;
    case "success":
      return Number(a.success_count || 0) - Number(a.fail_count || 0);
    case "last_used":
      return a.last_used_at ? new Date(a.last_used_at).getTime() : 0;
    default:
      return "";
  }
}

function renderAccounts() {
  const q = (_acctSearch || "").trim().toLowerCase();
  const list = (_accountsCache || []).filter((a) => {
    const g = a.call_gate || "";
    if (_acctFilter === "ready" && !(g === "ready" || g === "quota_exhausted")) return false;
    if (_acctFilter === "quota_exhausted" && g !== "quota_exhausted") return false;
    if (_acctFilter === "disabled" && !(!!a.disabled || g === "disabled")) return false;

    if (q) {
      const hay = `${a.email || ""} ${a.id || ""}`.toLowerCase();
      if (!hay.includes(q)) return false;
    }
    return true;
  });
  const totalEl = $("accounts-total");
  if (totalEl) totalEl.textContent = `共 ${_accountsCache.length} 个 · 显示 ${list.length}`;
  const tb = document.querySelector("#accounts-table tbody");
  const mini = $("acct-mini");
  if (!list.length) {
    tb.innerHTML = `<tr><td colspan="11" class="empty">暂无账号</td></tr>`;
    if (mini) mini.innerHTML = `<div class="empty">暂无账号</div>`;
    _selAccounts.clear();
    syncSelCount("acct");
    return;
  }
  // 剪枝按全量而不是筛选结果：切换筛选标签不该清空用户已勾选的账号。
  const keep = new Set((_accountsCache || []).map((a) => a.id));
  for (const id of [..._selAccounts]) {
    if (!keep.has(id)) _selAccounts.delete(id);
  }
  const sortedList = applySort("accounts-table", list, acctSortValue);
  tb.innerHTML = sortedList
    .map((a) => {
      const q = a.quota || {};
      const period = q.period_label || q.period || "—";
      const ri = resetInfo(q);
      const toggleBtn = a.disabled
        ? `<button class="btn sm" onclick="enableAccount('${esc(a.id)}')">启用</button>`
        : `<button class="btn sm" onclick="disableAccount('${esc(a.id)}')">禁用</button>`;
      const errText = formatLastError(a.last_error);
      const err = errText
        ? `<div class="cell-err" title="${esc(a.last_error)}">${esc(errText)}</div>`
        : "";
      const okN = a.success_count ?? 0;
      const failN = a.fail_count ?? 0;
      const checked = _selAccounts.has(a.id) ? "checked" : "";
      const healthCls = a.circuit === 1 ? "health-circuit" : (a.call_gate === "ready" ? "health-ready" : (a.call_gate === "quota_exhausted" ? "health-depleted" : ""));
      return `<tr class="${healthCls}">
        <td class="col-check"><input type="checkbox" class="row-check" data-id="${esc(a.id)}" ${checked}></td>
        <td class="col-email"><div class="email" title="${esc(a.email)}">${esc(a.email)}${a.channel === "cn" ? ' <span class="badge cn">CN</span>' : ""}</div>${err}</td>
        <td class="col-gate">${badge(a.call_gate || (a.disabled ? "disabled" : "unknown"))}</td>
        <td class="col-pkg nowrap">${esc(q.package_name || "—")}</td>
        <td class="col-period"><div>${esc(period)}</div>${ri ? `<div class="quota-hint ${ri.cls}" title="${esc(ri.title)}">${esc(ri.txt)}</div>` : ""}</td>
        <td class="col-quota">${usageCell(q)}</td>
        <td class="col-expire nowrap">${expireText(q)}</td>
        <td class="col-calls nowrap"><span class="badge bg">${okN}</span> / <span class="badge ${failN ? "br" : "bb"}">${failN}</span></td>
        <td class="col-proxy">
          <select class="acct-proxy" data-id="${esc(a.id)}" onchange="setAccountProxy(this.dataset.id, this.value)">
            ${proxyOptionsHTML(a.proxy_id || "")}
          </select>
        </td>
        <td class="col-used nowrap">${esc(fmtTime(a.last_used_at))}</td>
        <td class="col-ops">
          <div class="ops">
            <button class="btn sm" onclick="fetchQuota('${esc(a.id)}')">额度</button>
            ${toggleBtn}
            <button class="btn sm danger" onclick="deleteAccount('${esc(a.id)}')">删除</button>
          </div>
        </td>
      </tr>`;
    })
    .join("");
  syncSelCount("acct");
  fillAcctBatchProxySelect();

  if (mini) {
    // 快览是全池视角（国际 + 国内），rows 会带渠道徽标
    const list = _allAccountsCache || _accountsCache || [];
    if (!list.length) { mini.innerHTML = `<div class="empty">暂无账号</div>`; return; }
    const rows = list.map((a) => {
      const okN = a.success_count ?? 0;
      const failN = a.fail_count ?? 0;
      const q = a.quota || {};
      const models = a.last_model || (a.models || []).join(", ") || "—";
      // 输入/输出 token：优先用 per-account 统计，没有就按 quota 兜底
      const acctPrompt = a.prompt_tokens_total || a.prompt_tokens || 0;
      const acctCompl = a.completion_tokens_total || a.completion_tokens || 0;
      const inTok = acctPrompt > 0 ? fmtTokensExact(acctPrompt) : (q.used_tokens > 0 ? fmtTokensExact(q.used_tokens) : "0");
      const outTok = acctCompl > 0 ? fmtTokensExact(acctCompl) : "—";
      // 耗时
      const latMS = a.last_latency_ms || 0;
      const lat = latMS > 0 ? (latMS >= 1000 ? (latMS / 1000).toFixed(1) + "s" : latMS + "ms") : "—";
      const gate = a.call_gate || (a.disabled ? "disabled" : "unknown");
      const lastUsed = a.last_used_at ? fmtTime(a.last_used_at) : "—";
      const remain = q.remain_tokens > 0 ? fmtTokens(q.remain_tokens) : "—";
      const riMini = resetInfo(q);
      const healthCls = a.circuit === 1 ? "health-circuit" : (a.call_gate === "ready" ? "health-ready" : (a.call_gate === "quota_exhausted" ? "health-depleted" : ""));
      return `<tr class="${healthCls}">
        <td class="mini-email" title="${esc(a.email)}">${esc(a.email)}${a.channel === "cn" ? ' <span class="badge cn">CN</span>' : ""}</td>
        <td class="mini-models" title="${esc(models)}">${esc(models.length > 30 ? models.slice(0, 28) + "…" : models)}</td>
        <td class="mini-num">${esc(inTok)}</td>
        <td class="mini-num">${esc(outTok)}</td>
        <td class="mini-num">${esc(remain)}</td>
        <td class="mini-time">${esc(riMini ? riMini.txt : "—")}</td>
        <td class="mini-num">${esc(lat)}</td>
        <td>${badge(gate)}</td>
        <td class="mini-num">${okN}/${failN}</td>
        <td class="mini-time">${esc(lastUsed)}</td>
      </tr>`;
    }).join("");
    mini.innerHTML = `<table class="mini-table">
      <thead><tr>
        <th>账号</th><th>模型</th><th>输入Token</th><th>输出Token</th><th>剩余额度</th><th>额度重置</th><th>最近耗时</th><th>状态</th><th>成/败</th><th>最后使用</th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
  }
}

async function runAccountBatch(action) {
  const ids = [..._selAccounts];
  if (!ids.length) return toast("先勾选账号");
  const body = { action, ids };
  if (action === "set_proxy" || action === "clear_proxy") {
    body.action = "set_proxy";
    body.proxy_id = action === "clear_proxy" ? "" : ($("acct-batch-proxy")?.value || "");
    // 下拉首项是"直连"：忘了选代理就点应用，会把选中的账号全部改成直连，
    // 真实出口从代理切回本机且无确认、无回滚。
    if (action === "set_proxy" && !body.proxy_id) {
      return toast("先选择要绑定的代理（当前是直连）");
    }
  }
  if (action === "delete" && !confirm(`确认删除选中的 ${ids.length} 个账号？`)) return;
  try {
    toast("批量处理中…");
    const d = await api("/api/accounts/batch", { method: "POST", body: JSON.stringify(body) });
    toast(`完成 ${d.success || 0}/${d.total || 0}` + (d.failed ? ` · 失败 ${d.failed}` : ""));
    _selAccounts.clear();
    await refreshAll(action === "refresh_quota");
  } catch (e) {
    toast(e.message);
  }
}

async function disableAccount(id) {
  try {
    await api(`/api/accounts/${id}/disable`, { method: "PUT", body: "{}" });
    toast("已禁用");
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
}

async function enableAccount(id) {
  try {
    await api(`/api/accounts/${id}/enable`, { method: "PUT", body: "{}" });
    toast("已启用并刷新额度");
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
}

async function refreshAllQuotas() {
  try {
    toast("正在批量刷新额度…");
    const d = await api("/api/accounts/quota/refresh-all", { method: "POST", body: "{}" });
    const ok = (d.items || []).filter((x) => x.ok).length;
    toast(`额度刷新完成 ${ok}/${d.total || 0}`);
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
}

async function loadModels() {
  // 管理端双渠道目录（含可用账号数与福利标记）
  let d = null;
  try {
    d = await api("/api/models/catalog");
  } catch (e) {
    $("models-hint").textContent = "目录加载失败: " + e.message;
    return;
  }
  const intl = d.intl || [];
  const cn = d.cn || [];

  $("models-hint").textContent =
    `国际版 ${d.intl_total || 0} 个模型 · 国内码道 ${d.cn_total || 0} 个模型（${d.cn_accounts || 0} 个 CN 账号检出）` +
    (d.cn_enabled ? "" : " · CN 渠道已关闭");

  $("intl-model-count").textContent = `(${intl.length})`;
  $("intl-models-body").innerHTML = intl.length
    ? intl.map((m) => `<tr>
        <td><b>${esc(m.id)}</b></td>
        <td>${m.accounts > 0 ? `<span class="badge bg">${m.accounts}</span>` : `<span class="muted">—</span>`}</td>
        <td>${m.fallback ? '<span class="badge bb">内置兜底</span>' : '<span class="badge bg">上游目录</span>'}</td>
      </tr>`).join("")
    : `<tr><td colspan="3" class="empty">尚未同步国际模型目录（点右上「同步」或等上游探测）</td></tr>`;

  $("cn-model-count").textContent = `(${cn.length})`;
  const cnNote = $("cn-models-note");
  if (!d.cn_enabled) {
    cnNote.innerHTML = `CN 渠道已关闭（<code>CN_ENABLED=false</code>），国内模型不参与调度。`;
  } else if ((d.cn_accounts || 0) === 0) {
    cnNote.innerHTML = `还没有国内码道账号。先用 <code>cnlogin</code> 登录，再到「国内码道」页一键导入 auths 目录。`;
  } else if (cn.length === 0) {
    cnNote.innerHTML = `CN 账号已就位，模型目录将在首轮看护或首次 <code>cn:</code> 请求后自动拉取。`;
  } else {
    cnNote.innerHTML = `每个 CN 账号首轮看护或首次请求后会拉取自己的可用模型目录。请求时用 <code>cn:模型ID</code> 走国内渠道。`;
  }
  $("cn-models-body").innerHTML = cn.length
    ? cn.map((m) => `<tr>
        <td><b>${esc(m.id)}</b></td>
        <td><code class="muted">${esc(m.prefixed)}</code></td>
        <td>${m.accounts > 0 ? `<span class="badge bg">${m.accounts}</span>` : "—"}</td>
        <td>${m.benefit ? '<span class="badge bo">限时福利</span>' : '<span class="badge bb">常规</span>'}</td>
      </tr>`).join("")
    : `<tr><td colspan="4" class="empty">暂无国内模型目录</td></tr>`;

  // 原始 /v1/models 以 pill 展示（客户端真实拿到的合流结果）
  try {
    const res = await fetch(BASE + "/v1/models", { credentials: "same-origin" });
    const raw = await res.json().catch(() => ({}));
    const list = (raw.data || []).map((m) => m.id || "");
    $("raw-models").innerHTML = list.length
      ? list.map((id) => `<span class="usage-pill"><b>${esc(id)}</b></span>`).join("")
      : `<div class="empty">${esc(raw.error || "无返回")}</div>`;
  } catch (e) {
    $("raw-models").innerHTML = `<div class="empty">${esc(e.message)}</div>`;
  }
}

async function fillOAuthAccounts() {
  // CLI 授权是国际版专属流程（IAM AK/SK）；国内码道走 cnlogin，不在此列
  const list = await api("/api/accounts?channel=intl");
  const sel = $("oauth-account");
  window._oauthAccounts = list || [];
  sel.innerHTML = list.map((a) => `<option value="${esc(a.id)}">${esc(a.email)}</option>`).join("");
  if (!list.length) sel.innerHTML = `<option value="">暂无账号</option>`;
  updateOAuthLoginLine();
}

function updateOAuthLoginLine() {
  const el = $("oauth-login-line");
  if (!el) return;
  const id = $("oauth-account")?.value;
  const acc = (window._oauthAccounts || []).find((a) => a.id === id);
  if (!acc || !acc.email) {
    el.textContent = "选中账号后显示 邮箱----密码";
    el.classList.add("empty");
    return;
  }
  const line = `${acc.email}----`;
  el.textContent = line + "（密码仅在授权请求中使用）";
  el.classList.remove("empty");
  el.dataset.line = line;
}

async function refreshToken(id) {
  try {
    const a = await api(`/api/accounts/${id}/token/refresh`, { method: "POST" });
    toast("刷新成功 " + (a.email || id));
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
}

async function fetchQuota(id) {
  try {
    const d = await api(`/api/accounts/${id}/quota`);
    // CN 渠道没有国际额度体系，后端返回说明文案
    if (d.channel === "cn") {
      toast(d.message || "国内码道渠道无国际额度");
      return;
    }
    const q = d.quota || {};
    const used = Number(q.used_tokens || 0);
    const total = Number(q.package_tokens || 0);
    const remain = remainOf(q);
    const ri = resetInfo(q);
    toast(`${d.email || id} · 已用 ${fmtTokensExact(used)} · 总量 ${fmtTokensExact(total)} · 剩余 ${fmtTokensExact(remain)}` + (ri ? ` · ${ri.txt}` : ""));
    if ($("oauth-output")) $("oauth-output").textContent = JSON.stringify(d, null, 2);
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
}

async function deleteAccount(id) {
  if (!confirm("删除该账号?")) return;
  try {
    await api(`/api/accounts/${id}`, { method: "DELETE" });
    toast("已删除");
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
}

async function startOAuth(accountId) {
  try {
    const q = accountId ? `?account_id=${encodeURIComponent(accountId)}` : "";
    const d = await api("/api/oauth/start" + q);
    $("oauth-output").textContent = JSON.stringify(d, null, 2);
    const line = d.login_line || `${d.login_email || ""}----`;
    const lineEl = $("oauth-login-line");
    if (lineEl && line && line !== "----") {
      lineEl.textContent = line;
      lineEl.dataset.line = line;
      lineEl.classList.remove("empty");
    }
    const st = $("oauth-status");
    if (st) st.textContent = "等待浏览器授权… ticket=" + (d.ticket_id || "");
    if (d.browser_opened || d.browser_private) {
      toast("已用无痕窗口打开授权页");
    } else if (d.authorize_url) {
      openExternal(d.authorize_url);
      toast("请在打开的窗口完成授权");
    }
    if (d.ticket_id) pollOAuthStatus(d.ticket_id);
  } catch (e) {
    toast(e.message);
  }
}

async function pollOAuthStatus(ticketId) {
  const st = $("oauth-status");
  for (let i = 0; i < 150; i++) {
    await new Promise((r) => setTimeout(r, 2000));
    try {
      const d = await api("/api/oauth/status?ticket_id=" + encodeURIComponent(ticketId));
      if (st) st.textContent = "状态: " + d.status + (d.error ? " · " + d.error : "");
      if (d.status === "success") {
        toast("OAuth 成功");
        refreshAll();
        return;
      }
      if (d.status === "failed") {
        toast("OAuth 失败: " + (d.error || ""));
        return;
      }
    } catch (e) {
      if (st) st.textContent = e.message;
    }
  }
  if (st) st.textContent = "超时，请重试全新 OAuth";
}

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("login-error").textContent = "";
  try {
    clearSessionCookies();
    await api("/api/login", {
      method: "POST",
      body: JSON.stringify({ password: $("password").value }),
    });
    await loadCSRF();
    $("password").value = "";
    await init();
  } catch (err) {
    $("login-error").textContent = err.message === "invalid password" ? "密码错误" : err.message;
  }
});

$("logout-btn").addEventListener("click", async () => {
  try {
    await api("/api/logout", { method: "POST" });
  } catch (e) {
    // 退出失败就明说，不能切到登录页假装成功——服务端会话还活着。
    toast("退出失败：" + e.message);
    return;
  }
  stopAutoRefresh();
  showLogin();
  location.reload();
});

$("add-account").addEventListener("click", async () => {
  const email = $("new-email").value.trim();
  const password = $("new-password").value;
  const country = $("new-country").value.trim() || "SG";
  if (!email) return toast("邮箱必填");
  try {
    await api("/api/accounts", {
      method: "POST",
      body: JSON.stringify({ email, password, country_code: country }),
    });
    $("new-email").value = "";
    $("new-password").value = "";
    toast("账号已添加");
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
});

$("import-file-btn")?.addEventListener("click", () => $("import-file")?.click());

$("import-file")?.addEventListener("change", async (e) => {
  const f = e.target.files && e.target.files[0];
  const nameEl = $("import-file-name");
  if (nameEl) {
    if (f) {
      nameEl.textContent = f.name;
      nameEl.classList.add("has-file");
      nameEl.title = f.name;
    } else {
      nameEl.textContent = "未选择文件";
      nameEl.classList.remove("has-file");
      nameEl.title = "";
    }
  }
  if (!f) return;
  try {
    $("import-text").value = await f.text();
    toast("已载入 " + f.name);
  } catch (err) {
    toast(err.message || "读文件失败");
  }
});

$("import-accounts")?.addEventListener("click", async () => {
  const text = $("import-text")?.value?.trim() || "";
  if (!text) return toast("先粘贴或选择 txt");
  const refresh_quota = !!$("import-refresh-quota")?.checked;
  const country = $("new-country")?.value?.trim() || "SG";
  try {
    const d = await api("/api/accounts/import", {
      method: "POST",
      body: JSON.stringify({ text, country_code: country, refresh_quota }),
    });
    const el = $("import-result");
    if (el) {
      el.textContent = JSON.stringify(d, null, 2);
      el.classList.remove("empty");
    }
    toast(`导入完成：新建 ${d.created || 0} · 重复跳过 ${d.duplicates || 0} · 更新 ${d.updated || 0} · 失败 ${d.parse_fail || 0}`);
    refreshAll(true);
  } catch (e) {
    toast(e.message);
  }
});

$("oauth-start").addEventListener("click", async () => {
  await startOAuth($("oauth-account").value);
});

$("oauth-account")?.addEventListener("change", () => {
  updateOAuthLoginLine();
});

$("iam-save")?.addEventListener("click", async () => {
  const id = $("oauth-account")?.value;
  const ak = $("iam-ak")?.value?.trim();
  const sk = $("iam-sk")?.value?.trim();
  if (!id) return toast("先选账号");
  if (!ak || !sk) return toast("填写 AK 和 SK");
  try {
    const d = await api(`/api/accounts/${id}/iam-aksk`, {
      method: "POST",
      body: JSON.stringify({ access_key_id: ak, secret_access_key: sk }),
    });
    const sample = $("cli-env-sample");
    if (sample) {
      sample.textContent = `PowerShell:\n$env:CODEARTS_CLI_AK="${ak}"\n$env:CODEARTS_CLI_SK="${sk}"`;
      sample.classList.remove("empty");
    }
    $("oauth-output").textContent = JSON.stringify(d, null, 2);
    toast(d.models_error || d.quota_error ? "已写入，但拉取有报错" : "永久 AK/SK 已写入");
    refreshAll();
  } catch (e) {
    toast(e.message);
  }
});

$("oauth-copy-login")?.addEventListener("click", async () => {
  const line = $("oauth-login-line")?.dataset?.line || $("oauth-login-line")?.textContent || "";
  if (!line || line.includes("选中账号")) return toast("先选账号");
  try {
    await navigator.clipboard.writeText(line);
    toast("已复制 " + line);
  } catch {
    toast(line);
  }
});

document.querySelectorAll(".nav a").forEach((a) =>
  a.addEventListener("click", () => goPage(a.dataset.page))
);

$("acct-filter")?.addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-filter]");
  if (!btn) return;
  _acctFilter = btn.dataset.filter || "all";
  $("acct-filter").querySelectorAll("button").forEach((b) => b.classList.toggle("active", b === btn));
  renderAccounts();
});

$("acct-search")?.addEventListener("input", (e) => {
  _acctSearch = e.target.value || "";
  renderAccounts();
});

$("refresh-all-quota")?.addEventListener("click", () => refreshAllQuotas());

function proxyForm() {
  return {
    name: $("px-name")?.value?.trim() || "",
    type: $("px-type")?.value || "socks5h",
    host: $("px-host")?.value?.trim() || "",
    port: Number($("px-port")?.value || 0),
    username: $("px-user")?.value?.trim() || "",
    password: $("px-pass")?.value || "",
    enabled: true,
  };
}

function parseProxyLine(line) {
  line = (line || "").trim();
  if (!line) return null;
  let type = "socks5h",
    user = "",
    pass = "",
    host = "",
    port = 0;
  const m = line.match(/^(?:(socks5h?|https?):\/\/)?(?:([^:@\/]+):([^@\/]*)@)?([^:\/\s]+):(\d+)\s*$/i);
  if (!m) return null;
  if (m[1]) type = m[1].toLowerCase();
  user = m[2] || "";
  pass = m[3] || "";
  host = m[4];
  port = Number(m[5]);
  return { type, username: user, password: pass, host, port };
}

function proxySortValue(key, p) {
  const cooling = p.cooldown_until && new Date(p.cooldown_until) > new Date();
  switch (key) {
    case "name":
      return (p.name || p.id || "").toLowerCase();
    case "type":
      return p.type || "";
    case "address":
      return p.address || `${p.host || ""}:${p.port || ""}`;
    case "status":
      return !p.enabled ? 0 : cooling ? 1 : 2;
    case "success":
      return Number(p.success_count || 0) - Number(p.fail_count || 0);
    default:
      return "";
  }
}

function renderProxiesTable() {
  const items = window._proxies || [];
  const tb = document.querySelector("#proxies-table tbody");
  if (!tb) return;
  const keep = new Set(items.map((p) => p.id));
  for (const id of [..._selProxies]) {
    if (!keep.has(id)) _selProxies.delete(id);
  }
  if (!items.length) {
    tb.innerHTML = `<tr><td colspan="8" class="empty">暂无代理 · 添加后账号可绑定 sticky</td></tr>`;
    _selProxies.clear();
    syncSelCount("proxy");
    return;
  }
  const sorted = applySort("proxies-table", items, proxySortValue);
  tb.innerHTML = sorted
    .map((p) => {
      const cooling = p.cooldown_until && new Date(p.cooldown_until) > new Date();
      const stBadge = !p.enabled
        ? '<span class="badge br">关</span>'
        : cooling
          ? '<span class="badge bo">冷却</span>'
          : '<span class="badge bg">开</span>';
      const err = p.last_error ? esc(String(p.last_error).slice(0, 48)) : "—";
      const checked = _selProxies.has(p.id) ? "checked" : "";
      return `<tr>
        <td class="col-check"><input type="checkbox" class="row-check" data-id="${esc(p.id)}" ${checked}></td>
        <td><b>${esc(p.name || p.id)}</b><div class="muted" style="font-size:11px">${esc(p.id)}</div></td>
        <td>${esc(p.type)}</td>
        <td>${esc(p.address || p.host + ":" + p.port)}</td>
        <td>${stBadge}</td>
        <td>${p.success_count || 0} / ${p.fail_count || 0}</td>
        <td class="muted">${err}</td>
        <td>
          <button class="btn sm" onclick="testProxy('${esc(p.id)}')">测试</button>
          <button class="btn sm" onclick="toggleProxy('${esc(p.id)}', ${p.enabled ? "false" : "true"})">${p.enabled ? "禁用" : "启用"}</button>
          <button class="btn sm danger" onclick="deleteProxy('${esc(p.id)}')">删除</button>
        </td>
      </tr>`;
    })
    .join("");
  syncSelCount("proxy");
}

async function loadProxies() {
  try {
    const d = await api("/api/proxies");
    const items = d.items || [];
    window._proxies = items;
    if ($("proxies-total")) $("proxies-total").textContent = `共 ${items.length} 个`;
    const st = d.stats || {};
    if ($("proxy-stats")) {
      $("proxy-stats").textContent =
        `可用 ${st.usable ?? 0} · 启用 ${st.enabled ?? 0} · 冷却 ${st.cooling ?? 0} · 禁用 ${st.disabled ?? 0}`;
    }
    if ($("proxy-strategy") && d.strategy) $("proxy-strategy").value = d.strategy;
    renderProxiesTable();
    fillAcctBatchProxySelect();
    await fillProxyBindSelects();
  } catch (e) {
    toast(e.message);
  }
}

async function runProxyBatch(action) {
  const ids = [..._selProxies];
  if (!ids.length) return toast("先勾选代理");
  if (action === "delete" && !confirm(`确认删除选中的 ${ids.length} 个代理？`)) return;
  try {
    toast("批量处理中…");
    const d = await api("/api/proxies/batch", { method: "POST", body: JSON.stringify({ action, ids }) });
    toast(`完成 ${d.success || 0}/${d.total || 0}` + (d.failed ? ` · 失败 ${d.failed}` : ""));
    _selProxies.clear();
    await loadProxies();
    if (action === "delete") await loadAccounts();
  } catch (e) {
    toast(e.message);
  }
}

async function fillProxyBindSelects() {
  const sa = $("bind-account");
  const sp = $("bind-proxy");
  if (!sa || !sp) return;
  let accounts = [];
  try {
    accounts = await api("/api/accounts");
  } catch (e) {
    // 失败 ≠ 没有账号：直接显示"暂无账号"会让用户以为账号池被清空。
    sa.innerHTML = `<option value="">加载失败：${esc(e.message)}</option>`;
    return;
  }
  sa.innerHTML = (accounts || [])
    .map((a) => {
      const tag = a.proxy_id ? ` · ${a.proxy_id.slice(0, 10)}` : "";
      return `<option value="${esc(a.id)}">${esc(a.email)}${esc(tag)}</option>`;
    })
    .join("");
  if (!accounts?.length) sa.innerHTML = `<option value="">暂无账号</option>`;
  const cur = accounts?.find((a) => a.id === sa.value);
  sp.innerHTML = proxyOptionsHTML(cur?.proxy_id || "");
  sp.value = cur?.proxy_id || "";
  sa.onchange = () => {
    const a = accounts.find((x) => x.id === sa.value);
    sp.value = a?.proxy_id || "";
  };
}

async function testProxy(id) {
  try {
    const d = await api("/api/proxies/test", {
      method: "POST",
      body: JSON.stringify({ proxy_id: id }),
    });
    const el = $("px-test-out");
    if (el) {
      el.textContent = JSON.stringify(d, null, 2);
      el.classList.remove("empty");
    }
    toast(d.ok ? `连通 OK ${d.latency_ms || d.ms || ""}ms` : `失败: ${d.message || d.error || ""}`);
    loadProxies();
  } catch (e) {
    toast(e.message);
  }
}

async function toggleProxy(id, enabled) {
  try {
    await api(`/api/proxies/${id}/enable`, { method: "POST", body: JSON.stringify({ enabled }) });
    toast(enabled ? "已启用" : "已禁用");
    loadProxies();
  } catch (e) {
    toast(e.message);
  }
}

async function deleteProxy(id) {
  if (!confirm("删除该代理？已绑定账号会清除绑定。")) return;
  try {
    await api(`/api/proxies/${id}`, { method: "DELETE" });
    toast("已删除");
    loadProxies();
    loadAccounts();
  } catch (e) {
    toast(e.message);
  }
}

$("px-parse-line")?.addEventListener("click", () => {
  const p = parseProxyLine($("px-line")?.value);
  if (!p) return toast("无法解析，示例 socks5h://u:p@host:1080");
  if ($("px-type")) $("px-type").value = p.type;
  if ($("px-host")) $("px-host").value = p.host;
  if ($("px-port")) $("px-port").value = String(p.port);
  if ($("px-user")) $("px-user").value = p.username || "";
  if ($("px-pass")) $("px-pass").value = p.password || "";
  toast("已填入");
});

$("px-test-inline")?.addEventListener("click", async () => {
  const proxy = proxyForm();
  if (!proxy.host || !proxy.port) return toast("填写 host/port");
  try {
    const d = await api("/api/proxies/test", {
      method: "POST",
      body: JSON.stringify({ proxy }),
    });
    const el = $("px-test-out");
    if (el) {
      el.textContent = JSON.stringify(d, null, 2);
      el.classList.remove("empty");
    }
    toast(d.ok ? "连通 OK" : `失败: ${d.message || ""}`);
  } catch (e) {
    toast(e.message);
  }
});

$("px-add")?.addEventListener("click", async () => {
  const proxy = proxyForm();
  if (!proxy.host || !proxy.port) return toast("填写 host/port");
  try {
    await api("/api/proxies", { method: "POST", body: JSON.stringify(proxy) });
    toast("已添加");
    ["px-name", "px-host", "px-port", "px-user", "px-pass", "px-line"].forEach((id) => {
      if ($(id)) $(id).value = "";
    });
    loadProxies();
  } catch (e) {
    toast(e.message);
  }
});

$("proxy-strategy-save")?.addEventListener("click", async () => {
  try {
    const strategy = $("proxy-strategy")?.value || "round_robin";
    await api("/api/proxies/strategy", { method: "PUT", body: JSON.stringify({ strategy }) });
    toast("策略已保存: " + strategy);
    loadProxies();
  } catch (e) {
    toast(e.message);
  }
});

$("bind-proxy-save")?.addEventListener("click", async () => {
  const id = $("bind-account")?.value;
  const proxy_id = $("bind-proxy")?.value || "";
  if (!id) return toast("先选账号");
  try {
    await api(`/api/accounts/${id}/proxy`, { method: "PUT", body: JSON.stringify({ proxy_id }) });
    toast(proxy_id ? "已绑定代理" : "已设为直连");
    loadProxies();
    loadAccounts();
  } catch (e) {
    toast(e.message);
  }
});

$("bind-proxy-clear")?.addEventListener("click", async () => {
  const id = $("bind-account")?.value;
  if (!id) return toast("先选账号");
  try {
    await api(`/api/accounts/${id}/proxy`, { method: "PUT", body: JSON.stringify({ proxy_id: "" }) });
    toast("已设为直连");
    loadProxies();
    loadAccounts();
  } catch (e) {
    toast(e.message);
  }
});

function keyScopeIsPick() {
  return document.querySelector('input[name="key-scope"]:checked')?.value === "pick";
}

async function fillKeyAccountPicker(selected, gate) {
  const box = $("key-acct-list");
  if (!box) return;
  const sel = new Set(selected || []);
  // gate 必须参与渲染：事后 querySelectorAll 补勾会命中重绘前的空 DOM，
  // 用户看到的是"按钮点了没反应"，实际建出的是全量 Key。
  const gateMatch = (g) => {
    if (!gate || gate === "*") return true;
    if (gate === "ready") return g === "ready" || g === "quota_exhausted";
    return g === gate;
  };
  // API Key 可绑定全池（国际 + 国内），单独拉全量而不是复用国际版账户池缓存
  let items = _accountsCache || [];
  let loadErr = "";
  try {
    items = await api("/api/accounts");
  } catch (e) {
    loadErr = e.message || "网络错误";
  }
  box.hidden = !keyScopeIsPick();
  if (loadErr) {
    // 后端 500 时显示"暂无账号"会让用户以为账号被清空。
    box.innerHTML = `<div class="empty">账号列表加载失败：${esc(loadErr)}（刷新重试）</div>`;
    return;
  }
  if (!items.length) {
    box.innerHTML = `<div class="empty">还没有账号，先去账户池 / 国内码道导入</div>`;
    return;
  }
  box.innerHTML = items.map((a) => {
    const g = a.call_gate || "";
    const on = sel.size ? sel.has(a.id) : gateMatch(g);
    const checked = on ? "checked" : "";
    const cn = a.channel === "cn" ? ' <span class="badge cn">CN</span>' : "";
    return `<label class="pick-row" data-gate="${esc(g)}">
      <input type="checkbox" value="${esc(a.id)}" ${checked}>
      <span>${esc(a.email || a.id)}${cn}</span>
      <span>${badge(g)}</span>
    </label>`;
  }).join("");
}

function collectPickedAccountIDs(root) {
  const scope = root || document;
  const sel = scope.classList?.contains("acct-pick-list")
    ? "input[type=checkbox]:checked"
    : ".acct-pick-list input[type=checkbox]:checked";
  return [...scope.querySelectorAll(sel)].map((el) => el.value).filter(Boolean);
}

async function setPickerByGate(gate) {
  document.querySelectorAll('input[name="key-scope"]').forEach((el) => {
    el.checked = el.value === "pick";
  });
  await fillKeyAccountPicker(null, gate);
}

function keyAllowLabel(k) {
  const ids = k.account_ids || [];
  if (!ids.length) return "全部账号";
  const emails = ids.map((id) => {
    const a = (_accountsCache || []).find((x) => x.id === id);
    return a?.email || id;
  });
  if (emails.length <= 2) return emails.join("、");
  return `${emails.length} 个：${emails.slice(0, 2).join("、")}…`;
}

document.querySelectorAll('input[name="key-scope"]').forEach((el) => {
  el.addEventListener("change", () => fillKeyAccountPicker());
});
$("key-pick-exhausted")?.addEventListener("click", () => setPickerByGate("quota_exhausted"));
$("key-pick-ready")?.addEventListener("click", () => setPickerByGate("ready"));
$("key-pick-none")?.addEventListener("click", () => {
  document.querySelectorAll('input[name="key-scope"]').forEach((el) => {
    el.checked = el.value === "pick";
  });
  fillKeyAccountPicker();
});

let _apikeysCache = [];

function apikeySortValue(key, k) {
  switch (key) {
    case "name":
      return (k.name || "").toLowerCase();
    case "status":
      return k.enabled ? 1 : 0;
    case "in_flight":
      return Number(k.in_flight || 0);
    case "usage":
      return Number(k.prompt_tokens || 0) + Number(k.completion_tokens || 0);
    case "created":
      return k.created_at ? new Date(k.created_at).getTime() : 0;
    case "last_used":
      return k.last_used_at ? new Date(k.last_used_at).getTime() : 0;
    default:
      return "";
  }
}

function renderAPIKeysTable() {
  const items = _apikeysCache || [];
  const tb = $("apikeys-table")?.querySelector("tbody");
  if (!tb) return;
  const keep = new Set(items.map((k) => k.id));
  for (const id of [..._selAPIKeys]) {
    if (!keep.has(id)) _selAPIKeys.delete(id);
  }
  if (!items.length) {
    tb.innerHTML = `<tr><td colspan="10" class="empty">还没有 API Key，先创建一个</td></tr>`;
    _selAPIKeys.clear();
    syncSelCount("apikey");
    return;
  }
  const sorted = applySort("apikeys-table", items, apikeySortValue);
  tb.innerHTML = sorted.map((k) => {
    const checked = _selAPIKeys.has(k.id) ? "checked" : "";
    return `<tr>
    <td class="col-check"><input type="checkbox" class="row-check" data-id="${esc(k.id)}" ${checked}></td>
    <td>${esc(k.name)}</td>
    <td><code>${esc(k.prefix)}</code></td>
    <td>${k.enabled ? badge("ready") : badge("disabled")}</td>
    <td title="${esc((k.account_ids || []).join(", "))}">${esc(keyAllowLabel(k))}</td>
    <td class="nowrap">${esc(k.in_flight ?? 0)}</td>
    <td class="col-quota">${keyUsageCell(k)}</td>
    <td>${fmtTime(k.created_at)}</td>
    <td>${fmtTime(k.last_used_at)}</td>
    <td class="ops">
      <button class="btn sm" onclick="editAPIKeyAccounts('${esc(k.id)}')">账号</button>
      <button class="btn sm" onclick="toggleAPIKey('${esc(k.id)}', ${k.enabled ? "false" : "true"})">${k.enabled ? "禁用" : "启用"}</button>
      <button class="btn sm danger" onclick="deleteAPIKey('${esc(k.id)}')">删除</button>
    </td>
  </tr>`;
  }).join("");
  syncSelCount("apikey");
}

async function loadAPIKeys() {
  try {
    const d = await api("/api/apikeys");
    _apikeysCache = d.items || [];
    if ($("apikeys-meta")) {
      $("apikeys-meta").textContent = `共 ${_apikeysCache.length} 个 · 启用 ${d.enabled_count || 0}` +
        (d.auth_required ? " · /v1 需要鉴权" : " · /v1 当前开放");
    }
    renderAPIKeysTable();
    if ($("apikey-sample")) {
      $("apikey-sample").textContent =
        `curl ${location.origin}${BASE}/v1/models \\\n` +
        `  -H "Authorization: Bearer sk-hw-你的密钥"\n\n` +
        `curl ${location.origin}${BASE}/v1/chat/completions \\\n` +
        `  -H "Authorization: Bearer sk-hw-你的密钥" \\\n` +
        `  -H "Content-Type: application/json" \\\n` +
        `  -d '{"model":"DeepSeek-V3","messages":[{"role":"user","content":"hi"}]}'`;
    }
  } catch (e) {
    toast(e.message);
  }
}

async function runAPIKeyBatch(action) {
  const ids = [..._selAPIKeys];
  if (!ids.length) return toast("先勾选 API Key");
  if (action === "delete" && !confirm(`确认删除选中的 ${ids.length} 个 Key？`)) return;
  try {
    toast("批量处理中…");
    const d = await api("/api/apikeys/batch", { method: "POST", body: JSON.stringify({ action, ids }) });
    toast(`完成 ${d.success || 0}/${d.total || 0}` + (d.failed ? ` · 失败 ${d.failed}` : ""));
    _selAPIKeys.clear();
    await loadAPIKeys();
  } catch (e) {
    toast(e.message);
  }
}

async function toggleAPIKey(id, enabled) {
  try {
    await api(`/api/apikeys/${id}`, { method: "PUT", body: JSON.stringify({ enabled }) });
    toast(enabled ? "已启用" : "已禁用");
    loadAPIKeys();
  } catch (e) {
    toast(e.message);
  }
}

async function deleteAPIKey(id) {
  if (!confirm("确定删除这个 API Key？")) return;
  try {
    await api(`/api/apikeys/${id}`, { method: "DELETE" });
    toast("已删除");
    loadAPIKeys();
  } catch (e) {
    toast(e.message);
  }
}

async function editAPIKeyAccounts(id) {
  const box = $("apikey-edit-box");
  if (!box) return;
  // 必须用全池：_accountsCache 只含国际版。拿它渲染编辑弹层再保存，
  // PUT 提交的是"仅国际版"的 id 集合，该 Key 已有的国内码道绑定会被静默删掉。
  let items = [];
  try {
    items = await api("/api/accounts");
  } catch (e) {
    return toast("加载账号失败：" + e.message);
  }
  const keys = (await api("/api/apikeys")).items || [];
  const k = keys.find((x) => x.id === id);
  if (!k) return toast("key 不存在");
  const selected = new Set(k.account_ids || []);
  const missing = (k.account_ids || []).filter((aid) => !items.some((a) => a.id === aid));
  box.style.display = "block";
  box.innerHTML =
    `<div class="muted" style="margin-bottom:8px">编辑「${esc(k.name)}」可用账号 · 一个都不勾 = 全部账号</div>` +
    items.map((a) => {
      const checked = selected.has(a.id) ? "checked" : "";
      return `<label class="pick-row"><input type="checkbox" value="${esc(a.id)}" ${checked}><span>${esc(a.email || a.id)}</span><span>${badge(a.call_gate)}</span></label>`;
    }).join("") +
    `<div class="row" style="margin-top:10px">
      <button class="btn primary" id="apikey-edit-save">保存账号范围</button>
      <button class="btn" id="apikey-edit-cancel">取消</button>
    </div>`;
  $("apikey-edit-save")?.addEventListener("click", async () => {
    const ids = collectPickedAccountIDs(box);
    if (!ids.length && missing.length &&
        !confirm(`该 Key 原有 ${missing.length} 个绑定账号已不在账号池，保存后会清空为"全部账号"。继续？`)) return;
    try {
      await api(`/api/apikeys/${id}`, { method: "PUT", body: JSON.stringify({ account_ids: ids }) });
      toast(ids.length ? `已限定 ${ids.length} 个账号` : "已改为全部账号");
      box.style.display = "none";
      loadAPIKeys();
    } catch (e) {
      toast(e.message);
    }
  });
  $("apikey-edit-cancel")?.addEventListener("click", () => {
    box.style.display = "none";
  });
};

$("apikey-create")?.addEventListener("click", async () => {
  const name = $("apikey-name")?.value?.trim() || "default";
  const account_ids = keyScopeIsPick() ? collectPickedAccountIDs($("key-acct-list")) : [];
  if (keyScopeIsPick() && !account_ids.length) return toast("指定账号模式下至少勾选一个号");
  try {
    const d = await api("/api/apikeys", { method: "POST", body: JSON.stringify({ name, account_ids }) });
    const secret = d.secret || "";
    const box = $("apikey-secret-box");
    if (box) {
      box.style.display = "block";
      box.classList.remove("empty");
      box.textContent = `请立即复制保存（只显示一次）：\n${secret}`;
    }
    try {
      await navigator.clipboard.writeText(secret);
      toast("已创建，密钥已复制到剪贴板");
    } catch {
      toast("已创建，请手动复制密钥");
    }
    if ($("apikey-name")) $("apikey-name").value = "";
    loadAPIKeys();
  } catch (e) {
    toast(e.message);
  }
});

// —— 批量勾选 / 操作 ——
document.addEventListener("change", (e) => {
  const t = e.target;
  if (!(t instanceof HTMLInputElement) || t.type !== "checkbox") return;
  if (t.id === "acct-check-all") {
    document.querySelectorAll("#accounts-table .row-check").forEach((box) => {
      box.checked = t.checked;
      const id = box.getAttribute("data-id");
      if (!id) return;
      if (t.checked) _selAccounts.add(id);
      else _selAccounts.delete(id);
    });
    syncSelCount("acct");
    return;
  }
  if (t.id === "proxy-check-all") {
    document.querySelectorAll("#proxies-table .row-check").forEach((box) => {
      box.checked = t.checked;
      const id = box.getAttribute("data-id");
      if (!id) return;
      if (t.checked) _selProxies.add(id);
      else _selProxies.delete(id);
    });
    syncSelCount("proxy");
    return;
  }
  if (t.id === "apikey-check-all") {
    document.querySelectorAll("#apikeys-table .row-check").forEach((box) => {
      box.checked = t.checked;
      const id = box.getAttribute("data-id");
      if (!id) return;
      if (t.checked) _selAPIKeys.add(id);
      else _selAPIKeys.delete(id);
    });
    syncSelCount("apikey");
    return;
  }
  if (t.classList.contains("row-check")) {
    const id = t.getAttribute("data-id");
    if (!id) return;
    const table = t.closest("table");
    if (table?.id === "accounts-table") {
      if (t.checked) _selAccounts.add(id);
      else _selAccounts.delete(id);
      syncSelCount("acct");
    } else if (table?.id === "proxies-table") {
      if (t.checked) _selProxies.add(id);
      else _selProxies.delete(id);
      syncSelCount("proxy");
    } else if (table?.id === "apikeys-table") {
      if (t.checked) _selAPIKeys.add(id);
      else _selAPIKeys.delete(id);
      syncSelCount("apikey");
    }
  }
});

document.querySelectorAll("[data-batch-acct]").forEach((btn) => {
  btn.addEventListener("click", () => runAccountBatch(btn.getAttribute("data-batch-acct")));
});
document.querySelectorAll("[data-batch-proxy]").forEach((btn) => {
  btn.addEventListener("click", () => runProxyBatch(btn.getAttribute("data-batch-proxy")));
});
document.querySelectorAll("[data-batch-apikey]").forEach((btn) => {
  btn.addEventListener("click", () => runAPIKeyBatch(btn.getAttribute("data-batch-apikey")));
});

$("px-import-btn")?.addEventListener("click", async () => {
  const text = $("px-import-text")?.value || "";
  if (!text.trim()) return toast("先粘贴代理列表");
  try {
    const d = await api("/api/proxies/import", { method: "POST", body: JSON.stringify({ text }) });
    const msg = `导入成功 ${d.success || 0}/${d.total || 0}` + (d.failed ? ` · 失败 ${d.failed}` : "");
    if ($("px-import-result")) $("px-import-result").textContent = msg;
    toast(msg);
    if ($("px-import-text")) $("px-import-text").value = "";
    loadProxies();
  } catch (e) {
    toast(e.message);
  }
});

let _cnCache = null;

function cnTokenBadge(v) {
  const map = {
    ok: ["bg", "正常"],
    due: ["bo", "将到期"],
    expired: ["br", "已过期"],
    none: ["bb", "无"],
  };
  const [cls, label] = map[v.token_state] || ["bb", v.token_state || "—"];
  const exp = v.token_expires ? `<div class="muted" style="font-size:11px">${esc(fmtTime(v.token_expires))}</div>` : "";
  return `<span class="badge ${esc(cls)}">${esc(label)}</span>${exp}`;
}

async function loadCN() {
  let d;
  try {
    d = await api("/api/cn/dashboard");
  } catch (e) {
    toast("CN 状态加载失败: " + e.message);
    return;
  }
  _cnCache = d;

  $("cn-off-tip").style.display = d.enabled ? "none" : "block";
  $("cn-sub").textContent =
    `账号 ${d.accounts || 0} · 可调用 ${d.callable || 0} · 待重登 ${d.needs_relogin || 0}` +
    ` · Token 临期 ${d.token_due || 0} · 今日签到 ${d.checkin_today || 0}` +
    (d.last_active ? ` · 最近活动 ${fmtTime(d.last_active)}` : "");

  const cards = [
    { icon: "🀄", bg: "rgba(210,153,34,.12)", val: d.enabled ? "已启用" : "已关闭", key: "CN 渠道", trend: `auths ${d.auth_file_count || 0} 个凭证` },
    { icon: "👥", bg: "rgba(78,168,255,.12)", val: d.accounts || 0, key: "CN 账号", trend: `可调用 ${d.callable || 0}` },
    { icon: "🔑", bg: "rgba(248,81,73,.12)", val: d.needs_relogin || 0, key: "待重新登录", trend: `Token 临期 ${d.token_due || 0}` },
    { icon: "🎁", bg: "rgba(63,185,80,.12)", val: `${d.checkin_today || 0}/${d.accounts || 0}`, key: "今日已签到", trend: d.today || "" },
  ];
  $("cn-stats").innerHTML = cards
    .map((c) =>
      `<div class="stat"><div class="icon" style="background:${c.bg}">${c.icon}</div>` +
      `<div class="val">${esc(c.val)}</div><div class="key">${esc(c.key)}</div>` +
      `<div class="trend">${esc(c.trend || "")}</div></div>`)
    .join("");

  $("cn-acct-count").textContent = `(${d.accounts || 0})`;
  const rows = (d.account_views || []).map((v) => {
    const gate = `<span class="badge ${v.call_gate === "ready" ? "bg" : (v.call_gate === "quota_exhausted" ? "bo" : "bb")}">${esc(v.call_gate || "—")}</span>` +
      (v.needs_relogin ? ` <span class="badge br">需重登</span>` : "") +
      (v.disabled ? ` <span class="badge bb">已禁用</span>` : "");
    const errLine = v.last_error ? `<div class="muted" style="font-size:11px;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(v.last_error)}">${esc(v.last_error)}</div>` : "";
    const checkin = v.checkin_date
      ? `<span class="badge bg">✓ ${esc(v.checkin_date)}</span><div class="muted" style="font-size:11px" title="${esc(v.checkin_msg || "")}">${esc(v.checkin_msg || "")}</div>`
      : `<span class="badge bb">未签</span>`;
    return `<tr>
      <td><div class="email" title="${esc(v.email)}">${esc(v.email || v.id)}</div>${errLine}</td>
      <td class="nowrap"><span class="muted" style="font-size:11px">${esc(v.user_id || "—")}</span></td>
      <td>${gate}</td>
      <td>${cnTokenBadge(v)}</td>
      <td>${checkin}</td>
      <td class="nowrap">${esc(fmtTime(v.last_active || v.last_keepalive || ""))}</td>
      <td class="nowrap"><span class="badge bg">${v.success_count || 0}</span> / <span class="badge ${v.fail_count ? "br" : "bb"}">${v.fail_count || 0}</span></td>
      <td class="nowrap">
        <div class="ops">
          <button class="btn sm" onclick="cnRefreshToken('${esc(v.id)}')">续期</button>
          <button class="btn sm" onclick="cnCheckin('${esc(v.id)}')">签到</button>
          ${v.needs_relogin ? `<button class="btn sm" onclick="cnClearRelogin('${esc(v.id)}')">清重登标记</button>` : ""}
        </div>
      </td>
    </tr>`;
  });
  $("cn-acct-body").innerHTML = rows.length
    ? rows.join("")
    : `<tr><td colspan="8" class="empty">暂无 CN 账号。用 <code>cnlogin</code> 登录后点「导入 auths 目录」，或把 auths/*.json 放进 CN_AUTH_DIR。</td></tr>`;

  const models = d.models || [];
  $("cn-models").innerHTML = models.length
    ? models.map((m) =>
        `<span class="usage-pill" title="${esc(m.id)} · ${m.accounts} 个账号可用">` +
        `<b>${esc(m.prefixed)}</b>${m.benefit ? ' <span class="badge bo">福利</span>' : ""}` +
        ` <span class="muted">×${m.accounts}</span></span>`).join("")
    : `<div class="empty">CN 账号尚未检出模型目录（首轮看护或任意一次 CN 请求后自动拉取）。</div>`;

  $("cn-config").innerHTML = (d.config || [])
    .map((c) => `<tr><td class="nowrap"><code>${esc(c.key)}</code></td><td>${esc(c.label)}</td><td class="nowrap">${esc(c.value || "—")}</td></tr>`)
    .join("");
}

// ---------- 国内码道：面板内置 OAuth 登录 ----------
let _cnLoginID = null;
let _cnLoginTimer = null;

function cnLoginReset() {
  if (_cnLoginTimer) clearInterval(_cnLoginTimer);
  _cnLoginTimer = null;
  _cnLoginID = null;
  const box = $("cn-login-box");
  if (box) box.style.display = "none";
  const cancel = $("cn-login-cancel");
  if (cancel) cancel.style.display = "none";
}

async function cnLoginStart() {
  const portEl = $("cn-login-port");
  const port = Number((portEl && portEl.value) || location.port || 0);
  const status = $("cn-login-status");
  try {
    const d = await api("/api/cn/login/start", {
      method: "POST",
      body: JSON.stringify({ port }),
    });
    _cnLoginID = d.session_id;
    $("cn-login-box").style.display = "";
    $("cn-login-cancel").style.display = "";
    $("cn-login-url").value = d.auth_url || "";
    if (status) status.textContent = d.hint || "等待浏览器完成授权…";
    if (_cnLoginTimer) clearInterval(_cnLoginTimer);
    _cnLoginTimer = setInterval(cnLoginPoll, 2000);
  } catch (e) {
    toast(e.message);
  }
}

async function cnLoginPoll() {
  if (!_cnLoginID) return;
  try {
    const d = await api(`/api/cn/login/status?id=${encodeURIComponent(_cnLoginID)}`);
    const status = $("cn-login-status");
    if (d.status === "done") {
      const a = d.account || {};
      cnLoginReset();
      if (status) status.textContent = `登录成功：${a.name || a.uid || ""}（凭据已入库）`;
      toast("国内码道账号已添加");
      loadCN();
    } else if (d.status === "error") {
      if (status) status.textContent = "登录失败：" + (d.message || "");
      if (_cnLoginTimer) clearInterval(_cnLoginTimer);
      _cnLoginTimer = null;
    } else if (d.status === "expired") {
      cnLoginReset();
      if (status) status.textContent = d.message || "会话已过期，请重新生成链接";
    } else if (d.message && status) {
      status.textContent = d.message;
    }
  } catch (_) {}
}

async function cnLoginSubmit() {
  const paste = ($("cn-login-paste").value || "").trim();
  if (!_cnLoginID) return toast("先点「生成登录链接」");
  if (!paste) return toast("请粘贴浏览器地址栏里的回调 URL");
  try {
    const d = await api(`/api/cn/login/callback?id=${encodeURIComponent(_cnLoginID)}`, {
      method: "POST",
      body: JSON.stringify({ callback_url: paste }),
    });
    $("cn-login-paste").value = "";
    const status = $("cn-login-status");
    if (d.next_url) {
      if (status) status.textContent = d.message || "回调已接收";
      openExternal(d.next_url);
    } else if (d.status === "done") {
      const a = d.account || {};
      cnLoginReset();
      toast(`登录成功：${a.name || a.uid || ""}`);
      loadCN();
    } else if (status) {
      status.textContent = d.message || "已提交";
    }
  } catch (e) {
    toast(e.message);
  }
}

async function cnImportAuths() {
  try {
    const r = await api("/api/cn/import", { method: "POST", body: "{}" });
    toast(`已导入 ${r.imported || 0} 个 CN 账号`);
    await loadCN();
  } catch (e) {
    toast("导入失败: " + e.message);
  }
}

async function cnRefreshToken(id) {
  try {
    const r = await api(`/api/cn/accounts/${encodeURIComponent(id)}/refresh`, { method: "POST", body: "{}" });
    toast("续期成功，到期 " + fmtTime(r.token_expires || ""));
  } catch (e) {
    toast("续期失败: " + e.message);
  }
  loadCN();
}

async function cnCheckin(id) {
  try {
    const r = await api(`/api/cn/accounts/${encodeURIComponent(id)}/checkin`, { method: "POST", body: "{}" });
    toast(`签到完成：${r.message || "ok"}${r.credits ? ` · +${r.credits} 积分` : ""}`);
  } catch (e) {
    toast("签到失败: " + e.message);
  }
  loadCN();
}

async function cnClearRelogin(id) {
  try {
    await api(`/api/cn/accounts/${encodeURIComponent(id)}/relogin-clear`, { method: "POST", body: "{}" });
    toast("已清除重登标记");
  } catch (e) {
    toast("操作失败: " + e.message);
  }
  loadCN();
}

$("cn-login-start")?.addEventListener("click", cnLoginStart);
$("cn-login-cancel")?.addEventListener("click", cnLoginReset);
$("cn-login-submit")?.addEventListener("click", cnLoginSubmit);
$("cn-login-paste")?.addEventListener("keydown", (e) => { if (e.key === "Enter") cnLoginSubmit(); });
$("cn-login-open")?.addEventListener("click", () => {
  const u = $("cn-login-url")?.value;
  if (u) window.open(u, "_blank", "noopener");
});
$("cn-login-copy")?.addEventListener("click", async () => {
  const u = $("cn-login-url")?.value || "";
  try {
    await navigator.clipboard.writeText(u);
    toast("链接已复制");
  } catch (_) {
    $("cn-login-url")?.select();
    toast("已选中，按 Ctrl/Cmd+C 复制");
  }
});
bindSortableHeaders("accounts-table", acctSortValue, renderAccounts);
bindSortableHeaders("proxies-table", proxySortValue, renderProxiesTable);
bindSortableHeaders("apikeys-table", apikeySortValue, renderAPIKeysTable);

try {
  init();
} catch (e) {
  console.error(e);
  try {
    showLogin();
  } catch (_) {}
}
