# Inventory Service – Entity Relationship Overview

The inventory service centralises item master, stock control, purchasing, and reservation data for all BengoBox channels (food delivery, POS, ecommerce, logistics).  
Schemas are defined via Ent and designed for strict multi-tenancy.

> **Conventions**
> - UUID primary keys.
> - `tenant_id` on every table unless noted.
> - Monetary fields use `NUMERIC(18,2)`, quantities use `NUMERIC(18,6)`.
> - Timestamps use `TIMESTAMPTZ`.

---

## Master Data

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `items` | `id`, `tenant_id`, `sku`, `name`, `description`, `default_uom`, `item_type`, `status`, `category_id`, `barcode`, `created_at`, `updated_at` | Canonical item catalogue (stocked, service, composite). |
| `item_categories` | `id`, `tenant_id`, `parent_id`, `name`, `code`, `description`, `display_order` | Hierarchical category tree. |
| `item_variants` | `id`, `item_id`, `variant_code`, `attributes_json`, `status`, `created_at` | Size/flavour/packaging variants. |
| `item_uoms` | `id`, `tenant_id`, `code`, `description`, `conversion_factor`, `base_uom_code`, `created_at` | Units of measure and conversions. |
| `item_boms` | `id`, `tenant_id`, `parent_item_id`, `version`, `status`, `effective_from`, `effective_to`, `yield_percent`, `metadata` | Bill of materials / recipes. |
| `item_bom_components` | `id`, `bom_id`, `component_item_id`, `quantity`, `uom_code`, `scrap_percent` | Components required per BOM. |
| `item_suppliers` | `id`, `item_id`, `supplier_id`, `lead_time_days`, `min_order_qty`, `cost`, `currency`, `is_primary` | Supplier sourcing data. |

## Organisational Structure

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `warehouses` | `id`, `tenant_id`, `tenant_slug`, `code`, `name`, `type`, `address_json`, `timezone`, `status`, `created_at`, `updated_at` | Warehouses, kitchens, dark stores, or retail outlets tied to the shared tenant/outlet registry. |
| `warehouse_zones` | `id`, `warehouse_id`, `code`, `name`, `zone_type`, `metadata` | Logical partitioning (ambient, chilled, staging). |
| `locations` | `id`, `warehouse_id`, `zone_id`, `barcode`, `capacity`, `temperature_range`, `is_active`, `metadata` | Storage bins/locations. |
| `warehouse_contacts` | `id`, `warehouse_id`, `contact_name`, `phone`, `email`, `role`, `metadata` | Operational contacts. |

## Inventory Balances & Ledger

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `inventory_balances` | `id`, `tenant_id`, `item_id`, `warehouse_id`, `location_id`, `on_hand`, `reserved`, `available`, `in_transit`, `damaged`, `as_of_date` | Rolling inventory totals. |
| `inventory_snapshots` | `id`, `tenant_id`, `warehouse_id`, `item_id`, `snapshot_date`, `on_hand`, `available`, `unit_cost` | Periodic snapshots for analytics (daily/hourly). |
| `inventory_ledger_entries` | `id`, `tenant_id`, `item_id`, `warehouse_id`, `location_id`, `quantity_delta`, `uom_code`, `unit_cost`, `reference_type`, `reference_id`, `source`, `reason_code`, `created_at`, `metadata` | Immutable movement log (receipts, adjustments, sales, wastage). |
| `lot_batches` | `id`, `tenant_id`, `item_id`, `lot_code`, `manufactured_at`, `expires_at`, `status`, `metadata` | Traceability records. |
| `serial_numbers` | `id`, `tenant_id`, `item_id`, `serial`, `status`, `received_at`, `dispatched_at`, `metadata` | Serialized asset tracking. |

## Purchasing & Replenishment

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `suppliers` | `id`, `tenant_id`, `name`, `code`, `contact_json`, `payment_terms`, `status`, `metadata`, `created_at`, `updated_at` | Supplier master data. |
| `purchase_orders` | `id`, `tenant_id`, `supplier_id`, `warehouse_id`, `po_number`, `status`, `currency`, `requested_by`, `ordered_at`, `expected_at`, `metadata` | Purchase order header. |
| `purchase_order_lines` | `id`, `purchase_order_id`, `item_id`, `ordered_qty`, `received_qty`, `unit_cost`, `uom_code`, `status`, `metadata` | Line items. |
| `asn_receipts` | `id`, `tenant_id`, `purchase_order_id`, `document_number`, `received_at`, `status`, `metadata` | Advance shipping notices & receipt documents. |
| `goods_receipts` | `id`, `tenant_id`, `warehouse_id`, `reference_type`, `reference_id`, `received_by`, `received_at`, `metadata` | Warehouse receipt transactions. |
| `goods_receipt_lines` | `id`, `goods_receipt_id`, `item_id`, `lot_batch_id`, `serial_id`, `received_qty`, `damaged_qty`, `uom_code` | Detailed receipt lines. |
| `replenishment_rules` | `id`, `tenant_id`, `item_id`, `warehouse_id`, `min_level`, `max_level`, `safety_stock`, `reorder_point`, `review_frequency`, `metadata` | Auto-replenishment settings. |
| `replenishment_suggestions` | `id`, `tenant_id`, `item_id`, `warehouse_id`, `suggested_qty`, `suggested_at`, `reason_code`, `metadata` | Generated proposals for planners. |

## Reservations & Fulfilment

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `inventory_reservations` | `id`, `tenant_id`, `item_id`, `warehouse_id`, `reservation_type`, `source_reference`, `reserved_qty`, `status`, `reserved_at`, `expires_at`, `metadata` | Soft/hard allocations for orders, POS tickets, meal kits. |
| `reservation_events` | `id`, `reservation_id`, `event_type`, `quantity_delta`, `occurred_at`, `actor`, `metadata` | Reservation lifecycle, used for audit. |
| `pick_waves` | `id`, `tenant_id`, `warehouse_id`, `wave_number`, `status`, `scheduled_at`, `released_at`, `completed_at`, `metadata` | Batched picking for high volume orders. |
| `pick_wave_assignments` | `id`, `pick_wave_id`, `task_id`, `picker_id`, `status`, `metadata` | Link to individual pick tasks (syncs with logistics). |
| `transfer_orders` | `id`, `tenant_id`, `source_warehouse_id`, `destination_warehouse_id`, `transfer_number`, `status`, `requested_at`, `shipped_at`, `received_at`, `metadata` | Inter-warehouse transfers. |
| `transfer_lines` | `id`, `transfer_order_id`, `item_id`, `requested_qty`, `shipped_qty`, `received_qty`, `uom_code`, `status` | Transfer details. |
| `transfer_events` | `id`, `transfer_order_id`, `event_type`, `payload`, `occurred_at`, `actor` | State changes (released, shipped, received). |

## Adjustments, Counts & Compliance

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `stock_adjustments` | `id`, `tenant_id`, `item_id`, `warehouse_id`, `location_id`, `adjustment_type`, `quantity_delta`, `reason_code`, `reference_type`, `reference_id`, `approved_by`, `approved_at`, `metadata` | Manual adjustments (waste, shrink, recount). |
| `cycle_counts` | `id`, `tenant_id`, `warehouse_id`, `count_number`, `status`, `scheduled_at`, `completed_at`, `counted_by`, `metadata` | Cycle count campaigns. |
| `cycle_count_results` | `id`, `cycle_count_id`, `item_id`, `expected_qty`, `counted_qty`, `variance_qty`, `variance_cost`, `variance_reason`, `metadata` | Variance tracking. |
| `compliance_events` | `id`, `tenant_id`, `event_type`, `item_id`, `lot_batch_id`, `warehouse_id`, `reported_at`, `status`, `metadata` | Recalls, expiry alerts, regulatory escalations. |
| `temperature_logs` | `id`, `tenant_id`, `warehouse_id`, `location_id`, `captured_at`, `temperature_celsius`, `device_id`, `status`, `metadata` | HACCP monitoring. |

## Integrations & Webhooks

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `integration_endpoints` | `id`, `tenant_id`, `service_code`, `config_json`, `status`, `last_synced_at`, `metadata` | Integration settings for POS, food delivery, ERP, supplier APIs. |
| `outbox_events` | `id`, `tenant_id`, `aggregate_type`, `aggregate_id`, `event_type`, `payload`, `status`, `attempts`, `last_attempt_at`, `created_at` | Reliable event dispatch to other services (orders, reservations, stock alerts). |
| `webhook_subscriptions` | `id`, `tenant_id`, `event_key`, `target_url`, `secret`, `status`, `last_delivery_status`, `retry_count` | Configurable outbound hooks for partner integrations. |
| `import_jobs` | `id`, `tenant_id`, `job_type`, `status`, `file_url`, `requested_by`, `started_at`, `completed_at`, `error_message` | Bulk upload pipeline for catalog/stock data. |
| `export_jobs` | `id`, `tenant_id`, `job_type`, `parameters`, `status`, `requested_by`, `result_url`, `completed_at` | Scheduled exports to analytics or finance. |
| `tenant_sync_events` | `id`, `tenant_id`, `tenant_slug`, `source_service`, `payload`, `synced_at`, `status` | Records inbound tenant/outlet discovery callbacks from auth/food-delivery/POS ensuring metadata is hydrated before inventory data is stored. |

## Analytics & Forecasting (Roadmap)

| Table | Key Columns | Description |
|-------|-------------|-------------|
| `demand_forecasts` | `id`, `tenant_id`, `item_id`, `warehouse_id`, `forecast_date`, `forecast_qty`, `confidence_interval`, `generated_at`, `model_version` | Statistical forecast results. |
| `replenishment_overrides` | `id`, `forecast_id`, `adjusted_qty`, `justification`, `approved_by`, `approved_at` | Planner overrides to recommendations. |

## Relationships & Interfaces

- `items` link to `inventory_balances`, `inventory_ledger_entries`, `item_boms`, and `item_suppliers`.
- `inventory_ledger_entries.reference_type` points to upstream documents (purchase orders, sales orders, transfer orders) using polymorphic references.
- `goods_receipts` and `transfer_orders` feed reservations and the ledger via transactions.
- `inventory_reservations` are consumed by food-delivery backend, POS, and logistics services to validate fulfilment eligibility using the same tenant/outlet identifiers.
- `outbox_events` emit domain events to:
  - Food delivery backend (`inventory.reservation.created`, `inventory.low_stock`).
  - POS service (catalog sync, stock movements).
  - Logistics service (transfer released, pick wave ready).
  - Notifications service (expiry alerts).
- Integration endpoints coordinate authentication secrets and sync windows with external providers; tenant/outlet discovery callbacks are processed before accepting domain payloads to guarantee referential integrity.

## Seed & Reference Data

- Sample SKUs, warehouses, suppliers, and replenishment rules seeded for Urban Café demo tenants.
- Default reason codes: `SHRINK`, `WASTE`, `RECOUNT`, `PROMO`, `KIT_BUILD`.

---

Update this ERD whenever Ent schemas change. Run `go generate ./internal/ent` and refresh downstream documentation to keep integrations aligned.

