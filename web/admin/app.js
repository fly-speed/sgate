"use strict";
const { t, localText, localAttr } = sgateI18n;
const $ = (id) => document.getElementById(id);
let state = {},
  revision = "",
  epoch = 0,
  previous = null,
  points = [],
  chartPointer = null,
  servicePID = 0,
  serviceRestarting = false;
let servicePendingListeners = [];
let serverTunnelListeners = [];
let tunnelAccountsLoad = 0;
let tunnelAccountsRevision = "", tunnelAccountRows = [], tunnelAccountsBusy = false, tunnelAccountsEnabled = false, tunnelAccountsCanCreate = false;
let tunnelAccountCreateMappings = null;
let configRole = "";
// Keep each role's expanded sections across saves and language changes.
const configGroupOpen = new Map();
let configInputs = [],
  configSaving = false,
  loggingOut = false;

// 字段值由服务端解析；浏览器不解析或拼接 .cf，避免破坏引号、注释与密码。
function renderConfigFields(fields, runtime, role) {
  configRole = role || state.role || "";
  if (role === "client" && !fields.some((field) => field.key === "proxies")) {
    fields = [...fields, { key: "proxies", kind: "proxies", mappings: [], group: t("代理与通道") }];
  }
  if (role === "client") {
    fields = [
      ...fields.filter((field) => field.kind === "proxies"),
      ...fields.filter((field) => field.kind !== "proxies"),
    ];
  }
  const container = $("config-fields");
  container.replaceChildren();
  configInputs = [];
  const groups = new Map();
  const overrides = new Set(runtime.cli_overrides || []);
  for (const field of fields) {
    let group = groups.get(field.group);
    if (!group) {
      group = document.createElement("details");
      group.className = "config-group";
      const groupKey = configRole + ":" + field.group;
      group.open = configGroupOpen.get(groupKey) ?? false;
      group.addEventListener("toggle", () => configGroupOpen.set(groupKey, group.open));
      const summary = document.createElement("summary");
      localText(summary, () => t(field.group));
      group.append(summary);
      container.append(group);
      groups.set(field.group, group);
    }
    if (field.kind === "proxies") {
      renderProxyMappings(group, field, overrides.has("proxies") || overrides.has("proxies-file"));
      continue;
    }
    const row = document.createElement("div");
    row.className = "config-row";
    const label = document.createElement("label");
    const id = "config-option-" + configInputs.length;
    label.htmlFor = id;
    const name = document.createElement("span");
    name.className = "config-key";
    localText(name, () => configRole === "client" && field.key === "username"
      ? t("服务端账号") + " (username)"
      : configRole === "client" && field.key === "password"
        ? t("服务端密码") + " (password)" : field.key);
    const description = document.createElement("small");
    description.id = id + "-help";
    localText(description, () => ((t(field.description) || t("配置项：") + field.key) +
      (field.validation?.hint ? " · " + t(field.validation.hint) : "") +
      (["noise-min-bytes", "noise-max-bytes"].includes(field.key)
        ? t(" · 这是每次发送的负载总量，不是 B/s 速率；一秒发送多次时，图表会累计这些流量。")
        : "")));
    label.append(name, description);
    const overridden = overrides.has(field.key === "admin_enabled" ? "admin-enabled" : field.key);
    if (overridden) {
      const badge = document.createElement("span");
      badge.className = "override-badge";
      localText(badge, () => t("命令行优先"));
      label.append(badge);
    }
    if (field.validation?.live) {
      const badge = document.createElement("span");
      badge.className = "live-badge";
      localText(badge, () => overridden ? t("支持即时生效 · 命令行已覆盖") : t("保存后立即生效"));
      localAttr(badge, "title", () => t("同步写入配置文件，无需重启代理；命令行显式参数优先。"));
      label.append(badge);
    }
    const boolean =
      field.validation?.type === "boolean" ||
      (!field.validation &&
        !field.secret &&
        /^(admin_enabled|master_|app_)/.test(field.key) &&
        ["true", "false", "yes", "no", "y", "n"].includes(field.value));
    const input = document.createElement(boolean || field.validation?.choices ? "select" : "input");
    if (boolean || field.validation?.choices) {
      const values =
        field.validation?.choices ||
        (["true", "false"].includes(field.value)
          ? ["true", "false"]
          : ["yes", "no"].includes(field.value)
            ? ["yes", "no"]
            : ["y", "n"]);
      for (const value of values) {
        const option = document.createElement("option");
        option.value = value;
        localText(option, () => (value +
          (boolean
            ? ["true", "yes", "y", "1", "t", "T", "TRUE", "True"].includes(value)
              ? t("（是）")
              : t("（否）")
            : "")));
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
    if (field.secret) localAttr(input, "placeholder", () => t("已隐藏 · 不修改则保留原值"));
    const saveButton = document.createElement("button");
    saveButton.type = "button";
    saveButton.className = "secondary config-save";
    saveButton.innerHTML =
      '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h12l4 4v12a2 2 0 0 1-2 2Z"/><path d="M7 3v6h10V3M7 21v-8h10v8"/></svg>';
    localAttr(saveButton, "aria-label", () => t("保存 ") + field.key);
    const error = document.createElement("small");
    error.className = "config-error";
    error.hidden = true;
    error.id = id + "-error";
    input.setAttribute("aria-describedby", description.id + " " + error.id);
    const editor = document.createElement("div");
    editor.className = "config-editor";
    editor.append(input, error);
    const entry = {
      rule: field.validation || {},
      error,
      key: field.key,
      input,
      saveButton,
      original: input.value,
      secret: field.secret,
      dirty: false,
    };
    input.addEventListener("input", () => {
      entry.dirty = input.value !== entry.original;
      validateConfigForm(null, false);
      updateConfigSaveButton(entry, configSaving || serviceRestarting);
      localText($("pending-label"), () => Object.keys(configEdits()).length
        ? t("有未保存修改")
        : t("没有未保存修改"));
    });
    updateConfigSaveButton(entry);
    saveButton.addEventListener("click", () => saveConfigField(entry));
    configInputs.push(entry);
    row.append(label, editor, saveButton);
    group.append(row);
  }
}

// 使用服务端下发的规则，不把配置文件原文交给浏览器解析。
function configByteLength(text) {
  let size = 0;
  for (const char of text) {
    const point = char.codePointAt(0);
    size += point <= 0x7f ? 1 : point <= 0x7ff ? 2 : point <= 0xffff ? 3 : 4;
  }
  return size;
}
function configNumber(value, type) {
  if (type === "duration") {
    const sign = value.startsWith("-") ? -1n : 1n;
    value = value.replace(/^[-+]/, "");
    if (value === "0") return 0n;
    const units = {
      ns: 1n,
      us: 1000n,
      µs: 1000n,
      μs: 1000n,
      ms: 1000000n,
      s: 1000000000n,
      m: 60000000000n,
      h: 3600000000000n,
    };
    const parts = value.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g);
    let total = 0n,
      end = 0;
    for (const part of parts) {
      if (part.index !== end) throw new Error(t("时间需要单位，例如 500ms、1s、2m 或 1h30m"));
      const [whole, fraction = ""] = part[1].split(".");
      const scale = 10n ** BigInt(fraction.length);
      total += (BigInt((whole || "0") + fraction) * units[part[2]]) / scale;
      end += part[0].length;
    }
    if (!end || end !== value.length) throw new Error(t("时间需要单位，例如 500ms、1s、2m 或 1h30m"));
    return sign * total;
  }
  if (type === "bytes") {
    const match = /^(\d+)(KIB|MIB|KB|MB|K|M|B)?$/i.exec(value.trim());
    if (!match) throw new Error(t("大小须为整数字节数或 K/M 后缀，例如 1024、1K、10M"));
    const unit = (match[2] || "B")[0].toUpperCase();
    return BigInt(match[1]) * (unit === "M" ? 1048576n : unit === "K" ? 1024n : 1n);
  }
  if (!/^[-+]?\d+$/.test(value)) throw new Error(t("请填写整数"));
  return BigInt(value);
}
function configAddress(value, listen = false) {
  const match = /^(?:\[([^\]]+)\]|([^:\s]*)):(\d+)$/.exec(value);
  if (!match) throw new Error(t("地址须为 主机:端口，IPv6 使用 [地址]:端口"));
  const host = match[1] || match[2];
  if (!host && !listen) throw new Error(t("后端主机不能为空"));
  const port = Number(match[3]);
  if (port < (listen ? 0 : 1) || port > 65535) throw new Error(t("端口超出允许范围"));
  if (host) {
    if (match[1]) {
      try {
        new URL("http://[" + host + "]");
      } catch {
        throw new Error(t("IPv6 地址无效"));
      }
    } else if (
      host.length > 253 ||
      !host
        .replace(/\.$/, "")
        .split(".")
        .every((label) => label.length <= 63 && /^[a-z\d](?:[a-z\d-]*[a-z\d])?$/i.test(label))
    )
      throw new Error(t("主机名或 IP 地址无效"));
    if (/^\d+\.\d+\.\d+\.\d+$/.test(host) && host.split(".").some((n) => Number(n) > 255))
      throw new Error(t("IPv4 地址无效"));
  }
  return port;
}
function configURL(value, checkAddress = true) {
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new Error(t("需要有效的 ws://、wss:// 或 quic:// URL"));
  }
  if (
    !["ws:", "wss:", "quic:"].includes(url.protocol) ||
    !url.hostname ||
    url.username ||
    url.password ||
    url.hash
  )
    throw new Error(t("需要不含账号或片段的 ws://、wss:// 或 quic:// URL"));
  if (url.protocol === "quic:" && !url.port) throw new Error(t("QUIC URL 必须指定 UDP 端口"));
  if (url.port === "0") throw new Error(t("URL 端口范围为 1～65535"));
  if (!/^(wss?|quic):\/\//.test(value) || /\s/.test(value)) throw new Error(t("WebSocket URL 格式无效"));
  const addresses = checkAddress ? url.searchParams.getAll("addr") : [];
  if (addresses.length > 1) throw new Error(t("URL 不能重复指定 addr"));
  if (addresses.length) configAddress(addresses[0]);
}
function configArguments(value, options) {
  const args = value.trim().split(/\s+/);
  for (let i = 0; i < args.length; i++) {
    const match = /^--?([^=]+)(?:=(.*))?$/.exec(args[i]);
    if (!match || !Object.hasOwn(options, match[1]))
      throw new Error(t("master_args 包含未知选项或多余的位置参数"));
    const rule = options[match[1]];
    let argument = match[2];
    if (argument === undefined) argument = rule.type === "boolean" ? "true" : args[++i];
    if (argument === undefined) throw new Error(t("master_args 有选项缺少值"));
    const error = configValueError(match[1], argument, { ...rule, required: false });
    if (error) throw new Error(t("master_args 中 ") + match[1] + t("：") + error);
    if (rule.type === "proxies" && argument) {
      let mappings;
      try {
        mappings = JSON.parse(argument);
      } catch {
        throw new Error(t("master_args 的 proxies 必须为不加 shell 引号的紧凑 JSON"));
      }
      if (!Array.isArray(mappings) || mappings.length > 64)
        throw new Error(t("master_args 的映射须为数组且不超过 64 项"));
      const ports = new Set();
      for (const mapping of mappings) {
        const port = configAddress(mapping.listen, true);
        if (!port || ports.has(port)) throw new Error(t("master_args 的映射监听端口无效或重复"));
        ports.add(port);
        configURL(mapping.server);
        if (mapping.addr) configAddress(mapping.addr);
      }
    }
  }
}
function configValueError(key, value, rule = {}, values = {}) {
  if (/[\r\n\x00]/.test(value)) return t("不能包含换行或空字符");
  if (/^(master_|app_)/.test(key) && (value.includes("#") || value !== value.trim()))
    return t("框架参数不能包含 # 或首尾空白");
  if (configByteLength(value) > 4095) return t("配置项过长，模板每行最多支持 4095 字节");
  if (!value) return rule.required ? t("不能为空") : "";
  try {
    if (rule.type === "arguments" && rule.options) {
      configArguments(value, rule.options);
    } else if (["integer", "bytes", "duration", "seconds"].includes(rule.type)) {
      if (rule.type === "seconds" && value === "-") return "";
      const number = configNumber(value, rule.type);
      if (
        (rule.min !== undefined && number < BigInt(rule.min)) ||
        (rule.max !== undefined && number > BigInt(rule.max))
      )
        return t("超出允许范围；") + t(rule.hint);
    } else if (rule.choices && !rule.choices.includes(value)) return t("请使用允许的选项");
    else if (["address", "listen"].includes(rule.type))
      configAddress(value, rule.type === "listen");
    else if (rule.type === "addresses")
      for (const address of value.split(",")) configAddress(address.trim());
    else if (rule.type === "url")
      configURL(
        value,
        !values.addr && !["false", "0", "f", "F", "FALSE", "False"].includes(values["send-addr"]),
      );
    else if (rule.type === "listeners") {
      if (value.startsWith("/")) return "";
      const ports = new Set();
      for (const address of value.split(",")) {
        let text = address.trim();
        if (/^\d+$/.test(text)) text = ":" + text;
        else text = text.replace(/\|(?=[^|]*$)/, ":");
        const port = configAddress(text, true);
        if (!port || ports.has(text))
          throw new Error(t("框架监听端口须为 1～65535，监听地址不能重复"));
        ports.add(text);
      }
    } else if (rule.type === "route" && (!value.startsWith("/") || /[?#\s]/.test(value)))
      return t("路径须以 / 开头，不含空白、查询参数或片段");
    else if (rule.type === "username" && (configByteLength(value) > 128 || /[\s\p{Cc}\p{Z}]/u.test(value)))
      return t("用户名不能超过 128 个 UTF-8 字节，且不能含空白或控制字符");
    else if (rule.type === "password" && configByteLength(value) > Number(rule.max))
      return t("密码不能超过 ") + rule.max + t(" 个 UTF-8 字节");
  } catch (error) {
    return errorText(error);
  }
  return "";
}
function setConfigError(input, error, element) {
  input.setCustomValidity?.(error);
  input.setAttribute("aria-invalid", error ? "true" : "false");
  if (element) {
    localText(element, () => error);
    element.hidden = !error;
  }
}
function configCandidate(single) {
  const values = {};
  for (const entry of configInputs) {
    const edit = !single || entry === single;
    values[entry.key] =
      entry.kind === "proxies"
        ? edit
          ? entry.read()
          : JSON.parse(entry.original)
        : edit
          ? entry.input.value
          : entry.original;
  }
  return values;
}
function configRelationErrors(values) {
  const errors = new Map();
  const has = (...keys) => keys.every((key) => Object.hasOwn(values, key));
  const fail = (keys, message) => keys.forEach((key) => errors.set(key, message));
  const number = (key, type, fallback) => configNumber(values[key], type) || fallback;
  if (has("noise-interval", "noise-interval-min", "noise-interval-max")) {
    const keys = ["noise-interval", "noise-interval-min", "noise-interval-max"];
    const [fixed, min, max] = keys.map((key) => number(key, "duration", 0n));
    if ((min || max) && (fixed || !min || max < min))
      fail(keys, t("固定间隔须为 0，随机范围须满足 0 < min ≤ max；关联项请一起修改后整页保存"));
  }
  if (has("noise-min-bytes", "noise-max-bytes")) {
    if (number("noise-min-bytes", "bytes", 1024n) > number("noise-max-bytes", "bytes", 10240n))
      fail(
        ["noise-min-bytes", "noise-max-bytes"],
        t("噪音最小值不能大于最大值；关联项请一起修改后整页保存"),
      );
  }
  if (
    has("reconnect-min", "reconnect-max") &&
    number("reconnect-min", "duration", 250000000n) >
      number("reconnect-max", "duration", 10000000000n)
  )
    fail(["reconnect-min", "reconnect-max"], t("重连最大间隔不能小于最小间隔"));
  if (has("buffer-bytes")) {
    const buffer = number("buffer-bytes", "integer", 4194304n);
    if (buffer < 16409n) fail(["buffer-bytes"], t("缓存至少为 16409 字节，或填 0 使用默认值"));
  }
  if (has("heartbeat", "heartbeat-timeout")) {
    const heartbeat = number("heartbeat", "duration", 20000000000n);
    const timeout = number("heartbeat-timeout", "duration", heartbeat * 3n);
    if (timeout <= heartbeat || timeout <= 15000000000n)
      fail(
        ["heartbeat", "heartbeat-timeout"],
        t("心跳超时须大于心跳间隔和 15s 写入超时；0 使用心跳间隔的三倍"),
      );
  }
  for (const [cert, key] of [
    ["tls-cert", "tls-key"],
    ["admin_tls_cert", "admin_tls_key"],
    ["tls_cert_file", "tls_key_file"],
  ])
    if (has(cert, key) && !!values[cert] !== !!values[key])
      fail([cert, key], t("证书与私钥路径须同时填写或同时留空"));
  if (has("server") && !values.server && !values.proxies?.length)
    fail(["server"], t("单代理模式需要服务端 URL；或先配置至少一个映射"));
  if (has("path") && (values.path === "/admin" || values.path.startsWith("/admin/")))
    fail(["path"], t("隧道路径不能占用 /admin 管理入口"));
  const loopback = (address) => /^(localhost|127(?:\.\d+){3}|\[::1\]):/i.test(address);
  if (
    configRole === "client" &&
    has("admin_listen", "admin_tls_cert") &&
    !loopback(values.admin_listen) &&
    !values.admin_tls_cert
  )
    fail(
      ["admin_listen", "admin_tls_cert", "admin_tls_key"],
      t("管理界面监听非回环地址时必须配置 HTTPS 证书与私钥"),
    );
  if (has("admin_users_file", "admin_session_dir", "admin_setup_token_file")) {
    const keys = ["admin_users_file", "admin_session_dir", "admin_setup_token_file"];
    if (new Set(keys.map((key) => values[key])).size < 3)
      fail(keys, t("账号、会话和初始化令牌的存储路径不能相同"));
  }
  return errors;
}
function validateConfigForm(single = null, report = true) {
  const selected = single ? [single] : configInputs;
  const candidate = configCandidate(single);
  let first = null;
  for (const entry of selected) {
    if (entry.kind === "proxies") {
      const ports = new Set();
      for (const row of entry.rows)
        for (const key of ["listen", "server", "addr"]) {
          let error = "";
          try {
            if (key === "server")
              configURL(
                row[key].value,
                !row.addr.value &&
                  !["false", "0", "f", "F", "FALSE", "False"].includes(candidate["send-addr"]),
              );
            else if (row[key].value || key === "listen") {
              const port = configAddress(row[key].value, key === "listen");
              if (key === "listen") {
                if (!port || ports.has(port))
                  throw new Error(t("映射监听端口须为 1～65535，且不能重复"));
                ports.add(port);
              }
            }
          } catch (e) {
            error = errorText(e);
          }
          setConfigError(row[key], error, row.errors?.[key]);
          if (error && !first) first = { input: row[key], key: "proxies", error };
        }
      for (const row of entry.rows) {
        const error = row.auth?.checked ? mappingCredentialError(row) : "";
        setConfigError(row.password,error,row.errors?.password);
        if (error && !first) first={input:row.password,key:"proxies",error};
      }
      if (entry.rows.length > 64 && !first) first = { key: "proxies", error: t("最多允许 64 个映射") };
    } else {
      const error =
        entry.secret && !entry.dirty
          ? ""
          : configValueError(entry.key, entry.input.value, entry.rule, candidate);
      setConfigError(entry.input, error, entry.error);
      if (error && !first) first = { ...entry, error };
    }
  }
  if (!first) {
    try {
      const errors = configRelationErrors(candidate);
      for (const entry of selected)
        if (errors.has(entry.key)) {
          const error = errors.get(entry.key);
          if (entry.input) setConfigError(entry.input, error, entry.error);
          if (!first) first = { ...entry, error };
        }
    } catch {
      /* 非关联编辑项的未保存草稿不阻止当前单项保存。 */
    }
  }
  if (first && report) {
    // Validation must reveal a collapsed section before focusing its control.
    const control = first.input || first.controls?.[0];
    const group = control?.closest?.("details.config-group");
    if (group) group.open = true;
    notice(() => first.key + t("：") + t(first.error), true);
    first.input?.focus?.();
    first.input?.reportValidity?.();
  }
  return !first;
}

// 映射以结构化数组提交；后端负责配置文件的 JSON、引号和注释处理。
function configPayload(edits, apply) {
  const fields = { ...edits };
  const result = { fields, revision, apply };
  if (Object.prototype.hasOwnProperty.call(fields, "proxies")) {
    result.proxies = fields.proxies;
    delete fields.proxies;
  }
  return result;
}

function renderProxyMappings(group, field, overridden) {
  const section = document.createElement("div");
  section.className = "proxy-editor";
  const header = document.createElement("div");
  header.className = "proxy-editor-header";
  const title = document.createElement("strong");
  localText(title, () => t("多端口代理映射"));
  const help = document.createElement("p");
  help.className = "muted";
  localText(help, () => (t("每行将一个本地端口映射到远程服务。后端地址可选，须在服务端允许列表内；没有映射时使用下面的单代理配置。") +
    (overridden ? t(" 当前映射来源受命令行覆盖，保存配置不会移除启动时的覆盖参数。") : "") +
    " " + t("每个映射可使用独立账号密码；关闭独立认证则继承公共凭据。测试使用本行认证和地址，密钥、TLS 和 send-addr 沿用生效设置。密码不会回显，留空保留已保存密码。") +
    " " + t("配置 proxies-file 后映射保存在独立 JSON 文件；留空继续使用内联 proxies。更改文件路径时自动迁移当前映射，已有文件则加载其映射。") +
    " " + t("保存后自动在线更新映射并同步 master_service，未修改映射的连接继续保留。清空时使用单代理 listen；托管模式端口变化会自动通过 acl_master 重启客户端，并中断当前业务连接。")));
  const saveButton = document.createElement("button");
  saveButton.type = "button";
  saveButton.className = "secondary config-save";
  saveButton.innerHTML =
    '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h12l4 4v12a2 2 0 0 1-2 2Z"/><path d="M7 3v6h10V3M7 21v-8h10v8"/></svg>';
  localAttr(saveButton, "aria-label", () => t("保存代理映射"));
  const list = document.createElement("div");
  const add = document.createElement("button");
  add.type = "button";
  add.className = "secondary";
  localText(add, () => t("＋ 添加映射"));
  const entry = {
    key: "proxies",
    kind: "proxies",
    saveButton,
    rows: [],
    controls: [],
    dirty: false,
  };
  entry.read = () =>
    entry.rows.map((row) => ({
      listen: row.listen.value,
      server: row.server.value,
      ...(row.addr.value ? { addr: row.addr.value } : {}),
      ...(row.allowReverse.checked ? {allow_reverse:true} : {}),
      ...(row.auth.checked ? {
        ...(row.inheritUsername && !row.username.value ? {} : {username:row.username.value}),
        ...(row.password.value || row.savedPassword ? {password:row.password.value || row.savedPassword} : {}),
      } : {}),
    }));
  const changed = () => {
    entry.dirty = JSON.stringify(entry.read()) !== entry.original;
    validateConfigForm(null, false);
    updateConfigSaveButton(entry, configSaving || serviceRestarting);
    localText($("pending-label"), () => Object.keys(configEdits()).length
      ? t("有未保存修改")
      : t("没有未保存修改"));
  };
  const draw = (values) => {
    list.replaceChildren();
    entry.rows = [];
    entry.controls = [add];
    if (!values.length) {
      const empty = document.createElement("p");
      empty.className = "muted";
      localText(empty, () => t("尚未添加映射，当前使用单代理配置。"));
      list.append(empty);
    }
    for (const value of values) {
      const row = document.createElement("div");
      row.className = "proxy-mapping";
      const controls = { errors: {} };
      for (const [key, name, placeholder] of [
        ["listen", t("本地监听地址"), "127.0.0.1:9001"],
        ["server", t("远程服务端地址"), t("wss://服务器:端口/tunnel")],
        ["addr", t("后端地址（可选）"), t("127.0.0.1:8080 或 域名:端口")],
      ]) {
        const label = document.createElement("label");
        localText(label, () => t(name));
        const input = document.createElement("input");
        input.type = "text";
        input.autocomplete = "off";
        input.spellcheck = false;
        input.required = key !== "addr";
        localAttr(input, "placeholder", () => t(placeholder));
        input.value = value[key] || "";
        localAttr(input, "aria-label", () => t(name));
        input.addEventListener("input", () => {
          controls.testVersion = (controls.testVersion || 0) + 1;
          if (controls.result) controls.result.hidden = true;
          changed();
        });
        controls[key] = input;
        const error = document.createElement("small");
        error.className = "config-error";
        error.hidden = true;
        controls.errors[key] = error;
        entry.controls.push(input);
        label.append(input, error);
        row.append(label);
      }
      const authLabel = document.createElement("label");
      const auth = document.createElement("input");
      auth.type = "checkbox";
      auth.checked = value.username !== undefined || value.password !== undefined;
      controls.auth = auth;
      controls.inheritUsername = value.username === undefined && value.password !== undefined;
      localText(authLabel, () => t("独立认证（关闭时继承公共账号密码）"));
      authLabel.append(auth);
      entry.controls.push(auth);
      row.append(authLabel);
      controls.savedPassword = value.password === "__SGATE_KEEP_EXISTING_PASSWORD__" ? value.password : "";
      for (const [key, name] of [["username", t("认证账号")], ["password", t("认证密码")]]) {
        const label = document.createElement("label");
        localText(label, () => t(name));
        const input = document.createElement("input");
        input.type = key === "password" ? "password" : "text";
        input.autocomplete = "off";
        input.value = key === "password" && controls.savedPassword ? "" : value[key] || "";
        localAttr(input, "placeholder", () => key === "password"
          ? controls.savedPassword ? t("留空保留原密码") : t("留空继承公共密码")
          : controls.inheritUsername ? t("留空继承公共账号") : t("留空使用旧单密码模式"));
        localAttr(input, "aria-label", () => t(name));
        controls[key] = input;
        controls.errors[key] = document.createElement("small");
        controls.errors[key].className = "config-error";
        controls.errors[key].hidden = true;
        input.addEventListener("input", () => {
          controls.auth.checked = true;
          if (key === "username") controls.inheritUsername=false;
          controls.testVersion = (controls.testVersion || 0) + 1;
          controls.result.hidden = true;
          changed();
        });
        entry.controls.push(input);
        label.append(input, controls.errors[key]);
        row.append(label);
      }
      auth.addEventListener("change", () => {
        controls.testVersion = (controls.testVersion || 0) + 1;
        controls.result.hidden = true;
        changed();
      });
      const reversePermission = document.createElement("div");
      reversePermission.className = "proxy-reverse-permission";
      const reverseLabel = document.createElement("label");
      const allowReverse = document.createElement("input");
      allowReverse.type = "checkbox";
      allowReverse.checked = value.allow_reverse === true;
      controls.allowReverse = allowReverse;
      const reverseCaption = document.createElement("span");
      localText(reverseCaption, () => t("允许服务器反向代理"));
      localAttr(allowReverse, "aria-label", () => t("允许服务器反向代理"));
      reverseLabel.append(allowReverse, reverseCaption);
      const reverseHelp = document.createElement("small");
      reverseHelp.className = "muted";
      localText(reverseHelp, () => t("默认关闭。开启后，服务器可通过本映射的通道访问客户端可达的目标服务。更改此选项并保存会重建本映射通道，断开其现有连接。"));
      allowReverse.addEventListener("change", () => {
        controls.testVersion = (controls.testVersion || 0) + 1;
        controls.result.hidden = true;
        changed();
      });
      entry.controls.push(allowReverse);
      reversePermission.append(reverseLabel, reverseHelp);
      row.append(reversePermission);
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "secondary proxy-remove";
      localText(remove, () => t("删除"));
      localAttr(remove, "aria-label", () => t("删除本地端口 ") + (value.listen || t("未填写")) + t(" 的映射"));
      remove.addEventListener("click", () => {
        const values = entry.read();
        values.splice(entry.rows.indexOf(controls), 1);
        draw(values);
        changed();
      });
      const test = document.createElement("button");
      test.type = "button";
      test.className = "secondary proxy-test";
      localText(test, () => controls.testing ? t("测试中…") : t("测试"));
      localAttr(test, "aria-label", () => t("测试映射连通性"));
      controls.testButton = test;
      controls.result = document.createElement("small");
      controls.result.className = "proxy-test-result";
      controls.result.setAttribute("role", "status");
      controls.result.setAttribute("aria-live", "polite");
      controls.result.hidden = true;
      test.addEventListener("click", () => testProxyMapping(controls));
      const actions = document.createElement("div");
      actions.className = "proxy-actions";
      actions.append(test, remove);
      entry.controls.push(test, remove);
      entry.rows.push(controls);
      row.append(actions, controls.result);
      list.append(row);
    }
  };
  entry.replaceMappings = (mappings) => { draw(mappings); entry.original=JSON.stringify(entry.read());entry.dirty=false; };
  entry.saved = () => {
    for (const row of entry.rows) {
      if (!row.auth.checked) row.savedPassword="";
      else if (row.password.value) row.savedPassword="__SGATE_KEEP_EXISTING_PASSWORD__";
      row.password.value="";
    }
    entry.original=JSON.stringify(entry.read());
  };
  draw(field.mappings || []);
  entry.original = JSON.stringify(entry.read());
  add.addEventListener("click", () => {
    draw([...entry.read(), { listen: "", server: "", addr: "" }]);
    changed();
  });
  saveButton.addEventListener("click", () => saveConfigField(entry));
  updateConfigSaveButton(entry);
  configInputs.push(entry);
  if (field.validation?.live) {
    const badge = document.createElement("span");
    badge.className = "config-live";
    localText(badge, () => t("支持在线修改"));
    header.append(title, badge, saveButton);
  } else header.append(title, saveButton);
  section.append(header, help, list, add);
  group.append(section);
}

function mappingCredentialError(row) {
  const username = configValueError("username",row.username.value,{type:"username"});
  if (username) return username;
  const password=row.password.value || row.savedPassword;
  if (!password) return "";
  return configValueError("password",password,{type:"password",max:"1024"});
}

// Keep testing separate from saving: unrelated drafts and mapping overrides
// must not be submitted, persisted, or silently substituted into the probe.
async function testProxyMapping(row) {
  if (row.testing || configSaving || serviceRestarting) return;
  try {
    if (!configAddress(row.listen.value, true)) throw new Error(t("映射监听端口须为 1～65535，且不能重复"));
    configURL(row.server.value, !row.addr.value);
    if (row.addr.value) configAddress(row.addr.value);
    if (row.auth?.checked) {
      const error = mappingCredentialError(row);
      if (error) throw new Error(error);
    }
  } catch (error) {
    row.result.hidden = false;
    row.result.classList.toggle("bad", true);
    localText(row.result, () => errorText(error));
    return;
  }
  const version = row.testVersion || 0;
  row.testing = true;
  row.testButton.disabled = true;
  row.result.hidden = false;
  row.result.classList.toggle("bad", false);
  localText(row.testButton, () => row.testing ? t("测试中…") : t("测试"));
  localText(row.result, () => t("正在测试隧道和后端 TCP 连通性…"));
  try {
    const result = await api("proxy-test", {
      method: "POST",
      body: JSON.stringify({ listen: row.listen.value, server: row.server.value, addr: row.addr.value,
        ...(row.allowReverse?.checked ? {allow_reverse:true} : {}),
        ...(row.auth?.checked ? {
          ...(row.inheritUsername && !row.username.value ? {} : {username:row.username.value}),
          ...(row.password.value || row.savedPassword ? {password:row.password.value || row.savedPassword} : {}),
        } : {}) }),
    });
    if ((row.testVersion || 0) !== version) return;
    row.result.classList.toggle("bad", !result.ok);
    localText(row.result, () => (result.ok ? t("连通成功") : t("连通失败")) + " · " +
      (result.stage === "backend" ? t("后端 TCP") : result.stage === "command" ? t("测试命令") : result.stage === "config" ? t("客户端配置") : t("隧道连接")) + " · " +
      t(result.detail) +
      (result.target ? " · " + t("目标：") + result.target : "") +
      (result.error && result.error !== result.detail ? " · " + t("错误：") + t(result.error) : "") +
      " · " + result.elapsed_ms + " ms" +
      (result.test_id ? " · " + t("测试编号：") + result.test_id : ""));
  } catch (error) {
    if ((row.testVersion || 0) !== version) return;
    row.result.classList.toggle("bad", true);
    localText(row.result, () => t("测试失败：") + errorText(error));
  } finally {
    row.testing = false;
    row.testButton.disabled = configSaving || serviceRestarting;
    localText(row.testButton, () => t("测试"));
  }
}

function configEdits() {
  const edits = {};
  for (const entry of configInputs) {
    if (entry.kind === "proxies") {
      if (entry.dirty) edits.proxies = entry.read();
    } else if (entry.dirty && (entry.secret || entry.input.value !== entry.original)) {
      edits[entry.key] = entry.input.value;
    }
  }
  return edits;
}

function setConfigBusy(busy) {
  for (const entry of configInputs) {
    if (entry.kind === "proxies") {
      entry.controls.forEach((control) => (control.disabled = busy));
      entry.rows.forEach((row) => { if (row.testing) row.testButton.disabled = true; });
    }
    else entry.input.disabled = busy;
    updateConfigSaveButton(entry, busy);
  }
  for (const id of ["save-config", "apply-config", "reload-config"]) $(id).disabled = busy;
}

function updateConfigSaveButton(entry, busy = false) {
  entry.saveButton.disabled = busy || !entry.dirty;
  localAttr(entry.saveButton, "title", () => entry.dirty ? t("保存此项") : t("未修改"));
}

// 单项存盘不重新加载表单，其他行尚未保存的编辑与密码输入继续保留。
async function saveConfigField(entry) {
  if (configSaving || serviceRestarting) return;
  if (!validateConfigForm(entry)) return;
  if (
    entry.kind === "proxies"
      ? !entry.controls.every((control) => !control.reportValidity || control.reportValidity())
      : !entry.input.reportValidity()
  )
    return;
  if (
    !entry.dirty ||
    (entry.kind !== "proxies" && !entry.secret && entry.input.value === entry.original)
  ) {
    notice(() => entry.key + t(" 没有需要存盘的修改。"));
    return;
  }
  const value = entry.kind === "proxies" ? entry.read() : entry.input.value;
  configSaving = true;
  setConfigBusy(true);
  entry.saveButton.setAttribute("aria-busy", "true");
  try {
    const result = await api("config", {
      method: "PUT",
      body: JSON.stringify(configPayload({ [entry.key]: value }, false)),
    });
    revision = result.revision;
    if (result.proxy_mappings) {
      const mappings = configInputs.find(item => item.kind === "proxies");
      if (mappings && mappings !== entry && !mappings.dirty) mappings.replaceMappings(result.proxy_mappings);
    }
    // 后端把框架监听与映射一起保存；只刷新关联值，保留其他行的草稿。
    for (const [key, saved] of Object.entries(result.synced_fields || {})) {
      const linked = configInputs.find((item) => item.key === key);
      if (!linked || linked.kind === "proxies" || linked.secret) continue;
      if (!linked.dirty) linked.input.value = saved;
      linked.original = saved;
      linked.dirty = linked.input.value !== saved;
      updateConfigSaveButton(linked, configSaving || serviceRestarting);
    }
    if (entry.kind === "proxies") entry.saved();
    else entry.original = entry.input.value = entry.secret ? "" : value;
    entry.dirty = false;
    const pending = result.requires_restart !== false;
    localText($("pending-label"), () => ((Object.keys(configEdits()).length ? t("有未保存修改 · ") : "") +
      (pending ? t("已保存项待应用") : t("已保存项已生效"))));
    if (result.runtime) renderRuntime(result.runtime);
    if (result.service_restart_required) await restartSavedMappings(result);
    else notice(() => configSaveMessage(result, entry.key) + t("其他未保存修改已保留。"));
  } catch (error) {
    notice(() => errorText(error), true);
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
      const error = new Error(t(data.error) || t("请求失败"));
      error.status = r.status;
      throw error;
    }
    return data;
  } catch (e) {
    if (e.name === "AbortError") throw new Error(t("请求超时，请重试"));
    throw e;
  } finally {
    clearTimeout(timer);
  }
}
function notice(text, bad = false) {
  $("notice").hidden = false;
  localText($("notice"), () => t(typeof text === "function" ? text() : text));
  $("notice").classList.toggle("bad", bad);
}
function errorText(e) {
  return t(e.message) || t("网络连接失败，请稍后重试");
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
  if (n >= 86400) return Math.floor(n / 86400) + t(" 天 ") + Math.floor((n % 86400) / 3600) + t(" 小时");
  if (n >= 3600) return Math.floor(n / 3600) + t(" 小时 ") + Math.floor((n % 3600) / 60) + t(" 分");
  return Math.floor(n / 60) + t(" 分 ") + (n % 60) + t(" 秒");
}
async function refreshAuth() {
  const authEpoch = ++epoch;
  const status = await api("status");
  if (authEpoch !== epoch) return;
  state = status;
  $("dashboard").hidden = !state.authenticated;
  $("auth").hidden = state.authenticated;
  localText($("auth-error"), () => "");
  if (!state.authenticated) {
    renderSignedOut();
    return;
  }
  $("tunnel-accounts-nav").hidden = state.role !== "server";
  localText($("role-title"), () => state.role === "server" ? "SERVER" : "CLIENT");
  localText($("side-role"), () => state.role === "server" ? t("服务端代理") : t("客户端代理"));
  localText($("user-label"), () => state.username);
  localText($("session-user"), () => state.username);
  $("account-user").value = state.username;
  localText($("session-duration"), () => uptime(Math.floor(state.session_timeout_seconds)));
  localText($("session-expiry"), () => new Date(state.expires).toLocaleString(sgateI18n.language()));
  previous = null;
  points = [];
  chartPointer = null;
  $("traffic-tooltip").hidden = true;
  try {
    await loadConfig();
  } catch (e) {
    notice(() => errorText(e), true);
  }
  const run = epoch;
  poll(run);
}

function renderSignedOut() {
  tunnelAccountsLoad++;
  tunnelAccountsRevision = "";
  tunnelAccountRows = [];
  tunnelAccountsEnabled = false;
  tunnelAccountsCanCreate = false;
  $("tunnel-accounts-nav").hidden = true;
  $("tunnel-account-list").replaceChildren();
  $("tunnel-account-create-mappings").replaceChildren();
  tunnelAccountCreateMappings = null;
  $("tunnel-account-name").value = $("tunnel-account-secret").value = "";
  localText($("tunnel-accounts-path"), () => "");
  // 退出后也清除隐藏页面中的配置草稿、密码和统计，避免共享浏览器继续显示。
  configInputs = [];
  revision = "";
  servicePID = 0;
  serverTunnelListeners = [];
  previous = null;
  points = [];
  $("traffic-ip").value = "";
  renderTrafficPeers({ by_ip: [] });
  chartPointer = null;
  $("traffic-tooltip").hidden = true;
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
  for (const id of [
    "user-label",
    "session-user",
    "session-expiry",
    "config-path",
    "runtime-state",
    "noise-totals",
    "noise-observation",
    "noise-activity",
  ])
    localText($(id), () => "");
  $("notice").hidden = true;
  localText($("auth-error"), () => "");
  $("dashboard").hidden = true;
  $("auth").hidden = false;
  $("login-form").hidden = false;
  const setup = !!state.setup_required;
  localText($("auth-title"), () => setup ? t("创建首个管理员") : t("欢迎回来"));
  localText($("auth-help"), () => setup
    ? t("设置独立管理账号。初始化令牌位于管理配置指定的 ") + state.setup_token_file + t(" 文件中。")
    : t("登录后查看实时流量并管理代理配置。"));
  $("confirm-row").hidden = $("token-row").hidden = !setup;
  $("login-pass").minLength = setup ? 12 : 1;
  $("login-pass").autocomplete = setup ? "new-password" : "current-password";
  $("login-confirm").required = $("setup-token").required = setup;
  localText($("login-button"), () => setup ? t("创建账号并进入控制台 ↗") : t("登录控制台 ↗"));
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
    localText($("auth-help"), () => t("已安全退出，服务器上的本次登录会话已清理。"));
  } catch (error) {
    notice(() => errorText(error), true);
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
    throw new Error(t("配置表单需要新版程序，请更新并重启 sgate 服务。"));
  revision = data.revision;
  renderConfigFields(data.fields || [], data.runtime || {}, data.role);
  localText($("config-path"), () => data.path);
  localText($("pending-label"), () => data.requires_restart ? t("已保存 · 待应用") : t("配置文件已同步"));
  renderRuntime(data.runtime || {});
}
function renderRuntime(rt) {
  if (Array.isArray(rt.websocket_listeners)) {
    serverTunnelListeners = rt.websocket_listeners;
    validateAllReverseMappings(false);
  }
  servicePendingListeners = rt.pending_listeners || [];
  servicePID = rt.pid || 0;
  $("restart-service").hidden = $("restart-service-help").hidden = !rt.service_restart_available;
  $("restart-service").disabled = serviceRestarting || !!rt.service_restart_pending;
  localText($("runtime-state"), () => (t("代理状态：") +
    (rt.running ? t("运行中") : t("未运行")) +
    t(" · 配置代次 ") +
    (rt.generation || "—") +
    t(" · 生效监听 ") +
    ((rt.listeners?.length ? rt.listeners.join(", ") : rt.listen) || "—") +
    (rt.cli_overrides?.length ? t(" · 命令行优先项：") + rt.cli_overrides.join(", ") : "") +
    (rt.listener_owner === "acl_master" ? t(" · 监听由 acl_master 管理") : "") +
    (rt.last_error ? " · " + rt.last_error : "") +
    noiseDescription(rt.noise)));
}
// 显示当前运行参数，避免把只保存的文件值误认为已经生效。
function noiseDescription(noise) {
  if (!noise) return "";
  const wait =
    noise.mode === "random" ? noise.interval_min + "～" + noise.interval_max : noise.interval;
  return (
    t(" · 本端白噪音：") +
    (noise.mode === "off"
      ? t("关闭")
      : wait +
        t("，每次 ") +
        units(noise.min_bytes) +
        "～" +
        units(noise.max_bytes) +
        t("；传输中 ") +
        (noise.traffic === false ? t("不发") : t("发送")) +
        t("，空闲 ") +
        (noise.idle === false ? t("不发") : t("发送")) +
        t("，无连接 ") +
        (noise.empty === false ? t("不发") : t("发送")) +
        t("；空闲阈值 ") +
        (noise.idle_after || "1s"))
  );
}
async function poll(run) {
  if (run !== epoch || !state.authenticated) return;
  try {
    const data = await api("stats");
    if (run !== epoch) return;
    const rates = trafficRates(data, previous);
    if (rates) rates.byIP = peerTrafficRates(data, previous);
    renderTrafficPeers(data);
    if (!rates && previous) points = []; // 重启或计数回退后清除上一进程的时间序列。
    previous = data;
    if (rates) {
      points.push(rates);
      points = points.filter((point) => point.time_ms >= rates.time_ms - 60000).slice(-120);
    }
    localText($("tx-rate"), () => rates ? units(rates.tx, true) : t("采样中"));
    localText($("rx-rate"), () => rates ? units(rates.rx, true) : t("采样中"));
    localText($("noise-observation"), () => noiseObservationText(data, rates));
    localText($("noise-activity"), () => noiseActivityText(data.noise_activity));
    localText($("tx-total"), () => t("累计发送 ") + units(data.tx_bytes));
    localText($("rx-total"), () => t("累计接收 ") + units(data.rx_bytes));
    localText($("noise-totals"), () => (t("噪音负载累计：发送 ") +
      units(data.noise_tx_bytes || 0) +
      t(" · 接收 ") +
      units(data.noise_rx_bytes || 0) +
      t("（收发两端独立配置）")));
    localText($("connections"), () => data.connections);
    localText($("uptime"), () => uptime(data.uptime_seconds));
    localText($("live-status"), () => t("● 实时监测"));
    drawChart();
  } catch (e) {
    if (run === epoch) localText($("live-status"), () => t("● 数据更新中断"));
  }
  if (run === epoch && state.authenticated)
    setTimeout(() => poll(run), document.hidden ? 5000 : 1000);
}
// IP filtering affects the chart; summary cards retain process totals.
function peerTrafficRates(data, last) {
  const before = new Map((last?.by_ip || []).map((peer) => [peer.ip, peer]));
  const rates = Object.create(null);
  for (const peer of data.by_ip || []) {
    const old = before.get(peer.ip);
    rates[peer.ip] = trafficRates(
      { ...peer, time_ms: data.time_ms },
      old ? { ...old, time_ms: last.time_ms } : null,
    );
  }
  return rates;
}
function peerLabel(ip) {
  return ip === "__other__" ? t("其他 IP（超出统计上限）")
    : ip === "__unknown__" ? t("未知对端") : ip;
}
function renderTrafficPeers(data) {
  const select = $("traffic-ip");
  const selected = select.value || "";
  select.replaceChildren();
  const option = (value, text) => {
    const node = document.createElement("option");
    node.value = value;
    localText(node, () => text());
    select.append(node);
  };
  option("", () => t("全部连接 IP"));
  const peers = data.by_ip || [];
  for (const peer of peers) option(peer.ip, () => peerLabel(peer.ip) + " · " + peer.connections + t(" 个连接"));
  if (selected && !peers.some((peer) => peer.ip === selected))
    option(selected, () => peerLabel(selected) + t("（已离线）"));
  select.value = selected;
  renderTrafficScope(data);
}
function renderTrafficScope(data = previous) {
  const ip = $("traffic-ip").value;
  const peer = data?.by_ip?.find((peer) => peer.ip === ip);
  localText($("traffic-ip-summary"), () => ip
    ? peerLabel(ip) + " · " + (peer?.connections || 0) + t(" 个连接") +
      t(" · 累计发送 ") + units(peer?.tx_bytes || 0) + t(" · 累计接收 ") + units(peer?.rx_bytes || 0)
    : t("全部连接 IP"));
}
function chartSamples() {
  const ip = $("traffic-ip").value;
  if (!ip) return points;
  return points.map((point) => point.byIP?.[ip] || { time_ms: point.time_ms });
}
// 独立计算细分负载速率，不能从总流量曲线的高度推断噪音有无。
function trafficRates(data, last) {
  if (
    !last ||
    data.time_ms <= last.time_ms ||
    data.tx_bytes < last.tx_bytes ||
    data.rx_bytes < last.rx_bytes
  )
    return null;
  const seconds = (data.time_ms - last.time_ms) / 1000;
  const rate = (key) =>
    typeof data[key] === "number" && typeof last[key] === "number" && data[key] >= last[key]
      ? (data[key] - last[key]) / seconds
      : null;
  return {
    time_ms: data.time_ms,
    tx: rate("tx_bytes"),
    rx: rate("rx_bytes"),
    noiseTX: rate("noise_tx_bytes"),
    noiseRX: rate("noise_rx_bytes"),
    businessTX: rate("business_tx_bytes"),
    businessRX: rate("business_rx_bytes"),
  };
}
function noiseObservationText(data, rates) {
  const value = (v) => (v == null ? t("采样中/未提供") : units(v, true));
  return (
    t("白噪音：发送 ") +
    value(rates?.noiseTX) +
    t(" · 接收 ") +
    value(rates?.noiseRX) +
    t("；累计发送 ") +
    units(data.noise_tx_bytes || 0) +
    t(" · 接收 ") +
    units(data.noise_rx_bytes || 0) +
    t("。业务负载：发送 ") +
    value(rates?.businessTX) +
    t(" · 接收 ") +
    value(rates?.businessRX) +
    t("。负载统计不含协议开销，包含通道重发。")
  );
}
function noiseActivityText(activity) {
  if (!activity) return t("当前运行程序未提供隧道忙闲状态，更新程序并重启后可查看暂停原因。");
  const offline = activity.offline
    ? t("离线保留 ") +
      activity.offline +
      t(" 个会话、") +
      activity.offline_streams +
      t(" 条业务连接，等待客户端重连；离线期间无法发送噪音。")
    : "";
  const disconnected = activity.last_disconnect
    ? t("最近隧道断开（") +
      new Date(activity.last_disconnect_ms).toLocaleTimeString(sgateI18n.language(), { hour12: false }) +
      "）：" +
      activity.last_disconnect +
      t("。")
    : "";
  if (!activity.tunnels) return t("没有在线隧道，无法发送白噪音。") + offline + disconnected;
  const buffered = activity.receive_buffer_bytes
    ? t("业务应用待接收 ") + units(activity.receive_buffer_bytes) + t("；隧道仍可读取噪音和心跳。")
    : "";
  const reasons = [
    ["suppressed_traffic", t("业务活跃暂停")],
    ["suppressed_idle", t("空闲策略关闭")],
    ["suppressed_empty", t("无业务连接策略关闭")],
    ["disabled", t("发送间隔关闭")],
  ]
    .filter(([key]) => activity[key] > 0)
    .map(([key, label]) => label + " " + activity[key] + t(" 条"));
  return (
    t("在线隧道 ") +
    activity.tunnels +
    t(" 条：业务活跃 ") +
    activity.traffic +
    t("，空闲 ") +
    activity.idle +
    t("，无业务连接 ") +
    activity.empty +
    t("；当前允许噪音 ") +
    activity.permitted +
    t(" 条。") +
    offline +
    buffered +
    (reasons.length
      ? t("暂停原因：") + reasons.join(t("、")) + t("。")
      : t("按配置间隔发送，采样间隙可能为零。")) +
    (activity.suppressed_traffic ? t("少量后台业务数据也会刷新空闲阈值。") : "")
  );
}
function chartMetric() {
  switch ($("traffic-metric").value) {
    case "noise":
      return { keys: ["noiseTX", "noiseRX"], title: t("白噪音负载带宽") };
    case "business":
      return { keys: ["businessTX", "businessRX"], title: t("业务负载带宽") };
    default:
      return { keys: ["tx", "rx"], title: t("实时网络带宽") };
  }
}
// 绘制与命中使用同一套 CSS 像素坐标，不受高分屏 canvas 缩放影响。
function chartGeometry(w, h) {
  const left = 67,
    top = 12,
    right = w - 32,
    bottom = h - 32;
  return { left, top, right, bottom, height: bottom - top, width: right - left };
}
// 统一使用采样时间映射横坐标；后台5秒采样和请求延迟不会压缩时间。
function chartTimeRange(samples = chartSamples()) {
  const end = samples.at(-1)?.time_ms ?? Date.now();
  return { start: end - 60000, end };
}
function chartPointX(point, geometry, range = chartTimeRange()) {
  return geometry.left + ((point.time_ms - range.start) / 60000) * geometry.width;
}
function chartPointIndex(pointer, geometry, samples = chartSamples()) {
  if (
    !pointer ||
    !samples.length ||
    geometry.width <= 0 ||
    pointer.x < geometry.left ||
    pointer.x > geometry.right ||
    pointer.y < geometry.top ||
    pointer.y > geometry.bottom
  )
    return -1;
  const range = chartTimeRange(samples);
  const time = range.start + ((pointer.x - geometry.left) / geometry.width) * 60000;
  if (time < samples[0].time_ms - 30000 / geometry.width || time > range.end) return -1;
  let nearest = -1,
    distance = Infinity;
  samples.forEach((point, index) => {
    if (point.time_ms < range.start || point.time_ms > range.end) return;
    const next = Math.abs(point.time_ms - time);
    if (next < distance) {
      nearest = index;
      distance = next;
    }
  });
  return nearest;
}
function drawChartTimeAxis(c, geometry) {
  const range = chartTimeRange();
  const first = Math.ceil(range.start / 5000) * 5000;
  const labelEvery = Math.max(1, Math.ceil(48 / (geometry.width / 12)));
  c.fillStyle = "#8a98a3";
  c.strokeStyle = "#dce5e9";
  for (let time = first, index = 0; time <= range.end; time += 5000, index++) {
    const x = chartPointX({ time_ms: time }, geometry, range);
    c.beginPath();
    c.moveTo(x, geometry.bottom);
    c.lineTo(x, geometry.bottom + 5);
    c.stroke();
    // 窄屏仍保留5秒刻度，仅减少文字以免重叠。
    if (index % labelEvery) continue;
    c.textAlign = "center";
    c.fillText(new Date(time).toLocaleTimeString(sgateI18n.language(), { hour12: false }), x, geometry.bottom + 20);
  }
}
function chartHoverRate(value) {
  if (!Number.isFinite(value)) return t("未提供");
  return value < 1024
    ? value.toLocaleString(sgateI18n.language(), { maximumSignificantDigits: 4 }) + " B/s"
    : units(value, true);
}
function drawChartHover(c, geometry, metric, max) {
  const tooltip = $("traffic-tooltip");
  const index = chartPointIndex(chartPointer, geometry);
  const point = chartSamples()[index];
  if (!point || !metric.keys.some((key) => Number.isFinite(point[key]))) {
    tooltip.hidden = true;
    return;
  }
  const x = chartPointX(point, geometry);
  const y = (value) => geometry.bottom - (value / max) * geometry.height;
  c.beginPath();
  c.setLineDash([3, 3]);
  c.strokeStyle = "#8a98a3";
  c.lineWidth = 1;
  c.moveTo(x, geometry.top);
  c.lineTo(x, geometry.bottom);
  c.stroke();
  c.setLineDash([]);
  for (const [key, color] of [
    [metric.keys[0], "#087e68"],
    [metric.keys[1], "#5a83d7"],
  ]) {
    if (!Number.isFinite(point[key])) continue;
    c.beginPath();
    c.arc(x, y(point[key]), 4, 0, Math.PI * 2);
    c.fillStyle = color;
    c.fill();
  }
  const time = Number.isFinite(point.time_ms)
    ? new Date(point.time_ms).toLocaleTimeString(sgateI18n.language(), { hour12: false })
    : t("时间未提供");
  localText($("traffic-tooltip-time"), () => t("采样时间 ") + time);
  localText($("traffic-tooltip-tx"), () => chartHoverRate(point[metric.keys[0]]));
  localText($("traffic-tooltip-rx"), () => chartHoverRate(point[metric.keys[1]]));
  tooltip.hidden = false;
  // 靠近边缘时将提示框收进图内，避免遮挡或溢出页面。
  const width = tooltip.offsetWidth,
    height = tooltip.offsetHeight;
  const anchorY = Math.min(
    ...metric.keys
      .map((key) => point[key])
      .filter(Number.isFinite)
      .map(y),
  );
  const top = anchorY - height - 12 >= 8 ? anchorY - height - 12 : anchorY + 12;
  tooltip.style.left = Math.max(8, Math.min(x + 12, geometry.right + 8 - width - 8)) + "px";
  tooltip.style.top = Math.max(8, Math.min(top, geometry.bottom + 10 - height - 8)) + "px";
}
function clearChartHover() {
  const active = chartPointer !== null;
  chartPointer = null;
  $("traffic-tooltip").hidden = true;
  if (active) drawChart();
}
$("traffic-chart").addEventListener("pointermove", (event) => {
  const canvas = $("traffic-chart"),
    rect = canvas.getBoundingClientRect();
  if (!rect.width || !rect.height) {
    clearChartHover();
    return;
  }
  const next = {
    x: ((event.clientX - rect.left) * canvas.clientWidth) / rect.width,
    y: ((event.clientY - rect.top) * canvas.clientHeight) / rect.height,
  };
  const geometry = chartGeometry(canvas.clientWidth, canvas.clientHeight);
  const changed = chartPointIndex(next, geometry) !== chartPointIndex(chartPointer, geometry);
  chartPointer = next;
  // 提示框锚定采样点；同一点内移动无需反复重建高分屏画布。
  if (!changed) return;
  drawChart();
});
$("traffic-chart").addEventListener("pointerleave", clearChartHover);
$("traffic-chart").addEventListener("pointercancel", clearChartHover);
window.addEventListener("blur", clearChartHover);
function drawChart() {
  const metric = chartMetric();
  localText($("traffic-chart-title"), () => t(metric.title));
  const canvas = $("traffic-chart");
  if (canvas.clientWidth === 0) return;
  const ratio = window.devicePixelRatio || 1,
    w = canvas.clientWidth,
    h = canvas.clientHeight;
  canvas.width = Math.round(w * ratio);
  canvas.height = Math.round(h * ratio);
  const c = canvas.getContext("2d");
  c.scale(ratio, ratio);
  const geometry = chartGeometry(w, h);
  const { left, top, right, bottom, height, width } = geometry;
  const max =
    Math.max(
      1,
      ...chartSamples().flatMap((v) => metric.keys.map((key) => v[key]).filter(Number.isFinite)),
    ) * 1.18;
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
  drawChartTimeAxis(c, geometry);
  for (const [key, color] of [
    [metric.keys[1], "#5a83d7"],
    [metric.keys[0], "#087e68"],
  ]) {
    const segments = [[]];
    chartSamples().forEach((v) => {
      if (!Number.isFinite(v[key]) || v.time_ms < chartTimeRange().start) {
        if (segments.at(-1).length) segments.push([]);
        return;
      }
      segments.at(-1).push([chartPointX(v, geometry), bottom - (v[key] / max) * height]);
    });
    for (const coords of segments.filter((v) => v.length)) {
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
  drawChartHover(c, geometry, metric, max);
}
$("traffic-metric").addEventListener("change", drawChart);
$("traffic-ip").addEventListener("change", () => {
  clearChartHover();
  renderTrafficScope();
  drawChart();
});
$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  localText($("auth-error"), () => "");
  if (state.setup_required && $("login-pass").value !== $("login-confirm").value) {
    localText($("auth-error"), () => t("两次输入的密码不一致"));
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
    localText($("auth-error"), () => errorText(e));
  } finally {
    b.disabled = false;
  }
});
$("logout").addEventListener("click", logoutAdmin);
document.querySelectorAll("[data-tab]").forEach((b) =>
  b.addEventListener("click", () => {
    document.querySelectorAll("[data-tab]").forEach((x) => x.classList.toggle("selected", x === b));
    document.querySelectorAll(".tab").forEach((x) => (x.hidden = x.id !== b.dataset.tab));
    localText($("page-title"), () => ({
      overview: t("实时概览"),
      configuration: t("代理配置"),
      account: t("账号与会话"),
      "tunnel-accounts":t("客户端账号"),
    }[b.dataset.tab]));
    if (b.dataset.tab === "tunnel-accounts") loadTunnelAccounts().catch((e) => notice(() => errorText(e), true));
    drawChart();
  }),
);
$("reload-config").addEventListener("click", () =>
  loadConfig().catch((e) => notice(() => errorText(e), true)),
);
// 以服务端实际应用结果提示，不根据点击的按钮猜测是否发生重启。
function configSaveMessage(result, key = t("配置")) {
  if (result.applied) return key + t("已应用，业务代理已重启。管理登录继续有效。");
  if (result.live_applied?.length) {
    const overrides = result.runtime?.cli_overrides || [];
    const covered = result.live_applied.filter((name) => overrides.includes(name));
    return (
      key +
      t("已保存，在线参数已更新，无需重启代理。") +
      (covered.length ? t("命令行覆盖项仍使用启动值：") + covered.join(t("、")) + t("。") : "") +
      (result.requires_restart ? t("其他配置仍待应用或重启。") : "")
    );
  }
  return key + t("已校验并保存，尚未重启代理。");
}
async function saveConfig(apply) {
  if (serviceRestarting || configSaving) return;
  if (!validateConfigForm()) return;
  if (!$("config-form").reportValidity()) return;
  const fields = configEdits();
  configSaving = true;
  setConfigBusy(true);
  try {
    const result = await api("config", {
      method: "PUT",
      body: JSON.stringify(configPayload(fields, apply)),
    });
    if (result.service_restart_required) await restartSavedMappings(result);
    await loadConfig();
    if (!result.service_restart_required) notice(() => configSaveMessage(result));
  } catch (e) {
    notice(() => errorText(e), true);
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

// A saved managed port change needs master_ctl, because the parent still owns
// inherited sockets. Wait for a new running PID; do not discard other drafts.
async function restartSavedMappings(result) {
  const oldPID = result.runtime?.pid || servicePID;
  if (!oldPID) throw new Error(t("无法确定服务进程，配置已保存，请刷新后重试重启"));
  serviceRestarting = true;
  setConfigBusy(true);
  notice(() => state.role === "server"
    ? t("反向映射已保存，正在调用服务端停止和启动脚本，等待监听端口恢复…")
    : t("映射已保存，正在调用客户端停止和启动脚本，等待监听端口恢复…"));
  try {
    try {
      await api("restart-service", {method: "POST", body: JSON.stringify({expected_pid: oldPID})});
    } catch (error) { if (error.status) throw error; }
    const config = await waitForServiceRestart(oldPID, result.runtime?.pending_listeners || []);
    revision = config.revision || revision;
    if (config.runtime) renderRuntime(config.runtime);
    localText($("pending-label"), () => (Object.keys(configEdits()).length ? t("有未保存修改 · ") : "") + t("已保存项已生效"));
    notice(() => state.role === "server"
      ? t("反向映射已生效，服务端已通过停止和启动脚本重启，监听端口已核对。其他未保存修改已保留。")
      : t("映射已生效，客户端已通过停止和启动脚本重启，监听端口已核对，登录会话继续有效。其他未保存修改已保留。"));
  } finally {
    serviceRestarting = false;
    setConfigBusy(configSaving);
  }
}

async function restartService() {
  if (serviceRestarting || configSaving || !servicePID) return;
  const oldPID = servicePID;
  const expectedListeners = servicePendingListeners;
  const buttons = [$("restart-service"), $("save-config"), $("apply-config")];
  serviceRestarting = true;
  buttons.forEach((button) => (button.disabled = true));
  notice(() => t("正在请求 acl_master 重启服务，等待管理连接恢复…"));
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
    await waitForServiceRestart(oldPID, expectedListeners);
    await refreshAuth();
    if (state.authenticated) notice(() => t("服务已由 acl_master 重启，登录会话继续有效。"));
  } catch (error) {
    notice(() => errorText(error), true);
  } finally {
    serviceRestarting = false;
    buttons.forEach((button) => (button.disabled = false));
  }
}

// Wildcard framework listeners may expand to multiple interface addresses.
function restartListenersReady(expected, actual) {
  const endpoint = (value) => {
    const at = value.lastIndexOf(":");
    return { host: value.slice(0, at).replace(/^\[|\]$/g, ""), port: value.slice(at + 1) };
  };
  const wanted = expected.map(endpoint), bound = (actual || []).map(endpoint);
  const matches = (w, b) => w.port === b.port && (
    w.host === b.host || ["", "0.0.0.0", "::"].includes(w.host) ||
    (w.host === "localhost" && (b.host === "::1" || b.host.startsWith("127.")))
  );
  return wanted.every(w => bound.some(b => matches(w, b))) &&
    bound.every(b => wanted.some(w => w.port === b.port));
}
async function waitForServiceRestart(oldPID, expectedListeners = []) {
  const deadline = Date.now() + 90000;
  while (Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 1000));
    let runtime, config;
    try {
      config = await api("config");
      runtime = config.runtime || {};
    } catch (error) {
      if (error.status || !state.authenticated) throw error;
      // HTTP 监听重建期间继续轮询；只有新 PID 就绪才报告完成。
      continue;
    }
    if (runtime.pid && runtime.pid !== oldPID && runtime.running) {
      if (expectedListeners.length && !restartListenersReady(expectedListeners, runtime.listeners))
        throw new Error(t("服务已重启，但监听端口与已保存映射不一致，请检查启动脚本和 master_service。"));
      return config;
    }
    if (!runtime.service_restart_pending && runtime.last_error) {
      throw new Error(runtime.last_error);
    }
  }
  throw new Error(
    t("等待服务恢复超时，请检查 acl_master 日志；若管理地址或证书已更改，请打开新地址。"),
  );
}
$("account-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if ($("account-password").value !== $("account-confirm").value) {
    notice(() => t("两次输入的新密码不一致"), true);
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
    notice(() => errorText(e), true);
  } finally {
    b.disabled = false;
  }
});
window.addEventListener("resize", () => {
  chartPointer = null;
  drawChart();
});
sgateI18n.init(() => {
  validateAllReverseMappings(false);
  validateConfigForm(null, false);
  drawChart();
});
refreshAuth().catch((e) => {
  localText($("auth-title"), () => t("无法连接管理服务"));
  localText($("auth-help"), () => errorText(e));
});


// Each account owns its mapping draft; unrelated saves must not discard it.
function reverseMappingsDirty(row) {
  return JSON.stringify(row.reverse.read()) !== JSON.stringify(row.originalMappings);
}
function reverseMappingAddressError(value, listen) {
  if (!value) return t("请填写地址和端口");
  if (/\s/.test(value)) return t("地址不能包含空白字符");
  try { configAddress(value); } catch (error) { return error.message; }
  if (listen) {
    const match = /^(?:\[([^\]]+)\]|(\d+\.\d+\.\d+\.\d+)):(\d+)$/.exec(value);
    if (!match || String(Number(match[3])) !== match[3]) return t("监听地址须为明确的 IP 和 1～65535 的端口");
    if (match[2] && match[2].split(".").some((part) => String(Number(part)) !== part)) return t("监听地址须为明确的 IP 和 1～65535 的端口");
    if (match[1] && new URL("http://[" + match[1] + "]").hostname !== "[" + match[1] + "]") return t("请使用规范的 IPv6 监听地址，例如 [::1]:10022");
  }
  return "";
}
function reverseTunnelPortConflict(value) {
  const port = value.slice(value.lastIndexOf(":") + 1);
  const configured = configInputs.find(item => item.key === "listen");
  const listeners = [...serverTunnelListeners, configured?.original || ""];
  return listeners.flatMap(raw => raw.split(/[,;\s]+/)).some(address => {
    const normalized = address.replaceAll("|", ":");
    return Number(normalized.slice(normalized.lastIndexOf(":") + 1)) === Number(port);
  });
}
function validateReverseMappings(editor, username, show) {
  if (!editor) return true;
  const seen = new Set(tunnelAccountRows.filter((row) => row.username !== username)
    .flatMap((row) => row.originalMappings.map((mapping) => mapping.listen)));
  let valid = true;
  for (const row of editor.rows) {
    for (const key of ["listen", "target"]) {
      let error = reverseMappingAddressError(row[key].value, key === "listen");
      if (key === "listen" && !error) {
        if (reverseTunnelPortConflict(row.listen.value)) error = t("反向代理监听端口不能与 WebSocket 隧道服务端口相同");
        else if (seen.has(row.listen.value)) error = t("监听地址已被其他映射使用");
        seen.add(row.listen.value);
      }
      row[key].setCustomValidity(error);
      row.errors[key].hidden = !error || (!show && !row[key].value);
      localText(row.errors[key], () => reverseMappingAddressError(row[key].value, key === "listen") || (error ? (reverseTunnelPortConflict(row.listen.value) ? t("反向代理监听端口不能与 WebSocket 隧道服务端口相同") : t("监听地址已被其他映射使用")) : ""));
      if (error) {
        valid = false;
        if (show && !editor.firstInvalid) editor.firstInvalid = row[key];
      }
    }
    for (const key of ["tlsCert","tlsKey"]) {
      if (!row[key]) continue;
      let error="";
      if (row.listenTLS.checked && !row.reuseCert.checked) {
        if (!row[key].value) error=t("开启独立 SSL/TLS 证书时，必须填写证书和私钥路径");
        else if (row[key].value.trim() !== row[key].value || /[\r\n\x00]/.test(row[key].value)) error=t("证书路径不能包含首尾空白、换行或空字符");
      }
      row[key].setCustomValidity(error);row.errors[key].hidden=!error;
      localText(row.errors[key],()=>error);
      if (error) {valid=false;if(show && !editor.firstInvalid)editor.firstInvalid=row[key]}
    }
  }
  return valid;
}
function validateAllReverseMappings(show) {
  validateReverseMappings(tunnelAccountCreateMappings, "", show);
  for (const row of tunnelAccountRows) validateReverseMappings(row.reverse, row.username, show);
}
function createReverseMappingsEditor(values, changed) {
  const element = document.createElement("div");
  element.className = "reverse-editor";
  const header = document.createElement("div");
  header.className = "reverse-editor-heading";
  const title = document.createElement("strong");
  localText(title, () => t("反向代理端口映射"));
  const count = document.createElement("span");
  count.className = "muted";
  const addButton = document.createElement("button");
  addButton.type = "button";
  addButton.className = "secondary";
  localText(addButton, () => t("＋ 添加反向映射"));
  header.append(title, count, addButton);
  const help = document.createElement("p");
  help.className = "muted reverse-help";
  localText(help, () => t("服务端监听接入端口，通过该账号的客户端访问目标地址。目标可为客户端回环、局域网 IP 或主机名。"));
  const list = document.createElement("div");
  const empty = document.createElement("p");
  empty.className = "muted reverse-empty";
  localText(empty, () => t("尚未配置反向映射。保存时同步框架监听；托管模式端口变化会自动停止并启动服务端，中断当前业务连接。"));
  element.append(header, help, list, empty);
  const editor = {element, rows:[], addButton, busy:false};
  editor.read = () => editor.rows.map((row) => ({
    listen:row.listen.value, target:row.target.value,
    ...(row.listenTLS.checked ? {listen_tls:true, ...(!row.reuseCert.checked ? {listen_tls_cert:row.tlsCert.value,listen_tls_key:row.tlsKey.value} : {})} : {}),
    ...(row.targetTLS.checked ? {target_tls:true} : {})
  }));
  editor.setBusy = (busy) => {
    editor.busy = busy;
    addButton.disabled = busy || editor.rows.length >= 256;
    for (const row of editor.rows) {
      row.listen.disabled = row.target.disabled = row.removeButton.disabled = busy;
      row.updateTLS();
    }
  };
  const refresh = () => {
    list.replaceChildren();
    list.append(...editor.rows.map((row) => row.element));
    empty.hidden = editor.rows.length !== 0;
    localText(count, () => t("映射数量：") + editor.rows.length + " / 256");
    editor.setBusy(editor.busy);
  };
  editor.add = (value = {listen:"", target:""}) => {
    if (editor.busy) return;
    if (editor.rows.length >= 256) { notice(() => t("每个账号最多配置 256 个反向映射"), true); return; }
    const item = document.createElement("div");
    item.className = "reverse-mapping";
    const row = {element:item, errors:{}};
    for (const [key, text, placeholder] of [["listen", "服务端监听地址", "127.0.0.1:10022"], ["target", "客户端目标地址", "127.0.0.1:22"]]) {
      const label = document.createElement("label");
      const caption = document.createElement("span");
      localText(caption, () => t(text));
      const input = document.createElement("input");
      input.type = "text";
      input.autocomplete = "off";
      input.spellcheck = false;
      input.maxLength = key === "listen" ? 64 : 260;
      input.value = value[key] || "";
      input.placeholder = placeholder;
      localAttr(input, "aria-label", () => t(text));
      input.addEventListener("input", () => changed());
      const error = document.createElement("small");
      error.className = "config-error";
      error.hidden = true;
      row[key] = input;
      row.errors[key] = error;
      label.append(caption, input, error);
      const group = document.createElement("div");
      group.className = "reverse-address-group";
      group.append(label);
      row[key + "Group"] = group;
      item.append(group);
    }
    const checkbox = (parent, key, text, checked) => {
      const label = document.createElement("label");
      label.className = "reverse-tls-checkbox";
      const input = document.createElement("input"); input.type = "checkbox"; input.checked = checked;
      const caption = document.createElement("span"); localText(caption, () => t(text));
      localAttr(input, "aria-label", () => t(text));
      input.addEventListener("change", () => {row.updateTLS();changed()});
      label.append(input,caption); parent.append(label); row[key]=input;
      return label;
    };
    checkbox(row.listenGroup,"listenTLS","服务端监听启用 SSL/TLS",value.listen_tls === true);
    const certOptions = document.createElement("div"); certOptions.className="reverse-tls-certificates";
    row.listenGroup.append(certOptions);
    checkbox(certOptions,"reuseCert","复用隧道监听证书",!value.listen_tls_cert && !value.listen_tls_key);
    const customCertificates = document.createElement("div"); certOptions.append(customCertificates);
    for (const [key, field, text] of [["tlsCert","listen_tls_cert","独立证书路径"],["tlsKey","listen_tls_key","独立私钥路径"]]) {
      const label=document.createElement("label"), caption=document.createElement("span"), input=document.createElement("input"), error=document.createElement("small");
      localText(caption,()=>t(text)); input.type="text"; input.autocomplete="off"; input.spellcheck=false; input.maxLength=4096;
      input.value=value[field] || ""; input.placeholder=key === "tlsCert" ? "/path/server-cert.pem" : "/path/server-key.pem";
      localAttr(input,"aria-label",()=>t(text)); input.addEventListener("input",()=>changed());
      error.className="config-error"; error.hidden=true;
      row[key]=input;row.errors[key]=error;label.append(caption,input,error);customCertificates.append(label);
    }
    checkbox(row.targetGroup,"targetTLS","客户端目标启用 SSL/TLS",value.target_tls === true);
    const tlsHelp=document.createElement("small");tlsHelp.className="muted";
    localText(tlsHelp,()=>t("目标 TLS 校验证书链和地址主机名；自签名服务须由客户端 tls-ca 信任。"));row.targetGroup.append(tlsHelp);
    row.updateTLS=()=>{
      row.listenTLS.disabled=row.targetTLS.disabled=editor.busy;
      certOptions.hidden=!row.listenTLS.checked;
      row.reuseCert.disabled=editor.busy || !row.listenTLS.checked;
      customCertificates.hidden=row.reuseCert.checked;
      row.tlsCert.disabled=row.tlsKey.disabled=editor.busy || !row.listenTLS.checked || row.reuseCert.checked;
    };
    row.updateTLS();
    const removeButton = document.createElement("button");
    removeButton.type = "button";
    removeButton.className = "secondary";
    localText(removeButton, () => t("移除此映射"));
    removeButton.addEventListener("click", () => {
      if (editor.busy) return;
      editor.rows.splice(editor.rows.indexOf(row), 1);
      refresh();
      changed();
    });
    row.removeButton = removeButton;
    item.append(removeButton);
    editor.rows.push(row);
    refresh();
    return row;
  };
  for (const value of values) editor.add(value);
  refresh();
  addButton.addEventListener("click", () => {
    const row = editor.add();
    if (row) { row.listen.focus(); changed(); }
  });
  return editor;
}
function saveTunnelAccountRow(row) {
  row.reverse.firstInvalid = null;
  if (!validateReverseMappings(row.reverse, row.username, true)) {
    row.reverse.firstInvalid.focus();
    notice(() => t("请检查反向映射的监听地址和目标地址"), true);
    return;
  }
  return saveTunnelAccount({action:"update",username:row.username,password:row.password.value,disabled:!row.enabled.checked,
    ...(reverseMappingsDirty(row) ? {reverse_mappings:row.reverse.read()} : {})});
}

// Account JSON stays on the server. List responses contain no password values.
function tunnelAccountDirty(row) {
  return row.password.value !== "" || row.enabled.checked !== !row.originalDisabled || reverseMappingsDirty(row);
}
function setTunnelAccountsBusy(busy) {
  tunnelAccountsBusy = busy;
  for (const row of tunnelAccountRows) {
    row.password.disabled = row.enabled.disabled = row.deleteButton.disabled = busy;
    row.saveButton.disabled = busy || !tunnelAccountDirty(row);
    row.reverse.setBusy(busy);
  }
  if (tunnelAccountCreateMappings) tunnelAccountCreateMappings.setBusy(busy);
  for (const id of ["tunnel-account-name", "tunnel-account-secret"]) $(id).disabled = busy;
  $("tunnel-account-add").disabled = busy || !tunnelAccountsCanCreate;
  $("tunnel-accounts-reload").disabled = busy || !tunnelAccountsEnabled;
  $("tunnel-accounts-refresh").disabled = busy;
}
function renderTunnelAccounts(data, savedName) {
  const drafts = new Map(tunnelAccountRows.filter((row) => row.username !== savedName && tunnelAccountDirty(row))
    .map((row) => [row.username, {password:row.password.value,
      enabled:row.enabled.checked !== !row.originalDisabled ? row.enabled.checked : undefined,
      mappings:reverseMappingsDirty(row) ? row.reverse.read() : undefined}]));
  if (!tunnelAccountCreateMappings) {
    tunnelAccountCreateMappings = createReverseMappingsEditor([], () => validateReverseMappings(tunnelAccountCreateMappings, "", false));
    $("tunnel-account-create-mappings").append(tunnelAccountCreateMappings.element);
  }
  tunnelAccountsRevision = data.revision || "";
  tunnelAccountsEnabled = !!data.enabled;
  tunnelAccountsCanCreate = !!data.enabled || !!data.can_create;
  localText($("tunnel-account-add"), () => data.enabled ? t("新增客户端账号") : t("新增并启用多账号"));
  localText($("tunnel-accounts-path"), () => data.path || "");
  localText($("tunnel-accounts-help"), () => data.enabled
    ? t("账号数量：") + (data.accounts || []).length + (data.pending ? t(" · 文件有未应用修改，请点击应用账号文件。") : "")
    : data.setup_message ? t(data.setup_message)
      : data.can_create ? t("首次新增会自动创建账号 JSON、同步 accounts-file 并启用多账号；旧单密码隧道会断开，客户端需配置用户名。")
      : t("无法初始化账号，请刷新列表并检查服务端是否已更新。"));
  $("tunnel-account-list").replaceChildren();
  tunnelAccountRows = [];
  for (const account of data.accounts || []) {
    const element = document.createElement("div");
    element.className = "tunnel-account-row";
    const name = document.createElement("strong");
    name.textContent = account.username;
    const password = document.createElement("input");
    password.type = "password";
    password.autocomplete = "new-password";
    password.maxLength = 1024;
    localAttr(password, "placeholder", () => t("留空保留原密码"));
    localAttr(password, "aria-label", () => t("修改客户端密码：") + account.username);
    const enabled = document.createElement("input");
    enabled.type = "checkbox";
    enabled.checked = !account.disabled;
    const label = document.createElement("label");
    const labelText = document.createElement("span");
    localText(labelText, () => t("启用"));
    label.append(enabled, labelText);
    const saveButton = document.createElement("button");
    saveButton.type = "button";
    saveButton.className = "secondary";
    localText(saveButton, () => t("保存"));
    const deleteButton = document.createElement("button");
    deleteButton.type = "button";
    deleteButton.className = "secondary";
    localText(deleteButton, () => t("删除"));
    const row = {username:account.username, password, enabled, originalDisabled:account.disabled, saveButton, deleteButton,
      originalMappings:(account.reverse_mappings || []).map((mapping) => ({...mapping}))};
    const draft = drafts.get(account.username);
    if (draft) { password.value = draft.password; if (draft.enabled !== undefined) enabled.checked = draft.enabled; }
    const changed = () => {
      saveButton.disabled = tunnelAccountsBusy || !tunnelAccountDirty(row);
      validateReverseMappings(row.reverse, row.username, false);
    };
    row.reverse = createReverseMappingsEditor(draft?.mappings || row.originalMappings, changed);
    password.addEventListener("input", changed);
    enabled.addEventListener("change", changed);
    saveButton.addEventListener("click", () => saveTunnelAccountRow(row));
    deleteButton.addEventListener("click", () => {
      if (!row.confirmDelete) {
        row.confirmDelete = true;
        localText(deleteButton, () => t("再次点击确认删除"));
      } else saveTunnelAccount({action:"delete",username:row.username});
    });
    element.append(name, password, label, saveButton, deleteButton, row.reverse.element);
    $("tunnel-account-list").append(element);
    tunnelAccountRows.push(row);
  }
  setTunnelAccountsBusy(tunnelAccountsBusy);
  validateAllReverseMappings(false);
}
async function loadTunnelAccounts() {
  if (tunnelAccountsBusy || state.role !== "server") return;
  const run = epoch;
  const load = ++tunnelAccountsLoad;
  const data = await api("tunnel-accounts");
  if (load === tunnelAccountsLoad && run === epoch && state.authenticated) renderTunnelAccounts(data);
}
async function saveTunnelAccount(change) {
  if (tunnelAccountsBusy || serviceRestarting || configSaving || state.role !== "server" || !state.authenticated) return;
  if (change.password && configByteLength(change.password) > 1024) {
    notice(() => t("密码不能超过 1024 个 UTF-8 字节"), true); return;
  }
  const run = epoch;
  tunnelAccountsLoad++;
  setTunnelAccountsBusy(true);
  try {
    const data = await api("tunnel-accounts", {method:"PUT",body:JSON.stringify({...change,revision:tunnelAccountsRevision})});
    if (run !== epoch || !state.authenticated) return;
    renderTunnelAccounts(data, change.username);
    if (data.config_changed || data.listeners_changed) {
      revision = data.config_revision || revision;
      const entry = configInputs.find((item) => item.key === "accounts-file");
      if (entry && data.config_changed) {
        if (!entry.dirty) entry.input.value = data.path;
        entry.original = data.path;
        entry.dirty = entry.input.value !== entry.original;
        updateConfigSaveButton(entry);
      }
      localText($("pending-label"), () => Object.keys(configEdits()).length ? t("有未保存修改")
        : data.requires_restart ? t("已保存 · 待应用") : t("配置文件已同步"));
    }
    for (const [key, saved] of Object.entries(data.synced_fields || {})) {
      const linked = configInputs.find(item => item.key === key);
      if (!linked || linked.secret || linked.kind === "proxies") continue;
      if (!linked.dirty) linked.input.value = saved;
      linked.original = saved;
      linked.dirty = linked.input.value !== saved;
      updateConfigSaveButton(linked);
    }
    if (data.runtime) renderRuntime(data.runtime);
    if (change.action === "create") {
      $("tunnel-account-name").value = $("tunnel-account-secret").value = "";
      tunnelAccountCreateMappings = null;
      $("tunnel-account-create-mappings").replaceChildren();
      tunnelAccountCreateMappings = createReverseMappingsEditor([], () => validateReverseMappings(tunnelAccountCreateMappings, "", false));
      $("tunnel-account-create-mappings").append(tunnelAccountCreateMappings.element);
    }
    if (data.service_restart_required) await restartSavedMappings(data);
    else notice(() => t("客户端账号已保存并立即生效"));
  } catch (error) {
    if (run === epoch && state.authenticated) notice(() => errorText(error), true);
  } finally {
    setTunnelAccountsBusy(false);
  }
}
$("tunnel-accounts-refresh").addEventListener("click", () => loadTunnelAccounts().catch((error) => notice(() => errorText(error), true)));
$("tunnel-accounts-reload").addEventListener("click", () => saveTunnelAccount({action:"reload"}));
$("tunnel-account-create").addEventListener("submit", (event) => {
  event.preventDefault();
  const username = $("tunnel-account-name").value;
  const password = $("tunnel-account-secret").value;
  const error = configValueError("username",username,{type:"username",required:true},{});
  if (error || !password) { notice(() => error || t("不能为空"), true); return; }
  if (tunnelAccountCreateMappings) {
    tunnelAccountCreateMappings.firstInvalid = null;
    if (!validateReverseMappings(tunnelAccountCreateMappings, "", true)) {
      tunnelAccountCreateMappings.firstInvalid.focus();
      notice(() => t("请检查反向映射的监听地址和目标地址"), true);
      return;
    }
  }
  saveTunnelAccount({action:"create",username,password,reverse_mappings:tunnelAccountCreateMappings?.read() || []});
});
