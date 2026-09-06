package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/inventory-service/internal/audit"
	"github.com/bengobox/inventory-service/internal/ent"
	entitem "github.com/bengobox/inventory-service/internal/ent/item"
	entip "github.com/bengobox/inventory-service/internal/ent/itempricing"
	entpt "github.com/bengobox/inventory-service/internal/ent/pricingtier"
	invmiddleware "github.com/bengobox/inventory-service/internal/http/middleware"
	"github.com/bengobox/inventory-service/internal/modules/items"
	"github.com/bengobox/inventory-service/internal/modules/rbac"
	platformevents "github.com/bengobox/inventory-service/internal/platform/events"
)

type PricingTierHandler struct {
	log      *zap.Logger
	orm      *ent.Client
	rbacSvc  *rbac.Service
	auditSvc *audit.Service
	itemsSvc *items.Service
}

func NewPricingTierHandler(log *zap.Logger, orm *ent.Client, rbacSvc *rbac.Service) *PricingTierHandler {
	return &PricingTierHandler{
		log:     log.Named("pricing_tier.handler"),
		orm:     orm,
		rbacSvc: rbacSvc,
	}
}

// SetAuditService wires the centralized audit trail for selling-price changes.
func (h *PricingTierHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// SetItemsService injects the items service so price resolution can lazily promote a queued
// "new_stock_only" price change once its trigger has fired (see items.Service.
// PromotePendingPriceChanges).
func (h *PricingTierHandler) SetItemsService(svc *items.Service) { h.itemsSvc = svc }

// actorFromRequest resolves the acting user from the request's auth claims, falling back to
// uuid.Nil for S2S calls where there is no human actor.
func actorFromRequest(r *http.Request) uuid.UUID {
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		if id, err := claims.UserID(); err == nil {
			return id
		}
	}
	return uuid.Nil
}

func (h *PricingTierHandler) RegisterRoutes(r chi.Router) {
	perm := func(code string) func(http.Handler) http.Handler {
		if h.rbacSvc == nil {
			return func(next http.Handler) http.Handler { return next }
		}
		return invmiddleware.RequirePermission(h.rbacSvc, h.log, code)
	}

	r.Route("/inventory/pricing-tiers", func(pt chi.Router) {
		pt.Get("/", h.ListTiers)
		pt.With(perm(rbac.PermItemsAdd)).Post("/", h.CreateTier)
		pt.With(perm(rbac.PermItemsChange)).Put("/{tierID}", h.UpdateTier)
		pt.With(perm(rbac.PermItemsDelete)).Delete("/{tierID}", h.DeactivateTier)
		// Bulk-generate every item's price for this tier from the default tier (× factor) or
		// from cost + margin — so a Wholesale tier can be populated in one click instead of
		// item-by-item. Generated prices are clamped to each item's min/max band.
		pt.With(perm(rbac.PermItemsChange)).Post("/{tierID}/generate", h.GenerateTierPricing)
	})

	r.Route("/inventory/items/{itemID}/pricing", func(ip chi.Router) {
		ip.Get("/", h.GetItemPricing)
		// Idempotency-Key guarded: a retried request must not double-apply a price change.
		ip.With(perm(rbac.PermItemsChange), invmiddleware.Idempotency(h.orm)).Put("/", h.UpsertItemPricing)
		// Delete ONE outlet-scoped price row (never the all-outlets default — see DeleteItemPricing).
		ip.With(perm(rbac.PermItemsChange)).Delete("/{id}", h.DeleteItemPricing)
	})

	// Quantity-aware price resolution: returns unit price + total for N units using default tier.
	r.Get("/inventory/items/{itemID}/price", h.GetItemPrice)

	// Bulk endpoint: returns default-tier price for every item in the tenant.
	// Used by downstream services (pos-api, etc.) to show prices without N+1 calls.
	r.Get("/inventory/items/pricing", h.ListAllItemPricing)
}

// --- PricingTier DTOs ---

type pricingTierDTO struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	Name        string    `json:"name"`
	Code        string    `json:"code"`
	Description string    `json:"description,omitempty"`
	IsDefault   bool      `json:"is_default"`
	IsActive    bool      `json:"is_active"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
}

type createTierReq struct {
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
	IsDefault   bool   `json:"is_default"`
	SortOrder   int    `json:"sort_order"`
}

type updateTierReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsDefault   *bool  `json:"is_default"`
	IsActive    *bool  `json:"is_active"`
	SortOrder   *int   `json:"sort_order"`
}

// --- ItemPricing DTOs ---

type itemPricingDTO struct {
	ID            uuid.UUID  `json:"id"`
	ItemID        uuid.UUID  `json:"item_id"`
	PricingTierID uuid.UUID  `json:"pricing_tier_id"`
	TierName      string     `json:"tier_name,omitempty"`
	TierCode      string     `json:"tier_code,omitempty"`
	Price         float64    `json:"price"`
	Currency      string     `json:"currency"`
	OutletID      *uuid.UUID `json:"outlet_id,omitempty"`
	TierBasis     string     `json:"tier_basis,omitempty"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	IsActive      bool       `json:"is_active"`
}

type upsertItemPricingEntry struct {
	PricingTierID uuid.UUID  `json:"pricing_tier_id"`
	Price         float64    `json:"price"`
	Currency      string     `json:"currency"`
	OutletID      *uuid.UUID `json:"outlet_id"`
	TierBasis     string     `json:"tier_basis"`
	EffectiveFrom *time.Time `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
}

// --- PricingTier handlers ---

// defaultPricingTiers are seeded the first time a tenant's tiers are listed, so the pricing-profile
// feature (Retail/Wholesale prices at the POS) works out of the box. Per-tier item prices are then
// configured by admins via the item-pricing endpoint.
var defaultPricingTiers = []struct {
	Name      string
	Code      string
	IsDefault bool
	SortOrder int
}{
	{"Retail", "RETAIL", true, 0},
	{"Wholesale", "WHOLESALE", false, 1},
}

// ensureDefaultTiers creates the default Retail/Wholesale tiers for a tenant that has none yet.
// Concurrency-safe: the (tenant_id, code) unique index drops duplicate inserts.
func (h *PricingTierHandler) ensureDefaultTiers(ctx context.Context, tenantID uuid.UUID) {
	if exists, err := h.orm.PricingTier.Query().Where(entpt.TenantID(tenantID)).Exist(ctx); err != nil || exists {
		return
	}
	for _, t := range defaultPricingTiers {
		if _, err := h.orm.PricingTier.Create().
			SetTenantID(tenantID).
			SetName(t.Name).
			SetCode(t.Code).
			SetIsDefault(t.IsDefault).
			SetIsActive(true).
			SetSortOrder(t.SortOrder).
			Save(ctx); err != nil {
			h.log.Warn("ensureDefaultTiers: seed failed", zap.String("code", t.Code), zap.Error(err))
		}
	}
}

func (h *PricingTierHandler) ListTiers(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	h.ensureDefaultTiers(r.Context(), tenantID)

	tiers, err := h.orm.PricingTier.Query().
		Where(entpt.TenantID(tenantID)).
		Order(ent.Asc(entpt.FieldSortOrder), ent.Asc(entpt.FieldName)).
		All(r.Context())
	if err != nil {
		h.log.Error("list pricing tiers failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "Failed to list pricing tiers")
		return
	}

	dtos := make([]pricingTierDTO, 0, len(tiers))
	for _, t := range tiers {
		dtos = append(dtos, toPricingTierDTO(t))
	}
	writeJSON(w, http.StatusOK, dtos)
}

func (h *PricingTierHandler) CreateTier(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req createTierReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if req.Name == "" || req.Code == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "name and code are required")
		return
	}

	tier, err := h.orm.PricingTier.Create().
		SetTenantID(tenantID).
		SetName(req.Name).
		SetCode(req.Code).
		SetDescription(req.Description).
		SetIsDefault(req.IsDefault).
		SetSortOrder(req.SortOrder).
		Save(r.Context())
	if err != nil {
		h.log.Error("create pricing tier failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", "Failed to create pricing tier")
		return
	}
	writeJSON(w, http.StatusCreated, toPricingTierDTO(tier))
}

func (h *PricingTierHandler) UpdateTier(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	tierID, err := uuid.Parse(chi.URLParam(r, "tierID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TIER", "Invalid tier ID")
		return
	}

	existing, err := h.orm.PricingTier.Get(r.Context(), tierID)
	if err != nil || existing.TenantID != tenantID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Pricing tier not found")
		return
	}

	var req updateTierReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	upd := h.orm.PricingTier.UpdateOneID(tierID)
	if req.Name != "" {
		upd = upd.SetName(req.Name)
	}
	if req.Description != "" {
		upd = upd.SetDescription(req.Description)
	}
	if req.IsDefault != nil {
		upd = upd.SetIsDefault(*req.IsDefault)
	}
	if req.IsActive != nil {
		upd = upd.SetIsActive(*req.IsActive)
	}
	if req.SortOrder != nil {
		upd = upd.SetSortOrder(*req.SortOrder)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update pricing tier failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", "Failed to update pricing tier")
		return
	}
	writeJSON(w, http.StatusOK, toPricingTierDTO(updated))
}

func (h *PricingTierHandler) DeactivateTier(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	tierID, err := uuid.Parse(chi.URLParam(r, "tierID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TIER", "Invalid tier ID")
		return
	}

	existing, err := h.orm.PricingTier.Get(r.Context(), tierID)
	if err != nil || existing.TenantID != tenantID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Pricing tier not found")
		return
	}

	if _, err := h.orm.PricingTier.UpdateOneID(tierID).SetIsActive(false).Save(r.Context()); err != nil {
		h.log.Error("deactivate pricing tier failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "DEACTIVATE_FAILED", "Failed to deactivate tier")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated"})
}

// --- ItemPricing handlers ---

func (h *PricingTierHandler) GetItemPricing(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ITEM", "Invalid item ID")
		return
	}

	pricings, err := h.orm.ItemPricing.Query().
		Where(entip.TenantID(tenantID), entip.ItemID(itemID), entip.IsActive(true)).
		All(r.Context())
	if err != nil {
		h.log.Error("get item pricing failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "Failed to get item pricing")
		return
	}

	tierIDs := make([]uuid.UUID, 0, len(pricings))
	for _, p := range pricings {
		tierIDs = append(tierIDs, p.PricingTierID)
	}
	tiersByID := pricingTierNamesByID(r.Context(), h.orm, tenantID, tierIDs)

	dtos := make([]itemPricingDTO, 0, len(pricings))
	for _, p := range pricings {
		dtos = append(dtos, toItemPricingDTO(p, tiersByID))
	}
	writeJSON(w, http.StatusOK, dtos)
}

func (h *PricingTierHandler) UpsertItemPricing(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ITEM", "Invalid item ID")
		return
	}

	var entries []upsertItemPricingEntry
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Expected array of pricing entries")
		return
	}

	// Load the item's selling-price guardrails once and reject any out-of-band tier price.
	// Hard floor/ceiling — keeps catalog/POS prices coherent with the item's configured band.
	// Only applies to the ALL-OUTLETS (default) row: an outlet-scoped override (outlet_id set)
	// is a deliberate per-branch price decision that the whole per-outlet-pricing add-on exists
	// to allow — a branch legitimately charging more (or less) than the tenant-wide default is
	// the feature working as intended, not a mistake to block. Found live 2026-09-07: a branch
	// price of 782 was rejected against a tenant-wide max of 780.
	itm, err := h.orm.Item.Query().
		Where(entitem.TenantID(tenantID), entitem.ID(itemID)).
		Select(entitem.FieldMinSellingPrice, entitem.FieldMaxSellingPrice).
		First(r.Context())
	if err != nil {
		writeError(w, http.StatusNotFound, "ITEM_NOT_FOUND", "Item not found")
		return
	}
	for _, entry := range entries {
		if entry.OutletID != nil {
			continue
		}
		if itm.MinSellingPrice != nil && entry.Price < *itm.MinSellingPrice {
			writeError(w, http.StatusUnprocessableEntity, "PRICE_BELOW_MIN",
				fmt.Sprintf("price %.2f is below the item minimum %.2f", entry.Price, *itm.MinSellingPrice))
			return
		}
		if itm.MaxSellingPrice != nil && entry.Price > *itm.MaxSellingPrice {
			writeError(w, http.StatusUnprocessableEntity, "PRICE_ABOVE_MAX",
				fmt.Sprintf("price %.2f is above the item maximum %.2f", entry.Price, *itm.MaxSellingPrice))
			return
		}
	}

	ctx := r.Context()
	tx, err := h.orm.Tx(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TX_FAILED", "Failed to start transaction")
		return
	}

	// Audit rows are recorded outside the transaction (audit.Service.Record is deliberately
	// best-effort/fire-and-forget, per its own package contract) — collected here and written
	// only after a successful commit, so a rolled-back price change never leaves a misleading
	// audit trail behind.
	type auditPending struct {
		outletID  *uuid.UUID
		tierID    uuid.UUID
		prevPrice *float64
		newPrice  float64
	}
	var pendingAudits []auditPending

	entryTierIDs := make([]uuid.UUID, 0, len(entries))
	for _, entry := range entries {
		entryTierIDs = append(entryTierIDs, entry.PricingTierID)
	}
	tiersByID := pricingTierNamesByID(ctx, h.orm, tenantID, entryTierIDs)

	results := make([]itemPricingDTO, 0, len(entries))
	for _, entry := range entries {
		q := tx.ItemPricing.Query().
			Where(entip.TenantID(tenantID), entip.ItemID(itemID), entip.PricingTierID(entry.PricingTierID), entip.IsActive(true))
		if entry.OutletID != nil {
			q = q.Where(entip.OutletID(*entry.OutletID))
		} else {
			q = q.Where(entip.OutletIDIsNil())
		}
		existing, qErr := q.First(ctx)
		var prevPrice *float64
		if qErr == nil {
			p := existing.Price
			prevPrice = &p
		}

		effectiveFrom := time.Now()
		if entry.EffectiveFrom != nil {
			effectiveFrom = *entry.EffectiveFrom
		}
		currency := entry.Currency
		if currency == "" {
			currency = "KES"
		}

		// Supersede, never overwrite: delete the current active row (if any), then insert the
		// new price as its own row. No price history is kept (2026-08-31: changed from
		// soft-deactivate to hard-delete on tenant request — the is_active=false rows were
		// never read anywhere) — GetItemPrice always resolves the single is_active=true row,
		// so nothing downstream needs to change to keep working.
		if qErr == nil && existing.Price == entry.Price && entry.TierBasis == string(existing.TierBasis) {
			// No real change — skip the churn of closing + reopening an identical row.
			saved := existing
			results = append(results, toItemPricingDTO(saved, tiersByID))
			continue
		}
		if qErr == nil {
			// Hard-delete the superseded row (tenant request: stop accumulating unread
			// is_active=false price history) instead of soft-deactivating it.
			if cErr := tx.ItemPricing.DeleteOne(existing).Exec(ctx); cErr != nil {
				_ = tx.Rollback()
				h.log.Error("delete prior item pricing failed", zap.Error(cErr))
				writeError(w, http.StatusInternalServerError, "UPSERT_FAILED", "Failed to upsert item pricing")
				return
			}
		}
		creator := tx.ItemPricing.Create().
			SetTenantID(tenantID).
			SetItemID(itemID).
			SetPricingTierID(entry.PricingTierID).
			SetPrice(entry.Price).
			SetCurrency(currency).
			SetEffectiveFrom(effectiveFrom).
			SetIsActive(true)
		if entry.OutletID != nil {
			creator = creator.SetOutletID(*entry.OutletID)
		}
		if entry.TierBasis != "" {
			creator = creator.SetTierBasis(entip.TierBasis(entry.TierBasis))
		} else if qErr == nil {
			creator = creator.SetTierBasis(existing.TierBasis)
		}
		if entry.EffectiveTo != nil {
			creator = creator.SetEffectiveTo(*entry.EffectiveTo)
		}
		saved, err := creator.Save(ctx)
		if err != nil {
			_ = tx.Rollback()
			h.log.Error("upsert item pricing failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "UPSERT_FAILED", "Failed to upsert item pricing")
			return
		}
		results = append(results, toItemPricingDTO(saved, tiersByID))

		if prevPrice == nil || *prevPrice != saved.Price {
			pendingAudits = append(pendingAudits, auditPending{
				outletID: saved.OutletID, tierID: entry.PricingTierID, prevPrice: prevPrice, newPrice: saved.Price,
			})
		}

		// Publish inside the SAME transaction as the price write — a crash between "price
		// saved" and "event published" must not silently drop the event (the previous
		// fire-and-forget goroutine + post-commit outbox write could lose it).
		h.publishPricingUpdatedEventTx(ctx, tx, tenantID, itemID, saved, prevPrice)
	}

	if err := tx.Commit(); err != nil {
		h.log.Error("upsert item pricing: commit failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "COMMIT_FAILED", "Failed to save item pricing")
		return
	}

	if h.auditSvc != nil {
		actor := actorFromRequest(r)
		for _, pa := range pendingAudits {
			h.auditSvc.Record(ctx, audit.Entry{
				TenantID:    tenantID,
				OutletID:    pa.outletID,
				ActorUserID: actor,
				Action:      "item.selling_price_changed",
				EntityType:  "item_pricing",
				EntityID:    itemID.String(),
				Before:      map[string]any{"pricing_tier_id": pa.tierID, "price": pa.prevPrice},
				After:       map[string]any{"pricing_tier_id": pa.tierID, "price": pa.newPrice},
			})
		}
	}

	writeJSON(w, http.StatusOK, results)
}

// DeleteItemPricing removes ONE outlet-scoped ItemPricing row, reverting that outlet back to
// the tenant-wide (all-outlets) price for this tier. The all-outlets default row itself
// (outlet_id nil) can't be deleted here — it IS the base every outlet falls back to once its own
// override is gone, so there's nothing to "revert" it to; edit it via UpsertItemPricing instead.
//
// Cascades to pos-api's POSCatalogOverride.selling_price and ordering-backend's
// CatalogOverride.base_price for the SAME (tenant, sku, outlet), via a published
// "item.outlet_pricing_removed" event — a tenant deleting a per-branch price expects that branch
// to actually charge the standard price again, but pos-api/ordering-backend each maintain their
// OWN independent price override at their layer (inventory -> ordering -> pos, POS wins per the
// three-layer precedence), so without this cascade a stale downstream override would keep
// shadowing the now-reverted inventory price. Event-driven, not a direct S2S call, matching how
// every other inventory->downstream catalog sync in this codebase works (inventory-api has no
// direct dependency on pos-api/ordering-backend, deliberately, per the per-outlet-pricing plan).
// DELETE /inventory/items/{itemID}/pricing/{id}
func (h *PricingTierHandler) DeleteItemPricing(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ITEM", "Invalid item ID")
		return
	}
	pricingID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PRICING", "Invalid pricing id")
		return
	}

	ctx := r.Context()
	existing, err := h.orm.ItemPricing.Query().
		Where(entip.ID(pricingID), entip.TenantID(tenantID), entip.ItemID(itemID)).
		Only(ctx)
	if err != nil {
		writeError(w, http.StatusNotFound, "PRICING_NOT_FOUND", "Pricing entry not found")
		return
	}
	if existing.OutletID == nil {
		writeError(w, http.StatusUnprocessableEntity, "CANNOT_DELETE_DEFAULT",
			"The all-outlets default price can't be deleted this way — edit it instead")
		return
	}

	itm, err := h.orm.Item.Query().
		Where(entitem.TenantID(tenantID), entitem.ID(itemID)).
		Select(entitem.FieldSku).
		First(ctx)
	if err != nil {
		writeError(w, http.StatusNotFound, "ITEM_NOT_FOUND", "Item not found")
		return
	}

	tx, err := h.orm.Tx(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TX_FAILED", "Failed to start transaction")
		return
	}
	if err := tx.ItemPricing.DeleteOne(existing).Exec(ctx); err != nil {
		_ = tx.Rollback()
		h.log.Error("delete item pricing failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", "Failed to delete item pricing")
		return
	}
	// Publish inside the same transaction as the delete — same reasoning as
	// publishPricingUpdatedEventTx: a crash between "row deleted" and "event published" must not
	// silently strand a stale downstream override.
	platformevents.WriteOutboxTx(ctx, tx, h.log, tenantID, itemID, "inventory", "item.outlet_pricing_removed", map[string]any{
		"item_id":         itemID,
		"sku":             itm.Sku,
		"pricing_tier_id": existing.PricingTierID,
		"outlet_id":       *existing.OutletID,
	})
	if err := tx.Commit(); err != nil {
		h.log.Error("delete item pricing: commit failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "COMMIT_FAILED", "Failed to delete item pricing")
		return
	}

	if h.auditSvc != nil {
		actor := actorFromRequest(r)
		h.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tenantID,
			OutletID:    existing.OutletID,
			ActorUserID: actor,
			Action:      "item.outlet_pricing_deleted",
			EntityType:  "item_pricing",
			EntityID:    itemID.String(),
			Before:      map[string]any{"pricing_tier_id": existing.PricingTierID, "price": existing.Price, "outlet_id": existing.OutletID},
			After:       map[string]any{"reverted_to": "all_outlets_default"},
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

type generateTierPricingReq struct {
	// Source basis: "default_tier" (price = default tier price × factor) or
	// "cost_margin" (price = cost_price / (1 - margin_percent/100)).
	Source        string  `json:"source"`
	Factor        float64 `json:"factor"`         // multiplier for default_tier (e.g. 0.9 = 10% off)
	MarginPercent float64 `json:"margin_percent"` // margin for cost_margin (0 < m < 100)
	Overwrite     bool    `json:"overwrite"`      // replace existing prices on this tier
}

type generateTierPricingResp struct {
	Generated int      `json:"generated"`
	Skipped   int      `json:"skipped"`
	Clamped   int      `json:"clamped"`
	Warnings  []string `json:"warnings,omitempty"`
}

// GenerateTierPricing bulk-populates a pricing tier's per-item prices.
// POST /inventory/pricing-tiers/{tierID}/generate
func (h *PricingTierHandler) GenerateTierPricing(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	tierID, err := uuid.Parse(chi.URLParam(r, "tierID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TIER", "Invalid tier ID")
		return
	}
	var req generateTierPricingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if req.Source == "" {
		req.Source = "default_tier"
	}
	if req.Source == "default_tier" && req.Factor <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_FACTOR", "factor must be > 0 for default_tier source")
		return
	}
	if req.Source == "cost_margin" && (req.MarginPercent <= 0 || req.MarginPercent >= 100) {
		writeError(w, http.StatusBadRequest, "INVALID_MARGIN", "margin_percent must be between 0 and 100")
		return
	}

	// Validate the target tier belongs to the tenant and is active.
	tier, err := h.orm.PricingTier.Query().Where(entpt.ID(tierID), entpt.TenantID(tenantID), entpt.IsActive(true)).Only(r.Context())
	if err != nil {
		writeError(w, http.StatusNotFound, "TIER_NOT_FOUND", "Pricing tier not found")
		return
	}

	// Default-tier price per item (source=default_tier) — built from active default-tier pricings.
	defaultPrices := map[uuid.UUID]float64{}
	if req.Source == "default_tier" {
		defTier, derr := h.orm.PricingTier.Query().Where(entpt.TenantID(tenantID), entpt.IsActive(true), entpt.IsDefault(true)).First(r.Context())
		if derr != nil {
			writeError(w, http.StatusUnprocessableEntity, "NO_DEFAULT_TIER", "No default pricing tier to derive from")
			return
		}
		dps, _ := h.orm.ItemPricing.Query().
			Where(entip.TenantID(tenantID), entip.IsActive(true), entip.PricingTierID(defTier.ID), entip.OutletIDIsNil()).
			All(r.Context())
		for _, p := range dps {
			defaultPrices[p.ItemID] = p.Price
		}
	}

	// Existing target-tier prices (to honor overwrite=false).
	existing := map[uuid.UUID]bool{}
	{
		eps, _ := h.orm.ItemPricing.Query().
			Where(entip.TenantID(tenantID), entip.PricingTierID(tierID), entip.OutletIDIsNil(), entip.IsActive(true)).
			All(r.Context())
		for _, p := range eps {
			existing[p.ItemID] = true
		}
	}

	items, err := h.orm.Item.Query().
		Where(entitem.TenantID(tenantID), entitem.IsActive(true)).
		Select(entitem.FieldID, entitem.FieldCostPrice, entitem.FieldMinSellingPrice, entitem.FieldMaxSellingPrice).
		All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "Failed to load items")
		return
	}

	resp := generateTierPricingResp{}
	for _, it := range items {
		if existing[it.ID] && !req.Overwrite {
			resp.Skipped++
			continue
		}
		var base float64
		switch req.Source {
		case "default_tier":
			base = defaultPrices[it.ID]
			if base <= 0 {
				resp.Skipped++
				continue
			}
			base = base * req.Factor
		case "cost_margin":
			if it.CostPrice == nil || *it.CostPrice <= 0 {
				resp.Skipped++
				continue
			}
			base = *it.CostPrice / (1 - req.MarginPercent/100)
		default:
			writeError(w, http.StatusBadRequest, "INVALID_SOURCE", "source must be default_tier or cost_margin")
			return
		}
		price := math.Ceil(base)
		// Clamp into the item's configured selling-price band.
		if it.MinSellingPrice != nil && price < *it.MinSellingPrice {
			price = *it.MinSellingPrice
			resp.Clamped++
		}
		if it.MaxSellingPrice != nil && price > *it.MaxSellingPrice {
			price = *it.MaxSellingPrice
			resp.Clamped++
		}
		itemTx, txErr := h.orm.Tx(r.Context())
		if txErr != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("item %s: %v", it.ID, txErr))
			continue
		}
		if _, uerr := h.upsertTierPrice(r.Context(), itemTx, tenantID, it.ID, tierID, price); uerr != nil {
			_ = itemTx.Rollback()
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("item %s: %v", it.ID, uerr))
			continue
		}
		if cErr := itemTx.Commit(); cErr != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("item %s: %v", it.ID, cErr))
			continue
		}
		resp.Generated++
	}
	h.log.Info("generated tier pricing", zap.String("tier", tier.Code), zap.Int("generated", resp.Generated), zap.Int("skipped", resp.Skipped))
	writeJSON(w, http.StatusOK, resp)
}

// upsertTierPrice creates or updates the (tenant,item,tier) all-outlets pricing row at the given
// price AND publishes item.pricing_updated in the SAME transaction, so the write and its event
// are atomic — either both land or neither does.
func (h *PricingTierHandler) upsertTierPrice(ctx context.Context, tx *ent.Tx, tenantID, itemID, tierID uuid.UUID, price float64) (*ent.ItemPricing, error) {
	existing, err := tx.ItemPricing.Query().
		Where(entip.TenantID(tenantID), entip.ItemID(itemID), entip.PricingTierID(tierID), entip.OutletIDIsNil(), entip.IsActive(true)).
		First(ctx)
	var prevPrice *float64
	var saved *ent.ItemPricing
	now := time.Now()
	switch {
	case err == nil && existing.Price == price:
		// No real change — skip the churn of closing + reopening an identical row.
		return existing, nil
	case err == nil:
		p := existing.Price
		prevPrice = &p
		// Supersede, never overwrite: delete the current row, insert the new price as its own
		// row. No price history is kept (2026-08-31: hard-delete, not soft-deactivate — see
		// the sibling upsert paths in this file and items/pricing_enrich.go for why this is
		// safe against the concurrent-write race the soft-delete pattern originally guarded).
		if cErr := tx.ItemPricing.DeleteOne(existing).Exec(ctx); cErr != nil {
			return nil, cErr
		}
		saved, err = tx.ItemPricing.Create().
			SetTenantID(tenantID).SetItemID(itemID).SetPricingTierID(tierID).
			SetPrice(price).SetCurrency(existing.Currency).SetTierBasis(existing.TierBasis).
			SetEffectiveFrom(now).SetIsActive(true).Save(ctx)
	default:
		saved, err = tx.ItemPricing.Create().
			SetTenantID(tenantID).SetItemID(itemID).SetPricingTierID(tierID).
			SetPrice(price).SetCurrency("KES").SetEffectiveFrom(now).Save(ctx)
	}
	if err != nil {
		return nil, err
	}
	h.publishPricingUpdatedEventTx(ctx, tx, tenantID, itemID, saved, prevPrice)
	return saved, nil
}

// publishPricingUpdatedEventTx writes an inventory.item.pricing_updated outbox event inside the
// caller's transaction (via platformevents.WriteOutboxTx — the same "single home" transactional
// outbox helper used by stock/transfers/recipes) so it can never be silently lost by a crash
// between the price write and a separate, post-commit event write. previousPrice is nil for a
// brand-new pricing row.
func (h *PricingTierHandler) publishPricingUpdatedEventTx(ctx context.Context, tx *ent.Tx, tenantID, itemID uuid.UUID, p *ent.ItemPricing, previousPrice *float64) {
	platformevents.WriteOutboxTx(ctx, tx, h.log, tenantID, itemID, "inventory", "item.pricing_updated", map[string]any{
		"item_id":         itemID,
		"pricing_tier_id": p.PricingTierID,
		"price":           p.Price,
		"previous_price":  previousPrice,
		"currency":        p.Currency,
		"effective_from":  p.EffectiveFrom,
		"is_active":       p.IsActive,
		"updated_at":      p.UpdatedAt,
	})
}

// bulkItemPriceDTO is a flattened price entry used by downstream services (e.g. pos-api).
// `Price`/`TierCode` carry the DEFAULT tier (back-compat); `Prices` carries EVERY active tier's
// price keyed by tier code (e.g. {"RETAIL":320,"WHOLESALE":290}) so the POS terminal can switch
// pricing profiles client-side without a per-item round-trip.
type bulkItemPriceDTO struct {
	ItemID   uuid.UUID          `json:"item_id"`
	Price    float64            `json:"price"`
	Currency string             `json:"currency"`
	TierCode string             `json:"tier_code"`
	Prices   map[string]float64 `json:"prices"`
}

// ListAllItemPricing returns the default-tier price for every item in the tenant, plus the full
// per-tier price map for each item. Falls back to the first active pricing entry when no default
// tier exists.
// GET /v1/{slug}/inventory/items/pricing
func (h *PricingTierHandler) ListAllItemPricing(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	// Load pricing tiers to identify the default tier.
	tiers, err := h.orm.PricingTier.Query().
		Where(entpt.TenantID(tenantID), entpt.IsActive(true)).
		All(r.Context())
	if err != nil {
		h.log.Error("list pricing tiers failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "Failed to list pricing tiers")
		return
	}
	tierMeta := make(map[uuid.UUID]struct {
		code      string
		isDefault bool
	}, len(tiers))
	for _, t := range tiers {
		tierMeta[t.ID] = struct {
			code      string
			isDefault bool
		}{t.Code, t.IsDefault}
	}

	// Load all active pricing entries for the tenant in one query.
	pricings, err := h.orm.ItemPricing.Query().
		Where(entip.TenantID(tenantID), entip.IsActive(true)).
		All(r.Context())
	if err != nil {
		h.log.Error("list all item pricing failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "Failed to list item pricing")
		return
	}

	// Lazily promote any queued "new_stock_only" price changes whose trigger has fired — a
	// single query scoped to these items, cheap no-op when none of them have a pending row.
	// This is the bulk price-sync path pos-api polls, so it's also where a promotion becomes
	// visible platform-wide. A promotion rewrites ItemPricing rows, so re-load `pricings`
	// afterward rather than building the response from the now-stale snapshot above.
	if h.itemsSvc != nil {
		seen := make(map[uuid.UUID]bool, len(pricings))
		itemIDs := make([]uuid.UUID, 0, len(pricings))
		for _, p := range pricings {
			if !seen[p.ItemID] {
				seen[p.ItemID] = true
				itemIDs = append(itemIDs, p.ItemID)
			}
		}
		h.itemsSvc.PromotePendingPriceChanges(r.Context(), tenantID, itemIDs)
		if pricings2, rerr := h.orm.ItemPricing.Query().
			Where(entip.TenantID(tenantID), entip.IsActive(true)).
			All(r.Context()); rerr == nil {
			pricings = pricings2
		}
	}

	// For each item keep the default-tier entry (fall back to first found) AND accumulate every
	// tier's price into a per-item code→price map.
	type entry struct {
		price     float64
		currency  string
		tierCode  string
		isDefault bool
		prices    map[string]float64
	}
	best := make(map[uuid.UUID]entry, len(pricings))
	for _, p := range pricings {
		meta := tierMeta[p.PricingTierID]
		prev, exists := best[p.ItemID]
		if !exists {
			prev = entry{prices: map[string]float64{}}
		}
		// Record this tier's price in the per-item map (skip blank codes).
		if meta.code != "" {
			prev.prices[meta.code] = p.Price
		}
		// Promote to the default-tier entry when this is the default (or the first seen).
		if !exists || (!prev.isDefault && meta.isDefault) {
			prev.price = p.Price
			prev.currency = p.Currency
			prev.tierCode = meta.code
			prev.isDefault = meta.isDefault
		}
		best[p.ItemID] = prev
	}

	out := make([]bulkItemPriceDTO, 0, len(best))
	for itemID, e := range best {
		out = append(out, bulkItemPriceDTO{
			ItemID:   itemID,
			Price:    e.price,
			Currency: e.currency,
			TierCode: e.tierCode,
			Prices:   e.prices,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// itemPriceDTO is the response for the quantity-aware price resolution endpoint.
type itemPriceDTO struct {
	ItemID     uuid.UUID `json:"item_id"`
	TierID     uuid.UUID `json:"tier_id"`
	TierName   string    `json:"tier_name"`
	TierCode   string    `json:"tier_code"`
	UnitPrice  float64   `json:"unit_price"`
	Currency   string    `json:"currency"`
	Quantity   float64   `json:"quantity"`
	TotalPrice float64   `json:"total_price"`
}

// parseQuantityParam parses a ?quantity= query value into a positive float64, defaulting to 1
// for an empty, invalid, or non-positive input. float64 (not int) because a fractional
// quantity is the normal case for continuous-unit items sold by the ml/kg (e.g. a perfume
// refill) — strconv.Atoi would error on "1.5" and silently fall back to 1, resolving tier
// pricing against the wrong quantity.
func parseQuantityParam(qStr string) float64 {
	if qStr == "" {
		return 1
	}
	q, err := strconv.ParseFloat(qStr, 64)
	if err != nil || q <= 0 {
		return 1
	}
	return q
}

// GetItemPrice resolves the effective price for an item at a given quantity.
// Uses the default pricing tier; falls back to the first active tier if no default exists.
// GET /v1/{slug}/inventory/items/{itemID}/price?quantity=N
func (h *PricingTierHandler) GetItemPrice(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ITEM", "Invalid item ID")
		return
	}

	quantity := parseQuantityParam(r.URL.Query().Get("quantity"))
	// Optional explicit pricing tier (e.g. RETAIL, WHOLESALE); empty = default tier.
	tierCode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("tier")))

	ctx := r.Context()

	// Lazily promote a queued "new_stock_only" price change if its trigger has fired (every
	// pre-change cost layer for this item is now depleted). Cheap no-op when there's no pending
	// row for this item.
	if h.itemsSvc != nil {
		h.itemsSvc.PromotePendingPriceChanges(ctx, tenantID, []uuid.UUID{itemID})
	}

	// Load active pricing tiers to identify the default.
	tiers, err := h.orm.PricingTier.Query().
		Where(entpt.TenantID(tenantID), entpt.IsActive(true)).
		All(ctx)
	if err != nil {
		h.log.Error("GetItemPrice: load tiers failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "QUERY_FAILED", "Failed to load pricing tiers")
		return
	}

	tierMeta := make(map[uuid.UUID]*ent.PricingTier, len(tiers))
	for _, t := range tiers {
		tierMeta[t.ID] = t
	}

	// Load all active pricing entries for this item.
	allPricings, err := h.orm.ItemPricing.Query().
		Where(entip.TenantID(tenantID), entip.ItemID(itemID), entip.IsActive(true)).
		All(ctx)
	if err != nil {
		h.log.Error("GetItemPrice: load pricings failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "QUERY_FAILED", "Failed to load item pricing")
		return
	}

	// Defensive effective-window check: is_active=true rows created via UpsertItemPricing with
	// an explicit future effective_from (scheduled) or a past effective_to shouldn't resolve as
	// the current price yet/anymore, even though the supersede write path keeps exactly one
	// is_active row per (tier, outlet) — a caller-supplied window is still honored here.
	now := time.Now()
	pricings := make([]*ent.ItemPricing, 0, len(allPricings))
	for _, p := range allPricings {
		if p.EffectiveFrom.After(now) {
			continue
		}
		if p.EffectiveTo != nil && !p.EffectiveTo.After(now) {
			continue
		}
		pricings = append(pricings, p)
	}

	if len(pricings) == 0 {
		writeError(w, http.StatusNotFound, "NO_PRICING", "No active pricing found for this item")
		return
	}

	// Prefer a row scoped to the operating outlet (X-Outlet-ID) over an all-outlets row for the
	// SAME tier — previously outlet_id was ignored entirely, so an outlet-specific override could
	// silently lose to whichever row the DB happened to return first. Shared with the bulk
	// resolution path (items.defaultTierPrices) via items.OutletRank/BetterOutletPricing so the
	// two never disagree about which row wins for a given outlet.
	var operatingOutlet *uuid.UUID
	if outletStr := invmiddleware.GetOutletID(ctx); outletStr != "" {
		if oid, perr := uuid.Parse(outletStr); perr == nil {
			operatingOutlet = &oid
		}
	}
	betterCandidate := func(candidate, current *ent.ItemPricing) bool {
		return items.BetterOutletPricing(candidate, current, operatingOutlet)
	}

	var chosen *ent.ItemPricing
	var chosenTier *ent.PricingTier
	// Explicit tier requested → exact (case-insensitive) code match, best outlet candidate.
	if tierCode != "" {
		for _, p := range pricings {
			t := tierMeta[p.PricingTierID]
			if t == nil || !strings.EqualFold(t.Code, tierCode) {
				continue
			}
			if betterCandidate(p, chosen) {
				chosen, chosenTier = p, t
			}
		}
	}
	// Otherwise (or no match) prefer the default tier, then the first active entry — still
	// outlet-ranked within each tier.
	if chosen == nil {
		for _, p := range pricings {
			t := tierMeta[p.PricingTierID]
			switch {
			case chosen == nil:
				chosen, chosenTier = p, t
			case t != nil && t.IsDefault && (chosenTier == nil || !chosenTier.IsDefault):
				chosen, chosenTier = p, t
			case (chosenTier == nil || t == chosenTier) && betterCandidate(p, chosen):
				chosen, chosenTier = p, t
			}
		}
	}

	dto := itemPriceDTO{
		ItemID:     itemID,
		UnitPrice:  chosen.Price,
		Currency:   chosen.Currency,
		Quantity:   quantity,
		TotalPrice: chosen.Price * quantity,
	}
	if chosenTier != nil {
		dto.TierID = chosenTier.ID
		dto.TierName = chosenTier.Name
		dto.TierCode = chosenTier.Code
	} else {
		dto.TierID = chosen.PricingTierID
	}

	writeJSON(w, http.StatusOK, dto)
}

func toPricingTierDTO(t *ent.PricingTier) pricingTierDTO {
	return pricingTierDTO{
		ID:          t.ID,
		TenantID:    t.TenantID,
		Name:        t.Name,
		Code:        t.Code,
		Description: t.Description,
		IsDefault:   t.IsDefault,
		IsActive:    t.IsActive,
		SortOrder:   t.SortOrder,
		CreatedAt:   t.CreatedAt,
	}
}

// toItemPricingDTO converts one ItemPricing row. tiersByID is an optional lookup (built once
// per request by the caller, see pricingTierNamesByID) resolving PricingTierID to its real
// Name/Code — ItemPricing has no ent edge to PricingTier (a plain UUID field), so without this
// the frontend has nothing to render but the raw tier UUID (a real, long-standing UX bug: the
// Catalog item detail page's "Price profiles" table showed e.g.
// "3e0f6434-2f1f-4c94-a99f-ce5c2bc09814 (BOI ENTERPRISES)" instead of "Retail (BOI ENTERPRISES)",
// reported live 2026-09-07). tiersByID may be nil; a missing entry just leaves TierName/TierCode
// empty, same as before this fix.
func toItemPricingDTO(p *ent.ItemPricing, tiersByID map[uuid.UUID]*ent.PricingTier) itemPricingDTO {
	dto := itemPricingDTO{
		ID:            p.ID,
		ItemID:        p.ItemID,
		PricingTierID: p.PricingTierID,
		Price:         p.Price,
		Currency:      p.Currency,
		OutletID:      p.OutletID,
		TierBasis:     string(p.TierBasis),
		EffectiveFrom: p.EffectiveFrom,
		IsActive:      p.IsActive,
	}
	if t := tiersByID[p.PricingTierID]; t != nil {
		dto.TierName = t.Name
		dto.TierCode = t.Code
	}
	if p.EffectiveTo != nil {
		dto.EffectiveTo = p.EffectiveTo
	}
	return dto
}

// pricingTierNamesByID batch-resolves every distinct tier id referenced by ids into its
// PricingTier row, in one query — avoids an N+1 lookup per pricing row.
func pricingTierNamesByID(ctx context.Context, orm *ent.Client, tenantID uuid.UUID, ids []uuid.UUID) map[uuid.UUID]*ent.PricingTier {
	out := map[uuid.UUID]*ent.PricingTier{}
	if len(ids) == 0 {
		return out
	}
	tiers, err := orm.PricingTier.Query().Where(entpt.TenantID(tenantID), entpt.IDIn(ids...)).All(ctx)
	if err != nil {
		return out
	}
	for _, t := range tiers {
		out[t.ID] = t
	}
	return out
}
