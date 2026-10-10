import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router-dom";
import { useSession } from "../App";
import { api } from "../lib/api";
import { roleLabel } from "../lib/format";

function NavItem({ to, label, icon, badge }: { to: string; label: string; icon: string; badge?: number }) {
  const { pathname } = useLocation();
  // "/jobs" owns job detail pages, but not the dedicated "Run a command" page.
  const excluded = to === "/jobs" && pathname.startsWith("/jobs/new");
  return (
    <NavLink
      to={to}
      end={to === "/"}
      className={({ isActive: matched }) => {
        const isActive = matched && !excluded;
        return `flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition ${
          isActive ? "bg-indigo-50 text-indigo-700" : "text-slate-600 hover:bg-slate-100 hover:text-slate-900"
        }`;
      }}
    >
      <span aria-hidden className="w-4 text-center">{icon}</span>
      <span className="flex-1">{label}</span>
      {!!badge && <span className="rounded-full bg-amber-500 px-1.5 text-xs font-semibold text-white">{badge}</span>}
    </NavLink>
  );
}

export default function Layout() {
  const { me, can, logout } = useSession();
  const [inbox, setInbox] = useState(0);
  useEffect(() => {
    let alive = true;
    const load = () => api.openInteractions(true).then((r) => alive && setInbox(r.length)).catch(() => {});
    load();
    const t = setInterval(load, 10000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);
  const canRun = me.permissions.some((p) => p.startsWith("execute:"));
  return (
    <div className="flex min-h-screen">
      <aside className="fixed inset-y-0 flex w-60 flex-col border-r border-slate-200 bg-white">
        <div className="flex items-center gap-2 px-5 py-5">
          <svg viewBox="0 0 32 32" className="h-7 w-7"><path d="M16 2 30 28H2Z" className="fill-indigo-600" /><path d="M16 12 22 24H10Z" className="fill-white" /></svg>
          <div>
            <div className="text-base font-semibold tracking-tight">Astrion</div>
            <div className="text-[11px] text-slate-500">Agentic SDLC platform</div>
          </div>
        </div>
        <nav className="flex-1 space-y-1 px-3">
          <NavItem to="/" label="Dashboard" icon="◧" />
          {canRun && <NavItem to="/jobs/new" label="Run a command" icon="▶" />}
          <NavItem to="/jobs" label="Jobs" icon="≡" />
          <NavItem to="/inbox" label="Inbox" icon="✉" badge={inbox} />
          <NavItem to="/workspaces" label="Workspaces" icon="▣" />
          <NavItem to="/registry" label="Agents & registry" icon="⚙" />
          {can("audit:read") && <NavItem to="/audit" label="Audit log" icon="⎙" />}
        </nav>
        <div className="border-t border-slate-200 p-4">
          <div className="truncate text-sm font-medium">{me.user.name || me.user.subject}</div>
          <div className="mt-1 flex flex-wrap gap-1">
            {me.user.roles.map((r) => (
              <span key={r} className="rounded bg-slate-100 px-1.5 py-0.5 text-[11px] text-slate-600">{roleLabel[r] ?? r}</span>
            ))}
          </div>
          <button onClick={logout} className="mt-3 text-xs text-slate-500 hover:text-slate-800">Sign out</button>
        </div>
      </aside>
      <main className="ml-60 flex-1 px-8 py-8">
        <div className="mx-auto max-w-6xl">
          <Outlet />
        </div>
      </main>
    </div>
  );
}
