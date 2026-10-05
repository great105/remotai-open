import { useEffect, useState, type CSSProperties } from "react";
import { useNavigate } from "react-router-dom";
import { applySetting, getSettingsCatalog, setAutostart } from "../api";
import { t } from "../i18n";
import { haptic, tgConfirm } from "../telegram";
import { mapApiError, useToast, ownedText } from "@tgcontrol/shared";
import {
  placeCatalog,
  settingValueText,
  settingWireValue,
  type SettingPlacement,
  type SettingSpec,
} from "../settings/catalog";

/**
 * «Настройки компьютера» — весь список того, что вообще можно настроить.
 *
 * ЗАЧЕМ. Жалоба владельца 09.08.2026: «много разных функций и не всё понятно
 * где». Каталог настроек с описаниями и пометкой риска у нас был давно — но
 * его читал ТОЛЬКО AI-агент: клиент к `/api/settings/catalog` не обращался
 * вовсе. Настройки, у которых не оказалось своего экрана, человек не видел
 * никак: сторож зависшего VPN и доступ с других устройств аккаунта работали на
 * компьютере, агент про них рассказывал, а в приложении их не существовало.
 *
 * Раскладка «здесь / там / только показ» — правило вне React
 * (`apk/src/settings/catalog.ts`), проверяемое тестами без DOM. Здесь только
 * отрисовка.
 *
 * ГРАНИЦА ВЛАСТИ соблюдена: `POST /api/settings/apply` меняет лишь безопасное,
 * поэтому опасные настройки без своего экрана показываются значением и
 * подписью «меняется с компьютера», а не переключателем, который ответит
 * отказом.
 */
/**
 * Обёртка тумблера. Сам тумблер нарисован полоской 44×24 — для пальца это
 * половина цели, поэтому кнопка вокруг него держит 44×44, а полоска остаётся
 * прежнего размера внутри. Сброс инлайном, потому что класс `.settings-toggle`
 * рисуется на span в других местах и тега button не ждёт: браузерная рамка и
 * отступ сдвинули бы кружок, который позиционируется от края полоски.
 */
const TOGGLE_BUTTON: CSSProperties = {
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  minWidth: 44,
  minHeight: 44,
  padding: 0,
  border: 0,
  background: "none",
  cursor: "pointer",
};

export function ComputerSettings({ onPickFolder, refreshToken = 0 }: {
  /** Выбор папки проводником — он живёт на экране настроек (модалка со своим
   *  состоянием). Каталог только зовёт его: набирать длинный путь пальцем —
   *  источник опечаток, которые сервер принимал молча (N15). */
  onPickFolder?: (key: string) => void;
  /** Меняется, когда путь выбрали снаружи, — повод перечитать значения. */
  refreshToken?: number;
} = {}) {
  const navigate = useNavigate();
  const { toast, toastError } = useToast();
  const [items, setItems] = useState<SettingPlacement[] | null>(null);
  const [loadError, setLoadError] = useState(false);
  const [retry, setRetry] = useState(0);
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState("");
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  // Раскрытая настройка — ровно одна.
  //
  // Просьба владельца 09.08.2026: «есть простые настройки, есть сложные —
  // нажимаешь на сторож VPN, и там его настройки с пояснениями». В свёрнутом
  // виде строка отвечает «что это и как стоит сейчас», в раскрытом — «как оно
  // работает» и сам переключатель. Иначе список из одиннадцати настроек с
  // объяснениями к каждой — это снова простыня, на которую он и жаловался.
  const [openKey, setOpenKey] = useState("");

  useEffect(() => {
    let cancelled = false;
    setLoadError(false);
    setItems(null);
    void refreshToken; // перечитываем после выбора папки снаружи
    getSettingsCatalog()
      .then((res) => {
        if (cancelled) return;
        setItems(placeCatalog((res.settings || []) as SettingSpec[]));
      })
      .catch(() => { if (!cancelled) setLoadError(true); });
    return () => { cancelled = true; };
  }, [refreshToken, retry]);

  if (loadError) return <div className="setting-group" role="status">
    <p className="settings-catalog-empty">{t("settings.catalog.loadFailed")}</p>
    <button className="btn btn-secondary" onClick={() => setRetry(value => value + 1)}>{t("settings.configRetry")}</button>
    <button className="settings-link-row" onClick={() => navigate("/settings?section=connection")}>{t("settings.nav.connection")} <span aria-hidden="true">›</span></button>
  </div>;
  if (!items) return <div className="setting-group" role="status"><p className="settings-catalog-empty">{t("settings.catalog.loading")}</p></div>;
  if (items.length === 0) return <div className="setting-group"><p className="settings-catalog-empty">{t("settings.catalog.unsupported")}</p>
    <button className="btn btn-secondary" onClick={() => navigate("/settings?section=connection")}>{t("settings.nav.connection")}</button>
  </div>;

  // Search the server's descriptions as well as titles, so newly added
  // settings remain findable without a separate client-side list of keys.
  const wordsToFind = query.trim().toLocaleLowerCase().replace(/ё/g, "е").split(/\s+/).filter(Boolean);
  const visibleItems = items.map(item => ({ ...item, spec: {
    ...item.spec, title: ownedText(item.spec.title), hint: ownedText(item.spec.hint),
    details: item.spec.details?.map(ownedText),
  } })).filter(({ spec }) => {
    const text = [spec.title, spec.hint, ...(spec.details || [])].join(" ").toLocaleLowerCase().replace(/ё/g, "е");
    return wordsToFind.every(word => text.includes(word));
  });

  const words = {
    on: t("settings.catalog.on"),
    off: t("settings.catalog.off"),
    unset: t("settings.catalog.unset"),
  };

  const save = async (spec: SettingSpec, next: unknown) => {
    setBusy(spec.key);
    try {
      // Автозапуск меняется СВОЕЙ ручкой: `apply` отвечает отказом на всё
      // опасное (это граница для AI-агента), а человеку автозапуск переключать
      // можно и нужно — раньше ради этого его отправляли на другой экран.
      if (spec.key === "autostart") {
        // Второй орган ведёт себя как первый (аудит ИА 02.09.2026, P1-22 / V7).
        // Автозапуск переключается в ДВУХ местах: здесь и на «Системе». Оба
        // остаются — на «Систему» ведёт диагностика «служба не видит экран», и
        // там тумблер стоит рядом с объяснением. Но предупреждение обязано быть
        // одним: выключение — необратимое с телефона действие (после ближайшей
        // перезагрузки компьютер пропадёт из приложения, включить обратно
        // можно только сидя за машиной), а этот тумблер выключал молча.
        // Слова и кнопки — те же, что в SystemView.handleAutostart.
        const enable = !!next;
        const label = enable
          ? t("confirm.enableAutostart")
          : [t("confirm.disableAutostart"), t("confirm.disableAutostartConsequence")].join("\n\n");
        if (!(await tgConfirm(label, {
          danger: !enable,
          confirmText: enable ? t("confirm.btn.enable") : t("confirm.btn.disable"),
        }))) return;
        const st = await setAutostart(enable);
        setItems((list) => (list || []).map((item) => (
          item.spec.key === spec.key ? { ...item, spec: { ...item.spec, value: !!st.enabled } } : item
        )));
        toast(t("settings.catalog.saved"));
        return;
      }
      const res = await applySetting(spec.key, settingWireValue(spec, next));
      setItems((list) => (list || []).map((item) => (
        item.spec.key === spec.key ? { ...item, spec: { ...item.spec, value: res.value } } : item
      )));
      toast(t("settings.catalog.saved"));
    } catch (e: any) {
      // Единый безопасный перевод API-ошибок: не показываем сырые технические
      // сообщения и секретные детали ответа в toast.
      toastError(mapApiError(e));
    } finally {
      setBusy("");
    }
  };

  const renderControl = (spec: SettingSpec) => {
    if (spec.type === "bool") {
      const on = !!spec.value;
      // Тумблер был двумя пустыми span'ами: ни нажать с клавиатуры, ни узнать
      // от диктора, что это переключатель и в каком он положении. Настоящая
      // кнопка с role="switch" отвечает на оба вопроса; имя берём из заголовка
      // настройки — сама полоска слов не содержит.
      //
      // stopPropagation НА KEYDOWN обязателен: по Enter/Пробелу кнопка сама
      // шлёт click (его гасит stopPropagation в onClick), но keydown всплыл бы
      // до обработчика строки и заодно раскрыл подробности — тумблер щёлкал бы
      // и дёргал экран одним нажатием.
      return (
        <button
          type="button"
          role="switch"
          aria-checked={on}
          aria-label={spec.title}
          disabled={busy === spec.key}
          style={TOGGLE_BUTTON}
          onClick={(e) => { e.stopPropagation(); haptic(); void save(spec, !spec.value); }}
          onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") e.stopPropagation(); }}
        >
          <span className={`settings-toggle ${on ? "on" : ""}`} aria-hidden>
            <span className="settings-toggle-knob" />
          </span>
        </button>
      );
    }
    if (spec.options && spec.options.length > 0) {
      return (
        <select
          className="settings-catalog-select"
          value={String(spec.value ?? "")}
          disabled={busy === spec.key}
          onChange={(e) => { haptic(); void save(spec, e.target.value); }}
          aria-label={spec.title}
        >
          {spec.options.map((option) => <option key={option} value={option}>{option}</option>)}
        </select>
      );
    }
    const draft = drafts[spec.key] ?? String(spec.value ?? "");
    const changed = draft !== String(spec.value ?? "");
    // Папку ВЫБИРАЮТ, а не вписывают: «нажимаешь на неё и выбираешь папку, а не
    // руками что-то вписываешь» (владелец, 09.08.2026). Набор длинного пути
    // пальцем — источник опечаток, которые сервер принимал молча, а файлы потом
    // уезжали в чужую папку (N15). Поле остаётся для правки, но главное
    // действие — кнопка выбора.
    if (spec.type === "path" && onPickFolder) {
      return (
        <button
          className="btn btn-secondary settings-catalog-pick"
          onClick={(e) => { e.stopPropagation(); haptic(); onPickFolder(spec.key); }}
        >
          {t("settings.pickFolder")}
        </button>
      );
    }
    return (
      <span className="settings-catalog-edit">
        <input
          value={draft}
          inputMode={spec.type === "int" ? "numeric" : undefined}
          disabled={busy === spec.key}
          onChange={(e) => setDrafts((d) => ({ ...d, [spec.key]: e.target.value }))}
          onKeyDown={(e) => { if (e.key === "Enter" && changed) { e.preventDefault(); void save(spec, draft); } }}
          aria-label={spec.title}
        />
        {/* Кнопка появляется только когда есть что сохранять: иначе она стоит
            рядом с каждым полем и выглядит как незаконченное действие. */}
        {changed && (
          <button className="btn btn-primary" disabled={busy === spec.key} onClick={() => { haptic(); void save(spec, draft); }}>
            {t("generic.save")}
          </button>
        )}
      </span>
    );
  };

  return (
    <div className="setting-group">
      <h2 className="setting-label">{t("settings.catalog.title")}</h2>
      <div className="settings-catalog-hint">{t("settings.catalog.hint")}</div>
      <input type="search" className="settings-catalog-search" value={query} onChange={e => setQuery(e.target.value)}
        aria-label={t("settings.catalog.search")} placeholder={t("settings.catalog.search")} />
      {visibleItems.length === 0 && <p className="settings-catalog-empty" role="status">{t("settings.catalog.empty")}</p>}
      <div className="settings-info-card">
        {visibleItems.map((item) => {
          const { spec } = item;
          if (item.kind === "elsewhere") {
            // Дверь рисуется ТОЙ ЖЕ строкой, что и остальные: `settings-link-row`
            // рассчитан на другой контейнер, и внутри карточки его текст
            // слипался в одну строку без отступов (видно на первом же снимке).
            return (
              <button
                key={spec.key}
                className="settings-catalog-row settings-catalog-door"
                onClick={() => { haptic(); navigate(item.home.route); }}
              >
                <span className="settings-catalog-text">
                  <b>{spec.title}</b>
                  <small>{t("settings.catalog.livesIn", { place: t(item.home.placeKey) })}</small>
                </span>
                <span className="settings-catalog-value">{settingValueText(spec, words)}</span>
                <span className="settings-catalog-chevron" aria-hidden>{"›"}</span>
              </button>
            );
          }
          const open = openKey === spec.key;
          // Строка ОТКРЫВАЕТ подробности, а не переключает: у тумблера цена
          // случайного касания — изменённая настройка компьютера, и «хотел
          // почитать, а выключил сторожа» здесь недопустимо. Переключатель
          // нажимается сам по себе.
          const toggleOpen = () => { haptic(); setOpenKey(open ? "" : spec.key); };
          const explain = [spec.hint, ...(spec.details || [])].filter(Boolean);
          return (
            <div key={spec.key} className={`settings-catalog-item${open ? " open" : ""}`}>
              <div
                className="settings-catalog-row"
                role="button"
                tabIndex={0}
                aria-expanded={open}
                onClick={toggleOpen}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggleOpen(); }
                }}
              >
                <div className="settings-catalog-text">
                  <b>{spec.title}</b>
                  {/* В свёрнутом виде — ТОЛЬКО значение: одиннадцать пояснений
                      подряд и есть та простыня, на которую жаловался владелец.
                      Но если значение уже видно в самом поле или в списке
                      выбора, второй раз его не пишем — «3» под полем с «3»
                      выглядит как ошибка. */}
                  {(spec.type === "bool" || item.kind === "readonly") && (
                    <small>{settingValueText(spec, words)}</small>
                  )}
                </div>
                {/* Управление стоит в строке и работает БЕЗ раскрытия: простое
                    должно оставаться простым. Клик по нему не открывает
                    подробности — иначе каждый щелчок тумблера дёргал бы экран. */}
                {item.kind !== "readonly" && (
                  <span
                    className="settings-catalog-control"
                    // Обёртка только не пускает клик наверх: через неё проходят
                    // ещё список выбора и текстовое поле, и без этого нажатие
                    // по ним раскрывало бы строку. Само переключение живёт в
                    // кнопке тумблера — иначе логика раздваивается на два места
                    // и одно из них однажды забудут поправить.
                    onClick={(e) => e.stopPropagation()}
                  >
                    {renderControl(spec)}
                  </span>
                )}
                <span className={`settings-catalog-chevron${open ? " open" : ""}`} aria-hidden>{"›"}</span>
              </div>
              {open && (
                <div className="settings-catalog-details">
                  {explain.map((line, i) => <p key={i}>{line}</p>)}
                  {item.kind === "readonly" && (
                    <>
                      <div className="settings-catalog-owner">{t("settings.catalog.ownerOnly")}</div>
                      <div className="settings-catalog-current">
                        {t("settings.catalog.now", { value: settingValueText(spec, words) })}
                      </div>
                    </>
                  )}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
