"use client";

import { seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import { isTerminal, type Run, type RunEvent } from "@/lib/types";
import { compactValue, formatTime, isEmptyJson, prettyJson } from "@/lib/utils";
import { Empty, ErrorBox, Loading } from "@/components/common";
import { Badge, type BadgeVariant } from "@/components/ui/badge";

function kindVariant(kind: string): BadgeVariant {
  const k = kind.toLowerCase();
  if (/(fail|error|cancel|lost|timeout)/.test(k)) return "danger";
  if (/(succe|finish|complete|done)/.test(k)) return "success";
  if (/(start|running|submit)/.test(k)) return "info";
  if (/(warn|retry|degrad)/.test(k)) return "warning";
  return "outline";
}

export function EventsTab({ run }: { run: Run }) {
  const { data, error, loading } = useApi<RunEvent[]>(`/runs/${seg(run.id)}/events`, {
    interval: 5000,
    enabled: !isTerminal(run.status),
  });
  const { t } = useT();
  if (loading) return <Loading />;
  const events = [...(data ?? [])].sort((a, b) => a.ts.localeCompare(b.ts) || a.id - b.id);
  return (
    <div className="flex flex-col gap-3">
      <ErrorBox error={error} />
      {events.length === 0 ? (
        <Empty>{t("No events.")}</Empty>
      ) : (
        <ol className="relative ml-2 border-l border-border">
          {events.map((ev) => {
            const payload = ev.payload;
            const text = isEmptyJson(payload) ? "" : typeof payload === "string" ? payload : prettyJson(payload);
            const short = text && text.length <= 120 && !text.includes("\n");
            return (
              <li key={ev.id} className="relative pb-4 pl-5 last:pb-0">
                <span className="absolute -left-[5px] top-1.5 size-2.5 rounded-full border-2 border-card bg-muted-foreground" />
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant={kindVariant(ev.kind)}>{ev.kind}</Badge>
                  <time className="text-xs tabular-nums text-muted-foreground" dateTime={ev.ts}>
                    {formatTime(ev.ts)}
                  </time>
                  {short && <span className="font-mono text-xs">{compactValue(payload)}</span>}
                </div>
                {text && !short && (
                  <pre className="mt-1.5 max-h-60 overflow-auto rounded-md border border-border bg-code p-2 font-mono text-xs">
                    {text}
                  </pre>
                )}
              </li>
            );
          })}
        </ol>
      )}
    </div>
  );
}
