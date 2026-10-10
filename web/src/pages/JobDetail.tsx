import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  api, fetchArtifact, subscribeJobEvents, TERMINAL, type Artifact, type Execution, type Interaction, type JobDetail as JD,
  type JobEvent, type LogLine,
} from "../lib/api";
import { bytes, duration, shortId, stateLabel, timeAgo } from "../lib/format";
import { Badge, Button, Card, Empty, ErrorBanner, inputCls, KV, Spinner, StateBadge } from "../components/ui";
import InteractionCard from "../components/InteractionCard";

const PATH = ["SUBMITTED", "VALIDATING", "QUEUED", "PROVISIONING", "PREPARING_WORKSPACE", "RUNNING", "COLLECTING_ARTIFACTS", "CLEANING_UP"];

function Lifecycle({ state, seen }: { state: string; seen: Set<string> }) {
  const terminal = TERMINAL.includes(state as never);
  const waiting = state.startsWith("WAITING") || state === "NEEDS_INTERVENTION";
  return (
    <ol className="flex flex-wrap items-center gap-1 text-[11px]">
      {PATH.map((s) => {
        const done = seen.has(s) && s !== state;
        const cur = s === state;
        return (
          <li key={s} className={`rounded-md px-2 py-1 font-medium ${cur ? "bg-indigo-600 text-white" : done ? "bg-indigo-50 text-indigo-700" : "bg-slate-100 text-slate-400"}`}>
            {stateLabel(s)}
          </li>
        );
      })}
      {waiting && <li className="rounded-md bg-amber-500 px-2 py-1 font-medium text-white">{stateLabel(state)}</li>}
      {terminal && <li><StateBadge state={state} /></li>}
    </ol>
  );
}

function describe(e: JobEvent): string {
  const p = e.payload ?? {};
  switch (e.type) {
    case "job.status_changed": return `${p.from ? stateLabel(p.from) + " → " : ""}${stateLabel(p.to)}${p.step ? ` · ${p.step}` : ""}`;
    case "job.step_changed": return p.step;
    case "interaction.input_required": return "Agent is waiting for input";
    case "interaction.approval_required": return "Approval required";
    case "interaction.resolved": return `Response recorded (${String(p.decision).toLowerCase()})`;
    case "artifact.available": return `Artifact available: ${p.name}`;
    case "job.failed": return `Failed: ${p.message ?? p.failureClass ?? ""}`;
    case "job.completed": return "Completed";
    case "cleanup.failed": return `Cleanup failed: ${p.error ?? ""} (the reconciler will retry)`;
    default: return e.type;
  }
}

function ArtifactPreview({ a, onClose }: { a: Artifact; onClose: () => void }) {
  const [text, setText] = useState<string | null>(null);
  useEffect(() => {
    fetchArtifact(a.id, true).then((r) => r.text()).then((t) => setText(t.slice(0, 200_000))).catch(() => setText("Unable to load preview."));
  }, [a.id]);
  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-slate-900/40 p-6" onClick={onClose}>
      <div className="flex max-h-[85vh] w-full max-w-3xl flex-col rounded-xl bg-white shadow-xl" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between border-b border-slate-100 px-5 py-3">
          <div className="text-sm font-semibold">{a.name}</div>
          <Button size="sm" variant="ghost" onClick={onClose}>Close</Button>
        </div>
        <pre className="overflow-auto whitespace-pre-wrap p-5 font-mono text-xs text-slate-800">{text ?? "Loading…"}</pre>
      </div>
    </div>
  );
}

async function download(a: Artifact) {
  const res = await fetchArtifact(a.id, false);
  const url = URL.createObjectURL(await res.blob());
  const el = document.createElement("a");
  el.href = url;
  el.download = a.name;
  el.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

type Tab = "activity" | "logs" | "artifacts" | "interactions" | "executions";

export default function JobDetail() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const [detail, setDetail] = useState<JD | null>(null);
  const [events, setEvents] = useState<JobEvent[]>([]);
  const [logs, setLogs] = useState<LogLine[]>([]);
  const [artifacts, setArtifacts] = useState<Artifact[]>([]);
  const [interactions, setInteractions] = useState<Interaction[]>([]);
  const [executions, setExecutions] = useState<Execution[]>([]);
  const [tab, setTab] = useState<Tab>("activity");
  const [preview, setPreview] = useState<Artifact | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [live, setLive] = useState(false);
  const [reason, setReason] = useState("");
  const logEnd = useRef<HTMLDivElement>(null);

  const refresh = useCallback(() => {
    api.job(id).then(setDetail).catch(setError);
    api.jobInteractions(id).then(setInteractions).catch(() => {});
    api.jobArtifacts(id).then(setArtifacts).catch(() => {});
    api.jobExecutions(id).then(setExecutions).catch(() => {});
  }, [id]);

  useEffect(() => {
    setEvents([]);
    setLogs([]);
    refresh();
    setLive(true);
    let timer: ReturnType<typeof setTimeout> | undefined;
    const debounced = () => {
      clearTimeout(timer);
      timer = setTimeout(refresh, 250);
    };
    const stop = subscribeJobEvents(id, (e) => {
      if (e.type === "job.log_batch") {
        const lines: LogLine[] = (e.payload.lines ?? []).map((l: any) => ({ eventId: e.id, executionId: e.payload.executionId,
          attempt: e.payload.attempt, ts: l.ts, stream: l.stream, message: l.message }));
        setLogs((s) => (s.some((x) => x.eventId === e.id) ? s : [...s, ...lines].slice(-5000)));
        return;
      }
      setEvents((s) => (s.some((x) => x.id === e.id) ? s : [...s, e]));
      debounced();
    }, () => { setLive(false); refresh(); });
    return () => { stop(); clearTimeout(timer); };
  }, [id, refresh]);

  useEffect(() => {
    if (tab === "logs") logEnd.current?.scrollIntoView({ block: "end" });
  }, [logs, tab]);

  if (!detail) return error ? <ErrorBanner error={error} /> : <Spinner label="Loading job…" />;
  const { job, actions } = detail;
  const seen = new Set<string>(events.filter((e) => e.type === "job.status_changed").map((e) => e.payload.to));
  seen.add(job.state);
  const open = interactions.filter((i) => i.status === "OPEN");

  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try { await fn(); refresh(); } catch (e) { setError(e); }
  };

  const tabs: { id: Tab; label: string; count?: number }[] = [
    { id: "activity", label: "Activity" },
    { id: "logs", label: "Logs", count: logs.length },
    { id: "artifacts", label: "Artifacts", count: artifacts.length },
    { id: "interactions", label: "Interactions", count: interactions.length },
    { id: "executions", label: "Environments", count: executions.length },
  ];

  return (
    <>
      <div className="mb-2 text-sm text-slate-500"><Link to="/jobs" className="hover:text-slate-800">Jobs</Link> / {shortId(job.id)}</div>
      <div className="mb-5 flex flex-wrap items-start justify-between gap-4">
        <div>
          <div className="flex items-center gap-3">
            <h1 className="font-mono text-xl font-semibold">{job.command}</h1>
            <StateBadge state={job.state} />
            {live && <span className="inline-flex items-center gap-1 text-xs text-emerald-600"><span className="pulse-dot h-1.5 w-1.5 rounded-full bg-emerald-500" />live</span>}
          </div>
          <p className="mt-1 text-sm text-slate-500">
            {job.phase.toLowerCase()} · submitted {timeAgo(job.createdAt)} · {duration(job.createdAt, job.finishedAt)}
            {job.retryOf && <> · retry of <Link className="text-indigo-600" to={`/jobs/${job.retryOf}`}>{shortId(job.retryOf)}</Link></>}
          </p>
        </div>
        <div className="flex gap-2">
          {actions.cancel && <Button variant="danger" onClick={() => act(() => api.cancelJob(job.id))}>Cancel job</Button>}
          {actions.retry && <Button variant="primary" onClick={() => act(async () => { const j = await api.retryJob(job.id); nav(`/jobs/${j.id}`); })}>Retry as new job</Button>}
        </div>
      </div>

      <Card className="mb-5"><Lifecycle state={job.state} seen={seen} />
        {job.currentStep && !TERMINAL.includes(job.state) && <p className="mt-3 text-sm text-slate-600">Current step: {job.currentStep}</p>}
        {job.failureMessage && (
          <div className="mt-3 rounded-lg border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-800">
            <span className="font-semibold">{job.failureClass?.replace(/_/g, " ").toLowerCase()}:</span> {job.failureMessage}
          </div>
        )}
        {job.cancelRequestedAt && !TERMINAL.includes(job.state) && <p className="mt-3 text-sm text-slate-600">Cancellation requested — stopping the agent and cleaning up…</p>}
        {job.cleanupStatus === "FAILED" && <p className="mt-3 text-sm text-amber-700">Environment cleanup failed; the reconciler retries automatically.</p>}
      </Card>

      <ErrorBanner error={error} />

      {actions.operator && (
        <Card title="Operator action required" className="mb-5 border-violet-300">
          <p className="text-sm text-slate-600">The job could not continue automatically. Choose how to proceed — every action is audited.</p>
          <input className={`${inputCls} mt-3`} placeholder="Reason" value={reason} onChange={(e) => setReason(e.target.value)} />
          <div className="mt-3 flex gap-2">
            <Button variant="primary" onClick={() => act(() => api.operatorAction(job.id, "retry", reason))}>Retry attempt</Button>
            <Button onClick={() => act(() => api.operatorAction(job.id, "reopen", reason))}>Reopen interaction</Button>
            <Button variant="danger" onClick={() => act(() => api.operatorAction(job.id, "cancel", reason))}>Cancel</Button>
          </div>
        </Card>
      )}

      {open.length > 0 && (
        <div className="mb-5 space-y-3">
          {open.map((i) => <InteractionCard key={i.id} it={i} onResolved={refresh} />)}
        </div>
      )}

      <div className="grid gap-5 lg:grid-cols-3">
        <div className="lg:col-span-2">
          <div className="mb-3 flex gap-1 border-b border-slate-200">
            {tabs.map((t) => (
              <button key={t.id} onClick={() => setTab(t.id)}
                className={`-mb-px border-b-2 px-3 py-2 text-sm font-medium ${tab === t.id ? "border-indigo-600 text-indigo-700" : "border-transparent text-slate-500 hover:text-slate-800"}`}>
                {t.label}{t.count ? <span className="ml-1.5 rounded-full bg-slate-100 px-1.5 text-xs text-slate-600">{t.count}</span> : null}
              </button>
            ))}
          </div>
          {tab === "activity" && (
            <Card padded={false}>
              {events.length === 0 ? <div className="p-5"><Spinner label="Connecting to event stream…" /></div> : (
                <ol className="divide-y divide-slate-100">
                  {events.map((e) => (
                    <li key={e.id} className="flex gap-4 px-5 py-2.5 text-sm">
                      <span className="w-20 shrink-0 font-mono text-xs text-slate-400">{new Date(e.createdAt).toLocaleTimeString()}</span>
                      <span className={e.type.startsWith("interaction") ? "text-amber-700" : e.type === "job.failed" ? "text-rose-700" : "text-slate-700"}>{describe(e)}</span>
                    </li>
                  ))}
                </ol>
              )}
            </Card>
          )}
          {tab === "logs" && (
            <div className="max-h-[560px] overflow-auto rounded-xl bg-slate-950 p-4 font-mono text-xs leading-relaxed text-slate-200">
              {logs.length === 0 ? <span className="text-slate-500">No log output yet.</span> : logs.map((l, i) => (
                <div key={i} className="whitespace-pre-wrap">
                  <span className="text-slate-500">{new Date(l.ts).toLocaleTimeString()} </span>
                  <span className="text-slate-500">#{l.attempt} </span>
                  <span className={l.stream === "stderr" ? "text-rose-300" : l.stream === "supervisor" ? "text-sky-300" : ""}>{l.message}</span>
                </div>
              ))}
              <div ref={logEnd} />
            </div>
          )}
          {tab === "artifacts" && (
            <Card padded={false}>
              {artifacts.length === 0 ? <div className="p-5"><Empty title="No artifacts yet" /></div> : (
                <ul className="divide-y divide-slate-100">
                  {artifacts.map((a) => (
                    <li key={a.id} className="flex items-center gap-3 px-5 py-3 text-sm">
                      <Badge tone="indigo">{a.type}</Badge>
                      <span className="flex-1 truncate font-medium">{a.name}</span>
                      <span className="text-xs text-slate-400">{bytes(a.sizeBytes)}</span>
                      {(a.contentType.startsWith("text/") || a.contentType === "application/json") && <Button size="sm" onClick={() => setPreview(a)}>Preview</Button>}
                      <Button size="sm" onClick={() => download(a)}>Download</Button>
                    </li>
                  ))}
                </ul>
              )}
            </Card>
          )}
          {tab === "interactions" && (
            <div className="space-y-3">
              {interactions.length === 0 ? <Empty title="No human interactions" /> : interactions.map((i) => <InteractionCard key={i.id} it={i} onResolved={refresh} />)}
            </div>
          )}
          {tab === "executions" && (
            <Card padded={false}>
              {executions.length === 0 ? <div className="p-5"><Empty title="No environments provisioned yet" /></div> : (
                <table className="w-full text-sm">
                  <thead className="text-left text-xs uppercase tracking-wide text-slate-500"><tr>
                    <th className="px-5 py-2 font-medium">Attempt</th><th className="px-3 py-2 font-medium">Pool</th><th className="px-3 py-2 font-medium">Status</th>
                    <th className="px-3 py-2 font-medium">Mode</th><th className="px-3 py-2 font-medium">Cleanup</th><th className="px-5 py-2 font-medium">Note</th>
                  </tr></thead>
                  <tbody className="divide-y divide-slate-100">
                    {executions.map((x) => (
                      <tr key={x.id}>
                        <td className="px-5 py-2.5">#{x.attempt}</td>
                        <td className="px-3 py-2.5">{x.provider}</td>
                        <td className="px-3 py-2.5"><Badge tone={x.status === "COMPLETED" ? "green" : x.status === "FAILED" || x.status === "LOST" ? "red" : x.status === "SUSPENDED" ? "amber" : "blue"}>{x.status.toLowerCase()}</Badge></td>
                        <td className="px-3 py-2.5 text-xs text-slate-500">{x.interactionMode.replace(/_/g, " ").toLowerCase()}</td>
                        <td className="px-3 py-2.5 text-xs">{x.cleanupStatus.toLowerCase()}</td>
                        <td className="max-w-xs truncate px-5 py-2.5 text-xs text-slate-500">{x.errorMessage || x.summary || (x.resumeInteractionId ? "resumed from checkpoint" : "")}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Card>
          )}
        </div>
        <div className="space-y-5">
          <Card title="Details">
            <dl className="divide-y divide-slate-100">
              <KV k="Job ID"><code className="text-xs">{job.id}</code></KV>
              <KV k="Agent profile">{shortId(job.agentProfileId)} v{job.agentProfileVersion}</KV>
              <KV k="Framework">{job.frameworkId} {job.frameworkRevision}</KV>
              <KV k="Execution pool">{job.selectedProvider || job.requestedProvider}</KV>
              <KV k="Attempt">{job.attempt || "—"}</KV>
              <KV k="Cleanup">{job.cleanupStatus.toLowerCase().replace(/_/g, " ")}</KV>
            </dl>
          </Card>
          <Card title="Inputs">
            <pre className="overflow-auto rounded-lg bg-slate-50 p-3 font-mono text-xs">{JSON.stringify(job.inputs, null, 2)}</pre>
          </Card>
          {job.repositories.length > 0 && (
            <Card title="Repositories">
              <ul className="space-y-3 text-xs">
                {job.repositories.map((r) => (
                  <li key={r.repositoryId}>
                    <div className="truncate font-mono font-medium">{r.url}</div>
                    <div className="text-slate-500">rev {r.requestedRevision}{r.resolvedCommit && ` @ ${r.resolvedCommit.slice(0, 10)}`}</div>
                    {r.workingBranch && <div className="text-slate-500">branch {r.workingBranch}{r.finalCommit && ` → ${r.finalCommit.slice(0, 10)}`}</div>}
                  </li>
                ))}
              </ul>
            </Card>
          )}
        </div>
      </div>
      {preview && <ArtifactPreview a={preview} onClose={() => setPreview(null)} />}
    </>
  );
}
