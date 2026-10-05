// screen-frame — прогоняет запись сырого потока PTY через зеркало экрана и
// печатает получившийся кадр. Нужен для сквозной проверки: тот же поток
// скармливается настоящему xterm.js, и обе картинки сравниваются ПО ЯЧЕЙКАМ
// (build/qa/probe-screen-frame.mjs).
//
// Своего эмулятора у нас два: Go на компьютере и xterm.js в телефоне. Пока они
// сходятся ячейка в ячейку, кадр можно отдавать клиенту; разойдутся — картинка
// поедет, и лучше узнать об этом от пробы, чем от скриншота владельца.
//
//	go run ./tools/screen-frame -in dump.log -cols 48 -rows 30
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"tgcontrol/internal/pty"
)

// parseInts разбирает список «7,13,…»; пустая строка — пустой список.
func parseInts(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// parseResizes разбирает список смен геометрии для -snapshots-json:
// «off:COLSxROWS[:tail],…». :tail — при разрезе ровно на off снимок снят ДО
// resize (он первое событие хвоста); без него — после (pty.StreamResize).
func parseResizes(s string) ([]pty.StreamResize, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []pty.StreamResize
	for _, item := range strings.Split(s, ",") {
		f := strings.Split(strings.TrimSpace(item), ":")
		if len(f) < 2 || len(f) > 3 || (len(f) == 3 && f[2] != "tail") {
			return nil, fmt.Errorf("%q: ожидалось off:COLSxROWS[:tail]", item)
		}
		off, err := strconv.Atoi(f[0])
		if err != nil || off < 0 {
			return nil, fmt.Errorf("%q: позиция %q", item, f[0])
		}
		geom := strings.Split(f[1], "x")
		if len(geom) != 2 {
			return nil, fmt.Errorf("%q: геометрия %q, ожидалось COLSxROWS", item, f[1])
		}
		cols, err1 := strconv.Atoi(geom[0])
		rows, err2 := strconv.Atoi(geom[1])
		if err1 != nil || err2 != nil || cols <= 0 || rows <= 0 {
			return nil, fmt.Errorf("%q: геометрия %q", item, f[1])
		}
		out = append(out, pty.StreamResize{Off: off, Cols: cols, Rows: rows, AfterCut: len(f) == 3})
	}
	return out, nil
}

func main() {
	in := flag.String("in", "", "файл с записью сырого потока PTY")
	cols := flag.Int("cols", 80, "ширина")
	rows := flag.Int("rows", 24, "высота")
	chunk := flag.Int("chunk", 0, "резать поток на куски по N байт (0 — целиком): так его режет ConPTY")
	grid := flag.Bool("grid", false, "печатать СЕТКУ зеркала, а не кадр: так видно, расходится сам эмулятор или сборщик кадра")
	gridJSON := flag.Bool("grid-json", false, "печатать точную JSON-сетку: один элемент на ячейку, включая продолжения широких графем")
	history := flag.Int("history", 0, "печатать ИСТОРИЮ зеркала (до N строк) вместо кадра — для сверки со scrollback xterm.js")
	steps := flag.Int("steps", 0, "печатать сетку после каждых N байт потока — для поиска точного места расхождения")
	snapshotsJSON := flag.Bool("snapshots-json", false, "печатать JSON-массив снимков SnapshotAt на разрезах -cuts при подаче кусками -chunks (путь B раздела 6 плана)")
	cutsFlag := flag.String("cuts", "", "разрезы для -snapshots-json: all (каждая позиция 0..len) или список a,b,c; пусто — один разрез в конце потока")
	chunksFlag := flag.String("chunks", "", "план кусков для -snapshots-json по кругу: 7,13,…; пусто — одним куском")
	idle := flag.Bool("idle", false, "для -snapshots-json: придержанная графема считается давно без продолжения (детерминированная замена 100 мс тишины)")
	maxHistory := flag.Int("max-history", -1, "для -snapshots-json: потолок строк scrollback в истории снимка; -1 — как у сессии")
	resizesFlag := flag.String("resizes", "", "для -snapshots-json: смены геометрии в потоке off:COLSxROWS[:tail],… — Resize зеркала в той же точке потока, что маркер ACK в продукте; :tail — при разрезе ровно на off снимок до resize")
	noVtGuards := flag.Bool("no-vt-guards", false, "для -snapshots-json: снять предохранители от известных паник vt — проверка страховочной сетки (снимок untrusted до RIS)")
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "нужен -in")
		os.Exit(2)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *snapshotsJSON {
		// Один процесс на фикстуру: перебор разрезов поштучными запусками шёл
		// бы минуты (замер карты 05: 75 запусков — 2,2 с).
		chunks, err := parseInts(*chunksFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "-chunks:", err)
			os.Exit(2)
		}
		var cuts []int
		switch *cutsFlag {
		case "":
			cuts = []int{len(data)}
		case "all":
			cuts = make([]int, 0, len(data)+1)
			for c := 0; c <= len(data); c++ {
				cuts = append(cuts, c)
			}
		default:
			if cuts, err = parseInts(*cutsFlag); err != nil {
				fmt.Fprintln(os.Stderr, "-cuts:", err)
				os.Exit(2)
			}
		}
		resizes, err := parseResizes(*resizesFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "-resizes:", err)
			os.Exit(2)
		}
		var opts []pty.StreamOption
		if *noVtGuards {
			opts = append(opts, pty.WithoutVtGuards())
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(pty.SnapshotsFromStreamResized(data, *cols, *rows, chunks, cuts, *maxHistory, *idle, resizes, opts...)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *steps > 0 {
		// Сетка после каждых N байт одним прогоном: так проба находит ТОЧНОЕ
		// место расхождения с xterm.js, не запуская go на каждый шаг.
		for end := *steps; ; end += *steps {
			if end > len(data) {
				end = len(data)
			}
			fmt.Printf("--- %d\n", end)
			for _, line := range pty.GridFromStream(data[:end], *cols, *rows, *chunk) {
				fmt.Println(line)
			}
			if end == len(data) {
				break
			}
		}
		return
	}
	if *grid {
		for _, line := range pty.GridFromStream(data, *cols, *rows, *chunk) {
			fmt.Println(line)
		}
		return
	}
	if *gridJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(pty.GridCellsFromStream(data, *cols, *rows, *chunk)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *history > 0 {
		text, _ := pty.HistoryFromStream(data, *cols, *rows, *chunk, *history)
		os.Stdout.WriteString(text)
		return
	}
	frame := pty.FrameFromStream(data, *cols, *rows, *chunk)
	os.Stdout.WriteString(frame)
}
