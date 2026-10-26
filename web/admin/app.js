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
let configRole = "";
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
      group = document.createElement("fieldset");
      const legend = document.createElement("legend");
      localText(legend, () => t(field.group));
      group.append(legend);
      container.append(group);
      groups.set(field.group, group);
    }
    if (field.kind === "proxies") {
      renderProxyMappings(group, field, overrides.has("proxies"));
      continue;
    }
    const row = document.createElement("div");
    row.className = "config-row";
    const label = document.createElement("label");
    const id = "config-option-" + configInputs.length;
    label.htmlFor = id;
    const name = document.createElement("span");
    name.className = "config-key";
    localText(name, () => field.key);
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
    throw new Error(t("需要有效的 ws:// 或 wss:// URL"));
  }
  if (
    !["ws:", "wss:"].includes(url.protocol) ||
    !url.hostname ||
    url.username ||
    url.password ||
    url.hash
  )
    throw new Error(t("需要不含账号或片段的 ws:// 或 wss:// URL"));
  if (url.port === "0") throw new Error(t("URL 端口范围为 1～65535"));
  if (!/^wss?:\/\//.test(value) || /\s/.test(value)) throw new Error(t("WebSocket URL 格式无效"));
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
    (overridden ? t(" 当前由命令行 proxies 覆盖，保存后仍以命令行为准。") : "") +
    " " + t("测试使用本行地址及当前生效的密码、密钥、TLS 和 send-addr 设置；不保存、不监听本地端口，仅验证隧道及后端 TCP 连通性。") +
    " " + t("保存映射时会自动同步 master_service；清空映射后使用单代理 listen。托管模式端口变化后，请保存并通过 acl_master 重启整个服务。")));
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
  draw(field.mappings || []);
  entry.original = JSON.stringify(entry.read());
  add.addEventListener("click", () => {
    draw([...entry.read(), { listen: "", server: "", addr: "" }]);
    changed();
  });
  saveButton.addEventListener("click", () => saveConfigField(entry));
  updateConfigSaveButton(entry);
  configInputs.push(entry);
  header.append(title, saveButton);
  section.append(header, help, list, add);
  group.append(section);
}

// Keep testing separate from saving: unrelated drafts and mapping overrides
// must not be submitted, persisted, or silently substituted into the probe.
async function testProxyMapping(row) {
  if (row.testing || configSaving || serviceRestarting) return;
  try {
    if (!configAddress(row.listen.value, true)) throw new Error(t("映射监听端口须为 1～65535，且不能重复"));
    configURL(row.server.value, !row.addr.value);
    if (row.addr.value) configAddress(row.addr.value);
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
      body: JSON.stringify({ listen: row.listen.value, server: row.server.value, addr: row.addr.value }),
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
    // 后端把框架监听与映射一起保存；只刷新关联值，保留其他行的草稿。
    for (const [key, saved] of Object.entries(result.synced_fields || {})) {
      const linked = configInputs.find((item) => item.key === key);
      if (!linked || linked.kind === "proxies" || linked.secret) continue;
      if (!linked.dirty) linked.input.value = saved;
      linked.original = saved;
      linked.dirty = linked.input.value !== saved;
      updateConfigSaveButton(linked, configSaving || serviceRestarting);
    }
    if (entry.kind === "proxies") entry.original = JSON.stringify(value);
    else entry.original = entry.input.value = entry.secret ? "" : value;
    entry.dirty = false;
    const pending = result.requires_restart !== false;
    localText($("pending-label"), () => ((Object.keys(configEdits()).length ? t("有未保存修改 · ") : "") +
      (pending ? t("已保存项待应用") : t("已保存项已生效"))));
    if (result.runtime) renderRuntime(result.runtime);
    notice(() => configSaveMessage(result, entry.key) + t("其他未保存修改已保留。"));
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
  // 退出后也清除隐藏页面中的配置草稿、密码和统计，避免共享浏览器继续显示。
  configInputs = [];
  revision = "";
  servicePID = 0;
  previous = null;
  points = [];
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
function chartTimeRange(samples = points) {
  const end = samples.at(-1)?.time_ms ?? Date.now();
  return { start: end - 60000, end };
}
function chartPointX(point, geometry, range = chartTimeRange()) {
  return geometry.left + ((point.time_ms - range.start) / 60000) * geometry.width;
}
function chartPointIndex(pointer, geometry, samples = points) {
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
  const point = points[index];
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
      ...points.flatMap((v) => metric.keys.map((key) => v[key]).filter(Number.isFinite)),
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
    points.forEach((v) => {
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
    }[b.dataset.tab]));
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
    await loadConfig();
    notice(() => configSaveMessage(result));
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

async function restartService() {
  if (serviceRestarting || configSaving || !servicePID) return;
  const oldPID = servicePID;
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
    await waitForServiceRestart(oldPID);
    await refreshAuth();
    if (state.authenticated) notice(() => t("服务已由 acl_master 重启，登录会话继续有效。"));
  } catch (error) {
    notice(() => errorText(error), true);
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
  validateConfigForm(null, false);
  drawChart();
});
refreshAuth().catch((e) => {
  localText($("auth-title"), () => t("无法连接管理服务"));
  localText($("auth-help"), () => errorText(e));
});
