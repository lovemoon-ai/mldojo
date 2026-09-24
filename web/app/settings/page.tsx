"use client";

import * as React from "react";
import { Suspense } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { CheckCircle2, Eye, EyeOff, XCircle } from "lucide-react";
import { API_BASE, getToken, setToken } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { Health } from "@/lib/types";
import { ErrorBox, KV, Loading, Mono, PageFallback, PageHeader } from "@/components/common";
import { SecretsPanel } from "@/components/secrets-panel";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

function Bool({ ok, yes, no }: { ok: boolean; yes: string; no: string }) {
  return ok ? (
    <span className="inline-flex items-center gap-1 text-emerald-700 dark:text-emerald-400">
      <CheckCircle2 className="size-3.5" /> {yes}
    </span>
  ) : (
    <span className="inline-flex items-center gap-1 text-red-700 dark:text-red-400">
      <XCircle className="size-3.5" /> {no}
    </span>
  );
}

function TokenCard() {
  const [value, setValue] = React.useState("");
  const [show, setShow] = React.useState(false);
  const [saved, setSaved] = React.useState(false);
  const { t } = useT();
  React.useEffect(() => setValue(getToken()), []);
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("API token")}</CardTitle>
        <CardDescription>{t("Stored in this browser only (localStorage). Sent as a Bearer token.")}</CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="flex flex-col gap-2 sm:flex-row"
          onSubmit={(e) => {
            e.preventDefault();
            setToken(value.trim());
            setSaved(true);
            setTimeout(() => setSaved(false), 2000);
          }}
        >
          <div className="relative flex-1">
            <Input
              type={show ? "text" : "password"}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder="MLDOJO_TOKEN"
              autoComplete="off"
              className="pr-9 font-mono text-xs"
              aria-label={t("API token")}
            />
            <button
              type="button"
              aria-label={show ? t("Hide token") : t("Show token")}
              onClick={() => setShow((s) => !s)}
              className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
            >
              {show ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
            </button>
          </div>
          <div className="flex gap-2">
            <Button type="submit">{saved ? t("Saved") : t("Save")}</Button>
            <Button
              variant="outline"
              onClick={() => {
                setToken("");
                setValue("");
              }}
            >
              {t("Clear")}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

function HealthCard() {
  const { data, error, loading, reload } = useApi<Health>("/health", { interval: 15000 });
  const [origin, setOrigin] = React.useState(API_BASE);
  const { t } = useT();
  const plugins = Object.entries(data?.queue_plugins ?? {});
  React.useEffect(() => setOrigin(API_BASE || window.location.origin), []);
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("API health")}</CardTitle>
        <CardDescription>
          {t("Endpoint:")} <Mono>{origin}/api/v1</Mono>
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-2">
        <ErrorBox error={error} onRetry={reload} />
        {loading ? (
          <Loading />
        ) : data ? (
          <KV
            items={[
              [t("Status"), <Bool key="ok" ok={data.ok} yes={t("healthy")} no={t("unhealthy")} />],
              [t("Version"), data.version || "—"],
              [t("Database"), data.db || "—"],
              [t("Secrets"), data.secrets || "—"],
              [t("Agents online"), data.agents_online],
              [
                t("Queue plugins"),
                plugins.length ? (
                  <span key="qp" className="flex flex-wrap gap-x-3">
                    {plugins.map(([name, ok]) => (
                      <Bool key={name} ok={ok} yes={`${name}: ${t("ready")}`} no={`${name}: ${t("unavailable")}`} />
                    ))}
                  </span>
                ) : (
                  t("none configured")
                ),
              ],
            ]}
          />
        ) : null}
      </CardContent>
    </Card>
  );
}

function SettingsView() {
  const router = useRouter();
  const { t } = useT();
  const tab = useSearchParams().get("tab") === "secrets" ? "secrets" : "general";
  return (
    <div className="flex flex-col gap-4">
      <PageHeader title={t("Settings")} />
      <Tabs value={tab} onValueChange={(v) => router.replace(v === "general" ? "/settings" : `/settings?tab=${v}`, { scroll: false })}>
        <TabsList className="self-start">
          <TabsTrigger value="general">{t("General")}</TabsTrigger>
          <TabsTrigger value="secrets">{t("Secrets")}</TabsTrigger>
        </TabsList>
        <TabsContent value="general" className="flex max-w-3xl flex-col gap-4">
          <TokenCard />
          <HealthCard />
        </TabsContent>
        <TabsContent value="secrets">
          <SecretsPanel />
        </TabsContent>
      </Tabs>
    </div>
  );
}

export default function SettingsPage() {
  return (
    <Suspense fallback={<PageFallback />}>
      <SettingsView />
    </Suspense>
  );
}
