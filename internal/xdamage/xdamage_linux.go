//go:build linux

// Package xdamage спрашивает у самого X-сервера, менялось ли изображение с
// прошлого кадра.
//
// Зачем: захват кадра на X11 — это XGetImage на весь экран, то есть копия
// 1280×800×4 ≈ 4 МБ через сокет. Раньше стрим делал её 30 раз в секунду
// независимо от того, происходит на экране хоть что-то. На типовом VPS с ОДНИМ
// ядром это ядро отбиралось у самого браузера: страница тормозила ровно
// потому, что мы усердно перекладывали её неподвижную картинку.
//
// Расширение DAMAGE решает это в один вопрос: X сам сообщает, что часть экрана
// перерисовалась. Нет сообщений — нет и захвата.
//
// Границы намеренные:
//   - следим за корневым окном целиком (какая именно область изменилась, здесь
//     не важно: кодек всё равно жмёт кадр целиком);
//   - при любой неполадке (нет расширения, оборвалось соединение, сменился
//     DISPLAY) отвечаем «менялось» — то есть возвращаемся к прежнему поведению,
//     а не к чёрному экрану;
//   - вызывающий обязан раз в секунду захватывать кадр всё равно (см.
//     remote_grab_linux.go): DAMAGE не покрывает совсем экзотические случаи
//     вроде смены палитры, и залипнуть на устаревшем кадре нельзя.
package xdamage

import (
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/xproto"
)

type watcher struct {
	mu      sync.Mutex
	conn    *xgb.Conn
	display string // DISPLAY, на котором открыто соединение
	id      damage.Damage
	failed  bool // расширения нет / подключиться не вышло — больше не пробуем на этом display
	dirty   atomic.Bool
}

var w watcher

// Changed сообщает, менялось ли изображение с прошлого вызова, и сбрасывает
// отметку. true — «менялось или неизвестно»: захватывать кадр нужно.
func Changed() bool {
	if !w.ensure() {
		return true // следить не вышло — ведём себя как раньше
	}
	return w.dirty.Swap(false)
}

// Active — слежение работает (для диагностики и тестов).
func Active() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn != nil && !w.failed
}

// ensure открывает соединение с текущим DISPLAY и подписку на повреждения.
// Виртуальный браузер подменяет DISPLAY на лету, поэтому проверяем на каждом
// кадре: подписка на старый экран показывала бы «ничего не менялось» вечно.
func (w *watcher) ensure() bool {
	display := os.Getenv("DISPLAY")
	if display == "" {
		return false
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil && w.display == display {
		return !w.failed
	}
	w.closeLocked()
	w.display, w.failed = display, false

	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		w.failed = true
		return false
	}
	if err := damage.Init(conn); err != nil {
		// Расширения нет (редкая сборка X) — молча возвращаемся к захвату
		// каждого кадра.
		conn.Close()
		w.failed = true
		return false
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	id, err := damage.NewDamageId(conn)
	if err != nil {
		conn.Close()
		w.failed = true
		return false
	}
	// ReportLevelNonEmpty: одно событие на «стал непустым», дальше молчание до
	// нашего Subtract. Это ровно тот темп, который нужен: мы всё равно
	// спрашиваем раз в кадр, а поток событий на каждое движение курсора
	// нагрузил бы канал сильнее, чем экономит.
	if err := damage.CreateChecked(conn, id, xproto.Drawable(root),
		damage.ReportLevelNonEmpty).Check(); err != nil {
		conn.Close()
		w.failed = true
		return false
	}

	w.conn, w.id = conn, id
	w.dirty.Store(true) // первый кадр после подписки всегда снимаем
	go w.readLoop(conn, id)
	log.Printf("[XDAMAGE] watching %s", display)
	return true
}

// readLoop помечает экран изменившимся и разрешает X-серверу прислать
// следующее событие (без Subtract он замолкает навсегда).
func (w *watcher) readLoop(conn *xgb.Conn, id damage.Damage) {
	for {
		ev, err := conn.WaitForEvent()
		if ev == nil && err == nil {
			return // соединение закрыто
		}
		if err != nil {
			continue // протокольная жалоба — не повод бросать слежение
		}
		if _, ok := ev.(damage.NotifyEvent); ok {
			w.dirty.Store(true)
			damage.Subtract(conn, id, 0, 0)
		}
	}
}

func (w *watcher) closeLocked() {
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
}
