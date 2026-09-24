"use client";

import * as React from "react";
import { Suspense } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { GitCompare } from "lucide-react";
import { seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Experiment, Run } from "@/lib/types";
import { cn, formatTime, shortId } from "@/lib/utils";
import { Empty, ErrorBox, Loading, PageFallback, PageHeader } from "@/components/common";
import { ExperimentMatrix } from "@/components/experiment-matrix";
import { RunsTable } from "@/components/runs-table";
import { StatusBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { buttonVariants } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

/** Minimal YAML highlighting: comments, keys, list dashes. */
// Matches core.MaxCompareRuns on the server.
const MAX_COMPARE = 20;

function YamlView({ text }: { text: string }) {
  const lines = text.replace(/\n$/, "").split("\n");
  return (
    <pre className="max-h-[70vh] overflow-auto rounded-md border border-border bg-code py-3 font-mono text-xs leading-5">
      {lines.map((line, i) => {
        const m = line.match(/^(\s*(?:-\s+)?)([\w.\-/"']+)(:)(\s.*|$)/);
        const comment = /^\s*#/.test(line);
        return (
          <div key={i} className="flex">
            <span className="w-10 shrink-0 select-none pr-3 text-right text-muted-foreground/60">{i + 1}</span>
            <span className="whitespace-pre pr-3">
              {comment ? (
                <span className="text-muted-foreground italic">{line}</span>
              ) : m ? (
                <>
                  {m[1]}
                  <span className="text-blue-700 dark:text-sky-400">{m[2]}</span>
                  {m[3]}
                  <span>{m[4]}</span>
                </>
              ) : (
                line || " "
              )}
            </span>
          </div>
        );
      })}
    </pre>
  );
}

function ExperimentView() {
  const sp = useSearchParams();
  const project = sp.get("project") || "";
  const name = sp.get("name") || "";
  const preselect = sp.get("compare");
  const { t } = useT();

  const exp = useApi<Experiment>(project && name ? `/projects/${seg(project)}/experiments/${seg(name)}` : null);
const runsPath =
    project && name
      ? `/runs?project=${encodeURIComponent(project)}&experiment=${encodeURIComponent(name)}&limit=500`
      : null;
  const runs = useApi<Run[]>(runsPath, { interval: 10000 });

  const [selected, setSelected] = React.useState<string[]>([]);
  React.useEffect(() => {
    if (preselect) setSelected([preselect]);
  }, [preselect]);
  // Comparison used to be capped at two runs, which is also why the compare
  // page had no charts -- two points make a table, not a curve.
  const toggle = (id: string) =>
    setSelected((s) => (s.includes(id) ? s.filter((x) => x !== id) : [...s, id].slice(-MAX_COMPARE)));

  if (!project || !name) return <Empty>{t("Missing project or experiment name.")}</Empty>;
  if (exp.loading) return <Loading />;

  const e = exp.data;
  const list = runs.data ?? [];
  const counts = e?.run_counts ?? {};
  return (
    <div>
      <PageHeader
        crumbs={[
          { label: t("Projects"), href: "/projects" },
          { label: project, href: `/project?name=${encodeURIComponent(project)}` },
          { label: name },
        ]}
        title={name}
        description={e?.description || undefined}
        actions={
          <Link
            href={selected.length >= 2 ? `/compare?runs=${selected.map(encodeURIComponent).join(",")}` : "#"}
            aria-disabled={selected.length < 2}
            className={cn(
              buttonVariants({ size: "sm", variant: selected.length >= 2 ? "default" : "outline" }),
              selected.length < 2 && "pointer-events-none opacity-50",
            )}
          >
            <GitCompare /> {t("Compare")} {selected.length > 0 ? selected.length : ""}
          </Link>
        }
      >
        <div className="flex flex-wrap items-center gap-1.5">
          {Object.entries(counts).map(([status, n]) => (
            <span key={status} className="inline-flex items-center gap-1">
              <StatusBadge status={status} />
              <span className="text-xs tabular-nums text-muted-foreground">{n}</span>
            </span>
          ))}
          {(e?.tags ?? []).map((tag) => (
            <Badge key={tag} variant="outline">
              {tag}
            </Badge>
          ))}
          {e && <span className="text-xs text-muted-foreground">{t("created {time}", { time: formatTime(e.created_at) })}</span>}
        </div>
      </PageHeader>
      <ErrorBox error={exp.error} className="mb-3" />

      <Tabs defaultValue="runs">
        <TabsList className="self-start">
          <TabsTrigger value="runs">{t("Runs ({n})", { n: list.length })}</TabsTrigger>
          <TabsTrigger value="matrix">{t("Matrix")}</TabsTrigger>
          <TabsTrigger value="recipe">{t("Recipe")}</TabsTrigger>
        </TabsList>
        <TabsContent value="runs" className="flex flex-col gap-2">
          <p className="text-xs text-muted-foreground">
            {t("Select two or more runs to compare.")}
            {selected.length > 0 && (
              <>
                {" "}
                {t("Selected:")} <span className="font-mono">{selected.map((s) => shortId(s)).join(", ")}</span>{" "}
                <button className="underline hover:text-foreground" onClick={() => setSelected([])}>
                  {t("clear")}
                </button>
              </>
            )}
          </p>
          <ErrorBox error={runs.error} />
          {runs.loading ? (
            <Loading />
          ) : (
            <Card>
              <RunsTable runs={list} showProject={false} selectable selected={selected} onToggle={toggle} empty={t("No runs in this experiment.")} />
            </Card>
          )}
        </TabsContent>
        <TabsContent value="matrix">
          <ExperimentMatrix project={project} name={name} />
        </TabsContent>
        <TabsContent value="recipe">
          {e?.recipe_yaml ? <YamlView text={e.recipe_yaml} /> : <Empty>{t("No recipe stored.")}</Empty>}
        </TabsContent>
      </Tabs>
    </div>
  );
}

export default function ExperimentPage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <ExperimentView />
    </Suspense>
  );
}
