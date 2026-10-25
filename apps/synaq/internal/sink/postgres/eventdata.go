package postgres

import (
	"bytes"
	"encoding/json"
	"strings"
)

// normalizeEventData приводит event_data к тексту, который примет jsonb.
// ok == false означает SQL NULL.
//
//   - поле отсутствует или null → NULL;
//   - JSON-строка, содержимое которой — валидный JSON (SDK часто шлют
//     JSON.stringify(obj)), → само содержимое ("null" внутри строки → NULL);
//   - любая другая строка → JSON-строка (без символов NUL);
//   - объект, массив, число, bool → как есть.
//
// jsonb не принимает \u0000 (и непарные суррогаты): такие значения перекодируются
// с удалением NUL, остальное содержимое сохраняется.
func normalizeEventData(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	if raw[0] != '"' {
		return sanitizeJSON(raw), true
	}

	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Недостижимо для RawMessage из json.Unmarshal; на всякий случай — строкой.
		s = string(raw)
	}
	if inner := strings.TrimSpace(s); inner != "" && json.Valid([]byte(inner)) {
		if inner == "null" {
			return "", false
		}
		return sanitizeJSON([]byte(inner)), true
	}
	b, err := json.Marshal(stripNUL(s))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// needsSanitize — есть ли в JSON escape-последовательности, которые jsonb отвергнет:
// \u0000 и суррогаты (\uD800–\uDFFF; непарные Postgres не принимает).
func needsSanitize(b []byte) bool {
	return bytes.Contains(b, []byte(`\u0000`)) ||
		bytes.Contains(b, []byte(`\ud`)) || bytes.Contains(b, []byte(`\uD`))
}

// sanitizeJSON перекодирует валидный JSON, удаляя NUL из строк и ключей. Числа
// сохраняются дословно (UseNumber), непарные суррогаты декодер заменяет на U+FFFD.
// Если перекодировать не удалось, возвращается исходный текст (такую строку
// отбросит построчная вставка).
func sanitizeJSON(b []byte) string {
	if !needsSanitize(b) {
		return string(b)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(b)
	}
	out, err := json.Marshal(stripNULDeep(v))
	if err != nil {
		return string(b)
	}
	return string(out)
}

func stripNULDeep(v any) any {
	switch x := v.(type) {
	case string:
		return stripNUL(x)
	case []any:
		for i := range x {
			x[i] = stripNULDeep(x[i])
		}
		return x
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[stripNUL(k)] = stripNULDeep(val)
		}
		return m
	default:
		return v
	}
}

// stripNUL удаляет символы NUL: text и jsonb в PostgreSQL их не допускают.
func stripNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}
