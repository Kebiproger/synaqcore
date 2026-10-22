package clickhouse

import "testing"

func TestErrorCode(t *testing.T) {
	cases := []struct {
		name   string
		header string
		body   string
		want   string
	}{
		{"заголовок", "27", "Code: 241. DB::Exception", "27"},
		{"заголовок с пробелами", " 62 ", "", "62"},
		{"префикс тела", "", "Code: 27. DB::Exception: Cannot parse input: user@example.com", "27"},
		{"тело без кода", "", "DB::Exception: user@example.com\nподделанная строка лога", "неизвестен"},
		{"код без цифр", "", "Code: abc", "неизвестен"},
		{"нецифровой заголовок", "x\n1", "Code: 9.", "9"},
		{"длинный код обрезается", "", "Code: 12345678901", "123456"},
		{"пусто", "", "", "неизвестен"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := errorCode(c.header, []byte(c.body)); got != c.want {
				t.Errorf("errorCode(%q, %q) = %q, want %q", c.header, c.body, got, c.want)
			}
		})
	}
}
