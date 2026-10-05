package cdp

// То, что в телефонном браузере делается пальцем, а у нас без отдельной команды
// не делается вовсе.
//
// Кадр — это картинка страницы: по нему нельзя ни узнать, куда ведёт ссылка под
// пальцем, ни выделить текст, ни найти слово на длинной странице. В настоящем
// мобильном браузере всё это есть, и без этого «браузер» остаётся смотрелкой:
// долгое нажатие по ссылке ничего не открывает в новой вкладке, длинную статью
// нельзя обыскать, а найденный телефон компании нельзя скопировать себе.
//
// Все ответы собираются одним Runtime.evaluate: разбирать DOM по узлам через
// протокол дороже и хрупче, а нам нужен ровно тот же ответ, что дал бы сам
// браузер элементу под пальцем.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// HitTest — что находится под точкой страницы (координаты в CSS-пикселях
// viewport, ровно те же, в которых приходит ввод).
type HitTest struct {
	Link     string `json:"link,omitempty"`      // ссылка под пальцем
	LinkText string `json:"link_text,omitempty"` // её подпись
	Image    string `json:"image,omitempty"`     // адрес картинки
	Video    string `json:"video,omitempty"`
	Text     string `json:"text,omitempty"`     // текст элемента (для копирования)
	Editable bool   `json:"editable,omitempty"` // поле ввода — тогда меню другое
	Tag      string `json:"tag,omitempty"`
}

// hitTestJS возвращает описание элемента под точкой. Ищем ближайшую ссылку
// вверх по дереву: палец почти никогда не попадает точно в <a>, чаще — в
// картинку или подпись внутри неё.
const hitTestJS = `(() => {
  const el = document.elementFromPoint(%.2f, %.2f);
  if (!el) return null;
  const closest = (sel) => el.closest ? el.closest(sel) : null;
  const a = closest('a[href]');
  const img = el.tagName === 'IMG' ? el : (closest('picture') ? closest('picture').querySelector('img') : null);
  const video = el.tagName === 'VIDEO' ? el : null;
  const editable = (() => {
    const t = (el.tagName || '').toLowerCase();
    if (t === 'textarea') return true;
    if (t === 'input') {
      const k = (el.type || 'text').toLowerCase();
      return !['button','submit','reset','checkbox','radio','file','image','range','color'].includes(k);
    }
    return el.isContentEditable === true;
  })();
  const text = (el.innerText || el.textContent || '').trim().slice(0, 600);
  return {
    link: a ? a.href : '',
    link_text: a ? (a.innerText || a.textContent || '').trim().slice(0, 200) : '',
    image: img ? img.currentSrc || img.src : '',
    video: video ? video.currentSrc || video.src : '',
    text,
    editable,
    tag: (el.tagName || '').toLowerCase(),
  };
})()`

// HitTest спрашивает страницу, что лежит под точкой. Пустой результат (клик по
// пустому месту) — не ошибка: интерфейс покажет меню самой страницы.
func (c *Client) HitTest(ctx context.Context, x, y float64) (HitTest, error) {
	raw, err := c.evaluateJSON(ctx, fmt.Sprintf(hitTestJS, x, y))
	if err != nil {
		return HitTest{}, err
	}
	var hit HitTest
	if len(raw) == 0 || string(raw) == "null" {
		return HitTest{}, nil
	}
	if err := json.Unmarshal(raw, &hit); err != nil {
		return HitTest{}, err
	}
	return hit, nil
}

// IconURL — адрес значка сайта, как его объявила сама страница.
//
// Список целей DevTools значка НЕ содержит (проверено на живом сервере: в
// /json/list полей favicon нет вовсе), а угадывать «/favicon.ico» верно не
// всегда: половина сайтов кладёт значок по своему пути. Спрашиваем страницу —
// она знает точно.
func (c *Client) IconURL(ctx context.Context) (string, error) {
	raw, err := c.evaluateJSON(ctx, `(() => {
  const rels = ['link[rel~="icon"]', 'link[rel="shortcut icon"]', 'link[rel="apple-touch-icon"]'];
  for (const sel of rels) {
    const el = document.querySelector(sel);
    if (el && el.href) return el.href;
  }
  try { return new URL('/favicon.ico', location.href).href; } catch (e) { return ''; }
})()`)
	if err != nil {
		return "", err
	}
	var icon string
	_ = json.Unmarshal(raw, &icon)
	return icon, nil
}

// IconData — значок сайта, загруженный САМОЙ страницей и уже готовый к показу
// (data-URL).
//
// Тянуть значок отдельным запросом с агента ненадёжно: часть сайтов отвечает на
// «безымянного» клиента отказом. Живой пример — Википедия: `curl` с того же
// сервера значок отдаёт, а http-клиент агента получал отказ, и в списке вкладок
// значка не было. Страница же качает его собственным User-Agent и со своими
// куками — ровно так, как это делает браузер для своей вкладки.
func (c *Client) IconData(ctx context.Context) (string, error) {
	raw, err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": `(async () => {
  const rels = ['link[rel~="icon"]', 'link[rel="shortcut icon"]', 'link[rel="apple-touch-icon"]'];
  let href = '';
  for (const sel of rels) {
    const el = document.querySelector(sel);
    if (el && el.href) { href = el.href; break; }
  }
  if (!href) { try { href = new URL('/favicon.ico', location.href).href; } catch (e) { return ''; } }
  if (href.startsWith('data:')) return href.length > 64 ? href : '';
  try {
    const res = await fetch(href, {credentials: 'include'});
    if (!res.ok) return '';
    const blob = await res.blob();
    if (!blob.size || blob.size > 65536 || !/^image\//.test(blob.type || 'image/x-icon')) return '';
    return await new Promise((resolve) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result || ''));
      reader.onerror = () => resolve('');
      reader.readAsDataURL(blob);
    });
  } catch (e) { return ''; }
})()`,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	if !strings.HasPrefix(res.Result.Value, "data:image/") {
		return "", nil
	}
	return res.Result.Value, nil
}

// SelectionText — что человек выделил на странице (для «Копировать»).
func (c *Client) SelectionText(ctx context.Context) (string, error) {
	raw, err := c.evaluateJSON(ctx, `(() => {
  const s = window.getSelection();
  return s ? String(s).slice(0, 100000) : '';
})()`)
	if err != nil {
		return "", err
	}
	var text string
	_ = json.Unmarshal(raw, &text)
	return text, nil
}

// selectWordJS выделяет слово под точкой — начало «выделить и скопировать», как
// на телефоне: долгое нажатие подсвечивает слово, дальше человек тянет края.
const selectWordJS = `(() => {
  const x = %.2f, y = %.2f;
  let range = null;
  if (document.caretRangeFromPoint) range = document.caretRangeFromPoint(x, y);
  else if (document.caretPositionFromPoint) {
    const p = document.caretPositionFromPoint(x, y);
    if (p) { range = document.createRange(); range.setStart(p.offsetNode, p.offset); range.collapse(true); }
  }
  if (!range) return '';
  const sel = window.getSelection();
  sel.removeAllRanges();
  sel.addRange(range);
  if (sel.modify) {
    sel.modify('move', 'backward', 'word');
    sel.modify('extend', 'forward', 'word');
  }
  return String(sel).trim().slice(0, 5000);
})()`

// SelectWordAt выделяет слово под точкой и отдаёт его текст.
func (c *Client) SelectWordAt(ctx context.Context, x, y float64) (string, error) {
	raw, err := c.evaluateJSON(ctx, fmt.Sprintf(selectWordJS, x, y))
	if err != nil {
		return "", err
	}
	var text string
	_ = json.Unmarshal(raw, &text)
	return text, nil
}

// FindResult — состояние поиска по странице.
type FindResult struct {
	Matches int    `json:"matches"`
	Index   int    `json:"index"` // номер текущего совпадения, с единицы
	Query   string `json:"query"`
}

// findJS — поиск по странице силами самого браузера (window.find). Он честно
// прокручивает к найденному и подсвечивает его так же, как «найти на странице»
// в обычном браузере, поэтому человек видит результат в кадре без нашей
// подсветки. Счётчик совпадений считаем отдельным проходом по тексту.
const findJS = `(() => {
  const q = %s, forward = %t, fresh = %t;
  if (!q) { try { window.getSelection().removeAllRanges(); } catch (e) {} return {matches: 0, index: 0}; }
  if (fresh) { try { window.getSelection().removeAllRanges(); } catch (e) {} }
  const found = window.find(q, false, !forward, true, false, true, false);
  const text = document.body ? (document.body.innerText || '') : '';
  const needle = q.toLowerCase();
  let matches = 0, from = 0;
  const hay = text.toLowerCase();
  while (needle && from <= hay.length) {
    const at = hay.indexOf(needle, from);
    if (at < 0) break;
    matches++;
    from = at + needle.length;
    if (matches > 999) break;
  }
  return {matches, found: !!found};
})()`

// Find ищет текст на странице. fresh=true начинает поиск заново (новый запрос),
// иначе продолжает к следующему совпадению.
func (c *Client) Find(ctx context.Context, query string, forward, fresh bool) (FindResult, error) {
	q, err := json.Marshal(query)
	if err != nil {
		return FindResult{}, err
	}
	raw, err := c.evaluateJSON(ctx, fmt.Sprintf(findJS, string(q), forward, fresh))
	if err != nil {
		return FindResult{}, err
	}
	var res struct {
		Matches int  `json:"matches"`
		Found   bool `json:"found"`
	}
	_ = json.Unmarshal(raw, &res)
	return FindResult{Matches: res.Matches, Query: query}, nil
}

// ScrollState — где страница по вертикали. Нужен «потянуть вниз, чтобы
// обновить»: жест обязан срабатывать ТОЛЬКО у самого верха, иначе он крал бы
// обычную прокрутку.
type ScrollState struct {
	Y      float64 `json:"y"`
	Height float64 `json:"height"`
	View   float64 `json:"view"`
}

// Scroll отдаёт положение прокрутки главного документа.
func (c *Client) Scroll(ctx context.Context) (ScrollState, error) {
	raw, err := c.evaluateJSON(ctx, `({
  y: window.scrollY || 0,
  height: (document.documentElement && document.documentElement.scrollHeight) || 0,
  view: window.innerHeight || 0,
})`)
	if err != nil {
		return ScrollState{}, err
	}
	var st ScrollState
	_ = json.Unmarshal(raw, &st)
	return st, nil
}

// ScrollFocusIntoView поднимает поле ввода над клавиатурой. Сжатия окна обычно
// достаточно (страница сама подтягивает фокус), но не на всех сайтах: там, где
// поле лежит в собственном прокручиваемом блоке, оно остаётся под клавиатурой.
func (c *Client) ScrollFocusIntoView(ctx context.Context) error {
	_, err := c.evaluateJSON(ctx, `(() => {
  const el = document.activeElement;
  if (el && el.scrollIntoView) el.scrollIntoView({block: 'center', inline: 'nearest'});
  return true;
})()`)
	return err
}

// evaluateJSON выполняет выражение и отдаёт значение результата. Ошибка самой
// страницы (исключение в выражении) возвращается как ошибка — молчать о ней
// нельзя: интерфейс покажет действие, которое ничего не делает.
func (c *Client) evaluateJSON(ctx context.Context, expression string) (json.RawMessage, error) {
	raw, err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		// Жесты человека — пользовательские действия: без этого признака
		// window.find и открытие окон считаются «программными» и могут быть
		// заблокированы политикой страницы.
		"userGesture": true,
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if res.Exception != nil && strings.TrimSpace(res.Exception.Text) != "" {
		return nil, fmt.Errorf("страница ответила ошибкой: %s", res.Exception.Text)
	}
	return res.Result.Value, nil
}
