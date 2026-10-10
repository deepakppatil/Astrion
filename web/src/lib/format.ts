import type { JobState } from "./api";

export function timeAgo(iso?: string): string {
  if (!iso) return "—";
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return new Date(iso).toLocaleDateString();
}

export function duration(from?: string, to?: string): string {
  if (!from) return "—";
  const ms = (to ? new Date(to).getTime() : Date.now()) - new Date(from).getTime();
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

export const stateLabel = (s: string) => s.replace(/_/g, " ").toLowerCase().replace(/^\w/, (c) => c.toUpperCase());

export function stateTone(s: JobState | string): "green" | "red" | "amber" | "blue" | "slate" | "violet" {
  switch (s) {
    case "SUCCEEDED": return "green";
    case "FAILED": case "TIMED_OUT": return "red";
    case "NEEDS_INTERVENTION": return "violet";
    case "WAITING_FOR_INPUT": case "WAITING_FOR_APPROVAL": return "amber";
    case "CANCELLED": return "slate";
    default: return "blue";
  }
}

export const shortId = (id: string) => (id.length > 14 ? `${id.slice(0, 4)}…${id.slice(-6)}` : id);

export const roleLabel: Record<string, string> = {
  business_analyst: "BA / PM", architect: "Architect", developer: "Developer", qa_engineer: "QA",
  release_engineer: "Release / Platform", platform_admin: "Admin", viewer: "Viewer",
};
