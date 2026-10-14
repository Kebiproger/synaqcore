import { useMemo, useRef, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, ApiError } from "../api/client";
import { useAuth } from "../auth/AuthProvider";
import { Alert, btn, Card, inputCls } from "../components/ui";
import { seriesColor } from "../lib/format";
import { hasErrors, MAX_VARIANTS, parseWeight, validateForm, type FormErrors, type VariantRow } from "../lib/validation";

function FieldError({ id, msg }: { id: string; msg?: string }) {
  return msg ? (
    <p id={id} className="mt-1 text-xs text-red-700 dark:text-red-300">
      {msg}
    </p>
  ) : null;
}

export function CreateExperiment() {
  const { isAdmin } = useAuth();
  const navigate = useNavigate();
  const nextId = useRef(3);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [isActive, setIsActive] = useState(true);
  const [rows, setRows] = useState<VariantRow[]>([
    { id: 1, name: "A", weight: "50" },
    { id: 2, name: "B", weight: "50" },
  ]);
  const [submitted, setSubmitted] = useState(false);
  const [saving, setSaving] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);

  const errors: FormErrors = useMemo(() => validateForm(name, url, rows), [name, url, rows]);
  const shown = submitted ? errors : { rows: {} as FormErrors["rows"] };

  const total = rows.reduce((s, r) => s + (parseWeight(r.weight) ?? 0), 0);

  if (!isAdmin) {
    return <Alert title="Недостаточно прав">Создавать эксперименты может только администратор.</Alert>;
  }

  function updateRow(id: number, patch: Partial<VariantRow>) {
    setRows((prev) => prev.map((r) => (r.id === id ? { ...r, ...patch } : r)));
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setSubmitted(true);
    setServerError(null);
    if (hasErrors(errors)) return;
    const variants: Record<string, number> = {};
    for (const r of rows) variants[r.name.trim()] = parseWeight(r.weight) ?? 0;
    setSaving(true);
    try {
      const trimmedUrl = url.trim();
      await api.createExperiment({
        name: name.trim(),
        variants,
        url: trimmedUrl ? trimmedUrl : null,
        is_active: isActive,
      });
      navigate("/");
    } catch (err) {
      setServerError(err instanceof ApiError ? err.message : "Не удалось создать эксперимент.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="mx-auto max-w-2xl space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">Новый эксперимент</h1>
        <Link to="/" className="text-sm text-blue-700 hover:underline dark:text-blue-400">
          ← к списку
        </Link>
      </div>
      <Card>
        <form className="space-y-5" onSubmit={(e) => void onSubmit(e)} noValidate>
          <div>
            <label htmlFor="exp-name" className="mb-1 block text-sm font-medium">
              Имя
            </label>
            <input
              id="exp-name"
              className={inputCls}
              value={name}
              maxLength={50}
              autoComplete="off"
              placeholder="checkout_button_color"
              aria-invalid={!!shown.name}
              aria-describedby="exp-name-err"
              onChange={(e) => setName(e.target.value)}
            />
            <p className="mt-1 text-xs text-slate-500">Латиница, цифры, «_», «.», «-»; до 50 символов.</p>
            <FieldError id="exp-name-err" msg={shown.name} />
          </div>

          <div>
            <label htmlFor="exp-url" className="mb-1 block text-sm font-medium">
              URL-паттерн <span className="font-normal text-slate-500">(необязательно)</span>
            </label>
            <input
              id="exp-url"
              className={inputCls}
              value={url}
              maxLength={200}
              autoComplete="off"
              placeholder="/checkout"
              aria-invalid={!!shown.url}
              aria-describedby="exp-url-err"
              onChange={(e) => setUrl(e.target.value)}
            />
            <FieldError id="exp-url-err" msg={shown.url} />
          </div>

          <fieldset>
            <legend className="mb-2 text-sm font-medium">Варианты и веса</legend>
            <div className="space-y-2">
              {rows.map((row, i) => {
                const w = parseWeight(row.weight);
                const pct = w && total ? (w / total) * 100 : 0;
                const re = shown.rows[row.id];
                return (
                  <div key={row.id}>
                    <div className="flex items-center gap-2">
                      <span className="h-3 w-3 shrink-0 rounded-full" style={{ background: seriesColor(i) }} aria-hidden />
                      <input
                        className={`${inputCls} min-w-0 flex-1`}
                        value={row.name}
                        maxLength={50}
                        aria-label={`Имя варианта ${i + 1}`}
                        aria-invalid={!!re?.name}
                        onChange={(e) => updateRow(row.id, { name: e.target.value })}
                      />
                      <input
                        className={`${inputCls} w-24 tabular-nums`}
                        value={row.weight}
                        inputMode="numeric"
                        aria-label={`Вес варианта ${i + 1}`}
                        aria-invalid={!!re?.weight}
                        onChange={(e) => updateRow(row.id, { weight: e.target.value })}
                      />
                      <span className="w-14 text-right text-sm tabular-nums text-slate-600 dark:text-slate-300">
                        {pct.toFixed(1).replace(".", ",")}%
                      </span>
                      <button
                        type="button"
                        className={`${btn.secondary} px-2`}
                        aria-label={`Удалить вариант ${i + 1}`}
                        disabled={rows.length <= 1}
                        onClick={() => setRows((prev) => prev.filter((r) => r.id !== row.id))}
                      >
                        ✕
                      </button>
                    </div>
                    {(re?.name || re?.weight) && (
                      <p className="ml-5 mt-1 text-xs text-red-700 dark:text-red-300">
                        {[re.name, re.weight].filter(Boolean).join(" ")}
                      </p>
                    )}
                  </div>
                );
              })}
            </div>

            {total > 0 && (
              <div className="mt-3 flex h-2.5 w-full gap-0.5 overflow-hidden rounded-full" aria-hidden>
                {rows.map((r, i) => {
                  const w = parseWeight(r.weight);
                  return w ? (
                    <div key={r.id} style={{ flexGrow: w, background: seriesColor(i) }} title={r.name} />
                  ) : null;
                })}
              </div>
            )}

            <div className="mt-3 flex items-center justify-between">
              <button
                type="button"
                className={btn.secondary}
                disabled={rows.length >= MAX_VARIANTS}
                onClick={() => {
                  const id = nextId.current++;
                  const letter = String.fromCharCode(65 + Math.min(rows.length, 25));
                  setRows((prev) => [...prev, { id, name: letter, weight: "50" }]);
                }}
              >
                + Добавить вариант
              </button>
              <span className="text-xs text-slate-500">
                {rows.length} / {MAX_VARIANTS} · сумма весов {total}
              </span>
            </div>
            <FieldError id="variants-err" msg={shown.variants} />
          </fieldset>

          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              className="h-4 w-4 rounded border-slate-300 accent-blue-600"
              checked={isActive}
              onChange={(e) => setIsActive(e.target.checked)}
            />
            Запустить сразу (активен)
          </label>

          {serverError && <Alert title="Сервер отклонил запрос">{serverError}</Alert>}
          {submitted && hasErrors(errors) && <Alert tone="warn">Исправьте ошибки в форме.</Alert>}

          <div className="flex justify-end gap-2">
            <Link to="/" className={btn.secondary}>
              Отмена
            </Link>
            <button type="submit" className={btn.primary} disabled={saving}>
              {saving ? "Создание…" : "Создать"}
            </button>
          </div>
        </form>
      </Card>
    </div>
  );
}
