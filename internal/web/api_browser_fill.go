package web

// «Заполнить» и «Сохранить вход» — то, ради чего с телефона вообще можно
// РЕГИСТРИРОВАТЬСЯ на сайтах через сервер.
//
// Без этого регистрация выглядит так: набрать почту по буквам через удалённый
// экран, придумать пароль в голове, набрать его дважды и запомнить навсегда —
// потому что записать его некуда, свой менеджер паролей на телефоне о сайтах
// сервера не знает. С этим — два нажатия: «Заполнить» ставит анкету и новый
// пароль, «Сохранить» кладёт пару в шифрованное хранилище рядом с браузером.
//
// Пароль НИКОГДА не уходит наружу: клиент называет запись по идентификатору, а
// значение попадает в поле страницы внутри агента.

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"tgcontrol/internal/cdp"
	"tgcontrol/internal/vbrowser"
)

// currentBrowserHost — сайт, открытый в браузере (для привязки пароля).
func currentBrowserHost(ctx context.Context, client *cdp.Client) string {
	return normalizeVaultHost(client.URL(ctx))
}

// GET /api/vbrowser/logins — сохранённые входы (без паролей). Если браузер на
// связи, сначала идут подходящие открытому сайту.
func (s *Server) apiBrowserLogins(w http.ResponseWriter, r *http.Request, _ int64) {
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		if client := currentCDPClient(); client != nil {
			ctx, cancel := browserCtx(r)
			defer cancel()
			host = currentBrowserHost(ctx, client)
		}
	}
	resp := map[string]any{
		"host":    normalizeVaultHost(host),
		"logins":  vault.List(host),
		"all":     len(vault.List("")),
		"profile": vault.Profile(),
		"foreign": vault.Foreign(),
	}
	jsonResp(w, resp)
}

// POST /api/vbrowser/logins {host?, login, password, note?} — сохранить вход.
// Пустой пароль означает «возьми то, что сейчас в полях страницы»: человек уже
// его набрал, и просить набрать второй раз было бы издевательством.
func (s *Server) apiBrowserLoginSave(w http.ResponseWriter, r *http.Request, _ int64) {
	var req struct {
		Host     string `json:"host"`
		Login    string `json:"login"`
		Password string `json:"password"`
		Note     string `json:"note"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "bad request", nil)
		return
	}
	if req.Password == "" || req.Login == "" || req.Host == "" {
		client := controlCDPClient()
		if client == nil {
			jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
			return
		}
		ctx, cancel := browserCtx(r)
		defer cancel()
		if req.Host == "" {
			req.Host = currentBrowserHost(ctx, client)
		}
		creds, err := client.ReadCredentials(ctx)
		if err != nil {
			jsonErrorCode(w, http.StatusBadGateway, "read_failed", err.Error(), nil)
			return
		}
		if req.Login == "" {
			req.Login = creds.Login
		}
		if req.Password == "" {
			req.Password = creds.Password
		}
	}
	if req.Password == "" {
		jsonErrorCode(w, http.StatusBadRequest, "no_password",
			"На странице не нашлось заполненного пароля.", nil)
		return
	}
	rec, err := vault.Save(req.Host, req.Login, req.Password, req.Note)
	if err != nil {
		jsonErrorCode(w, http.StatusConflict, "save_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "login": rec})
}

// DELETE /api/vbrowser/logins/{id} — забыть вход.
func (s *Server) apiBrowserLoginDelete(w http.ResponseWriter, r *http.Request, _ int64) {
	if !vault.Delete(r.PathValue("id")) {
		jsonErrorCode(w, http.StatusNotFound, "not_found", "Такой записи нет.", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}

// PUT /api/vbrowser/profile — анкета для форм регистрации.
func (s *Server) apiBrowserProfileSave(w http.ResponseWriter, r *http.Request, _ int64) {
	var req BrowserProfile
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "bad request", nil)
		return
	}
	if err := vault.SetProfile(req); err != nil {
		jsonErrorCode(w, http.StatusConflict, "save_failed", err.Error(), nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "profile": vault.Profile()})
}

// GET /api/vbrowser/form — что за форма на странице: какие поля она просит и
// есть ли чем их заполнить. По этому ответу интерфейс решает, показывать ли
// кнопку «Заполнить» и что на ней написать.
func (s *Server) apiBrowserForm(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	ctx, cancel := browserCtx(r)
	defer cancel()
	fields, err := client.ScanForm(ctx)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "form_failed", err.Error(), nil)
		return
	}
	host := currentBrowserHost(ctx, client)
	kinds := map[string]int{}
	for _, f := range fields {
		kinds[f.Kind]++
	}
	jsonResp(w, map[string]any{
		"host":     host,
		"fields":   fields,
		"kinds":    kinds,
		"logins":   vault.List(host),
		"profile":  vault.Profile(),
		"has_form": len(fields) > 0,
		// Регистрация узнаётся по второму полю пароля: там нужен не
		// сохранённый вход, а новый пароль и анкета.
		"signup": kinds["password2"] > 0,
	})
}

// POST /api/vbrowser/fill — заполнить форму.
//
//	{"login_id": "abc"}                  — сохранённым входом
//	{"profile": true, "new_password": 20} — анкетой и новым паролем
//	{"values": {"email": "..."}}          — конкретными значениями
//
// Ответ содержит НОВЫЙ пароль, если он был сгенерирован: человек должен видеть,
// что именно попало в поле, и решить, сохранять ли.
func (s *Server) apiBrowserFill(w http.ResponseWriter, r *http.Request, _ int64) {
	client := controlCDPClient()
	if client == nil {
		jsonErrorCode(w, http.StatusConflict, "no_browser", "Браузер не на связи.", nil)
		return
	}
	var req struct {
		LoginID     string            `json:"login_id"`
		Profile     bool              `json:"profile"`
		NewPassword int               `json:"new_password"`
		Values      map[string]string `json:"values"`
		Submit      bool              `json:"submit"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, http.StatusBadRequest, "bad_request", "bad request", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	fields, err := client.ScanForm(ctx)
	if err != nil {
		jsonErrorCode(w, http.StatusBadGateway, "form_failed", err.Error(), nil)
		return
	}
	if len(fields) == 0 {
		jsonErrorCode(w, http.StatusConflict, "no_form", "На странице нет полей для заполнения.", nil)
		return
	}

	values := map[string]string{}
	for kind, value := range req.Values {
		values[cdp.NormalizeFieldKind(kind)] = value
	}
	if req.Profile {
		p := vault.Profile()
		fillFromProfile(values, p)
	}
	generated := ""
	if req.NewPassword > 0 {
		pass, err := generatePassword(req.NewPassword)
		if err != nil {
			jsonErrorCode(w, http.StatusInternalServerError, "password_failed", err.Error(), nil)
			return
		}
		generated = pass
		values["password"] = pass
		values["password2"] = pass
	}
	if req.LoginID != "" {
		rec, ok := vault.Secret(req.LoginID)
		if !ok {
			jsonErrorCode(w, http.StatusNotFound, "not_found", "Такой записи нет.", nil)
			return
		}
		if rec.Login != "" {
			values["login"] = rec.Login
			if strings.Contains(rec.Login, "@") {
				values["email"] = rec.Login
			}
		}
		values["password"] = rec.Password
	}
	if len(values) == 0 {
		jsonErrorCode(w, http.StatusBadRequest, "nothing_to_fill", "Нечем заполнять.", nil)
		return
	}

	filled := 0
	skipped := []string{}
	for _, field := range fields {
		value, ok := valueForField(values, field)
		if !ok || value == "" {
			continue
		}
		if field.Type == "select-one" || field.Type == "select" {
			if err := client.SelectOption(ctx, field.Index, value); err != nil {
				skipped = append(skipped, field.Kind)
				continue
			}
			filled++
			continue
		}
		if err := client.FillField(ctx, field.Index, value); err != nil {
			skipped = append(skipped, field.Kind)
			continue
		}
		filled++
	}
	if filled == 0 {
		jsonErrorCode(w, http.StatusConflict, "nothing_filled",
			"Подходящих полей на странице не нашлось.", nil)
		return
	}
	if req.Submit {
		if err := client.SubmitForm(ctx); err != nil {
			log.Printf("[VBROWSER] отправка формы: %v", err)
		}
	}
	vbrowser.NoteActivity()
	resp := map[string]any{"ok": true, "filled": filled}
	if generated != "" {
		// Пароль возвращается ОДИН раз и только тому, кто его сейчас создал:
		// человек должен увидеть, что попало в поле, до того как решит сохранять.
		resp["password"] = generated
	}
	if len(skipped) > 0 {
		resp["skipped"] = skipped
	}
	jsonResp(w, resp)
}

// fillFromProfile раскладывает анкету по видам полей.
func fillFromProfile(values map[string]string, p BrowserProfile) {
	set := func(kind, value string) {
		if value == "" {
			return
		}
		if _, exists := values[kind]; !exists {
			values[kind] = value
		}
	}
	set("email", p.Email)
	set("phone", p.Phone)
	set("first", p.FirstName)
	set("last", p.LastName)
	set("birthday", p.Birthday)
	set("city", p.City)
	set("address", p.Address)
	set("zip", p.ZIP)
	set("country", p.Country)
	if p.FirstName != "" || p.LastName != "" {
		set("name", strings.TrimSpace(p.FirstName+" "+p.LastName))
	}
	// Логин, о котором сайт не сказал ничего конкретного, чаще всего почта.
	set("login", p.Email)
}

// valueForField выбирает значение под конкретное поле. Заполненные поля не
// трогаем без нужды: человек мог уже что-то ввести сам, и затирать это молча
// нельзя (единственное исключение — пароль, он всегда наш).
func valueForField(values map[string]string, field cdp.FormField) (string, bool) {
	value, ok := values[field.Kind]
	if !ok {
		return "", false
	}
	if !field.Empty && field.Kind != "password" && field.Kind != "password2" {
		return "", false
	}
	return value, true
}
