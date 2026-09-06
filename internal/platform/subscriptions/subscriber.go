package subscriptions

import (
	"context"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	notifmod "github.com/bengobox/inventory-service/internal/modules/notifications"
)

// CacheSubscriber listens for tenant.subscription.updated events (plan changes, suspensions,
// and — since 2026-09-07 — TenantFeatureGrant add-on grants/revokes) and (1) invalidates the
// shared tenant branding/metadata cache so downstream reads pick up new plan data, and (2) pushes
// a real-time "entitlements_changed" nudge over the same notification hub inventory-ui already
// holds open for stock/catalog pushes, so an already-open browser tab reflects a revoke/grant/
// plan-change within seconds instead of only on its next full page load. Without this, a tenant
// admin revoking an add-on (e.g. multi_branch_pricing) sees the change reflected on THEIR OWN
// screen, but any other already-open tenant session keeps showing the stale entitlement until
// that tab is reloaded — see the per-outlet-pricing plan's Phase 6 notes for the live report.
type CacheSubscriber struct {
	redis    *redis.Client
	logger   *zap.Logger
	sub      *nats.Subscription
	notifHub *notifmod.Hub
}

// NewCacheSubscriber creates a CacheSubscriber.
func NewCacheSubscriber(redisClient *redis.Client, logger *zap.Logger) *CacheSubscriber {
	return &CacheSubscriber{
		redis:  redisClient,
		logger: logger.Named("subscriptions.cache-subscriber"),
	}
}

// SetNotifHub wires the real-time push hub (optional — nil degrades to cache-invalidation-only,
// the pre-existing behavior). Call before Start.
func (s *CacheSubscriber) SetNotifHub(hub *notifmod.Hub) { s.notifHub = hub }

// Start subscribes to tenant.subscription.updated on the provided NATS connection.
func (s *CacheSubscriber) Start(conn *nats.Conn) error {
	sub, err := eventslib.QueueSubscribe(s.logger, conn, "tenant.subscription.updated", "inventory-subcache", s.handle)
	if err != nil {
		return err
	}
	s.sub = sub
	s.logger.Info("subscribed to tenant.subscription.updated")
	return nil
}

// Stop drains the NATS subscription.
func (s *CacheSubscriber) Stop() {
	if s.sub != nil {
		_ = s.sub.Drain()
	}
}

func (s *CacheSubscriber) handle(msg *nats.Msg) {
	evt, err := eventslib.FromJSON(msg.Data)
	if err != nil {
		s.logger.Warn("failed to parse subscription.updated event", zap.Error(err))
		return
	}

	slug := evt.TenantSlug
	if slug == "" {
		if v, ok := evt.Payload["tenant_slug"].(string); ok {
			slug = v
		}
	}
	if slug == "" {
		s.logger.Warn("subscription.updated event missing tenant_slug, skipping")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cacheKey := "tenant:" + slug
	if err := s.redis.Del(ctx, cacheKey).Err(); err != nil {
		s.logger.Warn("failed to invalidate tenant cache",
			zap.String("key", cacheKey),
			zap.Error(err),
		)
	} else {
		s.logger.Debug("invalidated tenant cache on subscription update", zap.String("key", cacheKey))
	}

	// Real-time push: nudge every open browser tab for this tenant to refetch its entitlements.
	// Best-effort — a missing/nil TenantID (a malformed or legacy event) just skips the push,
	// same fail-open posture as the rest of this best-effort cache-invalidation subscriber.
	if s.notifHub != nil && evt.TenantID != uuid.Nil {
		s.notifHub.BroadcastToTenant(evt.TenantID, notifmod.Message{
			Type:    "entitlements_changed",
			Payload: map[string]any{"tenant_id": evt.TenantID},
		})
	}
}
