# SHIFT web (`apps/web`)

Marketing site, documentation, and organization dashboard for SHIFT — moving
supported computation between Linux machines. Next.js (App Router, `src/`
directory), React 19, Tailwind v4, zustand + react-query.

## Scripts

| Command | What it does |
|---------|--------------|
| `npm run dev` | Sync the OpenAPI spec copy, then start the dev server |
| `npm run build` | Sync the OpenAPI spec copy, then `next build --webpack` |
| `npm run lint` | ESLint (eslint-config-next) |
| `npm run sync-openapi` | Copy `api/openapi/shift.yaml` → `public/api/openapi.yaml` and run the types drift gate |
| `npm run validate-content` | Sync + full content gate (CLI claims, forbidden copy, security.txt, sitemap ↔ routes, types drift) |
| `npm run test:content` | Content gate only (`scripts/validate-content.mjs`) |

Run everything from `apps/web`. The scripts resolve their paths from their own
location, so `node apps/web/scripts/validate-content.mjs` also works from the
repo root.

**The OpenAPI sync is mandatory.** `public/api/` is generated: `dev` and
`build` run `scripts/sync-openapi.mjs` first, and the copy is verified
byte-identical against `api/openapi/shift.yaml` (the canonical spec, owned at
the repo root). `scripts/check-types-drift.mjs` additionally fails the sync
whenever `src/lib/types.ts` (the hand-maintained client types) drifts from the
spec's enums or property contracts.

## Environment

See `.env.example`. Two variables, both `NEXT_PUBLIC_*` (inlined at build):

- `NEXT_PUBLIC_API_URL` — control-plane origin. Dev default
  `http://127.0.0.1:8090`. Production builds fail (see `next.config.ts`) when
  unset; `.env.production` supplies `https://shiftgate.dev`.
- `NEXT_PUBLIC_SITE_URL` — canonical site origin for metadata, Open Graph,
  `robots.ts`, and `sitemap.ts`.

`.env` and `.env*.local` are gitignored; `.env.example` and `.env.production`
are checked in.

## Layout

- `src/proxy.ts` — per-request CSP (nonce + `strict-dynamic` on `/app`), HSTS,
  and the security header pipeline. The CSP lives here because it needs a fresh
  nonce per request; do not move it to `next.config.ts`.
- `src/app/layout.tsx` — root layout, metadata, next/font wiring
  (`--font-shift-display` / `--font-shift-mono` feed the theme tokens in
  `globals.css`).
- `src/app/{error,global-error,loading,not-found}.tsx` and
  `src/app/app/{error,loading}.tsx` — segment boundaries. Error boundaries are
  Client Components and receive `retry` (this Next version's name for reset).
- `src/app/**/page.tsx` — marketing pages, `/docs`, `/guides`, `/compare`,
  `/use-cases`; `src/app/app/**` is the dashboard shell (`app-shell.tsx`).
- `src/app/sitemap.ts` / `robots.ts` — generated routes; the content gate fails
  when the sitemap and the route tree disagree.
- `scripts/validate-content.mjs` — content gate: phantom CLI commands and flags
  (parsed from `cmd/shift/main.go`), unqualified promises, stale API fields,
  RFC 9116 `security.txt`, sitemap completeness.

## Fonts and images

Space Grotesk (display) and IBM Plex Mono (code) via `next/font`, self-hosted.
`public/` ships `logo.png` (wordmark), `icon.png` (mark), `favicon.png` (square
app icon), `og.png` (1200×630 social card), and `.well-known/security.txt`.
