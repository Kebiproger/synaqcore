import { NavLink, Outlet } from "react-router-dom";
import { useAuth } from "../auth/AuthProvider";
import { btn } from "./ui";

export function Layout() {
  const { displayName, isAdmin, logout } = useAuth();
  const link = ({ isActive }: { isActive: boolean }) =>
    `rounded-md px-3 py-1.5 text-sm font-medium ${
      isActive
        ? "bg-slate-200 text-slate-900 dark:bg-slate-800 dark:text-white"
        : "text-slate-600 hover:text-slate-900 dark:text-slate-400 dark:hover:text-white"
    }`;
  return (
    <div className="min-h-dvh">
      <header className="border-b border-slate-200 bg-white/80 backdrop-blur dark:border-slate-800 dark:bg-slate-900/80">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3">
          <NavLink to="/" className="flex items-center gap-2 font-semibold">
            <img src={`${import.meta.env.BASE_URL}favicon.svg`} alt="" className="h-6 w-6" />
            SynaqCore
          </NavLink>
          <nav className="flex gap-1">
            <NavLink to="/" end className={link}>
              Эксперименты
            </NavLink>
            {isAdmin && (
              <NavLink to="/new" className={link}>
                Создать
              </NavLink>
            )}
          </nav>
          <div className="ml-auto flex items-center gap-3 text-sm">
            <span className="hidden max-w-48 truncate text-slate-600 sm:inline dark:text-slate-400" title={displayName}>
              {displayName}
              <span className="ml-1.5 rounded bg-slate-100 px-1.5 py-0.5 text-xs dark:bg-slate-800">
                {isAdmin ? "admin" : "analyst"}
              </span>
            </span>
            <button type="button" className={btn.secondary} onClick={() => void logout()}>
              Выйти
            </button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6">
        <Outlet />
      </main>
    </div>
  );
}
