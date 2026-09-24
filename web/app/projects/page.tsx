"use client";

import Link from "next/link";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Project } from "@/lib/types";
import { formatTime, relativeTime } from "@/lib/utils";
import { CliHint, Empty, ErrorBox, Loading, PageHeader } from "@/components/common";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

export default function ProjectsPage() {
  const { data, error, loading, reload } = useApi<Project[]>("/projects");
  const projects = data ?? [];
  const { t } = useT();
  return (
    <div>
      <PageHeader title={t("Projects")} description={t("Project → Experiment → Run.")} />
      <ErrorBox error={error} onRetry={reload} className="mb-3" />
      {loading ? (
        <Loading />
      ) : projects.length === 0 ? (
        <div className="flex flex-col gap-3">
          <Empty>{t("No projects yet.")}</Empty>
          <CliHint title={t("Create one from the CLI")} command="mldojo project create <name>" />
        </div>
      ) : (
        <Card>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{t("Name")}</TableHead>
                <TableHead className="text-right">{t("Experiments")}</TableHead>
                <TableHead className="text-right">{t("Runs")}</TableHead>
                <TableHead className="hidden sm:table-cell">{t("Owner")}</TableHead>
                <TableHead className="hidden md:table-cell">{t("Updated")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {projects.map((p) => (
                <TableRow key={p.id || p.name}>
                  <TableCell className="max-w-md">
                    <Link href={`/project?name=${encodeURIComponent(p.name)}`} className="font-medium hover:underline">
                      {p.name}
                    </Link>
                    {p.description && <div className="truncate text-xs text-muted-foreground">{p.description}</div>}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">{p.experiment_count}</TableCell>
                  <TableCell className="text-right tabular-nums">{p.run_count}</TableCell>
                  <TableCell className="hidden text-muted-foreground sm:table-cell">{p.owner || "—"}</TableCell>
                  <TableCell className="hidden whitespace-nowrap text-muted-foreground md:table-cell" title={formatTime(p.updated_at)}>
                    {relativeTime(p.updated_at)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  );
}
