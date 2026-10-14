import { UserManager, WebStorageStateStore } from "oidc-client-ts";

export interface OidcRuntimeConfig {
  issuer: string;
  project_id: string;
  client_id: string;
}

export const BASE_PATH = "/dashboard";
export const CALLBACK_PATH = `${BASE_PATH}/callback`;

export class OidcConfigError extends Error {}

const ID_RE = /^[A-Za-z0-9@._-]{1,128}$/;

/** Загружает /dashboard/oidc.json (пишется bootstrap-скриптом Zitadel). */
export async function loadOidcConfig(): Promise<OidcRuntimeConfig> {
  let res: Response;
  try {
    res = await fetch(`${BASE_PATH}/oidc.json`, { cache: "no-store", credentials: "omit" });
  } catch {
    throw new OidcConfigError("Не удалось загрузить конфигурацию OIDC.");
  }
  if (!res.ok) throw new OidcConfigError("Файл oidc.json не найден.");
  let raw: unknown;
  try {
    raw = await res.json();
  } catch {
    throw new OidcConfigError("oidc.json повреждён или ещё не записан.");
  }
  if (!raw || typeof raw !== "object") throw new OidcConfigError("oidc.json имеет неверный формат.");
  const r = raw as Record<string, unknown>;
  const { issuer, project_id, client_id } = r;
  if (typeof issuer !== "string" || typeof project_id !== "string" || typeof client_id !== "string") {
    throw new OidcConfigError("В oidc.json нет issuer / project_id / client_id.");
  }
  let issuerUrl: URL;
  try {
    issuerUrl = new URL(issuer);
  } catch {
    throw new OidcConfigError("issuer в oidc.json не является URL.");
  }
  if (issuerUrl.protocol !== "https:" && issuerUrl.protocol !== "http:") {
    throw new OidcConfigError("issuer в oidc.json должен быть http(s) URL.");
  }
  if (!ID_RE.test(project_id) || !ID_RE.test(client_id)) {
    throw new OidcConfigError("project_id / client_id в oidc.json имеют неверный формат.");
  }
  return { issuer: issuer.replace(/\/+$/, ""), project_id, client_id };
}

export function buildScope(projectId: string): string {
  return [
    "openid",
    "profile",
    "email",
    "offline_access",
    `urn:zitadel:iam:org:project:id:${projectId}:aud`,
    "urn:zitadel:iam:org:projects:roles",
  ].join(" ");
}

export function createUserManager(cfg: OidcRuntimeConfig): UserManager {
  const origin = window.location.origin;
  // Токены и состояние запроса — только в sessionStorage (не localStorage).
  const store = new WebStorageStateStore({ store: window.sessionStorage });
  return new UserManager({
    authority: cfg.issuer,
    client_id: cfg.client_id,
    redirect_uri: `${origin}${CALLBACK_PATH}`,
    // Тот же зарегистрированный redirect URI: используется, только если нет refresh token.
    silent_redirect_uri: `${origin}${CALLBACK_PATH}`,
    post_logout_redirect_uri: `${origin}${BASE_PATH}/`,
    response_type: "code",
    scope: buildScope(cfg.project_id),
    userStore: store,
    stateStore: store,
    automaticSilentRenew: true,
    loadUserInfo: false,
    monitorSession: false,
    accessTokenExpiringNotificationTimeInSeconds: 60,
    // При выходе отзываем refresh token на IdP (revocation endpoint).
    revokeTokensOnSignout: true,
    revokeTokenTypes: ["refresh_token"],
  });
}

/** Безопасная цель возврата после логина: только пути внутри /dashboard/. */
export function safeReturnPath(value: unknown): string {
  if (typeof value !== "string") return `${BASE_PATH}/`;
  if (!value.startsWith(`${BASE_PATH}/`) || value.startsWith("//") || value.includes("\\")) {
    return `${BASE_PATH}/`;
  }
  if (value.startsWith(CALLBACK_PATH)) return `${BASE_PATH}/`;
  return value;
}
