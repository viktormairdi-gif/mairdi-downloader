'use strict';
const $ = s => document.querySelector(s);
const msg = m => new Promise(r => chrome.runtime.sendMessage(m, r));
const hostOf = u => { try { return new URL(u).hostname.replace(/^www\./, '').toLowerCase(); } catch { return ''; } };

(async () => {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  const url = (tab && tab.url) || '';
  const host = hostOf(url);
  const o = await chrome.storage.local.get({ intercept: true, excluded: [] });
  $('#intercept').checked = o.intercept;
  $('#exclude').checked = !!host && o.excluded.includes(host);
  $('#exclude').disabled = !host;
  $('#site').textContent = host ? 'Сайт: ' + host : '';

  const st = $('#st');
  const p = await msg({ type: 'ping', start: false });
  if (p && p.ok) { st.textContent = 'на связи'; st.className = 'ok'; }
  else if (p && p.noHost) { st.textContent = 'не установлена'; st.className = 'bad'; $('#msg').textContent = 'Запустите MAIRDI Загрузчик хотя бы один раз — он сам подключит расширение.'; }
  else { st.textContent = 'не запущена'; st.className = 'bad'; }

  const v = await msg({ type: 'isVideo', url });
  $('#videoBox').hidden = !(v && v.video);
  // «Сразу на Google Диск» — только если Диск подключён в программе; выбор запоминаем
  const drive = $('label.drive'), toDrive = $('.toDrive');
  drive.hidden = !(p && p.drive);
  toDrive.checked = !!(await chrome.storage.local.get({ toDrive: false })).toDrive;
  toDrive.onchange = e => chrome.storage.local.set({ toDrive: e.target.checked });
  const wantDrive = () => !drive.hidden && toDrive.checked;

  $('#intercept').onchange = e => chrome.storage.local.set({ intercept: e.target.checked });
  $('#exclude').onchange = async e => {
    const cur = (await chrome.storage.local.get({ excluded: [] })).excluded.filter(x => x !== host);
    if (e.target.checked) cur.push(host);
    chrome.storage.local.set({ excluded: cur });
  };
  const done = r => { $('#msg').textContent = r && r.ok ? (r.duplicate ? 'Уже скачано раньше — повторно не качаю' : wantDrive() ? 'Скачаю и загружу на Google Диск ✔' : 'Добавлено в загрузчик ✔') : 'Ошибка: ' + ((r && r.error) || 'нет ответа'); };
  $('#dlVideo').onclick = async () => { $('#msg').textContent = 'Отправляю…'; done(await msg({ type: 'video', url, quality: $('#q').value, toDrive: wantDrive() })); };
  $('#dlPageVideo').onclick = async () => { $('#msg').textContent = 'Отправляю…'; done(await msg({ type: 'video', url })); };
  $('#dlAll').onclick = async () => { await msg({ type: 'all', tab }); window.close(); };
  $('#open').onclick = async () => { await msg({ type: 'open' }); window.close(); };
})();
