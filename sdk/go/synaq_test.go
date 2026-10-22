package synaq

import (
	"fmt"
	"math"
	"strconv"
	"testing"
)

func TestLocalEvaluation(t *testing.T) {
	client := NewClient(Config{
		BaseURL:   "http://mock",
		AutoStart: false,
	})

	client.experiments["landing_cta"] = ExperimentConfig{
		Name:     "landing_cta",
		Variants: map[string]int{"A": 50, "B": 50},
	}

	client.experiments["three_way"] = ExperimentConfig{
		Name:     "three_way",
		Variants: map[string]int{"control": 60, "v1": 20, "v2": 20},
	}

	// 1. Известные детерминированные тесты
	for i := 0; i < 50; i++ {
		userID := fmt.Sprintf("user_go_%d", i)
		v := client.GetVariant("landing_cta", userID)
		if v != "A" && v != "B" {
			t.Fatalf("unexpected variant for user %s: %s", userID, v)
		}
	}

	// 2. Несуществующий эксперимент должен вернуть "control"
	if v := client.GetVariant("non_existent", "user_1"); v != "control" {
		t.Fatalf("expected control, got %s", v)
	}

	// 3. Пустой userID должен вернуть "control"
	if v := client.GetVariant("landing_cta", ""); v != "control" {
		t.Fatalf("expected control, got %s", v)
	}
}

func BenchmarkGetVariant(b *testing.B) {
	client := NewClient(Config{
		BaseURL:   "http://mock",
		AutoStart: false,
	})

	client.experiments["bench_exp"] = ExperimentConfig{
		Name:     "bench_exp",
		Variants: map[string]int{"control": 50, "v1": 25, "v2": 25},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = client.GetVariant("bench_exp", "bench_user_42")
	}
}

// goldenVectors сгенерированы Python-функцией сервера get_user_variant()
// (Python-версия experiment_api, тег v1-python); эталон сервера synaq — apps/synaq/internal/allocation/testdata/golden.json. Если тест падает — Go SDK
// разошёлся с сервером: пользователи получат разные варианты в SDK и через API.
// Последние строки — точные границы бакетов, где float-сравнение ошибалось.
var goldenVectors = []struct {
	userID, experiment string
	variants           map[string]int
	want               string
}{
	{"golden_user_0", "checkout", map[string]int{"treatment": 50, "control": 50}, "control"},                        // bucket 480
	{"golden_user_1", "checkout", map[string]int{"treatment": 50, "control": 50}, "control"},                        // bucket 2729
	{"golden_user_2", "checkout", map[string]int{"treatment": 50, "control": 50}, "control"},                        // bucket 334
	{"golden_user_3", "checkout", map[string]int{"treatment": 50, "control": 50}, "treatment"},                      // bucket 5430
	{"golden_user_4", "checkout", map[string]int{"treatment": 50, "control": 50}, "treatment"},                      // bucket 9533
	{"golden_user_5", "checkout", map[string]int{"treatment": 50, "control": 50}, "treatment"},                      // bucket 6941
	{"golden_user_6", "checkout", map[string]int{"treatment": 50, "control": 50}, "control"},                        // bucket 140
	{"golden_user_7", "checkout", map[string]int{"treatment": 50, "control": 50}, "control"},                        // bucket 1532
	{"golden_user_0", "landing", map[string]int{"control": 50, "v2": 50}, "control"},                                // bucket 3126
	{"golden_user_1", "landing", map[string]int{"control": 50, "v2": 50}, "v2"},                                     // bucket 5567
	{"golden_user_2", "landing", map[string]int{"control": 50, "v2": 50}, "v2"},                                     // bucket 7752
	{"golden_user_3", "landing", map[string]int{"control": 50, "v2": 50}, "control"},                                // bucket 367
	{"golden_user_4", "landing", map[string]int{"control": 50, "v2": 50}, "control"},                                // bucket 742
	{"golden_user_5", "landing", map[string]int{"control": 50, "v2": 50}, "v2"},                                     // bucket 6958
	{"golden_user_6", "landing", map[string]int{"control": 50, "v2": 50}, "control"},                                // bucket 2353
	{"golden_user_7", "landing", map[string]int{"control": 50, "v2": 50}, "v2"},                                     // bucket 7230
	{"golden_user_0", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "control"},                   // bucket 7060
	{"golden_user_1", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "blue"},                      // bucket 71
	{"golden_user_2", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "red"},                       // bucket 9505
	{"golden_user_3", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "control"},                   // bucket 6733
	{"golden_user_4", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "red"},                       // bucket 9084
	{"golden_user_5", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "control"},                   // bucket 6703
	{"golden_user_6", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "blue"},                      // bucket 1571
	{"golden_user_7", "pricing", map[string]int{"red": 20, "blue": 30, "control": 50}, "red"},                       // bucket 9741
	{"golden_user_0", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 2378
	{"golden_user_1", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 136
	{"golden_user_2", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 5688
	{"golden_user_3", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 1807
	{"golden_user_4", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 452
	{"golden_user_5", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 9871
	{"golden_user_6", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 6426
	{"golden_user_7", "solo", map[string]int{"only": 100}, "only"},                                                  // bucket 3026
	{"golden_user_0", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "variant_long_name"}, // bucket 8309
	{"golden_user_1", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "control"},           // bucket 1472
	{"golden_user_2", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "b"},                 // bucket 1201
	{"golden_user_3", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "control"},           // bucket 5784
	{"golden_user_4", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "control"},           // bucket 3894
	{"golden_user_5", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "control"},           // bucket 2416
	{"golden_user_6", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "control"},           // bucket 3259
	{"golden_user_7", "uneven", map[string]int{"variant_long_name": 7, "b": 3, "control": 13}, "control"},           // bucket 6547
	{"edge_user_779", "edge_a", map[string]int{"a": 2, "b": 9, "c": 33}, "c"},                                       // bucket 2500
	{"edge_user_483", "edge_b", map[string]int{"a": 3, "b": 10, "c": 13}, "c"},                                      // bucket 5000
	{"edge_user_9603", "edge_c", map[string]int{"a": 7, "b": 5, "c": 13}, "b"},                                      // bucket 2800
	{"edge_user_7707", "half", map[string]int{"A": 50, "B": 50}, "B"},                                               // bucket 5000
	{"edge_user_4656", "half", map[string]int{"A": 50, "B": 50}, "A"},                                               // bucket 4999
	{"edge_user_6057", "last", map[string]int{"A": 1, "B": 1}, "B"},                                                 // bucket 9999
	{"edge_user_5788", "first", map[string]int{"A": 1, "B": 999}, "A"},                                              // bucket 0
}

func TestGoldenVectorsMatchServer(t *testing.T) {
	for _, tc := range goldenVectors {
		if got := allocate(tc.userID, tc.experiment, tc.variants, "fallback"); got != tc.want {
			t.Errorf("allocate(%q, %q, %v) = %q, want %q", tc.userID, tc.experiment, tc.variants, got, tc.want)
		}
	}
}

// TestGoldenVectorsHugeWeights: произведение bucket*total не влезает в 64 бита —
// проверка 128-битного сравнения. Ожидания тоже получены из Python-сервера.
func TestGoldenVectorsHugeWeights(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("нужен 64-битный int")
	}
	huge := map[string]int{"A": math.MaxInt, "B": math.MaxInt}
	for _, tc := range []struct{ userID, experiment, want string }{
		{"u", "e", "A"},
		{"big_user_1", "e", "B"},
		{"big_user_2", "big", "A"},
	} {
		if got := allocate(tc.userID, tc.experiment, huge, "fallback"); got != tc.want {
			t.Errorf("allocate(%q, %q, huge) = %q, want %q", tc.userID, tc.experiment, got, tc.want)
		}
	}
	// Сумма весов переполняет uint64 -> fallback.
	overflow := map[string]int{"A": math.MaxInt, "B": math.MaxInt, "C": math.MaxInt}
	if got := allocate("u", "e", overflow, "fallback"); got != "fallback" {
		t.Errorf("allocate(overflow) = %q, want fallback", got)
	}
}

func TestGetVariantUsesAllocation(t *testing.T) {
	client := NewClient(Config{BaseURL: "http://mock", AutoStart: false})
	for _, tc := range goldenVectors {
		client.experiments[tc.experiment] = ExperimentConfig{Name: tc.experiment, Variants: tc.variants}
		if got := client.GetVariant(tc.experiment, tc.userID); got != tc.want {
			t.Errorf("GetVariant(%q, %q) = %q, want %q", tc.experiment, tc.userID, got, tc.want)
		}
	}
}

func TestAllocateInvalidWeights(t *testing.T) {
	for _, v := range []map[string]int{
		nil,
		{"A": 0, "B": 100},
		{"A": -1, "B": 100},
	} {
		if got := allocate("u", "e", v, "fallback"); got != "fallback" {
			t.Errorf("allocate(%v) = %q, want fallback", v, got)
		}
	}
}
