// Exercise the actual page's asynchronous restart flow without a browser dependency.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

async function page() {
  const elements = new Map();
  const context = vm.createContext({
    document: {
      getElementById(id) {
        if (!elements.has(id)) {
          elements.set(id, {
            addEventListener() {},
            replaceChildren() {},
            classList: { toggle() {} },
          });
        }
        return elements.get(id);
      },
      querySelectorAll: () => [],
    },
    window: { addEventListener() {} },
    AbortController,
    setTimeout: (fn, ms) => setTimeout(fn, ms === 1000 ? 0 : ms),
    clearTimeout,
    fetch: async () => ({ ok: true, json: async () => ({ authenticated: false }) }),
  });
  const source = fs.readFileSync(path.join(__dirname, "../web/admin/app.js"), "utf8");
  vm.runInContext(source, context);
  await new Promise(setImmediate);
  vm.runInContext(
    "state = {authenticated: true}; servicePID = 101; refreshAuth = async () => {};",
    context,
  );
  return { context, elements };
}

test("restart survives a lost response and waits for a running replacement PID", async () => {
  const { context, elements } = await page();
  const requests = [];
  const configurations = [
    new Error("listener unavailable"),
    { runtime: { pid: 101, running: true, service_restart_pending: true } },
    { runtime: { pid: 102, running: false } },
    { runtime: { pid: 102, running: true } },
  ];
  context.api = async (route, options) => {
    requests.push(route);
    if (route === "restart-service") {
      assert.equal(JSON.parse(options.body).expected_pid, 101);
      throw new Error("connection reset before response");
    }
    const next = configurations.shift();
    if (next instanceof Error) throw next;
    assert.ok(next, "unexpected extra poll");
    return next;
  };
  const pending = vm.runInContext("restartService()", context);
  assert.equal(elements.get("apply-config").disabled, true);
  await vm.runInContext("restartService()", context);
  await pending;
  assert.equal(requests.filter((route) => route === "restart-service").length, 1);
  assert.equal(configurations.length, 0);
  assert.match(elements.get("notice").textContent, /服务已由 acl_master 重启/);
  assert.equal(elements.get("apply-config").disabled, false);
});

test("an explicit HTTP refusal is shown without polling for replacement", async () => {
  const { context, elements } = await page();
  const requests = [];
  context.api = async (route) => {
    requests.push(route);
    throw Object.assign(new Error("master_ctl unavailable"), { status: 409 });
  };
  await vm.runInContext("restartService()", context);
  assert.deepEqual(requests, ["restart-service"]);
  assert.equal(elements.get("notice").textContent, "master_ctl unavailable");
  assert.equal(elements.get("restart-service").disabled, false);
});

test("command exit without a replacement cannot display restart success", async () => {
  const { context, elements } = await page();
  context.api = async (route) =>
    route === "restart-service"
      ? { restart_requested: true }
      : {
          runtime: {
            pid: 101,
            running: true,
            service_restart_pending: false,
            last_error: "master_ctl restart failed",
          },
        };
  await vm.runInContext("restartService()", context);
  assert.equal(elements.get("notice").textContent, "master_ctl restart failed");
  assert.equal(elements.get("restart-service").disabled, false);
});

for (const runtime of [
  { pid: 101, running: true, service_restart_pending: true },
  { pid: 102, running: false },
]) {
  test(`timeout cannot report success for PID ${runtime.pid}, running=${runtime.running}`, async () => {
    const { context, elements } = await page();
    let ticks = 0;
    context.Date = { now: () => (ticks += 31000) };
    context.api = async (route) =>
      route === "restart-service" ? { restart_requested: true } : { runtime };
    await vm.runInContext("restartService()", context);
    assert.match(elements.get("notice").textContent, /等待服务恢复超时/);
    assert.equal(elements.get("restart-service").disabled, false);
  });
}
