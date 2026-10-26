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
