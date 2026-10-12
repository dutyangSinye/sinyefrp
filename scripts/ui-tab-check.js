'use strict';
/**
 * 单点检查：切到「管理平台自身」tab 并把它真正渲染出来的内容抓下来。
 *
 * 为什么单独写一个：
 *   ui-walkthrough.js 只按路由截图，不点 tab。Element Plus 的 el-tab-pane 是
 *   「隐藏而非卸载」——隐藏面板里的运行时错误会连带整页崩掉（走查能发现），
 *   但**内容渲染错了不会报任何错**（例如 platform_version 取不到、渲染成空白）。
 *   这块只能切过去看真内容。
 *
 * 用法：node scripts/ui-tab-check.js <cdpPort> --base <url> --label <tab名> --out <png>
 */
const http = require('http');
const fs = require('fs');

const argv = process.argv.slice(2);
const cdpPort = Number(argv[0] && !argv[0].startsWith('--') ? argv[0] : 9333);
function argOf(name, fallback) {
  const i = argv.indexOf('--' + name);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}
const BASE = argOf('base', 'http://127.0.0.1:7830').replace(/\/$/, '');
const ROUTE = argOf('route', '/packages');
const LABEL = argOf('label', '管理平台自身');
const OUT = argOf('out', null);
const SESSION = process.env.SINYEFRP_SESSION;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function getJSON(url) {
  return new Promise((resolve, reject) => {
    http.get(url, (res) => {
      let d = '';
      res.on('data', (c) => (d += c));
      res.on('end', () => {
        try { resolve(JSON.parse(d)); } catch (e) { reject(e); }
      });
    }).on('error', reject);
  });
}

(async () => {
  const targets = await getJSON(`http://127.0.0.1:${cdpPort}/json/list`);
  // 必须挑我们自己的那个页面：无头 Edge 可能还有别的 page 目标。
  const page = targets.find((t) => t.type === 'page' && t.url.indexOf(BASE) === 0)
    || targets.find((t) => t.type === 'page');
  if (!page) throw new Error('没有找到 page 类型的调试目标');

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let seq = 0;
  const pending = new Map();
  ws.addEventListener('message', (ev) => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  });
  await new Promise((res, rej) => { ws.addEventListener('open', res); ws.addEventListener('error', rej); });
  const send = (method, params = {}) => new Promise((res) => { const id = ++seq; pending.set(id, res); ws.send(JSON.stringify({ id, method, params })); });

  await send('Page.enable');
  await send('Runtime.enable');
  await send('Network.enable');
  if (SESSION) {
    let v = SESSION;
    const eq = v.indexOf('=');
    if (eq >= 0) v = v.slice(eq + 1);
    await send('Network.setCookie', { name: 'sinyefrp_session', value: v.trim(), url: BASE + '/', path: '/', httpOnly: true });
  }

  await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1800, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: BASE + ROUTE });
  await sleep(2000);

  const evalJS = async (expression) => {
    const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    const res = r.result || {};
    if (res.exceptionDetails) return 'EXCEPTION: ' + JSON.stringify(res.exceptionDetails.text || res.exceptionDetails);
    return res.result ? res.result.value : undefined;
  };

  const me = await evalJS("fetch('/api/auth/me',{credentials:'same-origin'}).then(r=>r.status)");
  console.log('/api/auth/me => ' + me);
  if (me !== 200) { console.error('会话未生效，检查无意义'); process.exit(1); }

  const tabs = await evalJS("JSON.stringify(Array.from(document.querySelectorAll('.el-tabs__item')).map(e=>e.innerText.trim()))");
  console.log('页面上的 tab: ' + tabs);

  const clicked = await evalJS(`(() => {
    const el = Array.from(document.querySelectorAll('.el-tabs__item')).find(e => e.innerText.trim() === ${JSON.stringify(LABEL)});
    if (!el) return 'NOT_FOUND';
    el.click();
    return 'CLICKED';
  })()`);
  console.log('点击「' + LABEL + '」=> ' + clicked);
  if (clicked !== 'CLICKED') process.exit(1);

  await sleep(900);

  // 只读**当前可见**的那个面板：隐藏面板也留在 DOM 里，直接取全部会混进别的 tab 的内容。
  const paneText = await evalJS(`(() => {
    const panes = Array.from(document.querySelectorAll('.el-tab-pane'));
    const vis = panes.find(p => p.offsetParent !== null);
    return vis ? vis.innerText.replace(/\\n{2,}/g, '\\n').trim() : 'NO_VISIBLE_PANE';
  })()`);
  console.log('---- 可见面板内容 ----');
  console.log(paneText);
  console.log('---- 面板内容结束 ----');

  if (OUT) {
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result && shot.result.data) {
      fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64'));
      console.log('截图 -> ' + OUT);
    }
  }
  ws.close();
  process.exit(0);
})().catch((e) => { console.error('FATAL: ' + (e && e.stack ? e.stack : e)); process.exit(1); });
