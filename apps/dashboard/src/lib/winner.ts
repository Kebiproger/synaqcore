import type { StatsResponse } from "../api/types";

/** Как в бэкенде: control = "A" если есть, иначе "control"; variant — первый другой ключ. */
export function pickPair(variants: Record<string, unknown>): { control: string; variant: string | null } {
  const keys = Object.keys(variants);
  const control = keys.includes("A") ? "A" : "control";
  const variant = keys.find((k) => k !== control) ?? null;
  return { control, variant };
}

export type WinnerResult =
  | { kind: "winner"; name: string }
  | { kind: "none" };

export function deriveWinner(stats: StatsResponse): WinnerResult {
  const { control, variant } = pickPair(stats.variants ?? {});
  const st = stats.statistical;
  const freq = st?.frequentist;
  const diff = st?.sample_rates?.absolute_difference;
  if (!variant || !freq || freq.is_significant !== true || typeof diff !== "number") return { kind: "none" };
  return { kind: "winner", name: diff > 0 ? variant : control };
}
