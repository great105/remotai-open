package cdp

// Формы: чтобы «зарегистрироваться где угодно» не означало «набрать двадцать
// полей пальцем через удалённый экран».
//
// Здесь две операции. Первая — РАЗОБРАТЬ форму: пройти по видимым полям и
// понять, что каждое просит (почту, телефон, пароль, имя, индекс). Вторая —
// ЗАПОЛНИТЬ поле по-человечески: поставить в него курсор и напечатать
// настоящими нажатиями, а не присвоить value из скрипта.
//
// Разница между «напечатать» и «присвоить» здесь принципиальная: современные
// формы (React и его родня) слушают события ввода, а не значение поля. Строка,
// вписанная присваиванием, видна глазами, но кнопка «Продолжить» остаётся
// серой, а при отправке улетает пустое значение — с точки зрения человека
// «Remotai заполнил, а сайт не принял».

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// FormField — поле формы, распознанное на странице.
type FormField struct {
	Index int    `json:"index"` // порядковый номер в разметке (метка для фокуса)
	Kind  string `json:"kind"`  // login | password | password2 | email | phone | first | last | name | birthday | city | address | zip | country | search | other
	Type  string `json:"type"`
	Label string `json:"label,omitempty"`
	Value string `json:"value,omitempty"` // у паролей не заполняется
	Empty bool   `json:"empty"`
}

// formScanJS помечает видимые поля и рассказывает, что каждое просит.
//
// Классификация читает всё, по чему человек сам понимает назначение поля:
// autocomplete (его ставят те, кто заботится о заполнении), затем type, имя,
// подпись, подсказку внутри и ближайший <label>. Порядок важен: autocomplete
// точнее любых догадок по имени, а по одному только name легко перепутать
// «user» и «username of your company».
const formScanJS = `(() => {
  const out = [];
  const seen = new Set();
  const visible = (el) => {
    if (el.disabled || el.readOnly) return false;
    if (el.type === 'hidden') return false;
    const r = el.getBoundingClientRect();
    if (r.width < 8 || r.height < 8) return false;
    const st = getComputedStyle(el);
    return st.visibility !== 'hidden' && st.display !== 'none' && Number(st.opacity) > 0.05;
  };
  const labelOf = (el) => {
    let text = '';
    if (el.labels && el.labels.length) text = el.labels[0].innerText || '';
    if (!text && el.getAttribute('aria-label')) text = el.getAttribute('aria-label');
    if (!text && el.placeholder) text = el.placeholder;
    return (text || '').trim().slice(0, 80);
  };
  const kindOf = (el) => {
    const ac = (el.getAttribute('autocomplete') || '').toLowerCase();
    const type = (el.type || '').toLowerCase();
    const hay = [el.name, el.id, el.placeholder, el.getAttribute('aria-label'), labelOf(el)]
      .filter(Boolean).join(' ').toLowerCase();
    if (type === 'password' || ac.includes('password')) return 'password';
    if (type === 'email' || ac === 'email' || /e-?mail|почт/.test(hay)) return 'email';
    if (type === 'tel' || ac === 'tel' || /phone|tel|телефон/.test(hay)) return 'phone';
    if (type === 'search' || ac === 'search' || /search|поиск/.test(hay)) return 'search';
    if (ac === 'username' || /login|username|user_?name|логин|никнейм|nickname/.test(hay)) return 'login';
    if (ac === 'given-name' || /first_?name|firstname|\bимя\b|given/.test(hay)) return 'first';
    if (ac === 'family-name' || /last_?name|lastname|surname|фамили/.test(hay)) return 'last';
    if (ac === 'name' || /full_?name|your name|\bfio\b|фио/.test(hay)) return 'name';
    if (type === 'date' || ac === 'bday' || /birth|дата рожд|день рожд/.test(hay)) return 'birthday';
    if (ac.includes('postal') || /zip|postal|индекс/.test(hay)) return 'zip';
    if (ac.includes('locality') || /\bcity\b|город/.test(hay)) return 'city';
    if (ac.includes('country') || /country|страна/.test(hay)) return 'country';
    if (ac.includes('street') || ac === 'address-line1' || /address|адрес|улиц/.test(hay)) return 'address';
    if (type === 'text' && /code|код|otp|sms/.test(hay)) return 'code';
    return 'other';
  };

  const all = document.querySelectorAll('input, textarea, select');
  let index = 0;
  let passwords = 0;
  for (const el of all) {
    const type = (el.type || '').toLowerCase();
    if (['button','submit','reset','image','file','checkbox','radio','range','color'].includes(type)) continue;
    if (!visible(el)) continue;
    if (seen.has(el)) continue;
    seen.add(el);
    el.setAttribute('data-remotai-field', String(index));
    let kind = kindOf(el);
    if (kind === 'password') {
      passwords++;
      // Второе поле пароля на странице — это «повторите пароль»: заполнять его
      // надо тем же значением, а не считать отдельным паролем.
      if (passwords > 1) kind = 'password2';
    }
    out.push({
      index,
      kind,
      type: type || el.tagName.toLowerCase(),
      label: labelOf(el),
      value: type === 'password' ? '' : String(el.value || '').slice(0, 200),
      empty: !String(el.value || '').length,
    });
    index++;
  }
  return out;
})()`

// ScanForm возвращает поля формы на текущей странице.
func (c *Client) ScanForm(ctx context.Context) ([]FormField, error) {
	raw, err := c.evaluateJSON(ctx, formScanJS)
	if err != nil {
		return nil, err
	}
	var fields []FormField
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// focusFieldJS ставит курсор в помеченное поле и очищает его. Очистка именно
// выделением: страница обязана увидеть замену текста, а не появление второго
// значения впритык к первому.
const focusFieldJS = `(() => {
  const el = document.querySelector('[data-remotai-field="%d"]');
  if (!el) return false;
  el.scrollIntoView({block: 'center', inline: 'nearest'});
  el.focus();
  if (el.select) { try { el.select(); } catch (e) {} }
  return document.activeElement === el;
})()`

// clearFieldJS убирает остаток значения, если выделение не сработало (бывает у
// полей с масками). Делается нативным сеттером плюс событиями — иначе форма на
// React не заметит очистки.
const clearFieldJS = `(() => {
  const el = document.querySelector('[data-remotai-field="%d"]');
  if (!el || !el.value) return true;
  const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  const setter = Object.getOwnPropertyDescriptor(proto, 'value');
  if (setter && setter.set) setter.set.call(el, '');
  else el.value = '';
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return true;
})()`

// FillField ставит курсор в поле и печатает значение настоящими нажатиями.
func (c *Client) FillField(ctx context.Context, index int, value string) error {
	raw, err := c.evaluateJSON(ctx, fmt.Sprintf(focusFieldJS, index))
	if err != nil {
		return err
	}
	var focused bool
	_ = json.Unmarshal(raw, &focused)
	if !focused {
		return fmt.Errorf("поле %d не нашлось на странице", index)
	}
	ctrl := NewController(c)
	// Выделенное заменяется первым же набранным символом, но там, где выделение
	// не сработало, остаток стёрли бы «задом наперёд» — чистим явно.
	if _, err := c.evaluateJSON(ctx, fmt.Sprintf(clearFieldJS, index)); err != nil {
		return err
	}
	if value == "" {
		return nil
	}
	return ctrl.TypeText(value)
}

// SelectOption выбирает значение в выпадающем списке (страна, месяц рождения).
// Набирать в них нечего: там нет ввода, есть выбор.
func (c *Client) SelectOption(ctx context.Context, index int, value string) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.evaluateJSON(ctx, fmt.Sprintf(`(() => {
  const el = document.querySelector('[data-remotai-field="%d"]');
  if (!el || el.tagName !== 'SELECT') return false;
  const want = String(%s).toLowerCase();
  for (const opt of el.options) {
    const text = (opt.text || '').toLowerCase();
    const val = (opt.value || '').toLowerCase();
    if (text === want || val === want || text.includes(want) || val.includes(want)) {
      el.value = opt.value;
      el.dispatchEvent(new Event('input', {bubbles: true}));
      el.dispatchEvent(new Event('change', {bubbles: true}));
      return true;
    }
  }
  return false;
})()`, index, string(payload)))
	return err
}

// Credentials — что сейчас введено в полях входа. По ним предлагается сохранить
// пару: человек уже набрал логин и пароль, спрашивать их второй раз в отдельной
// форме было бы издевательством.
type Credentials struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// ReadCredentials читает логин и пароль прямо из полей страницы.
func (c *Client) ReadCredentials(ctx context.Context) (Credentials, error) {
	raw, err := c.evaluateJSON(ctx, `(() => {
  const visible = (el) => {
    const r = el.getBoundingClientRect();
    return r.width > 8 && r.height > 8;
  };
  let password = '';
  for (const el of document.querySelectorAll('input[type=password]')) {
    if (el.value) { password = el.value; break; }
  }
  let login = '';
  const cands = document.querySelectorAll('input[type=email], input[type=text], input[type=tel], input:not([type])');
  for (const el of cands) {
    if (!el.value || !visible(el)) continue;
    const hay = [el.name, el.id, el.getAttribute('autocomplete'), el.placeholder].filter(Boolean).join(' ').toLowerCase();
    if (/search|поиск|captcha|code|otp/.test(hay)) continue;
    login = el.value;
    break;
  }
  return {login: login.slice(0, 200), password: password.slice(0, 200)};
})()`)
	if err != nil {
		return Credentials{}, err
	}
	var creds Credentials
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &creds)
	}
	return creds, nil
}

// SubmitForm нажимает «отправить» в форме, где стоит курсор: после
// автозаполнения человек ждёт того же, что на телефоне, — заполнилось и ушло.
func (c *Client) SubmitForm(ctx context.Context) error {
	_, err := c.evaluateJSON(ctx, `(() => {
  const el = document.activeElement;
  const form = el && el.form ? el.form : document.querySelector('form');
  if (!form) return false;
  const btn = form.querySelector('button[type=submit], input[type=submit], button:not([type])');
  if (btn) { btn.click(); return true; }
  if (form.requestSubmit) { form.requestSubmit(); return true; }
  form.submit();
  return true;
})()`)
	return err
}

// FieldKindsForProfile — какие поля заполняются из анкеты. Пароли сюда не
// входят: они приходят из хранилища, а не из анкеты.
var FieldKindsForProfile = map[string]bool{
	"email": true, "phone": true, "first": true, "last": true, "name": true,
	"birthday": true, "city": true, "address": true, "zip": true, "country": true,
	"login": true,
}

// NormalizeFieldKind защищает от опечаток на стороне клиента.
func NormalizeFieldKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}
