import type { BasicStats, Experiment, ExperimentCreate, ExperimentPatch, StatsResponse } from "./types";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

interface ApiAuth {
  getToken: () => Promise<string | null>;
  onUnauthorized: () => Promise<void>;
  /** Лимит перелогинов исчерпан: бэкенд отвергает свежие токены. */
  onTokenRejected: () => void;
}

let auth: ApiAuth | null = null;
export function setApiAuth(a: ApiAuth): void {
  auth = a;
}

const RELOGIN_KEY = "synaq.relogin";
const RELOGIN_MAX_ATTEMPTS = 2;
const RELOGIN_WINDOW_MS = 60_000;

let redirecting = false;

interface ReloginState {
  count: number;
  firstAt: number;
}

function readRelogin(): ReloginState {
  try {
    const raw = sessionStorage.getItem(RELOGIN_KEY);
    if (raw) {
      const v = JSON.parse(raw) as Partial<ReloginState>;
      if (typeof v.count === "number" && typeof v.firstAt === "number") return { count: v.count, firstAt: v.firstAt };
    }
  } catch {
    /* sessionStorage недоступен или значение повреждено */
  }
  return { count: 0, firstAt: 0 };
}

function writeRelogin(v: ReloginState): void {
  try {
    sessionStorage.setItem(RELOGIN_KEY, JSON.stringify(v));
  } catch {
    /* ignore */
  }
}

/** Сброс счётчика перелогинов: после успешного авторизованного ответа или ручного повтора. */
export function resetReloginGuard(): void {
  try {
    sessionStorage.removeItem(RELOGIN_KEY);
  } catch {
    /* ignore */
  }
}

/**
 * Защита от бесконечного цикла IdP ↔ дашборд: не больше RELOGIN_MAX_ATTEMPTS
 * перелогинов за RELOGIN_WINDOW_MS. Счётчик сбрасывается только первым
 * успешным (2xx) ответом API с токеном, а не самим callback-ом.
 */
function tryConsumeRelogin(): boolean {
  const now = Date.now();
  let st = readRelogin();
  if (now - st.firstAt > RELOGIN_WINDOW_MS || st.firstAt > now) st = { count: 0, firstAt: now };
  if (st.count >= RELOGIN_MAX_ATTEMPTS) return false;
  writeRelogin({ count: st.count + 1, firstAt: st.firstAt });
  return true;
}

/** Приводит detail FastAPI (строка или список ошибок pydantic) к тексту. */
function formatDetail(body: unknown): string | null {
  if (!body || typeof body !== "object") return null;
  const detail = (body as { detail?: unknown }).detail;
  if (typeof detail === "string") return detail;
  if (Array.isArray(detail)) {
    const msgs = detail
      .map((d) => {
        if (!d || typeof d !== "object") return null;
        const item = d as { msg?: unknown; loc?: unknown };
        const msg = typeof item.msg === "string" ? item.msg : null;
        if (!msg) return null;
        const loc = Array.isArray(item.loc) ? item.loc.filter((x) => x !== "body").join(".") : "";
        return loc ? `${loc}: ${msg}` : msg;
      })
      .filter((x): x is string => !!x);
    if (msgs.length) return msgs.join("; ");
  }
  return null;
}

const DEFAULT_MESSAGES: Record<number, string> = {
  400: "Некорректный запрос.",
  401: "Требуется вход.",
  403: "Недостаточно прав для этого действия.",
  404: "Не найдено.",
  409: "Конфликт состояния.",
  422: "Ошибка валидации данных.",
  502: "Ошибка хранилища аналитики (ClickHouse).",
  503: "Сервис временно недоступен.",
};

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  const token = auth ? await auth.getToken() : null;
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";

  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      credentials: "omit",
      cache: "no-store",
    });
  } catch {
    throw new ApiError(0, "Сеть недоступна или шлюз не отвечает.");
  }

  if (res.status === 401 && auth) {
    // Параллельные 401 одной загрузки страницы расходуют одну попытку, а не несколько.
    if (redirecting) throw new ApiError(401, "Перенаправление на вход…");
    if (tryConsumeRelogin()) {
      redirecting = true;
      try {
        await auth.onUnauthorized();
      } catch {
        redirecting = false;
        throw new ApiError(401, "Не удалось перейти на страницу входа.");
      }
      throw new ApiError(401, "Перенаправление на вход…");
    }
    auth.onTokenRejected();
    throw new ApiError(401, "Сервер отклоняет токен — проверьте настройки OIDC.");
  }

  if (res.ok && token) resetReloginGuard();

  if (res.status === 204) return undefined as T;
  let data: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const msg = formatDetail(data) ?? DEFAULT_MESSAGES[res.status] ?? `Ошибка HTTP ${res.status}.`;
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

const enc = encodeURIComponent;

export const api = {
  listExperiments: () => request<Experiment[]>("GET", "/api/v1/experiments/"),
  createExperiment: (body: ExperimentCreate) => request<Experiment>("POST", "/api/v1/experiments/", body),
  patchExperiment: (name: string, body: ExperimentPatch) =>
    request<Experiment>("PATCH", `/api/v1/experiments/${enc(name)}`, body),
  deleteExperiment: (name: string) => request<unknown>("DELETE", `/api/v1/experiments/${enc(name)}`),
  stats: (name: string, goalEvent: string) =>
    request<StatsResponse>(
      "GET",
      `/api/v1/stats/${enc(name)}?method=both&goal_event=${enc(goalEvent)}`,
    ),
  basicStats: (name: string) => request<BasicStats>("GET", `/api/v1/stats/${enc(name)}/basic`),
};
