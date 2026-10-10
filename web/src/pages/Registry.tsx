import { useEffect, useState } from "react";
import { api, PHASES, type AgentProfile, type Command, type Framework, type Pool } from "../lib/api";
import { roleLabel } from "../lib/format";
import { Badge, Card, ErrorBanner, PageHeader, Spinner } from "../components/ui";

export default function Registry() {
  const [agents, setAgents] = useState<AgentProfile[] | null>(null);
  const [frameworks, setFrameworks] = useState<Framework[]>([]);
  const [commands, setCommands] = useState<Command[]>([]);
  const [pools, setPools] = useState<Pool[]>([]);
  const [integrations, setIntegrations] = useState<any[]>([]);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    Promise.all([api.agents(), api.frameworks(), api.pools(), api.integrations()]).then(async ([a, f, p, i]) => {
      setAgents(a);
      setFrameworks(f);
      setPools(p.items);
      setIntegrations(i);
      if (f[0]) setCommands(await api.commands(f[0].id));
    }).catch(setError);
  }, []);
  if (!agents) return error ? <ErrorBanner error={error} /> : <Spinner />;
  return (
    <>
      <PageHeader title="Agents & registry" subtitle="Pluggable agents, metadata-driven framework commands and execution pools." />
      <div className="grid gap-5 lg:grid-cols-2">
        <Card title="Agent profiles" padded={false}>
          <ul className="divide-y divide-slate-100">
            {agents.map((a) => (
              <li key={a.id} className="px-5 py-3 text-sm">
                <div className="flex items-center justify-between">
                  <span className="font-medium">{a.name}</span>
                  <Badge tone={a.enabled ? "green" : "slate"}>{a.enabled ? "enabled" : "disabled"}</Badge>
                </div>
                <div className="mt-1 text-xs text-slate-500">adapter <code>{a.provider}</code> · {a.cliName} {a.cliVersion} · v{a.version} · {a.interactionMode.replace(/_/g, " ").toLowerCase() || "adapter default"}</div>
              </li>
            ))}
          </ul>
        </Card>
        <Card title="Execution pools" padded={false}>
          <ul className="divide-y divide-slate-100">
            {pools.map((p) => (
              <li key={p.name} className="flex items-center gap-2 px-5 py-3 text-sm">
                <span className="flex-1 font-medium">{p.name}</span>
                <Badge>{p.type}</Badge><Badge tone={p.network === "PRIVATE" ? "indigo" : "amber"}>{p.network.toLowerCase()}</Badge>
                <Badge>≤ {p.maxClassification.toLowerCase()}</Badge>
              </li>
            ))}
          </ul>
        </Card>
      </div>
      {frameworks.map((f) => (
        <Card key={f.id} className="mt-5" title={<span>{f.name} <span className="font-normal text-slate-500">· {f.version} · {f.revision}</span></span>} padded={false}>
          {PHASES.map((ph) => {
            const cs = commands.filter((c) => c.phase === ph);
            if (!cs.length) return null;
            return (
              <div key={ph} className="border-t border-slate-100 px-5 py-3 first:border-t-0">
                <div className="mb-2 text-xs font-semibold uppercase tracking-wide text-slate-500">{ph.toLowerCase()}</div>
                <div className="grid gap-2 md:grid-cols-2">
                  {cs.map((c) => (
                    <div key={c.id} className="rounded-lg border border-slate-200 px-3 py-2">
                      <div className="flex items-center justify-between">
                        <code className="text-sm font-semibold">{c.canonical}</code>
                        <span className="text-[11px] text-slate-400">v{c.version}</span>
                      </div>
                      <p className="mt-0.5 text-xs text-slate-500">{c.description}</p>
                      <div className="mt-1.5 flex flex-wrap gap-1">
                        {c.personas.map((p) => <Badge key={p}>{roleLabel[p] ?? p}</Badge>)}
                        {c.sideEffects.filter((s) => s !== "NONE").map((s) => <Badge key={s} tone="amber">{s.toLowerCase().replace(/_/g, " ")}</Badge>)}
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            );
          })}
        </Card>
      ))}
      <Card title="Integrations" className="mt-5" padded={false}>
        <ul className="divide-y divide-slate-100">
          {integrations.map((i) => (
            <li key={i.id} className="flex items-center gap-3 px-5 py-3 text-sm">
              <span className="w-40 font-medium">{i.name}</span>
              <Badge tone={i.status === "available" ? "green" : "slate"}>{i.status.replace(/_/g, " ")}</Badge>
              <span className="flex-1 text-xs text-slate-500">{i.notes}</span>
            </li>
          ))}
        </ul>
      </Card>
    </>
  );
}
