import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import type { User, UserManager } from "oidc-client-ts";
import { rolesFromAccessToken, type Role } from "./roles";
import type { OidcRuntimeConfig } from "./config";
import { resetReloginGuard, setApiAuth } from "../api/client";

interface AuthState {
  user: User | null;
  loading: boolean;
  roles: Set<Role>;
  isAdmin: boolean;
  canView: boolean;
  displayName: string;
  /** Бэкенд отверг токен после исчерпания лимита перелогинов. */
  tokenRejected: boolean;
  retryLogin: () => Promise<void>;
  login: (returnTo?: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthState | null>(null);

function currentPath(): string {
  return window.location.pathname + window.location.search;
}

export function AuthProvider({
  userManager,
  config,
  children,
}: {
  userManager: UserManager;
  config: OidcRuntimeConfig;
  children: ReactNode;
}) {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);
  const [tokenRejected, setTokenRejected] = useState(false);

  const login = useCallback(
    async (returnTo?: string) => {
      await userManager.signinRedirect({ state: returnTo ?? currentPath() });
    },
    [userManager],
  );

  const retryLogin = useCallback(async () => {
    resetReloginGuard();
    await userManager.removeUser();
    await userManager.signinRedirect({ state: currentPath() });
  }, [userManager]);

  const logout = useCallback(async () => {
    // removeUser() не вызываем заранее: signoutRedirect сам берёт пользователя из
    // хранилища, отзывает refresh token (revokeTokensOnSignout) и затем удаляет его.
    // id_token_hint подставляется библиотекой из сохранённого пользователя.
    try {
      await userManager.signoutRedirect();
    } catch {
      // IdP/revocation недоступен — хотя бы очищаем локальную сессию.
      await userManager.removeUser().catch(() => undefined);
      setUser(null);
    }
  }, [userManager]);

  useEffect(() => {
    let alive = true;
    userManager
      .getUser()
      .then((u) => {
        if (!alive) return;
        setUser(u && !u.expired ? u : null);
      })
      .catch(() => alive && setUser(null))
      .finally(() => alive && setLoading(false));

    const onLoaded = (u: User) => setUser(u);
    const onUnloaded = () => setUser(null);
    const onExpired = () => setUser(null);
    const onRenewError = () => {
      // Refresh не удался: если токен уже истёк — экран входа.
      void userManager.getUser().then((u) => {
        if (!u || u.expired) setUser(null);
      });
    };
    userManager.events.addUserLoaded(onLoaded);
    userManager.events.addUserUnloaded(onUnloaded);
    userManager.events.addAccessTokenExpired(onExpired);
    userManager.events.addSilentRenewError(onRenewError);
    return () => {
      alive = false;
      userManager.events.removeUserLoaded(onLoaded);
      userManager.events.removeUserUnloaded(onUnloaded);
      userManager.events.removeAccessTokenExpired(onExpired);
      userManager.events.removeSilentRenewError(onRenewError);
    };
  }, [userManager]);

  // API-клиент берёт актуальный токен и умеет перелогинить при 401.
  useEffect(() => {
    setApiAuth({
      getToken: async () => {
        const u = await userManager.getUser();
        return u && !u.expired ? u.access_token : null;
      },
      onUnauthorized: async () => {
        await userManager.removeUser();
        await userManager.signinRedirect({ state: currentPath() });
      },
      onTokenRejected: () => setTokenRejected(true),
    });
  }, [userManager]);

  const value = useMemo<AuthState>(() => {
    const roles = rolesFromAccessToken(user?.access_token, config.project_id);
    const profile = user?.profile;
    const displayName =
      (typeof profile?.name === "string" && profile.name) ||
      (typeof profile?.preferred_username === "string" && profile.preferred_username) ||
      (typeof profile?.email === "string" && profile.email) ||
      "";
    return {
      user,
      loading,
      roles,
      isAdmin: roles.has("admin"),
      canView: roles.has("admin") || roles.has("analyst"),
      displayName,
      tokenRejected,
      retryLogin,
      login,
      logout,
    };
  }, [user, loading, config.project_id, tokenRejected, retryLogin, login, logout]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth вне AuthProvider");
  return ctx;
}
