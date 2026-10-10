// Typed client for the Astrion control-plane API. The UI never makes
// authorization decisions: it hides controls for convenience, and the
// server enforces every permission.

export type JobState =
  | "SUBMITTED" | "VALIDATING" | "QUEUED" | "PROVISIONING" | "PREPARING_WORKSPACE" | "RUNNING"
  | "WAITING_FOR_INPUT" | "WAITING_FOR_APPROVAL" | "COLLECTING_ARTIFACTS" | "CLEANING_UP"
  | "SUCCEEDED" | "FAILED" | "CANCELLED" | "TIMED_OUT" | "NEEDS_INTERVENTION";

export type Phase = "PLAN" | "DESIGN" | "IMPLEMENT" | "TEST" | "DEPLOY" | "MAINTAIN";
export const PHASES: Phase[] = ["PLAN", "DESIGN", "IMPLEMENT", "TEST", "DEPLOY", "MAINTAIN"];

export const TERMINAL: JobState[] = ["SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT"];

export interface Principal {
  userId: string; tenantId: string; subject: string; email: string; name: string; roles: string[];
}
export interface Me { user: Principal; permissions: string[] }

export interface JobRepository {
  repositoryId: string; url: string; requestedRevision: string; resolvedCommit?: string;
  workingBranch?: string; finalCommit?: string; changeRequestUrl?: string;
}
export interface Job {
  id: string; workspaceId: string; initiatedBy: string; phase: Phase; command: string; commandId: string;
  agentProfileId: string; agentProfileVersion: number; frameworkId: string; frameworkRevision: string;
  requestedProvider: string; selectedProvider?: string; inputs: Record<string, unknown>;
  repositories: JobRepository[]; state: JobState; currentStep: string; failureClass?: string;
  failureMessage?: string; cleanupStatus: string; attempt: number; retryOf?: string; parentJobId?: string;
  cancelRequestedAt?: string; createdAt: string; updatedAt: string; startedAt?: string; finishedAt?: string;
}
export interface JobDetail {
  job: Job; allowedTransitions: JobState[]; actions: { cancel: boolean; retry: boolean; operator: boolean };
}
export interface InputField {
  name: string; label: string; type: "string" | "text" | "boolean" | "integer" | "enum" | "artifactRef" | "jiraKey";
  required: boolean; description?: string; enum?: string[]; maxLength?: number; default?: unknown;
}
export interface Command {
  id: string; frameworkId: string; version: number; displayName: string; canonical: string; description: string;
  phase: Phase; personas: string[]; compatibleAgents: string[]; inputs: InputField[]; repositoryAccess: string;
  sideEffects: string[]; timeoutSeconds: number; approval: { beforeExecution: boolean; approverRole?: string };
  expectedArtifacts: { type: string; required: boolean }[];
}
export interface Workspace {
  id: string; name: string; description: string; type: string; lifecycleMode: string; dataClassification: string;
  defaultAgentProfileId?: string; defaultFrameworkId?: string; defaultProvider?: string; createdAt: string;
}
export interface Repository {
  id: string; workspaceId: string; provider: string; url: string; defaultBranch: string; permittedOperations: string[];
}
export interface AgentProfile {
  id: string; name: string; version: number; provider: string; cliName: string; cliVersion: string;
  interactionMode: string; enabled: boolean; capabilities?: Record<string, unknown>;
}
export interface Framework { id: string; name: string; version: string; revision: string; source: string }
export interface InteractionOption { value: string; label: string; description?: string }
export interface Interaction {
  id: string; jobId: string; executionId: string; stepId: string; kind: string; prompt: string;
  options?: InteractionOption[]; schema: { required: boolean; maxLength?: number; minLength?: number; multiple?: boolean };
  requestedRole?: string; status: "OPEN" | "RESOLVED" | "EXPIRED" | "CANCELLED" | "RECORDED";
  createdAt: string; openedAt?: string; expiresAt?: string; resolvedAt?: string;
  response?: { decision: string; values?: string[]; text?: string; comment?: string; respondedBy: string; createdAt: string };
  jobCommand?: string; jobState?: string; canRespond?: boolean; platform?: boolean; productionApproval?: boolean;
}
export interface Artifact {
  id: string; jobId: string; type: string; name: string; contentType: string; sizeBytes: number; sha256: string;
  sourceStep: string; createdAt: string;
}
export interface Execution {
  id: string; attempt: number; provider: string; status: string; interactionMode: string; exitCode?: number;
  summary?: string; errorMessage?: string; failureClass?: string; cleanupStatus: string; createdAt: string; finishedAt?: string;
  resumeInteractionId?: string;
}
export interface JobEvent { id: number; type: string; payload: any; createdAt: string }
export interface LogLine { eventId: number; executionId: string; attempt: number; ts: string; stream: string; message: string }
export interface Dashboard {
  active: number; awaitingInput: number; failed24h: number; succeeded24h: number; avgDurationSeconds: number;
  byState: Record<string, number>; cleanupFailures: number;
}
export interface AuditEvent {
  id: number; actor: string; actorType: string; action: string; targetType: string; targetId: string; details?: any; createdAt: string;
}
export interface Pool { name: string; type: string; network: string; region?: string; maxClassification: string; enabled: boolean }

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string, public field?: string) {
    super(message);
  }
}

const TOKEN_KEY = "astrion.token";
export const auth = {
  get: () => sessionStorage.getItem(TOKEN_KEY),
  set: (t: string) => sessionStorage.setItem(TOKEN_KEY, t),
  clear: () => sessionStorage.removeItem(TOKEN_KEY),
};

async function request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const token = auth.get();
  const res = await fetch(`/api/v1${path}`, {
    method,
    headers: {
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...headers,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  const data = text ? JSON.parse(text) : undefined;
  if (!res.ok) {
    const e = data?.error ?? {};
    if (res.status === 401) window.dispatchEvent(new Event("astrion:unauthorized"));
    throw new ApiError(res.status, e.code ?? "error", e.message ?? res.statusText, e.field);
  }
  return data as T;
}

export const newIdempotencyKey = () =>
  (crypto.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`).replace(/[^A-Za-z0-9-]/g, "");

type Items<T> = { items: T[] };

export const api = {
  me: () => request<Me>("GET", "/me"),
  dashboard: () => request<Dashboard>("GET", "/dashboard"),
  workspaces: () => request<Items<Workspace>>("GET", "/workspaces").then((r) => r.items),
  workspace: (id: string) => request<Workspace>("GET", `/workspaces/${id}`),
  createWorkspace: (w: Partial<Workspace>) => request<Workspace>("POST", "/workspaces", w),
  repositories: (ws: string) => request<Items<Repository>>("GET", `/workspaces/${ws}/repositories`).then((r) => r.items),
  addRepository: (ws: string, r: { url: string; defaultBranch?: string }) => request<Repository>("POST", `/workspaces/${ws}/repositories`, r),
  removeRepository: (ws: string, id: string) => request<void>("DELETE", `/workspaces/${ws}/repositories/${id}`),
  agents: () => request<Items<AgentProfile>>("GET", "/agents").then((r) => r.items),
  frameworks: () => request<Items<Framework>>("GET", "/frameworks").then((r) => r.items),
  commands: (fw: string, phase?: Phase, agent?: string) => {
    const q = new URLSearchParams();
    if (phase) q.set("phase", phase);
    if (agent) q.set("agent", agent);
    return request<Items<Command>>("GET", `/frameworks/${fw}/commands?${q}`).then((r) => r.items);
  },
  pools: () => request<{ items: Pool[]; canSelect: boolean }>("GET", "/execution-pools"),
  integrations: () => request<Items<any>>("GET", "/integrations").then((r) => r.items),
  jobs: (params: { state?: string; mine?: boolean; workspaceId?: string; before?: string } = {}) => {
    const q = new URLSearchParams();
    if (params.state) q.set("state", params.state);
    if (params.mine) q.set("mine", "true");
    if (params.workspaceId) q.set("workspaceId", params.workspaceId);
    if (params.before) q.set("before", params.before);
    return request<{ items: Job[]; nextBefore?: string }>("GET", `/jobs?${q}`);
  },
  job: (id: string) => request<JobDetail>("GET", `/jobs/${id}`),
  submitJob: (body: Record<string, unknown>, key: string) => request<Job>("POST", "/jobs", body, { "Idempotency-Key": key }),
  cancelJob: (id: string) => request<Job>("POST", `/jobs/${id}/cancel`),
  retryJob: (id: string) => request<Job>("POST", `/jobs/${id}/retry`),
  operatorAction: (id: string, action: string, reason: string) => request<void>("POST", `/jobs/${id}/operator-actions`, { action, reason }),
  jobLogs: (id: string, after = 0) => request<{ items: LogLine[]; next: number }>("GET", `/jobs/${id}/logs?after=${after}&limit=500`),
  jobArtifacts: (id: string) => request<Items<Artifact>>("GET", `/jobs/${id}/artifacts`).then((r) => r.items),
  jobInteractions: (id: string) => request<Items<Interaction>>("GET", `/jobs/${id}/interactions`).then((r) => r.items),
  jobExecutions: (id: string) => request<Items<Execution>>("GET", `/jobs/${id}/executions`).then((r) => r.items),
  openInteractions: (mine = false) => request<Items<Interaction>>("GET", `/interactions?status=OPEN${mine ? "&mine=true" : ""}`).then((r) => r.items),
  respond: (id: string, body: { values?: string[]; text?: string; comment?: string }, key: string) =>
    request<Interaction>("POST", `/interactions/${id}/responses`, body, { "Idempotency-Key": key }),
  approve: (id: string, comment: string, key: string) => request<Interaction>("POST", `/interactions/${id}/approve`, { comment }, { "Idempotency-Key": key }),
  reject: (id: string, comment: string, key: string) => request<Interaction>("POST", `/interactions/${id}/reject`, { comment }, { "Idempotency-Key": key }),
  audit: () => request<Items<AuditEvent>>("GET", "/audit").then((r) => r.items),
};

/** Fetches an artifact with the bearer token and returns its text or a blob URL. */
export async function fetchArtifact(id: string, inline: boolean): Promise<Response> {
  const token = auth.get();
  return fetch(`/api/v1/artifacts/${id}/content${inline ? "?inline=true" : ""}`, {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  });
}

/**
 * Subscribes to a job's event stream using fetch (so the bearer token stays
 * in a header, never in a URL). On disconnect it reconnects with
 * Last-Event-ID; the server replays durable events, so nothing is missed.
 */
export function subscribeJobEvents(jobId: string, onEvent: (e: JobEvent) => void, onEnd: () => void): () => void {
  const ctrl = new AbortController();
  let lastId = 0;
  let stopped = false;
  const run = async () => {
    let backoff = 1000;
    while (!stopped) {
      try {
        const token = auth.get();
        const res = await fetch(`/api/v1/jobs/${jobId}/events`, {
          headers: { Accept: "text/event-stream", ...(token ? { Authorization: `Bearer ${token}` } : {}),
            ...(lastId ? { "Last-Event-ID": String(lastId) } : {}) },
          signal: ctrl.signal,
        });
        if (res.status === 401 || res.status === 403 || res.status === 404) return;
        if (!res.ok || !res.body) throw new Error(`stream ${res.status}`);
        backoff = 1000;
        const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
        let buf = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += value;
          let idx;
          while ((idx = buf.indexOf("\n\n")) >= 0) {
            const frame = buf.slice(0, idx);
            buf = buf.slice(idx + 2);
            let type = "message", data = "", id = "";
            for (const line of frame.split("\n")) {
              if (line.startsWith("event: ")) type = line.slice(7);
              else if (line.startsWith("data: ")) data += line.slice(6);
              else if (line.startsWith("id: ")) id = line.slice(4);
            }
            if (id) lastId = Number(id);
            if (type === "end") {
              stopped = true;
              onEnd();
              return;
            }
            if (data && type !== "message") {
              try {
                onEvent(JSON.parse(data));
              } catch {
                /* ignore malformed frame */
              }
            }
          }
        }
      } catch (e) {
        if (ctrl.signal.aborted) return;
      }
      await new Promise((r) => setTimeout(r, backoff));
      backoff = Math.min(backoff * 2, 15000);
    }
  };
  void run();
  return () => {
    stopped = true;
    ctrl.abort();
  };
}
