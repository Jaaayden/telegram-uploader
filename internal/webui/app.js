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
  const groupInfo = new Map(); // id -> { seq, index, total } for groups with >= 2 items
  const groupMembers = new Map(); // groupId -> [job, ...] in queue order
  const pinnedGroups = new Set();
  let groupCount = 0;
  let dragging = null;
  let dropLine = null;

  const MAX_CAPTION = 1024;
  const PINNED_STATES = new Set(['uploading', 'sending', 'confirming', 'sent', 'moving', 'moved']);
  const EDITABLE_STATES = new Set(['queued', 'interrupted', 'failed', 'cancelled', 'skipped']);

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

  function defaultNameCaption(name) {
    return name && name.toLowerCase().endsWith('.mp4') ? name.slice(0, -4) : (name || '');
  }
  function utf16Truncate(s, maxUnits = MAX_CAPTION) {
    if (!s || s.length <= maxUnits) return s || '';
    const limit = maxUnits - 1;
    let out = '', units = 0;
    for (const ch of s) {
      if (units + ch.length > limit) break;
      out += ch;
      units += ch.length;
    }
    return out + '…';
  }
  function isJobMovable(job) {
    if (!job || PINNED_STATES.has(job.state) || job.removing) return false;
    if (S && S.queue && S.queue.activeId === job.id) return false;
    if (job.groupId && pinnedGroups.has(job.groupId)) return false;
    return true;
  }
  function isJobCaptionEditable(job) {
    if (!job || !EDITABLE_STATES.has(job.state) || job.removing) return false;
    if (S && S.queue && S.queue.activeId === job.id) return false;
    if (job.groupId && pinnedGroups.has(job.groupId)) return false;
    return true;
  }

  // ---------- job presentation ----------
  function describe(job) {
    if (job.removing) return { tone: 'muted', text: '正在取消并删除…', actions: [] };
    const warn = job.truncated ? '源文件尾部不完整，原样上传 · ' : '';
    const canCap = isJobCaptionEditable(job);
    const withCap = (list) => (canCap ? [['edit-caption', 'Caption'], ...list] : list);
    switch (job.state) {
      case 'queued':
        if (job.staged) return { tone: 'accent', text: '已上传，等待同组合并发送', bar: 1, actions: withCap([['cancel', '取消']]) };
        return { tone: '', text: warn + '等待上传', actions: withCap([['cancel', '取消']]) };
      case 'oversize': return { tone: 'warn', text: '超过 Bot 的上传上限，不会上传', actions: [] };
      case 'uploading': {
        const percent = job.size > 0 ? Math.floor((job.uploaded / job.size) * 100) : 0;
        const rate = job.speed > 0 ? ' · ' + speed(job.speed) : '';
        return { tone: 'accent', text: `${warn}上传中 ${percent}%${rate}`, bar: job.size > 0 ? job.uploaded / job.size : 0, actions: [['cancel', '取消']] };
      }
      case 'sending': return { tone: 'accent', text: job.note || '正在提交频道消息…', bar: 1, actions: [] };
      case 'confirming': return {
        tone: 'warn', text: '不确定是否已送达，请到频道里确认' + (job.error ? '（' + job.error + '）' : ''),
        actions: [['mark-sent', '已送达'], ['retry-confirm', '重新上传'], ['skip', '跳过']],
      };
      case 'sent': return { tone: 'success', text: '已发送', actions: [] };
      case 'cancelled': return { tone: 'muted', text: '已取消', actions: withCap([['retry', '重试']]) };
      case 'failed': return { tone: 'danger', text: '失败' + (job.error ? '：' + job.error : ''), actions: withCap([['retry', '重试'], ['skip', '跳过']]) };
      case 'skipped': return { tone: 'muted', text: '已跳过', actions: withCap([['retry', '重试']]) };
      case 'moving': return { tone: 'accent', text: '正在移动…', actions: [] };
      case 'moved': return { tone: 'success', text: '已移动到超限文件夹', actions: [] };
      case 'interrupted': {
        if (job.staged) {
          return { tone: 'warn', text: '已上传，继续时将随同组合并发送', bar: 1, actions: withCap([['cancel', '取消']]) };
        }
        const percent = job.size > 0 ? Math.floor((job.uploaded / job.size) * 100) : 0;
        return {
          tone: 'warn', actions: withCap([['cancel', '取消']]), bar: percent > 0 ? job.uploaded / job.size : null,
          text: percent > 0 ? `已暂停在 ${percent}%，继续时接着传` : '已中断，继续时重新上传',
        };
      }
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
    const groups = new Map();
    groupMembers.clear();
    pinnedGroups.clear();
    view = [];
    for (const id of order) {
      const job = jobs.get(id);
      if (!job) continue;
      if (job.groupId) {
        let list = groups.get(job.groupId);
        if (!list) { list = []; groups.set(job.groupId, list); }
        list.push(id);
        let mlist = groupMembers.get(job.groupId);
        if (!mlist) { mlist = []; groupMembers.set(job.groupId, mlist); }
        mlist.push(job);
        if (PINNED_STATES.has(job.state) || job.removing || (S && S.queue && S.queue.activeId === job.id)) {
          pinnedGroups.add(job.groupId);
        }
      }
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
    groupInfo.clear();
    groupCount = 0;
    for (const ids of groups.values()) {
      if (ids.length < 2) continue;
      groupCount++;
      ids.forEach((id, index) => groupInfo.set(id, { seq: groupCount, index: index + 1, total: ids.length }));
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
    const busy = S.move.active;
    const allSelected = view.length > 0 && view.every((id) => selected.has(id));
    $('selectAllBtn').textContent = allSelected ? '取消全选' : '全选';
    $('selectAllBtn').disabled = view.length === 0 || busy;

    $('resetBtn').disabled = counts.resettable.all === 0 || busy;
    $('resetCount').hidden = counts.resettable.all === 0;
    $('resetCount').textContent = String(counts.resettable.all);

    $('moveOversizeBtn').hidden = counts.oversize === 0;
    $('moveOversizeBtn').disabled = S.queue.running || busy || counts.oversize === 0;
    $('oversizeCount').textContent = String(counts.oversize);

    $('cleanBtn').disabled = order.length === 0 || busy;

    const bar = $('selectionBar');
    bar.hidden = selected.size === 0;
    if (selected.size) {
      $('selectionText').textContent = `已选择 ${selected.size} 项`;
      let selMovable = 0, selEditable = 0;
      for (const id of selected) {
        const j = jobs.get(id);
        if (isJobMovable(j)) selMovable++;
        if (isJobCaptionEditable(j)) selEditable++;
      }
      bar.querySelector('[data-act="edit-captions"]').disabled = busy || selEditable === 0;
      for (const act of ['move-up', 'move-down', 'move-top', 'move-bottom']) {
        bar.querySelector(`[data-act="${act}"]`).disabled = busy || selMovable === 0;
      }
      bar.querySelector('[data-act="reset-selected"]').disabled = busy || counts.resettable.selected === 0;
      bar.querySelector('[data-act="remove-selected"]').disabled = busy;
    }
    $('addBtn').disabled = S.scanning || busy;
    $('addBtnText').textContent = S.scanning ? '正在扫描…' : '添加视频';
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
          '把视频或文件夹拖进窗口，或点“添加视频”选择；按文件名顺序排队', '添加视频', 'add'))));
  }

  function buildRow() {
    const check = el('input', { type: 'checkbox', tabIndex: -1 });
    const grip = el('button', { class: 'row-grip', type: 'button', title: '拖动调整顺序', 'aria-label': '拖动调整顺序', text: '⋮⋮', tabIndex: -1 });
    const index = el('div', { class: 'row-index' });
    const name = el('div', { class: 'row-name' });
    const group = el('span', { class: 'row-group', hidden: true });
    const caption = el('span', { class: 'row-caption', hidden: true });
    const status = el('span', { class: 'row-status' });
    const size = el('span', { class: 'row-size' });
    const fill = el('i');
    const bar = el('div', { class: 'row-bar' }, fill);
    const actions = el('div', { class: 'row-actions' });
    const node = el('div', { class: 'row-item' },
      el('label', { class: 'row-check' }, check), grip, index,
      el('div', { class: 'row-body' }, name, el('div', { class: 'row-meta' }, group, caption, size, bar, status)), actions);
    node.parts = { check, grip, index, name, group, caption, status, size, bar, fill, actions };
    return node;
  }

  function paintRow(node, job, position) {
    const info = describe(job);
    const parts = node.parts;
    const g = groupInfo.get(job.id);
    const prevJob = position > 0 ? jobs.get(view[position - 1]) : null;
    const nextJob = position >= 0 && position + 1 < view.length ? jobs.get(view[position + 1]) : null;
    const inGroup = !!g;
    const groupStart = inGroup && (!prevJob || prevJob.groupId !== job.groupId);
    const groupEnd = inGroup && (!nextJob || nextJob.groupId !== job.groupId);
    const movable = isJobMovable(job) && !S.move.active;
    const isDragging = !!(dragging && dragging.moved && dragging.idSet.has(job.id));
    node.dataset.id = job.id;
    node.style.transform = `translateY(${position * ROW}px)`;
    node.className = 'row-item'
      + (info.tone ? ' tone-' + info.tone : '')
      + (selected.has(job.id) ? ' selected' : '')
      + (inGroup ? ' in-group' : '')
      + (groupStart ? ' group-start' : '')
      + (groupEnd ? ' group-end' : '')
      + (isDragging ? ' dragging' : '');
    parts.check.checked = selected.has(job.id);
    parts.check.disabled = S.move.active;
    parts.grip.disabled = !movable;
    parts.grip.classList.toggle('off', !movable);
    parts.grip.title = movable ? '按住拖动调整顺序' : '当前状态不能调整顺序';
    parts.index.textContent = order.length === view.length ? position + 1 : order.indexOf(job.id) + 1;
    if (parts.name.textContent !== job.name) { parts.name.textContent = job.name; parts.name.title = job.path; }
    parts.group.hidden = !g;
    if (g) parts.group.textContent = groupCount > 1 ? `合并 #${g.seq} · ${g.index}/${g.total}` : `合并 ${g.index}/${g.total}`;
    const showCaption = !!job.captionCustom && (!g || g.index === 1);
    parts.caption.hidden = !showCaption;
    if (showCaption) {
      const cap = job.customCaption || '';
      parts.caption.textContent = cap ? 'Caption: ' + cap.replace(/\s+/g, ' ') : '无 Caption';
      parts.caption.title = cap ? '自定义 Caption：\n' + cap : '已设为不带 Caption（留空）';
      parts.caption.classList.toggle('empty-cap', !cap);
    }
    parts.size.textContent = bytes(job.size);
    parts.status.textContent = info.text;
    parts.status.title = info.text;
    parts.bar.hidden = info.bar == null;
    if (info.bar != null) parts.fill.style.width = Math.min(100, info.bar * 100).toFixed(1) + '%';
    const signature = info.actions.map((action) => action[0]).join(',');
    if (node.dataset.actions !== signature) {
      node.dataset.actions = signature;
      parts.actions.replaceChildren(...info.actions.map(([act, label]) =>
        el('button', { class: 'btn small' + (act === 'cancel' || act === 'skip' || act === 'edit-caption' ? ' ghost' : ''), type: 'button', text: label, 'data-act': act })));
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
      start.textContent = q.pauseRequested ? '立即暂停' : '暂停';
      start.title = q.pauseRequested ? '正在等当前视频传完；点这里立即停下，之后接着传' : '立即停下，之后从停下的位置接着传';
      start.disabled = false;
      start.dataset.act = 'pause';
      start.classList.remove('primary');
    } else {
      start.textContent = q.paused ? '继续上传' : '开始上传';
      start.title = '';
      start.disabled = busy || counts.runnable === 0;
      start.dataset.act = 'start';
      start.classList.add('primary');
    }
    $('cancelAllBtn').hidden = !q.running;
    $('finishCurrentBtn').hidden = !(q.running && !q.pauseRequested);
    $('finishCurrentBtn').disabled = busy;
    $('totalBar').style.width = (q.totalBytes > 0 ? Math.min(100, (q.doneBytes / q.totalBytes) * 100) : 0).toFixed(2) + '%';
    $('summary').textContent = order.length
      ? `${bytes(q.doneBytes)} / ${bytes(q.totalBytes)} · 已完成 ${counts.completed} / ${order.length}`
      : '队列为空';
    let activity = '';
    if (q.running && q.speed > 0) activity = speed(q.speed) + (q.etaSeconds > 0 ? ' · 剩余约 ' + duration(q.etaSeconds) : '');
    else if (q.running && q.pauseRequested) activity = '传完当前视频后暂停';
    else if (q.running) activity = '正在准备…';
    else if (q.paused) activity = S.schedule.at && !S.schedule.due ? '已暂停，' + clock(S.schedule.at) + ' 继续' : '已暂停';
    else if (q.lastError) activity = q.lastError;
    $('activity').textContent = activity;
    $('activity').title = activity;

    const schedule = S.schedule;
    const button = $('scheduleBtn');
    button.classList.toggle('scheduled', schedule.at > 0);
    if (schedule.at > 0) {
      $('scheduleText').textContent = schedule.due ? '定时已到 · ' + (schedule.waiting || '') : clock(schedule.at) + (q.paused ? ' 继续' : ' 开始');
      button.title = '点击修改或取消定时';
      button.disabled = false;
    } else {
      $('scheduleText').textContent = '定时开始';
      button.title = q.running ? '现在暂停，到指定时间自动继续' : '在指定时间自动开始或继续上传（应用需保持打开）';
      button.disabled = busy || (!q.running && counts.runnable === 0);
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

  const STATE_LABEL = {
    queued: '等待上传', oversize: '超过上限', uploading: '正在上传', sending: '正在提交', confirming: '待确认',
    sent: '已发送', cancelled: '已取消', failed: '失败', skipped: '已跳过', moving: '正在移动', moved: '已移动', interrupted: '已中断',
  };

  async function pickAndAdd(method) {
    let scan;
    try { scan = await call(method); } catch (error) { toast('error', error.message); return; }
    if (scan && !scan.cancelled) chooseCandidates(scan);
  }

  // Shows what a scan found and lets the user decide what to queue. Files
  // that are already queued are never added silently: the user chooses
  // between skipping them and queueing them again.
  async function chooseCandidates(scan) {
    const ignored = scan.ignored ? `已忽略 ${scan.ignored} 个不是 MP4 的项目。` : '';
    if (!scan.candidates.length) {
      toast('info', ignored ? ignored.replace('。', '') + '，没有可添加的视频' : '没有找到 MP4 视频（文件夹只看顶层，不进子文件夹）');
      return;
    }
    const items = scan.candidates;
    const duplicates = items.filter((item) => item.duplicate);
    const replaceable = duplicates.filter((item) => item.replaceable).length;
    const similar = items.filter((item) => item.similar).length;
    const folders = new Set(items.map((item) => item.folder));
    const MAX_ALBUM = 10;
    let replace = false;
    let merge = false;
    // Probable copies start unchecked; exact duplicates follow the choice below.
    const picked = items.map((item) => !item.duplicate && !item.similar);

    const result = await openModal((box, close) => {
      const count = el('span', { class: 'hint spacer' });
      const add = el('button', { class: 'btn primary', text: '加入队列' });
      const list = el('div', { class: 'cand-list' });
      const boxes = [];
      let modeHint = null;
      const selectable = (item) => (item.duplicate ? replace && item.replaceable : true);
      const refresh = () => {
        const total = picked.filter(Boolean).length;
        count.textContent = `已选 ${total} / ${items.length}`;
        add.disabled = total === 0;
        items.forEach((item, index) => {
          boxes[index].disabled = !selectable(item);
          boxes[index].checked = picked[index];
          boxes[index].closest('.cand').classList.toggle('off', !selectable(item));
        });
        if (modeHint) {
          const chosen = items.filter((item, index) => picked[index] && selectable(item));
          const uploadable = chosen.filter((item) => !item.oversize).length;
          const oversize = chosen.length - uploadable;
          if (!merge) {
            modeHint.textContent = uploadable > 0
              ? `每个视频各发一条消息（共 ${uploadable} 条），Caption 为文件名。`
              : '每个视频各发一条消息，Caption 为文件名。';
          } else if (uploadable < 2) {
            modeHint.textContent = uploadable === 1
              ? '当前只选了 1 个可上传视频，会单独发成一条消息；超限的视频不会参与合并。'
              : '至少选择 2 个不超过上限的视频才能合并发送。';
          } else {
            const groups = Math.floor(uploadable / MAX_ALBUM);
            const rem = uploadable % MAX_ALBUM;
            const fullGroups = groups + (rem >= 2 ? 1 : 0);
            const single = rem === 1 ? 1 : 0;
            let summary = `${uploadable} 个视频将按顺序合并为 ${fullGroups} 条消息`;
            if (single) summary += '，最后 1 个单独发送';
            summary += `（每条最多 ${MAX_ALBUM} 个，默认 Caption 为同组文件名列表，加入队列后可自定义）`;
            if (oversize > 0) summary += `；另有 ${oversize} 个超限视频不参与合并`;
            modeHint.textContent = summary + '。';
          }
        }
      };
      items.forEach((item, index) => {
        const check = el('input', { type: 'checkbox', onchange: () => { picked[index] = check.checked; refresh(); } });
        boxes.push(check);
        let note = '', warn = false;
        if (item.duplicate) {
          note = ' · 已在队列（' + (STATE_LABEL[item.duplicateState] || item.duplicateState) + '）' + (item.replaceable ? '' : '，处理中不能替换');
          warn = true;
        } else if (item.similar) { note = ' · 疑似重复：队列里有同名同大小的视频'; warn = true; }
        else if (item.oversize) { note = ' · 超过上限'; warn = true; }
        list.append(el('label', { class: 'cand' }, check,
          el('span', { class: 'name', title: item.folder + ' / ' + item.name },
            el('span', { text: item.name }),
            folders.size > 1 ? el('span', { class: 'folder', text: item.folder.split(/[\\/]/).pop() }) : null),
          el('span', { class: 'meta' + (warn ? ' warn' : ''), text: bytes(item.size) + note })));
      });
      const setAll = (value) => {
        items.forEach((item, index) => { if (selectable(item)) picked[index] = value; });
        refresh();
      };

      let duplicateBox = null;
      if (duplicates.length) {
        const modes = el('div', { class: 'segmented' });
        const hint = el('div', { class: 'hint' });
        const setMode = (value) => {
          replace = value;
          items.forEach((item, index) => { if (item.duplicate) picked[index] = replace && item.replaceable; });
          modes.children[0].classList.toggle('active', !replace);
          modes.children[1].classList.toggle('active', replace);
          hint.textContent = replace
            ? '队列里的旧条目会被删除，这些视频作为新的待上传条目排到队尾。已经发送过的会再发一次，频道里会出现重复消息。'
            : '保持队列里现有的条目不变，只添加新的视频。';
          refresh();
        };
        modes.append(
          el('button', { type: 'button', text: '跳过重复的', onclick: () => setMode(false) }),
          el('button', { type: 'button', text: `重新加入队列（${replaceable}）`, disabled: replaceable === 0, onclick: () => setMode(true) }));
        duplicateBox = el('div', { class: 'dup-box' },
          el('div', {}, el('b', { text: `${duplicates.length} 个视频已经在队列里。` }), '要怎么处理？'), modes, hint);
        queueMicrotask(() => setMode(false));
      }

      let modeBox = null;
      if (items.length >= 2) {
        const sendModes = el('div', { class: 'segmented' });
        modeHint = el('div', { class: 'hint' });
        const setSendMode = (value) => {
          merge = value;
          sendModes.children[0].classList.toggle('active', !merge);
          sendModes.children[1].classList.toggle('active', merge);
          refresh();
        };
        const singleBtn = el('button', { type: 'button', class: 'active', text: '单独发送', onclick: () => setSendMode(false) });
        const mergeBtn = el('button', { type: 'button', text: `合并发送（最多 ${MAX_ALBUM} 个 / 条）`, onclick: () => setSendMode(true) });
        sendModes.append(singleBtn, mergeBtn);
        modeBox = el('div', { class: 'mode-box' },
          el('div', { class: 'mode-head' }, el('b', { text: '发送方式' }), sendModes),
          modeHint);
      }

      add.onclick = () => close({ indexes: picked.flatMap((value, index) => (value ? [index] : [])), replace, merge });
      box.append(
        el('div', { class: 'modal-head', text: '选择要加入队列的视频' }),
        el('div', { class: 'modal-body plain' },
          el('div', { class: 'hint', text: scan.source + (ignored ? ' · ' + ignored : ''), title: scan.source }),
          duplicateBox,
          modeBox,
          similar ? el('div', { class: 'hint', text: `${similar} 个疑似重复的视频默认没有勾选，确认不是同一个再勾上。` }) : null,
          el('div', { class: 'quick' },
            el('button', { class: 'btn small', type: 'button', text: '全选', onclick: () => setAll(true) }),
            el('button', { class: 'btn small', type: 'button', text: '全不选', onclick: () => setAll(false) })),
          list),
        el('div', { class: 'modal-foot' }, count,
          el('button', { class: 'btn', text: '取消', onclick: () => close(null) }), add));
      refresh();
    }, { wide: true });
    if (result) await run('addCandidates', { token: scan.token, indexes: result.indexes, replaceDuplicates: result.replace, merge: !!result.merge });
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
          el('p', { text: (S.queue.running
            ? '上传会立即暂停，到点后自动继续（正在传的视频尽量从停下的位置接着传）。'
            : S.queue.paused ? '到点后自动继续上传。' : '到点后自动开始上传。')
            + '应用需要保持打开；退出后计划会保留，下次打开时恢复。' }),
          el('div', { class: 'quick' }, ...quick.map(([label, make]) =>
            el('button', { class: 'btn small', type: 'button', text: label, onclick: () => { input.value = local(make()); error.textContent = ''; } }))),
          input, error),
        el('div', { class: 'modal-foot' },
          current ? el('button', { class: 'btn danger spacer', text: '取消定时', onclick: () => close({ cancel: true }) }) : null,
          el('button', { class: 'btn', text: '关闭', onclick: () => close(null) }),
          el('button', { class: 'btn primary', text: current ? '修改时间' : S.queue.running ? '暂停并定时' : '设置定时', onclick: submit })));
    });
    if (!result) return;
    if (result.cancel) { if (await run('cancelSchedule')) toast('info', '已取消定时'); return; }
    if (await run('setSchedule', { unix: result.unix })) toast('info', '已设置：' + clock(result.unix * 1000) + ' 自动开始');
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

  async function reorderSelected(action) {
    const ids = order.filter((id) => selected.has(id) && isJobMovable(jobs.get(id)));
    if (!ids.length) return;
    await run('reorderJobs', { ids, action, targetId: '' });
  }

  function buildCaptionTargets(ids) {
    const idSet = new Set(ids);
    const seenGroups = new Set();
    const targets = [];
    for (const id of order) {
      if (!idSet.has(id)) continue;
      const job = jobs.get(id);
      if (!job || !isJobCaptionEditable(job)) continue;
      const members = job.groupId ? (groupMembers.get(job.groupId) || [job]) : [job];
      if (job.groupId && members.length >= 2) {
        if (seenGroups.has(job.groupId)) continue;
        seenGroups.add(job.groupId);
        const activeMembers = members.filter((m) => m.state === 'queued' || m.state === 'interrupted');
        const basis = activeMembers.length ? activeMembers : members;
        const defaultCaption = utf16Truncate(basis.map((m) => defaultNameCaption(m.name)).filter(Boolean).join('\n'), MAX_CAPTION);
        const customMember = members.find((m) => m.captionCustom);
        targets.push({
          editId: members[0].id,
          title: `合并组（${members.length} 个视频）`,
          subtitle: members.map((m) => m.name).slice(0, 3).join(' / ') + (members.length > 3 ? ` 等 ${members.length} 个` : ''),
          defaultCaption,
          initialCustom: !!customMember,
          currentCaption: customMember ? (customMember.customCaption || '') : defaultCaption,
          isGroup: true,
        });
      } else {
        const defaultCaption = utf16Truncate(defaultNameCaption(job.name), MAX_CAPTION);
        targets.push({
          editId: job.id,
          title: job.name,
          subtitle: '',
          defaultCaption,
          initialCustom: !!job.captionCustom,
          currentCaption: job.captionCustom ? (job.customCaption || '') : defaultCaption,
          isGroup: false,
        });
      }
    }
    return targets;
  }

  async function captionDialog(ids) {
    const targets = buildCaptionTargets(ids);
    if (!targets.length) {
      toast('info', '所选任务当前不能修改 Caption（上传中、待确认或已完成）');
      return;
    }

    if (targets.length === 1) {
      const target = targets[0];
      const result = await openModal((box, close) => {
        const area = el('textarea', {
          class: 'input caption-input', rows: 5, value: target.currentCaption,
          placeholder: '留空表示发送时不带 Caption', autofocus: true,
        });
        const counter = el('div', { class: 'caption-counter' });
        const save = el('button', { class: 'btn primary', type: 'button', text: '保存' });
        const sync = () => {
          const len = area.value.length;
          counter.textContent = `${len} / ${MAX_CAPTION}`;
          counter.classList.toggle('over', len > MAX_CAPTION);
          save.disabled = len > MAX_CAPTION;
        };
        area.addEventListener('input', sync);
        save.onclick = () => close({ edits: [{ id: target.editId, custom: true, caption: area.value }] });
        const note = target.isGroup
          ? '合并发送时，这条 Caption 会显示在频道聊天里这条媒体组气泡的正下方；留空表示不带 Caption。'
          : '留空表示不带 Caption；点击“恢复默认”会重新使用文件名。';
        box.append(
          el('div', { class: 'modal-head', text: '编辑 Caption' }),
          el('div', { class: 'modal-body plain caption-modal' },
            el('div', { class: 'strong', text: target.title }),
            target.subtitle ? el('div', { class: 'hint', text: target.subtitle }) : null,
            el('p', { class: 'hint', text: note }),
            area,
            el('div', { class: 'caption-bar' },
              el('span', { class: 'hint', text: '留空即无 Caption' }),
              counter)),
          el('div', { class: 'modal-foot' },
            el('button', { class: 'btn ghost spacer', type: 'button', text: '恢复默认', onclick: () => close({ edits: [{ id: target.editId, custom: false, caption: '' }] }) }),
            el('button', { class: 'btn', type: 'button', text: '取消', onclick: () => close(null) }),
            save));
        sync();
      });
      if (result) await run('setCaptions', { edits: result.edits });
      return;
    }

    const result = await openModal((box, close) => {
      let mode = 'template';
      const modes = el('div', { class: 'segmented' });
      const tplPane = el('div', { class: 'cap-pane' });
      const trPane = el('div', { class: 'cap-pane', hidden: true });
      const indPane = el('div', { class: 'cap-pane', hidden: true });
      const applyBtn = el('button', { class: 'btn primary', type: 'button', text: `应用到 ${targets.length} 条消息` });

      const tplArea = el('textarea', {
        class: 'input caption-input', rows: 3, autofocus: true,
        placeholder: '输入统一的 Caption，支持 {name}、{index}、{total}；留空表示全部不带 Caption',
      });
      const insertToken = (token) => {
        const start = tplArea.selectionStart ?? tplArea.value.length;
        const end = tplArea.selectionEnd ?? tplArea.value.length;
        tplArea.value = tplArea.value.slice(0, start) + token + tplArea.value.slice(end);
        tplArea.focus();
        tplArea.selectionStart = tplArea.selectionEnd = start + token.length;
        sync();
      };
      const tplPreview = el('div', { class: 'cap-preview' });
      tplPane.append(
        tplArea,
        el('div', { class: 'quick' },
          el('button', { class: 'btn small', type: 'button', text: '插入 {name}（默认名）', onclick: () => insertToken('{name}') }),
          el('button', { class: 'btn small', type: 'button', text: '插入 {index}（序号）', onclick: () => insertToken('{index}') }),
          el('button', { class: 'btn small', type: 'button', text: '插入 {total}（总数）', onclick: () => insertToken('{total}') }),
          el('button', { class: 'btn small ghost', type: 'button', text: '清空（无 Caption）', onclick: () => { tplArea.value = ''; sync(); } })),
        tplPreview);

      const prefixIn = el('input', { class: 'input', placeholder: '例如：【合集】 ' });
      const suffixIn = el('input', { class: 'input', placeholder: '例如： #归档' });
      const findIn = el('input', { class: 'input', placeholder: '要查找替换的文字' });
      const replaceIn = el('input', { class: 'input', placeholder: '替换为（留空即删除）' });
      const trPreview = el('div', { class: 'cap-preview' });
      trPane.append(
        el('div', { class: 'field-row' },
          el('label', { class: 'field' }, el('span', { text: '添加前缀' }), prefixIn),
          el('label', { class: 'field' }, el('span', { text: '添加后缀' }), suffixIn)),
        el('div', { class: 'field-row' },
          el('label', { class: 'field' }, el('span', { text: '查找文本' }), findIn),
          el('label', { class: 'field' }, el('span', { text: '替换为' }), replaceIn)),
        trPreview);

      const indList = el('div', { class: 'cap-batch-list' });
      const indRows = targets.map((t, idx) => {
        const state = { useDefault: !t.initialCustom };
        const area = el('textarea', { class: 'input caption-mini', rows: 2, value: t.currentCaption, placeholder: '留空表示不带 Caption' });
        const countEl = el('span', { class: 'caption-counter' });
        const resetBtn = el('button', {
          class: 'btn small ghost', type: 'button', text: '恢复默认',
          onclick: () => { state.useDefault = true; area.value = t.defaultCaption; sync(); },
        });
        area.addEventListener('input', () => { state.useDefault = false; sync(); });
        indList.append(el('div', { class: 'cap-row' },
          el('div', { class: 'cap-row-head' },
            el('span', { class: 'strong', text: `${idx + 1}. ${t.title}`, title: t.subtitle || t.title }),
            countEl, resetBtn),
          area));
        state.area = area;
        state.countEl = countEl;
        return state;
      });
      indPane.append(indList);

      const computeEdits = () => {
        if (mode === 'template') {
          const tpl = tplArea.value;
          return targets.map((t, i) => ({
            id: t.editId,
            custom: true,
            caption: tpl.replace(/\{name\}/g, t.defaultCaption).replace(/\{index\}/g, String(i + 1)).replace(/\{total\}/g, String(targets.length)),
          }));
        }
        if (mode === 'transform') {
          const pre = prefixIn.value, suf = suffixIn.value, find = findIn.value, rep = replaceIn.value;
          return targets.map((t) => {
            let text = t.currentCaption;
            if (find) text = text.split(find).join(rep);
            return { id: t.editId, custom: true, caption: pre + text + suf };
          });
        }
        return targets.map((t, i) => (indRows[i].useDefault
          ? { id: t.editId, custom: false, caption: '' }
          : { id: t.editId, custom: true, caption: indRows[i].area.value }));
      };

      const paintPreview = (container, edits) => {
        container.replaceChildren(...edits.map((ed, i) => {
          const len = ed.caption.length;
          const over = len > MAX_CAPTION;
          return el('div', { class: 'cap-prev-item' + (over ? ' over' : '') },
            el('div', { class: 'cap-prev-head' },
              el('span', { class: 'strong', text: `${i + 1}. ${targets[i].title}` }),
              el('span', { class: 'caption-counter' + (over ? ' over' : ''), text: `${len} / ${MAX_CAPTION}` })),
            el('div', { class: 'cap-prev-text' + (!ed.caption ? ' empty-cap' : ''), text: ed.caption || '（留空，无 Caption）' }));
        }));
      };

      const sync = () => {
        const edits = computeEdits();
        let anyOver = false;
        for (const ed of edits) if (ed.custom && ed.caption.length > MAX_CAPTION) anyOver = true;
        applyBtn.disabled = anyOver;
        if (mode === 'template') paintPreview(tplPreview, edits);
        else if (mode === 'transform') paintPreview(trPreview, edits);
        else {
          indRows.forEach((row) => {
            const len = row.area.value.length;
            row.countEl.textContent = `${len} / ${MAX_CAPTION}`;
            row.countEl.classList.toggle('over', len > MAX_CAPTION);
          });
        }
      };

      [tplArea, prefixIn, suffixIn, findIn, replaceIn].forEach((input) => input.addEventListener('input', sync));
      const modeDefs = [['template', '统一设置'], ['transform', '前后缀 / 替换'], ['individual', '逐条编辑']];
      const switchMode = (next) => {
        mode = next;
        modeDefs.forEach(([key], idx) => modes.children[idx].classList.toggle('active', key === mode));
        tplPane.hidden = mode !== 'template';
        trPane.hidden = mode !== 'transform';
        indPane.hidden = mode !== 'individual';
        sync();
      };
      modes.append(...modeDefs.map(([key, label]) => el('button', { type: 'button', text: label, onclick: () => switchMode(key) })));
      applyBtn.onclick = () => close({ edits: computeEdits() });

      box.append(
        el('div', { class: 'modal-head', text: `批量编辑 Caption（${targets.length} 条消息）` }),
        el('div', { class: 'modal-body plain caption-modal' },
          el('p', { class: 'hint', text: '支持统一模板、批量添加前后缀/查找替换，或逐条修改；合并发送的视频组会作为一个整体设置。' }),
          modes,
          tplPane, trPane, indPane),
        el('div', { class: 'modal-foot' },
          el('button', {
            class: 'btn ghost spacer', type: 'button', text: '全部恢复默认',
            onclick: () => close({ edits: targets.map((t) => ({ id: t.editId, custom: false, caption: '' })) }),
          }),
          el('button', { class: 'btn', type: 'button', text: '取消', onclick: () => close(null) }),
          applyBtn));
      switchMode('template');
    }, { wide: true });
    if (result) await run('setCaptions', { edits: result.edits });
  }

  const actions = {
    settings: () => openSettings(),
    bind: bindChannel,
    // Deferred: the click that triggered this would close the menu again.
    add: () => setTimeout(() => { $('addMenu').hidden = false; }),
    'add-files': () => { $('addMenu').hidden = true; pickAndAdd('addFiles'); },
    'add-folder': () => { $('addMenu').hidden = true; pickAndAdd('addFolder'); },
    'toggle-select-all': () => {
      const allSelected = view.length > 0 && view.every((id) => selected.has(id));
      if (allSelected) {
        for (const id of view) selected.delete(id);
      } else {
        for (const id of view) selected.add(id);
      }
      recompute();
    },
    'reset-dialog': resetDialog,
    'move-oversize': () => run('moveOversize'),
    'pause-after': () => run('pause', { finishCurrent: true }),
    'edit-captions': () => captionDialog(order.filter((id) => selected.has(id))),
    'move-up': () => reorderSelected('up'),
    'move-down': () => reorderSelected('down'),
    'move-top': () => reorderSelected('top'),
    'move-bottom': () => reorderSelected('bottom'),
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
      case 'edit-caption': return captionDialog([id]);
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

  function buildCleanMenu() {
    const menu = $('cleanMenu');
    const item = (label, count, handler, danger) => el('button', {
      class: 'menu-item' + (danger ? ' danger' : ''), type: 'button', disabled: !count || S.move.active,
      onclick: () => { menu.hidden = true; handler(); },
    }, el('span', { text: label }), typeof count === 'number' ? el('span', { class: 'count', text: String(count) }) : null);
    menu.replaceChildren(
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
      case 'scan': chooseCandidates(payload); break;
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
      for (const [popover, anchor] of [['limitPopover', 'limitChip'], ['cleanMenu', 'cleanBtn'], ['addMenu', 'addBtn']]) {
        if (!$(popover).hidden && !event.target.closest('#' + popover) && !event.target.closest('#' + anchor)) $(popover).hidden = true;
      }
    });
    $('connChip').onclick = () => openSettings('secConnection');
    $('settingsBtn').onclick = () => openSettings();
    $('settingsClose').onclick = closeSettings;
    $('scrim').onclick = closeSettings;
    $('updateBadge').onclick = () => openSettings('secUpdate');
    $('limitChip').onclick = () => { buildLimitMenu(); $('limitPopover').hidden = !$('limitPopover').hidden; };
    $('cleanBtn').onclick = () => { buildCleanMenu(); $('cleanMenu').hidden = !$('cleanMenu').hidden; };
    $('addBtn').onclick = () => { $('addMenu').hidden = !$('addMenu').hidden; };
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
    dropLine = el('div', { id: 'dropLine', class: 'drop-line', hidden: true });
    $('listSpacer').append(dropLine);
    list.addEventListener('scroll', () => renderAll(), { passive: true });
    new ResizeObserver(() => renderAll()).observe(list);
    list.addEventListener('click', (event) => {
      const row = event.target.closest('.row-item');
      if (!row || event.target.closest('[data-act]') || event.target.closest('.row-grip') || S.move.active) return;
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
      if (job && !event.target.closest('[data-act]') && !event.target.closest('.row-grip')) copyText(job.path);
    });

    const stopDrag = (commit) => {
      if (!dragging) return;
      const info = dragging;
      dragging = null;
      if (info.raf) cancelAnimationFrame(info.raf);
      if (dropLine) dropLine.hidden = true;
      window.removeEventListener('pointermove', onDragMove);
      window.removeEventListener('pointerup', onDragEnd);
      window.removeEventListener('pointercancel', onDragEnd);
      document.body.classList.remove('reordering');
      renderAll();
      if (commit && info.moved && info.targetId) {
        run('reorderJobs', { ids: info.ids, action: info.after ? 'after' : 'before', targetId: info.targetId });
      }
    };
    const updateDragTarget = (clientY) => {
      if (!dragging || !view.length) return;
      const rect = list.getBoundingClientRect();
      const yInSpacer = Math.max(0, Math.min(view.length * ROW - 1, clientY - rect.top + list.scrollTop));
      const idx = Math.max(0, Math.min(view.length - 1, Math.floor(yInSpacer / ROW)));
      const after = (yInSpacer - idx * ROW) >= ROW / 2;
      dragging.targetId = view[idx];
      dragging.after = after;
      dropLine.style.top = ((after ? idx + 1 : idx) * ROW) + 'px';
      dropLine.hidden = false;
    };
    const dragScrollStep = () => {
      if (!dragging) return;
      const rect = list.getBoundingClientRect();
      let delta = 0;
      if (dragging.lastY < rect.top + 44) delta = -Math.max(4, Math.round((rect.top + 44 - dragging.lastY) / 3));
      else if (dragging.lastY > rect.bottom - 44) delta = Math.max(4, Math.round((dragging.lastY - (rect.bottom - 44)) / 3));
      if (delta !== 0) {
        list.scrollTop += delta;
        updateDragTarget(dragging.lastY);
        dragging.raf = requestAnimationFrame(dragScrollStep);
      } else {
        dragging.raf = 0;
      }
    };
    const onDragMove = (event) => {
      if (!dragging || event.pointerId !== dragging.pointerId) return;
      dragging.lastY = event.clientY;
      if (!dragging.moved && Math.abs(event.clientY - dragging.startY) >= 4) {
        dragging.moved = true;
        document.body.classList.add('reordering');
        renderAll();
      }
      if (!dragging.moved) return;
      updateDragTarget(event.clientY);
      if (!dragging.raf) dragging.raf = requestAnimationFrame(dragScrollStep);
    };
    const onDragEnd = (event) => {
      if (!dragging || event.pointerId !== dragging.pointerId) return;
      stopDrag(event.type === 'pointerup');
    };
    list.addEventListener('pointerdown', (event) => {
      if (event.button !== 0 || S.move.active) return;
      const grip = event.target.closest('.row-grip');
      if (!grip || grip.disabled) return;
      const row = grip.closest('.row-item');
      const dragId = row && row.dataset.id;
      const job = dragId && jobs.get(dragId);
      if (!job || !isJobMovable(job) || view.length < 2) return;
      const ids = selected.has(dragId)
        ? order.filter((id) => selected.has(id) && isJobMovable(jobs.get(id)))
        : [dragId];
      if (!ids.length) return;
      event.preventDefault();
      dragging = {
        pointerId: event.pointerId,
        ids,
        idSet: new Set(ids),
        startY: event.clientY,
        lastY: event.clientY,
        moved: false,
        targetId: null,
        after: false,
        raf: 0,
      };
      window.addEventListener('pointermove', onDragMove);
      window.addEventListener('pointerup', onDragEnd);
      window.addEventListener('pointercancel', onDragEnd);
    });

    document.addEventListener('keydown', (event) => {
      const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(event.target.tagName);
      if (event.key === 'Escape') {
        if (dragging) { stopDrag(false); return; }
        if ($('modalRoot').children.length) return;
        if (!$('limitPopover').hidden || !$('cleanMenu').hidden || !$('addMenu').hidden) { $('limitPopover').hidden = true; $('cleanMenu').hidden = true; $('addMenu').hidden = true; }
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
      } else if (event.altKey && (event.key === 'ArrowUp' || event.key === 'ArrowDown') && selected.size && !S.move.active) {
        event.preventDefault();
        reorderSelected(event.key === 'ArrowUp' ? 'up' : 'down');
      }
    });
    // The page is an application, not a document: no browser context menu
    // outside text fields, and no file drops navigating away.
    document.addEventListener('contextmenu', (event) => { if (!/^(INPUT|TEXTAREA)$/.test(event.target.tagName)) event.preventDefault(); });
    // Dropped files: the page only shows the hint. The real paths reach Go
    // through the window (macOS reads the drag pasteboard, Windows receives
    // the file handles), and Go answers with a "scan" event.
    let dragDepth = 0;
    const carriesFiles = (event) => [...(event.dataTransfer?.types || [])].includes('Files');
    const endDrag = () => { dragDepth = 0; $('dropHint').hidden = true; };
    document.addEventListener('dragenter', (event) => {
      if (!carriesFiles(event)) return;
      event.preventDefault();
      dragDepth++;
      $('dropHint').hidden = false;
    });
    document.addEventListener('dragleave', () => { if (--dragDepth <= 0) endDrag(); });
    document.addEventListener('dragover', (event) => {
      event.preventDefault();
      if (event.dataTransfer) event.dataTransfer.dropEffect = carriesFiles(event) ? 'copy' : 'none';
    });
    document.addEventListener('drop', (event) => {
      event.preventDefault();
      const files = carriesFiles(event);
      endDrag();
      if (!files) return;
      if (typeof window.__tvuDropFiles === 'function') {
        if (!window.__tvuDropFiles(event.dataTransfer.files)) toast('error', '系统的 WebView2 版本太旧，不支持拖入；请用“添加视频”按钮');
      } else if (!native) {
        toast('info', '浏览器预览拿不到文件路径；请在应用窗口里拖入');
      }
    });
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
