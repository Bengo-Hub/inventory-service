package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/bengobox/inventory-service/internal/http/handlers"
	"github.com/bengobox/inventory-service/internal/modules/items"
	"github.com/bengobox/inventory-service/internal/modules/stock"
)

// ─── Mock Services ──────────────────────────────────────────────────────

type mockItemsSvc struct {
	getStockFn       func(ctx context.Context, tenantID uuid.UUID, sku string) (*items.StockAvailability, error)
	bulkAvailFn      func(ctx context.Context, tenantID uuid.UUID, skus []string) ([]items.StockAvailability, error)
	bomAvailFn       func(ctx context.Context, tenantID uuid.UUID, skus []string) ([]items.BOMAvailabilityResult, error)
	summaryFn        func(ctx context.Context, tenantID uuid.UUID) (*items.InventorySummary, error)
	createItemFn     func(ctx context.Context, tenantID uuid.UUID, dto items.ItemDTO) (*items.ItemDTO, error)
	updateItemFn     func(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, dto items.ItemDTO) (*items.ItemDTO, error)
	listItemsFn      func(ctx context.Context, tenantID uuid.UUID, typeFilter, statusFilter string, limit, offset int, categoryID *uuid.UUID, unitID *uuid.UUID, search string, outletID *uuid.UUID, warehouseID *uuid.UUID, useCase string, tagsFilter ...string) ([]items.ItemDTO, int, error)
	listCategoriesFn func(ctx context.Context, tenantID uuid.UUID) ([]items.CategoryDTO, error)
	createCategoryFn func(ctx context.Context, tenantID uuid.UUID, dto items.CategoryDTO) (*items.CategoryDTO, error)
	updateCategoryFn func(ctx context.Context, tenantID, id uuid.UUID, dto items.CategoryDTO) (*items.CategoryDTO, error)
}

func (m *mockItemsSvc) GetStockAvailability(ctx context.Context, tenantID uuid.UUID, sku string) (*items.StockAvailability, error) {
	if m.getStockFn != nil {
		return m.getStockFn(ctx, tenantID, sku)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) EnsureDefaultPrice(ctx context.Context, tenantID, itemID uuid.UUID, price float64) error {
	return nil
}

func (m *mockItemsSvc) SetSellingPriceBySKU(ctx context.Context, tenantID uuid.UUID, sku string, price float64) (*items.ItemDTO, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) DeactivateItemBySKU(ctx context.Context, tenantID uuid.UUID, sku string) error {
	return nil
}

func (m *mockItemsSvc) BulkItemAction(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID, action string) (*items.BulkActionResult, error) {
	return &items.BulkActionResult{Skipped: []items.BulkSkipped{}}, nil
}

func (m *mockItemsSvc) MarkItemEOL(ctx context.Context, tenantID uuid.UUID, sku string) (*items.ItemDTO, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) RestoreItemEOL(ctx context.Context, tenantID uuid.UUID, sku string) (*items.ItemDTO, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) HardDeleteItemBySKU(ctx context.Context, tenantID uuid.UUID, sku string) error {
	return nil
}

func (m *mockItemsSvc) StockValuation(ctx context.Context, tenantID, warehouseID uuid.UUID) (*items.StockValuation, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) StockDeadstock(ctx context.Context, tenantID uuid.UUID, days int) (*items.DeadstockReport, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) StockFastMoving(ctx context.Context, tenantID uuid.UUID, days int) (*items.FastMovingReport, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) BulkAvailability(ctx context.Context, tenantID uuid.UUID, skus []string) ([]items.StockAvailability, error) {
	if m.bulkAvailFn != nil {
		return m.bulkAvailFn(ctx, tenantID, skus)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) GetBOMAvailability(ctx context.Context, tenantID uuid.UUID, skus []string) ([]items.BOMAvailabilityResult, error) {
	if m.bomAvailFn != nil {
		return m.bomAvailFn(ctx, tenantID, skus)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) GetInventorySummary(ctx context.Context, tenantID uuid.UUID) (*items.InventorySummary, error) {
	if m.summaryFn != nil {
		return m.summaryFn(ctx, tenantID)
	}
	return nil, nil
}

func (m *mockItemsSvc) CreateItem(ctx context.Context, tenantID uuid.UUID, dto items.ItemDTO) (*items.ItemDTO, error) {
	if m.createItemFn != nil {
		return m.createItemFn(ctx, tenantID, dto)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) UpdateItem(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, dto items.ItemDTO) (*items.ItemDTO, error) {
	if m.updateItemFn != nil {
		return m.updateItemFn(ctx, tenantID, id, dto)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) ListItems(ctx context.Context, tenantID uuid.UUID, typeFilter, statusFilter string, limit, offset int, categoryID *uuid.UUID, unitID *uuid.UUID, search string, outletID *uuid.UUID, warehouseID *uuid.UUID, useCase string, tagsFilter ...string) ([]items.ItemDTO, int, error) {
	if m.listItemsFn != nil {
		return m.listItemsFn(ctx, tenantID, typeFilter, statusFilter, limit, offset, categoryID, unitID, search, outletID, warehouseID, useCase, tagsFilter...)
	}
	return nil, 0, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) ListItemVariants(ctx context.Context, tenantID, itemID uuid.UUID) ([]items.VariantDTO, error) {
	return []items.VariantDTO{}, nil
}

func (m *mockItemsSvc) ListEventItems(ctx context.Context, tenantID uuid.UUID, limit, offset int) ([]items.ItemDTO, int, error) {
	return []items.ItemDTO{}, 0, nil
}

func (m *mockItemsSvc) CreateCategory(ctx context.Context, tenantID uuid.UUID, dto items.CategoryDTO) (*items.CategoryDTO, error) {
	if m.createCategoryFn != nil {
		return m.createCategoryFn(ctx, tenantID, dto)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) UpdateCategory(ctx context.Context, tenantID, id uuid.UUID, dto items.CategoryDTO) (*items.CategoryDTO, error) {
	if m.updateCategoryFn != nil {
		return m.updateCategoryFn(ctx, tenantID, id, dto)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) ListCategoriesFiltered(ctx context.Context, tenantID uuid.UUID, _, _ bool) ([]items.CategoryDTO, error) {
	return m.ListCategories(ctx, tenantID)
}

func (m *mockItemsSvc) ListCategories(ctx context.Context, tenantID uuid.UUID) ([]items.CategoryDTO, error) {
	if m.listCategoriesFn != nil {
		return m.listCategoriesFn(ctx, tenantID)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockItemsSvc) DeleteCategory(ctx context.Context, tenantID, id uuid.UUID) error {
	return nil
}

func (m *mockItemsSvc) ListBrands(ctx context.Context, tenantID uuid.UUID) ([]items.BrandDTO, error) {
	return []items.BrandDTO{}, nil
}

func (m *mockItemsSvc) CreateBrand(ctx context.Context, tenantID uuid.UUID, dto items.BrandDTO) (*items.BrandDTO, error) {
	return &items.BrandDTO{ID: uuid.New(), Name: dto.Name, Code: dto.Code, IsActive: true}, nil
}

func (m *mockItemsSvc) CountItemImages(ctx context.Context, tenantID, itemID uuid.UUID) (int, error) {
	return 0, nil
}

func (m *mockItemsSvc) ListItemImages(ctx context.Context, tenantID, itemID uuid.UUID) ([]items.ItemImageDTO, error) {
	return []items.ItemImageDTO{}, nil
}

func (m *mockItemsSvc) AddItemImage(ctx context.Context, tenantID, itemID uuid.UUID, file multipart.File, header *multipart.FileHeader, setPrimary bool) (*items.ItemImageDTO, error) {
	return &items.ItemImageDTO{ID: uuid.New(), IsPrimary: setPrimary}, nil
}

func (m *mockItemsSvc) UpdateItemImage(ctx context.Context, tenantID, itemID, imageID uuid.UUID, in items.UpdateItemImageInput) (*items.ItemImageDTO, error) {
	return &items.ItemImageDTO{ID: imageID}, nil
}

func (m *mockItemsSvc) DeleteItemImage(ctx context.Context, tenantID, itemID, imageID uuid.UUID) error {
	return nil
}

type mockStockSvc struct {
	createReservationFn      func(ctx context.Context, tenantID uuid.UUID, req stock.ReservationRequest) (*stock.ReservationResponse, error)
	getReservationFn         func(ctx context.Context, tenantID, reservationID uuid.UUID) (*stock.ReservationResponse, error)
	getReservationsByOrderFn func(ctx context.Context, tenantID, orderID uuid.UUID) ([]stock.ReservationResponse, error)
	releaseReservationFn     func(ctx context.Context, tenantID, reservationID uuid.UUID, reason string) error
	consumeReservationFn     func(ctx context.Context, tenantID, reservationID uuid.UUID) (*stock.ConsumeReservationResponse, error)
	recordConsumptionFn      func(ctx context.Context, tenantID uuid.UUID, req stock.ConsumptionRequest) (*stock.ConsumptionResponse, error)
	adjustStockFn            func(ctx context.Context, tenantID uuid.UUID, req stock.AdjustStockRequest) (*stock.AdjustStockResponse, error)
	listAdjustmentsFn        func(ctx context.Context, tenantID uuid.UUID, req stock.ListAdjustmentsRequest) ([]stock.StockAdjustmentDTO, int, error)
	listReservationsFn       func(ctx context.Context, tenantID uuid.UUID, filter stock.ReservationListFilter) ([]stock.ReservationSummary, int, error)
}

func (m *mockStockSvc) CreateReservation(ctx context.Context, tenantID uuid.UUID, req stock.ReservationRequest) (*stock.ReservationResponse, error) {
	if m.createReservationFn != nil {
		return m.createReservationFn(ctx, tenantID, req)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) Breakdown(ctx context.Context, tenantID uuid.UUID, req stock.BreakdownRequest) (*stock.BreakdownResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) ReverseConsumption(ctx context.Context, tenantID uuid.UUID, req stock.ReverseConsumptionRequest) (*stock.ReverseConsumptionResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) GetReservation(ctx context.Context, tenantID, reservationID uuid.UUID) (*stock.ReservationResponse, error) {
	if m.getReservationFn != nil {
		return m.getReservationFn(ctx, tenantID, reservationID)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) GetReservationsByOrderID(ctx context.Context, tenantID, orderID uuid.UUID) ([]stock.ReservationResponse, error) {
	if m.getReservationsByOrderFn != nil {
		return m.getReservationsByOrderFn(ctx, tenantID, orderID)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) ReleaseReservation(ctx context.Context, tenantID, reservationID uuid.UUID, reason string) error {
	if m.releaseReservationFn != nil {
		return m.releaseReservationFn(ctx, tenantID, reservationID, reason)
	}
	return fmt.Errorf("not implemented")
}

func (m *mockStockSvc) ConsumeReservation(ctx context.Context, tenantID, reservationID uuid.UUID) (*stock.ConsumeReservationResponse, error) {
	if m.consumeReservationFn != nil {
		return m.consumeReservationFn(ctx, tenantID, reservationID)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) RecordConsumption(ctx context.Context, tenantID uuid.UUID, req stock.ConsumptionRequest) (*stock.ConsumptionResponse, error) {
	if m.recordConsumptionFn != nil {
		return m.recordConsumptionFn(ctx, tenantID, req)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) AdjustStock(ctx context.Context, tenantID uuid.UUID, req stock.AdjustStockRequest) (*stock.AdjustStockResponse, error) {
	if m.adjustStockFn != nil {
		return m.adjustStockFn(ctx, tenantID, req)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) RelocateItemLocation(ctx context.Context, tenantID uuid.UUID, req stock.RelocateItemLocationRequest) (*stock.RelocateItemLocationResult, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) SetItemOutletMembership(ctx context.Context, tenantID uuid.UUID, req stock.SetItemOutletMembershipRequest) (*stock.SetItemOutletMembershipResult, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) BulkAdjustStock(ctx context.Context, tenantID uuid.UUID, req stock.BulkAdjustStockRequest) (*stock.BulkAdjustStockResult, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) ItemStockHistory(ctx context.Context, tenantID uuid.UUID, sku string, f stock.StockHistoryFilter) (*stock.StockHistoryResult, error) {
	return &stock.StockHistoryResult{Movements: []stock.MovementRow{}}, nil
}

func (m *mockStockSvc) ListAdjustments(ctx context.Context, tenantID uuid.UUID, req stock.ListAdjustmentsRequest) ([]stock.StockAdjustmentDTO, int, error) {
	if m.listAdjustmentsFn != nil {
		return m.listAdjustmentsFn(ctx, tenantID, req)
	}
	return nil, 0, fmt.Errorf("not implemented")
}

func (m *mockStockSvc) ListReservations(ctx context.Context, tenantID uuid.UUID, filter stock.ReservationListFilter) ([]stock.ReservationSummary, int, error) {
	if m.listReservationsFn != nil {
		return m.listReservationsFn(ctx, tenantID, filter)
	}
	return nil, 0, fmt.Errorf("not implemented")
}

// ─── Test Helpers ───────────────────────────────────────────────────────

var (
	testTenantID      = uuid.New()
	testItemID        = uuid.New()
	testWarehouseID   = uuid.New()
	testOrderID       = uuid.New()
	testReservationID = uuid.New()
)

func newTestHandler(t *testing.T, itemsSvc handlers.ItemsServicer, stockSvc handlers.StockServicer) *handlers.InventoryHandler {
	log := zaptest.NewLogger(t)
	return handlers.NewInventoryHandler(log, itemsSvc, stockSvc, nil, nil)
}

func newChiRouter(h *handlers.InventoryHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/v1/{tenant}", func(sub chi.Router) {
		h.RegisterRoutes(sub)
	})
	return r
}

// ─── Items Handler Tests ────────────────────────────────────────────────

func TestGetStockAvailability_Success(t *testing.T) {
	itemsMock := &mockItemsSvc{
		getStockFn: func(_ context.Context, _ uuid.UUID, sku string) (*items.StockAvailability, error) {
			return &items.StockAvailability{
				ItemID:        testItemID,
				SKU:           sku,
				WarehouseID:   testWarehouseID,
				OnHand:        50,
				Available:     45,
				Reserved:      5,
				UnitOfMeasure: "pcs",
				UpdatedAt:     "2026-02-16T10:00:00Z",
			}, nil
		},
	}

	h := newTestHandler(t, itemsMock, &mockStockSvc{})
	r := newChiRouter(h)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/%s/inventory/items/LATTE-001", testTenantID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var avail items.StockAvailability
	require.NoError(t, json.NewDecoder(w.Body).Decode(&avail))
	assert.Equal(t, "LATTE-001", avail.SKU)
	// Stock quantities are float64 (fractional units like 4.5 L are valid).
	assert.Equal(t, float64(50), avail.OnHand)
	assert.Equal(t, float64(45), avail.Available)
	assert.Equal(t, float64(5), avail.Reserved)
}

func TestGetStockAvailability_NotFound(t *testing.T) {
	itemsMock := &mockItemsSvc{
		getStockFn: func(_ context.Context, _ uuid.UUID, _ string) (*items.StockAvailability, error) {
			return nil, fmt.Errorf("items: item not found: sku=INVALID")
		},
	}

	h := newTestHandler(t, itemsMock, &mockStockSvc{})
	r := newChiRouter(h)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/%s/inventory/items/INVALID", testTenantID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetStockAvailability_MissingSKU(t *testing.T) {
	h := newTestHandler(t, &mockItemsSvc{}, &mockStockSvc{})
	r := newChiRouter(h)

	// chi routes {sku} so an empty sku won't match the route at all → 405
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/%s/inventory/items/", testTenantID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Route won't match without a sku param
	assert.True(t, w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed)
}

func TestBulkAvailability_Success(t *testing.T) {
	itemsMock := &mockItemsSvc{
		bulkAvailFn: func(_ context.Context, _ uuid.UUID, skus []string) ([]items.StockAvailability, error) {
			results := make([]items.StockAvailability, len(skus))
			for i, sku := range skus {
				results[i] = items.StockAvailability{
					ItemID:        uuid.New(),
					SKU:           sku,
					OnHand:        100,
					Available:     95,
					Reserved:      5,
					UnitOfMeasure: "pcs",
				}
			}
			return results, nil
		},
	}

	h := newTestHandler(t, itemsMock, &mockStockSvc{})
	r := newChiRouter(h)

	body, _ := json.Marshal(map[string][]string{"skus": {"LATTE-001", "SALAD-001"}})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/availability", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var results []items.StockAvailability
	require.NoError(t, json.NewDecoder(w.Body).Decode(&results))
	assert.Len(t, results, 2)
	assert.Equal(t, "LATTE-001", results[0].SKU)
}

func TestBulkAvailability_EmptySKUs(t *testing.T) {
	h := newTestHandler(t, &mockItemsSvc{}, &mockStockSvc{})
	r := newChiRouter(h)

	body, _ := json.Marshal(map[string][]string{"skus": {}})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/availability", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─── Stock Handler Tests ────────────────────────────────────────────────

func TestCreateReservation_Success(t *testing.T) {
	now := time.Now()
	stockMock := &mockStockSvc{
		createReservationFn: func(_ context.Context, tenantID uuid.UUID, req stock.ReservationRequest) (*stock.ReservationResponse, error) {
			return &stock.ReservationResponse{
				ID:       testReservationID,
				TenantID: tenantID,
				OrderID:  req.OrderID,
				Status:   "pending",
				Items: []stock.ReservedItem{
					{SKU: "LATTE-001", RequestedQty: 3, ReservedQty: 3, AvailableQty: 50, IsFullyReserved: true},
				},
				CreatedAt: now,
			}, nil
		},
	}

	h := newTestHandler(t, &mockItemsSvc{}, stockMock)
	r := newChiRouter(h)

	reqBody := stock.ReservationRequest{
		OrderID: testOrderID,
		Items:   []stock.ReservationItem{{SKU: "LATTE-001", Quantity: 3}},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/reservations", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp stock.ReservationResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, testReservationID, resp.ID)
	assert.Equal(t, "pending", resp.Status)
	assert.Len(t, resp.Items, 1)
	assert.True(t, resp.Items[0].IsFullyReserved)
}

func TestCreateReservation_MissingOrderID(t *testing.T) {
	h := newTestHandler(t, &mockItemsSvc{}, &mockStockSvc{})
	r := newChiRouter(h)

	reqBody := stock.ReservationRequest{
		Items: []stock.ReservationItem{{SKU: "LATTE-001", Quantity: 3}},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/reservations", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateReservation_MissingItems(t *testing.T) {
	h := newTestHandler(t, &mockItemsSvc{}, &mockStockSvc{})
	r := newChiRouter(h)

	reqBody := stock.ReservationRequest{
		OrderID: testOrderID,
		Items:   []stock.ReservationItem{},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/reservations", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetReservation_Success(t *testing.T) {
	stockMock := &mockStockSvc{
		getReservationFn: func(_ context.Context, tenantID, reservationID uuid.UUID) (*stock.ReservationResponse, error) {
			return &stock.ReservationResponse{
				ID:       reservationID,
				TenantID: tenantID,
				OrderID:  testOrderID,
				Status:   "pending",
				Items: []stock.ReservedItem{
					{SKU: "LATTE-001", RequestedQty: 3, ReservedQty: 3, AvailableQty: 50, IsFullyReserved: true},
				},
			}, nil
		},
	}

	h := newTestHandler(t, &mockItemsSvc{}, stockMock)
	r := newChiRouter(h)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/%s/inventory/reservations/%s", testTenantID, testReservationID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp stock.ReservationResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, testReservationID, resp.ID)
}

func TestReleaseReservation_Success(t *testing.T) {
	stockMock := &mockStockSvc{
		releaseReservationFn: func(_ context.Context, _, _ uuid.UUID, _ string) error {
			return nil
		},
	}

	h := newTestHandler(t, &mockItemsSvc{}, stockMock)
	r := newChiRouter(h)

	body, _ := json.Marshal(map[string]string{"reason": "customer cancelled"})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/reservations/%s/release", testTenantID, testReservationID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestConsumeReservation_Success(t *testing.T) {
	stockMock := &mockStockSvc{
		consumeReservationFn: func(_ context.Context, _, _ uuid.UUID) (*stock.ConsumeReservationResponse, error) {
			return &stock.ConsumeReservationResponse{Status: "consumed"}, nil
		},
	}

	h := newTestHandler(t, &mockItemsSvc{}, stockMock)
	r := newChiRouter(h)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/reservations/%s/consume", testTenantID, testReservationID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRecordConsumption_Success(t *testing.T) {
	stockMock := &mockStockSvc{
		recordConsumptionFn: func(_ context.Context, tenantID uuid.UUID, req stock.ConsumptionRequest) (*stock.ConsumptionResponse, error) {
			return &stock.ConsumptionResponse{
				ID:          uuid.New(),
				TenantID:    tenantID,
				OrderID:     req.OrderID,
				Status:      "processed",
				ProcessedAt: time.Now(),
			}, nil
		},
	}

	h := newTestHandler(t, &mockItemsSvc{}, stockMock)
	r := newChiRouter(h)

	reqBody := stock.ConsumptionRequest{
		OrderID: testOrderID,
		Items:   []stock.ConsumptionItem{{SKU: "LATTE-001", Quantity: 2.0}},
		Reason:  "sale",
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/consumption", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp stock.ConsumptionResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "processed", resp.Status)
}

func TestRecordConsumption_MissingOrderID(t *testing.T) {
	h := newTestHandler(t, &mockItemsSvc{}, &mockStockSvc{})
	r := newChiRouter(h)

	reqBody := stock.ConsumptionRequest{
		Items: []stock.ConsumptionItem{{SKU: "LATTE-001", Quantity: 2.0}},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/consumption", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRecordConsumption_MissingItems(t *testing.T) {
	h := newTestHandler(t, &mockItemsSvc{}, &mockStockSvc{})
	r := newChiRouter(h)

	reqBody := stock.ConsumptionRequest{
		OrderID: testOrderID,
		Items:   []stock.ConsumptionItem{},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/%s/inventory/consumption", testTenantID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
