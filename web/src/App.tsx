import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { api, auth, type Me } from "./lib/api";
import Layout from "./components/Layout";
import Login from "./pages/Login";
import Dashboard from "./pages/Dashboard";
import NewJob from "./pages/NewJob";
import Jobs from "./pages/Jobs";
import JobDetail from "./pages/JobDetail";
import Inbox from "./pages/Inbox";
import Workspaces from "./pages/Workspaces";
import Registry from "./pages/Registry";
import Audit from "./pages/Audit";
import { Spinner } from "./components/ui";

interface Session {
  me: Me;
  can: (perm: string) => boolean;
  hasRole: (role: string) => boolean;
  logout: () => void;
}
const SessionCtx = createContext<Session | null>(null);
export const useSession = () => {
  const s = useContext(SessionCtx);
  if (!s) throw new Error("no session");
  return s;
};

function SessionProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(!!auth.get());
  const load = useCallback(() => {
    if (!auth.get()) {
      setMe(null);
      setLoading(false);
      return;
    }
    setLoading(true);
    api.me().then(setMe).catch(() => {
      auth.clear();
      setMe(null);
    }).finally(() => setLoading(false));
  }, []);
  useEffect(() => {
    load();
    const onUnauth = () => {
      auth.clear();
      setMe(null);
    };
    window.addEventListener("astrion:unauthorized", onUnauth);
    return () => window.removeEventListener("astrion:unauthorized", onUnauth);
  }, [load]);

  if (loading) return <div className="grid h-screen place-items-center"><Spinner label="Loading…" /></div>;
  if (!me) return <Login onLogin={load} />;
  const perms = new Set(me.permissions);
  const session: Session = {
    me,
    can: (p) => perms.has(p),
    hasRole: (r) => me.user.roles.includes(r),
    logout: () => {
      auth.clear();
      setMe(null);
    },
  };
  return <SessionCtx.Provider value={session}>{children}</SessionCtx.Provider>;
}

export default function App() {
  return (
    <BrowserRouter>
      <SessionProvider>
        <Routes>
          <Route element={<Layout />}>
            <Route index element={<Dashboard />} />
            <Route path="jobs" element={<Jobs />} />
            <Route path="jobs/new" element={<NewJob />} />
            <Route path="jobs/:id" element={<JobDetail />} />
            <Route path="inbox" element={<Inbox />} />
            <Route path="workspaces" element={<Workspaces />} />
            <Route path="registry" element={<Registry />} />
            <Route path="audit" element={<Audit />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Route>
        </Routes>
      </SessionProvider>
    </BrowserRouter>
  );
}
