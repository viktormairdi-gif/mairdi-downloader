'use strict';
// MAIRDI Загрузчик — фоновая часть расширения.
// Связь с программой — через native messaging (без открытых портов).

const HOST = 'kg.mairdi.downloader';
const VIDEO_HOSTS = ['youtube.com', 'youtu.be', 'instagram.com', 'tiktok.com', 'vk.com', 'vkvideo.ru', 'vk.ru', 'rutube.ru',
  'facebook.com', 'fb.watch', 'x.com', 'twitter.com', 'vimeo.com', 'dailymotion.com', 'ok.ru', 'twitch.tv', 'soundcloud.com',
  'reddit.com', 'bilibili.com', 'dzen.ru', 'kick.com', 'threads.net', 'coub.com', 'pinterest.com', 't.me'];
// Сайты, которым не передаём вход: YouTube может заблокировать аккаунт за скачивание с cookies.
const NO_COOKIE_HOSTS = ['youtube.com', 'youtu.be', 'google.com'];

const EXT_VER = chrome.runtime.getManifest().version;

function send(msg) {
  msg = { ...msg, extVer: EXT_VER }; // программа видит, что расширение стоит и какой версии
  return new Promise(resolve => {
    try {
      chrome.runtime.sendNativeMessage(HOST, msg, r => {
        if (chrome.runtime.lastError) resolve({ ok: false, error: chrome.runtime.lastError.message, noHost: true });
        else resolve(r || { ok: false });
      });
    } catch (e) { resolve({ ok: false, error: String(e), noHost: true }); }
  });
}

const hostOf = u => { try { return new URL(u).hostname.replace(/^www\./, '').toLowerCase(); } catch { return ''; } };
const matchHost = (h, list) => list.some(x => h === x || h.endsWith('.' + x));
const isVideoPage = u => matchHost(hostOf(u), VIDEO_HOSTS);

async function opts() {
  return chrome.storage.local.get({ intercept: true, excluded: [], minKB: 0 });
}

async function cookieHeader(url) {
  try {
    const list = await chrome.cookies.getAll({ url });
    return list.map(c => `${c.name}=${c.value}`).join('; ');
  } catch { return ''; }
}
async function cookieList(url) {
  if (matchHost(hostOf(url), NO_COOKIE_HOSTS)) return [];
  try {
    return (await chrome.cookies.getAll({ url })).map(c => ({
      name: c.name, value: c.value, domain: c.domain, path: c.path, secure: c.secure, httpOnly: c.httpOnly,
      expirationDate: c.expirationDate || 0,
    }));
  } catch { return []; }
}

function notify(title, message) {
  chrome.notifications.create({ type: 'basic', iconUrl: 'icons/icon128.png', title, message, priority: 0 });
}

// ---------- перехват загрузок браузера ----------
chrome.downloads.onCreated.addListener(async item => {
  if (item.byExtensionId === chrome.runtime.id) return; // наш же запасной вариант
  const url = item.finalUrl || item.url || '';
  if (!/^(https?|ftp):/i.test(url)) return;            // blob:, data:, file: — браузер справится сам
  if (item.state && item.state !== 'in_progress') return;
  const o = await opts();
  if (!o.intercept) return;
  const page = item.referrer || '';
  const h = hostOf(page) || hostOf(url);
  if (o.excluded.some(x => h === x || h.endsWith('.' + x))) return;
  if (o.minKB > 0 && item.totalBytes > 0 && item.totalBytes < o.minKB * 1024) return;

  // Ставим браузерную закачку на паузу, пока программа не подтвердит, что взяла файл.
  try { await chrome.downloads.pause(item.id); } catch { }
  const res = await send({
    type: 'add', source: 'intercept', url, referrer: page, pageUrl: page,
    filename: item.filename ? item.filename.split(/[\\/]/).pop() : '',
    cookies: await cookieHeader(url), userAgent: navigator.userAgent, mode: 'file',
  });
  if (res && res.ok) {
    try { await chrome.downloads.cancel(item.id); } catch { }
    try { await chrome.downloads.erase({ id: item.id }); } catch { }
    if (res.duplicate) notify('Уже скачано', `${res.name || 'Файл'} уже есть: ${res.path || ''}`);
  } else {
    try { await chrome.downloads.resume(item.id); } catch { } // программа не запущена или отказалась — качает браузер
    if (res && res.noHost) badge('!');
  }
});

function badge(text) {
  chrome.action.setBadgeText({ text });
  chrome.action.setBadgeBackgroundColor({ color: '#D64545' });
}

// ---------- контекстное меню ----------
function buildMenus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: 'link', title: 'Скачать через MAIRDI Загрузчик', contexts: ['link', 'image', 'video', 'audio'] });
    chrome.contextMenus.create({ id: 'video', title: 'Скачать видео с этой страницы', contexts: ['page', 'video', 'frame'] });
    chrome.contextMenus.create({ id: 'audio', title: 'Скачать только звук (MP3)', contexts: ['page', 'video'] });
    chrome.contextMenus.create({ id: 'linkvideo', title: 'Скачать видео по ссылке', contexts: ['link'] });
    chrome.contextMenus.create({ id: 'all', title: 'Скачать все файлы со страницы…', contexts: ['page'] });
    // «в Google Диск»: программа скачает, разложит в «Загрузки\<папка>» на Диске и уберёт копию с компьютера
    chrome.contextMenus.create({ id: 'sepDrive', type: 'separator', contexts: ['link', 'image', 'video', 'audio', 'page'] });
    chrome.contextMenus.create({ id: 'link-drive', title: 'Скачать в Google Диск', contexts: ['link', 'image', 'video', 'audio'] });
    chrome.contextMenus.create({ id: 'video-drive', title: 'Скачать видео в Google Диск', contexts: ['page', 'video', 'frame'] });
    chrome.contextMenus.create({ id: 'linkvideo-drive', title: 'Скачать видео по ссылке в Google Диск', contexts: ['link'] });
  });
}
// Сразу после установки/обновления и при запуске браузера — «я на месте» (программу не запускаем).
const hello = () => send({ type: 'ping', start: false });
chrome.runtime.onInstalled.addListener(() => { buildMenus(); hello(); });
chrome.runtime.onStartup.addListener(() => { buildMenus(); hello(); });

async function addVideo(url, pageUrl, quality, toDrive) {
  const r = await send({
    type: 'add', source: 'menu', mode: 'video', url, pageUrl, quality: quality || '',
    cookieList: await cookieList(url), userAgent: navigator.userAgent, toDrive: toDrive ? '1' : '',
  });
  report(r, toDrive ? 'Видео скачается и уйдёт на Google Диск' : 'Видео добавлено в загрузчик');
  return r;
}

function report(r, okText) {
  if (r && r.ok) {
    chrome.action.setBadgeText({ text: '' });
    if (r.duplicate) notify('Уже скачано', `${r.name || 'Файл'} уже есть — повторно не качаю`);
    else notify('MAIRDI Загрузчик', okText);
  } else notify('MAIRDI Загрузчик', 'Не получилось: ' + ((r && r.error) || 'программа не отвечает'));
}

async function addLink(url, page, toDrive) {
  const r = await send({
    type: 'add', source: 'menu', url, referrer: page, pageUrl: page,
    cookies: await cookieHeader(url), userAgent: navigator.userAgent, toDrive: toDrive ? '1' : '',
  });
  report(r, toDrive ? 'Скачаю и загружу на Google Диск' : 'Закачка добавлена');
}

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  const page = info.pageUrl || (tab && tab.url) || '';
  if (info.menuItemId === 'link' || info.menuItemId === 'link-drive') {
    const url = info.linkUrl || info.srcUrl;
    if (!url) return;
    await addLink(url, page, info.menuItemId === 'link-drive');
  } else if (info.menuItemId === 'video-drive') {
    await addVideo(page, page, '', true);
  } else if (info.menuItemId === 'linkvideo-drive' && info.linkUrl) {
    await addVideo(info.linkUrl, page, '', true);
  } else if (info.menuItemId === 'video') {
    await addVideo(page, page);
  } else if (info.menuItemId === 'audio') {
    await addVideo(page, page, 'audio');
  } else if (info.menuItemId === 'linkvideo' && info.linkUrl) {
    await addVideo(info.linkUrl, page);
  } else if (info.menuItemId === 'all' && tab) {
    await downloadAllFromTab(tab);
  }
});

// Все ссылки на файлы со страницы (архивы, документы, установщики, медиа).
const FILE_RE = /\.(zip|rar|7z|tar|gz|exe|msi|iso|apk|pdf|docx?|xlsx?|csv|pptx?|mp4|mkv|avi|mov|mp3|flac|torrent|epub|fb2|djvu)(\?|#|$)/i;
async function downloadAllFromTab(tab) {
  let result = [];
  try {
    const res = await chrome.scripting.executeScript({ target: { tabId: tab.id }, func: () => [...document.links].map(a => a.href) });
    result = (res && res[0] && res[0].result) || [];
  } catch { return notify('MAIRDI Загрузчик', 'Эту страницу браузер не даёт прочитать'); }
  const urls = [...new Set((result || []).filter(u => FILE_RE.test(u) || u.startsWith('magnet:')))];
  if (!urls.length) return notify('MAIRDI Загрузчик', 'На странице не найдено ссылок на файлы');
  const r = await send({ type: 'add', source: 'menu', urls, referrer: tab.url, pageUrl: tab.url, userAgent: navigator.userAgent });
  report(r, `Добавлено файлов: ${(r.ids || []).length}`);
}

// ---------- сообщения из всплывающего окна ----------
chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
  (async () => {
    switch (msg.type) {
      case 'ping': reply(await send({ type: 'ping', start: !!msg.start })); break;
      case 'open': reply(await send({ type: 'open' })); break;
      case 'video': reply(await addVideo(msg.url, msg.url, msg.quality, msg.toDrive)); break;
      case 'all': reply(await downloadAllFromTab(msg.tab)); break;
      case 'isVideo': reply({ video: isVideoPage(msg.url) }); break;
    }
  })();
  return true;
});
