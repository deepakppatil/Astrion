import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError, newIdempotencyKey, PHASES, type AgentProfile, type Command, type InputField, type Phase, type Pool, type Repository, type Workspace } from "../lib/api";
import { Badge, Button, Card, Empty, ErrorBanner, Field, inputCls, PageHeader, Spinner } from "../components/ui";

const phaseBlurb: Record<Phase, string> = {
  PLAN: "Shape ideas into specs, tasks and tickets",
  DESIGN: "Build context and architecture",
  IMPLEMENT: "Write code on a working branch",
  TEST: "Run and report on test suites",
  DEPLOY: "Prepare and execute deployments",
  MAINTAIN: "Diagnose and keep systems healthy",
};

function InputControl({ f, value, onChange }: { f: InputField; value: unknown; onChange: (v: unknown) => void }) {
  switch (f.type) {
    case "text":
      return <textarea className={inputCls} rows={4} maxLength={f.maxLength || undefined} value={(value as string) ?? ""} onChange={(e) => onChange(e.target.value)} />;
    case "boolean":
      return (
        <label className="inline-flex items-center gap-2 text-sm">
          <input type="checkbox" className="h-4 w-4 rounded border-slate-300 text-indigo-600" checked={!!value} onChange={(e) => onChange(e.target.checked)} />
          Enabled
        </label>
      );
    case "integer":
      return <input type="number" className={inputCls} value={(value as number) ?? ""} onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))} />;
    case "enum":
      return (
        <select className={inputCls} value={(value as string) ?? ""} onChange={(e) => onChange(e.target.value || undefined)}>
          <option value="">Select…</option>
          {f.enum?.map((o) => <option key={o} value={o}>{o}</option>)}
        </select>
      );
    default:
      return <input className={inputCls} maxLength={f.maxLength || undefined} placeholder={f.type === "artifactRef" ? "art_…" : f.type === "jiraKey" ? "PROJ-123" : ""}
        value={(value as string) ?? ""} onChange={(e) => onChange(e.target.value)} />;
  }
}

export default function NewJob() {
  const nav = useNavigate();
  const [workspaces, setWorkspaces] = useState<Workspace[] | null>(null);
  const [wsId, setWsId] = useState("");
  const [phase, setPhase] = useState<Phase>("PLAN");
  const [commands, setCommands] = useState<Command[] | null>(null);
  const [cmd, setCmd] = useState<Command | null>(null);
  const [inputs, setInputs] = useState<Record<string, unknown>>({});
  const [agents, setAgents] = useState<AgentProfile[]>([]);
  const [agentId, setAgentId] = useState("");
  const [repos, setRepos] = useState<Repository[]>([]);
  const [selectedRepos, setSelectedRepos] = useState<Record<string, string>>({});
  const [pools, setPools] = useState<{ items: Pool[]; canSelect: boolean }>({ items: [], canSelect: false });
  const [provider, setProvider] = useState("AUTO");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  // One key per submission attempt: double clicks and retries after a
  // network error never create duplicate jobs.
  const [idemKey, setIdemKey] = useState(newIdempotencyKey);

  const ws = workspaces?.find((w) => w.id === wsId);

  useEffect(() => {
    Promise.all([api.workspaces(), api.agents(), api.pools()]).then(([w, a, p]) => {
      setWorkspaces(w);
      setAgents(a.filter((x) => x.enabled));
      setPools(p);
      if (w.length) setWsId(w[0].id);
    }).catch(setError);
  }, []);

  useEffect(() => {
    if (!ws) return;
    setAgentId(ws.defaultAgentProfileId ?? "");
    api.repositories(ws.id).then((r) => {
      setRepos(r);
      setSelectedRepos(Object.fromEntries(r.map((x) => [x.id, x.defaultBranch])));
    }).catch(setError);
  }, [ws]);

  const agent = agents.find((a) => a.id === agentId);
  useEffect(() => {
    if (!ws?.defaultFrameworkId) return;
    setCommands(null);
    setCmd(null);
    api.commands(ws.defaultFrameworkId, phase, agent?.provider).then(setCommands).catch(setError);
  }, [ws, phase, agent?.provider]);

  const selectCommand = (c: Command) => {
    setCmd(c);
    setError(null);
    setInputs(Object.fromEntries(c.inputs.filter((f) => f.default !== undefined).map((f) => [f.name, f.default])));
    setIdemKey(newIdempotencyKey());
  };

  const missing = useMemo(() => cmd?.inputs.filter((f) => f.required && (inputs[f.name] === undefined || inputs[f.name] === "")) ?? [], [cmd, inputs]);

  const submit = async () => {
    if (!cmd || !ws) return;
    setBusy(true);
    setError(null);
    const clean = Object.fromEntries(Object.entries(inputs).filter(([, v]) => v !== "" && v !== undefined));
    const body: Record<string, unknown> = { workspaceId: ws.id, command: cmd.canonical, agentProfileId: agentId, inputs: clean, provider };
    if (cmd.repositoryAccess !== "NONE") {
      body.repositories = Object.entries(selectedRepos).map(([repositoryId, revision]) => ({ repositoryId, revision }));
    }
    try {
      const job = await api.submitJob(body, idemKey);
      nav(`/jobs/${job.id}`);
    } catch (e) {
      setError(e instanceof ApiError && e.field ? new Error(`${e.field}: ${e.message}`) : e);
    } finally {
      setBusy(false);
    }
  };

  if (!workspaces) return <Spinner label="Loading…" />;
  if (!workspaces.length) return <><PageHeader title="Run a command" /><Empty title="No workspaces yet" hint="An administrator needs to create a workspace first." /></>;

  return (
    <>
      <PageHeader title="Run a command" subtitle="Pick a lifecycle phase and a framework command. Commands run in an isolated, ephemeral environment."
        actions={
          <select className={`${inputCls} w-64`} value={wsId} onChange={(e) => setWsId(e.target.value)} aria-label="Workspace">
            {workspaces.map((w) => <option key={w.id} value={w.id}>{w.name}</option>)}
          </select>
        } />
      <div className="mb-5 grid grid-cols-3 gap-2 lg:grid-cols-6">
        {PHASES.map((p) => (
          <button key={p} onClick={() => setPhase(p)}
            className={`rounded-xl border px-3 py-3 text-left transition ${p === phase ? "border-indigo-500 bg-indigo-50 ring-1 ring-indigo-500" : "border-slate-200 bg-white hover:border-slate-300"}`}>
            <div className="text-sm font-semibold">{p[0] + p.slice(1).toLowerCase()}</div>
            <div className="mt-0.5 text-[11px] leading-tight text-slate-500">{phaseBlurb[p]}</div>
          </button>
        ))}
      </div>
      <div className="grid gap-6 lg:grid-cols-5">
        <div className="space-y-2 lg:col-span-2">
          {commands === null ? <Spinner label="Loading commands…" /> : commands.length === 0 ? (
            <Empty title="No commands available" hint="Your roles do not permit commands in this phase, or none support the selected agent." />
          ) : commands.map((c) => (
            <button key={c.id} onClick={() => selectCommand(c)}
              className={`w-full rounded-xl border bg-white px-4 py-3 text-left transition ${cmd?.id === c.id ? "border-indigo-500 ring-1 ring-indigo-500" : "border-slate-200 hover:border-slate-300"}`}>
              <div className="flex items-center justify-between gap-2">
                <code className="text-sm font-semibold text-slate-900">{c.canonical}</code>
                <div className="flex gap-1">
                  {c.repositoryAccess === "WRITE" && <Badge tone="amber">writes code</Badge>}
                  {c.approval.beforeExecution && <Badge tone="violet">approval</Badge>}
                </div>
              </div>
              <p className="mt-1 text-xs text-slate-500">{c.description}</p>
            </button>
          ))}
        </div>
        <div className="lg:col-span-3">
          {!cmd ? <Card><Empty title="Select a command" hint="Its inputs and execution options appear here." /></Card> : (
            <Card title={<span>Configure <code>{cmd.canonical}</code></span>}>
              <div className="space-y-4">
                {cmd.inputs.map((f) => (
                  <Field key={f.name} label={f.label || f.name} hint={f.description} required={f.required}>
                    <InputControl f={f} value={inputs[f.name]} onChange={(v) => setInputs((s) => ({ ...s, [f.name]: v }))} />
                  </Field>
                ))}
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label="Agent">
                    <select className={inputCls} value={agentId} onChange={(e) => setAgentId(e.target.value)}>
                      <option value="">Select…</option>
                      {agents.map((a) => <option key={a.id} value={a.id}>{a.name} (v{a.version})</option>)}
                    </select>
                  </Field>
                  <Field label="Execution pool" hint={pools.canSelect ? undefined : "Selected automatically by policy"}>
                    <select className={inputCls} value={provider} disabled={!pools.canSelect} onChange={(e) => setProvider(e.target.value)}>
                      <option value="AUTO">Automatic (policy-based)</option>
                      {pools.items.filter((p) => p.enabled).map((p) => <option key={p.name} value={p.name}>{p.name} · {p.network.toLowerCase()}</option>)}
                    </select>
                  </Field>
                </div>
                {cmd.repositoryAccess !== "NONE" && (
                  <Field label={`Repositories (${cmd.repositoryAccess === "WRITE" ? "read & write on a working branch" : "read only"})`}>
                    {repos.length === 0 ? <p className="text-sm text-amber-700">This workspace has no repositories.</p> : (
                      <div className="space-y-2">
                        {repos.map((r) => (
                          <div key={r.id} className="flex items-center gap-3 rounded-lg border border-slate-200 px-3 py-2">
                            <input type="checkbox" className="h-4 w-4 rounded border-slate-300 text-indigo-600" checked={r.id in selectedRepos}
                              onChange={(e) => setSelectedRepos((s) => {
                                const n = { ...s };
                                if (e.target.checked) n[r.id] = r.defaultBranch; else delete n[r.id];
                                return n;
                              })} />
                            <span className="min-w-0 flex-1 truncate font-mono text-xs" title={r.url}>{r.url}</span>
                            {r.id in selectedRepos && (
                              <div className="w-36 shrink-0">
                                <input className={`${inputCls} py-1 font-mono text-xs`} value={selectedRepos[r.id]} aria-label="Revision"
                                  onChange={(e) => setSelectedRepos((s) => ({ ...s, [r.id]: e.target.value }))} />
                              </div>
                            )}
                          </div>
                        ))}
                      </div>
                    )}
                  </Field>
                )}
                <div className="rounded-lg bg-slate-50 px-4 py-3 text-xs text-slate-600">
                  <div>Expected outputs: {cmd.expectedArtifacts.filter((a) => a.required).map((a) => a.type).join(", ") || "none declared"}</div>
                  <div>Timeout: {Math.round(cmd.timeoutSeconds / 60)} min · Side effects: {cmd.sideEffects.filter((s) => s !== "NONE").join(", ") || "none"}</div>
                  {cmd.approval.beforeExecution && <div className="mt-1 font-medium text-violet-700">Requires approval before the agent starts.</div>}
                </div>
                <ErrorBanner error={error} />
                <div className="flex justify-end">
                  <Button variant="primary" disabled={busy || missing.length > 0 || !agentId} onClick={submit}>
                    {busy ? "Submitting…" : `Run ${cmd.canonical}`}
                  </Button>
                </div>
              </div>
            </Card>
          )}
        </div>
      </div>
    </>
  );
}
