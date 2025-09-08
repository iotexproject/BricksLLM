package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/bricks-cloud/bricksllm/internal/event"
	"github.com/bricks-cloud/bricksllm/internal/key"
	"github.com/bricks-cloud/bricksllm/internal/policy"
	"github.com/bricks-cloud/bricksllm/internal/provider"
	"github.com/bricks-cloud/bricksllm/internal/provider/custom"
	"github.com/bricks-cloud/bricksllm/internal/telemetry"
	"github.com/bricks-cloud/bricksllm/internal/util"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type ProviderSettingsManager interface {
	CreateSetting(setting *provider.Setting) (*provider.Setting, error)
	UpdateSetting(id string, setting *provider.UpdateSetting) (*provider.Setting, error)
	GetSettingViaCache(id string) (*provider.Setting, error)
	GetSettingsViaCache(ids []string) ([]*provider.Setting, error)
}

type KeyManager interface {
	GetKeys(tags, keyIds []string, provider string) ([]*key.ResponseKey, error)
	GetKeysV2(tags, keyIds []string, revoked *bool, limit, offset int, name, order string, returnCount bool) (*key.GetKeysResponse, error)
	UpdateKey(id string, key *key.UpdateKey) (*key.ResponseKey, error)
	CreateKey(key *key.RequestKey) (*key.ResponseKey, error)
	DeleteKey(id string) error
}

type KeyReportingManager interface {
	GetTopKeyReporting(r *event.KeyReportingRequest) (*event.KeyReportingResponse, error)
	GetKeyReporting(keyId string) (*key.KeyReporting, error)
	GetEvents(userId, customId string, keyIds []string, start int64, end int64) ([]*event.Event, error)
	GetEventsV2(r *event.EventRequest) (*event.EventResponse, error)
	GetEventReporting(e *event.ReportingRequest) (*event.ReportingResponse, error)
	GetAggregatedEventByDayReporting(e *event.ReportingRequest) (*event.ReportingResponseV2, error)
	GetCustomIds(keyId string) ([]string, error)
	GetUserIds(keyId string) ([]string, error)
}

type PoliciesManager interface {
	CreatePolicy(p *policy.Policy) (*policy.Policy, error)
	UpdatePolicy(id string, p *policy.UpdatePolicy) (*policy.Policy, error)
	GetPoliciesByTags(tags []string) ([]*policy.Policy, error)
}

type ErrorResponse struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

type AdminServer struct {
	server *http.Server
	log    *zap.Logger
	m      KeyManager
}

func NewAdminServer(log *zap.Logger, mode string, m KeyManager, krm KeyReportingManager, psm ProviderSettingsManager, cpm CustomProvidersManager, rm RouteManager, pm PoliciesManager, um UserManager, adminPass string) (*AdminServer, error) {
	router := gin.New()

	prod := mode == "production"
	router.Use(getAdminLoggerMiddleware(log, "admin", prod, adminPass))

	router.GET("/api/health", getGetHealthCheckHandler())

	router.POST("/api/v2/key-management/keys", getGetKeysV2Handler(m, prod))
	router.GET("/api/key-management/keys", getGetKeysHandler(m, prod))
	router.PUT("/api/key-management/keys", getCreateKeyHandler(m, prod))
	router.PATCH("/api/key-management/keys/:id", getUpdateKeyHandler(m, prod))
	router.DELETE("/api/key-management/keys/:id", getDeleteKeyHandler(m, prod))

	router.GET("/api/reporting/keys/:id", getGetKeyReportingHandler(krm, prod))
	router.POST("/api/reporting/events", getGetEventMetricsHandler(krm, prod))
	router.POST("/api/reporting/events-by-day", getGetEventMetricsByDayHandler(krm, prod))
	router.GET("/api/events", getGetEventsHandler(krm, prod))
	router.POST("/api/v2/events", getGetEventsV2Handler(krm, prod))
	router.GET("/api/reporting/user-ids", getGetUserIdsHandler(krm, prod))
	router.POST("/api/reporting/top-keys", getGetTopKeysMetricsHandler(krm, prod))

	router.GET("/api/reporting/custom-ids", getGetCustomIdsHandler(krm, prod))

	router.PUT("/api/provider-settings", getCreateProviderSettingHandler(psm, prod))
	router.GET("/api/provider-settings", getGetProviderSettingsHandler(psm, prod))
	router.PATCH("/api/provider-settings/:id", getUpdateProviderSettingHandler(psm, prod))

	router.POST("/api/custom/providers", getCreateCustomProviderHandler(cpm, prod))
	router.GET("/api/custom/providers", getGetCustomProvidersHandler(cpm, prod))
	router.PATCH("/api/custom/providers/:id", getUpdateCustomProvidersHandler(cpm, prod))

	router.POST("/api/routes", getCreateRouteHandler(rm, prod))
	router.GET("/api/routes/:id", getGetRouteHandler(rm, prod))
	router.GET("/api/routes", getGetRoutesHandler(rm, prod))
	router.DELETE("/api/routes/:id", getDeleteRouteHandler(rm, prod))

	router.POST("/api/policies", getCreatePolicyHandler(pm, prod))
	router.PATCH("/api/policies/:id", getUpdatePolicyHandler(pm, prod))
	router.GET("/api/policies", getGetPoliciesByTagsHandler(pm, prod))

	router.POST("/api/users", getCreateUserHandler(um, prod))
	router.PATCH("/api/users/:id", getUpdateUserHandler(um, prod))
	router.PATCH("/api/users", getUpdateUserViaTagsAndUserIdHandler(um, prod))
	router.GET("/api/users", getGetUsersHandler(um, prod))

	// Static file serving with caching for swagger documentation and admin interface
	staticGroup := router.Group("/")
	staticGroup.Use(staticCacheMiddleware())
	// Serve the entire docs directory to make dist/ assets available
	staticGroup.Static("/dist", "/docs/dist")
	staticGroup.StaticFile("/admin.html", "/docs/admin.html")
	staticGroup.StaticFile("/admin.yaml", "/docs/admin.yaml")

	srv := &http.Server{
		Addr:    ":8001",
		Handler: router,
	}

	return &AdminServer{
		log:    log,
		server: srv,
		m:      m,
	}, nil
}

func (as *AdminServer) Run() {
	go func() {
		as.log.Info("admin server listening at 8001")
		as.log.Info("PORT 8001 | GET    | /api/health is set up for health checking the admin server")
		as.log.Info("PORT 8001 | GET    | /api/key-management/keys is set up for retrieving keys using a query param called tag")
		as.log.Info("PORT 8001 | POST   | /api/v2/key-management/keys is set up for retrieving keys")
		as.log.Info("PORT 8001 | PUT    | /api/key-management/keys is set up for creating a key")
		as.log.Info("PORT 8001 | PATCH  | /api/key-management/keys/:id is set up for updating a key using an id")
		as.log.Info("PORT 8001 | GET    | /api/provider-settings is set up for getting provider settings")
		as.log.Info("PORT 8001 | PUT    | /api/provider-settings is set up for creating a provider setting")
		as.log.Info("PORT 8001 | PATCH  | /api/provider-settings:id is set up for updating provider setting")
		as.log.Info("PORT 8001 | POST   | /api/reporting/events is set up for retrieving api metrics")
		as.log.Info("PORT 8001 | GET    | /api/events is set up for retrieving events")
		as.log.Info("PORT 8001 | POST   | /api/v2/events is set up for retrieving events")
		as.log.Info("PORT 8001 | POST   | /api/custom/providers is set up for creating a custom provider")
		as.log.Info("PORT 8001 | GET    | /api/custom/providers is set up for retrieving all custom providers")
		as.log.Info("PORT 8001 | PATCH  | /api/custom/providers/:id is set up for updating a custom provider")
		as.log.Info("PORT 8001 | POST   | /api/routes is set up for creating a custom route")
		as.log.Info("PORT 8001 | GET    | /api/routes/:id is set up for retrieving a route")
		as.log.Info("PORT 8001 | GET    | /api/routes is set up for retrieving routes")
		as.log.Info("PORT 8001 | DELETE | /api/routes/:id is set up for deleting a route")
		as.log.Info("PORT 8001 | POST   | /api/policies is set up for creating a policy")
		as.log.Info("PORT 8001 | PATCH  | /api/policies/:id is set up for retrieving a policy")
		as.log.Info("PORT 8001 | GET    | /api/policies is set up for retrieving policies")
		as.log.Info("PORT 8001 | POST   | /api/users is set up for creating a user")
		as.log.Info("PORT 8001 | GET    | /api/users is set up for retrieving users")
		as.log.Info("PORT 8001 | PATCH  | /api/users is set up for updating a user")

		if err := as.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			as.log.Sugar().Fatalf("error admin server listening: %v", err)
		}
	}()
}

func (as *AdminServer) Shutdown(ctx context.Context) error {
	if err := as.server.Shutdown(ctx); err != nil {
		as.log.Sugar().Infof("error shutting down admin server: %v", err)
		return err
	}

	return nil
}

func getGetHealthCheckHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Status(http.StatusOK)
	}
}

func getGetKeysHandler(m KeyManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_get_keys_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_get_keys_handler.latency", dur, nil, 1)
		}()

		tag := c.Query("tag")
		tags := c.QueryArray("tags")
		keyIds := c.QueryArray("keyIds")
		provider := c.Query("provider")

		path := "/api/key-management/keys"

		if len(tags) == 0 && len(tag) == 0 && len(provider) == 0 && len(keyIds) == 0 {
			c.JSON(http.StatusBadRequest, &ErrorResponse{
				Type:     "/errors/missing-filteres",
				Title:    "filters are not found",
				Status:   http.StatusBadRequest,
				Detail:   "filters are missing from the request url. it is required for retrieving keys.",
				Instance: path,
			})
			return
		}

		selected := []string{}

		if len(tag) != 0 {
			selected = append(selected, tag)
		}

		for _, t := range tags {
			if len(t) != 0 && t != tag {
				selected = append(selected, t)
			}
		}

		keys, err := m.GetKeys(selected, keyIds, provider)
		if err != nil {
			telemetry.Incr("bricksllm.admin.get_get_keys_handler.get_keys_by_tag_err", nil, 1)

			logError(log, "error when getting api keys by tag", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/getting-keys",
				Title:    "getting keys errored out",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_get_keys_handler.success", nil, 1)
		c.JSON(http.StatusOK, keys)
	}
}

func getGetKeysV2Handler(m KeyManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_get_keys_v2_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_get_keys_v2_handler.latency", dur, nil, 1)
		}()

		path := "/api/key-management/keys"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logError(log, "error when reading get keys request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/request-body-read",
				Title:    "get key request body reader error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		request := &key.KeyRequest{}
		err = json.Unmarshal(data, request)
		if err != nil {
			logError(log, "error when unmarshalling get key request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/json-unmarshal",
				Title:    "json unmarshaller error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		keys, err := m.GetKeysV2(request.Tags, request.KeyIds, request.Revoked, request.Limit, request.Offset, request.Name, request.Order, request.ReturnCount)
		if err != nil {
			errType := "internal"

			defer func() {
				telemetry.Incr("bricksllm.admin.get_get_keys_v2_handler.get_keys_v2_err", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(validationError); ok {
				errType = "validation"
				c.JSON(http.StatusBadRequest, &ErrorResponse{
					Type:     "/errors/validation",
					Title:    "get keys request validation failed",
					Status:   http.StatusBadRequest,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when getting keys", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/key-manager",
				Title:    "getting keys errored out",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_get_keys_v2_handler.success", nil, 1)
		c.JSON(http.StatusOK, keys)
	}
}

type validationError interface {
	Error() string
	Validation()
}

func getGetProviderSettingsHandler(m ProviderSettingsManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_get_provider_settings.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_get_provider_settings.latency", dur, nil, 1)
		}()

		path := "/api/provider-settings"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		created, err := m.GetSettingsViaCache(c.QueryArray("ids"))
		if err != nil {
			errType := "internal"

			defer func() {
				telemetry.Incr("bricksllm.admin.get_get_provider_settings.get_settings_error", []string{
					"error_type:" + errType,
				}, 1)
			}()

			logError(log, "error when getting provider settings", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/provider-settings-manager",
				Title:    "get provider settings failed",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_get_provider_settings.success", nil, 1)

		c.JSON(http.StatusOK, created)
	}
}

func getCreateProviderSettingHandler(m ProviderSettingsManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_create_provider_setting_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_create_provider_setting_handler.latency", dur, nil, 1)
		}()

		path := "/api/provider-settings"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logError(log, "error when reading api key create request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/request-body-read",
				Title:    "request body reader error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		setting := &provider.Setting{}
		err = json.Unmarshal(data, setting)
		if err != nil {
			logError(log, "error when unmarshalling provider setting update request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/json-unmarshal",
				Title:    "json unmarshaller error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		created, err := m.CreateSetting(setting)
		if err != nil {
			errType := "internal"

			defer func() {
				telemetry.Incr("bricksllm.admin.get_create_provider_setting_handler.create_setting_error", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(validationError); ok {
				errType = "validation"

				c.JSON(http.StatusBadRequest, &ErrorResponse{
					Type:     "/errors/validation",
					Title:    "provider setting validation failed",
					Status:   http.StatusBadRequest,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when creating a provider setting", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/provider-settings-manager",
				Title:    "provider setting creation failed",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_create_provider_setting_handler.success", nil, 1)

		c.JSON(http.StatusOK, created)
	}
}

func getCreateKeyHandler(m KeyManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_create_key_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_create_key_handler.latency", dur, nil, 1)
		}()

		path := "/api/key-management/keys"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logError(log, "error when reading key creation request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/request-body-read",
				Title:    "request body reader error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		rk := &key.RequestKey{}
		err = json.Unmarshal(data, rk)
		if err != nil {
			logError(log, "error when unmarshalling key creation request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/json-unmarshal",
				Title:    "json unmarshaller error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		resk, err := m.CreateKey(rk)
		if err != nil {
			errType := "internal"

			defer func() {
				telemetry.Incr("bricksllm.admin.get_create_key_handler.create_key_error", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(validationError); ok {
				errType = "validation"

				c.JSON(http.StatusBadRequest, &ErrorResponse{
					Type:     "/errors/validation",
					Title:    "key validation failed",
					Status:   http.StatusBadRequest,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when creating api key", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/key-manager",
				Title:    "key creation error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_create_key_handler.success", nil, 1)

		c.JSON(http.StatusOK, resk)
	}
}

func getUpdateProviderSettingHandler(m ProviderSettingsManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_update_provider_setting_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_update_provider_setting_handler.latency", dur, nil, 1)
		}()

		path := "/api/provider-settings/:id"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		id := c.Param("id")
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logError(log, "error when reading api key update request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/request-body-read",
				Title:    "request body reader error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		setting := &provider.UpdateSetting{}
		err = json.Unmarshal(data, setting)
		if err != nil {
			logError(log, "error when unmarshalling provider setting update request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/json-unmarshal",
				Title:    "json unmarshaller error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		updated, err := m.UpdateSetting(id, setting)
		if err != nil {
			errType := "internal"

			defer func() {
				telemetry.Incr("bricksllm.admin.get_update_provider_setting_handler.update_setting_error", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(notFoundError); ok {
				errType = "not_found"
				c.JSON(http.StatusNotFound, &ErrorResponse{
					Type:     "/errors/not-found",
					Title:    "provider setting is not found",
					Status:   http.StatusNotFound,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			if _, ok := err.(validationError); ok {
				errType = "validation"
				c.JSON(http.StatusBadRequest, &ErrorResponse{
					Type:     "/errors/validation",
					Title:    "provider setting validation failed",
					Status:   http.StatusBadRequest,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when updating a provider setting", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/provider-settings-manager",
				Title:    "provider setting update failed",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_update_provider_setting_handler.success", nil, 1)

		c.JSON(http.StatusOK, updated)
	}
}

func getUpdateKeyHandler(m KeyManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_update_key_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_update_key_handler.latency", dur, nil, 1)
		}()

		path := "/api/key-management/keys/:id"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		id := c.Param("id")
		if len(id) == 0 {
			c.JSON(http.StatusBadRequest, &ErrorResponse{
				Type:     "/errors/missing-param-id",
				Title:    "id is empty",
				Status:   http.StatusBadRequest,
				Detail:   "id url param is missing from the request url. it is required for updating a key.",
				Instance: path,
			})

			return
		}

		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logError(log, "error when reading api key update request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/request-body-read",
				Title:    "request body reader error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		uk := &key.UpdateKey{}
		err = json.Unmarshal(data, uk)
		if err != nil {
			logError(log, "error when unmarshalling api key update request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/json-unmarshal",
				Title:    "json unmarshaller error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		resk, err := m.UpdateKey(id, uk)
		if err != nil {
			errType := "internal"
			defer func() {
				telemetry.Incr("bricksllm.admin.get_update_key_handler.update_key_error", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(validationError); ok {
				errType = "validation"
				c.JSON(http.StatusBadRequest, &ErrorResponse{
					Type:     "/errors/validation",
					Title:    "key validation failed",
					Status:   http.StatusBadRequest,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			if _, ok := err.(notFoundError); ok {
				errType = "not_found"
				c.JSON(http.StatusNotFound, &ErrorResponse{
					Type:     "/errors/not-found",
					Title:    "update key failed",
					Status:   http.StatusNotFound,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when updating api key", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/key-manager",
				Title:    "update key error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_update_key_handler.success", nil, 1)

		c.JSON(http.StatusOK, resk)
	}
}

func getDeleteKeyHandler(m KeyManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		path := "/api/key-management/keys/:id"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		id := c.Param("id")
		if len(id) == 0 {
			c.JSON(http.StatusBadRequest, &ErrorResponse{
				Type:     "/errors/missing-param-id",
				Title:    "id is empty",
				Status:   http.StatusBadRequest,
				Detail:   "id url param is missing from the request url. it is required for deleting a key.",
				Instance: path,
			})

			return
		}

		err := m.DeleteKey(id)
		if err != nil {
			logError(log, "error when deleting api key", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/key-manager",
				Title:    "key deletion error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		c.Status(http.StatusOK)
	}
}

type notFoundError interface {
	Error() string
	NotFound()
}

func getGetKeyReportingHandler(m KeyReportingManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_get_key_reporting_hanlder.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_get_key_reporting_hanlder.latency", dur, nil, 1)
		}()

		path := "/api/reporting/keys/:id"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		id := c.Param("id")
		if len(id) == 0 {
			c.JSON(http.StatusBadRequest, &ErrorResponse{
				Type:     "/errors/missing-param-id",
				Title:    "id is empty",
				Status:   http.StatusBadRequest,
				Detail:   "id url param is missing from the request url. it is required for retrieving api key reporting",
				Instance: path,
			})

			return
		}

		kr, err := m.GetKeyReporting(id)
		if err != nil {
			errType := "internal"

			defer func() {
				telemetry.Incr("bricksllm.admin.get_get_key_reporting_hanlder.get_key_reporting_error", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(notFoundError); ok {
				errType = "not_found"

				logError(log, "key not found", prod, err)
				c.JSON(http.StatusInternalServerError, &ErrorResponse{
					Type:     "/errors/key-not-found",
					Title:    "key not found error",
					Status:   http.StatusNotFound,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when getting api key reporting", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/key-reporting-manager",
				Title:    "key reporting error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_get_key_reporting_hanlder.success", nil, 1)

		c.JSON(http.StatusOK, kr)
	}
}

type CustomProvidersManager interface {
	CreateCustomProvider(setting *custom.Provider) (*custom.Provider, error)
	GetCustomProviders() ([]*custom.Provider, error)
	GetRouteConfigFromMem(name, path string) *custom.RouteConfig
	GetCustomProviderFromMem(name string) *custom.Provider
	UpdateCustomProvider(id string, setting *custom.UpdateProvider) (*custom.Provider, error)
}

func getCreateCustomProviderHandler(m CustomProvidersManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_create_custom_provider_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_create_custom_provider_handler.latency", dur, nil, 1)
		}()

		path := "/api/providers"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logError(log, "error when reading create a custom provider request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/request-body-read",
				Title:    "request body reader error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		setting := &custom.Provider{}
		err = json.Unmarshal(data, setting)
		if err != nil {
			logError(log, "error when unmarshalling create a custom provider request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/json-unmarshal",
				Title:    "json unmarshaller error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		cp, err := m.CreateCustomProvider(setting)
		if err != nil {
			errType := "internal"

			defer func() {
				telemetry.Incr("bricksllm.admin.get_create_custom_provider_handler.create_custom_provider_err", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(validationError); ok {
				errType = "validation"
				c.JSON(http.StatusBadRequest, &ErrorResponse{
					Type:     "/errors/validation",
					Title:    "custom provider validation failed",
					Status:   http.StatusBadRequest,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when creating a custom provider", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/custom-provider-manager",
				Title:    "creating a custom provider error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_create_custom_provider_handler.success", nil, 1)
		c.JSON(http.StatusOK, cp)
	}
}

func getGetCustomProvidersHandler(m CustomProvidersManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_get_custom_providers_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_get_custom_providers_handler.latency", dur, nil, 1)
		}()

		path := "/api/providers"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		cps, err := m.GetCustomProviders()
		if err != nil {
			errType := "internal"
			defer func() {
				telemetry.Incr("bricksllm.admin.get_get_custom_providers_handler.get_custom_providers_err", []string{
					"error_type:" + errType,
				}, 1)
			}()

			logError(log, "error when getting custom providers", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/custom-provider-manager",
				Title:    "getting custom providers error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_get_custom_providers_handler.success", nil, 1)
		c.JSON(http.StatusOK, cps)
	}
}

func getUpdateCustomProvidersHandler(m CustomProvidersManager, prod bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := util.GetLogFromCtx(c)
		telemetry.Incr("bricksllm.admin.get_update_custom_providers_handler.requests", nil, 1)

		start := time.Now()
		defer func() {
			dur := time.Since(start)
			telemetry.Timing("bricksllm.admin.get_update_custom_providers_handler.latency", dur, nil, 1)
		}()

		path := "/api/providers/:id"
		if c == nil || c.Request == nil {
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/empty-context",
				Title:    "context is empty error",
				Status:   http.StatusInternalServerError,
				Detail:   "gin context is empty",
				Instance: path,
			})
			return
		}

		id := c.Param("id")
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			logError(log, "error when reading update a custom provider request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/request-body-read",
				Title:    "request body reader error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		setting := &custom.UpdateProvider{}
		err = json.Unmarshal(data, setting)
		if err != nil {
			logError(log, "error when unmarshalling update a custom provider request body", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/json-unmarshal",
				Title:    "json unmarshaller error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		cps, err := m.UpdateCustomProvider(id, setting)
		if err != nil {
			errType := "internal"
			defer func() {
				telemetry.Incr("bricksllm.admin.get_update_custom_provider_handler.update_custom_provider_error", []string{
					"error_type:" + errType,
				}, 1)
			}()

			if _, ok := err.(validationError); ok {
				errType = "validation"
				c.JSON(http.StatusBadRequest, &ErrorResponse{
					Type:     "/errors/validation",
					Title:    "custom provider validation failed",
					Status:   http.StatusBadRequest,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			if _, ok := err.(notFoundError); ok {
				errType = "not_found"
				c.JSON(http.StatusNotFound, &ErrorResponse{
					Type:     "/errors/not-found",
					Title:    "custom provider is not found",
					Status:   http.StatusNotFound,
					Detail:   err.Error(),
					Instance: path,
				})
				return
			}

			logError(log, "error when updating a custom provider", prod, err)
			c.JSON(http.StatusInternalServerError, &ErrorResponse{
				Type:     "/errors/custom-provider-manager",
				Title:    "updating a custom provider error",
				Status:   http.StatusInternalServerError,
				Detail:   err.Error(),
				Instance: path,
			})
			return
		}

		telemetry.Incr("bricksllm.admin.get_update_custom_provider_handler.success", nil, 1)
		c.JSON(http.StatusOK, cps)
	}
}

func logError(log *zap.Logger, msg string, prod bool, err error) {
	if prod {
		log.Debug(msg, zap.Error(err))
		return
	}

	log.Debug(fmt.Sprintf("%s | %v", msg, err))
}

// staticCacheMiddleware adds caching headers for static files
func staticCacheMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Set cache control headers
		c.Header("Cache-Control", "public, max-age=3600") // Cache for 1 hour
		c.Header("ETag", fmt.Sprintf(`"%x"`, time.Now().Unix()))
		
		// Handle conditional requests
		if match := c.GetHeader("If-None-Match"); match != "" {
			c.Status(http.StatusNotModified)
			c.Abort()
			return
		}

		// Set content type based on file extension
		path := c.Request.URL.Path
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".html":
			c.Header("Content-Type", "text/html; charset=utf-8")
		case ".css":
			c.Header("Content-Type", "text/css")
		case ".js":
			c.Header("Content-Type", "application/javascript")
		case ".json":
			c.Header("Content-Type", "application/json")
		case ".png":
			c.Header("Content-Type", "image/png")
		case ".jpg", ".jpeg":
			c.Header("Content-Type", "image/jpeg")
		case ".svg":
			c.Header("Content-Type", "image/svg+xml")
		case ".ico":
			c.Header("Content-Type", "image/x-icon")
		}
		
		c.Next()
	}
}