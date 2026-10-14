import type { ReactNode } from "react";
import { btn } from "../components/ui";

function Centered({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-dvh items-center justify-center px-4">
      <div className="w-full max-w-md rounded-xl border border-slate-200 bg-white p-6 text-center shadow-sm dark:border-slate-800 dark:bg-slate-900">
        {children}
      </div>
    </div>
  );
}

export function ConfigErrorPage({ detail }: { detail: string }) {
  return (
    <Centered>
      <h1 className="text-lg font-semibold">Zitadel ещё не инициализирован</h1>
      <p className="mt-2 text-sm text-slate-600 dark:text-slate-300">
        Конфигурация входа пока недоступна. Дождитесь завершения bootstrap-контейнера Zitadel и обновите страницу.
      </p>
      <p className="mt-2 text-xs text-slate-500">{detail}</p>
      <button type="button" className={`${btn.primary} mt-5`} onClick={() => window.location.reload()}>
        Обновить
      </button>
    </Centered>
  );
}

export function LoginPage({ onLogin, error }: { onLogin: () => void; error?: string }) {
  return (
    <Centered>
      <img src={`${import.meta.env.BASE_URL}favicon.svg`} alt="" className="mx-auto h-10 w-10" />
      <h1 className="mt-3 text-lg font-semibold">SynaqCore</h1>
      <p className="mt-1 text-sm text-slate-600 dark:text-slate-300">Панель управления A/B-экспериментами</p>
      {error && <p className="mt-3 text-sm text-red-700 dark:text-red-300">{error}</p>}
      <button type="button" className={`${btn.primary} mt-5 w-full`} onClick={onLogin}>
        Войти через Zitadel
      </button>
    </Centered>
  );
}

export function NoAccessPage({ onLogout }: { onLogout: () => void }) {
  return (
    <Centered>
      <h1 className="text-lg font-semibold">Нет доступа</h1>
      <p className="mt-2 text-sm text-slate-600 dark:text-slate-300">
        У вашей учётной записи нет ролей <b>admin</b> или <b>analyst</b> в проекте SynaqCore. Обратитесь к
        администратору.
      </p>
      <button type="button" className={`${btn.secondary} mt-5`} onClick={onLogout}>
        Выйти
      </button>
    </Centered>
  );
}

export function TokenRejectedPage({ onRetry, onLogout }: { onRetry: () => void; onLogout: () => void }) {
  return (
    <Centered>
      <h1 className="text-lg font-semibold">Сервер отклоняет токен — проверьте настройки OIDC</h1>
      <p className="mt-2 text-sm text-slate-600 dark:text-slate-300">
        Вход в Zitadel прошёл, но API несколько раз подряд ответил 401 на свежий токен. Автоматический повторный
        вход остановлен, чтобы не зациклиться. Проверьте OIDC_ISSUER / OIDC_AUDIENCE / JWKS на бэкенде.
      </p>
      <div className="mt-5 flex justify-center gap-2">
        <button type="button" className={btn.primary} onClick={onRetry}>
          Войти снова
        </button>
        <button type="button" className={btn.secondary} onClick={onLogout}>
          Выйти
        </button>
      </div>
    </Centered>
  );
}

export function NotFoundPage() {
  return (
    <div className="py-16 text-center">
      <h1 className="text-lg font-semibold">Страница не найдена</h1>
    </div>
  );
}
