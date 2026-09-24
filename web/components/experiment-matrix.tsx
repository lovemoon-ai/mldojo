"use client";

import * as React from "react";
import Link from "next/link";
import { seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { ExpSummary, JsonValue } from "@/lib/types";
import { cn, shortId } from "@/lib/utils";
import { Empty, ErrorBox, Loading } from "@/components/common";
import { Card } from "@/components/ui/card";
import { Select } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

/**
 * The unit of iteration is a matrix, not a run.
 *
 * `--matrix setting,chunk` produces four runs and the runs list shows four
 * rows; what the work is actually about is the 2x2:
 *
 *                  chunk 16     chunk 50
 *   clean            70%          80%
 *   randomized        0%          25%
 *
 * A pivot over the parameters that vary says that in one look, and says
 * which cells are empty -- the run that never got submitted is invisible in
 * a list and obvious in a grid.
 */
export function ExperimentMatrix({ project, name }: { project: string; name: string }) {
  const sum = useApi<ExpSummary>(`/ai/experiments/${seg(project)}/${seg(name)}/summary`, { interval: 15000 });
  const runs = React.useMemo(() => sum.data?.ranking ?? [], [sum.data]);
  const { t } = useT();

  // The axes worth pivoting on are the parameters that actually differ.
  const axes = React.useMemo(() => {
    const vals = new Map<string, Set<string>>();
    for (const r of runs) {
      for (const [k, v] of Object.entries(r.params ?? {})) {
        if (!vals.has(k)) vals.set(k, new Set());
        vals.get(k)!.add(fmt(v));
      }
    }
    return [...vals.entries()].filter(([, s]) => s.size > 1).map(([k]) => k).sort();
  }, [runs]);

  const [row, setRow] = React.useState("");
  const [col, setCol] = React.useState("");
  React.useEffect(() => {
    // Default to the first two varying axes, but never fight the user.
    setRow((r) => (r && axes.includes(r) ? r : (axes[0] ?? "")));
    setCol((c) => (c && axes.includes(c) ? c : (axes[1] ?? "")));
  }, [axes]);

  if (sum.loading && !sum.data) return <Loading />;
  if (sum.error) return <ErrorBox error={sum.error} />;
  if (axes.length === 0) return <Empty>{t("Every run has the same parameters: there is no matrix to pivot.")}</Empty>;
  if (!row) return <Loading />; // the axes effect has not run yet

  const rowVals = distinct(runs, row);
  const colVals = col ? distinct(runs, col) : [""];
  const metric = sum.data?.metric ?? "";
  const lower = sum.data?.lower_is_better ?? true;
  const best = bestValue(runs, lower);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <span>{t("rows")}</span>
        <Select value={row} onChange={(e) => setRow(e.target.value)} className="w-40">
          {axes.map((a) => (
            <option key={a} value={a}>{a}</option>
          ))}
        </Select>
        <span>{t("columns")}</span>
        <Select value={col} onChange={(e) => setCol(e.target.value)} className="w-40">
          <option value="">{t("(none)")}</option>
          {axes.filter((a) => a !== row).map((a) => (
            <option key={a} value={a}>{a}</option>
          ))}
        </Select>
        <span className="ml-auto font-mono">{metric || t("no metric")} {metric && (lower ? t("(lower is better)") : t("(higher is better)"))}</span>
      </div>
      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{row}</TableHead>
              {colVals.map((c) => (
                <TableHead key={c} className="text-right">{col ? `${col}=${c}` : metric}</TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {rowVals.map((rv) => (
              <TableRow key={rv}>
                <TableCell className="font-medium">{rv}</TableCell>
                {colVals.map((cv) => {
                  const cell = runs.filter((r) => fmt(r.params?.[row]) === rv && (!col || fmt(r.params?.[col]) === cv));
                  return (
                    <TableCell key={cv} className="text-right tabular-nums">
                      {cell.length === 0 ? (
                        <span className="text-muted-foreground">-</span>
                      ) : (
                        cell.map((r) => (
                          <Link
                            key={r.id}
                            href={`/run?id=${encodeURIComponent(r.id)}`}
                            className={cn("ml-2 hover:underline", r.value === best && "font-semibold text-foreground")}
                            title={`${shortId(r.id)} ${r.name} (${r.status})`}
                          >
                            {r.value == null ? <span className="text-muted-foreground">{r.status}</span> : num(r.value)}
                          </Link>
                        ))
                      )}
                    </TableCell>
                  );
                })}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Card>
    </div>
  );
}

type Score = NonNullable<ExpSummary["ranking"]>[number];

function distinct(runs: Score[], key: string): string[] {
  const s = new Set(runs.map((r) => fmt(r.params?.[key])));
  return [...s].sort(byNumberThenText);
}

function bestValue(runs: Score[], lower: boolean): number | undefined {
  const vals = runs.map((r) => r.value).filter((v): v is number => v != null);
  if (vals.length === 0) return undefined;
  return lower ? Math.min(...vals) : Math.max(...vals);
}

function byNumberThenText(a: string, b: string) {
  const x = Number(a);
  const y = Number(b);
  if (!Number.isNaN(x) && !Number.isNaN(y)) return x - y;
  return a.localeCompare(b);
}

function fmt(v: JsonValue | undefined): string {
  return v == null ? "" : String(v);
}

function num(v: number): string {
  return Math.abs(v) >= 0.001 && Math.abs(v) < 1e6 ? String(Number(v.toFixed(4))) : v.toExponential(2);
}
