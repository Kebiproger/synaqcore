package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"synaq/internal/model"
	"synaq/internal/store/sqlcdb"
)

// sqlstateUniqueViolation — нарушение уникальности (занятое имя эксперимента).
const sqlstateUniqueViolation = "23505"

// Repository — model.Repository поверх PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
	q    *sqlcdb.Queries
}

var _ model.Repository = (*Repository)(nil)

// NewRepository создаёт репозиторий на готовом пуле (см. Open, Migrate).
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool, q: sqlcdb.New(pool)}
}

// List возвращает эксперименты в порядке created_at DESC, name.
func (r *Repository) List(ctx context.Context) ([]model.Experiment, error) {
	rows, err := r.q.ListExperiments(ctx)
	if err != nil {
		return nil, fmt.Errorf("список экспериментов: %w", err)
	}
	out := make([]model.Experiment, 0, len(rows))
	for _, row := range rows {
		e, err := toModel(row)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Get возвращает эксперимент по имени или model.ErrNotFound.
func (r *Repository) Get(ctx context.Context, name string) (model.Experiment, error) {
	row, err := r.q.GetExperiment(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Experiment{}, model.ErrNotFound
	}
	if err != nil {
		return model.Experiment{}, fmt.Errorf("чтение эксперимента: %w", err)
	}
	return toModel(row)
}

// Create вставляет эксперимент; занятое имя -> model.ErrExists.
// created_at выставляет БД (DEFAULT now()).
func (r *Repository) Create(ctx context.Context, e model.Experiment) error {
	variants, err := json.Marshal(e.Variants)
	if err != nil {
		return fmt.Errorf("variants: %w", err)
	}
	err = r.q.CreateExperiment(ctx, sqlcdb.CreateExperimentParams{
		Name:     e.Name,
		Variants: variants,
		Config:   normalizeJSON(e.Config),
		Url:      e.URL,
		IsActive: e.IsActive,
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateUniqueViolation {
		return model.ErrExists
	}
	if err != nil {
		return fmt.Errorf("создание эксперимента: %w", err)
	}
	return nil
}

// Update применяет патч в одной транзакции под SELECT ... FOR UPDATE: проверка
// активности и запись атомарны относительно параллельных PATCH. Правило 409 как в
// experiment_api: варианты переданы, отличаются от текущих (равенство словарей) и
// эксперимент активен после применения is_active из того же запроса.
func (r *Repository) Update(ctx context.Context, name string, p model.ExperimentPatch) (model.Experiment, error) {
	var updated model.Experiment
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetExperimentForUpdate(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("чтение эксперимента: %w", err)
		}
		cur, err := toModel(row)
		if err != nil {
			return err
		}

		params := sqlcdb.UpdateExperimentParams{
			Name:     name,
			Variants: row.Variants,
			Config:   cur.Config,
			Url:      cur.URL,
			IsActive: cur.IsActive,
		}
		if p.IsActive != nil {
			params.IsActive = *p.IsActive
		}
		if p.Variants != nil {
			if !maps.Equal(*p.Variants, cur.Variants) && params.IsActive {
				return model.ErrActiveVariantsChange
			}
			if params.Variants, err = json.Marshal(*p.Variants); err != nil {
				return fmt.Errorf("variants: %w", err)
			}
		}
		if p.ConfigSet {
			params.Config = nil
			if p.Config != nil {
				params.Config = normalizeJSON(*p.Config)
			}
		}
		if p.URLSet {
			params.Url = p.URL
		}

		row, err = q.UpdateExperiment(ctx, params)
		if err != nil {
			return fmt.Errorf("обновление эксперимента: %w", err)
		}
		updated, err = toModel(row)
		return err
	})
	if err != nil {
		return model.Experiment{}, err
	}
	return updated, nil
}

// Delete удаляет эксперимент; если строки нет — model.ErrNotFound.
func (r *Repository) Delete(ctx context.Context, name string) error {
	n, err := r.q.DeleteExperiment(ctx, name)
	if err != nil {
		return fmt.Errorf("удаление эксперимента: %w", err)
	}
	if n == 0 {
		return model.ErrNotFound
	}
	return nil
}

// toModel переводит строку БД в доменный тип.
func toModel(row sqlcdb.Experiment) (model.Experiment, error) {
	var variants map[string]int64
	if err := json.Unmarshal(row.Variants, &variants); err != nil {
		return model.Experiment{}, fmt.Errorf("variants эксперимента %q: %w", row.Name, err)
	}
	return model.Experiment{
		Name:      row.Name,
		Variants:  variants,
		Config:    normalizeJSON(row.Config),
		URL:       row.Url,
		IsActive:  row.IsActive,
		CreatedAt: row.CreatedAt,
	}, nil
}

// normalizeJSON сводит JSON null к nil (SQL NULL). SQLAlchemy по умолчанию
// (JSON.none_as_null=False) записывает Python None как jsonb 'null', а не SQL NULL;
// для API оба случая — отсутствие конфига.
func normalizeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
}
