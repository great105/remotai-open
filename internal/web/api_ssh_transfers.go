package web

// HTTP-ручки переносов ПК ↔ SSH-сервер (#9). Логика заданий — в
// ssh_transfers.go; здесь только разбор запроса, открытие концов и запуск.
//
// Обе ручки отвечают 202 + id СРАЗУ: копирование идёт горутиной на ПК и
// переживает закрытие приложения. Прогресс — событием transfer и через
// GET /api/ssh/sftp/transfers.

import (
	"context"
	"io"
	"net/http"
	"os"
	urlpath "path"
	"path/filepath"
)

// POST /api/ssh/sftp/push {host,port,user,password,trust_host,local,path}
// — отправить файл С ПК на сервер. local — файл на ПК, path — папка назначения.
func (s *Server) apiSFTPPush(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		sftpBody
		Local string `json:"local"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	if body.Local == "" || body.Path == "" {
		jsonErrorCode(w, 400, "bad_request", "local and path are required", nil)
		return
	}
	local, err := validatePath(body.Local)
	if err != nil {
		jsonErrorCode(w, 403, "outside_roots", err.Error(), nil)
		return
	}
	f, err := os.Open(local)
	if err != nil {
		code, status := localFSErrCode(err)
		jsonErrorCode(w, status, code, "failed to open local file: "+err.Error(), nil)
		return
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		f.Close()
		// Папку целиком не переносим: это рекурсия с отдельной семантикой
		// ошибок и прогресса, а не «тот же перенос, но много раз».
		jsonErrorCode(w, 400, "not_a_file", "only files can be transferred", nil)
		return
	}

	p := body.params()
	c, release, ok := s.sftpLease(w, p)
	if !ok {
		f.Close()
		return
	}
	name := filepath.Base(local)
	remote := urlpath.Join(body.Path, name)
	j, err := s.sshTransfers.start(uid, "push", name, p.label(), remote, local, st.Size())
	if err != nil {
		f.Close()
		release()
		jsonErrorCode(w, 429, "too_many_transfers", "too many active transfers", nil)
		return
	}
	// Дескрипторы под отменой: без этого горутина висит в сетевом чтении до
	// TCP-таймаута, хотя «Отмена» уже ответила ok.
	j.addClosers(f)

	s.sshTransfers.run(j, func(ctx context.Context) error {
		src := &transferProgressReader{r: f, size: st.Size(), ctx: ctx, j: j, m: s.sshTransfers}
		return s.sftpUploadOne(c, p, body.Path, name, src, st.Size())
	}, func(failed bool) {
		f.Close()
		release()
	})
	sshJSONStatus(w, 202, map[string]any{"ok": true, "id": j.id, "transfer": j.snapshot()})
}

// POST /api/ssh/sftp/pull {host,port,user,password,trust_host,path,local}
// — забрать файл С СЕРВЕРА на ПК. path — файл на сервере, local — папка на ПК.
func (s *Server) apiSFTPPull(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		sftpBody
		Local string `json:"local"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	if body.Local == "" || body.Path == "" {
		jsonErrorCode(w, 400, "bad_request", "local and path are required", nil)
		return
	}
	dir, err := validatePath(body.Local)
	if err != nil {
		jsonErrorCode(w, 403, "outside_roots", err.Error(), nil)
		return
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		jsonErrorCode(w, 400, "not_a_dir", "local must be an existing directory", nil)
		return
	}

	p := body.params()
	c, release, ok := s.sftpLease(w, p)
	if !ok {
		return
	}
	src, err := c.Open(body.Path)
	if err != nil {
		release()
		writeSFTPErr(w, err, "sftp open")
		return
	}
	total := int64(0)
	if st, err := src.Stat(); err == nil {
		if st.IsDir() {
			src.Close()
			release()
			jsonErrorCode(w, 400, "not_a_file", "only files can be transferred", nil)
			return
		}
		total = st.Size()
	}

	name := sanitizeRemoteName(urlpath.Base(body.Path))
	if name == "" {
		src.Close()
		release()
		jsonErrorCode(w, 400, "bad_filename", "bad filename", nil)
		return
	}
	dstPath := filepath.Join(dir, name)
	dst, err := os.Create(dstPath)
	if err != nil {
		src.Close()
		release()
		code, status := localFSErrCode(err)
		jsonErrorCode(w, status, code, "failed to create local file: "+err.Error(), nil)
		return
	}

	j, err := s.sshTransfers.start(uid, "pull", name, p.label(), body.Path, dstPath, total)
	if err != nil {
		src.Close()
		dst.Close()
		_ = os.Remove(dstPath)
		release()
		jsonErrorCode(w, 429, "too_many_transfers", "too many active transfers", nil)
		return
	}
	j.addClosers(src, dst)

	s.sshTransfers.run(j, func(ctx context.Context) error {
		// Обёртка на ПРИЁМНИКЕ: io.Copy увидит WriterTo у sftp.File и поедет
		// конкурентными окнами, а не по 32 КБ за круговую задержку.
		pw := &transferProgressWriter{w: dst, ctx: ctx, j: j, m: s.sshTransfers}
		_, err := io.Copy(pw, src)
		return err
	}, func(failed bool) {
		src.Close()
		dst.Close()
		release()
		if failed {
			// Недописанный файл на ПК хуже отсутствующего: его легко принять
			// за целый дамп.
			_ = os.Remove(dstPath)
		}
	})
	sshJSONStatus(w, 202, map[string]any{"ok": true, "id": j.id, "transfer": j.snapshot()})
}

// GET /api/ssh/sftp/transfers — активные и недавно завершённые переносы.
func (s *Server) apiSFTPTransfers(w http.ResponseWriter, r *http.Request, uid int64) {
	jsonResp(w, map[string]any{"transfers": s.sshTransfers.list(uid)})
}

// POST /api/ssh/sftp/transfers/cancel {id} — прервать перенос.
func (s *Server) apiSFTPTransferCancel(w http.ResponseWriter, r *http.Request, uid int64) {
	var body struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &body); err != nil || body.ID == "" {
		jsonErrorCode(w, 400, "bad_request", "id is required", nil)
		return
	}
	if !s.sshTransfers.cancel(uid, body.ID) {
		jsonErrorCode(w, 404, "not_found", "transfer not found", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}
