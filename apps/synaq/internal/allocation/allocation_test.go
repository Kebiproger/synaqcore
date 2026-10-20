package allocation

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
)

// goldenVector — ожидание, посчитанное Python-сервером (testdata/gen.py).
type goldenVector struct {
	Source     string           `json:"source"`
	UserID     string           `json:"user_id"`
	Experiment string           `json:"experiment"`
	Variants   map[string]int64 `json:"variants"`
	Bucket     uint64           `json:"bucket"`
	Want       string           `json:"want"`
}

func loadGolden(t testing.TB) []goldenVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("чтение golden.json: %v", err)
	}
	var vs []goldenVector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatalf("разбор golden.json: %v", err)
	}
	return vs
}

// Если тест падает — Go разошёлся с сервером: пользователи получат разные
// варианты в монолите и в старом API/SDK.
func TestGoldenVectorsMatchPythonServer(t *testing.T) {
	vs := loadGolden(t)
	if len(vs) < 250 {
		t.Fatalf("слишком мало golden-векторов: %d", len(vs))
	}
	sources := map[string]int{}
	for _, v := range vs {
		sources[v.Source]++
		if got := Bucket(v.UserID, v.Experiment); got != v.Bucket {
			t.Errorf("Bucket(%q, %q) = %d, want %d", v.UserID, v.Experiment, got, v.Bucket)
		}
		if got := Allocate(v.UserID, v.Experiment, v.Variants); got != v.Want {
			t.Errorf("[%s] Allocate(%q, %q, %v) = %q, want %q (bucket %d)",
				v.Source, v.UserID, v.Experiment, v.Variants, got, v.Want, v.Bucket)
		}
	}
	for _, s := range []string{"sdk_go", "sdk_go_huge", "boundary", "random"} {
		if sources[s] == 0 {
			t.Errorf("нет векторов источника %q", s)
		}
	}
}

// Порядок обхода map в Go случайный; результат от него зависеть не должен.
func TestMapOrderIndependence(t *testing.T) {
	vs := loadGolden(t)
	for _, v := range vs {
		if len(v.Variants) < 2 {
			continue
		}
		for i := 0; i < 100; i++ {
			// Новая map с тем же содержимым — другой порядок вставки и итерации.
			m := make(map[string]int64, len(v.Variants))
			for k, w := range v.Variants {
				m[k] = w
			}
			if got := Allocate(v.UserID, v.Experiment, m); got != v.Want {
				t.Fatalf("итерация %d: Allocate(%q, %q) = %q, want %q", i, v.UserID, v.Experiment, got, v.Want)
			}
		}
	}
}

func TestFallback(t *testing.T) {
	cases := map[string]map[string]int64{
		"nil":             nil,
		"empty":           {},
		"zero weight":     {"A": 0, "B": 100},
		"negative":        {"A": -1, "B": 100},
		"all zero":        {"A": 0},
		"min int64":       {"A": math.MinInt64, "B": 10},
		"uint64 overflow": {"A": math.MaxInt64, "B": math.MaxInt64, "C": math.MaxInt64},
	}
	for name, v := range cases {
		for _, u := range []string{"u", "golden_user_0", "пользователь"} {
			if got := Allocate(u, "e", v); got != DefaultVariant {
				t.Errorf("%s: Allocate(%q, %v) = %q, want %q", name, u, v, got, DefaultVariant)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	v := map[string]int64{"control": 50, "treatment": 50}
	want := Allocate("user_42", "checkout", v)
	for i := 0; i < 1000; i++ {
		if got := Allocate("user_42", "checkout", v); got != want {
			t.Fatalf("недетерминированно: %q != %q", got, want)
		}
	}
}

func BenchmarkAllocate(b *testing.B) {
	for _, n := range []int{2, 5, 20} {
		v := make(map[string]int64, n)
		for i := 0; i < n; i++ {
			v[fmt.Sprintf("variant_%02d", i)] = int64(i + 1)
		}
		b.Run(fmt.Sprintf("variants=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = Allocate("user_1234567890", "checkout_experiment", v)
			}
		})
	}
}
