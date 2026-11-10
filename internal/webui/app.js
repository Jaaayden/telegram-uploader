(() => {
  'use strict';

  // ---------- bridge ----------
  // In the application the page talks to Go through the web view's message
  // channel. With --dev-http the same protocol runs over fetch + SSE so the
  // interface can be opened in a normal browser.
  const pending = new Map();
  let nextCall = 1;
  const native = typeof window.__tvuPost === 'function';

  function call(method, params) {
    const id = nextCall++;
    return new Promise((resolve, reject) => {
      pending.set(id, { resolve, reject });
      const message = JSON.stringify({ id, method, params: params ?? null });
      if (native) {
        window.__tvuPost(message);
      } else {
        fetch('/rpc', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: message })
          .then((response) => response.json())
          .then(window.__tvuReceive)
          .catch((error) => window.__tvuReceive({ id, error: String(error) }));
      }
    });
  }

  window.__tvuReceive = (message) => {
    if (message.event) {
      onEvent(message.event, message.payload);
      return;
    }
    const waiter = pending.get(message.id);
    if (!waiter) return;
    pending.delete(message.id);
    if (message.error) waiter.reject(new Error(message.error));
    else waiter.resolve(message.result);
  };

  // Runs a request and reports failures (and optional success text) as toasts.
  async function run(method, params) {
    try {
      const result = await call(method, params);
      if (result && result.message) toast('info', result.message);
      return result ?? true;
    } catch (error) {
      toast('error', error.message);
      return null;
    }
  }

  // ---------- state ----------
  const $ = (id) => document.getElementById(id);
  const ROW = 56;
  const OVERSCAN = 6;
  let S = null; // service state (everything but queue rows)
  const jobs = new Map();
  let order = [];
  let jobsRev = 0;
  let ready = false;
  const queued = []; // events that arrived before init finished
  const selected = new Set();
  let filter = 'all';
  let view = []; // ids visible under the current filter
  let counts = {};
  let anchorIndex = -1;
  const rows = new Map(); // id -> row element currently in the DOM

  const FILTERS = [
    ['all', '全部', () => true],
    ['pending', '待上传', (j) => j.state === 'queued' || j.state === 'interrupted'],
    ['active', '进行中', (j) => j.state === 'uploading' || j.state === 'sending' || j.state === 'moving'],
    ['done', '已完成', (j) => j.state === 'sent' || j.state === 'moved'],
    ['attention', '需处理', (j) => ['failed', 'confirming', 'oversize', 'cancelled', 'skipped'].includes(j.state)],
  ];
  const RESETTABLE = new Set(['cancelled', 'failed', 'interrupted', 'skipped']);
  const LIMIT_PRESETS = [0, 512, 1024, 2048, 5120, 10240]; // KiB/s

  // ---------- formatting ----------
  function bytes(value) {
    if (!(value > 0)) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let index = 0;
    while (value >= 1024 && index < units.length - 1) { value /= 1024; index++; }
    return (index === 0 || value >= 100 ? Math.round(value) : value.toFixed(value >= 10 ? 1 : 2)) + ' ' + units[index];
  }
  const speed = (value) => bytes(value) + '/s';
  function duration(seconds) {
    if (!(seconds > 0)) return '';
    if (seconds < 60) return '不到 1 分钟';
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return minutes + ' 分钟';
    const hours = Math.floor(minutes / 60);
    return hours + ' 小时' + (minutes % 60 ? ' ' + (minutes % 60) + ' 分钟' : '');
  }
  function limitLabel(kbps) {
    if (!kbps) return '不限速';
    return kbps >= 1024 ? +(kbps / 1024).toFixed(2) + ' MB/s' : kbps + ' KB/s';
  }
  function clock(ms) {
    const date = new Date(ms);
    const now = new Date();
    const day = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
    const delta = Math.round((day(date) - day(now)) / 86400000);
    const time = String(date.getHours()).padStart(2, '0') + ':' + String(date.getMinutes()).padStart(2, '0');
    if (delta === 0) return '今天 ' + time;
    if (delta === 1) return '明天 ' + time;
    return (date.getMonth() + 1) + ' 月 ' + date.getDate() + ' 日 ' + time;
  }
  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value == null || value === false) continue;
        if (key === 'class') node.className = value;
        else if (key === 'text') node.textContent = value;
        else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
        else if (key in node && key !== 'list') node[key] = value;
        else node.setAttribute(key, value);
      }
    }
    for (const child of children) if (child != null) node.append(child);
    return node;
  }

  // ---------- toasts ----------
  function toast(kind, text) {
    if (!text) return;
    const box = $('toasts');
    const node = el('div', { class: 'toast ' + kind, text });
    box.append(node);
    while (box.children.length > 4) box.firstChild.remove();
    setTimeout(() => node.remove(), kind === 'error' ? 8000 : 4200);
  }

  // ---------- modals ----------
  function openModal(build, { wide = false, dismissable = true } = {}) {
    return new Promise((resolve) => {
      const scrim = el('div', { class: 'modal-scrim' });
      const box = el('div', { class: 'modal' + (wide ? ' wide' : ''), role: 'dialog' });
      const close = (value) => { scrim.remove(); document.removeEventListener('keydown', onKey, true); resolve(value); };
      const onKey = (event) => {
        if (event.key === 'Escape' && dismissable && scrim === $('modalRoot').lastChild) { event.stopPropagation(); close(null); }
      };
      scrim.addEventListener('mousedown', (event) => { if (event.target === scrim && dismissable) close(null); });
      document.addEventListener('keydown', onKey, true);
      build(box, close);
      scrim.append(box);
      $('modalRoot').append(scrim);
      const focus = box.querySelector('[autofocus]') || box.querySelector('.modal-foot .btn:last-child');
      if (focus) focus.focus();
    });
  }
  function confirmBox({ title, body, ok = '确定', danger = false }) {
    return openModal((box, close) => {
      box.append(
        el('div', { class: 'modal-head', text: title }),
        el('div', { class: 'modal-body', text: body }),
        el('div', { class: 'modal-foot' },
          el('button', { class: 'btn', text: '取消', onclick: () => close(false) }),
          el('button', { class: 'btn ' + (danger ? 'danger solid' : 'primary'), text: ok, onclick: () => close(true) })),
      );
    });
  }
  function closeModals() { $('modalRoot').replaceChildren(); }

  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // Pages loaded from a string are not a secure context in every web view.
      const area = el('textarea', { value: text, style: 'position:fixed;opacity:0' });
      document.body.append(area);
      area.select();
      document.execCommand('copy');
      area.remove();
    }
    toast('info', '已复制');
  }

  // ---------- job presentation ----------
  function describe(job) {
    if (job.removing) return { tone: 'muted', text: '正在取消并删除…', actions: [] };
    const warn = job.truncated ? '源文件尾部不完整，原样上传 · ' : '';
    switch (job.state) {
      case 'queued': return { tone: '', text: warn + '等待上传', actions: [['cancel', '取消']] };
      case 'oversize': return { tone: 'warn', text: '超过 Bot 的上传上限，不会上传', actions: [] };
      case 'uploading': {
        const percent = job.size > 0 ? Math.floor((job.uploaded / job.size) * 100) : 0;
        const rate = job.speed > 0 ? ' · ' + speed(job.speed) : '';
        return { tone: 'accent', text: `${warn}上传中 ${percent}%${rate}`, bar: job.size > 0 ? job.uploaded / job.size : 0, actions: [['cancel', '取消']] };
      }
      case 'sending': return { tone: 'accent', text: '正在提交频道消息…', bar: 1, actions: [] };
      case 'confirming': return {
        tone: 'warn', text: '不确定是否已送达，请到频道里确认' + (job.error ? '（' + job.error + '）' : ''),
        actions: [['mark-sent', '已送达'], ['retry-confirm', '重新上传'], ['skip', '跳过']],
      };
      case 'sent': return { tone: 'success', text: '已发送', actions: [] };
      case 'cancelled': return { tone: 'muted', text: '已取消', actions: [['retry', '重试']] };
      case 'failed': return { tone: 'danger', text: '失败' + (job.error ? '：' + job.error : ''), actions: [['retry', '重试'], ['skip', '跳过']] };
      case 'skipped': return { tone: 'muted', text: '已跳过', actions: [['retry', '重试']] };
      case 'moving': return { tone: 'accent', text: '正在移动…', actions: [] };
      case 'moved': return { tone: 'success', text: '已移动到超限文件夹', actions: [] };
      case 'interrupted': return { tone: 'warn', text: '上次运行时中断，会重新上传', actions: [['cancel', '取消']] };
      default: return { tone: '', text: job.state, actions: [] };
    }
  }

  // ---------- queue data ----------
  function applyJobs(delta) {
    if (!delta || delta.rev <= jobsRev) return;
    jobsRev = delta.rev;
    for (const job of delta.upsert) {
      const previous = jobs.get(job.id);
      // The transfer rate only travels in progress events; keep it while the
      // row is still uploading.
      if (previous && job.state === 'uploading') job.speed = previous.speed;
      jobs.set(job.id, job);
    }
    if (delta.order) {
      order = delta.order;
      const alive = new Set(order);
      for (const id of jobs.keys()) if (!alive.has(id)) jobs.delete(id);
      for (const id of selected) if (!alive.has(id)) selected.delete(id);
    }
    recompute();
  }

  function recompute() {
    counts = Object.fromEntries(FILTERS.map(([key]) => [key, 0]));
    counts.runnable = 0; counts.oversize = 0; counts.completed = 0;
    counts.resettable = { cancelled: 0, failed: 0, skipped: 0, all: 0, selected: 0 };
    const match = FILTERS.find(([key]) => key === filter)[2];
    view = [];
    for (const id of order) {
      const job = jobs.get(id);
      if (!job) continue;
      for (const [key, , test] of FILTERS) if (test(job)) counts[key]++;
      if (job.state === 'queued' || job.state === 'interrupted') counts.runnable++;
      if (job.state === 'oversize') counts.oversize++;
      if (job.state === 'sent' || job.state === 'moved') counts.completed++;
      if (RESETTABLE.has(job.state)) {
        counts.resettable.all++;
        if (job.state === 'cancelled') counts.resettable.cancelled++;
        else if (job.state === 'skipped') counts.resettable.skipped++;
        else counts.resettable.failed++;
        if (selected.has(id)) counts.resettable.selected++;
      }
      if (match(job)) view.push(id);
    }
    renderAll();
  }

  // ---------- rendering ----------
  let frame = 0;
  function renderAll() {
    if (frame) return;
    frame = requestAnimationFrame(() => {
      frame = 0;
      if (!S) return;
      renderTop();
      renderToolbar();
      renderList();
      renderTransport();
      if (!$('settings').hidden) renderSettings();
    });
  }

  function connectionSummary() {
    const c = S.connection;
    const channel = S.channel.id ? S.channel.title : '';
    switch (c.status) {
      case 'connected':
        return { dot: 'ok', title: channel ? '发送到 ' + channel : '已连接，还没有绑定频道', sub: '@' + c.bot };
      case 'connecting': return { dot: 'busy', title: '正在连接 Telegram…', sub: channel };
      case 'retrying': return { dot: 'busy', title: 'Telegram 暂时连不上', sub: '会自动重试' };
      case 'error': return { dot: 'bad', title: '连接失败，请检查设置', sub: c.message };
      case 'unconfigured': return { dot: '', title: '还没有连接 Telegram', sub: '点这里填写凭据' };
      default: return { dot: '', title: 'Telegram 未连接', sub: channel };
    }
  }

  function renderTop() {
    const summary = connectionSummary();
    $('connDot').className = 'dot ' + summary.dot;
    $('connTitle').textContent = summary.title;
    $('connSub').textContent = summary.sub || '';
    $('connSub').hidden = !summary.sub;

    const limit = S.settings.limitKBps;
    $('limitChipText').textContent = limit ? '限速 ' + limitLabel(limit) : '不限速';
    $('limitChip').classList.toggle('on', limit > 0);

    const update = S.update;
    const badge = $('updateBadge');
    badge.hidden = !['available', 'downloading', 'ready'].includes(update.status);
    if (!badge.hidden) {
      badge.textContent = update.status === 'ready' ? '重启以更新到 v' + update.version : '新版本 v' + update.version;
    }

    // One banner at a time, most blocking first.
    const banner = $('banner');
    let text = '', action = '', act = '', error = false;
    if (S.settings.problem) { text = S.settings.problem; action = '打开设置'; act = 'settings'; error = true; }
    else if (order.length && S.connection.status === 'unconfigured') { text = '还没有连接 Telegram，暂时不能上传。'; action = '去连接'; act = 'settings'; }
    else if (order.length && S.connection.status === 'error') { text = 'Telegram 连接失败：' + (S.connection.message || ''); action = '打开设置'; act = 'settings'; error = true; }
    else if (order.length && S.connection.status === 'connected' && !S.channel.id) { text = '还没有绑定目标频道。'; action = '绑定频道'; act = 'bind'; }
    else if (S.move.active) {
      const percent = S.move.totalBytes > 0 ? Math.floor((S.move.doneBytes / S.move.totalBytes) * 100) : 0;
      text = `正在移动超限文件… ${bytes(S.move.doneBytes)} / ${bytes(S.move.totalBytes)}（${percent}%）`; action = '取消移动'; act = 'cancel-move';
    } else if (S.connection.floodWaitUntil > Date.now()) {
      text = 'Telegram 要求稍等一会儿，到 ' + clock(S.connection.floodWaitUntil) + ' 后自动继续。';
    }
    banner.hidden = !text;
    banner.classList.toggle('error', error);
    $('bannerText').textContent = text;
    $('bannerAction').hidden = !action;
    $('bannerAction').textContent = action;
    $('bannerAction').dataset.act = act;
  }

  function renderToolbar() {
    const tabs = $('filters');
    if (!tabs.children.length) {
      for (const [key, label] of FILTERS) {
        tabs.append(el('button', { class: 'tab', type: 'button', 'data-filter': key },
          el('span', { text: label }), el('span', { class: 'count' })));
      }
    }
    for (const tab of tabs.children) {
      const key = tab.dataset.filter;
      tab.classList.toggle('active', key === filter);
      tab.classList.toggle('alert', key === 'attention' && counts.attention > 0);
      tab.lastChild.textContent = counts[key] || (key === 'all' ? '0' : '');
    }
    const bar = $('selectionBar');
    bar.hidden = selected.size === 0;
    if (selected.size) {
      $('selectionText').textContent = `已选择 ${selected.size} 项`;
      bar.querySelector('[data-act="reset-selected"]').disabled = counts.resettable.selected === 0;
      for (const button of bar.querySelectorAll('button')) if (button.dataset.act !== 'select-none') button.disabled ||= S.move.active;
    }
    $('addBtn').disabled = S.scanning || S.move.active;
    $('addBtn').lastChild.textContent = S.scanning ? ' 正在扫描…' : ' 添加文件夹';
  }

  function renderEmpty() {
    const box = $('empty');
    if (order.length && view.length) { box.hidden = true; return; }
    box.hidden = false;
    if (order.length) {
      box.replaceChildren(el('div', { class: 'empty-card' }, el('p', { text: '这个分类下没有条目。' })));
      return;
    }
    const connected = S.connection.status === 'connected';
    const configured = S.connection.status !== 'unconfigured';
    const bound = !!S.channel.id;
    const step = (number, done, current, title, text, label, act) => el('div', { class: 'step' + (done ? ' done' : current ? ' current' : '') },
      el('div', { class: 'step-num', text: done ? '✓' : String(number) }),
      el('div', { class: 'step-text' }, el('b', { text: title }), el('span', { text })),
      !done && current ? el('button', { class: 'btn small primary', type: 'button', text: label, 'data-act': act }) : null);
    box.replaceChildren(el('div', { class: 'empty-card' },
      el('h2', { text: '队列是空的' }),
      el('p', { text: '三步开始：连接 Bot、绑定频道，然后把视频文件夹加进来。' }),
      el('div', { class: 'steps' },
        step(1, connected, !connected, '连接 Telegram Bot',
          connected ? '已连接 @' + S.connection.bot : configured ? connectionSummary().title : '填写 API ID、API Hash 和 Bot Token', '去连接', 'settings'),
        step(2, bound, connected && !bound, '绑定目标频道',
          bound ? S.channel.title : '在频道里发一条验证码，自动完成绑定', '绑定频道', 'bind'),
        step(3, false, connected && bound, '添加视频',
          '选择文件夹，勾选要上传的 MP4，按文件名顺序排队', '添加文件夹', 'add'))));
  }

  function buildRow() {
    const check = el('input', { type: 'checkbox', tabIndex: -1 });
    const index = el('div', { class: 'row-index' });
    const name = el('div', { class: 'row-name' });
    const status = el('span', { class: 'row-status' });
    const size = el('span', { class: 'row-size' });
    const fill = el('i');
    const bar = el('div', { class: 'row-bar' }, fill);
    const actions = el('div', { class: 'row-actions' });
    const node = el('div', { class: 'row-item' },
      el('label', { class: 'row-check' }, check), index,
      el('div', { class: 'row-body' }, name, el('div', { class: 'row-meta' }, size, bar, status)), actions);
    node.parts = { check, index, name, status, size, bar, fill, actions };
    return node;
  }

  function paintRow(node, job, position) {
    const info = describe(job);
    const parts = node.parts;
    node.dataset.id = job.id;
    node.style.transform = `translateY(${position * ROW}px)`;
    node.className = 'row-item' + (info.tone ? ' tone-' + info.tone : '') + (selected.has(job.id) ? ' selected' : '');
    parts.check.checked = selected.has(job.id);
    parts.check.disabled = S.move.active;
    parts.index.textContent = order.length === view.length ? position + 1 : order.indexOf(job.id) + 1;
    if (parts.name.textContent !== job.name) { parts.name.textContent = job.name; parts.name.title = job.path; }
    parts.size.textContent = bytes(job.size);
    parts.status.textContent = info.text;
    parts.status.title = info.text;
    parts.bar.hidden = info.bar == null;
    if (info.bar != null) parts.fill.style.width = Math.min(100, info.bar * 100).toFixed(1) + '%';
    const signature = info.actions.map((action) => action[0]).join(',');
    if (node.dataset.actions !== signature) {
      node.dataset.actions = signature;
      parts.actions.replaceChildren(...info.actions.map(([act, label]) =>
        el('button', { class: 'btn small' + (act === 'cancel' || act === 'skip' ? ' ghost' : ''), type: 'button', text: label, 'data-act': act })));
    }
  }

  // Only the rows inside the viewport (plus a small margin) exist in the DOM,
  // so painting costs the same for 10 rows as for 10,000.
  function renderList() {
    renderEmpty();
    const list = $('list');
    $('listSpacer').style.height = view.length * ROW + 'px';
    const first = Math.max(0, Math.floor(list.scrollTop / ROW) - OVERSCAN);
    const last = Math.min(view.length, Math.ceil((list.scrollTop + list.clientHeight) / ROW) + OVERSCAN);
    const wanted = new Set();
    for (let position = first; position < last; position++) {
      const id = view[position];
      const job = jobs.get(id);
      if (!job) continue;
      wanted.add(id);
      let node = rows.get(id);
      if (!node) {
        node = buildRow();
        rows.set(id, node);
        $('listSpacer').append(node);
      }
      paintRow(node, job, position);
    }
    for (const [id, node] of rows) {
      if (!wanted.has(id)) { node.remove(); rows.delete(id); }
    }
  }

  function renderTransport() {
    const q = S.queue;
    const start = $('startBtn');
    const busy = S.move.active;
    if (q.running) {
      start.textContent = q.pauseRequested ? '当前视频传完后暂停' : '暂停';
      start.disabled = q.pauseRequested;
      start.dataset.act = 'pause';
      start.classList.remove('primary');
    } else {
      start.textContent = q.paused ? '继续上传' : '开始上传';
      start.disabled = busy || counts.runnable === 0;
      start.dataset.act = 'start';
      start.classList.add('primary');
    }
    $('cancelAllBtn').hidden = !q.running;
    $('totalBar').style.width = (q.totalBytes > 0 ? Math.min(100, (q.doneBytes / q.totalBytes) * 100) : 0).toFixed(2) + '%';
    $('summary').textContent = order.length
      ? `${bytes(q.doneBytes)} / ${bytes(q.totalBytes)} · 已完成 ${counts.completed} / ${order.length}`
      : '队列为空';
    let activity = '';
    if (q.running && q.speed > 0) activity = speed(q.speed) + (q.etaSeconds > 0 ? ' · 剩余约 ' + duration(q.etaSeconds) : '');
    else if (q.running && q.pauseRequested) activity = '传完当前视频后暂停';
    else if (q.running) activity = '正在准备…';
    else if (q.paused) activity = '已暂停';
    else if (q.lastError) activity = q.lastError;
    $('activity').textContent = activity;
    $('activity').title = activity;

    const schedule = S.schedule;
    const button = $('scheduleBtn');
    button.classList.toggle('scheduled', schedule.at > 0);
    if (schedule.at > 0) {
      $('scheduleText').textContent = schedule.due ? '定时已到 · ' + (schedule.waiting || '') : clock(schedule.at) + ' 开始';
      button.title = '点击修改或取消定时';
      button.disabled = false;
    } else {
      $('scheduleText').textContent = '定时开始';
      button.title = '在指定时间自动开始上传（应用需保持打开）';
      button.disabled = q.running || q.paused || busy || counts.runnable === 0;
    }
  }

  // ---------- settings panel ----------
  let formLoaded = false;
  function openSettings(section) {
    $('settings').hidden = false;
    $('scrim').hidden = false;
    if (!formLoaded) loadForm();
    renderSettings();
    if (section) $(section).scrollIntoView({ block: 'start' });
  }
  function closeSettings() {
    $('settings').hidden = true;
    $('scrim').hidden = true;
    formLoaded = false;
  }
  function loadForm() {
    const s = S.settings;
    $('fApiId').value = s.apiId || '';
    $('fApiHash').value = '';
    $('fBotToken').value = '';
    $('fProxyEnabled').checked = s.proxyEnabled;
    $('fProxyAddress').value = s.proxyAddress || '';
    $('fProxyUser').value = s.proxyUsername || '';
    $('fProxyPass').value = '';
    $('proxyFields').hidden = !s.proxyEnabled;
    formLoaded = true;
  }
  function segmented(root, options, current, onPick) {
    if (root.children.length !== options.length) {
      root.replaceChildren(...options.map(([value, label]) =>
        el('button', { type: 'button', text: label, onclick: () => onPick(value) })));
    }
    options.forEach(([value], index) => root.children[index].classList.toggle('active', value === current));
  }
  function renderSettings() {
    const s = S.settings, c = S.connection, q = S.queue;
    $('settingsProblem').hidden = !s.problem;
    $('settingsProblem').textContent = s.problem || '';
    // Stored secrets never travel to the page; an empty field keeps them.
    $('fApiHash').placeholder = s.hasApiHash ? '已保存（留空则不修改）' : '从 my.telegram.org 获取';
    $('fBotToken').placeholder = s.hasBotToken ? '已保存（留空则不修改）' : '从 BotFather 获取';
    $('fProxyPass').placeholder = s.hasProxyPassword ? '已保存（留空则不修改）' : '';
    const locked = q.running || S.move.active;
    for (const input of $('connForm').querySelectorAll('input')) input.disabled = locked;
    $('connSave').disabled = locked || c.status === 'connecting';
    $('connSave').textContent = c.status === 'connected' ? '保存并重新连接' : '保存并连接';
    $('connStatus').textContent = locked ? '上传或移动进行中，暂时不能修改'
      : c.status === 'connected' ? '已连接 @' + c.bot
      : c.status === 'connecting' ? '正在连接…'
      : c.status === 'retrying' ? '暂时连不上，会自动重试'
      : c.status === 'error' ? c.message : '';

    $('channelName').textContent = S.channel.id ? S.channel.title : '尚未绑定';
    $('bindBtn').textContent = S.channel.id ? '更换频道' : '绑定频道';
    $('bindBtn').disabled = c.status !== 'connected' || q.running;
    $('channelHint').textContent = c.status === 'connected'
      ? 'Bot 只需要频道的“发布消息”权限。' : '连接 Telegram 后才能绑定频道。';

    segmented($('concurrency'), [[4, '兼容 · 4 路'], [8, '均衡 · 8 路'], [12, '高速 · 12 路']], s.concurrency,
      (value) => run('setConcurrency', { value }));
    const presets = LIMIT_PRESETS.map((value) => [value, value ? limitLabel(value).replace(' MB/s', ' MB').replace(' KB/s', ' KB') : '不限']);
    segmented($('limitSeg'), presets, s.limitKBps, setLimit);
    if (document.activeElement !== $('limitInput')) $('limitInput').value = s.limitKBps ? +(s.limitKBps / 1024).toFixed(2) : '';
    $('limitHint').textContent = s.limitKBps
      ? `当前限速 ${limitLabel(s.limitKBps)}。直接输入数值或点上面的快捷档位，立即生效。`
      : '当前不限速，会用满上行带宽。输入数值（留空或 0 表示不限）或点快捷档位，立即生效，不用重新连接。';
    $('maxSize').textContent = c.status === 'connected' && c.maxBytes
      ? (c.maxExact ? '当前 Bot 的单文件上限：' : '协议上限（服务端数值未确认）：') + bytes(c.maxBytes) : '';

    const u = S.update;
    $('aboutVersion').textContent = `${S.app.name} v${S.app.version}` + (S.app.commit ? `（${S.app.commit.slice(0, 7)}）` : '');
    const button = $('updateBtn');
    const progress = u.status === 'downloading';
    $('updateProgress').hidden = !progress;
    if (progress) $('updateBar').style.width = (u.totalBytes > 0 ? (u.doneBytes / u.totalBytes) * 100 : 0).toFixed(1) + '%';
    $('updateNotes').hidden = !u.notes || !['available', 'downloading', 'ready', 'error'].includes(u.status);
    $('updateNotes').textContent = u.notes || '';
    button.hidden = !u.supported;
    button.disabled = false;
    button.className = 'btn';
    let status = '';
    switch (u.status) {
      case 'checking': status = '正在检查更新…'; button.textContent = '检查中…'; button.disabled = true; button.dataset.act = ''; break;
      case 'uptodate': status = '已是最新版本'; button.textContent = '检查更新'; button.dataset.act = 'checkUpdate'; break;
      case 'available':
        status = '发现新版本 v' + u.version + (u.installHint ? '。' + u.installHint + '，请到发布页面手动下载。' : '');
        button.textContent = u.installHint ? '打开发布页面' : '下载并安装';
        button.dataset.act = u.installHint ? 'openRelease' : 'downloadUpdate';
        button.className = 'btn primary';
        break;
      case 'downloading':
        status = `正在下载 v${u.version}… ${bytes(u.doneBytes)} / ${bytes(u.totalBytes)}`;
        button.textContent = '取消下载'; button.dataset.act = 'cancelUpdateDownload'; break;
      case 'ready':
        status = `v${u.version} 已下载并校验` + (q.running ? '；上传结束或暂停后再重启' : '');
        button.textContent = '重启并更新'; button.dataset.act = 'installUpdate'; button.className = 'btn primary'; button.disabled = q.running; break;
      case 'error':
        status = u.message || '更新失败';
        button.textContent = u.version ? '重试下载' : '重新检查';
        button.dataset.act = u.version ? 'downloadUpdate' : 'checkUpdate'; break;
      default: status = u.supported ? '' : '这个构建不支持应用内更新'; button.textContent = '检查更新'; button.dataset.act = 'checkUpdate';
    }
    $('updateStatus').textContent = status;
    $('fAutoUpdate').checked = s.autoUpdateCheck;
    $('fAutoUpdate').disabled = !u.supported;
  }

  async function setLimit(kbps) {
    await run('setUploadLimit', { kbps });
  }

  // ---------- flows ----------
  async function bindChannel() {
    let result;
    try { result = await call('beginBinding'); } catch (error) { toast('error', error.message); return; }
    await openModal((box, close) => {
      box.dataset.kind = 'bind';
      box.append(
        el('div', { class: 'modal-head', text: '绑定目标频道' }),
        el('div', { class: 'modal-body plain' },
          el('p', { text: '1. 把 Bot 加为目标频道的管理员，授予“发布消息”权限。' }),
          el('p', { text: '2. 在这个频道里发送下面这条验证码（发完可以删除）：' }),
          el('div', { class: 'code-box', text: result.code }),
          el('p', { class: 'hint', text: '收到后会自动验证并保存频道，这个窗口会自己关闭。' })),
        el('div', { class: 'modal-foot' },
          el('button', { class: 'btn', text: '关闭', onclick: () => close(null) }),
          el('button', { class: 'btn primary', text: '复制验证码', onclick: () => copyText(result.code) })));
    });
  }

  async function addFolder() {
    let scan;
    try { scan = await call('addFolder'); } catch (error) { toast('error', error.message); return; }
    if (!scan || scan.cancelled) return;
    if (!scan.candidates.length) { toast('info', '这个文件夹的顶层没有 MP4 文件（不会扫描子文件夹）'); return; }
    const picked = scan.candidates.map((candidate) => !candidate.duplicate);
    const added = await openModal((box, close) => {
      const count = el('span', { class: 'hint spacer' });
      const add = el('button', { class: 'btn primary', text: '加入队列' });
      const list = el('div', { class: 'cand-list' });
      const boxes = [];
      const refresh = () => {
        const total = picked.filter(Boolean).length;
        const duplicates = scan.candidates.filter((candidate) => candidate.duplicate).length;
        count.textContent = `已选 ${total} / ${scan.candidates.length}` + (duplicates ? `，${duplicates} 个已在队列` : '');
        add.disabled = total === 0;
      };
      scan.candidates.forEach((candidate, index) => {
        const check = el('input', { type: 'checkbox', checked: picked[index], disabled: candidate.duplicate,
          onchange: () => { picked[index] = check.checked; refresh(); } });
        boxes.push(check);
        list.append(el('label', { class: 'cand' + (candidate.duplicate ? ' off' : '') }, check,
          el('span', { class: 'name', text: candidate.name, title: candidate.name }),
          el('span', { class: 'meta' + (candidate.oversize ? ' warn' : ''),
            text: bytes(candidate.size) + (candidate.duplicate ? ' · 已在队列' : candidate.oversize ? ' · 超过上限' : '') })));
      });
      const setAll = (value) => {
        scan.candidates.forEach((candidate, index) => { if (!candidate.duplicate) { picked[index] = value; boxes[index].checked = value; } });
        refresh();
      };
      add.onclick = () => close(picked.flatMap((value, index) => (value ? [index] : [])));
      box.append(
        el('div', { class: 'modal-head', text: '选择要加入队列的视频' }),
        el('div', { class: 'modal-body plain' },
          el('div', { class: 'hint', text: scan.folder, title: scan.folder }),
          el('div', { class: 'quick' },
            el('button', { class: 'btn small', type: 'button', text: '全选', onclick: () => setAll(true) }),
            el('button', { class: 'btn small', type: 'button', text: '全不选', onclick: () => setAll(false) })),
          list),
        el('div', { class: 'modal-foot' }, count,
          el('button', { class: 'btn', text: '取消', onclick: () => close(null) }), add));
      refresh();
    }, { wide: true });
    if (added) await run('addCandidates', { token: scan.token, indexes: added });
    else call('discardScan', { token: scan.token }).catch(() => {});
  }

  async function scheduleDialog() {
    const current = S.schedule.at;
    const local = (date) => {
      const pad = (n) => String(n).padStart(2, '0');
      return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
    };
    const initial = current > Date.now() ? new Date(current) : new Date(Math.ceil((Date.now() + 10 * 60000) / 60000) * 60000);
    const result = await openModal((box, close) => {
      const input = el('input', { class: 'input', type: 'datetime-local', value: local(initial), autofocus: true });
      const error = el('div', { class: 'field-error' });
      const at = (hour, minute = 0) => {
        const date = new Date(); date.setHours(hour, minute, 0, 0);
        if (date <= new Date()) date.setDate(date.getDate() + 1);
        return date;
      };
      const quick = [['30 分钟后', () => new Date(Date.now() + 30 * 60000)], ['1 小时后', () => new Date(Date.now() + 3600000)],
        ['今晚 23:00', () => at(23)], ['凌晨 02:00', () => at(2)]];
      const submit = () => {
        const when = new Date(input.value);
        if (isNaN(when)) { error.textContent = '请选择日期和时间'; return; }
        if (when <= new Date()) { error.textContent = '时间必须晚于现在'; return; }
        close({ unix: Math.floor(when.getTime() / 1000) });
      };
      box.append(
        el('div', { class: 'modal-head', text: '定时开始上传' }),
        el('div', { class: 'modal-body plain' },
          el('p', { text: '到点后自动开始。应用需要保持打开；退出后计划会保留，下次打开时恢复。' }),
          el('div', { class: 'quick' }, ...quick.map(([label, make]) =>
            el('button', { class: 'btn small', type: 'button', text: label, onclick: () => { input.value = local(make()); error.textContent = ''; } }))),
          input, error),
        el('div', { class: 'modal-foot' },
          current ? el('button', { class: 'btn danger spacer', text: '取消定时', onclick: () => close({ cancel: true }) }) : null,
          el('button', { class: 'btn', text: '关闭', onclick: () => close(null) }),
          el('button', { class: 'btn primary', text: current ? '修改时间' : '设置定时', onclick: submit })));
    });
    if (!result) return;
    if (result.cancel) { if (await run('cancelSchedule')) toast('info', '已取消定时'); return; }
    if (await run('setSchedule', { unix: result.unix })) toast('info', '已设置：' + clock(result.unix * 1000) + ' 开始上传');
  }

  async function resetDialog() {
    const r = counts.resettable;
    const mode = await openModal((box, close) => {
      const option = (label, count, value) => el('button', { class: 'menu-item', type: 'button', disabled: count === 0, onclick: () => close(value) },
        el('span', { text: label }), el('span', { class: 'count', text: String(count) }));
      box.append(
        el('div', { class: 'modal-head', text: '重置任务' }),
        el('div', { class: 'modal-body plain' },
          el('p', { text: '把任务恢复为“待上传”，不用重新扫描。为避免重复消息，待确认、已发送、已移动和超限的任务不会被批量重置。' }),
          el('div', { class: 'option-list' },
            option('所选任务', r.selected, 'selected'), option('已取消的任务', r.cancelled, 'cancelled'),
            option('失败或中断的任务', r.failed, 'failed'), option('已跳过的任务', r.skipped, 'skipped'),
            option('全部可恢复的任务', r.all, 'all'))),
        el('div', { class: 'modal-foot' }, el('button', { class: 'btn', text: '关闭', onclick: () => close(null) })));
    });
    if (mode) await run('resetJobs', { mode, ids: mode === 'selected' ? [...selected] : [] });
  }

  async function removeSelected() {
    const ids = order.filter((id) => selected.has(id));
    if (!ids.length) return;
    const active = ids.includes(S.queue.activeId);
    const ok = await confirmBox({
      title: `从队列删除 ${ids.length} 个条目？`,
      body: '只是从本地队列里移除，不会删除磁盘上的视频，也不会删除频道里已发送的消息。'
        + (active ? '\n\n正在上传的那个会先取消，安全结束后再删除。' : ''),
      ok: '删除', danger: true,
    });
    if (ok && await run('removeJobs', { ids })) { selected.clear(); recompute(); }
  }

  const actions = {
    settings: () => openSettings(),
    bind: bindChannel,
    add: addFolder,
    'cancel-move': () => run('cancelMove'),
    start: () => run('start').then((ok) => { if (!ok && S.connection.status !== 'connected') openSettings(); }),
    pause: () => run('pause'),
    'select-none': () => { selected.clear(); recompute(); },
    'reset-selected': () => run('resetJobs', { mode: 'selected', ids: [...selected] }),
    'remove-selected': removeSelected,
    checkUpdate: () => run('checkUpdate'),
    downloadUpdate: () => run('downloadUpdate'),
    cancelUpdateDownload: () => run('cancelUpdateDownload'),
    openRelease: () => run('openLink', { target: 'release' }),
    installUpdate: async () => {
      if (await confirmBox({ title: '重启并更新？', body: '应用会关闭并自动重新打开，队列和设置都会保留。', ok: '重启并更新' })) run('installUpdate');
    },
  };

  async function rowAction(act, id) {
    switch (act) {
      case 'cancel': return run('cancelJob', { id });
      case 'retry': return run('retryJob', { id });
      case 'skip': return run('skipJob', { id });
      case 'retry-confirm':
        if (await confirmBox({ title: '重新上传这个视频？', body: '请先到频道里确认它确实没有出现；否则可能产生重复消息。', ok: '重新上传' })) run('retryJob', { id });
        return;
      case 'mark-sent':
        if (await confirmBox({ title: '标记为已发送？', body: '只在你已经在频道里看到这条视频时使用。', ok: '标记为已发送' })) run('markSent', { id });
        return;
    }
  }

  function buildMoreMenu() {
    const menu = $('moreMenu');
    const item = (label, count, handler, danger) => el('button', {
      class: 'menu-item' + (danger ? ' danger' : ''), type: 'button', disabled: !count || S.move.active,
      onclick: () => { menu.hidden = true; handler(); },
    }, el('span', { text: label }), typeof count === 'number' ? el('span', { class: 'count', text: String(count) }) : null);
    menu.replaceChildren(
      item('全选', order.length, () => { for (const id of view) selected.add(id); recompute(); }),
      item('重置任务…', counts.resettable.all, resetDialog),
      item('移动超限文件…', S.queue.running ? 0 : counts.oversize, () => run('moveOversize')),
      el('div', { class: 'menu-sep' }),
      item('删除已完成', counts.completed, async () => {
        if (await confirmBox({ title: `删除 ${counts.completed} 个已完成的条目？`, body: '频道里的消息和磁盘上的文件都不会被删除。', ok: '删除', danger: true })) run('removeCompleted');
      }),
      item(S.queue.running ? '取消并清空队列' : '清空队列', order.length, async () => {
        const ok = await confirmBox({
          title: `清空队列里的 ${order.length} 个条目？`,
          body: '不会删除磁盘文件或频道消息。' + (S.queue.running ? '\n\n正在上传的视频会先取消；提交结果未知的任务会保留为“待确认”。' : ''),
          ok: '清空', danger: true,
        });
        if (ok && await run('clearQueue')) { selected.clear(); recompute(); }
      }, true));
  }

  function buildLimitMenu() {
    const current = S.settings.limitKBps;
    $('limitPresets').replaceChildren(...LIMIT_PRESETS.map((value) => el('button', {
      class: 'menu-item' + (value === current ? ' checked' : ''), type: 'button', text: limitLabel(value),
      onclick: () => { $('limitPopover').hidden = true; setLimit(value); },
    })));
    $('limitCustom').value = current ? +(current / 1024).toFixed(2) : '';
  }

  // ---------- events from Go ----------
  function onEvent(event, payload) {
    if (!ready) { queued.push([event, payload]); return; }
    switch (event) {
      case 'state': S = payload; renderAll(); break;
      case 'jobs': applyJobs(payload); break;
      case 'progress': {
        const job = jobs.get(payload.id);
        if (job) { job.uploaded = payload.uploaded; job.speed = payload.speed; }
        S.queue.doneBytes = payload.doneBytes; S.queue.totalBytes = payload.totalBytes;
        S.queue.speed = payload.queueSpeed; S.queue.etaSeconds = payload.etaSeconds;
        const node = rows.get(payload.id);
        if (node && job) paintRow(node, job, view.indexOf(payload.id));
        renderTransport();
        break;
      }
      case 'toast': toast(payload.kind, payload.text); break;
      case 'channel-bound':
        for (const box of document.querySelectorAll('.modal[data-kind="bind"]')) box.parentNode.remove();
        toast('success', '频道绑定成功：' + payload.title);
        break;
      case 'confirm-quit': confirmQuit(payload); break;
    }
  }

  let quitAsking = false;
  async function confirmQuit(payload) {
    if (quitAsking) return;
    quitAsking = true;
    const uploading = payload.reason === 'uploading';
    const ok = await confirmBox({
      title: uploading ? '退出并取消上传？' : '退出应用？',
      body: uploading
        ? '还有视频正在上传。退出会取消当前和排队中的上传，已经发送的消息不受影响。'
        : `定时计划（${clock(payload.at)}）会保留，但应用退出期间不会自动开始，下次打开时恢复。`,
      ok: '退出', danger: uploading,
    });
    quitAsking = false;
    if (ok) call('quit').catch(() => {});
  }

  // ---------- wiring ----------
  function wire() {
    document.addEventListener('click', (event) => {
      const link = event.target.closest('[data-link]');
      if (link) { event.preventDefault(); run('openLink', { target: link.dataset.link }); return; }
      const button = event.target.closest('[data-act]');
      if (button && !button.disabled) {
        const row = button.closest('.row-item');
        if (row) rowAction(button.dataset.act, row.dataset.id);
        else if (actions[button.dataset.act]) actions[button.dataset.act]();
      }
      // Clicking elsewhere closes open popovers.
      for (const [popover, anchor] of [['limitPopover', 'limitChip'], ['moreMenu', 'moreBtn']]) {
        if (!$(popover).hidden && !event.target.closest('#' + popover) && !event.target.closest('#' + anchor)) $(popover).hidden = true;
      }
    });
    $('connChip').onclick = () => openSettings('secConnection');
    $('settingsBtn').onclick = () => openSettings();
    $('settingsClose').onclick = closeSettings;
    $('scrim').onclick = closeSettings;
    $('updateBadge').onclick = () => openSettings('secUpdate');
    $('limitChip').onclick = () => { buildLimitMenu(); $('limitPopover').hidden = !$('limitPopover').hidden; };
    $('moreBtn').onclick = () => { buildMoreMenu(); $('moreMenu').hidden = !$('moreMenu').hidden; };
    $('addBtn').onclick = addFolder;
    $('scheduleBtn').onclick = scheduleDialog;
    $('bindBtn').onclick = bindChannel;
    $('logsBtn').onclick = () => run('openLogs');
    $('cancelAllBtn').onclick = async () => {
      if (await confirmBox({ title: '立即取消全部上传？', body: '会中断当前视频并取消所有还没发送的视频；已经发送的消息不受影响。之后可以用“重置任务”恢复。', ok: '全部取消', danger: true })) run('cancelAll');
    };
    // Both limit inputs take MB/s; empty or 0 removes the limit.
    const submitLimit = (input) => (event) => {
      event.preventDefault();
      const text = $(input).value.trim();
      const value = text === '' ? 0 : parseFloat(text);
      if (!(value >= 0)) { toast('error', '请输入数字，单位是 MB/s；留空或 0 表示不限速'); return; }
      $('limitPopover').hidden = true;
      $(input).blur();
      setLimit(value === 0 ? 0 : Math.max(1, Math.round(value * 1024)));
    };
    $('limitCustomForm').onsubmit = submitLimit('limitCustom');
    $('limitForm').onsubmit = submitLimit('limitInput');
    $('fProxyEnabled').onchange = () => { $('proxyFields').hidden = !$('fProxyEnabled').checked; };
    $('fAutoUpdate').onchange = () => run('setAutoUpdateCheck', { enabled: $('fAutoUpdate').checked });
    $('connForm').onsubmit = async (event) => {
      event.preventDefault();
      const secret = (id) => ($(id).value.trim() ? $(id).value.trim() : null);
      const ok = await run('saveConnection', {
        apiId: parseInt($('fApiId').value.trim(), 10) || 0,
        apiHash: secret('fApiHash'), botToken: secret('fBotToken'),
        proxyEnabled: $('fProxyEnabled').checked,
        proxyAddress: $('fProxyAddress').value.trim(), proxyUsername: $('fProxyUser').value.trim(),
        // An untouched password field keeps the stored password.
        proxyPassword: $('fProxyPass').value ? $('fProxyPass').value : null,
      });
      if (ok) { $('fApiHash').value = ''; $('fBotToken').value = ''; $('fProxyPass').value = ''; }
    };
    $('filters').addEventListener('click', (event) => {
      const tab = event.target.closest('[data-filter]');
      if (!tab) return;
      filter = tab.dataset.filter;
      anchorIndex = -1;
      $('list').scrollTop = 0;
      recompute();
    });

    const list = $('list');
    list.addEventListener('scroll', () => renderAll(), { passive: true });
    new ResizeObserver(() => renderAll()).observe(list);
    list.addEventListener('click', (event) => {
      const row = event.target.closest('.row-item');
      if (!row || event.target.closest('[data-act]') || S.move.active) return;
      const id = row.dataset.id;
      const index = view.indexOf(id);
      const onCheck = !!event.target.closest('.row-check');
      if (event.shiftKey && anchorIndex >= 0) {
        const [from, to] = index < anchorIndex ? [index, anchorIndex] : [anchorIndex, index];
        for (let i = from; i <= to; i++) selected.add(view[i]);
      } else if (onCheck || event.metaKey || event.ctrlKey) {
        if (selected.has(id)) selected.delete(id); else selected.add(id);
        anchorIndex = index;
      } else {
        const only = selected.size === 1 && selected.has(id);
        selected.clear();
        if (!only) selected.add(id);
        anchorIndex = index;
      }
      if (onCheck) event.preventDefault();
      recompute();
    });
    list.addEventListener('dblclick', (event) => {
      const row = event.target.closest('.row-item');
      const job = row && jobs.get(row.dataset.id);
      if (job && !event.target.closest('[data-act]')) copyText(job.path);
    });

    document.addEventListener('keydown', (event) => {
      const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(event.target.tagName);
      if (event.key === 'Escape') {
        if ($('modalRoot').children.length) return;
        if (!$('limitPopover').hidden || !$('moreMenu').hidden) { $('limitPopover').hidden = true; $('moreMenu').hidden = true; }
        else if (!$('settings').hidden) closeSettings();
        else if (selected.size) { selected.clear(); recompute(); }
        return;
      }
      if (typing || $('modalRoot').children.length || !$('settings').hidden) return;
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'a') {
        event.preventDefault();
        for (const id of view) selected.add(id);
        recompute();
      } else if ((event.key === 'Delete' || event.key === 'Backspace') && selected.size) {
        event.preventDefault();
        removeSelected();
      }
    });
    // The page is an application, not a document: no browser context menu
    // outside text fields, and no file drops navigating away.
    document.addEventListener('contextmenu', (event) => { if (!/^(INPUT|TEXTAREA)$/.test(event.target.tagName)) event.preventDefault(); });
    for (const type of ['dragover', 'drop']) document.addEventListener(type, (event) => event.preventDefault());
    // Relative times ("今天 23:00") and flood-wait banners age without events.
    setInterval(() => { if (S && (S.schedule.at || S.connection.floodWaitUntil)) renderAll(); }, 30000);
  }

  async function start() {
    wire();
    if (!native) {
      const source = new EventSource('/events');
      source.onmessage = (event) => window.__tvuReceive(JSON.parse(event.data));
    }
    let init;
    for (;;) {
      try { init = await call('init'); break; } catch { await new Promise((resolve) => setTimeout(resolve, 500)); }
    }
    S = init.state;
    ready = true;
    applyJobs(init.jobs);
    $('app').hidden = false;
    recompute();
    for (const notice of init.notices) toast('info', notice);
    for (const [event, payload] of queued.splice(0)) onEvent(event, payload);
  }

  start();
})();
