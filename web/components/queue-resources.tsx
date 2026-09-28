"use client";

import * as React from "react";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { QueueResource, QueueResources } from "@/lib/types";
import { cn, formatDuration } from "@/lib/utils";
import { ErrorBox, Section } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

function UsageBar({ q }: { q: QueueResource }) {
  const pct = q.total > 0 ? Math.min(100, (q.used / q.total) * 100) : 0;
  const full = q.total > 0 && q.free <= 0;
  return (
    <div className="flex min-w-32 items-center gap-2">
      <div
        className={cn("h-1.5 flex-1 overflow-hidden rounded-full", full ? "bg-amber-500/20" : "bg-[color-mix(in_srgb,var(--chart-1)_18%,transparent)]")}
        role="meter"
        aria-valuemin={0}
        aria-valuemax={q.total}
        aria-valuenow={q.used}
      >
        <div className={cn("h-full rounded-full", full && "bg-amber-500")} style={{ width: `${pct}%`, background: full ? undefined : "var(--chart-1)" }} />
      </div>
      <span className="whitespace-nowrap text-xs tabular-nums text-muted-foreground">
        {q.used}/{q.total}
      </span>
    </div>
  );
}

/** Live capacity of the scheduler queues behind the queue plugins (GET /queues/resources). */
export function QueueResourcesCard() {
  const { data, error, reload } = useApi<QueueResources>("/queues/resources", { interval: 30000 });
  const [showAll, setShowAll] = React.useState(false);
  const { t } = useT();
  const all = data?.queues ?? [];
  const errors = Object.entries(data?.errors ?? {});
  if (!error && all.length === 0 && errors.length === 0) return null;
  const hidden = all.filter((q) => !q.usable).length;
  const rows = showAll ? all : all.filter((q) => q.usable);
  return (
    <Section
      title={t("Cluster resources")}
      actions={
        hidden > 0 && (
          <Button variant="ghost" size="xs" onClick={() => setShowAll(!showAll)}>
            {showAll ? t("Hide queues without permission") : t("Show {n} queues without permission", { n: hidden })}
          </Button>
        )
      }
    >
      <ErrorBox error={error} onRetry={reload} />
      {errors.map(([plugin, msg]) => (
        <div key={plugin} className="text-xs text-amber-700 dark:text-amber-400">
          {plugin}: {msg}
        </div>
      ))}
      {rows.length > 0 && (
        <Card>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{t("Queue")}</TableHead>
                <TableHead>{t("Accelerator")}</TableHead>
                <TableHead>{t("Used / total")}</TableHead>
                <TableHead className="text-right">{t("Free")}</TableHead>
                <TableHead className="text-right">{t("Running / queued jobs")}</TableHead>
                <TableHead className="hidden text-right md:table-cell">{t("Utilization")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((q) => (
                <TableRow key={`${q.plugin}/${q.name}`} className={cn(!q.usable && "opacity-60")}>
                  <TableCell className="max-w-md">
                    <div className="flex items-center gap-1.5">
                      <span className="truncate font-mono text-xs font-medium">{q.name}</span>
                      {q.queue_id && <Badge variant="info">{t("registered")}</Badge>}
                      {!q.usable && <Badge variant="muted">{t("no permission")}</Badge>}
                    </div>
                    <div className="truncate text-xs text-muted-foreground">
                      {q.plugin} · {q.cluster}
                    </div>
                  </TableCell>
                  <TableCell className="text-xs">
                    {q.accelerator.replaceAll("_", " ")}
                    {q.unit === "cpu" && <span className="text-muted-foreground"> ({t("cores")})</span>}
                  </TableCell>
                  <TableCell>
                    <UsageBar q={q} />
                  </TableCell>
                  <TableCell className={cn("text-right tabular-nums", q.free > 0 ? "text-emerald-700 dark:text-emerald-400" : "text-muted-foreground")}>
                    {q.free}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {q.running_jobs} / <span className={cn(q.queued_jobs > 0 && "text-amber-700 dark:text-amber-400")}>{q.queued_jobs}</span>
                    {q.queued_jobs > 0 && q.queued_wait_sec > 0 && (
                      <div className="text-xs text-muted-foreground">{t("oldest waiting {d}", { d: formatDuration(q.queued_wait_sec * 1000) })}</div>
                    )}
                  </TableCell>
                  <TableCell className="hidden text-right tabular-nums md:table-cell">
                    {q.utilization == null ? "—" : `${Math.round(q.utilization * 100)}%`}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}
    </Section>
  );
}
