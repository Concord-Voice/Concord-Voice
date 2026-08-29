# Concord Voice Admin Portal

Browser-based operations client served by the control plane at `/admin/`. It
uses the same origin for the SPA, admin authentication, enrollment, and the
fixed read-only metrics API.

## Requirements

- Node.js 24.15+ (Node 26 is supported)
- npm 10+

## Development

```bash
npm ci
npm run dev
```

Open `http://127.0.0.1:5173/admin/`. Vite does not proxy API requests.
`/admin/api/v1/*` targets the Vite origin, and it fails unless a test
intercepts it. Use the production control-plane image for live same-origin API
integration.

## Checks and builds

```bash
npm run format:check
npm run lint
npm run typecheck
npm run test
npm run test:coverage
npm run build
npm run test:e2e
```

The Playwright suite builds and previews the portal itself, then intercepts API
requests. To inspect a production build manually:

```bash
npm run build
npx vite preview --host 127.0.0.1 --port 4181 --strictPort
```

Open `http://127.0.0.1:4181/admin/`.

## Production image

The control-plane Dockerfile reads this package through the named `admin_ui`
build context. Build through Compose from the repository root:

```bash
docker compose build control-plane
```

For direct Buildx use, pass both contexts explicitly. A bare `docker build` is
not sufficient:

```bash
docker buildx build \
  --build-context admin_ui=./client/admin \
  -f services/control-plane/Dockerfile \
  services/control-plane
```

The Node build stage copies only `dist/` into the final Alpine image. Node and
npm are not present at runtime.

## Current scope

The portal supports enrollment, password-plus-WebAuthn login, logout, and the
read-only health and metrics workspaces. It has no UI yet for creating later admins or
adding backup credentials.
