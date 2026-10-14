export type Role = "admin" | "analyst";

function decodeJwtPayload(token: string): Record<string, unknown> | null {
  const parts = token.split(".");
  if (parts.length !== 3 || !parts[1]) return null;
  try {
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
    const bin = atob(padded);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    const json = new TextDecoder().decode(bytes);
    const payload: unknown = JSON.parse(json);
    return payload && typeof payload === "object" ? (payload as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

/**
 * Роли из проектного claim access token'а. Только для UI (скрыть кнопки);
 * реальную проверку подписи и ролей делает бэкенд.
 */
export function rolesFromAccessToken(accessToken: string | undefined, projectId: string): Set<Role> {
  const roles = new Set<Role>();
  if (!accessToken) return roles;
  const payload = decodeJwtPayload(accessToken);
  if (!payload) return roles;
  const aud = payload.aud;
  const audList = Array.isArray(aud) ? aud : [aud];
  if (!audList.includes(projectId)) return roles;
  const claim = payload[`urn:zitadel:iam:org:project:${projectId}:roles`];
  if (claim && typeof claim === "object" && !Array.isArray(claim)) {
    for (const key of Object.keys(claim)) {
      if (key === "admin" || key === "analyst") roles.add(key);
    }
  }
  return roles;
}
