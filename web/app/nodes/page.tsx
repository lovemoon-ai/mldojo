"use client";

import Link from "next/link";
import { Cpu, HardDrive, MemoryStick } from "lucide-react";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Node } from "@/lib/types";
import { gpuSummary, relativeTime } from "@/lib/utils";
import { CliHint, Empty, ErrorBox, Loading, PageHeader } from "@/components/common";
import { GpuBars, useLiveGpu } from "@/components/gpu-bars";
import { AgentBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader } from "@/components/ui/card";

const NODE_ADD_HINT = `mldojo node add --id <id> --ssh <user>@<host> [--port 22] \\
    [--identity secret://ssh_keys/<key>] [--via <node>] \\
    [--labels 5090,8gpu] [--proxy http://proxy:port]`;

function NodeCard({ node }: { node: Node }) {
  const { stats, live } = useLiveGpu(node.id, node.gpu_stats, node.agent_status !== "offline");
  const cap = node.capacity;
  const { t } = useT();
  return (
    <Card className="flex flex-col">
      <CardHeader className="pb-3">
        <div className="flex items-start justify-between gap-2">
          <div className="min-w-0">
            <Link href={`/node?id=${encodeURIComponent(node.id)}`} className="font-semibold hover:underline">
              {node.id}
            </Link>
            {node.display_name && node.display_name !== node.id && (
              <div className="truncate text-xs text-muted-foreground">{node.display_name}</div>
            )}
          </div>
          <AgentBadge status={node.agent_status} />
        </div>
        {(node.labels ?? []).length > 0 && (
          <div className="flex flex-wrap gap-1 pt-1">
            {(node.labels ?? []).map((l) => (
              <Badge key={l} variant="outline" className="font-normal">
                {l}
              </Badge>
            ))}
          </div>
        )}
      </CardHeader>
      <CardContent className="flex flex-1 flex-col gap-3">
        <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
          <span>{gpuSummary(node)}</span>
          {cap && (
            <>
              <span className="inline-flex items-center gap-1">
                <Cpu className="size-3" />
                {cap.cpu} CPU
              </span>
              <span className="inline-flex items-center gap-1">
                <MemoryStick className="size-3" />
                {cap.mem_gb} GB
              </span>
              <span className="inline-flex items-center gap-1">
                <HardDrive className="size-3" />
                {cap.disk_gb} GB
              </span>
            </>
          )}
        </div>
        <GpuBars stats={stats} compact />
        <div className="mt-auto flex items-center justify-between border-t border-border pt-2 text-xs text-muted-foreground">
          <span>
            {t(node.active_runs === 1 ? "{n} active run" : "{n} active runs", { n: node.active_runs })}
          </span>
          <span className="flex items-center gap-1.5">
            {live && <span className="size-1.5 animate-pulse rounded-full bg-emerald-500" title={t("live")} />}
            {node.last_heartbeat ? t("heartbeat {time}", { time: relativeTime(node.last_heartbeat) }) : t("no heartbeat")}
          </span>
        </div>
      </CardContent>
    </Card>
  );
}

export default function NodesPage() {
  const { data, error, loading, reload } = useApi<Node[]>("/nodes", { interval: 15000 });
  const nodes = data ?? [];
  const { t } = useT();
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={t("Nodes")} description={t("Machines running mldojo-agent (reverse-connected).")} />
      <ErrorBox error={error} onRetry={reload} />
      {loading ? (
        <Loading />
      ) : nodes.length === 0 ? (
        <Empty>{t("No nodes registered yet.")}</Empty>
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 2xl:grid-cols-3">
          {nodes.map((n) => (
            <NodeCard key={n.id} node={n} />
          ))}
        </div>
      )}
      <CliHint title={t("Nodes are added from the CLI (deploys the agent over SSH):")} command={NODE_ADD_HINT} />
    </div>
  );
}
