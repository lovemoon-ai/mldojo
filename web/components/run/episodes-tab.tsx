"use client";

import * as React from "react";
import { rawUrl, seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { EpisodesResponse, Run } from "@/lib/types";
import { isTerminal } from "@/lib/types";
import { cn } from "@/lib/utils";
import { Empty, ErrorBox, Loading } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";

type Filter = "all" | "failure" | "success";

/** Episodes are numbered, so a grid in index order is the natural reading
 *  order: you scan for the failures, and they are the ones you open. */
export function EpisodesTab({ run }: { run: Run }) {
  // An evaluation usually writes its manifest at the end, so the count jumps
  // from nothing to everything; keep polling until the run is over.
  const { data, error, loading } = useApi<EpisodesResponse>(`/runs/${seg(run.id)}/episodes`, {
    interval: isTerminal(run.status) ? undefined : 10_000,
  });
  const [filter, setFilter] = React.useState<Filter>("all");
  const [open, setOpen] = React.useState<number | null>(null);
  const { t } = useT();

  if (loading && !data) return <Loading />;
  if (error) return <ErrorBox error={error} />;
  const eps = data?.episodes ?? [];
  if (eps.length === 0) {
    return (
      <Empty>
        {t("No episodes recorded. An evaluation reports them by pointing")}{" "}
        <code className="font-mono text-xs">outputs.episodes</code>{" "}
        {t("at the manifest or video directory its harness already writes.")}
      </Empty>
    );
  }

  const s = data!.summary;
  const shown = eps.filter((e) => filter === "all" || (filter === "success") === e.success);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-baseline gap-2">
          <span className="text-2xl font-semibold tabular-nums">
            {s.successes}/{s.total}
          </span>
          <span className="text-sm text-muted-foreground">{t("{pct}% success", { pct: (s.success_rate * 100).toFixed(0) })}</span>
        </div>
        <div className="flex gap-1 rounded-md border border-border p-0.5 text-xs">
          {(["all", "failure", "success"] as Filter[]).map((f) => (
            <button
              key={f}
              onClick={() => setFilter(f)}
              className={cn(
                "rounded px-2 py-1 transition-colors",
                filter === f ? "bg-accent font-medium" : "text-muted-foreground hover:text-foreground",
              )}
            >
              {f === "all"
                ? t("All {n}", { n: s.total })
                : f === "failure"
                  ? t("Failed {n}", { n: s.total - s.successes })
                  : t("Passed {n}", { n: s.successes })}
            </button>
          ))}
        </div>
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
        {shown.map((e) => {
          const src = e.video_uri
            ? rawUrl(`/runs/${seg(run.id)}/artifacts/raw`, { uri: e.video_uri })
            : undefined;
          const isOpen = open === e.index;
          return (
            <Card key={e.index} className="flex flex-col gap-2 p-2">
              <div className="flex items-center justify-between gap-2 text-xs">
                <span className="font-mono font-medium">#{e.index}</span>
                <Badge variant={e.success ? "success" : "danger"}>{e.success ? t("pass") : t("fail")}</Badge>
              </div>
              {src ? (
                // Only the opened video loads: twenty autoplaying rollouts
                // would pull twenty files nobody asked for.
                isOpen ? (
                  <video
                    controls
                    autoPlay
                    loop
                    playsInline
                    preload="metadata"
                    src={src}
                    className="aspect-video w-full rounded bg-black"
                  />
                ) : (
                  <button
                    onClick={() => setOpen(e.index)}
                    className="flex aspect-video w-full items-center justify-center rounded bg-muted text-xs text-muted-foreground transition-colors hover:bg-accent"
                  >
                    {t("play")}
                  </button>
                )
              ) : (
                <div className="flex aspect-video w-full items-center justify-center rounded bg-muted text-xs text-muted-foreground">
                  {t("no video")}
                </div>
              )}
              <dl className="flex flex-wrap gap-x-3 gap-y-0.5 text-[11px] text-muted-foreground">
                {e.seed !== undefined && (
                  <div className="flex gap-1">
                    <dt>{t("seed")}</dt>
                    <dd className="font-mono tabular-nums text-foreground">{e.seed}</dd>
                  </div>
                )}
                {!!e.steps && (
                  <div className="flex gap-1">
                    <dt>{t("steps")}</dt>
                    <dd className="tabular-nums text-foreground">{e.steps}</dd>
                  </div>
                )}
                {!!e.duration_ms && (
                  <div className="flex gap-1">
                    <dt>{t("time")}</dt>
                    <dd className="tabular-nums text-foreground">{(e.duration_ms / 1000).toFixed(1)}s</dd>
                  </div>
                )}
              </dl>
            </Card>
          );
        })}
      </div>
      {shown.length === 0 && <Empty>{t("No episodes match this filter.")}</Empty>}
    </div>
  );
}
