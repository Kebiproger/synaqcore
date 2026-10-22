// Package model содержит доменные типы экспериментов и контракты слоя хранения:
// Repository (PostgreSQL, источник истины) и Cache (снимок конфигураций в памяти
// для раздачи вариантов). Реализации живут в internal/store и internal/configcache.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Сигнальные ошибки репозитория; HTTP-слой сопоставляет их кодам ответа.
var (
	// ErrNotFound — эксперимент не найден (404).
	ErrNotFound = errors.New("эксперимент не найден")
	// ErrExists — эксперимент с таким именем уже существует (400, как в Python-версии experiment_api, тег v1-python).
	ErrExists = errors.New("эксперимент уже существует")
	// ErrActiveVariantsChange — попытка изменить варианты активного эксперимента (409).
	ErrActiveVariantsChange = errors.New("нельзя менять варианты активного эксперимента")
)

// Experiment — конфигурация эксперимента (строка таблицы experiments).
type Experiment struct {
	Name string
	// Variants — имя варианта -> вес. Порядок ключей не важен: аллокация
	// обходит варианты в алфавитном порядке.
	Variants map[string]int64
	// Config — произвольный JSON; nil означает SQL NULL.
	Config json.RawMessage
	// URL — nil означает SQL NULL.
	URL       *string
	IsActive  bool
	CreatedAt time.Time
}

// ExperimentPatch — частичное обновление (PATCH). Для Variants и IsActive nil
// означает «поле не передано». Для Config и URL различаются три состояния:
//   - ConfigSet == false: поле не передано, значение не меняется;
//   - ConfigSet == true, Config == nil: передан явный null, значение очищается (SQL NULL);
//   - ConfigSet == true, Config != nil: новое значение.
//
// Аналогично URLSet/URL.
type ExperimentPatch struct {
	Variants  *map[string]int64
	Config    *json.RawMessage
	ConfigSet bool
	URL       *string
	URLSet    bool
	IsActive  *bool
}

// Empty сообщает, что в патче нет ни одного поля (400, как в Python-версии experiment_api, тег v1-python).
func (p ExperimentPatch) Empty() bool {
	return p.Variants == nil && !p.ConfigSet && !p.URLSet && p.IsActive == nil
}

// Repository — хранилище экспериментов (PostgreSQL).
type Repository interface {
	// List возвращает все эксперименты, отсортированные по created_at DESC, затем по name.
	List(ctx context.Context) ([]Experiment, error)
	// Get возвращает эксперимент по имени или ErrNotFound.
	Get(ctx context.Context, name string) (Experiment, error)
	// Create создаёт эксперимент; если имя занято — ErrExists.
	Create(ctx context.Context, e Experiment) error
	// Update применяет патч в транзакции: SELECT ... FOR UPDATE, ErrNotFound, если
	// эксперимента нет; ErrActiveVariantsChange, если меняются варианты активного
	// эксперимента; затем запись. Возвращает обновлённый эксперимент.
	Update(ctx context.Context, name string, p ExperimentPatch) (Experiment, error)
	// Delete удаляет эксперимент или возвращает ErrNotFound.
	Delete(ctx context.Context, name string) error
}

// Snapshot — неизменяемый снимок конфигураций в памяти. Безопасен для
// конкурентного чтения.
type Snapshot interface {
	// Get возвращает эксперимент по имени.
	Get(name string) (Experiment, bool)
	// All возвращает все эксперименты в порядке created_at DESC.
	All() []Experiment
}

// Cache хранит актуальный Snapshot и перечитывает его из Repository.
type Cache interface {
	// Snapshot возвращает текущий снимок (без блокировок на горячем пути).
	Snapshot() Snapshot
	// Reload перечитывает конфигурации из хранилища и атомарно заменяет снимок.
	Reload(ctx context.Context) error
}
