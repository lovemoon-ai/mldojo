"use client";

import * as React from "react";
import { ChevronDown, ChevronRight, Pin } from "lucide-react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { api, openFrameSocket, seg } from "@/lib/api";
import { useT } from "@/lib/i18n";
import type { MetricPoint, MetricsResponse, Run } from "@/lib/types";
import { isTerminal } from "@/lib/types";
import { cn, formatDuration, formatNumber } from "@/lib/utils";
import { Empty, ErrorBox, Loading } from "@/components/common";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

type Series = Record<string, MetricPoint[]>;
type XMode = "step" | "time";
const MAX_CHART_POINTS = 2000;
// A real run logs ~335 keys, 216 of them per-layer `moe_*/layerNN` noise: a group
// this big is never what you came to look at, so it starts closed.
const COLLAPSE_OVER = 8;
// Points per curve once a chart is actually shown. The client downsamples
// to MAX_CHART_POINTS anyway; this is about what crosses the network.
// Points per curve. A 44px-tall chart cannot show two thousand of them, and
// the flat wire format repeats the key name and a full timestamp on every
// one -- nineteen curves at full resolution is 4 MB. A caller that singled
// out a few gets the detail; a whole group gets the shape.
const POINTS_PER_KEY = 500;
const POINTS_PER_KEY_FEW = 2000;
const FEW = 4;
// A stable identity, so a chart whose data has not arrived does not remount
// on every render.
const EMPTY: MetricPoint[] = [];

/** A group opens by default when it is small, or when it holds a metric
 *  anybody would look for. On the run this was tuned against `training`
 *  has twenty members -- over the collapse threshold, and the only group
 *  worth seeing. */
function startsOpen(g: { members: string[]; lead: boolean }) {
  return g.lead || g.members.length <= COLLAPSE_OVER;
}
// Pins are global rather than per run — the same few keys matter across runs.
const PINS_KEY = "mldojo.metrics.pins";
/** Leaf names worth seeing first; their groups sort to the top. */
const PRIMARY =
  /^(steptime|lr|learning_rate|grad_norm|.*loss|reward|accuracy)$/i;
const SMOOTHING = [0, 0.6, 0.9, 0.99];
const UNGROUPED = "(ungrouped)";
const GRID = "mt-2 grid grid-cols-1 gap-3 lg:grid-cols-2 2xl:grid-cols-3";

/** Merge points into per-key arrays sorted by step (same step = replace). */
function merge(prev: Series, points: MetricPoint[]): Series {
  if (!points.length) return prev;
  const next: Series = { ...prev };
  const touched = new Set<string>();
  for (const p of points) {
    if (!p || typeof p.key !== "string" || typeof p.step !== "number") continue;
    if (!touched.has(p.key)) {
      next[p.key] = next[p.key] ? next[p.key].slice() : [];
      touched.add(p.key);
    }
    const arr = next[p.key];
    const last = arr[arr.length - 1];
    if (!last || p.step > last.step) arr.push(p);
    else if (p.step === last.step) arr[arr.length - 1] = p;
    else {
      let i = arr.length - 1;
      while (i > 0 && arr[i - 1].step >= p.step) i--;
      if (arr[i].step === p.step) arr[i] = p;
      else arr.splice(i, 0, p);
    }
  }
  return next;
}

function downsample<T>(pts: T[]): T[] {
  if (pts.length <= MAX_CHART_POINTS) return pts;
  const stride = Math.ceil(pts.length / MAX_CHART_POINTS);
  const out: T[] = [];
  for (let i = 0; i < pts.length; i += stride) out.push(pts[i]);
  if (out[out.length - 1] !== pts[pts.length - 1])
    out.push(pts[pts.length - 1]);
  return out;
}

function useChartColors() {
  const [c, setC] = React.useState({
    line: "#2a78d6",
    grid: "#e4e4e7",
    axis: "#71717a",
    surface: "#ffffff",
  });
  React.useEffect(() => {
    const read = () => {
      const s = getComputedStyle(document.documentElement);
      const v = (n: string, d: string) => s.getPropertyValue(n).trim() || d;
      setC({
        line: v("--chart-1", "#2a78d6"),
        grid: v("--chart-grid", "#e4e4e7"),
        axis: v("--chart-axis", "#71717a"),
        surface: v("--card", "#fff"),
      });
    };
    read();
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener("change", read);
    return () => mq.removeEventListener("change", read);
  }, []);
  return c;
}

/** Short axis labels: 1.2K, 0.35, 3e-4. */
const compact = (v: number) => {
  const a = Math.abs(v);
  if (a >= 1000)
    return Intl.NumberFormat(undefined, {
      notation: "compact",
      maximumFractionDigits: 1,
    }).format(v);
  if (a !== 0 && a < 1e-2) return v.toExponential(1).replace(".0e", "e");
  return String(Number(v.toPrecision(3)));
};

/** Seconds since the run's first point, as "1h12m" — short enough for a tick. */
const elapsedLabel = (s: number) => formatDuration(s * 1000).replace(/ /g, "");

const groupOf = (k: string) => {
  const i = k.lastIndexOf("/");
  return i < 0 ? UNGROUPED : k.slice(0, i);
};
const leafOf = (k: string) => k.slice(k.lastIndexOf("/") + 1);

interface Row {
  step: number;
  t: number; // seconds since the run's first point
  value: number;
  ema?: number;
}

/** Debiased EMA, so the head of the curve isn't dragged toward the seed. */
function emaOf(pts: MetricPoint[], a: number): number[] {
  const out = new Array<number>(pts.length);
  let s = 0;
  for (let i = 0; i < pts.length; i++) {
    s = a * s + (1 - a) * pts[i].value;
    out[i] = s / (1 - Math.pow(a, i + 1));
  }
  return out;
}

function ChartTooltip({
  active,
  payload,
}: {
  active?: boolean;
  payload?: { payload: Row }[];
}) {
  const { t } = useT();
  if (!active || !payload?.length) return null;
  const r = payload[0].payload;
  return (
    <div className="rounded-md border border-border bg-card px-2.5 py-1.5 text-xs shadow-md">
      <div className="font-semibold tabular-nums">
        {formatNumber(r.value, 6)}
      </div>
      {r.ema !== undefined && (
        <div className="tabular-nums text-muted-foreground">
          {t("ema")} {formatNumber(r.ema, 6)}
        </div>
      )}
      <div className="text-muted-foreground tabular-nums">
        {t("step {n}", { n: r.step.toLocaleString() })} · {elapsedLabel(r.t)}
      </div>
    </div>
  );
}

const MetricChart = React.memo(function MetricChart({
  name,
  points,
  colors,
  t0,
  xMode,
  log,
  smooth,
  pinned,
  onPin,
}: {
  name: string;
  points: MetricPoint[];
  colors: ReturnType<typeof useChartColors>;
  t0: number;
  xMode: XMode;
  log: boolean;
  smooth: number;
  pinned: boolean;
  onPin: (key: string) => void;
}) {
  const { data, dropped } = React.useMemo(() => {
    // A log axis can't place <= 0; drop those points rather than the series.
    const src = log ? points.filter((p) => p.value > 0) : points;
    // Smooth at full resolution, then thin — the reverse would widen the window.
    const ema = smooth > 0 ? emaOf(src, smooth) : undefined;
    const rows = src.map((p, i) => {
      const ms = Date.parse(p.ts);
      return {
        step: p.step,
        t: Number.isFinite(ms) ? (ms - t0) / 1000 : 0,
        value: p.value,
        ema: ema?.[i],
      };
    });
    return { data: downsample(rows), dropped: points.length - src.length };
  }, [points, log, smooth, t0]);
  const last = points[points.length - 1];
  const { t } = useT();
  return (
    <Card className="p-3">
      <div className="mb-1 flex items-baseline justify-between gap-2">
        <h3 className="truncate font-mono text-xs font-medium" title={name}>
          {name}
        </h3>
        <div className="flex shrink-0 items-baseline gap-1.5">
          {dropped > 0 && (
            <span className="text-[11px] text-muted-foreground">
              {t("{n} ≤0 hidden", { n: dropped })}
            </span>
          )}
          <span className="text-xs tabular-nums text-muted-foreground">
            {last ? formatNumber(last.value) : "—"}
          </span>
          <button
            type="button"
            onClick={() => onPin(name)}
            aria-pressed={pinned}
            aria-label={pinned ? t("Unpin {name}", { name }) : t("Pin {name}", { name })}
            className={cn(
              "cursor-pointer self-center rounded transition-colors",
              pinned
                ? "text-foreground"
                : "text-muted-foreground/40 hover:text-foreground",
            )}
          >
            <Pin className={cn("size-3.5", pinned && "fill-current")} />
          </button>
        </div>
      </div>
      <div className="h-44">
        {points.length === 0 ? (
          // Its history is on the way. Drawing an empty axis would look
          // like a metric that exists and is flat at zero.
          <div className="flex h-full items-center justify-center text-xs text-muted-foreground">
            {t("loading…")}
          </div>
        ) : (
          <ResponsiveContainer width="100%" height="100%">
            <LineChart
              data={data}
              margin={{ top: 6, right: 8, bottom: 0, left: 0 }}
            >
              <CartesianGrid stroke={colors.grid} vertical={false} />
              <XAxis
                dataKey={xMode === "time" ? "t" : "step"}
                type="number"
                domain={["dataMin", "dataMax"]}
                tick={{ fontSize: 10, fill: colors.axis }}
                tickLine={false}
                axisLine={{ stroke: colors.grid }}
                tickFormatter={xMode === "time" ? elapsedLabel : compact}
                minTickGap={24}
              />
              <YAxis
                width={44}
                scale={log ? "log" : "auto"}
                domain={["auto", "auto"]}
                tick={{ fontSize: 10, fill: colors.axis }}
                tickLine={false}
                axisLine={false}
                tickFormatter={compact}
              />
              <Tooltip
                content={<ChartTooltip />}
                cursor={{ stroke: colors.axis, strokeWidth: 1 }}
                isAnimationActive={false}
              />
              {smooth > 0 && (
                <Line
                  type="linear"
                  dataKey="value"
                  stroke={colors.line}
                  strokeOpacity={0.25}
                  strokeWidth={1.5}
                  dot={false}
                  activeDot={false}
                  isAnimationActive={false}
                />
              )}
              <Line
                type="linear"
                dataKey={smooth > 0 ? "ema" : "value"}
                stroke={colors.line}
                strokeWidth={2}
                strokeLinejoin="round"
                strokeLinecap="round"
                dot={false}
                activeDot={{
                  r: 4,
                  fill: colors.line,
                  stroke: colors.surface,
                  strokeWidth: 2,
                }}
                isAnimationActive={false}
              />
            </LineChart>
          </ResponsiveContainer>
        )}
      </div>
    </Card>
  );
});

function Seg<T extends string | number>({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: T;
  options: { v: T; label: string }[];
  onChange: (v: T) => void;
}) {
  return (
    <div className="flex items-center gap-1.5">
      <span className="text-xs text-muted-foreground">{label}</span>
      <div className="inline-flex rounded-md bg-muted p-0.5">
        {options.map((o) => (
          <button
            key={String(o.v)}
            type="button"
            aria-pressed={o.v === value}
            onClick={() => onChange(o.v)}
            className={cn(
              "cursor-pointer rounded px-2 py-0.5 text-xs transition-colors",
              o.v === value
                ? "bg-card font-medium text-foreground shadow-sm"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {o.label}
          </button>
        ))}
      </div>
    </div>
  );
}

export function MetricsTab({ run }: { run: Run }) {
  const runId = run.id;
  const terminal = isTerminal(run.status);
  const [series, setSeries] = React.useState<Series>({});
  const [order, setOrder] = React.useState<string[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<unknown>();
  const [filter, setFilter] = React.useState("");
  const [xMode, setXMode] = React.useState<XMode>("step");
  const [log, setLog] = React.useState(false);
  const [smooth, setSmooth] = React.useState(0);
  const [pins, setPins] = React.useState<Set<string>>(new Set());
  const [openOverride, setOpenOverride] = React.useState<
    Record<string, boolean>
  >({});
  const colors = useChartColors();
  const { t } = useT();
  const maxStep = React.useRef(0);
  const pending = React.useRef<MetricPoint[]>([]);
  // Keys whose history has been requested, so expanding a group twice does
  // not fetch it twice.
  const fetched = React.useRef<Set<string>>(new Set());

  // Read after mount so the server and first client render agree.
  React.useEffect(() => {
    try {
      const raw = window.localStorage.getItem(PINS_KEY);
      if (raw) setPins(new Set(JSON.parse(raw) as string[]));
    } catch {
      /* unreadable storage just means no pins */
    }
  }, []);

  const togglePin = React.useCallback((key: string) => {
    setPins((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      try {
        window.localStorage.setItem(PINS_KEY, JSON.stringify([...next]));
      } catch {
        /* pins stay for this session only */
      }
      return next;
    });
  }, []);

  const ingest = React.useCallback((pts: MetricPoint[]) => {
    for (const p of pts) if (p.step > maxStep.current) maxStep.current = p.step;
    setSeries((prev) => merge(prev, pts));
    setOrder((prev) => {
      const seen = new Set(prev);
      const add = pts
        .map((p) => p.key)
        .filter((k) => !seen.has(k) && (seen.add(k), true));
      return add.length ? [...prev, ...add] : prev;
    });
  }, []);

  React.useEffect(() => {
    let cancelled = false;
    setSeries({});
    setOrder([]);
    setLoading(true);
    setError(undefined);
    maxStep.current = 0;
    fetched.current = new Set();
    // Only the key list up front. A real run logs 335 of them; pulling
    // every series to show four is ~10 MB now and ~75 MB by the end of
    // training, which is what made this tab hang.
    api
      .get<MetricsResponse>(`/runs/${seg(runId)}/metrics?keys_only=1`)
      .then((r) => {
        if (cancelled) return;
        setOrder(r?.keys ?? []);
      })
      .catch((e) => !cancelled && setError(e))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [runId, ingest]);

  // Live updates if the run was active when loaded (batched to one render per
  // 250ms); the socket ends itself with an `eof` frame when the run finishes.
  const terminalRef = React.useRef(terminal);
  terminalRef.current = terminal;
  React.useEffect(() => {
    if (terminalRef.current || loading) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const sock = openFrameSocket(`/runs/${seg(runId)}/metrics/ws`, {
      // History comes from the REST fetches above, per visible curve. Ask
      // the socket for live points only, or it replays every key from step
      // zero -- the thing this panel was just taught not to do.
      params: () => ({ since: `step:${maxStep.current}`, live_only: "1" }),
      onFrame: (f) => {
        if (f.kind !== "metric" || !Array.isArray(f.payload)) return;
        pending.current.push(...(f.payload as unknown as MetricPoint[]));
        if (!timer)
          timer = setTimeout(() => {
            timer = undefined;
            const pts = pending.current;
            pending.current = [];
            ingest(pts);
          }, 250);
      },
    });
    return () => {
      sock.close();
      if (timer) clearTimeout(timer);
    };
  }, [runId, loading, ingest]);

  const keys = React.useMemo(
    () => order.filter((k) => k.toLowerCase().includes(filter.toLowerCase())),
    [order, filter],
  );
  const stats = React.useMemo(
    () =>
      keys
        .filter((k) => series[k]?.length)
        .map((k) => {
          const pts = series[k];
          let min = Infinity;
          let max = -Infinity;
          for (const p of pts) {
            if (p.value < min) min = p.value;
            if (p.value > max) max = p.value;
          }
          return { key: k, last: pts[pts.length - 1], min, max, n: pts.length };
        }),
    [keys, series],
  );
  // Wall-clock x is relative to the run as a whole, not to each key.
  const t0 = React.useMemo(() => {
    let first = Infinity;
    for (const k of keys) {
      const pts = series[k];
      if (!pts?.length) continue;
      const ms = Date.parse(pts[0].ts);
      if (ms < first) first = ms;
    }
    return Number.isFinite(first) ? first : 0;
  }, [keys, series]);

  const pinned = React.useMemo(
    () => keys.filter((k) => pins.has(k)),
    [keys, pins],
  );
  const groups = React.useMemo(() => {
    const by = new Map<string, string[]>();
    for (const k of keys) {
      if (pins.has(k)) continue; // already shown in the pinned section
      const g = groupOf(k);
      const members = by.get(g);
      if (members) members.push(k);
      else by.set(g, [k]);
    }
    return [...by.entries()]
      .map(([name, members]) => ({
        name,
        members,
        lead: members.some((k) => PRIMARY.test(leafOf(k))),
      }))
      .sort(
        (a, b) =>
          Number(b.lead) - Number(a.lead) || a.name.localeCompare(b.name),
      );
  }, [keys, pins]);

  // Which charts are actually on screen: pinned, plus every member of an
  // open group. This is the set worth downloading.
  const visible = React.useMemo(() => {
    const out = new Set(pinned);
    for (const g of groups) {
      if (openOverride[g.name] ?? startsOpen(g)) {
        for (const k of g.members) out.add(k);
      }
    }
    return out;
  }, [pinned, groups, openOverride]);

  React.useEffect(() => {
    const want = [...visible].filter((k) => !fetched.current.has(k));
    if (!want.length) return;
    for (const k of want) fetched.current.add(k);
    let cancelled = false;
    // One request for the batch: opening a 36-chart group should not be 36
    // round trips.
    const budget = want.length <= FEW ? POINTS_PER_KEY_FEW : POINTS_PER_KEY;
    const q = `key=${want.map(encodeURIComponent).join(",")}&max_points=${budget}`;
    api
      .get<MetricsResponse>(`/runs/${seg(runId)}/metrics?${q}`)
      .then((r) => !cancelled && ingest(r?.points ?? []))
      .catch(() => {
        // Let a failed batch be retried rather than leaving those charts
        // empty forever.
        for (const k of want) fetched.current.delete(k);
      });
    return () => {
      cancelled = true;
    };
  }, [visible, runId, ingest]);

  if (loading) return <Loading label={t("Loading metrics…")} />;
  if (error && !order.length) return <ErrorBox error={error} />;
  if (!order.length)
    return (
      <Empty>
        {t("No metrics yet. Metrics come from file scanning (tensorboard / jsonl outputs) or the SDK ingest endpoint.")}
      </Empty>
    );

  const chart = (k: string) => (
    <MetricChart
      key={k}
      name={k}
      points={series[k] ?? EMPTY}
      colors={colors}
      t0={t0}
      xMode={xMode}
      log={log}
      smooth={smooth}
      pinned={pins.has(k)}
      onPin={togglePin}
    />
  );

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <Input
          placeholder={t("Filter keys…")}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="h-8 max-w-60"
        />
        <Seg
          label="x"
          value={xMode}
          onChange={setXMode}
          options={[
            { v: "step" as XMode, label: t("step") },
            { v: "time" as XMode, label: t("time") },
          ]}
        />
        <Seg
          label="y"
          value={log ? "log" : "linear"}
          onChange={(v) => setLog(v === "log")}
          options={[
            { v: "linear", label: t("linear") },
            { v: "log", label: t("log") },
          ]}
        />
        <Seg
          label={t("ema")}
          value={smooth}
          onChange={setSmooth}
          options={SMOOTHING.map((a) => ({
            v: a,
            label: a ? String(a) : t("off"),
          }))}
        />
        <span className="text-xs text-muted-foreground">
          {t(keys.length === 1 ? "{n} key" : "{n} keys", { n: keys.length })}
          {!terminal && ` · ${t("live")}`}
        </span>
      </div>
      <Card>
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>{t("Key")}</TableHead>
              <TableHead className="text-right">{t("Step")}</TableHead>
              <TableHead className="text-right">{t("Latest")}</TableHead>
              <TableHead className="hidden text-right sm:table-cell">
                {t("Min")}
              </TableHead>
              <TableHead className="hidden text-right sm:table-cell">
                {t("Max")}
              </TableHead>
              <TableHead className="hidden text-right md:table-cell">
                {t("Points")}
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {stats.map((s) => (
              <TableRow key={s.key}>
                <TableCell className="font-mono text-xs">{s.key}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {s.last?.step.toLocaleString()}
                </TableCell>
                <TableCell className="text-right font-medium tabular-nums">
                  {formatNumber(s.last?.value)}
                </TableCell>
                <TableCell className="hidden text-right tabular-nums text-muted-foreground sm:table-cell">
                  {formatNumber(s.min)}
                </TableCell>
                <TableCell className="hidden text-right tabular-nums text-muted-foreground sm:table-cell">
                  {formatNumber(s.max)}
                </TableCell>
                <TableCell className="hidden text-right tabular-nums text-muted-foreground md:table-cell">
                  {s.n.toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Card>
      {pinned.length > 0 && (
        <section>
          <div className="flex items-center gap-1.5 px-1 text-xs">
            <Pin className="size-3.5 fill-current" />
            <span className="font-medium">{t("Pinned")}</span>
            <span className="text-muted-foreground">({pinned.length})</span>
          </div>
          <div className={GRID}>{pinned.map(chart)}</div>
        </section>
      )}
      {groups.map((g) => {
        const open = openOverride[g.name] ?? startsOpen(g);
        return (
          <section key={g.name}>
            <button
              type="button"
              aria-expanded={open}
              onClick={() =>
                setOpenOverride((s) => ({ ...s, [g.name]: !open }))
              }
              className="flex w-full cursor-pointer items-center gap-1.5 rounded px-1 py-0.5 text-xs hover:bg-accent"
            >
              {open ? (
                <ChevronDown className="size-3.5" />
              ) : (
                <ChevronRight className="size-3.5" />
              )}
              <span className="truncate font-mono font-medium">{g.name === UNGROUPED ? t(UNGROUPED) : g.name}</span>
              <span className="text-muted-foreground">
                ({g.members.length})
              </span>
            </button>
            {open && <div className={GRID}>{g.members.map(chart)}</div>}
          </section>
        );
      })}
    </div>
  );
}
