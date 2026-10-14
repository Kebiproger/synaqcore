export const EXPERIMENT_NAME_RE = /^[A-Za-z0-9_.-]+$/;
export const VARIANT_KEY_RE = /^[A-Za-z0-9_-]+$/;
export const GOAL_EVENT_RE = /^[A-Za-z0-9_-]{1,50}$/;
export const RESERVED_NAMES = new Set(["active", "config"]);
export const MAX_VARIANTS = 20;
export const MAX_URL_LENGTH = 200;

export interface VariantRow {
  id: number;
  name: string;
  weight: string;
}

export interface FormErrors {
  name?: string;
  url?: string;
  variants?: string;
  rows: Record<number, { name?: string; weight?: string }>;
}

export function validateExperimentName(name: string): string | undefined {
  if (!name) return "Укажите имя эксперимента.";
  if (name.length > 50) return "Имя не длиннее 50 символов.";
  if (!EXPERIMENT_NAME_RE.test(name)) return "Допустимы только латиница, цифры и символы _ . -";
  if (RESERVED_NAMES.has(name.toLowerCase())) return `Имя «${name}» зарезервировано.`;
  return undefined;
}

export function parseWeight(raw: string): number | null {
  if (!/^\d{1,9}$/.test(raw.trim())) return null;
  const n = Number(raw.trim());
  return Number.isSafeInteger(n) && n > 0 ? n : null;
}

export function validateForm(name: string, url: string, rows: VariantRow[]): FormErrors {
  const errors: FormErrors = { rows: {} };
  const nameErr = validateExperimentName(name.trim());
  if (nameErr) errors.name = nameErr;
  if (url.length > MAX_URL_LENGTH) errors.url = `URL не длиннее ${MAX_URL_LENGTH} символов.`;

  if (rows.length < 1) errors.variants = "Нужен хотя бы один вариант.";
  else if (rows.length > MAX_VARIANTS) errors.variants = `Не больше ${MAX_VARIANTS} вариантов.`;

  const seen = new Set<string>();
  for (const row of rows) {
    const e: { name?: string; weight?: string } = {};
    const key = row.name.trim();
    if (!key) e.name = "Укажите имя варианта.";
    else if (key.length > 50) e.name = "Не длиннее 50 символов.";
    else if (!VARIANT_KEY_RE.test(key)) e.name = "Только латиница, цифры, _ и -";
    else if (seen.has(key)) e.name = "Имена вариантов должны быть уникальны.";
    seen.add(key);
    if (parseWeight(row.weight) === null) e.weight = "Вес — целое число больше 0.";
    if (e.name || e.weight) errors.rows[row.id] = e;
  }
  return errors;
}

export function hasErrors(e: FormErrors): boolean {
  return !!(e.name || e.url || e.variants || Object.keys(e.rows).length);
}
