# marketplace

Handles the listings and bidding domain for the CoverOnes platform — posting jobs or tenders, submitting bids, and awarding contracts to successful bidders.

## What it does

- Lets KYC-verified users browse and search job/tender listings
- Lets verified freelancers submit bids on open listings
- Lets listing owners accept or reject bids; accepting a bid triggers contract creation downstream
- Enforces KYC tier gates: browse requires Tier 1, publishing and bidding require Tier 2
- Publishes domain events (bid placed, bid accepted, etc.) via Redis for downstream consumers
- Calls the workspace service after a bid is accepted to create the resulting contract

## Where it sits

marketplace is the entry point for the jobs/bidding lifecycle. It sits behind the API gateway and has no direct browser exposure. When a bid is accepted, it calls workspace via an internal S2S endpoint to hand off contract creation.

```
browser → gateway → marketplace
                  ↘ workspace (S2S, on AcceptBid)
```

## API (high level)

All routes live under `/v1`. Full request/response contract: [`conventions/http-api.md`](../conventions/http-api.md).

| Group | Endpoints | Min tier |
|-------|-----------|----------|
| Health | `GET /healthz`, `GET /readyz` | public |
| Listings | `GET /listings`, `GET /listings/search`, `GET /listings/:id` | 1 |
| Listings (write) | `POST /listings`, `PATCH /listings/:id` | 2 |
| Bids | `GET /listings/:id/bids`, `GET /bids` | 2 |
| Bids (write) | `POST /listings/:id/bids`, `POST /bids/:id/accept`, `POST /bids/:id/reject`, `POST /bids/:id/withdraw` | 2 |

## Tech

| Item | Detail |
|------|--------|
| Language | Go 1.25 |
| Framework | Gin |
| Database | PostgreSQL (pgx v5) |
| Cache / events | Redis (optional; falls back gracefully) |
| Migrations | golang-migrate, embedded SQL |

## Project structure

```
cmd/server/      — entrypoint; wires config, pool, services, router
internal/
  config/        — env-based config loading
  domain/        — core types (Listing, Bid, Award)
  service/       — business logic
  store/postgres — SQL queries and transaction manager
  handler/       — HTTP handlers and router
  platform/      — shared middleware, health, logger
  client/        — S2S client for workspace
  events/        — Redis publisher and noop fallback
migrations/      — versioned SQL migration files
```

## Run locally

```sh
cd ../dev-stack && docker compose up -d
```

See [`dev-stack/README.md`](../dev-stack/README.md) for full setup instructions.

## Environment variables

| Variable | Purpose |
|----------|---------|
| `MARKETPLACE_PORT` | HTTP listen port (default 8081) |
| `MARKETPLACE_POSTGRES_DSN` | PostgreSQL connection string |
| `MARKETPLACE_DB_SCHEMA` | Postgres schema for multi-tenant DB sharing |
| `MARKETPLACE_REDIS_URL` | Redis URL (optional) |
| `MARKETPLACE_WORKSPACE_BASE_URL` | Base URL of the workspace service for S2S calls |
| `MARKETPLACE_WORKSPACE_SERVICE_TOKEN` | Token used for authenticating S2S calls to workspace |
| `MARKETPLACE_GATEWAY_HMAC_SECRET` | Shared secret for verifying gateway-origin requests |
| `MARKETPLACE_LOG_LEVEL` | Log level (debug / info / warn / error) |
