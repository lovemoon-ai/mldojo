import { CheckCircle2, Circle, CircleDashed, Loader2, MinusCircle, WifiOff, XCircle, AlertTriangle, Radio } from "lucide-react";
import type { ComponentType } from "react";
import { Badge, type BadgeVariant } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

const PHASE: Record<string, { variant: BadgeVariant; icon: ComponentType<{ className?: string }> }> = {
  queued: { variant: "muted", icon: CircleDashed },
  starting: { variant: "warning", icon: Loader2 },
  running: { variant: "info", icon: Loader2 },
  succeeded: { variant: "success", icon: CheckCircle2 },
  failed: { variant: "danger", icon: XCircle },
  cancelled: { variant: "secondary", icon: MinusCircle },
};

/** Run phase badge: status color + icon + label (never color alone). */
export function StatusBadge({ status, className }: { status: string; className?: string }) {
  const p = PHASE[status] ?? { variant: "outline" as BadgeVariant, icon: Circle };
  const Icon = p.icon;
  const spinning = status === "running" || status === "starting";
  return (
    <Badge variant={p.variant} className={className}>
      <Icon className={cn(spinning && "animate-spin [animation-duration:2s]")} />
      {status || "unknown"}
    </Badge>
  );
}

const AGENT: Record<string, { variant: BadgeVariant; icon: ComponentType<{ className?: string }> }> = {
  online: { variant: "success", icon: Radio },
  degraded: { variant: "warning", icon: AlertTriangle },
  offline: { variant: "muted", icon: WifiOff },
};

export function AgentBadge({ status, className }: { status: string; className?: string }) {
  const a = AGENT[status] ?? { variant: "outline" as BadgeVariant, icon: Circle };
  const Icon = a.icon;
  return (
    <Badge variant={a.variant} className={className}>
      <Icon />
      {status || "unknown"}
    </Badge>
  );
}

export function TargetBadge({ target, className }: { target: string; className?: string }) {
  const isQueue = target.startsWith("queue:");
  return (
    <Badge variant={isQueue ? "violet" : "outline"} className={cn("max-w-52 font-mono font-normal", className)} title={target}>
      <span className="truncate">{target || "—"}</span>
    </Badge>
  );
}
