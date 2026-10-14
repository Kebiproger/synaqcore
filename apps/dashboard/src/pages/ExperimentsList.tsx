import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError } from "../api/client";
import type { Experiment } from "../api/types";
import { useAuth } from "../auth/AuthProvider";
import { Alert, btn, Card, ConfirmDialog, Spinner, StatusBadge } from "../components/ui";
import { seriesColor } from "../lib/format";

function VariantChips({ variants }: { variants: Record<string, number> }) {
  const entries = Object.entries(variants);
  const total = entries.reduce((s, [, w]) => s + (Number(w) || 0), 0) || 1;
  return (
    <div className="flex flex-wrap gap-1.5">
      {entries.map(([name, w], i) => (
        <span
          key={name}
          className="inline-flex items-center gap-1.5 rounded-md bg-slate-100 px-2 py-0.5 text-xs dark:bg-slate-800"
          title={`вес ${w}`}
        >
          <span className="h-2 w-2 rounded-full" style={{ background: seriesColor(i) }} aria-hidden />
          <span className="font-medium">{name}</span>
          <span className="tabular-nums text-slate-500 dark:text-slate-400">
            {w} · {Math.round((Number(w) / total) * 100)}%
          </span>
        </span>
      ))}
    </div>
  );
}

export function ExperimentsList() {
  const { isAdmin } = useAuth();
  const [items, setItems] = useState<Experiment[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [toDelete, setToDelete] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      const data = await api.listExperiments();
      setItems(Array.isArray(data) ? [...data].sort((a, b) => a.name.localeCompare(b.name)) : []);
    } catch (e) {
      setError(e instanceof ApiError ? e : new ApiError(0, "Неизвестная ошибка."));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function toggle(exp: Experiment) {
    setBusy(exp.name);
    setActionError(null);
    try {
      const updated = await api.patchExperiment(exp.name, { is_active: !exp.is_active });
      setItems((prev) =>
        prev?.map((e) => (e.name === exp.name ? { ...e, ...(updated ?? {}), is_active: updated?.is_active ?? !exp.is_active } : e)) ?? null,
      );
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : "Не удалось изменить статус.");
    } finally {
      setBusy(null);
    }
  }

  async function confirmDelete() {
    if (!toDelete) return;
    const name = toDelete;
    setBusy(name);
    setActionError(null);
    try {
      await api.deleteExperiment(name);
      setItems((prev) => prev?.filter((e) => e.name !== name) ?? null);
      setToDelete(null);
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : "Не удалось удалить эксперимент.");
      setToDelete(null);
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">Эксперименты</h1>
        <div className="flex gap-2">
          <button type="button" className={btn.secondary} onClick={() => void load()}>
            Обновить
          </button>
          {isAdmin && (
            <Link to="/new" className={btn.primary}>
              Новый эксперимент
            </Link>
          )}
        </div>
      </div>

      {actionError && <Alert title="Действие не выполнено">{actionError}</Alert>}

      {error ? (
        <Alert title={error.status === 403 ? "Доступ запрещён" : "Не удалось загрузить список"}>{error.message}</Alert>
      ) : items === null ? (
        <Spinner />
      ) : items.length === 0 ? (
        <Card>
          <p className="text-sm text-slate-600 dark:text-slate-300">Экспериментов пока нет.</p>
        </Card>
      ) : (
        <ul className="grid gap-3">
          {items.map((exp) => (
            <li key={exp.name}>
              <Card className="flex flex-col gap-3 md:flex-row md:items-center">
                <div className="min-w-0 flex-1 space-y-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <Link
                      to={`/experiments/${encodeURIComponent(exp.name)}`}
                      className="truncate font-semibold text-blue-700 hover:underline dark:text-blue-400"
                    >
                      {exp.name}
                    </Link>
                    <StatusBadge active={exp.is_active} />
                  </div>
                  <VariantChips variants={exp.variants ?? {}} />
                  {exp.url && (
                    <div className="truncate text-xs text-slate-500 dark:text-slate-400" title={exp.url}>
                      URL: <span className="font-mono">{exp.url}</span>
                    </div>
                  )}
                </div>
                <div className="flex shrink-0 flex-wrap gap-2">
                  <Link to={`/experiments/${encodeURIComponent(exp.name)}`} className={btn.secondary}>
                    Результаты
                  </Link>
                  {isAdmin && (
                    <>
                      <button
                        type="button"
                        className={btn.secondary}
                        disabled={busy === exp.name}
                        onClick={() => void toggle(exp)}
                      >
                        {exp.is_active ? "Остановить" : "Запустить"}
                      </button>
                      <button
                        type="button"
                        className={btn.danger}
                        disabled={busy === exp.name}
                        onClick={() => setToDelete(exp.name)}
                      >
                        Удалить
                      </button>
                    </>
                  )}
                </div>
              </Card>
            </li>
          ))}
        </ul>
      )}

      <ConfirmDialog
        open={toDelete !== null}
        title="Удалить эксперимент?"
        message={
          <>
            Эксперимент <span className="font-mono font-semibold">{toDelete}</span> будет удалён без возможности
            восстановления. Собранные события в аналитике останутся.
          </>
        }
        confirmLabel="Удалить"
        busy={busy !== null && busy === toDelete}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setToDelete(null)}
      />
    </div>
  );
}
