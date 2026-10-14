export function num(v: unknown): number {
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) ? n : 0;
}

export function maybeNum(v: unknown): number | null {
  if (v === null || v === undefined || v === "") return null;
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) ? n : null;
}

const intFmt = new Intl.NumberFormat("ru-RU");
export const fmtInt = (v: unknown) => intFmt.format(num(v));

export function fmtPct(v: number | null | undefined, digits = 2): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  return `${v.toFixed(digits).replace(".", ",")}%`;
}

export function fmtSignedPct(v: number | null | undefined, digits = 2): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  const s = v > 0 ? "+" : v < 0 ? "−" : "";
  return `${s}${Math.abs(v).toFixed(digits).replace(".", ",")}%`;
}

export function fmtP(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (v < 0.0001) return "< 0,0001";
  return v.toFixed(4).replace(".", ",");
}

/** Цвет варианта по его порядковому индексу (фиксированный порядок, без циклов). */
export function seriesColor(index: number): string {
  return index < 8 ? `var(--series-${index + 1})` : "var(--chart-axis)";
}
