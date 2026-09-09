package stock

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/inventory-service/internal/ent"
	entconsumption "github.com/bengobox/inventory-service/internal/ent/consumption"
	"github.com/bengobox/inventory-service/internal/ent/inventorybalance"
	entlot "github.com/bengobox/inventory-service/internal/ent/inventorylot"
	entinvuser "github.com/bengobox/inventory-service/internal/ent/inventoryuser"
	"github.com/bengobox/inventory-service/internal/ent/item"
	"github.com/bengobox/inventory-service/internal/ent/itemvariant"
	"github.com/bengobox/inventory-service/internal/ent/predicate"
	"github.com/bengobox/inventory-service/internal/ent/reservation"
	entschema "github.com/bengobox/inventory-service/internal/ent/schema"
	"github.com/bengobox/inventory-service/internal/ent/stockadjustment"
	"github.com/bengobox/inventory-service/internal/ent/stocklevelevent"
	enttenantcfg "github.com/bengobox/inventory-service/internal/ent/tenantinventoryconfig"
	"github.com/bengobox/inventory-service/internal/ent/warehouse"
	"github.com/bengobox/inventory-service/internal/modules/units"
	platformevents "github.com/bengobox/inventory-service/internal/platform/events"
)

// ReservationRequest matches the ordering-backend client DTO.
type ReservationRequest struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	OrderID     uuid.UUID `json:"order_id"`
	WarehouseID uuid.UUID `json:"warehouse_id,omitempty"`
	// OutletID scopes the reservation to the selling outlet's own warehouse when no explicit
	// WarehouseID is supplied (same rule as consumption) — an outlet must reserve against its own
	// stock, not the tenant-default warehouse. uuid.Nil = tenant-default fallback.
	OutletID       uuid.UUID         `json:"outlet_id,omitempty"`
	Items          []ReservationItem `json:"items"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// ReservationItem represents a single item to reserve (fractional-capable).
type ReservationItem struct {
	SKU      string  `json:"sku"`
	Quantity float64 `json:"quantity"`
	// Modifiers are selected modifier options on this line whose stock must also be
	// reserved (e.g. "Extra Cheese"). Mirrors the pos.sale.finalized modifiers contract
	// so ordering S2S reservations deduct modifier stock the same way POS sales do.
	Modifiers []ModifierLine `json:"modifiers,omitempty"`
}

// ModifierLine is a selected modifier option carried on a reservation/consumption line.
// The caller may send the inventory modifier-option id (preferred — inventory owns the
// option→SKU mapping) and/or a direct sku. Quantity is per single unit of the parent
// line and is scaled by the line quantity.
type ModifierLine struct {
	SKU                       string  `json:"sku,omitempty"`
	InventoryModifierOptionID string  `json:"inventory_modifier_option_id,omitempty"`
	Quantity                  float64 `json:"quantity,omitempty"`
}

// ReservationResponse matches the ordering-backend client DTO.
type ReservationResponse struct {
	ID          uuid.UUID      `json:"id"`
	TenantID    uuid.UUID      `json:"tenant_id"`
	OrderID     uuid.UUID      `json:"order_id"`
	Status      string         `json:"status"`
	Items       []ReservedItem `json:"items"`
	ExpiresAt   *time.Time     `json:"expires_at,omitempty"`
	ConfirmedAt *time.Time     `json:"confirmed_at,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// ReservedItem matches the ordering-backend client DTO.
type ReservedItem struct {
	SKU             string  `json:"sku"`
	RequestedQty    float64 `json:"requested_qty"`
	ReservedQty     float64 `json:"reserved_qty"`
	AvailableQty    float64 `json:"available_qty"`
	IsFullyReserved bool    `json:"is_fully_reserved"`
}

// ConsumptionRequest matches the ordering-backend client DTO.
type ConsumptionRequest struct {
	TenantID uuid.UUID `json:"tenant_id"`
	OrderID  uuid.UUID `json:"order_id"`
	// WarehouseID, when set, is the explicit warehouse the sale must deduct from and always wins.
	WarehouseID uuid.UUID `json:"warehouse_id,omitempty"`
	// OutletID scopes the deduction to the SELLING outlet's own warehouse when no explicit
	// WarehouseID is supplied. Critical for multi-outlet tenants: POS on_hand is outlet-scoped, so
	// a sale must deduct from the outlet's OWN warehouse — not the tenant-default (often the HQ /
	// hotel) warehouse, where the item usually has no balance and the deduction silently shortfalls
	// (root cause of "sold several but stock still shows full"). uuid.Nil = tenant-default fallback
	// (preserves the legacy behaviour for callers that carry no outlet context).
	OutletID       uuid.UUID         `json:"outlet_id,omitempty"`
	Items          []ConsumptionItem `json:"items"`
	Reason         string            `json:"reason,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	// OrderNumber/CustomerName/CustomerPhone/ServedByUserID/ServedByName denormalize the
	// triggering sale's own identity onto every ConsumptionLine this call writes, so the
	// stock-history ledger (see stock.ItemStockHistory) can show the real POS order number,
	// buyer and cashier without a cross-service call. All optional — S2S/ordering callers
	// with no POS order simply omit them.
	OrderNumber    string     `json:"order_number,omitempty"`
	CustomerName   string     `json:"customer_name,omitempty"`
	CustomerPhone  string     `json:"customer_phone,omitempty"`
	ServedByUserID *uuid.UUID `json:"served_by_user_id,omitempty"`
	ServedByName   string     `json:"served_by_name,omitempty"`
	// SaleDate, when set, is the date this consumption's ConsumptionLine rows are stamped
	// with (stock-history ledger "Date" column) — the triggering sale's own effective date
	// (business_date backdate when set, else CreatedAt), NOT necessarily when this request is
	// processed. Nil (S2S/ordering callers with no such concept, or an older pos-api build)
	// falls back to time.Now(), the pre-existing behavior. Deliberately does NOT affect
	// Consumption.ProcessedAt, which stays the real processing timestamp for ops/audit.
	SaleDate *time.Time `json:"sale_date,omitempty"`
}

// ConsumptionItem represents an item to consume.
type ConsumptionItem struct {
	SKU      string  `json:"sku"`
	Quantity float64 `json:"quantity"`
	// UOM optionally carries the unit the quantity is expressed in (sale-line uom_code).
	// When it differs from the item's stock unit it is converted before deduction —
	// including the content-per-unit bridge (a 30 ml pour of a bottle stocked in pieces
	// deducts 0.04 pieces). Empty = already in stock units (legacy behavior).
	UOM string `json:"uom,omitempty"`
	// Modifiers are selected modifier options whose stock must also be consumed.
	// Mirrors the pos.sale.finalized modifiers contract.
	Modifiers []ModifierLine `json:"modifiers,omitempty"`
}

// ConsumptionResponse matches the ordering-backend client DTO.
type ConsumptionResponse struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	OrderID     uuid.UUID `json:"order_id"`
	Status      string    `json:"status"`
	ProcessedAt time.Time `json:"processed_at"`
	// LotsConsumed reports which InventoryLot(s) each SKU was drawn from, when the tenant's
	// costing method is lot-ordered (fifo/lifo/fefo). Additive field — callers that don't
	// read it (ordering-backend) are unaffected. pos-api uses this to stamp POSOrderLine's
	// lot_number/expiry_date so the sold unit's batch is known on the receipt/regulator log.
	LotsConsumed []ConsumedLot `json:"lots_consumed,omitempty"`
}

// ConsumedLot is one lot's contribution to a single SKU's consumption within a sale.
type ConsumedLot struct {
	SKU        string     `json:"sku"`
	LotID      uuid.UUID  `json:"lot_id"`
	LotNumber  string     `json:"lot_number,omitempty"`
	ExpiryDate *time.Time `json:"expiry_date,omitempty"`
	Quantity   float64    `json:"quantity"`
}

// ConsumeReservationResponse reports which lot(s) a reservation's items were actually drawn
// from at consume time, mirroring ConsumptionResponse.LotsConsumed. Additive — callers that
// only checked the old bare-error return (ordering-backend, hotel checkout) are unaffected.
type ConsumeReservationResponse struct {
	Status       string        `json:"status"`
	LotsConsumed []ConsumedLot `json:"lots_consumed,omitempty"`
}

// Service handles stock reservation and consumption business logic.
type Service struct {
	client *ent.Client
	log    *zap.Logger
	// transferRecorder, when wired, lets BulkAdjustStock's destination-warehouse move give the
	// result a transfer_number/audit record via transfers.Service — see transfer_recorder.go.
	transferRecorder TransferRecorder
}

// NewService creates a new stock service.
func NewService(client *ent.Client, log *zap.Logger) *Service {
	return &Service{
		client: client,
		log:    log.Named("stock.service"),
	}
}

// AdjustStockRequest represents a stock adjustment request.
type AdjustStockRequest struct {
	SKU         string    `json:"sku"`
	Adjustment  float64   `json:"adjustment"`
	Reason      string    `json:"reason"`
	Reference   string    `json:"reference,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	AdjustedBy  uuid.UUID `json:"adjusted_by"`
	WarehouseID uuid.UUID `json:"warehouse_id,omitempty"`
	// OutletID is the operating outlet (from the X-Outlet-ID request context). When WarehouseID is
	// omitted the adjustment defaults to this outlet's own warehouse, not the tenant default — set
	// by the handler, not trusted from the request body.
	OutletID uuid.UUID  `json:"-"`
	UnitID   *uuid.UUID `json:"unit_id,omitempty"` // optional; when set, records the balance's unit of measure
	// ApprovalIntentID ties a large adjustment to its approval workflow: the
	// client passes a stable UUID on the first (blocked) attempt and again on the
	// retry after a manager approves.
	ApprovalIntentID *uuid.UUID `json:"approval_intent_id,omitempty"`
}

// AdjustStockResponse represents the result of a stock adjustment.
type AdjustStockResponse struct {
	ID         uuid.UUID `json:"id"`
	SKU        string    `json:"sku"`
	OnHand     float64   `json:"on_hand"`
	Available  float64   `json:"available"`
	Reserved   float64   `json:"reserved"`
	Reason     string    `json:"reason"`
	QtyBefore  float64   `json:"quantity_before"`
	QtyChange  float64   `json:"quantity_change"`
	QtyAfter   float64   `json:"quantity_after"`
	AdjustedAt time.Time `json:"adjusted_at"`
}

// StockAdjustmentDTO represents a stock adjustment for listing.
type StockAdjustmentDTO struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	ItemID         uuid.UUID `json:"item_id"`
	ItemName       string    `json:"item_name,omitempty"`
	WarehouseID    uuid.UUID `json:"warehouse_id"`
	WarehouseName  string    `json:"warehouse_name,omitempty"`
	QuantityBefore float64   `json:"quantity_before"`
	QuantityChange float64   `json:"quantity_change"`
	QuantityAfter  float64   `json:"quantity_after"`
	Reason         string    `json:"reason"`
	Reference      string    `json:"reference,omitempty"`
	Notes          string    `json:"notes,omitempty"`
	AdjustedBy     uuid.UUID `json:"adjusted_by"`
	AdjustedByName string    `json:"adjusted_by_name,omitempty"`
	AdjustedAt     time.Time `json:"adjusted_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListAdjustmentsRequest contains filters for listing stock adjustments.
type ListAdjustmentsRequest struct {
	ItemID      uuid.UUID `json:"item_id,omitempty"`
	WarehouseID uuid.UUID `json:"warehouse_id,omitempty"`
	// WarehouseIDs restricts to a set of warehouses (outlet drill-down: the selected outlet's
	// warehouses + shared ones). Ignored when the explicit WarehouseID is set.
	WarehouseIDs []uuid.UUID `json:"warehouse_ids,omitempty"`
	Reason       string      `json:"reason,omitempty"`
	DateFrom     time.Time   `json:"date_from,omitempty"`
	DateTo       time.Time   `json:"date_to,omitempty"`
	// Search matches against the adjustment's own reference/batch number plus the linked item's
	// and warehouse's names (resolved via a name lookup, since StockAdjustment stores only their
	// IDs) — mirrors what the Adjustments page's search box promises ("item, reason, or
	// warehouse"), now applied server-side instead of only over whatever page happened to load.
	Search string
	// Limit/Offset page through the full matching set (see pagination.Parse in the handler).
	// Zero Limit falls back to the pre-pagination default of 200 for any caller that doesn't set it.
	Limit  int
	Offset int
}

// AdjustStock adjusts stock levels for an item, creates an audit trail, and publishes events.
func (s *Service) AdjustStock(ctx context.Context, tenantID uuid.UUID, req AdjustStockRequest) (*AdjustStockResponse, error) {
	whID, err := s.resolveWarehouseIDForOutlet(ctx, tenantID, req.WarehouseID, req.OutletID)
	if err != nil {
		return nil, err
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	itm, err := tx.Item.Query().
		Where(item.TenantID(tenantID), item.Sku(req.SKU)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: item not found: sku=%s: %w", req.SKU, err)
	}
	// SERVICE items (bookable rooms/facilities/conference slots, fees) represent capacity or a
	// charge, not physical stock — they must never be adjustable here. Seeing one get a real
	// balance row that then depletes like normal stock is a live-observed bug (a co-working-space
	// SERVICE item at urban-loft had actually decremented 100->89 as if every booking sold a unit).
	if itm.Type == item.TypeSERVICE {
		return nil, fmt.Errorf("stock: %q is a SERVICE item (capacity/booking, not stock) and cannot be stock-adjusted", req.SKU)
	}

	bal, err := tx.InventoryBalance.Query().
		Where(
			inventorybalance.TenantID(tenantID),
			inventorybalance.ItemID(itm.ID),
			inventorybalance.WarehouseID(whID),
		).
		First(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return nil, fmt.Errorf("stock: query balance: %w", err)
		}
		// No balance row exists yet for this item in this warehouse. Rather than failing the
		// adjustment (the common case for Initial Stock Count / Found on a fresh item),
		// initialize a zero balance and apply the adjustment against it.
		bal, err = tx.InventoryBalance.Create().
			SetTenantID(tenantID).
			SetItemID(itm.ID).
			SetWarehouseID(whID).
			Save(ctx)
		if err != nil {
			return nil, fmt.Errorf("stock: init balance for sku=%s: %w", req.SKU, err)
		}
	}

	// Read-compute-write with an optimistic-concurrency guard, retried on conflict: a plain
	// read-then-write here (read bal, compute newOnHand in Go, write it back) would race a
	// concurrent adjustment/consumption of the SAME balance row — both read the same stale
	// on_hand, both compute off it, and the second write silently clobbers the first's change
	// instead of compounding it, corrupting both the balance AND this function's own
	// quantity_before/quantity_change/quantity_after audit trail (which must reflect what was
	// REALLY there at write time). The `.Where(OnHand(...), Available(...))` guard makes the
	// final write affect 0 rows (ent surfaces this as NotFound) if either value moved between
	// our read and write; on that signal we re-read the fresh row and recompute from scratch
	// rather than proceed on data we know is stale.
	var qtyBefore, qtyChange, qtyAfter, newOnHand, newAvailable float64
	var updatedBal *ent.InventoryBalance
	const maxAdjustRetries = 5
	for attempt := 0; ; attempt++ {
		qtyBefore = bal.OnHand
		qtyChange = req.Adjustment

		// A manual adjustment applies the literal delta a manager enters, whatever sign the
		// result lands on. Floor-at-zero here used to silently destroy a real oversell debt
		// instead of settling it — a restock adjustment after an oversold sale must be able to
		// bring on_hand from e.g. -3 up to -1, not reset it to a fresh positive number. See
		// [[oversell-negative-stock-settlement]].
		newOnHand = round4(bal.OnHand + req.Adjustment)
		newAvailable = round4(bal.Available + req.Adjustment)

		qtyAfter = newOnHand

		// Reject a result that leaves a discrete/count-based item (e.g. a phone stocked in PIECE)
		// with a fractional balance — checked on the RESULT, not the raw delta, so a corrective
		// adjustment that fixes an already-fractional balance back to a whole number (e.g. -0.67 to
		// bring 4427.67 down to 4427) is allowed, while a delta that would newly introduce or worsen
		// a fraction is rejected. Covers this endpoint's manual "Record Adjustment" callers AND the
		// bulk-import InitialStock sheet, which posts opening quantities through here too.
		if itm.UnitID != nil {
			if u, uErr := tx.Unit.Get(ctx, *itm.UnitID); uErr == nil {
				if vErr := units.ValidateQuantityForUnit(qtyAfter, u.Type, u.Name, itm.UnitContentQty != nil); vErr != nil {
					err = fmt.Errorf("stock: %w (sku=%s)", vErr, req.SKU)
					return nil, err
				}
			}
		}

		balUpdate := tx.InventoryBalance.UpdateOne(bal).
			SetOnHand(newOnHand).
			SetAvailable(newAvailable).
			Where(inventorybalance.OnHand(bal.OnHand), inventorybalance.Available(bal.Available))
		// A positive result means the item is genuinely present here again — clear any prior
		// "removed from this location" marker (set by a transfer shipping the last unit out)
		// regardless of how the restock happened (manual correction, purchase, stock take, opening
		// balance all route through here). Never SET the flag from this function on a decrement to
		// zero — that's an organic stock-out/correction, which must keep showing for reordering.
		if newOnHand > 0 && bal.RemovedFromLocation {
			balUpdate = balUpdate.SetRemovedFromLocation(false)
		}
		// Record the unit of measure when the caller specifies one (defaults to the
		// existing balance UoM / item base unit when omitted).
		if req.UnitID != nil {
			if u, uErr := tx.Unit.Get(ctx, *req.UnitID); uErr == nil && u.Name != "" {
				balUpdate = balUpdate.SetUnitOfMeasure(u.Name)
			}
		}

		// Reuse the function-scoped err (not a fresh local) — the deferred rollback above checks
		// this exact variable, so every error path here must assign to it, not a shadow.
		updatedBal, err = balUpdate.Save(ctx)
		if err == nil {
			break
		}
		if !ent.IsNotFound(err) {
			return nil, fmt.Errorf("stock: update balance for sku=%s: %w", req.SKU, err)
		}
		if attempt >= maxAdjustRetries {
			err = fmt.Errorf("stock: update balance for sku=%s: concurrent adjustment conflict, exhausted %d retries", req.SKU, maxAdjustRetries)
			return nil, err
		}
		bal, err = tx.InventoryBalance.Query().
			Where(
				inventorybalance.TenantID(tenantID),
				inventorybalance.ItemID(itm.ID),
				inventorybalance.WarehouseID(whID),
			).
			Only(ctx)
		if err != nil {
			return nil, fmt.Errorf("stock: re-read balance for sku=%s: %w", req.SKU, err)
		}
	}

	// Validate reason for the enum
	adjReason := stockadjustment.Reason(req.Reason)
	if err := stockadjustment.ReasonValidator(adjReason); err != nil {
		adjReason = stockadjustment.ReasonOther
	}

	// Create StockAdjustment audit record
	now := time.Now()
	adjBuilder := tx.StockAdjustment.Create().
		SetTenantID(tenantID).
		SetItemID(itm.ID).
		SetWarehouseID(whID).
		SetQuantityBefore(qtyBefore).
		SetQuantityChange(qtyChange).
		SetQuantityAfter(qtyAfter).
		SetReason(adjReason).
		SetAdjustedAt(now)

	if req.AdjustedBy != uuid.Nil {
		adjBuilder.SetAdjustedBy(req.AdjustedBy)
	} else {
		adjBuilder.SetAdjustedBy(uuid.Nil)
	}
	if req.Reference != "" {
		adjBuilder.SetReference(req.Reference)
	}
	if req.Notes != "" {
		adjBuilder.SetNotes(req.Notes)
	}

	adj, err := adjBuilder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: create adjustment record: %w", err)
	}

	// Publish stock updated event
	s.writeOutboxEvent(ctx, tx, tenantID, itm.ID, "inventory", "stock.updated", map[string]any{
		"tenant_id":       tenantID.String(),
		"item_id":         itm.ID.String(),
		"sku":             itm.Sku,
		"warehouse_id":    whID.String(),
		"adjustment_id":   adj.ID.String(),
		"quantity_before": qtyBefore,
		"quantity_change": qtyChange,
		"quantity_after":  qtyAfter,
		"reason":          req.Reason,
		"on_hand":         newOnHand,
		"available":       newAvailable,
	})

	// Any downward adjustment draws down cost layers too (same mechanism a sale uses), so a
	// write-off/shrinkage/damage removes the SPECIFIC stock's own cost from the books instead of
	// the item's current flat cost, and the layer totals don't drift away from the balance
	// they're supposed to mirror.
	var adjLots []LotConsumption
	if qtyChange < 0 {
		adjLots = s.consumeLots(ctx, tx, tenantID, itm.ID, whID, -qtyChange, s.costingMethod(ctx, tenantID))
	}

	// GL-postable adjustments additionally publish a VALUED inventory.stock.adjusted event so
	// treasury records the corresponding journal entry:
	//  - Expense-bearing downward reasons (floor-stock issue of consumables, damage, expiry,
	//    shrinkage) -> operating-expense/wastage entry, as before.
	//  - Everything else GL-postable (opening_balance, initial_count, correction, count_variance,
	//    found, other) -> a value-movement entry (Opening Balance Equity for the two onboarding
	//    reasons, Wastage & Shrinkage credited/debited by direction for the rest) -- without this,
	//    corrections and opening balances change on-hand quantity but never reach the books, so
	//    the Inventory (1500) GL balance permanently drifts from what's physically on the shelf.
	// Downward adjustments draw their value from the SAME cost layers just consumed above (matches
	// the sale-time costing method); upward adjustments have no lot to draw from (stock is being
	// added, not removed) so they value at the item's current cost price.
	if qtyChange != 0 && glPostableReason(adjReason) {
		var costValue float64
		if qtyChange < 0 {
			var layeredValue, layeredQty float64
			for _, lot := range adjLots {
				if lot.UnitCost != nil {
					layeredValue += lot.QtyTaken * *lot.UnitCost
					layeredQty += lot.QtyTaken
				}
			}
			remainder := -qtyChange - layeredQty
			if remainder > 0 && itm.CostPrice != nil {
				layeredValue += remainder * *itm.CostPrice
			}
			costValue = round4(layeredValue)
		} else if itm.CostPrice != nil {
			costValue = round4(qtyChange * *itm.CostPrice)
		}
		// Nothing valued to post (item has no cost basis at all) -- skip rather than post a
		// meaningless zero-amount journal entry.
		if costValue > 0.009 {
			uom := ""
			if itm.UnitID != nil {
				if u, uErr := tx.Unit.Get(ctx, *itm.UnitID); uErr == nil {
					uom = u.Abbreviation
				}
			}
			direction := "increase"
			if qtyChange < 0 {
				direction = "decrease"
			}
			s.writeOutboxEvent(ctx, tx, tenantID, adj.ID, "inventory", "stock.adjusted", map[string]any{
				"tenant_id":     tenantID.String(),
				"adjustment_id": adj.ID.String(),
				"item_id":       itm.ID.String(),
				"sku":           itm.Sku,
				"item_name":     itm.Name,
				"warehouse_id":  whID.String(),
				"reason":        string(adjReason),
				"direction":     direction,
				"quantity":      math.Abs(qtyChange),
				"uom":           uom,
				"cost_value":    costValue,
				"reference":     req.Reference,
				"notes":         req.Notes,
				"adjusted_at":   now.UTC().Format(time.RFC3339),
			})
		}
	}

	// Check for low stock and publish event
	s.checkAndPublishLowStock(ctx, tx, tenantID, itm, updatedBal, whID)

	// If a positive correction lifted this ingredient back above zero, re-enable any
	// recipes its depletion had gated. The goods-receipt path already cascades a
	// restock (line ~1081); a corrective upward adjustment must do the same, or
	// recipes disabled by the stock-out cascade would stay hidden until a receipt.
	if qtyBefore <= 0 && qtyAfter > 0 {
		s.cascadeIngredientRestocked(ctx, tx, tenantID, itm.ID, whID)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("stock: commit adjustment: %w", err)
	}

	s.log.Info("stock adjusted",
		zap.String("sku", req.SKU),
		zap.Float64("adjustment", req.Adjustment),
		zap.String("reason", req.Reason),
		zap.Float64("new_on_hand", newOnHand),
		zap.String("adjustment_id", adj.ID.String()),
	)

	return &AdjustStockResponse{
		ID:         adj.ID,
		SKU:        req.SKU,
		OnHand:     newOnHand,
		Available:  newAvailable,
		Reserved:   bal.Reserved,
		Reason:     req.Reason,
		QtyBefore:  qtyBefore,
		QtyChange:  qtyChange,
		QtyAfter:   qtyAfter,
		AdjustedAt: now,
	}, nil
}

// ListAdjustments returns stock adjustments filtered by the given criteria, along with the true
// total matching count (independent of Limit/Offset) so a caller can page through the full set —
// previously this hard-capped at 200 rows with no way to reach anything older, silently making
// any adjustment past that cutoff invisible once a tenant had more than 200 on record.
func (s *Service) ListAdjustments(ctx context.Context, tenantID uuid.UUID, req ListAdjustmentsRequest) ([]StockAdjustmentDTO, int, error) {
	q := s.client.StockAdjustment.Query().
		Where(stockadjustment.TenantID(tenantID))

	if req.ItemID != uuid.Nil {
		q = q.Where(stockadjustment.ItemID(req.ItemID))
	}
	if req.WarehouseID != uuid.Nil {
		q = q.Where(stockadjustment.WarehouseID(req.WarehouseID))
	} else if len(req.WarehouseIDs) > 0 {
		q = q.Where(stockadjustment.WarehouseIDIn(req.WarehouseIDs...))
	}
	if req.Reason != "" {
		reason := stockadjustment.Reason(req.Reason)
		if stockadjustment.ReasonValidator(reason) == nil {
			q = q.Where(stockadjustment.ReasonEQ(reason))
		}
	}
	if !req.DateFrom.IsZero() {
		q = q.Where(stockadjustment.AdjustedAtGTE(req.DateFrom))
	}
	if !req.DateTo.IsZero() {
		q = q.Where(stockadjustment.AdjustedAtLTE(req.DateTo))
	}
	if search := strings.TrimSpace(req.Search); search != "" {
		// StockAdjustment only stores item_id/warehouse_id, not their names, so a name match has
		// to be resolved via a separate lookup first — same idea as TransferNumberContainsFold,
		// just one join short of being a native column search.
		searchPreds := []predicate.StockAdjustment{stockadjustment.ReferenceContainsFold(search)}
		if itemIDs, ierr := s.client.Item.Query().
			Where(item.TenantID(tenantID), item.NameContainsFold(search)).
			IDs(ctx); ierr == nil && len(itemIDs) > 0 {
			searchPreds = append(searchPreds, stockadjustment.ItemIDIn(itemIDs...))
		}
		if whIDs, werr := s.client.Warehouse.Query().
			Where(warehouse.TenantID(tenantID), warehouse.NameContainsFold(search)).
			IDs(ctx); werr == nil && len(whIDs) > 0 {
			searchPreds = append(searchPreds, stockadjustment.WarehouseIDIn(whIDs...))
		}
		q = q.Where(stockadjustment.Or(searchPreds...))
	}

	// Count the full matching set BEFORE Limit/Offset — this is the real total the frontend
	// pager needs; `len(page-of-results)` (the previous behavior) is only ever <= the page size.
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("stock: count adjustments: %w", err)
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 200 // pre-pagination default, kept for any caller that doesn't set Limit
	}

	adjustments, err := q.
		Order(ent.Desc(stockadjustment.FieldAdjustedAt)).
		Limit(limit).
		Offset(req.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("stock: list adjustments: %w", err)
	}

	// Collect unique item and warehouse IDs for batch lookup.
	itemIDSet := make(map[uuid.UUID]struct{})
	whIDSet := make(map[uuid.UUID]struct{})
	for _, a := range adjustments {
		itemIDSet[a.ItemID] = struct{}{}
		whIDSet[a.WarehouseID] = struct{}{}
	}

	itemIDs := make([]uuid.UUID, 0, len(itemIDSet))
	for id := range itemIDSet {
		itemIDs = append(itemIDs, id)
	}
	whIDs := make([]uuid.UUID, 0, len(whIDSet))
	for id := range whIDSet {
		whIDs = append(whIDs, id)
	}

	// Batch-fetch item names.
	itemNames := make(map[uuid.UUID]string)
	if len(itemIDs) > 0 {
		items, itemErr := s.client.Item.Query().
			Where(item.IDIn(itemIDs...)).
			Select(item.FieldID, item.FieldName).
			All(ctx)
		if itemErr == nil {
			for _, itm := range items {
				itemNames[itm.ID] = itm.Name
			}
		}
	}

	// Batch-fetch warehouse names.
	warehouseNames := make(map[uuid.UUID]string)
	if len(whIDs) > 0 {
		warehouses, whErr := s.client.Warehouse.Query().
			Where(warehouse.IDIn(whIDs...)).
			Select(warehouse.FieldID, warehouse.FieldName).
			All(ctx)
		if whErr == nil {
			for _, wh := range warehouses {
				warehouseNames[wh.ID] = wh.Name
			}
		}
	}

	// Batch-resolve the adjusting user's display name — surfaced so admins/managers can audit
	// who made each stock correction, matching the same lookup the adjustment-note PDF already
	// does per-batch (handlers.adjusterLabel) but here for every row on the list in one query.
	adjustedByNames := make(map[uuid.UUID]string)
	actorIDSet := make(map[uuid.UUID]struct{})
	for _, a := range adjustments {
		if a.AdjustedBy != uuid.Nil {
			actorIDSet[a.AdjustedBy] = struct{}{}
		}
	}
	if len(actorIDSet) > 0 {
		actorIDs := make([]uuid.UUID, 0, len(actorIDSet))
		for id := range actorIDSet {
			actorIDs = append(actorIDs, id)
		}
		if users, uErr := s.client.InventoryUser.Query().
			Where(entinvuser.TenantID(tenantID), entinvuser.AuthServiceUserIDIn(actorIDs...)).
			All(ctx); uErr == nil {
			for _, u := range users {
				if u.Name != "" {
					adjustedByNames[u.AuthServiceUserID] = u.Name
				} else {
					adjustedByNames[u.AuthServiceUserID] = u.Email
				}
			}
		}
	}

	result := make([]StockAdjustmentDTO, len(adjustments))
	for i, a := range adjustments {
		result[i] = StockAdjustmentDTO{
			ID:             a.ID,
			TenantID:       a.TenantID,
			ItemID:         a.ItemID,
			ItemName:       itemNames[a.ItemID],
			WarehouseID:    a.WarehouseID,
			WarehouseName:  warehouseNames[a.WarehouseID],
			QuantityBefore: a.QuantityBefore,
			QuantityChange: a.QuantityChange,
			QuantityAfter:  a.QuantityAfter,
			Reason:         string(a.Reason),
			Reference:      a.Reference,
			Notes:          a.Notes,
			AdjustedBy:     a.AdjustedBy,
			AdjustedByName: adjustedByNames[a.AdjustedBy],
			AdjustedAt:     a.AdjustedAt,
			CreatedAt:      a.CreatedAt,
		}
	}
	return result, total, nil
}

// checkAndPublishLowStock checks if stock is at or below reorder level and publishes an event.
// Also publishes a stock-out event when available reaches zero.
// costingMethod returns the tenant's configured inventory costing/consumption method
// (wavg|fifo|lifo|fefo). Defaults to "wavg" when no config row exists.
func (s *Service) costingMethod(ctx context.Context, tenantID uuid.UUID) string {
	cfg, err := s.client.TenantInventoryConfig.Query().
		Where(enttenantcfg.TenantID(tenantID)).Only(ctx)
	if err != nil || cfg == nil {
		return "wavg"
	}
	return cfg.CostingMethod.String()
}

// LotConsumption records one lot's contribution to a consumeLots draw-down — a single sale
// line can legitimately span two lots at a FEFO/FIFO/LIFO boundary. Returned so callers
// (RecordConsumption) can persist which lot(s) a sale actually drew from, closing the
// recall-traceability gap: without this, InventoryLot.quantity decrements but nothing records
// which order/consumption drew from which batch.
type LotConsumption struct {
	LotID      uuid.UUID
	LotNumber  string
	ExpiryDate *time.Time
	QtyTaken   float64
	// UnitCost is this specific layer's own cost_price — the actual price paid for the stock
	// drawn, never the flat Item.cost_price. Nil when the layer carries no recorded cost (e.g.
	// a legacy lot created before cost capture), in which case the caller must fall back.
	UnitCost *float64
	// IsCostLayer marks a layer auto-created for a non-lot-tracked item purely to preserve cost
	// (see InventoryLot.is_cost_layer). Callers must never surface LotNumber/ExpiryDate from
	// these on anything customer- or label-facing — they carry no lot identity a shopper or
	// regulator should see.
	IsCostLayer bool
}

// consumeLots draws down a quantity across a warehouse's active InventoryLot rows (real lots AND
// internal cost layers — same table) for an item, in the order dictated by the costing method:
// fifo=oldest received first, lifo=newest first, fefo=earliest expiry first. wavg tenants also
// draw in received order, but every open layer already carries the SAME blended cost (see
// RecomputeStandardCost), so which one is drawn first doesn't change the cost charged — a single
// draw-at-layer-cost naturally IS weighted-average costing, with no separate branch needed.
// Decrements lot quantity and marks a lot depleted at zero. Best-effort: errors are logged, not
// propagated (the balance is already the authoritative on-hand figure). Returns the lot(s)
// actually drawn from, in draw order, each carrying its OWN cost — this is what lets a purchase
// at a new price change the cost of new stock without touching stock bought at the old price.
func (s *Service) consumeLots(ctx context.Context, tx *ent.Tx, tenantID, itemID, warehouseID uuid.UUID, qty float64, method string) []LotConsumption {
	if qty <= 0 {
		return nil
	}
	q := tx.InventoryLot.Query().Where(
		entlot.TenantID(tenantID),
		entlot.ItemID(itemID),
		entlot.WarehouseID(warehouseID),
		entlot.StatusEQ(entlot.StatusActive),
		entlot.QuantityGT(0),
	)
	switch method {
	case "lifo":
		q = q.Order(ent.Desc(entlot.FieldReceivedAt), ent.Desc(entlot.FieldCreatedAt))
	case "fefo":
		// Earliest expiry first; lots without an expiry sort last (treated as far-future).
		q = q.Order(ent.Asc(entlot.FieldExpiryDate), ent.Asc(entlot.FieldReceivedAt), ent.Asc(entlot.FieldCreatedAt))
	default: // fifo, wavg
		q = q.Order(ent.Asc(entlot.FieldReceivedAt), ent.Asc(entlot.FieldCreatedAt))
	}
	lots, err := q.All(ctx)
	if err != nil {
		s.log.Warn("consumeLots: query lots failed", zap.Error(err))
		return nil
	}
	remaining := qty
	drawn := make([]LotConsumption, 0, len(lots))
	for _, lot := range lots {
		if remaining <= 0 {
			break
		}
		take := lot.Quantity
		if take > remaining {
			take = remaining
		}
		newQty := lot.Quantity - take
		upd := tx.InventoryLot.UpdateOne(lot).SetQuantity(newQty)
		if newQty <= 0 {
			upd = upd.SetStatus(entlot.StatusDepleted)
		}
		if _, e := upd.Save(ctx); e != nil {
			s.log.Warn("consumeLots: update lot failed", zap.Error(e))
			return drawn
		}
		drawn = append(drawn, LotConsumption{
			LotID:       lot.ID,
			LotNumber:   lot.LotNumber,
			ExpiryDate:  lot.ExpiryDate,
			QtyTaken:    take,
			UnitCost:    lot.CostPrice,
			IsCostLayer: lot.IsCostLayer,
		})
		remaining -= take
	}
	return drawn
}

// CostChange describes the outcome of RecomputeStandardCost.
type CostChange struct {
	ItemID       uuid.UUID
	SKU          string
	PreviousCost *float64
	NewCost      *float64
	Changed      bool
}

// RecomputeStandardCost recalculates Item.cost_price — the default/STANDARD cost used only to
// pre-fill new purchase orders and estimate margin — from the item's active InventoryLot cost
// layers (both real lots and the internal is_cost_layer rows auto-created for non-lot-tracked
// items at goods receipt). It is safe to change on every purchase: nothing downstream reads
// Item.cost_price for the VALUE of stock already on hand — COGS and stock valuation read each
// lot's own cost_price directly, so stock bought at the old price keeps costing at the old price
// no matter how often the standard cost changes. Publishes inventory.item.cost_changed on a real
// change, inside the caller's transaction (best-effort: a publish failure never fails the
// receipt/edit that triggered it).
//
// wavg tenants get the quantity-weighted average across every active layer; fifo/lifo/fefo
// tenants get the most-recently-received layer's cost, matching "the next purchase order should
// pre-fill at what I most recently paid."
func (s *Service) RecomputeStandardCost(ctx context.Context, tx *ent.Tx, tenantID, itemID uuid.UUID, source string) (*CostChange, error) {
	itm, err := tx.Item.Query().Where(item.TenantID(tenantID), item.ID(itemID)).Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: recompute standard cost: load item: %w", err)
	}
	unchanged := &CostChange{ItemID: itemID, SKU: itm.Sku, PreviousCost: itm.CostPrice, NewCost: itm.CostPrice, Changed: false}

	layers, err := tx.InventoryLot.Query().
		Where(entlot.TenantID(tenantID), entlot.ItemID(itemID), entlot.StatusEQ(entlot.StatusActive), entlot.CostPriceNotNil()).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: recompute standard cost: query layers: %w", err)
	}
	if len(layers) == 0 {
		return unchanged, nil
	}

	var newCost float64
	if s.costingMethod(ctx, tenantID) == "wavg" {
		var totalQty, totalValue float64
		for _, l := range layers {
			totalQty += l.Quantity
			totalValue += l.Quantity * *l.CostPrice
		}
		if totalQty <= 0 {
			return unchanged, nil
		}
		newCost = round4(totalValue / totalQty)
		// wavg blends into ONE cost shared by every open layer, so consumeLots' per-layer draw
		// (which always charges the layer's own cost_price) naturally IS weighted-average costing
		// — no separate branch needed anywhere downstream. Only active/open layers are touched;
		// a depleted layer's historical cost is left alone (nothing reads it again).
		for _, l := range layers {
			if l.CostPrice != nil && *l.CostPrice == newCost {
				continue
			}
			if _, e := tx.InventoryLot.UpdateOne(l).SetCostPrice(newCost).Save(ctx); e != nil {
				s.log.Warn("recompute standard cost: blend layer cost failed",
					zap.String("lot_id", l.ID.String()), zap.Error(e))
			}
		}
	} else {
		// fifo/lifo/fefo: pre-fill from the most recently received layer.
		newest := layers[0]
		newestAt := newest.CreatedAt
		if newest.ReceivedAt != nil {
			newestAt = *newest.ReceivedAt
		}
		for _, l := range layers[1:] {
			at := l.CreatedAt
			if l.ReceivedAt != nil {
				at = *l.ReceivedAt
			}
			if at.After(newestAt) {
				newest, newestAt = l, at
			}
		}
		newCost = *newest.CostPrice
	}

	if itm.CostPrice != nil && *itm.CostPrice == newCost {
		return unchanged, nil
	}

	if _, err := tx.Item.UpdateOneID(itemID).SetCostPrice(newCost).Save(ctx); err != nil {
		return nil, fmt.Errorf("stock: recompute standard cost: update item: %w", err)
	}

	platformevents.WriteOutboxTx(ctx, tx, s.log, tenantID, itemID, "inventory", "item.cost_changed", map[string]any{
		"item_id":       itemID,
		"sku":           itm.Sku,
		"previous_cost": itm.CostPrice,
		"new_cost":      newCost,
		"source":        source,
	})

	return &CostChange{ItemID: itemID, SKU: itm.Sku, PreviousCost: itm.CostPrice, NewCost: &newCost, Changed: true}, nil
}

func (s *Service) checkAndPublishLowStock(ctx context.Context, tx *ent.Tx, tenantID uuid.UUID, itm *ent.Item, bal *ent.InventoryBalance, warehouseID uuid.UUID) {
	// Non-depleting items are never auto-86'd or alerted: their balances are not
	// maintained by sales, so a zero/low reading is meaningless noise.
	if s.itemNonDepletingLazy(ctx, itm) {
		return
	}

	// Band the balance and compare against the last recorded band so events fire only on
	// a TRANSITION (ok→low, low→out, out→low, …), never repeatedly while the balance sits
	// in the same band. Without this, every sale of an already-depleted ingredient
	// republished stock.out — hundreds of duplicate alert emails a day per busy tenant.
	state := stockBandOK
	if bal.Available <= 0 {
		state = stockBandOut
	} else if bal.Available <= float64(bal.ReorderLevel) {
		state = stockBandLow
	}
	prev := s.lastStockLevelState(ctx, tx, tenantID, itm.ID, warehouseID)

	outletID := s.outletIDForWarehouse(ctx, tx, warehouseID)
	var outletUUID *uuid.UUID
	if oid, perr := uuid.Parse(outletID); perr == nil {
		outletUUID = &oid
	}

	if state == stockBandOK {
		// Leaving an alerted band re-arms the state machine (and gives reports the
		// recovery edge of the phase band). No outbox event: goods-receipt paths already
		// publish stock.updated/stock.in for catalog consumers.
		if prev == stockBandLow || prev == stockBandOut {
			s.persistStockLevelEvent(ctx, tx, tenantID, itm.ID, warehouseID, outletUUID, "restocked", bal.Available, bal.ReorderLevel)
		}
		return
	}
	if state == prev {
		return
	}

	notification := s.stockAlertNotification(ctx, tenantID)
	if state == stockBandOut {
		s.persistStockLevelEvent(ctx, tx, tenantID, itm.ID, warehouseID, outletUUID, "out", bal.Available, bal.ReorderLevel)
		s.writeOutboxEvent(ctx, tx, tenantID, itm.ID, "inventory", "stock.out", map[string]any{
			"tenant_id":    tenantID.String(),
			"item_id":      itm.ID.String(),
			"sku":          itm.Sku,
			"name":         itm.Name,
			"available":    bal.Available,
			"warehouse_id": warehouseID.String(),
			"outlet_id":    outletID,
			"notification": notification,
		})
		s.log.Warn("stock-out alert published",
			zap.String("sku", itm.Sku),
			zap.Float64("available", bal.Available),
		)
		// Cascade: mark recipe items as unavailable when an ingredient runs out.
		s.cascadeIngredientStockOut(ctx, tx, tenantID, itm.ID, warehouseID, notification)
	} else {
		s.persistStockLevelEvent(ctx, tx, tenantID, itm.ID, warehouseID, outletUUID, "low", bal.Available, bal.ReorderLevel)
		s.writeOutboxEvent(ctx, tx, tenantID, itm.ID, "inventory", "stock.low", map[string]any{
			"tenant_id":     tenantID.String(),
			"item_id":       itm.ID.String(),
			"sku":           itm.Sku,
			"name":          itm.Name,
			"available":     bal.Available,
			"reorder_level": bal.ReorderLevel,
			"warehouse_id":  warehouseID.String(),
			"outlet_id":     outletID,
			"notification":  notification,
		})
		s.log.Info("low stock alert published",
			zap.String("sku", itm.Sku),
			zap.Float64("available", bal.Available),
			zap.Int("reorder_level", bal.ReorderLevel),
		)
	}
}

// Stock bands for the low/out alert state machine. Values match the StockLevelEvent
// event_type enum so lastStockLevelState can compare them directly.
const (
	stockBandOK  = "ok"
	stockBandLow = "low"
	stockBandOut = "out"
)

// lastStockLevelState returns the band recorded by the most recent StockLevelEvent for the
// item+warehouse ("low"/"out"), "ok" after a restock, or "" when no event exists yet.
func (s *Service) lastStockLevelState(ctx context.Context, tx *ent.Tx, tenantID, itemID, warehouseID uuid.UUID) string {
	ev, err := tx.StockLevelEvent.Query().
		Where(
			stocklevelevent.TenantID(tenantID),
			stocklevelevent.ItemID(itemID),
			stocklevelevent.WarehouseID(warehouseID),
		).
		Order(ent.Desc(stocklevelevent.FieldOccurredAt)).
		First(ctx)
	if err != nil {
		return ""
	}
	if ev.EventType == stocklevelevent.EventTypeRestocked {
		return stockBandOK
	}
	return string(ev.EventType)
}

// stockAlertNotification builds the notification block embedded in stock.low/stock.out
// events. Alert emails are OPT-IN: enabled only when the tenant's inventory config has
// enable_low_stock_notifications set — a missing config row means no emails. The optional
// notification_email overrides the tenant contact address downstream.
func (s *Service) stockAlertNotification(ctx context.Context, tenantID uuid.UUID) map[string]any {
	n := map[string]any{"target": "staff", "enabled": false}
	cfg, err := s.client.TenantInventoryConfig.Query().
		Where(enttenantcfg.TenantID(tenantID)).Only(ctx)
	if err != nil || cfg == nil {
		return n
	}
	n["enabled"] = cfg.EnableLowStockNotifications
	if cfg.NotificationEmail != nil && *cfg.NotificationEmail != "" {
		n["email"] = *cfg.NotificationEmail
	}
	return n
}

// resolveWarehouseID returns the provided warehouse ID or the tenant's default warehouse.
// Used by S2S / event-driven paths that carry no operating-outlet context.
func (s *Service) resolveWarehouseID(ctx context.Context, tenantID, warehouseID uuid.UUID) (uuid.UUID, error) {
	return s.resolveWarehouseIDForOutlet(ctx, tenantID, warehouseID, uuid.Nil)
}

// resolveWarehouseIDForOutlet resolves the warehouse a stock movement should post to.
//
//   - An explicit warehouseID always wins.
//   - Otherwise, when an operating outlet is in play (X-Outlet-ID), the movement defaults to THAT
//     outlet's OWN warehouse. This is the key fix for "I stocked it but the POS still shows out of
//     stock": POS on_hand is outlet-scoped (the outlet's own warehouse ∪ null-outlet warehouses),
//     so an adjustment/receipt/count that silently defaulted to the tenant's is_default warehouse
//     (often HQ / the hotel outlet) never surfaced on a different outlet's terminal.
//   - Falls back to the tenant default only when there is no outlet context, or the outlet has no
//     warehouse of its own.
func (s *Service) resolveWarehouseIDForOutlet(ctx context.Context, tenantID, warehouseID, outletID uuid.UUID) (uuid.UUID, error) {
	if warehouseID != uuid.Nil {
		return warehouseID, nil
	}
	if outletID != uuid.Nil {
		wh, err := s.client.Warehouse.Query().
			Where(
				warehouse.TenantID(tenantID),
				warehouse.OutletIDEQ(outletID),
				warehouse.IsActive(true),
			).
			First(ctx)
		if err == nil {
			return wh.ID, nil
		}
		if !ent.IsNotFound(err) {
			return uuid.Nil, fmt.Errorf("stock: query outlet warehouse: %w", err)
		}
		// Outlet has no warehouse of its own → fall through to the tenant default.
	}
	wh, err := s.client.Warehouse.Query().
		Where(
			warehouse.TenantID(tenantID),
			warehouse.IsDefault(true),
			warehouse.IsActive(true),
		).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			// Include the resolved tenant so callers can see when an S2S request
			// resolved a wrong/empty tenant (e.g. consumption from a NATS-driven path).
			return uuid.Nil, fmt.Errorf("stock: no default warehouse for tenant %s", tenantID)
		}
		return uuid.Nil, fmt.Errorf("stock: query default warehouse: %w", err)
	}
	return wh.ID, nil
}

// ResolveItemBySKU resolves a sale/cart SKU to its stock-bearing parent Item.
//
// It looks the SKU up against Item first (the common case). If that misses, it
// falls back to ItemVariant: a variant carries its OWN sku (unique per item_id)
// but has NO own InventoryBalance and NO own recipe/BOM — it SHARES the parent
// Item's stock and BOM. So a variant SKU resolves to its parent Item, and all
// downstream stock/BOM logic operates on the parent. Returns ent.NotFound when
// neither an item nor a variant matches.
func (s *Service) ResolveItemBySKU(ctx context.Context, tenantID uuid.UUID, sku string) (*ent.Item, error) {
	itm, err := s.client.Item.Query().
		Where(item.TenantID(tenantID), item.Sku(sku), item.IsActive(true)).
		Only(ctx)
	if err == nil {
		return itm, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	// Fall back to a variant SKU → parent item. ItemVariant has no tenant_id of its
	// own (it's keyed by item_id); scope the tenant via the parent Item edge.
	v, verr := s.client.ItemVariant.Query().
		Where(
			itemvariant.Sku(sku),
			itemvariant.IsActive(true),
			itemvariant.HasItemWith(item.TenantID(tenantID)),
		).
		WithItem().
		Only(ctx)
	if verr != nil {
		return nil, err // surface the original item-not-found
	}
	if v.Edges.Item != nil {
		return v.Edges.Item, nil
	}
	return s.client.Item.Get(ctx, v.ItemID)
}

// resolveStockSKU maps a sale/cart SKU to the SKU that actually keys stock/BOM:
// the SKU itself for a real Item, or the PARENT item's SKU for a variant SKU.
// Variants share the parent's recipe + balance, so BOM explosion and balance
// lookups must run against the parent SKU. Unknown SKUs pass through unchanged
// so existing "item not found" handling downstream is preserved.
func (s *Service) resolveStockSKU(ctx context.Context, tenantID uuid.UUID, sku string) string {
	// Fast path: a real item with this SKU needs no remapping.
	exists, err := s.client.Item.Query().
		Where(item.TenantID(tenantID), item.Sku(sku)).
		Exist(ctx)
	if err == nil && exists {
		return sku
	}
	v, verr := s.client.ItemVariant.Query().
		Where(
			itemvariant.Sku(sku),
			itemvariant.IsActive(true),
			itemvariant.HasItemWith(item.TenantID(tenantID)),
		).
		WithItem().
		Only(ctx)
	if verr != nil {
		return sku
	}
	if v.Edges.Item != nil {
		return v.Edges.Item.Sku
	}
	if parent, perr := s.client.Item.Get(ctx, v.ItemID); perr == nil {
		return parent.Sku
	}
	return sku
}

// modifierConsumption expands the selected modifier options on a line into stock-consumption
// lines, scaled by the line quantity. Each modifier option resolves to a consumable SKU (its
// own sku, or — when only the option id is sent — looked up from the ModifierOption table,
// since inventory owns the option→SKU mapping). The resolved SKU is then run through the same
// variant→parent + BOM explosion as any item, so a modifier that is itself a recipe explodes.
// Best-effort per modifier: an option without a SKU (price-only, e.g. "No Sauce") is skipped.
func (s *Service) modifierConsumption(ctx context.Context, tenantID, warehouseID uuid.UUID, mods []ModifierLine, lineQty float64) []explodedIngredient {
	var out []explodedIngredient
	for _, m := range mods {
		sku := m.SKU
		// perUnit is how much of sku ONE selection of this option consumes (e.g. 20 for 20g
		// of honey on "Extra Honey") — authoritative source is the option's own deduction_qty,
		// NOT the caller-sent Quantity: every known caller (pos-api's sale-finalized event)
		// actually populates Quantity with the PARENT LINE's quantity, not a per-unit amount,
		// which would double-count once multiplied by lineQty below. Only fall back to the
		// caller's Quantity when the option can't be resolved at all (bare-sku legacy callers).
		perUnit := m.Quantity
		if m.InventoryModifierOptionID != "" {
			if oid, perr := uuid.Parse(m.InventoryModifierOptionID); perr == nil {
				if opt, oerr := s.client.ModifierOption.Get(ctx, oid); oerr == nil {
					if sku == "" {
						sku = opt.Sku
					}
					perUnit = opt.DeductionQty
				}
			}
		}
		if sku == "" {
			continue
		}
		if perUnit <= 0 {
			perUnit = 1
		}
		qty := perUnit * lineQty
		stockSKU := s.resolveStockSKU(ctx, tenantID, sku)
		if ings, isBOM := s.explodeBOM(ctx, tenantID, warehouseID, stockSKU, qty); isBOM {
			out = append(out, ings...)
		} else {
			out = append(out, explodedIngredient{SKU: stockSKU, Quantity: qty})
		}
	}
	return out
}

// explodedIngredient + explodeBOM live in bom.go: the ONE shared BOM-explosion path
// (unit conversion, content-per-unit bridge, sub-recipe backflush, waste factor) used
// by reservations, S2S consumption and the POS sale-finalized consumer alike.

// reserveIngredient reserves a single resolved ingredient SKU in a warehouse, decrementing
// available + incrementing reserved (capped at on-hand). Returns the qty actually reserved,
// the available qty seen, whether the request was fully satisfied, and whether the line was
// SKIPPED (non-depleting item or unit-mismatch line: no stock effect, never constrains the
// order — callers must not record skipped lines on the reservation, or release/consume
// would move stock that was never held). Shared by the parent line and modifier reservation
// so both go through identical balance handling.
func (s *Service) reserveIngredient(ctx context.Context, tx *ent.Tx, tenantID, whID uuid.UUID, ing explodedIngredient, cfg *ent.TenantInventoryConfig) (reservedQty, availableQty float64, fullyReserved, skipped bool, err error) {
	if ing.UnitMismatch {
		return 0, 0, true, true, nil
	}
	itm, qerr := tx.Item.Query().
		Where(item.TenantID(tenantID), item.Sku(ing.SKU), item.IsActive(true)).
		Only(ctx)
	if qerr != nil {
		s.log.Warn("ingredient item not found during reservation",
			zap.String("sku", ing.SKU), zap.Error(qerr))
		return 0, 0, false, false, nil
	}
	if isNonDepleting(itm, cfg) {
		return 0, 0, true, true, nil
	}

	bal, berr := tx.InventoryBalance.Query().
		Where(
			inventorybalance.TenantID(tenantID),
			inventorybalance.ItemID(itm.ID),
			inventorybalance.WarehouseID(whID),
		).
		First(ctx)
	if berr != nil {
		if ent.IsNotFound(berr) {
			return 0, 0, false, false, nil
		}
		return 0, 0, false, false, fmt.Errorf("stock: query balance: sku=%s: %w", ing.SKU, berr)
	}

	availableQty = bal.Available
	reserveQty := ing.Quantity
	fullyReserved = true
	if reserveQty > availableQty {
		reserveQty = availableQty
		fullyReserved = false
	}
	if reserveQty > 0 {
		if _, uerr := tx.InventoryBalance.UpdateOne(bal).
			SetAvailable(bal.Available - reserveQty).
			SetReserved(bal.Reserved + reserveQty).
			Save(ctx); uerr != nil {
			return 0, 0, false, false, fmt.Errorf("stock: update balance for sku=%s: %w", ing.SKU, uerr)
		}
	}
	return reserveQty, availableQty, fullyReserved, false, nil
}

// CreateReservation reserves stock for an order within a transaction.
// If a requested SKU has a recipe, the BOM is exploded and raw ingredients are reserved.
func (s *Service) CreateReservation(ctx context.Context, tenantID uuid.UUID, req ReservationRequest) (*ReservationResponse, error) {
	// Outlet-aware (same rule as RecordConsumption): explicit warehouse > selling outlet's own
	// warehouse > tenant default, so a multi-outlet reservation holds stock in the right place.
	whID, err := s.resolveWarehouseIDForOutlet(ctx, tenantID, req.WarehouseID, req.OutletID)
	if err != nil {
		return nil, err
	}

	// Check idempotency
	if req.IdempotencyKey != "" {
		existing, err := s.client.Reservation.Query().
			Where(reservation.IdempotencyKey(req.IdempotencyKey)).
			First(ctx)
		if err == nil {
			return s.mapReservation(existing), nil
		}
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	reservedItems := make([]entschema.ReservedItemJSON, 0, len(req.Items))
	// Tenant policy resolved once per reservation: non-depleting items are skipped
	// (no stock held, never constrain the order).
	cfg := s.tenantConfig(ctx, tenantID)

	for _, ri := range req.Items {
		// Resolve a variant SKU to its stock-bearing parent SKU (real items pass through),
		// then expand recipe/BOM into raw ingredients before reserving. If the SKU has no
		// recipe, falls back to a direct reservation against the resolved (parent) SKU.
		stockSKU := s.resolveStockSKU(ctx, tenantID, ri.SKU)
		ingredientsToReserve, isBOM := s.explodeBOM(ctx, tenantID, whID, stockSKU, ri.Quantity)
		if !isBOM {
			ingredientsToReserve = []explodedIngredient{{SKU: stockSKU, Quantity: ri.Quantity}}
		}

		totalReservedQty := 0.0
		fullyReserved := true

		for _, ing := range ingredientsToReserve {
			reserveQty, availableQty, ingFully, skipped, rerr := s.reserveIngredient(ctx, tx, tenantID, whID, ing, cfg)
			if rerr != nil {
				return nil, rerr
			}
			if skipped {
				// No stock effect (non-depleting / unit-mismatch): must not be recorded
				// on the reservation or release/consume would move stock never held.
				// The line never constrains the order (treated as fully reserved).
				continue
			}
			if !ingFully {
				fullyReserved = false
			}

			// For BOM items, only count ingredient reservations proportionally.
			if isBOM {
				totalReservedQty = ri.Quantity // treat as reserved at menu-item level
			} else {
				totalReservedQty = reserveQty
			}

			// Record each ingredient reservation for BOM items (for audit/release).
			if isBOM {
				reservedItems = append(reservedItems, entschema.ReservedItemJSON{
					SKU:             ing.SKU,
					RequestedQty:    ing.Quantity,
					ReservedQty:     reserveQty,
					AvailableQty:    availableQty,
					IsFullyReserved: reserveQty >= ing.Quantity,
				})
			}
		}

		// Reserve selected modifier stock (e.g. "Extra Cheese") as additional ingredient
		// lines, so ordering S2S reservations deduct modifier stock the same way POS sales
		// do. Recorded as their own reserved-item entries for audit/release.
		for _, ming := range s.modifierConsumption(ctx, tenantID, whID, ri.Modifiers, ri.Quantity) {
			reserveQty, availableQty, ingFully, skipped, rerr := s.reserveIngredient(ctx, tx, tenantID, whID, ming, cfg)
			if rerr != nil {
				return nil, rerr
			}
			if skipped {
				continue
			}
			if !ingFully {
				fullyReserved = false
			}
			reservedItems = append(reservedItems, entschema.ReservedItemJSON{
				SKU:             ming.SKU,
				RequestedQty:    ming.Quantity,
				ReservedQty:     reserveQty,
				AvailableQty:    availableQty,
				IsFullyReserved: reserveQty >= ming.Quantity,
			})
		}

		if !isBOM {
			// Direct item reservation — record with original SKU.
			reservedItems = append(reservedItems, entschema.ReservedItemJSON{
				SKU:             ri.SKU,
				RequestedQty:    ri.Quantity,
				ReservedQty:     totalReservedQty,
				AvailableQty:    totalReservedQty,
				IsFullyReserved: fullyReserved,
			})
		} else if fullyReserved {
			// Add a summary entry for the composite (menu-item) SKU.
			reservedItems = append(reservedItems, entschema.ReservedItemJSON{
				SKU:             ri.SKU,
				RequestedQty:    ri.Quantity,
				ReservedQty:     ri.Quantity,
				AvailableQty:    ri.Quantity,
				IsFullyReserved: true,
			})
		}
	}

	builder := tx.Reservation.Create().
		SetTenantID(tenantID).
		SetOrderID(req.OrderID).
		SetWarehouseID(whID).
		SetStatus("pending").
		SetItems(reservedItems)

	if req.ExpiresAt != nil {
		builder.SetExpiresAt(*req.ExpiresAt)
	}
	if req.IdempotencyKey != "" {
		builder.SetIdempotencyKey(req.IdempotencyKey)
	}

	resv, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: create reservation: %w", err)
	}

	s.writeOutboxEvent(ctx, tx, tenantID, resv.ID, "inventory", "reservation.confirmed", map[string]any{
		"order_id":     req.OrderID.String(),
		"warehouse_id": whID.String(),
		"items":        reservedItems,
	})

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("stock: commit reservation: %w", err)
	}

	s.log.Info("reservation created",
		zap.String("reservation_id", resv.ID.String()),
		zap.String("order_id", req.OrderID.String()),
		zap.Int("items", len(reservedItems)),
	)

	return s.mapReservation(resv), nil
}

// GetReservation returns a reservation by ID.
func (s *Service) GetReservation(ctx context.Context, tenantID, reservationID uuid.UUID) (*ReservationResponse, error) {
	resv, err := s.client.Reservation.Query().
		Where(
			reservation.ID(reservationID),
			reservation.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("stock: reservation not found")
		}
		return nil, fmt.Errorf("stock: query reservation: %w", err)
	}
	return s.mapReservation(resv), nil
}

// GetReservationsByOrderID returns reservations for an order.
func (s *Service) GetReservationsByOrderID(ctx context.Context, tenantID, orderID uuid.UUID) ([]ReservationResponse, error) {
	reservations, err := s.client.Reservation.Query().
		Where(
			reservation.TenantID(tenantID),
			reservation.OrderID(orderID),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: query reservations by order: %w", err)
	}

	result := make([]ReservationResponse, len(reservations))
	for i, r := range reservations {
		result[i] = *s.mapReservation(r)
	}
	return result, nil
}

// ReservationListFilter filters ListReservations' tenant-wide view.
type ReservationListFilter struct {
	Status string
	// OrderID, when set, scopes to a single order. The only other native, indexed field on
	// Reservation besides tenant/status/order_id is the `items` JSON blob (sku/qty pairs, no
	// item name) — not a queryable column — so an order-ID lookup is the whole of "search" here.
	OrderID *uuid.UUID
	Limit   int
	Offset  int
}

// ReservationSummary is one row of the tenant-wide Reservations list — one row PER Reservation
// (a single order's hold, possibly across several items), matching how every other multi-line
// document list in this codebase (Transfers, Adjustments) summarizes a document as one row
// rather than flattening every line into the top-level list.
type ReservationSummary struct {
	ID            uuid.UUID      `json:"id"`
	OrderID       uuid.UUID      `json:"order_id"`
	WarehouseID   *uuid.UUID     `json:"warehouse_id,omitempty"`
	WarehouseName string         `json:"warehouse_name,omitempty"`
	Status        string         `json:"status"`
	Items         []ReservedItem `json:"items"`
	ItemCount     int            `json:"item_count"`
	TotalQuantity float64        `json:"total_quantity"`
	ExpiresAt     *time.Time     `json:"expires_at,omitempty"`
	ConfirmedAt   *time.Time     `json:"confirmed_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

// ListReservations returns a tenant-wide, paginated view of stock reservations — real
// limit/offset with an accurate total, same shape as ListTransfers/ListAdjustments. Before this,
// GetReservationsByOrderID was the ONLY way to read reservations, and it hard-required an
// order_id (400s without one) — so inventory-ui's Reservations page, which has no order context
// to supply, could never load anything at all.
func (s *Service) ListReservations(ctx context.Context, tenantID uuid.UUID, filter ReservationListFilter) ([]ReservationSummary, int, error) {
	q := s.client.Reservation.Query().Where(reservation.TenantID(tenantID))
	if filter.Status != "" {
		q = q.Where(reservation.Status(filter.Status))
	}
	if filter.OrderID != nil {
		q = q.Where(reservation.OrderID(*filter.OrderID))
	}

	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("stock: count reservations: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	reservations, err := q.
		Order(ent.Desc(reservation.FieldCreatedAt)).
		Limit(limit).
		Offset(filter.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("stock: list reservations: %w", err)
	}

	// Batch-resolve warehouse names for display — one query for the whole page, mirroring the
	// itemDisplayMap/actorNamesByID batch-lookup pattern used elsewhere rather than a query per row.
	whIDs := make(map[uuid.UUID]bool, len(reservations))
	for _, r := range reservations {
		if r.WarehouseID != nil {
			whIDs[*r.WarehouseID] = true
		}
	}
	whIDSlice := make([]uuid.UUID, 0, len(whIDs))
	for id := range whIDs {
		whIDSlice = append(whIDSlice, id)
	}
	whNameByID := make(map[uuid.UUID]string, len(whIDSlice))
	if len(whIDSlice) > 0 {
		if warehouses, werr := s.client.Warehouse.Query().Where(warehouse.IDIn(whIDSlice...)).All(ctx); werr == nil {
			for _, w := range warehouses {
				whNameByID[w.ID] = w.Name
			}
		}
	}

	result := make([]ReservationSummary, len(reservations))
	for i, r := range reservations {
		mapped := s.mapReservation(r)
		summary := ReservationSummary{
			ID:          mapped.ID,
			OrderID:     mapped.OrderID,
			WarehouseID: r.WarehouseID,
			Status:      mapped.Status,
			Items:       mapped.Items,
			ItemCount:   len(mapped.Items),
			ExpiresAt:   mapped.ExpiresAt,
			ConfirmedAt: mapped.ConfirmedAt,
			CreatedAt:   mapped.CreatedAt,
		}
		if r.WarehouseID != nil {
			summary.WarehouseName = whNameByID[*r.WarehouseID]
		}
		for _, it := range mapped.Items {
			summary.TotalQuantity += it.ReservedQty
		}
		result[i] = summary
	}
	return result, total, nil
}

// ReleaseReservation releases a stock reservation, restoring available quantities.
func (s *Service) ReleaseReservation(ctx context.Context, tenantID, reservationID uuid.UUID, reason string) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("stock: begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	resv, err := tx.Reservation.Query().
		Where(
			reservation.ID(reservationID),
			reservation.TenantID(tenantID),
			reservation.StatusIn("pending", "confirmed"),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return fmt.Errorf("stock: reservation not found or already released")
		}
		return fmt.Errorf("stock: query reservation: %w", err)
	}

	whID := uuid.Nil
	if resv.WarehouseID != nil {
		whID = *resv.WarehouseID
	}

	for _, ri := range resv.Items {
		if ri.ReservedQty <= 0 {
			continue
		}

		itm, err := tx.Item.Query().
			Where(item.TenantID(tenantID), item.Sku(ri.SKU)).
			Only(ctx)
		if err != nil {
			continue
		}

		bal, err := tx.InventoryBalance.Query().
			Where(
				inventorybalance.TenantID(tenantID),
				inventorybalance.ItemID(itm.ID),
				inventorybalance.WarehouseID(whID),
			).
			First(ctx)
		if err != nil {
			continue
		}

		_, err = tx.InventoryBalance.UpdateOne(bal).
			SetAvailable(bal.Available + ri.ReservedQty).
			SetReserved(max(0, bal.Reserved-ri.ReservedQty)).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("stock: update balance for sku=%s: %w", ri.SKU, err)
		}
	}

	_, err = tx.Reservation.UpdateOne(resv).
		SetStatus("released").
		Save(ctx)
	if err != nil {
		return fmt.Errorf("stock: update reservation status: %w", err)
	}

	s.writeOutboxEvent(ctx, tx, tenantID, reservationID, "inventory", "reservation.released", map[string]any{
		"order_id": resv.OrderID.String(),
		"reason":   reason,
	})

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("stock: commit release: %w", err)
	}

	s.log.Info("reservation released",
		zap.String("reservation_id", reservationID.String()),
		zap.String("reason", reason),
	)
	return nil
}

// ConsumeReservation converts a reservation to actual consumption, deducting on-hand stock.
func (s *Service) ConsumeReservation(ctx context.Context, tenantID, reservationID uuid.UUID) (*ConsumeReservationResponse, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	resv, err := tx.Reservation.Query().
		Where(
			reservation.ID(reservationID),
			reservation.TenantID(tenantID),
			reservation.StatusIn("pending", "confirmed"),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("stock: reservation not found or already consumed")
		}
		return nil, fmt.Errorf("stock: query reservation: %w", err)
	}

	whID := uuid.Nil
	if resv.WarehouseID != nil {
		whID = *resv.WarehouseID
	}

	method := s.costingMethod(ctx, tenantID)
	var lotsConsumed []ConsumedLot

	for _, ri := range resv.Items {
		if ri.ReservedQty <= 0 {
			continue
		}

		itm, err := tx.Item.Query().
			Where(item.TenantID(tenantID), item.Sku(ri.SKU)).
			Only(ctx)
		if err != nil {
			continue
		}

		bal, err := tx.InventoryBalance.Query().
			Where(
				inventorybalance.TenantID(tenantID),
				inventorybalance.ItemID(itm.ID),
				inventorybalance.WarehouseID(whID),
			).
			First(ctx)
		if ent.IsNotFound(err) {
			// First-ever consumption of this item at this warehouse — auto-create a zero
			// balance (same pattern as RecordConsumption/AdjustStock) instead of silently
			// skipping the line: without a row to decrement, this reservation's consumption
			// left no trace anywhere. See [[oversell-negative-stock-settlement]].
			bal, err = tx.InventoryBalance.Create().
				SetTenantID(tenantID).
				SetItemID(itm.ID).
				SetWarehouseID(whID).
				Save(ctx)
		}
		if err != nil {
			continue
		}

		// Atomic on_hand delta (matches RecordConsumption's pattern, closes a lost-update race
		// under concurrent consumption of the same row) with NO floor at zero — a reservation
		// drawn against stock that's already run out while pending must carry the resulting
		// debt as negative on_hand, same as a direct-sale oversell, so a later
		// transfer/GRN/adjustment settles it instead of a floor silently erasing it. Reserved
		// keeps its own floor-at-0 (a currently-held-reservations count should never go
		// negative — unrelated, correct invariant). See [[oversell-negative-stock-settlement]].
		onHandBefore := bal.OnHand
		updatedBal, updateErr := tx.InventoryBalance.UpdateOne(bal).
			AddOnHand(-ri.ReservedQty).
			SetReserved(round4(max(0, bal.Reserved-ri.ReservedQty))).
			Save(ctx)
		if updateErr != nil {
			err = updateErr
			return nil, fmt.Errorf("stock: update balance for sku=%s: %w", ri.SKU, err)
		}
		if roundedOnHand := round4(updatedBal.OnHand); roundedOnHand != updatedBal.OnHand {
			updatedBal, updateErr = tx.InventoryBalance.UpdateOne(updatedBal).SetOnHand(roundedOnHand).Save(ctx)
			if updateErr != nil {
				err = updateErr
				return nil, fmt.Errorf("stock: round balance for sku=%s: %w", ri.SKU, err)
			}
		}
		if sf := eventShortfall(ri.ReservedQty, onHandBefore); sf > 0 {
			s.log.Warn("reservation consume exceeds on-hand — balance now carries the debt as negative stock",
				zap.String("sku", ri.SKU),
				zap.Float64("needed", ri.ReservedQty),
				zap.Float64("shortfall", sf),
				zap.Float64("resulting_on_hand", updatedBal.OnHand),
			)
		}

		// Draw down lot cost-layers in FIFO/LIFO/FEFO/wavg order, same mechanism
		// RecordConsumption/AdjustStock already use — previously ConsumeReservation only moved
		// the aggregate InventoryBalance and never touched InventoryLot at all, so a
		// FEFO-costed tenant's reservation-based dispense (e.g. a pharmacy prescription) had no
		// server-verified batch/expiry per unit dispensed, only whatever a clinician typed in by
		// hand. Reservations are already exploded down to raw SKUs at CreateReservation time, so
		// no BOM/recipe handling is needed here (unlike RecordConsumption).
		for _, lot := range s.consumeLots(ctx, tx, tenantID, itm.ID, whID, ri.ReservedQty, method) {
			if lot.IsCostLayer {
				continue // no lot identity a caller should ever see
			}
			lotsConsumed = append(lotsConsumed, ConsumedLot{
				SKU:        ri.SKU,
				LotID:      lot.LotID,
				LotNumber:  lot.LotNumber,
				ExpiryDate: lot.ExpiryDate,
				Quantity:   lot.QtyTaken,
			})
		}

		// Check for low stock after consumption
		s.checkAndPublishLowStock(ctx, tx, tenantID, itm, updatedBal, whID)
	}

	now := time.Now()
	_, err = tx.Reservation.UpdateOne(resv).
		SetStatus("consumed").
		SetConfirmedAt(now).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: update reservation status: %w", err)
	}

	s.writeOutboxEvent(ctx, tx, tenantID, reservationID, "inventory", "stock.consumed", map[string]any{
		"order_id":    resv.OrderID.String(),
		"consumed_at": now.UTC().Format(time.RFC3339),
		"items_count": len(resv.Items),
	})

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("stock: commit consume: %w", err)
	}

	s.log.Info("reservation consumed",
		zap.String("reservation_id", reservationID.String()),
		zap.Int("lots_consumed", len(lotsConsumed)),
	)
	return &ConsumeReservationResponse{Status: "consumed", LotsConsumed: lotsConsumed}, nil
}

// RecordConsumption records direct stock consumption without a prior reservation.
func (s *Service) RecordConsumption(ctx context.Context, tenantID uuid.UUID, req ConsumptionRequest) (*ConsumptionResponse, error) {
	// Resolve the warehouse OUTLET-AWARE: explicit WarehouseID > the selling outlet's own
	// warehouse > the tenant default. A multi-outlet sale (e.g. a supermarket outlet) must deduct
	// from its OWN warehouse — falling back to the tenant-default (HQ/hotel) warehouse deducted
	// against a warehouse with no balance for the item, silently shortfalling every sale.
	whID, err := s.resolveWarehouseIDForOutlet(ctx, tenantID, req.WarehouseID, req.OutletID)
	if err != nil {
		return nil, err
	}

	if req.IdempotencyKey != "" {
		existing, idempErr := s.client.Consumption.Query().
			Where(entconsumption.IdempotencyKeyEQ(req.IdempotencyKey)).
			First(ctx)
		if idempErr == nil {
			return &ConsumptionResponse{
				ID:          existing.ID,
				TenantID:    existing.TenantID,
				OrderID:     existing.OrderID,
				Status:      existing.Status,
				ProcessedAt: existing.ProcessedAt,
			}, nil
		}
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Tenant policy resolved once: non-depletion + theoretical-usage recording.
	cfg := s.tenantConfig(ctx, tenantID)

	// Explode each requested SKU into its raw ingredients (mirrors CreateReservation) so
	// callers may pass a menu-item SKU and we consume the recipe BOM. A SKU with no recipe
	// passes through unchanged, so directly-stocked goods still deduct correctly. Without
	// this, POS sale backflush (which sends menu SKUs) deducted the menu item's own balance
	// instead of its ingredients.
	flattened := make([]explodedIngredient, 0, len(req.Items))
	for _, ci := range req.Items {
		// A non-depleting menu item is never exploded: its usage is recorded
		// theoretically (AvT reporting) but no ingredient stock moves. Its modifiers
		// still deduct below — modifier items carry their own tracking mode.
		if itm, ierr := s.ResolveItemBySKU(ctx, tenantID, ci.SKU); ierr == nil && isNonDepleting(itm, cfg) {
			flattened = append(flattened, explodedIngredient{SKU: itm.Sku, Quantity: ci.Quantity, Theoretical: true, RequestedUOM: ci.UOM, FinishedItemSKU: itm.Sku})
			for _, ming := range s.modifierConsumption(ctx, tenantID, whID, ci.Modifiers, ci.Quantity) {
				ming.FinishedItemSKU = itm.Sku
				flattened = append(flattened, ming)
			}
			continue
		}

		// Map a variant SKU to its parent (real items unchanged) so both the BOM
		// explosion and the direct fallback deduct the parent item's balance.
		stockSKU := s.resolveStockSKU(ctx, tenantID, ci.SKU)
		ingredients, isBOM := s.explodeBOM(ctx, tenantID, whID, stockSKU, ci.Quantity)
		if !isBOM {
			// Direct line — convert a sale-line UOM (e.g. a 30 ml pour of a bottle
			// stocked in pieces) into the item's stock unit when one is provided.
			line := explodedIngredient{SKU: stockSKU, Quantity: ci.Quantity, FinishedItemSKU: stockSKU}
			if ci.UOM != "" {
				if itm, ierr := s.client.Item.Query().
					Where(item.TenantID(tenantID), item.Sku(stockSKU)).
					WithUnits().
					Only(ctx); ierr == nil {
					if converted, ok := ConvertToStockUnit(itm, ci.Quantity, ci.UOM); ok {
						if converted != ci.Quantity {
							line.RequestedQty, line.RequestedUOM = ci.Quantity, ci.UOM
						}
						line.Quantity = round4(converted)
					} else {
						line.UnitMismatch = true
						line.RequestedQty, line.RequestedUOM = ci.Quantity, ci.UOM
						line.Quantity = 0
					}
				}
			}
			flattened = append(flattened, line)
		} else {
			// BOM lines already carry RecipeID/RecipeSKU from explodeBOM; FinishedItemSKU
			// is the sale-line context only RecordConsumption knows.
			for i := range ingredients {
				ingredients[i].FinishedItemSKU = stockSKU
			}
			flattened = append(flattened, ingredients...)
		}
		// Consume selected modifier stock as additional lines (mirrors POS sale backflush
		// and the reservation path), so ordering S2S consumption deducts modifier stock.
		for _, ming := range s.modifierConsumption(ctx, tenantID, whID, ci.Modifiers, ci.Quantity) {
			ming.FinishedItemSKU = stockSKU
			flattened = append(flattened, ming)
		}
	}

	method := s.costingMethod(ctx, tenantID)
	reason := req.Reason
	if reason == "" {
		reason = "sale"
	}
	consumptionID := uuid.New()
	now := time.Now()
	// saleDate is what every ConsumptionLine.consumed_at below is stamped with (drives the
	// stock-history ledger's "Date" column) — the triggering sale's own effective date when the
	// caller supplied one (see ConsumptionRequest.SaleDate), else the real processing time `now`
	// (unchanged pre-existing behavior for S2S/ordering callers). `now` itself is left untouched
	// for Consumption.ProcessedAt below, which must stay the real audit timestamp.
	saleDate := now
	if req.SaleDate != nil {
		saleDate = *req.SaleDate
	}
	outletID := s.resolveOutletID(ctx, tx, whID)

	consumptionItems := make([]entschema.ConsumptionItemJSON, 0, len(flattened))
	lotDraws := make([]ConsumedLot, 0)
	for _, cl := range flattened {
		entry := entschema.ConsumptionItemJSON{
			SKU:          cl.SKU,
			Quantity:     cl.Quantity,
			UnitMismatch: cl.UnitMismatch,
			Theoretical:  cl.Theoretical,
			RequestedUOM: cl.RequestedUOM,
		}

		if cl.UnitMismatch {
			// Cross-dimension line with no conversion bridge: deducting the raw number
			// would corrupt the balance. Record the full theoretical need as shortfall
			// so variance reports surface it, but touch no stock.
			entry.Quantity = cl.RequestedQty
			entry.ShortfallQty = cl.RequestedQty
			consumptionItems = append(consumptionItems, entry)
			continue
		}

		itm, ierr := tx.Item.Query().
			Where(item.TenantID(tenantID), item.Sku(cl.SKU)).
			Only(ctx)
		if ierr != nil {
			// Resilient event processing: one unknown SKU must not poison the whole
			// sale (NAK/redelivery storms on the consumer). Record it as unsatisfied.
			s.log.Warn("consumption: item not found — recorded with shortfall, no deduction",
				zap.String("sku", cl.SKU), zap.String("tenant_id", tenantID.String()))
			entry.ShortfallQty = cl.Quantity
			consumptionItems = append(consumptionItems, entry)
			continue
		}

		// Non-depleting ingredient/goods (e.g. ice cubes flagged non_depleting): record
		// theoretical usage only, per tenant policy.
		if cl.Theoretical || isNonDepleting(itm, cfg) {
			entry.Theoretical = true
			if cfg == nil || cfg.RecordTheoreticalUsage {
				consumptionItems = append(consumptionItems, entry)
				s.recordConsumptionLine(ctx, tx, tenantID, consumptionLineInput{
					consumptionID:    consumptionID,
					orderID:          req.OrderID,
					orderNumber:      req.OrderNumber,
					customerName:     req.CustomerName,
					customerPhone:    req.CustomerPhone,
					servedByUserID:   req.ServedByUserID,
					servedByName:     req.ServedByName,
					warehouseID:      whID,
					outletID:         outletID,
					recipeID:         cl.RecipeID,
					recipeSKU:        cl.RecipeSKU,
					finishedItemSKU:  cl.FinishedItemSKU,
					ingredientItemID: itm.ID,
					ingredientSKU:    cl.SKU,
					quantity:         cl.Quantity,
					unitCost:         itemCostPrice(itm),
					theoretical:      true,
					reason:           reason,
					consumedAt:       saleDate,
				})
			}
			continue
		}

		bal, berr := tx.InventoryBalance.Query().
			Where(
				inventorybalance.TenantID(tenantID),
				inventorybalance.ItemID(itm.ID),
				inventorybalance.WarehouseID(whID),
			).
			First(ctx)
		if ent.IsNotFound(berr) {
			// No balance row exists yet for this item in this warehouse — this can be a
			// genuinely first-ever sale at a new outlet/warehouse, not just a data gap.
			// Auto-create a zero balance (the same first-touch pattern AdjustStock already
			// uses) instead of silently no-opping the deduction below: without a row to
			// decrement, an oversell here left ZERO trace anywhere at all — worse than the
			// floor-at-zero bug, since not even a negative balance or shortfall signal
			// survived. See [[oversell-negative-stock-settlement]].
			var createErr error
			bal, createErr = tx.InventoryBalance.Create().
				SetTenantID(tenantID).
				SetItemID(itm.ID).
				SetWarehouseID(whID).
				Save(ctx)
			if createErr != nil {
				return nil, fmt.Errorf("stock: init balance for sku=%s: %w", cl.SKU, createErr)
			}
			berr = nil
		}
		switch {
		case berr == nil:
			deduct := cl.Quantity // keep fractional — do not truncate sub-unit consumption

			// Atomic SQL-level decrement (SET on_hand = on_hand + $delta), not a read-then-
			// computed-write — two concurrent consumptions against the SAME balance row (e.g.
			// two POS terminals ringing up refills of the same fractional-content-bridge
			// bottle, or two tots pulled off the same bottle, at once) previously raced: both
			// read the same starting on_hand, both computed their new value off that one
			// stale snapshot, and the second UPDATE silently clobbered the first's decrement
			// instead of compounding it (a lost update — remaining stock read as higher than
			// it really was, and a real oversell could go completely undetected). AddOnHand/
			// AddAvailable let Postgres apply the delta at the row itself; Postgres's own
			// per-row UPDATE lock (held for the rest of THIS transaction) means a concurrent
			// consumer of the same row blocks until this one fully commits — including the
			// shortfall clamp below — so the two always correctly compound in some real order,
			// never lose each other's decrement.
			// Reuse the function-scoped err (not a fresh local) — this function's own deferred
			// rollback (below, at tx creation) checks this exact variable, so every error path
			// here must assign to it rather than shadow it with a fresh :=.
			var updatedBal *ent.InventoryBalance
			updatedBal, err = tx.InventoryBalance.UpdateOneID(bal.ID).
				AddOnHand(-deduct).
				AddAvailable(-deduct).
				Save(ctx)
			if err != nil {
				return nil, fmt.Errorf("stock: update balance for sku=%s: %w", cl.SKU, err)
			}
			// Isolate THIS event's own contribution to any shortfall — not the balance's total
			// carried-forward debt, which would double-count an earlier sale's unsettled
			// shortfall as if it were new. onHandBefore is the pre-delta value (AddOnHand
			// above already applied -deduct).
			onHandBefore := updatedBal.OnHand + deduct
			if thisEventShortfall := eventShortfall(deduct, onHandBefore); thisEventShortfall > 0 {
				// Oversell signal: this consumption's own need exceeded what was really
				// on-hand at the time. The balance now carries the resulting debt as a
				// genuine negative on_hand/available — a later transfer/GRN/adjustment
				// settles it by adding on top, instead of the debt being silently erased by
				// a floor-at-zero clamp (the prior, deliberately-deferred behavior — see
				// [[oversell-negative-stock-settlement]]).
				entry.ShortfallQty = thisEventShortfall
				s.log.Warn("consumption exceeds on-hand — balance now carries the debt as negative stock",
					zap.String("sku", cl.SKU),
					zap.Float64("needed", deduct),
					zap.Float64("shortfall", thisEventShortfall),
					zap.Float64("resulting_on_hand", updatedBal.OnHand),
				)
			}
			// Round away float64 accumulation drift (repeated AddOnHand/AddAvailable calls on
			// a long-lived row otherwise drift to noise like 0.8999999999999999) in the same
			// follow-up update — this is itself a second atomic update scoped to this same
			// locked row, safe to apply unconditionally since nothing else can be racing it
			// mid-transaction. Negative values are NOT clamped: a genuinely oversold balance
			// stays negative until real stock arrives to settle it.
			roundedOnHand, roundedAvailable := round4(updatedBal.OnHand), round4(updatedBal.Available)
			if roundedOnHand != updatedBal.OnHand || roundedAvailable != updatedBal.Available {
				var roundedBal *ent.InventoryBalance
				roundedBal, err = tx.InventoryBalance.UpdateOneID(bal.ID).
					SetOnHand(roundedOnHand).
					SetAvailable(roundedAvailable).
					Save(ctx)
				if err != nil {
					return nil, fmt.Errorf("stock: round balance for sku=%s: %w", cl.SKU, err)
				}
				updatedBal = roundedBal
			}

			// Check for low stock after consumption
			s.checkAndPublishLowStock(ctx, tx, tenantID, itm, updatedBal, whID)

			// Lot-ordered consumption: draw down InventoryLot cost layers (real lots AND the
			// internal is_cost_layer rows created for non-lot-tracked items — same table) in
			// FIFO/LIFO/FEFO/wavg order BEFORE persisting the ConsumptionLine(s), so each line
			// records the SPECIFIC layer's own cost — what this stock actually cost when it was
			// bought — rather than the item's current flat cost_price. This is what stops a
			// later purchase at a new price from retroactively changing the recorded cost of
			// stock already sold or still on hand.
			lots := s.consumeLots(ctx, tx, tenantID, itm.ID, whID, deduct, method)
			var drawnQty float64
			for _, lot := range lots {
				drawnQty += lot.QtyTaken
			}
			for _, lot := range lots {
				lid := lot.LotID
				unitCost := itemCostPrice(itm)
				if lot.UnitCost != nil {
					unitCost = *lot.UnitCost
				}
				// A cost-layer row carries no lot identity a customer/regulator should ever see —
				// only real lot-tracked/perishable batches surface their lot number/expiry.
				lotNumber, expiryDate := lot.LotNumber, lot.ExpiryDate
				if lot.IsCostLayer {
					lotNumber, expiryDate = "", nil
				}
				s.recordConsumptionLine(ctx, tx, tenantID, consumptionLineInput{
					consumptionID:    consumptionID,
					orderID:          req.OrderID,
					orderNumber:      req.OrderNumber,
					customerName:     req.CustomerName,
					customerPhone:    req.CustomerPhone,
					servedByUserID:   req.ServedByUserID,
					servedByName:     req.ServedByName,
					warehouseID:      whID,
					outletID:         outletID,
					recipeID:         cl.RecipeID,
					recipeSKU:        cl.RecipeSKU,
					finishedItemSKU:  cl.FinishedItemSKU,
					ingredientItemID: itm.ID,
					ingredientSKU:    cl.SKU,
					quantity:         lot.QtyTaken,
					unitCost:         unitCost,
					reason:           reason,
					consumedAt:       saleDate,
					lotID:            &lid,
					lotNumber:        lotNumber,
					expiryDate:       expiryDate,
				})
				if !lot.IsCostLayer {
					lotDraws = append(lotDraws, ConsumedLot{
						SKU:        cl.SKU,
						LotID:      lot.LotID,
						LotNumber:  lot.LotNumber,
						ExpiryDate: lot.ExpiryDate,
						Quantity:   lot.QtyTaken,
					})
				}
			}
			// Shortfall: layers didn't cover the full quantity (e.g. a legacy item with no
			// opening-balance layer yet, or genuine drift between InventoryBalance and layer
			// totals). Never leave it uncosted or cost it at zero — that silently inflates
			// margin, which is worse than the flat-cost bug this replaces. Fall back to the
			// item's standard cost for exactly the uncovered remainder.
			if shortfall := round4(deduct - drawnQty); shortfall > 0 {
				s.recordConsumptionLine(ctx, tx, tenantID, consumptionLineInput{
					consumptionID:    consumptionID,
					orderID:          req.OrderID,
					orderNumber:      req.OrderNumber,
					customerName:     req.CustomerName,
					customerPhone:    req.CustomerPhone,
					servedByUserID:   req.ServedByUserID,
					servedByName:     req.ServedByName,
					warehouseID:      whID,
					outletID:         outletID,
					recipeID:         cl.RecipeID,
					recipeSKU:        cl.RecipeSKU,
					finishedItemSKU:  cl.FinishedItemSKU,
					ingredientItemID: itm.ID,
					ingredientSKU:    cl.SKU,
					quantity:         shortfall,
					unitCost:         itemCostPrice(itm),
					reason:           reason,
					consumedAt:       saleDate,
				})
			}
		default:
			// A real query error — the no-balance-row case is handled above (auto-creates a
			// zero balance and falls into the berr==nil branch), so berr here is never
			// ent.IsNotFound.
			return nil, fmt.Errorf("stock: query balance for sku=%s: %w", cl.SKU, berr)
		}

		consumptionItems = append(consumptionItems, entry)
	}

	builder := tx.Consumption.Create().
		SetID(consumptionID).
		SetTenantID(tenantID).
		SetOrderID(req.OrderID).
		SetItems(consumptionItems).
		SetReason(reason).
		SetStatus("processed").
		SetProcessedAt(now)

	if whID != uuid.Nil {
		builder.SetWarehouseID(whID)
	}
	if req.IdempotencyKey != "" {
		builder.SetIdempotencyKey(req.IdempotencyKey)
	}

	cons, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("stock: create consumption: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("stock: commit consumption: %w", err)
	}

	s.log.Info("consumption recorded",
		zap.String("consumption_id", cons.ID.String()),
		zap.String("order_id", req.OrderID.String()),
	)

	return &ConsumptionResponse{
		ID:           cons.ID,
		TenantID:     cons.TenantID,
		OrderID:      cons.OrderID,
		Status:       cons.Status,
		ProcessedAt:  cons.ProcessedAt,
		LotsConsumed: lotDraws,
	}, nil
}

// writeOutboxEvent stores a domain event in the outbox within an Ent transaction.
// Non-fatal: logs on failure so the business operation still succeeds.
func (s *Service) writeOutboxEvent(ctx context.Context, tx *ent.Tx, tenantID, aggregateID uuid.UUID, aggregateType, eventType string, payload map[string]any) {
	platformevents.WriteOutboxTx(ctx, tx, s.log, tenantID, aggregateID, aggregateType, eventType, payload)
}

// RestockItem represents a single item to restock (reverse consumption).
type RestockItem struct {
	SKU      string  `json:"sku"`
	Quantity float64 `json:"quantity"`
}

// RestockItems restores stock for returned items, incrementing on_hand and available.
// Used by return/refund consumers to restock the warehouse after a customer return.
//
// outletID scopes the restock to the SELLING outlet's own warehouse when no explicit warehouseID
// is supplied (explicit warehouse > outlet's own warehouse > tenant default) — a returned item must
// go back to the outlet it was sold from, not the tenant-default warehouse. uuid.Nil = legacy
// tenant-default fallback (manufacturing/production-cancel callers that carry no outlet).
func (s *Service) RestockItems(ctx context.Context, tenantID, warehouseID, outletID uuid.UUID, items []RestockItem, idempotencyKey string) error {
	whID, err := s.resolveWarehouseIDForOutlet(ctx, tenantID, warehouseID, outletID)
	if err != nil {
		return err
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("stock: begin restock tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for _, ri := range items {
		itm, err := tx.Item.Query().
			Where(item.TenantID(tenantID), item.Sku(ri.SKU)).
			Only(ctx)
		if err != nil {
			s.log.Warn("restock: item not found, skipping", zap.String("sku", ri.SKU))
			continue
		}

		qty := ri.Quantity // fractional-capable restock
		bal, err := tx.InventoryBalance.Query().
			Where(
				inventorybalance.TenantID(tenantID),
				inventorybalance.ItemID(itm.ID),
				inventorybalance.WarehouseID(whID),
			).
			First(ctx)
		if ent.IsNotFound(err) {
			// First-time stock-in (e.g. a make-to-stock product completing its first production
			// batch, or a return of an item never previously stocked here) — CREATE the balance
			// row instead of silently dropping the stock. Mirrors the GRN applyStockIn path.
			if _, cerr := tx.InventoryBalance.Create().
				SetTenantID(tenantID).SetItemID(itm.ID).SetWarehouseID(whID).
				SetOnHand(qty).SetAvailable(qty).SetReserved(0).Save(ctx); cerr != nil {
				return fmt.Errorf("stock: create restock balance sku=%s: %w", ri.SKU, cerr)
			}
		} else if err != nil {
			return fmt.Errorf("stock: query restock balance sku=%s: %w", ri.SKU, err)
		} else {
			if _, uerr := tx.InventoryBalance.UpdateOne(bal).
				SetOnHand(bal.OnHand + qty).
				SetAvailable(bal.Available + qty).
				Save(ctx); uerr != nil {
				return fmt.Errorf("stock: restock balance sku=%s: %w", ri.SKU, uerr)
			}
		}

		// Cascade: unblock recipe items when all their ingredients are back in stock.
		s.cascadeIngredientRestocked(ctx, tx, tenantID, itm.ID, whID)

		s.writeOutboxEvent(ctx, tx, tenantID, itm.ID, "inventory", "stock.restocked", map[string]any{
			"tenant_id":    tenantID.String(),
			"item_id":      itm.ID.String(),
			"sku":          ri.SKU,
			"quantity":     qty,
			"warehouse_id": whID.String(),
			"reason":       "customer_return",
		})
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("stock: commit restock: %w", err)
	}

	s.log.Info("items restocked",
		zap.Int("count", len(items)),
		zap.String("idempotency_key", idempotencyKey),
	)
	return nil
}

func (s *Service) mapReservation(r *ent.Reservation) *ReservationResponse {
	resp := &ReservationResponse{
		ID:          r.ID,
		TenantID:    r.TenantID,
		OrderID:     r.OrderID,
		Status:      r.Status,
		ExpiresAt:   r.ExpiresAt,
		ConfirmedAt: r.ConfirmedAt,
		CreatedAt:   r.CreatedAt,
	}

	resp.Items = make([]ReservedItem, len(r.Items))
	for i, ri := range r.Items {
		resp.Items[i] = ReservedItem{
			SKU:             ri.SKU,
			RequestedQty:    ri.RequestedQty,
			ReservedQty:     ri.ReservedQty,
			AvailableQty:    ri.AvailableQty,
			IsFullyReserved: ri.IsFullyReserved,
		}
	}

	return resp
}
