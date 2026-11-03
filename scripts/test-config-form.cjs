// Exercise the real page's form behavior, including hidden secrets and failed saves.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

async function page() {
  const elements = new Map();
  const allElements = [];
  function element() {
    const item = {
      children: [],
      disabled: false,
      setCustomValidity(message) {
        this.validationMessage = message;
      },
      focus() {
        this.focused = true;
      },
      listeners: {},
      value: "",
      classList: { toggle() {} },
      append(...children) {
        this.children.push(...children);
      },
      replaceChildren() {
        this.children = [];
      },
      setAttribute(key, value) {
        this[key] = value;
      },
      addEventListener(name, fn) {
        this.listeners[name] = fn;
      },
      reportValidity() {
        this.reported = true;
        return !this.validationMessage;
      },
    };
    allElements.push(item);
    return item;
  }
  const context = vm.createContext({
    document: {
      createElement: element,
      getElementById(id) {
        if (!elements.has(id)) elements.set(id, element());
        return elements.get(id);
      },
      querySelectorAll: (selector) => selector === "*" ? allElements : [],
      documentElement: { lang: "zh-CN" },
    },
    window: { addEventListener() {} },
    URL,
    AbortController,
    setTimeout,
    clearTimeout,
    fetch: async () => ({ ok: true, json: async () => ({ authenticated: false }) }),
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, "../web/admin/i18n.js"), "utf8"), context);
  vm.runInContext(fs.readFileSync(path.join(__dirname, "../web/admin/app.js"), "utf8"), context);
  await new Promise(setImmediate);
  context.fields = [
    {
      key: "password",
      value: "__SGATE_KEEP_EXISTING_PASSWORD__",
      secret: true,
      group: "代理与通道",
      description: "密码",
    },
    {
      key: "target",
      value: "127.0.0.1:9001",
      secret: false,
      group: "代理与通道",
      description: "<script>bad()</script>",
    },
    {
      key: "admin_enabled",
      value: "true",
      secret: false,
      group: "Web 管理",
      description: "启用管理",
    },
  ];
  vm.runInContext('renderConfigFields(fields, {cli_overrides: ["target"]})', context);
  return { context, elements };
}

test("form groups rows, masks secrets and submits only explicitly changed values", async () => {
  const { context, elements } = await page();
  assert.equal(elements.get("config-fields").children.length, 2);
  assert.equal(vm.runInContext("configInputs[0].input.type", context), "password");
  assert.equal(vm.runInContext("configInputs[0].input.value", context), "");
  assert.equal(vm.runInContext("JSON.stringify(configEdits())", context), "{}");
  vm.runInContext(
    'configInputs[1].input.value = "new#address"; configInputs[1].input.listeners.input()',
    context,
  );
  assert.deepEqual(JSON.parse(vm.runInContext("JSON.stringify(configEdits())", context)), {
    target: "new#address",
  });
  vm.runInContext("configInputs[0].input.listeners.input()", context);
  assert.equal(
    JSON.parse(vm.runInContext("JSON.stringify(configEdits())", context)).password,
    undefined,
  );
  const row = elements.get("config-fields").children[0].children[2];
  assert.equal(row.children[0].children[1].textContent, "<script>bad()</script>");
  assert.equal(row.children[0].children[2].textContent, "命令行优先");
});

test("configuration sections retain independent collapse state for each role", async () => {
  const { context, elements } = await page();
  vm.runInContext('renderConfigFields(fields, {}, "client")', context);
  let groups = elements.get("config-fields").children;
  assert.equal(groups[0].className, "config-group");
  assert.equal(groups[0].children[0].textContent, "代理与通道");
  assert.ok(groups.every(group => group.open === false));
  groups[0].open = true;
  groups[0].listeners.toggle();
  assert.equal(groups[1].open, false);
  vm.runInContext('renderConfigFields(fields, {}, "client")', context);
  groups = elements.get("config-fields").children;
  assert.equal(groups[0].open, true);
  assert.equal(groups[1].open, false);
  vm.runInContext('renderConfigFields(fields, {}, "server")', context);
  assert.ok(elements.get("config-fields").children.every(group => group.open === false));
  vm.runInContext('renderConfigFields(fields, {}, "client")', context);
  assert.equal(elements.get("config-fields").children[0].open, true);
});

test("invalid fields reveal their collapsed section before validation feedback", async () => {
  const { context, elements } = await page();
  const group = elements.get("config-fields").children[0];
  group.open = false;
  context.collapsedGroup = group;
  vm.runInContext('configInputs[1].input.closest = () => collapsedGroup; configInputs[1].rule = {required:true}; configInputs[1].input.value = ""; configInputs[1].dirty = true; validateConfigForm()', context);
  assert.equal(group.open, true);
  assert.equal(vm.runInContext('configInputs[1].input.focused', context), true);
});

test("failed save retains edits and restores controls without exposing raw content", async () => {
  const { context, elements } = await page();
  let payload;
  context.api = async (_, options) => {
    payload = JSON.parse(options.body);
    throw new Error("配置已被其他操作修改");
  };
  vm.runInContext(
    'revision="r1"; configInputs[1].input.value="new"; configInputs[1].input.listeners.input()',
    context,
  );
  await vm.runInContext("saveConfig(true)", context);
  assert.deepEqual(payload, { fields: { target: "new" }, revision: "r1", apply: true });
  assert.equal(elements.get("notice").textContent, "配置已被其他操作修改");
  assert.equal(elements.get("reload-config").disabled, false);
  assert.equal(vm.runInContext("configInputs[1].input.disabled", context), false);
  assert.equal(vm.runInContext("configInputs[1].input.value", context), "new");
});

test("single-row save keeps other edits and advances the revision for the next save", async () => {
  const { context, elements } = await page();
  const requests = [];
  let finish;
  context.api = async (_, options) => {
    requests.push(JSON.parse(options.body));
    return new Promise((resolve) => {
      finish = resolve;
    });
  };
  vm.runInContext(
    'revision="r1"; configInputs[0].input.value="new-secret"; configInputs[0].input.listeners.input(); configInputs[1].input.value="127.0.0.1:9002"; configInputs[1].input.listeners.input()',
    context,
  );
  const saving = vm.runInContext("saveConfigField(configInputs[1])", context);
  await vm.runInContext("saveConfigField(configInputs[0]); saveConfig(true)", context);
  assert.equal(requests.length, 1, "overlapping mutations must be blocked");
  assert.equal(vm.runInContext("configInputs[0].saveButton.disabled", context), true);
  finish({ revision: "r2" });
  await saving;
  assert.deepEqual(requests[0], {
    fields: { target: "127.0.0.1:9002" },
    revision: "r1",
    apply: false,
  });
  assert.equal(vm.runInContext("revision", context), "r2");
  assert.equal(vm.runInContext("configInputs[1].dirty", context), false);
  assert.equal(vm.runInContext("configInputs[0].input.value", context), "new-secret");
  assert.equal(vm.runInContext("configInputs[0].dirty", context), true);
  assert.match(elements.get("pending-label").textContent, /有未保存修改/);
  const second = vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.equal(requests[1].revision, "r2");
  finish({ revision: "r3" });
  await second;
  assert.equal(vm.runInContext("configInputs[0].input.value", context), "");
  assert.equal(vm.runInContext("configInputs[0].dirty", context), false);
  assert.equal(vm.runInContext("configInputs[0].saveButton.disabled", context), true);
});

test("failed single-row save preserves its draft and revision for retry", async () => {
  const { context, elements } = await page();
  context.api = async () => {
    throw new Error("配置校验失败");
  };
  vm.runInContext(
    'revision="r1"; configInputs[1].input.value="bad"; configInputs[1].input.listeners.input()',
    context,
  );
  await vm.runInContext("saveConfigField(configInputs[1])", context);
  assert.equal(vm.runInContext("revision", context), "r1");
  assert.equal(vm.runInContext("configInputs[1].input.value", context), "bad");
  assert.equal(vm.runInContext("configInputs[1].dirty", context), true);
  assert.equal(vm.runInContext("configInputs[1].saveButton.title", context), "保存此项");
  assert.equal(vm.runInContext("configInputs[1].saveButton.disabled", context), false);
  assert.equal(elements.get("notice").textContent, "配置校验失败");
});

test("save icons start disabled, enable on edits and disable again on revert", async () => {
  const { context } = await page();
  assert.equal(
    vm.runInContext("configInputs.every(entry => entry.saveButton.disabled)", context),
    true,
  );
  assert.match(vm.runInContext("configInputs[1].saveButton.innerHTML", context), /<svg /);
  vm.runInContext(
    'configInputs[1].input.value="changed"; configInputs[1].input.listeners.input()',
    context,
  );
  assert.equal(vm.runInContext("configInputs[1].saveButton.disabled", context), false);
  vm.runInContext(
    "configInputs[1].input.value=configInputs[1].original; configInputs[1].input.listeners.input()",
    context,
  );
  assert.equal(vm.runInContext("configInputs[1].saveButton.disabled", context), true);
  vm.runInContext(
    'configInputs[0].input.value="new-password"; configInputs[0].input.listeners.input()',
    context,
  );
  assert.equal(vm.runInContext("configInputs[0].saveButton.disabled", context), false);
  vm.runInContext(
    'configInputs[0].input.value=""; configInputs[0].input.listeners.input(); setConfigBusy(true); setConfigBusy(false)',
    context,
  );
  assert.equal(vm.runInContext("configInputs[0].saveButton.disabled", context), true);
});

test("logout waits for server cleanup, prevents duplicate clicks and removes local drafts", async () => {
  const { context, elements } = await page();
  let finish;
  let calls = 0;
  context.api = async (route) => {
    assert.equal(route, "logout");
    calls++;
    return new Promise((resolve) => {
      finish = resolve;
    });
  };
  vm.runInContext(
    'state={authenticated:true,csrf:"secret-csrf"}; epoch=10; configInputs[0].input.value="draft-password"',
    context,
  );
  const pending = vm.runInContext("logoutAdmin()", context);
  await vm.runInContext("logoutAdmin()", context);
  assert.equal(calls, 1);
  assert.equal(elements.get("logout").disabled, true);
  assert.equal(vm.runInContext("state.authenticated", context), true);
  finish({ ok: true });
  await pending;
  assert.equal(vm.runInContext("state.authenticated", context), false);
  assert.equal(vm.runInContext("state.csrf", context), undefined);
  assert.equal(vm.runInContext("epoch", context), 11);
  assert.equal(vm.runInContext("configInputs.length", context), 0);
  assert.equal(elements.get("config-fields").children.length, 0);
  assert.equal(elements.get("dashboard").hidden, true);
  assert.equal(elements.get("auth").hidden, false);
  assert.match(elements.get("auth-help").textContent, /会话已清理/);
});

test("logout failure keeps authentication and drafts available for retry", async () => {
  const { context, elements } = await page();
  context.api = async () => {
    throw new Error("退出登录失败");
  };
  vm.runInContext('state={authenticated:true}; configInputs[0].input.value="draft"', context);
  await vm.runInContext("logoutAdmin()", context);
  assert.equal(vm.runInContext("state.authenticated", context), true);
  assert.equal(vm.runInContext("configInputs[0].input.value", context), "draft");
  assert.equal(elements.get("logout").disabled, false);
  assert.equal(elements.get("notice").textContent, "退出登录失败");
});

test("a delayed configuration response cannot refill the page after logout", async () => {
  const { context, elements } = await page();
  let finish;
  context.api = async (route) =>
    route === "config"
      ? new Promise((resolve) => {
          finish = resolve;
        })
      : { ok: true };
  vm.runInContext("state={authenticated:true}", context);
  const pending = vm.runInContext("loadConfig()", context);
  await vm.runInContext("logoutAdmin()", context);
  finish({ fields: context.fields, revision: "old" });
  await pending;
  assert.equal(elements.get("config-fields").children.length, 0);
  assert.equal(vm.runInContext("revision", context), "");
});

test("proxy mappings render as pairs, submit structured data, and preserve other drafts", async () => {
  const { context, elements } = await page();
  context.fields.push({
    key: "proxies",
    kind: "proxies",
    group: "代理与通道",
    mappings: [
      { listen: "127.0.0.1:9001", server: "ws://localhost:8081/tunnel", addr: "localhost:8080" },
    ],
  });
  vm.runInContext('renderConfigFields(fields, {}); revision="r1"', context);
  assert.equal(vm.runInContext("configInputs[3].rows.length", context), 1);
  assert.equal(vm.runInContext("configInputs[3].saveButton.disabled", context), true);
  vm.runInContext(
    'configInputs[1].input.value="other-draft"; configInputs[1].input.listeners.input(); configInputs[3].rows[0].addr.value="localhost:8088"; configInputs[3].rows[0].addr.listeners.input()',
    context,
  );
  let payload;
  context.api = async (_, options) => {
    payload = JSON.parse(options.body);
    return { revision: "r2" };
  };
  await vm.runInContext("saveConfigField(configInputs[3])", context);
  assert.deepEqual(payload, {
    fields: {},
    proxies: [
      { listen: "127.0.0.1:9001", server: "ws://localhost:8081/tunnel", addr: "localhost:8088" },
    ],
    revision: "r1",
    apply: false,
  });
  assert.equal(vm.runInContext("configInputs[1].input.value", context), "other-draft");
  assert.equal(vm.runInContext("configInputs[3].saveButton.disabled", context), true);
  const editor = elements.get("config-fields").children[0].children.at(-1);
  const add = editor.children.at(-1);
  add.listeners.click();
  assert.equal(vm.runInContext("configInputs[3].rows.length", context), 2);
  const second = editor.children[2].children[1];
  second.children.find((child) => child.className === "proxy-actions").children.at(-1).listeners.click();
  assert.equal(vm.runInContext("configInputs[3].rows.length", context), 1);
  assert.equal(vm.runInContext("configInputs[3].dirty", context), false);
});

test("failed proxy save keeps mapping draft, revision and editable controls", async () => {
  const { context } = await page();
  context.fields.push({
    key: "proxies",
    kind: "proxies",
    group: "代理与通道",
    mappings: [{ listen: "127.0.0.1:9001", server: "ws://localhost/tunnel" }],
  });
  vm.runInContext(
    'renderConfigFields(fields, {}); revision="r1"; configInputs[3].rows[0].listen.value="bad"; configInputs[3].rows[0].listen.listeners.input()',
    context,
  );
  context.api = async () => {
    throw new Error("端口无效");
  };
  await vm.runInContext("saveConfigField(configInputs[3])", context);
  assert.equal(vm.runInContext("revision", context), "r1");
  assert.equal(vm.runInContext("configInputs[3].dirty", context), true);
  assert.equal(vm.runInContext("configInputs[3].rows[0].listen.disabled", context), false);
  assert.equal(vm.runInContext("configInputs[3].rows[0].listen.value", context), "bad");
});

test("client configurations without proxies get an empty structured editor first", async () => {
  const { context } = await page();
  vm.runInContext('renderConfigFields(fields, {cli_overrides:["proxies"]}, "client")', context);
  assert.equal(vm.runInContext("configInputs[0].kind", context), "proxies");
  assert.equal(vm.runInContext("configInputs[0].rows.length", context), 0);
  assert.equal(vm.runInContext("configInputs[0].saveButton.disabled", context), true);
  assert.equal(vm.runInContext("JSON.stringify(configEdits())", context), "{}");
});

test("mapping add/delete/clear saves preserve password and ordinary drafts across consecutive commits", async () => {
  const { context } = await page();
  context.fields.push({
    key: "proxies",
    kind: "proxies",
    group: "代理与通道",
    mappings: [{ listen: "127.0.0.1:9001", server: "ws://localhost/tunnel", addr: "backend:80" }],
  });
  vm.runInContext(
    'renderConfigFields(fields, {}, "client"); revision="r1"; configInputs.find(e=>e.key==="password").input.value="unsaved-password"; configInputs.find(e=>e.key==="password").input.listeners.input(); configInputs.find(e=>e.key==="target").input.value="other-draft"; configInputs.find(e=>e.key==="target").input.listeners.input()',
    context,
  );
  const requests = [];
  let complete;
  context.api = async (_, options) => {
    requests.push(JSON.parse(options.body));
    return new Promise((resolve) => {
      complete = resolve;
    });
  };
  vm.runInContext(
    'configInputs[0].controls[0].listeners.click(); configInputs[0].rows[1].listen.value="127.0.0.1:9002"; configInputs[0].rows[1].server.value="wss://localhost/tunnel"; configInputs[0].rows[1].server.listeners.input()',
    context,
  );
  let pending = vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.equal(vm.runInContext("configInputs[0].controls.every(c=>c.disabled)", context), true);
  await vm.runInContext("saveConfigField(configInputs[0]); saveConfig(true)", context);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].proxies.length, 2);
  assert.deepEqual(requests[0].fields, {});
  complete({ revision: "r2" });
  await pending;
  assert.equal(
    vm.runInContext('configInputs.find(e=>e.key==="password").input.value', context),
    "unsaved-password",
  );
  assert.equal(
    vm.runInContext('configInputs.find(e=>e.key==="target").input.value', context),
    "other-draft",
  );
  vm.runInContext(
    'configInputs[0].controls.find(c=>c.textContent==="删除").listeners.click()',
    context,
  );
  pending = vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.equal(requests[1].revision, "r2");
  assert.equal(requests[1].proxies.length, 1);
  complete({ revision: "r3" });
  await pending;
  vm.runInContext(
    'configInputs[0].controls.find(c=>c.textContent==="删除").listeners.click()',
    context,
  );
  pending = vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.deepEqual(requests[2], { fields: {}, proxies: [], revision: "r3", apply: false });
  complete({ revision: "r4" });
  await pending;
  assert.equal(vm.runInContext("configInputs[0].saveButton.disabled", context), true);
  assert.equal(vm.runInContext('configInputs.find(e=>e.key==="password").dirty', context), true);
  assert.equal(vm.runInContext('configInputs.find(e=>e.key==="target").dirty', context), true);
});

test("noise summary reports running values independently of edited file fields", async () => {
  const { context } = await page();
  assert.equal(vm.runInContext('noiseDescription({mode:"off"})', context), " · 本端白噪音：关闭");
  const random = vm.runInContext(
    'noiseDescription({mode:"random",interval_min:"1s",interval_max:"3s",min_bytes:1024,max_bytes:10485760})',
    context,
  );
  assert.match(random, /1s～3s/);
  assert.match(random, /10.0 MiB/);
  const scenarios = vm.runInContext(
    'noiseDescription({mode:"random",interval_min:"1s",interval_max:"3s",min_bytes:1024,max_bytes:4096,traffic:false,idle:true,empty:false,idle_after:"100ms"})',
    context,
  );
  assert.match(scenarios, /传输中 不发，空闲 发送，无连接 不发/);
  assert.match(scenarios, /空闲阈值 100ms/);
  assert.match(
    vm.runInContext(
      'noiseDescription({mode:"fixed",interval:"500ms",min_bytes:1024,max_bytes:10240})',
      context,
    ),
    /500ms/,
  );
});

test("typed fields show units and reject invalid values before saving without losing drafts", async () => {
  const { context, elements } = await page();
  context.fields = [
    {
      key: "noise-max-bytes",
      value: "10M",
      group: "代理与通道",
      validation: {
        type: "bytes",
        required: true,
        min: "0",
        max: "10485760",
        hint: "单位字节；支持 K/M，最多 10M",
      },
    },
    {
      key: "heartbeat",
      value: "20s",
      group: "代理与通道",
      validation: {
        type: "duration",
        required: true,
        min: "0",
        max: "9223372036854775807",
        hint: "单位 ms/s/m/h",
      },
    },
    {
      key: "password",
      value: "hidden",
      secret: true,
      group: "代理与通道",
      validation: { type: "password", max: "1024" },
    },
  ];
  vm.runInContext('renderConfigFields(fields, {}); revision="r1"', context);
  assert.match(
    elements.get("config-fields").children[0].children[1].children[0].children[1].textContent,
    /单位字节/,
  );
  let calls = 0;
  context.api = async () => {
    calls++;
    return { revision: "r2" };
  };
  for (const value of ["11M", "-1", "1.5M", "abc", ""]) {
    context.bad = value;
    vm.runInContext(
      "configInputs[0].input.value=bad; configInputs[0].input.listeners.input()",
      context,
    );
    await vm.runInContext("saveConfigField(configInputs[0])", context);
    assert.equal(calls, 0);
    assert.equal(vm.runInContext("revision", context), "r1");
    assert.equal(vm.runInContext("configInputs[0].input.value", context), value);
    assert.equal(vm.runInContext("configInputs[0].input.focused", context), true);
    assert.equal(vm.runInContext("configInputs[0].error.hidden", context), false);
  }
  vm.runInContext(
    'configInputs[1].input.value="2"; configInputs[1].input.listeners.input(); configInputs[2].input.value="draft-password"; configInputs[2].input.listeners.input(); configInputs[0].input.value="2M"; configInputs[0].input.listeners.input()',
    context,
  );
  await vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.equal(calls, 1, "another row's unsaved invalid draft blocked this valid single save");
  assert.equal(vm.runInContext("configInputs[2].input.value", context), "draft-password");
  await vm.runInContext("saveConfig(true)", context);
  assert.equal(calls, 1, "whole-form save sent an invalid duration");
  assert.match(elements.get("notice").textContent, /heartbeat/);
});

test("field formats cover units, overflow, endpoints, choices, framework values and password bytes", async () => {
  const { context } = await page();
  const cases = [
    ["heartbeat", "1h30m", { type: "duration", min: "0", max: "9223372036854775807" }, true],
    [
      "heartbeat",
      "9223372036854775808ns",
      { type: "duration", min: "0", max: "9223372036854775807" },
      false,
    ],
    ["heartbeat", "1", { type: "duration" }, false],
    ["buffer-bytes", "4M", { type: "integer" }, false],
    ["max-streams", "1.5", { type: "integer" }, false],
    [
      "admin_session_timeout",
      "500ms",
      { type: "duration", min: "1000000000", max: "31536000000000000" },
      false,
    ],
    ["master_wakeup", "-", { type: "seconds", min: "0", max: "2147483647" }, true],
    ["master_wakeup", "10s", { type: "seconds" }, false],
    ["listen", "[::1]:9081", { type: "listen" }, true],
    ["listen", ":0", { type: "listen" }, true],
    ["addr", "bad_host:9", { type: "address" }, false],
    ["target", "999.1.1.1:80", { type: "address" }, false],
    ["target", "localhost:65536", { type: "address" }, false],
    ["allowed-targets", "localhost:80,[::1]:443", { type: "addresses" }, true],
    ["server", "http://localhost/tunnel", { type: "url" }, false],
    ["server", "wss://user:pass@localhost/tunnel", { type: "url" }, false],
    ["server", "wss://localhost:65536/tunnel", { type: "url" }, false],
    ["master_service", "127.0.0.1|9001,127.0.0.1|9002", { type: "listeners" }, true],
    ["master_service", "127.0.0.1|9001,127.0.0.1|9001", { type: "listeners" }, false],
    ["send-addr", "maybe", { type: "boolean", choices: ["true", "false"] }, false],
    ["master_type", "udp", { type: "enum", choices: ["sock", "inet", "unix"] }, false],
    ["master_args", "-password a#b", { type: "text" }, false],
    ["path", "/tunnel?x=1", { type: "route" }, false],
    ["password", "密".repeat(342), { type: "password", max: "1024" }, false],
  ];
  for (const [key, value, rule, valid] of cases) {
    context.check = { key, value, rule };
    assert.equal(
      vm.runInContext('configValueError(check.key,check.value,check.rule)===""', context),
      valid,
      key + " " + value,
    );
  }
});

test("related rows must be saved together and invalid proxy drafts never reach the API", async () => {
  const { context, elements } = await page();
  context.fields = [
    ...["noise-interval", "noise-interval-min", "noise-interval-max"].map((key) => ({
      key,
      value: "0",
      group: "代理与通道",
      validation: { type: "duration", required: true, min: "0", max: "9223372036854775807" },
    })),
    {
      key: "proxies",
      kind: "proxies",
      group: "代理与通道",
      mappings: [
        { listen: "127.0.0.1:9001", server: "ws://localhost/tunnel" },
        { listen: "127.0.0.1:9002", server: "ws://localhost/tunnel" },
      ],
    },
  ];
  vm.runInContext('renderConfigFields(fields, {}); revision="r1"', context);
  let payload,
    calls = 0;
  context.api = async (_, options) => {
    calls++;
    payload = JSON.parse(options.body);
    return { revision: "r2" };
  };
  vm.runInContext(
    'configInputs[1].input.value="1s";configInputs[1].input.listeners.input();configInputs[2].input.value="3s";configInputs[2].input.listeners.input()',
    context,
  );
  await vm.runInContext("saveConfigField(configInputs[1])", context);
  assert.equal(calls, 0);
  assert.match(elements.get("notice").textContent, /整页保存/);
  context.loadConfig = async () => {};
  await vm.runInContext("saveConfig(false)", context);
  assert.equal(calls, 1);
  assert.deepEqual(payload.fields, { "noise-interval-min": "1s", "noise-interval-max": "3s" });
  vm.runInContext(
    'configInputs[3].rows[1].listen.value="127.0.0.1:9001";configInputs[3].rows[1].listen.listeners.input()',
    context,
  );
  await vm.runInContext("saveConfigField(configInputs[3])", context);
  assert.equal(calls, 1);
  assert.match(elements.get("notice").textContent, /重复/);
  assert.equal(vm.runInContext("configInputs[3].rows[1].listen.value", context), "127.0.0.1:9001");
});

test("master_args validates option names, units, booleans and compact mapping JSON", async () => {
  const { context } = await page();
  context.rule = {
    type: "arguments",
    options: {
      "noise-max-bytes": { type: "bytes", min: "0", max: "10485760" },
      heartbeat: { type: "duration", min: "0", max: "9223372036854775807" },
      "send-addr": { type: "boolean", choices: ["true", "false"] },
      proxies: { type: "proxies" },
    },
  };
  for (const [value, valid] of [
    ["-noise-max-bytes 10M -heartbeat 1s -send-addr=false", true],
    ["-noise-max-bytes 11M", false],
    ["-unknown-option value", false],
    ["-heartbeat 2", false],
    ["-send-addr false", false],
    ['-proxies [{"listen":"127.0.0.1:9001","server":"ws://localhost/tunnel"}]', true],
    ["-proxies '[]'", false],
    ["-noise-max-bytes", false],
  ]) {
    context.value = value;
    assert.equal(
      vm.runInContext('configValueError("master_args",value,rule)===""', context),
      valid,
      value,
    );
  }
});

test("relationship validation catches reconnect, sizes, heartbeat, TLS and shared storage", async () => {
  const { context } = await page();
  for (const [values, key] of [
    [{ "reconnect-min": "2s", "reconnect-max": "1s" }, "reconnect-min"],
    [{ "noise-min-bytes": "10M", "noise-max-bytes": "1K" }, "noise-min-bytes"],
    [{ heartbeat: "20s", "heartbeat-timeout": "15s" }, "heartbeat-timeout"],
    [{ "buffer-bytes": "1024" }, "buffer-bytes"],
    [{ "tls-cert": "cert.pem", "tls-key": "" }, "tls-cert"],
    [
      { admin_users_file: "same", admin_session_dir: "same", admin_setup_token_file: "token" },
      "admin_users_file",
    ],
    [{ server: "", proxies: [] }, "server"],
    [{ path: "/admin/api" }, "path"],
  ]) {
    context.values = values;
    context.key = key;
    assert.equal(vm.runInContext("configRelationErrors(values).has(key)", context), true, key);
  }
  context.values = {
    server: "",
    proxies: [{ listen: "127.0.0.1:9001", server: "ws://localhost/tunnel" }],
  };
  assert.equal(vm.runInContext("configRelationErrors(values).size", context), 0);
});

test("server shared HTTP accepts wildcard addresses while client HTTPS rules remain", async () => {
  const { context } = await page();
  context.values = {
    listen: "0.0.0.0:8081",
    "tls-cert": "",
    "tls-key": "",
    admin_enabled: "true",
  };
  vm.runInContext('configRole = "server"', context);
  assert.equal(vm.runInContext("configRelationErrors(values).size", context), 0);
  context.values["tls-cert"] = "cert.pem";
  assert.equal(vm.runInContext('configRelationErrors(values).has("tls-key")', context), true);
  context.values = { admin_listen: "0.0.0.0:9081", admin_tls_cert: "", admin_tls_key: "" };
  vm.runInContext('configRole = "client"', context);
  assert.equal(vm.runInContext('configRelationErrors(values).has("admin_listen")', context), true);
});

test(
  "all deployed application fields pass the frontend rules emitted by Go",
  {
    skip: !process.env.SGATE_CONFIG_RULES_REPORT,
  },
  async () => {
    const reports = JSON.parse(fs.readFileSync(process.env.SGATE_CONFIG_RULES_REPORT, "utf8"));
    for (const report of reports) {
      const { context, elements } = await page();
      context.report = report;
      vm.runInContext("renderConfigFields(report.fields,{},report.role)", context);
      assert.equal(
        vm.runInContext("validateConfigForm()", context),
        true,
        report.name + ": " + elements.get("notice").textContent,
      );
      assert.equal(
        vm.runInContext(
          'configInputs.every(entry => entry.kind === "proxies" || !!entry.rule.hint)',
          context,
        ),
        true,
        report.name,
      );
    }
  },
);

test("explicit text metadata keeps notification addresses editable when their value is no", async () => {
  const { context } = await page();
  context.fields = ["master_notify_addr", "master_notify_recipients"].map((key) => ({
    key,
    value: "no",
    validation: { type: "text", hint: "通知地址，可关闭。" },
  }));
  vm.runInContext("renderConfigFields(fields,{})", context);
  assert.equal(
    vm.runInContext('configInputs.every(entry => entry.input.type === "text")', context),
    true,
  );
});

test("live badges and save feedback reflect actual runtime without discarding other drafts", async () => {
  const { context, elements } = await page();
  context.fields = [
    {
      key: "noise-max-bytes",
      value: "10K",
      validation: { type: "bytes", max: "10485760", live: true },
    },
    { key: "target", value: "localhost:80", validation: { type: "address" } },
  ];
  vm.runInContext('renderConfigFields(fields,{cli_overrides:["noise-max-bytes"]})', context);
  const group = elements.get("config-fields").children[0];
  assert.match(group.children[1].children[0].children.at(-1).textContent, /即时生效.*命令行已覆盖/);
  context.api = async () => ({
    revision: "r2",
    requires_restart: false,
    live_applied: ["noise-max-bytes"],
    runtime: { noise: { mode: "off" }, cli_overrides: ["noise-max-bytes"] },
  });
  vm.runInContext(
    'configInputs[0].input.value="1M";configInputs[0].input.listeners.input();configInputs[1].input.value="localhost:81";configInputs[1].input.listeners.input()',
    context,
  );
  await vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.match(elements.get("notice").textContent, /无需重启代理/);
  assert.match(elements.get("notice").textContent, /命令行覆盖项仍使用启动值/);
  assert.equal(vm.runInContext("configInputs[1].dirty", context), true);
  assert.equal(vm.runInContext("configInputs[1].input.value", context), "localhost:81");
  assert.match(elements.get("pending-label").textContent, /有未保存修改.*已生效/);
  assert.match(
    vm.runInContext(
      'configSaveMessage({live_applied:["noise-interval"],requires_restart:true})',
      context,
    ),
    /其他配置仍待应用/,
  );
});

test("traffic samples distinguish tiny business traffic from noise and handle reset or missing counters", async () => {
  const { context } = await page();
  context.lastSample = {
    time_ms: 1000,
    tx_bytes: 100,
    rx_bytes: 200,
    noise_tx_bytes: 10,
    noise_rx_bytes: 20,
    business_tx_bytes: 1,
    business_rx_bytes: 2,
  };
  context.nextSample = {
    time_ms: 2000,
    tx_bytes: 4194404,
    rx_bytes: 201,
    noise_tx_bytes: 1034,
    noise_rx_bytes: 20,
    business_tx_bytes: 5,
    business_rx_bytes: 6,
  };
  const rates = JSON.parse(
    vm.runInContext("JSON.stringify(trafficRates(nextSample,lastSample))", context),
  );
  assert.equal(rates.tx, 4194304);
  assert.equal(rates.noiseTX, 1024);
  assert.equal(rates.businessTX, 4);
  assert.equal(rates.noiseRX, 0);
  assert.equal(vm.runInContext("trafficRates(nextSample,null)", context), null);
  assert.equal(
    vm.runInContext("trafficRates({...nextSample,time_ms:1000},lastSample)", context),
    null,
  );
  assert.equal(
    vm.runInContext("trafficRates({...nextSample,tx_bytes:0},lastSample)", context),
    null,
  );
  assert.equal(
    vm.runInContext(
      "trafficRates({...nextSample,business_tx_bytes:undefined},lastSample).businessTX",
      context,
    ),
    null,
  );
  assert.match(
    vm.runInContext(
      "noiseObservationText(nextSample,trafficRates(nextSample,lastSample))",
      context,
    ),
    /4 B\/s/,
  );
  assert.match(
    vm.runInContext(
      "noiseActivityText({tunnels:1,traffic:1,idle:0,empty:0,permitted:0,suppressed_traffic:1})",
      context,
    ),
    /业务活跃暂停 1 条.*后台业务数据/,
  );
  assert.match(vm.runInContext("noiseActivityText({tunnels:0})", context), /没有在线隧道/);
  assert.match(vm.runInContext("noiseActivityText(null)", context), /当前运行程序未提供/);
});

test("noise and business charts use independent scales instead of the total traffic peak", async () => {
  const { context, elements } = await page();
  const labels = [],
    arcs = [];
  const drawing = {
    scale() {},
    setLineDash() {},
    beginPath() {},
    moveTo() {},
    lineTo() {},
    stroke() {},
    closePath() {},
    fill() {},
    fillText(value) {
      labels.push(value);
    },
    arc(...value) {
      arcs.push(value);
    },
    createLinearGradient() {
      return { addColorStop() {} };
    },
  };
  context.document.getElementById("traffic-chart").clientWidth = 900;
  elements.get("traffic-chart").clientHeight = 250;
  elements.get("traffic-chart").getContext = () => drawing;
  context.chartPoints = [
    { time_ms: 59000, tx: 4194304, rx: 0, noiseTX: 1024, noiseRX: 0, businessTX: 4, businessRX: 0 },
    { time_ms: 60000, tx: 0, rx: 0, noiseTX: 1024, noiseRX: 0, businessTX: 4, businessRX: 0 },
  ];
  vm.runInContext("points=chartPoints;drawChart()", context);
  assert.match(labels[0], /MiB\/s/);
  elements.get("traffic-metric").value = "noise";
  labels.length = 0;
  arcs.length = 0;
  elements.get("traffic-metric").listeners.change();
  assert.match(labels[0], /KiB\/s/);
  assert.ok(arcs.at(-1)[1] < 100, "noise trace should be visibly above baseline");
  assert.equal(elements.get("traffic-chart-title").textContent, "白噪音负载带宽");
  elements.get("traffic-metric").value = "business";
  labels.length = 0;
  vm.runInContext("drawChart()", context);
  assert.match(labels[0], /B\/s/);
  assert.doesNotMatch(labels[0], /KiB|MiB/);
  vm.runInContext("points=[{time_ms:60000,tx:0,rx:0,businessTX:null,businessRX:null}]", context);
  arcs.length = 0;
  vm.runInContext("drawChart()", context);
  assert.equal(arcs.length, 0, "missing telemetry must not be drawn as measured zero");
});

test("hover selects only existing samples and keeps zero and missing values distinct", async () => {
  const { context } = await page();
  context.geometry = { left: 67, right: 657, top: 12, bottom: 240, width: 590, height: 228 };
  const index = (x, y, times) => {
    context.samples = times.map((time_ms) => ({ time_ms }));
    return vm.runInContext(`chartPointIndex({x:${x},y:${y}},geometry,samples)`, context);
  };
  const full = Array.from({ length: 61 }, (_, i) => i * 1000);
  assert.equal(index(67, 20, full), 0);
  assert.equal(index(657, 240, full), 60);
  assert.equal(index(647, 20, [59000, 60000]), 0);
  assert.equal(index(657, 20, [59000, 60000]), 1);
  assert.equal(index(67, 20, [59000, 60000]), -1, "unused left area has no samples");
  assert.equal(index(657, 20, [60000]), 0);
  assert.equal(index(657, 20, []), -1);
  assert.equal(index(658, 20, full), -1);
  assert.equal(index(67, 11, full), -1);
  // 不等间隔采样必须按时间命中，不能按数组下标等距拉伸。
  assert.equal(index(362, 20, [0, 1000, 30000, 60000]), 2);
  assert.equal(
    vm.runInContext("chartPointX({time_ms:30000},geometry,{start:0,end:60000})", context),
    362,
  );
  const ticks = [];
  context.tickDrawing = {
    beginPath() {},
    moveTo() {},
    lineTo() {},
    stroke() {},
    fillText(label, x) {
      ticks.push({ label, x });
    },
  };
  vm.runInContext("points=[{time_ms:60000}];drawChartTimeAxis(tickDrawing,geometry)", context);
  assert.equal(ticks.length, 13);
  assert.equal(ticks[0].x, 67);
  assert.equal(ticks.at(-1).x, 657);
  for (let i = 1; i < ticks.length; i++)
    assert.ok(Math.abs(ticks[i].x - ticks[i - 1].x - 590 / 12) < 1e-6);
  assert.equal(vm.runInContext("chartHoverRate(0)", context), "0 B/s");
  assert.equal(vm.runInContext("chartHoverRate(0.25)", context), "0.25 B/s");
  assert.equal(vm.runInContext("chartHoverRate(null)", context), "未提供");
  const sample = vm.runInContext(
    "trafficRates({time_ms:2000,tx_bytes:1,rx_bytes:1},{time_ms:1000,tx_bytes:0,rx_bytes:0})",
    context,
  );
  assert.equal(sample.time_ms, 2000);
});

test("hover tooltip follows selected metric, refresh and scaled pointer coordinates, then clears on leave", async () => {
  const { context, elements } = await page();
  const canvas = context.document.getElementById("traffic-chart");
  const arcs = [];
  const drawing = {
    scale() {},
    setLineDash() {},
    beginPath() {},
    moveTo() {},
    lineTo() {},
    stroke() {},
    closePath() {},
    fill() {},
    fillText() {},
    arc(...v) {
      arcs.push(v);
    },
    createLinearGradient() {
      return { addColorStop() {} };
    },
  };
  canvas.clientWidth = 900;
  canvas.clientHeight = 250;
  canvas.getBoundingClientRect = () => ({ left: 100, top: 50, width: 1800, height: 500 });
  canvas.getContext = () => drawing;
  const tooltip = context.document.getElementById("traffic-tooltip");
  tooltip.style = {};
  tooltip.offsetWidth = 200;
  tooltip.offsetHeight = 100;
  context.chartPoints = [
    { time_ms: 1000, tx: 2048, rx: 0, noiseTX: 10, noiseRX: null, businessTX: 0.25, businessRX: 4 },
    { time_ms: 2000, tx: 4096, rx: 0, noiseTX: 20, noiseRX: null, businessTX: 0.5, businessRX: 8 },
  ];
  vm.runInContext("points=chartPoints;drawChart()", context);
  assert.equal(tooltip.hidden, true);
  canvas.listeners.pointermove({ clientX: 100 + 868 * 2, clientY: 50 + 150 * 2 });
  assert.equal(tooltip.hidden, false);
  assert.equal(elements.get("traffic-tooltip-tx").textContent, "4.00 KiB/s");
  assert.equal(elements.get("traffic-tooltip-rx").textContent, "0 B/s");
  assert.match(elements.get("traffic-tooltip-time").textContent, /采样时间/);
  assert.ok(parseFloat(tooltip.style.left) + tooltip.offsetWidth <= 900);
  assert.ok(parseFloat(tooltip.style.top) + tooltip.offsetHeight <= 250);
  elements.get("traffic-metric").value = "noise";
  elements.get("traffic-metric").listeners.change();
  assert.equal(elements.get("traffic-tooltip-tx").textContent, "20 B/s");
  assert.equal(elements.get("traffic-tooltip-rx").textContent, "未提供");
  elements.get("traffic-metric").value = "business";
  vm.runInContext("drawChart()", context);
  assert.equal(elements.get("traffic-tooltip-tx").textContent, "0.5 B/s");
  vm.runInContext("points[1]={...points[1],time_ms:3000,businessTX:0.75};drawChart()", context);
  assert.equal(elements.get("traffic-tooltip-tx").textContent, "0.75 B/s");
  canvas.listeners.pointerleave();
  assert.equal(tooltip.hidden, true);
  assert.equal(vm.runInContext("chartPointer", context), null);
  canvas.listeners.pointermove({ clientX: 100 + 20 * 2, clientY: 50 + 150 * 2 });
  assert.equal(tooltip.hidden, true, "axis labels and empty areas should not show values");
  canvas.listeners.pointermove({ clientX: 100 + 868 * 2, clientY: 50 + 150 * 2 });
  vm.runInContext("renderSignedOut()", context);
  assert.equal(tooltip.hidden, true);
  assert.equal(vm.runInContext("chartPointer", context), null);
});

test("offline business sessions explain zero noise and expose the last disconnect safely", async () => {
  const { context } = await page();
  context.activity = {
    tunnels: 0,
    offline: 1,
    offline_streams: 128,
    last_disconnect_ms: 60000,
    last_disconnect: "read: i/o timeout",
  };
  const text = vm.runInContext("noiseActivityText(activity)", context);
  assert.match(text, /离线保留 1 个会话、128 条业务连接/);
  assert.match(text, /等待客户端重连/);
  assert.match(text, /最近隧道断开.*i\/o timeout/);
});

test("receive backpressure is distinct from a disconnected tunnel", async () => {
  const { context } = await page();
  context.activity = {
    tunnels: 1,
    traffic: 0,
    idle: 1,
    empty: 0,
    permitted: 1,
    receive_buffer_bytes: 32768,
  };
  assert.match(
    vm.runInContext("noiseActivityText(activity)", context),
    /业务应用待接收 32.0 KiB.*仍可读取噪音和心跳/,
  );
});

test("language switches client and server forms without losing drafts, mappings or secrets", async () => {
  for (const role of ["client", "server"]) {
    const { context, elements } = await page();
    context.drawChart = () => {};
    context.role = role;
    vm.runInContext('renderConfigFields(fields, {cli_overrides: ["target"]}, role)', context);
    vm.runInContext(`
      const password = configInputs.find((entry) => entry.key === "password");
      password.input.value = "unsaved-private-password";
      password.input.listeners.input();
      const target = configInputs.find((entry) => entry.key === "target");
      target.input.value = "127.0.0.1:9012";
      target.input.listeners.input();
      const mapping = configInputs.find((entry) => entry.key === "proxies");
      if (mapping) {
        mapping.controls[0].listeners.click();
        mapping.rows[0].listen.value = "127.0.0.1:9013";
        mapping.rows[0].server.value = "ws://example.com/tunnel";
        mapping.rows[0].listen.listeners.input();
      }
    `, context);
    const before = vm.runInContext("JSON.stringify(configPayload(configEdits(), false))", context);
    let requests = 0;
    context.fetch = async () => { requests++; throw new Error("switch must not call API"); };
    for (const lang of ["en", "zh-CN", "en"]) {
      vm.runInContext(`sgateI18n.setLanguage(${JSON.stringify(lang)})`, context);
      assert.equal(context.document.documentElement.lang, lang);
      assert.equal(vm.runInContext("JSON.stringify(configPayload(configEdits(), false))", context), before);
      assert.equal(vm.runInContext('configInputs.find(e => e.key === "password").input.value', context), "unsaved-private-password");
      assert.equal(vm.runInContext('configInputs.find(e => e.key === "target").saveButton.disabled', context), false);
      assert.match(elements.get("pending-label").textContent, lang === "en" ? /Unsaved/ : /未保存/);
    }
    assert.equal(requests, 0);
    assert.equal(vm.runInContext('configInputs.find(e => e.key === "target").saveButton.title', context), "Save this setting");
    assert.equal(vm.runInContext('configInputs.find(e => e.key === "admin_enabled").input.children[0].textContent', context), "true (yes)");
  }
});

test("English validation, live state and chart labels preserve configuration values", async () => {
  const { context } = await page();
  context.drawChart = () => {};
  vm.runInContext('sgateI18n.setLanguage("en")', context);
  assert.equal(vm.runInContext('configValueError("heartbeat", "bad", {type:"duration"})', context), "Include a time unit, for example 500ms, 1s, 2m or 1h30m");
  assert.equal(vm.runInContext('chartMetric().title', context), "Live network bandwidth");
  assert.equal(vm.runInContext('t("单位必须填写：ns、us/µs、ms、s、m、h，可组合如 1m30s；0 的含义依参数而定。 0 使用默认 20s。")', context).includes("0 uses the default of 20s."), true);
  assert.match(vm.runInContext('configSaveMessage({applied: true})', context), /business proxy restarted/);
  assert.match(vm.runInContext('noiseActivityText({tunnels: 0})', context), /No online tunnel/);
  assert.equal(vm.runInContext('t("my-custom-description")', context), "my-custom-description");
});

test("switching after sign-in keeps account identity and localizes existing save feedback", async () => {
  const { context, elements } = await page();
  context.drawChart = () => {};
  context.loadConfig = async () => {};
  context.poll = () => {};
  context.api = async () => ({authenticated: true, role: "client", username: "管理员", expires: "2026-10-27T00:00:00Z", session_timeout_seconds: 3600});
  await vm.runInContext("refreshAuth()", context);
  vm.runInContext('notice(() => configSaveMessage({live_applied: ["heartbeat"]})); sgateI18n.setLanguage("en")', context);
  assert.equal(elements.get("user-label").textContent, "管理员");
  assert.equal(elements.get("session-user").textContent, "管理员");
  assert.match(elements.get("notice").textContent, /live settings updated/);
  vm.runInContext('sgateI18n.setLanguage("zh-CN")', context);
  assert.equal(elements.get("user-label").textContent, "管理员");
  assert.match(elements.get("notice").textContent, /在线参数已更新/);
});

test("mapping row save refreshes synchronized master_service while preserving unrelated drafts", async () => {
  const { context } = await page();
  context.fields = [
    {key: "proxies", kind: "proxies", mappings: [{listen: "127.0.0.1:9000", server: "ws://localhost/tunnel"}], group: "Proxy"},
    {key: "master_service", value: "127.0.0.1|9000", group: "Framework"},
    {key: "password", value: "", secret: true, group: "Proxy"},
  ];
  vm.runInContext('renderConfigFields(fields, {}, "client"); configInputs[0].controls[0].listeners.click(); configInputs[0].rows[1].listen.value="127.0.0.1:2022"; configInputs[0].rows[1].server.value="ws://localhost/tunnel"; configInputs[0].rows[1].listen.listeners.input(); configInputs[2].input.value="keep-private-draft"; configInputs[2].input.listeners.input()', context);
  context.api = async () => ({revision: "new", requires_restart: true, synced_fields: {master_service: "127.0.0.1|9000, 127.0.0.1|2022"}});
  await vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.equal(vm.runInContext("configInputs[1].input.value", context), "127.0.0.1|9000, 127.0.0.1|2022");
  assert.equal(vm.runInContext("configInputs[1].dirty", context), false);
  assert.equal(vm.runInContext("configInputs[2].input.value", context), "keep-private-draft");
  // A pending manual framework edit remains a draft against the new saved value.
  vm.runInContext('configInputs[1].input.value="127.0.0.1|9009"; configInputs[1].input.listeners.input(); configInputs[0].rows[0].addr.value="localhost:80"; configInputs[0].rows[0].addr.listeners.input()', context);
  await vm.runInContext("saveConfigField(configInputs[0])", context);
  assert.equal(vm.runInContext("configInputs[1].input.value", context), "127.0.0.1|9009");
  assert.equal(vm.runInContext("configInputs[1].original", context), "127.0.0.1|9000, 127.0.0.1|2022");
  assert.equal(vm.runInContext("configInputs[1].dirty", context), true);
});

test("mapping test uses only the row draft, debounces clicks, and does not save configuration", async () => {
  const { context } = await page();
  vm.runInContext('renderConfigFields([{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel",addr:"localhost:8088"}]}],{}); revision="unchanged"', context);
  let calls = 0, payload, finish;
  context.api = async (path, options) => {
    calls++;
    assert.equal(path, "proxy-test");
    payload = JSON.parse(options.body);
    return await new Promise((resolve) => { finish = resolve; });
  };
  const first = vm.runInContext("configInputs[0].rows[0].testButton.listeners.click()", context);
  assert.equal(vm.runInContext("configInputs[0].rows[0].testButton.disabled", context), true);
  await vm.runInContext("configInputs[0].rows[0].testButton.listeners.click()", context);
  assert.equal(calls, 1);
  assert.deepEqual(payload, {listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel",addr:"localhost:8088"});
  vm.runInContext("setConfigBusy(false)", context);
  assert.equal(vm.runInContext("configInputs[0].rows[0].testButton.disabled", context), true);
  finish({ok:true,stage:"backend",detail:"Tunnel authenticated; backend TCP reachable",elapsed_ms:12,test_id:"ab".repeat(32)});
  await first;
  assert.match(vm.runInContext("configInputs[0].rows[0].result.textContent", context), /连通成功.*12 ms.*测试编号：.*abab/);
  assert.equal(vm.runInContext("configInputs[0].testButton?.disabled", context), undefined);
  assert.equal(vm.runInContext("revision", context), "unchanged");
  assert.equal(vm.runInContext("JSON.stringify(configEdits())", context), "{}");
  context.drawChart = () => {};
  vm.runInContext('sgateI18n.setLanguage("en")', context);
  assert.match(vm.runInContext("configInputs[0].rows[0].result.textContent", context), /Connected.*Backend TCP.*Test ID:.*abab/);
});

test("mapping edits invalidate pending test results and invalid values never reach the API", async () => {
  const { context } = await page();
  vm.runInContext('renderConfigFields([{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel",addr:"localhost:8088"}]}],{})', context);
  let finish, calls = 0;
  context.api = async () => { calls++; return await new Promise((resolve) => { finish = resolve; }); };
  const pending = vm.runInContext("testProxyMapping(configInputs[0].rows[0])", context);
  vm.runInContext('configInputs[0].rows[0].addr.value="localhost:8089"; configInputs[0].rows[0].addr.listeners.input()', context);
  finish({ok:false,stage:"backend",detail:"backend TCP connection refused",elapsed_ms:1});
  await pending;
  assert.equal(vm.runInContext("configInputs[0].rows[0].result.hidden", context), true);
  assert.equal(vm.runInContext("configInputs[0].dirty", context), true);
  vm.runInContext('configInputs[0].rows[0].server.value="not-a-url"', context);
  await vm.runInContext("testProxyMapping(configInputs[0].rows[0])", context);
  assert.equal(calls, 1);
  assert.equal(vm.runInContext("configInputs[0].rows[0].result.hidden", context), false);
});

test("mapping test failures restore the button and preserve drafts", async () => {
  const { context } = await page();
  vm.runInContext('renderConfigFields([{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel"}]}],{})', context);
  context.api = async () => { throw new Error("network unavailable"); };
  await vm.runInContext("testProxyMapping(configInputs[0].rows[0])", context);
  assert.equal(vm.runInContext("configInputs[0].rows[0].testButton.disabled", context), false);
  assert.match(vm.runInContext("configInputs[0].rows[0].result.textContent", context), /network unavailable/);
  assert.equal(vm.runInContext("configInputs[0].rows[0].listen.value", context), "127.0.0.1:9001");
});

test("mapping results distinguish encrypted command failures and show the correlated test ID", async () => {
  const { context } = await page();
  vm.runInContext('renderConfigFields([{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel"}]}],{})', context);
  context.api = async () => ({ok:false,stage:"command",detail:"invalid connectivity test result command or test ID",elapsed_ms:3,test_id:"cd".repeat(32)});
  await vm.runInContext("testProxyMapping(configInputs[0].rows[0])", context);
  assert.match(vm.runInContext("configInputs[0].rows[0].result.textContent", context), /连通失败.*测试命令.*测试编号：.*cdcd/);
  assert.equal(vm.runInContext("configInputs[0].rows[0].testButton.disabled", context), false);
});

test("failed mapping tests display target and detailed error with safe text and bilingual labels", async () => {
  const { context } = await page();
  vm.runInContext('renderConfigFields([{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel"}]}],{})', context);
  context.api = async () => ({ok:false,stage:"backend",detail:"backend TCP connection refused",error:"dial tcp 127.0.0.1:8088: connect: connection refused <text>",target:"127.0.0.1:8088",elapsed_ms:2,test_id:"ab".repeat(32)});
  await vm.runInContext("testProxyMapping(configInputs[0].rows[0])", context);
  assert.match(vm.runInContext("configInputs[0].rows[0].result.textContent", context), /连通失败.*目标：127.0.0.1:8088.*错误：dial tcp.*connection refused <text>.*测试编号：/);
  context.drawChart = () => {};
  vm.runInContext('sgateI18n.setLanguage("en")', context);
  assert.match(vm.runInContext("configInputs[0].rows[0].result.textContent", context), /Connection failed.*Target: 127.0.0.1:8088.*Error: dial tcp.*Test ID:/);
});

test("managed mapping save automatically restarts and waits for the replacement without losing drafts", async () => {
  const { context, elements } = await page();
  context.fields.push({key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel"}]});
  vm.runInContext('renderConfigFields(fields,{}); revision="old"; configInputs[1].input.value="keep-draft";configInputs[1].input.listeners.input();configInputs[3].rows[0].listen.value="127.0.0.1:9002";configInputs[3].rows[0].listen.listeners.input()', context);
  const calls=[];
  context.renderRuntime=()=>{};
  context.waitForServiceRestart=async (pid,listeners)=>{assert.equal(pid,101);assert.deepEqual(listeners,["127.0.0.1:9002"]);return {revision:"new",runtime:{pid:102,running:true}}};
  context.api=async(route,options)=>{
    calls.push(route);
    if(route==="config")return {revision:"saved",requires_restart:true,service_restart_required:true,runtime:{pid:101,pending_listeners:["127.0.0.1:9002"]}};
    assert.equal(route,"restart-service");assert.equal(JSON.parse(options.body).expected_pid,101);return {restart_requested:true};
  };
  await vm.runInContext("saveConfigField(configInputs[3])",context);
  assert.deepEqual(calls,["config","restart-service"]);
  assert.equal(vm.runInContext("revision",context),"new");
  assert.equal(vm.runInContext("configInputs[1].input.value",context),"keep-draft");
  assert.match(elements.get("notice").textContent,/映射已生效/);
  assert.equal(vm.runInContext("serviceRestarting || configSaving",context),false);
});

test("failed automatic mapping restart never displays success", async () => {
 const {context,elements}=await page();
 context.api=async()=>{throw Object.assign(new Error("master_ctl unavailable"),{status:409})};
 await assert.rejects(vm.runInContext("restartSavedMappings({runtime:{pid:101}})",context),/master_ctl unavailable/);
 assert.doesNotMatch(elements.get("notice").textContent,/映射已生效/);
 assert.equal(vm.runInContext("serviceRestarting",context),false);
});

test("IP chart samples isolate peers, metrics and reset gaps without changing totals", async () => {
  const { context, elements } = await page();
  vm.runInContext(`
    const beforeIP = {time_ms:1000, by_ip:[{ip:"127.0.0.1",tx_bytes:10,rx_bytes:20,noise_tx_bytes:2,noise_rx_bytes:3,business_tx_bytes:5,business_rx_bytes:7}]};
    const afterIP = {time_ms:2000, by_ip:[{ip:"127.0.0.1",connections:2,tx_bytes:110,rx_bytes:220,noise_tx_bytes:12,noise_rx_bytes:23,business_tx_bytes:55,business_rx_bytes:77},{ip:"::1",connections:1,tx_bytes:30,rx_bytes:40}]};
    points=[{time_ms:2000,tx:999,rx:888,byIP:peerTrafficRates(afterIP,beforeIP)}];
    renderTrafficPeers(afterIP);
  `, context);
  const select = elements.get("traffic-ip");
  assert.equal(select.children.length, 3);
  select.value = "127.0.0.1";
  const filtered = JSON.parse(vm.runInContext("JSON.stringify(chartSamples()[0])", context));
  assert.equal(filtered.tx, 100);
  assert.equal(filtered.rx, 200);
  assert.equal(filtered.noiseTX, 10);
  assert.equal(filtered.businessRX, 70);
  vm.runInContext("renderTrafficScope(afterIP)", context);
  assert.match(elements.get("traffic-ip-summary").textContent, /127\.0\.0\.1.*2 个连接/);
  select.value = "::1";
  assert.equal(vm.runInContext("chartSamples()[0].tx", context), undefined);
  select.value = "";
  assert.equal(vm.runInContext("chartSamples()[0].tx", context), 999);
  assert.equal(vm.runInContext("peerTrafficRates({...afterIP,by_ip:[{ip:'127.0.0.1',tx_bytes:0,rx_bytes:0}]},beforeIP)['127.0.0.1']", context), null);
  select.value = "127.0.0.1";
  vm.runInContext("renderTrafficPeers({by_ip:[]})", context);
  assert.equal(select.value, "127.0.0.1");
  assert.match(select.children.at(-1).textContent, /已离线/);
});

test("tunnel usernames validate UTF-8 length and whitespace in both languages", async () => {
  const { context, elements } = await page();
  elements.get("traffic-chart").clientWidth = 0;
  for (const lang of ["zh-CN", "en"]) {
    vm.runInContext(`sgateI18n.setLanguage(${JSON.stringify(lang)})`, context);
    assert.equal(vm.runInContext('configValueError("username", "客户A", {type:"username"}, {})', context), "");
    assert.equal(vm.runInContext('configValueError("username", "", {type:"username"}, {})', context), "");
    assert.notEqual(vm.runInContext('configValueError("username", "two names", {type:"username"}, {})', context), "");
    assert.notEqual(vm.runInContext('configValueError("username", "界".repeat(43), {type:"username"}, {})', context), "");
  }
});

test("tunnel account rows hide existing passwords and retain other drafts across live saves", async () => {
  const { context, elements } = await page();
  context.accountsView = {enabled:true,path:"/private/accounts.json",revision:"v1",accounts:[{username:"alice",disabled:false},{username:"bob",disabled:false}]};
  vm.runInContext('state={authenticated:true,role:"server"}; renderTunnelAccounts(accountsView)', context);
  assert.equal(vm.runInContext('tunnelAccountRows[0].password.value',context), "");
  assert.equal(vm.runInContext('tunnelAccountRows[0].saveButton.disabled',context), true);
  vm.runInContext('tunnelAccountRows[1].password.value="bob-unsaved-secret"; tunnelAccountRows[1].password.listeners.input()',context);
  let payload;
  context.api = async (path,options) => {
    assert.equal(path,"tunnel-accounts");
    payload=JSON.parse(options.body);
    return {...context.accountsView,revision:"v2",accounts:[{username:"alice",disabled:true},{username:"bob",disabled:false}]};
  };
  await vm.runInContext('saveTunnelAccount({action:"update",username:"alice",password:"",disabled:true})',context);
  assert.equal(payload.revision,"v1");
  assert.equal(payload.password,"");
  assert.equal(vm.runInContext('tunnelAccountRows[1].password.value',context),"bob-unsaved-secret");
  assert.equal(vm.runInContext('tunnelAccountRows[0].enabled.checked',context),false);
  assert.equal(vm.runInContext('tunnelAccountsRevision',context),"v2");
  assert.match(elements.get("notice").textContent,/已保存并立即生效/);
});

test("tunnel account failures preserve drafts and never report success", async () => {
  const { context,elements }=await page();
  context.accountsView={enabled:true,revision:"old",accounts:[{username:"alice",disabled:false}]};
  vm.runInContext('state={authenticated:true,role:"server"}; renderTunnelAccounts(accountsView); tunnelAccountRows[0].password.value="unsaved-secret"',context);
  context.api=async()=>{throw new Error("客户端账号文件已变化，请刷新列表后重试")};
  await vm.runInContext('saveTunnelAccount({action:"update",username:"alice",password:"unsaved-secret"})',context);
  assert.equal(vm.runInContext('tunnelAccountRows[0].password.value',context),"unsaved-secret");
  assert.equal(vm.runInContext('tunnelAccountsRevision',context),"old");
  assert.equal(vm.runInContext('tunnelAccountRows[0].saveButton.disabled',context),false);
  assert.match(elements.get("notice").textContent,/刷新列表/);
});

test("tunnel account deletion needs confirmation and client pages never load server identities", async () => {
  const { context }=await page();
  context.accountsView={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false}]};
  vm.runInContext('state={authenticated:true,role:"server"}; renderTunnelAccounts(accountsView)',context);
  let calls=0;
  context.api=async()=>{calls++;return {enabled:true,revision:"v2",accounts:[]}};
  vm.runInContext('tunnelAccountRows[0].deleteButton.listeners.click()',context);
  assert.equal(calls,0);
  vm.runInContext('tunnelAccountRows[0].deleteButton.listeners.click()',context);
  await new Promise(setImmediate);
  assert.equal(calls,1);
  assert.equal(vm.runInContext('tunnelAccountRows.length',context),0);
  vm.runInContext('state.role="client"',context);
  await vm.runInContext('loadTunnelAccounts()',context);
  assert.equal(calls,1);
});

test("logout clears client-account secrets and ignores a delayed account list", async () => {
  const { context,elements }=await page();
  let finish;
  context.api=()=>new Promise(resolve=>{finish=resolve});
  vm.runInContext('state={authenticated:true,role:"server"}',context);
  const pending=vm.runInContext('loadTunnelAccounts()',context);
  elements.get("tunnel-account-secret").value="new-account-draft";
  vm.runInContext('epoch++;state={authenticated:false};renderSignedOut()',context);
  finish({enabled:true,revision:"late",accounts:[{username:"alice",disabled:false}]});
  await pending;
  assert.equal(elements.get("tunnel-account-secret").value,"");
  assert.equal(vm.runInContext('tunnelAccountRows.length',context),0);
  assert.equal(elements.get("tunnel-account-list").children.length,0);
});

test("first client account has editable inputs and creates and enables its JSON from the page", async () => {
  const { context,elements }=await page();
  context.setupView={enabled:false,can_create:true,revision:"config-v1",path:"/private/tunnel-accounts.json",accounts:[]};
  vm.runInContext('state={authenticated:true,role:"server"}; fields.push({key:"accounts-file",value:"",group:"代理与通道"}); renderConfigFields(fields,{},"server"); renderTunnelAccounts(setupView)',context);
  assert.equal(elements.get("tunnel-account-name").disabled,false);
  assert.equal(elements.get("tunnel-account-secret").disabled,false);
  assert.equal(elements.get("tunnel-account-add").disabled,false);
  assert.equal(elements.get("tunnel-accounts-reload").disabled,true);
  assert.match(elements.get("tunnel-account-add").textContent,/启用多账号/);
  vm.runInContext('configInputs.find(row=>row.key==="target").input.value="unsaved-other-target"; configInputs.find(row=>row.key==="target").input.listeners.input()',context);
  elements.get("tunnel-account-name").value="alice";
  elements.get("tunnel-account-secret").value="new-secret";
  let payload;
  context.api=async(_,options)=>{
    payload=JSON.parse(options.body);
    return {enabled:true,revision:"json-v1",config_changed:true,config_revision:"config-v2",path:"/private/tunnel-accounts.json",accounts:[{username:"alice",disabled:false}]};
  };
  await vm.runInContext('saveTunnelAccount({action:"create",username:"alice",password:"new-secret"})',context);
  assert.equal(payload.revision,"config-v1");
  assert.equal(vm.runInContext('revision',context),"config-v2");
  assert.equal(vm.runInContext('configInputs.find(row=>row.key==="accounts-file").input.value',context),"/private/tunnel-accounts.json");
  assert.equal(vm.runInContext('configInputs.find(row=>row.key==="target").input.value',context),"unsaved-other-target");
  assert.equal(elements.get("tunnel-account-secret").value,"");
  assert.equal(elements.get("tunnel-accounts-reload").disabled,false);
});

test("explicit disabled accounts show the reason while username and password stay editable", async () => {
  const { context,elements }=await page();
  context.setupView={enabled:false,can_create:false,setup_message:"命令行将 accounts-file 设为空，请移除此覆盖并重启后再新增账号",accounts:[]};
  vm.runInContext('renderTunnelAccounts(setupView)',context);
  assert.equal(elements.get("tunnel-account-name").disabled,false);
  assert.equal(elements.get("tunnel-account-secret").disabled,false);
  assert.equal(elements.get("tunnel-account-add").disabled,true);
  assert.match(elements.get("tunnel-accounts-help").textContent,/命令行/);
});


test("client credentials are editable, grouped, masked and saved together with CLI badges", async () => {
  const { context, elements } = await page();
  context.credentials = [
    {key:"username",value:"alice",group:"服务端连接认证",validation:{type:"username",max:"128"}},
    {key:"password",value:"__SGATE_KEEP_EXISTING_PASSWORD__",secret:true,group:"服务端连接认证",validation:{type:"password",min:"1",max:"1024"}},
  ];
  vm.runInContext('renderConfigFields(credentials, {cli_overrides:["username"]}, "client")', context);
  const username = vm.runInContext('configInputs.find(e => e.key === "username")', context);
  const password = vm.runInContext('configInputs.find(e => e.key === "password")', context);
  assert.equal(username.input.disabled, false);
  assert.equal(password.input.disabled, false);
  assert.equal(password.input.type, "password");
  assert.equal(password.input.value, "");
  const group = elements.get("config-fields").children.find(g => g.children[0].textContent === "服务端连接认证");
  assert.ok(group);
  assert.equal(group.children[1].children[0].children[0].textContent, "服务端账号 (username)");
  assert.equal(group.children[1].children[0].children[2].textContent, "命令行优先");
  assert.equal(vm.runInContext('JSON.stringify(configEdits())', context), "{}");
  username.input.value = "bad user";
  username.input.listeners.input();
  assert.match(username.input.validationMessage, /空白/);
  username.input.value = "bob";
  username.input.listeners.input();
  password.input.value = "bob-secret";
  password.input.listeners.input();
  let payload;
  context.api = async (_, options) => {
    if (options) { payload = JSON.parse(options.body); return {applied:true}; }
    return {fields:context.credentials,role:"client",revision:"saved"};
  };
  vm.runInContext('revision="before"; state.authenticated=true', context);
  await vm.runInContext('saveConfig(true)', context);
  assert.deepEqual(payload, {fields:{username:"bob",password:"bob-secret"},revision:"before",apply:true});
  assert.equal(vm.runInContext('configInputs.find(e => e.key === "password").input.value', context), "");
});

test("mapping credentials stay private across redraw/save, preserve markers, and can inherit common credentials", async () => {
  const {context} = await page();
  vm.runInContext('renderConfigFields([{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost/tunnel",username:"alice",password:"__SGATE_KEEP_EXISTING_PASSWORD__"}]}],{},"client");revision="before"',context);
  const entry=vm.runInContext('configInputs[0]',context);
  let row=entry.rows[0];
  assert.equal(row.auth.checked,true);
  assert.equal(row.password.type,"password");
  assert.equal(row.password.value,"");
  assert.equal(entry.read()[0].password,"__SGATE_KEEP_EXISTING_PASSWORD__");
  row.password.value="draft-secret"; row.password.listeners.input();
  entry.controls[0].listeners.click();
  assert.equal(entry.rows[0].password.value,"draft-secret");
  entry.rows[1].listen.value="127.0.0.1:9002";
  entry.rows[1].server.value="ws://localhost/tunnel";
  row=entry.rows[0];
  let request;
  context.api=async(_,options)=>{request=JSON.parse(options.body);return {revision:"after",live_applied:["proxies"],requires_restart:false}};
  await vm.runInContext('saveConfigField(configInputs[0])',context);
  assert.equal(request.proxies[0].username,"alice");
  assert.equal(request.proxies[0].password,"draft-secret");
  assert.equal(row.password.value,"");
  assert.equal(entry.read()[0].password,"__SGATE_KEEP_EXISTING_PASSWORD__");
  row.auth.checked=false;row.auth.listeners.change();
  await vm.runInContext('saveConfigField(configInputs[0])',context);
  assert.equal(request.proxies[0].username,undefined);
  assert.equal(request.proxies[0].password,undefined);
  row.auth.checked=true;row.auth.listeners.change();
  row.username.value="bob";row.username.listeners.input();
  assert.equal(entry.read()[0].password,undefined,"new blank password should inherit common credentials");
  row.password.value="x".repeat(1025);row.password.listeners.input();
  request=null;
  await vm.runInContext('saveConfigField(configInputs[0])',context);
  assert.equal(request,null,"invalid mapping credentials must not be submitted");
});

test("server accounts-file shows live status and single-row save preserves unrelated drafts", async () => {
  const {context,elements}=await page();
  vm.runInContext('renderConfigFields([{key:"accounts-file",value:"",group:"代理与通道",validation:{type:"file",live:true}},{key:"target",value:"localhost:8080",group:"代理与通道"}],{cli_overrides:["accounts-file"]},"server");revision="old"',context);
  const group=elements.get("config-fields").children[0];
  assert.match(group.children[1].children[0].children.at(-1).textContent,/即时生效.*命令行已覆盖/);
  vm.runInContext('configInputs[0].input.value="tunnel-accounts.json";configInputs[0].input.listeners.input();configInputs[1].input.value="localhost:8088";configInputs[1].input.listeners.input()',context);
  let payload;
  context.api=async(_,options)=>{payload=JSON.parse(options.body);return {revision:"new",live_applied:["accounts-file"],requires_restart:false,runtime:{cli_overrides:["accounts-file"]}}};
  await vm.runInContext('saveConfigField(configInputs[0])',context);
  assert.deepEqual(payload,{fields:{"accounts-file":"tunnel-accounts.json"},revision:"old",apply:false});
  assert.equal(vm.runInContext('configInputs[1].input.value',context),"localhost:8088");
  assert.equal(vm.runInContext('configInputs[1].dirty',context),true);
  assert.match(elements.get("notice").textContent,/无需重启代理/);
  assert.match(elements.get("notice").textContent,/命令行覆盖项仍使用启动值/);
});

test("account reverse mappings render, edit and save through the row button", async () => {
  const {context}=await page();
  context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false,reverse_mappings:[{listen:"127.0.0.1:10022",target:"127.0.0.1:22"},{listen:"[::1]:18080",target:"intranet.example:8080"}]}]};
  vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view)',context);
  let row=vm.runInContext('tunnelAccountRows[0]',context);
  assert.equal(row.reverse.rows.length,2);
  assert.equal(row.reverse.rows[1].target.value,"intranet.example:8080");
  assert.equal(row.saveButton.disabled,true);
  row.reverse.rows[0].target.value="192.168.1.20:22";
  row.reverse.rows[0].target.listeners.input();
  assert.equal(row.saveButton.disabled,false);
  let payload;
  context.api=async(_,options)=>{payload=JSON.parse(options.body);return {...context.view,revision:"v2",accounts:[{username:"alice",disabled:false,reverse_mappings:payload.reverse_mappings}]}};
  await row.saveButton.listeners.click();
  assert.deepEqual(payload,{action:"update",username:"alice",password:"",disabled:false,revision:"v1",reverse_mappings:[{listen:"127.0.0.1:10022",target:"192.168.1.20:22"},{listen:"[::1]:18080",target:"intranet.example:8080"}]});
  row=vm.runInContext('tunnelAccountRows[0]',context);
  assert.equal(row.saveButton.disabled,true);
  assert.equal(row.password.value,"");
});

test("removing all reverse mappings sends an explicit empty array and locks controls during save", async () => {
  const {context}=await page();
  context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false,reverse_mappings:[{listen:"127.0.0.1:10022",target:"localhost:22"}]}]};
  vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view)',context);
  const row=vm.runInContext('tunnelAccountRows[0]',context);
  row.reverse.rows[0].removeButton.listeners.click();
  assert.equal(row.reverse.rows.length,0);
  assert.equal(row.saveButton.disabled,false);
  let payload,finish;
  context.api=(_,options)=>{payload=JSON.parse(options.body);return new Promise(resolve=>{finish=resolve})};
  const pending=row.saveButton.listeners.click();
  assert.deepEqual(payload.reverse_mappings,[]);
  assert.equal(row.reverse.addButton.disabled,true);
  assert.equal(vm.runInContext('tunnelAccountCreateMappings.addButton.disabled',context),true);
  row.reverse.addButton.listeners.click();
  assert.equal(row.reverse.rows.length,0);
  finish({enabled:true,revision:"v2",accounts:[{username:"alice",disabled:false}]});
  await pending;
  assert.equal(vm.runInContext('tunnelAccountRows[0].reverse.rows.length',context),0);
  assert.equal(vm.runInContext('tunnelAccountRows[0].saveButton.disabled',context),true);
});

test("reverse mapping validation blocks incomplete, duplicate and invalid addresses in both languages", async () => {
  const {context,elements}=await page();
  elements.get("traffic-chart").clientWidth=0;
  context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false},{username:"bob",disabled:false,reverse_mappings:[{listen:"127.0.0.1:10022",target:"localhost:22"}]}]};
  vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view)',context);
  const account=vm.runInContext('tunnelAccountRows[0]',context);
  account.reverse.addButton.listeners.click();
  const row=account.reverse.rows[0];
  let calls=0;context.api=async()=>{calls++;throw new Error("must not submit")};
  for(const language of ["zh-CN","en"]){
    vm.runInContext(`sgateI18n.setLanguage(${JSON.stringify(language)})`,context);
    for(const [listen,target] of [["",""],["localhost:10023","localhost:22"],["127.0.0.1:0","localhost:22"],["127.0.0.1:65536","localhost:22"],["127.0.0.1:010023","localhost:22"],["127.0.0.1:10023",""],["127.0.0.1:10023","http://localhost:80"],["127.0.0.1:10023","localhost:80\n"],["127.0.0.1:10022","localhost:22"]]){
      row.listen.value=listen;row.target.value=target;
      await account.saveButton.listeners.click();
      assert.ok(row.listen.validationMessage||row.target.validationMessage,`${listen} / ${target}`);
      if(language==="en")assert.doesNotMatch(row.listen.validationMessage+row.target.validationMessage,/[\p{Script=Han}]/u);
    }
  }
  assert.equal(calls,0);
  row.listen.value="127.0.0.1:10023";row.target.value="localhost:22";
  account.reverse.addButton.listeners.click();
  const duplicate=account.reverse.rows[1];duplicate.listen.value=row.listen.value;duplicate.target.value="localhost:80";
  await account.saveButton.listeners.click();
  assert.match(duplicate.listen.validationMessage,/already used/);
  assert.equal(calls,0);
  for(const listen of ["127.0.0.1:1","0.0.0.0:65535","[::1]:10023","[::]:10023"]){
    context.address=listen;
    assert.equal(vm.runInContext('reverseMappingAddressError(address,true)',context),"");
  }
});

test("mapping drafts survive other account saves, errors and language switches", async () => {
  const {context,elements}=await page();elements.get("traffic-chart").clientWidth=0;
  context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false},{username:"bob",disabled:false,reverse_mappings:[{listen:"127.0.0.1:10022",target:"localhost:22"}]}]};
  vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view)',context);
  let bob=vm.runInContext('tunnelAccountRows[1]',context);
  bob.reverse.rows[0].target.value="192.168.1.20:22";bob.reverse.rows[0].target.listeners.input();
  vm.runInContext('tunnelAccountCreateMappings.addButton.listeners.click();tunnelAccountCreateMappings.rows[0].listen.value="127.0.0.1:10080"',context);
  context.api=async()=>({...context.view,revision:"v2"});
  await vm.runInContext('saveTunnelAccount({action:"update",username:"alice",password:"new-secret"})',context);
  bob=vm.runInContext('tunnelAccountRows[1]',context);
  assert.equal(bob.reverse.rows[0].target.value,"192.168.1.20:22");
  assert.equal(bob.saveButton.disabled,false);
  assert.equal(vm.runInContext('tunnelAccountCreateMappings.rows[0].listen.value',context),"127.0.0.1:10080");
  vm.runInContext('sgateI18n.setLanguage("en")',context);
  assert.equal(bob.reverse.rows[0].target.value,"192.168.1.20:22");
  context.api=async()=>{throw new Error("账号未生效，已恢复原账号文件")};
  await bob.saveButton.listeners.click();
  assert.equal(bob.reverse.rows[0].target.value,"192.168.1.20:22");
  assert.equal(bob.reverse.rows[0].target.disabled,false);
  assert.equal(bob.saveButton.disabled,false);
  assert.equal(vm.runInContext('tunnelAccountsRevision',context),"v2");
  assert.match(elements.get("notice").textContent,/failed/);
});

test("new accounts can configure reverse mappings atomically and logout clears the editor", async () => {
  const {context,elements}=await page();
  context.view={enabled:false,can_create:true,revision:"setup-v1",accounts:[]};
  vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view);tunnelAccountCreateMappings.addButton.listeners.click()',context);
  elements.get("tunnel-account-name").value="alice";elements.get("tunnel-account-secret").value="new-secret";
  const editor=vm.runInContext('tunnelAccountCreateMappings',context);
  editor.rows[0].listen.value="127.0.0.1:10022";editor.rows[0].target.value="localhost:22";
  let payload;
  context.api=async(_,options)=>{payload=JSON.parse(options.body);return{enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false,reverse_mappings:payload.reverse_mappings}]}};
  elements.get("tunnel-account-create").listeners.submit({preventDefault(){}});
  await new Promise(setImmediate);
  assert.deepEqual(payload.reverse_mappings,[{listen:"127.0.0.1:10022",target:"localhost:22"}]);
  assert.equal(payload.action,"create");assert.equal(payload.revision,"setup-v1");
  assert.equal(vm.runInContext('tunnelAccountCreateMappings.rows.length',context),0);
  assert.equal(vm.runInContext('tunnelAccountRows[0].reverse.rows.length',context),1);
  vm.runInContext('epoch++;state={authenticated:false};renderSignedOut()',context);
  assert.equal(vm.runInContext('tunnelAccountCreateMappings',context),null);
  assert.equal(elements.get("tunnel-account-create-mappings").children.length,0);
});

test("reverse mapping editors cap additions at 256 and retain saved mappings on password-only edits", async () => {
  const {context}=await page();
  context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false,reverse_mappings:[{listen:"127.0.0.1:10022",target:"localhost:22"}]}]};
  vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view)',context);
  const row=vm.runInContext('tunnelAccountRows[0]',context);
  row.password.value="password-draft";
  context.view.accounts[0].reverse_mappings[0].target="192.168.1.20:22";
  vm.runInContext('renderTunnelAccounts(view)',context);
  assert.equal(vm.runInContext('tunnelAccountRows[0].reverse.rows[0].target.value',context),"192.168.1.20:22");
  const editor=vm.runInContext('tunnelAccountCreateMappings',context);
  for(let i=0;i<256;i++)editor.add({listen:`127.0.0.1:${10000+i}`,target:"localhost:22"});
  assert.equal(editor.rows.length,256);assert.equal(editor.addButton.disabled,true);
  editor.addButton.listeners.click();assert.equal(editor.rows.length,256);
});

test("client mappings deny reverse proxy by default and require independent opt-in", async () => {
  const {context,elements}=await page();elements.get("traffic-chart").clientWidth=0;
  context.mappingFields=[{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[
    {listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel"},
    {listen:"127.0.0.1:9002",server:"ws://localhost:8081/tunnel",allow_reverse:true}
  ]}];
  vm.runInContext('state={authenticated:true,role:"client"};renderConfigFields(mappingFields,{},"client");revision="v1"',context);
  const entry=vm.runInContext('configInputs[0]',context);
  assert.equal(entry.rows[0].allowReverse.checked,false);
  assert.equal(entry.rows[1].allowReverse.checked,true);
  assert.equal(entry.saveButton.disabled,true);
  entry.rows[0].allowReverse.checked=true;entry.rows[0].allowReverse.listeners.change();
  assert.equal(entry.saveButton.disabled,false);
  let payload,finish;
  context.api=(_,options)=>{payload=JSON.parse(options.body);return new Promise(resolve=>{finish=resolve})};
  const pending=vm.runInContext('saveConfigField(configInputs[0])',context);
  assert.equal(payload.proxies[0].allow_reverse,true);
  assert.equal(payload.proxies[1].allow_reverse,true);
  assert.equal(entry.rows[0].allowReverse.disabled,true);
  finish({revision:"v2"});await pending;
  vm.runInContext('sgateI18n.setLanguage("en")',context);
  assert.equal(entry.rows[0].allowReverse.checked,true);
  // Adding a mapping must not inherit the preceding mapping's consent.
  entry.controls[0].listeners.click();
  assert.equal(entry.rows[2].allowReverse.checked,false);
  assert.equal(entry.rows[0].allowReverse.checked,true);
});

test("client mapping reverse permission can be revoked and failed saves preserve consent drafts", async () => {
  const {context}=await page();
  context.mappingFields=[{key:"proxies",kind:"proxies",group:"代理与通道",mappings:[{listen:"127.0.0.1:9001",server:"ws://localhost:8081/tunnel",allow_reverse:true}]}];
  vm.runInContext('state={authenticated:true,role:"client"};renderConfigFields(mappingFields,{},"client");revision="v1"',context);
  const entry=vm.runInContext('configInputs[0]',context);
  entry.rows[0].allowReverse.checked=false;entry.rows[0].allowReverse.listeners.change();
  let payload;
  context.api=async(_,options)=>{payload=JSON.parse(options.body);throw new Error("save failed")};
  await vm.runInContext('saveConfigField(configInputs[0])',context);
  assert.notEqual(payload.proxies[0].allow_reverse,true);
  assert.equal(entry.rows[0].allowReverse.checked,false);
  assert.equal(entry.rows[0].allowReverse.disabled,false);
  assert.equal(entry.dirty,true);
  context.api=async(_,options)=>{payload=JSON.parse(options.body);return {revision:"v2"}};
  await vm.runInContext('saveConfigField(configInputs[0])',context);
  assert.equal(entry.dirty,false);
  assert.equal(entry.rows[0].allowReverse.checked,false);
});

test("managed reverse account save synchronizes listeners and restarts the server while preserving drafts", async () => {
  const {context,elements}=await page();
  context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false,reverse_mappings:[]}]};
  vm.runInContext('state={authenticated:true,role:"server"};renderConfigFields([{key:"master_service",value:"|1080",group:"监听"},{key:"target",value:"localhost:80",group:"代理与通道"}],{},"server");revision="old";configInputs[1].input.value="keep-draft";configInputs[1].input.listeners.input();renderTunnelAccounts(view)',context);
  const calls=[];
  context.renderRuntime=()=>{};
  context.waitForServiceRestart=async(pid,listeners)=>{
    assert.equal(pid,101);assert.deepEqual(listeners,[":1080","127.0.0.1:10022"]);
    return {revision:"new",runtime:{pid:102,running:true}};
  };
  context.api=async(route,options)=>{
    calls.push(route);
    if(route==="tunnel-accounts") return {...context.view,revision:"v2",listeners_changed:true,config_revision:"saved",synced_fields:{master_service:"|1080, 127.0.0.1|10022"},service_restart_required:true,runtime:{pid:101,pending_listeners:[":1080","127.0.0.1:10022"]}};
    assert.equal(route,"restart-service");assert.equal(JSON.parse(options.body).expected_pid,101);
    // The old process may exit before returning the restart response.
    throw new Error("connection reset");
  };
  await vm.runInContext('saveTunnelAccount({action:"update",username:"alice",reverse_mappings:[{listen:"127.0.0.1:10022",target:"localhost:22"}]})',context);
  assert.deepEqual(calls,["tunnel-accounts","restart-service"]);
  assert.equal(vm.runInContext('configInputs[0].input.value',context),"|1080, 127.0.0.1|10022");
  assert.equal(vm.runInContext('configInputs[1].input.value',context),"keep-draft");
  assert.equal(vm.runInContext('configInputs[1].dirty',context),true);
  assert.equal(vm.runInContext('revision',context),"new");
  assert.match(elements.get("notice").textContent,/反向映射已生效，服务端.*监听端口已核对/);
  assert.equal(vm.runInContext('serviceRestarting || tunnelAccountsBusy',context),false);
});

test("failed server restart preserves saved reverse mappings and never reports them active",async()=>{
 const {context,elements}=await page();
 context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false,reverse_mappings:[]}]};
 vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view)',context);
 context.renderRuntime=()=>{};
 context.api=async(route)=>{
  if(route==="tunnel-accounts")return {...context.view,revision:"v2",service_restart_required:true,runtime:{pid:101}};
  throw Object.assign(new Error("stop script failed"),{status:409});
 };
 await vm.runInContext('saveTunnelAccount({action:"update",username:"alice"})',context);
 assert.equal(vm.runInContext('tunnelAccountsRevision',context),"v2");
 assert.match(elements.get("notice").textContent,/stop script failed/);
 assert.doesNotMatch(elements.get("notice").textContent,/已生效/);
 assert.equal(vm.runInContext('serviceRestarting || tunnelAccountsBusy',context),false);
});

test("reverse listeners reject WebSocket service ports across bind IPs in both languages",async()=>{
 const {context}=await page();
 context.drawChart=()=>{};
 context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:true,reverse_mappings:[{listen:"127.0.0.1:10022",target:"localhost:22"}]}]};
 vm.runInContext('state={authenticated:true,role:"server"};renderRuntime({websocket_listeners:["127.0.0.1:1080","[::1]:8081"]});renderTunnelAccounts(view)',context);
 let calls=0;context.api=async()=>{calls++;throw new Error("must not save invalid mapping")};
 for(const language of ["zh","en"]){
  vm.runInContext(`sgateI18n.setLanguage("${language}")`,context);
  const account=vm.runInContext('tunnelAccountRows[0]',context),row=account.reverse.rows[0];
  for(const listen of ["127.0.0.1:1080","127.0.0.2:1080","0.0.0.0:1080","[::]:1080","[::1]:8081"]){
   row.listen.value=listen;row.listen.listeners.input();
   await account.saveButton.listeners.click();
   assert.match(row.listen.validationMessage,/WebSocket/);
   assert.match(row.errors.listen.textContent,/WebSocket/);
   if(language==="en")assert.doesNotMatch(row.listen.validationMessage,/[\p{Script=Han}]/u);
  }
  row.listen.value="127.0.0.1:10022";row.listen.listeners.input();
  assert.equal(row.listen.validationMessage,"");
 }
 assert.equal(calls,0);
});

test("reverse TLS defaults off, supports tunnel or independent certificates, and preserves failed drafts",async()=>{
 const {context}=await page();context.drawChart=()=>{};
 context.view={enabled:true,revision:"v1",accounts:[{username:"alice",disabled:false,reverse_mappings:[{listen:"127.0.0.1:10022",target:"localhost:443"}]}]};
 vm.runInContext('state={authenticated:true,role:"server"};renderTunnelAccounts(view)',context);
 let account=vm.runInContext('tunnelAccountRows[0]',context),row=account.reverse.rows[0];
 assert.equal(row.listenTLS.checked,false);assert.equal(row.targetTLS.checked,false);assert.equal(row.reuseCert.checked,true);
 row.listenTLS.checked=true;row.listenTLS.listeners.change();row.targetTLS.checked=true;row.targetTLS.listeners.change();
 let payload;context.api=async(_,options)=>{payload=JSON.parse(options.body);return {...context.view,revision:"v2",accounts:[{username:"alice",disabled:false,reverse_mappings:payload.reverse_mappings}]}};
 await account.saveButton.listeners.click();
 assert.deepEqual(payload.reverse_mappings,[{listen:"127.0.0.1:10022",target:"localhost:443",listen_tls:true,target_tls:true}]);
 account=vm.runInContext('tunnelAccountRows[0]',context);row=account.reverse.rows[0];
 row.reuseCert.checked=false;row.reuseCert.listeners.change();
 let calls=0;context.api=async()=>{calls++;throw new Error("must not save incomplete certificates")};
 await account.saveButton.listeners.click();assert.equal(calls,0);assert.ok(row.tlsCert.validationMessage);assert.ok(row.tlsKey.validationMessage);
 row.tlsCert.value="/srv/cert.pem";row.tlsKey.value="/srv/key.pem";row.tlsCert.listeners.input();row.tlsKey.listeners.input();
 let finish;context.api=(_,options)=>{payload=JSON.parse(options.body);return new Promise((resolve,reject)=>{finish=reject})};
 const pending=account.saveButton.listeners.click();
 assert.equal(row.listenTLS.disabled,true);assert.equal(row.targetTLS.disabled,true);assert.equal(row.tlsKey.disabled,true);
 assert.deepEqual(payload.reverse_mappings,[{listen:"127.0.0.1:10022",target:"localhost:443",listen_tls:true,listen_tls_cert:"/srv/cert.pem",listen_tls_key:"/srv/key.pem",target_tls:true}]);
 finish(new Error("bad certificate"));await pending;
 assert.equal(row.tlsCert.value,"/srv/cert.pem");assert.equal(row.tlsKey.disabled,false);
 vm.runInContext('sgateI18n.setLanguage("en")',context);
 assert.equal(row.targetTLS.checked,true);
 row.reuseCert.checked=true;row.reuseCert.listeners.change();
 assert.equal(row.tlsKey.disabled,true);
 assert.equal(account.reverse.read()[0].listen_tls_cert,undefined);
 row.listenTLS.checked=false;row.listenTLS.listeners.change();row.targetTLS.checked=false;row.targetTLS.listeners.change();
 assert.deepEqual(JSON.parse(JSON.stringify(account.reverse.read())),[{listen:"127.0.0.1:10022",target:"localhost:443"}]);
});

 test("QUIC endpoint validation requires an explicit UDP port", async () => {
 const {context} = await page();
 for (const value of ["quic://example.com:8443/tunnel", "quic://[::1]:443/tunnel?addr=localhost:80"]) {
  context.quicTestURL = value;
  assert.doesNotThrow(() => vm.runInContext("configURL(quicTestURL)", context));
 }
 for (const value of ["quic://example.com/tunnel", "quic://example.com:0/tunnel", "quic://user:pass@example.com:443/tunnel"]) {
  context.quicTestURL = value;
  assert.throws(() => vm.runInContext("configURL(quicTestURL)", context));
 }
 });

test("QUIC transport metrics distinguish packet traffic, RTT and application replay", async () => {
  const { context } = await page();
  context.drawChart = () => {};
  assert.equal(vm.runInContext("quicObservationText({})", context), "");
  context.quicSample = {quic: {connections_total: 3, active_connections: 2, max_smoothed_rtt_ms: 12.345, lost_packets: 7, packet_tx_bytes: 4096, packet_rx_bytes: 2048, application_replay_bytes: 1024}};
  const zh = vm.runInContext("quicObservationText(quicSample)", context);
  assert.match(zh, /12\.35 ms/);
  assert.match(zh, /丢包判定累计：7/);
  vm.runInContext('sgateI18n.setLanguage("en")', context);
  const en = vm.runInContext("quicObservationText(quicSample)", context);
  assert.match(en, /Packets declared lost: 7/);
  assert.match(en, /Application replay:/);
});

test("degraded transport health remains distinct from a running TCP service", async () => {
  const {context,elements}=await page();
  context.runtimeSample={running:true,healthy:false,generation:1,listen:"127.0.0.1:8081",quic:{enabled:true,healthy:false}};
  vm.runInContext("renderRuntime(runtimeSample)",context);
  assert.match(elements.get("runtime-state").textContent,/降级/);
});

test("TCP and QUIC curves use independent counters, IP filtering and protocol selection", async () => {
  const { context, elements } = await page();
  context.before = {time_ms:1000,tx_bytes:100,rx_bytes:100,
    transports:{tcp:{tx_bytes:60,rx_bytes:80},quic:{tx_bytes:40,rx_bytes:20}}};
  context.after = {time_ms:3000,tx_bytes:300,rx_bytes:500,
    transports:{tcp:{tx_bytes:100,rx_bytes:200},quic:{tx_bytes:200,rx_bytes:300}}};
  const rates = JSON.parse(vm.runInContext("JSON.stringify(trafficRates(after,before))",context));
  assert.equal(rates.tcpTX,20);
  assert.equal(rates.tcpRX,60);
  assert.equal(rates.quicTX,80);
  assert.equal(rates.quicRX,140);
  assert.equal(rates.tcpTX+rates.quicTX,rates.tx);
  assert.equal(rates.tcpRX+rates.quicRX,rates.rx);
  vm.runInContext("points=[trafficRates(after,before)]",context);
  assert.deepEqual(JSON.parse(vm.runInContext("JSON.stringify(chartMetric().keys)",context)),["tcpTX","tcpRX","quicTX","quicRX"]);
  vm.runInContext("renderChartSeries(chartMetric())",context);
  assert.equal(elements.get("traffic-quic-legend").hidden,false);
  assert.equal(elements.get("traffic-legend-label-0").textContent,"TCP 发送");
  assert.equal(elements.get("traffic-legend-label-2").textContent,"QUIC 发送");
  elements.get("traffic-metric").value="quic";
  assert.deepEqual(JSON.parse(vm.runInContext("JSON.stringify(chartMetric().keys)",context)),["quicTX","quicRX"]);
  vm.runInContext("renderChartSeries(chartMetric())",context);
  assert.equal(elements.get("traffic-quic-legend").hidden,true);
  elements.get("traffic-metric").value="tcp";
  assert.deepEqual(JSON.parse(vm.runInContext("JSON.stringify(chartMetric().keys)",context)),["tcpTX","tcpRX"]);
  elements.get("traffic-ip").value="127.0.0.1";
  vm.runInContext('points[0].byIP={"127.0.0.1":{...points[0],tcpTX:7,quicTX:9}}',context);
  assert.equal(vm.runInContext("chartSamples()[0].tcpTX",context),7);
  assert.equal(vm.runInContext("chartSamples()[0].quicTX",context),9);
  assert.equal(vm.runInContext("trafficRates({...after,transports:{tcp:{tx_bytes:1}}},before).tcpTX",context),null);
  assert.equal(vm.runInContext("trafficRates({...after,transports:undefined},before).quicTX",context),undefined);
});

test("combined transport chart draws four colors and shows all four hover rates", async () => {
  const {context,elements}=await page();
  const canvas=context.document.getElementById("traffic-chart"),colors=[];
  const drawing={scale(){},setLineDash(){},beginPath(){},moveTo(){},lineTo(){},stroke(){colors.push(this.strokeStyle)},closePath(){},fill(){},fillText(){},arc(){},createLinearGradient(){return {addColorStop(){}}}};
  canvas.clientWidth=900;canvas.clientHeight=250;canvas.getContext=()=>drawing;
  const tooltip=context.document.getElementById("traffic-tooltip");
  tooltip.style={};tooltip.offsetWidth=230;tooltip.offsetHeight=150;
  vm.runInContext('points=[{time_ms:60000,tcpTX:10,tcpRX:20,quicTX:30,quicRX:40}];chartPointer={x:868,y:100};drawChart()',context);
  for(const color of ["#087e68","#5a83d7","#d07818","#9455c8"])assert.ok(colors.includes(color),color);
  assert.equal(tooltip.hidden,false);
  assert.equal(elements.get("traffic-tooltip-tx").textContent,"10 B/s");
  assert.equal(elements.get("traffic-tooltip-rx").textContent,"20 B/s");
  assert.equal(elements.get("traffic-tooltip-quic-tx").textContent,"30 B/s");
  assert.equal(elements.get("traffic-tooltip-quic-rx").textContent,"40 B/s");
  elements.get("traffic-metric").value="quic";
  colors.length=0;
  vm.runInContext("drawChart()",context);
  assert.equal(elements.get("traffic-tooltip-quic-tx-row").hidden,true);
  assert.equal(elements.get("traffic-tooltip-tx").textContent,"30 B/s");
  assert.equal(elements.get("traffic-legend-dot-0").className,"quic-tx-dot");
  assert.ok(colors.includes("#d07818"));
  assert.ok(!colors.includes("#087e68"));
});


test("noise form explains QUIC per-second units independently of WS bursts", async () => {
  const { context, elements } = await page();
  context.rateField = {key:"noise-max-bytes",value:"1M",group:"代理与通道",validation:{type:"bytes",min:"0",max:"10485760",hint:""}};
  vm.runInContext('renderConfigFields([rateField],{},"server")', context);
  const row=elements.get("config-fields").children[0].children[1];
  assert.match(row.children[0].children[1].textContent,/QUIC 为每秒/);
  assert.match(row.children[0].children[1].textContent,/WS\/WSS 为每批/);
  context.statusNoise={mode:"fixed",interval:"1s",min_bytes:1048576,max_bytes:1048576,traffic:true,idle:true,empty:true};
  vm.runInContext('renderRuntime({running:true,noise:statusNoise})', context);
  assert.match(elements.get("runtime-state").textContent,/WS 每批 \/ QUIC 每秒/);
});

test("VPN status and IP bandwidth use independent counters and survive a VPN restart", async () => {
  const { context } = await page();
  const result = vm.runInContext(`JSON.stringify(trafficRates(
    {time_ms:2000,tx_bytes:10,rx_bytes:20,health:{vpn:{stats:{tx_ip_bytes:4096,rx_ip_bytes:8192}}}},
    {time_ms:1000,tx_bytes:10,rx_bytes:20,health:{vpn:{stats:{tx_ip_bytes:1024,rx_ip_bytes:2048}}}}
  ))`, context);
  const rates = JSON.parse(result);
  assert.equal(rates.vpnTX, 3072);
  assert.equal(rates.vpnRX, 6144);
  assert.equal(rates.tx, 0);
  const status = vm.runInContext('vpnObservationText({enabled:true,state:"failed",last_error:"permission denied",stats:{}})',context);
  assert.match(status,/permission denied/);
  assert.doesNotMatch(status,/undefined/);
  assert.equal(vm.runInContext('trafficRates({time_ms:2000,tx_bytes:0,rx_bytes:0,health:{vpn:{stats:{tx_ip_bytes:0}}}},{time_ms:1000,tx_bytes:0,rx_bytes:0,health:{vpn:{stats:{tx_ip_bytes:100}}}}).vpnTX',context), null);
});

test("VPN-only configuration hides proxy creation and respects mode CLI overrides", async () => {
  const {context} = await page();
  vm.runInContext('fields=[{key:"vpn-mode",value:"only",group:"独立 VPN",validation:{type:"enum",choices:["disabled","both","only"]}}]; renderConfigFields(fields,{},"client")', context);
  assert.equal(vm.runInContext('configInputs.some(row=>row.field?.key==="proxies")',context),false);
  assert.equal(vm.runInContext('configInputs.length',context),1);
  vm.runInContext('renderConfigFields([{key:"vpn-mode",value:"disabled",group:"独立 VPN"}],{cli_overrides:["vpn-mode"],vpn_mode:"only"},"client")',context);
  assert.equal(vm.runInContext('configInputs.length',context),1);
});

test("VPN-only form can save without a proxy upstream or backend", async () => {
  const {context} = await page();
  vm.runInContext(`renderConfigFields([
    {key:"vpn-mode",value:"only",group:"独立 VPN"},
    {key:"vpn-server",value:"quic://vpn.example.com:8444/vpn",group:"独立 VPN",validation:{type:"url"}},
    {key:"server",value:"",group:"代理与通道",validation:{type:"url"}},
    {key:"target",value:"",group:"代理与通道",validation:{type:"address",required:true}}
  ],{},"client")`,context);
  assert.equal(vm.runInContext('validateConfigForm(null,false)',context),true);
  assert.equal(vm.runInContext('configRelationErrors({"vpn-mode":"both","vpn-server":""}).has("vpn-server")',context),true);
});
