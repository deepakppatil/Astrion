import { useEffect, useState } from "react";
import { api, type Repository, type Workspace } from "../lib/api";
import { Badge, Button, Card, Empty, ErrorBanner, Field, inputCls, PageHeader, Spinner } from "../components/ui";
import { useSession } from "../App";

function Repos({ ws, canEdit }: { ws: Workspace; canEdit: boolean }) {
  const [repos, setRepos] = useState<Repository[] | null>(null);
  const [url, setUrl] = useState("");
  const [branch, setBranch] = useState("main");
  const [error, setError] = useState<unknown>(null);
  const load = () => api.repositories(ws.id).then(setRepos).catch(setError);
  useEffect(() => { load(); }, [ws.id]); // eslint-disable-line react-hooks/exhaustive-deps
  const add = async () => {
    setError(null);
    try {
      await api.addRepository(ws.id, { url, defaultBranch: branch });
      setUrl("");
      load();
    } catch (e) { setError(e); }
  };
  return (
    <div className="space-y-2">
      {repos === null ? <Spinner /> : repos.length === 0 ? <p className="text-sm text-slate-500">No repositories.</p> : repos.map((r) => (
        <div key={r.id} className="flex items-center gap-3 rounded-lg border border-slate-200 px-3 py-2 text-sm">
          <Badge>{r.provider.toLowerCase()}</Badge>
          <span className="flex-1 truncate font-mono text-xs">{r.url}</span>
          <span className="text-xs text-slate-500">{r.defaultBranch}</span>
          {canEdit && <Button size="sm" variant="ghost" onClick={() => api.removeRepository(ws.id, r.id).then(load).catch(setError)}>Remove</Button>}
        </div>
      ))}
      {canEdit && (
        <div className="flex gap-2 pt-1">
          <input className={inputCls} placeholder="https://github.com/org/repo.git" value={url} onChange={(e) => setUrl(e.target.value)} />
          <div className="w-32 shrink-0"><input className={inputCls} value={branch} onChange={(e) => setBranch(e.target.value)} aria-label="Default branch" /></div>
          <Button onClick={add} disabled={!url}>Add</Button>
        </div>
      )}
      <ErrorBanner error={error} />
    </div>
  );
}

export default function Workspaces() {
  const { can } = useSession();
  const [list, setList] = useState<Workspace[] | null>(null);
  const [name, setName] = useState("");
  const [desc, setDesc] = useState("");
  const [cls, setCls] = useState("INTERNAL");
  const [error, setError] = useState<unknown>(null);
  const load = () => api.workspaces().then(setList).catch(setError);
  useEffect(() => { load(); }, []);
  const canEdit = can("workspace:write");
  const create = async () => {
    setError(null);
    try {
      const frameworks = await api.frameworks();
      const agents = await api.agents();
      await api.createWorkspace({ name, description: desc, dataClassification: cls, defaultFrameworkId: frameworks[0]?.id,
        defaultAgentProfileId: agents[0]?.id, defaultProvider: "AUTO" });
      setName("");
      setDesc("");
      load();
    } catch (e) { setError(e); }
  };
  return (
    <>
      <PageHeader title="Workspaces" subtitle="Repository sets, policies and defaults that jobs run against." />
      <ErrorBanner error={error} />
      {canEdit && (
        <Card title="New workspace" className="mb-6">
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="Name" required><input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} /></Field>
            <Field label="Description"><input className={inputCls} value={desc} onChange={(e) => setDesc(e.target.value)} /></Field>
            <Field label="Data classification" hint="Restricts which execution pools may run its jobs">
              <select className={inputCls} value={cls} onChange={(e) => setCls(e.target.value)}>
                {["PUBLIC", "INTERNAL", "CONFIDENTIAL", "RESTRICTED"].map((c) => <option key={c}>{c}</option>)}
              </select>
            </Field>
          </div>
          <div className="mt-4 flex justify-end"><Button variant="primary" disabled={!name} onClick={create}>Create workspace</Button></div>
        </Card>
      )}
      {list === null ? <Spinner /> : list.length === 0 ? <Empty title="No workspaces" /> : (
        <div className="grid gap-5">
          {list.map((w) => (
            <Card key={w.id} title={w.name} actions={<><Badge tone="indigo">{w.dataClassification.toLowerCase()}</Badge><Badge>{w.type.replace(/_/g, " ").toLowerCase()}</Badge></>}>
              {w.description && <p className="mb-3 text-sm text-slate-600">{w.description}</p>}
              <Repos ws={w} canEdit={canEdit} />
            </Card>
          ))}
        </div>
      )}
    </>
  );
}
