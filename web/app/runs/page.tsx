"use client";

import * as React from "react";
import { Suspense } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { RefreshCw, X } from "lucide-react";
import { useApi } from "@/lib/hooks";
import { useT, type Msg } from "@/lib/i18n";
import { PHASES, type Node, type Project, type Queue, type Run } from "@/lib/types";
import { ErrorBox, Loading, PageFallback, PageHeader } from "@/components/common";
import { RunsTable } from "@/components/runs-table";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";

const FILTERS = ["status", "project", "experiment", "target", "tag", "q"] as const;
const PAGE = 50;
const SORTS: { value: string; label: Msg }[] = [
  { value: "", label: "Newest first" },
  { value: "created:asc", label: "Oldest first" },
  { value: "started:desc", label: "Recently started" },
  { value: "duration:desc", label: "Longest running" },
  { value: "duration:asc", label: "Shortest running" },
  { value: "name:asc", label: "Name A–Z" },
  { value: "status:asc", label: "Status" },
  { value: "target:asc", label: "Target" },
];

function RunsView() {
  const sp = useSearchParams();
  const router = useRouter();
  const { t } = useT();
  const status = sp.get("status") || "";
  const project = sp.get("project") || "";
  const experiment = sp.get("experiment") || "";
  const target = sp.get("target") || "";
  const tag = sp.get("tag") || "";
  const search = sp.get("q") || "";
  const sort = sp.get("sort") || "";
  const page = Math.max(0, Number(sp.get("page") || 0));

  const q = new URLSearchParams();
  for (const k of FILTERS) {
    const v = sp.get(k);
    if (v) q.set(k, v);
  }
  if (sort) {
    const [by, order] = sort.split(":");
    q.set("sort", by);
    if (order) q.set("order", order);
  }
  q.set("limit", String(PAGE));
  if (page > 0) q.set("offset", String(page * PAGE));
  const runs = useApi<Run[]>(`/runs?${q.toString()}`, { interval: 10000 });
  const projects = useApi<Project[]>("/projects");
  const nodes = useApi<Node[]>("/nodes");
  const queues = useApi<Queue[]>("/queues");

  const targets = React.useMemo(
    () => [...(nodes.data ?? []).map((n) => `node:${n.id}`), ...(queues.data ?? []).map((x) => `queue:${x.id}`)],
    [nodes.data, queues.data],
  );

  const setFilter = (key: string, value: string) => {
    const next = new URLSearchParams(sp.toString());
    if (value) next.set(key, value);
    else next.delete(key);
    // Any change to what is being listed invalidates the current page.
    if (key !== "page") next.delete("page");
    const s = next.toString();
    router.replace(s ? `/runs?${s}` : "/runs", { scroll: false });
  };

  const [targetText, setTargetText] = React.useState(target);
  React.useEffect(() => setTargetText(target), [target]);
  const [searchText, setSearchText] = React.useState(search);
  React.useEffect(() => setSearchText(search), [search]);

  const active = FILTERS.some((k) => sp.get(k));
  const shown = runs.data?.length ?? 0;
  const hasNext = shown === PAGE;
  const first = shown === 0 ? 0 : page * PAGE + 1;

  return (
    <div>
      <PageHeader
        title={t("Runs")}
        actions={
          <Button size="sm" variant="outline" onClick={() => runs.reload()}>
            <RefreshCw /> {t("Refresh")}
          </Button>
        }
      />
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Select value={status} onChange={(e) => setFilter("status", e.target.value)} aria-label={t("Status")} className="w-36">
          <option value="">{t("All statuses")}</option>
          {PHASES.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </Select>
        <Select value={project} onChange={(e) => setFilter("project", e.target.value)} aria-label={t("Project")} className="w-44">
          <option value="">{t("All projects")}</option>
          {project && !(projects.data ?? []).some((p) => p.name === project) && <option value={project}>{project}</option>}
          {(projects.data ?? []).map((p) => (
            <option key={p.name} value={p.name}>
              {p.name}
            </option>
          ))}
        </Select>
        <form
          className="flex"
          onSubmit={(e) => {
            e.preventDefault();
            setFilter("target", targetText.trim());
          }}
        >
          <Input
            list="run-targets"
            value={targetText}
            onChange={(e) => setTargetText(e.target.value)}
            onBlur={() => targetText.trim() !== target && setFilter("target", targetText.trim())}
            placeholder={t("Target (e.g. {example})", { example: "node:gpu-1" })}
            aria-label={t("Target")}
            className="w-56 font-mono text-xs"
          />
          <datalist id="run-targets">
            {targets.map((x) => (
              <option key={x} value={x} />
            ))}
          </datalist>
        </form>
        <form
          className="flex"
          onSubmit={(e) => {
            e.preventDefault();
            setFilter("q", searchText.trim());
          }}
        >
          <Input
            value={searchText}
            onChange={(e) => setSearchText(e.target.value)}
            onBlur={() => searchText.trim() !== search && setFilter("q", searchText.trim())}
            placeholder={t("Search names")}
            aria-label={t("Search run or experiment name")}
            className="w-44"
          />
        </form>
        <Select value={sort} onChange={(e) => setFilter("sort", e.target.value)} aria-label={t("Sort")} className="w-44">
          {SORTS.map((s) => (
            <option key={s.value} value={s.value}>
              {t(s.label)}
            </option>
          ))}
        </Select>
        {tag && (
          <span className="inline-flex h-8 items-center gap-1 rounded-md border border-border px-2 text-xs">
            {t("tag:")} {tag}
            <button aria-label={t("Clear tag filter")} onClick={() => setFilter("tag", "")}>
              <X className="size-3" />
            </button>
          </span>
        )}
        {experiment && (
          <span className="inline-flex h-8 items-center gap-1 rounded-md border border-border px-2 text-xs">
            {t("experiment:")} {experiment}
            <button aria-label={t("Clear experiment filter")} onClick={() => setFilter("experiment", "")}>
              <X className="size-3" />
            </button>
          </span>
        )}
        {active && (
          <Button size="sm" variant="ghost" onClick={() => router.replace("/runs", { scroll: false })}>
            {t("Clear filters")}
          </Button>
        )}
      </div>
      <ErrorBox error={runs.error} onRetry={runs.reload} className="mb-3" />
      {runs.loading ? (
        <Loading />
      ) : (
        <Card>
          <RunsTable runs={runs.data ?? []} empty={active ? t("No runs match these filters.") : t("No runs yet.")} />
        </Card>
      )}
      {(page > 0 || hasNext) && (
        <div className="mt-3 flex items-center gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={page === 0}
            onClick={() => setFilter("page", String(page - 1))}
          >
            {t("Previous")}
          </Button>
          <Button size="sm" variant="outline" disabled={!hasNext} onClick={() => setFilter("page", String(page + 1))}>
            {t("Next")}
          </Button>
          <span className="text-xs text-muted-foreground">
            {shown === 0 ? t("No runs on this page") : t("Showing {from}–{to}", { from: first, to: first + shown - 1 })}
          </span>
        </div>
      )}
    </div>
  );
}

export default function RunsPage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <RunsView />
    </Suspense>
  );
}
