import { useState } from "react";
import { api, newIdempotencyKey, type Interaction } from "../lib/api";
import { timeAgo } from "../lib/format";
import { Badge, Button, ErrorBanner, inputCls } from "./ui";

const DECISION_KINDS = new Set(["APPROVAL", "PERMISSION", "TOOL_AUTHORIZATION", "ARTIFACT_REVIEW"]);

/**
 * Renders one human-in-the-loop request. The response is validated and
 * authorized by the server; the job resumes only after it is durably stored.
 */
export default function InteractionCard({ it, onResolved }: { it: Interaction; onResolved?: () => void }) {
  const [values, setValues] = useState<string[]>([]);
  const [text, setText] = useState("");
  const [comment, setComment] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [key] = useState(newIdempotencyKey);
  const decision = DECISION_KINDS.has(it.kind);
  const choice = !decision && (it.options?.length ?? 0) > 0;
  const open = it.status === "OPEN";

  const send = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await fn();
      onResolved?.();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  const toggle = (v: string) =>
    setValues((s) => (it.schema.multiple ? (s.includes(v) ? s.filter((x) => x !== v) : [...s, v]) : [v]));

  return (
    <div className={`rounded-xl border p-4 ${open ? "border-amber-300 bg-amber-50/40" : "border-slate-200 bg-white"}`}>
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={decision ? "violet" : "amber"}>{it.kind.replace(/_/g, " ").toLowerCase()}</Badge>
        {it.platform && <Badge tone="indigo">platform gate</Badge>}
        {it.productionApproval && <Badge tone="red">production</Badge>}
        {it.requestedRole && <Badge>for {it.requestedRole.replace(/_/g, " ")}</Badge>}
        <span className="ml-auto text-xs text-slate-500">
          {open ? `opened ${timeAgo(it.openedAt)}` : it.status.toLowerCase()}
          {open && it.expiresAt && ` · expires ${new Date(it.expiresAt).toLocaleString()}`}
        </span>
      </div>
      <p className="mt-3 whitespace-pre-wrap text-sm text-slate-800">{it.prompt}</p>

      {it.response && (
        <div className="mt-3 rounded-lg bg-white px-3 py-2 text-sm ring-1 ring-slate-200">
          <span className="font-medium">{it.response.decision.toLowerCase()}</span>
          {it.response.values?.length ? `: ${it.response.values.join(", ")}` : ""}
          {it.response.text && <span className="block whitespace-pre-wrap text-slate-700">{it.response.text}</span>}
          {it.response.comment && <span className="block text-xs text-slate-500">“{it.response.comment}”</span>}
          <span className="block text-xs text-slate-400">by {it.response.respondedBy} · {timeAgo(it.response.createdAt)}</span>
        </div>
      )}

      {open && it.canRespond === false && <p className="mt-3 text-xs text-slate-500">You are not an authorized responder for this request.</p>}
      {open && it.canRespond !== false && (
        <div className="mt-4 space-y-3">
          {choice && (
            <div className="grid gap-2">
              {it.options!.map((o) => (
                <label key={o.value} className={`flex cursor-pointer items-start gap-3 rounded-lg border bg-white px-3 py-2 ${values.includes(o.value) ? "border-indigo-500 ring-1 ring-indigo-500" : "border-slate-200"}`}>
                  <input type={it.schema.multiple ? "checkbox" : "radio"} name={`opt-${it.id}`} className="mt-0.5" checked={values.includes(o.value)} onChange={() => toggle(o.value)} />
                  <span>
                    <span className="text-sm font-medium">{o.label}</span>
                    {o.description && <span className="block text-xs text-slate-500">{o.description}</span>}
                  </span>
                </label>
              ))}
            </div>
          )}
          {!decision && !choice && (
            <textarea className={inputCls} rows={3} placeholder="Your answer" maxLength={it.schema.maxLength || 50000} value={text} onChange={(e) => setText(e.target.value)} />
          )}
          <input className={inputCls} placeholder={decision ? "Comment (recorded in the audit log)" : "Optional comment"} value={comment} onChange={(e) => setComment(e.target.value)} maxLength={5000} />
          <ErrorBanner error={error} />
          <div className="flex justify-end gap-2">
            {decision ? (
              <>
                <Button variant="danger" disabled={busy} onClick={() => send(() => api.reject(it.id, comment, key + "-r"))}>Reject</Button>
                <Button variant="primary" disabled={busy} onClick={() => send(() => api.approve(it.id, comment, key + "-a"))}>Approve</Button>
              </>
            ) : (
              <Button variant="primary" disabled={busy || (choice ? values.length === 0 : !text.trim())}
                onClick={() => send(() => api.respond(it.id, choice ? { values, comment } : { text, comment }, key))}>
                Submit answer
              </Button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
