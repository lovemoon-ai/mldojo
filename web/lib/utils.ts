import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";
import { translate } from "./i18n";
import type { JsonValue, Node, Run } from "./types";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function shortId(id: string | undefined | null, n = 8): string {
  return id ? id.slice(0, n) : "";
}

export function formatTime(ts: string | null | undefined): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 1971) return "—";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function relativeTime(ts: string | null | undefined, now = Date.now()): string {
  if (!ts) return "—";
  const t = new Date(ts).getTime();
  if (Number.isNaN(t) || t < 0) return "—";
  const s = Math.round((now - t) / 1000);
  if (s < 0) return translate("just now");
  if (s < 60) return translate("{n}s ago", { n: s });
  if (s < 3600) return translate("{n}m ago", { n: Math.floor(s / 60) });
  if (s < 86400) return translate("{n}h ago", { n: Math.floor(s / 3600) });
  return translate("{n}d ago", { n: Math.floor(s / 86400) });
}

export function formatDuration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms < 0) return "—";
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${String(m).padStart(2, "0")}m`;
  if (m > 0) return `${m}m ${String(sec).padStart(2, "0")}s`;
  return `${sec}s`;
}

/** Elapsed run time: started_at -> finished_at (or now while running). */
export function runDurationMs(run: Pick<Run, "started_at" | "finished_at">, now = Date.now()): number | null {
  if (!run.started_at) return null;
  const start = new Date(run.started_at).getTime();
  const end = run.finished_at ? new Date(run.finished_at).getTime() : now;
  if (Number.isNaN(start) || Number.isNaN(end)) return null;
  return end - start;
}

export function formatBytes(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

export function formatNumber(v: number | null | undefined, digits = 4): string {
  if (v === null || v === undefined || typeof v !== "number") return "—";
  if (!Number.isFinite(v)) return String(v);
  if (Number.isInteger(v) && Math.abs(v) < 1e9) return v.toLocaleString();
  const abs = Math.abs(v);
  if (abs !== 0 && (abs < 1e-3 || abs >= 1e6)) return v.toExponential(3);
  return Number(v.toPrecision(digits + 1)).toString();
}

export function prettyJson(v: unknown): string {
  if (v === undefined) return "";
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

/** Compact single-line rendering of any JSON value. */
export function compactValue(v: JsonValue | undefined): string {
  if (v === undefined || v === null) return "—";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return JSON.stringify(v);
}

export function isEmptyJson(v: unknown): boolean {
  if (v === null || v === undefined || v === "") return true;
  if (Array.isArray(v)) return v.length === 0;
  if (typeof v === "object") return Object.keys(v as object).length === 0;
  return false;
}

/** "k=v, k2=v2" for run params; falls back to run.name. */
export function runLabel(run: Run): string {
  if (run.name) return run.name;
  const p = run.metadata?.params;
  if (p && Object.keys(p).length) {
    return Object.entries(p)
      .map(([k, v]) => `${k}=${compactValue(v)}`)
      .join(", ");
  }
  return shortId(run.id);
}

// ANSI CSI / OSC escape sequences and other C0 controls (except \t \n \r).
// eslint-disable-next-line no-control-regex
const ANSI_RE = /\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g;
export function stripAnsi(s: string): string {
  return s.replace(ANSI_RE, "");
}

export function basename(uri: string): string {
  const clean = uri.split(/[?#]/)[0].replace(/\/+$/, "");
  const i = clean.lastIndexOf("/");
  return i >= 0 ? clean.slice(i + 1) : clean;
}

export function extOf(uri: string): string {
  const b = basename(uri).toLowerCase();
  const i = b.lastIndexOf(".");
  return i >= 0 ? b.slice(i + 1) : "";
}

export function gpuSummary(n: Node): string {
  const gpus = n.capacity?.gpus ?? [];
  if (!gpus.length) return n.gpu_stats?.length ? `${n.gpu_stats.length}× ${n.gpu_stats[0].model}` : translate("no GPU");
  const models = new Map<string, number>();
  for (const g of gpus) models.set(g.model, (models.get(g.model) ?? 0) + 1);
  return [...models].map(([m, c]) => `${c}× ${m}`).join(", ");
}

const shq = (s: string) => (/^[\w@%+=:,./~-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`);

/** The `mldojo node add` command equivalent to an existing node's config. */
export function nodeAddCommand(n: Node): string {
  const c = n.connection ?? { type: "local" };
  const parts = ["mldojo node add", `--id ${shq(n.id)}`];
  if (c.type === "ssh" && c.host) parts.push(`--ssh ${shq(c.user ? `${c.user}@${c.host}` : c.host)}`);
  if (c.port && c.port !== 22) parts.push(`--port ${c.port}`);
  if (c.identity) parts.push(`--identity ${shq(c.identity)}`);
  for (const v of c.via ?? []) parts.push(`--via ${shq(v.node)}`);
  if (n.labels?.length) parts.push(`--labels ${shq(n.labels.join(","))}`);
  if (n.proxy?.https || n.proxy?.http) parts.push(`--proxy ${shq((n.proxy.https || n.proxy.http)!)}`);
  return parts.join(" \\\n    ");
}
