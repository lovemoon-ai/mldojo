"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";

export interface ApiState<T> {
  data: T | undefined;
  error: unknown;
  loading: boolean;
  reload: () => Promise<void>;
  setData: (d: T | undefined) => void;
}

/**
 * GET an API path (null = skip). Keeps the previous data while refetching.
 * `interval` polls while the tab is visible; `enabled=false` pauses polling.
 */
export function useApi<T>(path: string | null, opts: { interval?: number; enabled?: boolean } = {}): ApiState<T> {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<unknown>(undefined);
  const [loading, setLoading] = useState<boolean>(!!path);
  const ctrl = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    if (!path) return;
    ctrl.current?.abort();
    const c = new AbortController();
    ctrl.current = c;
    try {
      const d = await api.get<T>(path, undefined, { signal: c.signal });
      if (c.signal.aborted) return;
      setData(d);
      setError(undefined);
    } catch (e) {
      if ((e as Error)?.name === "AbortError" || c.signal.aborted) return;
      setError(e);
    } finally {
      if (!c.signal.aborted) setLoading(false);
    }
  }, [path]);

  useEffect(() => {
    setData(undefined);
    setError(undefined);
    setLoading(!!path);
    void load();
    return () => ctrl.current?.abort();
  }, [path, load]);

  const { interval, enabled = true } = opts;
  useEffect(() => {
    if (!path || !interval || !enabled) return;
    const t = setInterval(() => {
      if (document.visibilityState === "visible") void load();
    }, interval);
    return () => clearInterval(t);
  }, [path, interval, enabled, load]);

  return { data, error, loading, reload: load, setData };
}

/** Current time, ticking every `ms` (for live elapsed counters). */
export function useNow(ms = 1000, active = true): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms, active]);
  return now;
}
