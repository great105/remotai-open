package web

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"path/filepath"
	"testing"
)

// История — не журнал повторов: перезагрузка страницы должна поднимать запись
// вверх, а не добавлять вторую. Иначе десять одинаковых строк подряд закрывают
// то, что человек ищет.
func TestHistoryKeepsOneRowPerPage(t *testing.T) {
	store := &placesStore{loaded: true, path: filepath.Join(t.TempDir(), "places.json")}
	store.Note("https://example.com/a", "Первая")
	store.Note("https://example.com/b", "Вторая")
	store.Note("https://example.com/a", "Первая, обновлённая")

	all := store.History("", 0)
	if len(all) != 2 {
		t.Fatalf("записей %d, ожидались две: %+v", len(all), all)
	}
	if all[0].URL != "https://example.com/a" {
		t.Fatalf("свежая запись не первая: %+v", all)
	}
	if all[0].Title != "Первая, обновлённая" {
		t.Fatalf("заголовок не обновился: %q", all[0].Title)
	}

	// Поиск идёт и по заголовку, и по адресу — человек помнит то или другое.
	if got := store.History("обновл", 0); len(got) != 1 {
		t.Fatalf("поиск по заголовку дал %d", len(got))
	}
	if got := store.History("/b", 0); len(got) != 1 {
		t.Fatalf("поиск по адресу дал %d", len(got))
	}
	if got := store.History("нет такого", 0); len(got) != 0 {
		t.Fatalf("поиск нашёл лишнее: %+v", got)
	}

	if removed := store.ForgetHistory("https://example.com/b"); removed != 1 {
		t.Fatalf("удаление одной страницы вернуло %d", removed)
	}
	if removed := store.ForgetHistory(""); removed != 1 {
		t.Fatalf("очистка вернула %d, а осталась одна запись", removed)
	}
	if got := store.History("", 0); len(got) != 0 {
		t.Fatalf("после очистки осталось %+v", got)
	}
}

// Служебные адреса в историю не попадают: человек их не выбирал, а видеть
// «about:blank» в списке посещений бессмысленно.
func TestHistorySkipsInternalPages(t *testing.T) {
	store := &placesStore{loaded: true, path: filepath.Join(t.TempDir(), "places.json")}
	store.Note("chrome://new-tab-page/", "Новая вкладка")
	store.Note("about:blank", "")
	store.Note("file:///tmp/x.html", "Файл")
	if got := store.History("", 0); len(got) != 0 {
		t.Fatalf("внутренние страницы попали в историю: %+v", got)
	}
}

// Превью вкладки — уменьшенный кадр: полноразмерный JPEG в списке вкладок весил
// бы как сама страница.
func TestShrinkJPEGReducesFrame(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 900, 1600))
	for y := 0; y < 1600; y++ {
		for x := 0; x < 900; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 90, A: 255})
		}
	}
	var full bytes.Buffer
	if err := jpeg.Encode(&full, src, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}

	small, err := shrinkJPEG(full.Bytes(), 240)
	if err != nil {
		t.Fatalf("уменьшение: %v", err)
	}
	if len(small) >= full.Len() {
		t.Fatalf("превью не меньше кадра: %d против %d", len(small), full.Len())
	}
	img, err := jpeg.Decode(bytes.NewReader(small))
	if err != nil {
		t.Fatalf("превью не декодируется: %v", err)
	}
	if img.Bounds().Dx() != 240 {
		t.Fatalf("ширина превью %d вместо 240", img.Bounds().Dx())
	}
	// Пропорции сохраняются: растянутая картинка вкладки читалась бы неверно.
	if got := img.Bounds().Dy(); got < 400 || got > 440 {
		t.Fatalf("высота превью %d — пропорции потеряны", got)
	}
}

// Значок помнится ПО ДОМЕНУ (так он появляется и у остальных вкладок сайта), а
// «значка нет» тоже помнится — иначе каждый опрос списка вкладок дёргал бы сеть.
func TestIconCacheByHost(t *testing.T) {
	icons.mu.Lock()
	icons.byHost = nil
	icons.mu.Unlock()

	rememberIcon("https://www.example.com/page", "data:image/png;base64,AAA")
	if got := browserIconForHost("https://example.com/other"); got != "data:image/png;base64,AAA" {
		t.Fatalf("значок домена не найден: %q", got)
	}
	if got := browserIconForHost("https://other.test/"); got != "" {
		t.Fatalf("чужой домен получил значок: %q", got)
	}
	rememberIcon("no-icon.test", "")
	if got := browserIconForHost("no-icon.test"); got != "" {
		t.Fatalf("отрицательная запись отдала значок: %q", got)
	}
}
