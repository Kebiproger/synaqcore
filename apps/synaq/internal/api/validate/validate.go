package validate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"synaq/internal/model"
)

// Ограничения схем; совпадают с pydantic-схемами Python-версии experiment_api (тег v1-python).
const (
	NamePattern       = "^[A-Za-z0-9_.-]+$"
	VariantKeyPattern = "^[A-Za-z0-9_-]+$"
	MaxNameLen        = 50
	MaxVariantKeyLen  = 50
	MaxVariants       = 20
	MinVariantWeight  = 1
	MaxVariantWeight  = 1_000_000
	MaxURLLen         = 200
	MaxBatchSize      = 50
)

// Имена, совпадающие с системными путями /api/v1/variants/* (сравнение без учёта регистра).
var reservedNames = map[string]struct{}{
	"active": {}, "config": {}, "batch": {}, "eval-configs": {},
}

// ErrorItem — одна ошибка валидации в формате pydantic/FastAPI.
type ErrorItem struct {
	Type string `json:"type"`
	// Loc — путь до поля: строки (имена полей/ключи) и int (смещение в json_invalid).
	Loc []any  `json:"loc"`
	Msg string `json:"msg"`
	// Input — исходное JSON-значение поля; nil — не выводится.
	Input json.RawMessage `json:"input,omitempty"`
}

// Error — результат неуспешного разбора тела: HTTP-код и содержимое "detail"
// (для 422 — []ErrorItem, для 400 — строка). Обработчик отвечает
// apierr.Abort(c, err.Status, err.Detail).
type Error struct {
	Status int
	Detail any
}

func (e *Error) Error() string {
	return fmt.Sprintf("validate: %d %v", e.Status, e.Detail)
}

// BatchRequest — тело POST /api/v1/variants/batch.
type BatchRequest struct {
	UserID      string
	Experiments []string
}

func unprocessable(items []ErrorItem) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Detail: items}
}

func loc(parts ...any) []any { return append([]any{"body"}, parts...) }

func missing(field string) ErrorItem {
	return ErrorItem{Type: "missing", Loc: loc(field), Msg: "Field required"}
}

// field — пара ключ/значение JSON-объекта в порядке первого появления ключа
// (повтор ключа заменяет значение, как dict в Python).
type field struct {
	key string
	raw json.RawMessage
}

type object struct {
	fields []field
	index  map[string]int
}

func (o *object) get(key string) (json.RawMessage, bool) {
	i, ok := o.index[key]
	if !ok {
		return nil, false
	}
	return o.fields[i].raw, true
}

// parseObject разбирает JSON-объект потоково, сохраняя порядок ключей.
// ok == false, если raw — не объект.
func parseObject(raw json.RawMessage) (*object, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	o := &object{index: map[string]int{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		if i, dup := o.index[key]; dup {
			o.fields[i].raw = v
			continue
		}
		o.index[key] = len(o.fields)
		o.fields = append(o.fields, field{key: key, raw: v})
	}
	return o, true
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		// Python (email.message) берёт тип до ';' даже при битых параметрах.
		mt = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	}
	main, sub, ok := strings.Cut(mt, "/")
	if !ok || main != "application" {
		return false
	}
	return sub == "json" || strings.HasSuffix(sub, "+json")
}

// readBody проверяет Content-Type и синтаксис JSON и возвращает тело как объект.
func readBody(contentType string, body []byte) (*object, *Error) {
	if len(body) == 0 {
		return nil, unprocessable([]ErrorItem{{Type: "missing", Loc: loc(), Msg: "Field required"}})
	}
	notObject := func(input json.RawMessage) *Error {
		return unprocessable([]ErrorItem{{
			Type:  "model_attributes_type",
			Loc:   loc(),
			Msg:   "Input should be a valid dictionary or object to extract fields from",
			Input: input,
		}})
	}
	if !utf8.Valid(body) {
		return nil, &Error{Status: http.StatusBadRequest, Detail: "There was an error parsing the body"}
	}
	if !isJSONContentType(contentType) {
		// FastAPI не разбирает тело без JSON Content-Type и валидирует его как bytes.
		input, _ := json.Marshal(string(body))
		return nil, notObject(input)
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))

	dec := json.NewDecoder(bytes.NewReader(body))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, jsonInvalid(syntaxOffset(err, len(body)))
	}
	rest := bytes.TrimLeft(body[dec.InputOffset():], " \t\r\n")
	if len(rest) > 0 {
		return nil, jsonInvalid(len(body) - len(rest))
	}
	if string(raw) == "null" {
		return nil, unprocessable([]ErrorItem{{Type: "missing", Loc: loc(), Msg: "Field required"}})
	}
	o, ok := parseObject(raw)
	if !ok {
		return nil, notObject(raw)
	}
	return o, nil
}

func syntaxOffset(err error, n int) int {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return int(se.Offset)
	}
	if errors.Is(err, io.EOF) {
		return 0
	}
	return n
}

func jsonInvalid(offset int) *Error {
	return unprocessable([]ErrorItem{{
		Type: "json_invalid", Loc: loc(offset), Msg: "JSON decode error", Input: json.RawMessage("{}"),
	}})
}

// --- проверки отдельных значений ---

func isNull(raw json.RawMessage) bool { return string(raw) == "null" }

// checkString проверяет тип и длину строки (в символах, как len() в Python).
func checkString(raw json.RawMessage, l []any, minLen, maxLen int) (string, *ErrorItem) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", &ErrorItem{Type: "string_type", Loc: l, Msg: "Input should be a valid string", Input: raw}
	}
	n := utf8.RuneCountInString(s)
	if n < minLen {
		return s, &ErrorItem{Type: "string_too_short", Loc: l, Input: raw,
			Msg: fmt.Sprintf("String should have at least %d %s", minLen, plural(minLen, "character"))}
	}
	if maxLen > 0 && n > maxLen {
		return s, &ErrorItem{Type: "string_too_long", Loc: l, Input: raw,
			Msg: fmt.Sprintf("String should have at most %d %s", maxLen, plural(maxLen, "character"))}
	}
	return s, nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func matchCharset(s string, allowDot bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		case c == '.' && allowDot:
		default:
			return false
		}
	}
	return true
}

// IsValidName сообщает, подходит ли строка как имя эксперимента (без проверки
// зарезервированных имён).
func IsValidName(s string) bool {
	return utf8.RuneCountInString(s) <= MaxNameLen && matchCharset(s, true)
}

// IsReservedName сообщает, совпадает ли имя с системным путём (без учёта регистра).
func IsReservedName(s string) bool {
	_, ok := reservedNames[strings.ToLower(s)]
	return ok
}

func patternMismatch(l []any, raw json.RawMessage, pattern string) *ErrorItem {
	return &ErrorItem{Type: "string_pattern_mismatch", Loc: l, Input: raw,
		Msg: fmt.Sprintf("String should match pattern '%s'", pattern)}
}

// checkName — ExperimentName: строка 1..50 по NamePattern.
func checkName(raw json.RawMessage, l []any) (string, *ErrorItem) {
	s, e := checkString(raw, l, 1, MaxNameLen)
	if e != nil {
		return "", e
	}
	if !matchCharset(s, true) {
		return "", patternMismatch(l, raw, NamePattern)
	}
	return s, nil
}

func checkBool(raw json.RawMessage, l []any) (bool, *ErrorItem) {
	switch string(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, &ErrorItem{Type: "bool_type", Loc: l, Msg: "Input should be a valid boolean", Input: raw}
}

// weightClass: 0 — в пределах, -1 — меньше минимума, +1 — больше максимума.
func checkWeight(raw json.RawMessage, l []any) (int64, int, *ErrorItem) {
	s := string(raw)
	isInt := s != "" && (s[0] == '-' || (s[0] >= '0' && s[0] <= '9')) && !strings.ContainsAny(s, ".eE")
	if !isInt {
		return 0, 0, &ErrorItem{Type: "int_type", Loc: l, Msg: "Input should be a valid integer", Input: raw}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil { // переполнение int64: заведомо вне диапазона
		if s[0] == '-' {
			return 0, -1, nil
		}
		return 0, 1, nil
	}
	switch {
	case v < MinVariantWeight:
		return v, -1, nil
	case v > MaxVariantWeight:
		return v, 1, nil
	}
	return v, 0, nil
}

func lengthError(l []any, raw json.RawMessage, kind string, n, minLen, maxLen int) *ErrorItem {
	if n < minLen {
		return &ErrorItem{Type: "too_short", Loc: l, Input: raw,
			Msg: fmt.Sprintf("%s should have at least %d %s after validation, not %d", kind, minLen, plural(minLen, "item"), n)}
	}
	if n > maxLen {
		return &ErrorItem{Type: "too_long", Loc: l, Input: raw,
			Msg: fmt.Sprintf("%s should have at most %d %s after validation, not %d", kind, maxLen, plural(maxLen, "item"), n)}
	}
	return nil
}

// checkVariants — Dict[VariantKey, int] c 1..20 элементами и весами 1..1_000_000.
// Ошибки элементов — в порядке ключей; длина и веса проверяются, только если
// все элементы корректны (как в pydantic).
func checkVariants(raw json.RawMessage) (map[string]int64, []ErrorItem) {
	l := loc("variants")
	o, ok := parseObject(raw)
	if !ok {
		return nil, []ErrorItem{{Type: "dict_type", Loc: l, Msg: "Input should be a valid dictionary", Input: raw}}
	}
	var errs []ErrorItem
	out := make(map[string]int64, len(o.fields))
	low, high := false, false
	for _, f := range o.fields {
		keyRaw, _ := json.Marshal(f.key)
		keyLoc := loc("variants", f.key, "[key]")
		if _, e := checkString(keyRaw, keyLoc, 1, MaxVariantKeyLen); e != nil {
			errs = append(errs, *e)
		} else if !matchCharset(f.key, false) {
			errs = append(errs, *patternMismatch(keyLoc, keyRaw, VariantKeyPattern))
		}
		w, class, e := checkWeight(f.raw, loc("variants", f.key))
		if e != nil {
			errs = append(errs, *e)
			continue
		}
		low = low || class < 0
		high = high || class > 0
		out[f.key] = w
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if e := lengthError(l, raw, "Dictionary", len(o.fields), 1, MaxVariants); e != nil {
		return nil, []ErrorItem{*e}
	}
	if low {
		return nil, []ErrorItem{{Type: "value_error", Loc: l, Input: raw, Msg: "Value error, Все веса должны быть положительными"}}
	}
	if high {
		return nil, []ErrorItem{{Type: "value_error", Loc: l, Input: raw,
			Msg: fmt.Sprintf("Value error, Вес варианта не может превышать %d", MaxVariantWeight)}}
	}
	return out, nil
}

// checkConfig — объект или null (nil). Возвращает компактный JSON.
func checkConfig(raw json.RawMessage) (json.RawMessage, *ErrorItem) {
	if isNull(raw) {
		return nil, nil
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, &ErrorItem{Type: "dict_type", Loc: loc("config"), Msg: "Input should be a valid dictionary", Input: raw}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, &ErrorItem{Type: "dict_type", Loc: loc("config"), Msg: "Input should be a valid dictionary", Input: raw}
	}
	return json.RawMessage(buf.Bytes()), nil
}

// checkURL — строка до 200 символов или null (nil).
func checkURL(raw json.RawMessage) (*string, *ErrorItem) {
	if isNull(raw) {
		return nil, nil
	}
	s, e := checkString(raw, loc("url"), 0, MaxURLLen)
	if e != nil {
		return nil, e
	}
	return &s, nil
}

// --- модели ---

// ExperimentCreate разбирает тело POST /api/v1/experiments/ (ExperimentConfigCreate).
// contentType — сырой заголовок Content-Type. Лишние поля игнорируются, is_active
// по умолчанию true.
func ExperimentCreate(contentType string, body []byte) (model.Experiment, *Error) {
	o, verr := readBody(contentType, body)
	if verr != nil {
		return model.Experiment{}, verr
	}
	exp := model.Experiment{IsActive: true}
	var errs []ErrorItem
	add := func(e *ErrorItem) {
		if e != nil {
			errs = append(errs, *e)
		}
	}

	if raw, ok := o.get("name"); !ok {
		errs = append(errs, missing("name"))
	} else if name, e := checkName(raw, loc("name")); e != nil {
		add(e)
	} else if IsReservedName(name) {
		errs = append(errs, ErrorItem{Type: "value_error", Loc: loc("name"), Input: raw,
			Msg: fmt.Sprintf("Value error, Имя '%s' зарезервировано", name)})
	} else {
		exp.Name = name
	}

	if raw, ok := o.get("variants"); !ok {
		errs = append(errs, missing("variants"))
	} else {
		v, e := checkVariants(raw)
		errs = append(errs, e...)
		exp.Variants = v
	}

	if raw, ok := o.get("config"); ok {
		cfg, e := checkConfig(raw)
		add(e)
		exp.Config = cfg
	}
	if raw, ok := o.get("url"); ok {
		u, e := checkURL(raw)
		add(e)
		exp.URL = u
	}
	if raw, ok := o.get("is_active"); ok {
		b, e := checkBool(raw, loc("is_active"))
		add(e)
		exp.IsActive = b
	}

	if len(errs) > 0 {
		return model.Experiment{}, unprocessable(errs)
	}
	return exp, nil
}

var updateFields = map[string]struct{}{"is_active": {}, "variants": {}, "config": {}, "url": {}}

// ExperimentUpdate разбирает тело PATCH /api/v1/experiments/{name} (ExperimentUpdate,
// extra=forbid). Явный null для config/url очищает значение; для is_active/variants —
// ошибка в ["body"]. Пустой патч не ошибка валидации: его проверяет обработчик
// (model.ExperimentPatch.Empty → 400).
func ExperimentUpdate(contentType string, body []byte) (model.ExperimentPatch, *Error) {
	o, verr := readBody(contentType, body)
	if verr != nil {
		return model.ExperimentPatch{}, verr
	}
	var p model.ExperimentPatch
	var errs []ErrorItem
	var nullField string

	if raw, ok := o.get("is_active"); ok {
		if isNull(raw) {
			nullField = "is_active"
		} else if b, e := checkBool(raw, loc("is_active")); e != nil {
			errs = append(errs, *e)
		} else {
			p.IsActive = &b
		}
	}
	if raw, ok := o.get("variants"); ok {
		if isNull(raw) {
			if nullField == "" {
				nullField = "variants"
			}
		} else if v, e := checkVariants(raw); len(e) > 0 {
			errs = append(errs, e...)
		} else {
			p.Variants = &v
		}
	}
	if raw, ok := o.get("config"); ok {
		cfg, e := checkConfig(raw)
		if e != nil {
			errs = append(errs, *e)
		} else {
			p.ConfigSet = true
			if cfg != nil {
				p.Config = &cfg
			}
		}
	}
	if raw, ok := o.get("url"); ok {
		u, e := checkURL(raw)
		if e != nil {
			errs = append(errs, *e)
		} else {
			p.URLSet = true
			p.URL = u
		}
	}
	for _, f := range o.fields {
		if _, known := updateFields[f.key]; !known {
			errs = append(errs, ErrorItem{Type: "extra_forbidden", Loc: loc(f.key),
				Msg: "Extra inputs are not permitted", Input: f.raw})
		}
	}

	if len(errs) > 0 {
		return model.ExperimentPatch{}, unprocessable(errs)
	}
	// Аналог model_validator: срабатывает, только если поля прошли проверку.
	if nullField != "" {
		return model.ExperimentPatch{}, unprocessable([]ErrorItem{{Type: "value_error", Loc: loc(),
			Msg: fmt.Sprintf("Value error, Поле '%s' не может быть null", nullField)}})
	}
	return p, nil
}

// Batch разбирает тело POST /api/v1/variants/batch (BatchVariantRequest):
// user_id — непустая строка, experiments — список 1..50 имён экспериментов
// (зарезервированные имена здесь допустимы, как в Python). Лишние поля игнорируются.
func Batch(contentType string, body []byte) (BatchRequest, *Error) {
	o, verr := readBody(contentType, body)
	if verr != nil {
		return BatchRequest{}, verr
	}
	var req BatchRequest
	var errs []ErrorItem

	if raw, ok := o.get("user_id"); !ok {
		errs = append(errs, missing("user_id"))
	} else if s, e := checkString(raw, loc("user_id"), 1, 0); e != nil {
		errs = append(errs, *e)
	} else {
		req.UserID = s
	}

	if raw, ok := o.get("experiments"); !ok {
		errs = append(errs, missing("experiments"))
	} else {
		names, e := checkNameList(raw)
		errs = append(errs, e...)
		req.Experiments = names
	}

	if len(errs) > 0 {
		return BatchRequest{}, unprocessable(errs)
	}
	return req, nil
}

func checkNameList(raw json.RawMessage) ([]string, []ErrorItem) {
	l := loc("experiments")
	var items []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, []ErrorItem{{Type: "list_type", Loc: l, Msg: "Input should be a valid list", Input: raw}}
	}
	// pydantic проверяет превышение max_length до элементов.
	if len(items) > MaxBatchSize {
		return nil, []ErrorItem{*lengthError(l, raw, "List", len(items), 1, MaxBatchSize)}
	}
	var errs []ErrorItem
	names := make([]string, 0, len(items))
	for i, it := range items {
		name, e := checkName(it, loc("experiments", i))
		if e != nil {
			errs = append(errs, *e)
			continue
		}
		names = append(names, name)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if e := lengthError(l, raw, "List", len(items), 1, MaxBatchSize); e != nil {
		return nil, []ErrorItem{*e}
	}
	return names, nil
}
