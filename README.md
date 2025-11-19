# Inventory Service

The Inventory Service provides a unified stock, purchasing, and reservation backbone for all BengoBox channels (food delivery, POS, ecommerce, logistics) keyed to the shared `tenant_slug`/outlet registry that spans every Go microservice.  
It exposes APIs and events for real-time availability, replenishment, and compliance tracking.

## Highlights

- Master data governance for items, variants, BOMs, suppliers.
- Multi-warehouse inventory balances with lot/serial tracking.
- Purchase orders, receipts, transfers, cycle counts, compliance events.
- Reservation engine powering order fulfilment, omnichannel click-and-collect, and POS consumption.
- Event-driven integrations with cafe-backend, POS service, logistics service, notifications, and treasury using signed webhook callbacks instead of polling.

## Stack

- Go 1.22+, Ent ORM, PostgreSQL, Redis.
- REST (chi) with OpenAPI specs, optional ConnectRPC/gRPC feeds.
- Outbox pattern to NATS/Kafka for cross-service propagation.
- Observability via zap logging, Prometheus metrics, OpenTelemetry.

## Getting Started

```shell
cp config/example.env .env
make deps
docker compose up -d postgres redis
go generate ./internal/ent
go run ./cmd/server
```

APIs default to `http://localhost:4102`. Configure via `INVENTORY_HTTP_PORT`.

## Repository Layout

- `cmd/` – service binaries (`server`, `migrate`, `seed`, `worker`).
- `internal/app` – bootstrapping and wiring.
- `internal/ent` – schema definitions and generated code.
- `internal/modules` – bounded contexts (masterdata, stock, purchasing, reservations, compliance).
- `docs/` – ERD, ADRs, integration contracts.

## Integrations

- **Food Delivery Backend:** inventory reservations, menu availability, substitution signals.
- **POS Service:** stock deduction events, price book sync, drawer alerts.
- **Logistics Service:** transfer orders, pick wave assignments, shipment confirmations.
- **Notifications Service:** low stock, expiry, compliance alerts.
- **Treasury:** valuation feeds, supplier invoice reconciliation, subscription entitlements.
- **Auth Service:** SSO tenant claims; tenant/outlet discovery callbacks hydrate inventory metadata automatically (no polling).

Refer to `plan.md` and `docs/erd.md` for detailed design.

## Project Status

- Planning/architecture phase; schema modelling underway.
- Track progress via `CHANGELOG.md`.

