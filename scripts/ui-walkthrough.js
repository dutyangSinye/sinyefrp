'use strict';
/**
 * 前端真实渲染走查：用 CDP 驱动无头 Edge 逐个访问受保护路由，截图并收集
 * 控制台告警 / 未捕获异常。
 *
 * 为什么需要它（而不是只跑 vue-tsc）：
 *   - vue-tsc 检查不到模板里 el-table 插槽的 `row`——它是 any，
 *     写成 row.status 还是 row.online 都编译通过，只有真跑一次才会露出
 *     「在线客户端被显示成离线」这类错。
 *   - Element Plus 的 el-tab-pane 默认把**所有**面板都渲染出来（隐藏而非卸载），
 *     所以一个隐藏面板里的 null.length 能把整个页面打崩。
 *   这两类缺陷都只在真实浏览器里才现形，静态检查一律放过。
 *
 * 用法：
 *   1) 先起一个带调试端口的无头 Edge（注意：必须与下面的 node 调用在同一条
 *      命令里，浏览器子进程才会在这次调用期间存活）：
 *
 *        msedge.exe --headless=new --disable-gpu \
 *          --remote-debugging-port=9333 \
 *          --user-data-dir=<临时目录> http://127.0.0.1:7810/ &
 *
 *   2) 再把会话 Cookie 注入进去跑本脚本（登录接口有 IP 限速，
 *      走查反复登录会拿到 429，所以直接注入 Cookie）：
 *
 *        SINYEFRP_SESSION=<token> node scripts/ui-walkthrough.js 9333 \
 *          --base http://127.0.0.1:7810 --out .workbuddy/shots
 *
 *      SINYEFRP_SESSION 收的是 token 本身；若直接粘贴 `Set-Cookie` 头里的
 *      `sinyefrp_session=<token>` 整串也可以，脚本会自动去掉前缀。
 *      注入错了不会报错、只会全程 401，因此脚本对「鉴权失效」有硬判据（见下）。
 *
 *    可选参数 --full-page：截整页而非仅视口。页面高于一屏时建议加上，
 *    否则视口下方的区块（如设置页的接入点）不会出现在截图里。
 *
 * 退出码：0 = 每个路由都真正停在目标路径且无告警/异常；
 *         1 = 有页面报错、或没停在目标路径（会打印在对应小节下）。
 *     「被踢回 /gate」一律算失败 —— 否则会把登录页截图 N 遍却报全绿。
 */

const http = require('http');
const fs = require('fs');
const path = require('path');

// ---------- 参数 ----------
const argv = process.argv.slice(2);
const cdpPort = Number(argv[0] && !argv[0].startsWith('--') ? argv[0] : 9333);

function argOf(name, fallback) {
  const i = argv.indexOf('--' + name);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}

const BASE = argOf('base', 'http://127.0.0.1:7810').replace(/\/$/, '');
const OUT = argOf('out', path.join('.workbuddy', 'shots'));
// --full-page：截整页而不是仅视口。
// 页面长到超过一屏时，视口截图会把下半部分（例如设置页的「客户端接入点」区块）
// 整块裁掉，看截图的人会以为该区块没渲染出来。
const FULL = argv.includes('--full-page');
const COOKIE_NAME = 'sinyefrp_session';

// SINYEFRP_SESSION 收的是**会话 Cookie 的值**（token 本身）。
// 但直接从 curl 的 `Set-Cookie` 头复制粘贴时，拿到的是 `name=value` 整串——
// 早先把整串塞进 Network.setCookie 的 value，浏览器照收不误，只是此后每个
// 请求都 401、页面被路由守卫踢回 /gate，而脚本当时并不检查这一点，
// 于是「六页全绿」其实是把登录页截了六遍。这里两种写法都接受，去掉前缀。
let SESSION = process.env.SINYEFRP_SESSION;
if (!SESSION) {
  console.error('缺少 SINYEFRP_SESSION 环境变量（会话 Cookie 的值）');
  process.exit(2);
}
if (SESSION.indexOf('=') >= 0) {
  const eq = SESSION.indexOf('=');
  const name = SESSION.slice(0, eq).trim();
  if (name !== COOKIE_NAME) {
    console.error('SINYEFRP_SESSION 传的是 Cookie 对，但名字是 "' + name + '"，期望 "' + COOKIE_NAME + '"');
    process.exit(2);
  }
  SESSION = SESSION.slice(eq + 1).trim();
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function getJSON(url) {
  return new Promise((resolve, reject) => {
    http
      .get(url, (res) => {
        let d = '';
        res.on('data', (c) => (d += c));
        res.on('end', () => {
          try {
            resolve(JSON.parse(d));
          } catch (e) {
            reject(e);
          }
        });
      })
      .on('error', reject);
  });
}

// 把 CDP 给的堆栈拼成一行的可读形式。
//
// 为什么要专门做：走查的价值就在于「编译通过、一跑就崩」这类运行时缺陷，
// 而这类缺陷的原始信息往往只有一句 TypeError。没有堆栈时，
// 定位只能靠猜哪个组件用了 .id/.url —— 太慢，而且容易猜错方向。
// 只保留打包产物里的函数名与坐标，最多 3 帧：够定位，又不至于把日志刷满。
function fmtStack(st, fallbackUrl, fallbackLine, fallbackCol) {
  const frames = (st && st.callFrames) || [];
  if (!frames.length) {
    if (!fallbackUrl) return '';
    return '  @ ' + fallbackUrl + ':' + fallbackLine + ':' + fallbackCol;
  }
  return '  @ ' + frames.slice(0, 3)
    .map((f) => (f.functionName || '<anon>') + '(' + String(f.url).split('/').pop() + ':' + f.lineNumber + ')')
    .join(' <- ');
}

const ROUTES = [
  ['/overview', '01-总览'],
  ['/clients', '02-客户端'],
  ['/proxies', '03-代理'],
  ['/packages', '04-部署包'],
  ['/records', '05-生成记录'],
  ['/registry', '06-客户端台账'],
  ['/settings', '07-设置'],
];

(async () => {
  fs.mkdirSync(OUT, { recursive: true });

  const targets = await getJSON(`http://127.0.0.1:${cdpPort}/json/list`);
  const page = targets.find((t) => t.type === 'page');
  if (!page) throw new Error('没有找到 page 类型的调试目标');

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let seq = 0;
  const pending = new Map();
  let bucket = [];
  const report = [];

  ws.addEventListener('message', (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
      return;
    }
    if (msg.method === 'Runtime.exceptionThrown') {
      const d = msg.params.exceptionDetails;
      // 堆栈必须一起留下：只报「Cannot read properties of undefined (reading 'id')」
      // 时，光看这句话无法判断是哪个组件/哪一行 —— 实践里会退化成靠猜。
      bucket.push('EXCEPTION: ' + (d.exception && d.exception.description ? d.exception.description : d.text)
        + fmtStack(d.stackTrace, d.url, d.lineNumber, d.columnNumber));
    }
    if (msg.method === 'Runtime.consoleAPICalled') {
      const t = msg.params.type;
      if (t === 'error' || t === 'warning') {
        const txt = msg.params.args
          .map((a) => (a.value !== undefined ? a.value : a.description || a.type))
          .join(' ');
        bucket.push(t.toUpperCase() + ': ' + txt + fmtStack(msg.params.stackTrace));
      }
    }
  });

  await new Promise((res, rej) => {
    ws.addEventListener('open', res);
    ws.addEventListener('error', rej);
  });

  const send = (method, params = {}) =>
    new Promise((res) => {
      const id = ++seq;
      pending.set(id, res);
      ws.send(JSON.stringify({ id, method, params }));
    });

  await send('Page.enable');
  await send('Runtime.enable');
  await send('Network.enable');

  const setCookie = await send('Network.setCookie', {
    name: COOKIE_NAME,
    value: SESSION,
    url: BASE + '/',
    path: '/',
    httpOnly: true,
  });
  console.log('注入会话 Cookie =>', JSON.stringify(setCookie.result));

  const evalJS = async (expression) => {
    const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    const res = r.result || {};
    if (res.exceptionDetails) return undefined;
    return res.result ? res.result.value : undefined;
  };

  const waitFor = async (expression, timeout = 10000) => {
    const t0 = Date.now();
    while (Date.now() - t0 < timeout) {
      if (await evalJS(expression)) return true;
      await sleep(200);
    }
    return false;
  };

  let bad = 0;

  // 全页模式：把这个 SPA 的视口调高，而不是用 captureBeyondViewport。
  // 原因：布局是「侧栏 + 内层滚动容器」，body 高度恒等于视口高度，
  // captureBeyondViewport 抓不到任何额外内容。把视口本身调高，
  // 内层容器随视口变高，整页内容才会真正渲染进这一张图。
  if (FULL) {
    await send('Emulation.setDeviceMetricsOverride', {
      width: 1440,
      height: 2600,
      deviceScaleFactor: 1,
      mobile: false,
    });
    console.log('全页模式：视口已调高到 1440x2600');
  }

  for (const [route, label] of ROUTES) {
    bucket = [];
    await send('Page.navigate', { url: BASE + route });
    await sleep(1500);
    await waitFor("document.querySelector('#app') && document.querySelector('#app').innerText.trim().length > 10");

    const me = await evalJS("fetch('/api/auth/me',{credentials:'same-origin'}).then(r=>r.status)");
    const info = await evalJS(
      "JSON.stringify({title:document.title, path:location.pathname, len:document.body.innerText.length, head:document.body.innerText.slice(0,220).replace(/\\n/g,' | ')})"
    );
    const parsed = info ? JSON.parse(info) : {};
    // 全页效果由上方的视口覆盖实现，这里保持标准截图调用。
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result && shot.result.data) {
      fs.writeFileSync(path.join(OUT, label + '.png'), Buffer.from(shot.result.data, 'base64'));
    }

    const problems = bucket.slice(0, 8);
    // 鉴权失败必须让整轮判为失败。
    // 只统计控制台告警/异常是不够的：会话失效时路由守卫把页面踢回 /gate，
    // 登录页本身渲染得好好的、无告警无异常，于是六个路由「全绿」，
    // 实际只把登录页截了六遍 —— 目标页面一个都没验过。
    // 这种假绿比直接报错更危险，因此这里把「没停在目标路由」也算作问题。
    if (me !== 200) {
      problems.push(
        'AUTH: /api/auth/me => ' + me + '（会话未生效；实际停在 ' + (parsed.path || '?') + '，本轮并未渲染目标路由）'
      );
    } else if (parsed.path && parsed.path !== route) {
      problems.push('REDIRECT: 期望停在 ' + route + '，实际停在 ' + parsed.path);
    }

    report.push({ route, authMe: me, info, problems });
    console.log('\n=== ' + label + ' (' + route + ') ===');
    console.log('  /api/auth/me => ' + me);
    console.log('  ' + info);
    if (problems.length) {
      bad++;
      problems.forEach((p) => console.log('  !! ' + p));
    } else {
      console.log('  (无控制台告警/异常)');
    }
  }

  fs.writeFileSync(path.join(OUT, 'report.json'), JSON.stringify(report, null, 2), 'utf8');
  console.log('\nDONE -> ' + OUT + '   有问题的页面数: ' + bad);
  ws.close();
  process.exit(bad > 0 ? 1 : 0);
})().catch((e) => {
  console.error('FATAL: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
