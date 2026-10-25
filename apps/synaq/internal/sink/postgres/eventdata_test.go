package postgres

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeEventData(t *testing.T) {
	cases := []struct {
		name   string
		raw    string // "" — поле отсутствует
		want   string
		wantOK bool
	}{
		{"отсутствует", "", "", false},
		{"null", `null`, "", false},
		{"объект как есть", `{"amount":42}`, `{"amount":42}`, true},
		{"массив", `[1,2]`, `[1,2]`, true},
		{"число", `12.50`, `12.50`, true},
		{"bool", `true`, `true`, true},
		{"строка с JSON-объектом", `"{\"amount\":42}"`, `{"amount":42}`, true},
		{"строка с JSON-числом", `"42"`, `42`, true},
		{"строка с null", `"null"`, "", false},
		{"обычная строка", `"hello"`, `"hello"`, true},
		{"пустая строка", `""`, `""`, true},
		{"битый JSON в строке", `"{amount:"`, `"{amount:"`, true},
		{"NUL в строке", `"a\u0000b"`, `"ab"`, true},
		{"NUL в объекте", `{"k\u0000":"v\u0000","n":1}`, `{"k":"v","n":1}`, true},
		{"NUL в JSON внутри строки", `"{\"k\":\"a\\u0000b\"}"`, `{"k":"ab"}`, true},
		{"экранированный обратный слэш не трогается", `{"k":"\\u0000"}`, `{"k":"\\u0000"}`, true},
		{"большое число дословно", `{"n":12345678901234567890.5}`, `{"n":12345678901234567890.5}`, true},
		{"непарный суррогат", `{"k":"\ud800x","n":1}`, `{"k":"�x","n":1}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			got, ok := normalizeEventData(raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("результат не JSON: %q", got)
			}
			// Сравнение по смыслу: перекодированные объекты могут отличаться форматированием.
			if !jsonEqual(t, got, tc.want) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Fatalf("unmarshal %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	// Числа сравниваются отдельно дословно, чтобы поймать потерю точности.
	return string(ja) == string(jb) && sameNumbers(a, b)
}

// sameNumbers — грубая проверка: длинное число из want присутствует в got дословно.
func sameNumbers(got, want string) bool {
	const big = "12345678901234567890.5"
	if strings.Contains(want, big) {
		return strings.Contains(got, big)
	}
	return true
}

func TestDecodeRows(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"event_id":"e\u00001","user_id":"u","event_name":"purchase","timestamp":1700000000123,"experiment_name":"exp","variant":"B","event_data":{"amount":1}}`),
		[]byte(`не json`),
		[]byte(`{"event_id":"e2","user_id":"u","event_name":"view","timestamp":1700000000123,"experiment_name":"","variant":"","event_data":null}`),
	}
	rows, bad := decodeRows(payloads)
	if bad != 1 || len(rows) != 2 {
		t.Fatalf("rows=%d bad=%d, want 2 и 1", len(rows), bad)
	}
	if rows[0].eventID != "e1" {
		t.Errorf("eventID = %q, NUL не удалён", rows[0].eventID)
	}
	if rows[0].ts.Location().String() != "UTC" || rows[0].ts.UnixMilli() != 1700000000123 {
		t.Errorf("ts = %v", rows[0].ts)
	}
	if rows[0].eventData == nil || *rows[0].eventData != `{"amount":1}` {
		t.Errorf("eventData = %v", rows[0].eventData)
	}
	if rows[1].eventData != nil {
		t.Errorf("eventData null → %q, want NULL", *rows[1].eventData)
	}
}
