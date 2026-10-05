/**
 * Юнит-тесты правил клиента.
 *
 * Тестируем не «компоненты», а ПРАВИЛА — чистые функции, в которых живёт смысл
 * продукта и которые переписывались по нескольку раз за релиз: как зовут машину,
 * что значит «агент отстал», когда парк считается простым, что человек читает
 * вместо кода ошибки. До этого фронт (43k строк) не имел ни одного теста, и
 * каждая такая правка проверялась живым прогоном с телефона.
 *
 * Окружение — node: правила от DOM не зависят и не должны.
 */
import { defineConfig } from "vitest/config";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  resolve: {
    alias: {
      "@tgcontrol/shared": path.resolve(root, "packages/shared/src/index.ts"),
      // Те же алиасы, что в apk/vite.config.ts: @xterm лежит в apk/node_modules
      // (npm не хоистит его в корень), а импортируется из packages/shared.
      "@xterm/xterm": path.resolve(root, "apk/node_modules/@xterm/xterm"),
      "@xterm/addon-search": path.resolve(root, "apk/node_modules/@xterm/addon-search"),
    },
  },
  test: {
    environment: "node",
    include: ["apk/src/**/*.test.ts", "packages/shared/src/**/*.test.ts"],
    reporters: ["default"],
  },
});
