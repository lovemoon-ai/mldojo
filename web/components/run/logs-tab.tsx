"use client";

import * as React from "react";
import { ArrowDownToLine, WrapText } from "lucide-react";
import { apiFetch, openFrameSocket, seg, type SocketState } from "@/lib/api";
import { translate, useT, type Msg } from "@/lib/i18n";
import type { LogPayload, Run } from "@/lib/types";
import { cn, formatBytes, stripAnsi } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";

const CAP = 2 * 1024 * 1024; // max buffered characters (~2 MB)
const TRIM_TO = Math.floor(CAP * 0.85);
const STREAMS = ["all", "stdout", "stderr", "system"] as const;
type StreamSel = (typeof STREAMS)[number];

interface Segment {
  stream: string;
  text: string;
}

interface Buffer {
  segs: Segment[];
  size: number;
  truncated: boolean;
  ends: Record<string, number>; // per-stream byte offset already received
}

const enc = new TextEncoder();
const dec = new TextDecoder();

function newBuffer(): Buffer {
  return { segs: [], size: 0, truncated: false, ends: {} };
}

function append(buf: Buffer, stream: string, raw: string) {
  const text = stripAnsi(raw);
  if (!text) return;
  const last = buf.segs[buf.segs.length - 1];
  if (last && last.stream === stream) last.text += text;
  else buf.segs.push({ stream, text });
  buf.size += text.length;
  if (buf.size <= CAP) return;
  // Drop from the front (whole lines where possible).
  let excess = buf.size - TRIM_TO;
  while (excess > 0 && buf.segs.length > 1 && buf.segs[0].text.length <= excess) {
    const s = buf.segs.shift()!;
    excess -= s.text.length;
    buf.size -= s.text.length;
  }
  if (excess > 0 && buf.segs.length) {
    const first = buf.segs[0];
    const nl = first.text.indexOf("\n", excess);
    const cut = nl < 0 ? excess : nl + 1;
    first.text = first.text.slice(cut);
    buf.size -= cut;
  }
  buf.truncated = true;
}

/** Collapse carriage-return progress updates (tqdm) to their last state. */
function collapseCR(s: string): string {
  if (!s.includes("\r")) return s;
  return s.replace(/\r\n/g, "\n").replace(/[^\n]*\r(?=[^\n])/g, "");
}

const STREAM_CLASS: Record<string, string> = {
  stderr: "text-red-700 dark:text-red-400",
  system: "text-blue-700 dark:text-sky-400 italic",
};

export function LogsTab({ run, onStatus }: { run: Run; onStatus?: (r: Run) => void }) {
  const runId = run.id;
  const [stream, setStream] = React.useState<StreamSel>("all");
  const [wrap, setWrap] = React.useState(true);
  const [follow, setFollow] = React.useState(true);
  const [state, setState] = React.useState<SocketState>("connecting");
  const [serverError, setServerError] = React.useState<string>("");
  const [, setVersion] = React.useState(0);
  const { t } = useT();

  const bufRef = React.useRef<Buffer>(newBuffer());
  const followRef = React.useRef(true);
  const scrollRef = React.useRef<HTMLDivElement>(null);
  const onStatusRef = React.useRef(onStatus);
  onStatusRef.current = onStatus;

  React.useEffect(() => {
    const buf = newBuffer();
    bufRef.current = buf;
    setServerError("");
    setVersion((v) => v + 1);

    let flushTimer: ReturnType<typeof setTimeout> | undefined;
    const scheduleFlush = () => {
      if (flushTimer) return;
      flushTimer = setTimeout(() => {
        flushTimer = undefined;
        setVersion((v) => v + 1);
      }, 100);
    };

    const ingest = (p: LogPayload) => {
      const s = p.stream || (stream === "all" ? "stdout" : stream);
      let data = p.data ?? "";
      const bytes = enc.encode(data);
      const known = buf.ends[s] ?? 0;
      const off = typeof p.offset === "number" ? p.offset : known;
      if (off + bytes.length <= known && bytes.length > 0) return; // duplicate after reconnect
      if (off < known) data = dec.decode(bytes.subarray(known - off));
      buf.ends[s] = off + bytes.length;
      append(buf, s, data);
      scheduleFlush();
    };

    let cancelled = false;
    let sock: { close: () => void } | undefined;

    const start = async () => {
      // Single streams: fetch the tail over REST first, then follow from its end.
      if (stream !== "all") {
        try {
          const res = await apiFetch("GET", `/runs/${seg(runId)}/logs`, { params: { stream, tail: CAP } });
          const size = Number(res.headers.get("X-Log-Size"));
          const text = await res.text();
          if (cancelled) return;
          if (res.headers.has("X-Log-Size") && Number.isFinite(size)) {
            let t = text;
            const got = enc.encode(text).length;
            if (size > got) {
              const nl = t.indexOf("\n");
              if (nl >= 0) t = t.slice(nl + 1); // drop the partial first line
              buf.truncated = true;
            }
            buf.ends[stream] = size;
            append(buf, stream, t);
            scheduleFlush();
          }
        } catch {
          /* fall back to streaming from offset 0 */
        }
      }
      if (cancelled) return;
      sock = openFrameSocket(`/runs/${seg(runId)}/logs/ws`, {
        // Resume where we left off on reconnect. For `all` the offset is
        // per-stream on the server, so resend from 0 and de-dupe client-side.
        params: () => ({ stream, follow: 1, offset: stream === "all" ? 0 : (buf.ends[stream] ?? 0) }),
        onState: setState,
        onFrame: (f) => {
          if (f.kind === "log") ingest(f.payload as unknown as LogPayload);
          else if (f.kind === "status") onStatusRef.current?.(f.payload as unknown as Run);
          else if (f.kind === "error") {
            const e = (f.payload as { error?: string } | null)?.error;
            setServerError(e || translate("stream error"));
          }
        },
      });
    };
    void start();

    return () => {
      cancelled = true;
      sock?.close();
      if (flushTimer) clearTimeout(flushTimer);
    };
  }, [runId, stream]);

  // Autoscroll after every flush while following.
  React.useLayoutEffect(() => {
    const el = scrollRef.current;
    if (el && followRef.current) el.scrollTop = el.scrollHeight;
  });

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 32;
    if (atBottom !== followRef.current) {
      followRef.current = atBottom;
      setFollow(atBottom);
    }
  };

  const jumpToBottom = () => {
    followRef.current = true;
    setFollow(true);
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  };

  const buf = bufRef.current;
  const stateLabel: Record<SocketState, [Msg, string]> = {
    connecting: ["connecting", "bg-amber-500"],
    open: ["live", "bg-emerald-500"],
    reconnecting: ["reconnecting", "bg-amber-500"],
    done: ["complete", "bg-zinc-400"],
    closed: ["closed", "bg-zinc-400"],
  };
  const [label, dot] = stateLabel[state];

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={stream} onChange={(e) => setStream(e.target.value as StreamSel)} aria-label={t("Log stream")} className="w-32">
          {STREAMS.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </Select>
        <Button size="sm" variant={wrap ? "secondary" : "ghost"} onClick={() => setWrap((w) => !w)} aria-pressed={wrap}>
          <WrapText /> {t("Wrap")}
        </Button>
        <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <span className={cn("size-2 rounded-full", dot, state === "open" && "animate-pulse")} />
          {t(label)}
        </span>
        <span className="ml-auto text-xs text-muted-foreground tabular-nums">
          {formatBytes(buf.size)}
          {buf.truncated && ` · ${t("earlier output truncated")}`}
        </span>
      </div>
      {serverError && (
        <div className="rounded-md border border-red-600/25 bg-red-500/10 px-3 py-1.5 text-xs text-red-700 dark:text-red-400">
          {serverError}
        </div>
      )}
      <div className="relative">
        <div
          ref={scrollRef}
          onScroll={onScroll}
          className="h-[calc(100dvh-22rem)] min-h-72 overflow-auto rounded-md border border-border bg-code"
        >
          <pre className={cn("p-3 font-mono text-xs leading-5", wrap ? "whitespace-pre-wrap break-all" : "whitespace-pre")}>
            {buf.segs.length === 0 ? (
              <span className="text-muted-foreground">
                {state === "done" ? t("No output.") : t("Waiting for output…")}
              </span>
            ) : (
              buf.segs.map((s, i) => (
                <span key={i} className={stream === "all" ? STREAM_CLASS[s.stream] : undefined}>
                  {collapseCR(s.text)}
                </span>
              ))
            )}
            {state === "done" && buf.segs.length > 0 && (
              <span className="mt-2 block text-muted-foreground">{t("— end of log —")}</span>
            )}
          </pre>
        </div>
        {!follow && (
          <Button size="sm" variant="outline" className="absolute bottom-3 right-4 shadow-md" onClick={jumpToBottom}>
            <ArrowDownToLine /> {t("Follow")}
          </Button>
        )}
      </div>
    </div>
  );
}
