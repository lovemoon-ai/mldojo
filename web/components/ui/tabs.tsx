"use client";

import * as React from "react";
import { cn } from "@/lib/utils";

interface TabsCtx {
  value: string;
  setValue: (v: string) => void;
  id: string;
}
const Ctx = React.createContext<TabsCtx | null>(null);
const useTabs = () => {
  const c = React.useContext(Ctx);
  if (!c) throw new Error("Tabs components must be used inside <Tabs>");
  return c;
};

export interface TabsProps extends Omit<React.HTMLAttributes<HTMLDivElement>, "onChange"> {
  value?: string;
  defaultValue?: string;
  onValueChange?: (v: string) => void;
}

export function Tabs({ value, defaultValue, onValueChange, className, ...props }: TabsProps) {
  const [inner, setInner] = React.useState(defaultValue ?? "");
  const id = React.useId();
  const current = value ?? inner;
  const setValue = React.useCallback(
    (v: string) => {
      if (value === undefined) setInner(v);
      onValueChange?.(v);
    },
    [value, onValueChange],
  );
  return (
    <Ctx.Provider value={{ value: current, setValue, id }}>
      <div className={cn("flex flex-col gap-3", className)} {...props} />
    </Ctx.Provider>
  );
}

export function TabsList({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      role="tablist"
      className={cn(
        "inline-flex h-9 max-w-full items-center gap-0.5 overflow-x-auto rounded-lg bg-muted p-1 text-muted-foreground",
        className,
      )}
      {...props}
    />
  );
}

export function TabsTrigger({
  value,
  className,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { value: string }) {
  const t = useTabs();
  const active = t.value === value;
  return (
    <button
      type="button"
      role="tab"
      id={`${t.id}-t-${value}`}
      aria-selected={active}
      aria-controls={`${t.id}-p-${value}`}
      data-state={active ? "active" : "inactive"}
      onClick={() => t.setValue(value)}
      className={cn(
        "inline-flex h-7 cursor-pointer items-center justify-center gap-1.5 whitespace-nowrap rounded-md px-2.5 text-[13px] font-medium transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring/50 hover:text-foreground [&_svg]:size-3.5",
        active && "bg-card text-foreground shadow-sm",
        className,
      )}
      {...props}
    />
  );
}

export function TabsContent({
  value,
  className,
  ...props
}: React.HTMLAttributes<HTMLDivElement> & { value: string }) {
  const t = useTabs();
  if (t.value !== value) return null;
  return (
    <div
      role="tabpanel"
      id={`${t.id}-p-${value}`}
      aria-labelledby={`${t.id}-t-${value}`}
      className={cn("outline-none", className)}
      {...props}
    />
  );
}
