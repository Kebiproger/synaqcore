import { BrowserRouter, Route, Routes } from "react-router-dom";
import { useAuth } from "./auth/AuthProvider";
import { BASE_PATH } from "./auth/config";
import { Layout } from "./components/Layout";
import { Spinner } from "./components/ui";
import { CreateExperiment } from "./pages/CreateExperiment";
import { ExperimentResults } from "./pages/ExperimentResults";
import { ExperimentsList } from "./pages/ExperimentsList";
import { LoginPage, NoAccessPage, NotFoundPage, TokenRejectedPage } from "./pages/StatusPages";

export function App({ loginError }: { loginError?: string }) {
  const { user, loading, canView, tokenRejected, retryLogin, login, logout } = useAuth();

  if (loading) {
    return (
      <div className="flex min-h-dvh items-center justify-center">
        <Spinner />
      </div>
    );
  }
  if (tokenRejected) {
    return <TokenRejectedPage onRetry={() => void retryLogin()} onLogout={() => void logout()} />;
  }
  if (!user) return <LoginPage error={loginError} onLogin={() => void login()} />;
  if (!canView) return <NoAccessPage onLogout={() => void logout()} />;

  return (
    <BrowserRouter basename={BASE_PATH}>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<ExperimentsList />} />
          <Route path="new" element={<CreateExperiment />} />
          <Route path="experiments/:name" element={<ExperimentResults />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
