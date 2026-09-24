"use client";

import type { Run } from "@/lib/types";
import { useT } from "@/lib/i18n";
import { compactValue, isEmptyJson } from "@/lib/utils";
import { JsonBlock, KV, Mono } from "@/components/common";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table";

function JsonCard({ title, value }: { title: string; value: unknown }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>
        {isEmptyJson(value) ? <div className="text-sm text-muted-foreground">—</div> : <JsonBlock value={value} maxHeight="20rem" />}
      </CardContent>
    </Card>
  );
}

export function OverviewTab({ run }: { run: Run }) {
  const m = run.metadata ?? {};
  const params = Object.entries(m.params ?? {});
  const { t } = useT();
  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <Card className="lg:col-span-2">
        <CardHeader>
          <CardTitle>{t("Command")}</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <pre className="overflow-x-auto rounded-md border border-border bg-code p-3 font-mono text-xs">{m.cmd || "—"}</pre>
          <KV
            items={[
              [t("Run ID"), <Mono key="id" className="select-all">{run.id}</Mono>],
              [t("Workdir"), m.workdir ? <Mono>{m.workdir}</Mono> : "—"],
              [t("Backend"), `${run.backend_kind || "—"} · ${run.backend_id || "—"}`],
              [t("Submitter"), m.submitter || "—"],
              [t("Tags"), m.tags?.length ? m.tags.join(", ") : "—"],
              ...(m.datasets?.length
                ? ([[t("Datasets"), m.datasets.map((d) => `${d.name}@${d.version} → ${d.mount}`).join("\n")]] as [string, string][])
                : []),
              // Lineage: what this run read, and what it registers when it
              // succeeds. An evaluation's number is worth little without the
              // first line.
              ...(m.models?.length
                ? ([[t("Models"), m.models.map((x) => `${x.name}@${x.version} → \${${x.as || "model"}} = ${x.uri}`).join("\n")]] as [string, string][])
                : []),
              ...(m.outputs?.model ? ([[t("Registers"), m.outputs.model]] as [string, string][]) : []),
              ...(m.notes ? ([[t("Notes"), m.notes]] as [string, string][]) : []),
              ...(m.message ? ([[t("Message"), m.message]] as [string, string][]) : []),
              ...(m.summary ? ([[t("Summary"), m.summary]] as [string, string][]) : []),
            ]}
            className="[&_dd]:whitespace-pre-line"
          />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("Params")}</CardTitle>
        </CardHeader>
        <CardContent>
          {params.length === 0 ? (
            <div className="text-sm text-muted-foreground">—</div>
          ) : (
            <Table>
              <TableBody>
                {params.map(([k, v]) => (
                  <TableRow key={k}>
                    <TableCell className="w-1/3 font-mono text-xs text-muted-foreground">{k}</TableCell>
                    <TableCell className="font-mono text-xs">{compactValue(v)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {(m.env_lock || m.image_digest) && (
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle>{t("Environment lock")}</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="mb-2 text-xs text-muted-foreground">
              {t(
                "What the environment actually resolved to at run time. The spec alone (an environment.yaml, or a mutable image tag) resolves differently later.",
              )}
            </p>
            {m.image_digest && (
              <div className="mb-2 break-all font-mono text-xs">
                <span className="text-muted-foreground">{t("image:")} </span>
                {m.image_digest}
              </div>
            )}
            {m.env_lock && (
              <pre className="max-h-72 overflow-auto rounded-md bg-muted p-2 font-mono text-[11px] leading-relaxed">
                {m.env_lock}
              </pre>
            )}
          </CardContent>
        </Card>
      )}

      <JsonCard title={t("Resources")} value={run.resources} />
      <JsonCard title={t("Env")} value={run.env} />
      <JsonCard title={t("Backend handle")} value={run.backend_handle} />
      <div className="lg:col-span-2">
        <JsonCard title={t("Metadata")} value={run.metadata} />
      </div>
    </div>
  );
}
