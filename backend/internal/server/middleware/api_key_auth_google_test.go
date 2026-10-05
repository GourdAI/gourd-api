package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGoogleAPIKeyAuthRejectsOversizedCredentialsBeforeLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	repo := fakeAPIKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
		calls.Add(1)
		return nil, service.ErrAPIKeyNotFound
	}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	r := gin.New()
	var reason IngressRejectReason
	var rejected bool
	r.Use(func(c *gin.Context) {
		c.Next()
		reason, rejected = GetIngressRejectReason(c)
	})
	r.Use(APIKeyAuthGoogle(svc, cfg))
	r.GET("/v1beta/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("x-goog-api-key", strings.Repeat("x", service.MaxAPIKeyCredentialBytes+1))
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, calls.Load())
	require.True(t, rejected)
	require.Equal(t, IngressRejectInvalidAPIKey, reason)
}

func TestGoogleAPIKeyAuthMarksLookupBulkheadRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := fakeAPIKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
		return nil, service.ErrAPIKeyAuthOverloaded
	}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	r := gin.New()
	var reason IngressRejectReason
	var rejected bool
	r.Use(func(c *gin.Context) {
		c.Next()
		reason, rejected = GetIngressRejectReason(c)
	})
	r.Use(APIKeyAuthGoogle(svc, cfg))
	r.GET("/v1beta/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("x-goog-api-key", "valid-shape")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.True(t, rejected)
	require.Equal(t, IngressRejectAPIKeyAuthOverloaded, reason)
}

type fakeAPIKeyRepo struct {
	getByKey       func(ctx context.Context, key string) (*service.APIKey, error)
	updateLastUsed func(ctx context.Context, id int64, usedAt time.Time) error
}

// fakeGoogleSubscriptionRepo 实现「个人额度钱包」仓储契约（service.UserSubscriptionRepository）。
//
// 钱包化后订阅不再以 (userID, groupID) 为槽位：中间件热路径唯一会调用的读方法是
// ListActiveByUserID（按 expires_at 升序 = 消耗顺序），因此数据钩子是 listActive。
// 窗口维护类方法（ActivateWindows / ResetDaily|Weekly|MonthlyUsage）已随契约删除。
type fakeGoogleSubscriptionRepo struct {
	getByID      func(ctx context.Context, id int64) (*service.UserSubscription, error)
	listActive   func(ctx context.Context, userID int64) ([]service.UserSubscription, error)
	existsActive func(ctx context.Context, userID int64) (bool, error)
	updateStatus func(ctx context.Context, subscriptionID int64, status string) error
}

func (f fakeAPIKeyRepo) Create(ctx context.Context, key *service.APIKey) error {
	return errors.New("not implemented")
}
func (f fakeAPIKeyRepo) GetByID(ctx context.Context, id int64) (*service.APIKey, error) {
	return nil, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) GetKeyAndOwnerID(ctx context.Context, id int64) (string, int64, error) {
	return "", 0, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) GetByKey(ctx context.Context, key string) (*service.APIKey, error) {
	if f.getByKey == nil {
		return nil, errors.New("unexpected call")
	}
	return f.getByKey(ctx, key)
}
func (f fakeAPIKeyRepo) GetByKeyForAuth(ctx context.Context, key string) (*service.APIKey, error) {
	return f.GetByKey(ctx, key)
}
func (f fakeAPIKeyRepo) Update(ctx context.Context, key *service.APIKey, _ service.APIKeyUpdateFields) error {
	return errors.New("not implemented")
}
func (f fakeAPIKeyRepo) Delete(ctx context.Context, id int64) error {
	return errors.New("not implemented")
}
func (f fakeAPIKeyRepo) DeleteWithAudit(ctx context.Context, id int64) error {
	return errors.New("not implemented")
}
func (f fakeAPIKeyRepo) ListByUserID(ctx context.Context, userID int64, params pagination.PaginationParams, _ service.APIKeyListFilters) ([]service.APIKey, *pagination.PaginationResult, error) {
	return nil, nil, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) VerifyOwnership(ctx context.Context, userID int64, apiKeyIDs []int64) ([]int64, error) {
	return nil, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) CountByUserID(ctx context.Context, userID int64) (int64, error) {
	return 0, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) ExistsByKey(ctx context.Context, key string) (bool, error) {
	return false, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) ListByGroupID(ctx context.Context, groupID int64, params pagination.PaginationParams) ([]service.APIKey, *pagination.PaginationResult, error) {
	return nil, nil, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) SearchAPIKeys(ctx context.Context, userID int64, keyword string, limit int) ([]service.APIKey, error) {
	return nil, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) ClearGroupIDByGroupID(ctx context.Context, groupID int64) (int64, error) {
	return 0, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) CountByGroupID(ctx context.Context, groupID int64) (int64, error) {
	return 0, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) ListKeysByUserID(ctx context.Context, userID int64) ([]string, error) {
	return nil, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) ListKeysByGroupID(ctx context.Context, groupID int64) ([]string, error) {
	return nil, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) (float64, error) {
	return 0, errors.New("not implemented")
}
func (f fakeAPIKeyRepo) UpdateLastUsed(ctx context.Context, id int64, usedAt time.Time) error {
	if f.updateLastUsed != nil {
		return f.updateLastUsed(ctx, id, usedAt)
	}
	return nil
}
func (f fakeAPIKeyRepo) IncrementRateLimitUsage(ctx context.Context, id int64, cost float64) error {
	return nil
}
func (f fakeAPIKeyRepo) ResetRateLimitWindows(ctx context.Context, id int64) error {
	return nil
}
func (f fakeAPIKeyRepo) GetRateLimitData(ctx context.Context, id int64) (*service.APIKeyRateLimitData, error) {
	return &service.APIKeyRateLimitData{}, nil
}
func (f fakeAPIKeyRepo) UpdateGroupIDByUserAndGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (int64, error) {
	return 0, errors.New("not implemented")
}

func (f fakeGoogleSubscriptionRepo) Create(ctx context.Context, sub *service.UserSubscription) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) GetByID(ctx context.Context, id int64) (*service.UserSubscription, error) {
	if f.getByID != nil {
		return f.getByID(ctx, id)
	}
	return nil, errors.New("not implemented")
}

func (f fakeGoogleSubscriptionRepo) GetByIDForUpdate(ctx context.Context, id int64) (*service.UserSubscription, error) {
	return f.GetByID(ctx, id)
}
func (f fakeGoogleSubscriptionRepo) GetByIDIncludeDeleted(ctx context.Context, id int64) (*service.UserSubscription, error) {
	return nil, errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) Update(ctx context.Context, sub *service.UserSubscription) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) Delete(ctx context.Context, id int64) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) Restore(ctx context.Context, subscriptionID int64, restoredStatus string) (*service.UserSubscription, error) {
	return nil, errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) ListByUserID(ctx context.Context, userID int64) ([]service.UserSubscription, error) {
	return nil, errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) ListActiveByUserID(ctx context.Context, userID int64) ([]service.UserSubscription, error) {
	if f.listActive != nil {
		return f.listActive(ctx, userID)
	}
	return nil, service.ErrSubscriptionNotFound
}
func (f fakeGoogleSubscriptionRepo) List(ctx context.Context, params pagination.PaginationParams, userID, planID *int64, status, sortBy, sortOrder string) ([]service.UserSubscription, *pagination.PaginationResult, error) {
	return nil, nil, errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) ExistsActiveByUserID(ctx context.Context, userID int64) (bool, error) {
	if f.existsActive != nil {
		return f.existsActive(ctx, userID)
	}
	return false, errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) UpdateAssignedLimit(ctx context.Context, id int64, total *float64) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) ExtendExpiry(ctx context.Context, subscriptionID int64, newExpiresAt time.Time) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) UpdateStatus(ctx context.Context, subscriptionID int64, status string) error {
	if f.updateStatus != nil {
		return f.updateStatus(ctx, subscriptionID, status)
	}
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) UpdateNotes(ctx context.Context, subscriptionID int64, notes string) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) ResetUsage(ctx context.Context, id int64) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) IncrementUsage(ctx context.Context, id int64, costUSD float64) error {
	return errors.New("not implemented")
}
func (f fakeGoogleSubscriptionRepo) BatchUpdateExpiredStatus(ctx context.Context) (int64, error) {
	return 0, errors.New("not implemented")
}

type googleErrorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func newTestAPIKeyService(repo service.APIKeyRepository) *service.APIKeyService {
	return service.NewAPIKeyService(
		repo,
		nil, // userRepo (unused in GetByKey)
		nil, // groupRepo
		nil, // userSubRepo
		nil, // userGroupRateRepo
		nil, // cache
		&config.Config{},
	)
}

func TestApiKeyAuthWithSubscriptionGoogle_MissingKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return nil, errors.New("should not be called")
		},
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, &config.Config{}))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, http.StatusUnauthorized, resp.Error.Code)
	require.Equal(t, "API key is required", resp.Error.Message)
	require.Equal(t, "UNAUTHENTICATED", resp.Error.Status)
}

func TestApiKeyAuthWithSubscriptionGoogle_QueryApiKeyRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return nil, errors.New("should not be called")
		},
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, &config.Config{}))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test?api_key=legacy", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, http.StatusBadRequest, resp.Error.Code)
	require.Equal(t, "Query parameter api_key is deprecated. Use Authorization header or key instead.", resp.Error.Message)
	require.Equal(t, "INVALID_ARGUMENT", resp.Error.Status)
}

func TestApiKeyAuthWithSubscriptionGoogleSetsGroupContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	group := &service.Group{
		ID:       99,
		Name:     "g1",
		Status:   service.StatusActive,
		Platform: service.PlatformGemini,
		Hydrated: true,
	}
	user := &service.User{
		ID:          7,
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     10,
		Concurrency: 3,
	}
	apiKey := &service.APIKey{
		ID:     100,
		UserID: user.ID,
		Key:    "test-key",
		Status: service.StatusActive,
		User:   user,
		Group:  group,
	}
	apiKey.GroupID = &group.ID

	apiKeyService := service.NewAPIKeyService(
		fakeAPIKeyRepo{
			getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
				if key != apiKey.Key {
					return nil, service.ErrAPIKeyNotFound
				}
				clone := *apiKey
				return &clone, nil
			},
		},
		nil,
		nil,
		nil,
		nil,
		nil,
		&config.Config{RunMode: config.RunModeSimple},
	)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	r := gin.New()
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, cfg))
	r.GET("/v1beta/test", func(c *gin.Context) {
		groupFromCtx, ok := c.Request.Context().Value(ctxkey.Group).(*service.Group)
		if !ok || groupFromCtx == nil || groupFromCtx.ID != group.ID {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("x-api-key", apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestApiKeyAuthWithSubscriptionGoogle_QueryKeyAllowedOnV1Beta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return &service.APIKey{
				ID:     1,
				Key:    key,
				Status: service.StatusActive,
				User: &service.User{
					ID:     123,
					Status: service.StatusActive,
				},
			}, nil
		},
	})
	cfg := &config.Config{RunMode: config.RunModeSimple}
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, cfg))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test?key=valid", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestApiKeyAuthWithSubscriptionGoogle_InvalidKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return nil, service.ErrAPIKeyNotFound
		},
	})
	var rejectReason IngressRejectReason
	var rejected bool
	r.Use(func(c *gin.Context) {
		c.Next()
		rejectReason, rejected = GetIngressRejectReason(c)
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, &config.Config{}))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, http.StatusUnauthorized, resp.Error.Code)
	require.Equal(t, "Invalid API key", resp.Error.Message)
	require.Equal(t, "UNAUTHENTICATED", resp.Error.Status)
	require.True(t, rejected)
	require.Equal(t, IngressRejectInvalidAPIKey, rejectReason)
}

func TestApiKeyAuthWithSubscriptionGoogle_MarksUnavailableGroupBusinessLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(101)
	user := &service.User{
		ID:          7,
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     10,
		Concurrency: 3,
	}
	apiKey := &service.APIKey{
		ID:      100,
		UserID:  user.ID,
		GroupID: &groupID,
		Key:     "google-group-deleted",
		Status:  service.StatusActive,
		User:    user,
		Group: &service.Group{
			ID:       groupID,
			Name:     "deleted",
			Status:   "deleted",
			Platform: service.PlatformGemini,
			Hydrated: true,
		},
	}

	r := gin.New()
	var markedBusinessLimited bool
	var businessLimitedReason string
	var rejectReason IngressRejectReason
	var rejected bool
	r.Use(func(c *gin.Context) {
		c.Next()
		markedBusinessLimited = service.HasOpsClientBusinessLimited(c)
		rejectReason, rejected = GetIngressRejectReason(c)
		if v, ok := c.Get(service.OpsClientBusinessLimitedReasonKey); ok {
			businessLimitedReason, _ = v.(string)
		}
	})
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, &config.Config{RunMode: config.RunModeSimple}))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("x-goog-api-key", apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "API Key 所属分组已删除", resp.Error.Message)
	require.True(t, markedBusinessLimited)
	require.Equal(t, service.OpsClientBusinessLimitedReasonAPIKeyGroupUnavailable, businessLimitedReason)
	require.True(t, rejected)
	require.Equal(t, IngressRejectGroupDeleted, rejectReason)
}

func TestApiKeyAuthWithSubscriptionGoogle_RepoError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return nil, errors.New("db down")
		},
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, &config.Config{}))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("Authorization", "Bearer any")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, http.StatusInternalServerError, resp.Error.Code)
	require.Equal(t, "Failed to validate API key", resp.Error.Message)
	require.Equal(t, "INTERNAL", resp.Error.Status)
}

func TestApiKeyAuthWithSubscriptionGoogle_DisabledKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return &service.APIKey{
				ID:     1,
				Key:    key,
				Status: service.StatusDisabled,
				User: &service.User{
					ID:     123,
					Status: service.StatusActive,
				},
			}, nil
		},
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, &config.Config{}))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("Authorization", "Bearer disabled")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, http.StatusUnauthorized, resp.Error.Code)
	require.Equal(t, "API key is disabled", resp.Error.Message)
	require.Equal(t, "UNAUTHENTICATED", resp.Error.Status)
}

func TestApiKeyAuthWithSubscriptionGoogle_InsufficientBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return &service.APIKey{
				ID:     1,
				Key:    key,
				Status: service.StatusActive,
				User: &service.User{
					ID:      123,
					Status:  service.StatusActive,
					Balance: 0,
				},
			}, nil
		},
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, &config.Config{}))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("Authorization", "Bearer ok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, http.StatusForbidden, resp.Error.Code)
	require.Equal(t, "Insufficient account balance", resp.Error.Message)
	require.Equal(t, "PERMISSION_DENIED", resp.Error.Status)
}

func TestApiKeyAuthWithSubscriptionGoogle_BalanceBelowMinimumReserve(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 鉴权层保持历史语义：MinimumBalanceReserve 只用于 billing-cache 预检，
	// 0 < balance < reserve 的用户不得在鉴权中间件被硬 403。
	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return &service.APIKey{
				ID:     1,
				Key:    key,
				Status: service.StatusActive,
				User: &service.User{
					ID:      123,
					Status:  service.StatusActive,
					Balance: 0.005,
				},
			}, nil
		},
	})
	cfg := &config.Config{}
	cfg.Billing.MinimumBalanceReserve = 0.01
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, cfg))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("Authorization", "Bearer ok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestApiKeyAuthWithSubscriptionGoogle_RejectsExhaustedBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			return &service.APIKey{
				ID:     1,
				Key:    key,
				Status: service.StatusActive,
				User: &service.User{
					ID:      123,
					Status:  service.StatusActive,
					Balance: 0,
				},
			}, nil
		},
	})
	cfg := &config.Config{}
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, cfg))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("Authorization", "Bearer ok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, http.StatusForbidden, resp.Error.Code)
	require.Equal(t, "Insufficient account balance", resp.Error.Message)
	require.Equal(t, "PERMISSION_DENIED", resp.Error.Status)
}

func TestApiKeyAuthWithSubscriptionGoogle_TouchesLastUsedOnSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := &service.User{
		ID:          11,
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     10,
		Concurrency: 3,
	}
	apiKey := &service.APIKey{
		ID:     201,
		UserID: user.ID,
		Key:    "google-touch-ok",
		Status: service.StatusActive,
		User:   user,
	}

	var touchedID int64
	var touchedAt time.Time
	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			touchedID = id
			touchedAt = usedAt
			return nil
		},
	})
	cfg := &config.Config{RunMode: config.RunModeSimple}
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, cfg))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("x-goog-api-key", apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, apiKey.ID, touchedID)
	require.False(t, touchedAt.IsZero())
}

func TestApiKeyAuthWithSubscriptionGoogle_TouchFailureDoesNotBlock(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := &service.User{
		ID:          12,
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     10,
		Concurrency: 3,
	}
	apiKey := &service.APIKey{
		ID:     202,
		UserID: user.ID,
		Key:    "google-touch-fail",
		Status: service.StatusActive,
		User:   user,
	}

	touchCalls := 0
	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			touchCalls++
			return errors.New("write failed")
		},
	})
	cfg := &config.Config{RunMode: config.RunModeSimple}
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, cfg))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("x-goog-api-key", apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, touchCalls)
}

func TestApiKeyAuthWithSubscriptionGoogle_TouchesLastUsedInStandardMode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := &service.User{
		ID:          13,
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     10,
		Concurrency: 3,
	}
	apiKey := &service.APIKey{
		ID:     203,
		UserID: user.ID,
		Key:    "google-touch-standard",
		Status: service.StatusActive,
		User:   user,
	}

	touchCalls := 0
	r := gin.New()
	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			touchCalls++
			return nil
		},
	})
	cfg := &config.Config{RunMode: config.RunModeStandard}
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, nil, cfg))
	r.GET("/v1beta/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, touchCalls)
}

// TestApiKeyAuthWithSubscriptionGoogle_ExhaustedWalletStillTakesOverBilling 锁定钱包化后的关键语义（契约第 4/5/6 节）。
//
// 旧行为（已删除）：鉴权中间件在鉴权阶段逐项比对日/周/月额度，超限直接 429
// "daily usage limit exceeded"。新行为：额度超限判定整体移交计费层
// （BillingCacheService.CheckBillingEligibility / ErrSubscriptionQuotaExhausted）。
// 中间件只看「钱包是否接管扣费」：只要持有**设了额度**的钱包（哪怕已耗尽），
// 就跳过余额硬闸，请求放行到 handler，由计费层做真正的额度拒绝。
//
// 本用例是防回归锚点（不得删除），它同时钉住：
//  1. 钱包余量耗尽 + 用户余额为 0 → 绝不在鉴权层 403/429（拒绝只能出自计费层，
//     避免“刚分配额度就被清除分组权限的用户继续访问”这类判定散落在两层而被绕过）；
//  2. ctx 写入的是**订阅切片** []*service.UserSubscription（多钱包，按消耗顺序），
//     不再是旧的单份指针。
func TestApiKeyAuthWithSubscriptionGoogle_ExhaustedWalletStillTakesOverBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)

	totalLimit := 1.0
	planID := int64(9001)
	group := &service.Group{
		ID:       77,
		Name:     "gemini-standard",
		Status:   service.StatusActive,
		Platform: service.PlatformGemini,
		Hydrated: true,
	}
	// 余额为 0：若钱包不接管扣费，本请求必然被余额硬闸 403（参照 RejectsExhaustedBalance）。
	user := &service.User{
		ID:          999,
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     0,
		Concurrency: 3,
	}
	apiKey := &service.APIKey{
		ID:      501,
		UserID:  user.ID,
		Key:     "google-wallet-exhausted",
		Status:  service.StatusActive,
		User:    user,
		GroupID: &group.ID,
		Group:   group,
	}

	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
	})

	now := time.Now()
	// 一份耗尽的限额钱包 + 一份排在后面的限额钱包（多钱包语义，按 expires_at 升序）。
	exhausted := service.UserSubscription{
		ID:            601,
		UserID:        user.ID,
		PlanID:        &planID,
		Status:        service.SubscriptionStatusActive,
		StartsAt:      now.Add(-time.Hour),
		ExpiresAt:     now.Add(24 * time.Hour),
		TotalLimitUSD: &totalLimit,
		TotalUsageUSD: 10,
	}
	second := service.UserSubscription{
		ID:            602,
		UserID:        user.ID,
		PlanID:        &planID,
		Status:        service.SubscriptionStatusActive,
		StartsAt:      now.Add(-time.Hour),
		ExpiresAt:     now.Add(72 * time.Hour),
		TotalLimitUSD: &totalLimit,
	}
	subscriptionService := service.NewSubscriptionService(nil, fakeGoogleSubscriptionRepo{
		listActive: func(ctx context.Context, userID int64) ([]service.UserSubscription, error) {
			if userID != user.ID {
				return nil, service.ErrSubscriptionNotFound
			}
			return []service.UserSubscription{exhausted, second}, nil
		},
		updateStatus: func(ctx context.Context, subscriptionID int64, status string) error { return nil },
	}, nil, nil, &config.Config{RunMode: config.RunModeStandard})
	t.Cleanup(subscriptionService.Stop)

	var (
		handlerReached   bool
		subsFromCtx      []*service.UserSubscription
		subsCtxOK        bool
		writtenAsSlice   bool
		writtenAsPointer bool
		firstFromCtx     *service.UserSubscription
		firstFromCtxOK   bool
	)
	r := gin.New()
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, &config.Config{RunMode: config.RunModeStandard}))
	r.GET("/v1beta/test", func(c *gin.Context) {
		handlerReached = true
		subsFromCtx, subsCtxOK = GetSubscriptionsFromContext(c)
		firstFromCtx, firstFromCtxOK = GetSubscriptionFromContext(c)
		if v, exists := c.Get(string(ContextKeySubscription)); exists {
			_, writtenAsSlice = v.([]*service.UserSubscription)
			_, writtenAsPointer = v.(*service.UserSubscription)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("x-goog-api-key", apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// 1. 额度耗尽的钱包仍然接管扣费 → 鉴权层既不 429（不再有 daily usage limit）也不 403
	//    （余额硬闸被跳过），请求抵达 handler，且响应体不含任何错误。
	require.Equal(t, http.StatusOK, rec.Code, "耗尽钱包必须接管扣费，不得在鉴权层 403/429")
	require.True(t, handlerReached)
	require.NotContains(t, rec.Body.String(), "RESOURCE_EXHAUSTED")
	require.NotContains(t, rec.Body.String(), "daily usage limit")
	require.NotContains(t, rec.Body.String(), "Insufficient account balance")

	// 2. ctx 写入的是订阅切片（两份钱包，顺序 = 消耗顺序），而不是单指针。
	require.True(t, writtenAsSlice, "ContextKeySubscription 必须写入 []*service.UserSubscription")
	require.False(t, writtenAsPointer, "不得再写入旧的单指针形态")
	require.True(t, subsCtxOK, "GetSubscriptionsFromContext 必须读回切片")
	require.Len(t, subsFromCtx, 2)
	require.Equal(t, exhausted.ID, subsFromCtx[0].ID)
	require.Equal(t, second.ID, subsFromCtx[1].ID)
	require.NotNil(t, subsFromCtx[0].TotalLimitUSD)
	require.Equal(t, totalLimit, *subsFromCtx[0].TotalLimitUSD)
	require.Equal(t, 10.0, subsFromCtx[0].TotalUsageUSD)
	require.NotNil(t, subsFromCtx[0].PlanID)
	require.Equal(t, planID, *subsFromCtx[0].PlanID)

	// 3. 便捷入口返回最先到期的一份（展示/定位用）；计费判定仍须走聚合切片。
	require.True(t, firstFromCtxOK)
	require.NotNil(t, firstFromCtx)
	require.Equal(t, exhausted.ID, firstFromCtx.ID)
}

// TestApiKeyAuthWithSubscriptionGoogle_UnlimitedWalletFallsBackToBalance 钉住资损闸门另一半：
// 全额为 nil（不限额）的钱包**不接管扣费**（契约第 7 节刻意行为，不得“修”成默认不限额），
// 因此余额为 0 的用户仍必须在鉴权层被余额硬闸 403；同时钱包依旧写入 ctx 供展示定位。
func TestApiKeyAuthWithSubscriptionGoogle_UnlimitedWalletFallsBackToBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	group := &service.Group{
		ID:       78,
		Name:     "gemini-standard",
		Status:   service.StatusActive,
		Platform: service.PlatformGemini,
		Hydrated: true,
	}
	user := &service.User{
		ID:          998,
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     0,
		Concurrency: 3,
	}
	apiKey := &service.APIKey{
		ID:      502,
		UserID:  user.ID,
		Key:     "google-wallet-unlimited",
		Status:  service.StatusActive,
		User:    user,
		GroupID: &group.ID,
		Group:   group,
	}

	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
	})

	subscriptionService := service.NewSubscriptionService(nil, fakeGoogleSubscriptionRepo{
		listActive: func(ctx context.Context, userID int64) ([]service.UserSubscription, error) {
			if userID != user.ID {
				return nil, service.ErrSubscriptionNotFound
			}
			return []service.UserSubscription{{
				ID:        611,
				UserID:    user.ID,
				Status:    service.SubscriptionStatusActive,
				ExpiresAt: time.Now().Add(24 * time.Hour),
			}}, nil
		},
	}, nil, nil, &config.Config{RunMode: config.RunModeStandard})
	t.Cleanup(subscriptionService.Stop)

	var (
		handlerReached bool
		subsFromCtx    []*service.UserSubscription
		subsCtxOK      bool
	)
	r := gin.New()
	// Google 入口与主中间件的写入时机不同：它在余额硬闸**之前**就写入订阅 ctx，
	// 因此必须用 c.Next() 后的观察者读取（403 中止路径上 handler 根本不会运行）。
	r.Use(func(c *gin.Context) {
		c.Next()
		subsFromCtx, subsCtxOK = GetSubscriptionsFromContext(c)
	})
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, &config.Config{RunMode: config.RunModeStandard}))
	r.GET("/v1beta/test", func(c *gin.Context) {
		handlerReached = true
		subsFromCtx, subsCtxOK = GetSubscriptionsFromContext(c)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("x-goog-api-key", apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.False(t, handlerReached, "不限额钱包不得接管扣费")
	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "Insufficient account balance", resp.Error.Message)
	require.Equal(t, "PERMISSION_DENIED", resp.Error.Status)
	require.True(t, subsCtxOK, "钱包不接管也应写入 ctx 供展示与记账定位")
	require.Len(t, subsFromCtx, 1)
	require.Equal(t, int64(611), subsFromCtx[0].ID)
}

// TestApiKeyAuthWithSubscriptionGoogle_SubscriptionNeverGrantsGroupAdmission 反向断言（Gemini 入口）：
// 持有**已耗尽限额钱包**的用户，对其专属分组的授权被清除后，仍必须被准入校验 403 拦住
// （订阅绝不授予分组准入，契约第 6 节）——即“刚分配额度就被清除分组权限”的用户不得继续访问。
func TestApiKeyAuthWithSubscriptionGoogle_SubscriptionNeverGrantsGroupAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)

	totalLimit := 100.0
	group := &service.Group{
		ID:          79,
		Name:        "gemini-exclusive",
		Status:      service.StatusActive,
		Platform:    service.PlatformGemini,
		Hydrated:    true,
		IsExclusive: true,
	}
	user := &service.User{
		ID:            997,
		Role:          service.RoleUser,
		Status:        service.StatusActive,
		Balance:       10,
		Concurrency:   3,
		AllowedGroups: []int64{},
	}
	apiKey := &service.APIKey{
		ID:      503,
		UserID:  user.ID,
		Key:     "google-sub-no-admission",
		Status:  service.StatusActive,
		User:    user,
		GroupID: &group.ID,
		Group:   group,
	}

	apiKeyService := newTestAPIKeyService(fakeAPIKeyRepo{
		getByKey: func(ctx context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
	})

	walletProbed := false
	subscriptionService := service.NewSubscriptionService(nil, fakeGoogleSubscriptionRepo{
		listActive: func(ctx context.Context, userID int64) ([]service.UserSubscription, error) {
			walletProbed = true
			return []service.UserSubscription{{
				ID:            621,
				UserID:        user.ID,
				Status:        service.SubscriptionStatusActive,
				ExpiresAt:     time.Now().Add(24 * time.Hour),
				TotalLimitUSD: &totalLimit,
				TotalUsageUSD: 100,
			}}, nil
		},
	}, nil, nil, &config.Config{RunMode: config.RunModeStandard})
	t.Cleanup(subscriptionService.Stop)

	reachedHandler := false
	r := gin.New()
	r.Use(APIKeyAuthWithSubscriptionGoogle(apiKeyService, subscriptionService, &config.Config{RunMode: config.RunModeStandard}))
	r.GET("/v1beta/test", func(c *gin.Context) {
		reachedHandler = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1beta/test", nil)
	req.Header.Set("x-goog-api-key", apiKey.Key)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.False(t, reachedHandler, "持钱包不得绕过分组准入校验")
	require.False(t, walletProbed, "准入不通过必须早于钱包探测，不得因钱包改变准入结果")
	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp googleErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "API Key 所属专属分组不再允许当前用户使用", resp.Error.Message)
	require.NotContains(t, rec.Body.String(), "Insufficient account balance")
}
