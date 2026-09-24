"use client";

import * as React from "react";
import { Suspense } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { CheckCircle2, Loader2, PlugZap, Trash2, XCircle } from "lucide-react";
import { api, seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Node, NodeTestResult, OkResponse, Run } from "@/lib/types";
import { compactValue, formatTime, gpuSummary, nodeAddCommand, relativeTime } from "@/lib/utils";
import { CliHint, ConfirmDialog, Empty, ErrorBox, KV, Loading, Mono, PageFallback, PageHeader, Section } from "@/components/common";
import { GpuBars, useLiveGpu } from "@/components/gpu-bars";
import { RunsTable } from "@/components/runs-table";
import { AgentBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

function TestResult({ r }: { r: NodeTestResult }) {
  const { t } = useT();
  const Flag = ({ ok, label }: { ok: boolean; label: string }) => (
    <span className={ok ? "inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-400" : "inline-flex items-center gap-1 text-red-700 dark:text-red-400"}>
      {ok ? <CheckCircle2 className="size-3.5" /> : <XCircle className="size-3.5" />}
      {label}
    </span>
  );
  return (
    <div className="flex flex-col gap-1 rounded-md border border-border bg-muted/40 px-3 py-2 text-sm">
      <div className="flex flex-wrap items-center gap-3">
        <Flag ok={r.ok} label={r.ok ? t("OK") : t("Failed")} />
        <Flag ok={r.reachable} label={t("reachable")} />
        <Flag ok={r.agent_online} label={t("agent online")} />
        {typeof r.latency_ms === "number" && <span className="tabular-nums text-muted-foreground">{r.latency_ms} ms</span>}
        {r.via && <span className="text-muted-foreground">{t("via {node}", { node: compactValue(r.via) })}</span>}
      </div>
      {r.message && <div className="text-xs text-muted-foreground">{r.message}</div>}
    </div>
  );
}

function NodeView() {
  const id = useSearchParams().get("id") || "";
  const router = useRouter();
  const { t } = useT();
  const { data: node, error, loading } = useApi<Node>(id ? `/nodes/${seg(id)}` : null, { interval: 15000 });
  const runs = useApi<Run[]>(id ? `/runs?target=${encodeURIComponent(`node:${id}`)}&limit=20` : null, { interval: 15000 });
  const { stats, live } = useLiveGpu(id, node?.gpu_stats, !!node && node.agent_status !== "offline");

  const [testing, setTesting] = React.useState(false);
  const [test, setTest] = React.useState<NodeTestResult>();
  const [testError, setTestError] = React.useState<unknown>();
  const [confirmDelete, setConfirmDelete] = React.useState(false);
  const [stopAgent, setStopAgent] = React.useState(true);

  const runTest = async () => {
    setTesting(true);
    setTest(undefined);
    setTestError(undefined);
    try {
      setTest(await api.post<NodeTestResult>(`/nodes/${seg(id)}/test`));
    } catch (e) {
      setTestError(e);
    } finally {
      setTesting(false);
    }
  };

  if (!id) return <Empty>{t("No node id given.")}</Empty>;
  if (loading) return <Loading />;
  if (!node) return <ErrorBox error={error ?? t("Node not found")} />;

  const c = node.connection ?? { type: "local" };
  const cap = node.capacity;
  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        crumbs={[{ label: t("Nodes"), href: "/nodes" }, { label: node.id }]}
        title={node.display_name || node.id}
        description={node.display_name && node.display_name !== node.id ? <Mono>{node.id}</Mono> : undefined}
        actions={
          <>
            <Button size="sm" variant="outline" onClick={runTest} disabled={testing}>
              {testing ? <Loader2 className="animate-spin" /> : <PlugZap />}
              {t("Test")}
            </Button>
            <Button size="sm" variant="destructive" onClick={() => setConfirmDelete(true)}>
              <Trash2 /> {t("Delete")}
            </Button>
          </>
        }
      >
        <div className="flex flex-wrap items-center gap-1.5">
          <AgentBadge status={node.agent_status} />
          {(node.labels ?? []).map((l) => (
            <Badge key={l} variant="outline" className="font-normal">
              {l}
            </Badge>
          ))}
          <span className="text-xs text-muted-foreground">
            {node.agent_version && `${t("agent {version}", { version: node.agent_version })} · `}
            {node.last_heartbeat ? t("heartbeat {time}", { time: relativeTime(node.last_heartbeat) }) : t("no heartbeat")}
          </span>
        </div>
        {error ? <ErrorBox error={error} /> : null}
        <ErrorBox error={testError} />
        {test && <TestResult r={test} />}
      </PageHeader>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader className="flex-row items-center justify-between">
            <CardTitle>{t("GPUs")}</CardTitle>
            <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
              {live && <span className="size-1.5 animate-pulse rounded-full bg-emerald-500" />}
              {live ? t("live") : t("snapshot")} · {gpuSummary(node)}
            </span>
          </CardHeader>
          <CardContent>
            <GpuBars stats={stats} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{t("Capacity")}</CardTitle>
          </CardHeader>
          <CardContent>
            {cap ? (
              <KV
                items={[
                  [t("Hostname"), cap.hostname || "—"],
                  [t("MLDojo dir"), cap.mldojo_dir || "—"],
                  [t("OS / arch"), [cap.os, cap.arch].filter(Boolean).join(" / ") || "—"],
                  ["CPU", cap.cpu],
                  [t("Memory"), `${cap.mem_gb} GB`],
                  [t("Disk"), `${cap.disk_gb} GB`],
                  [t("GPUs"), (cap.gpus ?? []).map((g) => `#${g.index} ${g.model} (${g.mem_gb} GB)`).join("\n") || "—"],
                  [t("Active runs"), node.active_runs],
                ]}
                className="[&_dd]:whitespace-pre-line"
              />
            ) : (
              <div className="text-sm text-muted-foreground">{t("Not reported yet (the agent reports capacity on connect).")}</div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{t("Connection")}</CardTitle>
          </CardHeader>
          <CardContent>
            <KV
              items={[
                [t("Type"), c.type],
                [t("Host"), c.host ? <Mono>{c.host}{c.port ? `:${c.port}` : ""}</Mono> : "—"],
                [t("User"), c.user ? <Mono>{c.user}</Mono> : "—"],
                [t("Identity"), c.identity ? <Mono>{c.identity}</Mono> : "—"],
                [t("Password"), c.password ? <Mono>{c.password}</Mono> : "—"],
                [t("Via"), c.via?.length ? c.via.map((v) => v.node).join(" → ") : t("direct")],
                [t("Reverse tunnel"), c.reverse_tunnel ? t("yes") : t("no")],
                ...(c.agent_server_url ? ([[t("Agent server"), <Mono key="u">{c.agent_server_url}</Mono>]] as [string, React.ReactNode][]) : []),
                ...(c.extra_ssh_options?.length ? ([[t("SSH options"), <Mono key="o">{c.extra_ssh_options.join(" ")}</Mono>]] as [string, React.ReactNode][]) : []),
              ]}
            />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{t("Paths & proxy")}</CardTitle>
          </CardHeader>
          <CardContent>
            <KV
              items={[
                [t("Workdir root"), node.workdir_root ? <Mono>{node.workdir_root}</Mono> : "—"],
                [t("Datasets cache"), node.datasets_cache_root ? <Mono>{node.datasets_cache_root}</Mono> : "—"],
                [t("HTTP proxy"), node.proxy?.http ? <Mono>{node.proxy.http}</Mono> : "—"],
                [t("HTTPS proxy"), node.proxy?.https ? <Mono>{node.proxy.https}</Mono> : "—"],
                [t("No proxy"), node.proxy?.no_proxy?.length ? <Mono>{node.proxy.no_proxy.join(", ")}</Mono> : "—"],
                [t("Registered"), formatTime(node.created_at)],
              ]}
            />
          </CardContent>
        </Card>
      </div>

      <Section title={t("Recent runs on this node")}>
        <ErrorBox error={runs.error} />
        {runs.loading ? (
          <Loading />
        ) : (
          <Card>
            <RunsTable runs={runs.data ?? []} empty={t("No runs on this node yet.")} />
          </Card>
        )}
      </Section>

      <CliHint title={t("Equivalent CLI (nodes are added from the CLI):")} command={nodeAddCommand(node)} />

      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={t("Delete node {id}?", { id: node.id })}
        description={t("Removes the node from MLDojo. Runs already recorded are kept.")}
        confirmLabel={t("Delete node")}
        destructive
        onConfirm={async () => {
          await api.del<OkResponse>(`/nodes/${seg(node.id)}`, { stop_agent: stopAgent ? 1 : undefined });
          router.push("/nodes");
        }}
      >
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" className="size-4" checked={stopAgent} onChange={(e) => setStopAgent(e.target.checked)} />
          {t("Also stop the agent on the node")}
        </label>
      </ConfirmDialog>
    </div>
  );
}

export default function NodePage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <NodeView />
    </Suspense>
  );
}
