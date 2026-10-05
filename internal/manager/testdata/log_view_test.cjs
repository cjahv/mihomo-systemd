const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync('web/index.html', 'utf8');
const source = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const escape = text => text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
class Element {
  constructor(tag = 'div') { this.tag = tag; this.children = []; this.style = {}; this.dataset = {}; this.listeners = {}; this.attributes = {}; this.hidden = false; this.scrollTop = 0; this.value = ''; this.checked = false; this.disabled = false; this.classList = { add() {}, remove() {} }; }
  addEventListener(name, callback) { (this.listeners[name] ||= []).push(callback); }
  click() { if (this.disabled) return Promise.resolve(); return Promise.all((this.listeners.click || []).map(callback => callback())); }
  contains(node) { return node === this || this.children.some(child => child.contains(node)); }
  dispatch(name, event = {}) { for (const callback of this.listeners[name] || []) callback(event); }
  setAttribute(name, value) { this.attributes[name] = value; }
  append(...nodes) { for (const node of nodes) { if (node.tag === 'fragment') { this.append(...node.children.slice()); node.children = []; } else { node.remove(); this.children.push(node); node.parent = this; } } }
  appendChild(node) { this.append(node); return node; }
  prepend(...nodes) { const fragment = new Element(); fragment.append(...nodes); const incoming = fragment.children.slice(); incoming.forEach(node => node.parent = this); this.children.unshift(...incoming); }
  replaceChildren(...nodes) { this.children.forEach(node => node.parent = null); this.children = []; this.raw = ''; this.append(...nodes); }
  remove() { if (this.parent) { this.parent.children = this.parent.children.filter(node => node !== this); this.parent = null; } }
  get firstElementChild() { return this.children[0]; }
  get lastElementChild() { return this.children.at(-1); }
  get isConnected() { return !!this.parent; }
  get offsetTop() { return this.parent ? this.parent.children.indexOf(this) * 40 : 0; }
  set textContent(text) { this.replaceChildren(); this.raw = String(text); }
  get textContent() { return (this.raw || '') + this.children.map(node => node.textContent).join(''); }
  set innerHTML(text) { this.replaceChildren(); this.rawHTML = text; }
  get innerHTML() { return this.rawHTML || escape(this.raw || '') + this.children.map(node => node.tag === '#text' ? escape(node.textContent) : `<${node.tag}>${node.innerHTML}</${node.tag}>`).join(''); }
}
const settle = () => new Promise(resolve => setImmediate(resolve));
async function harness(initialTask = {}, overrides = {}) {
  const elements = Object.fromEntries([...html.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()]));
  elements.updateOverlay.hidden = true;
  elements.logsView.append(elements.updateBtn, elements.updateOverlay);
  let ready;
  const streams = [];
  const requests = [];
  let task = initialTask;
  const response = value => ({ ok: true, json: async () => value });
  const fetch = async (url, options = {}) => {
    requests.push(url);
    if (overrides[url]) return overrides[url](options);
    if (url === '/check_secret') return response({ success: true });
    if (url === '/update_status' || url === '/reload') return response({ ...task });
    if (url === '/get_settings') return response({ success: true, data: {} });
    assert.equal(url, '/logs');
    let pending;
    const queued = [];
    const stream = { signal: options.signal, push(value) { const chunk = { value: Buffer.from(value), done: false }; if (pending) { const old = pending; pending = null; old.resolve(chunk); } else queued.push(chunk); }, end() { if (pending) { pending.resolve({ done: true }); pending = null; } }, read() { if (options.signal.aborted) return Promise.reject(new DOMException('Aborted', 'AbortError')); if (queued.length) return Promise.resolve(queued.shift()); return new Promise((resolve, reject) => pending = { resolve, reject }); }, releaseLock() {} };
    options.signal.addEventListener('abort', () => { pending?.reject(new DOMException('Aborted', 'AbortError')); pending = null; });
    streams.push(stream);
    return { ok: true, body: { getReader: () => stream } };
  };
  const windowEvents = {};
  const documentEvents = {};
  let selection = null;
  const window = { location: { hostname: 'localhost' }, getSelection: () => selection, addEventListener(name, callback) { windowEvents[name] = callback; } };
  let clockTime = Date.now();
  const timers = new Map();
  class ClockDate extends Date { static now() { return clockTime; } }
  const setTimer = (callback, duration) => {
    if (duration <= 1000) return setTimeout(callback, duration);
    const id = {}; timers.set(id, { callback, expires: clockTime + duration }); return id;
  };
  const clearTimer = id => { if (!timers.delete(id)) clearTimeout(id); };
  const advance = duration => {
    clockTime += duration;
    for (const [id, timer] of [...timers]) if (timer.expires <= clockTime) { timers.delete(id); timer.callback(); }
  };
  const context = vm.createContext({ document: { getElementById: id => elements[id], createElement: tag => new Element(tag), createDocumentFragment: () => new Element('fragment'), createTextNode: text => { const node = new Element('#text'); node.textContent = text; return node; }, addEventListener(name, cb) { if (name === 'DOMContentLoaded') ready = cb; else documentEvents[name] = cb; } }, window, location: { reload() {} }, localStorage: { getItem: () => 'example', setItem() {}, removeItem() {} }, fetch, TextDecoder, AbortController, DOMException, console, Date: ClockDate, setTimeout: setTimer, clearTimeout: clearTimer, Node: { TEXT_NODE: 3 }, NodeFilter: { SHOW_TEXT: 4 } });
  vm.runInContext(source, context);
  await ready(); await settle();
  return { elements, streams, requests, context, advance, windowEvents, documentEvents, setSelection(value) { selection = value; }, notices: vm.runInContext('notifications', context), setTask(value) { task = value; } };
}
const messageOf = row => row.children[2].textContent;
const notice = (elements, id) => elements.notifications.children.find(card => card.dataset.notice === id);
const line = message => JSON.stringify({ type: 'log', message, time: '2026-10-05T04:00:00Z' }) + '\n';
const visible = element => !element.hidden && element.style.display !== 'none' && (!element.parent || visible(element.parent));

// These fixtures model selected text offsets; native Range/layout/clipboard
// behavior is also verified in a real browser, rather than inferred from this DOM.
function logSelectionFixture(h) {
 const rows = [
  ['00:40:46.868', 'WARN', '中文错误\n  保留缩进 <tag> 😀'],
  ['00:40:46.867', 'INFO', 'Start initial provider apple'],
 ];
 const nodes = rows.flatMap(values => {
  const row = {};
  return values.map(data => {
   const field = { closest: selector => selector === '.log-line' ? row : field };
   return { nodeType: 3, data, length: data.length, parentElement: field };
  });
 });
 const container = h.elements.logOutput;
 const outside = {};
 container.contains = node => node === container || nodes.includes(node);
 h.context.document.createTreeWalker = root => {
  const texts = root.nodeType === 3 ? [] : nodes;
  let index = 0;
  return { nextNode: () => texts[index++] || null };
 };
 const range = (first, start, last, end) => ({
  startContainer: nodes[first], startOffset: start, endContainer: nodes[last], endOffset: end,
  commonAncestorContainer: first === last ? nodes[first] : container,
  intersectsNode: node => nodes.indexOf(node) >= first && nodes.indexOf(node) <= last,
 });
 const select = (...ranges) => h.setSelection({ isCollapsed: false, rangeCount: ranges.length, getRangeAt: index => ranges[index] });
 const copy = (overrides = {}) => {
  const formats = {};
  const event = { defaultPrevented: false, clipboardData: { setData: (type, text) => formats[type] = text }, preventDefault() { this.defaultPrevented = true; }, ...overrides };
  h.documentEvents.copy(event);
  return { formats, prevented: event.defaultPrevented };
 };
 return { nodes, container, outside, range, select, copy };
}

test('log copy separates fields and records while retaining selected offsets and message whitespace', async () => {
 const h = await harness();
 const f = logSelectionFixture(h);
 f.select(f.range(0, 0, 5, f.nodes[5].length));
 assert.deepEqual(f.copy(), { formats: { 'text/plain': '00:40:46.868 WARN 中文错误\n  保留缩进 <tag> 😀\n00:40:46.867 INFO Start initial provider apple' }, prevented: true });
 f.select(f.range(1, 2, 4, 2));
 assert.equal(f.copy().formats['text/plain'], 'RN 中文错误\n  保留缩进 <tag> 😀\n00:40:46.867 IN');
 f.select(f.range(2, 5, 2, 11));
 assert.equal(f.copy().formats['text/plain'], '  保留缩进');
 f.select(f.range(2, f.nodes[2].length, 3, f.nodes[3].length));
 assert.equal(f.copy().formats['text/plain'], '00:40:46.867');
 f.select(f.range(0, 0, 0, 2), f.range(5, 0, 5, 5));
 assert.equal(f.copy().formats['text/plain'], '00\nStart');
});

test('log copy leaves unrelated, collapsed, empty and unavailable clipboard selections to the browser', async () => {
 const h = await harness();
 const f = logSelectionFixture(h);
 assert.equal(f.copy().prevented, false);
 h.setSelection({ isCollapsed: true, rangeCount: 0 });
 assert.equal(f.copy().prevented, false);
 f.select(f.range(0, 0, 0, 0));
 assert.equal(f.copy().prevented, false);
 f.select({ ...f.range(0, 0, 2, 3), startContainer: f.outside });
 assert.equal(f.copy().prevented, false);
 f.select(f.range(0, 0, 2, 3), { ...f.range(3, 0, 5, 3), endContainer: f.outside });
 assert.equal(f.copy().prevented, false);
 f.select(f.range(0, 0, 2, 3));
 assert.equal(f.copy({ clipboardData: null }).prevented, false);
 assert.deepEqual(f.copy({ defaultPrevented: true }).formats, {});
});

test('Mihomo event ordering, chunk boundaries, independent overlay, connection lifecycle', async () => {
 const h = await harness(); const e = h.elements;
 await e.logsBtn.click(); await settle();
 await e.logsBtn.click(); assert.equal(h.streams.length, 1);
 const bytes = Buffer.from(line('旧日志') + line('新日志\n第一行\n第二行'));
 for (let i = 0; i < bytes.length; i += 3) h.streams[0].push(bytes.subarray(i, i + 3));
 await settle();
 assert.deepEqual(e.logOutput.children.map(messageOf), ['新日志\n第一行\n第二行', '旧日志']);
 e.logOutput.scrollTop = 120;
 h.streams[0].push(line('实时新日志')); await settle();
 assert.equal(messageOf(e.logOutput.firstElementChild), '实时新日志');
 assert.equal(e.logOutput.scrollTop, 160);
 h.setTask({ id: 'task-1', running: true, stage: 'preparing', message: '正在下载订阅' });
 const updating = e.updateBtn.click(); await settle();
 assert.equal(h.streams.length, 1);
 assert.equal(e.updateSummary.textContent, '准备新订阅');
 assert.equal(e.updateOverlay.hidden, false);
 assert.equal(visible(e.updateOverlay), true);
 assert.equal(notice(e, 'update-result'), undefined);
 assert.equal(e.logOutput.children.length, 3);
 h.streams[0].push(line('更新期间的核心日志')); await settle();
 assert.equal(messageOf(e.logOutput.firstElementChild), '更新期间的核心日志');
 await e.updateToggle.click(); assert.equal(e.updateHistory.hidden, true);
 await e.settingsBtn.click(); await settle();
 assert.equal(h.streams[0].signal.aborted, true);
 assert.equal(e.updateOverlay.hidden, false);
 assert.equal(visible(e.updateOverlay), false);
 assert.equal(visible(e.updateBtn), false);
 h.setTask({ id: 'task-1', running: false, stage: 'finished', result: 'rolled_back', message: '恢复完成' });
 await updating;
 assert.equal(e.updateSummary.textContent, '更新失败，回滚成功');
 assert.equal(e.updateOverlay.hidden, true);
 assert.match(notice(e, 'update-result').textContent, /更新失败，回滚成功.*恢复完成/);
 h.advance(8000);
 assert.equal(notice(e, 'update-result'), undefined);
 assert.equal(e.settingsView.style.display, 'block');
 await e.logsBtn.click(); await settle(); assert.equal(h.streams.length, 2);
 h.streams[0].push(line('过期连接')); h.streams[1].push(line('重新连接')); await settle();
 assert.deepEqual(e.logOutput.children.map(messageOf), ['重新连接']);
 await e.logoutBtn.click(); await settle(); assert.equal(h.streams[1].signal.aborted, true);
 assert.equal(e.updateOverlay.hidden, true);
});

test('retention, diagnostics, safe message text and end-of-stream retry', async () => {
 const h = await harness(); const e = h.elements;
 await e.logsBtn.click(); await settle();
 h.streams[0].push(Array.from({ length: 1005 }, (_, index) => line(`event-${index}`)).join('')); await settle();
 assert.equal(e.logOutput.children.length, 1000);
 assert.equal(messageOf(e.logOutput.firstElementChild), 'event-1004');
 assert.equal(messageOf(e.logOutput.lastElementChild), 'event-5');
 h.streams[0].push(JSON.stringify({ type: 'error', message: 'journal 权限不足' }) + '\n'); await settle();
 assert.equal(e.logOutput.children.length, 1000);
 assert.equal(e.logStatus.textContent, '日志源异常');
 assert.match(notice(e, 'logs').textContent, /权限不足/);
 h.streams[0].push(line('<script>bad</script>')); await settle();
 assert.equal(messageOf(e.logOutput.firstElementChild), '<script>bad</script>');
 assert.equal(e.logOutput.firstElementChild.children[2].innerHTML, '&lt;script&gt;bad&lt;/script&gt;');
 h.streams[0].end(); await settle();
 await e.logsBtn.click(); await settle(); assert.equal(h.streams.length, 2);
 await e.manageBtn.click(); await settle();
});


test('restored task stays secondary and logout cancels polling without canceling the task', async () => {
 const h = await harness({ id: 'restored', running: true, stage: 'probing', message: '检查 Google' });
 const e = h.elements;
 assert.equal(e.uiContainer.style.display, 'block');
 assert.equal(e.updateOverlay.hidden, false);
 assert.equal(visible(e.updateOverlay), false);
 assert.equal(e.updateSummary.textContent, '检测 Google');
 await e.logsBtn.click(); await settle();
 assert.equal(visible(e.updateOverlay), true);
 const statusCount = h.requests.filter(url => url === '/update_status').length;
 await e.logoutBtn.click(); await settle();
 await new Promise(resolve => setTimeout(resolve, 1100));
 assert.equal(h.requests.filter(url => url === '/update_status').length, statusCount);
 assert.equal(h.requests.includes('/reload'), false);
 assert.equal(h.streams[0].signal.aborted, true);
});

test('message budget trims oldest records and an individual multiline record remains whole', async () => {
 const h = await harness(); const e = h.elements;
 await e.logsBtn.click(); await settle();
 h.streams[0].push(Array.from({ length: 4 }, (_, i) => line(String(i) + 'x'.repeat(700000))).join('')); await settle();
 assert.equal(e.logOutput.children.length, 2);
 assert.equal(messageOf(e.logOutput.firstElementChild).startsWith('3'), true);
 assert.equal(messageOf(e.logOutput.lastElementChild).startsWith('2'), true);
 await e.manageBtn.click(); await settle();
});

test('failed task displays multiline command diagnostics and full URL without altering core logs', async () => {
 const detail = '准备候选配置失败: 更新脚本（update.sh --prepare）失败: exit status 1\n命令输出：\n[下载] https://example.invalid/sub?token=example\ncurl: (6) Could not resolve host: example.invalid\n[ERROR] 订阅下载失败';
 const h = await harness();
 h.setTask({id:'failure',running:false,stage:'finished',result:'rejected',message:detail});
 const e = h.elements;
 await e.logsBtn.click(); await settle();
 h.streams[0].push(line('Mihomo 仍在运行')); await settle();
 await e.updateBtn.click(); await settle();
 assert.equal(e.updateHistory.firstElementChild.textContent.includes(detail),true);
 assert.equal(e.updateSummary.textContent,'更新被拒绝，当前配置保持不变');
 assert.equal(e.updateOverlay.hidden,true);
 assert.equal(notice(e, 'update-result').children[1].children[1].textContent,detail);
 assert.deepEqual(e.logOutput.children.map(messageOf),['Mihomo 仍在运行']);
 await e.manageBtn.click(); await settle();
});


test('settings errors use global notifications, state selection is announced, and failed saves do not start updates', async () => {
 const h = await harness({}, {
  '/get_settings': async () => ({ ok: true, json: async () => ({ success: true, data: { CONFIG_URL: 'https://example.com/subscription', QUIC: 'true' } }) }),
  '/save_settings': async options => { assert.equal(JSON.parse(options.body).CONFIG_URL, 'https://example.com/subscription'); return { ok: true, json: async () => ({ success: false, msg: '磁盘空间不足' }) }; },
 });
 const e = h.elements;
 await e.settingsBtn.click(); await settle();
 assert.equal(e.settingsBtn.attributes['aria-pressed'], 'true');
 assert.equal(e.manageBtn.attributes['aria-pressed'], 'false');
 assert.equal(e.enableQuic.checked, true);
 assert.equal(e.configFileUrl.value, 'https://example.com/subscription');
 assert.equal(e.saveSettingsBtn.disabled, false);
 await e.saveSettingsBtn.click(); await settle();
 assert.equal(notice(e, 'settings').dataset.tone, 'error');
 assert.match(notice(e, 'settings').textContent, /磁盘空间不足/);
 assert.equal(h.requests.includes('/reload'), false);
 assert.equal(e.saveSettingsBtn.disabled, false);
 assert.equal(e.saveSettingsBtn.attributes['aria-busy'], 'false');
});

test('leaving settings cancels a pending load and its late response cannot overwrite fields', async () => {
 let resolveSettings; let signal;
 const h = await harness({}, { '/get_settings': options => { signal = options.signal; return new Promise(resolve => resolveSettings = resolve); } });
 const e = h.elements;
 await e.settingsBtn.click(); await settle();
 assert.equal(e.saveSettingsBtn.disabled, true);
 assert.equal(e.configFileUrl.disabled, true);
 await e.logsBtn.click(); await settle();
 assert.equal(signal.aborted, true);
 resolveSettings({ ok: true, json: async () => ({ success: true, data: { CONFIG_URL: 'https://late.example.com' } }) });
 await settle();
 assert.equal(e.configFileUrl.value, '');
 assert.equal(e.logsBtn.attributes['aria-pressed'], 'true');
 assert.equal(e.saveSettingsBtn.disabled, true);
 await e.logoutBtn.click(); await settle();
});

test('successful save restores button state before starting one update', async () => {
 let saves = 0;
 const h = await harness({}, { '/save_settings': async () => { saves++; return { ok: true, json: async () => ({ success: true }) }; } });
 const e = h.elements;
 await e.settingsBtn.click(); await settle();
 h.setTask({ id: 'saved-update', running: false, stage: 'finished', result: 'unchanged', message: '配置未变化' });
 await e.saveSettingsBtn.click(); await settle();
 assert.equal(saves, 1);
 assert.equal(h.requests.filter(url => url === '/reload').length, 1);
 assert.equal(e.logsView.style.display, 'flex');
 assert.equal(e.updateSummary.textContent, '配置未变化');
 assert.equal(e.updateBtn.attributes['aria-busy'], 'false');
 assert.equal(e.updateOverlay.dataset.state, 'unchanged');
 assert.equal(e.updateOverlay.hidden, true);
 assert.match(notice(e, 'update-result').textContent, /配置未变化/);
 await e.logoutBtn.click(); await settle();
});

test('update controls belong to logs and a failed creation closes progress with a retryable result', async () => {
 const logsMarkup = html.split('id="logsView"')[1].split('</main>')[0];
 assert.match(logsMarkup, /id="updateBtn"/);
 assert.match(logsMarkup, /id="updateOverlay"/);
 assert.doesNotMatch(html.match(/<header[\s\S]*?<\/header>/)[0], /id="updateBtn"/);
 const h = await harness({}, { '/reload': async () => { throw new Error('连接不可用'); } });
 const e = h.elements;
 await e.logsBtn.click(); await settle();
 assert.equal(e.updateOverlay.hidden, true);
 await e.updateBtn.click(); await settle();
 assert.equal(e.updateOverlay.hidden, true);
 assert.match(notice(e, 'update-result').textContent, /无法创建更新任务.*连接不可用/);
 assert.equal(e.updateBtn.disabled, false);
 await e.manageBtn.click();
 assert.equal(e.uiContainer.style.display, 'block');
 assert.ok(notice(e, 'update-result'));
 await e.logoutBtn.click();
});

test('authentication network errors keep the login operable and explain failure', async () => {
 const h = await harness({}, { '/check_secret': async () => { throw new Error('连接不可用'); } });
 const e = h.elements;
 assert.equal(e.authOverlay.style.display, 'flex');
 e.secretInput.value = 'example';
 await e.secretSubmitBtn.click();
 assert.match(notice(e, 'auth').textContent, /连接不可用/);
 assert.equal(e.secretInput.attributes['aria-invalid'], 'false');
 assert.equal(e.secretSubmitBtn.disabled, false);
 assert.equal(e.secretSubmitBtn.attributes['aria-busy'], 'false');
});


test('a failed settings load cannot submit empty defaults over the installed configuration', async () => {
 const h = await harness({}, { '/get_settings': async () => ({ ok: false, status: 503 }) });
 const e = h.elements;
 await e.settingsBtn.click(); await settle();
 assert.equal(notice(e, 'settings').dataset.tone, 'error');
 assert.match(notice(e, 'settings').textContent, /503/);
 assert.equal(e.saveSettingsBtn.disabled, true);
 assert.equal(e.configFileUrl.disabled, true);
 await e.saveSettingsBtn.click();
 assert.equal(h.requests.includes('/save_settings'), false);
});

test('finished history is silent; an observed completion notifies once and expires without a fixed result slot', async () => {
 assert.doesNotMatch(html, /id="updateResult|id="secretError|id="settingsStatus|<small|<p>/);
 const h = await harness({ id: 'past', running: false, result: 'updated', stage: 'finished' });
 const e = h.elements;
 assert.equal(e.notifications.children.length, 0);
 await e.logsBtn.click(); await settle();
 assert.equal(e.updateOverlay.hidden, true);
 assert.equal(e.notifications.children.length, 0);
 h.setTask({id:'new', running:false, stage:'finished', result:'updated', message:'配置已验证'});
 await e.updateBtn.click(); await settle();
 assert.equal(notice(e, 'update-result').dataset.tone, 'success');
 assert.equal(e.updateOverlay.hidden, true);
 await e.logsBtn.click(); await settle();
 assert.equal(e.notifications.children.length, 1);
 h.advance(8000);
 assert.equal(e.notifications.children.length, 0);
 await e.logsBtn.click(); await settle();
 assert.equal(e.notifications.children.length, 0);
 await e.logoutBtn.click();
});

test('structured fields render once, unknown fields remain truthful, and multiline messages stay aligned', async () => {
 const h = await harness(); const e = h.elements;
 await e.logsBtn.click(); await settle();
 h.streams[0].push(JSON.stringify({type:'log',time:'2026-10-05T04:00:00.123Z',level:'warning',message:'[UDP] timeout\nretry scheduled'}) + '\n');
 await settle();
 const row = e.logOutput.firstElementChild;
 assert.match(row.children[0].textContent, /\.123$/);
 assert.equal(row.children[1].textContent, 'WARN');
 assert.equal(row.children[1].dataset.level, 'warning');
 assert.equal(messageOf(row), '[UDP] timeout\nretry scheduled');
 h.streams[0].push(JSON.stringify({type:'log',message:'startup message'}) + '\n'); await settle();
 assert.equal(e.logOutput.firstElementChild.children[0].textContent, '—');
 assert.equal(e.logOutput.firstElementChild.children[1].textContent, '—');
 assert.equal(messageOf(e.logOutput.firstElementChild), 'startup message');
 await e.logoutBtn.click();
});

test('global notifications replace by key, bound the queue, escape details and support close and timed dismissal', async () => {
 const h = await harness(); const e = h.elements; const n = h.notices;
 n.show({id:'one',title:'保存成功',tone:'success'});
 n.show({id:'one',title:'保存成功',tone:'success'});
 assert.equal(e.notifications.children.length, 1);
 n.show({id:'one',title:'保存失败',message:'<script>bad</script>',tone:'error'});
 const card = notice(e, 'one');
 assert.equal(card.attributes.role, 'alert');
 assert.equal(card.children[1].children[1].innerHTML, '&lt;script&gt;bad&lt;/script&gt;');
 n.show({id:'two',title:'二'}); n.show({id:'three',title:'三'}); n.show({id:'four',title:'四'});
 assert.equal(e.notifications.children.length, 3);
 assert.equal(notice(e, 'one'), undefined);
 await notice(e, 'two').children[0].children[1].click();
 assert.equal(notice(e, 'two'), undefined);
 h.advance(8000);
 assert.equal(e.notifications.children.length, 0);
});

test('reading notification details or focusing it pauses expiry until all interactions finish', async () => {
 const h = await harness(); const e = h.elements;
 h.notices.show({id:'read',title:'更新失败',message:'diagnostic details',duration:7000});
 const card = notice(e, 'read'); const details = card.children[1];
 h.advance(1000); card.dispatch('pointerenter');
 card.dispatch('focusin'); details.open = true; details.dispatch('toggle');
 h.advance(20000); assert.ok(notice(e, 'read'));
 card.dispatch('pointerleave'); card.dispatch('focusout', {relatedTarget:null});
 h.advance(10000); assert.ok(notice(e, 'read'));
 details.open = false; details.dispatch('toggle');
 h.advance(5000); assert.ok(notice(e, 'read'));
 h.advance(2000); assert.equal(notice(e, 'read'), undefined);
});

test('an incorrect key shows a global error and retains a retryable login', async () => {
 const h = await harness({}, {'/check_secret':async () => ({ok:true,json:async () => ({success:false,msg:'密钥错误'})})});
 const e = h.elements;
 e.secretInput.value = 'incorrect';
 await e.secretSubmitBtn.click();
 assert.equal(e.secretInput.attributes['aria-invalid'], 'true');
 assert.equal(notice(e, 'auth').dataset.tone, 'error');
 assert.match(notice(e, 'auth').textContent, /密钥错误/);
 assert.equal(e.secretSubmitBtn.disabled, false);
 h.advance(13000); assert.equal(notice(e, 'auth'), undefined);
});

test('loss of update observation keeps the task active; reconnecting observes completion instead of allowing a second task', async () => {
 let connected = true;
 const h = await harness({}, { '/update_status':async () => {
   if (!connected) throw new Error('连接中断');
   return {ok:true,json:async () => ({id:'observed',running:false,result:'rolled_back',stage:'finished',message:'恢复完成'})};
 }});
 const e = h.elements;
 h.setTask({id:'observed', running:true, stage:'preparing'});
 connected = false;
 await e.updateBtn.click(); await settle();
 assert.equal(e.updateBtn.disabled, true);
 assert.equal(e.updateOverlay.hidden, false);
 assert.match(notice(e, 'update-connection').textContent, /任务|后台/);
 await e.updateBtn.click();
 assert.equal(h.requests.filter(url => url === '/reload').length, 1);
 connected = true;
 await e.logsBtn.click(); await settle();
 assert.equal(e.updateBtn.disabled, false);
 assert.equal(e.updateOverlay.hidden, true);
 assert.match(notice(e, 'update-result').textContent, /回滚成功/);
 await e.logoutBtn.click();
});
