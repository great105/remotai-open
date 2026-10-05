import { useCallback, useEffect, useState } from "react";
import { mapApiError } from "@tgcontrol/shared";
import {
  getOpenRouter, setOpenRouterKey, forgetOpenRouterKey, getOpenRouterModels, setOpenRouterModel,
} from "../api";
import type { OpenRouterStatus, OpenRouterModel } from "../types";
import { haptic } from "../telegram";
import { openExternalLink } from "../openExternal";
import { t } from "../i18n";

/** Куда идти за ключом. Открывается штатной дверью наружу (openExternal.ts). */
const OPENROUTER_KEYS_URL = "https://openrouter.ai/keys";

/**
 * OpenRouter — один ключ вместо подписки на каждого вендора.
 *
 * ЗАЧЕМ ЭТО В ПРОДУКТЕ. Чтобы Remotai что-то значил, у человека уже должен быть
 * работающий CLI-агент с оплаченной подпиской: иначе он ставит приложение и
 * видит терминал и файлы, а вся история «агент работает, пока тебя нет» для него
 * не начинается. Ключ OpenRouter снимает это условие.
 *
 * ТРИ ПРАВИЛА ЭТОГО ЭКРАНА — каждое стоит того, чтобы его не «упростили»:
 *
 *  1. ГОВОРИМ «ПОТРАЧЕНО», А НЕ «БАЛАНС». Проверено живым ключом: баланс счёта
 *     обычным ключом вывода недоступен (`/api/v1/credits` → 403). Доступен
 *     расход и остаток ЛИМИТА КЛЮЧА, если человек его задал. Рисовать «баланс»
 *     из того, чего нам не отдают, — врать на главном экране про деньги.
 *  2. НАЗЫВАЕМ ДНЕВНУЮ КВОТУ ЧИСЛОМ. Пока человек ни разу не пополнял счёт, у
 *     бесплатных моделей 50 запросов в сутки — это МЕНЬШЕ ОДНОЙ агентной
 *     задачи. Промолчать здесь значит отпустить его упираться в стену посреди
 *     работы; вместо этого прямо предлагаем разовое пополнение, после которого
 *     квота становится 1000.
 *  3. БЕСПЛАТНАЯ МОДЕЛЬ БЕЗ ИНСТРУМЕНТОВ АГЕНТУ БЕСПОЛЕЗНА. Он не прочитает
 *     файл и не запустит команду. Такие модели показываем отдельно и помеченными,
 *     а не вперемешку с рабочими.
 *
 * Ключ на экран не приезжает НИКОГДА: он живёт в окружении процесса агента на
 * компьютере, а сюда приходит только маскированная метка от самого OpenRouter.
 */

export interface OpenRouterState {
  status: OpenRouterStatus | null;
  models: OpenRouterModel[];
  busy: boolean;
  error: string;
  reload: () => void;
  saveKey: (key: string) => Promise<boolean>;
  forget: () => Promise<void>;
  pickModel: (id: string) => Promise<void>;
  loadModels: () => Promise<void>;
  clearError: () => void;
}

export function useOpenRouter(enabled = true): OpenRouterState {
  const [status, setStatus] = useState<OpenRouterStatus | null>(null);
  const [models, setModels] = useState<OpenRouterModel[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const reload = useCallback(() => {
    if (!enabled) return;
    // Отказ молчаливый: старый агент про этот маршрут не знает вовсе, и это
    // нормальное состояние, а не ошибка (то же правило, что с аккаунтами).
    getOpenRouter().then(setStatus).catch(() => setStatus(null));
  }, [enabled]);

  useEffect(() => { reload(); }, [reload]);

  const loadModels = useCallback(async () => {
    setBusy(true);
    try {
      const d = await getOpenRouterModels();
      setModels(d.models || []);
    } catch (e) {
      setError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  }, []);

  const saveKey = async (key: string): Promise<boolean> => {
    setError("");
    setBusy(true);
    try {
      await setOpenRouterKey(key);
      const fresh = await getOpenRouter();
      setStatus(fresh);
      haptic("light");
      return true;
    } catch (e) {
      setError(mapApiError(e));
      return false;
    } finally {
      setBusy(false);
    }
  };

  const forget = async () => {
    setBusy(true);
    try {
      await forgetOpenRouterKey();
      setModels([]);
      setStatus(await getOpenRouter());
    } catch (e) {
      setError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  const pickModel = async (id: string) => {
    setBusy(true);
    try {
      await setOpenRouterModel(id);
      setStatus(await getOpenRouter());
      haptic("light");
    } catch (e) {
      setError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  return {
    status, models, busy, error, reload, saveKey, forget, pickModel, loadModels,
    clearError: () => setError(""),
  };
}

/** Деньги печатаем так, как их читают: $0.31, а не $0.3100000001. */
function money(v?: number): string {
  const n = typeof v === "number" ? v : 0;
  return n < 0.01 && n > 0 ? "<$0.01" : `$${n.toFixed(2)}`;
}

function priceLabel(m: OpenRouterModel): string {
  // Роутер — не модель, а автоподбор: цена и контекст про него ничего не
  // говорят, поэтому строка у него своя.
  if (m.router) return t("openrouter.model.router");
  if (m.free) return t("openrouter.model.free");
  return t("openrouter.model.price", {
    in: m.prompt_price.toFixed(2),
    out: m.completion_price.toFixed(2),
  });
}

function contextLabel(m: OpenRouterModel): string {
  const k = Math.round(m.context / 1000);
  return k >= 1000 ? `${Math.round(k / 1000)}M` : `${k}K`;
}

export function OpenRouterCard({ state }: { state: OpenRouterState }) {
  const { status, models, busy, error } = state;
  const [keyInput, setKeyInput] = useState("");
  const [modelsShown, setModelsShown] = useState(false);
  const [onlyFree, setOnlyFree] = useState(true);
  const connected = Boolean(status?.configured);

  const openModels = async () => {
    const next = !modelsShown;
    setModelsShown(next);
    if (next && models.length === 0) await state.loadModels();
  };

  /**
   * Поле для ключа — одно и то же и до подключения, и после отказа.
   *
   * Аудит онбординга 30.08.2026: при `key_state === "rejected"` совет
   * «подключите новый» стоял на экране, где поля ввода нет вовсе — оно
   * рисовалось только в состоянии «не подключено». Надо было догадаться
   * сначала нажать «Отключить ключ». Совет и средство теперь в одном месте.
   */
  const keyForm = (
    <div className="or-key-row">
      <input
        className="or-key-input"
        type="password"
        value={keyInput}
        placeholder="sk-or-v1-…"
        autoComplete="off"
        onChange={(e) => setKeyInput(e.target.value)}
      />
      <button
        className="btn primary"
        disabled={busy || !keyInput.trim()}
        onClick={async () => { if (await state.saveKey(keyInput.trim())) setKeyInput(""); }}
      >
        {busy ? t("common.wait") : t("openrouter.connect")}
      </button>
    </div>
  );

  // Ключ сохранён, но зашифрован другой учётной записью Windows. Сказать это
  // словами — не то же самое, что показать пустое поле: ключ не пропал.
  if (status?.foreign) {
    return (
      <div className="or-card">
        <div className="or-head"><span className="or-logo">🔀</span><b>OpenRouter</b></div>
        <p className="or-note">{t("openrouter.foreign")}</p>
      </div>
    );
  }

  return (
    <div className="or-card">
      <div className="or-head">
        <span className="or-logo">🔀</span>
        <b>OpenRouter</b>
        {connected && status?.label && <span className="or-label">{status.label}</span>}
      </div>

      {!connected && (
        <>
          <p className="or-note">{t("openrouter.pitch")}</p>
          {keyForm}
          {/* ЦЕНА НАЗВАНА ДО ПОДКЛЮЧЕНИЯ, а не после. Раньше «50 запросов в
              сутки» человек читал только с уже подключённым ключом, а до этого
              ему обещали «в том числе бесплатные» без единой оговорки — то
              есть решение он принимал, не зная главного числа. */}
          <p className="or-quota tight">{t("openrouter.freeBeforeConnect")}</p>
          <p className="or-hint">
            {t("openrouter.whereKeyPrefix")}{" "}
            <button type="button" className="or-link" onClick={() => { haptic(); void openExternalLink(OPENROUTER_KEYS_URL); }}>
              openrouter.ai/keys
            </button>
            {" "}{t("openrouter.whereKeySuffix")}
          </p>
        </>
      )}

      {connected && (
        <>
          {/* Состояние ключа: принят / отвергнут / сервис молчит. Это разные
              новости — первое чинит человек, второе проходит само. */}
          {status?.key_state === "rejected" && (
            <>
              <p className="or-error">{t("openrouter.rejected")}</p>
              {keyForm}
            </>
          )}
          {status?.key_state === "unreachable" && <p className="or-note">{t("openrouter.unreachable")}</p>}

          {status?.key_state === "ok" && (
            <>
              <div className="or-stats">
                <div className="or-stat">
                  <span className="or-stat-v">{money(status.usage_daily)}</span>
                  <span className="or-stat-k">{t("openrouter.today")}</span>
                </div>
                <div className="or-stat">
                  <span className="or-stat-v">{money(status.usage_monthly)}</span>
                  <span className="or-stat-k">{t("openrouter.month")}</span>
                </div>
                {typeof status.limit_remaining === "number" && (
                  <div className="or-stat">
                    <span className="or-stat-v">{money(status.limit_remaining)}</span>
                    <span className="or-stat-k">{t("openrouter.leftOnKey")}</span>
                  </div>
                )}
              </div>

              {/* ЧИСЛО, КОТОРОЕ РЕШАЕТ ВСЁ. 50 запросов в сутки — меньше одной
                  агентной задачи, поэтому рядом стоит выход, а не сожаление. */}
              <p className={`or-quota${status.is_free_tier ? " tight" : ""}`}>
                {t("openrouter.freeQuota", {
                  n: String(status.free_daily_quota ?? 0),
                  rpm: String(status.free_rpm ?? 20),
                })}
              </p>
              {status.is_free_tier && <p className="or-hint">{t("openrouter.topUpHint")}</p>}
            </>
          )}

          <div className="or-model-row">
            <span className="or-model-k">{t("openrouter.model")}</span>
            <b className="or-model-v">
              {/* Строку «openrouter/free» человек не обязан расшифровывать:
                  называем словами то, что она делает. */}
              {status?.model === "openrouter/free"
                ? t("openrouter.model.routerName")
                : status?.model || t("openrouter.modelDefault")}
            </b>
            <button className="btn small" onClick={openModels} disabled={busy}>
              {modelsShown ? t("common.hide") : t("openrouter.change")}
            </button>
          </div>

          {modelsShown && (
            <div className="or-models">
              <label className="or-only-free">
                <input type="checkbox" checked={onlyFree} onChange={(e) => setOnlyFree(e.target.checked)} />
                {t("openrouter.onlyFree")}
              </label>
              {busy && models.length === 0 && <p className="or-note">{t("common.wait")}</p>}
              {models
                .filter((m) => (onlyFree ? m.free : true))
                .slice(0, 60)
                .map((m) => {
                  const picked = status?.model === `openrouter/${m.id}`;
                  return (
                    <button
                      key={m.id}
                      className={`or-model-item${picked ? " picked" : ""}${!m.tools ? " no-tools" : ""}${m.router ? " router" : ""}`}
                      disabled={busy}
                      onClick={() => state.pickModel(`openrouter/${m.id}`)}
                    >
                      <span className="or-model-name">
                        {m.router ? t("openrouter.model.routerName") : m.name}
                      </span>
                      <span className="or-model-meta">
                        {/* У роутера нет ни своего качества, ни своего окна —
                            он про «просто работай», поэтому вместо цены и
                            контекста объясняем, что он делает. */}
                        {m.router ? priceLabel(m) : `${priceLabel(m)} · ${contextLabel(m)}`}
                        {/* Без инструментов агент не прочитает файл и не
                            запустит команду — это не мелочь, это отказ. */}
                        {!m.tools && ` · ${t("openrouter.noTools")}`}
                      </span>
                    </button>
                  );
                })}
            </div>
          )}

          {/* Терминалы, открытые до подключения ключа, его не получат: они
              родились раньше и унаследовали прежнее окружение. Молчать об этом
              нельзя — человек решит, что ключ не работает. */}
          <p className="or-hint">{t("openrouter.newTerminalsOnly")}</p>

          <button className="btn small danger-text" disabled={busy} onClick={() => state.forget()}>
            {t("openrouter.forget")}
          </button>
        </>
      )}

      {error && (
        <p className="or-error" onClick={state.clearError}>{error}</p>
      )}
    </div>
  );
}
