"use client";

import * as React from "react";
import { Suspense } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { ExternalLink, GitCompare, RotateCcw, Square, Timer, UserRound } from "lucide-react";
import { api, seg } from "@/lib/api";
import { useApi, useNow } from "@/lib/hooks";
import { useT, type Msg } from "@/lib/i18n";
import { isTerminal, type Run } from "@/lib/types";
import { formatDuration, formatTime, runDurationMs, runLabel, shortId } from "@/lib/utils";
import { ConfirmDialog, Empty, ErrorBox, Loading, PageFallback, PageHeader } from "@/components/common";
import { StatusBadge, TargetBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { LogsTab } from "@/components/run/logs-tab";
import { MetricsTab } from "@/components/run/metrics-tab";
import { CodeTab } from "@/components/run/code-tab";
import { ArtifactsTab } from "@/components/run/artifacts-tab";
import { EventsTab } from "@/components/run/events-tab";
import { EpisodesTab } from "@/components/run/episodes-tab";
import { OverviewTab } from "@/components/run/overview-tab";
import Link from "next/link";

const TABS = ["logs", "metrics", "episodes", "code", "artifacts", "events", "overview"] as const;
const TAB_LABELS: Record<(typeof TABS)[number], Msg> = {
  logs: "Logs",
  metrics: "Metrics",
  episodes: "Episodes",
  code: "Code",
  artifacts: "Artifacts",
  events: "Events",
  overview: "Overview",
};

function Stat({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="text-[11px] uppercase tracking-wide text-muted-foreground">{label}</div>
      <div className="truncate text-sm tabular-nums">{children}</div>
    </div>
  );
}

function RunView() {
  const sp = useSearchParams();
  const router = useRouter();
  const { t } = useT();
  const id = sp.get("id") || "";
  const tabParam = sp.get("tab") || "logs";
  const tab = (TABS as readonly string[]).includes(tabParam) ? tabParam : "logs";

  const [terminal, setTerminal] = React.useState(false);
  const { data: run, error, loading, setData } = useApi<Run>(id ? `/runs/${seg(id)}` : null, {
    interval: 5000,
    enabled: !terminal,
  });
  React.useEffect(() => setTerminal(isTerminal(run?.status)), [run?.status]);

  const now = useNow(1000, !!run && !terminal && !!run.started_at);
  const [confirmCancel, setConfirmCancel] = React.useState(false);
  const [confirmRerun, setConfirmRerun] = React.useState(false);

  const setTab = (t: string) => {
    const q = new URLSearchParams(sp.toString());
    q.set("tab", t);
    router.replace(`/run?${q.toString()}`, { scroll: false });
  };

  const onStatus = React.useCallback(
    (r: Run) => {
      if (r && r.id) setData(r);
    },
    [setData],
  );

  if (!id) return <Empty>{t("No run id given. Open a run from the runs list.")}</Empty>;
  if (loading) return <Loading label={t("Loading run…")} />;
  if (!run) return <ErrorBox error={error ?? t("Run not found")} />;

  const m = run.metadata ?? {};
  return (
    <div>
      <PageHeader
        crumbs={[
          { label: t("Projects"), href: "/projects" },
          { label: run.project, href: `/project?name=${encodeURIComponent(run.project)}` },
          {
            label: run.experiment,
            href: `/experiment?project=${encodeURIComponent(run.project)}&name=${encodeURIComponent(run.experiment)}`,
          },
          { label: runLabel(run) },
        ]}
        title={
          <span className="flex items-baseline gap-2">
            {runLabel(run)}
            <span className="font-mono text-xs font-normal text-muted-foreground">{shortId(run.id, 12)}</span>
          </span>
        }
        actions={
          <>
            <Link
              href={`/experiment?project=${encodeURIComponent(run.project)}&name=${encodeURIComponent(run.experiment)}&compare=${encodeURIComponent(run.id)}`}
              className={buttonVariants({ size: "sm", variant: "outline" })}
            >
              <GitCompare /> {t("Compare")}
            </Link>
            <Button size="sm" variant="outline" onClick={() => setConfirmRerun(true)}>
              <RotateCcw /> {t("Rerun")}
            </Button>
            {!isTerminal(run.status) && (
              <Button size="sm" variant="destructive" onClick={() => setConfirmCancel(true)}>
                <Square /> {t("Cancel")}
              </Button>
            )}
          </>
        }
      >
        <div className="flex flex-wrap items-center gap-1.5">
          <StatusBadge status={run.status} />
          <TargetBadge target={run.target} />
          {m.submitter && (
            <Badge variant="muted" title={t("Submitter")}>
              <UserRound /> {m.submitter}
            </Badge>
          )}
          {m.log_mode === "near-realtime" && (
            <Badge variant="warning" title={t("Logs are polled from the queue backend and diffed; expect 5–10s delay.")}>
              <Timer /> {t("near-realtime logs (5–10s)")}
            </Badge>
          )}
          {m.code_dirty && <Badge variant="warning">{t("dirty worktree")}</Badge>}
          {m.external_url && (
            <a
              href={m.external_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground hover:underline"
            >
              <ExternalLink className="size-3" /> {t("backend")}
            </a>
          )}
        </div>
        <div className="mt-1 grid grid-cols-2 gap-3 rounded-lg border border-border bg-card px-4 py-3 sm:grid-cols-4">
          <Stat label={t("Started")}>{formatTime(run.started_at)}</Stat>
          <Stat label={isTerminal(run.status) ? t("Duration") : t("Elapsed")}>{formatDuration(runDurationMs(run, now))}</Stat>
          <Stat label={t("Exit code")}>
            {run.exit_code === null || run.exit_code === undefined ? (
              "—"
            ) : (
              <span className={run.exit_code === 0 ? "" : "font-medium text-red-600 dark:text-red-400"}>{run.exit_code}</span>
            )}
          </Stat>
          <Stat label={t("Created")}>{formatTime(run.created_at)}</Stat>
        </div>
        {error ? <ErrorBox error={error} /> : null}
      </PageHeader>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList className="self-start">
          {TABS.map((x) => (
            <TabsTrigger key={x} value={x}>
              {t(TAB_LABELS[x])}
            </TabsTrigger>
          ))}
        </TabsList>
        <TabsContent value="logs">
          <LogsTab run={run} onStatus={onStatus} />
        </TabsContent>
        <TabsContent value="metrics">
          <MetricsTab run={run} />
        </TabsContent>
        <TabsContent value="episodes">
          <EpisodesTab run={run} />
        </TabsContent>
        <TabsContent value="code">
          <CodeTab run={run} />
        </TabsContent>
        <TabsContent value="artifacts">
          <ArtifactsTab run={run} />
        </TabsContent>
        <TabsContent value="events">
          <EventsTab run={run} />
        </TabsContent>
        <TabsContent value="overview">
          <OverviewTab run={run} />
        </TabsContent>
      </Tabs>

      <ConfirmDialog
        open={confirmRerun}
        onOpenChange={setConfirmRerun}
        title={t("Rerun this run?")}
        description={t("Submits a new run on {target} with the same code, environment and parameters.", { target: run.target })}
        confirmLabel={t("Rerun")}
        onConfirm={async () => {
          const res = await api.post<{ runs?: Run[] }>(`/runs/${seg(run.id)}/rerun`, {});
          const next = res?.runs?.[0];
          if (next?.id) router.push(`/run?id=${encodeURIComponent(next.id)}`);
        }}
      />

      <ConfirmDialog
        open={confirmCancel}
        onOpenChange={setConfirmCancel}
        title={t("Cancel run?")}
        description={t("Stops {run} ({id}) on {target}.", { run: runLabel(run), id: shortId(run.id), target: run.target })}
        confirmLabel={t("Cancel run")}
        destructive
        onConfirm={async () => {
          const r = await api.post<Run>(`/runs/${seg(run.id)}/cancel`);
          if (r?.id) setData(r);
        }}
      />
    </div>
  );
}

export default function RunPage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <RunView />
    </Suspense>
  );
}
