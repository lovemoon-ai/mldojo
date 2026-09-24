"use client";

import * as React from "react";
import Link from "next/link";
import { CartesianGrid, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { CompareManyResponse } from "@/lib/types";
import { compactValue, formatNumber, runLabel, shortId } from "@/lib/utils";
import { Empty, ErrorBox, Loading, Section } from "@/components/common";
import { StatusBadge, TargetBadge } from "@/components/status";
import { Card } from "@/components/ui/card";
import { Select } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

// Distinguishable at a glance in both themes, and stable per run index so a
// run keeps its colour across every chart on the page.
const COLORS = [
  "#2563eb", "#dc2626", "#16a34a", "#d97706", "#7c3aed",
  "#0891b2", "#db2777", "#65a30d", "#ea580c", "#4f46e5",
];

/** Rows for recharts: one entry per step, one field per run. */
function chartData(res: CompareManyResponse, key: string) {
  const steps = new Set<number>();
  for (const r of res.runs) for (const p of r.series?.[key] ?? []) steps.add(p.step);
  const ordered = [...steps].sort((a, b) => a - b);
  const byRun = new Map<string, Map<number, number>>();
  for (const r of res.runs) {
    byRun.set(r.id, new Map((r.series?.[key] ?? []).map((p) => [p.step, p.value])));
  }
  return ordered.map((step) => {
    const row: Record<string, number> = { step };
    for (const r of res.runs) {
      const v = byRun.get(r.id)?.get(step);
      if (v !== undefined && Number.isFinite(v)) row[r.id] = v;
    }
    return row;
  });
}

export function CompareMany({ ids }: { ids: string[] }) {
  const [metric, setMetric] = React.useState<string>("");
  const path = ids.length ? `/compare/many?runs=${ids.map(encodeURIComponent).join(",")}&max_points=500` : null;
  const res = useApi<CompareManyResponse>(path);
  const { t } = useT();

  if (res.loading) return <Loading />;
  if (res.error) return <ErrorBox error={res.error} onRetry={res.reload} />;
  const data = res.data;
  if (!data || data.runs.length === 0) return <Empty>{t("No runs to compare.")}</Empty>;

  const shortName = (r: CompareManyResponse["runs"][number]) =>
    r.name || runLabel({ ...r, metadata: {} } as never) || shortId(r.id);
  const charts = metric ? [metric] : data.metric_keys;

  return (
    <div className="flex flex-col gap-4">
      <Section title={t("Leaderboard ({n} runs)", { n: data.runs.length })}>
        <p className="mb-2 text-xs text-muted-foreground">
          {t("Hyperparameters against the last value of every metric.")}
        </p>
        <Card className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("Run")}</TableHead>
                <TableHead>{t("Status")}</TableHead>
                {data.param_keys.map((k) => (
                  <TableHead key={`p-${k}`} className="font-mono text-xs">
                    {k}
                  </TableHead>
                ))}
                {data.metric_keys.map((k) => (
                  <TableHead key={`m-${k}`} className="font-mono text-xs">
                    {k}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.runs.map((r, i) => (
                <TableRow key={r.id}>
                  <TableCell>
                    <span className="flex items-center gap-2">
                      <span
                        className="inline-block size-2.5 shrink-0 rounded-full"
                        style={{ backgroundColor: COLORS[i % COLORS.length] }}
                        aria-hidden
                      />
                      <Link href={`/run?id=${encodeURIComponent(r.id)}`} className="font-medium hover:underline">
                        {shortName(r)}
                      </Link>
                      <span className="font-mono text-[11px] text-muted-foreground">{shortId(r.id)}</span>
                    </span>
                  </TableCell>
                  <TableCell>
                    <span className="flex items-center gap-1.5">
                      <StatusBadge status={r.status} />
                      <TargetBadge target={r.target} />
                    </span>
                  </TableCell>
                  {data.param_keys.map((k) => (
                    <TableCell key={`p-${k}`} className="font-mono text-xs">
                      {r.params?.[k] === undefined ? "—" : compactValue(r.params[k])}
                    </TableCell>
                  ))}
                  {data.metric_keys.map((k) => (
                    <TableCell key={`m-${k}`} className="font-mono text-xs">
                      {r.latest?.[k] === undefined ? "—" : formatNumber(r.latest[k])}
                    </TableCell>
                  ))}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      </Section>

      <Section
        title={t("Metrics")}
        actions={
          data.metric_keys.length > 1 ? (
            <Select value={metric} onChange={(e) => setMetric(e.target.value)} aria-label={t("Metric")} className="w-48">
              <option value="">{t("All metrics")}</option>
              {data.metric_keys.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </Select>
          ) : undefined
        }
      >
        {data.sampled && (
          <p className="mb-2 text-xs text-muted-foreground">{t("Curves are sampled to keep the charts responsive.")}</p>
        )}
        {charts.length === 0 ? (
          <Empty>{t("These runs reported no metrics.")}</Empty>
        ) : (
          <div className="grid gap-3 lg:grid-cols-2">
            {charts.map((key) => {
              const rows = chartData(data, key);
              if (rows.length === 0) return null;
              return (
                <Card key={key} className="p-3">
                  <div className="mb-2 font-mono text-xs text-muted-foreground">{key}</div>
                  <div className="h-56">
                    <ResponsiveContainer width="100%" height="100%">
                      <LineChart data={rows} margin={{ top: 4, right: 8, bottom: 4, left: 0 }}>
                        <CartesianGrid strokeDasharray="3 3" className="stroke-border" />
                        <XAxis dataKey="step" tick={{ fontSize: 11 }} stroke="currentColor" />
                        <YAxis tick={{ fontSize: 11 }} width={56} stroke="currentColor" tickFormatter={formatNumber} />
                        <Tooltip
                          formatter={(v, name) => [
                            formatNumber(Number(v)),
                            shortName(data.runs.find((r) => r.id === String(name)) ?? data.runs[0]),
                          ]}
                          labelFormatter={(s) => t("step {n}", { n: String(s) })}
                          contentStyle={{ fontSize: 12 }}
                        />
                        {data.runs.length > 1 && (
                          <Legend
                            formatter={(name) =>
                              shortName(data.runs.find((r) => r.id === String(name)) ?? data.runs[0])
                            }
                            wrapperStyle={{ fontSize: 11 }}
                          />
                        )}
                        {data.runs.map((r, i) => (
                          <Line
                            key={r.id}
                            type="monotone"
                            dataKey={r.id}
                            stroke={COLORS[i % COLORS.length]}
                            dot={false}
                            strokeWidth={1.75}
                            isAnimationActive={false}
                            connectNulls
                          />
                        ))}
                      </LineChart>
                    </ResponsiveContainer>
                  </div>
                </Card>
              );
            })}
          </div>
        )}
      </Section>
    </div>
  );
}
