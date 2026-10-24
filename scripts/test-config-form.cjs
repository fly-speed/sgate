// Exercise the real page's form behavior, including hidden secrets and failed saves.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

async function page() {
  const elements = new Map();
  function element() {
    return {
      children: [],
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
      reportValidity: () => true,
    };
  }
  const context = vm.createContext({
    document: {
      createElement: element,
      getElementById(id) {
        if (!elements.has(id)) elements.set(id, element());
        return elements.get(id);
      },
      querySelectorAll: () => [],
    },
    window: { addEventListener() {} },
    AbortController,
    setTimeout,
    clearTimeout,
    fetch: async () => ({ ok: true, json: async () => ({ authenticated: false }) }),
  });
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
