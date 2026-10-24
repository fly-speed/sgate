"use strict";
const $ = (id) => document.getElementById(id);
let state = {},
  revision = "",
  epoch = 0,
  previous = null,
  points = [],
  servicePID = 0,
  serviceRestarting = false;
let configInputs = [],
  configSaving = false,
  loggingOut = false;

// 字段值由服务端解析；浏览器不解析或拼接 .cf，避免破坏引号、注释与密码。
function renderConfigFields(fields, runtime) {
  const container = $("config-fields");
  container.replaceChildren();
  configInputs = [];
  const groups = new Map();
  const overrides = new Set(runtime.cli_overrides || []);
  for (const field of fields) {
    let group = groups.get(field.group);
    if (!group) {
      group = document.createElement("fieldset");
      const legend = document.createElement("legend");
      legend.textContent = field.group;
      group.append(legend);
      container.append(group);
      groups.set(field.group, group);
    }
    const row = document.createElement("div");
    row.className = "config-row";
    const label = document.createElement("label");
    const id = "config-option-" + configInputs.length;
    label.htmlFor = id;
    const name = document.createElement("span");
    name.className = "config-key";
    name.textContent = field.key;
    const description = document.createElement("small");
    description.id = id + "-help";
    description.textContent = field.description || "配置项：" + field.key;
    label.append(name, description);
    const overridden = overrides.has(field.key === "admin_enabled" ? "admin-enabled" : field.key);
    if (overridden) {
      const badge = document.createElement("span");
      badge.className = "override-badge";
      badge.textContent = "命令行优先";
      label.append(badge);
    }
    const boolean =
      !field.secret &&
      /^(admin_enabled|master_|app_)/.test(field.key) &&
      ["true", "false", "yes", "no", "y", "n"].includes(field.value);
    const input = document.createElement(boolean ? "select" : "input");
    if (boolean) {
      const values = ["true", "false"].includes(field.value)
        ? ["true", "false"]
        : ["yes", "no"].includes(field.value)
          ? ["yes", "no"]
          : ["y", "n"];
      for (const value of values) {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = value + (["true", "yes", "y"].includes(value) ? "（是）" : "（否）");
        input.append(option);
      }
    } else {
      input.type = field.secret ? "password" : "text";
      input.autocomplete = "off";
      input.spellcheck = false;
    }
    input.id = id;
    input.name = field.key;
    input.setAttribute("aria-describedby", description.id);
    input.value = field.secret ? "" : field.value;
    if (field.secret) input.placeholder = "已隐藏 · 不修改则保留原值";
    const saveButton = document.createElement("button");
    saveButton.type = "button";
    saveButton.className = "secondary config-save";
    saveButton.innerHTML =
      '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h12l4 4v12a2 2 0 0 1-2 2Z"/><path d="M7 3v6h10V3M7 21v-8h10v8"/></svg>';
    saveButton.setAttribute("aria-label", "保存 " + field.key);
    const entry = {
      key: field.key,
      input,
      saveButton,
      original: input.value,
      secret: field.secret,
      dirty: false,
    };
    input.addEventListener("input", () => {
      entry.dirty = input.value !== entry.original;
      updateConfigSaveButton(entry, configSaving || serviceRestarting);
      $("pending-label").textContent = Object.keys(configEdits()).length
        ? "有未保存修改"
        : "没有未保存修改";
    });
    updateConfigSaveButton(entry);
    saveButton.addEventListener("click", () => saveConfigField(entry));
    configInputs.push(entry);
    row.append(label, input, saveButton);
    group.append(row);
  }
}

function configEdits() {
  const edits = {};
  for (const entry of configInputs) {
    if (entry.dirty && (entry.secret || entry.input.value !== entry.original))
      edits[entry.key] = entry.input.value;
  }
  return edits;
}

function setConfigBusy(busy) {
  for (const entry of configInputs) {
    entry.input.disabled = busy;
    updateConfigSaveButton(entry, busy);
  }
  for (const id of ["save-config", "apply-config", "reload-config"]) $(id).disabled = busy;
}

function updateConfigSaveButton(entry, busy = false) {
  entry.saveButton.disabled = busy || !entry.dirty;
  entry.saveButton.title = entry.dirty ? "保存此项" : "未修改";
}

// 单项存盘不重新加载表单，其他行尚未保存的编辑与密码输入继续保留。
async function saveConfigField(entry) {
  if (configSaving || serviceRestarting) return;
  if (!entry.input.reportValidity()) return;
  if (!entry.dirty || (!entry.secret && entry.input.value === entry.original)) {
    notice(entry.key + " 没有需要存盘的修改。");
    return;
  }
  const value = entry.input.value;
  configSaving = true;
  setConfigBusy(true);
  entry.saveButton.setAttribute("aria-busy", "true");
  try {
    const result = await api("config", {
      method: "PUT",
      body: JSON.stringify({ fields: { [entry.key]: value }, revision, apply: false }),
    });
    revision = result.revision;
    entry.original = entry.input.value = entry.secret ? "" : value;
    entry.dirty = false;
    $("pending-label").textContent = Object.keys(configEdits()).length
      ? "有未保存修改 · 已存盘项待应用"
      : "已保存 · 待应用";
    notice(entry.key + " 已存盘，尚未重启代理。其他未保存修改已保留。");
  } catch (error) {
    notice(errorText(error), true);
  } finally {
    configSaving = false;
    entry.saveButton.setAttribute("aria-busy", "false");
    setConfigBusy(serviceRestarting);
  }
}
async function api(path, options = {}) {
  const controller = new AbortController(),
    timer = setTimeout(() => controller.abort(), 20000);
  try {
    const r = await fetch("api/" + path, {
      ...options,
      credentials: "same-origin",
      signal: controller.signal,
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": state.csrf || "",
        ...(options.headers || {}),
      },
    });
    const data = await r.json();
    if (!r.ok) {
      if (r.status === 401 && path !== "login" && path !== "setup") {
        await refreshAuth();
      }
      const error = new Error(data.error || "请求失败");
      error.status = r.status;
      throw error;
    }
    return data;
  } catch (e) {
    if (e.name === "AbortError") throw new Error("请求超时，请重试");
    throw e;
  } finally {
    clearTimeout(timer);
  }
}
function notice(text, bad = false) {
  $("notice").hidden = false;
  $("notice").textContent = text;
  $("notice").classList.toggle("bad", bad);
}
function errorText(e) {
  return e.message || "网络连接失败，请稍后重试";
}
function units(n, rate = false) {
  if (!Number.isFinite(n)) return "—";
  const list = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < list.length - 1) {
    n /= 1024;
    i++;
  }
  return (n < 10 && i ? n.toFixed(2) : n.toFixed(i ? 1 : 0)) + " " + list[i] + (rate ? "/s" : "");
}
function uptime(n) {
  if (n >= 86400) return Math.floor(n / 86400) + " 天 " + Math.floor((n % 86400) / 3600) + " 小时";
  if (n >= 3600) return Math.floor(n / 3600) + " 小时 " + Math.floor((n % 3600) / 60) + " 分";
  return Math.floor(n / 60) + " 分 " + (n % 60) + " 秒";
}
async function refreshAuth() {
  const authEpoch = ++epoch;
  const status = await api("status");
  if (authEpoch !== epoch) return;
  state = status;
  $("dashboard").hidden = !state.authenticated;
  $("auth").hidden = state.authenticated;
  $("auth-error").textContent = "";
  if (!state.authenticated) {
    renderSignedOut();
    return;
  }
  $("role-title").textContent = state.role === "server" ? "SERVER" : "CLIENT";
  $("side-role").textContent = state.role === "server" ? "服务端代理" : "客户端代理";
  $("user-label").textContent = $("session-user").textContent = state.username;
  $("account-user").value = state.username;
  $("session-duration").textContent = uptime(Math.floor(state.session_timeout_seconds));
  $("session-expiry").textContent = new Date(state.expires).toLocaleString();
  previous = null;
  points = [];
  try {
    await loadConfig();
  } catch (e) {
    notice(errorText(e), true);
  }
  const run = epoch;
  poll(run);
}

function renderSignedOut() {
  // 退出后也清除隐藏页面中的配置草稿、密码和统计，避免共享浏览器继续显示。
  configInputs = [];
  revision = "";
  servicePID = 0;
  previous = null;
  points = [];
  $("config-fields").replaceChildren();
  for (const id of [
    "login-user",
    "login-pass",
    "login-confirm",
    "setup-token",
    "account-user",
    "account-current",
    "account-password",
    "account-confirm",
  ])
    $(id).value = "";
  for (const id of ["user-label", "session-user", "session-expiry", "config-path", "runtime-state"])
    $(id).textContent = "";
  $("notice").hidden = true;
  $("auth-error").textContent = "";
  $("dashboard").hidden = true;
  $("auth").hidden = false;
  $("login-form").hidden = false;
  const setup = !!state.setup_required;
  $("auth-title").textContent = setup ? "创建首个管理员" : "欢迎回来";
  $("auth-help").textContent = setup
    ? "设置独立管理账号。初始化令牌位于管理配置指定的 " + state.setup_token_file + " 文件中。"
    : "登录后查看实时流量并管理代理配置。";
  $("confirm-row").hidden = $("token-row").hidden = !setup;
  $("login-pass").minLength = setup ? 12 : 1;
  $("login-pass").autocomplete = setup ? "new-password" : "current-password";
  $("login-confirm").required = $("setup-token").required = setup;
  $("login-button").textContent = setup ? "创建账号并进入控制台 ↗" : "登录控制台 ↗";
}

async function logoutAdmin() {
  if (loggingOut) return;
  loggingOut = true;
  $("logout").disabled = true;
  try {
    // 服务端删除持久化会话并清除 Cookie 后才确认退出，失败时保留登录以便重试。
    await api("logout", { method: "POST", body: "{}" });
    epoch++;
    state = { authenticated: false };
    renderSignedOut();
    $("auth-help").textContent = "已安全退出，服务器上的本次登录会话已清理。";
  } catch (error) {
    notice(errorText(error), true);
  } finally {
    loggingOut = false;
    $("logout").disabled = false;
  }
}
async function loadConfig() {
  const configEpoch = epoch;
  const data = await api("config");
  if (configEpoch !== epoch || !state.authenticated) return;
  if (!Array.isArray(data.fields))
    throw new Error("配置表单需要新版程序，请更新并重启 sgate 服务。");
  revision = data.revision;
  renderConfigFields(data.fields || [], data.runtime || {});
  $("config-path").textContent = data.path;
  $("pending-label").textContent = data.requires_restart ? "已保存 · 待应用" : "配置文件已同步";
  const rt = data.runtime || {};
  servicePID = rt.pid || 0;
  $("restart-service").hidden = $("restart-service-help").hidden = !rt.service_restart_available;
  $("restart-service").disabled = serviceRestarting || !!rt.service_restart_pending;
  $("runtime-state").textContent =
    "代理状态：" +
    (rt.running ? "运行中" : "未运行") +
    " · 配置代次 " +
    (rt.generation || "—") +
    " · 生效监听 " +
    ((rt.listeners?.length ? rt.listeners.join(", ") : rt.listen) || "—") +
    (rt.cli_overrides?.length ? " · 命令行优先项：" + rt.cli_overrides.join(", ") : "") +
    (rt.listener_owner === "acl_master" ? " · 监听由 acl_master 管理" : "") +
    (rt.last_error ? " · " + rt.last_error : "");
}
async function poll(run) {
  if (run !== epoch || !state.authenticated) return;
  try {
    const data = await api("stats");
    if (run !== epoch) return;
    let tx = 0,
      rx = 0;
    if (
      previous &&
      data.time_ms > previous.time_ms &&
      data.tx_bytes >= previous.tx_bytes &&
      data.rx_bytes >= previous.rx_bytes
    ) {
      const seconds = (data.time_ms - previous.time_ms) / 1000;
      tx = (data.tx_bytes - previous.tx_bytes) / seconds;
      rx = (data.rx_bytes - previous.rx_bytes) / seconds;
    }
    previous = data;
    points.push({ tx, rx });
    if (points.length > 60) points.shift();
    $("tx-rate").textContent = units(tx, true);
    $("rx-rate").textContent = units(rx, true);
    $("tx-total").textContent = "累计发送 " + units(data.tx_bytes);
    $("rx-total").textContent = "累计接收 " + units(data.rx_bytes);
    $("connections").textContent = data.connections;
    $("uptime").textContent = uptime(data.uptime_seconds);
    $("live-status").textContent = "● 实时监测";
    drawChart();
  } catch (e) {
    if (run === epoch) $("live-status").textContent = "● 数据更新中断";
  }
  if (run === epoch && state.authenticated)
    setTimeout(() => poll(run), document.hidden ? 5000 : 1000);
}
function drawChart() {
  const canvas = $("traffic-chart");
  if (canvas.clientWidth === 0) return;
  const ratio = window.devicePixelRatio || 1,
    w = canvas.clientWidth,
    h = canvas.clientHeight;
  canvas.width = Math.round(w * ratio);
  canvas.height = Math.round(h * ratio);
  const c = canvas.getContext("2d");
  c.scale(ratio, ratio);
  const left = 67,
    top = 12,
    right = w - 8,
    bottom = h - 10,
    height = bottom - top,
    width = right - left,
    max = Math.max(1024, ...points.flatMap((x) => [x.tx, x.rx])) * 1.18;
  c.font = "10px -apple-system, sans-serif";
  for (let i = 0; i < 5; i++) {
    const y = top + (height * i) / 4;
    c.strokeStyle = "#eaf0f2";
    c.setLineDash([4, 4]);
    c.beginPath();
    c.moveTo(left, y);
    c.lineTo(right, y);
    c.stroke();
    c.fillStyle = "#8a98a3";
    c.textAlign = "right";
    c.fillText(units(max * (1 - i / 4), true), left - 12, y + 3);
  }
  c.setLineDash([]);
  for (const [key, color] of [
    ["rx", "#5a83d7"],
    ["tx", "#087e68"],
  ]) {
    const coords = points.map((v, i) => [
      left + (width * (60 - points.length + i)) / 59,
      bottom - (v[key] / max) * height,
    ]);
    if (!coords.length) continue;
    c.beginPath();
    c.moveTo(coords[0][0], bottom);
    for (const p of coords) c.lineTo(...p);
    c.lineTo(coords.at(-1)[0], bottom);
    c.closePath();
    const gradient = c.createLinearGradient(0, top, 0, bottom);
    gradient.addColorStop(0, color + "25");
    gradient.addColorStop(1, color + "02");
    c.fillStyle = gradient;
    c.fill();
    c.beginPath();
    coords.forEach((p, i) => (i ? c.lineTo(...p) : c.moveTo(...p)));
    c.strokeStyle = color;
    c.lineWidth = 2;
    c.stroke();
    const last = coords.at(-1);
    c.beginPath();
    c.arc(last[0], last[1], 3, 0, Math.PI * 2);
    c.fillStyle = color;
    c.fill();
  }
}
$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("auth-error").textContent = "";
  if (state.setup_required && $("login-pass").value !== $("login-confirm").value) {
    $("auth-error").textContent = "两次输入的密码不一致";
    return;
  }
  const b = $("login-button");
  b.disabled = true;
  try {
    await api(state.setup_required ? "setup" : "login", {
      method: "POST",
      body: JSON.stringify({
        username: $("login-user").value,
        password: $("login-pass").value,
        setup_token: $("setup-token").value,
      }),
    });
    await refreshAuth();
  } catch (e) {
    $("auth-error").textContent = errorText(e);
  } finally {
    b.disabled = false;
  }
});
$("logout").addEventListener("click", logoutAdmin);
document.querySelectorAll("[data-tab]").forEach((b) =>
  b.addEventListener("click", () => {
    document.querySelectorAll("[data-tab]").forEach((x) => x.classList.toggle("selected", x === b));
    document.querySelectorAll(".tab").forEach((x) => (x.hidden = x.id !== b.dataset.tab));
    $("page-title").textContent = {
      overview: "实时概览",
      configuration: "代理配置",
      account: "账号与会话",
    }[b.dataset.tab];
    drawChart();
  }),
);
$("reload-config").addEventListener("click", () =>
  loadConfig().catch((e) => notice(errorText(e), true)),
);
async function saveConfig(apply) {
  if (serviceRestarting || configSaving) return;
  if (!$("config-form").reportValidity()) return;
  const fields = configEdits();
  configSaving = true;
  setConfigBusy(true);
  try {
    await api("config", {
      method: "PUT",
      body: JSON.stringify({ fields, revision, apply }),
    });
    await loadConfig();
    notice(
      apply ? "配置已应用，业务代理已重启。管理登录继续有效。" : "配置已校验并保存，尚未重启代理。",
    );
  } catch (e) {
    notice(errorText(e), true);
  } finally {
    configSaving = false;
    setConfigBusy(serviceRestarting);
  }
}
$("config-form").addEventListener("submit", (event) => {
  event.preventDefault();
  saveConfig(false);
});
$("save-config").addEventListener("click", () => saveConfig(false));
$("apply-config").addEventListener("click", () => saveConfig(true));
$("restart-service").addEventListener("click", restartService);

async function restartService() {
  if (serviceRestarting || configSaving || !servicePID) return;
  const oldPID = servicePID;
  const buttons = [$("restart-service"), $("save-config"), $("apply-config")];
  serviceRestarting = true;
  buttons.forEach((button) => (button.disabled = true));
  notice("正在请求 acl_master 重启服务，等待管理连接恢复…");
  try {
    try {
      await api("restart-service", {
        method: "POST",
        body: JSON.stringify({ expected_pid: oldPID }),
      });
    } catch (error) {
      // 整进程替换可能早于 HTTP 响应；明确的 HTTP 拒绝则直接显示原因。
      if (error.status) throw error;
    }
    await waitForServiceRestart(oldPID);
    await refreshAuth();
    if (state.authenticated) notice("服务已由 acl_master 重启，登录会话继续有效。");
  } catch (error) {
    notice(errorText(error), true);
  } finally {
    serviceRestarting = false;
    buttons.forEach((button) => (button.disabled = false));
  }
}

async function waitForServiceRestart(oldPID) {
  const deadline = Date.now() + 90000;
  while (Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 1000));
    let runtime;
    try {
      const config = await api("config");
      runtime = config.runtime || {};
    } catch (error) {
      if (error.status || !state.authenticated) throw error;
      // HTTP 监听重建期间继续轮询；只有新 PID 就绪才报告完成。
      continue;
    }
    if (runtime.pid && runtime.pid !== oldPID && runtime.running) return;
    if (!runtime.service_restart_pending && runtime.last_error) {
      throw new Error(runtime.last_error);
    }
  }
  throw new Error(
    "等待服务恢复超时，请检查 acl_master 日志；若管理地址或证书已更改，请打开新地址。",
  );
}
$("account-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if ($("account-password").value !== $("account-confirm").value) {
    notice("两次输入的新密码不一致", true);
    return;
  }
  const b = e.submitter;
  b.disabled = true;
  try {
    await api("account", {
      method: "PUT",
      body: JSON.stringify({
        username: $("account-user").value,
        password: $("account-password").value,
        current_password: $("account-current").value,
      }),
    });
    e.target.reset();
    await refreshAuth();
  } catch (e) {
    notice(errorText(e), true);
  } finally {
    b.disabled = false;
  }
});
window.addEventListener("resize", drawChart);
refreshAuth().catch((e) => {
  $("auth-title").textContent = "无法连接管理服务";
  $("auth-help").textContent = errorText(e);
});
