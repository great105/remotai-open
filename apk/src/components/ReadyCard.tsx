import { useCallback, useEffect, useState } from "react";
import { getHealthReport, getSystemStats, onConnectionChange, type HealthCheck, type HealthReport } from "../api";
import type { SystemStats } from "../types";
import { t } from "../i18n";
import { usePolling } from "../hooks/usePolling";
import { mapApiError, isPcOffline, humanSize } from "@tgcontrol/shared";
import { useCapabilities } from "../hooks/useCapabilities";
import { getMode, getSelectedDeviceName } from "../config";
import { humanDeviceName } from "../devices";

// Названия строк проверки. Ключи словаря, а не готовые фразы: имена сущностей
// («Экран ПК», «Терминалы») должны меняться в одном месте.
const LABEL_KEYS: Record<string, string> = {
  app:    "health.app",
  relay:  "health.relay",
  bot:    "health.bot",
  tunnel: "health.tunnel",
  public: "health.public",
  pty:    "health.pty",
  notify: "health.notify",
};
const labelOf = (k: string) => (LABEL_KEYS[k] ? t(LABEL_KEYS[k]) : k);

// Строки развёрнутой диагностики. «Публичного URL» здесь нет намеренно: для
// человека это та же мысль, что «Доступ из интернета» (см. publicRow).
const ROW_KEYS = ["app", "relay", "bot", "public", "pty", "notify"];

// Проверки, провал которых НЕ отменяет ответ «компьютер на связи»: доступ из
// интернета и туннель — про удалённый доступ, а не про жизнь машины, и в
// LAN-режиме их отсутствие штатно.
const WARN_ONLY = new Set(["public", "tunnel"]);

// Ноты приходят с ПК готовым текстом, и агент версии ≤2.28.1 присылает их
// по-английски. Пока такой агент жив (обновляется он сам, но не мгновенно),
// подменяем две известные фразы, иначе в карточке висит «Web server up».
const LEGACY_NOTE_KEYS: Record<string, string> = {
  "Web server up": "health.note.appUp",
  "Bot token configured": "health.note.botToken",
};

// ── Диск на главной ────────────────────────────────────────────────────────
// Карточка отвечала только про связь: компьютер мог стоять с диском на 98%, и
// главная про это не говорила ни слова — а полный диск роняет сборки и агентов
// задолго до того, как порвётся связь.
//
// Взять `stats.disk.percent` напрямую нельзя, хотя поле в типе есть: агент
// отдаёт тома, отсортированные по заполненности, и первым на штатной Ubuntu
// всегда идёт snap-образ (squashfs — read-only архив, 100% по определению), а на
// Windows — смонтированный ISO. Тогда главная кричала бы «диск полон» там, где
// на корне триста гигабайт свободно.
//
// Правило выбора тома — копия apk/src/pages/SystemView.tsx (46–103, 948–958),
// чтобы главная и «Система» считали диск ОДИНАКОВО. Копия временная: правило
// просится в общий модуль, но SystemView сейчас за другим исполнителем — вынести
// вместе, одной правкой, иначе два фильтра разъедутся на первом же изменении.
type Volume = {
  total: number; used: number; free: number; percent: number;
  mount?: string; device?: string; fstype?: string;
};

/** Файловые системы, которые НЕ отвечают на вопрос «сколько осталось места». */
const SERVICE_FSTYPES = new Set([
  "squashfs", "snapfuse", "fuse.snapfuse", "iso9660", "cd9660", "udf", "cdfs",
  "overlay", "overlayfs", "tmpfs", "devtmpfs", "ramfs", "efivarfs", "erofs",
]);

/** Меньше двух гигабайт — служебный раздел (EFI, recovery, образ), не «диск». */
const MIN_VOLUME_BYTES = 2 * 1024 * 1024 * 1024;

function isServiceVolume(volume: Volume): boolean {
  if (SERVICE_FSTYPES.has((volume.fstype || "").toLowerCase())) return true;
  const device = (volume.device || "").toLowerCase();
  if (device.startsWith("/dev/loop")) return true;
  const mount = (volume.mount || "").toLowerCase();
  if (mount.startsWith("/snap/") || mount.startsWith("/var/snap/") || mount.startsWith("/var/lib/snapd/")) return true;
  return !Number.isFinite(volume.total) || volume.total < MIN_VOLUME_BYTES;
}

/** Системный том: тот, где живут профиль пользователя и рабочие папки агента. */
function isSystemVolume(volume: Volume): boolean {
  const mount = (volume.mount || "").trim();
  return mount === "/" || /^c:[\\/]?$/i.test(mount);
}

/** Человеческая подпись тома: «C:» вместо «C:\» и «/var» вместо сырого mount. */
function volumeLabel(volume: Volume): string {
  const mount = (volume.mount || "").trim();
  const winDrive = /^([a-zA-Z]):(?:[\\/]|$)/.exec(mount);
  if (winDrive) return `${winDrive[1].toUpperCase()}:`;
  if (mount) return mount.replace(/(.)[\\/]+$/, "$1");
  return volume.device || "";
}

/** Главный том: сначала выбрасываем служебные, потом берём системный, а не
 *  самый заполненный (иначе «главным» станет внешний диск или флешка). */
function mainVolume(stats: SystemStats | null): Volume | null {
  if (!stats) return null;
  const all: Volume[] = stats.disks?.length ? stats.disks : stats.disk ? [stats.disk] : [];
  const volumes = all.filter((volume) => !isServiceVolume(volume));
  return volumes.find(isSystemVolume) || volumes[0] || stats.disk || null;
}

// Порог тревоги по диску — то же число, что на «Системе» (SystemView: percent >= 90).
const DISK_ALERT = 90;
// Порог, с которого свёрнутая строка переходит с пары «процессор · память» на
// один худший показатель. Ниже него не переключаемся намеренно: при cpu 41 и
// ram 43 строка мигала бы между двумя словами каждые 30 секунд (такт поллинга).
const WORST_SHOW = 80;

/**
 * Худший показатель для свёрнутой строки. `null` — никто не дотянул до порога,
 * значит строка остаётся прежней парой чисел: падать до одного числа там, где
 * всё спокойно, значит терять CPU — а по нему и судят, занят ли компьютер.
 */
function pickWorstMetric(
  stats: SystemStats | null,
  disk: Volume | null,
): { kind: "cpu" | "ram" | "disk"; percent: number } | null {
  if (!stats) return null;
  const all = [
    { kind: "cpu" as const, percent: stats.cpu.percent },
    { kind: "ram" as const, percent: stats.memory.percent },
    ...(disk ? [{ kind: "disk" as const, percent: disk.percent }] : []),
  ].filter((m) => Number.isFinite(m.percent));
  const worst = all.reduce<{ kind: "cpu" | "ram" | "disk"; percent: number } | null>(
    (best, m) => (!best || m.percent > best.percent ? m : best),
    null,
  );
  return worst && worst.percent >= WORST_SHOW ? worst : null;
}

/**
 * Текст ноты проверки. Агент ≤2.28.1 клал число в extra, а в ноте оставлял
 * обрубок без цифры («Активных терминалов» у pty, задержка у public) — extra
 * никто не рендерил. Новый агент шлёт число прямо в ноте, здесь — совместимость
 * со старым. К конкретному ключу не привязываемся: обновление агента не
 * мгновенно, а число в extra кладут несколько проверок (см. api_health.go).
 * Условие — extra именно число (у tunnel там объект) и в ноте ещё нет цифр.
 */
function noteOf(c: HealthCheck): string {
  const raw = c.note || "";
  // t() при отсутствии ключа возвращает сам ключ — сравнением ловим это и
  // оставляем исходную ноту: английская фраза лучше, чем «health.note.appUp».
  const legacyKey = LEGACY_NOTE_KEYS[raw];
  const legacy = legacyKey ? t(legacyKey) : "";
  const note = legacy && legacy !== legacyKey ? legacy : raw;
  if (typeof c.extra === "number" && !/\d/.test(note)) {
    return note ? `${note}: ${c.extra}` : String(c.extra);
  }
  return note;
}

/**
 * Одна строка про доступ снаружи вместо двух. «Публичный URL» и «Доступ из
 * интернета» человек читает как одно и то же, причём первое — ещё и словом
 * «URL»: понять, какая строка важнее и что делать, если красная именно первая,
 * было невозможно. Адрес туннеля становится примечанием того самого доступа,
 * ради которого он и поднят.
 */
function publicRow(report: HealthReport): HealthCheck | undefined {
  const pub = report.checks["public"];
  const tunnel = report.checks["tunnel"];
  if (!pub) return tunnel;
  if (!tunnel) return pub;
  const tunnelNote = noteOf(tunnel);
  // Живой туннель добавляет к строке свой адрес, сломанный — свою причину;
  // «Не используется (доступ через облако)» не добавляет ничего.
  const extra = tunnel.ok
    ? (/^https?:/i.test(tunnelNote) ? tunnelNote : "")
    : tunnelNote;
  return {
    ok: pub.ok && tunnel.ok,
    note: [noteOf(pub), extra].filter(Boolean).join(" · "),
  };
}

interface Props {
  onOpenSettings?: () => void;
  /**
   * Дверь в «Система → Питание и нагрузка» прямо с главной (аудит ИА
   * 02.09.2026, волна 3): выключить или перезагрузить машину — самый частый
   * «системный» шаг, а путь к нему был «Система → вкладка Питание».
   */
  onOpenPower?: () => void;
  /**
   * Сообщить наверх, что ПК не в сети. Главная гасит по этому признаку запуск
   * терминала, плитки и шаги чеклиста: в LAN и окне exe живой канал причину не
   * называет («net»), и провал health — единственный честный сигнал.
   */
  onOfflineChange?: (offline: boolean) => void;
}

/**
 * Статус готовности. По умолчанию — одна строка-индикатор (главный экран не
 * должен начинаться с диагностической простыни); тап разворачивает детали.
 * Идентична miniapp/components/ReadyCard (project_apk_miniapp_dup.md).
 */
export function ReadyCard({ onOpenSettings, onOpenPower, onOfflineChange }: Props) {
  const { platform, hasDisplay, hostname } = useCapabilities();
  const serverEntity = platform === "linux" && !hasDisplay;
  const [report, setReport] = useState<HealthReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  // ПК не в сети — это не «не удалось проверить готовность», а понятная причина.
  const [offline, setOffline] = useState(false);
  // То же самое, но узнанное из живого канала (кадр agent_status): там об этом
  // уже написал глобальный баннер связи, и повторять его текст в карточке —
  // два одинаковых утверждения на одном экране.
  const [linkOffline, setLinkOffline] = useState(false);
  const [expanded, setExpanded] = useState(false);
  // Нагрузка ПК в свёрнутой строке: «всё готово» без цифр не отвечает на
  // главный вопрос — жив ли компьютер и не занят ли он под завязку.
  const [stats, setStats] = useState<SystemStats | null>(null);

  // useCallback: ссылка нужна стабильной для подписки на живой канал (эффект
  // ниже) — иначе он пересоздавался бы каждым рендером.
  const refresh = useCallback(async (silent = false) => {
    if (!silent) setRefreshing(true);
    try {
      const r = await getHealthReport();
      setReport(r);
      setError(null);
      // ПК ответил — снимаем «не в сети». Без этого сброса карточка после
      // единственного обрыва навсегда застревала в офлайн-ветке ниже.
      setOffline(false);
      setLinkOffline(false);
    } catch (e: any) {
      // Раньше здесь оказывался сырой английский «agent offline».
      setError(mapApiError(e));
      setOffline(isPcOffline(e));
    } finally {
      setRefreshing(false);
    }
    // Метрики — необязательная надстройка: их отсутствие не должно ломать
    // статус готовности.
    getSystemStats().then(setStats).catch(() => setStats(null));
  }, []);

  /**
   * Живой канал знает правду мгновенно, а карточка жила только по таймеру и до
   * минуты противоречила баннеру и точке в чипе устройства: то «Компьютер на
   * связи · CPU 12%» у спящей машины, то красное «не в сети» у уже включённой.
   *
   * Реагируем именно на `reason === "pc_offline"` (кадр agent_status от релея),
   * а не на `!connected`: в вебе, Telegram и окне exe сокет намеренно рвётся в
   * фоне, и это не значит, что ПК умер.
   */
  useEffect(() => onConnectionChange((state) => {
    if (state.reason === "pc_offline") {
      setOffline(true);
      setLinkOffline(true);
      setError(null);
      return;
    }
    // ПК вернулся — перечитываем сразу, не ожидая очередного такта поллинга.
    if (state.connected) void refresh(true);
  }), [refresh]);

  // Карточка отвечает на один вопрос «жив ли ПК» — 30 секунд достаточно, а
  // каждый прогон health занимает агента (замер CPU — ~500 мс). Первый запрос
  // делает сам хук, он же молчит, пока приложение в фоне; у офлайн-ПК
  // спрашиваем реже — он вернётся сам.
  usePolling(() => refresh(true), offline ? 60000 : 30000);

  useEffect(() => { onOfflineChange?.(offline); }, [offline, onOfflineChange]);

  // Офлайн проверяем ДО «Проверяю готовность…»: правда из живого канала
  // приходит раньше первого ответа health, и спиннер поверх неё был бы ложью.
  if (offline) {
    // Баннер связи над экраном уже сказал «Компьютер не в сети» — карточка не
    // дублирует ни заголовок, ни подсказку, а объясняет отсутствие цифр.
    if (linkOffline) {
      return (
        <div className="card ready-card ready-card-bad">
          <div className="ready-card-head">
            <span className="ready-status-dot bad" aria-hidden />
            <span className="ready-card-title">{t("health.offlinePaused")}</span>
            <button
              className="ready-card-refresh"
              onClick={() => refresh()}
              disabled={refreshing}
              aria-label={t("offline.retry")}
            >
              {refreshing ? "…" : "↻"}
            </button>
          </div>
          {/* Одна строка про ДЕЙСТВИЕ. Заголовок баннера не повторяем — он
              сверху и говорит, какая машина не в сети, — но «что мне делать»
              на главной не отвечал никто: обещание «подключится сам» жило
              только на «Терминалах» и в «Файлах». */}
          <div className="ready-card-hint">{t("health.offlinePausedHint")}</div>
        </div>
      );
    }
    return (
      <div className="card ready-card ready-card-bad">
        <div className="ready-card-head">
          <span className="ready-status-dot bad" aria-hidden />
          <span className="ready-card-title">{t(serverEntity ? "offline.titleServer" : "offline.title")}</span>
        </div>
        <div className="ready-card-hint">{t("offline.hint")}</div>
        <div style={{ padding: "0 14px 12px" }}>
          <button className="btn btn-sm btn-secondary" onClick={() => refresh()}>
            {t("offline.retry")}
          </button>
        </div>
      </div>
    );
  }
  if (!report && !error) {
    // Первый кадр продукта: текст обязан лежать внутри .ready-card-head, иначе
    // у свёрнутой карточки (padding: 0) он прижимается к левому и верхнему
    // краю плашки. ready-card-static снимает курсор-палец и «нажатие» — нажимать
    // здесь нечего.
    return (
      <div className="card ready-card ready-card-collapsed ready-card-static">
        <div className="ready-card-head">
          <span className="ready-card-title">{t("health.checking")}</span>
        </div>
      </div>
    );
  }
  if (error || !report) {
    return (
      <div className="card ready-card ready-card-bad">
        <div className="ready-card-title">{t("health.failed")}</div>
        <div style={{ fontSize: 12, opacity: 0.7 }}>{error}</div>
        <button className="btn btn-sm btn-secondary" onClick={() => refresh()} style={{ marginTop: 8 }}>
          {t("conn.retry")}
        </button>
      </div>
    );
  }

  const overall = report.ok ? "ok" : "warn";
  const order = ["app", "relay", "bot", "tunnel", "public", "pty", "notify"];
  // При проблеме называем ПЕРВУЮ причину: «Есть нерешённые проблемы» не говорит
  // ни что сломано, ни что делать — приходилось разворачивать карточку.
  const firstBad = order.find((k) => report.checks[k] && !report.checks[k].ok && !WARN_ONLY.has(k));
  const badNote = firstBad ? noteOf(report.checks[firstBad]) : "";
  // Заголовок ВСЕГДА отвечает на вопрос, ради которого открыли приложение:
  // включён ли компьютер. Раньше жалоба подсистемы («Telegram-бот: нет связи с
  // 15:04 — возможна блокировка API») ЗАМЕЩАЛА главный статус и обрезалась
  // многоточием, и ответа на экране не оставалось вовсе. Проблема переехала
  // строкой ниже (.ready-card-problem), где помещается целиком.
  //
  // Имя машины — здесь же: единственная машина, чьё имя не было видно, — та,
  // которой вы управляете (плитки соседних называли их 13px полужирным, а свою
  // знал только 11px-чип в шапке). После переключения экран внешне почти не
  // менялся — прямой путь ответить агенту не на том компьютере.
  //
  // Своей копии фильтра «имя похоже на адрес» здесь больше нет: то же правило
  // нужно чипу в шапке и заголовку удалёнки (там до сих пор писали
  // «127.0.0.1»), а три копии разъехались бы. Правило живёт в devices.ts.
  // Запасное слово пустое намеренно: «На связи · Этот компьютер» не добавляет
  // к «Компьютер на связи» ничего, кроме второго «компьютера» в одной строке.
  const deviceName = humanDeviceName(getMode() === "cloud" ? getSelectedDeviceName() : hostname, "");
  // Состояние идёт ПЕРВЫМ и целиком, имя машины — после него и с правом быть
  // обрезанным. «Рабочий компьютер на связи» в облаке превращалось в «Рабочий
  // компьютер на…»: пропадало ровно то слово, ради которого приложение и
  // открыли, — а имя своей машины строкой ниже повторяет помеченная плитка
  // («Управляете сейчас»), то есть оно здесь вторично.
  //
  // Короткая форма («На связи») осмысленна только рядом с именем. Нет имени
  // (LAN, адрес вместо имени) — остаётся полная фраза. И на всякий случай
  // сверяемся, что ключ в словаре есть: t() при отсутствии ключа возвращает
  // сам ключ (см. noteOf выше), а «home.status.onlineShort · Ноутбук» главным
  // ответом продукта быть не может.
  const shortOnline = t("home.status.onlineShort");
  const named = !!deviceName && shortOnline !== "home.status.onlineShort";
  const stateText = named
    ? shortOnline
    : t(serverEntity ? "home.status.onlineServer" : "home.status.online");
  const problem = firstBad
    ? `${labelOf(firstBad)}: ${badNote}`
    : overall === "ok" ? "" : t("health.problems");
  // Свёрнутая строка показывает пару «процессор · память», пока всё спокойно, и
  // ОДИН худший показатель, как только он перевалил за 80%: два спокойных числа
  // заслоняли то единственное, из-за которого работа и встанет.
  const disk = mainVolume(stats);
  const worst = pickWorstMetric(stats, disk);
  const metrics = !stats
    ? null
    : worst
      ? t("home.status.worst", {
          label: t(`home.status.metric.${worst.kind}`),
          n: Math.round(worst.percent),
        })
      : t("home.status.load", {
          cpu: Math.round(stats.cpu.percent),
          ram: Math.round(stats.memory.percent),
        });
  // Диск ≥90% — отдельная строка-предупреждение, а НЕ цвет карточки: зелёный
  // здесь отвечает ровно на один вопрос — «жив ли компьютер и есть ли связь», и
  // переопределять его местом на диске значило бы сделать цвет двусмысленным
  // навсегда (то же правило, что у WARN_ONLY выше). stats приезжает отдельным
  // запросом и может не прийти вовсе — тогда строки просто нет.
  const diskLow = disk && disk.percent >= DISK_ALERT
    ? t("home.status.diskLow", {
        volume: volumeLabel(disk),
        free: humanSize(disk.free),
        total: humanSize(disk.total),
      })
    : "";

  return (
    <div className={`card ready-card ready-card-${overall}${expanded ? "" : " ready-card-collapsed"}`}>
      <button
        className="ready-card-head"
        onClick={() => setExpanded(!expanded)}
        aria-expanded={expanded}
      >
        <span className={`ready-status-dot ${overall}`} aria-hidden />
        {/* Два span'а вместо одной строки: обрезаться (line-clamp у свёрнутой
            карточки) имеет право только хвост, а хвост здесь — имя машины. */}
        <span className="ready-card-title">
          <span className="ready-card-state">{stateText}</span>
          {named && <span className="ready-card-device">{" · "}{deviceName}</span>}
        </span>
        {/* Цифры нагрузки — самое неважное в строке: на узком экране им место
            уступают и состояние, и имя (правило прячет .ready-card-metrics). */}
        {metrics && !expanded && <span className="ready-card-metrics">{metrics}</span>}
        <span className={`ready-card-chevron${expanded ? " open" : ""}`} aria-hidden>{"›"}</span>
      </button>

      {/* Проблема — второй строкой под главным статусом, а не вместо него. */}
      {problem && !expanded && (
        <div className="ready-card-problem" role="status">{problem}</div>
      )}

      {/* Диск — ПОСЛЕ проблемы связи: сломанная связь всегда важнее места. */}
      {diskLow && !expanded && (
        <div className="ready-card-problem" role="status">{diskLow}</div>
      )}

      {/* «Питание и нагрузка →» видна и у СВЁРНУТОЙ карточки: разворачивают её
          редко, а ПК на связи — ровно то состояние, в котором его выключают
          или перезагружают. Отдельной строкой под шапкой, а не внутри неё:
          шапка — <button>, и кнопка в кнопке недопустима (клик к тому же
          разворачивал бы карточку). Отступы — как у .ready-card-problem:
          у свёрнутой карточки padding: 0. */}
      {onOpenPower && !expanded && (
        <div className="ready-card-foot" style={{ margin: "0 8px 4px" }}>
          {/* Цель под палец: у .ready-card-link padding: 0 и кегль 11 — ссылка
              выходила 114×12, гейт «цели для пальца» её ловил. */}
          <button
            className="ready-card-link"
            style={{ minHeight: 44, display: "inline-flex", alignItems: "center", padding: "0 6px", fontSize: 13 }}
            onClick={onOpenPower}
          >
            {t("health.power")}
          </button>
        </div>
      )}

      {expanded && (
        <>
          <ul className="ready-card-list">
            {ROW_KEYS.map((k) => {
              const c = k === "public" ? publicRow(report) : report.checks[k];
              if (!c) return null;
              const note = k === "public" ? c.note : noteOf(c);
              // Результат проверки нельзя передавать ОДНИМ цветом: точка помечена
              // aria-hidden (для скринридера её нет вовсе), а зелёная и красная
              // одного размера неразличимы при дальтонизме. Глиф даёт форму,
              // .sr-only — слово, ради которого карточку и разворачивают.
              const tone = c.ok ? "ok" : WARN_ONLY.has(k) ? "warn" : "bad";
              return (
                <li key={k} className="ready-card-row">
                  <span className={`ready-status-mark ${tone}`} aria-hidden>{c.ok ? "✓" : "✕"}</span>
                  <span className="ready-card-label">{labelOf(k)}</span>
                  <span className="sr-only">
                    {t(c.ok ? "health.rowOk" : tone === "warn" ? "health.rowWarn" : "health.rowBad")}
                  </span>
                  {note && <span className="ready-card-note" title={note}>{note}</span>}
                </li>
              );
            })}
            {/* «Диск» — единственная строка списка, которую считает КЛИЕНТ:
                ROW_KEYS выше — это имена СЕРВЕРНЫХ проверок (api_health.go), и
                проверки диска агент не присылает вовсе. Тон warn, а не bad:
                кончающееся место не отменяет ответ «компьютер на связи» — то же
                правило, по которому живёт WARN_ONLY. */}
            {disk && (
              <li className="ready-card-row">
                <span className={`ready-status-mark ${disk.percent < DISK_ALERT ? "ok" : "warn"}`} aria-hidden>
                  {disk.percent < DISK_ALERT ? "✓" : "✕"}
                </span>
                <span className="ready-card-label">{t("home.status.diskLabel")}</span>
                <span className="sr-only">
                  {t(disk.percent < DISK_ALERT ? "health.rowOk" : "health.rowWarn")}
                </span>
                <span className="ready-card-note">
                  {t("home.status.diskNote", {
                    volume: volumeLabel(disk),
                    percent: Math.round(disk.percent),
                    free: humanSize(disk.free),
                  })}
                </span>
              </li>
            )}
          </ul>
          <div className="ready-card-foot">
            <span>{t("health.checkedAt", { time: new Date(report.ran_at).toLocaleTimeString() })}</span>
            <button
              className="ready-card-refresh"
              onClick={() => refresh()}
              disabled={refreshing}
              aria-label={t("health.refresh")}
            >
              {refreshing ? "…" : "↻"}
            </button>
            {onOpenSettings && (
              <button className="ready-card-link" onClick={onOpenSettings}>
                {t("health.settings")}
              </button>
            )}
            {onOpenPower && (
              <button className="ready-card-link" onClick={onOpenPower}>
                {t("health.power")}
              </button>
            )}
          </div>
        </>
      )}
    </div>
  );
}
