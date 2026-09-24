"use client";

import * as React from "react";
import { Download, Eye, EyeOff, Loader2, RefreshCw } from "lucide-react";
import { api, rawUrl, seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Artifact, Run } from "@/lib/types";
import { basename, extOf, formatBytes, relativeTime, formatTime } from "@/lib/utils";
import { Empty, ErrorBox, Loading } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

const VIDEO_EXT = ["mp4", "webm", "m4v", "mov"];
const IMAGE_EXT = ["png", "jpg", "jpeg", "gif", "webp", "svg", "bmp", "avif"];

function previewKind(a: Artifact): "video" | "image" | null {
  const ext = extOf(a.uri);
  if (a.kind === "video" || VIDEO_EXT.includes(ext)) return "video";
  if (a.kind === "image" || IMAGE_EXT.includes(ext)) return "image";
  return null;
}

function Preview({ runId, a }: { runId: string; a: Artifact }) {
  const src = rawUrl(`/runs/${seg(runId)}/artifacts/raw`, { uri: a.uri });
  const { t } = useT();
  if (previewKind(a) === "video") {
    return (
      <video controls preload="metadata" playsInline src={src} className="max-h-[60vh] w-full rounded-md bg-black">
        {t("Your browser cannot play this video.")}
      </video>
    );
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img src={src} alt={basename(a.uri)} className="max-h-[60vh] max-w-full rounded-md border border-border object-contain" />
  );
}

export function ArtifactsTab({ run }: { run: Run }) {
  const { data, error, loading, setData } = useApi<Artifact[]>(`/runs/${seg(run.id)}/artifacts`);
  const [open, setOpen] = React.useState<Set<string>>(new Set());
  const [refreshing, setRefreshing] = React.useState(false);
  const [refreshError, setRefreshError] = React.useState<unknown>();
  const { t } = useT();

  const refresh = async () => {
    setRefreshing(true);
    setRefreshError(undefined);
    try {
      setData(await api.post<Artifact[]>(`/runs/${seg(run.id)}/artifacts/refresh`));
    } catch (e) {
      setRefreshError(e);
    } finally {
      setRefreshing(false);
    }
  };

  const toggle = (id: string) =>
    setOpen((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

  const artifacts = data ?? [];
  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <Button size="sm" variant="outline" onClick={refresh} disabled={refreshing}>
          {refreshing ? <Loader2 className="animate-spin" /> : <RefreshCw />}
          {t("Rescan")}
        </Button>
        <span className="text-xs text-muted-foreground">
          {t(artifacts.length === 1 ? "{n} artifact" : "{n} artifacts", { n: artifacts.length })} · {t("asks the agent to rescan outputs")}
        </span>
      </div>
      <ErrorBox error={refreshError || error} />
      {loading ? (
        <Loading />
      ) : artifacts.length === 0 ? (
        <Empty>{t("No artifacts recorded. Outputs are matched from the recipe's `outputs` globs.")}</Empty>
      ) : (
        <Card>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>{t("Name")}</TableHead>
                <TableHead>{t("Kind")}</TableHead>
                <TableHead className="text-right">{t("Size")}</TableHead>
                <TableHead className="hidden md:table-cell">{t("Created")}</TableHead>
                <TableHead className="text-right">{t("Actions")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {artifacts.map((a) => {
                const pk = previewKind(a);
                const isOpen = open.has(a.id || a.uri);
                return (
                  <React.Fragment key={a.id || a.uri}>
                    <TableRow>
                      <TableCell className="max-w-[28rem]">
                        <div className="truncate font-medium" title={a.uri}>
                          {basename(a.uri) || a.uri}
                        </div>
                        <div className="truncate font-mono text-[11px] text-muted-foreground" title={a.uri}>
                          {a.uri}
                        </div>
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline">{a.kind || "other"}</Badge>
                      </TableCell>
                      <TableCell className="whitespace-nowrap text-right tabular-nums">{formatBytes(a.size_bytes)}</TableCell>
                      <TableCell className="hidden whitespace-nowrap text-muted-foreground md:table-cell" title={formatTime(a.created_at)}>
                        {relativeTime(a.created_at)}
                      </TableCell>
                      <TableCell>
                        <div className="flex justify-end gap-1">
                          {pk && (
                            <Button size="xs" variant="ghost" onClick={() => toggle(a.id || a.uri)} aria-expanded={isOpen}>
                              {isOpen ? <EyeOff /> : <Eye />}
                              <span className="hidden sm:inline">{isOpen ? t("Hide") : t("Preview")}</span>
                            </Button>
                          )}
                          <a
                            href={rawUrl(`/runs/${seg(run.id)}/artifacts/raw`, { uri: a.uri })}
                            download={basename(a.uri)}
                            className={buttonVariants({ size: "xs", variant: "ghost" })}
                          >
                            <Download />
                            <span className="hidden sm:inline">{t("Download")}</span>
                          </a>
                        </div>
                      </TableCell>
                    </TableRow>
                    {pk && isOpen && (
                      <TableRow className="hover:bg-transparent">
                        <TableCell colSpan={5} className="bg-muted/30">
                          <Preview runId={run.id} a={a} />
                        </TableCell>
                      </TableRow>
                    )}
                  </React.Fragment>
                );
              })}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  );
}
