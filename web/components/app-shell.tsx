"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  Activity,
  Database,
  FolderKanban,
  KeyRound,
  Languages,
  Layers,
  LayoutDashboard,
  Menu,
  Server,
  Settings,
  UserRound,
  X,
} from "lucide-react";
import { getAuthStatus, logout, setToken } from "@/lib/api";
import type { AuthStatus } from "@/lib/types";
import { LANG_NAMES, useT, type Msg } from "@/lib/i18n";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";

type NavItem = { href: string; label: Msg; icon: React.ComponentType<{ className?: string }>; match: string[] };

const NAV: NavItem[] = [
  { href: "/", label: "Dashboard", icon: LayoutDashboard, match: ["/"] },
  { href: "/projects", label: "Projects", icon: FolderKanban, match: ["/projects", "/project", "/experiment"] },
  { href: "/runs", label: "Runs", icon: Activity, match: ["/runs", "/run", "/compare"] },
  { href: "/nodes", label: "Nodes", icon: Server, match: ["/nodes", "/node"] },
  { href: "/queues", label: "Queues", icon: Layers, match: ["/queues"] },
  { href: "/datasets", label: "Datasets", icon: Database, match: ["/datasets"] },
  { href: "/settings", label: "Settings", icon: Settings, match: ["/settings"] },
];
const MOBILE_NAV = ["/projects", "/runs", "/nodes", "/settings"];

function normalize(p: string | null): string {
  if (!p) return "/";
  const s = p.replace(/\.html$/, "").replace(/\/+$/, "");
  return s || "/";
}

function isActive(item: NavItem, path: string) {
  return item.match.includes(path);
}

export function Logo({ className }: { className?: string }) {
  return (
    <span className={cn("flex items-center gap-2 font-semibold tracking-tight", className)}>
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img src="/icons/mark.svg" alt="" className="size-5.5 dark:hidden" />
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img src="/icons/mark-dark.svg" alt="" className="hidden size-5.5 dark:block" />
      MLDojo
    </span>
  );
}

/** Signed-in identity + sign-out; renders nothing until /auth/me answers. */
function AuthBox({ status, className }: { status: AuthStatus | undefined; className?: string }) {
  const [busy, setBusy] = React.useState(false);
  if (!status?.authenticated) return null;
  const sso = status.mode === "sso";
  const u = status.user;
  const { t } = useT();
  const name = u?.name || u?.email || u?.phone || u?.id || "Conductor";

  const signOut = async () => {
    setBusy(true);
    if (sso) await logout().catch(() => {});
    else setToken("");
    window.location.href = "/login";
  };

  return (
    <div className={cn("flex items-center gap-2", className)}>
      {sso ? (
        <>
          <UserRound className="size-4 shrink-0 text-muted-foreground" />
          <span className="min-w-0 flex-1 truncate text-[13px]" title={u?.email || u?.phone || name}>
            {name}
          </span>
        </>
      ) : (
        <Badge variant="muted" className="flex-1" title={t("Signed in with an API token")}>
          <KeyRound /> {t("API token")}
        </Badge>
      )}
      <button
        type="button"
        disabled={busy}
        onClick={signOut}
        className="shrink-0 cursor-pointer rounded-md px-1.5 py-1 text-[11px] font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-50"
      >
        {t("Sign out")}
      </button>
    </div>
  );
}

/** Toggles between English and Simplified Chinese. */
function LangSwitch({ className }: { className?: string }) {
  const { lang, setLang } = useT();
  return (
    <button
      type="button"
      onClick={() => setLang(lang === "zh" ? "en" : "zh")}
      className={cn(
        "flex cursor-pointer items-center gap-1.5 rounded-md px-1.5 py-1 text-[11px] font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground",
        className,
      )}
    >
      <Languages className="size-3.5" />
      {LANG_NAMES[lang === "zh" ? "en" : "zh"]}
    </button>
  );
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const path = normalize(usePathname());
  const { t } = useT();
  const [menuOpen, setMenuOpen] = React.useState(false);
  const [auth, setAuth] = React.useState<AuthStatus>();

  React.useEffect(() => setMenuOpen(false), [path]);

  // Once per session (the login page asks for itself).
  const onLogin = path === "/login";
  React.useEffect(() => {
    if (onLogin) return;
    getAuthStatus()
      .then(setAuth)
      .catch(() => {});
  }, [onLogin]);

  React.useEffect(() => {
    if (process.env.NODE_ENV !== "production") return;
    if (!("serviceWorker" in navigator)) return;
    navigator.serviceWorker.register("/sw.js").catch(() => {});
  }, []);

  if (onLogin) {
    return (
      <main className="flex min-h-dvh flex-col items-center justify-center gap-3 p-4">
        {children}
        <LangSwitch />
      </main>
    );
  }

  return (
    <div className="flex min-h-dvh">
      {/* Desktop sidebar */}
      <aside className="sticky top-0 hidden h-dvh w-52 shrink-0 flex-col border-r border-border bg-sidebar md:flex">
        <Link href="/" className="flex h-12 items-center px-4">
          <Logo className="text-[15px]" />
        </Link>
        <nav className="flex flex-1 flex-col gap-0.5 px-2 py-2">
          {NAV.map((item) => {
            const active = isActive(item, path);
            const Icon = item.icon;
            return (
              <Link
                key={item.href}
                href={item.href}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "flex h-8 items-center gap-2.5 rounded-md px-2.5 text-[13px] font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground",
                  active && "bg-accent text-foreground",
                )}
              >
                <Icon className="size-4" />
                {t(item.label)}
              </Link>
            );
          })}
        </nav>
        <div className="flex flex-col gap-1 border-t border-border px-3 py-2">
          <AuthBox status={auth} />
          <LangSwitch className="self-start" />
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        {/* Mobile top bar */}
        <header className="sticky top-0 z-30 flex h-12 items-center justify-between border-b border-border bg-background/90 px-4 backdrop-blur md:hidden">
          <Link href="/">
            <Logo className="text-[15px]" />
          </Link>
          <button
            type="button"
            aria-label={t("Menu")}
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen((o) => !o)}
            className="rounded-md p-1.5 text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            {menuOpen ? <X className="size-5" /> : <Menu className="size-5" />}
          </button>
          {menuOpen && (
            <nav className="absolute inset-x-0 top-12 border-b border-border bg-background px-2 py-2 shadow-lg">
              {NAV.map((item) => {
                const Icon = item.icon;
                return (
                  <Link
                    key={item.href}
                    href={item.href}
                    className={cn(
                      "flex h-10 items-center gap-3 rounded-md px-3 text-sm text-muted-foreground hover:bg-accent",
                      isActive(item, path) && "bg-accent text-foreground",
                    )}
                  >
                    <Icon className="size-4" />
                    {t(item.label)}
                  </Link>
                );
              })}
              <AuthBox status={auth} className="mt-1 border-t border-border px-3 pt-2.5" />
              <LangSwitch className="mt-1 ml-1.5" />
            </nav>
          )}
        </header>

        <main className="mx-auto w-full max-w-[1400px] flex-1 px-4 pt-4 pb-24 md:px-6 md:pt-6 md:pb-10">{children}</main>

        {/* Mobile bottom nav */}
        <nav className="pb-safe fixed inset-x-0 bottom-0 z-30 border-t border-border bg-background/95 backdrop-blur md:hidden">
          <div className="grid grid-cols-4">
            {NAV.filter((n) => MOBILE_NAV.includes(n.href)).map((item) => {
              const active = isActive(item, path);
              const Icon = item.icon;
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "flex h-14 flex-col items-center justify-center gap-0.5 text-[11px] font-medium text-muted-foreground",
                    active && "text-foreground",
                  )}
                >
                  <Icon className="size-5" />
                  {t(item.label)}
                </Link>
              );
            })}
          </div>
        </nav>
      </div>
    </div>
  );
}
