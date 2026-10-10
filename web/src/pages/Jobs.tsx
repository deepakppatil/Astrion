import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type Job } from "../lib/api";
import { duration, timeAgo } from "../lib/format";
import { Button, Card, Empty, ErrorBanner, PageHeader, Spinner, StateBadge } from "../components/ui";

const FILTERS: { label: string; states?: string }[] = [
  { label: "All" },
  { label: "Active", states: "SUBMITTED,VALIDATING,QUEUED,PROVISIONING,PREPARING_WORKSPACE,RUNNING,COLLECTING_ARTIFACTS,CLEANING_UP" },
  { label: "Waiting", states: "WAITING_FOR_INPUT,WAITING_FOR_APPROVAL" },
  { label: "Needs intervention", states: "NEEDS_INTERVENTION" },
  { label: "Succeeded", states: "SUCCEEDED" },
  { label: "Failed", states: "FAILED,TIMED_OUT,CANCELLED" },
];

export default function Jobs() {
  const [filter, setFilter] = useState(0);
  const [mine, setMine] = useState(false);
  const [jobs, setJobs] = useState<Job[] | null>(null);
  const [next, setNext] = useState<string | undefined>();
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    let alive = true;
    const load = () => api.jobs({ state: FILTERS[filter].states, mine }).then((r) => {
      if (!alive) return;
      setJobs(r.items);
      setNext(r.nextBefore);
    }).catch(setError);
    setJobs(null);
    load();
    const t = setInterval(load, 5000);
    return () => { alive = false; clearInterval(t); };
  }, [filter, mine]);

  const more = () => api.jobs({ state: FILTERS[filter].states, mine, before: next }).then((r) => {
    setJobs((j) => [...(j ?? []), ...r.items]);
    setNext(r.nextBefore);
  }).catch(setError);

  return (
    <>
      <PageHeader title="Jobs" subtitle="Every command execution, its state and outcome." />
      <div className="mb-4 flex flex-wrap items-center gap-2">
        {FILTERS.map((f, i) => (
          <button key={f.label} onClick={() => setFilter(i)}
            className={`rounded-full px-3 py-1 text-sm ${i === filter ? "bg-slate-900 text-white" : "bg-white text-slate-600 ring-1 ring-slate-200 hover:bg-slate-50"}`}>
            {f.label}
          </button>
        ))}
        <label className="ml-auto inline-flex items-center gap-2 text-sm text-slate-600">
          <input type="checkbox" checked={mine} onChange={(e) => setMine(e.target.checked)} className="h-4 w-4 rounded border-slate-300 text-indigo-600" />
          Only mine
        </label>
      </div>
      <ErrorBanner error={error} />
      <Card padded={false}>
        {jobs === null ? <div className="p-5"><Spinner label="Loading jobs…" /></div> : jobs.length === 0 ? <div className="p-5"><Empty title="No jobs match" /></div> : (
          <table className="w-full text-sm">
            <thead className="border-b border-slate-100 text-left text-xs uppercase tracking-wide text-slate-500">
              <tr>
                <th className="px-5 py-2.5 font-medium">Command</th>
                <th className="px-3 py-2.5 font-medium">Phase</th>
                <th className="px-3 py-2.5 font-medium">Step</th>
                <th className="px-3 py-2.5 font-medium">Submitted</th>
                <th className="px-3 py-2.5 font-medium">Duration</th>
                <th className="px-5 py-2.5 text-right font-medium">State</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100">
              {jobs.map((j) => (
                <tr key={j.id} className="hover:bg-slate-50">
                  <td className="px-5 py-3"><Link to={`/jobs/${j.id}`} className="font-mono font-semibold text-slate-900 hover:text-indigo-600">{j.command}</Link></td>
                  <td className="px-3 py-3 text-slate-500">{j.phase.toLowerCase()}</td>
                  <td className="max-w-xs truncate px-3 py-3 text-slate-500">{j.failureMessage || j.currentStep || "—"}</td>
                  <td className="px-3 py-3 text-slate-500">{timeAgo(j.createdAt)}</td>
                  <td className="px-3 py-3 text-slate-500">{duration(j.createdAt, j.finishedAt)}</td>
                  <td className="px-5 py-3 text-right"><StateBadge state={j.state} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
      {next && <div className="mt-4 text-center"><Button onClick={more}>Load more</Button></div>}
    </>
  );
}
