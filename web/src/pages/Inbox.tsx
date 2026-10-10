import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type Interaction } from "../lib/api";
import { Empty, ErrorBanner, PageHeader, Spinner } from "../components/ui";
import InteractionCard from "../components/InteractionCard";

export default function Inbox() {
  const [items, setItems] = useState<Interaction[] | null>(null);
  const [all, setAll] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const load = useCallback(() => api.openInteractions(!all).then(setItems).catch(setError), [all]);
  useEffect(() => {
    load();
    const t = setInterval(load, 8000);
    return () => clearInterval(t);
  }, [load]);
  return (
    <>
      <PageHeader title="Inbox" subtitle="Questions and approvals from running agents. Jobs stay paused — durably — until someone answers."
        actions={
          <label className="inline-flex items-center gap-2 text-sm text-slate-600">
            <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} className="h-4 w-4 rounded border-slate-300 text-indigo-600" />
            Show all open requests
          </label>
        } />
      <ErrorBanner error={error} />
      {items === null ? <Spinner label="Loading…" /> : items.length === 0 ? <Empty title="Inbox zero" hint="Nothing is waiting for you." /> : (
        <div className="space-y-4">
          {items.map((i) => (
            <div key={i.id}>
              <div className="mb-1 text-xs text-slate-500">
                <Link to={`/jobs/${i.jobId}`} className="font-mono font-semibold text-indigo-600">{i.jobCommand}</Link> · job {i.jobId}
              </div>
              <InteractionCard it={i} onResolved={load} />
            </div>
          ))}
        </div>
      )}
    </>
  );
}
