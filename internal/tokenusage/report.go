package tokenusage

import (
	"sort"
	"time"
)

// TokensOut — счётчики с готовым итогом для клиента.
type TokensOut struct {
	Tokens
	Total int64 `json:"total"`
}

func out(t Tokens) TokensOut { return TokensOut{Tokens: t, Total: t.Total()} }

type DayRow struct {
	Day    string    `json:"day"`
	Tokens TokensOut `json:"tokens"`
}

type NamedRow struct {
	Key      string    `json:"key"`
	Provider string    `json:"provider,omitempty"`
	Label    string    `json:"label,omitempty"`
	Sessions int       `json:"sessions,omitempty"`
	Tokens   TokensOut `json:"tokens"`
}

type SessionRow struct {
	Session  string    `json:"session"`
	Provider string    `json:"provider"`
	Account  string    `json:"account"`
	Project  string    `json:"project"`
	Model    string    `json:"model"`
	Last     int64     `json:"last"`
	Tokens   TokensOut `json:"tokens"`
}

// Report — ответ /api/token-usage.
type Report struct {
	Days      int          `json:"days"`
	From      string       `json:"from"`
	To        string       `json:"to"`
	Progress  Progress     `json:"progress"`
	Total     TokensOut    `json:"total"`
	ByDay     []DayRow     `json:"by_day"`
	ByProject []NamedRow   `json:"by_project"`
	ByModel   []NamedRow   `json:"by_model"`
	ByAccount []NamedRow   `json:"by_account"`
	Top       []SessionRow `json:"top_sessions"`
}

const (
	topProjects = 20
	topSessions = 15
)

// Report собирает итоги за последние days дней, включая сегодня.
func (t *Tracker) Report(days int, now time.Time) Report {
	if days < 1 {
		days = 1
	}
	if days > RetentionDays {
		days = RetentionDays
	}
	today := now.Local()
	from := today.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	to := today.Format("2006-01-02")

	labels := map[string]string{}
	for _, s := range sources() {
		labels[s.Provider+"|"+s.AccountID] = s.Label
	}

	t.mu.Lock()
	rep := Report{Days: days, From: from, To: to, Progress: t.prog}
	var total Tokens
	byDay := map[string]*Tokens{}
	type agg struct {
		t        Tokens
		provider string
		sessions map[string]bool
	}
	byProject := map[string]*agg{}
	byModel := map[string]*agg{}
	byAccount := map[string]*agg{}
	sessions := map[string]*SessionRow{}
	sessionModels := map[string]map[string]int64{}
	get := func(m map[string]*agg, k, provider string) *agg {
		a := m[k]
		if a == nil {
			a = &agg{provider: provider, sessions: map[string]bool{}}
			m[k] = a
		}
		return a
	}
	for _, r := range t.ix.Rows {
		if r.Day < from || r.Day > to {
			continue
		}
		total.add(r.Tokens)
		d := byDay[r.Day]
		if d == nil {
			d = &Tokens{}
			byDay[r.Day] = d
		}
		d.add(r.Tokens)
		for _, g := range []struct {
			m map[string]*agg
			k string
		}{{byProject, r.Project}, {byModel, r.Model}, {byAccount, r.Provider + "|" + r.Account}} {
			a := get(g.m, g.k, r.Provider)
			a.t.add(r.Tokens)
			a.sessions[r.Session] = true
		}
		sk := r.Provider + "|" + r.Session
		s := sessions[sk]
		if s == nil {
			s = &SessionRow{Session: r.Session, Provider: r.Provider, Account: r.Account, Project: r.Project, Model: r.Model}
			sessions[sk] = s
		}
		s.Tokens.Tokens.add(r.Tokens)
		if r.Last > s.Last {
			s.Last = r.Last
		}
		// Модель сессии — та, что потратила больше (сабагенты ходят на других).
		mt := sessionModels[sk]
		if mt == nil {
			mt = map[string]int64{}
			sessionModels[sk] = mt
		}
		mt[r.Model] += r.Tokens.Total()
	}
	t.mu.Unlock()
	for sk, mt := range sessionModels {
		best, bestN := "", int64(-1)
		for m, n := range mt {
			if n > bestN || (n == bestN && m < best) {
				best, bestN = m, n
			}
		}
		sessions[sk].Model = best
	}

	rep.Total = out(total)
	for d := today.AddDate(0, 0, -(days - 1)); !d.After(today); d = d.AddDate(0, 0, 1) {
		k := d.Format("2006-01-02")
		v := Tokens{}
		if byDay[k] != nil {
			v = *byDay[k]
		}
		rep.ByDay = append(rep.ByDay, DayRow{Day: k, Tokens: out(v)})
	}
	named := func(m map[string]*agg, limit int, label func(string) string) []NamedRow {
		rows := make([]NamedRow, 0, len(m))
		for k, a := range m {
			rows = append(rows, NamedRow{Key: k, Provider: a.provider, Label: label(k), Sessions: len(a.sessions), Tokens: out(a.t)})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Tokens.Total != rows[j].Tokens.Total {
				return rows[i].Tokens.Total > rows[j].Tokens.Total
			}
			return rows[i].Key < rows[j].Key
		})
		if limit > 0 && len(rows) > limit {
			rows = rows[:limit]
		}
		return rows
	}
	none := func(string) string { return "" }
	rep.ByProject = named(byProject, topProjects, none)
	rep.ByModel = named(byModel, 0, none)
	rep.ByAccount = named(byAccount, 0, func(k string) string { return labels[k] })
	for _, s := range sessions {
		s.Tokens.Total = s.Tokens.Tokens.Total()
		rep.Top = append(rep.Top, *s)
	}
	sort.Slice(rep.Top, func(i, j int) bool {
		if rep.Top[i].Tokens.Total != rep.Top[j].Tokens.Total {
			return rep.Top[i].Tokens.Total > rep.Top[j].Tokens.Total
		}
		return rep.Top[i].Session < rep.Top[j].Session
	})
	if len(rep.Top) > topSessions {
		rep.Top = rep.Top[:topSessions]
	}
	return rep
}
