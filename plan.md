## Inventory Service Delivery Plan

### Vision & Objectives
- Provide a unified, multi-tenant inventory backbone for all BengoBox domains (food delivery, POS, ecommerce, logistics) with real-time stock visibility keyed by a shared `tenant_slug` and outlet/warehouse registry consumed by every Go microservice.
- Harmonise purchasing, warehouse operations, and fulfilment so downstream services (treasury, notifications, logistics, delivery app) consume accurate product and availability data.
- Support complex scenarios: multi-warehouse cafes/restaurants, retail stores with weigh-scale items, commissary kitchens, ecommerce fulfilment centres, dark stores, and vendor drop-ship.
- Embed subscription/licensing controls (from auth/treasury) to expose tiered features (advanced planning, forecasting, multi-warehouse) per tenant.

### Technical Foundations
- **Language & Runtime:** Go 1.22+, modules, idiomatic gofmt, golangci-lint.
- **Storage:** PostgreSQL (primary system of record), Redis for caching, rate limiting, reservation queues.
- **ORM & Migrations:** Ent (schema-as-code), migrations via `ent/migrate` executed by Go CLI tools.
- **API Transport:** chi router (REST), ConnectRPC (optional) for gRPC streaming (stock feeds). Generate OpenAPI/Swagger docs.
- **Eventing:** Outbox pattern -> NATS JetStream or Kafka for stock adjustments, PO status, replenishment events.
- **Configuration:** envconfig; secrets via Vault/Secrets Manager. Docker multi-stage builds, Helm charts, ArgoCD.
- **Testing & Observability:** Go test + Testcontainers, k6, Prometheus metrics, zap structured logs, OpenTelemetry tracing.

### Core Domain Modules
1. **Master Data Management**
   - Items, variants, units of measure, packaging hierarchy, barcodes/PLUs.
   - BOM/recipe definitions for kitchens and manufacturing.
   - Supplier catalogue, price lists, lead times, vendor performance metrics.
2. **Warehouse & Location Management**
   - Tenants, warehouses, storage locations (zone/aisle/bin, temperature), stocking rules.
   - Outlet/warehouse hierarchy (central kitchen → cafés, dark store → delivery node).
   - Multi-company setup with shared inventory pools vs dedicated stock.
3. **Inventory Balances & Valuation**
   - Real-time On Hand (OH), Available to Promise (ATP), Reserved, In Transit, Damaged, On Order.
   - Costing methods (FIFO, Weighted Average). Support snapshot and historical valuations.
   - Stock ledger tables for auditable adjustments.
4. **Replenishment & Purchasing**
   - Purchase orders, quotes, requisitions, vendor returns.
   - Auto-replenishment rules, min/max, EOQ, safety stock, demand forecasting (future feature).
   - Drop-ship workflows, inter-warehouse transfer requests.
5. **Sales & Consumption Integration**
   - Consume POS/delivery order events to decrement stock.
   - Recipe/bom explosion for kitchen production and yield tracking.
   - Ecommerce fulfilment reservations, partial picks, substitution support.
6. **Inventory Movements**
   - Goods receipt, put-away, picking, packing, staging, shipment confirmation.
   - Internal transfers, cycle counts, adjustments, waste/spoilage logging, quality holds.
   - Wave/batch picking for high-volume operations.
7. **Reservations & Allocation**
   - Soft/hard allocations for sales orders, loyalty redemptions, meal kits.
   - Backorder management, substitution logic, urgent override approvals.
8. **Compliance & Audit**
   - Traceability (lot/batch tracking, expiry dates, serialised items).
   - Temperature logs, HACCP compliance, recall management.
   - FDA/FSMA, local regulatory reporting, digital signatures for adjustments.
9. **Subscriptions & Feature Gating**
   - Integrate with treasury/auth to enforce plan entitlements (warehouses count, forecasting module, API limits).
   - Overage tracking (API calls, inventory locations) and automated billing event emission.
10. **Reporting & Analytics**
    - Stock status dashboards, inventory turn, fill rates, shrinkage, margin impact.
    - PO/transfer/warehouse performance metrics.
    - Scheduled exports to data warehouse (CSV/S3/BigQuery).

### External Integrations
- **POS Service:** Bi-directional sync of catalog, real-time sales consumption, stock warnings, offline queue reconciliation.
- **Food Delivery Backend:** Menu availability, loyalty bundle ingredient depletion, customer app stock status.
- **Logistics Service:** Fulfilment assignment, pick-pack confirmation, last-mile updates, cross-docking.
- **Treasury Service:** Cost accounting, invoice matching, subscription entitlement enforcement.
- **Notifications Service:** Alerts for low stock, PO approvals, expiry warnings, cycle count assignments.
- **Third-party WMS/ERP:** Connectors via webhooks/REST/EDI (future). Provide integration settings per tenant.
- **Supplier APIs:** Automated PO submission, ASN ingestion.
- **Auth Service:** Issues SSO tokens and tenant/outlet discovery callbacks to hydrate metadata before inventory events are processed.

### Data Model Highlights
- `inventory_items`, `item_variants`, `item_boms`, `item_suppliers`.
- `warehouses`, `locations`, `warehouse_zones`.
- `inventory_balances`, `inventory_ledger_entries`, `inventory_snapshots`.
- `purchase_orders`, `purchase_order_lines`, `asn_receipts`, `supplier_invoices`.
- `inventory_transfers`, `transfer_lines`, `transfer_events`.
- `stock_adjustments`, `cycle_counts`, `cycle_count_results`.
- `lot_batches`, `item_expiry`, `serial_numbers`.
- `inventory_reservations`, `reservation_events`.
- `subscription_entitlements`, `inventory_usage_metrics` for plan enforcement.
- `integration_configs`, `webhook_subscriptions`.

### System Architecture
- **Clean architecture** with domain services, repository interfaces, adapter layer (Postgres/Redis, external services).
- **Cache strategy:** Redis for frequently requested item/warehouse snapshots, reservation tokens, job locks.
- **Event dispatcher:** Outbox table flushes to JetStream with idempotent consumers in logistics/POS/notifications, coupled with webhook callbacks for tenant/outlet discovery and stock events (no polling endpoints).
- **Admin UI (future):** GraphQL proxy or REST to power management console.

### API Strategy
- RESTful endpoints: `/v1/{tenant}/inventory/items`, `/transfers`, `/reservations`, etc.
- Bulk endpoints for CSV upload/download, asynchronous jobs with status tracking.
- Webhooks: `inventory.low_stock`, `inventory.po.approved`, `inventory.transfer.completed`, `inventory.recall.initiated`.
- Streaming endpoints (gRPC/SSE) for high-frequency stock updates to POS kiosks or dashboards.

### Cross-Cutting Concerns
- **Security:** JWT validation via auth-service; enforce scope/audience permissions per route; RBAC queries to local table. Webhook signatures are verified before processing inventory events.
- **Multi-tenancy:** Row-level `tenant_id`, optional schema separation for enterprise tenants; warehouse/outlet identifiers mirror the canonical registry shared with food-delivery, logistics, and POS to eliminate duplication. Tenant/outlet discovery webhooks ensure new metadata is synced automatically.
- **Resilience:** Saga patterns for long-running operations (PO -> receipt -> invoice). Retry/backoff for external integrations.
- **Audit & Compliance:** Append-only ledgers, immutable logs (hash chain) optional, digital signing for regulated adjustments.
- **Localization:** Unit conversions, currency support (cost/pricing), multi-language item descriptions.
- **Performance:** Partition large tables (ledger) by date/tenant; caching; background aggregation jobs.

### Delivery Roadmap (Indicative Sprints)
1. **Sprint 0 – Foundations**: project scaffold, config, logging, health endpoints, ent base schema, CI/CD.
2. **Sprint 1 – Master Data**: items, variants, suppliers, warehouses CRUD, initial migrations, seed data.
3. **Sprint 2 – Inventory Balances & Ledger**: on hand tracking, adjustments API, basic reports.
4. **Sprint 3 – Purchasing & Replenishment**: POs, receiving, vendor integration events.
5. **Sprint 4 – Sales Consumption & Reservations**: POS/delivery hooks, reservation service, ATP calculations.
6. **Sprint 5 – Transfers & Movements**: inter-warehouse transfers, cycle counts, stock take mobile flows.
7. **Sprint 6 – Lot/Expiry & Compliance**: lot tracking, recall workflows, audit exports.
8. **Sprint 7 – Integration Layer**: webhooks, supplier connectors stub, data warehouse exports.
9. **Sprint 8 – Analytics & Hardening**: dashboards, SLA monitoring, security/performance tuning.
10. **Sprint 9 – Subscription Enforcement**: integration with treasury/auth, usage metering, billing events.
11. **Sprint 10 – Launch & Support**: runbooks, monitoring, alerting, post-launch backlog grooming.

### Backlog & Future Enhancements
- Demand forecasting, AI-assisted replenishment, labour planning.
- Radio-frequency (RF) scanning workflows, mobile pick/pack apps.
- Vendor-managed inventory (VMI), consignment stock.
- Integration marketplace (ERP/WMS connectors), EDI support.
- Sustainability metrics (food waste tracking, carbon reporting).

### Next Steps
- Align ERD with POS/logistics services, finalize API contracts for consumption/reservation flows.
- Establish integration contracts with treasury (billing), notifications (alerts), auth (SSO).
- Draft security threat model (token abuse, manipulation of stock), start Sprint 0 scaffolding post sign-off.

