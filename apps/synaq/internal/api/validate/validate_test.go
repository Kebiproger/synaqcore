package validate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const ctJSON = "application/json"

// short — упрощённая ошибка для сравнения: type + loc (+ msg, если задан).
type short struct {
	Type string
	Loc  string
	Msg  string
}

func locString(l []any) string {
	parts := make([]string, len(l))
	for i, p := range l {
		b, _ := json.Marshal(p)
		parts[i] = string(b)
	}
	return strings.Join(parts, ",")
}

func check(t *testing.T, verr *Error, wantStatus int, want []short) {
	t.Helper()
	if wantStatus == 0 {
		if verr != nil {
			t.Fatalf("ожидался успех, получено %v", verr)
		}
		return
	}
	if verr == nil {
		t.Fatalf("ожидалась ошибка %d, получен успех", wantStatus)
	}
	if verr.Status != wantStatus {
		t.Fatalf("status = %d, want %d (%v)", verr.Status, wantStatus, verr.Detail)
	}
	if want == nil {
		return
	}
	items, ok := verr.Detail.([]ErrorItem)
	if !ok {
		t.Fatalf("detail не []ErrorItem: %#v", verr.Detail)
	}
	if len(items) != len(want) {
		t.Fatalf("ошибок %d, want %d: %+v", len(items), len(want), items)
	}
	for i, w := range want {
		got := items[i]
		if got.Type != w.Type || locString(got.Loc) != w.Loc || (w.Msg != "" && got.Msg != w.Msg) {
			t.Errorf("ошибка %d = {%s %s %q}, want %+v", i, got.Type, locString(got.Loc), got.Msg, w)
		}
	}
}

func TestExperimentCreate(t *testing.T) {
	tests := []struct {
		name   string
		ct     string
		body   string
		status int
		want   []short
	}{
		{"valid", ctJSON, `{"name":"exp.1","variants":{"A":50,"B":50}}`, 0, nil},
		{"empty body", ctJSON, ``, 422, []short{{"missing", `"body"`, "Field required"}}},
		{"null body", ctJSON, `null`, 422, []short{{"missing", `"body"`, ""}}},
		{"not json content type", "text/plain", `{"name":"x"}`, 422, []short{{"model_attributes_type", `"body"`, ""}}},
		{"malformed json", ctJSON, `{"name":`, 422, []short{{"json_invalid", `"body",8`, "JSON decode error"}}},
		{"trailing data", ctJSON, `{} x`, 422, []short{{"json_invalid", `"body",3`, ""}}},
		{"NaN rejected", ctJSON, `{"name":"a","variants":{"A":NaN}}`, 422, nil},
		{"invalid utf8", ctJSON, "{\"name\":\"\xff\"}", 400, nil},
		{"array body", ctJSON, `[1]`, 422, []short{{"model_attributes_type", `"body"`, ""}}},
		{"missing fields in order", ctJSON, `{}`, 422, []short{
			{"missing", `"body","name"`, "Field required"},
			{"missing", `"body","variants"`, "Field required"},
		}},
		{"name type", ctJSON, `{"name":5,"variants":{"A":1}}`, 422, []short{{"string_type", `"body","name"`, "Input should be a valid string"}}},
		{"name empty", ctJSON, `{"name":"","variants":{"A":1}}`, 422, []short{{"string_too_short", `"body","name"`, "String should have at least 1 character"}}},
		{"name too long", ctJSON, `{"name":"` + strings.Repeat("a", 51) + `","variants":{"A":1}}`, 422, []short{{"string_too_long", `"body","name"`, "String should have at most 50 characters"}}},
		{"name pattern", ctJSON, `{"name":"bad name","variants":{"A":1}}`, 422, []short{{"string_pattern_mismatch", `"body","name"`, "String should match pattern '^[A-Za-z0-9_.-]+$'"}}},
		{"name reserved", ctJSON, `{"name":"Eval-Configs","variants":{"A":1}}`, 422, []short{{"value_error", `"body","name"`, "Value error, Имя 'Eval-Configs' зарезервировано"}}},
		{"variants type", ctJSON, `{"name":"a","variants":[1]}`, 422, []short{{"dict_type", `"body","variants"`, "Input should be a valid dictionary"}}},
		{"variants empty", ctJSON, `{"name":"a","variants":{}}`, 422, []short{{"too_short", `"body","variants"`, "Dictionary should have at least 1 item after validation, not 0"}}},
		{"variants too many", ctJSON, `{"name":"a","variants":{` + manyVariants(21) + `}}`, 422, []short{{"too_long", `"body","variants"`, "Dictionary should have at most 20 items after validation, not 21"}}},
		{"variant key pattern", ctJSON, `{"name":"a","variants":{"A.B":1}}`, 422, []short{{"string_pattern_mismatch", `"body","variants","A.B","[key]"`, "String should match pattern '^[A-Za-z0-9_-]+$'"}}},
		{"weight zero", ctJSON, `{"name":"a","variants":{"A":0,"B":1}}`, 422, []short{{"value_error", `"body","variants"`, "Value error, Все веса должны быть положительными"}}},
		{"weight too big", ctJSON, `{"name":"a","variants":{"A":1000001}}`, 422, []short{{"value_error", `"body","variants"`, "Value error, Вес варианта не может превышать 1000000"}}},
		{"weight int64 overflow", ctJSON, `{"name":"a","variants":{"A":99999999999999999999}}`, 422, []short{{"value_error", `"body","variants"`, "Value error, Вес варианта не может превышать 1000000"}}},
		{"strict weight string", ctJSON, `{"name":"a","variants":{"A":"5"}}`, 422, []short{{"int_type", `"body","variants","A"`, "Input should be a valid integer"}}},
		{"strict weight float", ctJSON, `{"name":"a","variants":{"A":5.0}}`, 422, []short{{"int_type", `"body","variants","A"`, ""}}},
		{"strict weight bool", ctJSON, `{"name":"a","variants":{"A":true}}`, 422, []short{{"int_type", `"body","variants","A"`, ""}}},
		{"item errors suppress length", ctJSON, `{"name":"a","variants":{"":"x"}}`, 422, []short{
			{"string_too_short", `"body","variants","","[key]"`, ""},
			{"int_type", `"body","variants",""`, ""},
		}},
		{"config not object", ctJSON, `{"name":"a","variants":{"A":1},"config":[1]}`, 422, []short{{"dict_type", `"body","config"`, ""}}},
		{"url too long", ctJSON, `{"name":"a","variants":{"A":1},"url":"` + strings.Repeat("я", 201) + `"}`, 422, []short{{"string_too_long", `"body","url"`, "String should have at most 200 characters"}}},
		{"strict is_active", ctJSON, `{"name":"a","variants":{"A":1},"is_active":"yes"}`, 422, []short{{"bool_type", `"body","is_active"`, "Input should be a valid boolean"}}},
		{"is_active null", ctJSON, `{"name":"a","variants":{"A":1},"is_active":null}`, 422, []short{{"bool_type", `"body","is_active"`, ""}}},
		{"errors in declaration order", ctJSON, `{"is_active":1,"url":5,"config":5,"variants":5,"name":5}`, 422, []short{
			{"string_type", `"body","name"`, ""},
			{"dict_type", `"body","variants"`, ""},
			{"dict_type", `"body","config"`, ""},
			{"string_type", `"body","url"`, ""},
			{"bool_type", `"body","is_active"`, ""},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, verr := ExperimentCreate(tt.ct, []byte(tt.body))
			check(t, verr, tt.status, tt.want)
		})
	}
}

func manyVariants(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = `"v` + strings.Repeat("x", i) + `":1`
	}
	return strings.Join(parts, ",")
}

func TestExperimentCreateParsed(t *testing.T) {
	body := `{"name":"exp","variants":{"A":1,"B":2,"A":3},"config":{ "A": {"c": 1} },"url":"/p","is_active":false,"extra":1}`
	exp, verr := ExperimentCreate("application/vnd.synaq+json; charset=utf-8", []byte(body))
	check(t, verr, 0, nil)
	if exp.Name != "exp" || !reflect.DeepEqual(exp.Variants, map[string]int64{"A": 3, "B": 2}) ||
		string(exp.Config) != `{"A":{"c":1}}` || exp.URL == nil || *exp.URL != "/p" || exp.IsActive {
		t.Fatalf("неверный разбор: %+v", exp)
	}

	exp, verr = ExperimentCreate(ctJSON, []byte("\xef\xbb\xbf"+`{"name":"e","variants":{"A":1},"config":null}`))
	check(t, verr, 0, nil)
	if !exp.IsActive || exp.Config != nil || exp.URL != nil {
		t.Fatalf("неверные значения по умолчанию: %+v", exp)
	}
}

func TestExperimentUpdate(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		want   []short
	}{
		{"empty object is valid", `{}`, 0, nil},
		{"extra forbidden after fields", `{"foo":1,"url":5}`, 422, []short{
			{"string_type", `"body","url"`, ""},
			{"extra_forbidden", `"body","foo"`, "Extra inputs are not permitted"},
		}},
		{"is_active null", `{"is_active":null,"variants":null}`, 422, []short{{"value_error", `"body"`, "Value error, Поле 'is_active' не может быть null"}}},
		{"variants null", `{"variants":null}`, 422, []short{{"value_error", `"body"`, "Value error, Поле 'variants' не может быть null"}}},
		{"field errors suppress model validator", `{"is_active":null,"url":1}`, 422, []short{{"string_type", `"body","url"`, ""}}},
		{"strict is_active", `{"is_active":1}`, 422, []short{{"bool_type", `"body","is_active"`, ""}}},
		{"weights", `{"variants":{"A":-1}}`, 422, []short{{"value_error", `"body","variants"`, "Value error, Все веса должны быть положительными"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, verr := ExperimentUpdate(ctJSON, []byte(tt.body))
			check(t, verr, tt.status, tt.want)
		})
	}
}

func TestExperimentUpdateParsed(t *testing.T) {
	p, verr := ExperimentUpdate(ctJSON, []byte(`{}`))
	check(t, verr, 0, nil)
	if !p.Empty() {
		t.Fatalf("пустой патч: %+v", p)
	}

	p, verr = ExperimentUpdate(ctJSON, []byte(`{"config":null,"url":null}`))
	check(t, verr, 0, nil)
	if !p.ConfigSet || p.Config != nil || !p.URLSet || p.URL != nil || p.IsActive != nil || p.Variants != nil {
		t.Fatalf("null должен очищать config/url: %+v", p)
	}

	p, verr = ExperimentUpdate(ctJSON, []byte(`{"is_active":false,"variants":{"A":1},"config":{"x":1},"url":"/u"}`))
	check(t, verr, 0, nil)
	if p.IsActive == nil || *p.IsActive || p.Variants == nil || (*p.Variants)["A"] != 1 ||
		p.Config == nil || string(*p.Config) != `{"x":1}` || p.URL == nil || *p.URL != "/u" {
		t.Fatalf("неверный разбор: %+v", p)
	}
}

func TestBatch(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		want   []short
	}{
		{"valid with reserved name", `{"user_id":"u","experiments":["a","config"],"x":1}`, 0, nil},
		{"missing", `{}`, 422, []short{{"missing", `"body","user_id"`, ""}, {"missing", `"body","experiments"`, ""}}},
		{"empty user", `{"user_id":"","experiments":["a"]}`, 422, []short{{"string_too_short", `"body","user_id"`, ""}}},
		{"not a list", `{"user_id":"u","experiments":"a"}`, 422, []short{{"list_type", `"body","experiments"`, "Input should be a valid list"}}},
		{"empty list", `{"user_id":"u","experiments":[]}`, 422, []short{{"too_short", `"body","experiments"`, "List should have at least 1 item after validation, not 0"}}},
		{"too long wins over items", `{"user_id":"u","experiments":[` + strings.Repeat(`"",`, 50) + `""]}`, 422, []short{{"too_long", `"body","experiments"`, "List should have at most 50 items after validation, not 51"}}},
		{"item errors by index", `{"user_id":"u","experiments":["ok",1,"b c"]}`, 422, []short{
			{"string_type", `"body","experiments",1`, ""},
			{"string_pattern_mismatch", `"body","experiments",2`, ""},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, verr := Batch(ctJSON, []byte(tt.body))
			check(t, verr, tt.status, tt.want)
		})
	}

	req, verr := Batch(ctJSON, []byte(`{"user_id":"u1","experiments":["a","a"]}`))
	check(t, verr, 0, nil)
	if req.UserID != "u1" || !reflect.DeepEqual(req.Experiments, []string{"a", "a"}) {
		t.Fatalf("неверный разбор: %+v", req)
	}
}

func TestErrorJSONShape(t *testing.T) {
	_, verr := ExperimentCreate(ctJSON, []byte(`{"name":"active","variants":{"A":1}}`))
	b, err := json.Marshal(map[string]any{"detail": verr.Detail})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"detail":[{"type":"value_error","loc":["body","name"],"msg":"Value error, Имя 'active' зарезервировано","input":"active"}]}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}

	_, verr = ExperimentCreate(ctJSON, []byte(`{}`))
	b, _ = json.Marshal(verr.Detail)
	if strings.Contains(string(b), `"input"`) {
		t.Fatalf("missing не должен содержать input: %s", b)
	}
}
