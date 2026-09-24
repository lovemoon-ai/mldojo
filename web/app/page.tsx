"use client";

import Link from "next/link";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Health, Node, Run } from "@/lib/types";
import { relativeTime } from "@/lib/utils";
import { Empty, ErrorBox, Loading, PageHeader, Section } from "@/components/common";
import { RunsTable } from "@/components/runs-table";
import { AgentBadge } from "@/components/status";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

function Tile({ label, value, sub, href }: { label: string; value: React.ReactNode; sub?: React.ReactNode; href?: string }) {
  const body = (
    <Card className="h-full px-4 py-3 transition-colors hover:bg-accent/40">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-1 text-2xl font-semibold">{value}</div>
      {sub && <div className="mt-0.5 text-xs text-muted-foreground">{sub}</div>}
    </Card>
  );
  return href ? <Link href={href}>{body}</Link> : body;
}

function avgUtil(n: Node): number | null {
  const g = n.gpu_stats;
  if (!g?.length) return null;
  return g.reduce((s, x) => s + x.util, 0) / g.length;
}

export default function Dashboard() {
  const { t } = useT();
  const recent = useApi<Run[]>("/runs?limit=15", { interval: 10000 });
  const running = useApi<Run[]>("/runs?status=running&limit=500", { interval: 10000 });
  const queued = useApi<Run[]>("/runs?status=queued&limit=500", { interval: 10000 });
  const nodes = useApi<Node[]>("/nodes", { interval: 10000 });
  const health = useApi<Health>("/health");

  const nodeList = nodes.data ?? [];
  const online = nodeList.filter((n) => n.agent_status === "online").length;
  const gpus = nodeList.reduce((s, n) => s + (n.gpu_stats?.length ?? n.capacity?.gpus?.length ?? 0), 0);
  const plugins = Object.entries(health.data?.queue_plugins ?? {});
  const busyGpus = nodeList.reduce((s, n) => s + (n.gpu_stats ?? []).filter((g) => g.util >= 10).length, 0);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={t("Dashboard")}
        description={
          health.data ? (
            <span>
              API {health.data.version || "?"} · db {health.data.db || "?"} · secrets {health.data.secrets || "?"} ·{" "}
              {t("queue plugins")}{" "}
              {plugins.length ? plugins.map(([name, ok]) => `${name} ${ok ? "✓" : "✗"}`).join(", ") : t("none configured")}
            </span>
          ) : undefined
        }
      />
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Tile label={t("Running runs")} value={running.data?.length ?? "—"} href="/runs?status=running" />
        <Tile label={t("Queued runs")} value={queued.data?.length ?? "—"} href="/runs?status=queued" />
        <Tile label={t("Nodes online")} value={nodes.data ? `${online}/${nodeList.length}` : "—"} href="/nodes" />
        <Tile label={t("GPUs busy")} value={nodes.data ? `${busyGpus}/${gpus}` : "—"} sub={t("util ≥ 10%")} href="/nodes" />
      </div>

      <ErrorBox error={recent.error || nodes.error} />

      <div className="grid grid-cols-1 gap-6 2xl:grid-cols-[minmax(0,1fr)_22rem]">
        <Section
          title={t("Recent runs")}
          actions={
            <Link href="/runs" className="text-xs text-muted-foreground hover:text-foreground hover:underline">
              {t("All runs →")}
            </Link>
          }
        >
          {recent.loading ? (
            <Loading />
          ) : (
            <Card>
              <RunsTable runs={recent.data ?? []} empty={t("No runs yet. Submit one with {cmd}.", { cmd: "`mldojo run submit -f recipe.yaml --target node:local`" })} />
            </Card>
          )}
        </Section>

        <Section
          title={t("Nodes")}
          actions={
            <Link href="/nodes" className="text-xs text-muted-foreground hover:text-foreground hover:underline">
              {t("All nodes →")}
            </Link>
          }
        >
          {nodes.loading ? (
            <Loading />
          ) : nodeList.length === 0 ? (
            <Empty>{t("No nodes registered.")}</Empty>
          ) : (
            <Card>
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>{t("Node")}</TableHead>
                    <TableHead>{t("Agent")}</TableHead>
                    <TableHead className="text-right">{t("GPU util")}</TableHead>
                    <TableHead className="text-right">{t("Runs")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {nodeList.map((n) => {
                    const u = avgUtil(n);
                    return (
                      <TableRow key={n.id}>
                        <TableCell>
                          <Link href={`/node?id=${encodeURIComponent(n.id)}`} className="font-medium hover:underline">
                            {n.id}
                          </Link>
                          <div className="text-[11px] text-muted-foreground">
                            {n.capacity?.gpus?.length ? `${n.capacity.gpus.length}× ${n.capacity.gpus[0].model}` : t("no GPU")}
                            {n.last_heartbeat && ` · ${relativeTime(n.last_heartbeat)}`}
                          </div>
                        </TableCell>
                        <TableCell>
                          <AgentBadge status={n.agent_status} />
                        </TableCell>
                        <TableCell className="text-right tabular-nums">{u === null ? "—" : `${u.toFixed(0)}%`}</TableCell>
                        <TableCell className="text-right tabular-nums">{n.active_runs}</TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </Card>
          )}
        </Section>
      </div>
    </div>
  );
}
