package router

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	handlers "github.com/bengobox/inventory-service/internal/http/handlers"
	sharedmw "github.com/bengobox/inventory-service/internal/shared/middleware"
)

func New(log *zap.Logger, health *handlers.HealthHandler, authMiddleware *authclient.AuthMiddleware) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(sharedmw.RequestID)
	r.Use(sharedmw.Tenant)
	r.Use(sharedmw.Logging(log))
	r.Use(sharedmw.Recover(log))
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Tenant-ID", "X-Request-ID"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/healthz", health.Liveness)
	r.Get("/readyz", health.Readiness)
	r.Get("/metrics", health.Metrics)
	r.Get("/v1/docs/*", httpSwagger.WrapHandler)

	r.Route("/api/v1", func(api chi.Router) {
		// Apply auth middleware to all v1 routes
		if authMiddleware != nil {
			api.Use(authMiddleware.RequireAuth)
		}

		api.Route("/{tenantID}", func(tenant chi.Router) {
			tenant.Route("/inventory", func(inventory chi.Router) {
				// Placeholder endpoints - to be implemented
				inventory.Get("/items", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNotImplemented)
					w.Write([]byte("Not implemented yet"))
				})
			})
		})
	})

	return r
}
