'use strict';
// MAIRDI Загрузчик — интерфейс окна.
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function api(path, body) {
  const r = await fetch('/api/' + path, {
    method: 'POST', headers: { 'X-Token': window.TOKEN, 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  if (!r.ok) throw new Error('HTTP ' + r.status);
  return r.json();
}

const CAT_ICON = {
  'Прайсы': '🏷️', 'Документы': '📄', 'Дистрибутивы': '💿', 'Программирование': '⌨️', 'Видео': '🎬', 'Музыка': '🎵',
  'Изображения': '🖼️', 'Книги': '📚', 'Архивы': '🗜️', 'Торренты': '🧲', 'Прочее': '📦',
};
const STATUS = {
  active: 'Загрузка', waiting: 'В очереди', paused: 'Пауза', done: 'Готово', error: 'Ошибка', retrying: 'Повтор…',
  seeding: 'Раздача', metadata: 'Данные торрента…', resolving: 'Ищу видео…', merging: 'Склейка…', moving: 'Раскладываю…',
  duplicate: 'Уже скачан',
};
const ACTIVE_ST = new Set(['active', 'waiting', 'paused', 'retrying', 'metadata', 'resolving', 'merging', 'moving']);

let S = null;              // состояние с сервера
let jobs = [];
let view = 'all';          // all | active | done | error | dup | cat:<Категория>
let sel = new Set();
let anchor = null;
let sortKey = 'added', sortAsc = false;
let lastPrompt = 0;
let detailTab = 'info';

// ---------- форматирование ----------
function size(b) {
  if (!b || b < 0) return '';
  const u = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ'];
  let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i ? b.toFixed(b < 10 ? 2 : b < 100 ? 1 : 0) : b) + ' ' + u[i];
}
const speed = b => b > 0 ? size(b) + '/с' : '';
function eta(s) {
  if (!s || s <= 0) return '';
  const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), c = s % 60;
  return h ? `${h} ч ${m} мин` : m ? `${m} мин ${c} с` : `${c} с`;
}
function when(t) {
  if (!t || t.startsWith('0001')) return '';
  const d = new Date(t), now = new Date();
  const hm = d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
  if (d.toDateString() === now.toDateString()) return 'сегодня ' + hm;
  return d.toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit', year: '2-digit' }) + ' ' + hm;
}
function fileIcon(j) {
  if (j.kind === 'upload') return '⇪';
  if (j.kind === 'torrent') return '🧲';
  if (j.kind === 'video') return j.quality === 'audio' ? '🎵' : '🎬';
  return CAT_ICON[j.category] || '📄';
}
const pct = j => j.total > 0 ? Math.min(100, j.done * 100 / j.total) : 0;

function toast(msg, kind = '', action) {
  const t = document.createElement('div');
  t.className = 'toast ' + kind; t.textContent = msg;
  if (action) {
    const b = document.createElement('button'); b.type = 'button'; b.className = 'link toast-act'; b.textContent = action[0];
    b.onclick = () => { t.remove(); action[1](); };
    t.appendChild(b);
  }
  $('#toasts').appendChild(t);
  setTimeout(() => t.remove(), action ? 9000 : 5000);
}

// ---------- фильтр и сортировка ----------
function visible() {
  const q = $('#search').value.trim().toLowerCase();
  let list = jobs.filter(j => {
    switch (view) {
      case 'active': if (!ACTIVE_ST.has(j.status)) return false; break;
      case 'done': if (j.status !== 'done' && j.status !== 'seeding') return false; break;
      case 'error': if (j.status !== 'error') return false; break;
      case 'dup': if (j.status !== 'duplicate') return false; break;
      case 'up': if (j.kind !== 'upload') return false; break;
      default: if (view.startsWith('cat:') && j.category !== view.slice(4)) return false;
    }
    if (q && !`${j.name} ${j.domain} ${j.dir} ${j.url}`.toLowerCase().includes(q)) return false;
    return true;
  });
  const key = {
    name: j => (j.name || '').toLowerCase(), total: j => j.total, progress: pct, speed: j => j.speed,
    eta: j => j.eta || 1e12, connections: j => j.connections, dir: j => j.dir || '', added: j => j.added,
  }[sortKey] || (j => j.added);
  list.sort((a, b) => {
    // активные всегда сверху, как в классических менеджерах загрузок
    const aa = ACTIVE_ST.has(a.status) ? 0 : 1, bb = ACTIVE_ST.has(b.status) ? 0 : 1;
    if (sortKey === 'added' && aa !== bb) return aa - bb;
    const x = key(a), y = key(b);
    return (x < y ? -1 : x > y ? 1 : 0) * (sortAsc ? 1 : -1);
  });
  return list;
}

// ---------- отрисовка ----------
function renderTree() {
  const c = { all: jobs.length, active: 0, done: 0, error: 0, dup: 0, up: 0 };
  const cats = {};
  for (const j of jobs) {
    if (ACTIVE_ST.has(j.status)) c.active++;
    if (j.status === 'done' || j.status === 'seeding') c.done++;
    if (j.status === 'error') c.error++;
    if (j.status === 'duplicate') c.dup++;
    if (j.kind === 'upload') c.up++;
    if (j.category) cats[j.category] = (cats[j.category] || 0) + 1;
  }
  const item = (key, ic, title, n) =>
    `<div class="item ${view === key ? 'on' : ''}" data-view="${esc(key)}"><span class="ic">${ic}</span><span>${esc(title)}</span>${n ? `<span class="n">${n}</span>` : ''}</div>`;
  let h = item('all', '🗃️', 'Все закачки', c.all) + item('active', '⬇️', 'Загружаются', c.active) +
    item('done', '✅', 'Завершённые', c.done) + item('error', '⚠️', 'Ошибки', c.error) + item('dup', '♻️', 'Уже было скачано', c.dup) +
    (c.up ? item('up', '⇪', 'Отправлено на Диск', c.up) : '');
  h += '<div class="group">Папки</div>';
  for (const cat of (S?.categories || [])) h += item('cat:' + cat, CAT_ICON[cat] || '📁', cat, cats[cat]);
  h += '<div class="group">Google Диск</div>';
  for (const a of (S?.g?.accounts || [])) {
    const key = 'gd:' + a.email;
    const ava = a.photo ? `<img class="ava" src="${esc(a.photo)}" alt="" referrerpolicy="no-referrer">` : '☁️';
    h += `<div class="item ${view === key ? 'on' : ''}" data-view="${esc(key)}" data-acc="${esc(a.email)}" title="${esc(a.email)}"><span class="ic">${ava}</span><span class="acc">${esc(a.name || a.email)}</span></div>`;
  }
  const waiting = S?.g?.login === 'wait';
  h += `<div class="item add" data-view="gd-add"><span class="ic">${waiting ? '⏳' : '＋'}</span><span>${waiting ? 'Вход в Google…' : (S?.g?.accounts?.length ? 'Добавить аккаунт' : 'Подключить Google Диск')}</span></div>`;
  $('#tree').innerHTML = h;
}

function progCell(j) {
  const p = pct(j);
  let cls = '', label = STATUS[j.status] || j.status;
  if (j.status === 'active' || j.status === 'waiting') label = j.total > 0 ? p.toFixed(1) + '%' : STATUS[j.status];
  if (j.status === 'paused' && j.total > 0) label = 'Пауза · ' + p.toFixed(0) + '%';
  if (j.status === 'done') cls = 'done';
  if (j.status === 'seeding') { cls = 'seed'; label = 'Раздача'; }
  if (j.status === 'error') cls = 'err';
  if (j.status === 'paused') cls = 'paused';
  if (j.status === 'duplicate') cls = 'dup';
  const busy = (j.status === 'resolving' || j.status === 'metadata' || j.status === 'merging' || j.status === 'moving' ||
    (j.status === 'active' && !(j.total > 0)));
  const w = (j.status === 'done' || j.status === 'seeding' || j.status === 'duplicate' || j.status === 'error') ? 100 : p;
  return `<div class="prog ${cls} ${busy ? 'busy' : ''}"><div style="width:${w}%"></div><span>${esc(label)}</span></div>`;
}

// «Скачать в Google Диск»: где файл на Диске и как идёт перенос
function driveWhere(j) {
  if (j.driveParent) return j.driveParentName || 'папка на Диске';
  return ['Мой диск', ...(j.drivePath || '').split('/').filter(Boolean)].join(' › ');
}
function driveText(j, short) {
  if (!j.toDrive) return '';
  if (short && j.driveStep === 'done') return j.localGone ? 'На Google Диске · копии на компьютере нет' : 'Загружено и на Google Диск';
  if (short && j.driveStep === 'error') return 'Не загрузилось на Диск: ' + (j.driveErr || 'ошибка');
  const n = j.driveTotal > 1 ? ` ${j.driveDone || 0} из ${j.driveTotal}` : '';
  switch (j.driveStep) {
    case '': case undefined: return 'После скачивания уйдёт на Google Диск' + (j.driveKeep ? '' : ', копия на компьютере удалится');
    case 'queued': return `Загружаю на Google Диск…${n}`;
    case 'done': return j.localGone ? `На Google Диске (${driveWhere(j)}) · копии на компьютере нет` : `Загружено на Google Диск (${driveWhere(j)})`;
    case 'error': return 'Не загрузилось на Google Диск: ' + (j.driveErr || 'ошибка');
    case 'cancelled': return 'Загрузку на Google Диск отменили';
  }
  return '';
}
function driveSub(j) {
  if (!j.toDrive || j.kind === 'upload' || !j.driveStep || j.driveStep === 'skip') return '';
  if (j.status === 'duplicate' && j.localGone) return ''; // причина уже говорит «лежит на Google Диске»
  const cls = j.driveStep === 'error' ? 'st-error' : j.driveStep === 'done' ? 'st-drive' : '';
  return `<div class="sub ${cls}" title="${esc(driveText(j))}">☁ ${esc(driveText(j, true))}</div>`;
}

const ROW_LIMIT = 500;
function renderRows() {
  const list = visible();
  const body = $('#rows');
  const ids = new Set(list.map(j => j.id));
  if (sel.size) { const have = new Set(jobs.map(j => j.id)); for (const id of [...sel]) if (!have.has(id)) sel.delete(id); }
  // тысячи строк (загрузка фотоархива) окно не тянет — рисуем первые ROW_LIMIT, остальное находит поиск
  const shown = list.length > ROW_LIMIT ? list.slice(0, ROW_LIMIT) : list;
  body.innerHTML = shown.map(j => {
    const sub = (j.status === 'error' || (j.status === 'retrying' && j.error) ? `<div class="sub st-error">${esc(j.error)}</div>`
      : j.status === 'duplicate' ? `<div class="sub st-dup">${esc(j.reason)}</div>`
      : (j.error && ACTIVE_ST.has(j.status)) ? `<div class="sub">${esc(j.error)}</div>` : '') + driveSub(j);
    const badge = (j.note ? `<span class="badge">${esc(j.note)}</span>` : '') +
      (j.toDrive && j.kind !== 'upload' && !j.driveStep ? '<span class="badge drive" title="После скачивания уйдёт на Google Диск">☁ в Диск</span>' : '');
    const conn = j.kind === 'torrent' ? (j.seeders || j.connections ? `${j.connections}/${j.seeders}` : '') : (j.connections || '');
    return `<tr data-id="${j.id}" class="${sel.has(j.id) ? 'sel' : ''}">
      <td><div class="name"><span class="fi">${fileIcon(j)}</span><div style="min-width:0"><div class="t" title="${esc(j.name)}">${esc(j.name)}</div>${sub}</div>${badge}</div></td>
      <td>${size(j.total)}</td>
      <td>${progCell(j)}</td>
      <td>${j.status === 'seeding' ? '↑ ' + speed(j.upSpeed) : speed(j.speed)}</td>
      <td>${j.status === 'active' ? eta(j.eta) : ''}</td>
      <td>${conn}</td>
      <td>${j.kind === 'upload' ? `<span class="muted" title="Папка на Google Диске">☁ ${esc(j.dir)}</span>`
        : j.localGone ? `<span class="muted" title="Файл только на Google Диске">☁ ${esc(driveWhere(j))}</span>`
        : j.dir ? `<span class="dirlink" data-dir="${esc(j.dir)}" title="${esc(j.reason || '')}">${esc(j.dir)}</span>` : ''}</td>
      <td>${when(j.added)}</td></tr>`;
  }).join('') + (list.length > shown.length
    ? `<tr><td colspan="8" class="more">Показаны первые ${ROW_LIMIT} из ${list.length} — остальные найдёт поиск или раздел слева</td></tr>` : '');
  $('#empty').hidden = list.length > 0;
  $$('.grid th').forEach(th => {
    th.classList.toggle('sorted', th.dataset.sort === sortKey);
    th.classList.toggle('asc', th.dataset.sort === sortKey && sortAsc);
  });
  const titles = { all: 'Все закачки', active: 'Загружаются', done: 'Завершённые', error: 'Ошибки', dup: 'Уже было скачано', up: 'Отправлено на Google Диск' };
  $('#viewTitle').textContent = view.startsWith('cat:') ? view.slice(4) : titles[view];
  $('#openCat').hidden = !view.startsWith('cat:');
  void ids;
}

function renderDetails() {
  const j = jobs.find(x => sel.has(x.id));
  const info = $('#tab-info');
  if (!j) { info.innerHTML = '<span class="muted">Выберите закачку, чтобы увидеть подробности.</span>'; drawMap(null); return; }
  const rows = [
    ['Имя', j.name], ['Состояние', (STATUS[j.status] || j.status) + (j.error ? ' — ' + j.error : '')],
    ['Ссылка', j.origUrl || j.url], j.pageUrl && j.pageUrl !== j.origUrl ? ['Страница', j.pageUrl] : null,
    ['Сайт', j.domain], ['Папка', j.dir], ['Почему сюда', j.reason], ['Файл', j.localGone ? 'только на Google Диске' : j.path],
    j.toDrive && j.kind !== 'upload' && j.driveStep !== 'skip' ? ['Google Диск', driveText(j) + (S?.g?.accounts?.length > 1 ? ' · ' + j.toDrive : '')] : null,
    ['Размер', j.total ? `${size(j.done)} из ${size(j.total)}` : ''], j.kind === 'torrent' ? ['Пиры / сиды', `${j.connections} / ${j.seeders}`] : null,
    j.retries ? ['Повторов после обрывов', j.retries] : null, ['Добавлен', when(j.added)], ['Завершён', when(j.finished)],
  ].filter(r => r && r[1] !== '' && r[1] != null);
  info.innerHTML = '<div class="kv">' + rows.map(([k, v]) => `<b>${esc(k)}</b><span>${esc(v)}</span>`).join('') + '</div>';
  drawMap(j);
}

// Карта секций: какие куски файла уже скачаны.
function drawMap(j) {
  const cv = $('#map'), note = $('#mapNote');
  const w = cv.clientWidth || 800; cv.width = w; cv.height = 64;
  const ctx = cv.getContext('2d');
  ctx.clearRect(0, 0, w, 64);
  if (!j) { note.textContent = ''; return; }
  const cs = getComputedStyle(document.documentElement);
  const on = cs.getPropertyValue('--accent').trim(), off = cs.getPropertyValue('--bar').trim();
  const n = j.numPieces || 0, bf = j.bitfield || '';
  const full = j.status === 'done' || j.status === 'seeding' || j.status === 'duplicate';
  const cols = Math.max(1, Math.floor(w / 6)), rows = 8, cells = cols * rows;
  for (let i = 0; i < cells; i++) {
    let have;
    if (full) have = true;
    else if (n && bf) {
      const from = Math.floor(i * n / cells), to = Math.max(from + 1, Math.floor((i + 1) * n / cells));
      let got = 0;
      for (let p = from; p < to; p++) { const nib = parseInt(bf[p >> 2], 16) || 0; if (nib & (8 >> (p & 3))) got++; }
      have = got / (to - from);
    } else have = pct(j) / 100 > i / cells;
    ctx.fillStyle = off; ctx.globalAlpha = 1;
    const x = (i % cols) * (w / cols), y = Math.floor(i / cols) * 8;
    ctx.fillRect(x + .5, y + .5, w / cols - 1, 7);
    if (have) { ctx.fillStyle = on; ctx.globalAlpha = typeof have === 'number' ? Math.max(.25, have) : 1; ctx.fillRect(x + .5, y + .5, w / cols - 1, 7); }
  }
  ctx.globalAlpha = 1;
  note.textContent = n ? `${n} частей · потоков сейчас: ${j.connections || 0}` : (full ? 'Файл скачан полностью' : '');
}

function renderStatus() {
  $('#stDown').textContent = '↓ ' + (speed(S.down) || '0');
  $('#stUp').textContent = '↑ ' + (speed(S.up) || '0');
  $('#stActive').textContent = S.active ? `Активных: ${S.active}` : 'Нет активных закачек';
  const t = S.tools;
  $('#stTools').textContent = t.busy ? `${t.busy} ${t.percent ? t.percent.toFixed(0) + '%' : ''}` : (t.error ? '⚠ ' + t.error : (!t.engine ? 'Запускаю движок…' : ''));
  $('#stRoot').textContent = '📁 ' + S.settings.root;
  $$('.speedmode button').forEach(b => b.classList.toggle('on', b.dataset.speed === S.settings.speedMode));
  $('#shutdown').checked = !!S.settings.shutdownWhenDone;
}

// Сервер присылает только изменившиеся закачки (rev/epoch), полный список — после перезапуска программы.
const jobMap = new Map();
let stEpoch = '', stRev = 0, lastRowsDraw = 0, lastView = '';
async function refresh() {
  try {
    const st = await api('state', { epoch: stEpoch, since: stRev });
    if (st.epoch === stEpoch && st.rev < stRev) return; // запоздавший ответ параллельного запроса
    S = st;
    if (S.full) jobMap.clear();
    for (const j of S.jobs || []) jobMap.set(j.id, j);
    for (const id of S.removed || []) jobMap.delete(id);
    const listChanged = S.full || (S.jobs || []).length > 0 || (S.removed || []).length > 0;
    if (listChanged) jobs = [...jobMap.values()];
    stEpoch = S.epoch; stRev = S.rev;
    driveLoginWatch();
    if (view.startsWith('gd:') && !(S.g?.accounts || []).some(a => 'gd:' + a.email === view)) view = 'all';
    $('#drive').hidden = !view.startsWith('gd:');
    $('#jobsView').hidden = view.startsWith('gd:');
    renderTree();
    // таблицу перерисовываем только при изменениях (и раз в минуту — для «сегодня 12:30»)
    if (listChanged || view !== lastView || Date.now() - lastRowsDraw > 60000) {
      renderRows(); lastRowsDraw = Date.now(); lastView = view;
    }
    renderStatus();
    pendingWatch(); uploadsWatch();
    if (detailTab === 'info' || detailTab === 'map') renderDetails();
    if (S.prompt && S.prompt.url && S.prompt.seq > lastPrompt) {
      lastPrompt = S.prompt.seq;
      api('prompt-ack');
      openAdd(S.prompt.url);
    }
  } catch (e) { $('#stTools').textContent = 'Нет связи с программой…'; }
}

// ---------- выбор строк ----------
// Двойной щелчок ловим сами по id строки: renderRows() пересоздаёт <tr> на каждом нажатии
// и раз в секунду в refresh(), поэтому штатный 'dblclick' до строки часто не доходит.
let lastDown = { id: '', t: 0 };
function rowDouble(id) {
  const j = jobs.find(x => x.id === id); if (!j) return;
  if (j.status === 'done' || j.status === 'seeding' || j.status === 'duplicate') act('open', [j.id]);
  else act(j.status === 'paused' || j.status === 'error' ? 'resume' : 'pause', [j.id]);
}
$('#rows').addEventListener('mousedown', e => {
  const tr = e.target.closest('tr'); if (!tr || !tr.dataset.id) return;
  const id = tr.dataset.id;
  if (e.button === 0 && !e.shiftKey && !e.ctrlKey) {
    const now = performance.now();
    if (lastDown.id === id && now - lastDown.t < 500) { lastDown = { id: '', t: 0 }; rowDouble(id); return; }
    lastDown = { id, t: now };
  } else lastDown = { id: '', t: 0 };
  if (e.button === 2 && sel.has(id)) return;
  const list = visible().map(j => j.id);
  if (e.shiftKey && anchor) {
    const a = list.indexOf(anchor), b = list.indexOf(id);
    sel = new Set(list.slice(Math.min(a, b), Math.max(a, b) + 1));
  } else if (e.ctrlKey) { sel.has(id) ? sel.delete(id) : sel.add(id); anchor = id; }
  else { sel = new Set([id]); anchor = id; }
  renderRows(); renderDetails();
});
$('#rows').addEventListener('click', e => {
  const d = e.target.closest('.dirlink');
  if (d) { e.stopPropagation(); api('open-dir', { dir: d.dataset.dir }); }
});
$('#tablewrap').addEventListener('mousedown', e => {
  if (!e.target.closest('tr') && !e.target.closest('th')) { sel.clear(); renderRows(); renderDetails(); }
});
$$('#jobsView .grid th[data-sort]').forEach(th => th.addEventListener('click', () => {
  if (sortKey === th.dataset.sort) sortAsc = !sortAsc; else { sortKey = th.dataset.sort; sortAsc = sortKey === 'name' || sortKey === 'dir'; }
  renderRows();
}));
$('#tree').addEventListener('click', e => {
  const it = e.target.closest('.item'); if (!it) return;
  if (it.dataset.view === 'gd-add') {
    if (S?.g?.login !== 'wait') return driveConnect();
    return showMenu(e, [
      ['Открыть вход ещё раз', async () => { const r = await api('gdrive/login-reopen'); if (!r.ok) toast(r.error, 'err'); }],
      ['Скопировать ссылку входа', () => {
        navigator.clipboard.writeText(S.g.loginUrl || '');
        toast('Ссылка скопирована — вставьте её в браузер, где выполнен вход в Google', 'ok');
      }],
      null,
      ['Отменить вход', () => api('gdrive/login-cancel').then(refresh)],
    ]);
  }
  const was = view;
  view = it.dataset.view;
  $('#drive').hidden = !view.startsWith('gd:');
  $('#jobsView').hidden = view.startsWith('gd:');
  if (view.startsWith('gd:') && view !== was) openDrive(it.dataset.acc);
  renderTree(); renderRows();
});
$('#tree').addEventListener('contextmenu', e => {
  e.preventDefault();
  const it = e.target.closest('.item[data-acc]'); if (!it) return;
  const acc = it.dataset.acc;
  showMenu(e, [
    ['Открыть Диск в браузере', () => api('open-url', { url: 'https://drive.google.com/' })],
    ['Добавить ещё аккаунт', driveConnect],
    null,
    ['Отключить аккаунт', async () => {
      if (!confirm(`Отключить ${acc}? Файлы на Диске и уже скачанные останутся, программа просто забудет доступ.`)) return;
      await api('gdrive/disconnect', { acc });
      if (view === 'gd:' + acc) view = 'all';
      toast('Аккаунт отключён', 'ok'); refresh();
    }, 'danger'],
  ]);
});
$('#openCat').onclick = () => api('open-dir', { dir: view.slice(4) });
$('#search').addEventListener('input', renderRows);
$$('.tabs button').forEach(b => b.onclick = () => {
  detailTab = b.dataset.tab;
  $$('.tabs button').forEach(x => x.classList.toggle('on', x === b));
  $('#tab-info').hidden = detailTab !== 'info'; $('#tab-map').hidden = detailTab !== 'map';
  renderDetails();
});

// ---------- действия ----------
async function act(action, ids = [...sel]) {
  if (!ids.length) { toast('Сначала выберите закачку'); return; }
  const r = await api('action', { ids, action });
  if (!r.ok) toast(r.error, 'err');
  refresh();
}
async function removeSelected(withFile) {
  const ids = [...sel]; if (!ids.length) return toast('Сначала выберите закачку');
  if (withFile && !confirm(`Удалить ${ids.length === 1 ? 'файл' : ids.length + ' файлов'} с диска? Это нельзя отменить.`)) return;
  await act(withFile ? 'removeWithFile' : 'remove', ids);
  sel.clear();
}
const CMD = {
  add: () => openAdd(''),
  resume: () => act('resume'), pause: () => act('pause'), remove: () => removeSelected(false),
  resumeAll: () => api('bulk', { action: 'resume' }).then(refresh), pauseAll: () => api('bulk', { action: 'pause' }).then(refresh),
  openRoot: () => api('open-root'),
  sortRoot: async () => {
    const r = await api('sort-root');
    toast(r.moved || r.dupes ? `Разложено файлов: ${r.moved}${r.dupes ? `, дубликатов в Корзину: ${r.dupes}` : ''}` : 'В корне папки загрузок нечего раскладывать', 'ok');
    refresh();
  },
  settings: openSettings,
};
$$('[data-cmd]').forEach(b => b.onclick = () => CMD[b.dataset.cmd]());
$$('.speedmode button').forEach(b => b.onclick = () => api('speed', { mode: b.dataset.speed }).then(refresh));
$('#shutdown').onchange = e => {
  api('shutdown-when-done', { on: e.target.checked });
  if (e.target.checked) toast('Компьютер выключится через минуту после окончания всех закачек');
};
$('#stRoot').onclick = () => api('open-root');

// ---------- контекстное меню ----------
const menu = $('#menu');
$('#rows').addEventListener('contextmenu', e => {
  e.preventDefault();
  const tr = e.target.closest('tr'); if (!tr) return;
  const j = jobs.find(x => x.id === tr.dataset.id); if (!j) return;
  const fin = j.status === 'done' || j.status === 'seeding' || j.status === 'duplicate';
  const items = [];
  if (j.kind === 'upload') {
    if (j.gFileId) items.push(['Открыть на Google Диске', () => act('drive')]);
    if (!j.deleteAfter) items.push(['Показать файл на компьютере', () => act('folder')]);
    if (j.status === 'paused' || j.status === 'error') items.push(['▶ Продолжить', () => act('resume')]);
    if (j.status === 'active' || j.status === 'waiting' || j.status === 'retrying') items.push(['❚❚ Пауза', () => act('pause')]);
    items.push(null, ['Убрать из списка', () => removeSelected(false)]);
    return showMenu(e, items);
  }
  const gone = !!j.localGone;
  if (fin && !gone) items.push(['Открыть', () => act('open')], ['Показать в папке', () => act('folder')]);
  if (j.toDrive && (j.driveStep === 'done' || gone)) items.push(['☁ Открыть на Google Диске', () => act('drive')]);
  if (j.driveStep === 'error' || j.driveStep === 'cancelled') items.push(['☁ Повторить загрузку на Диск', () => act('drive-retry')]);
  else if (fin && !gone && j.path && S?.g?.accounts?.length && j.driveStep !== 'queued' && !(j.toDrive && !j.driveStep))
    items.push([j.driveStep === 'done' ? '☁ Загрузить на Google Диск ещё раз' : '☁ Загрузить на Google Диск', () => act('to-drive')]);
  if (j.status === 'paused' || j.status === 'error') items.push(['▶ Продолжить', () => act('resume')]);
  if (ACTIVE_ST.has(j.status) && j.status !== 'paused') items.push(['❚❚ Пауза', () => act('pause')]);
  if (j.status === 'error') items.push(['↻ Повторить', () => act('retry')]);
  if (j.status === 'duplicate' && j.origUrl) items.push(['⬇ Скачать всё равно', () => act('force')]);
  if (j.status !== 'duplicate' && !gone) items.push(['Переложить в папку…', openMove]);
  if (j.origUrl || j.url) items.push(['Копировать ссылку', () => navigator.clipboard.writeText(j.origUrl || j.url)]);
  items.push(null, ['Удалить из списка', () => removeSelected(false)]);
  if (fin && j.status !== 'duplicate' && !gone) items.push(['Удалить вместе с файлом', () => removeSelected(true), 'danger']);
  showMenu(e, items);
});
function showMenu(e, items) {
  menu.innerHTML = '';
  for (const it of items) {
    if (!it) { menu.appendChild(document.createElement('hr')); continue; }
    const d = document.createElement('div'); d.textContent = it[0]; if (it[2]) d.className = it[2];
    d.onclick = () => { menu.hidden = true; it[1](); };
    menu.appendChild(d);
  }
  menu.hidden = false;
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.min(e.clientX, innerWidth - r.width - 8) + 'px';
  menu.style.top = Math.min(e.clientY, innerHeight - r.height - 8) + 'px';
}
document.addEventListener('mousedown', e => { if (!menu.contains(e.target)) menu.hidden = true; });
document.addEventListener('contextmenu', e => { if (!e.target.closest('#rows,#gRows,#tree') && !e.target.closest('textarea,input')) e.preventDefault(); });

// ---------- диалог «Добавить» ----------
function fillDirs(selEl, withAuto) {
  const dirs = S?.dirs || [];
  selEl.innerHTML = (withAuto ? '<option value="auto">Авто — программа решит сама</option>' : '') +
    dirs.map(d => `<option value="${esc(d)}">${esc(d)}</option>`).join('');
}
// addTarget — папка Диска, если окно открыли из файлового менеджера Диска («🔗 По ссылке»)
let addTarget = null;
function openAdd(text, opts = {}) {
  const d = $('#dlgAdd');
  fillDirs($('#addDir'), true);
  $('#addUrls').value = text || '';
  $('#addQuality').value = S?.settings.videoQuality || '1080';
  $('#addMode').value = 'auto';
  $('#addTorrent').value = '';
  ['addPlaylist', 'addPaused', 'addForce'].forEach(id => $('#' + id).checked = false);
  addTarget = opts.drive || null;
  const hasDrive = (S?.g?.accounts || []).length > 0;
  $('#addDriveBox').hidden = !hasDrive;
  $('#addToDrive').checked = !!addTarget;
  $('#addKeep').checked = !!S?.settings.driveKeepLocal;
  addDriveUpdate();
  $('#dlgAdd h2').textContent = addTarget ? 'Скачать в Google Диск' : 'Новая закачка';
  $('#addGo').textContent = addTarget ? 'Скачать в Диск' : 'Скачать';
  if (!d.open) d.showModal();
  setTimeout(() => $('#addUrls').focus(), 30);
}
function addDriveUpdate() {
  const on = $('#addToDrive').checked;
  $('#addDriveBox').classList.toggle('on', on);
  const s = S?.settings || {};
  const accs = S?.g?.accounts || [];
  const acc = addTarget?.acc || (accs.find(a => a.email === s.driveAcc) || accs[0] || {}).email || '';
  const many = accs.length > 1 ? ` · ${acc}` : '';
  $('#addDriveWhere').textContent = addTarget
    ? `в «${addTarget.name}»${many}`
    : `в «${(s.driveFolder || 'Загрузки').replace(/\//g, '\\')}\\<папка раскладки>»${many}`;
}
$('#addToDrive').onchange = addDriveUpdate;
const fileB64 = f => new Promise((ok, no) => {
  const r = new FileReader();
  r.onload = () => ok(String(r.result).split(',')[1]); r.onerror = no; r.readAsDataURL(f);
});
$('#addGo').onclick = async e => {
  e.preventDefault();
  const urls = $('#addUrls').value.split(/\s*\n\s*/).map(s => s.trim()).filter(Boolean);
  const base = {
    mode: $('#addMode').value, quality: $('#addQuality').value, dir: $('#addDir').value,
    playlist: $('#addPlaylist').checked, paused: $('#addPaused').checked, force: $('#addForce').checked,
  };
  const toDrive = !$('#addDriveBox').hidden && $('#addToDrive').checked;
  if (toDrive) {
    Object.assign(base, { toDrive: addTarget?.acc || '1', keepLocal: $('#addKeep').checked });
    if (addTarget) Object.assign(base, { driveParent: addTarget.id, driveParentName: addTarget.name });
  }
  const files = [...$('#addTorrent').files];
  if (!urls.length && !files.length) return toast('Вставьте ссылку или выберите .torrent', 'err');
  let n = 0, errs = [];
  if (urls.length) { const r = await api('add', { ...base, urls }); n += (r.ids || []).length; if (r.error) errs.push(r.error); }
  for (const f of files) { const r = await api('add', { ...base, torrent: await fileB64(f) }); n += (r.ids || []).length; if (r.error) errs.push(r.error); }
  $('#dlgAdd').close();
  if (n) toast(toDrive ? `Добавлено: ${n}. После скачивания уйдёт на Google Диск${$('#addKeep').checked ? '' : ', копия на компьютере удалится'}` : `Добавлено закачек: ${n}`, 'ok');
  if (errs.length) toast(errs.join('; '), 'err');
  if (!view.startsWith('gd:') || !addTarget) view = 'all';
  refresh();
};

// ---------- диалог «Переложить» ----------
function openMove() {
  const ids = [...sel]; if (!ids.length) return;
  const j = jobs.find(x => x.id === ids[0]);
  fillDirs($('#moveDir'), false);
  const cur = j.dir || 'Прочее';
  if (![...$('#moveDir').options].some(o => o.value === cur)) $('#moveDir').add(new Option(cur, cur), 0);
  $('#moveDir').value = cur;
  $('#moveNew').value = '';
  const ext = (j.name.match(/\.[^.]+$/) || [''])[0].toLowerCase();
  let h = '<label><input type="radio" name="rem" value="" checked> Только эти файлы</label>';
  if (j.domain) h += `<label><input type="radio" name="rem" value="domain"> Всегда класть сюда файлы с сайта <b>${esc(j.domain)}</b></label>`;
  if (ext) h += `<label><input type="radio" name="rem" value="ext"> Всегда класть сюда все <b>*${esc(ext)}</b></label>`;
  $('#moveRemember').innerHTML = h;
  $('#moveRemember').className = 'checks col';
  $('#dlgMove').showModal();
}
$('#moveGo').onclick = async e => {
  e.preventDefault();
  const dir = $('#moveNew').value.trim() || $('#moveDir').value;
  const remember = ($('input[name=rem]:checked') || {}).value || '';
  const r = await api('move', { ids: [...sel], dir, remember });
  $('#dlgMove').close();
  toast(r.ok ? (remember ? 'Переложено и запомнено' : 'Переложено') : r.error, r.ok ? 'ok' : 'err');
  refresh();
};

// ---------- настройки ----------
function openSettings() {
  const s = S.settings;
  $('#sRoot').value = s.root; $('#sConn').value = s.connections; $('#sMax').value = s.maxActive;
  $('#sMid').value = s.midKB; $('#sLow').value = s.lowKB; $('#sUp').value = s.uploadLimitKB; $('#sRatio').value = s.seedRatio;
  $('#sQuality').value = s.videoQuality;
  $('#sIntercept').checked = s.intercept; $('#sClip').checked = s.clipboard; $('#sAutoSort').checked = s.autoSortRoot;
  $('#sDupes').checked = s.skipDupes; $('#sTrackers').checked = s.trackers; $('#sAutostart').checked = s.autostart;
  $('#sExcluded').value = (s.excludedSites || []).join('\n');
  const accs = S.g?.accounts || [];
  $('#sDriveSet').hidden = !accs.length;
  $('#sDriveAcc').innerHTML = accs.map(a => `<option value="${esc(a.email)}">${esc(a.email)}</option>`).join('');
  $('#sDriveAcc').value = accs.some(a => a.email === s.driveAcc) ? s.driveAcc : (accs[0]?.email || '');
  $('#sDriveAcc').disabled = accs.length < 2;
  $('#sDriveFolder').value = (s.driveFolder || 'Загрузки').replace(/\//g, '\\');
  $('#sDriveKeep').checked = !!s.driveKeepLocal;
  $('#sGDrive').hidden = !S.gdrive;
  $('#sSendTo').textContent = S.g?.sendTo ? 'Убрать из Проводника «Отправить → Google Диск»' : 'Добавить в Проводник «Отправить → Google Диск»';
  $('#sVersion').textContent = 'Версия ' + S.version;
  $('#dlgSettings').showModal();
}
$('#sPick').onclick = async () => { const r = await api('pick-folder'); if (r.path) $('#sRoot').value = r.path; };
$('#sGDrive').onclick = () => { $('#sRoot').value = S.gdrive + '\\Загрузки'; };
$('#sDefault').onclick = () => { api('register-default'); toast('Откроется окно Windows — выберите «MAIRDI Загрузчик» для .torrent и magnet'); };
$('#sRules').onclick = openRules;
$('#sSendTo').onclick = async () => {
  const on = !S.g?.sendTo;
  const r = await api('sendto', { on });
  if (!r.ok) return toast(r.error, 'err');
  S.g.sendTo = on;
  $('#sSendTo').textContent = on ? 'Убрать из Проводника «Отправить → Google Диск»' : 'Добавить в Проводник «Отправить → Google Диск»';
  toast(on ? 'Готово: выделите файлы в Проводнике → правая кнопка → «Отправить» → «Google Диск (MAIRDI)»' : 'Пункт убран из Проводника', 'ok');
};
// ---------- мастер расширения ----------
let ext = null, extBr = 'chrome', extTimer = 0, extForce = false;
const agoText = ms => {
  const m = Math.round((Date.now() - ms) / 60000);
  return m < 1 ? 'только что' : m < 60 ? m + ' мин назад' : m < 1440 ? Math.round(m / 60) + ' ч назад' : new Date(ms).toLocaleDateString('ru-RU');
};
async function copyText(t) {
  try { await navigator.clipboard.writeText(t); return true; } catch {}
  const ta = Object.assign(document.createElement('textarea'), { value: t });
  document.body.appendChild(ta); ta.select();
  const ok = document.execCommand('copy'); ta.remove(); return ok;
}
function extFooter() {
  const el = $('#stExt'); if (!ext) { el.textContent = ''; return; }
  el.className = 'link ext-' + ext.state;
  el.textContent = { none: '🧩 Установить расширение', old: '🧩 Обновите расширение', ok: '🧩 Расширение ✓' }[ext.state];
}
function extRender() {
  if (!ext) return;
  const page = extBr === 'edge' ? 'edge://extensions' : 'chrome://extensions';
  $$('#dlgExt .brPage').forEach(e => e.textContent = page);
  $$('#extBr button').forEach(b => b.classList.toggle('on', b.dataset.br === extBr));
  $$('#dlgExt .brOnly').forEach(e => e.hidden = e.dataset.br !== extBr);
  $('#extBr').hidden = !(ext.chrome && ext.edge) || (ext.state === 'ok' && !extForce);
  $('#extPath').textContent = ext.path || '';
  const st = $('#extState');
  st.className = 'extstate ' + ext.state;
  if (ext.state === 'ok') st.innerHTML = `<b>✅ Расширение работает</b><span>версия ${esc(ext.version)}, на связи ${agoText(ext.seen)}</span>`;
  else if (ext.state === 'old') st.innerHTML = `<b>🟠 Установлена старая версия${ext.version ? ' ' + esc(ext.version) : ''}</b><span>Новая — ${esc(ext.want)}. Обновление — два шага ниже.</span>`;
  else st.innerHTML = `<b>Расширение ещё не подключено</b><span>Четыре шага ниже — около минуты. Программа сама увидит, когда всё получится.</span>`;
  if (!ext.chrome && !ext.edge) st.innerHTML += '<span>Chrome и Edge не найдены — откройте страницу расширений в своём браузере вручную.</span>';
  $('#extSteps').hidden = !(ext.state === 'none' || extForce);
  $('#extUpd').hidden = !(ext.state === 'old' && !extForce);
  $('#extNote').hidden = ext.state === 'ok' && !extForce;
  $('#extAgain').hidden = !(ext.state === 'ok' && !extForce);
}
async function extLoad() {
  try {
    const prev = ext && ext.state;
    ext = await api('ext-status');
    if (!ext.chrome && ext.edge) extBr = 'edge';
    if ($('#dlgExt').open && prev && prev !== 'ok' && ext.state === 'ok') { toast('Расширение подключено ✅', 'ok'); extForce = false; }
    extFooter(); if ($('#dlgExt').open) extRender();
  } catch {}
}
function openExtWizard() {
  extForce = false;
  $('#dlgExt').showModal(); extRender(); extLoad();
  clearInterval(extTimer); extTimer = setInterval(extLoad, 1500);
}
$('#dlgExt').addEventListener('close', () => { clearInterval(extTimer); extTimer = setInterval(extLoad, 30000); });
$('#sExt').onclick = openExtWizard;
$('#stExt').onclick = openExtWizard;
$('#extOpen').onclick = () => api('open-extension');
$('#extAgain').onclick = () => { extForce = true; extRender(); };
$$('#extBr button').forEach(b => b.onclick = () => { extBr = b.dataset.br; extRender(); });
$('#extCopy').onclick = async () => toast(await copyText(ext?.path || '') ? 'Путь скопирован — вставьте его Ctrl+V' : 'Не удалось скопировать', 'ok');
const extGo = async () => {
  const install = extForce || ext?.state === 'none';
  if (install) await copyText(ext?.path || '');
  try { const r = await api('ext-browser', { browser: extBr }); if (!r.ok) throw 0; toast(install ? 'Открыл страницу расширений, путь к папке скопирован' : 'Открыл страницу расширений', 'ok'); }
  catch { toast('Не удалось открыть браузер — наберите адрес вручную', 'err'); }
};
$('#extGo').onclick = extGo; $('#extGo2').onclick = extGo;
extLoad(); extTimer = setInterval(extLoad, 30000);
$('#sSave').onclick = async e => {
  e.preventDefault();
  const s = {
    ...S.settings, root: $('#sRoot').value.trim(), connections: +$('#sConn').value, maxActive: +$('#sMax').value,
    midKB: +$('#sMid').value, lowKB: +$('#sLow').value, uploadLimitKB: +$('#sUp').value, seedRatio: +$('#sRatio').value,
    videoQuality: $('#sQuality').value, intercept: $('#sIntercept').checked, clipboard: $('#sClip').checked,
    autoSortRoot: $('#sAutoSort').checked, skipDupes: $('#sDupes').checked, trackers: $('#sTrackers').checked,
    autostart: $('#sAutostart').checked, excludedSites: $('#sExcluded').value.split(/\s+/).filter(Boolean),
  };
  if (!$('#sDriveSet').hidden) Object.assign(s, {
    driveAcc: $('#sDriveAcc').value, driveFolder: $('#sDriveFolder').value.trim(), driveKeepLocal: $('#sDriveKeep').checked,
  });
  const r = await api('settings', s);
  if (!r.ok) return toast(r.error, 'err');
  $('#dlgSettings').close(); toast('Настройки сохранены', 'ok'); refresh();
};

// ---------- правила ----------
async function openRules() {
  const r = await api('rules');
  const kinds = { domain: 'Сайт', ext: 'Расширение', keyword: 'Слово в названии' };
  $('#rulesBody').innerHTML = (r.rules || []).map((x, i) =>
    `<tr><td>${kinds[x.kind] || x.kind}</td><td>${esc(x.value)}</td><td>${esc(x.dir)}</td><td><button type="button" data-del="${i}">Удалить</button></td></tr>`
  ).join('') || '<tr><td colspan="4" class="muted">Правил пока нет — программа раскладывает по встроенной логике.</td></tr>';
  fillDirs($('#rDir'), false);
  $$('[data-del]').forEach(b => b.onclick = async () => { await api('rules/delete', { index: +b.dataset.del }); openRules(); });
  if (!$('#dlgRules').open) $('#dlgRules').showModal();
}
$('#rAdd').onclick = async () => {
  const r = await api('rules/add', { kind: $('#rKind').value, value: $('#rValue').value, dir: $('#rDir').value });
  if (!r.ok) return toast(r.error, 'err');
  $('#rValue').value = ''; openRules();
};

// ---------- клавиатура, вставка, перетаскивание ----------
document.addEventListener('keydown', e => {
  if ($('dialog[open]') || e.target.matches('input,textarea,select')) return;
  if (view.startsWith('gd:')) return driveKey(e);
  if (e.key === 'Delete') removeSelected(e.shiftKey);
  else if (e.key === 'Enter') { const j = jobs.find(x => sel.has(x.id)); if (j) act('open', [j.id]); }
  else if (e.ctrlKey && e.key.toLowerCase() === 'a') { e.preventDefault(); sel = new Set(visible().map(j => j.id)); renderRows(); }
  else if (e.ctrlKey && e.key.toLowerCase() === 'n') { e.preventDefault(); openAdd(''); }
});
document.addEventListener('paste', e => {
  if ($('dialog[open]') || e.target.matches('input,textarea')) return;
  const t = (e.clipboardData.getData('text') || '').trim();
  if (!t) return;
  if (view.startsWith('gd:') && uploadTarget()) driveLink(t); // вставили ссылку в папке Диска — качаем туда
  else openAdd(t);
});
let dragDepth = 0;
document.addEventListener('dragenter', e => {
  if (G.drag) return;
  e.preventDefault(); dragDepth++; $('#drop').hidden = false;
  const t = uploadTarget();
  $('#drop div').textContent = view.startsWith('gd:')
    ? (t ? `Отпустите — загружу на Google Диск в «${t.name}»` : 'Откройте папку на Диске, куда загружать')
    : 'Отпустите — добавлю закачку';
});
document.addEventListener('dragleave', () => { if (--dragDepth <= 0) { dragDepth = 0; $('#drop').hidden = true; } });
document.addEventListener('dragover', e => e.preventDefault());
document.addEventListener('drop', async e => {
  e.preventDefault(); dragDepth = 0; $('#drop').hidden = true;
  if (G.drag) return;
  if (view.startsWith('gd:') && e.dataTransfer.files.length) return driveDrop(e.dataTransfer);
  if (view.startsWith('gd:')) { // ссылку перетащили из браузера в папку Диска
    const link = (e.dataTransfer.getData('text/uri-list') || e.dataTransfer.getData('text') || '').trim();
    if (link) driveLink(link.split(/\s*\n\s*/).filter(s => s && !s.startsWith('#')).join('\n'));
    return;
  }
  const files = [...e.dataTransfer.files].filter(f => f.name.toLowerCase().endsWith('.torrent'));
  for (const f of files) await api('add', { torrent: await fileB64(f) });
  const t = e.dataTransfer.getData('text/uri-list') || e.dataTransfer.getData('text');
  if (t) await api('add', { urls: t.split(/\s*\n\s*/).filter(s => s && !s.startsWith('#')) });
  if (files.length || t) toast('Закачка добавлена', 'ok');
  refresh();
});

// ---------- Google Диск ----------
const GSEC = { my: 'Мой диск', shared: 'Доступные мне', drives: 'Общие диски', recent: 'Недавние', starred: 'Помеченные', trash: 'Корзина' };
const G = { acc: '', sec: 'my', stack: [], q: '', files: [], next: '', sel: new Set(), anchor: null, loading: false, error: '', about: null, seq: 0,
  order: 'name', drag: null, undo: null };
let prevLogin = null, knownAccs = null;

function driveLoginWatch() {
  const L = S.g?.login ?? '', accs = (S.g?.accounts || []).map(a => a.email);
  if (prevLogin === 'wait' && L !== 'wait') {
    if (L) toast(L, 'err');
    else {
      const added = accs.find(a => !(knownAccs || []).includes(a)) || accs[accs.length - 1];
      toast('Google Диск подключён: ' + added, 'ok');
      if (added) { view = 'gd:' + added; openDrive(added); }
    }
  }
  prevLogin = L; knownAccs = accs;
}

async function driveConnect() {
  if (!S?.g?.configured) return openGKey();
  const r = await api('gdrive/connect');
  if (!r.ok) return toast(r.error, 'err');
  prevLogin = 'wait';
  toast('В Chrome откроется вход Google — выберите аккаунт и нажмите «Разрешить». Нет окна — щёлкните «Вход в Google…» слева');
  refresh();
}

function openGKey() {
  $('#gKeyId').value = ''; $('#gKeySecret').value = ''; $('#gKeyFile').value = '';
  $('#dlgGKey').showModal();
}
$$('[data-gurl]').forEach(b => b.onclick = () => api('open-url', { url: b.dataset.gurl }));
$('#gKeyFile').onchange = async e => {
  const f = e.target.files[0]; if (!f) return;
  $('#gKeyId').value = await f.text();
};
$('#gKeyGo').onclick = async e => {
  e.preventDefault();
  const r = await api('gdrive/client', { clientId: $('#gKeyId').value, clientSecret: $('#gKeySecret').value });
  if (!r.ok) return toast(r.error, 'err');
  $('#dlgGKey').close();
  S.g.configured = true;
  driveConnect();
};

const curFolder = () => G.stack[G.stack.length - 1];

function openDrive(acc) {
  if (!acc) return;
  Object.assign(G, { acc, sec: 'my', stack: [], q: '', files: [], next: '', error: '', about: null });
  G.sel.clear();
  $('#gSearch').value = '';
  loadDrive(); loadAbout();
}

async function loadAbout() {
  const acc = G.acc;
  const r = await api('gdrive/about', { acc }).catch(() => null);
  if (acc !== G.acc || !r?.ok) return;
  G.about = r.about; renderQuota(); if (!G.sel.size) renderDriveDetails();
}

async function loadDrive(more) {
  const seq = ++G.seq;
  if (!more) { G.files = []; G.next = ''; G.sel.clear(); G.anchor = null; }
  G.loading = true; G.error = '';
  renderDrive();
  const f = curFolder();
  const r = await api('gdrive/list', {
    acc: G.acc, section: G.sec, folder: f?.id || '', driveId: f?.driveId || '', q: G.q, pageToken: more ? G.next : '',
    order: G.order.startsWith('owner') ? '' : G.order,
  }).catch(e => ({ ok: false, error: e.message }));
  if (seq !== G.seq) return;
  G.loading = false;
  if (!r.ok) G.error = r.error;
  else { G.files = more ? G.files.concat(r.files) : r.files; G.next = r.next || ''; gSortOwner(); }
  renderDrive();
}

function gIcon(f) {
  if (f.driveId && f.folder && f.owner === 'общий диск') return '<span class="fi">🗄️</span>';
  if (f.icon) return `<img class="gi" src="${esc(f.icon)}" alt="" referrerpolicy="no-referrer">`;
  return `<span class="fi">${f.folder ? '📁' : '📄'}</span>`;
}

function renderDrive() {
  // хлебные крошки
  const root = G.q ? `Поиск: «${G.q}»` : GSEC[G.sec];
  const parts = [`<span data-crumb="0">${esc(root)}</span>`].concat(G.stack.map((s, i) => `<i>›</i><span data-crumb="${i + 1}">${esc(s.name)}</span>`));
  $('#gCrumbs').innerHTML = parts.join('');
  $$('#gSections button').forEach(b => b.classList.toggle('on', !G.q && b.dataset.sec === G.sec));
  $('#gUp').disabled = !G.stack.length && !G.q;
  $('#gDown').disabled = !G.sel.size;
  $('#gDown').textContent = G.sel.size > 1 ? `⬇ Скачать (${G.sel.size})` : '⬇ Скачать';
  // строки
  $('#gRows').innerHTML = G.files.map(f => {
    const kind = f.kind ? `<span class="kind" title="Скачается в формате ${esc(f.kind)}">.${esc(f.kind)}</span>` : '';
    const star = f.starred ? '<span class="star">★</span>' : '';
    const sc = f.shortcut ? '<span class="muted" title="Ярлык">↪</span>' : '';
    const cls = [G.sel.has(f.id) ? 'sel' : '', f.folder ? 'folder' : '', f.noDown ? 'muted-row' : ''].join(' ');
    return `<tr data-id="${esc(f.id)}" class="${cls}" draggable="true">
      <td><div class="name">${gIcon(f)}<div class="t" title="${esc(f.name)}">${esc(f.name)}</div>${sc}${star}${kind}</div></td>
      <td>${esc(f.owner || '')}</td><td>${when(f.modified)}</td><td>${f.folder ? '' : size(f.size)}</td></tr>`;
  }).join('') + (G.next ? `<tr><td colspan="4" class="more">${G.loading ? 'Загружаю…' : 'Прокрутите ниже, чтобы показать ещё'}</td></tr>` : '');
  const em = $('#gEmpty');
  em.hidden = G.files.length > 0;
  em.innerHTML = G.loading ? '<div class="big">☁️</div><div>Загружаю список с Google Диска…</div>'
    : G.error ? `<div class="big">⚠️</div><div class="st-error">${esc(G.error)}</div><button class="gbtn" id="gRetry">Повторить</button>`
    : G.q ? '<div class="big">🔍</div><div>Ничего не нашлось</div>'
    : '<div class="big">📂</div><div>Здесь пусто</div>';
  const rb = $('#gRetry'); if (rb) rb.onclick = () => loadDrive();
  const ok = G.order.replace(/-$/, ''), desc = G.order.endsWith('-');
  $$('.gtable th[data-gsort]').forEach(th => { th.classList.toggle('sorted', th.dataset.gsort === ok); th.classList.toggle('asc', th.dataset.gsort === ok && !desc); });
  renderActs();
  const canUp = !!uploadTarget();
  $('#gUpFiles').disabled = !canUp; $('#gUpDir').disabled = !canUp; $('#gLink').disabled = !canUp;
  renderPending();
  renderQuota();
  renderDriveDetails();
}

function renderQuota() {
  const q = $('#gQuota'), a = G.about;
  q.hidden = !a;
  if (!a) return;
  const p = a.limit > 0 ? Math.min(100, a.usage * 100 / a.limit) : 0;
  q.querySelector('span').textContent = a.limit > 0 ? `Занято ${size(a.usage)} из ${size(a.limit)}` : `Занято ${size(a.usage)}`;
  q.querySelector('.qbar').classList.toggle('full', p > 90);
  q.querySelector('.qbar > div').style.width = p + '%';
  q.title = a.trash ? `В корзине Диска: ${size(a.trash)}` : '';
}

function renderDriveDetails() {
  const sel = G.files.filter(f => G.sel.has(f.id));
  const prev = $('#gPrev'), info = $('#gInfo');
  if (sel.length === 1) {
    const f = sel[0];
    prev.innerHTML = f.thumb
      ? `<img src="/gthumb?t=${encodeURIComponent(window.TOKEN)}&acc=${encodeURIComponent(G.acc)}&u=${encodeURIComponent(f.thumb)}" alt="" onerror="this.replaceWith(Object.assign(document.createElement('div'),{className:'big',textContent:'📄'}))">`
      : `<div class="big">${f.folder ? '📁' : '📄'}</div>`;
    const rows = [
      ['Имя', f.name], ['Тип', f.folder ? 'Папка' : f.mime], ['Размер', f.folder ? '' : size(f.size)],
      ['Изменён', when(f.modified)], ['Владелец', f.owner],
      f.kind ? ['Скачается как', '.' + f.kind + ' (документ Google в формате Office)'] : null,
      f.noDown ? ['Скачивание', 'Недоступно: владелец запретил или это форма/сайт Google'] : null,
      f.folder ? ['Скачивание', 'Папка целиком ляжет в «Google Диск\\' + f.name + '» со всеми подпапками'] : null,
    ].filter(r => r && r[1]);
    info.innerHTML = '<div class="kv">' + rows.map(([k, v]) => `<b>${esc(k)}</b><span>${esc(v)}</span>`).join('') + '</div>';
  } else if (sel.length > 1) {
    prev.innerHTML = '<div class="big">🗂️</div>';
    const total = sel.reduce((s, f) => s + (f.size || 0), 0), dirs = sel.filter(f => f.folder).length;
    info.innerHTML = `<div class="kv"><b>Выбрано</b><span>${sel.length}${dirs ? `, из них папок: ${dirs}` : ''}</span><b>Размер файлов</b><span>${size(total) || '—'}</span></div>`;
  } else {
    prev.innerHTML = '<div class="big">☁️</div>';
    const a = G.about;
    info.innerHTML = a ? `<div class="kv"><b>Аккаунт</b><span>${esc(a.name)} · ${esc(a.email)}</span><b>Занято</b><span>${size(a.usage)}${a.limit ? ' из ' + size(a.limit) : ''}</span>${a.trash ? `<b>В корзине</b><span>${size(a.trash)}</span>` : ''}</div>
      <div class="muted" style="margin-top:8px">Двойной щелчок по папке — открыть, по файлу — скачать. Можно выбрать несколько (Ctrl, Shift) и нажать «Скачать».</div>`
      : '<span class="muted">Выберите файл или папку.</span>';
  }
}

function driveEnter(f) {
  if (f.folder) {
    const cur = curFolder();
    G.stack.push({ id: f.id, name: f.name, driveId: f.driveId || cur?.driveId || '' });
    if (G.q) { G.q = ''; $('#gSearch').value = ''; }
    loadDrive();
  } else driveDownload([f.id]);
}
function driveUp() {
  if (G.stack.length) { G.stack.pop(); loadDrive(); }
  else if (G.q) { G.q = ''; $('#gSearch').value = ''; loadDrive(); }
}
async function driveDownload(ids = [...G.sel]) {
  ids = ids.filter(id => !G.files.find(f => f.id === id)?.noDown);
  if (!ids.length) return toast('Выберите, что скачать');
  toast('Ставлю в очередь…');
  const r = await api('gdrive/download', { acc: G.acc, ids });
  const msg = [];
  if (r.added) msg.push(`Скачиваю: ${r.added}`);
  if (r.skipped) msg.push(`уже есть на компьютере: ${r.skipped}`);
  if (msg.length) toast(msg.join(', '), 'ok');
  if (r.errors?.length) toast(r.errors.slice(0, 3).join('; ') + (r.errors.length > 3 ? ` и ещё ${r.errors.length - 3}` : ''), 'err');
  refresh();
}

$('#gRows').addEventListener('mousedown', e => {
  const tr = e.target.closest('tr[data-id]'); if (!tr) return;
  const id = tr.dataset.id;
  if (e.button === 2 && G.sel.has(id)) return;
  const list = G.files.map(f => f.id);
  if (e.shiftKey && G.anchor) {
    const a = list.indexOf(G.anchor), b = list.indexOf(id);
    G.sel = new Set(list.slice(Math.min(a, b), Math.max(a, b) + 1));
  } else if (e.ctrlKey) { G.sel.has(id) ? G.sel.delete(id) : G.sel.add(id); G.anchor = id; }
  else { G.sel = new Set([id]); G.anchor = id; }
  updateGSel();
});
// выделение меняем на месте, не перерисовывая строки, — иначе теряется двойной щелчок
function updateGSel() {
  $$('#gRows tr[data-id]').forEach(tr => tr.classList.toggle('sel', G.sel.has(tr.dataset.id)));
  renderActs();
  $('#gDown').disabled = !G.sel.size;
  $('#gDown').textContent = G.sel.size > 1 ? `⬇ Скачать (${G.sel.size})` : '⬇ Скачать';
  renderDriveDetails();
}
$('#gRows').addEventListener('dblclick', e => {
  const tr = e.target.closest('tr[data-id]'); if (!tr) return;
  const f = G.files.find(x => x.id === tr.dataset.id); if (f) driveEnter(f);
});
$('#gRows').addEventListener('contextmenu', e => {
  e.preventDefault();
  const tr = e.target.closest('tr[data-id]'); if (!tr) return;
  const f = G.files.find(x => x.id === tr.dataset.id); if (!f) return;
  const items = [];
  if (f.folder && G.sel.size <= 1) items.push(['Открыть', () => driveEnter(f)]);
  if (!f.noDown) items.push([G.sel.size > 1 ? `⬇ Скачать выбранное (${G.sel.size})` : '⬇ Скачать на компьютер', () => driveDownload()]);
  if (!(f.driveId && f.owner === 'общий диск')) {
    items.push(['Открыть в браузере', async () => { const r = await api('gdrive/open', { acc: G.acc, id: f.id }); if (!r.ok) toast(r.error, 'err'); }]);
    if (f.link) items.push(['Копировать ссылку', () => { navigator.clipboard.writeText(f.link); toast('Ссылка скопирована', 'ok'); }]);
  }
  const A = gActState();
  if (G.sec === 'trash') {
    if (A.restore || A.forever) items.push(null);
    if (A.restore) items.push(['↩ Восстановить', driveRestore]);
    if (A.forever) items.push(['✖ Удалить навсегда', driveDeleteForever, 'danger']);
  } else if (A.any) {
    items.push(null);
    if (A.ren) items.push(['✎ Переименовать', () => driveRename()]);
    if (A.move) items.push(['➜ Переместить…', () => driveMovePick(false)]);
    if (A.copy) items.push(['⧉ Копировать в…', () => driveMovePick(true)]);
    if (A.share) items.push(['👥 Доступ…', () => driveShare()]);
    items.push([A.allStar ? '☆ Снять пометку' : '★ Пометить', driveStar]);
    if (A.trash) items.push(['🗑 В корзину', driveTrash, 'danger']);
  }
  showMenu(e, items);
});
$('#gWrap').addEventListener('mousedown', e => {
  if (!e.target.closest('tr') && !e.target.closest('th') && !e.target.closest('button')) { G.sel.clear(); updateGSel(); }
});
$('#gWrap').addEventListener('scroll', e => {
  const w = e.target;
  if (G.next && !G.loading && w.scrollTop + w.clientHeight > w.scrollHeight - 200) loadDrive(true);
});
$('#gCrumbs').addEventListener('click', e => {
  const c = e.target.closest('[data-crumb]'); if (!c) return;
  const n = +c.dataset.crumb;
  if (n === G.stack.length) return;
  G.stack = G.stack.slice(0, n); loadDrive();
});
$$('#gSections button').forEach(b => b.onclick = () => {
  G.sec = b.dataset.sec; G.stack = []; G.q = ''; $('#gSearch').value = ''; G.undo = null;
  if (G.sec === 'recent') G.order = ''; else if (!G.order) G.order = 'name';
  loadDrive();
});
$('#gUp').onclick = driveUp;
$('#gReload').onclick = () => { loadDrive(); loadAbout(); };
$('#gDown').onclick = () => driveDownload();
let gSearchT = 0;
$('#gSearch').addEventListener('input', e => {
  clearTimeout(gSearchT);
  gSearchT = setTimeout(() => {
    const q = e.target.value.trim();
    if (q === G.q) return;
    G.q = q; if (q) G.stack = [];
    loadDrive();
  }, 400);
});
// ---------- управление файлами на Диске ----------
// Ссылка на файл в Google Диске держится на его ID: перемещение и переименование её не меняют.
const opId = f => f.self || f.id; // у ярлыка операции — над самим ярлыком, а не над целью
const can = (f, ch) => (f.can || '').includes(ch);
const gSelFiles = () => G.files.filter(f => G.sel.has(f.id));
const isDriveRoot = f => f.driveId && f.folder && f.owner === 'общий диск';

function gActState() {
  const sel = gSelFiles().filter(f => !isDriveRoot(f)), n = sel.length, every = ch => n > 0 && sel.every(f => can(f, ch));
  return {
    any: n > 0, n, sel,
    mkdir: !!uploadTarget(),
    ren: n === 1 && can(sel[0], 'r'),
    move: every('m') && G.sec !== 'shared',
    copy: n > 0 && sel.every(f => !f.folder && can(f, 'c')),
    share: n === 1 && can(sel[0], 's'),
    trash: every('t'),
    restore: every('u'),
    forever: every('d'),
    allStar: n > 0 && sel.every(f => f.starred),
  };
}

function renderActs() {
  const A = gActState(), trash = G.sec === 'trash';
  const show = (id, on, enabled) => { const b = $(id); b.hidden = !on; b.disabled = !enabled; };
  show('#gNew', !trash, A.mkdir);
  show('#gRen', !trash, A.ren);
  show('#gMove', !trash, A.move);
  show('#gShare', !trash, A.share);
  show('#gStar', !trash, A.any);
  show('#gTrash', !trash, A.trash);
  show('#gRestore', trash, A.restore);
  show('#gDelForever', trash, A.forever);
  show('#gEmptyTrash', trash, G.files.length > 0);
  $('#gStar').textContent = A.allStar ? '☆ Снять пометку' : '★ Пометить';
  $$('#gActs .gsep').forEach(x => x.hidden = trash);
  $('#gSelInfo').textContent = A.n > 1 ? `Выбрано: ${A.n}` : '';
}

function gSortOwner() {
  if (!G.order.startsWith('owner')) return;
  const d = G.order.endsWith('-') ? -1 : 1;
  G.files.sort((a, b) => (b.folder - a.folder) || d * (a.owner || '').localeCompare(b.owner || '', 'ru') || a.name.localeCompare(b.name, 'ru', { numeric: true }));
}
$$('.gtable th[data-gsort]').forEach(th => th.addEventListener('click', () => {
  const k = th.dataset.gsort, cur = G.order.replace(/-$/, '');
  // имя и владелец — по возрастанию, дата и размер — сначала новые/большие
  G.order = cur === k ? (G.order.endsWith('-') ? k : k + '-') : (k === 'modified' || k === 'size' ? k + '-' : k);
  if (k === 'owner' && !G.next) { gSortOwner(); renderDrive(); } else loadDrive();
}));

async function gOp(op, extra, quiet) {
  const r = await api('gdrive/op', { acc: G.acc, op, ...extra }).catch(e => ({ ok: false, error: e.message }));
  if (r.errors?.length) toast((r.done ? `Готово: ${r.done}. ` : '') + 'Не вышло: ' + r.errors.slice(0, 3).join('; ') + (r.errors.length > 3 ? ` и ещё ${r.errors.length - 3}` : ''), 'err');
  else if (!r.ok && !quiet) toast(r.error || 'Не получилось', 'err');
  return r;
}
const plural = (n, one, few, many) => n % 10 === 1 && n % 100 !== 11 ? one : n % 10 >= 2 && n % 10 <= 4 && (n % 100 < 10 || n % 100 >= 20) ? few : many;
const nItems = n => `${n} ${plural(n, 'объект', 'объекта', 'объектов')}`;
const after = sel => { G.sel.clear(); loadDrive(); if (sel) loadAbout(); };

// окно «имя»: новая папка и переименование
function askName(title, value, btn, hint) {
  return new Promise(resolve => {
    const d = $('#dlgGName'), inp = $('#gNameInput');
    $('#gNameTitle').textContent = title; $('#gNameGo').textContent = btn; $('#gNameHint').textContent = hint || '';
    inp.value = value || '';
    d.returnValue = '';
    d.showModal();
    // имя без расширения — сразу под замену, как в Проводнике
    const dot = value ? value.lastIndexOf('.') : -1;
    inp.focus(); inp.setSelectionRange(0, dot > 0 ? dot : inp.value.length);
    d.onclose = () => resolve(d.returnValue === 'ok' && inp.value.trim() ? inp.value.trim() : null);
  });
}

async function driveNewFolder(target = uploadTarget()) {
  if (!target) return toast('Откройте папку, где создать новую', 'err');
  const name = await askName('Новая папка', 'Новая папка', 'Создать', `Внутри: ${target.name}`);
  if (!name) return null;
  const r = await gOp('mkdir', { to: target.id, name });
  if (r.ok) { toast(`Папка «${name}» создана`, 'ok'); if (target === uploadTarget() || target.id === uploadTarget()?.id) loadDrive(); }
  return r.ok ? r.id : null;
}

async function driveRename() {
  const f = gSelFiles()[0]; if (!f) return;
  const name = await askName('Переименовать', f.name, 'Переименовать', 'Ссылка на файл не изменится.');
  if (!name || name === f.name) return;
  const r = await gOp('rename', { ids: [opId(f)], name });
  if (r.ok) { toast('Переименовано', 'ok'); after(); }
}

async function driveStar() {
  const A = gActState(); if (!A.any) return;
  const r = await gOp('star', { ids: A.sel.map(opId), on: !A.allStar });
  if (r.done) after();
}

async function driveTrash() {
  const A = gActState(); if (!A.trash) return;
  const pub = A.sel.filter(f => f.shared), dirs = A.sel.filter(f => f.folder);
  if (pub.length) {
    const names = pub.slice(0, 5).map(f => '«' + f.name + '»').join(', ') + (pub.length > 5 ? ` и ещё ${pub.length - 5}` : '');
    if (!confirm(`Открыт доступ: ${names}.\n\nПосле удаления ссылки на ${pub.length === 1 ? 'него' : 'них'} перестанут работать у всех, кому вы их давали (пока файл в корзине — можно вернуть).\n\nВсё равно в корзину?`)) return;
  } else if (dirs.length && !confirm(`В корзину уйдут папки вместе со всем содержимым (${nItems(A.n)}). Если внутри были файлы с открытым доступом — их ссылки тоже перестанут работать.\n\nПродолжить?`)) return;
  const ids = A.sel.map(opId);
  const r = await gOp('trash', { ids });
  if (!r.done) return;
  G.undo = { op: 'trash', ids };
  toast(`В корзине: ${nItems(r.done)}`, 'ok', ['Отменить (Ctrl+Z)', driveUndo]);
  after(true);
}

async function driveRestore() {
  const A = gActState(); if (!A.restore) return;
  const r = await gOp('untrash', { ids: A.sel.map(opId) });
  if (r.done) { toast(`Восстановлено: ${nItems(r.done)} — файлы вернулись на прежние места`, 'ok'); after(true); }
}

async function driveDeleteForever() {
  const A = gActState(); if (!A.forever) return;
  if (!confirm(`Удалить навсегда ${nItems(A.n)}? Вернуть будет нельзя, ссылки перестанут работать.`)) return;
  const r = await gOp('delete', { ids: A.sel.map(opId) });
  if (r.done) { toast(`Удалено навсегда: ${nItems(r.done)}`, 'ok'); after(true); }
}

async function driveEmptyTrash() {
  const t = G.about?.trash;
  if (!confirm(`Очистить корзину Диска${t ? ` (${size(t)})` : ''}? Всё в ней удалится навсегда.`)) return;
  const r = await gOp('empty-trash', {});
  if (r.ok) { toast('Корзина очищена', 'ok'); after(true); }
}

async function driveUndo() {
  const u = G.undo; if (!u) return toast('Отменять нечего');
  G.undo = null;
  if (u.op === 'trash') {
    const r = await gOp('untrash', { ids: u.ids });
    if (r.done) toast(`Вернул из корзины: ${nItems(r.done)}`, 'ok');
  } else if (u.op === 'move') {
    const back = {};
    for (const [id, from] of Object.entries(u.from)) (back[from] = back[from] || []).push(id);
    let done = 0;
    for (const [to, ids] of Object.entries(back)) done += (await gOp('move', { ids, to })).done || 0;
    if (done) toast(`Вернул на место: ${nItems(done)}`, 'ok');
  }
  after(true);
}

async function driveMoveTo(ids, to, toName, copy) {
  if (!ids.length || !to) return;
  const r = await gOp(copy ? 'copy' : 'move', { ids, to });
  if (!r.done) return;
  if (copy) toast(`Копия в «${toName}»: ${nItems(r.done)}`, 'ok');
  else {
    G.undo = { op: 'move', from: r.from || {} };
    toast(`Перемещено в «${toName}»: ${nItems(r.done)}`, 'ok', Object.keys(r.from || {}).length ? ['Отменить (Ctrl+Z)', driveUndo] : null);
  }
  after();
}

// ---------- выбор папки: переместить / копировать ----------
const P = { sec: 'my', stack: [], files: [], pick: null, ids: [], skip: new Set(), copy: false, seq: 0, loading: false };
function pickTarget() {
  if (P.pick) { const f = P.files.find(x => x.id === P.pick); if (f) return { id: f.id, name: f.name }; }
  const cur = P.stack[P.stack.length - 1];
  if (cur) return { id: cur.id, name: cur.name };
  return P.sec === 'my' ? { id: 'root', name: 'Мой диск' } : null;
}
async function pickLoad() {
  const seq = ++P.seq, cur = P.stack[P.stack.length - 1];
  P.loading = true; P.pick = null; P.files = []; pickRender();
  let files = [], next = '';
  for (let i = 0; i < 10; i++) { // до 2000 папок в одной папке — хватит
    const r = await api('gdrive/list', { acc: G.acc, section: P.sec, folder: cur?.id || '', driveId: cur?.driveId || '', foldersOnly: true, order: 'name', pageToken: next })
      .catch(e => ({ ok: false, error: e.message }));
    if (seq !== P.seq) return;
    if (!r.ok) { toast(r.error, 'err'); break; }
    files = files.concat(r.files.filter(f => f.folder)); next = r.next || '';
    if (!next) break;
  }
  P.files = files; P.loading = false; pickRender();
}
function pickRender() {
  $$('#gPickSec button').forEach(b => b.classList.toggle('on', b.dataset.sec === P.sec));
  const root = P.sec === 'my' ? 'Мой диск' : 'Общие диски';
  $('#gPickCrumbs').innerHTML = [`<span data-crumb="0">${esc(root)}</span>`].concat(P.stack.map((s, i) => `<i>›</i><span data-crumb="${i + 1}">${esc(s.name)}</span>`)).join('');
  $('#gPickList').innerHTML = P.loading ? '<div class="muted gpick-empty">Загружаю папки…</div>'
    : !P.files.length ? '<div class="muted gpick-empty">Вложенных папок нет — можно положить прямо сюда</div>'
    : P.files.map(f => {
      const off = P.skip.has(f.id);
      return `<div class="gpick-row${P.pick === f.id ? ' sel' : ''}${off ? ' off' : ''}" data-id="${esc(f.id)}" title="${off ? 'Это одна из перемещаемых папок' : ''}">${gIcon(f)}<span>${esc(f.name)}</span></div>`;
    }).join('');
  const t = pickTarget(), here = !t || (!P.copy && P.stack.length === 0 && P.sec === 'my' && !P.pick && G.sec === 'my' && !G.stack.length);
  $('#gPickGo').disabled = !t || here; $('#gPickCopy').disabled = !t || !P.canCopy;
  $('#gPickGo').textContent = t ? `${P.copy ? 'Копировать' : 'Переместить'} в «${t.name}»` : 'Выберите папку';
  $('#gPickCopy').hidden = P.copy;
  $('#gPickNew').disabled = !t;
}
function driveMovePick(copy) {
  const A = gActState(); if (!(copy ? A.copy : A.move)) return;
  Object.assign(P, { sec: G.sec === 'drives' ? 'drives' : 'my', stack: G.sec === 'my' || G.sec === 'drives' ? G.stack.map(s => ({ ...s })) : [],
    ids: A.sel.map(opId), skip: new Set(A.sel.filter(f => f.folder).map(f => f.id)), copy, canCopy: A.copy });
  $('#gMoveTitle').textContent = `${copy ? 'Копировать' : 'Переместить'}: ${A.n === 1 ? '«' + A.sel[0].name + '»' : nItems(A.n)}`;
  $('#dlgGMove').showModal();
  pickLoad();
}
$('#gPickList').addEventListener('click', e => {
  const r = e.target.closest('.gpick-row'); if (!r || r.classList.contains('off')) return;
  P.pick = P.pick === r.dataset.id ? null : r.dataset.id; pickRender();
});
$('#gPickList').addEventListener('dblclick', e => {
  const r = e.target.closest('.gpick-row'); if (!r || r.classList.contains('off')) return;
  const f = P.files.find(x => x.id === r.dataset.id); if (!f) return;
  const cur = P.stack[P.stack.length - 1];
  P.stack.push({ id: f.id, name: f.name, driveId: f.driveId || cur?.driveId || '' }); pickLoad();
});
$('#gPickCrumbs').addEventListener('click', e => {
  const c = e.target.closest('[data-crumb]'); if (!c) return;
  const n = +c.dataset.crumb; if (n === P.stack.length) return;
  P.stack = P.stack.slice(0, n); pickLoad();
});
$$('#gPickSec button').forEach(b => b.onclick = () => { P.sec = b.dataset.sec; P.stack = []; pickLoad(); });
$('#gPickNew').onclick = async () => {
  const t = pickTarget(); if (!t) return;
  const id = await driveNewFolder({ id: t.id, name: t.name });
  if (id) { if (P.pick) { const f = P.files.find(x => x.id === P.pick); P.stack.push({ id: f.id, name: f.name, driveId: f.driveId || '' }); } await pickLoad(); P.pick = id; pickRender(); }
};
const pickGo = copy => { const t = pickTarget(); if (!t) return; $('#dlgGMove').close(); driveMoveTo(P.ids, t.id, t.name, copy); };
$('#gPickGo').onclick = () => pickGo(P.copy);
$('#gPickCopy').onclick = () => pickGo(true);

// перетащить выбранное на папку в списке или на «хлебную крошку»
$('#gRows').addEventListener('dragstart', e => {
  const tr = e.target.closest('tr[data-id]'); if (!tr) return;
  if (!G.sel.has(tr.dataset.id)) { G.sel = new Set([tr.dataset.id]); G.anchor = tr.dataset.id; updateGSel(); }
  const A = gActState();
  if (!A.move || G.sec === 'trash') { e.preventDefault(); return; }
  G.drag = A.sel.map(opId);
  e.dataTransfer.effectAllowed = 'move';
  e.dataTransfer.setData('text/x-mairdi-drive', G.drag.join(','));
});
document.addEventListener('dragend', () => { G.drag = null; $$('.gdrop').forEach(x => x.classList.toggle('gdrop', false)); });
function dropTarget(e) {
  if (!G.drag) return null;
  const tr = e.target.closest('#gRows tr[data-id]');
  if (tr) {
    const f = G.files.find(x => x.id === tr.dataset.id);
    if (f && f.folder && !G.drag.includes(f.id) && can(f, 'a')) return { el: tr, id: f.id, name: f.name };
    return null;
  }
  const c = e.target.closest('#gCrumbs [data-crumb]');
  if (c && !G.q) {
    const n = +c.dataset.crumb; if (n === G.stack.length) return null;
    if (n === 0) return G.sec === 'my' ? { el: c, id: 'root', name: 'Мой диск' } : null;
    const s = G.stack[n - 1]; return { el: c, id: s.id, name: s.name };
  }
  return null;
}
['#gRows', '#gCrumbs'].forEach(sel => {
  $(sel).addEventListener('dragover', e => {
    const t = dropTarget(e);
    $$('.gdrop').forEach(x => x !== t?.el && x.classList.toggle('gdrop', false));
    if (t) { e.preventDefault(); e.dataTransfer.dropEffect = 'move'; t.el.classList.toggle('gdrop', true); }
  });
  $(sel).addEventListener('drop', e => {
    const t = dropTarget(e); if (!t) return;
    e.preventDefault(); e.stopPropagation();
    const ids = G.drag; G.drag = null; t.el.classList.toggle('gdrop', false);
    driveMoveTo(ids, t.id, t.name, false);
  });
});

// ---------- доступ ----------
const ROLE = { owner: 'Владелец', organizer: 'Управляющий', fileOrganizer: 'Управляет файлами', writer: 'Редактор', commenter: 'Комментатор', reader: 'Читатель' };
const SH = { f: null, perms: [], link: '' };
async function driveShare() {
  const f = gSelFiles()[0]; if (!f) return;
  Object.assign(SH, { f, perms: [], link: f.link || '' });
  $('#gShareName').textContent = f.name;
  $('#gAddEmail').value = ''; $('#gAddNotify').checked = false;
  $('#gPeople').innerHTML = '<div class="muted">Загружаю…</div>';
  $('#gLinkWarn').textContent = '';
  $('#dlgGShare').showModal();
  await shareLoad();
}
async function shareLoad() {
  const r = await api('gdrive/perms', { acc: G.acc, id: opId(SH.f) }).catch(e => ({ ok: false, error: e.message }));
  if (!r.ok) { $('#gPeople').innerHTML = `<div class="st-error">${esc(r.error)}</div>`; return; }
  SH.perms = r.perms || []; SH.link = r.link || SH.link;
  shareRender();
}
function shareRender() {
  const anyone = SH.perms.find(p => p.type === 'anyone');
  $('#gLinkRole').value = anyone ? (['reader', 'commenter', 'writer'].includes(anyone.role) ? anyone.role : 'reader') : '';
  const dom = SH.perms.find(p => p.type === 'domain');
  $('#gLinkWarn').textContent = (anyone ? 'Файл открыт по ссылке. ' : '') + (dom ? `Ещё открыт всем в домене ${dom.domain}. ` : '');
  const people = SH.perms.filter(p => p.type === 'user' || p.type === 'group');
  $('#gPeople').innerHTML = people.length ? people.map(p => {
    const who = `<div class="who"><b>${esc(p.name || p.email)}</b>${p.name && p.email ? `<span class="muted">${esc(p.email)}</span>` : ''}</div>`;
    if (p.role === 'owner' || p.role === 'organizer') return `<div class="gperson">${who}<span class="muted">${ROLE[p.role]}</span></div>`;
    const opts = ['reader', 'commenter', 'writer'].map(r => `<option value="${r}"${r === p.role ? ' selected' : ''}>${ROLE[r]}</option>`).join('');
    return `<div class="gperson" data-perm="${esc(p.id)}">${who}<select>${opts}${ROLE[p.role] && !['reader', 'commenter', 'writer'].includes(p.role) ? `<option selected>${ROLE[p.role]}</option>` : ''}</select><button type="button" class="link" title="Закрыть доступ">✕</button></div>`;
  }).join('') : '<div class="muted">Никому лично не открыт</div>';
}
$('#gLinkRole').onchange = async e => {
  const role = e.target.value, was = SH.perms.find(p => p.type === 'anyone');
  if (!role && was && !confirm('Закрыть доступ по ссылке? Все, кому вы давали ссылку, больше не смогут открыть файл (люди из списка ниже — смогут).')) { shareRender(); return; }
  const r = await api('gdrive/perm-link', { acc: G.acc, id: opId(SH.f), role });
  if (!r.ok) toast(r.error, 'err'); else toast(role ? 'Доступ по ссылке: ' + ROLE[role].toLowerCase() : 'Доступ по ссылке закрыт', 'ok');
  await shareLoad(); SH.f.shared = SH.perms.length > 1;
};
$('#gLinkCopy').onclick = async () => { if (SH.link && await copyText(SH.link)) toast('Ссылка скопирована', 'ok'); };
$('#gPeople').addEventListener('change', async e => {
  const row = e.target.closest('[data-perm]'); if (!row || e.target.tagName !== 'SELECT') return;
  const r = await api('gdrive/perm-change', { acc: G.acc, id: opId(SH.f), perm: row.dataset.perm, role: e.target.value });
  if (!r.ok) toast(r.error, 'err');
  shareLoad();
});
$('#gPeople').addEventListener('click', async e => {
  const b = e.target.closest('button'), row = e.target.closest('[data-perm]'); if (!b || !row) return;
  const who = row.querySelector('.who b').textContent;
  if (!confirm(`Закрыть доступ для ${who}?`)) return;
  const r = await api('gdrive/perm-change', { acc: G.acc, id: opId(SH.f), perm: row.dataset.perm, role: '' });
  if (!r.ok) toast(r.error, 'err');
  shareLoad();
});
$('#gAddGo').onclick = async () => {
  const email = $('#gAddEmail').value.trim(); if (!email) return $('#gAddEmail').focus();
  const r = await api('gdrive/perm-add', { acc: G.acc, id: opId(SH.f), email, role: $('#gAddRole').value, notify: $('#gAddNotify').checked });
  if (!r.ok) return toast(r.error, 'err');
  toast(`Доступ открыт: ${email}`, 'ok'); $('#gAddEmail').value = '';
  shareLoad();
};
$('#gAddEmail').addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); $('#gAddGo').click(); } });
$('#dlgGShare').addEventListener('close', () => loadDrive());

$('#gNameInput').addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); $('#dlgGName').close('ok'); } });
$('#gNew').onclick = () => driveNewFolder();
$('#gRen').onclick = driveRename;
$('#gMove').onclick = () => driveMovePick(false);
$('#gShare').onclick = driveShare;
$('#gStar').onclick = driveStar;
$('#gTrash').onclick = driveTrash;
$('#gRestore').onclick = driveRestore;
$('#gDelForever').onclick = driveDeleteForever;
$('#gEmptyTrash').onclick = driveEmptyTrash;

// ---------- загрузка с компьютера на Диск ----------
// Куда загружать: открытая папка, корень «Моего диска» или общего диска. В «Недавних», поиске и т. п. — некуда.
function uploadTarget() {
  if (!G.acc || G.q) return null;
  const f = curFolder();
  const name = [GSEC[G.sec], ...G.stack.map(s => s.name)].join(' › ');
  if (f) return { id: f.id, name };
  if (G.sec === 'my') return { id: 'root', name };
  return null;
}
function needTarget() {
  const t = uploadTarget();
  if (!t) toast('Откройте на Диске папку, куда загружать (в «Моём диске» или общем диске)', 'err');
  return t;
}
function upToast(r) {
  if (r.cancel) return;
  const msg = [];
  if (r.added) msg.push(`На Диск: файлов ${r.added}`);
  if (r.folders) msg.push(`папок ${r.folders}`);
  if (msg.length) toast(msg.join(', ') + ' — ход загрузки в «Отправлено на Диск»', 'ok');
  if (r.errors?.length) toast(r.errors.slice(0, 3).join('; '), 'err');
  refresh();
}
async function drivePick(mode) {
  const t = needTarget(); if (!t) return;
  upToast(await api('gdrive/pick-upload', { acc: G.acc, parent: t.id, parentName: t.name, mode }));
}
// «🔗 По ссылке»: скачать по ссылке прямо в открытую папку Диска
function driveLink(text) {
  const t = needTarget(); if (!t) return;
  openAdd(text || '', { drive: { acc: G.acc, id: t.id, name: t.name } });
}
$('#gLink').onclick = () => driveLink('');
$('#gUpFiles').onclick = () => drivePick('files');
$('#gUpDir').onclick = () => drivePick('folder');

// Перетащили файлы или папки из Проводника: копируем их во временную папку программы
// (окно не знает настоящих путей) и ставим в очередь — дальше обычная загрузка с докачкой.
async function driveDrop(dt) {
  const t = needTarget(); if (!t) return;
  const entries = [...dt.items].map(i => i.webkitGetAsEntry && i.webkitGetAsEntry()).filter(Boolean);
  const files = [], tops = [];
  const walk = async (ent, rel) => {
    if (ent.isFile) {
      const f = await new Promise((ok, no) => ent.file(ok, no));
      files.push({ f, rel: rel + f.name });
    } else if (ent.isDirectory) {
      const rd = ent.createReader();
      let batch;
      do {
        batch = await new Promise((ok, no) => rd.readEntries(ok, no));
        for (const c of batch) await walk(c, rel + ent.name + '/');
      } while (batch.length);
      if (!files.some(x => x.rel.startsWith(rel + ent.name + '/'))) files.push({ dir: true, rel: rel + ent.name + '/' });
    }
  };
  for (const ent of entries) { tops.push(ent.name); await walk(ent, ''); }
  const real = files.filter(x => !x.dir);
  const total = real.reduce((s, x) => s + x.f.size, 0);
  const batch = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  toast(`Готовлю к загрузке: ${real.length} файлов, ${size(total) || '0 Б'}…`);
  let n = 0;
  for (const x of real) {
    const r = await fetch(`/spool?batch=${encodeURIComponent(batch)}&rel=${encodeURIComponent(x.rel)}`, {
      method: 'PUT', headers: { 'X-Token': window.TOKEN }, body: x.f,
    }).then(r => r.json()).catch(e => ({ ok: false, error: e.message }));
    if (!r.ok) { toast(`Не удалось прочитать «${x.f.name}»: ${r.error}`, 'err'); continue; }
    n++;
  }
  for (const x of files.filter(x => x.dir)) { // пустые папки тоже переносим
    await fetch(`/spool?batch=${encodeURIComponent(batch)}&rel=${encodeURIComponent(x.rel)}&dir=1`, { method: 'PUT', headers: { 'X-Token': window.TOKEN }, body: '' });
  }
  if (!n && !files.some(x => x.dir)) return;
  upToast(await api('gdrive/spool-commit', { acc: G.acc, parent: t.id, parentName: t.name, batch, tops }));
}

// «Отправить → Google Диск» из Проводника: файлы ждут, пока выберут папку на Диске.
function renderPending() {
  const p = S?.g?.pending || [];
  const bar = $('#gPending');
  bar.hidden = !p.length || !view.startsWith('gd:');
  if (bar.hidden) return;
  const names = p.map(x => x.split('\\').pop());
  const t = uploadTarget();
  $('#gPendingText').textContent = `Из Проводника: ${names.slice(0, 3).join(', ')}${names.length > 3 ? ` и ещё ${names.length - 3}` : ''} → ` +
    (t ? `в «${t.name}»` : 'откройте папку, куда загрузить');
  $('#gPendingGo').disabled = !t;
}
$('#gPendingGo').onclick = async () => {
  const t = needTarget(); if (!t) return;
  upToast(await api('gdrive/pending-commit', { acc: G.acc, parent: t.id, parentName: t.name }));
};
$('#gPendingNo').onclick = () => api('gdrive/pending-cancel').then(refresh);
let sawPending = 0;
function pendingWatch() {
  const n = (S?.g?.pending || []).length;
  if (n && !sawPending && !view.startsWith('gd:')) {
    const acc = (S.g.accounts || [])[0];
    if (!acc) toast('Чтобы отправлять на Google Диск, подключите его слева внизу', 'err');
    else { view = 'gd:' + acc.email; openDrive(acc.email); }
  }
  sawPending = n;
  renderPending();
}

// Загрузка закончилась в открытой папке — обновляем список, чтобы файл сразу был виден.
const upSeen = new Set();
let upReload = 0;
function uploadsWatch() {
  for (const j of jobs) {
    if (j.kind !== 'upload' || (j.status !== 'done' && j.status !== 'duplicate') || upSeen.has(j.id)) continue;
    upSeen.add(j.id);
    const t = uploadTarget();
    if (upSeen.ready && t && t.id === j.gParent && view === 'gd:' + j.gAcc) {
      clearTimeout(upReload); upReload = setTimeout(() => loadDrive(), 800);
    }
  }
  upSeen.ready = true;
}

function driveKey(e) {
  const k = e.key.toLowerCase(), A = gActState();
  if (e.ctrlKey && e.shiftKey && k === 'n') { e.preventDefault(); if (A.mkdir) driveNewFolder(); return; }
  if (e.ctrlKey && k === 'z') { e.preventDefault(); driveUndo(); return; }
  if (e.key === 'F2') { e.preventDefault(); if (A.ren) driveRename(); return; }
  if (e.key === 'Delete') { e.preventDefault(); if (G.sec === 'trash') { if (A.forever) driveDeleteForever(); } else if (A.trash) driveTrash(); return; }
  if (e.key === 'Backspace') { e.preventDefault(); driveUp(); }
  else if (e.key === 'F5') { e.preventDefault(); loadDrive(); loadAbout(); }
  else if (e.key === 'Enter') {
    const sel = G.files.filter(f => G.sel.has(f.id));
    if (sel.length === 1) driveEnter(sel[0]); else if (sel.length) driveDownload();
  } else if (e.ctrlKey && e.key.toLowerCase() === 'a') { e.preventDefault(); G.sel = new Set(G.files.map(f => f.id)); updateGSel(); }
  else if (e.ctrlKey && e.key.toLowerCase() === 'f') { e.preventDefault(); $('#gSearch').focus(); }
  else if (e.ctrlKey && e.key.toLowerCase() === 'n') { e.preventDefault(); openAdd(''); }
}

window.addEventListener('resize', () => renderDetails());
refresh();
setInterval(refresh, 1000);
