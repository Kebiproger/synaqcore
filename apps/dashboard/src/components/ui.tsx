import { useEffect, useRef, type ReactNode } from "react";

export function Spinner({ label = "Загрузка…" }: { label?: string }) {
  return (
    <div className="flex items-center gap-3 py-8 text-slate-500 dark:text-slate-400" role="status">
      <span className="h-5 w-5 animate-spin rounded-full border-2 border-slate-300 border-t-blue-600 dark:border-slate-700 dark:border-t-blue-400" />
      <span>{label}</span>
    </div>
  );
}

type Tone = "error" | "warn" | "info";
const toneCls: Record<Tone, string> = {
  error: "border-red-300 bg-red-50 text-red-800 dark:border-red-900 dark:bg-red-950/50 dark:text-red-200",
  warn: "border-amber-300 bg-amber-50 text-amber-900 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-200",
  info: "border-slate-300 bg-white text-slate-700 dark:border-slate-700 dark:bg-slate-900 dark:text-slate-300",
};

export function Alert({ tone = "error", title, children }: { tone?: Tone; title?: string; children?: ReactNode }) {
  return (
    <div className={`rounded-lg border px-4 py-3 text-sm ${toneCls[tone]}`} role={tone === "error" ? "alert" : "status"}>
      {title && <div className="mb-0.5 font-semibold">{title}</div>}
      {children}
    </div>
  );
}

export function StatusBadge({ active }: { active: boolean }) {
  return active ? (
    <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-100 px-2.5 py-0.5 text-xs font-medium text-emerald-800 dark:bg-emerald-900/50 dark:text-emerald-200">
      <span className="h-1.5 w-1.5 rounded-full bg-emerald-600 dark:bg-emerald-400" aria-hidden />
      активен
    </span>
  ) : (
    <span className="inline-flex items-center gap-1.5 rounded-full bg-slate-200 px-2.5 py-0.5 text-xs font-medium text-slate-700 dark:bg-slate-800 dark:text-slate-300">
      <span className="h-1.5 w-1.5 rounded-full bg-slate-500" aria-hidden />
      остановлен
    </span>
  );
}

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return (
    <section
      className={`rounded-xl border border-slate-200 bg-white p-4 shadow-sm sm:p-5 dark:border-slate-800 dark:bg-slate-900 ${className}`}
    >
      {children}
    </section>
  );
}

export const btn = {
  primary:
    "inline-flex items-center justify-center gap-2 rounded-lg bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-700 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-600 disabled:cursor-not-allowed disabled:opacity-50",
  secondary:
    "inline-flex items-center justify-center gap-2 rounded-lg border border-slate-300 bg-white px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-600 disabled:cursor-not-allowed disabled:opacity-50 dark:border-slate-700 dark:bg-slate-900 dark:text-slate-200 dark:hover:bg-slate-800",
  danger:
    "inline-flex items-center justify-center gap-2 rounded-lg border border-red-300 bg-white px-3 py-1.5 text-sm font-medium text-red-700 hover:bg-red-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-red-600 disabled:cursor-not-allowed disabled:opacity-50 dark:border-red-900 dark:bg-slate-900 dark:text-red-300 dark:hover:bg-red-950/40",
};

export const inputCls =
  "w-full rounded-lg border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 placeholder:text-slate-400 focus:border-blue-500 focus:outline-none focus:ring-2 focus:ring-blue-500/30 dark:border-slate-700 dark:bg-slate-950 dark:text-slate-100 aria-[invalid=true]:border-red-500";

export function ConfirmDialog({
  open,
  title,
  message,
  confirmLabel,
  busy,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  message: ReactNode;
  confirmLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      onCancel={(e) => {
        e.preventDefault();
        if (!busy) onCancel();
      }}
      className="m-auto w-[min(28rem,calc(100vw-2rem))] rounded-xl border border-slate-200 bg-white p-0 text-slate-900 shadow-xl backdrop:bg-black/50 dark:border-slate-700 dark:bg-slate-900 dark:text-slate-100"
    >
      <div className="p-5">
        <h2 className="text-lg font-semibold">{title}</h2>
        <div className="mt-2 text-sm text-slate-600 dark:text-slate-300">{message}</div>
        <div className="mt-5 flex justify-end gap-2">
          <button type="button" className={btn.secondary} onClick={onCancel} disabled={busy}>
            Отмена
          </button>
          <button type="button" className={btn.danger} onClick={onConfirm} disabled={busy}>
            {busy ? "Удаление…" : confirmLabel}
          </button>
        </div>
      </div>
    </dialog>
  );
}
