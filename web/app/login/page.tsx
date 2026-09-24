"use client";

import * as React from "react";
import { Suspense } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { Loader2, LogIn } from "lucide-react";
import { api, getAuthConfig, getAuthStatus, getToken, setToken, ssoLoginUrl } from "@/lib/api";
import { useT } from "@/lib/i18n";
import type { AuthConfig } from "@/lib/types";
import { ErrorBox, PageFallback } from "@/components/common";
import { Logo } from "@/components/app-shell";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";

function safeNext(next: string | null): string {
  // Only allow same-app relative paths.
  if (!next || !next.startsWith("/") || next.startsWith("//") || next.startsWith("/login")) return "/";
  return next;
}

function TokenForm({ next, secondary }: { next: string; secondary?: boolean }) {
  const router = useRouter();
  const { t } = useT();
  const [token, setValue] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<unknown>();

  React.useEffect(() => setValue(getToken()), []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const prev = getToken();
    setBusy(true);
    setError(undefined);
    setToken(token.trim());
    try {
      // Any authenticated endpoint validates the token.
      await api.get("/projects", undefined, { noAuthRedirect: true });
      router.replace(next);
    } catch (err) {
      setToken(prev);
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      <Input
        type="password"
        autoFocus
        autoComplete="current-password"
        placeholder="token"
        value={token}
        onChange={(e) => setValue(e.target.value)}
        className="font-mono text-xs"
        aria-label={t("API token")}
      />
      <ErrorBox error={error} />
      <Button type="submit" variant={secondary ? "secondary" : "default"} disabled={busy || !token.trim()}>
        {busy && <Loader2 className="animate-spin" />}
        {t("Sign in")}
      </Button>
    </form>
  );
}

function LoginForm() {
  const sp = useSearchParams();
  const router = useRouter();
  const { t } = useT();
  const next = safeNext(sp.get("next"));
  // The server redirects back here with ?error=… when the exchange fails.
  const serverError = sp.get("error") || "";
  const [config, setConfig] = React.useState<AuthConfig>();
  const [checking, setChecking] = React.useState(true);
  const [showToken, setShowToken] = React.useState(false);

  React.useEffect(() => {
    let cancelled = false;
    void (async () => {
      // Both are unauthenticated; an unreachable API just falls back to the token form.
      const [cfg, status] = await Promise.all([
        getAuthConfig().catch(() => undefined),
        getAuthStatus().catch(() => undefined),
      ]);
      if (cancelled) return;
      if (status?.authenticated) {
        router.replace(next);
        return;
      }
      setConfig(cfg);
      setShowToken(!cfg?.sso_enabled);
      setChecking(false);
    })();
    return () => {
      cancelled = true;
    };
  }, [next, router]);

  if (checking) return <PageFallback />;

  const sso = !!config?.sso_enabled;
  return (
    <Card className="w-full max-w-sm">
      <CardHeader className="gap-2">
        <Logo className="text-lg" />
        <CardDescription>
          {sso ? t("Sign in with your Conductor account, or use an API token.") : t("Paste your MLDojo API token (MLDOJO_TOKEN).")}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <ErrorBox error={serverError} />
        {sso && (
          <Button onClick={() => (window.location.href = ssoLoginUrl(next))}>
            <LogIn /> {t("Sign in with Conductor")}
          </Button>
        )}
        {sso && config?.token_login && !showToken && (
          <button
            type="button"
            onClick={() => setShowToken(true)}
            className="cursor-pointer self-center text-xs text-muted-foreground hover:text-foreground hover:underline"
          >
            {t("Sign in with an API token")}
          </button>
        )}
        {showToken && <TokenForm next={next} secondary={sso} />}
      </CardContent>
    </Card>
  );
}

export default function LoginPage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <LoginForm />
    </Suspense>
  );
}
