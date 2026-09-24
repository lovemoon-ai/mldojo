// UI smoke test against a live MLDojo API (not part of the build).
// Needs puppeteer-core and a local Chrome:
//   npm i --no-save puppeteer-core
//   BASE=http://127.0.0.1:8765 TOKEN=$(mldojo-api token issue) CHROME=/usr/bin/google-chrome \
//   OUT=/tmp/ui-shots node scripts/ui-smoke.mjs
// Visits every page (desktop + one mobile view), fails on page errors,
// console errors or missing key content, and writes screenshots to OUT.
import fs from "node:fs";
import puppeteer from "puppeteer-core";

const BASE = process.env.BASE || "http://127.0.0.1:8765";
const TOKEN = process.env.TOKEN;
const OUT = process.env.OUT || "/tmp/ui-shots";
const CHROME = process.env.CHROME || "/usr/bin/google-chrome";
if (!TOKEN) throw new Error("TOKEN is required");
fs.mkdirSync(OUT, { recursive: true });

const api = async (p) => {
  const r = await fetch(`${BASE}/api/v1${p}`, { headers: { Authorization: `Bearer ${TOKEN}` } });
  if (!r.ok) throw new Error(`${p}: ${r.status}`);
  return r.json();
};
const runs = await api("/runs?limit=50");
const ok = runs.filter((r) => r.status === "succeeded");
const run = ok[0];
const other = ok.find((r) => r.id !== run.id && r.experiment === run.experiment) || ok.find((r) => r.id !== run.id);
const nodes = await api("/nodes");
if (!run) throw new Error("need at least one succeeded run");

const pages = [
  { path: "/", expect: [run.id.slice(0, 8)] },
  { path: "/projects", expect: [run.project] },
  { path: `/project?name=${run.project}`, expect: [run.experiment] },
  { path: `/experiment?project=${run.project}&name=${run.experiment}`, expect: [run.name] },
  { path: "/runs", expect: [run.id.slice(0, 8)] },
  { path: `/run?id=${run.id}&tab=logs`, expect: ["done"], wait: 2500, name: "run-logs" },
  { path: `/run?id=${run.id}&tab=metrics`, expect: ["loss"], selector: "svg path.recharts-curve", name: "run-metrics" },
  { path: `/run?id=${run.id}&tab=code`, expect: [], name: "run-code" },
  { path: `/run?id=${run.id}&tab=artifacts`, expect: ["ckpt"], name: "run-artifacts" },
  { path: `/run?id=${run.id}&tab=events`, expect: ["succeeded"], name: "run-events" },
  { path: `/run?id=${run.id}&tab=overview`, expect: [run.target], name: "run-overview" },
  { path: "/nodes", expect: nodes.map((n) => n.id) },
  { path: `/node?id=${nodes[0].id}`, expect: [nodes[0].id] },
  { path: "/queues", expect: [] },
  { path: "/datasets", expect: [] },
  { path: "/settings?tab=secrets", expect: ["Stored secrets"], name: "settings-secrets" },
  { path: "/settings", expect: [] },
];
if (other) pages.push({ path: `/compare?a=${run.id}&b=${other.id}`, expect: ["loss"], name: "compare" });

const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox", "--disable-gpu"] });
const results = [];
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 900 });
  const errors = [];
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && errors.push(`console: ${m.text()}`));
  await page.goto(`${BASE}/login`, { waitUntil: "networkidle0" });
  await page.evaluate((t) => localStorage.setItem("mldojo.token", t), TOKEN);
  for (const p of pages) {
    errors.length = 0;
    await page.goto(`${BASE}${p.path}`, { waitUntil: "networkidle2", timeout: 30000 });
    if (p.selector) await page.waitForSelector(p.selector, { timeout: 10000 }).catch(() => errors.push(`missing ${p.selector}`));
    await new Promise((r) => setTimeout(r, p.wait || 800));
    const text = await page.evaluate(() => document.body.innerText);
    const missing = p.expect.filter((e) => !text.includes(e));
    const url = page.url();
    if (url.includes("/login") && !p.path.includes("/login")) errors.push("redirected to login");
    const name = p.name || (p.path.replace(/[/?=&]+/g, "_").replace(/^_|_$/g, "") || "home");
    await page.screenshot({ path: `${OUT}/${name}.png` });
    results.push({ page: p.path, ok: missing.length === 0 && errors.length === 0, missing, errors: [...errors] });
  }
  // Mobile view of the run page.
  await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
  await page.goto(`${BASE}/run?id=${run.id}&tab=logs`, { waitUntil: "networkidle2" });
  await new Promise((r) => setTimeout(r, 1500));
  await page.screenshot({ path: `${OUT}/mobile-run.png` });
  const hasBottomNav = await page.evaluate(() => [...document.querySelectorAll("nav")].some((n) => n.getBoundingClientRect().bottom >= window.innerHeight - 2));
  results.push({ page: "mobile /run", ok: hasBottomNav, missing: hasBottomNav ? [] : ["bottom nav"], errors: [] });
  // PWA: service worker registered in production builds.
  const sw = await page.evaluate(async () => !!(navigator.serviceWorker && (await navigator.serviceWorker.getRegistration())));
  results.push({ page: "service worker", ok: sw, missing: sw ? [] : ["registration"], errors: [] });
} finally {
  await browser.close();
}
let failed = 0;
for (const r of results) {
  if (!r.ok) failed++;
  console.log(`${r.ok ? "✓" : "✗"} ${r.page}${r.missing.length ? " missing=" + JSON.stringify(r.missing) : ""}${r.errors.length ? " errors=" + JSON.stringify(r.errors) : ""}`);
}
console.log(`ui-smoke: ${results.length - failed} passed, ${failed} failed (screenshots in ${OUT})`);
process.exit(failed ? 1 : 0);
