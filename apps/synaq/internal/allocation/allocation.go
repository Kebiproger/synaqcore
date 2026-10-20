// Package allocation — детерминированное распределение пользователей по вариантам.
//
// Правило (должно совпадать с experiment_api, sdk/python и sdk/go):
// bucket = int(SHA-256("<user_id>:<experiment_name>"), 16) % 10000; варианты
// обходятся в алфавитном порядке, побеждает первый, для которого
// bucket * total < cumulative * 10000 (точная целочисленная арифметика).
package allocation

// Allocate возвращает вариант для пользователя userID в эксперименте experimentName.
//
// TODO(T1): реализовать правило из описания пакета; пока это заглушка.
func Allocate(userID, experimentName string, variants map[string]int64) string {
	_, _, _ = userID, experimentName, variants
	return "control"
}
