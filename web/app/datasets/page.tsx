"use client";

import Link from "next/link";
import { Star } from "lucide-react";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Dataset } from "@/lib/types";
import { formatTime } from "@/lib/utils";
import { CliHint, Empty, ErrorBox, Loading, Mono, PageHeader } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

export default function DatasetsPage() {
  const { data, error, loading, reload } = useApi<Dataset[]>("/datasets");
  const datasets = data ?? [];
  const { t } = useT();
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={t("Datasets")} description={t("Registered datasets and where their copies live.")} />
      <ErrorBox error={error} onRetry={reload} />
      {loading ? (
        <Loading />
      ) : datasets.length === 0 ? (
        <Empty>{t("No datasets registered.")}</Empty>
      ) : (
        <div className="flex flex-col gap-3">
          {datasets.map((d) => (
            <Card key={d.id || `${d.name}@${d.version}`}>
              <CardHeader className="flex-row flex-wrap items-baseline justify-between gap-2 pb-2">
                <div className="flex items-baseline gap-2">
                  <span className="font-semibold">{d.name}</span>
                  <Badge variant="outline">{d.version}</Badge>
                  <span className="text-xs text-muted-foreground">
                    {t("mounted at")} <Mono>{d.mount || "—"}</Mono>
                  </span>
                </div>
                <span className="text-xs text-muted-foreground">{t("registered {time}", { time: formatTime(d.created_at) })}</span>
              </CardHeader>
              <CardContent className="px-0 pb-1">
                {(d.locations ?? []).length === 0 ? (
                  <div className="px-4 pb-3 text-sm text-muted-foreground">{t("No locations.")}</div>
                ) : (
                  <Table>
                    <TableHeader>
                      <TableRow className="hover:bg-transparent">
                        <TableHead className="pl-4">{t("Kind")}</TableHead>
                        <TableHead>{t("Where")}</TableHead>
                        <TableHead>{t("Path")}</TableHead>
                        <TableHead className="hidden md:table-cell">{t("Credentials")}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {(d.locations ?? []).map((l, i) => (
                        <TableRow key={l.id || i}>
                          <TableCell className="pl-4">
                            <span className="inline-flex items-center gap-1.5">
                              <Badge variant={l.kind === "bucket" ? "violet" : "outline"}>{l.kind}</Badge>
                              {l.authoritative && (
                                <Badge variant="success" title={t("Authoritative source")}>
                                  <Star /> {t("source")}
                                </Badge>
                              )}
                            </span>
                          </TableCell>
                          <TableCell className="whitespace-nowrap">
                            {l.kind === "node_path" && l.node ? (
                              <Link href={`/node?id=${encodeURIComponent(l.node)}`} className="hover:underline">
                                {l.node}
                              </Link>
                            ) : (
                              <span>{[l.provider, l.bucket].filter(Boolean).join(" / ") || "—"}</span>
                            )}
                          </TableCell>
                          <TableCell className="break-all">
                            <Mono>{l.path || "—"}</Mono>
                          </TableCell>
                          <TableCell className="hidden md:table-cell">
                            {l.credentials ? <Mono className="text-muted-foreground">{l.credentials}</Mono> : "—"}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
              </CardContent>
            </Card>
          ))}
        </div>
      )}
      <CliHint
        title={t("Register from the CLI:")}
        command={"mldojo dataset register --name pusht --version v1 \\\n    --location node:gpu-1:/data/pusht --authoritative"}
      />
    </div>
  );
}
