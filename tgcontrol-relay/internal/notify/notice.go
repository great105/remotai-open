// Package notify — нейтральные типы уведомления пользователю о том, что
// AI-агент на его ПК ждёт ответа.
//
// Пакет существует ровно для того, чтобы server и bot могли обмениваться этими
// данными, не импортируя друг друга (см. комментарий у server.Bot): server
// собирает Notice и отдаёт её через колбэк, bot превращает Notice в сообщение с
// инлайн-кнопками, а нажатие кнопки возвращается в server через PtyInputFunc.
// Зависимостей, кроме stdlib, здесь быть не должно.
package notify

import (
	"context"
	"errors"
)

// Kind — повод для уведомления. Соответствует pty_event.event на агенте
// (internal/pty/events.go): наружу нас интересуют только эти два.
type Kind string

const (
	KindWaiting Kind = "waiting_input" // агент задал вопрос и стоит на промпте
	KindError   Kind = "error"         // в терминале мелькнула ошибка
)

// Sentinel-ошибки ответа агенту. Возвращаются из PtyInputFunc, чтобы бот мог
// объяснить человеку причину отказа, не импортируя server/relayhub.
var (
	// ErrOffline — ПК не в сети: ответить физически некуда.
	ErrOffline = errors.New("компьютер не в сети")
	// ErrUnsupported — версия Remotai на ПК не умеет принимать ввод по REST
	// (нет POST /api/pty/{id}/input).
	ErrUnsupported = errors.New("агент не поддерживает ответ из Telegram")
	// ErrPtyGone — терминал уже закрыт или процесс в нём умер.
	ErrPtyGone = errors.New("терминал уже закрыт")
	// ErrPromptChanged — агент за это время спросил о другом: ответ из старого
	// уведомления подтвердил бы не то действие, поэтому ПК его не применил
	// (409 prompt_changed).
	ErrPromptChanged = errors.New("агент спрашивает уже о другом")
	// ErrKeyNotAllowed — попытка отправить в PTY что-то за пределами словаря
	// InputKeys (защита от «выполнить что угодно на ПК из Telegram»).
	ErrKeyNotAllowed = errors.New("недопустимый ответ")
	// ErrNoAccess — у этого аккаунта нет прав на этот ПК (или UserID не указан
	// вовсе). Проверку делает сам сервер в SendPtyInput, а не только вызывающий.
	ErrNoAccess                = errors.New("нет доступа к этому компьютеру")
	ErrSubscriptionRequired    = errors.New("для ответа через Telegram нужна подписка Про")
	ErrSubscriptionUnavailable = errors.New("не удалось проверить подписку; ответ не отправлен")
	// ErrRateLimited — ПК отбил ввод по своему лимитеру (code "rate_limited" /
	// HTTP 429): человеку нужно сказать «слишком часто», а не «не получилось».
	ErrRateLimited = errors.New("слишком часто")
)

// Notice — всё, что нужно боту, чтобы написать понятное сообщение.
type Notice struct {
	Kind Kind

	DeviceID   string
	DeviceName string
	PtyID      string
	PtyName    string

	Agent     string // pty.AgentKind: "claude" | "codex" | … ("" — не агент)
	FgProcess string // имя процесса на переднем плане
	Hint      string // сам вопрос: «Подтвердите: y/n»
	HintKind  string // yes_no | choice | enter | text ("" — тип не распознан)

	// StatusAt — начало эпизода ожидания (unix ms) на момент отправки. Уезжает
	// в кнопку и возвращается как expect_status_at: ПК не применит ответ, если
	// к моменту нажатия агент спрашивает уже о другом. 0 — неизвестно.
	StatusAt int64

	AgentOnline bool // ПК на связи прямо сейчас
	CanReply    bool // ПК на связи И версия умеет POST /api/pty/{id}/input
}

// InputRequest — «нажать клавишу в терминале ПК».
type InputRequest struct {
	// UserID — от чьего имени пишем. Обязателен: сервер сам проверяет права на
	// этот ПК (Server.SendPtyInput), поэтому 0 = отказ, а не «проверку сделал
	// вызывающий».
	UserID   int64
	DeviceID string
	PtyID    string
	Key      string
	// ExpectStatusAt — вопрос, который человек ВИДЕЛ (unix ms). 0 — без
	// проверки (старое сообщение, ПК был офлайн в момент отправки).
	ExpectStatusAt int64
}

// PtyInputFunc — реализуется сервером (Server.SendPtyInput), проводится в бота
// из main.go.
type PtyInputFunc func(ctx context.Context, req InputRequest) error

// inputKeys — ЕДИНСТВЕННЫЙ словарь ответов, которые разрешено отправлять из
// Telegram, и их подписи для чата. Произвольный текст (поле data агентского
// эндпоинта) через этот путь не проходит НИКОГДА: callback_data приходит из
// сообщения, которое живёт в чате вечно и может быть подделано клиентом
// Telegram, поэтому «выполнить что угодно на ПК» здесь недопустимо.
//
// Байты, в которые агент разворачивает ключи, совпадают с кнопками экрана
// терминала (apk/src/pages/PtyTermView.tsx): y→"y\r", n→"n\r", 1/2/3 — без CR,
// enter→"\r", esc→"\x1b".
var inputKeys = map[string]string{
	"y":     "Да",
	"n":     "Нет",
	"1":     "1",
	"2":     "2",
	"3":     "3",
	"enter": "Enter",
	"esc":   "Esc",
}

// KeyAllowed — можно ли отправить такой ответ из Telegram.
func KeyAllowed(key string) bool {
	_, ok := inputKeys[key]
	return ok
}

// KeyLabel — человекочитаемая подпись ответа («Отправлено: Да»).
func KeyLabel(key string) string {
	if label, ok := inputKeys[key]; ok {
		return label
	}
	return key
}
