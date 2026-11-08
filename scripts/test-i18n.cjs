const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");
const root = path.join(__dirname, "../web/admin");
const source = fs.readFileSync(path.join(root, "i18n.js"), "utf8");

function runtime(saved, browserLanguage = "zh-CN", blocked = false) {
  const storage = new Map(saved ? [["sgate.admin.language", saved]] : []);
  const elements = [];
  const context = vm.createContext({
    navigator: { language: browserLanguage },
    localStorage: {
      getItem(key) { if (blocked) throw new Error("storage denied"); return storage.get(key); },
      setItem(key, value) { if (blocked) throw new Error("storage denied"); storage.set(key, value); },
    },
    document: {
      documentElement: { lang: "" },
      querySelectorAll: (selector) => selector === "*" ? elements : [],
    },
  });
  vm.runInContext(source, context);
  return { context, storage, elements, run: (code) => vm.runInContext(code, context) };
}

test("browser preference is remembered, with safe fallbacks for invalid or blocked storage", () => {
  for (const [saved, browser, expected] of [[null, "en-US", "en"], ["zh-CN", "en-GB", "zh-CN"], ["en", "zh-CN", "en"], ["invalid", "zh-CN", "zh-CN"]]) {
    const page = runtime(saved, browser);
    assert.equal(page.run("sgateI18n.language()"), expected);
    page.run('sgateI18n.setLanguage("en")');
    assert.equal(runtime(page.storage.get("sgate.admin.language")).run("sgateI18n.language()"), "en");
    page.run('sgateI18n.setLanguage("invalid")');
    assert.equal(page.run("sgateI18n.language()"), "en");
  }
  const page = runtime(null, "en-US", true);
  page.run('sgateI18n.setLanguage("en")');
  assert.equal(page.run('sgateI18n.t("欢迎回来")'), "Welcome back");
});

test("a translated label retains its nested input and updates without resetting the value", () => {
  const page = runtime();
  const label = {
    set textContent(value) { this.firstChild = { parentNode: this, nodeValue: value }; },
  };
  const input = { value: "unsaved-private-value" };
  page.context.label = label;
  page.elements.push(label);
  page.run('sgateI18n.localText(label, () => sgateI18n.t("用户名"))');
  label.input = input;
  const node = label.firstChild;
  for (const language of ["en", "zh-CN", "en"]) {
    page.run(`sgateI18n.setLanguage(${JSON.stringify(language)})`);
    assert.equal(label.firstChild, node);
    assert.equal(label.input, input);
    assert.equal(input.value, "unsaved-private-value");
  }
  assert.equal(label.firstChild.nodeValue, "Username");
});

test("all static labels and explicit app messages have English translations", () => {
  const page = runtime("en");
  const html = fs.readFileSync(path.join(root, "index.html"), "utf8");
  const app = fs.readFileSync(path.join(root, "app.js"), "utf8");
  const sources = [...html.matchAll(/data-i18n(?:-aria-label|-placeholder)?="([^"]+)"/g)].map(m => m[1]);
  sources.push(...[...app.matchAll(/\bt\(("(?:\\.|[^"\\])*")\)/g)].map(m => JSON.parse(m[1])));
  for (const text of sources) {
    const translated = page.run(`sgateI18n.t(${JSON.stringify(text)})`);
    assert.doesNotMatch(translated, /[\p{Script=Han}]/u, text);
  }
  assert.equal(page.run('sgateI18n.t("custom comment / 自定义注释")'), "custom comment / 自定义注释");
  assert.equal(page.run('sgateI18n.t("用户名或密码不正确")'), "Incorrect username or password");
});

test("every client/server template and supported option has help keyed by option in both languages", () => {
  const repo = path.join(root, "../..");
  const keys = new Set();
  const templates = ["sgate-client.cf", "sgate-server.cf",
    ...fs.readdirSync(path.join(repo, "samples")).filter(name => name.endsWith(".cf")).map(name => "samples/" + name)];
  for (const name of templates) {
    for (const match of fs.readFileSync(path.join(repo, name), "utf8").matchAll(/^\s*([\w-]+)\s*=/gm)) keys.add(match[1]);
  }
  const vpn = fs.readFileSync(path.join(repo, "internal/vpn/settings.go"), "utf8");
  for (const match of vpn.matchAll(/(?:Setting)?\{"(vpn-[^"]+)"/g)) keys.add(match[1]);
  const rules = fs.readFileSync(path.join(repo, "internal/admin/config_rules.go"), "utf8").split("var configRules")[0];
  for (const match of rules.matchAll(/\badd\("([\w -]+)"/g)) {
    for (const key of match[1].split(" ")) keys.add(key);
  }
  for (const role of ["client", "server"]) {
    for (const language of ["zh-CN", "en"]) {
      const page = runtime(language);
      for (const key of keys) {
        const field = {key, description: "English deployment comment / 中文自定义注释"};
        const help = page.run(`sgateI18n.configDescription(${JSON.stringify(field)}, ${JSON.stringify(role)})`);
        assert.doesNotMatch(help, /deployment comment|自定义注释/, key);
        assert.notEqual(help, page.run(`sgateI18n.t("配置项：") + ${JSON.stringify(key)}`), "missing help: " + key);
        if (language === "en") assert.doesNotMatch(help, /\p{Script=Han}/u, role + ": " + key);
        else assert.match(help, /\p{Script=Han}/u, role + ": " + key);
        assert.equal(field.description, "English deployment comment / 中文自定义注释");
      }
    }
  }
});

test("all server validation hints, including composed duration/noise hints, translate to English", () => {
  const page = runtime("en");
  const rules = fs.readFileSync(path.join(root, "../../internal/admin/config_rules.go"), "utf8").split("var configRules")[0];
  const strings = [...rules.matchAll(/"(?:\\.|[^"\\])*"/g)].map(match => JSON.parse(match[0]));
  const hints = strings.filter(text => /\p{Script=Han}/u.test(text));
  for (const hint of hints) {
    assert.doesNotMatch(page.run(`sgateI18n.t(${JSON.stringify(hint)})`), /\p{Script=Han}/u, hint);
  }
  const unit = "单位必须填写：ns、us/µs、ms、s、m、h，可组合如 1m30s；0 的含义依参数而定。";
  const zero = "与 max 成对设置；均为 0 关闭随机发送。";
  const quic = "QUIC 中控制随机目标速率的重选周期（至少 1s），不再表示整批发送后的停顿；全部为 0 时关闭噪音。";
  const combined = `${unit} ${zero} ${quic}`;
  assert.doesNotMatch(page.run(`sgateI18n.t(${JSON.stringify(combined)})`), /\p{Script=Han}/u);
});

test("role-specific help and unknown-option fallback follow language without exposing file comments", () => {
  const page = runtime("zh-CN");
  for (const language of ["zh-CN", "en", "zh-CN"]) {
    page.run(`sgateI18n.setLanguage(${JSON.stringify(language)})`);
    const server = page.run('sgateI18n.configDescription({key:"admin_tls_cert"}, "server")');
    const client = page.run('sgateI18n.configDescription({key:"admin_tls_cert"}, "client")');
    assert.notEqual(server, client);
    assert.match(server, /tls-cert\/tls-key/);
    for (const key of ["custom-setting", "constructor", "__proto__"]) {
      const unknown = page.run(`sgateI18n.configDescription({key:${JSON.stringify(key)},description:"<script>untranslated 中文</script>"})`);
      assert.equal(unknown, page.run(`sgateI18n.t("配置项：") + ${JSON.stringify(key)}`));
    }
  }
});


test("actual backend field metadata uses the selected language, including composed hints", {
  skip: !process.env.SGATE_I18N_FIELDS_REPORT,
}, () => {
  const reports = JSON.parse(fs.readFileSync(process.env.SGATE_I18N_FIELDS_REPORT, "utf8"));
  for (const language of ["en", "zh-CN"]) {
    const page = runtime(language);
    for (const report of reports) {
      for (const field of report.fields) {
        const help = page.run(`sgateI18n.configDescription(${JSON.stringify(field)}, ${JSON.stringify(report.role)})`);
        const hint = page.run(`sgateI18n.t(${JSON.stringify(field.validation?.hint || "")})`);
        if (language === "en") assert.doesNotMatch(help + hint, /\p{Script=Han}/u, report.name + ": " + field.key);
        else assert.match(help, /\p{Script=Han}/u, report.name + ": " + field.key);
      }
    }
  }
});
