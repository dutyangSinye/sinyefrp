'use strict';
/**
 * 机器人类型分组的渲染断言（01-prd F20）。
 *
 * 为什么必须单独写一个：
 *   分组完全是前端算出来的视图 —— 排序、合并单元格、筛选计数。vue-tsc 只能保证
 *   类型没错，走查截图只能看出「崩没崩」。而分组算错的表现全都极其温和：
 *   合并块错位到别的行、筛选后计数全变成 0、未分类被排到列表开头、
 *   写错列下标导致整列内容消失……**一个异常都不会抛**，页面照样好看。
 *   只有读 DOM 才能把它们逼出来。
 *
 * 数据由脚本自己造：走查实例的台账是空的，没有数据时上述断言会全部假绿
 * （「找不到分组」与「分组是对的」在文本上区分不开）。所以先在浏览器内
 * fetch 生成三个客户端包：两个同类型（spray）+ 一个另一类型（rebar），
 * 这样既能看到两个分组块，也能验证「同类型合并成一个格子」（rowspan = 2）。
 *
 * 覆盖边界（诚实记录）：
 *   /clients 的数据来自 FRP 实时快照，走查实例没有真实 frpc 在线，所以那一页
 *   只断言「列与筛选条渲染正确」；分组逻辑本身在 /registry 上验证。
 *   /clients 的类型数据链路由 registry-test.ps1 在真实 frps + frpc 环境下断言。
 *
 * 用法：
 *   SINYEFRP_SESSION=<token> node scripts/ui-robot-group-check.js <cdpPort> \
 *     --base <url> [--out <png>]
 *   结果同时写到 <png>.result.json（UTF-8 直写，避免 PowerShell 捕获时乱码）。
 */
const http = require('http');
const fs = require('fs');
const path = require('path');

const argv = process.argv.slice(2);
const cdpPort = Number(argv[0] && !argv[0].startsWith('--') ? argv[0] : 9333);
function argOf(name, fallback) {
  const i = argv.indexOf('--' + name);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
}
const BASE = argOf('base', 'http://127.0.0.1:7830').replace(/\/$/, '');
const OUT = argOf('out', null);
const SESSION = process.env.SINYEFRP_SESSION;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const checks = [];
function check(name, ok, detail) {
  checks.push({ name, ok: !!ok, detail: detail === undefined ? '' : String(detail) });
}

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

function writeResult(exitCode) {
  const bad = checks.filter((c) => !c.ok);
  const payload = {
    passed: bad.length === 0 && exitCode === 0,
    total: checks.length,
    failed: bad.length,
    checks,
  };
  const text = JSON.stringify(payload, null, 2);
  if (OUT) {
    try { fs.writeFileSync(OUT + '.result.json', text, 'utf8'); } catch (e) { /* 落盘失败不影响退出码 */ }
  }
  console.log('---- 断言 ----');
  for (const c of checks) {
    console.log((c.ok ? '  [PASS] ' : '  [FAIL] ') + c.name + (c.detail ? '  (' + c.detail + ')' : ''));
  }
  console.log('---- 合计 ' + checks.length + ' 项，失败 ' + bad.length + ' 项 ----');
}

(async () => {
  const targets = await getJSON(`http://127.0.0.1:${cdpPort}/json/list`);
  const page = targets.find((t) => t.type === 'page' && t.url.indexOf(BASE) === 0)
    || targets.find((t) => t.type === 'page');
  if (!page) throw new Error('没有找到 page 类型的调试目标');

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let seq = 0;
  const pending = new Map();
  const problems = [];
  // 记下每条问题发生时的**墙钟时间与所在路由**。
  //
  // 为什么必须记：这条断言失败过若干次，但报出来的只有一句
  // "Cannot read properties of undefined (reading 'id')" —— 既没有在哪一行、
  // 也没说当时页面在哪一页。压缩后的生产产物里，光看消息文本无法定位，
  // 只能反复重跑碰运气。把 url:line:col 与当时路由一起带出来，
  // 下一次出现就能直接落到具体代码位置。
  //
  // 时间戳刻意用本地时间字符串：日志里与其它套件的时序比对才有意义
  // （异常常在「刚导航完、旧页面的异步回调还在飞」的窗口里出现）。
  const stamp = () => new Date().toISOString().slice(11, 23);
  ws.addEventListener('message', (ev) => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); return; }

    // 未捕获异常在 V8 里常常给不出 callFrames（浏览器把它算作「文档本身的错误」），
    // 于是日志里只剩一句 "reading 'id'"，定位无从下手。Debugger.setPauseOnExceptions
    // 能让 V8 在**抛出点**暂停，此时 Debugger.paused 带的才是真实调用栈。
    if (m.method === 'Debugger.paused') {
      const p = m.params;
      if (p.reason === 'exception' || p.reason === 'promiseRejection') {
        const frames = (p.callFrames || []).slice(0, 12).map((f) => {
          const u = (f.url || '').split('/').pop() || '?';
          return (f.functionName || '(anon)') + '@' + u + ':' + (f.location.lineNumber + 1) + ':' + (f.location.columnNumber + 1);
        }).join(' << ');
        problems.push('[' + stamp() + '] PAUSED(' + p.reason + '): ' + frames);
      }
      // 不登记进 pending：暂停必须无条件恢复，等响应反而会卡住页面。
      ws.send(JSON.stringify({ id: ++seq, method: 'Debugger.resume' }));
      return;
    }

    if (m.method === 'Runtime.exceptionThrown') {
      const d = m.params.exceptionDetails || {};
      // 顶层 url:line:col
      let at = '';
      if (d.url) at = ' @' + d.url + ':' + ((d.lineNumber || 0) + 1) + ':' + ((d.columnNumber || 0) + 1);
      // 调用栈最上面几帧 —— scriptId 也能反查 assets 里的具体 chunk
      const st = d.stackTrace;
      let frames = '';
      if (st && st.callFrames && st.callFrames.length) {
        frames = ' << ' + st.callFrames.slice(0, 6).map((f) => {
          const fn = f.functionName || '(anon)';
          const u = (f.url || '').split('/').pop() || '?';
          return fn + '@' + u + ':' + ((f.lineNumber || 0) + 1) + ':' + ((f.columnNumber || 0) + 1);
        }).join(' << ');
      }
      problems.push('[' + stamp() + '] exception: ' + (d.text || '') + at + frames
        + '  desc=' + ((d.exception && d.exception.description) || '').slice(0, 500));
    }
    if (m.method === 'Runtime.consoleAPICalled' && (m.params.type === 'error' || m.params.type === 'warning')) {
      const txt = (m.params.args || []).map((a) => (a.value !== undefined ? a.value : a.description || '')).join(' ');
      const p = m.params;
      let at = '';
      if (p.stackTrace && p.stackTrace.callFrames && p.stackTrace.callFrames.length) {
        const f = p.stackTrace.callFrames[0];
        at = ' @' + ((f.url || '').split('/').pop() || '?') + ':' + ((f.lineNumber || 0) + 1);
      }
      problems.push('[' + stamp() + '] ' + m.params.type + ':' + at + ' ' + String(txt).slice(0, 300));
    }
  });
  await new Promise((res, rej) => { ws.addEventListener('open', res); ws.addEventListener('error', rej); });
  const send = (method, params = {}) => new Promise((res) => { const id = ++seq; pending.set(id, res); ws.send(JSON.stringify({ id, method, params })); });

  await send('Page.enable');
  await send('Runtime.enable');
  // 抓真实调用栈用（临时诊断；稳定后可撤）
  await send('Debugger.enable');
  await send('Debugger.setPauseOnExceptions', { state: 'uncaught' });

  if (SESSION) {
    let v = SESSION;
    const eq = v.indexOf('=');
    if (eq >= 0) v = v.slice(eq + 1);
    await send('Network.setCookie', { name: 'sinyefrp_session', value: v.trim(), url: BASE + '/', path: '/', httpOnly: true });
  }
  await send('Emulation.setDeviceMetricsOverride', { width: 1600, height: 1800, deviceScaleFactor: 1, mobile: false });

  const evalJS = async (expression) => {
    const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    const res = r.result || {};
    if (res.exceptionDetails) return 'EXCEPTION: ' + JSON.stringify(res.exceptionDetails.text || res.exceptionDetails);
    return res.result ? res.result.value : undefined;
  };

  await send('Page.navigate', { url: BASE + '/registry' });
  await sleep(2200);

  // 会话必须先确认：注入错了会被踢回 /gate，那里零异常零告警，
  // 所有「找不到分组」的断言都会变成假的失败原因，最难排查。
  const me = await evalJS("fetch('/api/auth/me',{credentials:'same-origin'}).then(r=>r.status)");
  check('会话已生效（/api/auth/me = 200）', me === 200, 'me=' + me);
  if (me !== 200) {
    writeResult(1);
    ws.close();
    process.exit(1);
  }

  // ---- 1. 造数据：两个同类型 + 一个另一类型 ----
  //
  // server_addr 兜一个本地地址：走查实例**没有配置接入点**，此时 defaults 返回的
  // server_addr 是空串，而 spec 校验要求它非空（"服务器地址不能为空"）——
  // 不兜的话三次生成全是 400，后面所有分组断言都会因为「没有数据」而失败，
  // 看起来像分组功能坏了。造数据只为了让台账里有带类型的记录，
  // 并不验证 defaults 的行为（那条由 registry-test.ps1 在配好接入点的实例上断言）。
  //
  // 失败时把响应体一并带回来：只报「codes=400」会让人去猜是哪一条校验没过。
  const seed = await evalJS(`(async () => {
    const d = await (await fetch('/api/pkg/client/defaults', {credentials:'same-origin'})).json();
    const rnd = String(Math.floor(Math.random()*9000)+1000);
    const addr = d.server_addr || '127.0.0.1';
    const basePort = 20000 + Math.floor(Math.random()*2000);
    const mk = (suffix, port, type) => ({
      server_addr: addr,
      server_port: d.server_port,
      token: d.token,
      name: '分组走查-' + suffix + rnd,
      client_id: 'sf-uigrp-' + suffix + rnd,
      robot_type: type,
      log_level: 'info',
      log_max_days: 7,
      enable_tls: false,
      proxies: [{ name: 'uigrp-' + suffix + rnd, type: 'tcp', local_ip: '127.0.0.1', local_port: 9200, remote_port: port }]
    });
    const items = [mk('a', basePort, 'spray'), mk('b', basePort + 1, 'spray'), mk('c', basePort + 2, 'rebar')];
    const codes = [];
    const errs = [];
    for (const it of items) {
      const r = await fetch('/api/pkg/client', {method:'POST', credentials:'same-origin', headers:{'Content-Type':'application/json'}, body: JSON.stringify(it)});
      const body = await r.text();
      codes.push(r.status);
      if (r.status !== 200) errs.push(r.status + ' ' + body.slice(0, 300));
    }
    return JSON.stringify({ codes: codes.join(','), errs: errs });
  })()`);

  let seedInfo = {};
  try { seedInfo = JSON.parse(String(seed)); } catch (e) { seedInfo = { codes: String(seed), errs: [] }; }
  const seedDetail = 'codes=' + seedInfo.codes
    + (seedInfo.errs && seedInfo.errs.length ? '  原因: ' + seedInfo.errs.join(' | ') : '');
  check('造出三个带类型的客户端包（HTTP 全 200）', seedInfo.codes === '200,200,200', seedDetail);
  if (seedInfo.codes !== '200,200,200') {
    // 没有数据时后面的「找不到分组」全是同一个根因，继续断言只会把真正的
    // 失败原因埋在一堆连锁失败里。
    writeResult(1);
    ws.close();
    process.exit(1);
  }

  // ---- 2. 台账页：分组渲染 ----
  await send('Page.navigate', { url: BASE + '/registry' });
  await sleep(2200);

  const headers = await evalJS("JSON.stringify(Array.from(document.querySelectorAll('.el-table th')).map(e=>e.innerText.trim()))");
  check('台账页表头含「机器人类型」', String(headers).indexOf('机器人类型') >= 0, String(headers).slice(0, 200));

  // 本地端口列必须出现在**两张**端口表上（台账端口占用 + FRP 在线端口占用）。
  // 只数出现次数：关联对不对由 registry-test 的接口断言负责，这里守的是
  // 「后端加了字段、界面却忘了画」以及「只给一张表补、漏了另一张」这类漏改 ——
  // 它们都不报错，只是少一列，走查截图里看不出来。
  const localPortCols = (String(headers).match(/本地端口/g) || []).length;
  check('台账页两张端口表都有「本地端口」列', localPortCols === 2, 'count=' + localPortCols);

  // 第一张表就是台账表（后面两张是端口表）。
  const rows = await evalJS(`(() => {
    const t = document.querySelector('.el-table');
    if (!t) return 'NO_TABLE';
    return JSON.stringify(Array.from(t.querySelectorAll('.el-table__body tr')).map(function(tr){
      return tr.innerText.replace(/\\s+/g, ' ').trim();
    }));
  })()`);
  const rowText = String(rows);
  check('台账页渲染出台账行', rows !== 'NO_TABLE' && rowText.indexOf('sf-uigrp') >= 0, rowText.slice(0, 300));
  check('出现「喷涂机器人」分组并标出台数', /喷涂机器人\s*2\s*台/.test(rowText), rowText.slice(0, 300));
  check('出现「钢筋加工机器人」分组', /钢筋加工机器人/.test(rowText), rowText.slice(0, 300));
  // 单条的分组不标台数：「1 台」是噪音，台数只在真正发生合并的块上才有信息量。
  check('单条类型不标台数（只在合并块上标）', !/钢筋加工机器人\s*\d+\s*台/.test(rowText), rowText.slice(0, 300));

  // 注意 Element Plus 会给**每个** td 都输出 rowspan 属性（默认 1），
  // 被合并掉的单元格则是整格消失。所以判据是「值 >= 2 的恰好有一个，且等于 2」，
  // 而不是「数组里只该有一个元素」。
  const rowspans = await evalJS(`(() => {
    const t = document.querySelector('.el-table');
    if (!t) return 'NO_TABLE';
    return JSON.stringify(Array.from(t.querySelectorAll('td[rowspan]')).map(function(td){ return Number(td.getAttribute('rowspan')); }));
  })()`);
  let spanList = [];
  try { spanList = JSON.parse(String(rowspans)); } catch (e) { spanList = []; }
  const merged = spanList.filter((n) => n >= 2);
  check('同类型的行合并成一个格子（恰好一个跨 2 行的块）', merged.length === 1 && merged[0] === 2, 'merged=' + JSON.stringify(merged));

  const groups = await evalJS("JSON.stringify(Array.from(document.querySelectorAll('.el-radio-group')).map(e=>e.innerText.replace(/\\s+/g,' ').trim()))");
  check('筛选条按类型显示数量（喷涂机器人 2）', /喷涂机器人 2/.test(String(groups)), String(groups).slice(0, 300));
  check('筛选条按类型显示数量（钢筋加工机器人 1）', /钢筋加工机器人 1/.test(String(groups)), String(groups).slice(0, 300));

  // 交互：点「钢筋加工机器人」后，台账表应当只剩 1 行 —— 顺带证明筛选真的作用于
  // 分组之后（合并块跟着缩小，而不是把行滤掉却留下跨 2 行的空格子）。
  const clicked = await evalJS(`(() => {
    const btns = Array.from(document.querySelectorAll('.el-radio-button'));
    const b = btns.find(function(e){ return /钢筋加工机器人/.test(e.innerText); });
    if (!b) return 'NOT_FOUND';
    const input = b.querySelector('input');
    if (input) { input.click(); return 'CLICKED'; }
    b.click();
    return 'CLICKED';
  })()`);
  check('能点中「钢筋加工机器人」筛选按钮', clicked === 'CLICKED', clicked);
  await sleep(600);

  const afterRows = await evalJS(`(() => {
    const t = document.querySelector('.el-table');
    if (!t) return -1;
    return t.querySelectorAll('.el-table__body tr').length;
  })()`);
  check('筛选后台账表只剩该类记录（1 行）', Number(afterRows) === 1, 'rows=' + afterRows);

  const afterSpans = await evalJS(`(() => {
    const t = document.querySelector('.el-table');
    if (!t) return 'NO_TABLE';
    return JSON.stringify(Array.from(t.querySelectorAll('td[rowspan]')).map(function(td){ return Number(td.getAttribute('rowspan')); }));
  })()`);
  let afterSpanList = [];
  try { afterSpanList = JSON.parse(String(afterSpans)); } catch (e) { afterSpanList = []; }
  check('筛选后只剩单条记录，不做无意义的合并', !afterSpanList.some((n) => n >= 2), 'rowspans=' + afterSpans);

  // ---- 3. 客户端页：列与筛选条 ----
  await send('Page.navigate', { url: BASE + '/clients' });
  await sleep(2200);

  const cHeaders = await evalJS("JSON.stringify(Array.from(document.querySelectorAll('.el-table th')).map(e=>e.innerText.trim()))");
  check('客户端页表头含「机器人类型」', String(cHeaders).indexOf('机器人类型') >= 0, String(cHeaders).slice(0, 240));
  check('客户端页表头含「显示名称」', String(cHeaders).indexOf('显示名称') >= 0, String(cHeaders).slice(0, 240));
  const cGroups = await evalJS("JSON.stringify(Array.from(document.querySelectorAll('.el-radio-group')).map(e=>e.innerText.replace(/\\s+/g,' ').trim()))");
  check('客户端页有机器人类型筛选条', /全部/.test(String(cGroups)), String(cGroups).slice(0, 240));

  // ---- 4. 控制台必须干净 ----
  check('全程无控制台异常与告警', problems.length === 0, problems.slice(0, 5).join(' | '));

  if (OUT) {
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result && shot.result.data) {
      try {
        fs.mkdirSync(path.dirname(OUT), { recursive: true });
        fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64'));
      } catch (e) { /* 截图失败不影响断言结论 */ }
    }
  }

  // 断言失败时把问题清单**单独落一份全文**（不受 5 条截断限制）。
  // checks 里只带前 5 条是为了报告可读；真要排错需要完整栈，
  // 而 PowerShell 侧捕获 node 的 stdout 会重排格式，所以直写文件最稳。
  if (problems.length && OUT) {
    try {
      fs.writeFileSync(OUT + '.problems.txt',
        '当时路由与阶段见每条前缀的 UTC 时间；下面是完整记录\n\n' + problems.join('\n'), 'utf8');
    } catch (e) { /* ignore */ }
  }

  writeResult(0);
  ws.close();
  const failed = checks.filter((c) => !c.ok).length;
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => {
  check('脚本执行完成', false, e && e.stack ? e.stack : String(e));
  writeResult(1);
  process.exit(1);
});
