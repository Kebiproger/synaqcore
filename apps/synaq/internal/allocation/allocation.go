// Package allocation — детерминированное распределение пользователей по вариантам.
//
// Правило (должно совпадать с experiment_api, sdk/python и sdk/go):
// bucket = int(SHA-256("<user_id>:<experiment_name>"), 16) % 10000; варианты
// обходятся в алфавитном порядке, побеждает первый, для которого
// bucket * total < cumulative * 10000 (точная целочисленная арифметика).
//
// Паритет с Python-сервером (get_user_variant в
// apps/experiment_api/src/services/allocation.py) закреплён golden-векторами
// в testdata/golden.json (генерируются testdata/gen.py).
package allocation

import (
	"crypto/sha256"
	"math"
	"math/bits"
	"sort"
)

const (
	// Buckets — число бакетов, шаг 0.01%.
	Buckets = 10000
	// DefaultVariant возвращается, когда конфиг вариантов невалиден.
	DefaultVariant = "control"
)

// Allocate возвращает вариант для пользователя userID в эксперименте experimentName.
//
// Fallback DefaultVariant ("control") — как в Python: пустой набор вариантов
// или любой вес <= 0. Отличие только одно (Python считает в длинной
// арифметике): если сумма весов не влезает в uint64, тоже возвращается
// DefaultVariant — валидатор API таких весов не пропускает.
//
// Сортировка sort.Strings (по байтам UTF-8) совпадает с Python sorted()
// (по кодовым точкам): UTF-8 сохраняет порядок кодовых точек.
func Allocate(userID, experimentName string, variants map[string]int64) string {
	if len(variants) == 0 {
		return DefaultVariant
	}

	var total uint64
	keys := make([]string, 0, len(variants))
	for k, w := range variants {
		if w <= 0 || uint64(w) > math.MaxUint64-total {
			return DefaultVariant
		}
		total += uint64(w)
		keys = append(keys, k)
	}
	sort.Strings(keys)

	bucket := Bucket(userID, experimentName)

	// bucket*total и cumulative*10000 — 128-битные произведения, без float.
	lhsHi, lhsLo := bits.Mul64(bucket, total)
	var cumulative uint64
	for _, k := range keys {
		cumulative += uint64(variants[k])
		rhsHi, rhsLo := bits.Mul64(cumulative, Buckets)
		if lhsHi < rhsHi || (lhsHi == rhsHi && lhsLo < rhsLo) {
			return k
		}
	}

	// Недостижимо при положительных весах: последний cumulative == total.
	return DefaultVariant
}

// Bucket — номер бакета 0..9999 для пары (userID, experimentName).
func Bucket(userID, experimentName string) uint64 {
	sum := sha256.Sum256([]byte(userID + ":" + experimentName))
	// mod 10000 по big-endian байтам хэша == int(hash_hex, 16) % 10000 в Python.
	var bucket uint64
	for _, b := range sum {
		bucket = (bucket*256 + uint64(b)) % Buckets
	}
	return bucket
}
