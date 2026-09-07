package handlers

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/inventory-service/internal/audit"
	"github.com/bengobox/inventory-service/internal/ent"
	entbalance "github.com/bengobox/inventory-service/internal/ent/inventorybalance"
	entitem "github.com/bengobox/inventory-service/internal/ent/item"
	entcount "github.com/bengobox/inventory-service/internal/ent/stockcount"
	entcountline "github.com/bengobox/inventory-service/internal/ent/stockcountline"
	enttemplate "github.com/bengobox/inventory-service/internal/ent/stockcounttemplate"
	entwarehouse "github.com/bengobox/inventory-service/internal/ent/warehouse"
	invmiddleware "github.com/bengobox/inventory-service/internal/http/middleware"
	"github.com/bengobox/inventory-service/internal/modules/approvals"
	"github.com/bengobox/inventory-service/internal/modules/documents"
	"github.com/bengobox/inventory-service/internal/modules/rbac"
	"github.com/bengobox/inventory-service/internal/modules/stock"
)

// countableTypes are the item types that physically hold stock and therefore
// belong on a count sheet. RECIPE/SERVICE/VOUCHER items never track their own
// stock (a recipe's INGREDIENTS are what gets counted), so they are excluded
// from snapshot/template seeding and rejected on manual/scan add.
var countableTypes = []entitem.Type{entitem.TypeGOODS, entitem.TypeINGREDIENT, entitem.TypeEQUIPMENT}

// StockCountHandler manages cycle / physical stock counts and posts approved
// variance adjustments (reusing stock.Service.AdjustStock).
type StockCountHandler struct {
	log         *zap.Logger
	orm         *ent.Client
	stockSvc    *stock.Service
	rbacSvc     *rbac.Service
	auditSvc    *audit.Service
	approvalSvc *approvals.Service
	docSvc      *documents.Service
}

// NewStockCountHandler constructs a stock-count handler.
func NewStockCountHandler(log *zap.Logger, orm *ent.Client, stockSvc *stock.Service, rbacSvc *rbac.Service, auditSvc *audit.Service) *StockCountHandler {
	return &StockCountHandler{log: log.Named("stock_count.handler"), orm: orm, stockSvc: stockSvc, rbacSvc: rbacSvc, auditSvc: auditSvc}
}

// SetApprovalService wires the approval engine so a configured stock_count ApprovalRule gates the
// variance-posting Approve step. Optional — with no service (or no matching rule) the count's own
// RBAC-gated review→approve lifecycle is the only control (auto-approve by default).
func (h *StockCountHandler) SetApprovalService(a *approvals.Service) { h.approvalSvc = a }

// SetDocService wires tenant branding + document numbering: branded count-sheet/variance-report
// PDFs, and a minted reference for a count created without one. Optional — with no service the
// count still works, it just carries whatever reference the caller supplied and cannot render.
func (h *StockCountHandler) SetDocService(d *documents.Service) { h.docSvc = d }

// RegisterRoutes wires stock-count routes onto the inventory group.
func (h *StockCountHandler) RegisterRoutes(r chi.Router) {
	perm := func(code string) func(http.Handler) http.Handler {
		if h.rbacSvc == nil {
			return func(next http.Handler) http.Handler { return next }
		}
		return invmiddleware.RequirePermission(h.rbacSvc, h.log, code)
	}
	// Stock take is tier-gated (use-case PowerSuite: hosp/duka tier 2+, dawa tier 3).
	featGate := authclient.RequireFeatureCode("stock_take")
	r.Route("/inventory/stock-counts", func(sc chi.Router) {
		sc.Get("/", h.List)
		sc.With(featGate, perm(rbac.PermStockCountAdd)).Post("/", h.Create)
		sc.Get("/{id}", h.Get)
		sc.Get("/{id}/pdf", h.GenerateStockCountPDF)
		sc.With(featGate, perm(rbac.PermStockCountChange)).Post("/{id}/lines", h.UpsertLine)
		sc.With(featGate, perm(rbac.PermStockCountChange)).Post("/{id}/submit", h.Submit)
		sc.With(featGate, perm(rbac.PermStockCountApprove)).Post("/{id}/approve", h.Approve)
		sc.With(featGate, perm(rbac.PermStockCountChange)).Post("/{id}/cancel", h.Cancel)
	})
	// Reusable department count sheets (kitchen daily sheet, barista counter, …).
	// Configuring sheets is a supervisor task (change perm); anyone who can count can list them.
	r.Route("/inventory/stock-count-templates", func(tc chi.Router) {
		tc.Get("/", h.ListTemplates)
		tc.With(featGate, perm(rbac.PermStockCountChange)).Post("/", h.CreateTemplate)
		tc.With(featGate, perm(rbac.PermStockCountChange)).Put("/{id}", h.UpdateTemplate)
		tc.With(featGate, perm(rbac.PermStockCountChange)).Delete("/{id}", h.DeleteTemplate)
	})
}

type createCountRequest struct {
	WarehouseID uuid.UUID `json:"warehouse_id"`
	Reference   string    `json:"reference,omitempty"`
	Snapshot    bool      `json:"snapshot"` // pre-populate lines from current balances
	// TemplateID starts the count from a department sheet: only the template's items
	// (explicit + category members) are pre-populated, each at its current system
	// on-hand — the "expected closing stock". Overrides Snapshot.
	TemplateID *uuid.UUID `json:"template_id,omitempty"`
}

// Create handles POST /inventory/stock-counts.
func (h *StockCountHandler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	var req createCountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	ctx := r.Context()

	// Template-driven count: the department sheet supplies the item set and (when the
	// request doesn't name one) the location; the reference defaults to the sheet name.
	var tpl *ent.StockCountTemplate
	if req.TemplateID != nil {
		var tErr error
		tpl, tErr = h.orm.StockCountTemplate.Query().
			Where(enttemplate.ID(*req.TemplateID), enttemplate.TenantID(tenantID)).
			Only(ctx)
		if tErr != nil {
			writeError(w, http.StatusNotFound, "TEMPLATE_NOT_FOUND", "Count sheet template not found")
			return
		}
		if req.WarehouseID == uuid.Nil && tpl.WarehouseID != nil {
			req.WarehouseID = *tpl.WarehouseID
		}
		if req.Reference == "" {
			req.Reference = tpl.Name + " · " + time.Now().Format("2006-01-02 15:04")
		}
	}
	// No explicit warehouse (and none from a template) → default to the operating outlet's own
	// warehouse so the count — and the adjustments its approval posts — land where that outlet's
	// POS terminal reads stock, not on the tenant default warehouse.
	if req.WarehouseID == uuid.Nil {
		if oid := operatingOutletID(r); oid != uuid.Nil {
			if wh, e := h.orm.Warehouse.Query().
				Where(entwarehouse.TenantID(tenantID), entwarehouse.OutletIDEQ(oid), entwarehouse.IsActive(true)).
				First(ctx); e == nil {
				req.WarehouseID = wh.ID
			}
		}
	}
	if req.WarehouseID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "warehouse_id is required")
		return
	}
	// A count raised without a human reference ("CYCLE-2026-06", a sheet name) gets a minted
	// document number so its printed sheet and variance report have a stable identifier. Never
	// overwrites a reference the caller (or a template) already supplied; best-effort, since a
	// numbering failure must not block the count.
	if strings.TrimSpace(req.Reference) == "" && h.docSvc != nil {
		if n, e := h.docSvc.Seq().GenerateNumber(ctx, tenantID, documents.DocTypeStockCount); e == nil && n != "" {
			req.Reference = n
		}
	}

	b := h.orm.StockCount.Create().
		SetTenantID(tenantID).
		SetWarehouseID(req.WarehouseID).
		SetStatus("counting")
	if req.Reference != "" {
		b = b.SetReference(req.Reference)
	}
	if claims, ok := authclient.ClaimsFromContext(ctx); ok {
		if uid, e := claims.UserID(); e == nil {
			b = b.SetCountedBy(uid)
		}
	}
	count, err := b.Save(ctx)
	if err != nil {
		h.log.Error("create stock count failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "Failed to create count")
		return
	}

	switch {
	case tpl != nil:
		// Seed exactly the sheet's items: explicit item_ids plus every active item in
		// its categories, each snapshotted at the warehouse's current on-hand (0 when
		// no balance row — the sheet still lists it so staff record the physical count).
		idSet := map[uuid.UUID]struct{}{}
		for _, id := range tpl.ItemIds {
			idSet[id] = struct{}{}
		}
		if len(tpl.CategoryIds) > 0 {
			catItems, _ := h.orm.Item.Query().
				Where(
					entitem.TenantID(tenantID),
					entitem.IsActive(true),
					entitem.CategoryIDIn(tpl.CategoryIds...),
					entitem.TypeIn(countableTypes...),
				).
				IDs(ctx)
			for _, id := range catItems {
				idSet[id] = struct{}{}
			}
		}
		ids := make([]uuid.UUID, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			// TypeIn also sanitizes explicit item_ids — a sheet that references a
			// RECIPE/SERVICE item (or an item later converted to one) never seeds it.
			itms, _ := h.orm.Item.Query().
				Where(entitem.TenantID(tenantID), entitem.IDIn(ids...), entitem.TypeIn(countableTypes...)).
				All(ctx)
			onHand := map[uuid.UUID]float64{}
			bals, _ := h.orm.InventoryBalance.Query().
				Where(
					entbalance.TenantID(tenantID),
					entbalance.WarehouseID(req.WarehouseID),
					entbalance.ItemIDIn(ids...),
				).
				All(ctx)
			for _, bal := range bals {
				onHand[bal.ItemID] = bal.OnHand
			}
			for _, it := range itms {
				_, _ = h.orm.StockCountLine.Create().
					SetTenantID(tenantID).
					SetStockCountID(count.ID).
					SetItemID(it.ID).
					SetSku(it.Sku).
					SetSystemQty(onHand[it.ID]).
					Save(ctx)
			}
		}
	case req.Snapshot:
		balances, _ := h.orm.InventoryBalance.Query().
			Where(entbalance.TenantID(tenantID), entbalance.WarehouseID(req.WarehouseID)).
			All(ctx)
		itemIDs := make([]uuid.UUID, 0, len(balances))
		for _, bal := range balances {
			itemIDs = append(itemIDs, bal.ItemID)
		}
		// Only stock-tracked types are countable — legacy/phantom balance rows for
		// RECIPE items (menu entries hold no stock of their own) never seed a line.
		skuByID := make(map[uuid.UUID]string, len(itemIDs))
		if len(itemIDs) > 0 {
			itms, _ := h.orm.Item.Query().
				Where(entitem.TenantID(tenantID), entitem.IDIn(itemIDs...), entitem.TypeIn(countableTypes...)).
				All(ctx)
			for _, it := range itms {
				skuByID[it.ID] = it.Sku
			}
		}
		for _, bal := range balances {
			if _, countable := skuByID[bal.ItemID]; !countable {
				continue
			}
			_, _ = h.orm.StockCountLine.Create().
				SetTenantID(tenantID).
				SetStockCountID(count.ID).
				SetItemID(bal.ItemID).
				SetSku(skuByID[bal.ItemID]).
				SetSystemQty(bal.OnHand).
				Save(ctx)
		}
	}
	writeJSON(w, http.StatusCreated, countDTO(count))
}

type upsertLineRequest struct {
	ItemID     uuid.UUID `json:"item_id"`
	Sku        string    `json:"sku"`
	SystemQty  *float64  `json:"system_qty,omitempty"`
	CountedQty float64   `json:"counted_qty"`
	// Reason classifies the variance for posting (wastage → damaged, pilferage →
	// shrinkage, surplus → found, …). Empty keeps/clears to plain count_variance.
	Reason *string `json:"reason,omitempty"`
}

// varianceReasons are the StockAdjustment reasons a count line's variance may be
// classified as during review. count_variance is the default when unclassified.
var varianceReasons = map[string]bool{
	"damaged": true, "expired": true, "shrinkage": true, "found": true,
	"correction": true, "count_variance": true, "other": true,
}

// UpsertLine handles POST /inventory/stock-counts/{id}/lines — record a counted quantity.
func (h *StockCountHandler) UpsertLine(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	countID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid count id")
		return
	}
	var req upsertLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ItemID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "item_id is required")
		return
	}
	ctx := r.Context()
	// Countable types only: a RECIPE/SERVICE/VOUCHER item holds no stock of its own,
	// so counting it is meaningless (its INGREDIENTS are what gets counted). Reject
	// with guidance instead of silently recording an uncorrectable variance.
	itm, itemErr := h.orm.Item.Query().
		Where(entitem.ID(req.ItemID), entitem.TenantID(tenantID)).
		Only(ctx)
	if itemErr != nil {
		writeError(w, http.StatusNotFound, "ITEM_NOT_FOUND", "Item not found")
		return
	}
	countable := false
	for _, t := range countableTypes {
		if itm.Type == t {
			countable = true
			break
		}
	}
	if !countable {
		writeError(w, http.StatusBadRequest, "NOT_COUNTABLE",
			itm.Name+" is a "+strings.ToLower(string(itm.Type))+" item — it doesn't hold stock of its own, so it can't be counted. Count its ingredients/goods instead.")
		return
	}

	existing, _ := h.orm.StockCountLine.Query().
		Where(entcountline.StockCountID(countID), entcountline.ItemID(req.ItemID)).
		Only(ctx)

	sysQty := 0.0
	if req.SystemQty != nil {
		sysQty = *req.SystemQty
	} else if existing != nil {
		sysQty = existing.SystemQty
	} else if count, cErr := h.orm.StockCount.Query().
		Where(entcount.ID(countID), entcount.TenantID(tenantID)).
		Only(ctx); cErr == nil {
		// New line without an explicit snapshot qty (scan-added mid-count): snapshot the
		// warehouse balance now so the variance is measured against something real.
		if bal, bErr := h.orm.InventoryBalance.Query().
			Where(
				entbalance.TenantID(tenantID),
				entbalance.WarehouseID(count.WarehouseID),
				entbalance.ItemID(req.ItemID),
			).
			First(ctx); bErr == nil {
			sysQty = bal.OnHand
		}
	}
	// Counts support fractional units (kg/L) — keep 4 dp, matching balance precision.
	round4 := func(v float64) float64 { return math.Round(v*10000) / 10000 }
	countedQty := round4(req.CountedQty)
	variance := round4(countedQty - sysQty)

	reason := ""
	if req.Reason != nil {
		reason = strings.ToLower(strings.TrimSpace(*req.Reason))
		if reason != "" && !varianceReasons[reason] {
			writeError(w, http.StatusBadRequest, "INVALID_REASON", "reason must be one of: damaged, expired, shrinkage, found, correction, count_variance, other")
			return
		}
	}

	if existing != nil {
		upd := existing.Update().SetCountedQty(countedQty).SetVariance(variance)
		if req.Reason != nil {
			upd = upd.SetReason(reason)
		}
		_, err = upd.Save(ctx)
	} else {
		create := h.orm.StockCountLine.Create().
			SetTenantID(tenantID).
			SetStockCountID(countID).
			SetItemID(req.ItemID).
			SetSku(req.Sku).
			SetSystemQty(sysQty).
			SetCountedQty(countedQty).
			SetVariance(variance)
		if reason != "" {
			create = create.SetReason(reason)
		}
		_, err = create.Save(ctx)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "Failed to save line")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "variance": variance})
}

// Submit handles POST /inventory/stock-counts/{id}/submit — counting → review.
func (h *StockCountHandler) Submit(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, "review", nil)
}

// Cancel handles POST /inventory/stock-counts/{id}/cancel.
func (h *StockCountHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, "cancelled", nil)
}

// Approve handles POST /inventory/stock-counts/{id}/approve — posts variance
// adjustments for every counted line and marks the count approved.
func (h *StockCountHandler) Approve(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	countID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid count id")
		return
	}
	ctx := r.Context()
	count, err := h.orm.StockCount.Query().Where(entcount.ID(countID), entcount.TenantID(tenantID)).Only(ctx)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Count not found")
		return
	}
	if count.Status == "approved" || count.Status == "cancelled" {
		writeError(w, http.StatusBadRequest, "INVALID_STATE", "Count is already "+string(count.Status))
		return
	}

	var approver uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(ctx); ok {
		approver, _ = claims.UserID()
	}

	lines, _ := h.orm.StockCountLine.Query().
		Where(entcountline.StockCountID(countID), entcountline.Posted(false)).
		All(ctx)

	// Approval-engine gate: a configured stock_count ApprovalRule routes the variance posting
	// through the workflow, keyed on the count itself (objectID = countID). With no matching rule
	// the count's own RBAC review→approve step is authoritative (auto-approve — the default), so a
	// tenant that has not configured a rule is never blocked. The gate amount is the total absolute
	// variance being posted (net offsets don't mask a large exposure).
	if h.approvalSvc != nil {
		gateAmount := 0.0
		for _, ln := range lines {
			if ln.CountedQty == nil || ln.Variance == nil {
				continue
			}
			if v := *ln.Variance; v < 0 {
				gateAmount -= v
			} else {
				gateAmount += v
			}
		}
		ok, state, _ := h.approvalSvc.Satisfied(ctx, tenantID, "stock_count", countID, gateAmount)
		if !ok {
			// A rule exists but the count isn't approved yet: create the request on first sight,
			// then report it as pending so the configured approver(s) can act.
			if state == "not_submitted" {
				if _, gated, _ := h.approvalSvc.Submit(ctx, tenantID, "stock_count", countID, "stock-count:"+countID.String(), gateAmount, &approver); gated {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
						"approval_required": true, "module": "stock_count", "object_id": countID, "state": "pending",
					})
					return
				}
				// Submit reported no rule after all (raced/removed) — fall through and post.
			} else {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error": "STOCK_COUNT_APPROVAL_" + strings.ToUpper(state), "approval_required": true,
					"module": "stock_count", "object_id": countID, "state": state,
				})
				return
			}
		}
	}
	// Post every unposted variance line (and finalize the count when all post). Shared with
	// the approval-execution path so a matrix-approved count posts identically — the Posted
	// flag keeps it idempotent across the RBAC approve and the approval-matrix sign-off.
	result, err := h.stockSvc.PostStockCountVariances(ctx, tenantID, countID, approver)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "Failed to post count variances")
		return
	}
	posted, totalVar := result.Posted, result.TotalVariance

	// All-or-nothing: the count is marked approved only once EVERY variance line has posted. Posted
	// lines are flagged Posted(true), so re-invoking Approve is idempotent (it skips them) — the
	// operator fixes the failed SKUs and re-approves to post the remainder, instead of the count
	// being marked approved with some variances silently dropped.
	if len(result.FailedSKUs) > 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":         "partial",
			"posted_lines":   posted,
			"failed_skus":    result.FailedSKUs,
			"total_variance": totalVar,
			"message":        "Some variance lines could not post; resolve them and approve again to finish.",
		})
		return
	}

	updated, err := h.orm.StockCount.Query().Where(entcount.ID(countID), entcount.TenantID(tenantID)).Only(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "Failed to load approved count")
		return
	}

	if h.auditSvc != nil {
		oid := count.WarehouseID
		amt := totalVar
		h.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tenantID,
			OutletID:    &oid,
			ActorUserID: approver,
			ApproverID:  &approver,
			Action:      "stock.count_approved",
			EntityType:  "stock_count",
			EntityID:    countID.String(),
			Amount:      &amt,
			After:       map[string]any{"posted_lines": posted, "total_variance": totalVar},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": countDTO(updated), "posted_lines": posted, "total_variance": totalVar})
}

func (h *StockCountHandler) transition(w http.ResponseWriter, r *http.Request, status string, _ any) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	countID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid count id")
		return
	}
	n, err := h.orm.StockCount.Update().
		Where(entcount.ID(countID), entcount.TenantID(tenantID)).
		SetStatus(entcount.Status(status)).
		Save(r.Context())
	if err != nil || n == 0 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Count not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

// List handles GET /inventory/stock-counts.
func (h *StockCountHandler) List(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	q := h.orm.StockCount.Query().Where(entcount.TenantID(tenantID))
	if s := r.URL.Query().Get("status"); s != "" {
		q = q.Where(entcount.StatusEQ(entcount.Status(s)))
	}
	rows, err := q.Order(ent.Desc(entcount.FieldCreatedAt)).Limit(200).All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "QUERY_FAILED", "Failed to list counts")
		return
	}
	countIDs := make([]uuid.UUID, 0, len(rows))
	for _, c := range rows {
		countIDs = append(countIDs, c.ID)
	}
	summaries := h.varianceSummaryByCount(r.Context(), countIDs)

	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		dto := countDTO(c)
		if vc, ok := summaries[c.ID]; ok {
			dto["pending_lines"] = vc.pending
			dto["positive_lines"] = vc.positive
			dto["negative_lines"] = vc.negative
			dto["matched_lines"] = vc.matched
			dto["total_lines"] = vc.pending + vc.positive + vc.negative + vc.matched
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// varianceCounts is a per-stock-count line-status breakdown — pending (not yet counted), positive
// (surplus), negative (shortage), matched (counted, exact match). Powers the Stock Take list
// page's Variance column and the detail page's pending/positive/negative filter pills.
type varianceCounts struct{ pending, positive, negative, matched int }

// varianceSummaryByCount computes varianceCounts for every id in countIDs in ONE query (only the
// 3 columns needed, never the full line rows) — avoids an N+1 per row on the list page. Best-effort:
// a query failure returns an empty map, so the list still renders (just without the summary),
// matching this handler's existing fail-soft style elsewhere (e.g. item enrichment in Get).
func (h *StockCountHandler) varianceSummaryByCount(ctx context.Context, countIDs []uuid.UUID) map[uuid.UUID]varianceCounts {
	out := map[uuid.UUID]varianceCounts{}
	if len(countIDs) == 0 {
		return out
	}
	lines, err := h.orm.StockCountLine.Query().
		Where(entcountline.StockCountIDIn(countIDs...)).
		Select(entcountline.FieldStockCountID, entcountline.FieldCountedQty, entcountline.FieldVariance).
		All(ctx)
	if err != nil {
		h.log.Warn("variance summary query failed — list will render without it", zap.Error(err))
		return out
	}
	for _, ln := range lines {
		vc := out[ln.StockCountID]
		switch {
		case ln.CountedQty == nil:
			vc.pending++
		case ln.Variance != nil && *ln.Variance > 0:
			vc.positive++
		case ln.Variance != nil && *ln.Variance < 0:
			vc.negative++
		default:
			vc.matched++
		}
		out[ln.StockCountID] = vc
	}
	return out
}

// Get handles GET /inventory/stock-counts/{id} with its lines.
func (h *StockCountHandler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	countID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid count id")
		return
	}
	ctx := r.Context()
	count, err := h.orm.StockCount.Query().Where(entcount.ID(countID), entcount.TenantID(tenantID)).Only(ctx)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Count not found")
		return
	}
	lines, _ := h.orm.StockCountLine.Query().Where(entcountline.StockCountID(countID)).All(ctx)

	// Enrich lines with the item's name, barcode and stock unit so the count sheet
	// reads naturally ("Sugar · INGR-SUGAR · kg") and hardware/camera scans can be
	// matched client-side against barcode OR sku without extra lookups.
	itemIDs := make([]uuid.UUID, 0, len(lines))
	for _, ln := range lines {
		itemIDs = append(itemIDs, ln.ItemID)
	}
	type itemInfo struct {
		name, barcode, unit, itemType, categoryName string
	}
	infoByID := make(map[uuid.UUID]itemInfo, len(itemIDs))
	if len(itemIDs) > 0 {
		itms, _ := h.orm.Item.Query().
			Where(entitem.TenantID(tenantID), entitem.IDIn(itemIDs...)).
			WithUnits().
			WithItemCategory().
			All(ctx)
		for _, it := range itms {
			info := itemInfo{name: it.Name, barcode: it.Barcode, itemType: string(it.Type)}
			if it.Edges.Units != nil {
				info.unit = it.Edges.Units.Abbreviation
			}
			if it.Edges.ItemCategory != nil {
				info.categoryName = it.Edges.ItemCategory.Name
			}
			infoByID[it.ID] = info
		}
	}

	lineOut := make([]map[string]any, 0, len(lines))
	for _, ln := range lines {
		info := infoByID[ln.ItemID]
		lineOut = append(lineOut, map[string]any{
			"id": ln.ID, "item_id": ln.ItemID, "sku": ln.Sku,
			"item_name": info.name, "barcode": info.barcode, "unit": info.unit,
			"item_type": info.itemType, "category_name": info.categoryName,
			"system_qty": ln.SystemQty, "counted_qty": ln.CountedQty, "variance": ln.Variance,
			"reason": ln.Reason, "posted": ln.Posted,
		})
	}
	// Stable, human order: by item name (falling back to SKU) so the sheet matches how
	// shelves are walked, not insertion order.
	sort.Slice(lineOut, func(i, j int) bool {
		ni, _ := lineOut[i]["item_name"].(string)
		nj, _ := lineOut[j]["item_name"].(string)
		if ni == "" {
			ni, _ = lineOut[i]["sku"].(string)
		}
		if nj == "" {
			nj, _ = lineOut[j]["sku"].(string)
		}
		return strings.ToLower(ni) < strings.ToLower(nj)
	})
	dto := countDTO(count)
	dto["lines"] = lineOut
	writeJSON(w, http.StatusOK, dto)
}

func countDTO(c *ent.StockCount) map[string]any {
	return map[string]any{
		"id": c.ID, "warehouse_id": c.WarehouseID, "reference": c.Reference,
		"status": c.Status, "counted_by": c.CountedBy, "approved_by": c.ApprovedBy,
		"approved_at": c.ApprovedAt, "created_at": c.CreatedAt,
	}
}

// ── Count-sheet templates (department stock sheets) ─────────────────────────────

type templateRequest struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	WarehouseID *uuid.UUID  `json:"warehouse_id,omitempty"`
	ItemIDs     []uuid.UUID `json:"item_ids,omitempty"`
	CategoryIDs []uuid.UUID `json:"category_ids,omitempty"`
	IsActive    *bool       `json:"is_active,omitempty"`
}

func templateDTO(t *ent.StockCountTemplate) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "description": t.Description,
		"warehouse_id": t.WarehouseID, "item_ids": t.ItemIds, "category_ids": t.CategoryIds,
		"is_active": t.IsActive, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
	}
}

// ListTemplates handles GET /inventory/stock-count-templates.
func (h *StockCountHandler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	rows, err := h.orm.StockCountTemplate.Query().
		Where(enttemplate.TenantID(tenantID)).
		Order(ent.Asc(enttemplate.FieldName)).
		All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "QUERY_FAILED", "Failed to list count sheets")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, templateDTO(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// CreateTemplate handles POST /inventory/stock-count-templates.
func (h *StockCountHandler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	var req templateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "name is required")
		return
	}
	if len(req.ItemIDs) == 0 && len(req.CategoryIDs) == 0 {
		writeError(w, http.StatusBadRequest, "EMPTY_SHEET", "Pick at least one item or category for the sheet")
		return
	}
	b := h.orm.StockCountTemplate.Create().
		SetTenantID(tenantID).
		SetName(strings.TrimSpace(req.Name)).
		SetDescription(req.Description).
		SetItemIds(req.ItemIDs).
		SetCategoryIds(req.CategoryIDs).
		SetNillableWarehouseID(req.WarehouseID)
	t, err := b.Save(r.Context())
	if err != nil {
		if ent.IsConstraintError(err) {
			writeError(w, http.StatusConflict, "DUPLICATE_NAME", "A count sheet with this name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "Failed to create count sheet")
		return
	}
	writeJSON(w, http.StatusCreated, templateDTO(t))
}

// UpdateTemplate handles PUT /inventory/stock-count-templates/{id}.
func (h *StockCountHandler) UpdateTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid template id")
		return
	}
	var req templateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	ctx := r.Context()
	t, err := h.orm.StockCountTemplate.Query().
		Where(enttemplate.ID(id), enttemplate.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Count sheet not found")
		return
	}
	upd := t.Update()
	if strings.TrimSpace(req.Name) != "" {
		upd = upd.SetName(strings.TrimSpace(req.Name))
	}
	upd = upd.SetDescription(req.Description)
	if req.ItemIDs != nil {
		upd = upd.SetItemIds(req.ItemIDs)
	}
	if req.CategoryIDs != nil {
		upd = upd.SetCategoryIds(req.CategoryIDs)
	}
	if req.WarehouseID != nil {
		if *req.WarehouseID == uuid.Nil {
			upd = upd.ClearWarehouseID()
		} else {
			upd = upd.SetWarehouseID(*req.WarehouseID)
		}
	}
	if req.IsActive != nil {
		upd = upd.SetIsActive(*req.IsActive)
	}
	updated, err := upd.Save(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "Failed to update count sheet")
		return
	}
	writeJSON(w, http.StatusOK, templateDTO(updated))
}

// DeleteTemplate handles DELETE /inventory/stock-count-templates/{id}.
func (h *StockCountHandler) DeleteTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid template id")
		return
	}
	n, err := h.orm.StockCountTemplate.Delete().
		Where(enttemplate.ID(id), enttemplate.TenantID(tenantID)).
		Exec(r.Context())
	if err != nil || n == 0 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Count sheet not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
