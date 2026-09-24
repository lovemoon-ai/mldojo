"use client";

import * as React from "react";
import { Suspense } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useApi } from "@/lib/hooks";
import { useT, type Msg } from "@/lib/i18n";
import type { CompareResponse, JsonValue, Run } from "@/lib/types";
import { cn, compactValue, formatDuration, formatNumber, runDurationMs, runLabel, shortId } from "@/lib/utils";
import { Empty, ErrorBox, JsonBlock, Loading, PageFallback, PageHeader, Section } from "@/components/common";
import { CompareMany } from "@/components/compare-many";
import { DiffView, looksLikeDiff } from "@/components/diff-view";
import { StatusBadge, TargetBadge } from "@/components/status";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

function RunCard({ label, run }: { label: string; run: Run }) {
  return (
    <Card className="flex flex-col gap-1.5 p-3">
      <div className="flex items-center gap-2">
        <span className="flex size-5 items-center justify-center rounded bg-muted text-xs font-semibold">{label}</span>
        <Link href={`/run?id=${encodeURIComponent(run.id)}`} className="truncate font-medium hover:underline">
          {runLabel(run)}
        </Link>
        <span className="font-mono text-[11px] text-muted-foreground">{shortId(run.id)}</span>
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        <StatusBadge status={run.status} />
        <TargetBadge target={run.target} />
        <span className="text-xs text-muted-foreground">{formatDuration(runDurationMs(run))}</span>
      </div>
      <div className="truncate text-xs text-muted-foreground">
        {run.project} / {run.experiment}
      </div>
    </Card>
  );
}

function codeRows(a: Run, b: Run, yes: string, no: string): [string, string, string][] {
  const f = (r: Run) => ({
    source: r.metadata?.code_source || "—",
    repo: r.metadata?.code_repo || "—",
    ref: r.metadata?.code_ref || "—",
    commit: r.code_commit || "—",
    dirty: r.metadata?.code_dirty ? yes : no,
    patch: r.code_patch_uri || "—",
  });
  const fa = f(a);
  const fb = f(b);
  return (Object.keys(fa) as (keyof typeof fa)[]).map((k) => [k, fa[k], fb[k]]);
}

const CODE_FIELDS: Record<string, Msg> = {
  source: "Source",
  repo: "Repo",
  ref: "Ref",
  commit: "Commit",
  dirty: "Dirty",
  patch: "Patch",
};

const valueText = (v: JsonValue | undefined) => (v === undefined ? "—" : compactValue(v));

function CompareView() {
  const sp = useSearchParams();
  const { t } = useT();
  // ?runs=a,b,c is the general form; ?a=&b= is kept so older links still work.
  const ids = (sp.get("runs") || [sp.get("a"), sp.get("b")].filter(Boolean).join(","))
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
  const a = ids[0] || "";
  const b = ids[1] || "";
  // The A/B endpoint is the only source of the code and environment diff, so
  // it still runs, but only when exactly two runs are being compared.
  const pair = ids.length === 2;
  const { data, error, loading } = useApi<CompareResponse>(
    pair ? `/compare?a=${encodeURIComponent(a)}&b=${encodeURIComponent(b)}` : null,
  );

  if (ids.length < 2) return <Empty>{t("Pick two or more runs to compare from an experiment page.")}</Empty>;

  if (!pair) {
    return (
      <div>
        <PageHeader title={t("Comparing {n} runs", { n: ids.length })} />
        <CompareMany ids={ids} />
      </div>
    );
  }
  if (loading) return <Loading label={t("Comparing…")} />;
  if (!data) return <ErrorBox error={error ?? t("No data")} />;

  const metrics = data.metrics ?? [];
  const env = data.env ?? [];
  const code = data.code ?? {};
  const diffs = Object.entries(code).filter(([, v]) => typeof v === "string" && looksLikeDiff(v)) as [string, string][];
  const otherCode = Object.fromEntries(Object.entries(code).filter(([k]) => !diffs.some(([d]) => d === k)));

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        crumbs={[{ label: t("Runs"), href: "/runs" }, { label: t("Compare") }]}
        title={t("Compare runs")}
      />
      <div className="-mt-2 grid grid-cols-1 gap-3 md:grid-cols-2">
        <RunCard label="A" run={data.a} />
        <RunCard label="B" run={data.b} />
      </div>

      <Section title={t("Code")}>
        <Card>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-28">{t("Field")}</TableHead>
                <TableHead>A</TableHead>
                <TableHead>B</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {codeRows(data.a, data.b, t("yes"), t("no")).map(([k, va, vb]) => (
                <TableRow key={k} className={cn(va !== vb && "bg-amber-500/5")}>
                  <TableCell className="text-muted-foreground">{t(CODE_FIELDS[k as keyof typeof CODE_FIELDS])}</TableCell>
                  <TableCell className="break-all font-mono text-xs">{va}</TableCell>
                  <TableCell className={cn("break-all font-mono text-xs", va !== vb && "font-semibold")}>{vb}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
        {Object.keys(otherCode).length > 0 && <JsonBlock value={otherCode} maxHeight="16rem" />}
        {diffs.map(([k, v]) => (
          <div key={k} className="flex flex-col gap-1">
            <div className="font-mono text-xs text-muted-foreground">{k}</div>
            <DiffView patch={v} />
          </div>
        ))}
      </Section>

      {/* Curves and the params/metrics table, the same view N runs get. The
          A/B endpoint only ever returned final values, so this page had no
          charts at all. */}
      <CompareMany ids={ids} />

      <Section title={t("Metric deltas ({n})", { n: metrics.length })}>
        {metrics.length === 0 ? (
          <Empty>{t("No metrics to compare.")}</Empty>
        ) : (
          <Card>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{t("Key")}</TableHead>
                  <TableHead className="text-right">A</TableHead>
                  <TableHead className="text-right">B</TableHead>
                  <TableHead className="text-right">Δ (B − A)</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {metrics.map((m) => (
                  <TableRow key={m.key}>
                    <TableCell className="font-mono text-xs">{m.key}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatNumber(m.a)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatNumber(m.b)}</TableCell>
                    <TableCell
                      className={cn(
                        "text-right tabular-nums",
                        typeof m.delta === "number" && m.delta > 0 && "text-emerald-700 dark:text-emerald-400",
                        typeof m.delta === "number" && m.delta < 0 && "text-red-700 dark:text-red-400",
                      )}
                    >
                      {typeof m.delta === "number" ? `${m.delta > 0 ? "+" : ""}${formatNumber(m.delta)}` : "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        )}
      </Section>

      <Section title={t("Environment differences ({n})", { n: env.length })}>
        {env.length === 0 ? (
          <Empty>{t("No environment differences.")}</Empty>
        ) : (
          <Card>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>{t("Path")}</TableHead>
                  <TableHead>A</TableHead>
                  <TableHead>B</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {env.map((e) => (
                  <TableRow key={e.path}>
                    <TableCell className="font-mono text-xs text-muted-foreground">{e.path}</TableCell>
                    <TableCell className="max-w-md break-all font-mono text-xs">{valueText(e.a)}</TableCell>
                    <TableCell className="max-w-md break-all font-mono text-xs">{valueText(e.b)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        )}
      </Section>
    </div>
  );
}

export default function ComparePage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <CompareView />
    </Suspense>
  );
}
