"use client";

import { seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Run, RunCode } from "@/lib/types";
import { DiffView } from "@/components/diff-view";
import { Empty, ErrorBox, KV, Loading, Mono } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export function CodeTab({ run }: { run: Run }) {
  const { data, error, loading } = useApi<RunCode>(`/runs/${seg(run.id)}/code`);
  const { t } = useT();
  if (loading) return <Loading label={t("Loading code info…")} />;

  // Fall back to what the run itself records if /code fails.
  const m = run.metadata ?? {};
  const code: Partial<RunCode> = data ?? {
    source: m.code_source,
    repo: m.code_repo,
    ref: m.code_ref,
    commit: run.code_commit,
    dirty: m.code_dirty,
    bundle_uri: m.code_bundle_uri,
  };

  return (
    <div className="flex flex-col gap-4">
      <ErrorBox error={error} />
      <Card>
        <CardHeader>
          <CardTitle>{t("Source")}</CardTitle>
        </CardHeader>
        <CardContent>
          <KV
            items={[
              [t("Source"), code.source || "—"],
              [t("Repo"), code.repo ? <Mono>{code.repo}</Mono> : "—"],
              [t("Ref"), code.ref ? <Mono>{code.ref}</Mono> : "—"],
              [t("Commit"), code.commit ? <Mono className="select-all">{code.commit}</Mono> : "—"],
              [
                t("Worktree"),
                code.dirty ? <Badge variant="warning">{t("dirty")}</Badge> : <Badge variant="success">{t("clean")}</Badge>,
              ],
              [t("Bundle"), code.bundle_uri ? <Mono>{code.bundle_uri}</Mono> : "—"],
              [t("Patch URI"), run.code_patch_uri ? <Mono>{run.code_patch_uri}</Mono> : "—"],
            ]}
          />
        </CardContent>
      </Card>
      <div className="flex flex-col gap-2">
        <h2 className="text-sm font-semibold">{t("Dirty patch")}</h2>
        {data?.patch ? (
          <DiffView patch={data.patch} />
        ) : (
          <Empty>{code.dirty ? t("Patch not available.") : t("Worktree was clean at submit time — no patch.")}</Empty>
        )}
      </div>
    </div>
  );
}
