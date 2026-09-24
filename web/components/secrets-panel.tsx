"use client";

import * as React from "react";
import { Eye, EyeOff, Loader2, Lock, LockOpen, Trash2 } from "lucide-react";
import { api, seg } from "@/lib/api";
import { useApi } from "@/lib/hooks";
import { useT } from "@/lib/i18n";
import type { OkResponse, SecretMeta, SecretSetRequest, SecretStatus } from "@/lib/types";
import { cn, formatBytes, formatTime, relativeTime } from "@/lib/utils";
import { ConfirmDialog, Empty, ErrorBox, Loading, Mono, Section } from "@/components/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";

const NAMESPACES = ["ssh_keys", "passwords", "buckets"];
const NAME_RE = /^[A-Za-z0-9._-]+$/;

function bytesToB64(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(bin);
}

function LockStatus({ status, onChange }: { status: SecretStatus | undefined; onChange: (s: SecretStatus) => void }) {
  const [key, setKey] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<unknown>();
  const { t } = useT();
  if (!status) return null;
  const unlock = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      onChange(await api.post<SecretStatus>("/secrets/unlock", key ? { key } : {}));
      setKey("");
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card className={cn("px-4 py-3", status.unlocked ? "" : "border-amber-500/40")}>
      <div className="flex flex-wrap items-center gap-3">
        {status.unlocked ? (
          <Badge variant="success">
            <LockOpen /> {t("unlocked")}
          </Badge>
        ) : (
          <Badge variant="warning">
            <Lock /> {t("locked")}
          </Badge>
        )}
        <span className="text-xs text-muted-foreground">
          {status.source && <>{t("source:")} {status.source}</>}
          {status.recipient && (
            <>
              {status.source && " · "}{t("recipient")} <Mono>{status.recipient}</Mono>
            </>
          )}
        </span>
      </div>
      {!status.unlocked && (
        <form onSubmit={unlock} className="mt-3 flex flex-wrap items-center gap-2">
          <Input
            type="password"
            autoComplete="off"
            placeholder={t("age identity (optional; empty = server keychain/env/file)")}
            value={key}
            onChange={(e) => setKey(e.target.value)}
            className="max-w-md font-mono text-xs"
          />
          <Button type="submit" size="sm" disabled={busy}>
            {busy ? <Loader2 className="animate-spin" /> : <LockOpen />}
            {t("Unlock")}
          </Button>
          <ErrorBox error={error} className="w-full" />
        </form>
      )}
    </Card>
  );
}

function SetSecretForm({ onSaved }: { onSaved: () => void }) {
  const [ns, setNs] = React.useState("");
  const [name, setName] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [mode, setMode] = React.useState<"text" | "file">("text");
  const [text, setText] = React.useState("");
  const [file, setFile] = React.useState<File | null>(null);
  const [masked, setMasked] = React.useState(true);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<unknown>();
  const [saved, setSaved] = React.useState("");
  const fileRef = React.useRef<HTMLInputElement>(null);
  const { t } = useT();

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(undefined);
    setSaved("");
    if (!NAME_RE.test(ns) || !NAME_RE.test(name)) {
      setError(new Error(t("Namespace and name may only contain letters, digits, '.', '_' and '-'.")));
      return;
    }
    let bytes: Uint8Array;
    if (mode === "file") {
      if (!file) return setError(new Error(t("Choose a file.")));
      bytes = new Uint8Array(await file.arrayBuffer());
    } else {
      if (!text) return setError(new Error(t("Enter a value.")));
      bytes = new TextEncoder().encode(text);
    }
    setBusy(true);
    try {
      const body: SecretSetRequest = { value_b64: bytesToB64(bytes), description };
      const meta = await api.post<SecretMeta>(`/secrets/${seg(ns)}/${seg(name)}`, body);
      setSaved(meta?.ref || `secret://${ns}/${name}`);
      setText("");
      setFile(null);
      if (fileRef.current) fileRef.current.value = "";
      onSaved();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("Set a secret")}</CardTitle>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="flex flex-col gap-3">
          <div className="grid grid-cols-1 gap-2 sm:grid-cols-[12rem_1fr]">
            <Input list="secret-ns" placeholder={t("namespace")} value={ns} onChange={(e) => setNs(e.target.value.trim())} aria-label={t("Namespace")} className="font-mono text-xs" />
            <datalist id="secret-ns">
              {NAMESPACES.map((n) => (
                <option key={n} value={n} />
              ))}
            </datalist>
            <Input placeholder={t("name")} value={name} onChange={(e) => setName(e.target.value.trim())} aria-label={t("Name")} className="font-mono text-xs" />
          </div>
          <Input placeholder={t("description (optional)")} value={description} onChange={(e) => setDescription(e.target.value)} aria-label={t("Description")} />
          <div className="flex items-center gap-1">
            <Button size="xs" variant={mode === "text" ? "secondary" : "ghost"} onClick={() => setMode("text")}>
              {t("Paste value")}
            </Button>
            <Button size="xs" variant={mode === "file" ? "secondary" : "ghost"} onClick={() => setMode("file")}>
              {t("From file")}
            </Button>
            {mode === "text" && (
              <Button size="xs" variant="ghost" className="ml-auto" onClick={() => setMasked((m) => !m)} aria-pressed={!masked}>
                {masked ? <Eye /> : <EyeOff />}
                {masked ? t("Show") : t("Hide")}
              </Button>
            )}
          </div>
          {mode === "text" ? (
            <Textarea
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder={t("secret value")}
              autoComplete="off"
              spellCheck={false}
              rows={4}
              className={cn("font-mono text-xs", masked && "[-webkit-text-security:disc]")}
              aria-label={t("Secret value")}
            />
          ) : (
            <Input ref={fileRef} type="file" onChange={(e) => setFile(e.target.files?.[0] ?? null)} aria-label={t("Secret file")} />
          )}
          <ErrorBox error={error} />
          {saved && <div className="text-sm text-emerald-700 dark:text-emerald-400">{t("Saved")} <Mono>{saved}</Mono></div>}
          <div>
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="animate-spin" />}
              {t("Save secret")}
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            {t("Values are encrypted server-side and never shown again. Reference them as")} <Mono>secret://namespace/name</Mono>.
          </p>
        </form>
      </CardContent>
    </Card>
  );
}

export function SecretsPanel() {
  const list = useApi<SecretMeta[]>("/secrets");
  const status = useApi<SecretStatus>("/secrets/status");
  const [toDelete, setToDelete] = React.useState<SecretMeta | null>(null);
  const secrets = list.data ?? [];
  const { t } = useT();

  return (
    <div className="flex flex-col gap-5">
      <p className="text-sm text-muted-foreground">{t("Names and references only — values are never displayed.")}</p>
      <ErrorBox error={status.error} />
      <LockStatus status={status.data} onChange={status.setData} />

      <div className="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,1fr)_28rem]">
        <Section title={t("Stored secrets ({n})", { n: secrets.length })}>
          <ErrorBox error={list.error} onRetry={list.reload} />
          {list.loading ? (
            <Loading />
          ) : secrets.length === 0 ? (
            <Empty>{t("No secrets stored.")}</Empty>
          ) : (
            <Card>
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead>{t("Reference")}</TableHead>
                    <TableHead className="hidden md:table-cell">{t("Description")}</TableHead>
                    <TableHead className="text-right">{t("Size")}</TableHead>
                    <TableHead className="hidden sm:table-cell">{t("Updated")}</TableHead>
                    <TableHead className="w-10" />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {secrets.map((s) => (
                    <TableRow key={s.ref || `${s.namespace}/${s.name}`}>
                      <TableCell>
                        <Mono className="select-all">{s.ref || `secret://${s.namespace}/${s.name}`}</Mono>
                      </TableCell>
                      <TableCell className="hidden max-w-xs truncate text-muted-foreground md:table-cell">{s.description || "—"}</TableCell>
                      <TableCell className="text-right tabular-nums text-muted-foreground">{formatBytes(s.size)}</TableCell>
                      <TableCell className="hidden whitespace-nowrap text-muted-foreground sm:table-cell" title={formatTime(s.updated_at)}>
                        {relativeTime(s.updated_at)}
                      </TableCell>
                      <TableCell>
                        <Button size="icon" variant="ghost" aria-label={t("Delete {name}", { name: s.ref })} onClick={() => setToDelete(s)}>
                          <Trash2 className="text-muted-foreground" />
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </Card>
          )}
        </Section>
        <SetSecretForm onSaved={list.reload} />
      </div>

      <ConfirmDialog
        open={!!toDelete}
        onOpenChange={(o) => !o && setToDelete(null)}
        title={t("Delete secret?")}
        description={
          toDelete
            ? t("{ref} will be removed. Nodes, queues and datasets referencing it will fail.", {
                ref: toDelete.ref || `secret://${toDelete.namespace}/${toDelete.name}`,
              })
            : ""
        }
        confirmLabel={t("Delete")}
        destructive
        onConfirm={async () => {
          if (!toDelete) return;
          await api.del<OkResponse>(`/secrets/${seg(toDelete.namespace)}/${seg(toDelete.name)}`);
          await list.reload();
        }}
      />
    </div>
  );
}
