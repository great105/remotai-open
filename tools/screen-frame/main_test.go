package main

import (
	"reflect"
	"testing"

	"tgcontrol/internal/pty"
)

// -resizes стенда эквивалентности (§6): формат off:COLSxROWS[:tail]; ошибка
// разбора — явная, а не молча выпавшая смена геометрии.
func TestParseResizes(t *testing.T) {
	got, err := parseResizes(" 0:10x4, 12:30x8:tail ,12:20x6")
	if err != nil {
		t.Fatal(err)
	}
	want := []pty.StreamResize{
		{Off: 0, Cols: 10, Rows: 4},
		{Off: 12, Cols: 30, Rows: 8, AfterCut: true},
		{Off: 12, Cols: 20, Rows: 6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("разбор %+v, ожидалось %+v", got, want)
	}
	if got, err := parseResizes(""); err != nil || got != nil {
		t.Fatalf("пустой список: %+v %v", got, err)
	}
	for _, bad := range []string{"12", "12:30", "12:30x", "x:30x8", "-1:30x8", "12:0x8", "12:30x8:head", "12:30x8:tail:x"} {
		if _, err := parseResizes(bad); err == nil {
			t.Fatalf("%q разобрано без ошибки", bad)
		}
	}
}
