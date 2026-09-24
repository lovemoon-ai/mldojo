"use client";

import * as React from "react";
import { Button } from "@/components/ui/button";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";

const MAX_LINES = 4000;

function lineClass(l: string): string {
  if (l.startsWith("diff --git") || l.startsWith("index ") || l.startsWith("new file") || l.startsWith("deleted file"))
    return "font-semibold text-foreground bg-muted";
  if (l.startsWith("+++") || l.startsWith("---")) return "font-semibold text-muted-foreground";
  if (l.startsWith("@@")) return "text-blue-700 bg-blue-500/10 dark:text-blue-400";
  if (l.startsWith("+")) return "text-emerald-800 bg-emerald-500/10 dark:text-emerald-300";
  if (l.startsWith("-")) return "text-red-800 bg-red-500/10 dark:text-red-300";
  return "text-foreground/80";
}

/** Unified diff rendered with per-line coloring. */
export function DiffView({ patch, className }: { patch: string; className?: string }) {
  const [all, setAll] = React.useState(false);
  const { t } = useT();
  const lines = React.useMemo(() => patch.replace(/\n$/, "").split("\n"), [patch]);
  const shown = all ? lines : lines.slice(0, MAX_LINES);
  return (
    <div className={cn("overflow-hidden rounded-md border border-border", className)}>
      <pre className="max-h-[70vh] overflow-auto bg-code py-2 font-mono text-xs leading-5">
        {shown.map((l, i) => (
          <div key={i} className={cn("min-w-max px-3 whitespace-pre", lineClass(l))}>
            {l || " "}
          </div>
        ))}
      </pre>
      {lines.length > MAX_LINES && !all && (
        <div className="flex items-center justify-between border-t border-border px-3 py-2 text-xs text-muted-foreground">
          {t("Showing {n} of {total} lines", { n: MAX_LINES.toLocaleString(), total: lines.length.toLocaleString() })}
          <Button size="xs" variant="outline" onClick={() => setAll(true)}>
            {t("Show all")}
          </Button>
        </div>
      )}
    </div>
  );
}

export function looksLikeDiff(s: string): boolean {
  return /^(diff --git|--- |\+\+\+ |@@ )/m.test(s);
}
