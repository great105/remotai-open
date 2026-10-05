import type { IBuffer, IBufferLine } from "@xterm/xterm";

/**
 * Текст одной строки буфера как части ЛОГИЧЕСКОЙ строки (T-15, ST-07).
 *
 * Строка без продолжения обрезается справа, как раньше. У строки, за которой
 * идёт мягкий перенос, справа снимаются только НЕЗАПИСАННЫЕ ячейки (пустой
 * getChars и ширина 1). Такую ячейку xterm оставляет в последней колонке,
 * когда широкий символ туда не влез и уехал на следующую строку: раньше она
 * копировалась как пробел внутри команды — при cols=5 «abcd中ef» становилось
 * «abcd 中ef» (воспроизведено на @xterm/headless 6). Набранные пробелы у
 * переноса записаны (getChars() === " ") и сохраняются: для кода и путей они
 * значимы. Продолжение широкого символа имеет ширину 0 и остановит обрезку.
 */
export function logicalRowText(line: Pick<IBufferLine, "getCell" | "translateToString" | "length">,
  nextWrapped: boolean, cols: number): string {
  if (!nextWrapped) return line.translateToString(true);
  let end = Math.max(0, Math.min(Math.floor(cols), line.length));
  while (end > 0) {
    const cell = line.getCell(end - 1);
    if (!cell || cell.getChars() !== "" || cell.getWidth() !== 1) break;
    end--;
  }
  return line.translateToString(false, 0, end);
}

/** Join logical lines, preserving code indentation and spaces at a soft wrap. */
export function bufferText(buffer: Pick<IBuffer, "getLine" | "length">, start: number, end: number, visual = false): string {
  let text = "";
  const first = Math.max(0, start), last = Math.min(buffer.length - 1, end);
  for (let row = first; row <= last; row++) {
    const line = buffer.getLine(row);
    if (!line) continue;
    if (row > first && (visual || !line.isWrapped)) text += "\n";
    const continued = row < last && !!buffer.getLine(row + 1)?.isWrapped;
    // Визуальный режим («Копировать экран» построчно) переносы не склеивает.
    text += visual ? line.translateToString(true) : logicalRowText(line, continued, line.length);
  }
  return text;
}
