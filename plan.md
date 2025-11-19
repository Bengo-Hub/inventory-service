## Inventory Service Delivery Plan

### 1. Vision & Objectives
- Provide a unified, multi-tenant inventory backbone for all domains (cafe, POS, ecommerce, logistics) with real-time stock visibility keyed by a shared `tenant_slug` and outlet/warehouse registry consumed by every Go microservice.
- Harmonise purchasing, warehouse operations, and fulfilment so downstream services (treasury, notifications, logistics, cafe-backend) consume accurate product and availability data.
- Support complex scenarios: multi-warehouse cafes/restaurants, retail stores with weigh-scale items, commissary kitchens, ecommerce fulfilment centres, dark stores, and vendor drop-ship.
- Embed subscription/licensing controls (from auth/treasury) to expose tiered features (advanced planning, forecasting, multi-warehouse) per tenant.
- **Entity Ownership**: This service owns all catalog and inventory entities: items/SKUs, variants, categories, BOMs, suppliers, warehouses, inventory balances, purchase orders, transfer orders, reservations, and lot/batch tracking. POS and Cafe services sync catalog items as read-only cache but never duplicate item master data. All stock queries go through inventory-service APIs. See `docs/cross-service-entity-ownership.md` for complete ownership matrix.

### 2. Technical Foundations
- **Language & Runtime:** Go 1.22+, modules, idiomatic gofmt, golangci-lint.
- **Storage:** PostgreSQL (primary system of record), Redis for caching, rate limiting, reservation queues.
- **ORM & Migrations:** Ent (schema-as-code), migrations via `ent/migrate` executed by Go CLI tools.
- **API Transport:** chi router (REST), ConnectRPC (optional) for gRPC streaming (stock feeds). Generate OpenAPI/Swagger docs.
- **Eventing:** Outbox pattern -> NATS JetStream or Kafka for stock adjustments, PO status, replenishment events.
- **Configuration:** envconfig; secrets via Vault/Secrets Manager. Docker multi-stage builds, Helm charts, ArgoCD.
- **Testing & Observability:** Go test + Testcontainers, k6, Prometheus metrics, zap structured logs, OpenTelemetry tracing.
- **Auth-Service SSO Integration:** ✅ **COMPLETED** - Integrated `shared/auth-client` v0.1.0 library for production-ready JWT validation using JWKS from auth-service. All protected `/v1/{tenantID}` routes require valid Bearer tokens. Swagger documentation updated with BearerAuth security definition. **Deployment:** Uses monorepo `replace` directives with versioned dependency (`v0.1.0`). Go workspace (`go.work`) handles local development automatically. Each service has independent DevOps workflows and can be deployed separately while sharing the auth library. See `shared/auth-client/DEPLOYMENT.md` and `shared/auth-client/TAGGING.md` for details.

### 3. Core Domain Modules
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

### 4. External Integrations
- **POS Service:** Bi-directional sync of catalog, real-time sales consumption, stock warnings, offline queue reconciliation.
- **Food Delivery Backend:** Menu availability, loyalty bundle ingredient depletion, customer app stock status.
- **Logistics Service:** Fulfilment assignment, pick-pack confirmation, last-mile updates, cross-docking. Provide zone/branch-aware availability to prefer nearest stock and avoid inter-zone shipments when local stock exists.
- **Treasury Service:** Cost accounting, invoice matching, subscription entitlement enforcement.
- **Notifications Service:** Alerts for low stock, PO approvals, expiry warnings, cycle count assignments.
- **Third-party WMS/ERP:** Connectors via webhooks/REST/EDI (future). Provide integration settings per tenant.
- **Supplier APIs:** Automated PO submission, ASN ingestion.
- **Auth Service:** Issues SSO tokens and tenant/outlet discovery callbacks to hydrate metadata before inventory events are processed.
- **Ecommerce & Marketplaces:** Storefront inventory updates, order reservations and cancellations, returns and RMA flows.
- **Barcodes/IoT:** Scale/barcode scanners, temperature/humidity sensors for compliance (telemetry fed to inventory holds/alerts).

### 5. Data Model Highlights
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
- `rma_requests`, `rma_items`, `rma_events`.
- `consignment_stocks`, `vmi_agreements`.
- `landed_cost_allocations`, `shipment_receipts`, `freight_charges`.
- `quality_inspections`, `inspection_results`, `quarantine_holds`.
- `cycle_count_policies`, `abc_segments`.
- `slotting_rules`, `bin_slot_scores`.
- `channel_listings`, `channel_orders`.
- `pricing_rules`, `price_lists`.
- `inventory_policies` (zone/branch dispatch preferences and substitution rules).
- `provider_credentials` (encrypted at rest), `provider_configs` (per-tenant/provider settings for external systems: WMS/ERP, ecommerce, supplier EDI, IoT).

### 6. System Architecture
- **Clean architecture** with domain services, repository interfaces, adapter layer (Postgres/Redis, external services).
- **Cache strategy:** Redis for frequently requested item/warehouse snapshots, reservation tokens, job locks.
- **Event dispatcher:** Outbox table flushes to JetStream with idempotent consumers in logistics/POS/notifications, coupled with webhook callbacks for tenant/outlet discovery and stock events (no polling endpoints).
- **Admin UI (future):** GraphQL proxy or REST to power management console.

### 7. API Strategy
- RESTful endpoints: `/v1/{tenant}/inventory/items`, `/transfers`, `/reservations`, etc.
- Bulk endpoints for CSV upload/download, asynchronous jobs with status tracking.
- Webhooks: `inventory.low_stock`, `inventory.po.approved`, `inventory.transfer.completed`, `inventory.recall.initiated`.
- Streaming endpoints (gRPC/SSE) for high-frequency stock updates to POS kiosks or dashboards.
- Zone/dispatch collaboration:
  - `/v1/{tenant}/inventory/availability?zone={zone}&branch={branch}&sku={sku}` → nearest in-stock locations with priorities/ETAs for logistics.
  - `/v1/{tenant}/policies/dispatch-preferences` → zone/branch priority, substitution, inter-zone shipment rules.
- Returns & cost:
  - `/v1/{tenant}/rma` (initiate/approve/receive), `/v1/{tenant}/quality/inspections`, `/v1/{tenant}/landed-costs` (create/allocate).
- Channels:
  - `/v1/{tenant}/channels/{provider}` (Shopify/Amazon, etc.) for catalog/orders/fulfilment sync.
- Provider configuration & secrets APIs:
  - `/v1/{tenant}/integrations/providers` (list/configure enabled providers: WMS/ERP, Ecommerce, Supplier EDI, IoT)
  - `/v1/{tenant}/integrations/providers/{provider}/config` (GET/PUT) with server-side encryption and redaction on reads
  - `/v1/{tenant}/integrations/events` (ingest/monitor integration failures), `/v1/{tenant}/exports/schedules` (data feeds)

### 8. Cross-Cutting Concerns
- **Security:** JWT validation via auth-service; enforce scope/audience permissions per route; RBAC queries to local table. Webhook signatures are verified before processing inventory events.
- **Multi-tenancy:** Row-level `tenant_id`, optional schema separation for enterprise tenants; warehouse/outlet identifiers mirror the canonical registry shared with cafe-backend, logistics, and POS to eliminate duplication. Tenant/outlet discovery webhooks ensure new metadata is synced automatically.
- **Resilience:** Saga patterns for long-running operations (PO -> receipt -> invoice). Retry/backoff for external integrations.
- **Audit & Compliance:** Append-only ledgers, immutable logs (hash chain) optional, digital signing for regulated adjustments.
- **Localization:** Unit conversions, currency support (cost/pricing), multi-language item descriptions.
- **Performance:** Partition large tables (ledger) by date/tenant; caching; background aggregation jobs.
- **Configuration & Secrets Management:** All third-party provider settings are configurable per tenant and encrypted at rest (envelope encryption/KMS-backed). Secrets are redacted in API responses, rotated on schedule, and access is audited. Config precedence: environment defaults → tenant-level DB overrides → feature flags.

### 9. Delivery Roadmap (Production-Ready Sprints with Checklists)
1. **Sprint 0 – Foundations**
   - [ ] Project scaffold, configuration loader, structured logging, health/liveness endpoints.
   - [ ] Ent base schemas: tenants, warehouses, items, variants; migration tooling.
   - [ ] Auth-service integration (JWT middleware, scopes), service-level RBAC seed roles.
   - [ ] CI/CD, container images, Helm chart + HPA/VPA defaults; secrets management.
2. **Sprint 1 – Master Data**
   - [ ] CRUD: items, variants, suppliers, warehouses/locations; seed + validation.
   - [ ] BOM/recipe support; UOM conversions; barcode/PLU mapping.
   - [ ] Import/export (CSV) with async jobs and audit trails.
3. **Sprint 2 – Balances & Ledger**
   - [ ] Real-time OH/ATP, adjustments API; append-only ledger with audit signatures.
   - [ ] Snapshot jobs, valuation (FIFO/WAvg), baseline reports.
   - [ ] Prometheus metrics for stock events; dashboards in Grafana.
4. **Sprint 3 – Purchasing & Replenishment**
   - [ ] POs, receipts, returns; vendor lead times; min/max rules.
   - [ ] Supplier API hooks; ASN ingestion; exception workflows.
   - [ ] Notifications for approvals/overdues; retry/backoff policies.
5. **Sprint 4 – Sales Consumption & Reservations**
   - [ ] POS/delivery consumption webhooks; reservation & ATP logic.
   - [ ] Substitution/backorders; urgent override approvals.
   - [ ] HPA tuned with custom metrics (orders/sec, reservation latency).
6. **Sprint 5 – Transfers & Movements**
   - [ ] Inter-warehouse transfers; put-away/pick/pack/stage; cycle counts.
   - [ ] Mobile-friendly APIs; batch/wave picking; exception handling.
   - [ ] KEDA triggers for worker queues; VPA recommendations applied.
7. **Sprint 6 – Lot/Expiry & Compliance**
   - [ ] Lot/batch tracking, expiry holds; recall workflows.
   - [ ] Temperature/IoT telemetry ties to holds & alerts; HACCP reporting.
   - [ ] GDPR/PII minimization, data exports/deletes.
8. **Sprint 7 – Integration Layer & Provider Configs**
   - [ ] Tenant-level provider registry (WMS/ERP, Ecommerce, EDI, IoT) with encrypted secrets.
   - [ ] `/integrations/providers` API; redaction on reads; rotation job.
   - [ ] Data warehouse export jobs; webhook retries and DLQ.
9. **Sprint 8 – Analytics & Hardening**
   - [ ] Dashboards: turns, fill rates, shrinkage; SLA alerting.
   - [ ] Load tests, HA review, backup/restore runbooks.
   - [ ] Threat model updates; posture & performance tuning.
10. **Sprint 9 – Subscription Enforcement**
    - [ ] Entitlements from treasury/auth; usage meters (locations, API calls).
    - [ ] Billing event emission; overage alerts; admin UX.
    - [ ] Feature gating per tenant; auditability.
11. **Sprint 10 – Launch & Support**
    - [ ] On-call runbooks, SLOs/alerts; documentation.
    - [ ] Tenant onboarding playbooks; migration/backfill scripts.
    - [ ] Post-launch polish & backlog triage.
12. **Sprint 11 – Landed Cost & RMA**
   - [ ] Landed cost capture and allocation across receipts; apportionment rules.
   - [ ] RMA workflows (create/approve/receive/repair); quarantine/QC integration.
13. **Sprint 12 – Consignment & VMI**
   - [ ] Consignment stock tracking and ownership transfers.
   - [ ] Vendor-managed inventory agreements and replenishment triggers.
14. **Sprint 13 – Channels & Marketplace**
   - [ ] Shopify/Amazon connectors (catalog/orders/fulfilment); mapping and sync jobs.
   - [ ] Channel allocation rules and failure dashboards.
15. **Sprint 14 – ABC, Cycle & Slotting**
   - [ ] ABC analysis and cycle count policy scheduling.
   - [ ] Slotting rules and bin scoring; putaway/picking optimization APIs.
16. **Sprint 15 – Zone & Dispatch Collaboration**
   - [ ] Availability endpoint with zone/branch policies; dispatch-preferences admin.
   - [ ] Integration tests with `logistics-service` to prefer nearest branch and avoid inter-zone shipments when local stock exists.

### 10. Backlog & Future Enhancements
- Demand forecasting, AI-assisted replenishment, labour planning.
- Radio-frequency (RF) scanning workflows, mobile pick/pack apps.
- Vendor-managed inventory (VMI), consignment stock.
- Integration marketplace (ERP/WMS connectors), EDI support.
- Sustainability metrics (food waste tracking, carbon reporting).

### 11. Next Steps
- Align ERD with POS/logistics services, finalize API contracts for consumption/reservation flows.
- Establish integration contracts with treasury (billing), notifications (alerts), auth (SSO).
- Draft security threat model (token abuse, manipulation of stock), start Sprint 0 scaffolding post sign-off.

### 12. Glossary & Acronyms (Plain‑English Reference)
- ERP (Enterprise Resource Planning): Integrated software to manage core business processes (finance, supply chain, manufacturing).
- WMS (Warehouse Management System): Software for warehouse operations (receiving, putaway, picking, shipping).
- SKU (Stock Keeping Unit): Unique identifier for a product variant.
- FIFO / FEFO: First‑In‑First‑Out (oldest stock first); First‑Expired‑First‑Out (soonest expiry first).
- RMA (Return Merchandise Authorization): Process to handle returned goods for refund/replacement/repair.
- ASN (Advanced Shipping Notice): Pre‑shipment notice from supplier with details of an incoming delivery.
- BOM (Bill of Materials): List of components/ingredients required to build a product or recipe.
- ABC Analysis: Classifying inventory into A/B/C tiers by importance or turnover to prioritize control efforts.
- Landed Cost: Total cost of a product including purchase price, freight, duties, taxes, insurance, and handling.
- Consignment / VMI (Vendor‑Managed Inventory): Supplier retains ownership or manages replenishment at customer site.
- API / REST / gRPC / OpenAPI: Programmatic interfaces and protocols; see Logistics “Glossary” for definitions.
- Postgres / PostGIS, Redis, Kafka/NATS, Kubernetes/Helm/Argo CD, Prometheus/Grafana: Data, messaging, deployment, and observability technologies; see Logistics “Glossary” for concise descriptions.
