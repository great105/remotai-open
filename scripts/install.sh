#!/bin/sh
# Remotai headless installer.
#
#   curl -fsSL https://remotai.ru/install.sh | sh
#
# Скачивает бинарь (root → /usr/local/bin, иначе ~/.local/bin), а дальше всё
# делает сам бинарь: `remotai install` настраивает автозапуск (systemd
# system/user-unit, иначе печатает fallback-команды) и сразу запускает
# привязку `remotai pair` — код и ASCII-QR видны в этом же терминале
# (stdin pair'у не нужен, поэтому работает и через `curl | sh`).
#
# Переопределяемые переменные окружения:
#   REMOTAI_BASE  — базовый URL релиза (default https://remotai.ru/download)
#   REMOTAI_USER  — сервис-юзер system-юнита (default: вызвавший sudo, иначе root)
set -eu

# Keep shell output and the installed agent in the same selected language.
REMOTAI_INSTALL_LANGUAGE="${REMOTAI_LANGUAGE:-${LC_ALL:-${LC_MESSAGES:-${LANG:-en}}}}"
case "$REMOTAI_INSTALL_LANGUAGE" in
    ru|ru_*|ru-*|RU*|C|C.*|POSIX) REMOTAI_INSTALL_LANGUAGE=ru ;;
    *) REMOTAI_INSTALL_LANGUAGE=en ;;
esac
export REMOTAI_LANGUAGE="$REMOTAI_INSTALL_LANGUAGE"
remotai_message() {
    if [ "$REMOTAI_INSTALL_LANGUAGE" = en ]; then printf '%s\n' "$2";
    else printf '%s\n' "$1"; fi
}

REPO_BASE="${REMOTAI_BASE:-https://remotai.ru/download}"

# ── root или пользовательская установка ─────────────────────────────
# Root не обязателен: без него ставим в ~/.local/bin и настраиваем
# systemd user-unit (бинарь сам попросит sudo разве что для linger).
IS_ROOT=0
if [ "$(id -u)" -eq 0 ]; then
	IS_ROOT=1
	BIN_DIR="/usr/local/bin"
else
	BIN_DIR="$HOME/.local/bin"
fi
BIN_PATH="$BIN_DIR/remotai"

# ── архитектура ─────────────────────────────────────────────────────
arch="$(uname -m)"
case "$arch" in
	x86_64 | amd64) GOARCH="amd64" ;;
	aarch64 | arm64) GOARCH="arm64" ;;
	*)
		remotai_message "remotai: неподдерживаемая архитектура: $arch" "remotai: unsupported architecture: $arch" >&2
		exit 1
		;;
esac

# ── операционная система ────────────────────────────────────────────
# macOS ставится тем же скриптом: отличаются имя бинаря и способ автозапуска
# (LaunchAgent вместо systemd — им занимается сам бинарь в `remotai install`).
GOOS="linux"
case "$(uname -s)" in
	Darwin)
		GOOS="darwin"
		# Root на маке не нужен и вреден: агент работает в сессии человека,
		# иначе он не увидит его терминалы, ключи и Homebrew.
		if [ "$IS_ROOT" -eq 1 ]; then
			remotai_message "remotai: на macOS не запускайте установку под sudo." "remotai: do not run the macOS installer with sudo." >&2
			remotai_message "         Повторите без sudo: curl -fsSL https://remotai.ru/install.sh | sh" "         Run without sudo: curl -fsSL https://remotai.ru/install.sh | sh" >&2
			exit 1
		fi
		;;
	Linux) ;;
	*)
		remotai_message "remotai: неподдерживаемая система: $(uname -s)" "remotai: unsupported operating system: $(uname -s)" >&2
		exit 1
		;;
esac

# ── CA-сертификаты (частая болячка свежих контейнеров/VPS) ──────────
# Без ca-certificates curl не проверит TLS и скачивание упадёт позже с
# менее понятной ошибкой — проверяем заранее и подсказываем пакет.
if command -v curl >/dev/null 2>&1; then
	if ! curl -fsI https://remotai.ru >/dev/null 2>&1; then
		remotai_message "remotai: curl не смог проверить TLS-сертификат remotai.ru." "remotai: curl could not verify the TLS certificate of remotai.ru." >&2
		remotai_message "         Скорее всего, не установлены CA-сертификаты:" "         CA certificates may be missing:" >&2
		if command -v apt-get >/dev/null 2>&1; then
			echo "           apt install ca-certificates" >&2
		elif command -v apk >/dev/null 2>&1; then
			echo "           apk add ca-certificates" >&2
		elif command -v dnf >/dev/null 2>&1; then
			echo "           dnf install ca-certificates" >&2
		else
			remotai_message "           установите пакет ca-certificates вашего дистрибутива" "           install the ca-certificates package for your distribution" >&2
		fi
		exit 1
	fi
fi

# ── скачивание ──────────────────────────────────────────────────────
filename="remotai-$GOOS-$GOARCH"
url="$REPO_BASE/$filename"
tmp="$(mktemp)"
sums="$(mktemp)"
staged=""
trap 'rm -f "$tmp" "$sums"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT INT TERM

# ⚠ ${url}, а не $url: сразу за именем идёт многоточие «…», и /bin/sh на macOS
# (bash 3.2) затягивает его байты В ИМЯ ПЕРЕМЕННОЙ. С `set -eu` это мгновенный
# выход «url?: unbound variable» — установка на маке падала на первой же строке
# скачивания, ничего не скачав. Найдено на живом маке 09.08.2026, до этого
# darwin-ветку скрипта никто не запускал. Фигурные скобки задают границу имени
# явно; проверка `scripts/check-shell-vars.mjs` не даёт повториться.
remotai_message "→ Скачиваю remotai ($GOARCH) из ${url}…" "→ Downloading remotai ($GOARCH) from ${url}…"
if command -v curl >/dev/null 2>&1; then
	curl -fSL "$url" -o "$tmp"
	curl -fsSL "$REPO_BASE/SHA256SUMS" -o "$sums"
elif command -v wget >/dev/null 2>&1; then
	wget -O "$tmp" "$url"
	wget -O "$sums" "$REPO_BASE/SHA256SUMS"
else
	remotai_message "remotai: нужен curl или wget" "remotai: curl or wget is required" >&2
	exit 1
fi

# Never replace a working installation with an incomplete or mismatched file.
# The manifest is plain SHA256SUMS so fresh Macs need neither Python nor jq.
expected="$(awk -v name="$filename" '$2 == name { print $1 }' "$sums")"
case "$expected" in
	''|*[!0-9a-f]*) remotai_message 'remotai: нет корректной контрольной суммы для этой системы.' "remotai: no valid checksum is available for this operating system." >&2; exit 1 ;;
esac
if [ "${#expected}" -ne 64 ]; then
	remotai_message 'remotai: некорректная контрольная сумма.' "remotai: invalid checksum." >&2
	exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$tmp" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$tmp" | awk '{print $1}')"
else
	remotai_message 'remotai: для проверки загрузки нужен sha256sum или shasum.' "remotai: sha256sum or shasum is required to verify the download." >&2
	exit 1
fi
if [ "$actual" != "$expected" ]; then
	remotai_message 'remotai: загрузка не прошла проверку. Приложение не заменено; повторите установку.' "remotai: download verification failed. The application has been preserved; retry the installation." >&2
	exit 1
fi

# ── установка бинаря ────────────────────────────────────────────────
remotai_message "→ Устанавливаю в ${BIN_PATH}…" "→ Installing to ${BIN_PATH}…"
mkdir -p "$BIN_DIR"
# Same-filesystem rename preserves the running inode (Linux ETXTBSY) and
# exposes only a complete executable, including when updating a running agent.
staged="$(mktemp "$BIN_DIR/.remotai-install.XXXXXX")"
install -m 0755 "$tmp" "$staged"
mv -f "$staged" "$BIN_PATH"
staged=""

# PATH: пользовательский ~/.local/bin не всегда в PATH (добавится в новых
# сессиях через profile, в текущей — нет).
#
# Печатаем ГОТОВУЮ строку, а не описание «добавьте такую-то строчку в такой-то
# файл»: человек читает это в конце установки и должен уметь просто скопировать.
# Инструкцию, которую надо сначала понять и потом набрать руками, он выполнит с
# опечаткой — так уже было на маке 10.08.2026.
case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*)
		remotai_message "→ $BIN_DIR отсутствует в PATH. Выполните одной строкой:" "→ $BIN_DIR is missing from PATH. Run this command:" >&2
		# zsh (стандартный shell macOS) не читает ~/.profile. Оставляем HOME
		# переменной в готовой команде: пробелы и кавычки в имени не сломают её.
		profile='"$HOME/.profile"'
		case "${SHELL:-}" in
			*/zsh) profile='"${ZDOTDIR:-$HOME}/.zprofile"' ;;
		esac
		path_entry='$HOME/.local/bin'
		if [ "$IS_ROOT" -eq 1 ]; then path_entry='/usr/local/bin'; fi
		printf '%s\n' "      printf '%s\n' 'export PATH=\"$path_entry:\$PATH\"' >> $profile && export PATH=\"$path_entry:\$PATH\"" >&2
		;;
esac

# ── автозапуск + привязка (всё делает бинарь) ───────────────────────
# Сервис-юзер для system-режима пробрасываем явно: под `curl | sudo sh`
# SUDO_USER уже виден бинарю, REMOTAI_USER имеет приоритет (см. install.go).
# pair под сервис-юзером (`sudo -H -u`) бинарь выполняет сам — иначе
# device-JWT писался бы в дом root и «потерялся» для сервиса.
rc=0
if [ "$GOOS" = "darwin" ]; then
	# На маке способ один — LaunchAgent в сессии пользователя, флаги
	# --user/--system тут не нужны (см. install_darwin.go).
	"$BIN_PATH" install || rc=$?
elif [ "$IS_ROOT" -eq 1 ]; then
	SERVICE_USER="${REMOTAI_USER:-${SUDO_USER:-root}}"
	REMOTAI_USER="$SERVICE_USER" "$BIN_PATH" install --system || rc=$?
else
	"$BIN_PATH" install --user || rc=$?
fi

if [ "$rc" -ne 0 ]; then
	echo "" >&2
	remotai_message "remotai: install завершился с кодом $rc." "remotai: install exited with code $rc." >&2
	remotai_message "         Если прервали привязку — перевыполните: $BIN_PATH pair" "         If pairing was interrupted, run again: $BIN_PATH pair" >&2
	exit "$rc"
fi

if [ "$GOOS" = "darwin" ]; then
	# UID подставляем ЧИСЛОМ прямо сейчас: команду человек копирует из вывода
	# или набирает руками, и `$(id -u)` — приглашение к опечатке. Живой мак
	# 10.08.2026: из такой подсказки получилось `id-u` без пробела и два
	# «command not found» подряд.
	STATUS_CMD="launchctl print gui/$(id -u)/ru.remotai.agent | head -20"
	RESTART_CMD="launchctl kickstart -k gui/$(id -u)/ru.remotai.agent"
	LOGS_CMD="tail -f ~/Library/Logs/Remotai/remotai.err.log"
elif [ "$IS_ROOT" -eq 1 ]; then
	STATUS_CMD="systemctl status remotai"
	LOGS_CMD="journalctl -u remotai -f"
else
	STATUS_CMD="systemctl --user status remotai"
	LOGS_CMD="journalctl --user -u remotai -f"
fi

# ── проверка живучести user-сервиса (linger) ─────────────────────────
# Без linger systemd гасит менеджер пользователя вместе с последним сеансом —
# сервис умирает при выходе из SSH, и сервер навсегда «не в сети» в приложении.
# Бинарь включает linger через `sudo -n` и при отказе печатает [WARN] ПОСЕРЕДИНЕ
# вывода: он уезжает за экран, а в конце человек читает крупное «✅ установлен».
# Поэтому здесь проверяем ФАКТ и меняем итоговый блок.
# Возврат 0 = «поставлен user-сервис, linger выключен» (нужно предупредить).
linger_off() {
	[ "$GOOS" = "linux" ] || return 1 # на маке linger'а нет — там launchd
	[ "$IS_ROOT" -eq 0 ] || return 1 # root → system-unit, linger не нужен
	[ -f "$HOME/.config/systemd/user/remotai.service" ] || return 1
	command -v loginctl >/dev/null 2>&1 || return 1
	me="$(id -un)"
	state="$(loginctl show-user "$me" --property=Linger --value 2>/dev/null || true)"
	if [ -z "$state" ]; then
		# systemd < 240 не знает --value; вытаскиваем значение сами.
		state="$(loginctl show-user "$me" --property=Linger 2>/dev/null | sed -n 's/^Linger=//p')"
	fi
	case "$state" in
		yes | true | 1) return 1 ;;
		"") return 1 ;; # состояние не прочиталось — не пугаем догадками
		*) return 0 ;;
	esac
}

if linger_off; then
	ME="$(id -un)"
	if [ "$REMOTAI_INSTALL_LANGUAGE" = en ]; then
	cat <<EOF_EN

⚠️  Remotai is installed and paired, but will stop when you disconnect from SSH.

   The service is installed for your user (systemd --user), but linger is
   disabled for $ME. Systemd stops it when your session ends,
   and the computer will appear offline in the application.

   Copy this command to finish:

       sudo loginctl enable-linger $ME && systemctl --user restart remotai

   Check:   loginctl show-user $ME --property=Linger

   Status:  $STATUS_CMD
   Logs:    $LOGS_CMD
EOF_EN
	else
	cat <<EOF

⚠️  Remotai установлен и привязан, но выключится, когда вы выйдете из SSH.

   Сервис поставлен как пользовательский (systemd --user), а «linger» для
   пользователя $ME не включён — systemd гасит его вместе с вашим сеансом,
   и компьютер станет «не в сети» в приложении.

   Скопируйте одну строку — это всё, что осталось сделать:

       sudo loginctl enable-linger $ME && systemctl --user restart remotai

   Проверить:  loginctl show-user $ME --property=Linger

   Статус:  $STATUS_CMD
   Логи:    $LOGS_CMD
EOF
	fi
else
	if [ "$REMOTAI_INSTALL_LANGUAGE" = en ]; then
	cat <<EOF_EN

✅ Remotai is installed and paired.

   Status:  $STATUS_CMD
   Logs:    $LOGS_CMD
EOF_EN
	else
	cat <<EOF

✅ Remotai установлен и привязан.

   Статус:  $STATUS_CMD
   Логи:    $LOGS_CMD
EOF
	fi
	# На маке даём ещё и готовую команду перезапуска: если компьютер почему-то
	# не появился в приложении, это первое и обычно единственное, что нужно
	# сделать. Раньше её приходилось выпрашивать в переписке и набирать руками
	# — с двумя опечатками подряд (живой мак 10.08.2026).
	if [ "$GOOS" = "darwin" ]; then
		if [ "$REMOTAI_INSTALL_LANGUAGE" = en ]; then
		cat <<EOF_EN
   If the computer does not appear in the application, restart the service:
       $RESTART_CMD
EOF_EN
		else
		cat <<EOF
   Если компьютер не появился в приложении — перезапустите службу:
       $RESTART_CMD
EOF
		fi
	fi
fi

# Хвост про Remote Desktop — разный на Linux и маке. Печатать маководу совет
# доставить xdotool и xclip нельзя: это X11-пакеты, на macOS их не бывает, а
# сам Remote Desktop там пока не поддержан (нужен ScreenCaptureKit и бандл —
# см. docs/macos-port-status.md). Общий текст здесь просто врал.
if [ "$GOOS" = "darwin" ]; then
	if [ "$REMOTAI_INSTALL_LANGUAGE" = en ]; then
	cat <<EOF_EN

   Remote Desktop is not available on macOS yet. Terminals,
   files, SSH, monitoring and AI agents are fully supported.

   To open Remotai again: Finder home folder → Applications
   → Remotai. The interface opens in your browser without an extra terminal.
EOF_EN
	else
	cat <<EOF

   Экран компьютера (Remote Desktop) на macOS пока недоступен — терминалы,
   файлы, SSH, мониторинг и AI-агенты работают полностью.

   Открыть Remotai снова: домашняя папка Finder → Applications (Программы)
   → Remotai. Интерфейс откроется в браузере, без дополнительного терминала.
EOF
	fi
else
	if [ "$REMOTAI_INSTALL_LANGUAGE" = en ]; then
	cat <<EOF_EN

   Remote Desktop (in a graphical X11 session only) requires these packages:
       xdotool (mouse/keyboard input) and xclip (clipboard)
   Headless servers do not need them; terminals, files and agents work without them.
EOF_EN
	else
	cat <<EOF

   Remote Desktop (только на графической X11-сессии) требует пакетов:
       xdotool  (ввод мыши/клавиатуры) и  xclip  (буфер обмена)
   На headless-сервере они не нужны — терминалы/файлы/агенты работают без них.
EOF
	fi
fi
