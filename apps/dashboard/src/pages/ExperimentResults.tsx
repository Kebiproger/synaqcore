import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { api, ApiError } from "../api/client";
import type { Recommendation, StatsResponse } from "../api/types";
import { BarChart, type BarDatum } from "../components/BarChart";
import { LineChart, type LineSeries } from "../components/LineChart";
import { Alert, btn, Card, inputCls, Spinner } from "../components/ui";
import { fmtInt, fmtP, fmtPct, fmtSignedPct, maybeNum, num, seriesColor } from "../lib/format";
import { EXPERIMENT_NAME_RE, GOAL_EVENT_RE } from "../lib/validation";
import { deriveWinner, pickPair } from "../lib/winner";

const DEFAULT_GOAL = "purchase";

function errorView(err: ApiError) {
  switch (err.status) {
    case 404:
      return { tone: "info" as const, title: "Данных пока нет", text: "По этому эксперименту и целевому событию ещё нет событий в аналитике." };
    case 400:
      // Текст detail от сервера (например, недопустимое имя или goal_event).
      return { tone: "warn" as const, title: "Запрос отклонён сервером", text: err.message };
    case 403:
      return { tone: "error" as const, title: "Доступ запрещён", text: err.message };
    case 502:
      return { tone: "error" as const, title: "Хранилище аналитики недоступно", text: err.message };
    default:
      return { tone: "error" as const, title: "Не удалось загрузить результаты", text: err.message };
  }
}

function Kpi({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="rounded-xl border border-slate-200 bg-white p-4 dark:border-slate-800 dark:bg-slate-900">
      <div className="text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">{label}</div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value}</div>
      {hint && <div className="mt-0.5 text-xs text-slate-500 dark:text-slate-400">{hint}</div>}
    </div>
  );
}

function RecommendationItem({ r }: { r: Recommendation }) {
  const insights = Array.isArray(r.additional_insights) ? r.additional_insights.filter((x) => typeof x === "string") : [];
  return (
    <li className="rounded-lg border border-slate-200 p-3 dark:border-slate-800">
      <div className="flex flex-wrap items-center gap-2">
        {r.type && (
          <span className="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800">{String(r.type)}</span>
        )}
        {r.confidence !== undefined && r.confidence !== null && (
          <span className="text-xs text-slate-500 dark:text-slate-400">уверенность: {String(r.confidence)}</span>
        )}
      </div>
      {r.summary && <p className="mt-1.5 text-sm font-medium">{String(r.summary)}</p>}
      {r.action && <p className="mt-1 text-sm text-slate-700 dark:text-slate-300">→ {String(r.action)}</p>}
      {r.risk_note && <p className="mt-1 text-xs text-amber-800 dark:text-amber-300">⚠ {String(r.risk_note)}</p>}
      {insights.length > 0 && (
        <ul className="mt-1.5 list-disc space-y-0.5 pl-5 text-xs text-slate-600 dark:text-slate-400">
          {insights.map((t, i) => (
            <li key={i}>{t}</li>
          ))}
        </ul>
      )}
    </li>
  );
}

function Results({ stats }: { stats: StatsResponse }) {
  const variantNames = Object.keys(stats.variants ?? {});
  const colorOf = (n: string) => seriesColor(Math.max(0, variantNames.indexOf(n)));
  const { control, variant } = pickPair(stats.variants ?? {});
  const st = stats.statistical;
  const statError = st && typeof st.error === "string" ? st.error : null;
  const freq = st?.frequentist ?? null;
  const bayes = st?.bayesian ?? null;
  const lift = maybeNum(st?.sample_rates?.relative_lift_pct);
  const pValue = maybeNum(freq?.p_value);
  const pB = maybeNum(bayes?.probability_b_better);
  const winner = deriveWinner(stats);

  const bars: BarDatum[] = variantNames.map((n) => {
    const v = stats.variants[n]!;
    return {
      label: n,
      value: num(v.conversion_rate) * 100,
      users: num(v.total_users),
      conversions: num(v.converting_users),
      color: colorOf(n),
    };
  });

  const { series, days } = useMemo(() => {
    const rows = Array.isArray(stats.daily_trend) ? stats.daily_trend : [];
    const daySet = new Set<string>();
    const byVariant = new Map<string, Map<string, number>>();
    for (const r of rows) {
      const day = String(r.day ?? "");
      const vname = String(r.variant ?? "");
      if (!day || !vname) continue;
      daySet.add(day);
      if (!byVariant.has(vname)) byVariant.set(vname, new Map());
      byVariant.get(vname)!.set(day, num(r.daily_conversion_rate_pct));
    }
    const ordered = [...byVariant.keys()].sort((a, b) => {
      const ia = variantNames.indexOf(a);
      const ib = variantNames.indexOf(b);
      return (ia < 0 ? 999 : ia) - (ib < 0 ? 999 : ib) || a.localeCompare(b);
    });
    const s: LineSeries[] = ordered.map((n, i) => ({
      name: n,
      color: variantNames.includes(n) ? colorOf(n) : seriesColor(variantNames.length + i),
      points: byVariant.get(n)!,
    }));
    return { series: s, days: [...daySet].sort() };
  }, [stats]);

  const significant = freq?.is_significant;

  return (
    <div className="space-y-4">
      <Card className={winner.kind === "winner" ? "border-emerald-300 dark:border-emerald-800" : ""}>
        <div className="flex flex-wrap items-center gap-3">
          <span className="text-sm text-slate-500 dark:text-slate-400">Победитель:</span>
          {winner.kind === "winner" ? (
            <span className="inline-flex items-center gap-2 text-lg font-semibold text-emerald-700 dark:text-emerald-300">
              <span className="h-3 w-3 rounded-full" style={{ background: colorOf(winner.name) }} aria-hidden />
              {winner.name}
            </span>
          ) : (
            <span className="text-lg font-semibold text-slate-700 dark:text-slate-200">победитель не определён</span>
          )}
          <span className="ml-auto text-xs text-slate-500 dark:text-slate-400">
            сравнение: <span className="font-mono">{variant ?? "—"}</span> против контроля{" "}
            <span className="font-mono">{control}</span>
          </span>
        </div>
      </Card>

      {statError && <Alert tone="warn" title="Статистический анализ недоступен">{statError}</Alert>}

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Kpi label="Uplift" value={fmtSignedPct(lift)} hint="относительный прирост конверсии" />
        <Kpi label="p-value" value={fmtP(pValue)} hint="частотный z-тест" />
        <Kpi label="P(B лучше)" value={pB === null ? "—" : fmtPct(pB * 100, 1)} hint="байесовская оценка" />
        <Kpi
          label="Значимость"
          value={significant === true ? "✓ значимо" : significant === false ? "✗ не значимо" : "—"}
          hint={
            maybeNum(freq?.confidence_level) !== null
              ? `уровень доверия ${fmtPct(num(freq?.confidence_level) * (num(freq?.confidence_level) <= 1 ? 100 : 1), 0)}`
              : undefined
          }
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <h2 className="mb-3 text-sm font-semibold">Конверсия по вариантам</h2>
          {bars.length ? <BarChart data={bars} /> : <p className="text-sm text-slate-500">Нет данных.</p>}
          <table className="mt-4 w-full text-left text-sm">
            <thead className="text-xs text-slate-500 dark:text-slate-400">
              <tr>
                <th className="py-1 font-medium">Вариант</th>
                <th className="py-1 text-right font-medium">Пользователи</th>
                <th className="py-1 text-right font-medium">Конверсии</th>
                <th className="py-1 text-right font-medium">CR</th>
              </tr>
            </thead>
            <tbody className="tabular-nums">
              {bars.map((b) => (
                <tr key={b.label} className="border-t border-slate-100 dark:border-slate-800">
                  <td className="py-1.5">
                    <span className="inline-flex items-center gap-1.5">
                      <span className="h-2 w-2 rounded-full" style={{ background: b.color }} aria-hidden />
                      {b.label}
                    </span>
                  </td>
                  <td className="py-1.5 text-right">{fmtInt(b.users)}</td>
                  <td className="py-1.5 text-right">{fmtInt(b.conversions)}</td>
                  <td className="py-1.5 text-right">{fmtPct(b.value)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
        <Card>
          <h2 className="mb-3 text-sm font-semibold">Дневная конверсия</h2>
          {days.length ? (
            <LineChart series={series} days={days} />
          ) : (
            <p className="text-sm text-slate-500">Нет данных по дням.</p>
          )}
        </Card>
      </div>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">Рекомендации</h2>
        {Array.isArray(stats.recommendations) && stats.recommendations.length > 0 ? (
          <ul className="space-y-2">
            {stats.recommendations.map((r, i) => (
              <RecommendationItem key={i} r={r} />
            ))}
          </ul>
        ) : (
          <p className="text-sm text-slate-500">Рекомендаций нет.</p>
        )}
      </Card>
    </div>
  );
}

export function ExperimentResults() {
  const { name = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const goalParam = params.get("goal") ?? DEFAULT_GOAL;
  const goal = GOAL_EVENT_RE.test(goalParam) ? goalParam : DEFAULT_GOAL;
  const [draft, setDraft] = useState(goal);
  const [draftError, setDraftError] = useState<string | null>(null);
  const [stats, setStats] = useState<StatsResponse | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const validName = name.length >= 1 && name.length <= 50 && EXPERIMENT_NAME_RE.test(name);

  const load = useCallback(async () => {
    if (!validName) return;
    setLoading(true);
    setError(null);
    try {
      setStats(await api.stats(name, goal));
    } catch (e) {
      setStats(null);
      setError(e instanceof ApiError ? e : new ApiError(0, "Неизвестная ошибка."));
    } finally {
      setLoading(false);
    }
  }, [name, goal, validName]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => setDraft(goal), [goal]);

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    const v = draft.trim();
    if (!GOAL_EVENT_RE.test(v)) {
      setDraftError("Событие: 1–50 символов, латиница, цифры, _ и -");
      return;
    }
    setDraftError(null);
    if (v === goal) void load();
    else setParams(v === DEFAULT_GOAL ? {} : { goal: v });
  }

  if (!validName) return <Alert title="Некорректное имя эксперимента">Проверьте адрес страницы.</Alert>;

  const ev = error ? errorView(error) : null;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="min-w-0">
          <Link to="/" className="text-sm text-blue-700 hover:underline dark:text-blue-400">
            ← Эксперименты
          </Link>
          <h1 className="truncate text-xl font-semibold">{name}</h1>
        </div>
        <form onSubmit={onSubmit} className="flex items-start gap-2" noValidate>
          <div>
            <label htmlFor="goal" className="sr-only">
              Целевое событие
            </label>
            <input
              id="goal"
              className={`${inputCls} w-48`}
              value={draft}
              maxLength={50}
              placeholder="purchase"
              aria-invalid={!!draftError}
              onChange={(e) => setDraft(e.target.value)}
            />
            {draftError && <p className="mt-1 max-w-48 text-xs text-red-700 dark:text-red-300">{draftError}</p>}
          </div>
          <button type="submit" className={btn.primary} disabled={loading}>
            Анализ
          </button>
        </form>
      </div>
      <p className="text-xs text-slate-500 dark:text-slate-400">
        Целевое событие: <span className="font-mono">{goal}</span>
      </p>

      {loading && !stats ? (
        <Spinner label="Считаем статистику…" />
      ) : ev ? (
        <Alert tone={ev.tone} title={ev.title}>
          {ev.text}
        </Alert>
      ) : stats ? (
        <Results stats={stats} />
      ) : null}
    </div>
  );
}
