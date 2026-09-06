package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/inventory-service/internal/audit"
	"github.com/bengobox/inventory-service/internal/config"
	"github.com/bengobox/inventory-service/internal/ent"
	"github.com/bengobox/inventory-service/internal/ent/migrate"
	handlers "github.com/bengobox/inventory-service/internal/http/handlers"
	router "github.com/bengobox/inventory-service/internal/http/router"
	"github.com/bengobox/inventory-service/internal/modules/approvals"
	backupmod "github.com/bengobox/inventory-service/internal/modules/backup"
	"github.com/bengobox/inventory-service/internal/modules/backup/destination"
	"github.com/bengobox/inventory-service/internal/modules/bulkjobs"
	"github.com/bengobox/inventory-service/internal/modules/bundles"
	"github.com/bengobox/inventory-service/internal/modules/consumers"
	"github.com/bengobox/inventory-service/internal/modules/documents"
	"github.com/bengobox/inventory-service/internal/modules/expiry"
	"github.com/bengobox/inventory-service/internal/modules/items"
	"github.com/bengobox/inventory-service/internal/modules/modifiers"
	notifmod "github.com/bengobox/inventory-service/internal/modules/notifications"
	"github.com/bengobox/inventory-service/internal/modules/rbac"
	"github.com/bengobox/inventory-service/internal/modules/recipes"
	"github.com/bengobox/inventory-service/internal/modules/reports"
	"github.com/bengobox/inventory-service/internal/modules/stock"
	"github.com/bengobox/inventory-service/internal/modules/tenant"
	"github.com/bengobox/inventory-service/internal/modules/tickets"
	"github.com/bengobox/inventory-service/internal/modules/transfers"
	"github.com/bengobox/inventory-service/internal/modules/units"
	"github.com/bengobox/inventory-service/internal/platform/cache"
	"github.com/bengobox/inventory-service/internal/platform/database"
	"github.com/bengobox/inventory-service/internal/platform/events"
	"github.com/bengobox/inventory-service/internal/platform/subscriptions"
	"github.com/bengobox/inventory-service/internal/platform/treasury"
	"github.com/bengobox/inventory-service/internal/services/usersync"
	"github.com/bengobox/inventory-service/internal/shared/logger"
)

// terminalJWTSecret returns the PIN/terminal JWT signing secret, falling back to the shared
// INTERNAL_SERVICE_KEY when TERMINAL_JWT_SECRET isn't set (mirrors pos-api / library-api) so
// warehouse desk/kiosk PIN login works out of the box across the platform.
func terminalJWTSecret(cfg *config.Config) string {
	if cfg.Auth.TerminalJWTSecret != "" {
		return cfg.Auth.TerminalJWTSecret
	}
	return cfg.Auth.APIKey
}

// newReadOnlyEntClient opens a separate Ent client against cfg.ReadOnlyURL (a read replica, via
// pgbouncer's inventory_ro alias in prod) for the read-routing described where ormClient/
// readOrmClient are built in New(). A smaller pool than the primary's — this backs a handful of
// specific endpoints (ListItems' catalog fetch), not general traffic. No migration/schema
// management here; the replica streams the primary's schema.
func newReadOnlyEntClient(cfg config.PostgresConfig) (*ent.Client, error) {
	sqlDB, err := sql.Open("pgx", cfg.ReadOnlyURL)
	if err != nil {
		return nil, fmt.Errorf("sql open for read-replica ent client: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("read-replica ping: %w", err)
	}
	maxOpen := cfg.MaxOpenConns / 2
	if maxOpen < 2 {
		maxOpen = 2
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	return ent.NewClient(ent.Driver(drv)), nil
}

type App struct {
	cfg                           *config.Config
	log                           *zap.Logger
	httpServer                    *http.Server
	db                            *pgxpool.Pool
	cache                         *redis.Client
	events                        *nats.Conn
	orm                           *ent.Client
	outboxPublisher               *eventslib.OutboxPoller
	posSaleConsumer               *consumers.POSSaleEventsConsumer
	conferenceConsumer            *consumers.ConferenceEventsConsumer
	authConsumer                  *consumers.AuthEventsConsumer
	returnConsumer                *consumers.ReturnEventsConsumer
	stockConsumer                 *consumers.StockEventsConsumer
	ticketConsumer                *consumers.TicketIssuanceConsumer
	treasuryTaxConsumer           *consumers.TreasuryTaxEventsConsumer
	treasuryVendorBalanceConsumer *consumers.TreasuryVendorBalanceEventsConsumer
	etimsItemConsumer             *consumers.EtimsItemRegisteredConsumer
	tenantPurgeConsumer           *consumers.TenantPurgeConsumer
	quotationConsumer             *consumers.QuotationAcceptedConsumer
	deliveryNoteConsumer          *consumers.DeliveryNoteDispatchedConsumer
	notifHub                      *notifmod.Hub
	stockNotifyConsumer           *consumers.StockNotifyEventsConsumer
}

func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	log, err := logger.New(cfg.App.Env)
	if err != nil {
		return nil, fmt.Errorf("logger init: %w", err)
	}

	dbPool, err := database.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("postgres init: %w", err)
	}

	redisClient := cache.NewClient(cfg.Redis)

	natsConn, err := events.Connect(cfg.Events)
	if err != nil {
		log.Warn("event bus connection failed", zap.Error(err))
	}

	// Ensure inventory JetStream stream exists (for stock events consumed by notifications-api etc.)
	if natsConn != nil {
		if streamErr := events.EnsureStream(ctx, natsConn, cfg.Events); streamErr != nil {
			log.Warn("failed to ensure inventory stream", zap.Error(streamErr))
		}
	}

	healthHandler := handlers.NewHealthHandler(log, dbPool, redisClient, natsConn)

	// Initialize user management services (placeholder — real wiring after Ent client)
	syncService := usersync.NewService(cfg.Auth.ServiceURL, cfg.Auth.APIKey, log)

	// Initialize Ent ORM client
	sqlDB, err := sql.Open("pgx", cfg.Postgres.URL)
	if err != nil {
		return nil, fmt.Errorf("ent driver init: %w", err)
	}
	sqlDB.SetMaxIdleConns(cfg.Postgres.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.Postgres.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	ormClient := ent.NewClient(ent.Driver(drv))

	// readOrmClient points ListItems' heavy catalog fetch (search/list — POS terminal, Add Sale,
	// catalog browse) at a read replica instead of the primary, via cfg.Postgres.ReadOnlyURL
	// (pgbouncer's inventory_ro alias in prod — see devops-k8s). Unset (every environment that
	// hasn't been explicitly given the env var, incl. local dev) falls back to ormClient itself —
	// zero behavior change. A replica connection failure at startup is logged and swallowed, not
	// fatal: ListItems just keeps using the primary, same as before this existed.
	readOrmClient := ormClient
	if cfg.Postgres.ReadOnlyURL != "" {
		if roClient, err := newReadOnlyEntClient(cfg.Postgres); err != nil {
			log.Warn("read-replica postgres connection failed — read-heavy endpoints will use the primary", zap.Error(err))
		} else {
			readOrmClient = roClient
		}
	}

	// Run versioned migrations only when explicitly enabled.
	// In production, migrations are run by the entrypoint (cmd/migrate) before the server starts —
	// this inline path is a dev/local convenience only. Same WithDropColumn/WithDropIndex flags as
	// cmd/migrate/main.go for dev-parity: without them ent's live diff silently skips any
	// index/column removal a struct change implies (see that file's comment for the incident this
	// closed), and a dev testing locally with RunMigrations=true should see the same drop behavior
	// production's real migrate job does.
	if cfg.Postgres.RunMigrations {
		if err := ormClient.Schema.Create(ctx,
			schema.WithDir(migrate.Dir),
			schema.WithDropColumn(true),
			schema.WithDropIndex(true),
		); err != nil {
			return nil, fmt.Errorf("ent schema create: %w", err)
		}
		log.Info("versioned migrations applied (POSTGRES_RUN_MIGRATIONS=true)")
	}

	// Initialize outbox background publisher (Transactional Outbox Pattern)
	var outboxPublisher *eventslib.OutboxPoller
	if natsConn != nil && cfg.Events.OutboxEnabled {
		outboxRepo := eventslib.NewSQLOutboxRepository(sqlDB)
		outboxNatsPublisher := eventslib.NewNATSAdapter(natsConn, log)
		outboxCfg := eventslib.PollerConfig{
			BatchSize:  cfg.Events.OutboxBatchSize,
			PollPeriod: cfg.Events.OutboxPollPeriod,
		}
		outboxPublisher = eventslib.NewOutboxPoller(outboxRepo, outboxNatsPublisher, log, outboxCfg)
		outboxPublisher.Start(ctx)
		log.Info("outbox background publisher started",
			zap.Int("batch_size", cfg.Events.OutboxBatchSize),
			zap.Duration("poll_period", cfg.Events.OutboxPollPeriod))
	}

	// Initialize cache helper for read-heavy queries
	cacheAside := sharedcache.New(redisClient, log)

	// Initialize RBAC module (DB-backed, replaces in-memory stub)
	rbacRepo := rbac.NewEntRepository(ormClient)
	tenantSyncer := tenant.NewSyncer(ormClient, cfg.Auth.ServiceURL).WithDB(sqlDB)
	rbacService := rbac.NewService(rbacRepo, log, tenantSyncer)
	userHandler := handlers.NewUserHandler(log, rbacService, syncService, rbacRepo)
	rbacHandler := handlers.NewRBACHandler(log, rbacService, syncService, rbacRepo)
	authHandler := handlers.NewAuthHandler(log, rbacService, ormClient, cfg.Auth.ServiceURL, cfg.Auth.APIKey)

	// Initialize business modules
	itemsSvc := items.NewService(ormClient, log, cfg.Media.URLBase)
	// ListItems' heavy catalog fetch routed to a read replica when configured — see readOrmClient.
	itemsSvc.SetReadClient(readOrmClient)
	itemsSvc.SetCache(cacheAside)
	itemsSvc.SetMediaRoot(cfg.Media.Root) // persist multi-image uploads under MEDIA_ROOT
	// Treasury is the source of truth for tax codes/rates; resolve + cache VAT rates for item
	// enrichment, and expose the cached tax-code list to inventory-ui for the tax-code picker.
	treasuryClient := treasury.NewClient(cfg.Services.TreasuryURL, cfg.Auth.APIKey, cacheAside, log)
	itemsSvc.SetTaxResolver(treasuryClient)
	stockSvc := stock.NewService(ormClient, log)
	recipeSvc := recipes.NewService(ormClient, log).WithItemsService(itemsSvc)
	unitSvc := units.NewService(ormClient, log)
	modifiersSvc := modifiers.NewService(ormClient, log)
	// WithStockCascade: transfer ship/receive/cancel moves stock directly via their own
	// adjustBalance, bypassing AdjustStock — wiring stockSvc here makes those moves trigger the
	// exact same real-time downstream sync (POS/ordering catalog overrides, inventory-ui's live
	// push, low-stock alerts, recipe cascades) as every other stock mutation path.
	transferSvc := transfers.NewService(ormClient, log).WithStockCascade(stockSvc)
	ticketsSvc := tickets.NewService(ormClient, log)
	inventoryHandler := handlers.NewInventoryHandler(log, itemsSvc, stockSvc, recipeSvc, unitSvc)
	inventoryHandler.SetModifiersService(modifiersSvc)
	inventoryHandler.SetTicketsService(ticketsSvc)
	inventoryHandler.SetRBACService(rbacService)
	inventoryHandler.SetEntClient(ormClient) // for RequireOutletUseCase warehouse use_case lookup
	inventoryHandler.SetApprovalService(approvals.NewService(ormClient))
	warehouseHandler := handlers.NewWarehouseHandler(log, ormClient, rbacService)
	warehouseHandler.SetAuthURL(cfg.Auth.ServiceURL)
	auditSvc := audit.NewService(ormClient, log)
	warehouseHandler.SetAuditService(auditSvc)
	inventoryHandler.SetAuditService(auditSvc)
	itemsSvc.SetAuditService(auditSvc) // standard-cost / selling-price change audit trail
	stockCountHandler := handlers.NewStockCountHandler(log, ormClient, stockSvc, rbacService, auditSvc)
	stockCountHandler.SetApprovalService(approvals.NewService(ormClient))
	warehouseLocationHandler := handlers.NewWarehouseLocationHandler(log, ormClient, rbacService)
	pricingTierHandler := handlers.NewPricingTierHandler(log, ormClient, rbacService)
	pricingTierHandler.SetAuditService(auditSvc)
	pricingTierHandler.SetItemsService(itemsSvc)
	brandHandler := handlers.NewBrandHandler(log, ormClient, rbacService)
	transferHandler := handlers.NewTransferHandler(log, transferSvc, rbacService, approvals.NewService(ormClient))
	inventoryExtrasHandler := handlers.NewInventoryExtrasHandler(log, ormClient, rbacService)
	bundleSvc := bundles.NewService(ormClient, log)
	inventoryExtrasHandler.SetBundleService(bundleSvc)
	varianceSvc := recipes.NewVarianceService(ormClient, log, cfg.Services.OrderingURL, cfg.Services.POSURL, cfg.Auth.APIKey)
	inventoryExtrasHandler.SetVarianceService(varianceSvc)
	menuEngSvc := recipes.NewMenuEngineeringService(ormClient, log, cfg.Services.OrderingURL, cfg.Services.POSURL, cfg.Auth.APIKey)
	inventoryExtrasHandler.SetMenuEngineeringService(menuEngSvc)
	reportsSvc := reports.NewService(ormClient, log)
	inventoryExtrasHandler.SetReportsService(reportsSvc)
	docSvc := documents.NewService(ormClient, cacheAside, cfg.Auth.ServiceURL, log)
	transferSvc.WithSequence(docSvc.Seq())    // numeric-by-default transfer numbers via document sequence
	itemsSvc.SetSequenceService(docSvc.Seq()) // numeric-by-default SKUs via document sequence; legacy per-category prefix mode when a tenant has one configured
	transferHandler.SetDocService(docSvc)     // Dispatch/Transit Note + Goods-Received Note PDFs
	stockSvc.WithTransferRecorder(transferSvc)
	inventoryExtrasHandler.SetDocService(docSvc)
	inventoryHandler.SetDocService(docSvc)  // branded event-ticket PDFs (with QR) + stock-adjustment notes
	stockCountHandler.SetDocService(docSvc) // branded count sheet / variance report PDFs + count numbering
	inventoryExtrasHandler.SetStockService(stockSvc)
	inventoryExtrasHandler.SetAuditService(auditSvc) // goods-receipt cost-capture audit trail
	inventoryExtrasHandler.SetItemsService(itemsSvc)
	// Optional automated month-end depreciation (opt-in; depreciation_rate must be a
	// per-month fraction when enabled — see StartDepreciationScheduler). Off by default;
	// most tenants run depreciation manually at period close.
	if os.Getenv("ASSET_DEPRECIATION_SCHEDULER") == "true" {
		go inventoryExtrasHandler.StartDepreciationScheduler(ctx)
		log.Info("asset depreciation scheduler enabled")
	}
	analyticsHandler := handlers.NewAnalyticsHandler(log, ormClient)
	analyticsHandler.SetItemsService(itemsSvc) // outlet-scoped dashboard figures must match the Products list
	handlers.SetTenantDB(ormClient)            // Enable local slug-to-UUID lookups
	handlers.SetTenantSyncer(tenantSyncer)     // Enable slug-to-UUID resolution via auth-api

	// Subscriptions S2S client + gate: restrict cross-service stock sync (ordering/POS →
	// inventory) to tenants entitled to basic_inventory_access. Cached per tenant; fails open
	// on a subscriptions-api outage so a downtime never silently halts stock movements.
	subsClient := subscriptions.NewClient(subscriptions.Config{
		ServiceURL:     cfg.Subscriptions.ServiceURL,
		APIKey:         cfg.Subscriptions.APIKey,
		RequestTimeout: cfg.Subscriptions.RequestTimeout,
	}, log.Named("subscriptions.client"))
	consumerFeatureGate := func(ctx context.Context, tenantID, feature string) bool {
		return subsClient.ConsumerHasFeature(ctx, tenantID, feature)
	}

	// POS sale events consumer — consume stock on pos.sale.finalized (with BOM explosion)
	posSaleConsumer := consumers.NewPOSSaleEventsConsumer(log, stockSvc, ormClient)
	posSaleConsumer.SetFeatureGate(consumerFeatureGate)
	conferenceConsumer := consumers.NewConferenceEventsConsumer(log, stockSvc, ormClient)
	ticketConsumer := consumers.NewTicketIssuanceConsumer(log, ticketsSvc, ormClient)

	// Auth events consumer — proactive user sync + UserOutlet assignment projection from auth-service
	authConsumer := consumers.NewAuthEventsConsumer(log, rbacService, ormClient, cacheAside)

	// Return events consumer — restock inventory on pos.return.completed + ordering.return.approved
	returnConsumer := consumers.NewReturnEventsConsumer(log, stockSvc)

	// Stock low events consumer — auto-creates draft POs when auto_reorder_enabled
	stockConsumer := consumers.NewStockEventsConsumer(log, ormClient)

	// Real-time push hub + bridge consumer: inventory-ui connects to notifHub's WebSocket to learn
	// about stock changes live (POS sale consumption, manual adjustment, stock-take) instead of on
	// a manual refresh. Redis relay makes broadcasts reach clients on any replica.
	notifHub := notifmod.NewHub(log)
	notifHub.SetRedis(redisClient)
	stockNotifyConsumer := consumers.NewStockNotifyEventsConsumer(log, notifHub)

	// Background bulk-job runner (item relocation/membership, bulk stock adjustment) — reuses
	// notifHub to push bulk_job.completed the moment a queued job finishes.
	bulkJobsSvc := bulkjobs.NewService(ormClient, log, notifHub)
	inventoryHandler.SetBulkJobsService(bulkJobsSvc)

	// Procure-to-order consumer — on an ACCEPTED treasury sales quotation, auto-creates a draft
	// PurchaseOrder to buy the quoted items at their buying (cost) price. Gated by entitlement (fail-open).
	quotationConsumer := consumers.NewQuotationAcceptedConsumer(log, ormClient)
	quotationConsumer.SetFeatureGate(consumerFeatureGate)

	// Goods-issue consumer — on a DISPATCHED treasury delivery note, deducts the dispatched
	// quantities from the issuing warehouse via the canonical stock-adjustment path (auditable
	// StockAdjustment + balance decrement + lot drawdown). Idempotent on delivery_note_id; gated
	// by entitlement (fail-open).
	deliveryNoteConsumer := consumers.NewDeliveryNoteDispatchedConsumer(log, stockSvc, ormClient)
	deliveryNoteConsumer.SetFeatureGate(consumerFeatureGate)

	// Treasury tax-code change consumer — invalidates cached tax data so rate changes propagate immediately
	treasuryTaxConsumer := consumers.NewTreasuryTaxEventsConsumer(log, treasuryClient)

	// Treasury vendor-balance-updated consumer — closes the one-way sync gap where a bill
	// payment/vendor refund recorded directly in treasury-ui never reached inventory-api.
	treasuryVendorBalanceConsumer := consumers.NewTreasuryVendorBalanceEventsConsumer(log, ormClient)

	// eTIMS item-registered write-back consumer — mirrors KRA-assigned classification/pkg/qty
	// codes onto the inventory item so the Edit form reflects the item's real synced state.
	etimsItemConsumer := consumers.NewEtimsItemRegisteredConsumer(log, itemsSvc)

	// Tenant purge consumer — on platform-owner-confirmed dormancy purge (tenant.purge),
	// IRREVERSIBLY deletes ALL of the tenant's inventory data. Uses the raw *sql.DB for
	// FK-order-independent, transactional deletes.
	tenantPurgeConsumer := consumers.NewTenantPurgeConsumer(log, sqlDB)

	// Initialize auth-service JWT validator
	var authMiddleware *authclient.AuthMiddleware
	authConfig := authclient.DefaultConfig(
		cfg.Auth.JWKSUrl,
		cfg.Auth.Issuer,
		cfg.Auth.Audience,
	)
	authConfig.CacheTTL = cfg.Auth.JWKSCacheTTL
	authConfig.RefreshInterval = cfg.Auth.JWKSRefreshInterval

	validator, err := authclient.NewValidator(authConfig)
	if err != nil {
		return nil, fmt.Errorf("auth validator init: %w", err)
	}

	if cfg.Auth.EnableAPIKeyAuth {
		apiKeyValidator := authclient.NewAPIKeyValidator(cfg.Auth.ServiceURL, nil)
		authMiddleware = authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
	} else {
		authMiddleware = authclient.NewAuthMiddleware(validator)
	}

	// Initialize NATS event subscribers for proactive provisioning
	branchSub := tenant.NewBranchSubscriber(ormClient, log)
	if err := branchSub.Start(natsConn); err != nil {
		log.Warn("app: failed to start outlet event subscriptions", zap.Error(err))
	}
	warehouseHandler.SetOutletSyncer(branchSub)

	// Startup reconciliation: catch any outlet events missed while the pod was down.
	// Runs once 15 seconds after startup so NATS consumers are warm first.
	authURLForSync := strings.TrimRight(cfg.Auth.ServiceURL, "/")
	go func() {
		select {
		case <-time.After(15 * time.Second):
		case <-ctx.Done():
			return
		}
		if err := branchSub.ReconcileFromAuthAPI(ctx, authURLForSync, ""); err != nil {
			log.Warn("app: outlet startup reconciliation failed", zap.Error(err))
		}
	}()

	if natsConn != nil {
		subCacheSub := subscriptions.NewCacheSubscriber(redisClient, log)
		subCacheSub.SetNotifHub(notifHub)
		if err := subCacheSub.Start(natsConn); err != nil {
			log.Warn("app: failed to start subscription cache subscriber", zap.Error(err))
		}
	}

	// Initialize media handler for file uploads
	var mediaHandler *handlers.MediaHandler
	if cfg.Media.Root != "" {
		mediaHandler = handlers.NewMediaHandler(log, cfg.Media)
	}

	// Initialize service config handler for platform admin + tenant settings
	serviceConfigHandler := handlers.NewServiceConfigHandler(ormClient, log)

	// Typed tenant inventory config (thresholds, module toggles, tracking settings)
	inventorySettingsHandler := handlers.NewInventorySettingsHandler(log, ormClient)
	inventorySettingsHandler.SetRBACService(rbacService)
	inventorySettingsHandler.SetTreasuryClient(treasuryClient)

	// Aging Stock report + Start/Cancel Clearance (2026-09-06 pricing/tiering plan, Phase 2)
	stockClearanceHandler := handlers.NewStockClearanceHandler(log, itemsSvc, inventorySettingsHandler)
	stockClearanceHandler.SetRBACService(rbacService)

	// Tenant-scoped backups + daily 02:00 auto-backup scheduler + retention churn.
	backupSvc := backupmod.NewService(sqlDB, ormClient, cfg.Backup.Dir, log)
	// Pluggable backup destination (PVC primary + best-effort rclone remote mirror).
	// Secret backend params are encrypted at rest with a SECRET_KEY-derived AES key.
	backupDestHandler := handlers.NewBackupDestinationHandler(ormClient, destination.NewSecretKeyCipher(), rbacService, log)
	// Mirror every freshly-written PVC backup to the resolved remote, best-effort.
	backupSvc = backupSvc.WithMirrorer(backupDestHandler.Uploader())
	backupsHandler := handlers.NewBackups(log, backupSvc, rbacService, cfg.Backup.RetentionDays)
	backupmod.NewScheduler(backupSvc, backupmod.SchedulerConfig{
		Enabled:       cfg.Backup.ScheduleEnabled,
		Hour:          cfg.Backup.ScheduleHour,
		RetentionDays: cfg.Backup.RetentionDays,
	}, log).Start(ctx)

	// End-of-Life purge: hard-delete items whose EOL retention window has elapsed (audit-safe:
	// items with transactional history are skipped and kept hidden). Advisory-lock guarded so
	// only one replica runs it; tenant-generic.
	items.NewEOLPurgeScheduler(itemsSvc, sqlDB, items.EOLPurgeConfig{
		Enabled:       cfg.EOL.PurgeEnabled,
		RetentionDays: cfg.EOL.RetentionDays,
	}, log).Start(ctx)

	// Pharmacy (DAWA) lot-expiry alerts: consumes TenantInventoryConfig.expiry_warning_days /
	// enable_expiry_notifications, previously stored but never read by anything.
	expirySvc := expiry.NewService(ormClient, log)
	expiry.NewScheduler(expirySvc, sqlDB, expiry.SchedulerConfig{
		Enabled: cfg.ExpiryAlert.ScheduleEnabled,
	}, log).Start(ctx)

	// Terminal/PIN login: issues + validates HMAC terminal JWTs for warehouse desk sessions.
	pinAuthHandler := handlers.NewPINAuthHandler(ormClient, rbacService, subsClient, terminalJWTSecret(cfg), log)

	notificationsStreamHandler := handlers.NewNotificationsStreamHandler(log, notifHub)

	chiRouter := router.New(log, healthHandler, userHandler, inventoryHandler, warehouseHandler, warehouseLocationHandler, pricingTierHandler, brandHandler, transferHandler, inventoryExtrasHandler, analyticsHandler, rbacHandler, authHandler, authMiddleware, tenantSyncer, rbacService, cfg.HTTP.AllowedOrigins, mediaHandler, cfg.Media.Root, serviceConfigHandler, inventorySettingsHandler, redisClient, ormClient, stockCountHandler, backupsHandler, backupDestHandler, pinAuthHandler, cfg.Auth.APIKey, notificationsStreamHandler, stockClearanceHandler)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           chiRouter,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:                           cfg,
		log:                           log,
		httpServer:                    httpServer,
		db:                            dbPool,
		cache:                         redisClient,
		events:                        natsConn,
		orm:                           ormClient,
		outboxPublisher:               outboxPublisher,
		posSaleConsumer:               posSaleConsumer,
		conferenceConsumer:            conferenceConsumer,
		authConsumer:                  authConsumer,
		returnConsumer:                returnConsumer,
		stockConsumer:                 stockConsumer,
		ticketConsumer:                ticketConsumer,
		treasuryTaxConsumer:           treasuryTaxConsumer,
		treasuryVendorBalanceConsumer: treasuryVendorBalanceConsumer,
		etimsItemConsumer:             etimsItemConsumer,
		tenantPurgeConsumer:           tenantPurgeConsumer,
		quotationConsumer:             quotationConsumer,
		deliveryNoteConsumer:          deliveryNoteConsumer,
		notifHub:                      notifHub,
		stockNotifyConsumer:           stockNotifyConsumer,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	// Start auth events consumer for proactive user sync from auth-service
	if a.authConsumer != nil && a.events != nil {
		if err := a.authConsumer.Start(ctx, a.events); err != nil {
			a.log.Warn("auth events consumer not started", zap.Error(err))
		} else {
			a.log.Info("auth events consumer started")
		}
	}

	// Ordering->inventory stock is reconciled via direct S2S calls (consumeOrderReservation on
	// completion / releaseOrderReservation on cancellation in ordering-backend — the single,
	// intended deduction path), NOT via order lifecycle events. There is deliberately no
	// event-based order consumer: it never received events (ordering writes its order_events to a
	// DB audit table, not NATS) and, if wired, would double-deduct against the S2S path.
	if a.events != nil {
		js, err := a.events.JetStream()
		if err != nil {
			a.log.Warn("jetstream unavailable, downstream event consumers not started", zap.Error(err))
		} else {
			// Start POS sale events consumer
			if a.posSaleConsumer != nil {
				go func() {
					if err := a.posSaleConsumer.Start(ctx, js); err != nil {
						a.log.Error("pos sale events consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("pos sale events consumer started")
			}

			// Start conference meal-card redemption consumer (backflush meal BOM)
			if a.conferenceConsumer != nil {
				go func() {
					if err := a.conferenceConsumer.Start(ctx, js); err != nil {
						a.log.Error("conference events consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("conference meal-card events consumer started")
			}

			// Start ticket issuance consumer (ordering.order.payment_confirmed -> issue event tickets)
			if a.ticketConsumer != nil {
				go func() {
					if err := a.ticketConsumer.Start(ctx, js); err != nil {
						a.log.Error("ticket issuance consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("ticket issuance consumer started")
			}

			// Start return events consumers (pos.return.completed + ordering.return.approved)
			if a.returnConsumer != nil {
				go func() {
					if err := a.returnConsumer.StartPOSReturns(ctx, js); err != nil {
						a.log.Error("pos return events consumer stopped", zap.Error(err))
					}
				}()
				go func() {
					if err := a.returnConsumer.StartOrderingReturns(ctx, js); err != nil {
						a.log.Error("ordering return events consumer stopped", zap.Error(err))
					}
				}()
				go func() {
					if err := a.returnConsumer.StartExchangeReturns(ctx, js); err != nil {
						a.log.Error("pos exchange events consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("return events consumers started")
			}

			// Start stock low events consumer — auto-creates draft POs when auto_reorder_enabled
			if a.stockConsumer != nil {
				go func() {
					if err := a.stockConsumer.Start(ctx, js); err != nil {
						a.log.Error("stock events consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("stock events consumer started")
			}

			// Start stock-notify consumer — bridges stock.updated/low/out to the real-time
			// WebSocket push for inventory-ui (separate concern/durable from the auto-PO consumer
			// above).
			if a.stockNotifyConsumer != nil {
				go func() {
					if err := a.stockNotifyConsumer.Start(ctx, js); err != nil {
						a.log.Error("stock notify events consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("stock notify events consumer started")
			}

			// Start treasury tax-code change consumer — invalidates cached treasury tax data
			if a.treasuryTaxConsumer != nil {
				go func() {
					if err := a.treasuryTaxConsumer.Start(ctx, js); err != nil {
						a.log.Error("treasury tax events consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("treasury tax events consumer started")
			}

			// Start treasury vendor-balance-updated consumer — keeps VendorBalanceCache fresh.
			if a.treasuryVendorBalanceConsumer != nil {
				go func() {
					if err := a.treasuryVendorBalanceConsumer.Start(ctx, js); err != nil {
						a.log.Error("treasury vendor balance events consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("treasury vendor balance events consumer started")
			}

			// Start eTIMS item-registered write-back consumer — mirrors KRA codes onto items.
			if a.etimsItemConsumer != nil {
				go func() {
					if err := a.etimsItemConsumer.Start(ctx, js); err != nil {
						a.log.Error("etims item-registered consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("etims item-registered write-back consumer started")
			}

			// Start tenant purge consumer — IRREVERSIBLY deletes a tenant's data on a
			// platform-owner-confirmed dormancy purge (tenant.purge).
			if a.tenantPurgeConsumer != nil {
				go func() {
					if err := a.tenantPurgeConsumer.Start(ctx, js); err != nil {
						a.log.Error("tenant purge consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("tenant purge consumer started")
			}

			// Start procure-to-order consumer — on an accepted treasury sales quotation,
			// auto-creates a draft PO to buy the quoted items at their buying cost.
			if a.quotationConsumer != nil {
				go func() {
					if err := a.quotationConsumer.Start(ctx, js); err != nil {
						a.log.Error("quotation accepted (procure-to-order) consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("quotation accepted (procure-to-order) consumer started")
			}

			// Start goods-issue consumer — on a dispatched treasury delivery note, deducts the
			// dispatched quantities from the issuing warehouse's stock (auditable adjustments).
			if a.deliveryNoteConsumer != nil {
				go func() {
					if err := a.deliveryNoteConsumer.Start(ctx, js); err != nil {
						a.log.Error("delivery note dispatched (goods-issue) consumer stopped", zap.Error(err))
					}
				}()
				a.log.Info("delivery note dispatched (goods-issue) consumer started")
			}
		}
	}

	// Start the real-time notification hub's Redis cross-pod relay — no-op (single-pod only) if
	// Redis is unavailable.
	if a.notifHub != nil {
		go a.notifHub.Start(ctx)
	}

	errCh := make(chan error, 1)
	if a.cfg.HTTP.TLSCertFile != "" && a.cfg.HTTP.TLSKeyFile != "" {
		a.log.Info("inventory service starting with HTTPS",
			zap.String("addr", a.httpServer.Addr),
			zap.String("cert", a.cfg.HTTP.TLSCertFile),
			zap.String("key", a.cfg.HTTP.TLSKeyFile),
		)
		go func() {
			errCh <- a.httpServer.ListenAndServeTLS(a.cfg.HTTP.TLSCertFile, a.cfg.HTTP.TLSKeyFile)
		}()
	} else {
		a.log.Info("inventory service starting with HTTP", zap.String("addr", a.httpServer.Addr))
		go func() {
			errCh <- a.httpServer.ListenAndServe()
		}()
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}

		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server error: %w", err)
	}
}

func (a *App) Close() {
	// Stop outbox publisher first (before NATS connection)
	if a.outboxPublisher != nil {
		a.outboxPublisher.Stop()
		a.log.Info("outbox publisher stopped")
	}

	if a.events != nil {
		if err := a.events.Drain(); err != nil {
			a.log.Warn("nats drain failed", zap.Error(err))
		}
		a.events.Close()
	}

	if a.cache != nil {
		if err := a.cache.Close(); err != nil {
			a.log.Warn("redis close failed", zap.Error(err))
		}
	}

	if a.db != nil {
		a.db.Close()
	}

	if a.orm != nil {
		if err := a.orm.Close(); err != nil {
			a.log.Warn("ent client close failed", zap.Error(err))
		}
	}

	_ = a.log.Sync()
}
