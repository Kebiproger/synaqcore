import { useMemo, useState, type PointerEvent } from "react";
import { useWidth } from "./useWidth";
import { fmtPct } from "../lib/format";

export interface LineSeries {
  name: string;
  color: string;
  points: Map<string, number>; // day → конверсия, %
}

const H = 260;
const M = { top: 12, right: 16, bottom: 28, left: 48 };

function niceMax(v: number): number {
  if (v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  const f = v / exp;
  const nf = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10;
  return nf * exp;
}

function shortDay(day: string): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(day);
  return m ? `${m[3]}.${m[2]}` : day.slice(0, 10);
}

/** Линейный график дневной конверсии с перекрестием и подсказкой. */
export function LineChart({ series, days }: { series: LineSeries[]; days: string[] }) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);

  const plotW = Math.max(40, width - M.left - M.right);
  const plotH = H - M.top - M.bottom;
  const maxVal = useMemo(() => {
    let m = 0;
    for (const s of series) for (const v of s.points.values()) m = Math.max(m, v);
    return niceMax(m);
  }, [series]);

  const x = (i: number) => (days.length <= 1 ? plotW / 2 : (i / (days.length - 1)) * plotW);
  const y = (v: number) => plotH - (v / maxVal) * plotH;
  const yTicks = [0, 0.25, 0.5, 0.75, 1].map((t) => t * maxVal);
  const labelEvery = Math.max(1, Math.ceil(days.length / Math.max(2, Math.floor(plotW / 56))));

  const paths = series.map((s) => {
    let d = "";
    let pen = false;
    days.forEach((day, i) => {
      const v = s.points.get(day);
      if (v === undefined) {
        pen = false;
        return;
      }
      d += `${pen ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`;
      pen = true;
    });
    return d;
  });

  function onMove(e: PointerEvent<SVGRectElement>) {
    const rect = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - rect.left;
    if (days.length === 0) return;
    const idx = days.length === 1 ? 0 : Math.round((px / plotW) * (days.length - 1));
    setHover(Math.min(days.length - 1, Math.max(0, idx)));
  }

  const hoverDay = hover !== null ? days[hover] : undefined;
  const tooltipLeft = hover !== null ? M.left + x(hover) : 0;
  const flip = tooltipLeft > width * 0.6;

  return (
    <div className="w-full">
      {series.length > 1 && (
        <ul className="mb-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-slate-700 dark:text-slate-300" aria-label="Легенда">
          {series.map((s) => (
            <li key={s.name} className="flex items-center gap-1.5">
              <span className="h-0.5 w-4 rounded" style={{ background: s.color }} aria-hidden />
              {s.name}
            </li>
          ))}
        </ul>
      )}
      <div ref={ref} className="relative w-full">
        <svg width={width} height={H} role="img" aria-label="Дневная конверсия по вариантам" className="block">
          <g transform={`translate(${M.left},${M.top})`}>
            {yTicks.map((t) => (
              <g key={t}>
                <line x1={0} x2={plotW} y1={y(t)} y2={y(t)} style={{ stroke: "var(--chart-grid)" }} />
                <text x={-8} y={y(t)} textAnchor="end" dominantBaseline="central" className="text-[11px]" style={{ fill: "var(--chart-axis)" }}>
                  {fmtPct(t, maxVal < 5 ? 1 : 0)}
                </text>
              </g>
            ))}
            {days.map((d, i) =>
              i % labelEvery === 0 || i === days.length - 1 ? (
                <text key={d} x={x(i)} y={plotH + 18} textAnchor="middle" className="text-[11px]" style={{ fill: "var(--chart-axis)" }}>
                  {shortDay(d)}
                </text>
              ) : null,
            )}
            {hover !== null && (
              <line x1={x(hover)} x2={x(hover)} y1={0} y2={plotH} style={{ stroke: "var(--chart-axis)" }} strokeDasharray="3 3" />
            )}
            {series.map((s, si) => (
              <path
                key={s.name}
                d={paths[si]}
                fill="none"
                strokeWidth={2}
                strokeLinejoin="round"
                strokeLinecap="round"
                style={{ stroke: s.color }}
              />
            ))}
            {series.map((s) => {
              // одиночные точки (и точка под курсором), иначе линия не видна
              return days.map((day, i) => {
                const v = s.points.get(day);
                if (v === undefined) return null;
                const isolated = !s.points.has(days[i - 1] ?? "") && !s.points.has(days[i + 1] ?? "");
                if (hover !== i && !isolated) return null;
                return (
                  <circle
                    key={`${s.name}-${day}`}
                    cx={x(i)}
                    cy={y(v)}
                    r={4}
                    strokeWidth={2}
                    style={{ fill: s.color, stroke: "var(--chart-surface)" }}
                  />
                );
              });
            })}
            <rect
              x={0}
              y={0}
              width={plotW}
              height={plotH}
              fill="transparent"
              onPointerMove={onMove}
              onPointerLeave={() => setHover(null)}
            />
          </g>
        </svg>
        {hoverDay !== undefined && (
          <div
            className="pointer-events-none absolute z-10 min-w-36 rounded-lg border border-slate-200 bg-white px-3 py-2 text-xs shadow-lg dark:border-slate-700 dark:bg-slate-800"
            style={flip ? { right: width - tooltipLeft + 12, top: M.top } : { left: tooltipLeft + 12, top: M.top }}
          >
            <div className="mb-1 font-semibold">{hoverDay.slice(0, 10)}</div>
            {series.map((s) => {
              const v = s.points.get(hoverDay);
              return (
                <div key={s.name} className="flex items-center justify-between gap-3 text-slate-600 dark:text-slate-300">
                  <span className="flex items-center gap-1.5">
                    <span className="h-2 w-2 rounded-full" style={{ background: s.color }} aria-hidden />
                    {s.name}
                  </span>
                  <span className="tabular-nums">{v === undefined ? "—" : fmtPct(v)}</span>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
