"use client";

import * as React from "react";
import Link from "next/link";
import { AlertTriangle, ChevronRight, Loader2 } from "lucide-react";
import { errorMessage } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { cn, prettyJson } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Dialog, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";

export function PageHeader({
  title,
  description,
  actions,
  crumbs,
  children,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  crumbs?: { label: string; href?: string }[];
  children?: React.ReactNode;
}) {
  return (
    <div className="mb-4 flex flex-col gap-2">
      {crumbs && crumbs.length > 0 && <Breadcrumbs items={crumbs} />}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h1 className="truncate text-lg font-semibold tracking-tight">{title}</h1>
          {description && <div className="mt-0.5 text-sm text-muted-foreground">{description}</div>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children}
    </div>
  );
}

export function Breadcrumbs({ items }: { items: { label: string; href?: string }[] }) {
  const { t } = useT();
  return (
    <nav aria-label={t("Breadcrumb")} className="flex min-w-0 flex-wrap items-center gap-1 text-xs text-muted-foreground">
      {items.map((it, i) => (
        <React.Fragment key={i}>
          {i > 0 && <ChevronRight className="size-3 shrink-0" />}
          {it.href ? (
            <Link href={it.href} className="truncate hover:text-foreground hover:underline">
              {it.label}
            </Link>
          ) : (
            <span className="truncate text-foreground">{it.label}</span>
          )}
        </React.Fragment>
      ))}
    </nav>
  );
}

export function ErrorBox({ error, className, onRetry }: { error: unknown; className?: string; onRetry?: () => void }) {
  const { t } = useT();
  if (!error) return null;
  return (
    <div
      role="alert"
      className={cn(
        "flex items-start gap-2 rounded-md border border-red-600/25 bg-red-500/10 px-3 py-2 text-sm text-red-700 dark:text-red-400",
        className,
      )}
    >
      <AlertTriangle className="mt-0.5 size-4 shrink-0" />
      <div className="min-w-0 flex-1 break-words">{errorMessage(error)}</div>
      {onRetry && (
        <Button size="xs" variant="outline" onClick={onRetry}>
          {t("Retry")}
        </Button>
      )}
    </div>
  );
}

export function Loading({ label, className }: { label?: string; className?: string }) {
  const { t } = useT();
  return (
    <div className={cn("flex items-center gap-2 py-6 text-sm text-muted-foreground", className)}>
      <Loader2 className="size-4 animate-spin" />
      {label ?? t("Loading…")}
    </div>
  );
}

export function Empty({ children, className }: { children: React.ReactNode; className?: string }) {
  return (
    <div className={cn("rounded-md border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground", className)}>
      {children}
    </div>
  );
}

/** Suspense fallback for pages that read search params. */
export function PageFallback() {
  return <Loading />;
}

export function JsonBlock({ value, className, maxHeight = "24rem" }: { value: unknown; className?: string; maxHeight?: string }) {
  const text = value === undefined || value === null ? "null" : prettyJson(value);
  return (
    <pre
      className={cn("overflow-auto rounded-md border border-border bg-code p-3 font-mono text-xs leading-relaxed", className)}
      style={{ maxHeight }}
    >
      {text}
    </pre>
  );
}

/** Dense key/value list. */
export function KV({ items, className }: { items: [React.ReactNode, React.ReactNode][]; className?: string }) {
  return (
    <dl className={cn("grid grid-cols-[minmax(6rem,max-content)_1fr] gap-x-4 gap-y-1.5 text-sm", className)}>
      {items.map(([k, v], i) => (
        <React.Fragment key={i}>
          <dt className="text-muted-foreground">{k}</dt>
          <dd className="min-w-0 break-words">{v ?? "—"}</dd>
        </React.Fragment>
      ))}
    </dl>
  );
}

export function Mono({ children, className }: { children: React.ReactNode; className?: string }) {
  return <span className={cn("font-mono text-[12px]", className)}>{children}</span>;
}

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  destructive,
  onConfirm,
  children,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  title: string;
  description?: React.ReactNode;
  confirmLabel?: string;
  destructive?: boolean;
  onConfirm: () => Promise<void> | void;
  children?: React.ReactNode;
}) {
  const { t } = useT();
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<unknown>();
  React.useEffect(() => {
    if (open) setError(undefined);
  }, [open]);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogHeader>
        <DialogTitle>{title}</DialogTitle>
        {description && <DialogDescription>{description}</DialogDescription>}
      </DialogHeader>
      {children}
      <ErrorBox error={error} />
      <DialogFooter>
        <Button variant="outline" onClick={() => onOpenChange(false)} disabled={busy}>
          {t("Cancel")}
        </Button>
        <Button
          variant={destructive ? "destructive" : "default"}
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setError(undefined);
            try {
              await onConfirm();
              onOpenChange(false);
            } catch (e) {
              setError(e);
            } finally {
              setBusy(false);
            }
          }}
        >
          {busy && <Loader2 className="animate-spin" />}
          {confirmLabel ?? t("Confirm")}
        </Button>
      </DialogFooter>
    </Dialog>
  );
}

export function Section({
  title,
  actions,
  children,
  className,
}: {
  title: React.ReactNode;
  actions?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <section className={cn("flex min-w-0 flex-col gap-2", className)}>
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">{title}</h2>
        {actions}
      </div>
      {children}
    </section>
  );
}

export function CliHint({ title, command }: { title?: string; command: string }) {
  return (
    <div className="rounded-md border border-border bg-code px-3 py-2">
      {title && <div className="mb-1 text-xs text-muted-foreground">{title}</div>}
      <pre className="overflow-x-auto font-mono text-xs leading-relaxed">{command}</pre>
    </div>
  );
}
