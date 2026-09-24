// MLDojo API client (docs/api.md). REST under /api/v1, authenticated either by
// the SSO session cookie (httpOnly, same-origin, sent automatically) or by the
// bearer token from localStorage; WebSockets and raw artifact URLs carry that
// token as ?token= when one is stored.
import { translate } from "./i18n";
import type { ApiErrorBody, AuthConfig, AuthStatus, Frame, OkResponse } from "./types";

export const API_BASE = (process.env.NEXT_PUBLIC_API_BASE || "").replace(/\/+$/, "");
export const API_PREFIX = "/api/v1";
export const TOKEN_KEY = "mldojo.token";

export function getToken(): string {
  if (typeof window === "undefined") return "";
  try {
    return window.localStorage.getItem(TOKEN_KEY) || "";
  } catch {
    return "";
  }
}

export function setToken(token: string) {
  if (token) window.localStorage.setItem(TOKEN_KEY, token);
  else window.localStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, message: string, code: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

type Params = Record<string, string | number | boolean | null | undefined>;

function qs(params?: Params): string {
  if (!params) return "";
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "") continue;
    sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : "";
}

/** Absolute (or same-origin relative) URL for an API path such as `/runs`. */
export function apiUrl(path: string, params?: Params): string {
  return `${API_BASE}${API_PREFIX}${path}${qs(params)}`;
}

/** URL usable in <video src>/<img src>/<a href>; `?token=` only if one is stored. */
export function rawUrl(path: string, params?: Params): string {
  return apiUrl(path, { ...params, token: getToken() || undefined });
}

/** ws(s):// URL for a WebSocket endpoint; `?token=` only if one is stored. */
export function wsUrl(path: string, params?: Params): string {
  const base = API_BASE || window.location.origin;
  const url = new URL(`${base}${API_PREFIX}${path}${qs({ ...params, token: getToken() || undefined })}`, window.location.href);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return url.toString();
}

export function redirectToLogin() {
  if (typeof window === "undefined") return;
  if (window.location.pathname.startsWith("/login")) return;
  const next = window.location.pathname + window.location.search;
  window.location.href = `/login?next=${encodeURIComponent(next)}`;
}

export interface RequestOptions {
  params?: Params;
  body?: unknown;
  signal?: AbortSignal;
  /** Do not redirect to /login on 401 (used by the login page itself). */
  noAuthRedirect?: boolean;
  headers?: Record<string, string>;
}

/** Low-level fetch with auth + error handling; returns the Response on 2xx. */
export async function apiFetch(method: string, path: string, opts: RequestOptions = {}): Promise<Response> {
  const headers: Record<string, string> = { Accept: "application/json", ...opts.headers };
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  let body: BodyInit | undefined;
  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }
  let res: Response;
  try {
    res = await fetch(apiUrl(path, opts.params), { method, headers, body, signal: opts.signal });
  } catch (e) {
    if ((e as Error)?.name === "AbortError") throw e;
    throw new ApiError(0, translate("Network error: {msg}", { msg: (e as Error)?.message || translate("request failed") }), "unreachable");
  }
  if (res.ok) return res;

  let message = `${res.status} ${res.statusText}`.trim();
  let code = res.status === 401 ? "unauthorized" : res.status === 404 ? "not_found" : "internal";
  try {
    const text = await res.text();
    try {
      const j = JSON.parse(text) as Partial<ApiErrorBody>;
      if (j.error) message = j.error;
      if (j.code) code = j.code;
    } catch {
      if (text.trim()) message = text.trim().slice(0, 500);
    }
  } catch {
    /* ignore body read errors */
  }
  if (res.status === 401 && !opts.noAuthRedirect) redirectToLogin();
  throw new ApiError(res.status, message, code);
}

export async function request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
  const res = await apiFetch(method, path, opts);
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (!text) return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    return text as unknown as T;
  }
}

export const api = {
  get: <T>(path: string, params?: Params, opts: Omit<RequestOptions, "params"> = {}) =>
    request<T>("GET", path, { ...opts, params }),
  post: <T>(path: string, body?: unknown, opts: RequestOptions = {}) => request<T>("POST", path, { ...opts, body }),
  del: <T>(path: string, params?: Params, opts: Omit<RequestOptions, "params"> = {}) =>
    request<T>("DELETE", path, { ...opts, params }),
};

/** Encode one path segment (ids, names). Queue ids keep their `/`. */
export const seg = (s: string) => encodeURIComponent(s);
export const queuePath = (id: string) => id.split("/").map(seg).join("/");

export function errorMessage(e: unknown): string {
  if (!e) return "";
  if (e instanceof ApiError) return e.code && e.code !== "internal" ? `${e.message} (${e.code})` : e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}

// ---- Auth ------------------------------------------------------------------

/** GET /auth/config — unauthenticated; what the login page may offer. */
export const getAuthConfig = () => api.get<AuthConfig>("/auth/config", undefined, { noAuthRedirect: true });

/** GET /auth/me — unauthenticated; resolves the session cookie or the token. */
export const getAuthStatus = () => api.get<AuthStatus>("/auth/me", undefined, { noAuthRedirect: true });

/** POST /auth/logout — clears the session cookie server-side. */
export const logout = () => api.post<OkResponse>("/auth/logout", undefined, { noAuthRedirect: true });

/**
 * Conductor login endpoint: a 302 into the authorize page, so the whole window
 * must navigate there (`window.location.href = ssoLoginUrl(next)`).
 */
export const ssoLoginUrl = (next: string) => apiUrl("/auth/login", { next });

// ---- WebSocket frames ------------------------------------------------------

export type SocketState = "connecting" | "open" | "reconnecting" | "done" | "closed";

export interface FrameSocketOptions {
  /** Query params, re-evaluated on every (re)connect (e.g. to resume at an offset). */
  params?: () => Params;
  onFrame: (frame: Frame) => void;
  onState?: (state: SocketState) => void;
  /** Reconnect on unexpected close (default true). An `eof` frame always stops. */
  reconnect?: boolean;
}

/** Opens a Frame WebSocket with exponential-backoff reconnect. */
export function openFrameSocket(path: string, opts: FrameSocketOptions): { close: () => void } {
  let ws: WebSocket | null = null;
  let stopped = false;
  let done = false;
  let delay = 1000;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const connect = () => {
    if (stopped) return;
    opts.onState?.("connecting");
    try {
      ws = new WebSocket(wsUrl(path, opts.params?.()));
    } catch {
      schedule();
      return;
    }
    ws.onopen = () => {
      delay = 1000;
      opts.onState?.("open");
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data !== "string") return;
      let frame: Frame;
      try {
        frame = JSON.parse(ev.data) as Frame;
      } catch {
        return;
      }
      if (frame.kind === "eof") done = true;
      opts.onFrame(frame);
      if (done) ws?.close();
    };
    ws.onclose = () => {
      ws = null;
      if (stopped) return;
      if (done) {
        opts.onState?.("done");
        return;
      }
      if (opts.reconnect === false) {
        opts.onState?.("closed");
        return;
      }
      schedule();
    };
  };

  const schedule = () => {
    opts.onState?.("reconnecting");
    timer = setTimeout(connect, delay);
    delay = Math.min(delay * 2, 15000);
  };

  connect();
  return {
    close: () => {
      stopped = true;
      if (timer) clearTimeout(timer);
      if (ws) {
        ws.onclose = null;
        ws.close();
      }
      ws = null;
    },
  };
}
