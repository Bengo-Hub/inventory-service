package items

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entdialect "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	events "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/inventory-service/internal/audit"
	"github.com/bengobox/inventory-service/internal/ent"
	"github.com/bengobox/inventory-service/internal/ent/inventorybalance"
	"github.com/bengobox/inventory-service/internal/ent/item"
	"github.com/bengobox/inventory-service/internal/ent/itemasset"
	"github.com/bengobox/inventory-service/internal/ent/itembrand"
	"github.com/bengobox/inventory-service/internal/ent/itemcategory"
	"github.com/bengobox/inventory-service/internal/ent/itemvariant"
	"github.com/bengobox/inventory-service/internal/ent/predicate"
	"github.com/bengobox/inventory-service/internal/ent/recipe"
	"github.com/bengobox/inventory-service/internal/ent/recipeingredient"
	"github.com/bengobox/inventory-service/internal/ent/reservation"
	"github.com/bengobox/inventory-service/internal/ent/stockadjustment"
	"github.com/bengobox/inventory-service/internal/ent/tenantinventoryconfig"
	entunit "github.com/bengobox/inventory-service/internal/ent/unit"
	"github.com/bengobox/inventory-service/internal/ent/warehouse"
	"github.com/bengobox/inventory-service/internal/modules/documents"
	"github.com/bengobox/inventory-service/internal/modules/units"
)

// includeVariantsKey is a context flag set by the HTTP layer (?include=variants)
// to opt into eager-loading the variants edge on item lists. Variant loading is
// gated because it is wasteful for large catalogs that don't need variations.
type includeVariantsKey struct{}

// includeNonBillableKey is a context flag set by the HTTP layer (?include_non_billable=1).
// It widens ListItems' type filter to ALSO return non-billable items of any type — the POS
// catalog proxy uses it so free accompaniments and supplies (tissue, packaging) reach the
// terminal even when their item type is outside the use-case's sellable types.
type includeNonBillableKey struct{}

// itemIDFilterKey is a context flag set by the HTTP layer (?id=<uuid>) to restrict ListItems
// to a single item by primary key while REUSING the full list enrichment (category name,
// effective/tax price, on-hand/available, images, preferred supplier). The item-detail page
// fetches by id this way, so it renders the same fully-enriched shape as the catalog list
// instead of the availability-only /items/{sku} endpoint.
type itemIDFilterKey struct{}

// WithItemIDFilter returns a context that restricts ListItems to the single item with this id.
func WithItemIDFilter(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, itemIDFilterKey{}, id)
}

func itemIDFilter(ctx context.Context) *uuid.UUID {
	if v, ok := ctx.Value(itemIDFilterKey{}).(uuid.UUID); ok && v != uuid.Nil {
		return &v
	}
	return nil
}

// WithIncludeNonBillable returns a context that instructs ListItems to OR non-billable
// items into the type filter.
func WithIncludeNonBillable(ctx context.Context) context.Context {
	return context.WithValue(ctx, includeNonBillableKey{}, true)
}

// recipeInputScopeKey is a context flag set by the HTTP layer (?for_recipe=1). It scopes
// ListItems to items pickable as recipe-ingredient inputs: all GOODS/INGREDIENT plus
// RECIPE items explicitly flagged usable_in_recipes (reusable menu components like a
// Black Tea used inside an Iced Passion Tea). Overrides the plain type filter.
type recipeInputScopeKey struct{}

// WithRecipeInputScope returns a context that restricts ListItems to recipe-ingredient
// candidates: GOODS + INGREDIENT + (RECIPE where usable_in_recipes).
func WithRecipeInputScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, recipeInputScopeKey{}, true)
}

func recipeInputScope(ctx context.Context) bool {
	v, _ := ctx.Value(recipeInputScopeKey{}).(bool)
	return v
}

func includeNonBillable(ctx context.Context) bool {
	v, _ := ctx.Value(includeNonBillableKey{}).(bool)
	return v
}

// WithIncludeVariants returns a context that instructs ListItems to eager-load
// each item's active variants and surface them on the DTO.
func WithIncludeVariants(ctx context.Context) context.Context {
	return context.WithValue(ctx, includeVariantsKey{}, true)
}

func includeVariants(ctx context.Context) bool {
	v, _ := ctx.Value(includeVariantsKey{}).(bool)
	return v
}

// leanFetchKey is a context flag set by the HTTP layer (?lean=1) to skip the image-gallery and
// preferred-supplier eager loads below. S2S sync callers (pos-api, ordering-backend) never read
// ItemDTO.Images/PreferredSupplierName — those joins exist for inventory-ui's own item grid/edit
// form. mapToDTO's Edges.AssetsOrErr()/PreferredSupplierOrErr() calls already tolerate an
// unloaded edge (return an error, leave the field empty) instead of firing a fallback query, so
// this is a pure opt-in cost reduction with no behavior change for existing (non-lean) callers.
type leanFetchKey struct{}

// WithLeanFetch returns a context that skips ListItems' Assets/PreferredSupplier eager loads.
func WithLeanFetch(ctx context.Context) context.Context {
	return context.WithValue(ctx, leanFetchKey{}, true)
}

func leanFetch(ctx context.Context) bool {
	v, _ := ctx.Value(leanFetchKey{}).(bool)
	return v
}

// notForSaleFilterKey is a context flag set by the HTTP layer (?not_for_sale=only|exclude).
// "only" scopes the list to flagged items (the back-office "Not for selling" filter checkbox);
// "exclude" removes them — sales-surface fetches (POS catalog proxy, ordering storefront)
// use it as the server-side guarantee that not-for-sale items never leave inventory.
// Unset keeps the default back-office behaviour: all items listed.
type notForSaleFilterKey struct{}

// WithNotForSaleFilter scopes ListItems by the not_for_sale flag: mode "only" or "exclude".
func WithNotForSaleFilter(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, notForSaleFilterKey{}, mode)
}

func notForSaleFilter(ctx context.Context) string {
	v, _ := ctx.Value(notForSaleFilterKey{}).(string)
	return v
}

// brandFilterKey is a context flag set by the HTTP layer (?brand_id=) — scopes ListItems to
// one ItemBrand master row, same shape as the categoryID positional filter.
type brandFilterKey struct{}

// WithBrandFilter scopes ListItems to items on the given ItemBrand.
func WithBrandFilter(ctx context.Context, brandID uuid.UUID) context.Context {
	return context.WithValue(ctx, brandFilterKey{}, brandID)
}

func brandFilter(ctx context.Context) *uuid.UUID {
	if v, ok := ctx.Value(brandFilterKey{}).(uuid.UUID); ok {
		return &v
	}
	return nil
}

// modelFilterKey is a context flag set by the HTTP layer (?model=) — exact match
// (case-insensitive) on the item's free-text Model field, same value space as
// ModelCombobox's per-item suggestions (there is no Model master).
type modelFilterKey struct{}

// WithModelFilter scopes ListItems to items whose Model equals the given value.
func WithModelFilter(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, modelFilterKey{}, model)
}

func modelFilter(ctx context.Context) string {
	v, _ := ctx.Value(modelFilterKey{}).(string)
	return v
}

// listSortKey carries a validated ?sort=&dir= request into ListItems (DataTable server sort).
type listSortKey struct{}

// ListSortFields whitelists sortable Item columns (query-param value → ent field).
var ListSortFields = map[string]string{
	"sku":               item.FieldSku,
	"name":              item.FieldName,
	"type":              item.FieldType,
	"cost_price":        item.FieldCostPrice,
	"min_selling_price": item.FieldMinSellingPrice,
	"max_selling_price": item.FieldMaxSellingPrice,
	"is_active":         item.FieldIsActive,
	"created_at":        item.FieldCreatedAt,
	"updated_at":        item.FieldUpdatedAt,
}

// ListBalanceSortFields whitelists computed (aggregated-balance) sort keys — these aren't real
// Item columns (on_hand/available are summed from InventoryBalance rows, per the same
// outlet-scoping rules as the rest of ListItems), so they can't use the SQL-level ORDER BY
// ListSortFields/listOrder does. ListItems detects a request for one of these via
// balanceSortField and sorts the built DTOs in Go instead.
var ListBalanceSortFields = map[string]bool{
	"on_hand":   true,
	"available": true,
}

type listBalanceSortKey struct{}

// WithListSort orders ListItems by a whitelisted column (SQL-level, ListSortFields) or a
// computed balance field (in-Go, ListBalanceSortFields); dir "desc" descends, else ascends.
// Unknown fields are ignored (default SKU order) so a stale client can't 500 the list.
func WithListSort(ctx context.Context, field, dir string) context.Context {
	if dir != "desc" {
		dir = "asc"
	}
	if ListBalanceSortFields[field] {
		return context.WithValue(ctx, listBalanceSortKey{}, [2]string{field, dir})
	}
	if _, ok := ListSortFields[field]; !ok {
		return ctx
	}
	return context.WithValue(ctx, listSortKey{}, [2]string{field, dir})
}

// listOrder resolves the ctx sort into an ent order option (default: SKU ascending).
func listOrder(ctx context.Context) item.OrderOption {
	if v, ok := ctx.Value(listSortKey{}).([2]string); ok {
		if field, known := ListSortFields[v[0]]; known {
			if v[1] == "desc" {
				return item.OrderOption(ent.Desc(field))
			}
			return item.OrderOption(ent.Asc(field))
		}
	}
	return item.OrderOption(ent.Asc(item.FieldSku))
}

// balanceSortField resolves a ctx-carried computed-balance-field sort request, if any.
func balanceSortField(ctx context.Context) (field, dir string, ok bool) {
	if v, has := ctx.Value(listBalanceSortKey{}).([2]string); has {
		return v[0], v[1], true
	}
	return "", "", false
}

// balanceSortValue reads the requested computed field off a built DTO, treating an unstocked
// item (nil pointer — no InventoryBalance row) as 0 so it sorts to the low-stock end either way.
func balanceSortValue(dto ItemDTO, field string) float64 {
	switch field {
	case "on_hand":
		if dto.OnHand != nil {
			return *dto.OnHand
		}
	case "available":
		if dto.Available != nil {
			return *dto.Available
		}
	}
	return 0
}

// StandardTags defines well-known dietary and allergen tag values.
var StandardTags = []string{
	"vegan", "vegetarian", "gluten_free", "dairy_free", "nut_free",
	"halal", "kosher", "organic", "spicy", "contains_nuts",
	"contains_dairy", "contains_gluten", "sugar_free", "low_cal",
}

type ItemDTO struct {
	ID          uuid.UUID  `json:"id"`
	SKU         string     `json:"sku"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	CategoryID  *uuid.UUID `json:"category_id,omitempty"`
	BrandID     *uuid.UUID `json:"brand_id,omitempty"`
	UnitID      *uuid.UUID `json:"unit_id,omitempty"`
	// Preferred Supplier for procurement (drives per-vendor PO split in procure-to-order).
	// Accepted on create/update; PreferredSupplierName is read-only (populated when the edge is loaded).
	PreferredSupplierID   *uuid.UUID     `json:"preferred_supplier_id,omitempty"`
	PreferredSupplierName string         `json:"preferred_supplier_name,omitempty"`
	Type                  string         `json:"type"` // GOODS | SERVICE | RECIPE | INGREDIENT
	IsActive              bool           `json:"is_active"`
	ImageURL              string         `json:"image_url,omitempty"`
	Tags                  []string       `json:"tags,omitempty"`
	Metadata              map[string]any `json:"metadata,omitempty"`
	InitialQuantity       float64        `json:"initial_quantity,omitempty"` // opening on-hand in the item's base unit; fractional allowed (e.g. 4.5 L)
	ReorderLevel          int            `json:"reorder_level"`
	ReorderQuantity       int            `json:"reorder_quantity"`
	SuggestedPrice        *float64       `json:"suggested_price,omitempty"`
	AddToAllOutlets       bool           `json:"add_to_all_outlets,omitempty"`
	CategoryName          string         `json:"category_name,omitempty"`
	BrandName             string         `json:"brand_name,omitempty"`
	BrandCode             string         `json:"brand_code,omitempty"`
	// Item-attribute fields (retail / pharmacy)
	Manufacturer string `json:"manufacturer,omitempty"` // retail/pharmacy
	Model        string `json:"model,omitempty"`        // retail only
	// Drug-master fields (pharmacy) — surfaced so the POS prescription drug picker can
	// auto-fill dosage/form from the catalog instead of requiring manual re-entry.
	GenericName                 string `json:"generic_name,omitempty"`
	ActiveIngredient            string `json:"active_ingredient,omitempty"`
	DosageForm                  string `json:"dosage_form,omitempty"` // e.g. Tablet, Capsule, Syrup
	Strength                    string `json:"strength,omitempty"`    // e.g. 500mg
	DrugClass                   string `json:"drug_class,omitempty"`
	ControlledSubstanceSchedule string `json:"controlled_substance_schedule,omitempty"`
	// E-commerce / online-store attributes
	GTIN             string `json:"gtin,omitempty"`              // GTIN-8/12/13/14 for marketplace feeds
	MPN              string `json:"mpn,omitempty"`               // manufacturer part number
	Condition        string `json:"condition,omitempty"`         // NEW | REFURBISHED | USED | OPEN_BOX
	Slug             string `json:"slug,omitempty"`              // storefront URL slug / SEO
	ShortDescription string `json:"short_description,omitempty"` // product-card description
	MetaTitle        string `json:"meta_title,omitempty"`        // SEO
	MetaDescription  string `json:"meta_description,omitempty"`  // SEO
	CountryOfOrigin  string `json:"country_of_origin,omitempty"` // customs / marketplace compliance
	HSCode           string `json:"hs_code,omitempty"`           // customs tariff code
	// Pointers so a partial update (a client that doesn't send them) never clobbers the
	// stored flag, and so create can distinguish "unset" (use schema default) from an
	// explicit false — same rationale as NonBillable below. A plain bool can't do either.
	IsReturnable *bool `json:"is_returnable,omitempty"` // customer return allowed
	// Non-billable: never charged at POS even when a selling price exists (free
	// accompaniments like ugali, consumable supplies like tissue/packaging); stock still
	// deducts. Pointer so partial updates never clobber the stored flag.
	NonBillable *bool `json:"non_billable,omitempty"`
	// Not-for-sale: excluded from EVERY sales surface (POS terminal, back-office sales,
	// ordering storefront) while remaining fully stockable/purchasable — raw ingredients,
	// cleaning supplies, internal consumables. Distinct from NonBillable (still sold at 0)
	// and is_active=false (hidden everywhere). Pointer for partial-update semantics.
	NotForSale *bool `json:"not_for_sale,omitempty"`
	// Usable-in-recipes: a RECIPE-type item flagged here may be picked as an ingredient
	// in other recipes (reusable menu component, e.g. Black Tea inside an Iced Passion
	// Tea). Pointer for the same partial-update semantics as NonBillable.
	UsableInRecipes  *bool `json:"usable_in_recipes,omitempty"`
	ReturnWindowDays *int  `json:"return_window_days,omitempty"` // nil = tenant default
	AllowBackorder   *bool `json:"allow_backorder,omitempty"`    // order when out of stock
	IsDiscontinued   *bool `json:"is_discontinued,omitempty"`    // hidden from new listings, stock still sellable
	// End-of-Life: non-null = the item is marked EOL (hidden everywhere; is_active is false)
	// and awaiting hard-delete by the purge scheduler once past the retention window.
	EndOfLifeAt *time.Time `json:"end_of_life_at,omitempty"`
	// Product variations — surfaced from the ItemVariant edge so retail can sell variations.
	// HasVariants is always populated; Variants is populated when variants are eager-loaded
	// (inline for single-item reads, or for the list when ?include=variants is requested).
	HasVariants bool         `json:"has_variants"`
	Variants    []VariantDTO `json:"variants,omitempty"`
	// Images surfaces the item's IMAGE assets (multi-image gallery). Populated when the
	// `assets` edge is eager-loaded; primary first. ImageURL above remains the primary
	// image URL for backward compatibility with single-image clients.
	Images []ItemImageDTO `json:"images,omitempty"`
	// Extended fields for POS, logistics, compliance
	Barcode                 string             `json:"barcode,omitempty"`
	BarcodeType             string             `json:"barcode_type,omitempty"`
	RequiresAgeVerification bool               `json:"requires_age_verification"`
	IsControlledSubstance   bool               `json:"is_controlled_substance"` // pharmacy: scheduled drugs
	IsPerishable            bool               `json:"is_perishable"`
	TrackLots               bool               `json:"track_lots"`
	TrackSerialNumbers      bool               `json:"track_serial_numbers"`
	ShelfLifeDays           *int               `json:"shelf_life_days,omitempty"` // default shelf life; seeds lot expiry at receipt
	WeightKg                *float64           `json:"weight_kg,omitempty"`
	DimensionsCm            map[string]float64 `json:"dimensions_cm,omitempty"`
	DurationMinutes         *int               `json:"duration_minutes,omitempty"` // service duration (salon/barber)
	// Cost / pricing fields
	CostPrice *float64 `json:"cost_price,omitempty"`
	// Effective customer-facing price + tax split — enriched at read time for the POS/ordering
	// proxies (recipe selling price → default pricing tier → cost+margin suggestion).
	SellingPrice *float64 `json:"selling_price,omitempty"` // what the customer pays (gross when tax-inclusive)
	NetPrice     *float64 `json:"net_price,omitempty"`     // selling price excluding tax
	TaxAmount    *float64 `json:"tax_amount,omitempty"`    // tax portion of the selling price
	TaxRate      *float64 `json:"tax_rate,omitempty"`      // VAT rate % applied (resolved from treasury-api)
	// Purchase / supplier fields — enable auto EP-cost calculation
	PurchasePrice    *float64 `json:"purchase_price,omitempty"`
	PurchasePackSize *float64 `json:"purchase_pack_size,omitempty"`
	PurchaseUnit     string   `json:"purchase_unit,omitempty"`
	YieldPct         *float64 `json:"yield_pct,omitempty"` // 0 < y <= 1; default 1.0
	// Content-per-unit: how much of UnitContentUOM ONE stock unit contains (a 750ml
	// whiskey bottle stocked in pieces → 750 + "ml"). Lets ml/g recipe lines (tots,
	// pours) deduct fractional stock units.
	UnitContentQty *float64 `json:"unit_content_qty,omitempty"`
	UnitContentUOM string   `json:"unit_content_uom,omitempty"`
	// Stock tracking mode: "default" (RECIPE items follow the tenant non-depletion
	// policy) | "tracked" | "non_depleting" (sells without stock effect).
	StockTrackingMode string `json:"stock_tracking_mode,omitempty"`
	// Selling-price guardrails + goods margin (Phase 4).
	MinSellingPrice     *float64 `json:"min_selling_price,omitempty"`     // hard floor enforced at price upsert & POS
	MaxSellingPrice     *float64 `json:"max_selling_price,omitempty"`     // hard ceiling enforced at price upsert & POS
	TargetMarginPercent *float64 `json:"target_margin_percent,omitempty"` // GOODS auto-pricing margin %
	// KRA eTIMS tax fields
	TaxCodeID    string `json:"tax_code_id,omitempty"`
	TaxInclusive bool   `json:"tax_inclusive"`
	// KRA eTIMS catalog classification (drives treasury's eTIMS item registration —
	// inventory is the item source of truth; empty values fall back at registration).
	EtimsItemClsCd string `json:"etims_item_cls_cd,omitempty"` // UNSPSC leaf from KRA selectItemClass
	EtimsPkgUnitCd string `json:"etims_pkg_unit_cd,omitempty"` // KRA packaging unit (NT/CT/BX…)
	EtimsQtyUnitCd string `json:"etims_qty_unit_cd,omitempty"` // KRA quantity unit override (else the unit's mapping)
	// Event capacity fields — SERVICE type only
	TotalCapacity  *int       `json:"total_capacity,omitempty"`
	BookedCapacity *int       `json:"booked_capacity,omitempty"`
	EventStartAt   *time.Time `json:"event_start_at,omitempty"`
	EventEndAt     *time.Time `json:"event_end_at,omitempty"`
	EventVenue     *string    `json:"event_venue,omitempty"`
	// Hospitality fields — room-type / facility / amenity SERVICE items
	UseCase          string   `json:"use_case,omitempty"`  // RETAIL | FOOD_BEVERAGE | HOSPITALITY_ROOM | HOSPITALITY_FACILITY | CONFERENCE | SALON_SERVICE | AMENITY
	MealPlan         *string  `json:"meal_plan,omitempty"` // RO | BB | HB | FB | AI
	OccupancyBasis   *string  `json:"occupancy_basis,omitempty"`
	MaxAdults        *int     `json:"max_adults,omitempty"`
	MaxChildren      *int     `json:"max_children,omitempty"`
	ExtraBedAllowed  bool     `json:"extra_bed_allowed"`
	SingleSupplement *float64 `json:"single_supplement,omitempty"`
	// Current stock levels (aggregated across all warehouses).
	// Populated by ListItems; nil when no balance row exists.
	Available *float64 `json:"available,omitempty"`
	OnHand    *float64 `json:"on_hand,omitempty"`
	// UnitAbbreviation/UnitName resolve UnitID to display text (e.g. "btl"/"BOTTLE") so a
	// bridged item's on_hand/available (a fractional STOCK-unit count, not the content unit)
	// never renders as a bare, unlabeled number — see AvailableContentQty below for the
	// content-bridge companion figure. Populated by ListItems/buildDTOs; empty when the item
	// has no unit_id.
	UnitAbbreviation string `json:"unit_abbreviation,omitempty"`
	UnitName         string `json:"unit_name,omitempty"`
	// AvailableContentQty is Available expressed in the item's content-bridge unit (e.g. a
	// 0.86 btl balance with unit_content_qty=50/uom=ml -> 43 ml) — nil unless the item has a
	// unit_content bridge configured. Lets the UI show "0.86 btl (~43 ml)" instead of a bare
	// fractional stock-unit count that reads as meaningless to anyone not versed in the
	// content-bridge model.
	AvailableContentQty *float64  `json:"available_content_qty,omitempty"`
	OnHandContentQty    *float64  `json:"on_hand_content_qty,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	// ModifierGroups: this item's selectable modifiers (e.g. "Extra Honey" on a Dawa),
	// enriched by enrichModifierGroups. Populated by ListItems for every catalog-facing
	// caller (pos-api's terminal catalog proxy included) — see modifier_enrich.go.
	ModifierGroups []ItemModifierGroup `json:"modifier_groups,omitempty"`
}

// VariantDTO is the catalog-facing projection of an ItemVariant. Surfaced on the item
// so POS/ordering catalog proxies can offer variations for sale.
type VariantDTO struct {
	ID         uuid.UUID         `json:"id"`
	SKU        string            `json:"sku"`
	Name       string            `json:"name"`
	Price      float64           `json:"price"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Barcode    string            `json:"barcode,omitempty"`
	IsActive   bool              `json:"is_active"`
}

// ItemModifierOption is the catalog-facing projection of a ModifierOption.
type ItemModifierOption struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	SKU             string    `json:"sku,omitempty"`
	PriceAdjustment float64   `json:"price_adjustment"`
	IsDefault       bool      `json:"is_default"`
}

// ItemModifierGroup is the catalog-facing projection of a ModifierGroup + its options.
type ItemModifierGroup struct {
	ID            uuid.UUID            `json:"id"`
	Name          string               `json:"name"`
	IsRequired    bool                 `json:"is_required"`
	MinSelections int                  `json:"min_selections"`
	MaxSelections int                  `json:"max_selections"`
	Options       []ItemModifierOption `json:"options"`
}

type CategoryDTO struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Code        string     `json:"code,omitempty"`
	Description string     `json:"description,omitempty"`
	Icon        string     `json:"icon,omitempty"`
	ParentID    *uuid.UUID `json:"parent_id,omitempty"`
	ParentName  string     `json:"parent_name,omitempty"`
	IsActive    bool       `json:"is_active"`
	IsGlobal    bool       `json:"is_global,omitempty"`
	// Depth/Path mirror the ent schema's materialized-path hierarchy fields
	// (0 = root; path = root-id/parent-id/self-id) so clients (e.g. the
	// ordering-frontend "Shop by Category" flyout) can render trees without a
	// second round trip. Maintained by CreateCategory/UpdateCategory — see
	// resolveCategoryPath and cascadeCategoryDescendants below.
	Depth     int    `json:"depth"`
	Path      string `json:"path,omitempty"`
	SortOrder int    `json:"sort_order,omitempty"`
	// Outlet use_cases this category is relevant to (hospitality, pharmacy,
	// retail…); empty = universal. Keeps food categories out of a pharmacy
	// outlet's pickers (and vice versa) on mixed-use tenants.
	UseCases  []string  `json:"use_cases,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RelevantToUseCase reports whether the category applies to an outlet use_case:
// untagged categories are universal; tagged ones match only their use_cases.
func (d CategoryDTO) RelevantToUseCase(useCase string) bool {
	if useCase == "" || len(d.UseCases) == 0 {
		return true
	}
	for _, uc := range d.UseCases {
		if strings.EqualFold(uc, useCase) {
			return true
		}
	}
	return false
}

// StockAvailability matches the DTO expected by the ordering-backend client.
type StockAvailability struct {
	ItemID              uuid.UUID  `json:"item_id"`
	SKU                 string     `json:"sku"`
	WarehouseID         uuid.UUID  `json:"warehouse_id"`
	OnHand              float64    `json:"on_hand"`
	Available           float64    `json:"available"`
	Reserved            float64    `json:"reserved"`
	UnitOfMeasure       string     `json:"unit_of_measure"`
	ReorderLevel        int        `json:"reorder_level"`
	ReorderQuantity     int        `json:"reorder_quantity"`
	PreferredSupplierID *uuid.UUID `json:"preferred_supplier_id,omitempty"`
	UpdatedAt           string     `json:"updated_at"`
}

// Service handles item-related business logic.
type Service struct {
	client       *ent.Client
	cache        *sharedcache.Aside
	log          *zap.Logger
	mediaURLBase string      // public base URL for resolving relative /media/ paths
	mediaRoot    string      // filesystem root for persisting uploaded item images (MEDIA_ROOT)
	taxResolver  TaxResolver // resolves VAT rate from treasury-api (optional; nil → DefaultVATRate)
	auditSvc     *audit.Service
	// readClient, when set, is used ONLY for ListItems' multi-row catalog fetch (see rc()) — a
	// heavy, staleness-tolerant read routed to a replica when one is configured. Every other
	// query in this service (single-item lookups, writes, OutletScope) always uses client
	// (primary), unchanged.
	readClient *ent.Client
	// seq, when set, drives GenerateSKU's numeric-by-default auto-generated SKUs via the shared
	// document-sequence system. Nil (e.g. unwired scripts/tests) falls back to the legacy
	// category-prefix algorithm unconditionally — see GenerateSKU.
	seq *documents.SequenceService
}

// NewService creates a new items service.
func NewService(client *ent.Client, log *zap.Logger, mediaURLBase string) *Service {
	return &Service{
		client:       client,
		mediaURLBase: strings.TrimRight(mediaURLBase, "/"),
		log:          log.Named("items.service"),
	}
}

// SetReadClient wires an optional read-replica Ent client for ListItems' heavy catalog fetch.
// Nil (the default) means rc() falls back to client (primary) — zero behavior change when unset.
func (s *Service) SetReadClient(c *ent.Client) { s.readClient = c }

// SetSequenceService wires the document-sequence service so auto-generated SKUs are minted
// numeric-by-default (per tenant), falling back to the legacy category-prefix algorithm when
// unset — see GenerateSKU.
func (s *Service) SetSequenceService(seq *documents.SequenceService) { s.seq = seq }

// rc returns the read-replica client when one is configured, else the primary — see readClient.
func (s *Service) rc() *ent.Client {
	if s.readClient != nil {
		return s.readClient
	}
	return s.client
}

// resolveMediaURL converts a relative /media/ path to a full URL using MEDIA_URL_BASE.
// Also encodes spaces in filenames to ensure valid URLs.
func (s *Service) resolveMediaURL(path string) string {
	if path == "" || strings.HasPrefix(path, "http") {
		// Even full URLs may have unencoded spaces from legacy data
		return strings.ReplaceAll(path, " ", "%20")
	}
	path = strings.ReplaceAll(path, " ", "%20")
	if s.mediaURLBase != "" {
		return s.mediaURLBase + path
	}
	return path
}

// SetCache injects the cache helper (optional; caching is skipped if nil).
func (s *Service) SetCache(c *sharedcache.Aside) {
	s.cache = c
}

// SetTaxResolver injects the treasury-api tax-rate resolver (optional).
func (s *Service) SetTaxResolver(r TaxResolver) {
	s.taxResolver = r
}

// SetAuditService injects the centralized audit trail (optional) for standard-cost and
// selling-price changes made through this service.
func (s *Service) SetAuditService(a *audit.Service) {
	s.auditSvc = a
}

// actorFromContext resolves the acting user from the request's auth claims, falling back to
// uuid.Nil for S2S/system-initiated calls (bulk import, event-driven upserts) where there is no
// human actor — the audit row is still written, just without an attributable user.
func actorFromContext(ctx context.Context) uuid.UUID {
	if claims, ok := authclient.ClaimsFromContext(ctx); ok {
		if id, err := claims.UserID(); err == nil {
			return id
		}
	}
	return uuid.Nil
}

// GetStockAvailability returns stock availability for a single item by SKU.
// If the item type is RECIPE, it resolves the BOM and returns the minimum
// available portions based on ingredient stock levels (BOM explosion).
func (s *Service) GetStockAvailability(ctx context.Context, tenantID uuid.UUID, sku string) (*StockAvailability, error) {
	itm, err := s.client.Item.Query().
		Where(
			item.TenantID(tenantID),
			item.Sku(sku),
			item.IsActive(true),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("items: item not found: sku=%s", sku)
		}
		return nil, fmt.Errorf("items: query item: %w", err)
	}

	// BOM explosion: if item type is RECIPE, compute available portions from ingredients
	if itm.Type == item.TypeRECIPE {
		return s.getRecipeAvailability(ctx, tenantID, itm)
	}

	return s.getDirectAvailability(ctx, tenantID, itm)
}

// getDirectAvailability returns availability for a non-recipe item directly from InventoryBalance.
func (s *Service) getDirectAvailability(ctx context.Context, tenantID uuid.UUID, itm *ent.Item) (*StockAvailability, error) {
	bal, err := s.client.InventoryBalance.Query().
		Where(
			inventorybalance.TenantID(tenantID),
			inventorybalance.ItemID(itm.ID),
		).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return &StockAvailability{
				ItemID:        itm.ID,
				SKU:           itm.Sku,
				WarehouseID:   uuid.Nil,
				OnHand:        0,
				Available:     0,
				Reserved:      0,
				UnitOfMeasure: "",
				UpdatedAt:     itm.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			}, nil
		}
		return nil, fmt.Errorf("items: query balance: %w", err)
	}

	return &StockAvailability{
		ItemID:              itm.ID,
		SKU:                 itm.Sku,
		WarehouseID:         bal.WarehouseID,
		OnHand:              bal.OnHand,
		Available:           bal.Available,
		Reserved:            bal.Reserved,
		UnitOfMeasure:       bal.UnitOfMeasure,
		ReorderLevel:        bal.ReorderLevel,
		ReorderQuantity:     bal.ReorderQuantity,
		PreferredSupplierID: bal.PreferredSupplierID,
		UpdatedAt:           bal.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}, nil
}

// getRecipeAvailability performs BOM explosion: for a RECIPE item, looks up the recipe,
// checks each ingredient's available balance, and returns the minimum number of portions
// that can be produced (floor(ingredient_available / ingredient_qty_per_portion)).
func (s *Service) getRecipeAvailability(ctx context.Context, tenantID uuid.UUID, itm *ent.Item) (*StockAvailability, error) {
	rec, err := s.client.Recipe.Query().
		Where(recipe.TenantID(tenantID), recipe.Sku(itm.Sku), recipe.IsActive(true)).
		WithIngredients(func(q *ent.RecipeIngredientQuery) {
			q.Order(ent.Asc(recipeingredient.FieldDisplayOrder))
		}).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			// No BOM defined — fall back to direct balance check
			return s.getDirectAvailability(ctx, tenantID, itm)
		}
		return nil, fmt.Errorf("items: lookup recipe for sku=%s: %w", itm.Sku, err)
	}

	if len(rec.Edges.Ingredients) == 0 {
		return s.getDirectAvailability(ctx, tenantID, itm)
	}

	// Collect ingredient item IDs
	ingredientIDs := make([]uuid.UUID, len(rec.Edges.Ingredients))
	for i, ing := range rec.Edges.Ingredients {
		ingredientIDs[i] = ing.ItemID
	}

	// Fetch all ingredient balances in one query
	balances, err := s.client.InventoryBalance.Query().
		Where(
			inventorybalance.TenantID(tenantID),
			inventorybalance.ItemIDIn(ingredientIDs...),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: query ingredient balances: %w", err)
	}

	balMap := make(map[uuid.UUID]float64, len(balances))
	for _, b := range balances {
		balMap[b.ItemID] = b.Available
	}

	// BOM explosion: compute minimum available portions
	outputQty := rec.OutputQty
	if outputQty <= 0 {
		outputQty = 1
	}

	minPortions := math.MaxFloat64
	for _, ing := range rec.Edges.Ingredients {
		available := float64(balMap[ing.ItemID])
		qtyPerPortion := ing.Quantity / outputQty
		if qtyPerPortion <= 0 {
			continue
		}
		portions := available / qtyPerPortion
		if portions < minPortions {
			minPortions = portions
		}
	}

	if minPortions == math.MaxFloat64 || minPortions < 0 {
		// A negative minPortions means at least one ingredient is itself in oversold/negative
		// balance territory (now a legitimate state — see [[oversell-negative-stock-settlement]]).
		// "Portions currently producible" can't itself be negative, so floor at 0 here — mirrors
		// stock/cascade.go's produciblePortions, which already does the same.
		minPortions = 0
	}

	availablePortions := int(math.Floor(minPortions))

	return &StockAvailability{
		ItemID:        itm.ID,
		SKU:           itm.Sku,
		WarehouseID:   uuid.Nil,
		OnHand:        float64(availablePortions),
		Available:     float64(availablePortions),
		Reserved:      0,
		UnitOfMeasure: rec.UnitOfMeasure,
		UpdatedAt:     itm.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}, nil
}

// BOMAvailabilityResult represents the BOM-aware availability for a single SKU.
type BOMAvailabilityResult struct {
	SKU           string    `json:"sku"`
	ItemID        uuid.UUID `json:"item_id"`
	Available     float64   `json:"available"`
	Type          string    `json:"type"` // "recipe" or "simple"
	UnitOfMeasure string    `json:"unit_of_measure,omitempty"`
	UpdatedAt     string    `json:"updated_at"`
}

// GetBOMAvailability returns BOM-aware availability for multiple SKUs.
// For RECIPE items, it computes the maximum portions producible from ingredient stock.
// For non-recipe items, it returns direct stock availability.
func (s *Service) GetBOMAvailability(ctx context.Context, tenantID uuid.UUID, skus []string) ([]BOMAvailabilityResult, error) {
	results := make([]BOMAvailabilityResult, 0, len(skus))
	// Preload every requested item in ONE query instead of an Item.Query per SKU (N+1 on the
	// terminal's availability check). The per-recipe BOM computation below is inherently per-item.
	itemsBySku := make(map[string]*ent.Item, len(skus))
	if items, err := s.client.Item.Query().
		Where(item.TenantID(tenantID), item.SkuIn(skus...), item.IsActive(true)).
		All(ctx); err == nil {
		for _, it := range items {
			itemsBySku[it.Sku] = it
		}
	}
	for _, sku := range skus {
		itm := itemsBySku[sku]
		if itm == nil {
			s.log.Warn("bom availability: item not found", zap.String("sku", sku))
			continue
		}

		if itm.Type == item.TypeRECIPE {
			avail, err := s.getRecipeAvailability(ctx, tenantID, itm)
			if err != nil {
				s.log.Warn("bom availability: recipe check failed", zap.String("sku", sku), zap.Error(err))
				// Fall back to simple
				avail, err = s.getDirectAvailability(ctx, tenantID, itm)
				if err != nil {
					continue
				}
				results = append(results, BOMAvailabilityResult{
					SKU:           sku,
					ItemID:        itm.ID,
					Available:     avail.Available,
					Type:          "simple",
					UnitOfMeasure: avail.UnitOfMeasure,
					UpdatedAt:     avail.UpdatedAt,
				})
				continue
			}
			results = append(results, BOMAvailabilityResult{
				SKU:           sku,
				ItemID:        itm.ID,
				Available:     avail.Available,
				Type:          "recipe",
				UnitOfMeasure: avail.UnitOfMeasure,
				UpdatedAt:     avail.UpdatedAt,
			})
		} else {
			avail, err := s.getDirectAvailability(ctx, tenantID, itm)
			if err != nil {
				s.log.Warn("bom availability: direct check failed", zap.String("sku", sku), zap.Error(err))
				continue
			}
			results = append(results, BOMAvailabilityResult{
				SKU:           sku,
				ItemID:        itm.ID,
				Available:     avail.Available,
				Type:          "simple",
				UnitOfMeasure: avail.UnitOfMeasure,
				UpdatedAt:     avail.UpdatedAt,
			})
		}
	}
	return results, nil
}

// BulkAvailability returns stock availability for multiple items by SKU.
func (s *Service) BulkAvailability(ctx context.Context, tenantID uuid.UUID, skus []string) ([]StockAvailability, error) {
	items, err := s.client.Item.Query().
		Where(
			item.TenantID(tenantID),
			item.SkuIn(skus...),
			item.IsActive(true),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: query items: %w", err)
	}

	itemIDs := make([]uuid.UUID, len(items))
	itemMap := make(map[uuid.UUID]*ent.Item, len(items))
	for i, itm := range items {
		itemIDs[i] = itm.ID
		itemMap[itm.ID] = itm
	}

	balances, err := s.client.InventoryBalance.Query().
		Where(
			inventorybalance.TenantID(tenantID),
			inventorybalance.ItemIDIn(itemIDs...),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: query balances: %w", err)
	}

	balMap := make(map[uuid.UUID]*ent.InventoryBalance, len(balances))
	for _, b := range balances {
		balMap[b.ItemID] = b
	}

	result := make([]StockAvailability, 0, len(items))
	for _, itm := range items {
		avail := StockAvailability{
			ItemID:    itm.ID,
			SKU:       itm.Sku,
			UpdatedAt: itm.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		}
		if bal, ok := balMap[itm.ID]; ok {
			avail.WarehouseID = bal.WarehouseID
			avail.OnHand = bal.OnHand
			avail.Available = bal.Available
			avail.Reserved = bal.Reserved
			avail.UpdatedAt = bal.UpdatedAt.Format("2006-01-02T15:04:05Z")
		}
		result = append(result, avail)
	}

	return result, nil
}

// InventorySummary represents high-level stock metrics.
type InventorySummary struct {
	TotalItems          int `json:"total_items"`
	LowStockItems       int `json:"low_stock_items"`
	OutOfStockItems     int `json:"out_of_stock_items"`
	PendingReservations int `json:"pending_reservations"`
	WarehouseCount      int `json:"warehouse_count"`
}

// GetInventorySummary returns aggregated stock metrics for a tenant.
func (s *Service) GetInventorySummary(ctx context.Context, tenantID uuid.UUID) (*InventorySummary, error) {
	total, err := s.client.Item.Query().
		Where(item.TenantID(tenantID), item.IsActive(true)).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: count total items: %w", err)
	}

	// Assuming 10 is the default low stock threshold if not specified on item
	lowStock, err := s.client.InventoryBalance.Query().
		Where(
			inventorybalance.TenantID(tenantID),
			inventorybalance.AvailableLTE(10), // Simplification: threshold = 10
			inventorybalance.AvailableGT(0),
		).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: count low stock: %w", err)
	}

	outOfStock, err := s.client.InventoryBalance.Query().
		Where(
			inventorybalance.TenantID(tenantID),
			inventorybalance.AvailableLTE(0),
		).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: count out of stock: %w", err)
	}

	pendingReservations, err := s.client.Reservation.Query().
		Where(reservation.TenantID(tenantID), reservation.StatusEQ("pending")).
		Count(ctx)
	if err != nil {
		pendingReservations = 0 // non-fatal
	}

	warehouseCount, err := s.client.Warehouse.Query().
		Where(warehouse.TenantID(tenantID), warehouse.IsActive(true)).
		Count(ctx)
	if err != nil {
		warehouseCount = 0 // non-fatal
	}

	return &InventorySummary{
		TotalItems:          total,
		LowStockItems:       lowStock,
		OutOfStockItems:     outOfStock,
		PendingReservations: pendingReservations,
		WarehouseCount:      warehouseCount,
	}, nil
}

// boolPtr returns a pointer copy of b (DTO pointer-bool fields, e.g. non_billable).
func boolPtr(b bool) *bool { return &b }

func (s *Service) mapToDTO(i *ent.Item) *ItemDTO {
	dto := &ItemDTO{
		ID:                          i.ID,
		SKU:                         i.Sku,
		Name:                        i.Name,
		Description:                 i.Description,
		CategoryID:                  i.CategoryID,
		PreferredSupplierID:         i.PreferredSupplierID,
		BrandID:                     i.BrandID,
		UnitID:                      i.UnitID,
		Type:                        string(i.Type),
		IsActive:                    i.IsActive,
		Manufacturer:                i.Manufacturer,
		Model:                       i.Model,
		GenericName:                 i.GenericName,
		ActiveIngredient:            i.ActiveIngredient,
		DosageForm:                  i.DosageForm,
		Strength:                    i.Strength,
		DrugClass:                   i.DrugClass,
		ControlledSubstanceSchedule: string(i.ControlledSubstanceSchedule),
		GTIN:                        i.Gtin,
		MPN:                         i.Mpn,
		Condition:                   string(i.Condition),
		Slug:                        i.Slug,
		ShortDescription:            i.ShortDescription,
		MetaTitle:                   i.MetaTitle,
		MetaDescription:             i.MetaDescription,
		CountryOfOrigin:             i.CountryOfOrigin,
		HSCode:                      i.HsCode,
		IsReturnable:                boolPtr(i.IsReturnable),
		NonBillable:                 boolPtr(i.NonBillable),
		NotForSale:                  boolPtr(i.NotForSale),
		UsableInRecipes:             boolPtr(i.UsableInRecipes),
		ReturnWindowDays:            i.ReturnWindowDays,
		AllowBackorder:              boolPtr(i.AllowBackorder),
		IsDiscontinued:              boolPtr(i.IsDiscontinued),
		EndOfLifeAt:                 i.EndOfLifeAt,
		ImageURL:                    s.resolveMediaURL(i.ImageURL),
		Tags:                        i.Tags,
		Metadata:                    i.Metadata,
		Barcode:                     i.Barcode,
		BarcodeType:                 i.BarcodeType,
		RequiresAgeVerification:     i.RequiresAgeVerification,
		IsControlledSubstance:       i.IsControlledSubstance,
		IsPerishable:                i.IsPerishable,
		TrackLots:                   i.TrackLots,
		TrackSerialNumbers:          i.TrackSerialNumbers,
		ShelfLifeDays:               i.ShelfLifeDays,
		WeightKg:                    i.WeightKg,
		DimensionsCm:                i.DimensionsCm,
		DurationMinutes:             i.DurationMinutes,
		CostPrice:                   i.CostPrice,
		PurchasePrice:               i.PurchasePrice,
		PurchasePackSize:            i.PurchasePackSize,
		PurchaseUnit:                i.PurchaseUnit,
		YieldPct:                    i.YieldPct,
		UnitContentQty:              i.UnitContentQty,
		UnitContentUOM:              i.UnitContentUom,
		StockTrackingMode:           string(i.StockTrackingMode),
		MinSellingPrice:             i.MinSellingPrice,
		MaxSellingPrice:             i.MaxSellingPrice,
		TargetMarginPercent:         i.TargetMarginPercent,
		TaxCodeID:                   i.TaxCodeID,
		TaxInclusive:                i.TaxInclusive,
		EtimsItemClsCd:              derefStr(i.EtimsItemClsCd),
		EtimsPkgUnitCd:              derefStr(i.EtimsPkgUnitCd),
		EtimsQtyUnitCd:              derefStr(i.EtimsQtyUnitCd),
		TotalCapacity:               i.TotalCapacity,
		BookedCapacity:              i.BookedCapacity,
		EventStartAt:                i.EventStartAt,
		EventEndAt:                  i.EventEndAt,
		EventVenue:                  i.EventVenue,
		UseCase:                     string(i.UseCase),
		MealPlan:                    enumPtrToStr(i.MealPlan),
		OccupancyBasis:              enumPtrToStr(i.OccupancyBasis),
		MaxAdults:                   i.MaxAdults,
		MaxChildren:                 i.MaxChildren,
		ExtraBedAllowed:             i.ExtraBedAllowed,
		SingleSupplement:            i.SingleSupplement,
		CreatedAt:                   i.CreatedAt,
		UpdatedAt:                   i.UpdatedAt,
	}
	// Surface the preferred supplier's display name when the edge has been eager-loaded.
	if sup, err := i.Edges.PreferredSupplierOrErr(); err == nil && sup != nil {
		dto.PreferredSupplierName = sup.Name
	}
	// Surface product variations when the variants edge has been eager-loaded.
	// Only active variants are exposed for sale; HasVariants reflects that count.
	if variants, err := i.Edges.VariantsOrErr(); err == nil {
		for _, v := range variants {
			if !v.IsActive {
				continue
			}
			dto.Variants = append(dto.Variants, VariantDTO{
				ID:         v.ID,
				SKU:        v.Sku,
				Name:       v.Name,
				Price:      v.Price,
				Attributes: v.Attributes,
				Barcode:    v.Barcode,
				IsActive:   v.IsActive,
			})
		}
		dto.HasVariants = len(dto.Variants) > 0
	}
	// Surface the multi-image gallery when the assets edge has been eager-loaded. Only IMAGE
	// assets are exposed; primary first, then by display_order. When no explicit image_url is
	// set on the item, fall back to the primary asset URL so legacy single-image reads still work.
	if assets, err := i.Edges.AssetsOrErr(); err == nil {
		imgs := make([]ItemImageDTO, 0, len(assets))
		for _, a := range assets {
			if a.AssetType != AssetTypeImage {
				continue
			}
			imgs = append(imgs, s.mapAssetToDTO(a))
		}
		// Stable order: primary first, then display_order, then created_at.
		sort.SliceStable(imgs, func(x, y int) bool {
			if imgs[x].IsPrimary != imgs[y].IsPrimary {
				return imgs[x].IsPrimary
			}
			if imgs[x].DisplayOrder != imgs[y].DisplayOrder {
				return imgs[x].DisplayOrder < imgs[y].DisplayOrder
			}
			return imgs[x].CreatedAt.Before(imgs[y].CreatedAt)
		})
		dto.Images = imgs
		if dto.ImageURL == "" && len(imgs) > 0 {
			for _, im := range imgs {
				if im.IsPrimary {
					dto.ImageURL = im.URL
					break
				}
			}
			if dto.ImageURL == "" {
				dto.ImageURL = imgs[0].URL
			}
		}
	}
	return dto
}

// enumPtrToStr converts a nillable Ent enum pointer to a *string for DTO output.
func enumPtrToStr[T ~string](v *T) *string {
	if v == nil {
		return nil
	}
	s := string(*v)
	return &s
}

// outletScopeCacheTTL bounds how long a computed outlet-visibility scope is reused across the
// many page-fetches of one catalog sweep. pos-api's fetchAllInventoryItemPages walks a tenant's
// full catalog up to 8 pages concurrently (comment there: "every branch's stock can need ~35-40
// pages"), and — before this cache — EVERY one of those pages re-ran OutletScope's full-tenant
// InventoryBalance scan from scratch. Confirmed live on boi-enterprises (3,896 items / 7
// warehouses / 7,305 balance rows, needing ~39 pages): the resulting redundant load saturated the
// 8-connection DB pool badly enough that some pages missed pos-api's per-call 15s S2S timeout,
// logging "items: count: context canceled" here and leaving the terminal/inventory screen looking
// broken right after a perfectly valid PIN login. A short TTL is enough to dedupe one sweep's
// repeated calls without meaningfully delaying how fast a stock move changes what an outlet sells.
const outletScopeCacheTTL = sharedcache.TTLOperational

// outletScopeCached is OutletScope's result in a JSON-serialisable (cache-friendly) shape —
// warehouseIDs as a slice instead of a map, reassembled by OutletScope after a cache hit.
type outletScopeCached struct {
	ExcludeIDs            []uuid.UUID `json:"exclude_ids"`
	HasOperationalHistory bool        `json:"has_operational_history"`
	WarehouseIDs          []uuid.UUID `json:"warehouse_ids"`
}

// OutletScope computes an outlet's catalog-visibility scope: which items to exclude, whether
// the outlet has operational history, and the warehouse set it sells from. This is the single
// source of truth behind ListItems' outlet filtering (see the long rule comment there) — any
// other consumer that reports per-outlet item/stock counts (e.g. dashboard analytics) MUST
// reuse this instead of re-deriving the rule, or its numbers will silently drift from the
// Products list the moment either copy changes.
// outletID == nil returns zero values (no scoping). Result is cached per (tenant, outlet) for
// outletScopeCacheTTL — see that constant's comment for why. A nil/unconfigured s.cache (e.g.
// tests) falls back to computing fresh on every call, unchanged from before this cache existed.
func (s *Service) OutletScope(ctx context.Context, tenantID uuid.UUID, outletID *uuid.UUID) (excludeIDs []uuid.UUID, hasOperationalHistory bool, warehouseIDs map[uuid.UUID]struct{}, err error) {
	if outletID == nil {
		return nil, false, nil, nil
	}
	key := sharedcache.Key("inv", "outlet-scope", tenantID.String(), outletID.String())
	cached, err := sharedcache.GetOrSet(ctx, s.cache, key, outletScopeCacheTTL, func(ctx context.Context) (outletScopeCached, error) {
		ids, hist, whIDs, ferr := s.outletScopeUncached(ctx, tenantID, *outletID)
		if ferr != nil {
			return outletScopeCached{}, ferr
		}
		whSlice := make([]uuid.UUID, 0, len(whIDs))
		for id := range whIDs {
			whSlice = append(whSlice, id)
		}
		return outletScopeCached{ExcludeIDs: ids, HasOperationalHistory: hist, WarehouseIDs: whSlice}, nil
	})
	if err != nil {
		return nil, false, nil, err
	}
	warehouseIDs = make(map[uuid.UUID]struct{}, len(cached.WarehouseIDs))
	for _, id := range cached.WarehouseIDs {
		warehouseIDs[id] = struct{}{}
	}
	return cached.ExcludeIDs, cached.HasOperationalHistory, warehouseIDs, nil
}

// outletScopeUncached is OutletScope's actual computation, unchanged from before caching was
// added — see OutletScope for the cache wrapper now in front of it.
func (s *Service) outletScopeUncached(ctx context.Context, tenantID uuid.UUID, outletID uuid.UUID) (excludeIDs []uuid.UUID, hasOperationalHistory bool, warehouseIDs map[uuid.UUID]struct{}, err error) {
	wIDs, err := s.client.Warehouse.Query().
		Where(
			warehouse.TenantID(tenantID),
			warehouse.Or(
				warehouse.OutletIDEQ(outletID),
				warehouse.OutletIDIsNil(),
			),
		).IDs(ctx)
	if err != nil {
		return nil, false, nil, err
	}
	warehouseIDs = make(map[uuid.UUID]struct{}, len(wIDs))
	for _, id := range wIDs {
		warehouseIDs[id] = struct{}{}
	}

	// Fast path: this outlet has never had ANY balance row (positive, zero, or removed) at its
	// own warehouse(s) — a fresh/untouched outlet (kiosk, or a tenant with a large unreceived
	// bulk-import backlog, both called out in the comments above). A cheap indexed existence
	// check answers this without loading the tenant's entire balance table: when it's false,
	// stockedHere/removedHere are GUARANTEED empty (both are strict subsets of "rows at this
	// outlet's own warehouses"), so `candidates` stays nil either way — byte-identical result to
	// the full computation below, just without paying for it.
	anyBalanceHere, err := s.client.InventoryBalance.Query().
		Where(inventorybalance.TenantIDEQ(tenantID), inventorybalance.WarehouseIDIn(wIDs...)).
		Exist(ctx)
	if err != nil {
		return nil, false, warehouseIDs, err
	}
	if !anyBalanceHere {
		return nil, false, warehouseIDs, nil
	}
	hasOperationalHistory = true

	// Project only the 3 columns actually read below — InventoryBalance also carries
	// cost/quantity/reorder fields that are irrelevant here, so this cuts bytes-on-wire and Go
	// allocations versus a full-row fetch without changing which rows are examined.
	bals, err := s.client.InventoryBalance.Query().
		Where(inventorybalance.TenantIDEQ(tenantID)).
		Select(inventorybalance.FieldItemID, inventorybalance.FieldWarehouseID, inventorybalance.FieldRemovedFromLocation).
		All(ctx)
	if err != nil {
		return nil, hasOperationalHistory, warehouseIDs, err
	}
	stockedHere := make(map[uuid.UUID]struct{})
	// removedHere: this item has a balance at one of THIS outlet's own warehouses that was
	// explicitly moved/removed away (InventoryBalance.removed_from_location) — see the Move
	// Stock feature. Tracked separately from stockedHere so an item removed from warehouse A
	// but still actively stocked at this outlet's warehouse B isn't wrongly hidden.
	removedHere := make(map[uuid.UUID]struct{})
	stockedElsewhere := make(map[uuid.UUID]struct{})
	for _, b := range bals {
		if _, ok := warehouseIDs[b.WarehouseID]; ok {
			if b.RemovedFromLocation {
				removedHere[b.ItemID] = struct{}{}
			} else {
				stockedHere[b.ItemID] = struct{}{}
			}
		} else {
			stockedElsewhere[b.ItemID] = struct{}{}
		}
	}
	var candidates []uuid.UUID
	// An outlet that stocks NOTHING itself (fresh outlet, kiosk served from a central store) is
	// not location-separated — it sells the tenant catalog and receives stock later. Only
	// outlets with real operational history hide other outlets' goods.
	//
	// Live-reported regression: an outlet that ONCE had its own active balance(s) but had every
	// last one explicitly moved away since (Move Stock removing the final unit — sets
	// removed_from_location, so it lands in removedHere, not stockedHere) has real history
	// (hasOperationalHistory/anyBalanceHere is true) yet `len(stockedHere) > 0` is false — this
	// used to skip the stockedElsewhere exclusion entirely, so a genuinely-cleared-but-not-fresh
	// outlet (e.g. Junior Wholesalers, Eldoret Enterprises after their remaining stock was moved
	// out) fell back to the "fresh outlet" leniency and showed every item stocked anywhere else
	// in the tenant. Gate on hasOperationalHistory (stockedHere OR removedHere), not stockedHere
	// alone, so a cleared-but-experienced outlet is still treated as location-separated.
	if hasOperationalHistory {
		candidates = make([]uuid.UUID, 0, len(stockedElsewhere))
		for id := range stockedElsewhere {
			if _, ok := stockedHere[id]; !ok {
				candidates = append(candidates, id)
			}
		}
	}
	// Explicitly removed from this outlet's own warehouse(s) — hide unconditionally (even for an
	// otherwise "fresh" outlet), unless an active balance exists at another of this outlet's own
	// warehouses.
	for id := range removedHere {
		if _, ok := stockedHere[id]; !ok {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) > 0 {
		// Only stock-tracked, billable types are location-bound; recipes/services/vouchers and
		// free accompaniments are menu entries, not stock, and must never be hidden.
		excludeIDs, err = s.client.Item.Query().
			Where(
				item.IDIn(candidates...),
				item.TypeIn(item.TypeGOODS, item.TypeINGREDIENT),
				item.NonBillable(false),
			).IDs(ctx)
		if err != nil {
			return nil, hasOperationalHistory, warehouseIDs, err
		}
	}
	return excludeIDs, hasOperationalHistory, warehouseIDs, nil
}

// ListItems returns a paginated list of items for a tenant with DB-level filtering.
// statusFilter: "" or "active" = active only (default), "inactive" = inactive only, "all" = both.
// outletID: when set, restricts items to those with a balance in the outlet's warehouses or shared warehouses (outlet_id IS NULL).
// warehouseID: when set, narrows the returned Available/OnHand to ONLY that single warehouse
// (overriding the outlet-wide sum below) — added for Stock Transfer's "From Warehouse" picker,
// which was reading the outlet-wide aggregate across every warehouse in the outlet instead of the
// one warehouse actually being transferred FROM, silently overstating what was really shippable
// (confirmed live: boi-enterprises' MAIN outlet spans multiple warehouses). Independent of the
// outlet-visibility exclusion logic above, which still applies unchanged.
func (s *Service) ListItems(ctx context.Context, tenantID uuid.UUID, typeFilter, statusFilter string, limit, offset int, categoryID *uuid.UUID, unitID *uuid.UUID, search string, outletID *uuid.UUID, warehouseID *uuid.UUID, useCase string, tagsFilter ...string) ([]ItemDTO, int, error) {
	// Pre-compute outlet-scoped EXCLUSIONS when outlet context is active.
	//
	// Rule: an outlet hides only STOCK-TRACKED items (GOODS/INGREDIENT) that are stocked
	// EXCLUSIVELY in some OTHER outlet's warehouses — that stock belongs to a different
	// location — OR that were explicitly moved/removed away from every one of THIS outlet's own
	// warehouses (InventoryBalance.removed_from_location, set when a transfer ships the last
	// unit out — see Move Stock). Everything else always passes the outlet scope:
	//   - items with an active (non-removed) balance in this outlet's (or a shared,
	//     outlet-less) warehouse;
	//   - items with NO balance row anywhere (a new GOODS item not yet received — its stock
	//     simply surfaces as 0) — UNLESS this outlet has operational history (see below);
	//   - made-to-order types (RECIPE/SERVICE/VOUCHER) and non-billable accompaniments,
	//     which never carry own stock (a recipe's BOM ingredients do).
	// The previous rule ("keep only items with a balance in this outlet's warehouses")
	// collapsed whole catalogs the moment an outlet had ANY stock: urban-loft's POS lost 135
	// of its 269 sellable items (94 recipes + 41 unreceived goods), and the demo QSR outlet
	// shrank to 2 items once its prep stock was mirrored in.
	//
	// Live-reported follow-up bug: two outlets deliberately cleared of all stock (every balance
	// zeroed/removed) showed almost the tenant's ENTIRE catalog — ~1,666 of 3,714 items in the
	// reporting tenant have NEVER been received at ANY warehouse (a large bulk-import backlog
	// with no InitialStock row yet), and the "no balance row anywhere, show it anyway" rule
	// above meant literally all of them surfaced at every near-empty outlet, drowning the 1-2
	// items the outlet actually has real history with. Fix: that "never received anywhere"
	// leniency is only for a genuinely FRESH/untouched outlet (zero balance rows of ANY kind,
	// ANY status — the exact urban-loft scenario, still fully preserved below). An outlet with
	// real operational history (has interacted with the tenant's stock system before, even if
	// every balance is now zero or removed) is a real, distinct location, not a blank slate —
	// it only shows items it has actually touched (positive, zero-but-not-removed for
	// reordering, or explicitly present), never the tenant's unreceived backlog.
	// outletWarehouseIDs is the set of warehouses this outlet may sell from: its OWN warehouse(s)
	// plus tenant-wide shared warehouses (outlet_id nil, e.g. a central store). Hoisted to function
	// scope (not just this exclusion block) because buildDTOs' on-hand aggregation below MUST also
	// scope to it — summing balances across every warehouse in the tenant let a Malaba-HQ terminal
	// see (and oversell) stock that was physically sitting at Eldoret/Busia/Home/Nelly/Guest.
	outletExcludeIDs, hasOperationalHistory, outletWarehouseIDs, _ := s.OutletScope(ctx, tenantID, outletID)
	// balanceScopeWarehouseIDs is what the Available/OnHand summation below actually filters
	// against. A specific warehouseID (Stock Transfer's "From Warehouse") always narrows to just
	// that one warehouse, regardless of how many warehouses the outlet itself spans — the visibility
	// exclusion logic above is unaffected (an item still shows/hides per the outlet's normal rules;
	// only the reported quantity narrows).
	balanceScopeWarehouseIDs := outletWarehouseIDs
	if warehouseID != nil {
		balanceScopeWarehouseIDs = map[uuid.UUID]struct{}{*warehouseID: {}}
	}

	buildQuery := func() *ent.ItemQuery {
		// The heavy, potentially-many-row catalog fetch this function exists for — routed to a
		// read replica when configured (see rc()), EXCEPT the single-item detail fetch below:
		// that one stays on the primary so a just-created/edited item's detail page never shows
		// "not found" or stale data because of replication lag. OutletScope above and every
		// OTHER query in this file deliberately stay on s.client (primary) too, unchanged.
		listClient := s.client
		if itemIDFilter(ctx) == nil {
			listClient = s.rc()
		}
		q := listClient.Item.Query().Where(item.TenantID(tenantID))
		// Single-item detail fetch (?id=<uuid>): scope to exactly this item and let the rest
		// of the filters/enrichment run unchanged so the detail page gets the same shape as a
		// list row.
		if id := itemIDFilter(ctx); id != nil {
			q = q.Where(item.ID(*id))
		}
		switch statusFilter {
		case "eol":
			// Dedicated End-of-Life listing: only items marked EOL (awaiting restore or purge).
			q = q.Where(item.EndOfLifeAtNotNil())
		case "inactive":
			// Plain inactive items only — EOL items have their own tab (status=eol) and must
			// not bleed into the regular inactive listing.
			q = q.Where(item.IsActive(false), item.EndOfLifeAtIsNil())
		case "all":
			// no is_active filter (includes EOL items)
		default:
			// Active listing (also the POS/ordering live-catalog source) — EOL items are
			// is_active=false so they are already excluded here.
			q = q.Where(item.IsActive(true))
		}
		if recipeInputScope(ctx) {
			// Recipe-ingredient picker scope: raw stock (GOODS/INGREDIENT) plus RECIPE
			// items explicitly flagged as reusable menu components. Replaces the plain
			// type filter — the picker's notion of "ingredient" spans types.
			q = q.Where(item.Or(
				item.TypeIn(item.TypeGOODS, item.TypeINGREDIENT),
				item.And(item.TypeEQ(item.TypeRECIPE), item.UsableInRecipes(true)),
			))
		} else if typeFilter != "" {
			types := strings.Split(typeFilter, ",")
			typeVals := make([]item.Type, 0, len(types))
			for _, t := range types {
				typeVals = append(typeVals, item.Type(strings.TrimSpace(t)))
			}
			typePred := item.TypeIn(typeVals...)
			if includeNonBillable(ctx) {
				// Non-billable items (free accompaniments, supplies) surface regardless of
				// their type so the POS terminal can ring them up at KES 0.
				q = q.Where(item.Or(typePred, item.NonBillable(true)))
			} else {
				q = q.Where(typePred)
			}
		}
		// Not-for-sale scope: "exclude" is the sales-surface guarantee (POS/ordering
		// fetches never receive flagged items); "only" backs the back-office
		// "Not for selling" filter checkbox. Unset lists everything.
		switch notForSaleFilter(ctx) {
		case "only":
			q = q.Where(item.NotForSale(true))
		case "exclude":
			q = q.Where(item.NotForSale(false))
		}
		if useCase != "" {
			q = q.Where(item.UseCaseEQ(item.UseCase(useCase)))
		}
		if categoryID != nil {
			q = q.Where(item.CategoryID(*categoryID))
		}
		if bid := brandFilter(ctx); bid != nil {
			q = q.Where(item.BrandID(*bid))
		}
		if m := modelFilter(ctx); m != "" {
			q = q.Where(item.ModelEqualFold(m))
		}
		if unitID != nil {
			q = q.Where(item.UnitID(*unitID))
		}
		if search != "" {
			// Match every identifier a scanner or cashier might enter so a barcode/GTIN scan
			// resolves the item — an EAN/UPC/GTIN rarely equals the SKU. Also reaches a matching
			// VARIANT barcode (e.g. a per-size/per-colour SKU has its own scannable code).
			q = q.Where(item.Or(
				item.NameContainsFold(search),
				item.SkuContainsFold(search),
				item.BarcodeContainsFold(search),
				item.GtinContainsFold(search),
				item.HasVariantsWith(itemvariant.BarcodeContainsFold(search)),
			))
		}
		// Tag filtering via JSONB containment — each tag is ANDed at DB level.
		for _, tag := range tagsFilter {
			tagVal := tag
			q = q.Where(predicate.Item(func(s *entdialect.Selector) {
				s.Where(sqljson.ValueContains(item.FieldTags, tagVal))
			}))
		}
		// Outlet scope: hide only items stocked exclusively in OTHER outlets' warehouses.
		// A direct single-item fetch (?id=) is an explicit lookup, not a browse, so it must
		// never be hidden by outlet scoping — otherwise the detail page 404s for an item
		// stocked in another outlet.
		if len(outletExcludeIDs) > 0 && itemIDFilter(ctx) == nil {
			q = q.Where(item.IDNotIn(outletExcludeIDs...))
		}
		// An outlet with real operational history must not show the tenant's entire "never
		// received anywhere" backlog (see the long comment above `hasOperationalHistory`) —
		// only stockable+billable items with NO balance row anywhere are affected; recipes/
		// services/vouchers and non-billable accompaniments are never touched by this.
		if hasOperationalHistory && itemIDFilter(ctx) == nil {
			q = q.Where(item.Or(
				item.Not(item.TypeIn(item.TypeGOODS, item.TypeINGREDIENT)),
				item.NonBillable(true),
				item.HasBalances(),
			))
		}
		return q
	}

	buildDTOs := func(innerCtx context.Context, itms []*ent.Item) ([]ItemDTO, error) {
		catIDs := make([]uuid.UUID, 0, len(itms))
		brandIDs := make([]uuid.UUID, 0, len(itms))
		unitIDs := make([]uuid.UUID, 0, len(itms))
		itemIDs := make([]uuid.UUID, 0, len(itms))
		for _, it := range itms {
			if it.CategoryID != nil {
				catIDs = append(catIDs, *it.CategoryID)
			}
			if it.BrandID != nil {
				brandIDs = append(brandIDs, *it.BrandID)
			}
			if it.UnitID != nil {
				unitIDs = append(unitIDs, *it.UnitID)
			}
			itemIDs = append(itemIDs, it.ID)
		}
		catNames := make(map[uuid.UUID]string)
		if len(catIDs) > 0 {
			cats, _ := s.client.ItemCategory.Query().Where(itemcategory.IDIn(catIDs...)).All(innerCtx)
			for _, c := range cats {
				catNames[c.ID] = c.Name
			}
		}
		type brandMeta struct{ name, code string }
		brandInfo := make(map[uuid.UUID]brandMeta)
		if len(brandIDs) > 0 {
			brands, _ := s.client.ItemBrand.Query().Where(itembrand.IDIn(brandIDs...)).All(innerCtx)
			for _, b := range brands {
				brandInfo[b.ID] = brandMeta{name: b.Name, code: b.Code}
			}
		}
		type unitMeta struct{ abbr, name string }
		unitInfo := make(map[uuid.UUID]unitMeta)
		if len(unitIDs) > 0 {
			us, _ := s.client.Unit.Query().Where(entunit.IDIn(unitIDs...)).All(innerCtx)
			for _, u := range us {
				unitInfo[u.ID] = unitMeta{abbr: u.Abbreviation, name: u.Name}
			}
		}
		// Load balances per item to surface reorder_level, reorder_quantity, and total available/on_hand.
		type balSummary struct {
			reorderLevel    int
			reorderQuantity int
			available       float64
			onHand          float64
		}
		balMap := make(map[uuid.UUID]balSummary, len(itemIDs))
		if len(itemIDs) > 0 {
			bals, _ := s.client.InventoryBalance.Query().
				Where(inventorybalance.TenantIDEQ(tenantID), inventorybalance.ItemIDIn(itemIDs...)).
				All(innerCtx)
			for _, b := range bals {
				// Outlet scope: an active outlet only sees/sells its OWN warehouse(s) + shared
				// (outlet-less) stock — never another outlet's balance summed in. A multi-outlet
				// tenant (e.g. 7 warehouses) must never let a Malaba-HQ terminal display or oversell
				// stock that physically lives at a different outlet. No outlet context (HQ/all-
				// outlets view) keeps the tenant-wide total, matching the existing all-outlets UX.
				if balanceScopeWarehouseIDs != nil {
					if _, ok := balanceScopeWarehouseIDs[b.WarehouseID]; !ok {
						continue
					}
				}
				prev := balMap[b.ItemID]
				if prev.reorderLevel == 0 {
					prev.reorderLevel = b.ReorderLevel
					prev.reorderQuantity = b.ReorderQuantity
				}
				prev.available += b.Available
				prev.onHand += b.OnHand
				balMap[b.ItemID] = prev
			}
		}
		// Load tenant config once for suggested price computation.
		cfg, _ := s.client.TenantInventoryConfig.Query().
			Where(tenantinventoryconfig.TenantID(tenantID)).
			Only(innerCtx)
		dtos := make([]ItemDTO, len(itms))
		for i, it := range itms {
			dto := s.mapToDTO(it)
			if it.CategoryID != nil {
				dto.CategoryName = catNames[*it.CategoryID]
			}
			if it.BrandID != nil {
				if bm, ok := brandInfo[*it.BrandID]; ok {
					dto.BrandName = bm.name
					dto.BrandCode = bm.code
				}
			}
			if it.UnitID != nil {
				if um, ok := unitInfo[*it.UnitID]; ok {
					dto.UnitAbbreviation = um.abbr
					dto.UnitName = um.name
				}
			}
			if bs, ok := balMap[it.ID]; ok {
				dto.ReorderLevel = bs.reorderLevel
				dto.ReorderQuantity = bs.reorderQuantity
				dto.Available = &bs.available
				dto.OnHand = &bs.onHand
				if it.UnitContentQty != nil && *it.UnitContentQty > 0 {
					availContent := math.Round(bs.available**it.UnitContentQty*10000) / 10000
					onHandContent := math.Round(bs.onHand**it.UnitContentQty*10000) / 10000
					dto.AvailableContentQty = &availContent
					dto.OnHandContentQty = &onHandContent
				}
			}
			// Cost-plus-margin suggested price for GOODS ONLY: prefer the item's own
			// target_margin_percent, falling back to the tenant default. price = cost/(1-m).
			// INGREDIENT/EQUIPMENT items are consumed to make recipe items, never sold —
			// deriving a retail figure for them pollutes lists with meaningless prices;
			// their only meaningful money field is cost_price per BASE unit.
			if it.Type == item.TypeGOODS && it.CostPrice != nil && *it.CostPrice > 0 {
				var m float64
				if it.TargetMarginPercent != nil && *it.TargetMarginPercent > 0 && *it.TargetMarginPercent < 100 {
					m = *it.TargetMarginPercent
				} else if cfg != nil && cfg.DefaultTargetMarginPercent != nil {
					m = *cfg.DefaultTargetMarginPercent
				}
				if m > 0 && m < 100 {
					sp := *it.CostPrice / (1 - m/100)
					dto.SuggestedPrice = &sp
				}
			}
			dtos[i] = *dto
		}
		// Enrich with effective selling price + tax split for POS/ordering proxies.
		s.enrichPrices(innerCtx, tenantID, cfg, dtos)
		s.enrichModifierGroups(innerCtx, dtos)
		return dtos, nil
	}

	// DB-level pagination for all queries (including tag-filtered). Count() re-runs the exact
	// same filtered query on every page of pos-api's paginated catalog sweep even though the
	// filters — hence the result — never change within one sweep (same redundant-recompute class
	// as OutletScope above, see outletScopeCacheTTL's comment for the live BOI incident this is
	// closing). Cache it the same way, keyed by every dimension that can change the predicate.
	// Skipped for a single-item lookup (?id=) — rare, cheap, and correctness there (never showing
	// a false "not found") matters more than saving one Count() call.
	var total int
	var err error
	if id := itemIDFilter(ctx); id != nil {
		total, err = buildQuery().Count(ctx)
	} else {
		categoryKey, unitKey, outletKey, brandKey := "-", "-", "-", "-"
		if categoryID != nil {
			categoryKey = categoryID.String()
		}
		if unitID != nil {
			unitKey = unitID.String()
		}
		if outletID != nil {
			outletKey = outletID.String()
		}
		if bid := brandFilter(ctx); bid != nil {
			brandKey = bid.String()
		}
		countKey := sharedcache.Key("inv", "items-count", tenantID.String(),
			typeFilter, statusFilter, useCase, notForSaleFilter(ctx),
			categoryKey, unitKey, outletKey, brandKey, modelFilter(ctx), search, strings.Join(tagsFilter, ","),
			fmt.Sprintf("recipe=%v,nonbill=%v", recipeInputScope(ctx), includeNonBillable(ctx)),
		)
		total, err = sharedcache.GetOrSet(ctx, s.cache, countKey, outletScopeCacheTTL, func(ctx context.Context) (int, error) {
			return buildQuery().Count(ctx)
		})
	}
	if err != nil {
		return nil, 0, fmt.Errorf("items: count: %w", err)
	}
	if total == 0 {
		return []ItemDTO{}, 0, nil
	}

	applyEagerLoads := func(q *ent.ItemQuery) *ent.ItemQuery {
		if !leanFetch(ctx) {
			// Eager-load IMAGE assets so each list row carries its multi-image gallery (primary first).
			q = q.WithAssets(func(aq *ent.ItemAssetQuery) {
				aq.Where(itemasset.AssetType(AssetTypeImage)).
					Order(ent.Desc(itemasset.FieldIsPrimary), ent.Asc(itemasset.FieldDisplayOrder), ent.Asc(itemasset.FieldCreatedAt))
			})
			// Eager-load the preferred supplier so each row surfaces preferred_supplier_name (used by
			// the item edit form's preferred-supplier combobox to show the current selection).
			q = q.WithPreferredSupplier()
		}
		if includeVariants(ctx) {
			// Eager-load active variants so mapToDTO can surface them inline.
			q = q.WithVariants(func(vq *ent.ItemVariantQuery) {
				vq.Where(itemvariant.IsActive(true)).Order(ent.Asc(itemvariant.FieldName))
			})
		}
		return q
	}

	if field, dir, ok := balanceSortField(ctx); ok {
		// Computed-field sort (on_hand/available): these are aggregated per item from
		// InventoryBalance in buildDTOs, not a real Item column, so they can't use the SQL-level
		// ORDER BY + LIMIT/OFFSET path below. Build DTOs for every item matching the existing
		// WHERE filters (category/type/search/status/etc. still apply — only the final
		// pagination moves into Go), sort by the requested field, then slice the page. Fine for
		// the catalog sizes this list serves; a true DB-side aggregate ORDER BY would need a
		// materially larger rewrite of the outlet-scoping query above for marginal gain here.
		allItems, err := applyEagerLoads(buildQuery().Order(ent.Asc(item.FieldSku))).All(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("items: list (balance sort): %w", err)
		}
		allDTOs, err := buildDTOs(ctx, allItems)
		if err != nil {
			return nil, 0, err
		}
		sort.SliceStable(allDTOs, func(i, j int) bool {
			vi, vj := balanceSortValue(allDTOs[i], field), balanceSortValue(allDTOs[j], field)
			if dir == "desc" {
				return vi > vj
			}
			return vi < vj
		})
		start := offset
		if start > len(allDTOs) {
			start = len(allDTOs)
		}
		end := start + limit
		if limit <= 0 || end > len(allDTOs) {
			end = len(allDTOs)
		}
		return allDTOs[start:end], total, nil
	}

	listQuery := applyEagerLoads(buildQuery().Order(listOrder(ctx)).Limit(limit).Offset(offset))
	itms, err := listQuery.All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("items: list: %w", err)
	}
	dtos, err := buildDTOs(ctx, itms)
	if err != nil {
		return nil, 0, err
	}
	return dtos, total, nil
}

// ListItemVariants returns the active product variations for a single item.
// Surfaces the existing ItemVariant edge so retail/POS can sell variations.
func (s *Service) ListItemVariants(ctx context.Context, tenantID, itemID uuid.UUID) ([]VariantDTO, error) {
	// Ensure the item belongs to the tenant before exposing its variants.
	exists, err := s.client.Item.Query().
		Where(item.TenantID(tenantID), item.ID(itemID)).
		Exist(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: lookup item for variants: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("items: item not found")
	}
	variants, err := s.client.ItemVariant.Query().
		Where(itemvariant.ItemID(itemID), itemvariant.IsActive(true)).
		Order(ent.Asc(itemvariant.FieldName)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: list variants: %w", err)
	}
	dtos := make([]VariantDTO, 0, len(variants))
	for _, v := range variants {
		dtos = append(dtos, VariantDTO{
			ID:         v.ID,
			SKU:        v.Sku,
			Name:       v.Name,
			Price:      v.Price,
			Attributes: v.Attributes,
			Barcode:    v.Barcode,
			IsActive:   v.IsActive,
		})
	}
	return dtos, nil
}

// ListEventItems returns all active SERVICE-type items (one-time and recurring),
// ordered by event_start_at ascending; recurring events without a date appear last.
func (s *Service) ListEventItems(ctx context.Context, tenantID uuid.UUID, limit, offset int) ([]ItemDTO, int, error) {
	q := s.client.Item.Query().
		Where(
			item.TenantID(tenantID),
			item.IsActive(true),
			item.TypeEQ(item.TypeSERVICE),
		)

	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("items: count events: %w", err)
	}
	if total == 0 {
		return []ItemDTO{}, 0, nil
	}
	itms, err := q.
		Order(func(s *entdialect.Selector) {
			s.OrderExpr(entdialect.Expr(item.FieldEventStartAt + " ASC NULLS LAST"))
		}).
		Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("items: list events: %w", err)
	}
	dtos := make([]ItemDTO, len(itms))
	for i, it := range itms {
		dtos[i] = *s.mapToDTO(it)
	}
	cfg, _ := s.client.TenantInventoryConfig.Query().
		Where(tenantinventoryconfig.TenantID(tenantID)).
		Only(ctx)
	s.enrichPrices(ctx, tenantID, cfg, dtos)
	s.enrichModifierGroups(ctx, dtos)
	return dtos, total, nil
}

// DeactivateItemBySKU soft-deletes an item by setting is_active=false, resolving it directly
// by SKU. Unlike UpdateItem it touches ONLY the is_active flag (a full-DTO update with an empty
// name would fail the name validator), and unlike resolving via stock-availability it works for
// items that have no balance row yet. Returns a not-found error when the SKU doesn't exist.
func (s *Service) DeactivateItemBySKU(ctx context.Context, tenantID uuid.UUID, sku string) error {
	n, err := s.client.Item.Update().
		Where(item.TenantID(tenantID), item.Sku(sku)).
		SetIsActive(false).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("items: deactivate item: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("items: item not found")
	}
	return nil
}

// DeleteCategory removes a category. A category with NO items linked to it (in any
// status — active, inactive or EOL) is hard-deleted outright: an empty category is
// pure clutter (it can never legitimately appear on a sales surface, since every
// sellable-category listing already requires has_items=true) and keeping a dead row
// around only invites confusion (e.g. a stray "Installation & Setup" or "Consumables"
// category some staff member created and never used). A category that still has at
// least one item linked keeps the original reversible soft-delete (is_active=false) —
// deleting it outright would orphan those items' category_id.
func (s *Service) DeleteCategory(ctx context.Context, tenantID, id uuid.UUID) error {
	existing, err := s.client.ItemCategory.Query().
		Where(itemcategory.TenantID(tenantID), itemcategory.ID(id)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return fmt.Errorf("items: category not found")
		}
		return fmt.Errorf("items: query category: %w", err)
	}
	hasItems, err := s.client.Item.Query().
		Where(item.TenantID(tenantID), item.CategoryID(existing.ID)).
		Exist(ctx)
	if err != nil {
		return fmt.Errorf("items: check category items: %w", err)
	}
	if hasItems {
		if _, err := s.client.ItemCategory.UpdateOneID(existing.ID).SetIsActive(false).Save(ctx); err != nil {
			return fmt.Errorf("items: delete category: %w", err)
		}
	} else {
		// Hard delete: also detach any children pointing at this category as their
		// parent so a purge never leaves a dangling parent_id.
		if _, err := s.client.ItemCategory.Update().
			Where(itemcategory.TenantID(tenantID), itemcategory.ParentID(existing.ID)).
			ClearParentID().
			Save(ctx); err != nil {
			return fmt.Errorf("items: detach child categories: %w", err)
		}
		if err := s.client.ItemCategory.DeleteOneID(existing.ID).Exec(ctx); err != nil {
			return fmt.Errorf("items: hard delete category: %w", err)
		}
	}
	// Invalidate categories cache
	if s.cache != nil {
		s.cache.Invalidate(ctx, sharedcache.Key("inv", "categories", tenantID.String()))
	}
	return nil
}

// DuplicateCategoryError is returned by CreateCategory/UpdateCategory when a
// case-insensitive name collision is found within the tenant's visible category
// set (its own categories + global ones). Category names are unique per tenant
// regardless of parent — see the schema comment on ItemCategory.Indexes for why.
// Handlers use errors.As to map this to a 409 with an actionable message instead
// of a raw 500 / DB constraint error.
type DuplicateCategoryError struct {
	Name string
}

func (e *DuplicateCategoryError) Error() string {
	return fmt.Sprintf("a category named %q already exists", e.Name)
}

// checkDuplicateCategory looks for an existing ACTIVE category (tenant-owned or
// global — the same set ListCategories shows the tenant) whose name
// case-insensitively matches, excluding excludeID (used by updates so a category
// doesn't collide with itself). Soft-deleted categories never block reuse of
// their name.
func (s *Service) checkDuplicateCategory(ctx context.Context, tenantID uuid.UUID, name string, excludeID *uuid.UUID) error {
	q := s.client.ItemCategory.Query().Where(
		itemcategory.IsActive(true),
		itemcategory.Or(
			itemcategory.TenantID(tenantID),
			itemcategory.And(itemcategory.IsGlobal(true), itemcategory.TenantID(uuid.Nil)),
		),
		itemcategory.NameEqualFold(name),
	)
	if excludeID != nil {
		q = q.Where(itemcategory.IDNEQ(*excludeID))
	}
	existing, err := q.Only(ctx)
	if err == nil {
		return &DuplicateCategoryError{Name: existing.Name}
	}
	if !ent.IsNotFound(err) {
		return fmt.Errorf("items: check duplicate category: %w", err)
	}
	return nil
}

// resolveCategoryParent fetches the parent category for a proposed parentID,
// scoped the same way ListCategories resolves visibility (the tenant's own
// rows plus platform-global rows) so a tenant can nest under a global parent.
// Returns nil, nil when parentID is nil (root category).
func (s *Service) resolveCategoryParent(ctx context.Context, tenantID uuid.UUID, parentID *uuid.UUID) (*ent.ItemCategory, error) {
	if parentID == nil {
		return nil, nil
	}
	parent, err := s.client.ItemCategory.Query().
		Where(
			itemcategory.ID(*parentID),
			itemcategory.Or(
				itemcategory.TenantID(tenantID),
				itemcategory.And(itemcategory.IsGlobal(true), itemcategory.TenantID(uuid.Nil)),
			),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("items: parent category not found")
		}
		return nil, fmt.Errorf("items: query parent category: %w", err)
	}
	return parent, nil
}

// resolveCategoryPath computes the depth/materialized-path for selfID given
// its (already-resolved) parent, per the ent schema's "root-id/parent-id/self-id"
// convention (schema/itemcategory.go). A nil parent means selfID is a root:
// depth 0, path = selfID alone. Falls back to parent.ID when an existing
// parent predates path being populated (parent.Path == "").
func resolveCategoryPath(parent *ent.ItemCategory, selfID uuid.UUID) (depth int, path string) {
	if parent == nil {
		return 0, selfID.String()
	}
	parentPath := parent.Path
	if parentPath == "" {
		parentPath = parent.ID.String()
	}
	return parent.Depth + 1, parentPath + "/" + selfID.String()
}

// cascadeCategoryDescendants re-materializes depth/path for every existing
// descendant of a category whose own path just changed (e.g. it was
// re-parented, or its ancestor was). Without this, moving a parent would
// leave every child's path pointing at a now-stale prefix — the gap flagged
// in the category-hierarchy data-fix task. oldPath/newPath are the node's
// path before/after the update that triggered this cascade.
func (s *Service) cascadeCategoryDescendants(ctx context.Context, tenantID uuid.UUID, oldPath, newPath string) error {
	if oldPath == "" || oldPath == newPath {
		return nil
	}
	descendants, err := s.client.ItemCategory.Query().
		Where(itemcategory.TenantID(tenantID), itemcategory.PathHasPrefix(oldPath+"/")).
		All(ctx)
	if err != nil {
		return fmt.Errorf("items: query category descendants for cascade: %w", err)
	}
	for _, d := range descendants {
		suffix := strings.TrimPrefix(d.Path, oldPath)
		newDescPath := newPath + suffix
		newDescDepth := strings.Count(newDescPath, "/")
		if _, err := s.client.ItemCategory.UpdateOneID(d.ID).
			SetDepth(newDescDepth).
			SetPath(newDescPath).
			Save(ctx); err != nil {
			return fmt.Errorf("items: cascade path to descendant %s: %w", d.ID, err)
		}
	}
	return nil
}

// CreateCategory creates a new item category for the tenant.
// When dto.IsGlobal is true, the category is visible to all tenants.
func (s *Service) CreateCategory(ctx context.Context, tenantID uuid.UUID, dto CategoryDTO) (*CategoryDTO, error) {
	if err := s.checkDuplicateCategory(ctx, tenantID, dto.Name, nil); err != nil {
		return nil, err
	}
	parent, err := s.resolveCategoryParent(ctx, tenantID, dto.ParentID)
	if err != nil {
		return nil, err
	}
	// Pre-generate the ID (rather than letting Save() apply the schema's
	// client-side default) so depth/path can be materialized in the same
	// insert instead of a follow-up update.
	newID := uuid.New()
	depth, path := resolveCategoryPath(parent, newID)
	q := s.client.ItemCategory.Create().
		SetID(newID).
		SetTenantID(tenantID).
		SetName(dto.Name).
		SetIsActive(true).
		SetIsGlobal(dto.IsGlobal).
		SetDepth(depth).
		SetPath(path)
	if dto.Code != "" {
		q = q.SetCode(dto.Code)
	}
	if dto.Description != "" {
		q = q.SetDescription(dto.Description)
	}
	if dto.Icon != "" {
		q = q.SetIcon(dto.Icon)
	} else {
		// No icon supplied: infer a sensible default from the name (falling back to
		// the first use_case tag) rather than leaving it blank — see
		// category_icon_defaults.go. Never overwrites a caller-supplied icon.
		defaultUseCase := ""
		if len(dto.UseCases) > 0 {
			defaultUseCase = dto.UseCases[0]
		}
		q = q.SetIcon(InferDefaultCategoryIcon(dto.Name, defaultUseCase))
	}
	if parent != nil {
		q = q.SetParentID(parent.ID)
	}
	// Use-case tags: a category created from a hospitality outlet is stamped
	// hospitality so it never pollutes pharmacy/retail pickers. Untagged = universal.
	if len(dto.UseCases) > 0 {
		q = q.SetUseCases(dto.UseCases)
	}
	c, err := q.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, &DuplicateCategoryError{Name: dto.Name}
		}
		return nil, fmt.Errorf("items: create category: %w", err)
	}
	if s.cache != nil {
		// Invalidate both the tenant-specific key and force a global refresh.
		s.cache.Invalidate(ctx, sharedcache.Key("inv", "categories", tenantID.String()))
	}
	return &CategoryDTO{
		ID:          c.ID,
		Name:        c.Name,
		Code:        c.Code,
		Description: c.Description,
		Icon:        c.Icon,
		ParentID:    c.ParentID,
		IsActive:    c.IsActive,
		IsGlobal:    c.IsGlobal,
		Depth:       c.Depth,
		Path:        c.Path,
		SortOrder:   c.SortOrder,
		CreatedAt:   c.CreatedAt,
	}, nil
}

// UpdateCategory updates an existing item category.
func (s *Service) UpdateCategory(ctx context.Context, tenantID, id uuid.UUID, dto CategoryDTO) (*CategoryDTO, error) {
	existing, err := s.client.ItemCategory.Query().
		Where(itemcategory.TenantID(tenantID), itemcategory.ID(id)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("items: category not found")
		}
		return nil, fmt.Errorf("items: query category for update: %w", err)
	}
	// Only re-check for a name collision when the name is actually changing.
	// Without this guard, a category that happens to share its name with a
	// platform-global category (e.g. a tenant's own "Electronics" alongside
	// the global "Electronics" row) could never be updated at all — even a
	// no-op resend of its current name, or an unrelated field change like
	// re-parenting — because checkDuplicateCategory would find the other
	// tenant's/global row and report a false collision every time.
	if !strings.EqualFold(existing.Name, dto.Name) {
		if err := s.checkDuplicateCategory(ctx, tenantID, dto.Name, &id); err != nil {
			return nil, err
		}
	}
	// Resolve the (possibly new) parent and re-materialize this category's own
	// depth/path from it — CreateCategory/UpdateCategory previously left these
	// at their zero values entirely, so any re-parenting silently produced
	// dangling/incorrect path data. See resolveCategoryPath.
	parent, err := s.resolveCategoryParent(ctx, tenantID, dto.ParentID)
	if err != nil {
		return nil, err
	}
	newDepth, newPath := resolveCategoryPath(parent, id)
	q := s.client.ItemCategory.UpdateOneID(id).
		SetName(dto.Name).
		SetIsActive(dto.IsActive).
		SetDepth(newDepth).
		SetPath(newPath)
	if dto.Code != "" {
		q = q.SetCode(dto.Code)
	}
	if dto.Description != "" {
		q = q.SetDescription(dto.Description)
	}
	if dto.Icon != "" {
		q = q.SetIcon(dto.Icon)
	} else if existing.Icon == "" {
		// Icon left empty on both the request and the existing row: auto-assign a
		// default rather than leaving it blank. Never overwrites a non-empty existing
		// icon just because the caller didn't resend it.
		defaultUseCase := ""
		if len(dto.UseCases) > 0 {
			defaultUseCase = dto.UseCases[0]
		} else if len(existing.UseCases) > 0 {
			defaultUseCase = existing.UseCases[0]
		}
		q = q.SetIcon(InferDefaultCategoryIcon(dto.Name, defaultUseCase))
	}
	if parent != nil {
		q = q.SetParentID(parent.ID)
	} else {
		q = q.ClearParentID()
	}
	c, err := q.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, &DuplicateCategoryError{Name: dto.Name}
		}
		return nil, fmt.Errorf("items: update category: %w", err)
	}
	// This category's own path just moved (re-parented, or first materialized
	// from a legacy empty path) — ripple the new prefix onto every existing
	// descendant so their depth/path don't dangle pointing at the stale path.
	if err := s.cascadeCategoryDescendants(ctx, tenantID, existing.Path, newPath); err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.Invalidate(ctx, sharedcache.Key("inv", "categories", tenantID.String()))
	}
	return &CategoryDTO{
		ID:          c.ID,
		Name:        c.Name,
		Code:        c.Code,
		Description: c.Description,
		Icon:        c.Icon,
		ParentID:    c.ParentID,
		IsActive:    c.IsActive,
		Depth:       c.Depth,
		Path:        c.Path,
		SortOrder:   c.SortOrder,
		CreatedAt:   c.CreatedAt,
	}, nil
}

// ListCategories returns all item categories for a tenant (cached 5 min).
func (s *Service) ListCategories(ctx context.Context, tenantID uuid.UUID) ([]CategoryDTO, error) {
	return s.ListCategoriesFiltered(ctx, tenantID, false, false)
}

// ListCategoriesFiltered returns item categories for a tenant.
//   - hasItems=true: only categories with at least one active item linked to them are
//     returned — selection surfaces (e.g. label printing, which legitimately needs to
//     print barcode labels for not-for-sale internal stock too) use this so a chosen
//     category can never resolve to an empty selection.
//   - sellableOnly=true: the stricter mode POS/ordering use — a category only counts as
//     "has items" if at least one of them is also not_for_sale=false. A category whose
//     items are ALL raw ingredients / internal supplies (not_for_sale=true) has items in
//     the plain hasItems sense but is functionally an empty aisle on a sales surface.
func (s *Service) ListCategoriesFiltered(ctx context.Context, tenantID uuid.UUID, hasItems, sellableOnly bool) ([]CategoryDTO, error) {
	all, err := s.listCategoriesAll(ctx, tenantID)
	if err != nil || (!hasItems && !sellableOnly) {
		return all, err
	}
	withItems, err := s.categoryIDsWithItems(ctx, tenantID, sellableOnly)
	if err != nil {
		return nil, err
	}
	filtered := make([]CategoryDTO, 0, len(all))
	for _, c := range all {
		if _, ok := withItems[c.ID]; ok {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

// categoryIDsWithItems returns the set of category IDs that have at least one active
// item linked (and, when sellableOnly is set, at least one of those items is also
// not_for_sale=false). Cheap GROUP BY on the (tenant_id, category_id) index.
func (s *Service) categoryIDsWithItems(ctx context.Context, tenantID uuid.UUID, sellableOnly bool) (map[uuid.UUID]struct{}, error) {
	var rows []struct {
		CategoryID uuid.UUID `json:"category_id"`
	}
	preds := []predicate.Item{
		item.TenantID(tenantID),
		item.IsActive(true),
		item.CategoryIDNotNil(),
	}
	if sellableOnly {
		preds = append(preds, item.NotForSale(false))
	}
	err := s.client.Item.Query().
		Where(preds...).
		GroupBy(item.FieldCategoryID).
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("items: category-item linkage: %w", err)
	}
	set := make(map[uuid.UUID]struct{}, len(rows))
	for _, r := range rows {
		set[r.CategoryID] = struct{}{}
	}
	return set, nil
}

// listCategoriesAll returns every active category for a tenant (cached 5 min).
func (s *Service) listCategoriesAll(ctx context.Context, tenantID uuid.UUID) ([]CategoryDTO, error) {
	key := sharedcache.Key("inv", "categories", tenantID.String())
	fetch := func(ctx context.Context) ([]CategoryDTO, error) {
		// Tenant's own categories + PLATFORM globals only. The global leg is pinned to
		// the nil tenant (seed_global_categories.go rows): a tenant-owned row flagged
		// is_global must never leak into other tenants' pickers (bulk import used to
		// create exactly those — cross-tenant category pollution, fixed 2026-07-18).
		cats, err := s.client.ItemCategory.Query().
			Where(
				itemcategory.IsActive(true),
				itemcategory.Or(
					itemcategory.TenantID(tenantID),
					itemcategory.And(
						itemcategory.IsGlobal(true),
						itemcategory.TenantID(uuid.Nil),
					),
				),
			).
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("items: list categories: %w", err)
		}
		// Build a name lookup map for parent_name resolution
		nameMap := make(map[uuid.UUID]string, len(cats))
		for _, c := range cats {
			nameMap[c.ID] = c.Name
		}
		dtos := make([]CategoryDTO, 0, len(cats))
		// Case-insensitive de-duplication across the tenant+global union: a tenant that has
		// its OWN "Beverages" must not ALSO see the platform-global "Beverages" (the picker
		// would show the name twice, splitting items across two categories). The tenant-owned
		// row always wins over a same-named global; among same-scope collisions the first
		// active row wins (data is normally already unique per scope). Keyed on lower(trim(name)).
		seen := make(map[string]int, len(cats)) // normalized name → index in dtos
		for _, c := range cats {
			dto := CategoryDTO{
				ID:          c.ID,
				Name:        c.Name,
				Code:        c.Code,
				Description: c.Description,
				Icon:        s.resolveMediaURL(c.Icon),
				ParentID:    c.ParentID,
				IsActive:    c.IsActive,
				IsGlobal:    c.IsGlobal,
				Depth:       c.Depth,
				Path:        c.Path,
				SortOrder:   c.SortOrder,
				UseCases:    c.UseCases,
				CreatedAt:   c.CreatedAt,
			}
			if c.ParentID != nil {
				if pName, ok := nameMap[*c.ParentID]; ok {
					dto.ParentName = pName
				}
			}
			normName := strings.ToLower(strings.TrimSpace(c.Name))
			if idx, dup := seen[normName]; dup {
				// Keep the tenant-owned row over a global one; otherwise keep the existing pick.
				if dtos[idx].IsGlobal && !dto.IsGlobal {
					dtos[idx] = dto
				}
				continue
			}
			seen[normName] = len(dtos)
			dtos = append(dtos, dto)
		}
		return dtos, nil
	}
	return sharedcache.GetOrSet(ctx, s.cache, key, sharedcache.TTLReference, fetch)
}

// BrandDTO is a lightweight item-brand projection used by bulk import for
// name-based brand resolution / auto-create. Mirrors CategoryDTO.
type BrandDTO struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Code     string    `json:"code,omitempty"`
	IsActive bool      `json:"is_active"`
}

// ListBrands returns all active item brands for a tenant.
// Mirrors ListCategories so bulk import can resolve brands by name.
func (s *Service) ListBrands(ctx context.Context, tenantID uuid.UUID) ([]BrandDTO, error) {
	brands, err := s.client.ItemBrand.Query().
		Where(itembrand.TenantID(tenantID), itembrand.IsActive(true)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: list brands: %w", err)
	}
	dtos := make([]BrandDTO, len(brands))
	for i, b := range brands {
		dtos[i] = BrandDTO{
			ID:       b.ID,
			Name:     b.Name,
			Code:     b.Code,
			IsActive: b.IsActive,
		}
	}
	return dtos, nil
}

// CreateBrand creates a new tenant-scoped item brand.
// Mirrors CreateCategory; the code is required by the ItemBrand schema, so the
// caller slugifies the name when no explicit code is supplied.
func (s *Service) CreateBrand(ctx context.Context, tenantID uuid.UUID, dto BrandDTO) (*BrandDTO, error) {
	b, err := s.client.ItemBrand.Create().
		SetTenantID(tenantID).
		SetName(dto.Name).
		SetCode(dto.Code).
		SetIsActive(true).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: create brand: %w", err)
	}
	return &BrandDTO{
		ID:       b.ID,
		Name:     b.Name,
		Code:     b.Code,
		IsActive: b.IsActive,
	}, nil
}

// itemTypeCode maps item types to short codes for SKU generation.
var itemTypeCode = map[string]string{
	"GOODS":      "GDS",
	"SERVICE":    "SVC",
	"RECIPE":     "RCP",
	"INGREDIENT": "ING",
	"VOUCHER":    "VCH",
	"EQUIPMENT":  "EQP",
}

// GenerateSKU creates a unique auto-generated SKU for a tenant. Two modes, selected per tenant
// via the item_sku document-sequence config (Settings → Documents), exactly like every other
// inventory document number:
//
//   - Numeric (platform default, and boi-enterprises' convention — 98.5% of its live catalog
//     was already plain numeric before this was wired in): a plain zero-padded counter from
//     documents.SequenceService, e.g. "037124". No category/type logic at all.
//   - Prefix (opt-in; the convention every tenant that had ever auto-generated a SKU before this
//     was wired in was already using): the legacy dynamic {CAT_CODE}-{TYPE_CODE}-{SEQ:03d}
//     format below, unchanged. The item_sku sequence's configured Prefix string is a boolean
//     gate here ONLY — unlike PO/GRN/etc. it is never injected literally into the SKU, since the
//     legacy format's prefix varies per item (by category+type), not per tenant.
//
// s.seq == nil (unwired scripts/tests) always falls through to the legacy algorithm.
func (s *Service) GenerateSKU(ctx context.Context, tenantID uuid.UUID, categoryID *uuid.UUID, itemType string) (string, error) {
	if s.seq != nil {
		cfg, err := s.seq.GetConfig(ctx, tenantID, documents.DocTypeItemSKU)
		if err != nil {
			return "", fmt.Errorf("items: load item_sku sequence config: %w", err)
		}
		if cfg.Prefix == "" {
			return s.seq.GenerateNumber(ctx, tenantID, documents.DocTypeItemSKU)
		}
	}

	catCode := "GEN"
	if categoryID != nil {
		cat, err := s.client.ItemCategory.Get(ctx, *categoryID)
		if err == nil && cat.Code != "" {
			catCode = strings.ToUpper(cat.Code)
		} else if err == nil {
			// Derive code from first 3 chars of name
			name := strings.ToUpper(strings.ReplaceAll(cat.Name, " ", ""))
			if len(name) >= 3 {
				catCode = name[:3]
			} else {
				catCode = name
			}
		}
	}

	typeCode, ok := itemTypeCode[strings.ToUpper(itemType)]
	if !ok {
		typeCode = "GDS"
	}

	prefix := catCode + "-" + typeCode + "-"

	// Next sequence number is derived from the HIGHEST existing suffix under this prefix, not a
	// row count — a plain count silently collides once any item in the sequence has ever been
	// hard-deleted (count drops, but the max-numbered SKU is still in use), which is exactly how
	// boi-enterprises hit "duplicate key value violates unique constraint item_tenant_id_sku" on
	// a plain Add Product. CreateItem's caller additionally retries on a collision, covering the
	// remaining true-concurrency window (two requests generating the same next number at once).
	existingSKUs, err := s.client.Item.Query().
		Where(
			item.TenantID(tenantID),
			item.SkuHasPrefix(prefix),
		).
		Select(item.FieldSku).
		Strings(ctx)
	if err != nil {
		return "", fmt.Errorf("items: list items for SKU prefix %s: %w", prefix, err)
	}
	maxSeq := 0
	for _, sku := range existingSKUs {
		suffix := strings.TrimPrefix(sku, prefix)
		if n, convErr := strconv.Atoi(suffix); convErr == nil && n > maxSeq {
			maxSeq = n
		}
	}

	return fmt.Sprintf("%s%03d", prefix, maxSeq+1), nil
}

// resolveEPCost auto-computes cost_price (EP unit cost) from purchase fields when all three
// are present and cost_price was not explicitly provided.
// Formula: cost_price = purchase_price / purchase_pack_size / yield_pct
// validatePriceBand enforces a coherent selling-price guardrail: min must not exceed max.
// Returned errors are surfaced as 400s by the handlers.
func validatePriceBand(dto *ItemDTO) error {
	if dto.MinSellingPrice != nil && *dto.MinSellingPrice < 0 {
		return fmt.Errorf("min_selling_price cannot be negative")
	}
	if dto.MaxSellingPrice != nil && *dto.MaxSellingPrice < 0 {
		return fmt.Errorf("max_selling_price cannot be negative")
	}
	if dto.MinSellingPrice != nil && dto.MaxSellingPrice != nil && *dto.MinSellingPrice > *dto.MaxSellingPrice {
		return fmt.Errorf("min_selling_price (%.2f) cannot exceed max_selling_price (%.2f)", *dto.MinSellingPrice, *dto.MaxSellingPrice)
	}
	return nil
}

func resolveEPCost(dto *ItemDTO) {
	if dto.CostPrice != nil {
		return // caller provided cost_price explicitly — respect it
	}
	if dto.PurchasePrice == nil || dto.PurchasePackSize == nil {
		return
	}
	if *dto.PurchasePackSize <= 0 {
		return
	}
	yieldPct := 1.0
	if dto.YieldPct != nil && *dto.YieldPct > 0 && *dto.YieldPct <= 1 {
		yieldPct = *dto.YieldPct
	}
	ep := *dto.PurchasePrice / *dto.PurchasePackSize / yieldPct
	dto.CostPrice = &ep
}

// resolveReorderLevel returns the effective reorder_level for a new item.
// Resolution order: explicit DTO value → unit-based tenant default → global tenant default → hard fallback.
func resolveReorderLevel(ctx context.Context, client *ent.Client, tenantID uuid.UUID, unitAbbr string, dtoLevel int) int {
	if dtoLevel > 0 {
		return dtoLevel
	}
	cfg, err := client.TenantInventoryConfig.Query().
		Where(tenantinventoryconfig.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return 1 // hard fallback
	}
	if unitAbbr != "" && cfg.UnitReorderDefaults != nil {
		if v, ok := cfg.UnitReorderDefaults[unitAbbr]; ok && v > 0 {
			return v
		}
	}
	if cfg.DefaultReorderLevel > 0 {
		return cfg.DefaultReorderLevel
	}
	return 1
}

// IsStockTracked reports whether an item type holds physical stock that should
// appear in inventory balances. SERVICE and VOUCHER are non-stockable (e.g.
// "Conference charges"); RECIPE availability is derived from its BOM, so it is
// not tracked as a balance directly either. Only GOODS, INGREDIENT and EQUIPMENT
// carry real on-hand stock. Exported so every other balance-creating entry point
// (bulk import, manual adjustments, …) can share this single source of truth
// instead of re-deriving it — a bookable SERVICE item getting a real stock
// balance is a live-observed bug class (see urban-loft's Co-working-Space items).
func IsStockTracked(t item.Type) bool {
	switch t {
	case item.TypeGOODS, item.TypeINGREDIENT, item.TypeEQUIPMENT:
		return true
	default:
		return false
	}
}

// CreateItem creates a new item and records an outbox event within a transaction.
// DuplicateSKUError is returned by CreateItem when the (tenant_id, sku) unique constraint is
// violated — either an explicitly-provided SKU that already belongs to another item, or (after
// maxSKUGenerationAttempts retries) a persistently colliding auto-generated one. Handlers use
// errors.As to map this to a 409 with an actionable message instead of the raw ent/Postgres
// constraint error leaking straight to the UI (found live: boi-enterprises' Add Product surfaced
// "ent: constraint failed: ERROR: duplicate key value violates unique constraint
// \"item_tenant_id_sku\"" verbatim).
type DuplicateSKUError struct{ SKU string }

func (e *DuplicateSKUError) Error() string {
	return fmt.Sprintf("SKU %q is already in use for this tenant", e.SKU)
}

// maxSKUGenerationAttempts bounds CreateItem's auto-generated-SKU retry loop below.
const maxSKUGenerationAttempts = 5

// CreateItem creates a new inventory item, auto-generating a SKU when the caller didn't supply
// one. Item is the ONLY unique index on Item (tenant_id, sku) — see the schema — so any
// constraint violation from the create below is unambiguously a SKU collision.
//
// GenerateSKU's next-sequence-number itself is now gap-safe (derived from the max existing
// suffix, not a row count), but a concurrent create racing for the exact same next number is
// still possible — retrying with a freshly generated SKU closes that window instead of
// surfacing a raw duplicate-key error to the cashier mid-Add-Product.
func (s *Service) CreateItem(ctx context.Context, tenantID uuid.UUID, dto ItemDTO) (*ItemDTO, error) {
	autoSKU := dto.SKU == ""
	for attempt := 0; ; attempt++ {
		if dto.SKU == "" {
			sku, err := s.GenerateSKU(ctx, tenantID, dto.CategoryID, dto.Type)
			if err != nil {
				return nil, fmt.Errorf("items: auto-generate SKU: %w", err)
			}
			dto.SKU = sku
		}
		result, err := s.createItemOnce(ctx, tenantID, dto)
		if err == nil {
			return result, nil
		}
		if ent.IsConstraintError(err) {
			if !autoSKU {
				return nil, &DuplicateSKUError{SKU: dto.SKU}
			}
			if attempt+1 < maxSKUGenerationAttempts {
				dto.SKU = "" // force GenerateSKU to pick a fresh one next attempt
				continue
			}
			return nil, &DuplicateSKUError{SKU: dto.SKU}
		}
		return nil, err
	}
}

// createItemOnce does the actual create attempt for a single, already-resolved SKU — split out
// of CreateItem so the SKU-collision retry loop above can call it repeatedly without duplicating
// the rest of item creation (opening balance, events, ...).
func (s *Service) createItemOnce(ctx context.Context, tenantID uuid.UUID, dto ItemDTO) (*ItemDTO, error) {
	if err := validatePriceBand(&dto); err != nil {
		return nil, fmt.Errorf("items: %w", err)
	}
	// Auto-compute EP cost from purchase fields when not explicitly set.
	resolveEPCost(&dto)

	// Reject a fractional opening quantity for a discrete/count-based item (e.g. a phone stocked
	// in PIECE) before creating anything — covers manual item creation AND the bulk-import Items
	// sheet's initial_quantity column, both of which flow through here.
	if dto.UnitID != nil && dto.InitialQuantity != 0 {
		if u, uErr := s.client.Unit.Get(ctx, *dto.UnitID); uErr == nil {
			if vErr := units.ValidateQuantityForUnit(dto.InitialQuantity, u.Type, u.Name, dto.UnitContentQty != nil); vErr != nil {
				return nil, fmt.Errorf("items: %w", vErr)
			}
		}
	}

	// Default the tax code from the tenant compliance config when unset — treasury/eTIMS reads
	// this off the item on POS/ordering sales. The inclusive-vs-exclusive behaviour itself is
	// resolved at read time from the tenant setting, so it always follows the current setting.
	if dto.TaxCodeID == "" {
		if cfg, cErr := s.client.TenantInventoryConfig.Query().
			Where(tenantinventoryconfig.TenantID(tenantID)).Only(ctx); cErr == nil && cfg.DefaultTaxCode != "" {
			dto.TaxCodeID = cfg.DefaultTaxCode
		}
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	tags := dto.Tags
	if tags == nil {
		tags = []string{}
	}

	createBuilder := tx.Item.Create().
		SetTenantID(tenantID).
		SetSku(dto.SKU).
		SetName(dto.Name).
		SetNillableDescription(&dto.Description).
		SetNillableCategoryID(dto.CategoryID).
		SetNillableBrandID(dto.BrandID).
		SetNillableUnitID(dto.UnitID).
		SetType(item.Type(dto.Type)).
		SetIsActive(dto.IsActive).
		SetNillableImageURL(&dto.ImageURL).
		SetNillablePreferredSupplierID(dto.PreferredSupplierID).
		SetTags(tags).
		SetMetadata(dto.Metadata).
		SetNillableCostPrice(dto.CostPrice).
		SetNillablePurchasePrice(dto.PurchasePrice).
		SetNillablePurchasePackSize(dto.PurchasePackSize).
		SetNillableYieldPct(dto.YieldPct).
		SetNillableMinSellingPrice(dto.MinSellingPrice).
		SetNillableMaxSellingPrice(dto.MaxSellingPrice).
		SetNillableTargetMarginPercent(dto.TargetMarginPercent).
		SetRequiresAgeVerification(dto.RequiresAgeVerification).
		SetIsControlledSubstance(dto.IsControlledSubstance).
		SetIsPerishable(dto.IsPerishable).
		SetTrackLots(dto.TrackLots).
		SetTrackSerialNumbers(dto.TrackSerialNumbers).
		SetNillableShelfLifeDays(dto.ShelfLifeDays).
		SetNillableWeightKg(dto.WeightKg).
		SetNillableDurationMinutes(dto.DurationMinutes).
		SetTaxInclusive(dto.TaxInclusive)
	if len(dto.DimensionsCm) > 0 {
		createBuilder = createBuilder.SetDimensionsCm(dto.DimensionsCm)
	}
	if dto.PurchaseUnit != "" {
		createBuilder = createBuilder.SetPurchaseUnit(dto.PurchaseUnit)
	}
	if dto.UnitContentQty != nil {
		createBuilder = createBuilder.SetNillableUnitContentQty(dto.UnitContentQty)
	}
	if dto.UnitContentUOM != "" {
		createBuilder = createBuilder.SetUnitContentUom(dto.UnitContentUOM)
	}
	if dto.StockTrackingMode != "" {
		createBuilder = createBuilder.SetStockTrackingMode(item.StockTrackingMode(dto.StockTrackingMode))
	}
	if dto.Manufacturer != "" {
		createBuilder = createBuilder.SetManufacturer(dto.Manufacturer)
	}
	if dto.Model != "" {
		createBuilder = createBuilder.SetModel(dto.Model)
	}
	if dto.GenericName != "" {
		createBuilder = createBuilder.SetGenericName(dto.GenericName)
	}
	if dto.ActiveIngredient != "" {
		createBuilder = createBuilder.SetActiveIngredient(dto.ActiveIngredient)
	}
	if dto.DosageForm != "" {
		createBuilder = createBuilder.SetDosageForm(dto.DosageForm)
	}
	if dto.Strength != "" {
		createBuilder = createBuilder.SetStrength(dto.Strength)
	}
	if dto.DrugClass != "" {
		createBuilder = createBuilder.SetDrugClass(dto.DrugClass)
	}
	if dto.ControlledSubstanceSchedule != "" {
		createBuilder = createBuilder.SetControlledSubstanceSchedule(item.ControlledSubstanceSchedule(dto.ControlledSubstanceSchedule))
	}
	// E-commerce attributes (optional). is_returnable/allow_backorder/is_discontinued and
	// non_billable are all pointers: set only when the client sent them, so an omitted flag
	// falls through to the ent schema default (e.g. is_returnable defaults true) instead of
	// being forced to false by a zero-valued bool.
	if dto.NonBillable != nil {
		createBuilder = createBuilder.SetNonBillable(*dto.NonBillable)
	}
	if dto.NotForSale != nil {
		createBuilder = createBuilder.SetNotForSale(*dto.NotForSale)
	} else if dto.Type == string(item.TypeINGREDIENT) {
		// Raw ingredients are recipe/BOM inputs, not standalone sellable products — a
		// caller that doesn't explicitly say otherwise must never end up on a POS/ordering
		// sales surface (see not_for_sale's own schema comment). Staff can still flip this
		// per-item via inventory-ui's "Mark for sale" bulk action to explicitly sell a raw
		// ingredient directly (e.g. loose ice, a bottled sauce).
		createBuilder = createBuilder.SetNotForSale(true)
	}
	if dto.UsableInRecipes != nil {
		createBuilder = createBuilder.SetUsableInRecipes(*dto.UsableInRecipes)
	}
	if dto.GTIN != "" {
		createBuilder = createBuilder.SetGtin(dto.GTIN)
	}
	if dto.MPN != "" {
		createBuilder = createBuilder.SetMpn(dto.MPN)
	}
	if dto.Condition != "" {
		createBuilder = createBuilder.SetCondition(item.Condition(dto.Condition))
	}
	if dto.Slug != "" {
		createBuilder = createBuilder.SetSlug(dto.Slug)
	}
	if dto.ShortDescription != "" {
		createBuilder = createBuilder.SetShortDescription(dto.ShortDescription)
	}
	if dto.MetaTitle != "" {
		createBuilder = createBuilder.SetMetaTitle(dto.MetaTitle)
	}
	if dto.MetaDescription != "" {
		createBuilder = createBuilder.SetMetaDescription(dto.MetaDescription)
	}
	if dto.CountryOfOrigin != "" {
		createBuilder = createBuilder.SetCountryOfOrigin(dto.CountryOfOrigin)
	}
	if dto.HSCode != "" {
		createBuilder = createBuilder.SetHsCode(dto.HSCode)
	}
	createBuilder = createBuilder.
		SetNillableReturnWindowDays(dto.ReturnWindowDays).
		SetNillableIsReturnable(dto.IsReturnable).
		SetNillableAllowBackorder(dto.AllowBackorder).
		SetNillableIsDiscontinued(dto.IsDiscontinued)
	if dto.Barcode != "" {
		createBuilder = createBuilder.SetBarcode(dto.Barcode)
	}
	if dto.BarcodeType != "" {
		createBuilder = createBuilder.SetBarcodeType(dto.BarcodeType)
	}
	if dto.TaxCodeID != "" {
		createBuilder = createBuilder.SetTaxCodeID(dto.TaxCodeID)
	}
	if dto.EtimsItemClsCd != "" {
		createBuilder = createBuilder.SetEtimsItemClsCd(dto.EtimsItemClsCd)
	}
	if dto.EtimsPkgUnitCd != "" {
		createBuilder = createBuilder.SetEtimsPkgUnitCd(dto.EtimsPkgUnitCd)
	}
	if dto.EtimsQtyUnitCd != "" {
		createBuilder = createBuilder.SetEtimsQtyUnitCd(dto.EtimsQtyUnitCd)
	}
	if dto.TotalCapacity != nil {
		createBuilder = createBuilder.SetTotalCapacity(*dto.TotalCapacity)
	}
	if dto.EventStartAt != nil {
		createBuilder = createBuilder.SetEventStartAt(*dto.EventStartAt)
	}
	if dto.EventEndAt != nil {
		createBuilder = createBuilder.SetEventEndAt(*dto.EventEndAt)
	}
	if dto.EventVenue != nil {
		createBuilder = createBuilder.SetEventVenue(*dto.EventVenue)
	}
	if dto.UseCase != "" {
		createBuilder = createBuilder.SetUseCase(item.UseCase(dto.UseCase))
	}
	if dto.MealPlan != nil {
		createBuilder = createBuilder.SetMealPlan(item.MealPlan(*dto.MealPlan))
	}
	if dto.OccupancyBasis != nil {
		createBuilder = createBuilder.SetOccupancyBasis(item.OccupancyBasis(*dto.OccupancyBasis))
	}
	if dto.MaxAdults != nil {
		createBuilder = createBuilder.SetMaxAdults(*dto.MaxAdults)
	}
	if dto.MaxChildren != nil {
		createBuilder = createBuilder.SetMaxChildren(*dto.MaxChildren)
	}
	createBuilder = createBuilder.SetExtraBedAllowed(dto.ExtraBedAllowed)
	if dto.SingleSupplement != nil {
		createBuilder = createBuilder.SetSingleSupplement(*dto.SingleSupplement)
	}
	i, err := createBuilder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: create item: %w", err)
	}

	// Opening stock defaults to 0 — a brand-new item has no stock until it's received or counted
	// in. (Previously this forced 1, seeding phantom stock on every item.) A real opening quantity
	// is recorded with an opening_balance audit adjustment below so it doesn't bypass the ledger.
	initialQty := dto.InitialQuantity
	if initialQty < 0 {
		initialQty = 0
	}

	// Auto-seed reorder level from tenant unit defaults when not explicitly provided.
	unitAbbr := ""
	if dto.UnitID != nil {
		if u, uLookupErr := s.client.Unit.Get(ctx, *dto.UnitID); uLookupErr == nil {
			unitAbbr = u.Abbreviation
		}
	}
	reorderLevel := resolveReorderLevel(ctx, s.client, tenantID, unitAbbr, dto.ReorderLevel)

	// Resolve default warehouse
	wh, whErr := s.client.Warehouse.Query().
		Where(
			warehouse.TenantID(tenantID),
			warehouse.IsDefault(true),
			warehouse.IsActive(true),
		).
		First(ctx)
	if whErr == nil && IsStockTracked(i.Type) {
		// Resolve unit of measure name for the balance
		uom := "PIECE"
		if dto.UnitID != nil {
			u, uErr := s.client.Unit.Get(ctx, *dto.UnitID)
			if uErr == nil {
				uom = u.Name
			}
		}

		_, err = tx.InventoryBalance.Create().
			SetTenantID(tenantID).
			SetItemID(i.ID).
			SetWarehouseID(wh.ID).
			SetOnHand(initialQty).
			SetAvailable(initialQty).
			SetReserved(0).
			SetReorderLevel(reorderLevel).
			SetUnitOfMeasure(uom).
			Save(ctx)
		if err != nil {
			s.log.Warn("items: create initial balance failed", zap.Error(err), zap.String("sku", dto.SKU))
		} else if initialQty > 0 {
			// Record a real opening quantity in the audit ledger instead of silently writing the
			// balance, so opening stock has a StockAdjustment trail like any other movement.
			if _, aerr := tx.StockAdjustment.Create().
				SetTenantID(tenantID).
				SetItemID(i.ID).
				SetWarehouseID(wh.ID).
				SetQuantityBefore(0).
				SetQuantityChange(initialQty).
				SetQuantityAfter(initialQty).
				SetReason(stockadjustment.ReasonOpeningBalance).
				SetReference(dto.SKU).
				SetNotes("Opening balance at item creation").
				SetAdjustedBy(uuid.Nil).
				Save(ctx); aerr != nil {
				s.log.Warn("items: opening-balance adjustment failed", zap.Error(aerr), zap.String("sku", dto.SKU))
			}
		}

		// If "add to all outlets" is requested, create balances for all other active warehouses
		if dto.AddToAllOutlets {
			allWarehouses, whsErr := tx.Warehouse.Query().
				Where(
					warehouse.TenantID(tenantID),
					warehouse.IsActive(true),
					warehouse.IDNEQ(wh.ID), // skip the default warehouse (already created above)
				).
				All(ctx)
			if whsErr == nil {
				for _, w := range allWarehouses {
					_, balErr := tx.InventoryBalance.Create().
						SetTenantID(tenantID).
						SetItemID(i.ID).
						SetWarehouseID(w.ID).
						SetOnHand(initialQty).
						SetAvailable(initialQty).
						SetReserved(0).
						SetReorderLevel(reorderLevel).
						SetUnitOfMeasure(uom).
						Save(ctx)
					if balErr != nil {
						s.log.Warn("items: create balance for additional warehouse failed",
							zap.Error(balErr), zap.String("sku", dto.SKU), zap.String("warehouse", w.Code))
					}
				}
			}
		}
	}

	// Resolve category name for enriched event payload
	categoryName := ""
	if dto.CategoryID != nil {
		cat, catErr := s.client.ItemCategory.Get(ctx, *dto.CategoryID)
		if catErr == nil {
			categoryName = cat.Name
		}
	}

	// Resolve brand name (Item.BrandID -> ItemBrand.Name) for the enriched event payload —
	// distinct from the free-text Manufacturer field just below.
	brandName := ""
	if dto.BrandID != nil {
		brand, brandErr := s.client.ItemBrand.Get(ctx, *dto.BrandID)
		if brandErr == nil {
			brandName = brand.Name
		}
	}

	// Resolve unit name + KRA quantity-unit mapping for the enriched event payload
	unitName, unitAbbrev, unitKraQty := "", "", ""
	if dto.UnitID != nil {
		u, uErr := s.client.Unit.Get(ctx, *dto.UnitID)
		if uErr == nil {
			unitName = u.Name
			unitAbbrev = u.Abbreviation
			unitKraQty = u.KraQtyUnitCd
		}
	}

	// Publish enriched event to outbox
	event := &events.Event{
		ID:            uuid.New(),
		TenantID:      tenantID,
		AggregateType: "inventory",
		AggregateID:   i.ID,
		EventType:     "item.created",
		Payload: map[string]any{
			"id":                        i.ID,
			"sku":                       i.Sku,
			"name":                      i.Name,
			"description":               i.Description,
			"type":                      i.Type,
			"category_id":               i.CategoryID,
			"category_name":             categoryName,
			"manufacturer":              i.Manufacturer,
			"brand_name":                brandName,
			"model":                     i.Model,
			"unit_id":                   i.UnitID,
			"unit_name":                 unitName,
			"is_active":                 i.IsActive,
			"image_url":                 i.ImageURL,
			"tags":                      i.Tags,
			"barcode":                   i.Barcode,
			"barcode_type":              i.BarcodeType,
			"requires_age_verification": i.RequiresAgeVerification,
			"is_controlled_substance":   i.IsControlledSubstance,
			"is_perishable":             i.IsPerishable,
			"track_serial_numbers":      i.TrackSerialNumbers,
			"track_lots":                i.TrackLots,
			"weight_kg":                 i.WeightKg,
			"dimensions_cm":             i.DimensionsCm,
			"duration_minutes":          i.DurationMinutes,
			"use_case":                  i.UseCase,
			"meal_plan":                 i.MealPlan,
			"occupancy_basis":           i.OccupancyBasis,
			"max_adults":                i.MaxAdults,
			"max_children":              i.MaxChildren,
			"tax_code_id":               i.TaxCodeID,
			"tax_inclusive":             i.TaxInclusive,
			"cost_price":                i.CostPrice,
			"unit_content_qty":          i.UnitContentQty,
			"unit_content_uom":          i.UnitContentUom,
			"stock_tracking_mode":       i.StockTrackingMode,
		},
		Timestamp: time.Now().UTC(),
	}
	mergeEtimsEventFields(event.Payload, i, unitAbbrev, unitKraQty)

	payload, err := event.ToJSON()
	if err != nil {
		return nil, fmt.Errorf("items: marshal event: %w", err)
	}

	_, err = tx.OutboxEvent.Create().
		SetID(event.ID).
		SetTenantID(tenantID).
		SetAggregateType(event.AggregateType).
		SetAggregateID(event.AggregateID.String()).
		SetEventType(event.EventType).
		SetPayload(json.RawMessage(payload)).
		SetStatus("PENDING").
		SetCreatedAt(event.Timestamp).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: create outbox record: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("items: commit transaction: %w", err)
	}

	// Materialize the selling-price guardrails as real tier prices (Retail=max, Wholesale=min)
	// so the POS/ordering price-resolve reads the merchant's actual price, never a cost+margin
	// cooked price. Post-commit + best-effort: a failure here must not fail item creation.
	s.applyGuardrailTierPrices(ctx, tenantID, i.ID, dto)

	return s.mapToDTO(i), nil
}

// applyGuardrailTierPrices upserts Retail(=max_selling_price)/Wholesale(=min_selling_price) tier
// prices for the item from its guardrail fields, logging (never returning) any error.
func (s *Service) applyGuardrailTierPrices(ctx context.Context, tenantID, itemID uuid.UUID, dto ItemDTO) {
	var maxP, minP float64
	if dto.MaxSellingPrice != nil {
		maxP = *dto.MaxSellingPrice
	}
	if dto.MinSellingPrice != nil {
		minP = *dto.MinSellingPrice
	}
	if maxP <= 0 && minP <= 0 {
		return
	}
	if err := s.EnsureGuardrailTierPrices(ctx, tenantID, itemID, maxP, minP); err != nil {
		s.log.Warn("items: apply guardrail tier prices failed",
			zap.String("item_id", itemID.String()), zap.Error(err))
	}
}

// UpdateItem updates an item and records an outbox event within a transaction.
func (s *Service) UpdateItem(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, dto ItemDTO) (*ItemDTO, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	updateTags := dto.Tags
	if updateTags == nil {
		updateTags = []string{}
	}

	if err = validatePriceBand(&dto); err != nil {
		return nil, fmt.Errorf("items: %w", err)
	}
	// Auto-compute EP cost from purchase fields if not explicitly provided.
	resolveEPCost(&dto)

	// Capture the pre-update standard cost so a real change can be audited below. Best-effort:
	// a lookup failure just means no before/after audit row, never a blocked update.
	var prevCostPrice *float64
	if prevItm, pErr := tx.Item.Query().
		Where(item.TenantID(tenantID), item.ID(id)).
		Select(item.FieldCostPrice).
		Only(ctx); pErr == nil {
		prevCostPrice = prevItm.CostPrice
	}

	updateBuilder := tx.Item.UpdateOneID(id).
		Where(item.TenantID(tenantID)).
		SetName(dto.Name).
		SetNillableDescription(&dto.Description).
		SetNillableCategoryID(dto.CategoryID).
		SetNillableBrandID(dto.BrandID).
		SetNillableUnitID(dto.UnitID).
		SetType(item.Type(dto.Type)).
		SetIsActive(dto.IsActive).
		SetNillableImageURL(&dto.ImageURL).
		SetTags(updateTags).
		SetMetadata(dto.Metadata).
		SetNillableCostPrice(dto.CostPrice).
		SetNillablePurchasePrice(dto.PurchasePrice).
		SetNillablePurchasePackSize(dto.PurchasePackSize).
		SetNillableYieldPct(dto.YieldPct).
		SetNillableMinSellingPrice(dto.MinSellingPrice).
		SetNillableMaxSellingPrice(dto.MaxSellingPrice).
		SetNillableTargetMarginPercent(dto.TargetMarginPercent).
		SetRequiresAgeVerification(dto.RequiresAgeVerification).
		SetIsControlledSubstance(dto.IsControlledSubstance).
		SetIsPerishable(dto.IsPerishable).
		SetTrackLots(dto.TrackLots).
		SetTrackSerialNumbers(dto.TrackSerialNumbers).
		SetNillableShelfLifeDays(dto.ShelfLifeDays).
		SetNillableWeightKg(dto.WeightKg).
		SetNillableDurationMinutes(dto.DurationMinutes).
		SetManufacturer(dto.Manufacturer).
		SetModel(dto.Model).
		SetTaxInclusive(dto.TaxInclusive)
	if dto.GenericName != "" {
		updateBuilder = updateBuilder.SetGenericName(dto.GenericName)
	}
	if dto.ActiveIngredient != "" {
		updateBuilder = updateBuilder.SetActiveIngredient(dto.ActiveIngredient)
	}
	if dto.DosageForm != "" {
		updateBuilder = updateBuilder.SetDosageForm(dto.DosageForm)
	}
	if dto.Strength != "" {
		updateBuilder = updateBuilder.SetStrength(dto.Strength)
	}
	if dto.DrugClass != "" {
		updateBuilder = updateBuilder.SetDrugClass(dto.DrugClass)
	}
	if dto.ControlledSubstanceSchedule != "" {
		updateBuilder = updateBuilder.SetControlledSubstanceSchedule(item.ControlledSubstanceSchedule(dto.ControlledSubstanceSchedule))
	}
	// Preferred supplier: a non-nil, non-zero UUID assigns it; the zero UUID explicitly
	// unassigns (clears) it. nil leaves the existing value untouched (partial update).
	if dto.PreferredSupplierID != nil {
		if *dto.PreferredSupplierID == uuid.Nil {
			updateBuilder = updateBuilder.ClearPreferredSupplierID()
		} else {
			updateBuilder = updateBuilder.SetPreferredSupplierID(*dto.PreferredSupplierID)
		}
	}
	if len(dto.DimensionsCm) > 0 {
		updateBuilder = updateBuilder.SetDimensionsCm(dto.DimensionsCm)
	}
	if dto.PurchaseUnit != "" {
		updateBuilder = updateBuilder.SetPurchaseUnit(dto.PurchaseUnit)
	}
	// Content-per-unit + tracking mode: pointer/empty-string presence semantics so
	// partial updates never clobber stored values. unit_content_qty == 0 clears the bridge.
	if dto.UnitContentQty != nil {
		if *dto.UnitContentQty <= 0 {
			updateBuilder = updateBuilder.ClearUnitContentQty().ClearUnitContentUom()
		} else {
			updateBuilder = updateBuilder.SetUnitContentQty(*dto.UnitContentQty)
		}
	}
	if dto.UnitContentUOM != "" {
		updateBuilder = updateBuilder.SetUnitContentUom(dto.UnitContentUOM)
	}
	if dto.StockTrackingMode != "" {
		updateBuilder = updateBuilder.SetStockTrackingMode(item.StockTrackingMode(dto.StockTrackingMode))
	}
	// E-commerce attributes — set only when present so a partial update (e.g. an
	// edit form that doesn't carry these) never clobbers existing values. The boolean
	// flags (is_returnable/allow_backorder/is_discontinued) and non_billable are all
	// pointers, so presence is explicit: SetNillable applies them only when the client
	// actually sent a value, leaving stored values untouched otherwise.
	if dto.NonBillable != nil {
		updateBuilder = updateBuilder.SetNonBillable(*dto.NonBillable)
	}
	updateBuilder = updateBuilder.
		SetNillableIsReturnable(dto.IsReturnable).
		SetNillableAllowBackorder(dto.AllowBackorder).
		SetNillableIsDiscontinued(dto.IsDiscontinued).
		SetNillableUsableInRecipes(dto.UsableInRecipes).
		SetNillableNotForSale(dto.NotForSale)
	if dto.GTIN != "" {
		updateBuilder = updateBuilder.SetGtin(dto.GTIN)
	}
	if dto.MPN != "" {
		updateBuilder = updateBuilder.SetMpn(dto.MPN)
	}
	if dto.Condition != "" {
		updateBuilder = updateBuilder.SetCondition(item.Condition(dto.Condition))
	}
	if dto.Slug != "" {
		updateBuilder = updateBuilder.SetSlug(dto.Slug)
	}
	if dto.ShortDescription != "" {
		updateBuilder = updateBuilder.SetShortDescription(dto.ShortDescription)
	}
	if dto.MetaTitle != "" {
		updateBuilder = updateBuilder.SetMetaTitle(dto.MetaTitle)
	}
	if dto.MetaDescription != "" {
		updateBuilder = updateBuilder.SetMetaDescription(dto.MetaDescription)
	}
	if dto.CountryOfOrigin != "" {
		updateBuilder = updateBuilder.SetCountryOfOrigin(dto.CountryOfOrigin)
	}
	if dto.HSCode != "" {
		updateBuilder = updateBuilder.SetHsCode(dto.HSCode)
	}
	updateBuilder = updateBuilder.SetNillableReturnWindowDays(dto.ReturnWindowDays)
	if dto.Barcode != "" {
		updateBuilder = updateBuilder.SetBarcode(dto.Barcode)
	}
	if dto.BarcodeType != "" {
		updateBuilder = updateBuilder.SetBarcodeType(dto.BarcodeType)
	}
	if dto.TaxCodeID != "" {
		updateBuilder = updateBuilder.SetTaxCodeID(dto.TaxCodeID)
	}
	if dto.EtimsItemClsCd != "" {
		updateBuilder = updateBuilder.SetEtimsItemClsCd(dto.EtimsItemClsCd)
	}
	if dto.EtimsPkgUnitCd != "" {
		updateBuilder = updateBuilder.SetEtimsPkgUnitCd(dto.EtimsPkgUnitCd)
	}
	if dto.EtimsQtyUnitCd != "" {
		updateBuilder = updateBuilder.SetEtimsQtyUnitCd(dto.EtimsQtyUnitCd)
	}
	if dto.TotalCapacity != nil {
		updateBuilder = updateBuilder.SetTotalCapacity(*dto.TotalCapacity)
	}
	// Persist booked_capacity (previously ignored) and prevent overselling an event:
	// booked seats can never exceed total capacity.
	if dto.BookedCapacity != nil {
		if dto.TotalCapacity != nil && *dto.BookedCapacity > *dto.TotalCapacity {
			err = fmt.Errorf("booked_capacity (%d) cannot exceed total_capacity (%d)", *dto.BookedCapacity, *dto.TotalCapacity)
			return nil, err
		}
		if *dto.BookedCapacity < 0 {
			err = fmt.Errorf("booked_capacity cannot be negative")
			return nil, err
		}
		updateBuilder = updateBuilder.SetBookedCapacity(*dto.BookedCapacity)
	}
	if dto.EventStartAt != nil {
		updateBuilder = updateBuilder.SetEventStartAt(*dto.EventStartAt)
	}
	if dto.EventEndAt != nil {
		updateBuilder = updateBuilder.SetEventEndAt(*dto.EventEndAt)
	}
	if dto.EventVenue != nil {
		updateBuilder = updateBuilder.SetEventVenue(*dto.EventVenue)
	}
	if dto.UseCase != "" {
		updateBuilder = updateBuilder.SetUseCase(item.UseCase(dto.UseCase))
	}
	if dto.MealPlan != nil {
		updateBuilder = updateBuilder.SetMealPlan(item.MealPlan(*dto.MealPlan))
	}
	if dto.OccupancyBasis != nil {
		updateBuilder = updateBuilder.SetOccupancyBasis(item.OccupancyBasis(*dto.OccupancyBasis))
	}
	if dto.MaxAdults != nil {
		updateBuilder = updateBuilder.SetMaxAdults(*dto.MaxAdults)
	}
	if dto.MaxChildren != nil {
		updateBuilder = updateBuilder.SetMaxChildren(*dto.MaxChildren)
	}
	updateBuilder = updateBuilder.SetExtraBedAllowed(dto.ExtraBedAllowed)
	if dto.SingleSupplement != nil {
		updateBuilder = updateBuilder.SetSingleSupplement(*dto.SingleSupplement)
	}

	i, err := updateBuilder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("items: update item: %w", err)
	}

	// Sync the pricing-tier rows to the item's own guardrail fields — the SAME choke point
	// setSellingPrice uses. Without this, editing price via the general item-edit form updated
	// Item.MaxSellingPrice/MinSellingPrice directly but left item_pricings (what POS/ordering
	// price-resolution actually reads first) untouched, so a price typed into the edit form
	// silently never showed up anywhere downstream — a second, independent instance of the exact
	// "I changed the price and it never updated" bug, distinct from the dedicated price-only
	// endpoint's copy of the same class of bug. Best-effort here (a broad item-edit save covering
	// dozens of unrelated fields must not fail wholesale over a tier-price hiccup); the dedicated
	// SetSellingPrice endpoint remains the hard-failing path when price is the ONLY thing being set.
	maxP, minP := 0.0, 0.0
	if i.MaxSellingPrice != nil {
		maxP = *i.MaxSellingPrice
	}
	if i.MinSellingPrice != nil {
		minP = *i.MinSellingPrice
	}
	if maxP > 0 || minP > 0 {
		if terr := s.EnsureGuardrailTierPrices(ctx, tenantID, i.ID, maxP, minP); terr != nil {
			s.log.Warn("update item: tier price sync failed", zap.String("sku", i.Sku), zap.Error(terr))
		}
	}

	// Update reorder level/quantity on all InventoryBalance records for this item if provided.
	// Reorder policy LIVES on the balance (per warehouse) — the item DTO only mirrors it —
	// so an edit on a never-stocked item must CREATE the default-warehouse balance row or
	// the form's reorder values would silently vanish on save.
	if (dto.ReorderLevel > 0 || dto.ReorderQuantity > 0) && IsStockTracked(i.Type) {
		bals, balErr := tx.InventoryBalance.Query().
			Where(inventorybalance.ItemID(i.ID), inventorybalance.TenantID(tenantID)).
			All(ctx)
		if balErr == nil {
			if len(bals) == 0 {
				if wh, whErr := tx.Warehouse.Query().
					Where(warehouse.TenantID(tenantID), warehouse.IsDefault(true), warehouse.IsActive(true)).
					First(ctx); whErr == nil {
					_, _ = tx.InventoryBalance.Create().
						SetTenantID(tenantID).
						SetItemID(i.ID).
						SetWarehouseID(wh.ID).
						SetReorderLevel(dto.ReorderLevel).
						SetReorderQuantity(dto.ReorderQuantity).
						Save(ctx)
				}
			}
			for _, bal := range bals {
				upd := tx.InventoryBalance.UpdateOneID(bal.ID)
				if dto.ReorderLevel > 0 {
					upd = upd.SetReorderLevel(dto.ReorderLevel)
				}
				if dto.ReorderQuantity > 0 {
					upd = upd.SetReorderQuantity(dto.ReorderQuantity)
				}
				_, _ = upd.Save(ctx)
			}
		}
	}

	// The single choke point for the item.updated payload shape (category/unit resolution,
	// eTIMS fields, price fields — see eol.go) — was previously duplicated inline here with its
	// own copy that had silently drifted to omit min/max_selling_price entirely. One function now
	// serves UpdateItem, the EOL mutations, and SetCostPriceAndPublish.
	if err = s.emitItemUpdatedEvent(ctx, tx, tenantID, i); err != nil {
		return nil, fmt.Errorf("items: emit update event: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("items: commit transaction: %w", err)
	}

	// Audit a real standard-cost change. This is the "default/pre-fill" cost only — it never
	// touches the value of stock already on hand (see InventoryLot cost layers) — but every
	// change to it should still be attributable and reviewable.
	if s.auditSvc != nil && !floatPtrEqual(prevCostPrice, i.CostPrice) {
		s.auditSvc.Record(ctx, audit.Entry{
			TenantID:    tenantID,
			ActorUserID: actorFromContext(ctx),
			Action:      "item.standard_cost_changed",
			EntityType:  "item",
			EntityID:    i.ID.String(),
			Before:      map[string]any{"sku": i.Sku, "cost_price": prevCostPrice},
			After:       map[string]any{"sku": i.Sku, "cost_price": i.CostPrice},
		})
	}

	// Keep the Retail/Wholesale tier prices in step with edited guardrails (Retail=max,
	// Wholesale=min) so an edit that sets/raises the selling price is reflected on the POS.
	s.applyGuardrailTierPrices(ctx, tenantID, i.ID, dto)

	return s.mapToDTO(i), nil
}

// floatPtrEqual reports whether two optional float values are equal, treating nil as distinct
// from any concrete value (including 0).
func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// EtimsRegistration carries the KRA-assigned eTIMS codes minted by treasury-api when an item is
// registered in the eTIMS item master. Empty fields are left untouched.
type EtimsRegistration struct {
	ItemCd    string // KRA-assigned itemCd (fixed-width code)
	ItemClsCd string // classification (UNSPSC)
	PkgUnitCd string
	QtyUnitCd string
	// TaxCode is the tenant's treasury TaxCode.code resolved from the registered KRA tax band
	// (e.g. "VAT-16") — NOT the raw band letter. Only fills Item.tax_code_id when the item
	// currently has none; never overwrites a tenant/manually-set tax code.
	TaxCode string
}

// SetEtimsRegistration mirrors the KRA-registered eTIMS codes back onto the inventory item — a
// NARROW, partial update (never the full UpdateItem, which would clobber name/type/metadata). It
// is the write-back that makes the Edit-Item form show an item's real synced eTIMS classification/
// package/quantity codes instead of the tenant defaults. Resolves by item UUID, falling back to
// SKU when the UUID is unknown (an ad-hoc/auto-registered line). Idempotent and best-effort:
// blank codes are skipped, and a not-found item is a no-op (never an error the consumer must retry).
func (s *Service) SetEtimsRegistration(ctx context.Context, tenantID uuid.UUID, itemID *uuid.UUID, sku string, reg EtimsRegistration) error {
	// Resolve the target item: prefer the UUID, else the tenant-scoped SKU.
	q := s.client.Item.Query().Where(item.TenantID(tenantID))
	switch {
	case itemID != nil && *itemID != uuid.Nil:
		q = q.Where(item.ID(*itemID))
	case sku != "":
		q = q.Where(item.Sku(sku))
	default:
		return nil // nothing to resolve on
	}
	target, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil // item not in inventory (e.g. a pure ad-hoc invoice line) — nothing to mirror
		}
		return fmt.Errorf("items: resolve item for etims write-back: %w", err)
	}

	upd := s.client.Item.UpdateOneID(target.ID).Where(item.TenantID(tenantID))
	changed := false
	if reg.ItemClsCd != "" {
		upd = upd.SetEtimsItemClsCd(reg.ItemClsCd)
		changed = true
	}
	if reg.PkgUnitCd != "" {
		upd = upd.SetEtimsPkgUnitCd(reg.PkgUnitCd)
		changed = true
	}
	if reg.QtyUnitCd != "" {
		upd = upd.SetEtimsQtyUnitCd(reg.QtyUnitCd)
		changed = true
	}
	// Only fill tax_code_id when the item doesn't already have one — this is a "resolve when
	// missing" write-back, never an override of a tenant/manually-set tax code (see Workstream 1
	// fix in pricing_enrich.go, which already prefers an item's own tax_code_id over any default).
	if reg.TaxCode != "" && target.TaxCodeID == "" {
		upd = upd.SetTaxCodeID(reg.TaxCode)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := upd.Exec(ctx); err != nil {
		return fmt.Errorf("items: write eTIMS registration codes: %w", err)
	}
	return nil
}
