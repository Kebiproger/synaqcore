package configcache

import (
	"encoding/json"
	"maps"

	"synaq/internal/model"
)

// snapshot — неизменяемый снимок конфигураций. После создания не меняется,
// поэтому безопасен для конкурентного чтения без блокировок.
//
// Контракт неизменяемости: значения, возвращённые Get и All, разделяют с
// снимком карты Variants, байты Config и указатель URL. Вызывающий код НЕ
// должен их модифицировать (копирование на каждом Get было бы лишней
// аллокацией на горячем пути). Сами данные при загрузке глубоко копируются
// из результата репозитория, так что репозиторий тоже не может их изменить.
type snapshot struct {
	byName map[string]model.Experiment
	// list — эксперименты в порядке репозитория (created_at DESC, name).
	list []model.Experiment
}

var _ model.Snapshot = (*snapshot)(nil)

// emptySnapshot отдаётся до первой успешной загрузки.
var emptySnapshot = &snapshot{byName: map[string]model.Experiment{}}

// newSnapshot строит снимок, глубоко копируя эксперименты и сохраняя порядок.
func newSnapshot(exps []model.Experiment) *snapshot {
	s := &snapshot{
		byName: make(map[string]model.Experiment, len(exps)),
		list:   make([]model.Experiment, 0, len(exps)),
	}
	for _, e := range exps {
		c := cloneExperiment(e)
		s.list = append(s.list, c)
		s.byName[c.Name] = c
	}
	return s
}

func cloneExperiment(e model.Experiment) model.Experiment {
	c := e
	if e.Variants != nil {
		c.Variants = maps.Clone(e.Variants)
	}
	if e.Config != nil {
		c.Config = append(json.RawMessage(nil), e.Config...)
	}
	if e.URL != nil {
		u := *e.URL
		c.URL = &u
	}
	return c
}

// Get возвращает эксперимент по имени (включая неактивные).
func (s *snapshot) Get(name string) (model.Experiment, bool) {
	e, ok := s.byName[name]
	return e, ok
}

// All возвращает копию среза экспериментов в порядке created_at DESC, name.
// Перестановка/замена элементов среза не влияет на снимок; вложенные
// Variants/Config/URL менять нельзя (см. контракт неизменяемости).
func (s *snapshot) All() []model.Experiment {
	out := make([]model.Experiment, len(s.list))
	copy(out, s.list)
	return out
}
