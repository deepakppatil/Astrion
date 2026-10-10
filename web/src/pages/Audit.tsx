import { useEffect, useState } from "react";
import { api, type AuditEvent } from "../lib/api";
import { Card, ErrorBanner, PageHeader, Spinner } from "../components/ui";

export default function Audit() {
  const [items, setItems] = useState<AuditEvent[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => { api.audit().then(setItems).catch(setError); }, []);
  return (
    <>
      <PageHeader title="Audit log" subtitle="Append-only record of security-relevant actions. Secret values are never recorded." />
      <ErrorBanner error={error} />
      <Card padded={false}>
        {items === null ? <div className="p-5"><Spinner /></div> : (
          <div className="overflow-x-auto"><table className="w-full table-fixed text-sm">
            <thead className="text-left text-xs uppercase tracking-wide text-slate-500"><tr>
              <th className="w-48 px-5 py-2 font-medium">Time</th><th className="w-56 px-3 py-2 font-medium">Actor</th>
              <th className="w-40 px-3 py-2 font-medium">Action</th><th className="w-72 px-3 py-2 font-medium">Target</th><th className="px-5 py-2 font-medium">Details</th>
            </tr></thead>
            <tbody className="divide-y divide-slate-100">
              {items.map((a) => (
                <tr key={a.id}>
                  <td className="whitespace-nowrap px-5 py-2 text-xs text-slate-500">{new Date(a.createdAt).toLocaleString()}</td>
                  <td className="truncate px-3 py-2 text-xs" title={a.actor}>{a.actor}</td>
                  <td className="px-3 py-2 font-mono text-xs font-medium">{a.action}</td>
                  <td className="truncate px-3 py-2 font-mono text-xs text-slate-500" title={`${a.targetType}/${a.targetId}`}>{a.targetType}/{a.targetId}</td>
                  <td className="truncate px-5 py-2 font-mono text-[11px] text-slate-500" title={a.details ? JSON.stringify(a.details) : ""}>{a.details ? JSON.stringify(a.details) : ""}</td>
                </tr>
              ))}
            </tbody>
          </table></div>
        )}
      </Card>
    </>
  );
}
