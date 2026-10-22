// Package config содержит помощники для чтения настроек из переменных окружения.
//
// Функции Int/Duration возвращают ошибку для некорректных значений (их удобно
// тестировать), MustInt/MustDuration завершают процесс через log.Fatalf —
// так поступает main при старте сервиса.
package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

// String возвращает значение переменной key или defaultValue, если она не задана или пуста.
func String(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// Int читает положительное целое из переменной key. Пустое значение — defaultValue.
func Int(key string, defaultValue int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultValue, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("некорректное значение %s=%q: ожидается положительное целое число", key, raw)
	}
	return v, nil
}

// Duration читает положительную длительность (например, 500ms, 1s) из переменной key.
// Пустое значение — defaultValue.
func Duration(key string, defaultValue time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultValue, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("некорректное значение %s=%q: ожидается положительная длительность, например 1s", key, raw)
	}
	return v, nil
}

// MustInt — как Int, но некорректное значение — фатальная ошибка.
func MustInt(key string, defaultValue int) int {
	v, err := Int(key, defaultValue)
	if err != nil {
		log.Fatal(err)
	}
	return v
}

// MustDuration — как Duration, но некорректное значение — фатальная ошибка.
func MustDuration(key string, defaultValue time.Duration) time.Duration {
	v, err := Duration(key, defaultValue)
	if err != nil {
		log.Fatal(err)
	}
	return v
}
