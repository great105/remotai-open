import { useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { t } from "@tgcontrol/shared";
import { onConnectionChange } from "../api";
import {
  getMode,
  getSelectedDeviceId,
  getSelectedDeviceName,
  getSelectedDevicePlatform,
  getSelectedDeviceType,
  getSelectedWorkspaceName,
} from "../config";
import { humanDeviceName } from "../devices";
import { useCapabilities } from "../hooks/useCapabilities";
import { haptic } from "../telegram";

/**
 * Марки платформ и типа устройства — ЕДИНСТВЕННЫЙ источник на весь продукт.
 *
 * Зачем экспорт: одно и то же понятие «на чём работает эта машина» рисовалось
 * двумя разными языками — здесь эмодзи (🪟 🐧 🍎), а в «Моих компьютерах»
 * геометрией (▣ ◫ ◇). Оба знака видны ОДНОВРЕМЕННО: чип висит поверх того же
 * экрана, где лежат карточки машин, и человек видел два обозначения одной
 * Windows рядом. Решение владельца: марки платформ остаются эмодзи (логотип
 * ОС линией не нарисовать, а перерисовывать чужие товарные знаки нельзя), но
 * ОДИНАКОВЫМИ везде — поэтому карта живёт в одном месте и импортируется.
 *
 * Тип устройства («сервер») платформой НЕ является: у сервера своя марка, она
 * отвечает на другой вопрос — чем ты управляешь, а не под какой ОС.
 *
 * ⚠ Вторая карта пока жива: `PLATFORM_ICON` в pages/InfrastructureView.tsx
 * (▣ ◫ ◇ и запасное «▣»). Её место — здесь: карточки машин обязаны звать
 * `deviceMark()`, иначе на одном экране снова окажутся два обозначения одной
 * Windows. Новых карт платформ в продукте не заводить.
 */
export const PLATFORM_MARK: Record<string, string> = {
  windows: "🪟",
  linux: "🐧",
  darwin: "🍎",
};

/** Платформа неизвестна (старый агент, чужое устройство) — просто «компьютер». */
export const PLATFORM_MARK_UNKNOWN = "💻";

/** Сервер: стойка, а не ОС. Тот же знак и в списке машин. */
export const DEVICE_MARK_SERVER = "▰";

/** Марка машины для чипа и для карточек «Моих компьютеров» — одна на оба места. */
export function deviceMark(platform?: string | null, deviceType?: string | null): string {
  if (deviceType === "server") return DEVICE_MARK_SERVER;
  return PLATFORM_MARK[(platform || "").toLowerCase()] || PLATFORM_MARK_UNKNOWN;
}

/**
 * Постоянный указатель контекста: на любом рабочем экране видно, какой именно
 * машиной управляет пользователь. Тап ведёт в «Мои компьютеры» — туда, где
 * машины переключают и добавляют.
 *
 * Дверь открыта в ЛЮБОМ режиме. Раньше в self_hosted чип был мёртвым текстом:
 * список машин считался облачной привилегией. Но с 2.38.0 «Мои компьютеры»
 * работают и без облака (этот компьютер, кто к нему подключён, дверь в
 * SSH-серверы), а добавляют вторую машину именно там — значит человек,
 * подключившийся к своему ПК по QR, упирался в тупик ровно в том месте, где
 * начинается второй компьютер.
 */
export function DeviceChip() {
  const navigate = useNavigate();
  const location = useLocation();
  /**
   * Куда вернуться после смены машины. Машину меняют, чтобы продолжить ДЕЛО:
   * без этого человек оставался в списке, и возврат к работе стоил третьего
   * нажатия (аудит путей 29.08.2026). Список сам решает, безопасен ли адрес
   * (nextPath.ts), поэтому сюда кладём свой текущий путь как есть.
   */
  const backTo = `/infrastructure?back=${encodeURIComponent(location.pathname + location.search)}`;
  const [connected, setConnected] = useState(true);
  const capabilities = useCapabilities();
  const mode = getMode();
  const platform = getSelectedDevicePlatform() || capabilities.platform;
  const deviceType = getSelectedDeviceType();
  const workspaceName = getSelectedWorkspaceName();
  // В LAN используем имя подключённого ПК из общего кэша возможностей:
  // «Этот компьютер» на телефоне ошибочно обозначал устройство пользователя.
  const name = humanDeviceName(
    mode === "cloud" ? getSelectedDeviceName() : capabilities.hostname,
    t("devices.connectedComputer"),
  );

  useEffect(() => onConnectionChange((state) => setConnected(state.connected)), []);

  /**
   * Облако без выбранной машины (её запись удалили или доступ отозвали): чип не
   * должен изображать компьютер, которого нет, — вместо имени и точки статуса
   * предлагаем выбрать машину.
   */
  if (mode === "cloud" && !getSelectedDeviceId()) {
    return (
      <button
        type="button"
        className="device-chip"
        onClick={() => {
          haptic();
          navigate(backTo);
        }}
      >
        <span className="device-chip-platform" aria-hidden>{PLATFORM_MARK_UNKNOWN}</span>
        <span className="device-chip-name">{t("devices.chipChoose")}</span>
        <span className="device-chip-chevron" aria-hidden>›</span>
      </button>
    );
  }

  const content = (
    <>
      <span className="device-chip-platform" aria-hidden>{deviceMark(platform, deviceType)}</span>
      <span className="device-chip-name">{workspaceName ? `${workspaceName} · ${name}` : name}</span>
      <span className={`device-chip-dot ${connected ? "online" : "offline"}`} aria-hidden />
    </>
  );

  // Подпись — человеческим языком и словарным ключом: «Открыть инфраструктуру»
  // это жаргон, по которому невозможно понять, что тап покажет список машин.
  // На главной чипа больше нет вовсе (там машину называет карточка готовности,
  // а переключают плитки) — здесь он остаётся указателем «где я» на рабочих
  // экранах, и вести ему честно в список машин.
  return (
    <button
      type="button"
      className="device-chip"
      title={t("devices.chipOpen")}
      aria-label={t("devices.chipOpen")}
      onClick={() => {
        haptic();
        navigate(backTo);
      }}
    >
      {content}
      <span className="device-chip-chevron" aria-hidden>›</span>
    </button>
  );
}
