'use strict';
/**
 * 敏感字段（SecretField）渲染与取用流程检查。
 *
 * 为什么单独写一个：
 *   「遮蔽串」这件事曾经以**两张脸**同时出现在一个输入框里 —— 输入框用
 *   `type="password"` 渲染成圆点，`#prefix` 插槽里又画了一个 `Dh71**********N8`。
 *   同一个值被画两遍，看着像渲染坏了。这类问题 vue-tsc 查不出、控制台也不报错，
 *   只有真跑到那一页、真去读 DOM 才发现。
 *   所以这里不截图了事，而是**直接断言 DOM**：
 *     1. .sf-secret 里不许出现 type=password 的 input（禁止圆点）
 *     2. .sf-secret 里不许出现 .el-input__prefix（禁止遮蔽串塞进 prefix）
 *     3. 未显示明文且有值时：input 只读，且其值就是遮蔽串（含 * ）
 *     4. 空值不锁：没有可遮的东西时应当直接可编辑
 *     5. 点眼睛必弹确认；取消则原样不动；确认后转明文且可编辑；再点可回到遮蔽
 *
 * 用法：
 *   node scripts/ui-secret-check.js <cdpPort> --base http://127.0.0.1:7830 --out <png>
 *
 * 退出码：0 = 全部通过；1 = 有失败项（明细打印在前）。
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
const OUT = argOf('out', null);
const SESSION = process.env.SINYEFRP_SESSION;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let pass = 0;
const fails = [];
function check(name, ok, detail) {
  if (ok) {
    pass++;
    console.log('  PASS  ' + name);
  } else {
    fails.push(name + (detail ? ' :: ' + detail : ''));
    console.log('  FAIL  ' + name + (detail ? ' :: ' + detail : ''));
  }
}

function getJSON(url) {
  return new Promise((resolve, reject) => {
    http.get(url, (res) => {
      let d = '';
      res.on('data', (c) => (d += c));
      res.on('end', () => { try { resolve(JSON.parse(d)); } catch (e) { reject(e); } });
    }).on('error', reject);
  });
}

(async () => {
  const targets = await getJSON(`http://127.0.0.1:${cdpPort}/json/list`);
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
  await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1900, deviceScaleFactor: 1, mobile: false });
  await send('Page.navigate', { url: BASE + '/packages' });
  await sleep(2200);

  const evalJS = async (expression) => {
    const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    const res = r.result || {};
    if (res.exceptionDetails) return 'EXCEPTION: ' + JSON.stringify(res.exceptionDetails.text || res.exceptionDetails);
    return res.result ? res.result.value : undefined;
  };

  const me = await evalJS("fetch('/api/auth/me',{credentials:'same-origin'}).then(r=>r.status)");
  if (me !== 200) { console.error('会话未生效（/api/auth/me => ' + me + '），检查无意义'); process.exit(1); }

  // 切 tab 的辅助：Element Plus 的 pane 是「隐藏而非卸载」，必须只读当前可见的那个。
  const clickTab = async (label) => {
    const r = await evalJS(`(() => {
      const el = Array.from(document.querySelectorAll('.el-tabs__item')).find(e => e.innerText.trim() === ${JSON.stringify(label)});
      if (!el) return 'NOT_FOUND';
      el.click();
      return 'CLICKED';
    })()`);
    if (r === 'CLICKED') await sleep(800);
    return r;
  };

  // 敏感字段所在的「当前可见容器」。
  //
  // **优先弹窗**：客户端配置表单现在是弹窗（点「新增客户端」才挂载），
  // el-dialog 默认 teleport 到 body，**不在任何 .el-tab-pane 里**。
  // 只按可见 pane 找会一个字段都找不到，而且不会报任何错 ——
  // 表现为 count=0，看起来像「这个页面确实没有敏感字段」这种最危险的假绿。
  const VIS_ROOT = `(() => {
    const dlgs = Array.from(document.querySelectorAll('.el-dialog')).filter(d => d.offsetParent !== null);
    if (dlgs.length) return dlgs[dlgs.length - 1];
    const panes = Array.from(document.querySelectorAll('.el-tab-pane'));
    return panes.find(p => p.offsetParent !== null) || null;
  })()`;

  // 读当前可见容器里的敏感字段。
  const readFields = () => evalJS(`(() => {
    const root = ${VIS_ROOT};
    if (!root) return JSON.stringify({ error: 'NO_VISIBLE_PANE' });
    const fields = Array.from(root.querySelectorAll('.sf-secret'));
    return JSON.stringify({
      count: fields.length,
      items: fields.map(f => {
        const inp = f.querySelector('input');
        return {
          type: inp ? inp.type : null,
          readOnly: inp ? inp.readOnly : null,
          value: inp ? inp.value : null,
          placeholder: inp ? inp.placeholder : null,
          hasPrefix: !!f.querySelector('.el-input__prefix'),
          hasAppend: !!f.querySelector('.el-input-group__append'),
          // append 区里的按钮个数，以及相邻两个按钮的**水平净间距**。
          // 净间距 <= 0 意味着两个按钮贴死/重叠 —— 肉眼看着像一个按钮里挤了俩图标。
          // 这类纯布局缺陷不报错、也不影响任何行为断言，只能靠量几何拦下来。
          btnCount: (() => {
            const w = f.querySelector('.el-input-group__append');
            return w ? w.querySelectorAll('button').length : 0;
          })(),
          btnGap: (() => {
            const w = f.querySelector('.el-input-group__append');
            if (!w) return null;
            const bs = Array.from(w.querySelectorAll('button'));
            if (bs.length < 2) return null;
            return Math.round(bs[1].getBoundingClientRect().left - bs[0].getBoundingClientRect().right);
          })(),
        };
      }),
    });
  })()`);

  // 点第 idx 个敏感字段右侧的第 btn 个按钮（0 = 眼睛，1 = 复制）。
  const clickBtn = (idx, btn) => evalJS(`(() => {
    const root = ${VIS_ROOT};
    if (!root) return 'NO_VISIBLE_PANE';
    const f = Array.from(root.querySelectorAll('.sf-secret'))[${idx}];
    if (!f) return 'NO_FIELD';
    const wrap = f.querySelector('.el-input-group__append');
    if (!wrap) return 'NO_APPEND';
    const bs = Array.from(wrap.querySelectorAll('button'));
    if (!bs[${btn}]) return 'NO_BUTTON';
    bs[${btn}].click();
    return 'CLICKED';
  })()`);

  // ElMessageBox 关闭时有淡出过渡，DOM 里会短暂留着上一个。
  // 一律只看**当前可见**的框，并取最后一个，否则会点到残留框上的按钮、点了没反应。
  const msgBox = () => evalJS(`(() => {
    const boxes = Array.from(document.querySelectorAll('.el-message-box')).filter(b => b.offsetParent !== null);
    if (!boxes.length) return 'NONE';
    const box = boxes[boxes.length - 1];
    const btns = Array.from(box.querySelectorAll('.el-message-box__btns button')).map(b => b.innerText.trim());
    return JSON.stringify({ open: true, title: (box.querySelector('.el-message-box__title') || {}).innerText || '', btns });
  })()`);

  const clickMsgBox = (text) => evalJS(`(() => {
    const boxes = Array.from(document.querySelectorAll('.el-message-box')).filter(b => b.offsetParent !== null);
    if (!boxes.length) return 'NO_BOX';
    const box = boxes[boxes.length - 1];
    const b = Array.from(box.querySelectorAll('.el-message-box__btns button'))
      .find(x => x.innerText.trim() === ${JSON.stringify(text)});
    if (!b) return 'NO_BUTTON';
    b.click();
    return 'CLICKED';
  })()`);

  // 可见确认框个数。必须能查「有没有叠出两个」——点一次眼睛弹两个框，
  // 根因是兜底点击处理里的容器类名写错（EP 是 .el-input-group__append），
  // 按钮自己的处理完又冒泡到兜底、又走一次确认流程。
  const boxCount = () => evalJS(
    "Array.from(document.querySelectorAll('.el-message-box')).filter(b => b.offsetParent !== null).length"
  );

  // ---- 服务器部署包：token 与 Dashboard 密码都有默认值，是遮蔽态的主战场 ----
  console.log('');
  console.log('== 服务器部署包 tab ==');
  const tabSrv = await clickTab('服务器部署包');
  check('切到「服务器部署包」tab', tabSrv === 'CLICKED', tabSrv);
  if (tabSrv !== 'CLICKED') { console.log(''); console.log('未找到 tab，终止'); process.exit(1); }

  // 服务器表单自 2026-09-25 起与客户端同形，改成「点按钮 → 弹窗」（destroy-on-close），
  // tab 面板里只剩生成记录列表。不先打开弹窗，VIS_ROOT 会回退到可见 pane，
  // 读到「0 个敏感字段」—— 那是假绿，比直接报错危险得多。
  const srvOpened = await evalJS(`(() => {
    const btn = Array.from(document.querySelectorAll('button'))
      .find(b => (b.innerText || '').indexOf('新增服务器部署包') >= 0);
    if (!btn) return 'NOT_FOUND';
    btn.click();
    return 'CLICKED';
  })()`);
  check('点击「新增服务器部署包」打开配置弹窗', srvOpened === 'CLICKED', srvOpened);
  if (srvOpened === 'CLICKED') await sleep(1800);
  const srvDlg = await evalJS(
    "Array.from(document.querySelectorAll('.el-dialog')).filter(d => d.offsetParent !== null).length"
  );
  check('弹窗已渲染（服务器表单在其中）', srvDlg === 1, 'visible=' + srvDlg);

  const srvRaw = await readFields();
  const srv = JSON.parse(srvRaw);
  if (srv.error) { check('读到可见面板内的敏感字段', false, srv.error); process.exit(1); }
  check('该面板至少有 2 个敏感字段（token / Dashboard 密码）', srv.count >= 2, 'count=' + srv.count);
  console.log('    ' + JSON.stringify(srv.items));

  check('敏感字段内不存在 type=password 的输入框', srv.items.every((i) => i.type !== 'password'),
    JSON.stringify(srv.items.map((i) => i.type)));
  check('敏感字段内不存在 .el-input__prefix（遮蔽串不许塞进 prefix）', srv.items.every((i) => !i.hasPrefix));
  check('每个敏感字段都带显示/复制按钮', srv.items.every((i) => i.hasAppend),
    JSON.stringify(srv.items.map((i) => i.hasAppend)));
  // 回归防线：Element Plus 给 `.el-input-group__append .el-button` 设的是**负**外边距
  // （单个按钮时正好撑满 append）。放进两个按钮后负边距叠加，两个图标会贴死甚至重叠，
  // 看上去像一个按钮里挤了俩图标 —— 分不清是俩动作，也容易点错。
  // 修法是给 append 归一化 margin 并用 gap 拉开；这条断言保证它不会被改回去。
  check('每个敏感字段有 2 个按钮（显示明文 + 复制）',
    srv.items.every((i) => i.btnCount === 2),
    JSON.stringify(srv.items.map((i) => i.btnCount)));
  check('两个按钮未贴死/重叠（水平净间距 > 0）',
    srv.items.every((i) => i.btnGap === null || i.btnGap > 0),
    JSON.stringify(srv.items.map((i) => i.btnGap)));
  check('有值的敏感字段处于只读（要改先显示明文）',
    srv.items.filter((i) => i.value).every((i) => i.readOnly === true),
    JSON.stringify(srv.items.map((i) => i.readOnly)));
  const maskOk = srv.items.filter((i) => i.value).every((i) => /^.{0,4}\*{4,}.{0,2}$/.test(i.value));
  check('有值的敏感字段显示的是遮蔽串（含 *）', maskOk,
    JSON.stringify(srv.items.map((i) => i.value)));

  // ---- 显示明文：必须过确认这道闸 ----
  console.log('');
  console.log('== 显示明文的确认闸 ==');
  const idx = srv.items.findIndex((i) => i.value);
  const maskedValue = idx >= 0 ? srv.items[idx].value : null;
  check('存在一个可用于验证的带值字段', idx >= 0);

  if (idx >= 0) {
    const clicked = await clickBtn(idx, 0);
    check('点击眼睛按钮', clicked === 'CLICKED', clicked);
    await sleep(600);
    const boxes = await boxCount();
    check('点一次只弹一个确认框（不许叠）', boxes === 1, 'visible=' + boxes);
    const mb = await msgBox();
    let mbOpen = false;
    if (mb && mb !== 'NONE') {
      try { mbOpen = JSON.parse(mb).open === true; } catch { mbOpen = false; }
    }
    check('弹出确认框（不是直接显示明文）', mbOpen, mb);

    // 取消：必须一字不动
    const cancel = await clickMsgBox('取消');
    check('确认框有「取消」按钮并点击', cancel === 'CLICKED', cancel);
    await sleep(600);
    const afterCancelRaw = JSON.parse(await readFields());
    const afterCancel = afterCancelRaw.items[idx];
    check('取消后仍为遮蔽且只读',
      !!afterCancel && afterCancel.value === maskedValue && afterCancel.readOnly === true,
      JSON.stringify(afterCancel));

    // 确认：转明文且可编辑
    await clickBtn(idx, 0);
    await sleep(600);
    const confirm = await clickMsgBox('确认显示');
    check('确认框有「确认显示」按钮并点击', confirm === 'CLICKED', confirm);
    await sleep(700);
    const revealedRaw = JSON.parse(await readFields());
    const revealed = revealedRaw.items[idx];
    check('确认后转为明文（不再含遮蔽星号）',
      !!revealed && typeof revealed.value === 'string' && !/\*/.test(revealed.value),
      JSON.stringify(revealed && revealed.value));
    check('确认后可编辑（readOnly 已解除）', !!revealed && revealed.readOnly === false,
      JSON.stringify(revealed && revealed.readOnly));

    // 再点一次应回到遮蔽
    await clickBtn(idx, 0);
    await sleep(700);
    const backRaw = JSON.parse(await readFields());
    const back = backRaw.items[idx];
    check('再次点击可回到遮蔽态',
      !!back && back.value === maskedValue && back.readOnly === true,
      JSON.stringify(back));
  }

  // 关掉服务器弹窗再往下走。
  //
  // 必须关：下面客户端段的断言里有「可见弹窗恰好 1 个」，而服务器弹窗是
  // teleport 到 body 的，切 tab 不会带走它 —— 留着就会把那条断言顶成 2 而假失败。
  // VIS_ROOT 取的是「最后一个可见弹窗」，靠顺序兜也不稳，直接关掉最干净。
  const srvClosed = await evalJS(`(() => {
    const dlg = Array.from(document.querySelectorAll('.el-dialog')).filter(d => d.offsetParent !== null).pop();
    if (!dlg) return 'NO_DIALOG';
    const x = dlg.querySelector('.el-dialog__headerbtn');
    if (!x) return 'NO_CLOSE_BTN';
    x.click();
    return 'CLOSED';
  })()`);
  check('关闭服务器配置弹窗', srvClosed === 'CLOSED', srvClosed);
  await sleep(900);

  // ---- 客户端部署包：验「空值不锁」与「从空值录入直接转明文」 ----
  //
  // 不能假设这里一定是空值：接入点若已配好 token，客户端表单会把它带出来（遮蔽态）。
  // 所以先走一道确认把它显示出来、再清空，制造出「空值」这个起点。
  console.log('');
  console.log('== 客户端部署包 tab（空值分支）==');
  const tabCli = await clickTab('客户端部署包');
  check('切到「客户端部署包」tab', tabCli === 'CLICKED', tabCli);
  if (tabCli === 'CLICKED') {
    // 客户端表单在弹窗里（点「新增客户端」才挂载）。不打开弹窗就查不到任何字段，
    // 而且会因为 VIS_ROOT 回退到可见 pane 而读到「0 个敏感字段」这种假绿。
    const opened = await evalJS(`(() => {
      const btn = Array.from(document.querySelectorAll('button'))
        .find(b => (b.innerText || '').indexOf('新增客户端') >= 0);
      if (!btn) return 'NOT_FOUND';
      btn.click();
      return 'CLICKED';
    })()`);
    check('点击「新增客户端」打开配置弹窗', opened === 'CLICKED', opened);
    if (opened === 'CLICKED') await sleep(1500);
    const dlgVisible = await evalJS(
      "Array.from(document.querySelectorAll('.el-dialog')).filter(d => d.offsetParent !== null).length"
    );
    check('弹窗已渲染（客户端表单在其中）', dlgVisible === 1, 'visible=' + dlgVisible);

    const cli = JSON.parse(await readFields());
    check('该面板至少 1 个敏感字段', !cli.error && cli.count >= 1, JSON.stringify(cli));

    const ci = (cli.items || []).findIndex((i) => i.readOnly === true); // 带值遮蔽态的那个
    const cidx = ci >= 0 ? ci : 0;

    if (ci >= 0) {
      await clickBtn(cidx, 0);
      await sleep(600);
      const mb = await msgBox();
      check('客户端 token 的确认框已弹出', mb !== 'NONE' && mb.indexOf('确认显示敏感值') >= 0, mb);
      const ok = await clickMsgBox('确认显示');
      check('客户端 token 可显示明文（确认闸放行）', ok === 'CLICKED', ok);
      await sleep(600);
      const rev = JSON.parse(await readFields()).items[cidx];
      check('客户端 token 已转明文', !!rev && !/\*/.test(rev.value || ''), JSON.stringify(rev));
    }

    const setVal = (v) => evalJS(`(() => {
      const root = ${VIS_ROOT};
      const f = root && Array.from(root.querySelectorAll('.sf-secret'))[${cidx}];
      if (!f) return 'NO_FIELD';
      const inp = f.querySelector('input');
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      setter.call(inp, ${JSON.stringify(v)});
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      return 'SET';
    })()`);

    const cleared = await setVal('');
    check('清空该字段', cleared === 'SET', cleared);
    await sleep(500);
    const afterClear = JSON.parse(await readFields()).items[cidx];
    check('清空真的落在了模型上（输入框已空）', !!afterClear && afterClear.value === '',
      JSON.stringify(afterClear));

    // 隐藏回去：此时值为空，没有可遮的东西，应当直接可编辑。
    await clickBtn(cidx, 0);
    await sleep(600);
    let after = JSON.parse(await readFields()).items[cidx];
    check('空值字段不被锁（可直接编辑）', after.readOnly === false, JSON.stringify(after));
    check('空值字段不显示星号（没有可遮的东西）', !/\*/.test(after.value || ''),
      JSON.stringify(after));

    // 从空值开始录入：这是管理员自己的输入，不该再让他确认一次。
    const typ = await setVal('testval');
    check('录入测试值', typ === 'SET', typ);
    await sleep(500);
    after = JSON.parse(await readFields()).items[cidx];
    check('从空值录入时直接明文显示（不弹确认、不被遮蔽）',
      after.value === 'testval' && after.readOnly === false, JSON.stringify(after));

    // 收起：值已在，应回到遮蔽 + 只读。
    await clickBtn(cidx, 0);
    await sleep(600);
    after = JSON.parse(await readFields()).items[cidx];
    check('收起后回到遮蔽 + 只读', after.value === '*******' && after.readOnly === true,
      JSON.stringify(after));
    const leftover = await boxCount();
    check('流程结束时没有残留的确认框', leftover === 0, 'visible=' + leftover);
  }

  if (OUT) {
    // 截图前把可能残留的框关掉，否则截出来的是弹窗而不是页面。
    const left = await boxCount();
    if (left > 0) {
      await clickMsgBox('取消');
      await sleep(500);
    }
    // 客户端配置表单是弹窗，检查过程中一直开着；不关掉的话截图里
    // 是一块盖住页面的对话框，看不到要展示的遮蔽字段布局。
    await evalJS(`(() => {
      const dlg = Array.from(document.querySelectorAll('.el-dialog')).filter(d => d.offsetParent !== null).pop();
      if (!dlg) return 'NO_DIALOG';
      const x = dlg.querySelector('.el-dialog__headerbtn');
      if (!x) return 'NO_CLOSE';
      x.click();
      return 'CLICKED';
    })()`);
    await sleep(900);
    await clickTab('服务器部署包');
    const shot = await send('Page.captureScreenshot', { format: 'png' });
    if (shot.result && shot.result.data) {
      fs.writeFileSync(OUT, Buffer.from(shot.result.data, 'base64'));
      console.log('');
      console.log('截图 -> ' + OUT);
    }
  }

  console.log('');
  console.log('通过 ' + pass + ' 项，失败 ' + fails.length + ' 项');
  fails.forEach((f) => console.log('  !! ' + f));
  ws.close();
  process.exit(fails.length === 0 ? 0 : 1);
})().catch((e) => { console.error('FATAL: ' + (e && e.stack ? e.stack : e)); process.exit(1); });
