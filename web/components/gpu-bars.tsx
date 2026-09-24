"use client";

import * as React from "react";
import { Thermometer } from "lucide-react";
import { openFrameSocket, seg } from "@/lib/api";
import { useT } from "@/lib/i18n";
import type { GPUStat } from "@/lib/types";
import { cn } from "@/lib/utils";

/** Live GPU stats: seeded from the node, then `gpu` frames from /nodes/{id}/gpu/ws. */
export function useLiveGpu(nodeId: string, initial: GPUStat[] | undefined, enabled: boolean) {
  const [stats, setStats] = React.useState<GPUStat[] | undefined>(initial);
  const [live, setLive] = React.useState(false);

  React.useEffect(() => {
    if (initial) setStats(initial);
  }, [initial]);

  React.useEffect(() => {
    if (!enabled || !nodeId) return;
    const sock = openFrameSocket(`/nodes/${seg(nodeId)}/gpu/ws`, {
      onFrame: (f) => {
        if (f.kind === "gpu" && Array.isArray(f.payload)) setStats(f.payload as unknown as GPUStat[]);
      },
      onState: (s) => setLive(s === "open"),
    });
    return () => sock.close();
  }, [nodeId, enabled]);

  return { stats, live };
}

function Meter({ value, label, warn = 101 }: { value: number; label: string; warn?: number }) {
  const pct = Math.max(0, Math.min(100, Number.isFinite(value) ? value : 0));
  const hot = pct >= warn;
  return (
    <div className="flex min-w-0 flex-1 items-center" title={label}>
      <div
        className={cn("h-1.5 min-w-0 flex-1 overflow-hidden rounded-full", hot ? "bg-amber-500/20" : "bg-[color-mix(in_srgb,var(--chart-1)_18%,transparent)]")}
        role="meter"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(pct)}
        aria-label={label}
      >
        <div
          className={cn("h-full rounded-full transition-[width] duration-500", hot && "bg-amber-500")}
          style={{ width: `${pct}%`, background: hot ? undefined : "var(--chart-1)" }}
        />
      </div>
    </div>
  );
}

export function GpuBars({ stats, compact }: { stats: GPUStat[] | undefined; compact?: boolean }) {
  const { t } = useT();
  if (!stats || stats.length === 0) {
    return <div className="text-xs text-muted-foreground">{t("No GPU telemetry.")}</div>;
  }
  return (
    <div className={cn("grid gap-x-3 text-xs", compact ? "gap-y-1.5" : "gap-y-2.5")}>
      {stats.map((g) => {
        const memPct = g.mem_total_mb > 0 ? (g.mem_used_mb / g.mem_total_mb) * 100 : 0;
        const tempCls = g.temp >= 85 ? "text-red-600 dark:text-red-400" : g.temp >= 75 ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground";
        return (
          <div key={g.index} className="grid grid-cols-[2.25rem_1fr_1fr_3rem] items-center gap-x-2 sm:grid-cols-[2.25rem_1fr_1fr_3.25rem]">
            <span className="font-mono text-muted-foreground" title={g.model}>
              #{g.index}
            </span>
            <div className="flex min-w-0 flex-col gap-0.5">
              {!compact && <span className="text-[10px] uppercase tracking-wide text-muted-foreground">{t("util")}</span>}
              <div className="flex items-center gap-1.5">
                <Meter value={g.util} label={t("GPU {i} utilization {pct}%", { i: g.index, pct: g.util.toFixed(0) })} />
                <span className="w-8 text-right tabular-nums">{g.util.toFixed(0)}%</span>
              </div>
            </div>
            <div className="flex min-w-0 flex-col gap-0.5">
              {!compact && <span className="text-[10px] uppercase tracking-wide text-muted-foreground">{t("mem")}</span>}
              <div className="flex items-center gap-1.5">
                <Meter value={memPct} warn={95} label={t("GPU {i} memory {pct}%", { i: g.index, pct: memPct.toFixed(0) })} />
                <span className="hidden w-[4.5rem] text-right tabular-nums sm:inline">
                  {(g.mem_used_mb / 1024).toFixed(1)}/{(g.mem_total_mb / 1024).toFixed(0)}G
                </span>
              </div>
            </div>
            <span className={cn("flex items-center justify-end gap-0.5 tabular-nums", tempCls)}>
              <Thermometer className="size-3" />
              {g.temp.toFixed(0)}°
            </span>
          </div>
        );
      })}
    </div>
  );
}
