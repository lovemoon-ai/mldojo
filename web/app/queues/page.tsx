"use client";

import * as React from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Queue } from "@/lib/types";
import { isEmptyJson } from "@/lib/utils";
import { CliHint, Empty, ErrorBox, JsonBlock, KV, Loading, Mono, PageHeader } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

export default function QueuesPage() {
  const { data, error, loading, reload } = useApi<Queue[]>("/queues", { interval: 30000 });
  const [open, setOpen] = React.useState<string | null>(null);
  const { t } = useT();
  const queues = data ?? [];
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={t("Queues")} description={t("Submission queues provided by queue plugins. Target as queue:<id>.")} />
      <ErrorBox error={error} onRetry={reload} />
      {loading ? (
        <Loading />
      ) : queues.length === 0 ? (
        <Empty>{t("No queues registered.")}</Empty>
      ) : (
        <Card>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-6" />
                <TableHead>{t("Queue")}</TableHead>
                <TableHead>{t("Backend")}</TableHead>
                <TableHead className="hidden md:table-cell">{t("Labels")}</TableHead>
                <TableHead className="hidden lg:table-cell">{t("Client")}</TableHead>
                <TableHead className="text-right">{t("Active")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {queues.map((q) => {
                const isOpen = open === q.id;
                return (
                  <React.Fragment key={q.id}>
                    <TableRow className="cursor-pointer" onClick={() => setOpen(isOpen ? null : q.id)} aria-expanded={isOpen}>
                      <TableCell className="pr-0 text-muted-foreground">
                        {isOpen ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
                      </TableCell>
                      <TableCell className="max-w-md">
                        <div className="truncate font-mono text-xs font-medium">{q.id}</div>
                        {q.display_name && <div className="truncate text-xs text-muted-foreground">{q.display_name}</div>}
                      </TableCell>
                      <TableCell>
                        <Badge variant="violet">{q.backend}</Badge>
                      </TableCell>
                      <TableCell className="hidden md:table-cell">
                        <div className="flex flex-wrap gap-1">
                          {(q.labels ?? []).map((l) => (
                            <Badge key={l} variant="outline" className="font-normal">
                              {l}
                            </Badge>
                          ))}
                        </div>
                      </TableCell>
                      <TableCell className="hidden text-xs text-muted-foreground lg:table-cell">
                        {q.client?.sdk}
                        {q.client?.project_id && ` · ${q.client.project_id}`}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">{q.active_runs}</TableCell>
                    </TableRow>
                    {isOpen && (
                      <TableRow className="hover:bg-transparent">
                        <TableCell colSpan={6} className="bg-muted/30">
                          <div className="grid grid-cols-1 gap-4 py-1 lg:grid-cols-2">
                            <KV
                              items={[
                                [t("Target"), <Mono key="t">queue:{q.id}</Mono>],
                                [t("SDK"), q.client?.sdk || "—"],
                                [t("Project ID"), q.client?.project_id || "—"],
                                [t("Credentials"), q.client?.credentials ? <Mono>{q.client.credentials}</Mono> : "—"],
                                [t("Job password"), q.client?.job_password ? <Mono>{q.client.job_password}</Mono> : "—"],
                                [t("Proxy"), q.proxy?.https || q.proxy?.http ? <Mono>{q.proxy.https || q.proxy.http}</Mono> : "—"],
                              ]}
                            />
                            <div className="flex flex-col gap-2">
                              <div className="text-xs text-muted-foreground">{t("Defaults")}</div>
                              {isEmptyJson(q.defaults) ? <span className="text-sm">—</span> : <JsonBlock value={q.defaults} maxHeight="14rem" />}
                              <div className="text-xs text-muted-foreground">{t("Capacity hint")}</div>
                              {isEmptyJson(q.capacity_hint) ? <span className="text-sm">—</span> : <JsonBlock value={q.capacity_hint} maxHeight="10rem" />}
                            </div>
                          </div>
                        </TableCell>
                      </TableRow>
                    )}
                  </React.Fragment>
                );
              })}
            </TableBody>
          </Table>
        </Card>
      )}
      <CliHint
        title={t("Queues are added from the CLI:")}
        command="mldojo queue add --id <plugin>/<queue> --backend <plugin> --credentials secret://<plugin>/default [--defaults-file q.yaml]"
      />
    </div>
  );
}
