# MLDojo web

Next.js (App Router, static export) + Tailwind. Talks to `mldojo-api` over
`/api/v1` (REST + WebSocket, see `../docs/api.md`).

```sh
npm ci

# dev against a running API (the server allows CORS for cross-origin dev)
NEXT_PUBLIC_API_BASE=http://localhost:8765 npm run dev

# production build -> out/ (served by mldojo-api, /path falls back to /path.html)
npm run build

npx tsc --noEmit    # typecheck
npm run icons       # regenerate public/icons/*
```

`/login` offers Conductor SSO when `GET /auth/config` reports `sso_enabled`;
the session is an httpOnly cookie, so it only works same-origin (cross-origin
dev falls back to the token). The API token is stored in
`localStorage["mldojo.token"]` (set it on `/login` or `/settings`) and is still
sent as a Bearer header / `?token=`. Pages use query params instead of dynamic segments
(`/run?id=…`, `/experiment?project=…&name=…`) so the export is fully static.
