import { useState } from "react";
import { api, auth } from "../lib/api";
import { Button, ErrorBanner, Field, inputCls } from "../components/ui";

// Development personas match the default ASTRION_DEV_USERS. In production
// the API only accepts OIDC access tokens (static tokens are refused).
const PERSONAS = [
  { token: "dev-analyst-token", name: "Bea", role: "Business analyst / PM" },
  { token: "dev-architect-token", name: "Arch", role: "Architect" },
  { token: "dev-developer-token", name: "Dana", role: "Developer + QA" },
  { token: "dev-release-token", name: "Rel", role: "Release / platform engineer" },
  { token: "dev-admin-token", name: "Admin", role: "Platform administrator" },
];

export default function Login({ onLogin }: { onLogin: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  const signIn = async (t: string) => {
    setBusy(true);
    setError(null);
    auth.set(t.trim());
    try {
      await api.me();
      onLogin();
    } catch (e) {
      auth.clear();
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid min-h-screen place-items-center bg-gradient-to-br from-slate-50 to-indigo-50 px-4">
      <div className="w-full max-w-md">
        <div className="mb-8 flex items-center gap-3">
          <svg viewBox="0 0 32 32" className="h-10 w-10"><path d="M16 2 30 28H2Z" className="fill-indigo-600" /><path d="M16 12 22 24H10Z" className="fill-white" /></svg>
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">Astrion</h1>
            <p className="text-sm text-slate-500">Agentic software delivery, with humans in the loop.</p>
          </div>
        </div>
        <div className="rounded-xl border border-slate-200 bg-white p-6 shadow-sm">
          <h2 className="text-sm font-semibold">Sign in</h2>
          <p className="mt-1 text-sm text-slate-500">Paste an access token from your identity provider.</p>
          <form className="mt-4 space-y-3" onSubmit={(e) => { e.preventDefault(); void signIn(token); }}>
            <Field label="Access token">
              <input className={inputCls} type="password" value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" />
            </Field>
            <ErrorBanner error={error} />
            <Button variant="primary" className="w-full" disabled={!token || busy}>Sign in</Button>
          </form>
          <div className="mt-6 border-t border-slate-100 pt-4">
            <p className="text-xs font-medium uppercase tracking-wide text-slate-400">Development personas</p>
            <div className="mt-2 grid gap-2">
              {PERSONAS.map((p) => (
                <button key={p.token} disabled={busy} onClick={() => signIn(p.token)}
                  className="flex items-center justify-between rounded-lg border border-slate-200 px-3 py-2 text-left text-sm hover:border-indigo-300 hover:bg-indigo-50/50">
                  <span className="font-medium">{p.name}</span>
                  <span className="text-xs text-slate-500">{p.role}</span>
                </button>
              ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
