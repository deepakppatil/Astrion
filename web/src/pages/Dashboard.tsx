import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type Dashboard as Stats, type Interaction, type Job } from "../lib/api";
import { duration, timeAgo } from "../lib/format";
import { Card, Empty, ErrorBanner, PageHeader, Spinner, StateBadge, Button } from "../components/ui";
import { useSession } from "../App";

function Stat({ label, value, tone = "text-slate-900" }: { label: string; value: string | number; tone?: string }) {
  return (
    <div className="rounded-xl border border-slate-200 bg-white px-5 py-4 shadow-sm">
      <div className="text-xs font-medium uppercase tracking-wide text-slate-500">{label}</div>
      <div className={`mt-1 text-2xl font-semibold ${tone}`}>{value}</div>
    </div>
  );
}

export default function Dashboard() {
  const { me } = useSession();
  const [stats, setStats] = useState<Stats | null>(null);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [inbox, setInbox] = useState<Interaction[]>([]);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    const load = () =>
      Promise.all([api.dashboard(), api.jobs({}), api.openInteractions(true)])
        .then(([s, j, i]) => { setStats(s); setJobs(j.items.slice(0, 8)); setInbox(i); })
        .catch(setError);
    load();
    const t = setInterval(load, 8000);
    return () => clearInterval(t);
  }, []);

  const canRun = me.permissions.some((p) => p.startsWith("execute:"));
  return (
    <>
      <PageHeader title={`Welcome back, ${me.user.name || me.user.subject}`} subtitle="What your agents are working on right now."
        actions={canRun && <Link to="/jobs/new"><Button variant="primary">▶ Run a command</Button></Link>} />
      <ErrorBanner error={error} />
      {!stats ? <Spinner label="Loading dashboard…" /> : (
        <div className="grid grid-cols-2 gap-4 lg:grid-cols-5">
          <Stat label="Active jobs" value={stats.active} />
          <Stat label="Awaiting humans" value={stats.awaitingInput} tone={stats.awaitingInput ? "text-amber-600" : undefined} />
          <Stat label="Succeeded (24h)" value={stats.succeeded24h} tone="text-emerald-600" />
          <Stat label="Failed (24h)" value={stats.failed24h} tone={stats.failed24h ? "text-rose-600" : undefined} />
          <Stat label="Avg. duration" value={stats.avgDurationSeconds ? `${Math.round(stats.avgDurationSeconds)}s` : "—"} />
        </div>
      )}
      <div className="mt-6 grid gap-6 lg:grid-cols-3">
        <Card title="Recent jobs" className="lg:col-span-2" padded={false} actions={<Link className="text-xs text-indigo-600" to="/jobs">View all</Link>}>
          {jobs.length === 0 ? <div className="p-5"><Empty title="No jobs yet" hint="Run a framework command to get started." /></div> : (
            <ul className="divide-y divide-slate-100">
              {jobs.map((j) => (
                <li key={j.id}>
                  <Link to={`/jobs/${j.id}`} className="flex items-center gap-4 px-5 py-3 hover:bg-slate-50">
                    <code className="w-40 truncate text-sm font-semibold text-slate-800">{j.command}</code>
                    <span className="flex-1 truncate text-sm text-slate-500">{j.failureMessage || j.currentStep || "—"}</span>
                    <span className="w-20 text-right text-xs text-slate-400">{duration(j.createdAt, j.finishedAt)}</span>
                    <StateBadge state={j.state} />
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Card>
        <Card title="Waiting for you" padded={false} actions={<Link className="text-xs text-indigo-600" to="/inbox">Inbox</Link>}>
          {inbox.length === 0 ? <div className="p-5"><Empty title="Nothing needs your attention" /></div> : (
            <ul className="divide-y divide-slate-100">
              {inbox.slice(0, 6).map((i) => (
                <li key={i.id}>
                  <Link to={`/jobs/${i.jobId}`} className="block px-5 py-3 hover:bg-slate-50">
                    <div className="flex items-center justify-between">
                      <code className="text-xs font-semibold text-slate-700">{i.jobCommand}</code>
                      <span className="text-xs text-slate-400">{timeAgo(i.openedAt)}</span>
                    </div>
                    <p className="mt-1 line-clamp-2 text-sm text-slate-600">{i.prompt}</p>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </>
  );
}
