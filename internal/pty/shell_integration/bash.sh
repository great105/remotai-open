# Remotai: разметка команд в терминале (OSC 133) для bash. ST-10, T-39.
#
# Как сюда попадают: при включённой настройке «Разметка команд»
# (config.ShellIntegration) агент запускает `bash --rcfile <этот файл>` вместо
# голого `bash`. --rcfile ЗАМЕНЯЕТ ~/.bashrc, поэтому первым делом подключаем
# его сами: пользовательская конфигурация (алиасы, completion, starship,
# direnv) работает ровно как в обычном терминале, а разметка ставится ПОСЛЕ
# неё и поверх неё.
#
# /etc/bash.bashrc здесь НАМЕРЕННО не подключаем. bash, собранный с SYS_BASHRC
# (Debian, Ubuntu, Arch, openSUSE), читает его сам и при --rcfile — до этого
# файла (живая проверка 13.09.2026, Ubuntu 24.04, bash 5.2.21: к началу
# --rcfile PS1 уже с debian_chroot, command_not_found_handle уже задан).
# Подключив его ещё раз, мы прогнали бы системный rc дважды; а сборки без
# SYS_BASHRC не читают его и в обычном терминале. Итог совпадает с обычным
# запуском в обоих случаях — так же поступает VS Code.

if [ -r ~/.bashrc ]; then
	. ~/.bashrc
fi

# Разметка — только в интерактивном bash от 4.4: там появились PS0 (начало
# вывода, C) и ${var@P} (номер команды, по которому видно, выполнялась ли она).
# Старый bash (системный 3.2 на macOS) получает свой ~/.bashrc и больше ничего.
# Повторный source этого файла второй раз хуки не ставит.
if [[ $- == *i* && -z ${__remotai_si_on-} ]] &&
	((BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 4))); then
	__remotai_si_on=1
	# \# — номер следующей команды. Растёт ТОЛЬКО когда команда выполнилась:
	# пустой Enter и Ctrl+C на приглашении его не меняют (живая проверка bash
	# 5.2). Так D шлётся лишь после настоящей команды, без DEBUG-trap, который
	# ломает чужие trap-и и замедляет каждую команду. $_ для этого не годится:
	# подоболочка и конвейер его не обновляют.
	__remotai_si_num='\#'
	__remotai_si_last=
	# Все маркеры завершаются BEL (\a), а не ST: так их вырезает серверная
	# эвристика вывода (events.go, ansiRe), и так их понимает любой терминал.
	__remotai_si_a='\[\e]133;A\a\]'
	__remotai_si_b='\[\e]133;B\a\]'
	__remotai_si_c='\e]133;C\a'

	# Первый элемент PROMPT_COMMAND: $? здесь ещё код пользовательской команды.
	# Возвращаем его же, чтобы пользовательский PROMPT_COMMAND (starship,
	# oh-my-bash) видел настоящий код, а не наш.
	__remotai_si_precmd() {
		local ec=$? n=${__remotai_si_num@P}
		if [[ -n $__remotai_si_last && $n != "$__remotai_si_last" ]]; then
			builtin printf '\e]133;D;%s\a' "$ec"
		fi
		__remotai_si_last=$n
		return "$ec"
	}

	# Наши обёртки не должны уезжать в окружение дочерних процессов. Если
	# пользователь экспортирует PROMPT_COMMAND (рецепт общей истории
	# `export PROMPT_COMMAND="history -a; …"`) или PS1, либо включил set -a,
	# экспортируется уже НАША версия: вложенный bash, новая панель tmux,
	# `sudo -E bash` получают имена наших функций без самих функций (bash их не
	# экспортирует) и на каждом приглашении пишут «__remotai_si_precmd: command
	# not found», а обёрнутый PS1 шлёт маркеры из чужого шелла (ревью B2, живая
	# проверка WSL, bash 5.2.21). Поэтому экспорт снимается со всего, что мы
	# обернули, и с наших служебных переменных — при установке и на каждом
	# приглашении (экспортировать заново могут посреди работы). Цена: дочерний
	# процесс не наследует PROMPT_COMMAND/PS1/PS0; интерактивный bash задаёт их
	# сам из ~/.bashrc, как и рассчитывает рецепт общей истории. export -n
	# незаданную переменную не создаёт (проверено там же). При set -a bash
	# экспортирует и сами функции (BASH_FUNC_…) — с них экспорт тоже снимаем.
	__remotai_si_unexport() {
		builtin export -n PROMPT_COMMAND PS1 PS0 __remotai_si_on __remotai_si_num \
			__remotai_si_last __remotai_si_a __remotai_si_b __remotai_si_c
		builtin export -fn __remotai_si_precmd __remotai_si_postcmd __remotai_si_unexport
	}

	# Последний элемент PROMPT_COMMAND: обёртка PS1 ставится заново, если
	# пользовательский код перезаписал PS1 (starship и подобные строят его на
	# каждом приглашении). PS0 — так же. $? возвращаем тот, что был: приглашение
	# часто красит себя по коду последней команды.
	__remotai_si_postcmd() {
		local ec=$?
		if [[ $PS1 != *"$__remotai_si_a"* ]]; then
			PS1=$__remotai_si_a$PS1$__remotai_si_b
		fi
		if [[ ${PS0-} != *"$__remotai_si_c"* ]]; then
			PS0=${PS0-}$__remotai_si_c
		fi
		__remotai_si_unexport
		return "$ec"
	}

	# PROMPT_COMMAND бывает строкой и (bash 5.1+) массивом — сохраняем форму.
	# Разделитель — перевод строки, а не «;»: пользовательская строка может
	# кончаться на «;» или «&», и «;;» стало бы синтаксической ошибкой.
	if [[ $(builtin declare -p PROMPT_COMMAND 2>/dev/null) == "declare -a"* ]]; then
		PROMPT_COMMAND=(__remotai_si_precmd "${PROMPT_COMMAND[@]}" __remotai_si_postcmd)
	else
		PROMPT_COMMAND=__remotai_si_precmd${PROMPT_COMMAND:+$'\n'$PROMPT_COMMAND}$'\n'__remotai_si_postcmd
	fi
	__remotai_si_unexport
fi
