package synaq

import (
	"fmt"
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
