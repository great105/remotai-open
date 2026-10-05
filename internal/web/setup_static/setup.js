// Remotai — setup wizard + control panel (vanilla JS).
//
// Два режима одной страницы:
//  - не настроено → мастер первого запуска (шаги 0–4);
//  - настроено   → панель управления (#panel): статус облака, подключение
//    телефона по требованию, отключение облака, версия и обновления.
(function() {
  'use strict';

  let currentStep = 0;
  const totalSteps = 5;
  let state = {
    mode: 'central_bot',
    bot_token: '',
    allowed_users: '',
    capabilities: null,
    completionData: null,
    tunnelMode: 'quick',
    tunnelInstalled: false,
    tunnelURL: '',
    cloudPairConfirmed: false,
    version: '',
    configured: false,
    access: null,
    pairAutoShown: false,
    tunnelBound: false,
    completionStarted: false,
    lastStatus: null,
    // Шаг «AI-агенты»: что можно поставить, раскрыт ли блок установки.
    agentsInstallable: [],
    agentsInstallOpen: false,
    agentsBound: false,
    // Пользователь в режиме локальной сети нажал «Подключить через интернет»:
    // после подтверждения кода надо перевести ПК в облачный режим.
    cloudEnablePending: false,
    // Человек сам раскрыл локальный доступ (QR по Wi-Fi): до этого момента
    // облачному компьютеру не о чем говорить с брандмауэром.
    lanHelpRequested: false,
    lanPairShown: false,
  };

  // ── API helpers ──────────────────────────────────────────────

  async function api(method, path, body) {
    const opts = {
      method,
      headers: { 'Content-Type': 'application/json' },
    };
    if (body) opts.body = JSON.stringify(body);
    const res = await fetch(path, opts);
    if (!res.ok) {
      const err = await res.json().catch(() => ({ error: res.statusText }));
      // Машинный код (jsonErrorCode) важнее английской фразы: по нему
      // friendlyErr выбирает русский текст.
      const e = new Error(err.error || 'Request failed');
      e.code = err.code || '';
      e.status = res.status;
      throw e;
    }
    return res.json();
  }

  function el(id) { return document.getElementById(id); }

  // Пути к бинарникам и команды установки уходят в innerHTML — экранируем.
  function escapeHtml(value) {
    return String(value == null ? '' : value)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  // Переводит сырую ошибку релея/сети в понятную пользователю фразу.
  // fallback — текст, когда ни одно правило не сработало (у пейринга свой).
  function friendlyErr(e, fallback) {
    const code = e && e.code ? String(e.code) : '';
    if (code === 'autostart_failed') return 'Не удалось изменить автозапуск. Повторите попытку или откройте Remotai вручную после входа в систему.';
    if (code === 'port_busy') return 'Этот порт уже занят другой программой. Выберите другой свободный порт.';
    if (code === 'not_paired') return 'Компьютер ещё не добавлен в аккаунт. Отсканируйте QR-код в приложении.';
    const m = (e && e.message ? e.message : String(e || '')).toLowerCase();
    if (m.includes('already paired') || m.includes('conflict')) return 'Этот компьютер уже находится в другом аккаунте. Нажмите «Удалить этот компьютер из аккаунта» ниже, затем повторите.';
    if (m.includes('device limit')) return 'Достигнут лимит компьютеров для вашего тарифа.';
    if (m.includes('expired') || m.includes('gone')) return 'Код устарел. Получите новый.';
    // «Failed to fetch» на loopback-эндпоинте — это НЕ отсутствие интернета:
    // не ответила сама программа на этом компьютере (перезапуск, смена порта,
    // передача порта основному серверу после настройки).
    if (m.includes('failed to fetch') || m.includes('load failed')) return 'Remotai на этом компьютере не отвечает — возможно, программа перезапускается. Подождите пару секунд и повторите.';
    if (m.includes('unreachable') || m.includes('connection') || m.includes('dial') || m.includes('no such host') || m.includes('timeout') || m.includes('network')) return 'Нет связи с интернетом. Проверьте подключение и попробуйте снова.';
    if (m.includes('too many') || m.includes('rate') || m.includes('429')) return 'Слишком часто. Подождите минуту и попробуйте снова.';
    return fallback || 'Что-то пошло не так. Попробуйте ещё раз.';
  }

  // ── Toast ────────────────────────────────────────────────────

  function showToast(msg, isError) {
    let t = document.querySelector('.toast');
    if (!t) {
      t = document.createElement('div');
      t.className = 'toast';
      document.body.appendChild(t);
    }
    t.textContent = msg;
    t.style.background = isError ? 'var(--error)' : 'var(--accent)';
    t.style.color = isError ? '#2b0707' : 'var(--on-accent)';
    t.classList.add('show');
    clearTimeout(t._hideTimer);
    t._hideTimer = setTimeout(() => t.classList.remove('show'), 2500);
  }

  // ── Внешние ссылки ───────────────────────────────────────────
  // Окно программы — WebView2 без обработчика новых окон: клик по ссылке с
  // target="_blank" не открывает ни браузер, ни Telegram. Перехватываем такие
  // клики и просим программу открыть адрес в браузере пользователя; если не
  // вышло (например, страница открыта не на самом компьютере) — кладём ссылку
  // в буфер обмена и честно об этом говорим.
  function bindExternalLinks() {
    document.addEventListener('click', event => {
      if (event.defaultPrevented || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey) return;
      const link = event.target && event.target.closest ? event.target.closest('a[target="_blank"]') : null;
      if (!link) return;
      const href = link.getAttribute('href') || '';
      if (!/^https?:/i.test(href)) return;
      event.preventDefault();
      void openExternalLink(link.href);
    });
  }

  async function openExternalLink(url) {
    try {
      await api('POST', '/api/setup/open-external', { url });
      showToast('Открываем ссылку в браузере');
    } catch (e) {
      if (navigator.clipboard) {
        try {
          await navigator.clipboard.writeText(url);
          showToast('Скопировали ссылку — вставьте её в браузере', true);
          return;
        } catch (copyErr) { /* буфер недоступен — покажем обычную ошибку */ }
      }
      showToast(friendlyErr(e, 'Не удалось открыть ссылку. Откройте её в браузере вручную.'), true);
    }
  }

  // ── Version badges ───────────────────────────────────────────

  function renderVersion(v) {
    state.version = v || '';
    const label = v && v !== 'dev' && !v.startsWith('local-') ? 'v' + v : (v || '—');
    const badge = el('ver-badge');
    if (badge) badge.textContent = label;
    const foot = el('foot-ver');
    if (foot) foot.textContent = 'Remotai ' + label;
    const pv = el('p-version');
    if (pv) pv.textContent = label;
  }

  // ── Cloud pairing (общая фабрика для мастера и панели) ───────
  // Поколение (gen) защищает от утечки интервалов: только последний start
  // создаёт опрос, устаревшие вызовы после await молча выходят.

  // Подтверждение пейринга приходит по /api/setup/cloud/pair-status, но ровно в
  // этот момент setup-сервер может гаснуть (порт уходит основному серверу), и
  // запрос падает как «нет сети». Проверка по статусу агента: подключённый релей
  // = код уже подтверждён.
  async function pairingWentThrough() {
    try {
      const st = await api('GET', '/api/setup/status');
      state.lastStatus = st;
      return !!(st.relay_configured && st.relay_connected);
    } catch (e) {
      return false;
    }
  }

  function createPairing(ids, onConfirmed) {
    let gen = 0;
    let timer = null;
    let code = '';
    let done = false;
    let active = false;
    let startTask = null;
    let fails = 0;

    function stop() {
      if (timer) { clearInterval(timer); timer = null; }
      const accountEl = el(ids.account);
      if (accountEl) {
        accountEl.removeAttribute('href');
        accountEl.classList.add('hidden');
      }
    }

    /**
     * Кода нет (не дошли до релея) — прячем всё, что предлагает им
     * воспользоваться: инструкцию, QR и строку с самим кодом. Взамен
     * показываем рабочий выход, который интернета не требует.
     */
    function setPairingUnavailable(off) {
      const block = el(ids.qr)?.closest('.pairing-block');
      if (!block) return;
      block.classList.toggle('pair-offline', !!off);
    }

    async function start() {
      const g = ++gen;
      stop();
      done = false;
      active = true;
      code = '';
      fails = 0;
      const codeEl = el(ids.code), statusEl = el(ids.status),
            expiryEl = el(ids.expiry), qrEl = el(ids.qr),
            tgRowEl = el(ids.tgRow), tgLinkEl = el(ids.tgLink);
      if (codeEl) codeEl.textContent = '────────';
      if (qrEl) qrEl.removeAttribute('src');
      if (tgRowEl) tgRowEl.classList.add('hidden');
      if (statusEl) statusEl.textContent = 'Запрашиваем код…';
      const task = (async () => {
        const resp = await api('POST', '/api/setup/cloud/pair-request', {});
        if (g !== gen) return;
        code = resp.code;
        if (codeEl) codeEl.textContent = resp.code;
        const accountEl = el(ids.account);
        if (accountEl && resp.account_url) {
          accountEl.href = resp.account_url;
          accountEl.classList.remove('hidden');
        }
        if (qrEl && resp.qr_url) {
          qrEl.src = resp.qr_url;
          // Подвести окно к коду, если он не помещается целиком. Замер аудита
          // онбординга 30.08.2026: в окне 1366×768 видно было −18 % высоты QR
          // (то есть ничего), остальное закрывала липкая полоса статуса. Само
          // окно листается, но человек об этом не знает: он ждёт код, а видит
          // карточки выбора способа.
          requestAnimationFrame(() => {
            const target = accountEl && resp.account_url ? accountEl : qrEl;
            const box = target.getBoundingClientRect();
            const dock = document.querySelector('.pair-dock');
            const limit = dock ? dock.getBoundingClientRect().top : window.innerHeight;
            if (box.bottom > limit || box.top < 0) target.scrollIntoView({ block: 'center', behavior: 'smooth' });
          });
        }
        if (tgRowEl && tgLinkEl && resp.bot_link) {
          tgLinkEl.href = resp.bot_link;
          try {
            tgLinkEl.textContent = '@' + new URL(resp.bot_link).pathname.replace(/^\//, '');
          } catch (e) {
            tgLinkEl.textContent = 'Telegram-бота';
          }
          tgRowEl.classList.remove('hidden');
        }
        if (expiryEl) {
          const exp = new Date(resp.expires_at);
          expiryEl.textContent = 'Код действует до ' + exp.toLocaleTimeString();
        }
        // Это СТАТУС, а не инструкция: «отсканируйте» написано подписью под самим
        // QR-кодом, и повторять её здесь — печатать одно и то же дважды.
        if (statusEl) statusEl.textContent = '⏳ Ждём подтверждения в аккаунте…';
        setPairingUnavailable(false);
        timer = setInterval(() => poll(g), 2000);
      })();
      startTask = task;
      try {
        await task;
      } catch (e) {
        if (g !== gen) return;
        active = false;
        if (statusEl) statusEl.textContent = '❌ ' + friendlyErr(e, 'Что-то пошло не так. Попробуйте «Получить новый код».');
        // Кода нет — значит и сканировать нечего. Инструкцию «наведите камеру»
        // вместе с пустым квадратом QR убираем и называем рабочий выход:
        // локальная сеть интернета не требует вовсе (аудит онбординга
        // 30.08.2026 — шаг продолжал уговаривать сканировать несуществующий
        // код). Вернётся связь и код — вернётся и блок.
        setPairingUnavailable(true);
      } finally {
        if (startTask === task) startTask = null;
      }
    }

    async function cancel() {
      const shouldCancel = active || !!startTask;
      ++gen; // Ответ уже начатого pair-request больше не должен оживить UI.
      stop();
      const pendingStart = startTask;
      if (pendingStart) {
        try { await pendingStart; } catch (e) { /* cancel below is still useful */ }
      }
      if (shouldCancel) {
        try {
          await api('POST', '/api/setup/cloud/cancel', {});
        } catch (e) {
          // Оставляем попытку активной, чтобы повторный выбор режима снова
          // отправил cancel, а не пропустил серверную отмену после сетевой ошибки.
          active = true;
          throw e;
        }
      }
      active = false;
      done = false;
      code = '';
    }

    async function poll(g) {
      if (!code || done || g !== gen) return;
      const statusEl = el(ids.status);
      try {
        const st = await api('GET', '/api/setup/cloud/pair-status?code=' + encodeURIComponent(code));
        if (done || g !== gen) return;
        fails = 0;
        if (st.confirmed) {
          markConfirmed(g, statusEl);
        } else if (st.expired) {
          active = false;
          stop();
          if (statusEl) statusEl.textContent = '⌛ Код устарел. Получите новый.';
        } else if (statusEl) {
          // Успешный poll — связь есть: сбрасываем текст сбоя («⚠️ Нет связи»),
          // иначе он зависает навсегда даже после восстановления сети.
          statusEl.textContent = '⏳ Ждём подтверждения в аккаунте…';
        }
      } catch (e) {
        if (done || g !== gen) return;
        fails++;
        // Обрыв ровно в секунду успеха — штатный сценарий: подтверждение гасит
        // setup-сервер, он уступает порт основному. Прежде чем ругаться на сеть,
        // спрашиваем состояние агента: если ПК уже в облаке — это успех.
        if (await pairingWentThrough()) {
          if (done || g !== gen) return;
          markConfirmed(g, statusEl);
          return;
        }
        if (done || g !== gen) return;
        if (statusEl) {
          statusEl.textContent = fails < 3
            ? '⏳ Проверяем подключение…'
            : '⚠️ ' + friendlyErr(e, 'Что-то пошло не так. Попробуйте «Получить новый код».');
        }
      }
    }

    function markConfirmed(g, statusEl) {
      if (done || g !== gen) return;
      done = true;
      active = false;
      fails = 0;
      stop();
      if (statusEl) statusEl.textContent = '✅ Подключение подтверждено!';
      onConfirmed();
    }

    return { start, stop, cancel };
  }

  // Мастер: пейринг на шаге «Способ подключения»
  const wizardPairing = createPairing(
    { qr: 'pair-qr', code: 'pair-code', status: 'pair-status', expiry: 'pair-expiry', tgRow: 'pair-tg-row', tgLink: 'pair-tg', account: 'pair-account' },
    () => {
      state.cloudPairConfirmed = true;
      showToast('Компьютер добавлен в аккаунт');
      // Дальше по маршруту, а не сразу на «Готово!»: следующий шаг — «AI-агенты»,
      // ради которых Remotai и ставят. Прыжок в конец делал этот шаг
      // недостижимым даже после того, как он появился в облачном маршруте.
      const route = wizardRoute();
      const next = route[route.indexOf(1) + 1];
      showStep(next === undefined ? totalSteps - 1 : next);
    }
  );

  // Панель: пейринг добавляет ЭТОТ компьютер в аккаунт пользователя. Телефоны
  // и браузеры входят в тот же аккаунт отдельно и не являются парными устройствами.
  const panelPairing = createPairing(
    { qr: 'pp-qr', code: 'pp-code', status: 'pp-status', expiry: 'pp-expiry', tgRow: 'pp-tg-row', tgLink: 'pp-tg', account: 'pp-account' },
    async () => {
      showToast('Компьютер добавлен в аккаунт!');
      // Выход из режима локальной сети: режим переключаем ТОЛЬКО после
      // подтверждения, иначе отменённая попытка оставила бы ПК без LAN-QR.
      if (state.cloudEnablePending) {
        state.cloudEnablePending = false;
        try {
          await api('POST', '/api/setup/cloud/enable', {});
          state.lanPairShown = false;
        } catch (e) {
          showToast(friendlyErr(e, 'Компьютер добавлен, но переключить режим не удалось.'), true);
        }
      }
      setTimeout(() => {
        el('p-pair')?.classList.add('hidden');
        el('pp-hide')?.classList.add('hidden');
        refreshPanelStatus();
      }, 1200);
    }
  );

  // ── Control panel (configured) ───────────────────────────────

  let panelPollTimer = null;

  function showPanel(status) {
    state.configured = true;
    // Встроенный режим (вкладка «Панель ПК» в клиенте, ?embed=1): прячем то,
    // что дублирует приложение или не имеет смысла внутри вкладки —
    // плитки «открыть управление/терминал» и подсказку «окно можно закрыть».
    if (new URLSearchParams(location.search).has('embed')) {
      document.body.classList.add('embed');
    }
    document.body.classList.add('panel-mode');
    el('boot')?.classList.add('hidden');
    document.querySelector('.steps')?.classList.add('hidden');
    document.querySelectorAll('.step').forEach(s => s.classList.add('hidden'));
    document.querySelector('.btn-row')?.classList.add('hidden');
    el('btn-next')?.classList.add('hidden');
    el('btn-back')?.classList.add('hidden');
    // Версия и «обновляется автоматически» уже есть в карточке панели —
    // футер оставляем только мастеру, чтобы не дублировать.
    document.querySelector('.foot')?.classList.add('hidden');
    el('panel')?.classList.remove('hidden');

    renderPanelStatus(status);
    bindPanelOnce();
    loadAccessInfo();
    refreshLANCheck();
    void loadPanelAgents();

    if (!panelPollTimer) {
      panelPollTimer = setInterval(refreshPanelStatus, 5000);
    }
  }

  // Адреса для входа с других устройств + токен для перехода в /miniapp.
  async function loadAccessInfo() {
    const box = el('p-lan-urls');
    try {
      const acc = await api('GET', '/api/setup/local-access');
      state.access = acc;
      if (!box) return;
      const urls = acc.lan_urls || [];
      if (!urls.length) {
        box.innerHTML = '<span class="hint">сетевые адреса не найдены</span>';
        return;
      }
      box.innerHTML = urls.map(u =>
        `<button class="url-chip" type="button" data-url="${u}/miniapp?token=${encodeURIComponent(acc.token)}">${u.replace('http://', '')}</button>`
      ).join('');
      box.querySelectorAll('.url-chip').forEach(b => b.addEventListener('click', () =>
        navigator.clipboard?.writeText(b.dataset.url)
          .then(() => showToast('Ссылка скопирована'))
          .catch(() => {})));
    } catch (e) {
      if (box) box.innerHTML = '<span class="hint">' + friendlyErr(e) + '</span>';
    }
  }

  // Открыть локальный клиент (терминалы/файлы/экран) в этом же окне.
  async function openLocalClient() {
    try {
      const acc = state.access || await api('GET', '/api/setup/local-access');
      state.access = acc;
      location.href = '/miniapp?token=' + encodeURIComponent(acc.token);
    } catch (e) {
      showToast(friendlyErr(e), true);
    }
  }

  async function refreshPanelStatus() {
    try {
      const st = await api('GET', '/api/setup/status');
      renderPanelStatus(st);
    } catch (e) { /* сервер перезапускается — следующий тик подхватит */ }
  }

  function renderPanelStatus(st) {
    state.lastStatus = st;
    renderVersion(st.version);
    renderPanelSystem(st);
    renderInternetAccess(st);
    renderAccountIdentity(st);
    renderMoreDevices(st);

    const dot = el('p-dot'), stateEl = el('p-state'), host = el('p-host'),
          pill = el('p-phone-pill'), hint = el('p-phone-hint'),
          addBtn = el('btn-add-device');
    const hostName = st.hostname || 'этот компьютер';

    // Одноразовый баннер после фонового автообновления (маркер с сервера).
    if (st.just_updated && state.justUpdatedShown !== st.just_updated) {
      state.justUpdatedShown = st.just_updated;
      showToast('Remotai обновлён до v' + st.just_updated);
    }

    renderPendingUpdate(st.pending_update);

    // Standalone (LAN): облачного пейринга нет в принципе. Показываем LAN
    // QR/код вместо «Телефон не подключён» + облачного QR (который к тому же
    // молча писал relay-конфиг).
    if (st.mode === 'standalone') {
      if (dot) dot.className = 'status-dot';
      if (stateEl) stateEl.textContent = 'Работает по локальной сети';
      if (host) host.textContent = '«' + hostName + '» доступен с телефона в вашей Wi-Fi';
      if (pill) { pill.textContent = 'по Wi-Fi'; pill.className = 'cloud-pill on'; }
      if (hint) hint.textContent = 'Телефон должен быть в той же Wi-Fi сети. Отсканируйте QR ниже — доступ выдаётся кодом, облако не используется.';
      if (addBtn) addBtn.classList.add('hidden');
      el('pp-show')?.classList.add('hidden');
      el('btn-disconnect')?.classList.add('hidden');
      el('p-pair')?.classList.remove('hidden');
      // Путь обратно в облако: раньше выбор «локальной сети» (в том числе
      // «Подключу телефон позже») закрывал доступ через интернет навсегда.
      if (state.cloudEnablePending) {
        el('p-cloud-enable')?.classList.add('hidden');
        el('pp-hide')?.classList.remove('hidden');
      } else {
        el('p-cloud-enable')?.classList.remove('hidden');
        el('pp-hide')?.classList.add('hidden');
        // Пейринг мог подтвердиться фоном, пока окно было закрыто: тогда новый
        // код не нужен — остаётся только включить облачный режим.
        const enableBtn = el('btn-cloud-enable'), enableHint = el('p-cloud-enable-hint');
        if (enableBtn) {
          enableBtn.textContent = st.relay_configured
            ? 'Включить доступ через интернет'
            : 'Подключить через интернет';
        }
        if (enableHint) {
          enableHint.textContent = st.relay_configured
            ? 'Компьютер уже добавлен в аккаунт Remotai — осталось включить облачный режим.'
            : 'Компьютер появится в вашем аккаунте Remotai — управление станет доступно из любой сети, а не только из домашней Wi-Fi.';
        }
        if (!state.lanPairShown) showLanPairing(st);
        else renderLanPairStatus(st);
      }

      const au0 = el('p-autoupdate');
      if (au0) au0.textContent = st.auto_update ? 'обновляется автоматически' : 'автообновление выключено';
      return;
    }
    el('p-cloud-enable')?.classList.add('hidden');

    // Свой Telegram-бот. Раньше панель считала облачным всё, что не standalone:
    // человеку, сознательно отказавшемуся от облака Remotai, она писала
    // «Компьютер не добавлен в аккаунт» и без единого нажатия просила код у
    // remotai.ru. Здесь у режима свой статус и свои действия.
    if (st.mode === 'own_bot') {
      const bot = st.bot || {};
      const botOk = !!bot.configured && bot.live !== false;
      if (dot) dot.className = 'status-dot' + (botOk ? '' : ' warn');
      if (stateEl) {
        stateEl.textContent = !bot.configured ? 'Токен бота не задан'
          : botOk ? 'Работает через ваш Telegram-бот' : 'Ваш бот не отвечает';
      }
      if (host) {
        host.textContent = !bot.configured
          ? 'Добавьте TELEGRAM_BOT_TOKEN в файл .env и перезапустите Remotai'
          : '«' + hostName + '» отвечает в вашем боте, облако Remotai не используется';
      }
      if (pill) {
        pill.textContent = botOk ? 'свой бот' : 'нет связи';
        pill.className = 'cloud-pill ' + (botOk ? 'on' : 'err');
      }
      if (hint) {
        hint.textContent = botOk
          ? 'Пишите своему боту в Telegram. Приложение Remotai подключается к этому компьютеру по адресу туннеля или по локальной сети.'
          : 'Проверьте токен бота в файле .env и доступ Telegram из этой сети (VPN, прокси, фильтрация).';
      }
      addBtn?.classList.add('hidden');
      el('pp-show')?.classList.add('hidden');
      el('btn-disconnect')?.classList.toggle('hidden', !st.relay_configured);
      el('p-own-bot')?.classList.remove('hidden');
      void renderOwnBotCard(st);
      const auOwn = el('p-autoupdate');
      if (auOwn) auOwn.textContent = st.auto_update ? 'обновляется автоматически' : 'автообновление выключено';
      return;
    }
    el('p-own-bot')?.classList.add('hidden');

    // Три состояния, описанные человеческим языком:
    if (st.relay_configured && st.relay_connected) {
      if (dot) dot.className = 'status-dot';
      if (stateEl) stateEl.textContent = 'Всё работает';
      if (host) host.textContent = '«' + hostName + '» добавлен в ваш аккаунт';
      if (pill) { pill.textContent = 'подключён'; pill.className = 'cloud-pill on'; }
      if (hint) hint.textContent = 'Войдите в этот же аккаунт Remotai с любого телефона или браузера — компьютер появится в разделе «Мои компьютеры». Аккаунт создавался сканированием QR и остался гостевым? В приложении: Настройки → привязать Telegram или почту, потом войти тем же способом на втором телефоне.';
      if (addBtn) addBtn.textContent = 'Компьютер уже в аккаунте';
    } else if (st.relay_configured) {
      if (dot) dot.className = 'status-dot warn';
      const relayKind = st.relay_error && st.relay_error.kind;
      if (stateEl) stateEl.textContent = relayKind === 'revoked' ? 'Привязка отозвана' :
        relayKind === 'service' ? 'Сервис временно недоступен' : 'Нет соединения с интернетом';
      if (host) host.textContent = relayKind === 'revoked'
        ? 'Добавьте компьютер в аккаунт заново'
        : relayKind === 'dns' || relayKind === 'tls' || relayKind === 'timeout' || relayKind === 'proxy'
          ? 'Проверьте VPN, прокси и доступ в интернет'
          : 'Удалённый доступ пока недоступен — проверьте сеть';
      if (pill) { pill.textContent = 'нет связи'; pill.className = 'cloud-pill err'; }
      if (hint) hint.textContent = relayKind === 'revoked'
        ? 'Доступ этого компьютера отозван. Получите новый QR-код и добавьте компьютер в нужный аккаунт.'
        : relayKind === 'service'
          ? 'Облачный сервис Remotai временно не отвечает. Повторное соединение выполняется автоматически.'
          : 'Компьютер не может связаться с облаком. Возможно, соединению мешает VPN, прокси или DNS.';
      if (addBtn) addBtn.textContent = 'Подключить этот компьютер к аккаунту';
      if (relayKind === 'revoked' && !state.pairAutoShown) {
        state.pairAutoShown = true;
        el('p-pair')?.classList.remove('hidden');
        el('pp-hide')?.classList.remove('hidden');
        startPanelCloudPairing();
      }
    } else {
      if (dot) dot.className = 'status-dot off';
      if (stateEl) stateEl.textContent = 'Компьютер не добавлен в аккаунт';
      if (host) host.textContent = 'Добавьте этот компьютер в Remotai — это займёт минуту';
      if (pill) { pill.textContent = 'не подключён'; pill.className = 'cloud-pill'; }
      if (hint) hint.textContent = 'Откройте Remotai на телефоне или в браузере, войдите в нужный аккаунт и подтвердите код этого компьютера.';
    }

    // Компьютер не добавлен в аккаунт → QR показан сразу, без лишних кнопок.
    const paired = !!st.relay_configured;
    if (!paired && !state.pairAutoShown) {
      state.pairAutoShown = true;
      el('p-pair')?.classList.remove('hidden');
      startPanelCloudPairing();
    }
    // Новый телефон не требует нового пейринга: достаточно войти в тот же
    // аккаунт. Повторный код мог бы только создать конфликт аккаунтов.
    addBtn?.classList.add('hidden');

    // Пока компьютера нет в аккаунте (или привязка отозвана), окно обязано
    // держать способ показать QR СНОВА: раньше после «Скрыть QR-код» оставалась
    // инструкция «получите новый QR» и ни одного элемента, который её выполняет
    // (кнопку «Подключить…» гасил следующий опрос статуса).
    const revoked = !!(st.relay_error && st.relay_error.kind === 'revoked');
    const pairBox = el('p-pair');
    const pairVisible = !!pairBox && !pairBox.classList.contains('hidden');
    el('pp-show')?.classList.toggle('hidden', pairVisible || (paired && !revoked));
    el('pp-hide')?.classList.toggle('hidden', !pairVisible);

    // «Отвязать…» — только когда есть что отвязывать.
    el('btn-disconnect')?.classList.toggle('hidden', !paired);

    const au = el('p-autoupdate');
    if (au) au.textContent = st.auto_update ? 'обновляется автоматически' : 'автообновление выключено';
  }

  // «Из интернета: remotai.ru/app — работает из любого места» — правда только
  // для компьютера, который реально в облаке. В остальных состояниях ссылка
  // ведёт в тупик (в аккаунте не будет ни одного компьютера).
  function renderInternetAccess(st) {
    const box = el('p-inet-access');
    if (!box) return;
    const link = '<a class="link" href="https://remotai.ru/app" target="_blank" rel="noopener">remotai.ru/app</a>';
    if (st.mode === 'standalone') {
      box.innerHTML = '<span class="hint">недоступно в режиме локальной сети — подключите компьютер к аккаунту (кнопка «Подключить через интернет» выше)</span>';
    } else if (st.mode === 'own_bot') {
      // У своего бота своя дорога наружу: облачный клиент remotai.ru/app к
      // этому компьютеру не относится.
      box.innerHTML = '<span class="hint">через вашего Telegram-бота и ваш туннель — адрес в карточке «Свой Telegram-бот»</span>';
    } else if (!st.relay_configured) {
      box.innerHTML = '<span class="hint">появится после добавления компьютера в аккаунт</span>';
    } else if (!st.relay_connected) {
      box.innerHTML = link + ' — <span class="warn">сейчас нет связи с облаком</span>, доступ вернётся вместе с интернетом';
    } else {
      box.innerHTML = link + ' — работает из любого места';
    }
  }

  // В каком аккаунте компьютер. Без этого при двух аккаунтах (например, ПК
  // привязан со старого анонимного телефона, а вход теперь через Telegram)
  // панель бодро пишет «Всё работает», а компьютера в приложении нет.
  function renderAccountIdentity(st) {
    const tech = el('p-account-tech'), line = el('p-account-id'),
          mismatch = el('p-account-mismatch');
    const acc = st.account;
    const known = !!(acc && acc.user_id) && st.mode !== 'standalone';
    // Номер аккаунта — внутренний: в приложении его не видно, сверить не с чем,
    // поэтому он живёт в «технических деталях», а не строкой на виду.
    tech?.classList.toggle('hidden', !known);
    mismatch?.classList.toggle('hidden', !known);
    if (!known || !line) return;
    line.textContent = '№' + acc.user_id;
    line.title = 'Внутренний номер аккаунта Remotai, которому принадлежит этот компьютер. В приложении он не показывается.';
  }

  // «Как подключить второй телефон» — вопрос, на который окно раньше не
  // отвечало: гостевому (анонимному) аккаунту войти со второго устройства
  // нечем, а QR локальной сети в облачном режиме панель не показывала вовсе.
  function renderMoreDevices(st) {
    const card = el('p-more-devices'), hint = el('p-more-devices-hint');
    if (!card) return;
    // В режиме локальной сети QR и код уже показаны в карточке выше.
    const show = st.mode !== 'standalone';
    card.classList.toggle('hidden', !show);
    if (!show || !hint) return;
    hint.textContent = st.relay_configured
      ? 'Через интернет: войдите в этот же аккаунт Remotai на втором телефоне — компьютер появится сам. Гостевой аккаунт (вход был только по QR) сначала нужно закрепить: в приложении Настройки → привязать Telegram или почту.'
      : 'Компьютер пока не в аккаунте. Подключить телефон можно и без облака — по локальной сети, кодом с этого экрана.';
  }

  // Свой Telegram-бот: состояние бота и туннеля вместо облачных строк.
  async function renderOwnBotCard(st) {
    const botEl = el('p-own-bot-state');
    const bot = st.bot || {};
    if (botEl) {
      botEl.textContent = !bot.configured ? 'токен не задан (.env)'
        : bot.live === false ? 'не отвечает' : 'работает';
      botEl.className = 'access-v ' + (bot.configured && bot.live !== false ? 'ok' : 'warn');
    }
    const tunEl = el('p-own-tunnel');
    if (!tunEl) return;
    // Статус панели опрашивается каждые 5 с, а состояние туннеля так часто не
    // меняется: лишний запрос на каждый тик — тот же класс проблем, что и
    // шторм поллинга в клиенте.
    if (state.tunnelCheckedAt && Date.now() - state.tunnelCheckedAt < 30000) return;
    state.tunnelCheckedAt = Date.now();
    try {
      const t = await api('GET', '/api/setup/tunnel');
      if (t.url) {
        tunEl.textContent = t.url.replace(/^https?:\/\//, '');
        tunEl.className = 'access-v ok';
      } else if (t.status === 'error') {
        tunEl.textContent = 'ошибка запуска — ' + (t.error || 'см. лог');
        tunEl.className = 'access-v warn';
      } else {
        tunEl.textContent = 'не запущен — доступ только по локальной сети';
        tunEl.className = 'access-v';
      }
    } catch (e) {
      tunEl.textContent = 'состояние неизвестно';
      tunEl.className = 'access-v';
    }
  }

  function renderPanelSystem(st) {
    const portEl = el('p-port');
    if (portEl) portEl.textContent = String(st.port || '—');
    // У службы (systemd/Windows-служба) программу гасит система, а не окно —
    // кнопка «Завершить Remotai» там только обманывала бы.
    el('p-quit-line')?.classList.toggle('hidden', st.can_quit === false);
    const autostart = st.autostart || {};
    const toggle = el('p-autostart-toggle');
    if (toggle && document.activeElement !== toggle) toggle.checked = !!autostart.enabled;
    el('p-autostart-warning')?.classList.toggle('hidden', !!autostart.enabled);

    // Порт был занят чужой программой, Remotai взял свободный и работает.
    // Это состояние, а не авария: заголовок нейтральный, основное действие —
    // вернуться на прежний порт (когда он освободился), а не «сменить ещё раз».
    const bindBox = el('p-bind-error');
    if (st.bind_error && bindBox) {
      const be = st.bind_error;
      bindBox.classList.remove('hidden');
      const title = el('p-bind-title');
      if (title) title.textContent = 'Remotai работает на порту ' + (be.selected_port || st.port || '—');
      const text = el('p-bind-error-text');
      if (text) text.textContent = be.message ||
        ('Порт ' + be.port + ' был занят. Сейчас используется ' + be.selected_port + '.');
      const restore = el('btn-restore-port');
      if (restore) {
        restore.textContent = 'Вернуться на порт ' + be.port;
        restore.classList.toggle('hidden', !be.original_free);
        restore.dataset.port = String(be.port || '');
      }
    } else {
      bindBox?.classList.add('hidden');
    }
  }

  async function refreshLANCheck() {
    const status = el('p-lan-check'), button = el('btn-allow-firewall');
    try {
      const data = await api('GET', '/api/setup/lan-check');
      const ok = !!data.allowed && !!data.bind_all;
      // У облачного компьютера входящих LAN-подключений не было и не будет —
      // правила в брандмауэре нет ни у кого из таких пользователей. Жёлтая
      // строка «может быть заблокирован» и UAC-кнопка делали полностью рабочий
      // ПК наполовину сломанным. Кнопку показываем, только когда человек сам
      // раскрыл локальный доступ (см. showLanAccessHelp).
      const st = state.lastStatus || {};
      const lanMatters = st.mode === 'standalone' || !!state.lanHelpRequested;
      if (status) {
        if (ok) {
          status.textContent = 'разрешён';
          status.className = 'access-v ok';
        } else if (!lanMatters) {
          status.textContent = data.allowed
            ? 'сервер ещё запускается'
            : 'не требуется — телефон подключается через интернет';
          status.className = 'access-v';
        } else {
          status.textContent = data.allowed ? 'сервер ещё запускается' : 'может быть заблокирован';
          status.className = 'access-v warn';
        }
        status.title = data.detail || '';
      }
      button?.classList.toggle('hidden', st.platform !== 'windows' || !!data.allowed || !lanMatters);
    } catch (e) {
      if (status) status.textContent = 'проверка не удалась';
    }
  }

  // Человек раскрыл «Подключить ещё одно устройство» / QR локальной сети —
  // с этого момента брандмауэр для него уже не абстракция: возвращаем честную
  // жёлтую строку и кнопку «Разрешить в брандмауэре Windows».
  function showLanAccessHelp() {
    if (state.lanHelpRequested) return;
    state.lanHelpRequested = true;
    void refreshLANCheck();
  }

  // Облачный пейринг в панели: блок #p-pair общий с LAN-кодом, поэтому перед
  // запуском возвращаем облачное оформление.
  function startPanelCloudPairing() {
    setPairingPresentation('cloud');
    el('pp-lan-addr')?.classList.add('hidden');
    panelPairing.start();
  }

  // Оформление блока пейринга различается: облачный код — 8 символов крупно,
  // LAN-код — base64 «адрес|токен» (~80 символов), который вручную не набирают.
  function setPairingPresentation(kind) {
    const codeEl = el('pp-code'), row = el('pp-code-row'), copy = el('pp-copy'),
          manual = el('pp-manual-hint'), stepApp = el('pp-step-app'), lan = kind === 'lan';
    if (codeEl) codeEl.className = lan ? 'pair-code-long' : 'pair-code-big';
    row?.classList.toggle('stacked', lan);
    if (copy) {
      copy.className = lan ? 'btn btn-secondary' : 'btn btn-secondary btn-icon';
      copy.textContent = lan ? 'Скопировать код' : '⧉';
      copy.style.marginTop = lan ? '8px' : '';
    }
    if (manual) {
      manual.textContent = lan
        ? 'Не сканируется? Нажмите «Скопировать код» и вставьте его в приложении.'
        : 'Не сканируется? Введите этот код подключения в приложении вручную:';
    }
    if (stepApp) {
      stepApp.innerHTML = lan
        ? 'Откройте на телефоне приложение <b>Remotai</b> — телефон должен быть в той же Wi-Fi сети'
        : 'На телефоне или другом ПК откройте <a class="link" href="https://remotai.ru/app/" target="_blank" rel="noopener">Remotai в браузере</a> или приложение. Войдите в тот же аккаунт';
    }
    const stepCode = el('pp-step-code');
    if (stepCode) stepCode.textContent = lan
      ? 'Нажмите «Сканировать QR» или выберите ввод кода вручную'
      : 'В «Моих компьютерах» нажмите «Подключить компьютер» и введите код ниже. На телефоне можно отсканировать QR';
    el('pp-lan-addr')?.classList.toggle('hidden', !lan);
    el('pp-account-hint')?.classList.toggle('hidden', lan);
    if (lan) el('pp-account')?.classList.add('hidden');
    el('pp-expiry')?.classList.toggle('hidden', lan);
    el('pp-refresh')?.classList.toggle('hidden', lan);
    // remotai.ru/app к локальному ПК подключиться не может физически:
    // https-страница не ходит на http://192.168.x.x. Совет «откройте
    // remotai.ru/app» приводил к ложному диагнозу «проверьте сеть».
    el('pp-install-web')?.classList.toggle('hidden', lan);
  }

  // LAN-пейринг для standalone: QR/код с адресом этого ПК и api-токеном.
  // Формат payload — как у qr.go buildPairingURL / buildPairingCode.
  async function showLanPairing(st) {
    const statusEl = el('pp-status');
    try {
      const acc = state.access || await api('GET', '/api/setup/local-access');
      state.access = acc;
      const urls = acc.lan_urls || [];
      const serverUrl = urls[0] || ('http://localhost:' + acc.port);
      const q = new URLSearchParams({ url: serverUrl, token: acc.token });
      if (st && st.device_id) q.set('device', st.device_id);
      // Имя машины в payload: без него телефон называет компьютер IP-адресом
      // («192.168.1.50») во всех экранах, а после смены адреса роутером имя
      // ещё и меняется.
      if (st && st.hostname) q.set('name', st.hostname);
      const qrEl = el('pp-qr'), codeEl = el('pp-code'), addr = el('pp-lan-addr');
      setPairingPresentation('lan');
      if (qrEl) qrEl.src = '/api/setup/qr?data=' + encodeURIComponent('tgcontrol://pair?' + q.toString());
      if (codeEl) codeEl.textContent = btoa(serverUrl + '|' + acc.token);
      if (addr) addr.textContent = 'Адрес этого компьютера: ' + serverUrl.replace('http://', '');
      el('pp-tg-row')?.classList.add('hidden');
      state.lanPairShown = true;
      renderLanPairStatus(st);
    } catch (e) {
      if (statusEl) statusEl.textContent = '⚠️ ' + friendlyErr(e);
    }
  }

  // QR и код локальной сети для ВТОРОГО устройства — в любом режиме, включая
  // облачный. Payload тот же, что у режима «по локальной сети».
  async function fillLanDeviceQR() {
    const img = el('p-lan-qr-img'), codeEl = el('p-lan-code'), addr = el('p-lan-qr-addr');
    try {
      const acc = state.access || await api('GET', '/api/setup/local-access');
      state.access = acc;
      const st = state.lastStatus || {};
      const urls = acc.lan_urls || [];
      const serverUrl = urls[0] || ('http://localhost:' + acc.port);
      const q = new URLSearchParams({ url: serverUrl, token: acc.token });
      if (st.device_id) q.set('device', st.device_id);
      if (st.hostname) q.set('name', st.hostname);
      if (img) img.src = '/api/setup/qr?data=' + encodeURIComponent('tgcontrol://pair?' + q.toString());
      if (codeEl) codeEl.textContent = btoa(serverUrl + '|' + acc.token);
      if (addr) {
        addr.textContent = urls.length
          ? 'Адрес этого компьютера: ' + serverUrl.replace('http://', '')
          : 'Сетевой адрес не найден — этот компьютер сейчас не виден в Wi-Fi';
      }
    } catch (e) {
      if (addr) addr.textContent = friendlyErr(e);
    }
  }

  // Обратная связь на ПК. В облаке человек видит «⏳ Отсканируйте QR…» →
  // «✅ Подключение подтверждено», а в режиме локальной сети статус навсегда
  // застывал на «наведите камеру» — телефон уже работал, а окно всё просило.
  function renderLanPairStatus(st) {
    const statusEl = el('pp-status');
    if (!statusEl || !state.lanPairShown) return;
    statusEl.textContent = st && st.clients_live > 0
      ? '✅ Приложение подключилось к этому компьютеру'
      : '📶 Телефон в той же Wi-Fi сети — наведите камеру на QR';
  }

  // ── Панель: AI-помощники ─────────────────────────────────────
  // Вторая дверь к тому же детекту и той же установке, что и на шаге мастера:
  // в облачном (рекомендованном) маршруте шаг раньше пропускался, и человек,
  // поставивший Remotai ради Claude Code, не видел ни списка, ни кнопки.

  async function loadPanelAgents() {
    const list = el('p-agent-list');
    if (!list) return;
    list.innerHTML = '<span class="spinner"></span> Ищем помощников на этом компьютере…';
    try {
      const caps = await api('GET', '/api/setup/detect');
      state.capabilities = caps;
      renderPanelAgents(caps);
    } catch (e) {
      el('p-agent-install')?.classList.add('hidden');
      list.innerHTML = '<p class="hint">' +
        escapeHtml(friendlyErr(e, 'Не удалось проверить помощников на этом компьютере.')) + '</p>';
    }
  }

  function renderPanelAgents(caps) {
    const list = el('p-agent-list');
    const found = caps.agents_found || [];
    state.agentsInstallable = caps.agents_installable || [];
    if (!list) return;
    list.innerHTML = found.length
      ? found.map(a => `
        <div class="agent-item">
          <span class="agent-icon">${escapeHtml(a.icon)}</span>
          <div class="agent-info">
            <div class="agent-name">${escapeHtml(a.name)}</div>
            <div class="agent-path">${escapeHtml(a.path)}</div>
          </div>
          <span class="agent-status found">найден</span>
        </div>`).join('')
      : '<p class="hint">Пока никого не нашли. Помощника можно поставить прямо отсюда — терминал откроется на этом компьютере.</p>';

    const box = el('p-agent-install'), installList = el('p-agent-install-list'),
          toggle = el('btn-p-agent-install-toggle');
    if (!box || !installList || !toggle) return;
    box.classList.toggle('hidden', !state.agentsInstallable.length);
    toggle.textContent = found.length ? 'Установить ещё помощника…' : 'Установить помощника…';
    installList.innerHTML = state.agentsInstallable.map(a => `
      <div class="agent-item">
        <span class="agent-icon">${escapeHtml(a.icon)}</span>
        <div class="agent-info">
          <div class="agent-name">${escapeHtml(a.name)}</div>
          <div class="agent-path" title="${escapeHtml(a.command)}">Поставим сами — команда уйдёт в терминал этого компьютера</div>
        </div>
        <button class="btn btn-secondary btn-inline" type="button" data-install="${escapeHtml(a.id)}">Установить</button>
      </div>`).join('');
    installList.querySelectorAll('[data-install]').forEach(btn => {
      btn.addEventListener('click', () => void installAgent(btn.dataset.install, btn));
    });
    el('p-agent-install-body')?.classList.toggle('hidden', !state.agentsInstallOpen);
  }

  let panelBound = false;
  function bindPanelOnce() {
    if (panelBound) return;
    panelBound = true;

    el('btn-open-app')?.addEventListener('click', openLocalClient);
    el('pp-install-toggle')?.addEventListener('click', () => {
      const block = el('pp-install-block');
      const open = block?.classList.toggle('hidden') === false;
      const btn = el('pp-install-toggle');
      if (btn) btn.textContent = open ? 'Вернуться к подключению' : 'У меня ещё нет приложения — показать QR установки';
    });

    el('btn-host-terminal')?.addEventListener('click', async () => {
      const btn = el('btn-host-terminal');
      btn.disabled = true;
      try {
        await api('POST', '/api/setup/open-terminal', {});
        showToast('Окно терминала открыто — оно уже видно с телефона');
      } catch (e) {
        showToast(friendlyErr(e), true);
      } finally {
        btn.disabled = false;
      }
    });

    const showCloudPairing = () => {
      el('p-disconnect-confirm')?.classList.add('hidden');
      el('p-pair')?.classList.remove('hidden');
      el('btn-add-device')?.classList.add('hidden');
      el('pp-show')?.classList.add('hidden');
      el('pp-hide')?.classList.remove('hidden');
      startPanelCloudPairing();
    };
    el('btn-add-device')?.addEventListener('click', showCloudPairing);
    // «Показать QR-код» — постоянный путь назад после «Скрыть QR-код»
    // (в состояниях «не в аккаунте» и «привязка отозвана»).
    el('pp-show')?.addEventListener('click', showCloudPairing);
    // Свой бот → облако: тот же облачный код, но только по явной просьбе.
    el('btn-own-bot-cloud')?.addEventListener('click', showCloudPairing);

    // Помощники в панели: та же установка, что и в мастере.
    el('btn-p-agent-install-toggle')?.addEventListener('click', () => {
      state.agentsInstallOpen = !state.agentsInstallOpen;
      el('p-agent-install-body')?.classList.toggle('hidden', !state.agentsInstallOpen);
    });
    el('btn-p-agent-recheck')?.addEventListener('click', async () => {
      const btn = el('btn-p-agent-recheck');
      btn.disabled = true;
      btn.textContent = 'Проверяем…';
      try {
        await loadPanelAgents();
        const found = ((state.capabilities || {}).agents_found || []).length;
        showToast(found ? 'Найдено помощников: ' + found : 'Пока никого не нашли — установка ещё может идти');
      } finally {
        btn.disabled = false;
        btn.textContent = 'Проверить снова';
      }
    });

    // QR локальной сети в любом режиме: экран входа в приложении советует
    // «выберите в панели «По локальной сети»», а такого переключателя нет —
    // зато есть этот код, и он работает и у облачного компьютера.
    el('btn-lan-qr-toggle')?.addEventListener('click', () => {
      const box = el('p-lan-qr');
      const open = box?.classList.toggle('hidden') === false;
      const btn = el('btn-lan-qr-toggle');
      if (btn) btn.textContent = open ? 'Скрыть QR локальной сети' : 'Показать QR для этой Wi-Fi сети';
      if (open) {
        showLanAccessHelp();
        void fillLanDeviceQR();
      }
    });
    el('btn-lan-code-copy')?.addEventListener('click', () => {
      const code = el('p-lan-code')?.textContent || '';
      if (!code || code.startsWith('─')) return;
      navigator.clipboard?.writeText(code).then(() => showToast('Код скопирован')).catch(() => {});
    });
    // Режим локальной сети → облако: показываем облачный QR вместо LAN-кода,
    // режим переключится после подтверждения (см. panelPairing onConfirmed).
    el('btn-cloud-enable')?.addEventListener('click', async () => {
      const btn = el('btn-cloud-enable');
      // Компьютер уже в аккаунте (пейринг подтвердился фоном) — нового кода не
      // нужно, достаточно переключить режим.
      if (state.lastStatus && state.lastStatus.relay_configured) {
        btn.disabled = true;
        try {
          await api('POST', '/api/setup/cloud/enable', {});
          showToast('Доступ через интернет включён');
          await refreshPanelStatus();
        } catch (e) {
          showToast(friendlyErr(e), true);
        } finally {
          btn.disabled = false;
        }
        return;
      }
      state.cloudEnablePending = true;
      el('p-cloud-enable')?.classList.add('hidden');
      el('p-pair')?.classList.remove('hidden');
      el('pp-hide')?.classList.remove('hidden');
      startPanelCloudPairing();
    });
    el('pp-refresh')?.addEventListener('click', () => startPanelCloudPairing());
    el('pp-hide')?.addEventListener('click', () => {
      // «Скрыть» не равно «отменить»: фоновое ожидание подтверждения на ПК
      // намеренно переживает закрытие окна настройки.
      panelPairing.stop();
      if (state.cloudEnablePending) {
        // Возврат к LAN-коду: компьютер остаётся в режиме локальной сети.
        state.cloudEnablePending = false;
        state.lanPairShown = false;
        el('pp-hide')?.classList.add('hidden');
        el('p-cloud-enable')?.classList.remove('hidden');
        void showLanPairing(state.lastStatus);
        return;
      }
      el('p-pair')?.classList.add('hidden');
      el('pp-hide')?.classList.add('hidden');
      // «Подключить этот компьютер к аккаунту» гасит следующий опрос статуса
      // (каждые 5 с), а «Показать QR-код» его переживает — иначе в состоянии
      // «привязка отозвана» окно оставалось с инструкцией и без кнопок.
      el('pp-show')?.classList.remove('hidden');
    });
    el('pp-copy')?.addEventListener('click', () => {
      const code = el('pp-code')?.textContent || '';
      if (!code || code.startsWith('─')) return;
      navigator.clipboard?.writeText(code).then(() => showToast('Код скопирован')).catch(() => {});
    });

    el('p-access-toggle')?.addEventListener('click', () => {
      const body = el('p-access-body');
      const open = body?.classList.toggle('hidden') === false;
      const chev = el('p-access-chev');
      if (chev) chev.textContent = open ? '▴' : '▾';
    });

    el('p-autostart-toggle')?.addEventListener('change', async (event) => {
      const toggle = event.currentTarget;
      toggle.disabled = true;
      try {
        const value = await api('POST', '/api/setup/autostart', { enabled: toggle.checked });
        toggle.checked = !!value.enabled;
        el('p-autostart-warning')?.classList.toggle('hidden', !!value.enabled);
        showToast(value.enabled ? 'Автозапуск включён' : 'Автозапуск выключен');
      } catch (e) {
        toggle.checked = !toggle.checked;
        showToast(friendlyErr(e), true);
      } finally {
        toggle.disabled = false;
      }
    });
    el('btn-allow-firewall')?.addEventListener('click', async () => {
      const btn = el('btn-allow-firewall');
      btn.disabled = true;
      try {
        await api('POST', '/api/setup/lan-check/allow', {});
        showToast('Подтвердите разрешение в окне Windows');
        setTimeout(refreshLANCheck, 3000);
      } catch (e) {
        showToast(friendlyErr(e), true);
      } finally {
        btn.disabled = false;
      }
    });
    el('btn-change-port')?.addEventListener('click', () => void changePort(el('btn-change-port'), 0));
    el('btn-restore-port')?.addEventListener('click', () => {
      const btn = el('btn-restore-port');
      void changePort(btn, Number(btn.dataset.port || 0));
    });

    // Удаление этого компьютера из аккаунта — с инлайн-подтверждением.
    el('btn-disconnect')?.addEventListener('click', () => {
      panelPairing.stop();
      el('p-pair')?.classList.add('hidden');
      el('pp-hide')?.classList.add('hidden');
      el('pp-show')?.classList.remove('hidden');
      el('p-disconnect-confirm')?.classList.remove('hidden');
    });
    el('btn-disconnect-no')?.addEventListener('click', () => {
      el('p-disconnect-confirm')?.classList.add('hidden');
    });
    el('btn-disconnect-yes')?.addEventListener('click', async () => {
      const btn = el('btn-disconnect-yes');
      btn.disabled = true;
      try {
        await api('POST', '/api/setup/cloud/disconnect', {});
        showToast('Компьютер удалён из аккаунта');
        el('p-disconnect-confirm')?.classList.add('hidden');
        state.pairAutoShown = false; // статус сам развернёт QR заново
        refreshPanelStatus();
      } catch (e) {
        showToast(friendlyErr(e), true);
      } finally {
        btn.disabled = false;
      }
    });

    // Завершение программы — там, где его ищут (рядом с автозапуском), а не
    // «догадайся, что красная кнопка про отвязку — это не выход».
    el('btn-quit')?.addEventListener('click', () => {
      el('p-quit-confirm')?.classList.remove('hidden');
    });
    el('btn-quit-no')?.addEventListener('click', () => {
      el('p-quit-confirm')?.classList.add('hidden');
    });
    el('btn-quit-yes')?.addEventListener('click', async () => {
      const btn = el('btn-quit-yes');
      btn.disabled = true;
      try {
        await api('POST', '/api/setup/quit', {});
        el('p-quit-confirm')?.classList.add('hidden');
        if (panelPollTimer) { clearInterval(panelPollTimer); panelPollTimer = null; }
        const overlay = el('update-overlay'), text = el('update-overlay-text');
        overlay?.classList.remove('hidden');
        if (text) text.textContent = 'Remotai завершается. Окно можно закрыть — запустить снова можно ярлыком на рабочем столе.';
      } catch (e) {
        btn.disabled = false;
        showToast(friendlyErr(e, 'Не удалось завершить Remotai.'), true);
      }
    });

    // Обновления
    el('btn-check-update')?.addEventListener('click', checkUpdate);
    el('btn-apply-update')?.addEventListener('click', applyUpdate);
    el('btn-restart-update')?.addEventListener('click', restartPendingUpdate);
  }

  let pendingUpdate = null;

  // Обновление уже скачано, но рестарт отложен: это окно открыто. Показываем
  // плашку с кнопкой — без неё пользователь не знает, что работает на старой
  // версии (агент так простоял двое суток на v2.24.0).
  function renderPendingUpdate(pu) {
    const box = el('p-pending-update'), text = el('p-pending-update-text');
    if (!box || !text) return;
    if (!pu || !pu.version) {
      box.classList.add('hidden');
      return;
    }
    const mins = pu.waiting_since ? Math.round((Date.now() - pu.waiting_since) / 60000) : 0;
    // Причина ожидания — именно это открытое окно. Раньше объяснение жило
    // только в приложении на телефоне, где кнопки перезапуска как раз нет.
    text.textContent = 'Версия v' + pu.version + ' скачана и ждёт' +
      (mins >= 5 ? ' (' + mins + ' мин)' : '') +
      '. Ждём, пока это окно закроют, — иначе перезапустимся сами не позже чем через 2 часа. Перезапуск обновит окно, терминалы продолжат работать.';
    box.classList.remove('hidden');
  }

  async function restartPendingUpdate() {
    const overlay = el('update-overlay'), text = el('update-overlay-text');
    const oldVersion = state.version;
    overlay.classList.remove('hidden');
    text.textContent = 'Перезапускаемся… Окно обновится само.';
    try {
      await api('POST', '/api/setup/update-restart', {});
    } catch (e) {
      overlay.classList.add('hidden');
      showToast('Перезапуск не удался. ' + friendlyErr(e), true);
      return;
    }
    waitForNewVersion(oldVersion, text);
  }

  async function checkUpdate() {
    const btn = el('btn-check-update'), note = el('p-update-note'),
          applyBtn = el('btn-apply-update'), row = el('p-update-row');
    btn.disabled = true;
    btn.textContent = 'проверяем…';
    try {
      const resp = await api('GET', '/api/setup/update-check');
      const u = resp.update;
      note.classList.remove('hidden');
      if (resp.check_failed) {
        // Сервер не смог достучаться до remotai.ru — это не «последняя версия».
        pendingUpdate = null;
        note.className = 'update-note plain';
        note.textContent = 'Не удалось проверить — нет связи с remotai.ru.';
        row.classList.add('hidden');
      } else if (u && u.available) {
        pendingUpdate = u;
        note.className = 'update-note';
        note.textContent = 'Вышла версия v' + u.version +
          (u.changelog ? ' — ' + u.changelog : '') + '.';
        applyBtn.textContent = 'Обновить до v' + u.version;
        row.classList.remove('hidden');
      } else {
        pendingUpdate = null;
        note.className = 'update-note plain';
        note.textContent = 'У вас последняя версия.';
        row.classList.add('hidden');
      }
    } catch (e) {
      note.classList.remove('hidden');
      note.className = 'update-note plain';
      note.textContent = friendlyErr(e);
    } finally {
      btn.disabled = false;
      btn.textContent = 'проверить';
    }
  }

  async function applyUpdate() {
    const overlay = el('update-overlay'), text = el('update-overlay-text');
    overlay.classList.remove('hidden');
    text.textContent = 'Скачиваем обновление…';
    const oldVersion = state.version;
    try {
      await api('POST', '/api/setup/update-apply', {});
    } catch (e) {
      overlay.classList.add('hidden');
      showToast('Обновление не удалось. ' + friendlyErr(e), true);
      return;
    }
    text.textContent = 'Перезапускаемся… Окно обновится само.';
    waitForNewVersion(oldVersion, text);
  }

  // Сервер перезапускается в новый бинарник — ждём его и перезагружаем UI.
  // Общая для «Обновить» и «Перезапустить сейчас» (отложенное обновление).
  function waitForNewVersion(oldVersion, text) {
    const deadline = Date.now() + 90000;
    const tick = async () => {
      try {
        const st = await api('GET', '/api/setup/status');
        if (st.version && st.version !== oldVersion) {
          location.reload();
          return;
        }
      } catch (e) { /* ещё перезапускается */ }
      if (Date.now() < deadline) {
        setTimeout(tick, 1500);
      } else {
        text.textContent = 'Перезапуск затянулся. Закройте окно и откройте Remotai заново.';
      }
    };
    setTimeout(tick, 2500);
  }

  // Смена порта: сервер перезапускается, и окно должно уехать на новый адрес
  // только когда там уже кто-то отвечает. Слепой таймер 1,2 с попадал в момент,
  // когда новый процесс ещё не забиндил порт, и в окне (без адресной строки и
  // без «обновить») оставалась системная страница ошибки.
  async function changePort(btn, port) {
    if (btn) btn.disabled = true;
    const overlay = el('update-overlay'), text = el('update-overlay-text');
    try {
      const data = await api('POST', '/api/setup/port', port > 0 ? { port, restart: true } : { restart: true });
      overlay?.classList.remove('hidden');
      if (text) text.textContent = 'Переходим на порт ' + data.port + ' — Remotai перезапускается…';
      if (await waitForPort(data.port, 30000)) {
        // Во вкладке «Панель ПК» (iframe с ?embed=1) на новый порт должно уехать
        // ВСЁ окно: иначе страница-хозяин с терминалами и WebSocket остаётся на
        // старом адресе, где уже никто не слушает, и молча теряет связь.
        const target = 'http://localhost:' + data.port + '/setup?force=1';
        if (window.top && window.top !== window) {
          try {
            // Новый порт — другой origin, значит другой localStorage: без
            // токена клиент встретил бы экраном входа на только что работавшем ПК.
            const token = state.access && state.access.token;
            window.top.location.href = 'http://localhost:' + data.port + '/miniapp' +
              (token ? '?token=' + encodeURIComponent(token) : '');
            return;
          } catch (e) { /* кросс-ориджин top недоступен — уводим хотя бы себя */ }
        }
        location.href = target;
        return;
      }
      overlay?.classList.add('hidden');
      if (btn) btn.disabled = false;
      showToast('Перезапуск затянулся. Откройте окно заново из значка у часов.', true);
    } catch (e) {
      overlay?.classList.add('hidden');
      if (btn) btn.disabled = false;
      showToast(friendlyErr(e, 'Не удалось сменить порт.'), true);
    }
  }

  // Проверяем новый порт картинкой: fetch на другой порт — кросс-ориджин запрос
  // и падает всегда, а загрузка изображения CORS не ограничена.
  function waitForPort(port, timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    return new Promise(resolve => {
      const attempt = () => {
        const img = new Image();
        const finish = ok => {
          img.onload = null;
          img.onerror = null;
          if (ok) return resolve(true);
          if (Date.now() >= deadline) return resolve(false);
          setTimeout(attempt, 1000);
        };
        img.onload = () => finish(true);
        img.onerror = () => finish(false);
        img.src = 'http://localhost:' + port + '/api/setup/qr?data=ping&size=64&t=' + Date.now();
      };
      setTimeout(attempt, 800);
    });
  }

  // ── Step rendering (wizard) ──────────────────────────────────

  function wizardRoute() {
    // Cloud already supplies internet reachability; LAN deliberately has none.
    // cloudflared therefore remains only in the expert own_bot route.
    //
    // Шаг «AI-агенты» есть во ВСЕХ маршрутах: ради Claude Code/Codex продукт и
    // ставят, а в рекомендованном (облачном) маршруте он пропускался — человек
    // узнавал, что claude не установлен, уже открыв терминал.
    return state.mode === 'own_bot' ? [0, 1, 2, 3, 4] : [0, 1, 2, 4];
  }

  function updateSteps() {
    const route = wizardRoute();
    const currentIndex = route.indexOf(currentStep);
    document.querySelectorAll('.step-dot').forEach((dot, i) => {
      const visibleIndex = route.indexOf(i);
      dot.classList.toggle('hidden', visibleIndex < 0);
      dot.classList.toggle('active', i === currentStep);
      dot.classList.toggle('done', visibleIndex >= 0 && visibleIndex < currentIndex);
    });
    // «Назад» с «Готово!» возвращало в выбор режима уже настроенного ПК: мастер
    // тут же просил НОВЫЙ 15-минутный код и снова предлагал сменить режим.
    el('btn-back').classList.toggle('hidden', currentStep === 0 || currentStep === totalSteps - 1);
    const btn = el('btn-next');
    // Шаг «Способ подключения» в облачном режиме вперёд уходит САМ после скана
    // QR, поэтому «Далее» здесь умела только ругаться красным тостом. Осознанный
    // выход — кнопка «Подключу телефон позже» в самом блоке пейринга.
    btn.classList.toggle('hidden',
      currentStep === 1 && state.mode === 'central_bot' && !state.cloudPairConfirmed);
    if (currentStep === totalSteps - 1) {
      // Кнопка ведёт в КЛИЕНТ (/miniapp), а не в это окно настроек: словарь
      // разведён — окно = «Панель Remotai», клиент = «управление компьютером».
      btn.textContent = state.mode === 'standalone' ? 'Открыть управление компьютером' : 'Готово';
    } else if (currentStep === 0) {
      btn.textContent = 'Подключить этот компьютер';
    } else if (currentStep === 3) {
      btn.textContent = state.tunnelMode === 'disabled' ? 'Пропустить' : 'Далее';
    } else {
      btn.textContent = 'Далее';
    }
  }

  function showStep(n) {
    const route = wizardRoute();
    if (!route.includes(n)) n = route.find(step => step > n) ?? route[route.length - 1];
    document.querySelectorAll('.step').forEach((stepEl, i) => {
      stepEl.classList.toggle('hidden', i !== n);
    });
    currentStep = n;
    updateSteps();
    if (n === totalSteps - 1) clearProgress(); else saveProgress();
    // Заголовок вкладки называет ШАГ. Раньше на всех пяти было одно «Remotai»,
    // и переключаясь между окнами человек не понимал, на чём остановился
    // (аудит онбординга 30.08.2026).
    const stepTitle = document.querySelector('#step-' + n + ' h2');
    document.title = stepTitle ? 'Remotai — ' + stepTitle.textContent.trim() : 'Remotai';
    // 15-минутный код пейринга запрашиваем только с входа на шаг «Способ
    // подключения» — иначе он тикает, пока пользователь ещё на приветствии.
    // Уже подтверждённый пейринг заново не запрашиваем: возврат «Назад» на этот
    // шаг просил у релея НОВЫЙ 15-минутный код для компьютера, который только
    // что добавили в аккаунт.
    if (n === 1 && state.mode === 'central_bot' && !state.cloudPairConfirmed) wizardPairing.start();
    if (n === 2) void initAgentsStep();
    if (n === 3) initTunnelStep();
    if (n === 4) void initCompleteStep();
  }

  // ── Step 0: Welcome ──────────────────────────────────────────

  function renderWelcome(status) {
    const info = el('welcome-info');
    if (!info) return;
    info.innerHTML = `
      <p>Если ИИ-агенты и проекты находятся на «${escapeHtml(status.hostname || 'этом компьютере')}», нажмите «Подключить этот компьютер». После настройки вы сможете давать агентам задачи с телефона или другого ПК.</p>
      <p class="hint" style="margin-top:8px">Агенты работают на другой машине? Выберите «Открыть агентов на другой машине» и войдите в свой аккаунт Remotai. Позже можно подключить и этот компьютер.</p>
      <details class="hint" style="margin-top:12px">
        <summary>Технические детали</summary>
        <dl class="device-info" style="margin-top:8px">
          <dt>Идентификатор компьютера</dt><dd>${escapeHtml(status.device_id)}</dd>
          <dt>Имя в сети</dt><dd>${escapeHtml(status.hostname)}</dd>
          <dt>Система</dt><dd>${escapeHtml(status.platform)}</dd>
        </dl>
      </details>
    `;
  }

  // ── Step 1: Mode selection ───────────────────────────────────

  function initModeStep() {
    document.querySelectorAll('[data-mode]').forEach(opt => {
      opt.addEventListener('click', async () => {
        const nextMode = opt.dataset.mode;
        if (state.mode === 'central_bot' && nextMode !== 'central_bot') {
          try {
            await wizardPairing.cancel();
          } catch (e) {
            showToast(friendlyErr(e, 'Не удалось отменить подключение.'), true);
            return;
          }
        }
        document.querySelectorAll('[data-mode]').forEach(o => o.classList.remove('selected'));
        opt.classList.add('selected');
        state.mode = nextMode;
        updateModeFields();
        updateSteps();
        // Уже подтверждённый пейринг заново НЕ запрашиваем: человек, который
        // передумал и вернулся к «через интернет», видел «⏳ Ждём подтверждения
        // с телефона…» и новый код — для компьютера, который в аккаунте уже
        // есть (аудит онбординга 30.08.2026).
        if (state.mode === 'central_bot' && !state.cloudPairConfirmed) wizardPairing.start();
      });
    });
    document.querySelector(`[data-mode="${state.mode}"]`)?.classList.add('selected');
    updateModeFields();
    // Легаси-ветка «Свой Telegram-бот» открывается только по явной просьбе:
    // рядом с двумя рабочими вариантами её читали как «для опытных = лучше».
    el('btn-more-modes')?.addEventListener('click', () => {
      const box = el('more-modes');
      const open = box?.classList.toggle('hidden') === false;
      const btn = el('btn-more-modes');
      if (btn) btn.textContent = open ? 'Скрыть другие способы' : 'Другие способы (для опытных)';
    });
    el('btn-pair-refresh')?.addEventListener('click', () => wizardPairing.start());
    el('btn-copy-code')?.addEventListener('click', () => {
      const code = el('pair-code')?.textContent || '';
      if (!code || code.startsWith('─')) return;
      navigator.clipboard?.writeText(code).then(() => showToast('Код скопирован')).catch(() => {});
    });
    el('pair-install-toggle')?.addEventListener('click', () => {
      const block = el('pair-install-block');
      const open = block?.classList.toggle('hidden') === false;
      const btn = el('pair-install-toggle');
      if (btn) btn.textContent = open ? 'Вернуться к QR подключения' : 'У меня ещё нет приложения — QR установки';
    });
    // «Подключу телефон позже» НЕ переводит компьютер в режим локальной сети:
    // раньше это навсегда закрывало доступ через интернет, хотя человек выбрал
    // именно облако. Завершаем настройку в облачном режиме без привязки —
    // панель встретит состоянием «Компьютер не добавлен в аккаунт» и QR-кодом.
    el('btn-pair-later')?.addEventListener('click', async () => {
      const btn = el('btn-pair-later');
      btn.disabled = true;
      try {
        await wizardPairing.cancel();
        await api('POST', '/api/setup/mode', { mode: 'central_bot' });
        state.completionData = await api('POST', '/api/setup/complete', {});
        showToast('Телефон можно подключить позже — QR-код ждёт в Панели Remotai');
        finishSetup();
      } catch (e) {
        btn.disabled = false;
        showToast(friendlyErr(e), true);
      }
    });
  }

  function updateModeFields() {
    el('central-fields').classList.toggle('hidden', state.mode !== 'central_bot');
    el('own-bot-fields').classList.toggle('hidden', state.mode !== 'own_bot');
  }

  async function saveMode() {
    try {
      if (state.mode === 'standalone') {
        // Без Telegram: финализируется на последнем шаге через /api/setup/standalone.
        return true;
      }
      await api('POST', '/api/setup/mode', { mode: state.mode });
      if (state.mode === 'central_bot') {
        if (!state.cloudPairConfirmed) {
          // Отказ должен называть выход, который лежит в этом же экране, —
          // иначе человек без телефона под рукой считает, что застрял.
          showToast('Телефон ещё не подключён — отсканируйте QR или нажмите «Подключу телефон позже»', true);
          return false;
        }
        return true;
      }
      {
        const token = el('input-bot-token').value.trim();
        if (!token) {
          showToast('Bot Token обязателен', true);
          return false;
        }
        state.bot_token = token;
        state.allowed_users = el('input-allowed-users').value.trim();
        await api('POST', '/api/setup/own-bot', {
          bot_token: state.bot_token,
          allowed_users: state.allowed_users,
        });
      }
      return true;
    } catch (e) {
      showToast(friendlyErr(e), true);
      return false;
    }
  }

  // ── Step 2: Agents ───────────────────────────────────────────

  // Шаг показывает НАЙДЕННЫХ помощников, а не весь реестр: стена из строк
  // «не найден» на свежем Windows читалась как «главное здесь не работает».
  // Ненайденные живут в блоке установки — одной кнопкой на агента.
  async function initAgentsStep() {
    const list = el('agent-list');
    list.innerHTML = '<span class="spinner"></span> Ищем помощников на этом компьютере…';
    bindAgentsStepOnce();
    try {
      const caps = await api('GET', '/api/setup/detect');
      state.capabilities = caps;
      if (caps.tunnel) state.tunnelInstalled = caps.tunnel.installed;
      renderAgentsStep(caps);
    } catch (e) {
      el('agent-install')?.classList.add('hidden');
      list.innerHTML = '<p class="hint">' +
        escapeHtml(friendlyErr(e, 'Не удалось проверить помощников на этом компьютере. Это не мешает продолжить настройку.')) +
        '</p>';
    }
  }

  function renderAgentsStep(caps) {
    const list = el('agent-list');
    const found = caps.agents_found || [];
    state.agentsInstallable = caps.agents_installable || [];

    if (found.length) {
      list.innerHTML = found.map(a => `
        <div class="agent-item">
          <span class="agent-icon">${escapeHtml(a.icon)}</span>
          <div class="agent-info">
            <div class="agent-name">${escapeHtml(a.name)}</div>
            <div class="agent-path">${escapeHtml(a.path)}</div>
          </div>
          <span class="agent-status found">найден</span>
        </div>
      `).join('');
    } else {
      list.innerHTML = '<p class="hint">Пока ничего не найдено — это нормально. Настройку можно продолжать: помощники ставятся в любой момент, в том числе прямо отсюда.</p>';
    }

    const installBox = el('agent-install');
    const installList = el('agent-install-list');
    const toggle = el('btn-agent-install-toggle');
    if (installBox && installList && toggle) {
      installBox.classList.toggle('hidden', !state.agentsInstallable.length);
      toggle.textContent = found.length
        ? 'Установить ещё помощника…'
        : 'Установить помощника…';
      // ⚠ ПОДПИСЬ — НЕ КОМАНДА. Под именем каждого агента стояла строка
      // `npm install -g …`, и список из двенадцати агентов превращался в
      // двенадцать строк чужого синтаксиса — читать его человеку незачем, а
      // выполняем его мы сами (аудит онбординга 30.08.2026). Команда осталась
      // во всплывающей подсказке: тем, кто хочет знать, она доступна.
      installList.innerHTML = state.agentsInstallable.map(a => `
        <div class="agent-item">
          <span class="agent-icon">${escapeHtml(a.icon)}</span>
          <div class="agent-info">
            <div class="agent-name">${escapeHtml(a.name)}</div>
            <div class="agent-path" title="${escapeHtml(a.command)}">Поставим сами — команда уйдёт в терминал этого компьютера</div>
          </div>
          <button class="btn btn-secondary btn-inline" type="button" data-install="${escapeHtml(a.id)}">Установить</button>
        </div>
      `).join('');
      installList.querySelectorAll('[data-install]').forEach(btn => {
        btn.addEventListener('click', () => void installAgent(btn.dataset.install, btn));
      });
      // Список перерисован — раскрытость блока сохраняем.
      el('agent-install-body')?.classList.toggle('hidden', !state.agentsInstallOpen);
    }

    el('claude-settings')?.classList.toggle('hidden', !found.some(a => a.id === 'claude'));
  }

  function bindAgentsStepOnce() {
    if (state.agentsBound) return;
    state.agentsBound = true;
    el('btn-agent-install-toggle')?.addEventListener('click', () => {
      state.agentsInstallOpen = !state.agentsInstallOpen;
      el('agent-install-body')?.classList.toggle('hidden', !state.agentsInstallOpen);
    });
    el('btn-agent-recheck')?.addEventListener('click', async () => {
      const btn = el('btn-agent-recheck');
      btn.disabled = true;
      btn.textContent = 'Проверяем…';
      try {
        const caps = await api('GET', '/api/setup/detect');
        state.capabilities = caps;
        renderAgentsStep(caps);
        const found = (caps.agents_found || []).length;
        showToast(found ? 'Найдено помощников: ' + found : 'Пока никого не нашли — установка ещё может идти');
      } catch (e) {
        showToast(friendlyErr(e, 'Не удалось проверить помощников. Попробуйте ещё раз.'), true);
      } finally {
        btn.disabled = false;
        btn.textContent = 'Проверить снова';
      }
    });
  }

  // Установка помощника: агент открывает на ПК окно терминала и выполняет
  // команду из реестра. Если терминала нет (headless-сервер) — команду кладём
  // в буфер обмена, чтобы человек не остался без выхода.
  async function installAgent(id, btn) {
    const item = (state.agentsInstallable || []).find(a => a.id === id);
    const original = btn.textContent;
    btn.disabled = true;
    btn.textContent = 'Открываем…';
    try {
      await api('POST', '/api/setup/install-agent', { id });
      btn.textContent = 'Устанавливается';
      showToast('Открыли терминал на компьютере. Дождитесь конца установки и нажмите «Проверить снова»');
    } catch (e) {
      btn.disabled = false;
      btn.textContent = original;
      const command = item ? item.command : '';
      if (command && navigator.clipboard) {
        try {
          await navigator.clipboard.writeText(command);
          showToast('Не удалось открыть терминал. Команда скопирована — вставьте её в терминале', true);
          return;
        } catch (copyErr) { /* буфер недоступен — покажем обычную ошибку */ }
      }
      showToast(friendlyErr(e, 'Не удалось открыть терминал на этом компьютере.'), true);
    }
  }

  async function saveAgents() {
    try {
      const data = {};
      const modelSelect = el('select-claude-model');
      const permSelect = el('select-claude-perm');
      if (modelSelect) data.claude_model = modelSelect.value;
      if (permSelect) data.claude_permission_mode = permSelect.value;
      await api('POST', '/api/setup/agents', data);
      return true;
    } catch (e) {
      showToast(friendlyErr(e), true);
      return false;
    }
  }

  // ── Step 3: Tunnel ───────────────────────────────────────────

  function initTunnelStep() {
    if (state.tunnelBound) {
      updateTunnelUI();
      return;
    }
    state.tunnelBound = true;
    document.querySelectorAll('[data-tunnel]').forEach(opt => {
      opt.addEventListener('click', () => {
        document.querySelectorAll('[data-tunnel]').forEach(o => o.classList.remove('selected'));
        opt.classList.add('selected');
        state.tunnelMode = opt.dataset.tunnel;
        updateTunnelUI();
        updateSteps();
      });
    });
    el('btn-install-cf').addEventListener('click', installCloudflared);
    updateTunnelUI();
  }

  function updateTunnelUI() {
    const isNamed = state.tunnelMode === 'named';
    const needsCf = state.tunnelMode === 'quick' || isNamed;

    el('tunnel-install').classList.toggle('hidden', !needsCf || state.tunnelInstalled);
    el('tunnel-ready').classList.toggle('hidden', !needsCf || !state.tunnelInstalled);
    el('named-fields').classList.toggle('hidden', !isNamed);
    el('tunnel-starting').classList.add('hidden');
    el('tunnel-result').classList.add('hidden');

    if (state.tunnelInstalled) {
      const pathEl = el('cf-path');
      if (pathEl && state.capabilities && state.capabilities.tunnel) {
        pathEl.textContent = state.capabilities.tunnel.path || '';
      }
    }
  }

  async function installCloudflared() {
    const btn = el('btn-install-cf');
    const progress = el('install-progress');
    const statusEl = el('install-status');

    btn.disabled = true;
    btn.textContent = 'Установка...';
    progress.classList.remove('hidden');
    statusEl.textContent = 'Загрузка cloudflared...';

    try {
      await api('POST', '/api/setup/tunnel/install');
      state.tunnelInstalled = true;
      statusEl.textContent = 'Установлено!';
      el('install-fill').style.width = '100%';
      showToast('cloudflared установлен');

      const tunnelStatus = await api('GET', '/api/setup/tunnel');
      if (tunnelStatus.cloudflared_path) {
        if (state.capabilities && state.capabilities.tunnel) {
          state.capabilities.tunnel.installed = true;
          state.capabilities.tunnel.path = tunnelStatus.cloudflared_path;
        }
      }

      setTimeout(() => updateTunnelUI(), 500);
    } catch (e) {
      statusEl.textContent = friendlyErr(e);
      showToast(friendlyErr(e), true);
      btn.disabled = false;
      btn.textContent = 'Попробовать снова';
    }
  }

  async function saveTunnel() {
    if (state.tunnelMode === 'disabled') {
      await api('POST', '/api/setup/tunnel/config', { mode: 'disabled', autostart: false });
      return true;
    }

    if (!state.tunnelInstalled) {
      showToast('Сначала установите cloudflared', true);
      return false;
    }

    const tunnelCfg = {
      mode: state.tunnelMode,
      autostart: true,
    };

    if (state.tunnelMode === 'named') {
      const name = el('input-tunnel-name').value.trim();
      const url = el('input-tunnel-url').value.trim();
      if (!name) {
        showToast('Укажите имя туннеля', true);
        return false;
      }
      tunnelCfg.tunnel_name = name;
      if (url) tunnelCfg.tunnel_url = url;
    }

    try {
      await api('POST', '/api/setup/tunnel/config', tunnelCfg);
    } catch (e) {
      showToast(friendlyErr(e), true);
      return false;
    }

    el('tunnel-modes').style.opacity = '0.5';
    el('tunnel-modes').style.pointerEvents = 'none';
    el('tunnel-starting').classList.remove('hidden');
    el('tunnel-install').classList.add('hidden');
    el('tunnel-ready').classList.add('hidden');
    el('named-fields').classList.add('hidden');

    try {
      await api('POST', '/api/setup/tunnel/start', { mode: state.tunnelMode });

      let url = '';
      for (let i = 0; i < 20; i++) {
        await new Promise(r => setTimeout(r, 1500));
        const status = await api('GET', '/api/setup/tunnel');
        if (status.url) {
          url = status.url;
          break;
        }
        if (status.status === 'error') {
          throw new Error(status.error || 'Tunnel failed');
        }
      }

      el('tunnel-starting').classList.add('hidden');

      if (url) {
        state.tunnelURL = url;
        el('tunnel-url-display').textContent = url;
        el('tunnel-result').classList.remove('hidden');
      } else if (state.tunnelMode === 'named') {
        el('tunnel-result').classList.remove('hidden');
        el('tunnel-url-display').textContent = tunnelCfg.tunnel_url || 'Туннель запущен';
        document.querySelector('#tunnel-result .hint').textContent = 'Туннель запущен. URL: ' + (tunnelCfg.tunnel_url || 'см. Cloudflare Dashboard');
      } else {
        showToast('Туннель запущен, но URL не получен. Проверьте логи.', true);
      }

      el('tunnel-modes').style.opacity = '1';
      el('tunnel-modes').style.pointerEvents = 'auto';
      return true;
    } catch (e) {
      el('tunnel-starting').classList.add('hidden');
      el('tunnel-modes').style.opacity = '1';
      el('tunnel-modes').style.pointerEvents = 'auto';
      showToast(friendlyErr(e), true);
      updateTunnelUI();
      return false;
    }
  }

  // ── Step 4: Complete (standalone/own_bot финализация) ───────

  async function initCompleteStep() {
    if (state.completionStarted) return;
    state.completionStarted = true;
    const info = el('complete-info');
    const sub = el('step4-sub');
    info.innerHTML = '<span class="spinner"></span> Заканчиваем настройку…';
    try {
      const completeEndpoint = state.mode === 'standalone' ? '/api/setup/standalone' : '/api/setup/complete';
      // Подтверждение пейринга гасит setup-сервер (порт уходит основному
      // серверу), поэтому единственный запрос падал ровно в секунду успеха и
      // человек читал «Нет связи с интернетом». Ретраим, как в finishSetup.
      state.completionData = await completeWithRetry(completeEndpoint, 30000);
      const d = state.completionData;

      let urlsHtml = '';
      if (d.server_urls && d.server_urls.length > 0) {
        urlsHtml = d.server_urls.map(u => `<dd>${u}</dd>`).join('');
      }

      let tunnelHtml = '';
      if (state.tunnelURL) {
        tunnelHtml = `
          <div class="tunnel-url-box" style="margin-bottom:16px" onclick="navigator.clipboard.writeText('${state.tunnelURL}')">
            ${state.tunnelURL}
          </div>
        `;
      }

      // Если телефон уже подключился на шаге «Способ подключения», не просим
      // сканировать снова — показываем подтверждение (повторный QR путал).
      const pairHtml = state.cloudPairConfirmed
        ? `<div class="qr-container">
             <p class="hint ok" style="font-size:16px">✅ Телефон подключён</p>
             <p class="hint">Подключить ещё один можно позже в Панели Remotai (значок у часов → «Открыть окно»).</p>
           </div>`
        : `<div class="qr-container">
             <img src="${d.qr_url}" alt="Pairing QR Code" />
             <p class="hint" style="margin-top:8px">Отсканируйте в приложении Remotai</p>
           </div>
           <!-- ⚠ ЭТО ДРУГОЙ КОД. На шаге «Способ подключения» код короткий
                (AB12-CD34) и живёт на релее; здесь — длинная строка с адресом
                этого компьютера и ключом к нему, для подключения по локальной
                сети. Оба назывались «код подключения», и человек искал в
                приложении не то (аудит онбординга 30.08.2026). -->
           <p class="hint" style="margin-top:10px">Код этого компьютера для локальной сети — вставьте его в приложении, если QR не сканируется:</p>
           <div class="pairing-code" onclick="navigator.clipboard.writeText(this.dataset.code).then(()=>{document.querySelector('.copy-hint').textContent='Скопировано в буфер этого компьютера';});" data-code="${d.pairing_code}">
             ${d.pairing_code}
           </div>
           <p class="copy-hint">Нажмите, чтобы скопировать в буфер компьютера — набирать вручную его не нужно</p>`;

      // Подзаголовок шага не должен просить сканировать QR, когда телефон уже
      // подключён; технический дамп — под «Технические детали», как на welcome.
      if (sub) {
        sub.textContent = state.cloudPairConfirmed
          ? 'Компьютер подключён к вашему аккаунту Remotai'
          : 'Подключите телефон: отсканируйте QR или вставьте код этого компьютера';
      }

      info.innerHTML = `
        ${tunnelHtml}
        ${pairHtml}
        <dl class="device-info" style="margin-top:16px">
          <dt>Режим</dt><dd>${d.mode === 'central_bot' ? 'Через интернет' : d.mode === 'standalone' ? 'Локальная сеть' : 'Свой бот'}</dd>
          ${urlsHtml ? '<dt>Адрес</dt>' + urlsHtml : ''}
        </dl>
        <details class="hint" style="margin-top:10px">
          <summary>Технические детали</summary>
          <dl class="device-info" style="margin-top:8px">
            <dt>Идентификатор компьютера</dt><dd>${d.device_id}</dd>
          </dl>
        </details>
        <p class="hint" style="margin-top:12px">Это окно можно закрыть — Remotai продолжит работать в фоне (значок у часов).</p>
      `;
    } catch (e) {
      state.completionStarted = false;
      // Пейринг уже подтверждён — значит сбой запроса не отменяет успех:
      // сетевую ошибку в этом месте человек читал как «настройка не удалась».
      if (state.cloudPairConfirmed) {
        if (sub) sub.textContent = 'Компьютер подключён к вашему аккаунту Remotai';
        info.innerHTML = `
          <p class="hint ok" style="font-size:16px">✅ Телефон подключён</p>
          <p class="hint">Подключить ещё один можно позже в Панели Remotai (значок у часов → «Открыть окно»).</p>
          <p class="hint" style="margin-top:12px">Это окно можно закрыть — Remotai продолжит работать в фоне.</p>
        `;
        return;
      }
      info.innerHTML = `<p style="color:var(--error)">${friendlyErr(e)}</p>`;
    }
  }

  // POST завершения настройки с ретраями: setup-сервер в этот момент уступает
  // порт основному, и первый запрос закономерно падает.
  async function completeWithRetry(endpoint, timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      try {
        return await api('POST', endpoint, {});
      } catch (e) {
        if (Date.now() >= deadline) throw e;
        await new Promise(r => setTimeout(r, 1000));
      }
    }
  }

  // ── Navigation ───────────────────────────────────────────────

  async function nextStep() {
    const btn = el('btn-next');
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span>';

    let ok = true;
    if (currentStep === 1) ok = await saveMode();
    if (currentStep === 2) ok = await saveAgents();
    if (currentStep === 3) ok = await saveTunnel();

    btn.disabled = false;
    updateSteps();

    if (!ok) return;

    if (currentStep < totalSteps - 1) {
      const route = wizardRoute();
      const next = route[route.indexOf(currentStep) + 1];
      if (next !== undefined) showStep(next);
    }
  }

  async function prevStep() {
    if (currentStep > 0) {
      if (currentStep === 1 && state.mode === 'central_bot' && !state.cloudPairConfirmed) {
        try {
          await wizardPairing.cancel();
        } catch (e) {
          showToast(friendlyErr(e, 'Не удалось отменить подключение.'), true);
          return;
        }
      }
      const route = wizardRoute();
      const prev = route[route.indexOf(currentStep) - 1];
      if (prev !== undefined) showStep(prev);
    }
  }

  // Завершение мастера: открываем клиент с api-токеном (по образцу
  // openLocalClient). Голый /miniapp в свежем WebView без localStorage
  // упирался в RequireAuth — экран «Войдите через Telegram» на только что
  // настроенном ПК. Сервер в этот момент может ещё перезапускаться
  // (setup → основной) — подождём его ретраями.
  async function finishSetup(destination = '') {
    showToast('Настройка завершена!');
    const deadline = Date.now() + 30000;
    for (;;) {
      try {
        const acc = state.access || await api('GET', '/api/setup/local-access');
        state.access = acc;
        location.href = '/miniapp?token=' + encodeURIComponent(acc.token) + destination;
        return;
      } catch (e) {
        if (Date.now() >= deadline) {
          location.href = '/miniapp' + destination;
          return;
        }
        await new Promise(r => setTimeout(r, 1000));
      }
    }
  }

  // ── Init ─────────────────────────────────────────────────────

  // Мастер первого запуска нельзя показывать до ответа /api/setup/status: на
  // настроенном компьютере шаг «Способ подключения» перезаписал бы рабочую
  // конфигурацию. Пока статуса нет — нейтральный экран, при сбое — «Повторить».
  /**
   * Где человек остановился в прошлый раз.
   *
   * Аудит онбординга 30.08.2026: закрыл окно посреди мастера — вернулся на
   * «Добро пожаловать», выбранный способ подключения потерян. Мастер идёт
   * минуты, а окно закрывают буднично (пошли за телефоном), поэтому шаг и
   * способ переживают закрытие. Пейринг сюда НЕ входит: код живёт час на
   * релее, и его состояние спрашивается заново.
   */
  const PROGRESS_KEY = 'remotai.setup.progress.v1';

  function saveProgress() {
    try {
      localStorage.setItem(PROGRESS_KEY, JSON.stringify({ step: currentStep, mode: state.mode, at: Date.now() }));
    } catch (e) { /* приватное окно — прогресс просто не переживёт закрытия */ }
  }

  function readProgress() {
    try {
      const raw = JSON.parse(localStorage.getItem(PROGRESS_KEY) || 'null');
      if (!raw || typeof raw.step !== 'number') return null;
      // Сутки — граница «того же захода». Прошлогодний шаг восстанавливать
      // нельзя: за это время у машины могло измениться всё.
      if (!raw.at || Date.now() - raw.at > 86400000) return null;
      return raw;
    } catch (e) {
      return null;
    }
  }

  function clearProgress() {
    try { localStorage.removeItem(PROGRESS_KEY); } catch (e) { /* см. saveProgress */ }
  }

  function startWizard(status) {
    el('boot')?.classList.add('hidden');
    document.querySelector('.steps')?.classList.remove('hidden');
    document.querySelector('.btn-row')?.classList.remove('hidden');
    const saved = readProgress();
    if (saved && saved.mode) {
      state.mode = saved.mode;
      const card = document.querySelector('.mode-option[data-mode="' + saved.mode + '"]');
      if (card) {
        document.querySelectorAll('.mode-option').forEach(el2 => el2.classList.remove('selected'));
        card.classList.add('selected');
      }
    }
    // Последний шаг («Готово!») не восстанавливаем: настройка там уже
    // завершена, и мастеру нечего доделывать.
    renderWelcome(status);
    showStep(saved && saved.step > 0 && saved.step < totalSteps - 1 ? saved.step : 0);
  }

  function showBootError() {
    el('boot-spinner')?.classList.add('hidden');
    const title = el('boot-title'), sub = el('boot-sub');
    if (title) title.textContent = 'Не удаётся связаться с Remotai на этом компьютере';
    if (sub) {
      sub.textContent = 'Программа могла ещё не запуститься или сменить порт. ' +
        'Нажмите «Повторить»; окно всегда можно открыть заново из значка у часов.';
    }
    el('boot-actions')?.classList.remove('hidden');
  }

  // Статус с ретраями (интервал 1 с): окно открывается через ~0,5 с после
  // старта процесса, а после смены порта — сразу на новом адресе, поэтому
  // первый запрос закономерно может не успеть.
  async function loadStatusAndRender() {
    el('boot')?.classList.remove('hidden');
    el('boot-actions')?.classList.add('hidden');
    el('boot-spinner')?.classList.remove('hidden');
    const title = el('boot-title'), sub = el('boot-sub');
    if (title) title.textContent = 'Запускаем Remotai…';
    if (sub) sub.textContent = 'Проверяем состояние программы на этом компьютере.';
    const deadline = Date.now() + 30000;
    for (;;) {
      try {
        const status = await api('GET', '/api/setup/status');
        renderVersion(status.version);
        if (status.configured) showPanel(status);
        else startWizard(status);
        return;
      } catch (e) {
        if (Date.now() >= deadline) {
          showBootError();
          return;
        }
        await new Promise(r => setTimeout(r, 1000));
      }
    }
  }

  document.addEventListener('DOMContentLoaded', () => {
    initModeStep();
    bindExternalLinks();

    el('btn-control-other')?.addEventListener('click', async () => {
      const btn = el('btn-control-other');
      btn.disabled = true;
      el('btn-next').disabled = true;
      try {
        // Завершаем первый запуск без привязки этой машины к облаку.
        // Вход в клиенте открывает список ДРУГИХ компьютеров аккаунта.
        await api('POST', '/api/setup/mode', { mode: 'central_bot' });
        state.completionData = await api('POST', '/api/setup/complete', {});
        clearProgress();
        await finishSetup('#/cloud-login?next=' + encodeURIComponent('/infrastructure?back=' + encodeURIComponent('/pty?new=1&agent=1')));
      } catch (e) {
        btn.disabled = false;
        el('btn-next').disabled = false;
        showToast(friendlyErr(e), true);
      }
    });
    el('btn-next').addEventListener('click', () => {
      if (currentStep === totalSteps - 1) {
        finishSetup();
      } else {
        nextStep();
      }
    });
    el('btn-back').addEventListener('click', () => void prevStep());
    el('btn-boot-retry').addEventListener('click', () => void loadStatusAndRender());

    // Единая точка входа: статус решает — мастер или панель управления.
    // Пейринг НЕ стартует здесь: 15-минутный код начинает тикать со входа
    // на шаг «Способ подключения» (см. showStep), а не с загрузки страницы.
    void loadStatusAndRender();
  });
})();
