/**
 * QR-код, нарисованный на месте, без обращения к сети.
 *
 * Сканер у клиента был давно (jsqr), а показать свой QR он не умел вовсе:
 * рисует его только приложение на ПК (`/api/setup/qr`), у релея такой ручки
 * нет. Из-за этого на большом экране в браузере оставалась единственная
 * бессмысленная кнопка «Сканировать QR» — сканировать там нечего и нечем.
 *
 * Рисуем SVG, а не canvas: он масштабируется без мыла и печатается, а на
 * тёмной теме нужен светлый холст под кодом — камера иначе не читает.
 */
import qrcode from "qrcode-generator";

interface QrCodeProps {
  /** Что зашиваем в код. */
  value: string;
  /** Сторона картинки в пикселях. */
  size?: number;
  /** Подпись для скринридера. */
  title?: string;
}

export function QrCode({ value, size = 220, title = "QR-код" }: QrCodeProps) {
  // 0 — «подбери версию сам» по длине данных; M — уровень коррекции, при
  // котором код читается с экрана даже под углом и бликом.
  const qr = qrcode(0, "M");
  qr.addData(value);
  qr.make();

  const count = qr.getModuleCount();
  const quiet = 4; // тихая зона обязательна: без неё камера не находит код
  const total = count + quiet * 2;

  const cells: string[] = [];
  for (let row = 0; row < count; row++) {
    for (let col = 0; col < count; col++) {
      if (qr.isDark(row, col)) {
        cells.push(`M${col + quiet} ${row + quiet}h1v1h-1z`);
      }
    }
  }

  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${total} ${total}`}
      role="img"
      aria-label={title}
      style={{ display: "block", borderRadius: 12, background: "#fff" }}
      shapeRendering="crispEdges"
    >
      <path d={cells.join("")} fill="#000" />
    </svg>
  );
}
