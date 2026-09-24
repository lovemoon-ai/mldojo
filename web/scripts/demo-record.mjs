// Records an end-to-end demo video of the MLDojo web app against a live API.
// Headless Chrome + CDP screencast -> frames -> ffmpeg -> mp4 (the machine's
// real X display is never touched).
//
//   npm i --no-save puppeteer-core
//   BASE=http://127.0.0.1:8765 TOKEN=$(mldojo-api token issue) RUN_ID=<uuid> \
//   OUT=/tmp/mldojo-demo.mp4 node scripts/demo-record.mjs
import { spawn } from "node:child_process";
import fs from "node:fs";
import puppeteer from "puppeteer-core";

const BASE = process.env.BASE || "http://127.0.0.1:8765";
const TOKEN = process.env.TOKEN;
const OUT = process.env.OUT || "/tmp/mldojo-demo.mp4";
const CHROME = process.env.CHROME || "/usr/bin/google-chrome";
const FPS = 10;
if (!TOKEN) throw new Error("TOKEN is required");

const api = async (p) => {
  const r = await fetch(`${BASE}/api/v1${p}`, { headers: { Authorization: `Bearer ${TOKEN}` } });
  if (!r.ok) throw new Error(`${p}: ${r.status}`);
  return r.json();
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  defaultViewport: { width: 1440, height: 900 },
  args: ["--no-sandbox", "--disable-gpu", "--force-device-scale-factor=1", "--hide-scrollbars"],
});
const page = await browser.newPage();
await page.goto(`${BASE}/login`, { waitUntil: "networkidle0" });
await page.evaluate((t) => localStorage.setItem("mldojo.token", t), TOKEN);

// ---- caption overlay (survives React re-renders; re-injected per navigation)
const CAPTION = `(text) => {
  let el = document.getElementById('__cap');
  if (!el) {
    el = document.createElement('div');
    el.id = '__cap';
    el.style.cssText = 'position:fixed;left:0;right:0;bottom:0;z-index:2147483647;padding:10px 18px;' +
      'background:rgba(15,23,42,.92);color:#fff;font:600 17px/1.4 ui-sans-serif,system-ui,sans-serif;' +
      'letter-spacing:.2px;pointer-events:none;border-top:2px solid #2563eb';
    document.body.appendChild(el);
  }
  el.textContent = text;
}`;
const caption = async (text) => page.evaluate(eval(`(${CAPTION})`), text);
const go = async (path, text) => {
  await page.goto(`${BASE}${path}`, { waitUntil: "networkidle2", timeout: 30000 }).catch(() => {});
  if (text) await caption(text);
};
// click an element by its visible text (tabs, buttons, links)
const clickText = async (text) => {
  const ok = await page.evaluate((t) => {
    const els = [...document.querySelectorAll('button,a,[role="tab"]')];
    const el = els.find((e) => e.textContent.trim() === t || e.textContent.trim().startsWith(t));
    if (!el) return false;
    el.click();
    return true;
  }, text);
  await sleep(700);
  return ok;
};

// ---- screencast -> ffmpeg
fs.rmSync("/tmp/mldojo-frames", { recursive: true, force: true });
const ff = spawn("ffmpeg", ["-y", "-f", "image2pipe", "-framerate", String(FPS), "-i", "-",
  "-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2", "-c:v", "libx264", "-preset", "veryfast", "-crf", "24",
  "-pix_fmt", "yuv420p", "-movflags", "+faststart", OUT], { stdio: ["pipe", "ignore", "pipe"] });
let ffErr = "";
ff.stderr.on("data", (d) => (ffErr += d.toString()));

const client = await page.createCDPSession();
let frames = 0;
let last = null;
// Chrome only emits frames when something changes, so keep the newest frame
// and let a fixed-rate writer feed ffmpeg: the video then plays in real time.
client.on("Page.screencastFrame", async ({ data, sessionId }) => {
  last = Buffer.from(data, "base64");
  try { await client.send("Page.screencastFrameAck", { sessionId }); } catch {}
});
const pad = setInterval(() => { if (last && ff.stdin.writable) { ff.stdin.write(last); frames++; } }, 1000 / FPS);
await client.send("Page.startScreencast", { format: "jpeg", quality: 80, everyNthFrame: 1 });

// ---- the demo -------------------------------------------------------------
const runId = process.env.RUN_ID;
try {
  await go("/", "MLDojo · 总览：最近的 run 和节点状态");
  await sleep(3500);

  await go("/nodes", "节点：agent 反向连接回 API，GPU 利用率每 5 秒心跳上报");
  await sleep(4500);

  await go("/projects", "Project → Experiment → Run 三层结构");
  await sleep(3000);
  if (await clickText("smoke")) await caption("项目 smoke 下的实验列表");
  await sleep(3000);

  if (runId) {
    await go(`/run?id=${runId}&tab=logs`, "刚通过 CLI 提交到节点的 run：日志经 WebSocket 实时推送");
    await sleep(14000);
    await caption("训练还在跑，日志持续增长（stdout / stderr / system 三路）");
    await sleep(8000);

    if (await clickText("Metrics")) await caption("指标：agent 扫描 jsonl / tensorboard，曲线实时刷新");
    else await go(`/run?id=${runId}&tab=metrics`, "指标：曲线实时刷新");
    await sleep(12000);

    if (await clickText("Code")) await caption("代码溯源：提交时记录的 commit 和未提交改动的 patch");
    else await go(`/run?id=${runId}&tab=code`, "代码溯源");
    await sleep(6000);

    if (await clickText("Artifacts")) await caption("产物：留在节点上，只记 URI，预览时按需拉取");
    else await go(`/run?id=${runId}&tab=artifacts`, "产物");
    await sleep(5000);

    if (await clickText("Events")) await caption("事件流：run 状态机的完整审计");
    else await go(`/run?id=${runId}&tab=events`, "事件流");
    await sleep(4000);
  }

  // Compare two finished runs of the same experiment.
  const runs = (await api("/runs?limit=50")).filter((r) => r.status === "succeeded");
  const a = runs.find((r) => r.name?.startsWith("seed="));
  const b = runs.find((r) => r.id !== a?.id && r.experiment === a?.experiment);
  if (a && b) {
    await go(`/compare?a=${a.id}&b=${b.id}`, "对比两个 run：代码、指标、环境差异");
    await sleep(7000);
  }

  await go("/datasets", "数据集登记：节点路径 + bucket，首次使用自动同步到节点");
  await sleep(3500);
  await go("/settings?tab=secrets", "Secret：只显示名字，值用 age 加密存储，永不回显");
  await sleep(3500);
  await go("/settings", "设置：API token 与健康状态");
  await sleep(3500);

  // Phone view.
  await page.setViewport({ width: 420, height: 880, isMobile: true, hasTouch: true, deviceScaleFactor: 1 });
  await go(runId ? `/run?id=${runId}&tab=logs` : "/runs", "手机视图（PWA，可装到桌面）");
  await sleep(5000);
  await go("/nodes", "手机上的节点页：底部导航，响应式布局");
  await sleep(5000);
} finally {
  clearInterval(pad);
  try { await client.send("Page.stopScreencast"); } catch {}
  await sleep(300);
  ff.stdin.end();
  await new Promise((r) => ff.on("close", r));
  await browser.close();
  const sec = (frames / FPS).toFixed(1);
  if (!fs.existsSync(OUT) || fs.statSync(OUT).size < 10000) {
    console.error(ffErr.slice(-2000));
    throw new Error("recording failed");
  }
  console.log(`recorded ${frames} frames (~${sec}s) -> ${OUT} (${(fs.statSync(OUT).size / 1e6).toFixed(1)} MB)`);
}
