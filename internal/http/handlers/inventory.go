package handlers

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/inventory-service/internal/audit"
	"github.com/bengobox/inventory-service/internal/ent"
	entwarehouse "github.com/bengobox/inventory-service/internal/ent/warehouse"
	invmiddleware "github.com/bengobox/inventory-service/internal/http/middleware"
	"github.com/bengobox/inventory-service/internal/modules/approvals"
	"github.com/bengobox/inventory-service/internal/modules/bulkjobs"
	"github.com/bengobox/inventory-service/internal/modules/documents"
	"github.com/bengobox/inventory-service/internal/modules/items"
	"github.com/bengobox/inventory-service/internal/modules/modifiers"
	"github.com/bengobox/inventory-service/internal/modules/rbac"
	"github.com/bengobox/inventory-service/internal/modules/recipes"
	"github.com/bengobox/inventory-service/internal/modules/stock"
	"github.com/bengobox/inventory-service/internal/modules/tickets"
	"github.com/bengobox/inventory-service/internal/modules/units"
	"github.com/bengobox/inventory-service/internal/platform/subscriptions"
)

// ItemsServicer defines the contract for item availability and CRUD operations.
type ItemsServicer interface {
	GetStockAvailability(ctx context.Context, tenantID uuid.UUID, sku string) (*items.StockAvailability, error)
	BulkAvailability(ctx context.Context, tenantID uuid.UUID, skus []string) ([]items.StockAvailability, error)
	GetBOMAvailability(ctx context.Context, tenantID uuid.UUID, skus []string) ([]items.BOMAvailabilityResult, error)
	GetInventorySummary(ctx context.Context, tenantID uuid.UUID) (*items.InventorySummary, error)
	StockValuation(ctx context.Context, tenantID, warehouseID uuid.UUID) (*items.StockValuation, error)
	StockDeadstock(ctx context.Context, tenantID uuid.UUID, days int) (*items.DeadstockReport, error)
	StockFastMoving(ctx context.Context, tenantID uuid.UUID, days int) (*items.FastMovingReport, error)
	CreateItem(ctx context.Context, tenantID uuid.UUID, dto items.ItemDTO) (*items.ItemDTO, error)
	UpdateItem(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, dto items.ItemDTO) (*items.ItemDTO, error)
	DeactivateItemBySKU(ctx context.Context, tenantID uuid.UUID, sku string) error
	BulkItemAction(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID, action string) (*items.BulkActionResult, error)
	MarkItemEOL(ctx context.Context, tenantID uuid.UUID, sku string) (*items.ItemDTO, error)
	RestoreItemEOL(ctx context.Context, tenantID uuid.UUID, sku string) (*items.ItemDTO, error)
	HardDeleteItemBySKU(ctx context.Context, tenantID uuid.UUID, sku string) error
	EnsureDefaultPrice(ctx context.Context, tenantID, itemID uuid.UUID, price float64) error
	SetSellingPriceBySKU(ctx context.Context, tenantID uuid.UUID, sku string, price float64) (*items.ItemDTO, error)
	ListItems(ctx context.Context, tenantID uuid.UUID, typeFilter, statusFilter string, limit, offset int, categoryID *uuid.UUID, unitID *uuid.UUID, search string, outletID *uuid.UUID, warehouseID *uuid.UUID, useCase string, tagsFilter ...string) ([]items.ItemDTO, int, error)
	ListItemVariants(ctx context.Context, tenantID, itemID uuid.UUID) ([]items.VariantDTO, error)
	ListEventItems(ctx context.Context, tenantID uuid.UUID, limit, offset int) ([]items.ItemDTO, int, error)
	ListCategories(ctx context.Context, tenantID uuid.UUID) ([]items.CategoryDTO, error)
	ListCategoriesFiltered(ctx context.Context, tenantID uuid.UUID, hasItems, sellableOnly bool) ([]items.CategoryDTO, error)
	CreateCategory(ctx context.Context, tenantID uuid.UUID, dto items.CategoryDTO) (*items.CategoryDTO, error)
	UpdateCategory(ctx context.Context, tenantID, id uuid.UUID, dto items.CategoryDTO) (*items.CategoryDTO, error)
	DeleteCategory(ctx context.Context, tenantID, id uuid.UUID) error
	ListBrands(ctx context.Context, tenantID uuid.UUID) ([]items.BrandDTO, error)
	CreateBrand(ctx context.Context, tenantID uuid.UUID, dto items.BrandDTO) (*items.BrandDTO, error)
	// Multi-image (ItemAsset) management.
	CountItemImages(ctx context.Context, tenantID, itemID uuid.UUID) (int, error)
	ListItemImages(ctx context.Context, tenantID, itemID uuid.UUID) ([]items.ItemImageDTO, error)
	AddItemImage(ctx context.Context, tenantID, itemID uuid.UUID, file multipart.File, header *multipart.FileHeader, setPrimary bool) (*items.ItemImageDTO, error)
	UpdateItemImage(ctx context.Context, tenantID, itemID, imageID uuid.UUID, in items.UpdateItemImageInput) (*items.ItemImageDTO, error)
	DeleteItemImage(ctx context.Context, tenantID, itemID, imageID uuid.UUID) error
}

// StockServicer defines the contract for stock reservation and consumption operations.
type StockServicer interface {
	CreateReservation(ctx context.Context, tenantID uuid.UUID, req stock.ReservationRequest) (*stock.ReservationResponse, error)
	GetReservation(ctx context.Context, tenantID, reservationID uuid.UUID) (*stock.ReservationResponse, error)
	GetReservationsByOrderID(ctx context.Context, tenantID, orderID uuid.UUID) ([]stock.ReservationResponse, error)
	ListReservations(ctx context.Context, tenantID uuid.UUID, filter stock.ReservationListFilter) ([]stock.ReservationSummary, int, error)
	ReleaseReservation(ctx context.Context, tenantID, reservationID uuid.UUID, reason string) error
	ConsumeReservation(ctx context.Context, tenantID, reservationID uuid.UUID) (*stock.ConsumeReservationResponse, error)
	RecordConsumption(ctx context.Context, tenantID uuid.UUID, req stock.ConsumptionRequest) (*stock.ConsumptionResponse, error)
	ReverseConsumption(ctx context.Context, tenantID uuid.UUID, req stock.ReverseConsumptionRequest) (*stock.ReverseConsumptionResponse, error)
	AdjustStock(ctx context.Context, tenantID uuid.UUID, req stock.AdjustStockRequest) (*stock.AdjustStockResponse, error)
	BulkAdjustStock(ctx context.Context, tenantID uuid.UUID, req stock.BulkAdjustStockRequest) (*stock.BulkAdjustStockResult, error)
	RelocateItemLocation(ctx context.Context, tenantID uuid.UUID, req stock.RelocateItemLocationRequest) (*stock.RelocateItemLocationResult, error)
	SetItemOutletMembership(ctx context.Context, tenantID uuid.UUID, req stock.SetItemOutletMembershipRequest) (*stock.SetItemOutletMembershipResult, error)
	Breakdown(ctx context.Context, tenantID uuid.UUID, req stock.BreakdownRequest) (*stock.BreakdownResponse, error)
	ListAdjustments(ctx context.Context, tenantID uuid.UUID, req stock.ListAdjustmentsRequest) ([]stock.StockAdjustmentDTO, int, error)
	ItemStockHistory(ctx context.Context, tenantID uuid.UUID, sku string, f stock.StockHistoryFilter) (*stock.StockHistoryResult, error)
}

// RecipesServicer defines the contract for recipe management.
type RecipesServicer interface {
	ListRecipes(ctx context.Context, tenantID uuid.UUID, limit, offset int, outletID *uuid.UUID) ([]recipes.RecipeDTO, int, error)
	GetRecipe(ctx context.Context, tenantID, id uuid.UUID) (*recipes.RecipeDTO, error)
	CreateRecipe(ctx context.Context, tenantID uuid.UUID, dto recipes.RecipeDTO) (*recipes.RecipeDTO, error)
	UpdateRecipe(ctx context.Context, tenantID uuid.UUID, recipeID uuid.UUID, dto recipes.RecipeDTO) (*recipes.RecipeDTO, error)
	DeleteRecipe(ctx context.Context, tenantID uuid.UUID, recipeID uuid.UUID) error
	GetRecipeBySKU(ctx context.Context, tenantID uuid.UUID, sku string) (*recipes.RecipeDTO, error)
	// RecalculateCostsForIngredient cascades cost recalculation to all recipes using the given ingredient.
	RecalculateCostsForIngredient(ctx context.Context, tenantID, ingredientItemID uuid.UUID) error
	// RecalculateRecipeCosts recomputes total/unit cost for a single recipe from current ingredient prices.
	RecalculateRecipeCosts(ctx context.Context, tenantID, recipeID uuid.UUID) error
	// AuditRecipeUnits lists existing recipe lines whose units cannot deduct stock.
	AuditRecipeUnits(ctx context.Context, tenantID uuid.UUID) ([]recipes.UnitIssue, error)
	// SetSellingPriceByItem updates the linked recipe's selling price (RECIPE items are
	// priced by their recipe at the POS). Returns false when the item has no active recipe.
	SetSellingPriceByItem(ctx context.Context, tenantID, itemID uuid.UUID, price float64) (bool, error)
}

// UnitsServicer defines the contract for unit management.
type UnitsServicer interface {
	ListUnits(ctx context.Context, tenantID uuid.UUID) ([]units.UnitDTO, error)
	CreateUnit(ctx context.Context, tenantID uuid.UUID, dto units.UnitDTO) (*units.UnitDTO, error)
	UpdateUnit(ctx context.Context, tenantID, id uuid.UUID, dto units.UnitDTO) (*units.UnitDTO, error)
	DeleteUnit(ctx context.Context, tenantID, id uuid.UUID) error
}

// ModifiersServicer defines the contract for modifier group/option management.
type ModifiersServicer interface {
	ListAllModifierGroups(ctx context.Context, tenantID uuid.UUID, limit, offset int, search string) ([]modifiers.ModifierGroupDTO, int, error)
	GetModifierGroup(ctx context.Context, tenantID, groupID uuid.UUID) (*modifiers.ModifierGroupDTO, error)
	ListModifierGroups(ctx context.Context, tenantID, itemID uuid.UUID) ([]modifiers.ModifierGroupDTO, error)
	CreateModifierGroup(ctx context.Context, tenantID uuid.UUID, req modifiers.CreateModifierGroupRequest) (*modifiers.ModifierGroupDTO, error)
	UpdateModifierGroup(ctx context.Context, tenantID, groupID uuid.UUID, req modifiers.UpdateModifierGroupRequest) (*modifiers.ModifierGroupDTO, error)
	DeleteModifierGroup(ctx context.Context, tenantID, groupID uuid.UUID) error
	CreateModifierOption(ctx context.Context, tenantID, groupID uuid.UUID, req modifiers.CreateModifierOptionRequest) (*modifiers.ModifierOptionDTO, error)
	UpdateModifierOption(ctx context.Context, tenantID, optionID uuid.UUID, req modifiers.UpdateModifierOptionRequest) (*modifiers.ModifierOptionDTO, error)
	DeleteModifierOption(ctx context.Context, tenantID, optionID uuid.UUID) error
}

// InventoryHandler handles all inventory-related HTTP endpoints.
type InventoryHandler struct {
	log          *zap.Logger
	itemsSvc     ItemsServicer
	stockSvc     StockServicer
	recipeSvc    RecipesServicer
	unitSvc      UnitsServicer
	modifiersSvc ModifiersServicer
	ticketsSvc   *tickets.Service
	docSvc       *documents.Service
	rbacSvc      *rbac.Service
	authMW       *authclient.AuthMiddleware
	auditSvc     *audit.Service
	approvalSvc  *approvals.Service
	bulkJobsSvc  *bulkjobs.Service
	orm          *ent.Client
	pinSecret    []byte // terminal/PIN JWT secret; feature-gated GETs accept PIN sessions too
}

// SetBulkJobsService wires the background bulk-job runner (item relocation/membership, bulk
// stock adjustment) — see internal/modules/bulkjobs.
func (h *InventoryHandler) SetBulkJobsService(b *bulkjobs.Service) { h.bulkJobsSvc = b }

// SetEntClient wires the Ent client used by route-level middleware (e.g. resolving an
// outlet's use_case from its warehouse mirror for RequireOutletUseCase gating).
func (h *InventoryHandler) SetEntClient(c *ent.Client) { h.orm = c }

// SetAuditService wires the centralized audit trail for stock adjustments / write-offs.
func (h *InventoryHandler) SetAuditService(a *audit.Service) { h.auditSvc = a }

// SetApprovalService wires the amount-tiered approval workflow for large adjustments.
func (h *InventoryHandler) SetApprovalService(a *approvals.Service) { h.approvalSvc = a }

// NewInventoryHandler creates a new inventory handler.
func NewInventoryHandler(log *zap.Logger, itemsSvc ItemsServicer, stockSvc StockServicer, recipeSvc RecipesServicer, unitSvc UnitsServicer) *InventoryHandler {
	return &InventoryHandler{
		log:       log.Named("inventory.handler"),
		itemsSvc:  itemsSvc,
		stockSvc:  stockSvc,
		recipeSvc: recipeSvc,
		unitSvc:   unitSvc,
	}
}

// SetRBACService injects the RBAC service for per-route permission enforcement.
// When set, mutation routes require the corresponding inventory.*.{action} permission.
func (h *InventoryHandler) SetRBACService(svc *rbac.Service) {
	h.rbacSvc = svc
}

// SetAuthMiddleware injects the auth middleware so feature-gated GET routes can
// require authentication. The route group skips auth for GETs (to keep public/S2S
// reads open), which means claims are never extracted — so a feature-gated GET like
// /adjustments must opt back into auth explicitly, otherwise RequireFeature sees no
// claims and returns 401 even for logged-in users.
func (h *InventoryHandler) SetAuthMiddleware(mw *authclient.AuthMiddleware) {
	h.authMW = mw
}

// SetTerminalSecret wires the PIN/terminal JWT secret so feature-gated GET routes accept
// terminal (PIN) sessions in addition to SSO sessions.
func (h *InventoryHandler) SetTerminalSecret(secret []byte) { h.pinSecret = secret }

// requireAuthForFeatureGet returns RequireAuth when the auth middleware is wired,
// or a pass-through otherwise (preserving prior behavior in tests / setups without auth).
func (h *InventoryHandler) requireAuthForFeatureGet() func(http.Handler) http.Handler {
	if h.authMW == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	// Accept terminal (PIN) sessions as well as SSO on feature-gated GETs.
	return RequireAnyAuth(h.pinSecret, h.authMW)
}

// SetModifiersService injects the modifiers service (optional; modifier endpoints are skipped if nil).
func (h *InventoryHandler) SetModifiersService(svc ModifiersServicer) {
	h.modifiersSvc = svc
}

// SetTicketsService injects the tickets service (optional; ticket endpoints are skipped if nil).
func (h *InventoryHandler) SetTicketsService(svc *tickets.Service) {
	h.ticketsSvc = svc
}

// SetDocService injects the documents service (tenant branding + numbering) for ticket PDFs.
func (h *InventoryHandler) SetDocService(svc *documents.Service) {
	h.docSvc = svc
}

// parseTenantID is now defined in tenant.go with platform-owner override support.

// RegisterRoutes wires inventory routes onto the given chi.Router.
// When rbacSvc is set (via SetRBACService), mutation routes enforce per-action permissions.
func (h *InventoryHandler) RegisterRoutes(r chi.Router) {
	// perm returns a per-route permission middleware when rbacSvc is set, or a pass-through.
	perm := func(code string) func(http.Handler) http.Handler {
		if h.rbacSvc == nil {
			return func(next http.Handler) http.Handler { return next }
		}
		return invmiddleware.RequirePermission(h.rbacSvc, h.log, code)
	}

	r.Route("/inventory", func(inv chi.Router) {
		// Items
		inv.Get("/items", h.ListItems)
		// Branded PDF/CSV export of the catalog — same filters as the list, reuses the docs
		// report engine (see report_pdf_products.go). Static path, so it never collides with
		// the /items/{sku} wildcard below regardless of registration order.
		inv.Get("/items/export", h.ProductsExportPDF)
		inv.With(perm(rbac.PermItemsAdd)).Post("/items", h.CreateItem)
		inv.Get("/items/{sku}", h.GetStockAvailability)
		inv.With(perm(rbac.PermItemsChange)).Put("/items/{sku}", h.UpdateItem)
		// Targeted price correction (guardrails + tier rows + recipe selling price) —
		// called S2S by pos-api's "also update the catalog price" order-line edit. Idempotency-
		// Key guarded: a retried request must not double-apply a price change.
		inv.With(perm(rbac.PermItemsChange), invmiddleware.Idempotency(h.orm)).Patch("/items/{sku}/price", h.SetItemPrice)
		inv.With(perm(rbac.PermItemsDelete)).Delete("/items/{sku}", h.DeleteItem)
		// Bulk multi-select actions (DataTable): delete keeps the delete permission,
		// status changes (activate/deactivate/not-for-sale) need items.change.
		inv.With(perm(rbac.PermItemsDelete)).Post("/items/bulk-delete", h.BulkDeleteItems)
		inv.With(perm(rbac.PermItemsChange)).Post("/items/bulk-status", h.BulkItemStatus)
		// End-of-Life (EOL): mark hides the item everywhere (is_active=false + end_of_life_at)
		// and schedules it for hard-delete after the retention window; restore un-marks it. The
		// EOL listing itself reuses GET /items?status=eol.
		inv.With(perm(rbac.PermItemsDelete)).Post("/items/{sku}/eol", h.MarkItemEOL)
		inv.With(perm(rbac.PermItemsDelete)).Post("/items/{sku}/eol/restore", h.RestoreItemEOL)
		inv.Get("/items/{itemId}/variants", h.ListItemVariants)

		// Barcode + label printing (single-item PNG read + bulk label-print job).
		h.registerBarcodeRoutes(inv, perm)

		// Item images (multi-image gallery via ItemAsset). List is open (read); mutations
		// require items.change. Upload additionally enforces the multi-image feature + per-item
		// image cap inside the handler (returns 403/402 on lock/overage).
		inv.Get("/items/{itemID}/images", h.ListItemImages)
		inv.With(perm(rbac.PermItemsChange)).Post("/items/{itemID}/images", h.UploadItemImage)
		inv.With(perm(rbac.PermItemsChange)).Patch("/items/{itemID}/images/{imageID}", h.UpdateItemImage)
		inv.With(perm(rbac.PermItemsChange)).Delete("/items/{itemID}/images/{imageID}", h.DeleteItemImage)

		// Availability
		inv.Post("/availability", h.BulkAvailability)
		inv.Get("/availability/bom", h.GetBOMAvailability)

		// Stock adjustments — requires stock_tracking feature
		inv.With(authclient.RequireFeatureCode("stock_tracking"), perm(rbac.PermStockAdd)).Post("/adjust", h.AdjustStock)
		inv.With(authclient.RequireFeatureCode("stock_tracking"), perm(rbac.PermStockAdd)).Post("/stock/bulk-adjust", h.BulkAdjustStock)
		inv.With(authclient.RequireFeatureCode("stock_tracking"), perm(rbac.PermStockChange)).Post("/stock/set-membership", h.SetItemOutletMembership)
		inv.With(authclient.RequireFeatureCode("stock_tracking")).Get("/bulk-jobs/{id}", h.GetBulkJob)
		inv.With(authclient.RequireFeatureCode("stock_tracking"), perm(rbac.PermStockAdd)).Post("/adjustments", h.CreateAdjustment)
		inv.With(authclient.RequireFeatureCode("stock_tracking"), perm(rbac.PermStockChange)).Post("/breakdowns", h.CreateBreakdown)
		// Item location relocation — NOT a stock transfer (see RelocateItemLocation doc comment):
		// moves an item's whole balance between warehouses, no quantity chosen, so PermStockChange
		// (the same two-sided-mutation permission /breakdowns uses) rather than PermStockAdd.
		inv.With(authclient.RequireFeatureCode("stock_tracking"), perm(rbac.PermStockChange)).Post("/stock/relocate", h.RelocateItemLocation)
		// GET is exempt from the group-level auth (public/S2S reads), so opt back into
		// auth here to populate claims before the feature check — otherwise logged-in
		// users hit a spurious 401 "missing claims".
		inv.With(h.requireAuthForFeatureGet(), authclient.RequireFeatureCode("stock_tracking")).Get("/adjustments", h.ListAdjustments)
		inv.With(h.requireAuthForFeatureGet(), authclient.RequireFeatureCode("stock_tracking")).Get("/adjustments/document", h.GenerateStockAdjustmentDocument)
		// Product stock history — unified per-item ledger (adjustments + purchases +
		// sales + returns + transfers) with quantities-in/out summary cards. Same
		// auth/feature gating as the adjustments listing it generalizes.
		inv.With(h.requireAuthForFeatureGet(), authclient.RequireFeatureCode("stock_tracking")).Get("/items/{sku}/stock-history", h.ItemStockHistory)
		inv.With(h.requireAuthForFeatureGet(), authclient.RequireFeatureCode("stock_tracking")).Get("/items/{sku}/stock-history/document", h.GenerateStockHistoryDocument)

		// Categories
		inv.Get("/categories", h.ListCategories)
		inv.With(perm(rbac.PermItemsAdd)).Post("/categories", h.CreateCategory)
		inv.With(perm(rbac.PermItemsChange)).Put("/categories/{categoryID}", h.UpdateCategory)
		inv.With(perm(rbac.PermItemsDelete)).Delete("/categories/{categoryID}", h.DeleteCategory)

		// Reservations
		inv.With(perm(rbac.PermReservationsAdd)).Post("/reservations", h.CreateReservation)
		inv.Get("/reservations", h.GetReservationsByOrder)
		inv.Get("/reservations/{reservationID}", h.GetReservation)
		inv.With(perm(rbac.PermReservationsChange)).Post("/reservations/{reservationID}/release", h.ReleaseReservation)
		inv.With(perm(rbac.PermReservationsChange)).Post("/reservations/{reservationID}/consume", h.ConsumeReservation)

		// Consumption
		inv.With(perm(rbac.PermConsumptionsAdd)).Post("/consumption", h.RecordConsumption)
		// Reversal (S2S from pos-api's txn-reversal tool; same auth semantics as /consumption).
		inv.With(perm(rbac.PermConsumptionsAdd)).Post("/consumption/reverse", h.ReverseConsumption)

		// Summary
		inv.Get("/summary", h.GetInventorySummary)
		inv.Get("/reports/stock-valuation", h.StockValuationReport)
		inv.Get("/reports/stock-valuation.pdf", h.StockValuationReportPDF)
		inv.Get("/reports/deadstock", h.StockDeadstockReport)
		inv.Get("/reports/deadstock.pdf", h.StockDeadstockReportPDF)
		inv.Get("/reports/fast-moving", h.StockFastMovingReport)

		// Recipes / BOM — hospitality & quick_service (menu recipes), warehouse &
		// manufacturing (bills of materials), plus retail (fractional/refill sale
		// pricing e.g. selling a bottled item by the ml, and in-house production
		// e.g. a shop that bakes its own cupcakes). HQ/platform users bypass gating.
		// Retail's actual entitlement to USE recipes/production is enforced by the
		// "manufacturing" subscription feature in inventory-ui (frontend-only, matches
		// this route's existing lack of a feature-code check for every other use_case).
		inv.Group(func(rec chi.Router) {
			// Populate claims first (GET routes skip the group-level auth), then gate by
			// the active outlet's use_case — so the read list is gated for non-HQ users too,
			// not just the mutations.
			rec.Use(h.requireAuthForFeatureGet())
			rec.Use(invmiddleware.RequireOutletUseCase(h.orm, h.log, "hospitality", "quick_service", "warehouse", "manufacturing", "retail"))
			rec.Get("/recipes", h.ListRecipes)
			rec.With(perm(rbac.PermRecipesAdd)).Post("/recipes", h.CreateRecipe)
			rec.Get("/recipes/unit-audit", h.AuditRecipeUnits)
			rec.Get("/recipes/{recipeID}", h.GetRecipe)
			rec.With(perm(rbac.PermRecipesChange)).Put("/recipes/{recipeID}", h.UpdateRecipe)
			rec.With(perm(rbac.PermRecipesChange)).Post("/recipes/{recipeID}/recompute-cost", h.RecomputeRecipeCost)
			rec.With(perm(rbac.PermRecipesDelete)).Delete("/recipes/{recipeID}", h.DeleteRecipe)
		})

		// Events — SERVICE-type items with event_start_at set
		inv.Get("/events", h.ListEventItems)

		// Event ticketing (sell seats with capacity enforcement + check-in).
		// Mutations are tier-gated by events_module (use-case PowerSuite: hospitality Gold);
		// reads + the public PDF stay open so already-sold tickets keep working everywhere.
		if h.ticketsSvc != nil {
			eventsFeat := authclient.RequireFeatureCode("events_module")
			inv.Get("/events/{id}/availability", h.GetEventAvailability)
			// Public branded ticket PDF (with QR) by code — GET, no perm (the code is the secret).
			inv.Get("/tickets/{code}/pdf", h.GetPublicTicketPDF)
			inv.With(perm(rbac.PermTicketsView)).Get("/tickets", h.ListTickets)
			inv.With(perm(rbac.PermTicketsView)).Get("/tickets/{code}", h.GetTicket)
			inv.With(eventsFeat, perm(rbac.PermTicketsAdd)).Post("/tickets", h.CreateTicket)
			inv.With(eventsFeat, perm(rbac.PermTicketsChange)).Post("/tickets/{code}/redeem", h.RedeemTicket)
			inv.With(eventsFeat, perm(rbac.PermTicketsChange)).Post("/tickets/{id}/cancel", h.CancelTicket)
		}

		// Units (manage is platform-only; view is open)
		inv.Get("/units", h.ListUnits)
		inv.With(perm(rbac.PermUnitsAdd)).Post("/units", h.CreateUnit)
		inv.With(perm(rbac.PermUnitsChange)).Put("/units/{unitID}", h.UpdateUnit)
		inv.With(perm(rbac.PermUnitsDelete)).Delete("/units/{unitID}", h.DeleteUnit)

		// CSV bulk import (legacy — items only) — requires bulk_import feature
		inv.With(authclient.RequireFeatureCode("bulk_import"), perm(rbac.PermItemsAdd)).Post("/items/import", h.ImportItems)
		// Multi-format bulk import (CSV/XLSX — items, recipes, modifiers, stock) — requires bulk_import feature
		inv.With(authclient.RequireFeatureCode("bulk_import"), perm(rbac.PermItemsAdd)).Post("/bulk-import", h.BulkImport)
		inv.With(authclient.RequireFeatureCode("bulk_import")).Get("/import-template", h.ImportTemplate)
		// Composite menu-item create: item + recipe + ingredients + modifiers in one call
		inv.With(perm(rbac.PermItemsAdd)).Post("/items/menu-item", h.CreateMenuItemComposite)

		// Modifier Groups & Options
		inv.Get("/modifier-groups", h.ListAllModifierGroups)
		inv.Get("/modifier-groups/{id}", h.GetModifierGroup)
		inv.Get("/items/{itemId}/modifier-groups", h.ListModifierGroups)
		inv.With(perm(rbac.PermVariantsAdd)).Post("/modifier-groups", h.CreateModifierGroup)
		inv.With(perm(rbac.PermVariantsChange)).Put("/modifier-groups/{id}", h.UpdateModifierGroup)
		inv.With(perm(rbac.PermVariantsDelete)).Delete("/modifier-groups/{id}", h.DeleteModifierGroup)
		inv.With(perm(rbac.PermVariantsAdd)).Post("/modifier-groups/{id}/options", h.CreateModifierOption)
		inv.With(perm(rbac.PermVariantsChange)).Put("/modifier-options/{id}", h.UpdateModifierOption)
		inv.With(perm(rbac.PermVariantsDelete)).Delete("/modifier-options/{id}", h.DeleteModifierOption)
	})
}

// RegisterAdminRoutes wires platform-owner-only item admin routes (mirrors WarehouseHandler's
// and ServiceConfigHandler's RegisterAdminRoutes/RegisterPlatformRoutes pattern). The parent
// /admin tree has no {tenant} path segment, so the tenant is an explicit {tenantID} URL param.
func (h *InventoryHandler) RegisterAdminRoutes(r chi.Router) {
	r.Delete("/inventory/tenants/{tenantID}/items/{sku}", h.HardDeleteItemAdmin)
}

// HardDeleteItemAdmin handles DELETE /admin/inventory/tenants/{tenantID}/items/{sku} — a
// platform-owner-only permanent deletion, bypassing the EOL retention window entirely. Refuses
// with 409 when the item carries transactional/usage history that must be preserved for the
// audit trail — the caller should mark it End-of-Life instead and let the retention purge run.
func (h *InventoryHandler) HardDeleteItemAdmin(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	sku := chi.URLParam(r, "sku")
	if sku == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}
	if err := h.itemsSvc.HardDeleteItemBySKU(r.Context(), tenantID, sku); err != nil {
		switch {
		case strings.Contains(err.Error(), "not found"):
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Item not found")
		case strings.Contains(err.Error(), "cannot hard-delete"):
			writeError(w, http.StatusConflict, "HAS_HISTORY", err.Error())
		default:
			h.log.Error("hard delete item failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GetStockAvailability handles GET /v1/{tenant}/inventory/items/{sku}
func (h *InventoryHandler) GetStockAvailability(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	sku := chi.URLParam(r, "sku")
	if sku == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}

	avail, err := h.itemsSvc.GetStockAvailability(r.Context(), tenantID, sku)
	if err != nil {
		h.log.Error("get stock availability failed", zap.Error(err), zap.String("sku", sku))
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, avail)
}

// BulkAvailability handles POST /v1/{tenant}/inventory/availability
func (h *InventoryHandler) BulkAvailability(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req struct {
		SKUs []string `json:"skus"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	if len(req.SKUs) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_SKUS", "At least one SKU is required")
		return
	}

	results, err := h.itemsSvc.BulkAvailability(r.Context(), tenantID, req.SKUs)
	if err != nil {
		h.log.Error("bulk availability failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to check availability")
		return
	}

	writeJSON(w, http.StatusOK, results)
}

// CreateReservation handles POST /v1/{tenant}/inventory/reservations
func (h *InventoryHandler) CreateReservation(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req stock.ReservationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	req.TenantID = tenantID

	if req.OrderID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "MISSING_ORDER_ID", "Order ID is required")
		return
	}

	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_ITEMS", "At least one item is required")
		return
	}

	// Outlet-scope the reservation to the operating outlet's warehouse when the body carries no
	// explicit outlet/warehouse (X-Outlet-ID forwarded by S2S callers). Mirrors RecordConsumption.
	if req.OutletID == uuid.Nil && req.WarehouseID == uuid.Nil {
		if outletStr := invmiddleware.GetOutletID(r.Context()); outletStr != "" {
			if oid, perr := uuid.Parse(outletStr); perr == nil {
				req.OutletID = oid
			}
		}
	}

	result, err := h.stockSvc.CreateReservation(r.Context(), tenantID, req)
	if err != nil {
		h.log.Error("create reservation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "RESERVATION_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, result)
}

// GetReservation handles GET /v1/{tenant}/inventory/reservations/{reservationID}
func (h *InventoryHandler) GetReservation(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	reservationID, err := uuid.Parse(chi.URLParam(r, "reservationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid reservation ID")
		return
	}

	result, err := h.stockSvc.GetReservation(r.Context(), tenantID, reservationID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// GetReservationsByOrder handles GET /v1/{tenant}/inventory/reservations. With an `order_id`
// query param it returns that order's reservations as a raw array (unchanged since this is a
// live S2S contract — pos-api, hospital-api, ordering-backend and Cafe's client all call it this
// way and decode a bare array). Without one, it returns a tenant-wide, paginated list (a new
// capability — this path previously 400'd unconditionally with MISSING_ORDER_ID, which is why
// inventory-ui's Reservations page could never load anything at all).
func (h *InventoryHandler) GetReservationsByOrder(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	if orderIDStr := r.URL.Query().Get("order_id"); orderIDStr != "" {
		orderID, err := uuid.Parse(orderIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ORDER_ID", "Invalid order_id")
			return
		}
		results, err := h.stockSvc.GetReservationsByOrderID(r.Context(), tenantID, orderID)
		if err != nil {
			h.log.Error("get reservations by order failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, results)
		return
	}

	filter := stock.ReservationListFilter{Status: r.URL.Query().Get("status")}
	// The list has no other searchable text field (see ReservationListFilter's doc comment) — a
	// UUID-shaped `search` term is treated as an order-ID lookup, same as passing `order_id`
	// directly, so the page's search box still does something useful for its most common use.
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		if oid, perr := uuid.Parse(search); perr == nil {
			filter.OrderID = &oid
		}
	}
	p := pagination.Parse(r)
	filter.Limit, filter.Offset = p.Limit, p.Offset

	results, total, err := h.stockSvc.ListReservations(r.Context(), tenantID, filter)
	if err != nil {
		h.log.Error("list reservations failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":    results,
		"total":   total,
		"limit":   p.Limit,
		"page":    p.Page,
		"hasMore": p.Offset+len(results) < total,
	})
}

// ReleaseReservation handles POST /v1/{tenant}/inventory/reservations/{reservationID}/release
func (h *InventoryHandler) ReleaseReservation(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	reservationID, err := uuid.Parse(chi.URLParam(r, "reservationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid reservation ID")
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	if err := h.stockSvc.ReleaseReservation(r.Context(), tenantID, reservationID, req.Reason); err != nil {
		h.log.Error("release reservation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "RELEASE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

// ConsumeReservation handles POST /v1/{tenant}/inventory/reservations/{reservationID}/consume
func (h *InventoryHandler) ConsumeReservation(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	reservationID, err := uuid.Parse(chi.URLParam(r, "reservationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid reservation ID")
		return
	}

	resp, err := h.stockSvc.ConsumeReservation(r.Context(), tenantID, reservationID)
	if err != nil {
		h.log.Error("consume reservation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CONSUME_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// RecordConsumption handles POST /v1/{tenant}/inventory/consumption
func (h *InventoryHandler) RecordConsumption(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req stock.ConsumptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	req.TenantID = tenantID

	if req.OrderID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "MISSING_ORDER_ID", "Order ID is required")
		return
	}

	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_ITEMS", "At least one item is required")
		return
	}

	// Outlet-scope the deduction to the selling outlet's own warehouse when the body carries no
	// explicit outlet/warehouse: honour the X-Outlet-ID operating-outlet context (forwarded by
	// S2S callers such as ordering-backend). Without this, a multi-outlet tenant's consumption
	// silently falls back to the tenant-default warehouse and shortfalls — same class of bug as
	// the POS sale backflush. An explicit warehouse_id/outlet_id in the body still wins.
	if req.OutletID == uuid.Nil && req.WarehouseID == uuid.Nil {
		if outletStr := invmiddleware.GetOutletID(r.Context()); outletStr != "" {
			if oid, perr := uuid.Parse(outletStr); perr == nil {
				req.OutletID = oid
			}
		}
	}

	result, err := h.stockSvc.RecordConsumption(r.Context(), tenantID, req)
	if err != nil {
		h.log.Error("record consumption failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CONSUMPTION_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, result)
}

// ReverseConsumption handles POST /v1/{tenant}/inventory/consumption/reverse — the stock
// side of a POS sale reversal (called S2S by pos-api's txn-reversal tool). Returns the
// actually-deducted quantities to the warehouse balance and compensates the utilization
// records; idempotent on idempotency_key, capped so replays never over-return stock.
func (h *InventoryHandler) ReverseConsumption(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req stock.ReverseConsumptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if req.OrderID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "MISSING_ORDER_ID", "Order ID is required")
		return
	}

	result, err := h.stockSvc.ReverseConsumption(r.Context(), tenantID, req)
	if err != nil {
		h.log.Error("reverse consumption failed", zap.Error(err))
		writeError(w, http.StatusUnprocessableEntity, "REVERSAL_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// ListRecipes handles GET /v1/{tenant}/inventory/recipes
func (h *InventoryHandler) ListRecipes(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	sku := r.URL.Query().Get("sku")
	if sku != "" {
		recipe, err := h.recipeSvc.GetRecipeBySKU(r.Context(), tenantID, sku)
		if err != nil {
			if ent.IsNotFound(err) {
				writeJSON(w, http.StatusOK, []recipes.RecipeDTO{})
				return
			}
			h.log.Error("get recipe by sku failed", zap.Error(err), zap.String("sku", sku))
			writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to fetch recipe")
			return
		}
		writeJSON(w, http.StatusOK, []recipes.RecipeDTO{*recipe})
		return
	}

	var recipeOutletID *uuid.UUID
	if outletStr := invmiddleware.GetOutletID(r.Context()); outletStr != "" {
		if oid, err := uuid.Parse(outletStr); err == nil {
			recipeOutletID = &oid
		}
	}

	p := pagination.Parse(r)
	results, total, err := h.recipeSvc.ListRecipes(r.Context(), tenantID, p.Limit, p.Offset, recipeOutletID)
	if err != nil {
		h.log.Error("list recipes failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to list recipes")
		return
	}

	writeJSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// GetRecipe handles GET /v1/{tenant}/inventory/recipes/{recipeID}
func (h *InventoryHandler) GetRecipe(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	recipeID, err := uuid.Parse(chi.URLParam(r, "recipeID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid recipe ID")
		return
	}

	result, err := h.recipeSvc.GetRecipe(r.Context(), tenantID, recipeID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// CreateRecipe handles POST /v1/{tenant}/inventory/recipes
func (h *InventoryHandler) CreateRecipe(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req recipes.RecipeDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	result, err := h.recipeSvc.CreateRecipe(r.Context(), tenantID, req)
	if err != nil {
		var unitErr *recipes.UnitValidationError
		if errors.As(err, &unitErr) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  map[string]string{"code": "UNIT_MISMATCH", "message": unitErr.Error()},
				"issues": unitErr.Issues,
			})
			return
		}
		h.log.Error("create recipe failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, result)
}

// UpdateRecipe handles PUT /v1/{tenant}/inventory/recipes/{recipeID}
func (h *InventoryHandler) UpdateRecipe(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	recipeID, err := uuid.Parse(chi.URLParam(r, "recipeID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid recipe ID")
		return
	}

	var req recipes.RecipeDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	result, err := h.recipeSvc.UpdateRecipe(r.Context(), tenantID, recipeID, req)
	if err != nil {
		var unitErr *recipes.UnitValidationError
		if errors.As(err, &unitErr) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  map[string]string{"code": "UNIT_MISMATCH", "message": unitErr.Error()},
				"issues": unitErr.Issues,
			})
			return
		}
		h.log.Error("update recipe failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// AuditRecipeUnits handles GET /v1/{tenant}/inventory/recipes/unit-audit — lists every
// existing recipe line whose unit cannot deduct stock (legacy cross-dimension data),
// with per-line remediation guidance. Powers the recipes data-quality audit in the UI
// and the tenant remediation reports.
func (h *InventoryHandler) AuditRecipeUnits(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	issues, err := h.recipeSvc.AuditRecipeUnits(r.Context(), tenantID)
	if err != nil {
		h.log.Error("recipe unit audit failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "AUDIT_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues, "count": len(issues)})
}

// DeleteRecipe handles DELETE /v1/{tenant}/inventory/recipes/{recipeID}
func (h *InventoryHandler) DeleteRecipe(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	recipeID, err := uuid.Parse(chi.URLParam(r, "recipeID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid recipe ID")
		return
	}

	if err := h.recipeSvc.DeleteRecipe(r.Context(), tenantID, recipeID); err != nil {
		h.log.Error("delete recipe failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// RecomputeRecipeCost handles POST /v1/{tenant}/inventory/recipes/{recipeID}/recompute-cost —
// recomputes a recipe/BOM's cost from current ingredient prices.
func (h *InventoryHandler) RecomputeRecipeCost(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	recipeID, err := uuid.Parse(chi.URLParam(r, "recipeID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid recipe ID")
		return
	}
	if err := h.recipeSvc.RecalculateRecipeCosts(r.Context(), tenantID, recipeID); err != nil {
		h.log.Error("recompute recipe cost failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "RECOMPUTE_FAILED", err.Error())
		return
	}
	result, err := h.recipeSvc.GetRecipe(r.Context(), tenantID, recipeID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Recipe not found")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// GetInventorySummary handles GET /v1/{tenant}/inventory/summary
func (h *InventoryHandler) GetInventorySummary(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	summary, err := h.itemsSvc.GetInventorySummary(r.Context(), tenantID)
	if err != nil {
		h.log.Error("get inventory summary failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to fetch inventory summary")
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// StockValuationReport handles GET /v1/{tenant}/inventory/reports/stock-valuation?warehouse_id=
func (h *InventoryHandler) StockValuationReport(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	// An explicit ?warehouse_id= wins (the page's own location picker); an empty value means
	// "All Locations" was chosen deliberately and stays tenant-wide — it does NOT fall back to
	// the ambient X-Outlet-ID, unlike a write, so a report reader can always get the blended
	// total by clearing the picker regardless of which outlet happens to be active up top.
	warehouseID := uuid.Nil
	if wid := r.URL.Query().Get("warehouse_id"); wid != "" {
		parsed, werr := uuid.Parse(wid)
		if werr != nil {
			writeError(w, http.StatusBadRequest, "INVALID_WAREHOUSE_ID", "Invalid warehouse_id")
			return
		}
		warehouseID = parsed
	}
	val, err := h.itemsSvc.StockValuation(r.Context(), tenantID, warehouseID)
	if err != nil {
		h.log.Error("stock valuation report failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to compute stock valuation")
		return
	}
	writeJSON(w, http.StatusOK, val)
}

// StockDeadstockReport handles GET /v1/{tenant}/inventory/reports/deadstock?days=90
func (h *InventoryHandler) StockDeadstockReport(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	days := 90
	if d := r.URL.Query().Get("days"); d != "" {
		if n, e := strconv.Atoi(d); e == nil && n > 0 {
			days = n
		}
	}
	rep, err := h.itemsSvc.StockDeadstock(r.Context(), tenantID, days)
	if err != nil {
		h.log.Error("deadstock report failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to compute deadstock report")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// StockFastMovingReport handles GET /v1/{tenant}/inventory/reports/fast-moving?days=90
func (h *InventoryHandler) StockFastMovingReport(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	days := 90
	if d := r.URL.Query().Get("days"); d != "" {
		if n, e := strconv.Atoi(d); e == nil && n > 0 {
			days = n
		}
	}
	rep, err := h.itemsSvc.StockFastMoving(r.Context(), tenantID, days)
	if err != nil {
		h.log.Error("fast-moving report failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to compute fast-moving report")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ListUnits handles GET /v1/{tenant}/inventory/units
func (h *InventoryHandler) ListUnits(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	results, err := h.unitSvc.ListUnits(r.Context(), tenantID)
	if err != nil {
		h.log.Error("list units failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to list units")
		return
	}

	// ?use_case=<outlet use_case> keeps irrelevant units out of other verticals'
	// pickers: untagged units are universal, tagged ones (tot/pot/portion →
	// hospitality) only surface for their use_cases.
	if uc := r.URL.Query().Get("use_case"); uc != "" {
		filtered := results[:0]
		for _, u := range results {
			if u.RelevantToUseCase(uc) {
				filtered = append(filtered, u)
			}
		}
		results = filtered
	}

	writeJSON(w, http.StatusOK, results)
}

// CreateUnit handles POST /v1/{tenant}/inventory/units
func (h *InventoryHandler) CreateUnit(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req units.UnitDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	result, err := h.unitSvc.CreateUnit(r.Context(), tenantID, req)
	if err != nil {
		var dupErr *units.DuplicateUnitError
		if errors.As(err, &dupErr) {
			writeError(w, http.StatusConflict, "DUPLICATE_UNIT", fmt.Sprintf("A unit with %s %q already exists.", dupErr.Field, dupErr.Value))
			return
		}
		h.log.Error("create unit failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, result)
}

// UpdateUnit handles PUT /v1/{tenant}/inventory/units/{unitID} — updates a unit of measure.
func (h *InventoryHandler) UpdateUnit(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	unitID, err := uuid.Parse(chi.URLParam(r, "unitID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid unit ID")
		return
	}
	var req units.UnitDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	result, err := h.unitSvc.UpdateUnit(r.Context(), tenantID, unitID, req)
	if err != nil {
		var dupErr *units.DuplicateUnitError
		if errors.As(err, &dupErr) {
			writeError(w, http.StatusConflict, "DUPLICATE_UNIT", fmt.Sprintf("A unit with %s %q already exists.", dupErr.Field, dupErr.Value))
			return
		}
		h.log.Error("update unit failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ListItems handles GET /v1/{tenant}/inventory/items — returns a paginated list of active items.
func (h *InventoryHandler) ListItems(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	typeFilter := r.URL.Query().Get("type")
	statusFilter := r.URL.Query().Get("status") // "active" | "inactive" | "all"; default = active
	searchFilter := r.URL.Query().Get("search")
	useCaseFilter := r.URL.Query().Get("use_case") // e.g. HOSPITALITY_ROOM, CONFERENCE, AMENITY

	var categoryID *uuid.UUID
	if catStr := r.URL.Query().Get("category_id"); catStr != "" {
		if catID, parseErr := uuid.Parse(catStr); parseErr == nil {
			categoryID = &catID
		}
	}

	var unitID *uuid.UUID
	if unitStr := r.URL.Query().Get("unit_id"); unitStr != "" {
		if uID, parseErr := uuid.Parse(unitStr); parseErr == nil {
			unitID = &uID
		}
	}

	// Parse optional tags filter: ?tags=vegan,gluten_free
	var tagsFilter []string
	if tagsParam := r.URL.Query().Get("tags"); tagsParam != "" {
		for _, t := range strings.Split(tagsParam, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tagsFilter = append(tagsFilter, t)
			}
		}
	}

	var outletID *uuid.UUID
	if oid := operatingOutletID(r); oid != uuid.Nil {
		outletID = &oid
	}
	// ?warehouse_id= narrows the returned available/on_hand to ONE specific warehouse instead of
	// the outlet-wide sum above — Stock Transfer's "From Warehouse" picker needs this so the
	// figure it shows the user is what's actually shippable FROM that warehouse, not the whole
	// outlet's aggregate across every warehouse it spans (see items.Service.ListItems doc comment).
	var warehouseID *uuid.UUID
	if wid := r.URL.Query().Get("warehouse_id"); wid != "" {
		if parsed, werr := uuid.Parse(wid); werr == nil {
			warehouseID = &parsed
		}
	}

	// ?id=<uuid> restricts the list to a single item by primary key while reusing the full
	// list enrichment — the item-detail page fetches this way so it renders the same enriched
	// shape (category name, effective price, on-hand, images) as a catalog row.
	ctx := r.Context()
	if idStr := r.URL.Query().Get("id"); idStr != "" {
		if itemID, parseErr := uuid.Parse(idStr); parseErr == nil {
			ctx = items.WithItemIDFilter(ctx, itemID)
		}
	}

	// ?brand_id=<uuid> / ?model=<text> — catalog filter bar (Brand/Model comboboxes),
	// same shape as category_id above; carried via ctx (not a new positional param) like
	// every other optional ListItems filter below.
	if bidStr := r.URL.Query().Get("brand_id"); bidStr != "" {
		if bID, parseErr := uuid.Parse(bidStr); parseErr == nil {
			ctx = items.WithBrandFilter(ctx, bID)
		}
	}
	if model := strings.TrimSpace(r.URL.Query().Get("model")); model != "" {
		ctx = items.WithModelFilter(ctx, model)
	}

	// ?include=variants opts into eager-loading each item's active variations.
	for _, inc := range strings.Split(r.URL.Query().Get("include"), ",") {
		if strings.TrimSpace(inc) == "variants" {
			ctx = items.WithIncludeVariants(ctx)
		}
	}
	// ?include_non_billable=1 widens the type filter to also return non-billable items
	// (free accompaniments / supplies) — used by the POS catalog proxy.
	if v := r.URL.Query().Get("include_non_billable"); v == "1" || strings.EqualFold(v, "true") {
		ctx = items.WithIncludeNonBillable(ctx)
	}
	// ?lean=1 skips the image-gallery/preferred-supplier eager loads — for S2S sync callers
	// (pos-api, ordering-backend) that never read Images/preferred_supplier_name, saving two
	// joins per page. Opt-in: omitting it (every UI caller today) keeps the full load.
	if v := r.URL.Query().Get("lean"); v == "1" || strings.EqualFold(v, "true") {
		ctx = items.WithLeanFetch(ctx)
	}
	// ?for_recipe=1 scopes the list to recipe-ingredient candidates: GOODS + INGREDIENT
	// plus RECIPE items flagged usable_in_recipes (reusable menu components like Black
	// Tea inside an Iced Passion Tea). Used by the recipe-builder ingredient picker;
	// overrides the plain type filter.
	if v := r.URL.Query().Get("for_recipe"); v == "1" || strings.EqualFold(v, "true") {
		ctx = items.WithRecipeInputScope(ctx)
	}
	// ?not_for_sale=only|exclude — "only" backs the back-office "Not for selling" filter
	// checkbox; "exclude" is the sales-surface guarantee (pos-api catalog proxy and
	// ordering storefront fetch with it so flagged items never leave inventory). Legacy
	// truthy values ("1"/"true") mean "only" for filter-checkbox ergonomics.
	switch v := r.URL.Query().Get("not_for_sale"); {
	case v == "only" || v == "1" || strings.EqualFold(v, "true"):
		ctx = items.WithNotForSaleFilter(ctx, "only")
	case v == "exclude" || v == "0" || strings.EqualFold(v, "false"):
		ctx = items.WithNotForSaleFilter(ctx, "exclude")
	}
	// ?sort=<column>&dir=asc|desc — server-driven DataTable sorting over whitelisted
	// columns (unknown columns silently keep the default SKU order).
	if sortField := r.URL.Query().Get("sort"); sortField != "" {
		ctx = items.WithListSort(ctx, sortField, r.URL.Query().Get("dir"))
	}

	p := pagination.Parse(r)
	results, total, err := h.itemsSvc.ListItems(ctx, tenantID, typeFilter, statusFilter, p.Limit, p.Offset, categoryID, unitID, searchFilter, outletID, warehouseID, useCaseFilter, tagsFilter...)
	if err != nil {
		h.log.Error("list items failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to list items")
		return
	}

	// ?fields=sku,name,cost_price,selling_price — opt-in response projection for callers
	// whose UI only displays a subset of columns (e.g. inventory-ui's DataTable
	// column-visibility picker). Reduces payload/serialization cost; the full item is still
	// computed/enriched above regardless, so no business logic is affected. Omitting the
	// param (every existing caller today) returns the full DTO, unchanged.
	if fieldsParam := r.URL.Query().Get("fields"); fieldsParam != "" {
		fields := strings.Split(fieldsParam, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if projected, perr := projectFields(results, fields, "id", "sku"); perr == nil {
			writeJSON(w, http.StatusOK, pagination.NewResponse(projected, total, p))
			return
		} else {
			h.log.Warn("field projection failed, returning full payload", zap.Error(perr))
		}
	}

	writeJSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// ListEventItems handles GET /v1/{tenant}/inventory/events
// Returns SERVICE-type items that have event_start_at set, ordered by start time ascending.
func (h *InventoryHandler) ListEventItems(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	p := pagination.Parse(r)
	results, total, err := h.itemsSvc.ListEventItems(r.Context(), tenantID, p.Limit, p.Offset)
	if err != nil {
		h.log.Error("list event items failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to list events")
		return
	}
	writeJSON(w, http.StatusOK, pagination.NewResponse(results, total, p))
}

// ListItemVariants handles GET /v1/{tenant}/inventory/items/{itemId}/variants —
// returns the active product variations for an item so retail/POS can sell variations.
func (h *InventoryHandler) ListItemVariants(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	itemID, err := uuid.Parse(chi.URLParam(r, "itemId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ITEM_ID", "Invalid item ID")
		return
	}

	variants, err := h.itemsSvc.ListItemVariants(r.Context(), tenantID, itemID)
	if err != nil {
		h.log.Error("list item variants failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to list item variants")
		return
	}

	writeJSON(w, http.StatusOK, variants)
}

// CreateItem handles POST /v1/{tenant}/inventory/items — creates a new inventory item.
func (h *InventoryHandler) CreateItem(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req items.ItemDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	// SKU is optional — auto-generated if empty
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "MISSING_NAME", "Name is required")
		return
	}
	if req.Type == "" {
		req.Type = "GOODS"
	}
	req.IsActive = true

	if err := items.ValidateTicketTiers(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "TIER_CAPACITY", err.Error())
		return
	}

	// Enforce the plan's inventory_max_sku structural cap (hard-block, no overage).
	if _, total, cerr := h.itemsSvc.ListItems(r.Context(), tenantID, "", "all", 1, 0, nil, nil, "", nil, nil, ""); cerr == nil {
		if subscriptions.AssertLimit(w, r, "products", subscriptions.LimitSKU, total) {
			return
		}
	}

	result, err := h.itemsSvc.CreateItem(r.Context(), tenantID, req)
	if err != nil {
		var dupErr *items.DuplicateSKUError
		if errors.As(err, &dupErr) {
			writeError(w, http.StatusConflict, "DUPLICATE_SKU", fmt.Sprintf("SKU %q is already in use for this tenant — choose a different one.", dupErr.SKU))
			return
		}
		h.log.Error("create item failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, result)
}

// UpdateItem handles PUT /v1/{tenant}/inventory/items/{sku} — updates an existing item by SKU.
func (h *InventoryHandler) UpdateItem(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	sku := chi.URLParam(r, "sku")
	if sku == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}

	avail, err := h.itemsSvc.GetStockAvailability(r.Context(), tenantID, sku)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Item not found")
		return
	}

	var req items.ItemDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	if err := items.ValidateTicketTiers(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "TIER_CAPACITY", err.Error())
		return
	}

	// Capture cost_price before update for cascade detection.
	prevCostPrice := avail // only used to check if cost changed
	_ = prevCostPrice

	result, err := h.itemsSvc.UpdateItem(r.Context(), tenantID, avail.ItemID, req)
	if err != nil {
		h.log.Error("update item failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
		return
	}

	// Cascade: if cost_price changed on an INGREDIENT, recalculate all recipe costs that use it.
	if req.CostPrice != nil && h.recipeSvc != nil {
		go func() {
			if cErr := h.recipeSvc.RecalculateCostsForIngredient(r.Context(), tenantID, avail.ItemID); cErr != nil {
				h.log.Warn("ingredient price cascade failed", zap.Error(cErr), zap.String("sku", sku))
			}
		}()
	}

	writeJSON(w, http.StatusOK, result)
}

// SetItemPrice handles PATCH /v1/{tenant}/inventory/items/{sku}/price — a targeted
// selling-price correction that lands everywhere the POS price-resolve reads: the item's
// guardrails + RETAIL/WHOLESALE tier rows, and the linked recipe's selling_price for
// RECIPE items (recipe price outranks tier rows there). Called S2S by pos-api when a
// manager corrects a mispriced order line and opts to fix the catalog too.
func (h *InventoryHandler) SetItemPrice(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	sku := chi.URLParam(r, "sku")
	if sku == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}
	var req struct {
		Price *float64 `json:"price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Price == nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "price is required")
		return
	}
	if *req.Price < 0 {
		writeError(w, http.StatusBadRequest, "INVALID_PRICE", "price cannot be negative")
		return
	}

	dto, err := h.itemsSvc.SetSellingPriceBySKU(r.Context(), tenantID, sku, *req.Price)
	if err != nil {
		if ent.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Item not found")
			return
		}
		h.log.Error("set item price failed", zap.Error(err), zap.String("sku", sku))
		writeError(w, http.StatusInternalServerError, "PRICE_UPDATE_FAILED", err.Error())
		return
	}

	recipeUpdated := false
	if dto.Type == "RECIPE" && h.recipeSvc != nil {
		recipeUpdated, err = h.recipeSvc.SetSellingPriceByItem(r.Context(), tenantID, dto.ID, *req.Price)
		if err != nil {
			// The item-side update already landed; report the partial failure rather than 500.
			h.log.Warn("set recipe selling price failed", zap.Error(err), zap.String("sku", sku))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sku":            sku,
		"price":          *req.Price,
		"recipe_updated": recipeUpdated,
	})
}

// operatingOutletID returns the outlet the request is acting under (from the X-Outlet-ID context),
// or uuid.Nil when none is set (S2S / platform-wide requests). Stock movements use it to default
// their warehouse to the operating outlet's own warehouse.
func operatingOutletID(r *http.Request) uuid.UUID {
	if outletStr := invmiddleware.GetOutletID(r.Context()); outletStr != "" {
		if oid, err := uuid.Parse(outletStr); err == nil {
			return oid
		}
	}
	return uuid.Nil
}

// AdjustStock handles POST /v1/{tenant}/inventory/adjust — adjusts stock levels for an item.
func (h *InventoryHandler) AdjustStock(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req stock.AdjustStockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	if req.SKU == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}
	if req.Adjustment == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ADJUSTMENT", "Adjustment must be non-zero")
		return
	}
	if req.Reason == "" {
		req.Reason = "adjustment"
	}
	// Give an unreferenced adjustment its own document number so the movement is printable and
	// auditable (GET /inventory/adjustments/document?reference=…). Never overwrites a reference
	// the caller supplied.
	req.Reference = h.ensureAdjustmentReference(r.Context(), tenantID, req.Reference)
	// Default an unspecified warehouse to the operating outlet's own warehouse (not the tenant
	// default) so the movement is visible on that outlet's POS terminal.
	req.OutletID = operatingOutletID(r)

	// Same approval gate as CreateAdjustment: a configured stock_adjustment/stock_writeoff
	// ApprovalRule routes the movement through the workflow BEFORE any stock is mutated. With
	// no rule the movement applies immediately (auto-approve). This closes the /adjust bypass
	// so both adjustment entry points enforce the same control.
	if !h.gateStockAdjustment(w, r, tenantID, req) {
		return
	}

	result, err := h.stockSvc.AdjustStock(r.Context(), tenantID, req)
	if err != nil {
		h.log.Error("adjust stock failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "ADJUST_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// bulkAdjustStockRequest is the wire shape for POST /inventory/stock/bulk-adjust.
type bulkAdjustStockRequest struct {
	Lines []struct {
		SKU        string  `json:"sku"`
		Adjustment float64 `json:"adjustment"`
		// DestinationWarehouseID, when set, moves this line's quantity from WarehouseID to this
		// warehouse (transfer_out + transfer_in) instead of an in-place add/remove — see
		// stock.BulkAdjustLine's doc comment.
		DestinationWarehouseID string `json:"destination_warehouse_id,omitempty"`
	} `json:"lines"`
	Reason      string `json:"reason"`
	Reference   string `json:"reference,omitempty"`
	Notes       string `json:"notes,omitempty"`
	WarehouseID string `json:"warehouse_id,omitempty"`
}

// BulkAdjustStock handles POST /v1/{tenant}/inventory/stock/bulk-adjust — queues a per-item
// stock adjustment across many items (sharing a warehouse/reason/notes) as a background
// bulk job and returns immediately; the job notifies the tenant over the notification
// WebSocket (bulk_job.completed) when it finishes, with GET /inventory/bulk-jobs/{id} as a
// polling fallback. See stock.BulkAdjustStock's doc comment for exactly what it reuses from
// the single /adjust path (and its one documented gap: no per-line approval-workflow gate).
//
//	@Summary      Bulk stock adjustment
//	@Description  Queues a per-item adjustment (sku + delta) across many items against one shared warehouse/reason as a background job. Returns 202 with a job id; poll GET /inventory/bulk-jobs/{id} or listen for the bulk_job.completed WebSocket notification.
//	@Tags         stock
//	@Accept       json
//	@Produce      json
//	@Param        tenant  path      string                  true  "Tenant ID"
//	@Param        body    body      bulkAdjustStockRequest  true  "Lines + shared warehouse/reason"
//	@Success      202     {object}  bulkJobAccepted
//	@Failure      400     {object}  map[string]string
//	@Router       /{tenant}/inventory/stock/bulk-adjust [post]
func (h *InventoryHandler) BulkAdjustStock(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	if h.bulkJobsSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "BULK_JOBS_UNAVAILABLE", "Background job runner is not configured")
		return
	}

	var req bulkAdjustStockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if len(req.Lines) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_LINES", "lines is required")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "MISSING_REASON", "reason is required")
		return
	}
	var whID uuid.UUID
	if req.WarehouseID != "" {
		whID, err = uuid.Parse(req.WarehouseID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_WAREHOUSE", "warehouse_id must be a valid UUID")
			return
		}
	}

	var adjustedBy uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		adjustedBy, _ = claims.UserID()
	}
	outletID := operatingOutletID(r)

	lines := make([]stock.BulkAdjustLine, 0, len(req.Lines))
	for _, l := range req.Lines {
		line := stock.BulkAdjustLine{SKU: l.SKU, Adjustment: l.Adjustment}
		if l.DestinationWarehouseID != "" {
			if destID, derr := uuid.Parse(l.DestinationWarehouseID); derr == nil {
				line.DestinationWarehouseID = &destID
			}
		}
		lines = append(lines, line)
	}

	bulkReq := stock.BulkAdjustStockRequest{
		Lines:       lines,
		Reason:      req.Reason,
		Reference:   req.Reference,
		Notes:       req.Notes,
		AdjustedBy:  adjustedBy,
		WarehouseID: whID,
		OutletID:    outletID,
	}
	var notifyOutletID *uuid.UUID
	if outletID != uuid.Nil {
		notifyOutletID = &outletID
	}
	job, err := h.bulkJobsSvc.CreateAndRun(r.Context(), tenantID, notifyOutletID, "bulk_stock_adjust", len(lines),
		map[string]any{"reason": req.Reason, "warehouse_id": req.WarehouseID, "line_count": len(lines)},
		adjustedBy,
		func(ctx context.Context, _ *ent.BulkJob) (bulkjobs.RunResult, error) {
			result, rErr := h.stockSvc.BulkAdjustStock(ctx, tenantID, bulkReq)
			if rErr != nil {
				return bulkjobs.RunResult{}, rErr
			}
			return bulkjobs.RunResult{
				Processed: result.Processed,
				Failed:    len(result.Skipped),
				Detail:    map[string]any{"skipped": result.Skipped},
			}, nil
		})
	if err != nil {
		h.log.Error("queue bulk adjust stock failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "BULK_ADJUST_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, bulkJobAccepted{JobID: job.ID.String(), Status: string(job.Status), Total: job.Total})
}

// relocateItemLocationRequest is the wire shape for POST /inventory/stock/relocate.
type relocateItemLocationRequest struct {
	ItemIDs                []string `json:"item_ids"`
	SourceWarehouseID      string   `json:"source_warehouse_id"`
	DestinationWarehouseID string   `json:"destination_warehouse_id"`
	Notes                  string   `json:"notes,omitempty"`
}

// RelocateItemLocation handles POST /v1/{tenant}/inventory/stock/relocate — moves one or many
// items' ENTIRE current balance (including zero) from one warehouse to another. Distinct from a
// stock transfer: there is no quantity to choose and no "insufficient stock" gate — see
// stock.RelocateItemLocation's doc comment. Reuses the same PermStockChange gate as a stock
// adjustment (this mutates balances directly, same trust level).
//
//	@Summary      Relocate item(s) to another warehouse
//	@Description  Moves each item's entire balance (on_hand/available, whatever it currently is, including zero) from the source warehouse to the destination, marking the source removed_from_location. Not a stock transfer — no quantity is chosen, nothing can be "insufficient".
//	@Tags         stock
//	@Accept       json
//	@Produce      json
//	@Param        tenant  path      string                        true  "Tenant ID"
//	@Param        body    body      relocateItemLocationRequest  true  "Item ids + source/destination warehouse"
//	@Success      200     {object}  stock.RelocateItemLocationResult
//	@Failure      400     {object}  map[string]string
//	@Router       /{tenant}/inventory/stock/relocate [post]
func (h *InventoryHandler) RelocateItemLocation(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req relocateItemLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if len(req.ItemIDs) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_ITEMS", "item_ids is required")
		return
	}
	srcWH, err := uuid.Parse(req.SourceWarehouseID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SOURCE", "source_warehouse_id must be a valid UUID")
		return
	}
	destWH, err := uuid.Parse(req.DestinationWarehouseID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_DESTINATION", "destination_warehouse_id must be a valid UUID")
		return
	}
	itemIDs := make([]uuid.UUID, 0, len(req.ItemIDs))
	for _, s := range req.ItemIDs {
		id, pErr := uuid.Parse(s)
		if pErr != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ITEM_ID", "invalid item id: "+s)
			return
		}
		itemIDs = append(itemIDs, id)
	}

	var adjustedBy uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		adjustedBy, _ = claims.UserID()
	}

	result, err := h.stockSvc.RelocateItemLocation(r.Context(), tenantID, stock.RelocateItemLocationRequest{
		ItemIDs:                itemIDs,
		SourceWarehouseID:      srcWH,
		DestinationWarehouseID: destWH,
		AdjustedBy:             adjustedBy,
		Notes:                  req.Notes,
	})
	if err != nil {
		h.log.Error("relocate item location failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "RELOCATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// bulkJobAccepted is the 202 response body for any endpoint that queues a background bulk job.
type bulkJobAccepted struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Total  int    `json:"total"`
}

// setItemOutletMembershipRequest is the wire shape for POST /inventory/stock/set-membership.
type setItemOutletMembershipRequest struct {
	ItemIDs            []string `json:"item_ids"`
	TargetWarehouseIDs []string `json:"target_warehouse_ids"`
	Notes              string   `json:"notes,omitempty"`
	// MoveQuantity: only applies to a clean 1-dropped+1-added pair — moves exactly this amount,
	// leaving the remainder active at the source, instead of carrying everything. Omit (or a
	// value >= the source's on-hand) for today's default full-move behavior.
	MoveQuantity *float64 `json:"move_quantity,omitempty"`
	// ZeroStockMode: opt-in for the general many-to-many case — dropped outlets' stock is
	// discarded rather than pooled, and newly-added outlets start at zero. The UI must confirm
	// this with the user before sending it, since it's the one mode that can make real on-hand
	// quantity vanish rather than relocate.
	ZeroStockMode bool `json:"zero_stock_mode,omitempty"`
	// MoveWithStock: opt-in — dropped outlets' quantity is carried to the newly-added outlet(s)
	// instead of the default (just hide, quantity untouched). Requires at least one warehouse in
	// TargetWarehouseIDs; mutually exclusive with ZeroStockMode. Omitting both flags is the safe
	// default: an unchecked outlet is hidden only, never moved or cleared.
	MoveWithStock bool `json:"move_with_stock,omitempty"`
}

// SetItemOutletMembership handles POST /v1/{tenant}/inventory/stock/set-membership — the
// checkbox catalog-movement UX: for each item, check the outlets it should be stocked in and
// uncheck the rest; the diff against its current active warehouses is queued as a background
// bulk job (see stock.SetItemOutletMembership's doc comment for exactly how quantity is carried
// over) and this returns immediately with a job id — never blocks on however many items/outlets
// are involved. Same completion path as BulkAdjustStock: poll GET /inventory/bulk-jobs/{id} or
// listen for bulk_job.completed.
//
//	@Summary      Set which outlets an item is stocked in
//	@Description  Queues a background job that reconciles each item's current warehouse footprint against the given target set — check an outlet to add it, uncheck to remove. Returns 202 with a job id.
//	@Tags         stock
//	@Accept       json
//	@Produce      json
//	@Param        tenant  path      string                          true  "Tenant ID"
//	@Param        body    body      setItemOutletMembershipRequest  true  "Item ids + target warehouse set"
//	@Success      202     {object}  bulkJobAccepted
//	@Failure      400     {object}  map[string]string
//	@Router       /{tenant}/inventory/stock/set-membership [post]
func (h *InventoryHandler) SetItemOutletMembership(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	if h.bulkJobsSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "BULK_JOBS_UNAVAILABLE", "Background job runner is not configured")
		return
	}

	var req setItemOutletMembershipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if len(req.ItemIDs) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_ITEMS", "item_ids is required")
		return
	}
	itemIDs := make([]uuid.UUID, 0, len(req.ItemIDs))
	for _, s := range req.ItemIDs {
		id, pErr := uuid.Parse(s)
		if pErr != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ITEM_ID", "invalid item id: "+s)
			return
		}
		itemIDs = append(itemIDs, id)
	}
	targetWHs := make([]uuid.UUID, 0, len(req.TargetWarehouseIDs))
	for _, s := range req.TargetWarehouseIDs {
		id, pErr := uuid.Parse(s)
		if pErr != nil {
			writeError(w, http.StatusBadRequest, "INVALID_WAREHOUSE_ID", "invalid warehouse id: "+s)
			return
		}
		targetWHs = append(targetWHs, id)
	}

	var adjustedBy uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
		adjustedBy, _ = claims.UserID()
	}

	membershipReq := stock.SetItemOutletMembershipRequest{
		ItemIDs:            itemIDs,
		TargetWarehouseIDs: targetWHs,
		AdjustedBy:         adjustedBy,
		Notes:              req.Notes,
		MoveQuantity:       req.MoveQuantity,
		ZeroStockMode:      req.ZeroStockMode,
		MoveWithStock:      req.MoveWithStock,
	}
	// Validate synchronously so a bad request (e.g. move-with-stock with no destination outlet)
	// gets an immediate 400 instead of a background job that reports "failed" a moment later.
	if vErr := stock.ValidateSetItemOutletMembershipRequest(membershipReq); vErr != nil {
		writeError(w, http.StatusBadRequest, "INVALID_MEMBERSHIP_REQUEST", vErr.Error())
		return
	}
	// nil: this batch can span several target warehouses/outlets at once (that's the point of
	// outlet-membership editing), so there is no single outlet to scope the completion push to —
	// notifying the whole tenant is correct here, unlike a single-warehouse bulk adjustment.
	job, err := h.bulkJobsSvc.CreateAndRun(r.Context(), tenantID, nil, "item_relocation", len(itemIDs),
		map[string]any{"item_count": len(itemIDs), "target_warehouse_ids": req.TargetWarehouseIDs},
		adjustedBy,
		func(ctx context.Context, _ *ent.BulkJob) (bulkjobs.RunResult, error) {
			result, rErr := h.stockSvc.SetItemOutletMembership(ctx, tenantID, membershipReq)
			if rErr != nil {
				return bulkjobs.RunResult{}, rErr
			}
			return bulkjobs.RunResult{
				Processed: result.Processed,
				Failed:    len(result.Skipped),
				Detail:    map[string]any{"skipped": result.Skipped},
			}, nil
		})
	if err != nil {
		h.log.Error("queue item outlet membership change failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "MEMBERSHIP_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, bulkJobAccepted{JobID: job.ID.String(), Status: string(job.Status), Total: job.Total})
}

// GetBulkJob handles GET /v1/{tenant}/inventory/bulk-jobs/{id} — the polling fallback for a
// client that isn't (or can't stay) connected to the notification WebSocket's bulk_job.completed
// push.
//
//	@Summary      Get a bulk job's status
//	@Tags         stock
//	@Produce      json
//	@Param        tenant  path      string  true  "Tenant ID"
//	@Param        id      path      string  true  "Job ID"
//	@Success      200     {object}  ent.BulkJob
//	@Failure      404     {object}  map[string]string
//	@Router       /{tenant}/inventory/bulk-jobs/{id} [get]
func (h *InventoryHandler) GetBulkJob(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	if h.bulkJobsSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "BULK_JOBS_UNAVAILABLE", "Background job runner is not configured")
		return
	}
	jobID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JOB_ID", "id must be a valid UUID")
		return
	}
	job, err := h.bulkJobsSvc.GetJob(r.Context(), tenantID, jobID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// DeleteItem handles DELETE /v1/{tenant}/inventory/items/{sku} — soft-deletes an item by SKU.
func (h *InventoryHandler) DeleteItem(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	sku := chi.URLParam(r, "sku")
	if sku == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}

	// Soft-delete: resolve by SKU and set is_active=false only. Resolving directly (not via
	// stock-availability) means items with no balance row are still deletable; setting only the
	// flag avoids the empty-DTO UpdateItem path that blanked required fields like name.
	if err = h.itemsSvc.DeactivateItemBySKU(r.Context(), tenantID, sku); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Item not found")
			return
		}
		h.log.Error("delete item failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// MarkItemEOL handles POST /v1/{tenant}/inventory/items/{sku}/eol — marks an item End-of-Life.
// The item is set is_active=false and given an end_of_life_at timestamp, so it immediately
// disappears from item lists, the POS live catalog, and ordering; an inventory.item.updated event
// is emitted so downstream catalogs sync. The item is hard-deleted by the purge scheduler once the
// retention window elapses, unless restored first.
func (h *InventoryHandler) MarkItemEOL(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	sku := chi.URLParam(r, "sku")
	if sku == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}
	dto, err := h.itemsSvc.MarkItemEOL(r.Context(), tenantID, sku)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Item not found")
			return
		}
		h.log.Error("mark item EOL failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "EOL_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// RestoreItemEOL handles POST /v1/{tenant}/inventory/items/{sku}/eol/restore — un-marks an EOL
// item, clearing end_of_life_at and re-activating it so it reappears everywhere.
func (h *InventoryHandler) RestoreItemEOL(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	sku := chi.URLParam(r, "sku")
	if sku == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}
	dto, err := h.itemsSvc.RestoreItemEOL(r.Context(), tenantID, sku)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Item not found")
			return
		}
		h.log.Error("restore item EOL failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "EOL_RESTORE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// adjustmentApprovalModule maps a stock-adjustment reason to the approval module it gates under:
// damage/expiry/shrinkage are write-offs (a stricter, finance-owned control), everything else is a
// plain stock adjustment.
func adjustmentApprovalModule(reason string) string {
	switch reason {
	case "damaged", "expired", "shrinkage":
		return "stock_writeoff"
	}
	return "stock_adjustment"
}

// gateStockAdjustment enforces the stock-adjustment / stock-writeoff approval workflow BEFORE any
// stock is mutated. It returns true when the caller may proceed to apply the adjustment:
//   - no approval engine is wired, OR
//   - no active ApprovalRule matches this magnitude for the module (auto-approve — the default,
//     so a tenant with no rule configured is never blocked), OR
//   - the referenced ApprovalIntentID has already been approved by a manager.
//
// It returns false AND has already written the 422 response when approval is pending/required, so
// the caller must simply `return`. Shared by AdjustStock (/adjust) and CreateAdjustment
// (/adjustments) so both entry points enforce the identical control — no bypass.
func (h *InventoryHandler) gateStockAdjustment(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, req stock.AdjustStockRequest) bool {
	if h.approvalSvc == nil {
		return true
	}
	module := adjustmentApprovalModule(req.Reason)
	amount := req.Adjustment
	if amount < 0 {
		amount = -amount
	}
	actor := req.AdjustedBy
	if actor == uuid.Nil {
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			actor, _ = claims.UserID()
		}
	}
	if req.ApprovalIntentID != nil {
		// Retry after a manager decision. The adjustment now posts SERVER-SIDE the moment the
		// request is approved (the payload stashed at submission is replayed by the approval-
		// execution path), so this legacy retry must NOT re-apply — that would double-post. Report
		// the current state instead of mutating stock.
		ok, state, _ := h.approvalSvc.Satisfied(r.Context(), tenantID, module, *req.ApprovalIntentID, amount)
		if ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"approval_required": false, "state": "approved", "module": module,
				"intent_id": req.ApprovalIntentID,
				"message":   "This adjustment was approved and has already been posted.",
			})
			return false
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "ADJUSTMENT_APPROVAL_" + strings.ToUpper(state), "approval_required": true,
			"intent_id": req.ApprovalIntentID, "state": state, "module": module,
		})
		return false
	}
	// First attempt — create the approval request only if a rule gates this amount. The full
	// adjustment is stashed on the request as its payload so the approval-execution path can post
	// the stock the instant a manager approves (without this, an approved adjustment never moves
	// stock — the client never retries and nothing else replays it).
	intent := uuid.New()
	request, gated, _ := h.approvalSvc.SubmitWithPayload(r.Context(), tenantID, module, intent, req.Reference, amount, &actor, adjustmentPayload(req, actor))
	if gated {
		resp := map[string]any{"approval_required": true, "intent_id": intent, "module": module}
		if request != nil {
			resp["request_id"] = request.ID
		}
		writeJSON(w, http.StatusUnprocessableEntity, resp)
		return false
	}
	return true
}

// adjustmentPayload serializes a gated stock adjustment into the map persisted on its
// ApprovalRequest, so the approval-execution path can rebuild the exact AdjustStockRequest
// and post it once approved. Warehouse/outlet are stored as strings and re-resolved by
// AdjustStock at replay time (there is no HTTP outlet context then).
func adjustmentPayload(req stock.AdjustStockRequest, actor uuid.UUID) map[string]any {
	p := map[string]any{
		"sku":         req.SKU,
		"adjustment":  req.Adjustment,
		"reason":      req.Reason,
		"reference":   req.Reference,
		"notes":       req.Notes,
		"adjusted_by": actor.String(),
	}
	if req.WarehouseID != uuid.Nil {
		p["warehouse_id"] = req.WarehouseID.String()
	}
	if req.OutletID != uuid.Nil {
		p["outlet_id"] = req.OutletID.String()
	}
	if req.UnitID != nil {
		p["unit_id"] = req.UnitID.String()
	}
	return p
}

// CreateAdjustment handles POST /v1/{tenant}/inventory/adjustments — creates a stock adjustment with audit trail.
func (h *InventoryHandler) CreateAdjustment(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req stock.AdjustStockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}

	if req.SKU == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKU", "SKU is required")
		return
	}
	if req.Adjustment == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ADJUSTMENT", "Adjustment must be non-zero")
		return
	}
	if req.Reason == "" {
		req.Reason = "other"
	}
	// Same document-number mint as /adjust: an adjustment created without a reference gets one
	// so it can be printed and filed as a stock adjustment note.
	req.Reference = h.ensureAdjustmentReference(r.Context(), tenantID, req.Reference)
	// Default an unspecified warehouse to the operating outlet's own warehouse (not the tenant
	// default) so the movement is visible on that outlet's POS terminal.
	req.OutletID = operatingOutletID(r)

	// Large-adjustment approval gate (shared with /adjust): route adjustments whose magnitude
	// falls in a configured ApprovalRule band through the approval workflow BEFORE mutating
	// stock. Safe by default — with no rule configured for the stock_adjustment/stock_writeoff
	// module, nothing is blocked and the adjustment applies immediately.
	if !h.gateStockAdjustment(w, r, tenantID, req) {
		return
	}

	result, err := h.stockSvc.AdjustStock(r.Context(), tenantID, req)
	if err != nil {
		h.log.Error("create adjustment failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "ADJUST_FAILED", err.Error())
		return
	}

	if h.auditSvc != nil {
		actor := req.AdjustedBy
		if actor == uuid.Nil {
			if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
				actor, _ = claims.UserID()
			}
		}
		action := "stock.adjustment"
		if req.Reason == "damaged" || req.Reason == "expired" || req.Reason == "shrinkage" {
			action = "stock.writeoff"
		}
		amt := req.Adjustment
		var oid *uuid.UUID
		if req.WarehouseID != uuid.Nil {
			oid = &req.WarehouseID
		}
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tenantID,
			OutletID:    oid,
			ActorUserID: actor,
			Action:      action,
			EntityType:  "stock_adjustment",
			EntityID:    req.SKU,
			Reason:      req.Reason,
			Amount:      &amt,
			After:       map[string]any{"sku": req.SKU, "adjustment": req.Adjustment, "warehouse_id": req.WarehouseID.String()},
		})
	}

	writeJSON(w, http.StatusCreated, result)
}

// ListAdjustments handles GET /v1/{tenant}/inventory/adjustments — lists stock adjustments with filters.
func (h *InventoryHandler) ListAdjustments(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	var req stock.ListAdjustmentsRequest

	if itemIDStr := r.URL.Query().Get("item_id"); itemIDStr != "" {
		itemID, err := uuid.Parse(itemIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ITEM_ID", "Invalid item_id")
			return
		}
		req.ItemID = itemID
	}

	if whIDStr := r.URL.Query().Get("warehouse_id"); whIDStr != "" {
		whID, err := uuid.Parse(whIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_WAREHOUSE_ID", "Invalid warehouse_id")
			return
		}
		req.WarehouseID = whID
	} else if outletStr := invmiddleware.GetOutletID(r.Context()); outletStr != "" {
		// Outlet drill-down (X-Outlet-ID) with no explicit warehouse filter: scope to the
		// selected outlet's warehouses (+ shared ones with no outlet link), mirroring the
		// stock-levels list and ListItems outlet separation.
		if outletID, e := uuid.Parse(outletStr); e == nil {
			whIDs, werr := h.orm.Warehouse.Query().
				Where(
					entwarehouse.TenantID(tenantID),
					entwarehouse.Or(entwarehouse.OutletIDEQ(outletID), entwarehouse.OutletIDIsNil()),
				).
				IDs(r.Context())
			if werr == nil && len(whIDs) > 0 {
				req.WarehouseIDs = whIDs
			}
		}
	}

	if reason := r.URL.Query().Get("reason"); reason != "" {
		req.Reason = reason
	}

	if dateFrom := r.URL.Query().Get("date_from"); dateFrom != "" {
		t, err := time.Parse(time.RFC3339, dateFrom)
		if err != nil {
			// Also accept a plain YYYY-MM-DD (what the shared DateRangePicker sends).
			t, err = time.Parse("2006-01-02", dateFrom)
			if err != nil {
				writeError(w, http.StatusBadRequest, "INVALID_DATE_FROM", "date_from must be RFC3339 or YYYY-MM-DD")
				return
			}
		}
		req.DateFrom = t
	}

	if dateTo := r.URL.Query().Get("date_to"); dateTo != "" {
		t, err := time.Parse(time.RFC3339, dateTo)
		if err != nil {
			// Also accept a plain YYYY-MM-DD (what the shared DateRangePicker sends) — end-of-day inclusive.
			t, err = time.Parse("2006-01-02", dateTo)
			if err != nil {
				writeError(w, http.StatusBadRequest, "INVALID_DATE_TO", "date_to must be RFC3339 or YYYY-MM-DD")
				return
			}
			t = t.Add(24*time.Hour - time.Second)
		}
		req.DateTo = t
	}

	req.Search = r.URL.Query().Get("search")
	// Shared page/limit/offset parsing (same helper transfers and ItemStockHistory use) — this
	// endpoint previously hard-capped at 200 rows with no way to page past it, and reported
	// "total": len(results), which is just the size of that same capped page, not a real count.
	p := pagination.Parse(r)
	req.Limit = p.Limit
	req.Offset = p.Offset

	results, total, err := h.stockSvc.ListAdjustments(r.Context(), tenantID, req)
	if err != nil {
		h.log.Error("list adjustments failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to list adjustments")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":    results,
		"total":   total,
		"limit":   p.Limit,
		"page":    p.Page,
		"hasMore": p.Offset+len(results) < total,
	})
}

// GetBOMAvailability handles GET /v1/{tenant}/inventory/availability/bom?skus=SKU1,SKU2
func (h *InventoryHandler) GetBOMAvailability(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	skusParam := r.URL.Query().Get("skus")
	if skusParam == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SKUS", "skus query parameter is required")
		return
	}

	skus := strings.Split(skusParam, ",")
	if len(skus) == 0 {
		writeError(w, http.StatusBadRequest, "MISSING_SKUS", "At least one SKU is required")
		return
	}

	// Trim whitespace from SKUs
	for i := range skus {
		skus[i] = strings.TrimSpace(skus[i])
	}

	results, err := h.itemsSvc.GetBOMAvailability(r.Context(), tenantID, skus)
	if err != nil {
		h.log.Error("bom availability failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to check BOM availability")
		return
	}

	writeJSON(w, http.StatusOK, results)
}

// ListCategories handles GET /v1/{tenant}/inventory/categories — returns all active categories.
func (h *InventoryHandler) ListCategories(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	// has_items=true → only categories with at least one active item linked
	// (used by selection surfaces so a picked category never yields an empty set).
	hasItems := strings.EqualFold(r.URL.Query().Get("has_items"), "true") || r.URL.Query().Get("has_items") == "1"
	// sellable_only=true → same, but additionally requires that item to be not_for_sale=false.
	// A category whose only items are ALL flagged not_for_sale (raw ingredients, internal
	// supplies) has "items" in the has_items sense but is functionally empty on any sales
	// surface — POS/ordering ask for this stricter mode; label printing keeps plain has_items
	// so staff can still pick a component category to print internal-stock barcode labels.
	sellableOnly := strings.EqualFold(r.URL.Query().Get("sellable_only"), "true") || r.URL.Query().Get("sellable_only") == "1"

	results, err := h.itemsSvc.ListCategoriesFiltered(r.Context(), tenantID, hasItems, sellableOnly)
	if err != nil {
		h.log.Error("list categories failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to list categories")
		return
	}

	// ?use_case=<outlet use_case> keeps other verticals' categories (incl. global
	// ones) out of this outlet's pickers: untagged categories are universal, tagged
	// ones only surface for their use_cases. Applied post-cache so the cached list
	// stays use-case-agnostic.
	if uc := r.URL.Query().Get("use_case"); uc != "" {
		filtered := results[:0]
		for _, c := range results {
			if c.RelevantToUseCase(uc) {
				filtered = append(filtered, c)
			}
		}
		results = filtered
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":  results,
		"total": len(results),
	})
}

// DeleteCategory handles DELETE /inventory/categories/{categoryID} — soft-deletes a category.
func (h *InventoryHandler) DeleteCategory(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	categoryID, err := uuid.Parse(chi.URLParam(r, "categoryID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid category ID")
		return
	}
	if err := h.itemsSvc.DeleteCategory(r.Context(), tenantID, categoryID); err != nil {
		h.log.Error("delete category failed", zap.Error(err))
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Category not found or could not be deleted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// CreateCategory handles POST /v1/{tenant}/inventory/categories — creates a new item category.
func (h *InventoryHandler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	var req items.CategoryDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "MISSING_NAME", "Name is required")
		return
	}
	// is_global is reserved for platform seeds (nil tenant) — a tenant-facing create
	// must never mint a category visible to other tenants.
	req.IsGlobal = false
	result, err := h.itemsSvc.CreateCategory(r.Context(), tenantID, req)
	if err != nil {
		var dupErr *items.DuplicateCategoryError
		if errors.As(err, &dupErr) {
			writeError(w, http.StatusConflict, "DUPLICATE_CATEGORY", fmt.Sprintf("A category named %q already exists.", dupErr.Name))
			return
		}
		h.log.Error("create category failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// UpdateCategory handles PUT /v1/{tenant}/inventory/categories/{categoryID} — updates an item category.
func (h *InventoryHandler) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	categoryID, err := uuid.Parse(chi.URLParam(r, "categoryID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid category ID")
		return
	}
	var req items.CategoryDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "MISSING_NAME", "Name is required")
		return
	}
	result, err := h.itemsSvc.UpdateCategory(r.Context(), tenantID, categoryID, req)
	if err != nil {
		var dupErr *items.DuplicateCategoryError
		if errors.As(err, &dupErr) {
			writeError(w, http.StatusConflict, "DUPLICATE_CATEGORY", fmt.Sprintf("A category named %q already exists.", dupErr.Name))
			return
		}
		h.log.Error("update category failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// DeleteUnit handles DELETE /inventory/units/{unitID} — soft-deletes a unit of measure.
func (h *InventoryHandler) DeleteUnit(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}
	unitID, err := uuid.Parse(chi.URLParam(r, "unitID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "Invalid unit ID")
		return
	}
	if err := h.unitSvc.DeleteUnit(r.Context(), tenantID, unitID); err != nil {
		h.log.Error("delete unit failed", zap.Error(err))
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Unit not found or could not be deleted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type importResult struct {
	Created int      `json:"created"`
	Updated int      `json:"updated"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

// ImportItems handles POST /inventory/items/import — CSV bulk upsert.
// Expected CSV columns (header row required): name, sku, type, category_id, unit_id
func (h *InventoryHandler) ImportItems(w http.ResponseWriter, r *http.Request) {
	tenantID, err := parseTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_TENANT", "Invalid tenant ID")
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_FORM", "Expected multipart/form-data with a 'file' field")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "MISSING_FILE", "File field 'file' is required")
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	header, err := reader.Read()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CSV", "Failed to read CSV header")
		return
	}

	colIdx := make(map[string]int, len(header))
	for i, col := range header {
		colIdx[strings.ToLower(strings.TrimSpace(col))] = i
	}

	col := func(row []string, name string) string {
		i, ok := colIdx[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}

	// Load all existing items once for SKU→ID lookup (avoids N+1 queries).
	existingItems, _, _ := h.itemsSvc.ListItems(r.Context(), tenantID, "", "all", 10000, 0, nil, nil, "", nil, nil, "")
	skuToID := make(map[string]uuid.UUID, len(existingItems))
	for _, it := range existingItems {
		skuToID[it.SKU] = it.ID
	}

	var result importResult
	rows, _ := reader.ReadAll()
	for i, row := range rows {
		name := col(row, "name")
		sku := col(row, "sku")
		if name == "" || sku == "" {
			result.Failed++
			result.Errors = append(result.Errors, "row "+strings.Join([]string{}, "")+sku+": name and sku are required")
			_ = i
			continue
		}

		itemType := strings.ToUpper(col(row, "type"))
		if itemType == "" {
			itemType = "GOODS"
		}

		dto := items.ItemDTO{SKU: sku, Name: name, Type: itemType, IsActive: true}
		if catStr := col(row, "category_id"); catStr != "" {
			if catID, parseErr := uuid.Parse(catStr); parseErr == nil {
				dto.CategoryID = &catID
			}
		}
		if unitStr := col(row, "unit_id"); unitStr != "" {
			if unitID, parseErr := uuid.Parse(unitStr); parseErr == nil {
				dto.UnitID = &unitID
			}
		}

		if existingID, exists := skuToID[sku]; exists {
			if _, updateErr := h.itemsSvc.UpdateItem(r.Context(), tenantID, existingID, dto); updateErr != nil {
				result.Failed++
				result.Errors = append(result.Errors, "sku="+sku+": "+updateErr.Error())
			} else {
				result.Updated++
			}
		} else {
			if created, createErr := h.itemsSvc.CreateItem(r.Context(), tenantID, dto); createErr != nil {
				result.Failed++
				result.Errors = append(result.Errors, "sku="+sku+": "+createErr.Error())
			} else {
				skuToID[sku] = created.ID
				result.Created++
			}
		}
	}

	writeJSON(w, http.StatusOK, result)
}
