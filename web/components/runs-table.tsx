"use client";

import Link from "next/link";
import type { Run } from "@/lib/types";
import { useNow } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import { formatDuration, formatTime, relativeTime, runDurationMs, runLabel, shortId } from "@/lib/utils";
import { StatusBadge, TargetBadge } from "@/components/status";
import { Empty } from "@/components/common";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

export function RunsTable({
  runs,
  showProject = true,
  selectable,
  selected,
  onToggle,
  empty,
}: {
  runs: Run[];
  showProject?: boolean;
  selectable?: boolean;
  selected?: string[];
  onToggle?: (id: string) => void;
  empty?: React.ReactNode;
}) {
  const anyActive = runs.some((r) => r.status === "running" || r.status === "starting");
  const now = useNow(1000, anyActive);
  const { t } = useT();
  if (!runs.length) return <Empty>{empty ?? t("No runs yet.")}</Empty>;
  return (
    <Table>
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          {selectable && <TableHead className="w-8" />}
          <TableHead>{t("Run")}</TableHead>
          <TableHead>{t("Status")}</TableHead>
          {showProject && <TableHead className="hidden md:table-cell">{t("Project / Experiment")}</TableHead>}
          <TableHead className="hidden sm:table-cell">{t("Target")}</TableHead>
          <TableHead className="hidden lg:table-cell">{t("Started")}</TableHead>
          <TableHead className="text-right">{t("Duration")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {runs.map((r) => {
          const checked = selected?.includes(r.id) ?? false;
          return (
            <TableRow key={r.id} data-state={checked ? "selected" : undefined}>
              {selectable && (
                <TableCell className="pr-0">
                  <input
                    type="checkbox"
                    aria-label={t("Select run {id}", { id: r.id })}
                    className="size-4 cursor-pointer accent-current"
                    checked={checked}
                    onChange={() => onToggle?.(r.id)}
                  />
                </TableCell>
              )}
              <TableCell className="max-w-[18rem]">
                <Link href={`/run?id=${encodeURIComponent(r.id)}`} className="block truncate font-medium hover:underline">
                  {runLabel(r)}
                </Link>
                <div className="whitespace-nowrap font-mono text-[11px] text-muted-foreground">{shortId(r.id)}</div>
              </TableCell>
              <TableCell>
                <StatusBadge status={r.status} />
              </TableCell>
              {showProject && (
                <TableCell className="hidden max-w-[14rem] md:table-cell">
                  <div className="truncate">
                    <Link href={`/project?name=${encodeURIComponent(r.project)}`} className="hover:underline">
                      {r.project}
                    </Link>
                    <span className="text-muted-foreground"> / </span>
                    <Link
                      href={`/experiment?project=${encodeURIComponent(r.project)}&name=${encodeURIComponent(r.experiment)}`}
                      className="hover:underline"
                    >
                      {r.experiment}
                    </Link>
                  </div>
                </TableCell>
              )}
              <TableCell className="hidden sm:table-cell">
                <TargetBadge target={r.target} />
              </TableCell>
              <TableCell className="hidden whitespace-nowrap text-muted-foreground lg:table-cell" title={formatTime(r.started_at ?? r.created_at)}>
                {r.started_at ? relativeTime(r.started_at, now) : t("created {time}", { time: relativeTime(r.created_at, now) })}
              </TableCell>
              <TableCell className="whitespace-nowrap text-right tabular-nums">{formatDuration(runDurationMs(r, now))}</TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
