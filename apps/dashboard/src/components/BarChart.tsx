import { useState } from "react";
import { useWidth } from "./useWidth";
import { fmtInt, fmtPct } from "../lib/format";

export interface BarDatum {
  label: string;
  value: number; // конверсия, %
  users: number;
  conversions: number;
  color: string;
}

const ROW_H = 36;
const BAR_H = 20;
const PAD_TOP = 8;
const AXIS_H = 22;

function niceMax(v: number): number {
  if (v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  const f = v / exp;
  const nf = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10;
  return nf * exp;
}

/** Горизонтальная столбчатая диаграмма конверсии по вариантам (одна шкала, от нуля). */
export function BarChart({ data }: { data: BarDatum[] }) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);

  const labelW = Math.min(140, Math.max(60, width * 0.25));
  const valueW = 64;
  const plotW = Math.max(40, width - labelW - valueW);
  const height = PAD_TOP + data.length * ROW_H + AXIS_H;
  const max = niceMax(Math.max(...data.map((d) => d.value), 0));
  const x = (v: number) => (v / max) * plotW;
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((t) => t * max);

  const hovered = hover !== null ? data[hover] : undefined;

  return (
    <div ref={ref} className="relative w-full">
      <svg width={width} height={height} role="img" aria-label="Конверсия по вариантам" className="block overflow-visible">
        <g transform={`translate(${labelW},${PAD_TOP})`}>
          {ticks.map((t) => (
            <g key={t} transform={`translate(${x(t)},0)`}>
              <line y1={0} y2={data.length * ROW_H} style={{ stroke: "var(--chart-grid)" }} strokeWidth={1} />
              <text
                y={data.length * ROW_H + 16}
                textAnchor="middle"
                className="text-[11px]"
                style={{ fill: "var(--chart-axis)" }}
              >
                {fmtPct(t, max < 5 ? 1 : 0)}
              </text>
            </g>
          ))}
          {data.map((d, i) => {
            const y = i * ROW_H + (ROW_H - BAR_H) / 2;
            const w = Math.max(0, x(d.value));
            const r = Math.min(4, w);
            const path = w > 0
              ? `M0,${y} H${w - r} Q${w},${y} ${w},${y + r} V${y + BAR_H - r} Q${w},${y + BAR_H} ${w - r},${y + BAR_H} H0 Z`
              : "";
            const dim = hover !== null && hover !== i;
            return (
              <g key={d.label}>
                <text
                  x={-10}
                  y={y + BAR_H / 2}
                  dominantBaseline="central"
                  textAnchor="end"
                  className="fill-slate-700 text-xs font-medium dark:fill-slate-200"
                >
                  {d.label.length > 18 ? `${d.label.slice(0, 17)}…` : d.label}
                </text>
                {path && <path d={path} style={{ fill: d.color, opacity: dim ? 0.45 : 1 }} />}
                <text
                  x={w + 6}
                  y={y + BAR_H / 2}
                  dominantBaseline="central"
                  className="fill-slate-700 text-xs tabular-nums dark:fill-slate-200"
                >
                  {fmtPct(d.value)}
                </text>
                <rect
                  x={-labelW}
                  y={i * ROW_H}
                  width={labelW + plotW + valueW}
                  height={ROW_H}
                  fill="transparent"
                  onPointerEnter={() => setHover(i)}
                  onPointerLeave={() => setHover(null)}
                />
              </g>
            );
          })}
        </g>
      </svg>
      {hovered && hover !== null && (
        <div
          className="pointer-events-none absolute z-10 rounded-lg border border-slate-200 bg-white px-3 py-2 text-xs shadow-lg dark:border-slate-700 dark:bg-slate-800"
          style={{ left: labelW + 8, top: PAD_TOP + hover * ROW_H + ROW_H }}
        >
          <div className="mb-1 flex items-center gap-2 font-semibold">
            <span className="h-2.5 w-2.5 rounded-sm" style={{ background: hovered.color }} aria-hidden />
            {hovered.label}
          </div>
          <div className="text-slate-600 dark:text-slate-300">Конверсия: {fmtPct(hovered.value)}</div>
          <div className="text-slate-600 dark:text-slate-300">
            Конвертировались: {fmtInt(hovered.conversions)} из {fmtInt(hovered.users)}
          </div>
        </div>
      )}
    </div>
  );
}
