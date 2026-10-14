import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import { App } from "./App";
import { AuthProvider } from "./auth/AuthProvider";
import { CALLBACK_PATH, createUserManager, loadOidcConfig, OidcConfigError, safeReturnPath } from "./auth/config";
import { ConfigErrorPage } from "./pages/StatusPages";

const rootEl = document.getElementById("root")!;
const root = createRoot(rootEl);
const inIframe = window.self !== window.top;

async function boot(): Promise<void> {
  let config;
  try {
    config = await loadOidcConfig();
  } catch (e) {
    if (inIframe) return;
    const detail = e instanceof OidcConfigError ? e.message : "Ошибка загрузки конфигурации.";
    root.render(<ConfigErrorPage detail={detail} />);
    return;
  }

  const userManager = createUserManager(config);
  let loginError: string | undefined;

  if (window.location.pathname === CALLBACK_PATH) {
    try {
      // Обрабатывает и обычный redirect, и silent renew в iframe (если нет refresh token).
      const user = await userManager.signinCallback();
      if (inIframe) return;
      // Счётчик перелогинов НЕ сбрасывается здесь: только после первого успешного
      // ответа API (см. api/client.ts), иначе защита от цикла не работает.
      window.history.replaceState(null, "", safeReturnPath(user?.state));
    } catch {
      if (inIframe) return;
      loginError = "Вход не удался. Попробуйте ещё раз.";
      window.history.replaceState(null, "", "/dashboard/");
    }
  } else if (inIframe) {
    return;
  }

  root.render(
    <StrictMode>
      <AuthProvider userManager={userManager} config={config}>
        <App loginError={loginError} />
      </AuthProvider>
    </StrictMode>,
  );
}

void boot();
