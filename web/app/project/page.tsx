"use client";

import { Suspense } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Experiment, Project, Run } from "@/lib/types";
import { formatTime, relativeTime } from "@/lib/utils";
import { CliHint, Empty, ErrorBox, Loading, PageFallback, PageHeader, Section } from "@/components/common";
import { RunsTable } from "@/components/runs-table";
import { StatusBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

function ProjectView() {
  const name = useSearchParams().get("name") || "";
  const { t } = useT();
  const project = useApi<Project>(name ? `/projects/${seg(name)}` : null);
  const exps = useApi<Experiment[]>(name ? `/projects/${seg(name)}/experiments` : null);
  const runs = useApi<Run[]>(name ? `/runs?project=${encodeURIComponent(name)}&limit=20` : null, { interval: 10000 });

  if (!name) return <Empty>{t("No project name given.")}</Empty>;
  const list = exps.data ?? [];

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        crumbs={[{ label: t("Projects"), href: "/projects" }, { label: name }]}
        title={name}
        description={
          project.data ? (
            <span>
              {project.data.description ? `${project.data.description} · ` : ""}
              {t("{e} experiments · {r} runs", { e: project.data.experiment_count, r: project.data.run_count })}
            </span>
          ) : undefined
        }
      />
      <ErrorBox error={project.error} />

      <Section title={t("Experiments")}>
        <ErrorBox error={exps.error} />
        {exps.loading ? (
          <Loading />
        ) : list.length === 0 ? (
          <div className="flex flex-col gap-3">
            <Empty>{t("No experiments in this project.")}</Empty>
            <CliHint title={t("Create one from the CLI")} command={`mldojo exp create ${name} -f recipe.yaml`} />
          </div>
        ) : (
          <Card>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{t("Experiment")}</TableHead>
                  <TableHead>{t("Runs")}</TableHead>
                  <TableHead className="hidden md:table-cell">{t("Tags")}</TableHead>
                  <TableHead className="hidden sm:table-cell">{t("Updated")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.map((e) => (
                  <TableRow key={e.id || e.name}>
                    <TableCell className="max-w-md">
                      <Link
                        href={`/experiment?project=${encodeURIComponent(name)}&name=${encodeURIComponent(e.name)}`}
                        className="font-medium hover:underline"
                      >
                        {e.name}
                      </Link>
                      {e.description && <div className="truncate text-xs text-muted-foreground">{e.description}</div>}
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {Object.entries(e.run_counts ?? {}).map(([s, n]) => (
                          <span key={s} className="inline-flex items-center gap-0.5">
                            <StatusBadge status={s} />
                            <span className="text-xs tabular-nums text-muted-foreground">{n}</span>
                          </span>
                        ))}
                        {!Object.keys(e.run_counts ?? {}).length && <span className="text-muted-foreground">—</span>}
                      </div>
                    </TableCell>
                    <TableCell className="hidden md:table-cell">
                      <div className="flex flex-wrap gap-1">
                        {(e.tags ?? []).map((tag) => (
                          <Badge key={tag} variant="outline">
                            {tag}
                          </Badge>
                        ))}
                      </div>
                    </TableCell>
                    <TableCell className="hidden whitespace-nowrap text-muted-foreground sm:table-cell" title={formatTime(e.updated_at)}>
                      {relativeTime(e.updated_at)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        )}
      </Section>

      <Section
        title={t("Recent runs")}
        actions={
          <Link href={`/runs?project=${encodeURIComponent(name)}`} className="text-xs text-muted-foreground hover:text-foreground hover:underline">
            {t("All runs →")}
          </Link>
        }
      >
        <ErrorBox error={runs.error} />
        {runs.loading ? (
          <Loading />
        ) : (
          <Card>
            <RunsTable runs={runs.data ?? []} showProject />
          </Card>
        )}
      </Section>
    </div>
  );
}

export default function ProjectPage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <ProjectView />
    </Suspense>
  );
}
