package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// TLSFingerprintProfileRepository 定义 TLS 指纹模板的数据访问接口
type TLSFingerprintProfileRepository interface {
	List(ctx context.Context) ([]*model.TLSFingerprintProfile, error)
	GetByID(ctx context.Context, id int64) (*model.TLSFingerprintProfile, error)
	Create(ctx context.Context, profile *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error)
	Update(ctx context.Context, profile *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error)
	Delete(ctx context.Context, id int64) error
}

// TLSFingerprintProfileCache 定义 TLS 指纹模板的缓存接口
type TLSFingerprintProfileCache interface {
	Get(ctx context.Context) ([]*model.TLSFingerprintProfile, bool)
	Set(ctx context.Context, profiles []*model.TLSFingerprintProfile) error
	Invalidate(ctx context.Context) error
	NotifyUpdate(ctx context.Context) error
	SubscribeUpdates(ctx context.Context, handler func())
}

// TLSFingerprintProfileService TLS 指纹模板管理服务
type TLSFingerprintProfileService struct {
	repo  TLSFingerprintProfileRepository
	cache TLSFingerprintProfileCache

	// 本地 ID→Profile 映射缓存，用于 DoWithTLS 热路径快速查找
	localCache map[int64]*model.TLSFingerprintProfile
	localMu    sync.RWMutex
}

// NewTLSFingerprintProfileService 创建 TLS 指纹模板服务
func NewTLSFingerprintProfileService(
	repo TLSFingerprintProfileRepository,
	cache TLSFingerprintProfileCache,
) *TLSFingerprintProfileService {
	svc := &TLSFingerprintProfileService{
		repo:       repo,
		cache:      cache,
		localCache: make(map[int64]*model.TLSFingerprintProfile),
	}

	ctx := context.Background()
	if err := svc.reloadFromDB(ctx); err != nil {
		logger.LegacyPrintf("service.tls_fp_profile", "[TLSFPProfileService] Failed to load profiles from DB on startup: %v", err)
		if fallbackErr := svc.refreshLocalCache(ctx); fallbackErr != nil {
			logger.LegacyPrintf("service.tls_fp_profile", "[TLSFPProfileService] Failed to load profiles from cache fallback on startup: %v", fallbackErr)
		}
	}

	if cache != nil {
		cache.SubscribeUpdates(ctx, func() {
			if err := svc.refreshLocalCache(context.Background()); err != nil {
				logger.LegacyPrintf("service.tls_fp_profile", "[TLSFPProfileService] Failed to refresh cache on notification: %v", err)
			}
		})
	}

	return svc
}

// --- CRUD ---

// List 获取所有模板
func (s *TLSFingerprintProfileService) List(ctx context.Context) ([]*model.TLSFingerprintProfile, error) {
	return s.repo.List(ctx)
}

// GetByID 根据 ID 获取模板
func (s *TLSFingerprintProfileService) GetByID(ctx context.Context, id int64) (*model.TLSFingerprintProfile, error) {
	return s.repo.GetByID(ctx, id)
}

// Create 创建模板
func (s *TLSFingerprintProfileService) Create(ctx context.Context, profile *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}

	created, err := s.repo.Create(ctx, profile)
	if err != nil {
		return nil, err
	}

	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	s.invalidateAndNotify(refreshCtx)

	return created, nil
}

// Update 更新模板
func (s *TLSFingerprintProfileService) Update(ctx context.Context, profile *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}

	updated, err := s.repo.Update(ctx, profile)
	if err != nil {
		return nil, err
	}

	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	s.invalidateAndNotify(refreshCtx)

	return updated, nil
}

// Delete 删除模板
func (s *TLSFingerprintProfileService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}

	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	s.invalidateAndNotify(refreshCtx)

	return nil
}

// --- 热路径：运行时 Profile 查找 ---

// GetProfileByID 根据 ID 从本地缓存获取 Profile（用于 DoWithTLS 热路径）
// 返回 nil 表示未找到，调用方应 fallback 到内置默认 Profile
func (s *TLSFingerprintProfileService) GetProfileByID(id int64) *tlsfingerprint.Profile {
	s.localMu.RLock()
	p, ok := s.localCache[id]
	s.localMu.RUnlock()

	if ok && p != nil {
		return p.ToTLSProfile()
	}
	return nil
}

// pickStableRandomProfile 为「随机模板」（tls_fingerprint_profile_id == -1）按账号锚定选一个模板。
//
// 防封加固修正：原实现是 profiles[rand.IntN(len(profiles))]，每次出站现抛 ——
// 同一个账号在这一秒交 Node 形状、下一秒交另一个形状。指纹的意义在于"这是同一台客户端"，
// 形状高频漂移本身就是风控最易识别的自动化特征（也与本包 profiles.go 反复强调的
// "传输层形状要与 HTTP 层身份自洽"直接矛盾）。
//
// 新语义：**同账号恒定、跨账号互异、跨重启可复现** —— 用 sha256(盐 + 账号ID) 取模选模板，
// 与本仓既有纪律同源（geminicli.InstallationID、workbuddyDeriveAccountStableID、
// traeFingerprintSeed 都是"固定盐、账号间互异、跨重启稳定"的派生）。
//
// 候选集必须按模板 ID 升序取：localCache 是 map，直接 range 的遍历顺序是随机的，
// 那么即便索引确定，"第 i 个模板"指向谁仍会逐次变化 —— 那等于把随机换了一个更隐蔽的位置。
//
// 返回 nil 表示模板表为空，调用方按平台回落内置默认。
func (s *TLSFingerprintProfileService) pickStableRandomProfile(accountID int64) *tlsfingerprint.Profile {
	if accountID <= 0 {
		return nil
	}

	s.localMu.RLock()
	if len(s.localCache) == 0 {
		s.localMu.RUnlock()
		return nil
	}
	ids := make([]int64, 0, len(s.localCache))
	for id, p := range s.localCache {
		if p == nil || p.ID <= 0 {
			continue
		}
		ids = append(ids, id)
	}
	s.localMu.RUnlock()

	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte("tlsfp:random:" + strconv.FormatInt(accountID, 10)))
	idx := binary.BigEndian.Uint64(sum[:8]) % uint64(len(ids))

	s.localMu.RLock()
	defer s.localMu.RUnlock()
	// 二次查找：取锁间隙模板可能已被删除，此时回落平台默认而非 panic。
	if p := s.localCache[ids[idx]]; p != nil {
		return p.ToTLSProfile()
	}
	return nil
}

// ResolveTLSProfile 根据 Account 的配置解析出运行时 TLS Profile
//
// 逻辑：
//  1. 未启用 TLS 指纹 → 返回 nil（不伪装）
//  2. 启用 + 绑定了 profile_id → 从缓存查找对应 profile
//  3. 启用 + 绑定了 tls_fingerprint_profile_id == -1 → 按账号锚定选一个模板
//     （同账号恒定，跨账号互异；不逐次重抛，见 pickStableRandomProfile）
//  4. 启用 + 未绑定或找不到 → 按平台返回内置默认 Profile
//
// 第 3 步必须按平台区分：传输层指纹要与该平台上真实官方客户端的 TLS 栈同源，否则与
// HTTP 层声明的客户端身份互相矛盾（比不伪装更容易被判为异常）。
// OpenAI/Codex 的 CLI 是 Rust + rustls，套用 Node.js/Claude Code 形状即为错配，
// 故返回 rustls 形状；其余平台沿用历史 Node.js 默认值（本次改动不改变其行为）。
func (s *TLSFingerprintProfileService) ResolveTLSProfile(account *Account) *tlsfingerprint.Profile {
	if account == nil || !account.IsTLSFingerprintEnabled() {
		return nil
	}
	id := account.GetTLSFingerprintProfileID()
	if id > 0 {
		if p := s.GetProfileByID(id); p != nil {
			return p
		}
	}
	if id == -1 {
		// 随机档：按账号锚定，保证同账号的形状跨请求、跨重启都稳定。
		if p := s.pickStableRandomProfile(account.ID); p != nil {
			return p
		}
	}
	// TLS 启用但无可用模板 → 按平台回落内置默认
	return defaultTLSProfileForPlatform(account.Platform)
}

// ResolvePlatformProfile 忽略账号的启用开关，直接解析该账号应使用的 TLS Profile。
//
// 存在的理由：平台级默认开启（例如 WorkBuddy 本次改造）需要表达「运维没在账号上
// 表态，但该平台默认要伪装」。这种情况下不能复用 ResolveTLSProfile——它以
// IsTLSFingerprintEnabled() 为前提，会把「未表态」当成「关闭」。
//
// 语义边界：本方法只改「是否伪装」的判定入口，不改「伪装成什么」——绑定的模板
// 仍然优先，未绑定时仍然走 defaultTLSProfileForPlatform。因此调用方必须自己承担
// 「该平台确实应该默认伪装」这个决策（见 doUpstreamWithPlatformDefaultTLS）。
func (s *TLSFingerprintProfileService) ResolvePlatformProfile(account *Account) *tlsfingerprint.Profile {
	if s == nil || account == nil {
		return nil
	}
	id := account.GetTLSFingerprintProfileID()
	if id > 0 {
		if p := s.GetProfileByID(id); p != nil {
			return p
		}
	}
	if id == -1 {
		if p := s.pickStableRandomProfile(account.ID); p != nil {
			return p
		}
	}
	return defaultTLSProfileForPlatform(account.Platform)
}

// defaultTLSProfileForPlatform 返回平台对应的内置默认 TLS Profile。
//
// 未列出的平台一律沿用 Node.js 默认值，保持改动前的行为不变。
//
// WorkBuddy/Trae/Qoder 三平台拿的是 NodeOpenSSLH2Profile（Node/OpenSSL 形状 + ALPN h2），
// 不是 Claude 用的 NodeDefaultProfile。原因：这三家的指纹是「平台默认开启」，而
// 带指纹的出站只能跑 HTTP/1.1（http.Transport 在自定义 DialTLSContext 下拿不到
// crypto/tls 连接状态，永不升 h2），叠加账号隔离下 maxConnsPerHost 被夹成
// account.Concurrency（线上常见 1），同账号并发请求会**串行排队** —— 实测
// 12 并发从 105ms（改造前 h2）劣化到 779ms。声明 h2 后传输层会改用 http2.Transport
// （见 repository/http_upstream.go 的 buildFingerprintHTTP2Transport），实测回到 104ms。
// ALPN 含 h2 也与这三家官方客户端（Electron/Chromium，真实 ALPN 就是 h2）一致；
// Claude/Anthropic 保持 h1 形状不变，避免改动既有账号的 JA4。
//
// 已知局限（诚实口径，不要当成 bug 修）：WorkBuddy 桌面端是 Electron/Chromium
// （BoringSSL），而本包目前只有 Node/OpenSSL 与 rustls 两套内置形状，没有
// Chromium 形状。WorkBuddy 走平台默认时拿到的是 Node 形状——它与官方客户端不完全
// 同源，但已消除「Go 标准库 ClientHello」这个更明显的标记。要真正对齐需要先用
// tls-fingerprint-web 采集一份 Chromium 形状并当作模板绑定到账号（模板表机制已就位）。
// 此处不凭记忆手写 Chromium 参数：错配的指纹（chimera）比不伪装更容易被识别判异常。
func defaultTLSProfileForPlatform(platform string) *tlsfingerprint.Profile {
	switch platform {
	case PlatformOpenAI:
		return tlsfingerprint.CodexRustlsProfile()
	case PlatformWorkbuddy, PlatformTrae, PlatformQoder:
		return tlsfingerprint.NodeOpenSSLH2Profile()
	default:
		return tlsfingerprint.NodeDefaultProfile()
	}
}

// --- 缓存管理 ---

func (s *TLSFingerprintProfileService) refreshLocalCache(ctx context.Context) error {
	if s.cache != nil {
		if profiles, ok := s.cache.Get(ctx); ok {
			s.setLocalCache(profiles)
			return nil
		}
	}
	return s.reloadFromDB(ctx)
}

func (s *TLSFingerprintProfileService) reloadFromDB(ctx context.Context) error {
	profiles, err := s.repo.List(ctx)
	if err != nil {
		return err
	}

	if s.cache != nil {
		if err := s.cache.Set(ctx, profiles); err != nil {
			logger.LegacyPrintf("service.tls_fp_profile", "[TLSFPProfileService] Failed to set cache: %v", err)
		}
	}

	s.setLocalCache(profiles)
	return nil
}

func (s *TLSFingerprintProfileService) setLocalCache(profiles []*model.TLSFingerprintProfile) {
	m := make(map[int64]*model.TLSFingerprintProfile, len(profiles))
	for _, p := range profiles {
		m[p.ID] = p
	}

	s.localMu.Lock()
	s.localCache = m
	s.localMu.Unlock()
}

func (s *TLSFingerprintProfileService) newCacheRefreshContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

func (s *TLSFingerprintProfileService) invalidateAndNotify(ctx context.Context) {
	if s.cache != nil {
		if err := s.cache.Invalidate(ctx); err != nil {
			logger.LegacyPrintf("service.tls_fp_profile", "[TLSFPProfileService] Failed to invalidate cache: %v", err)
		}
	}

	if err := s.reloadFromDB(ctx); err != nil {
		logger.LegacyPrintf("service.tls_fp_profile", "[TLSFPProfileService] Failed to refresh local cache: %v", err)
		s.localMu.Lock()
		s.localCache = make(map[int64]*model.TLSFingerprintProfile)
		s.localMu.Unlock()
	}

	if s.cache != nil {
		if err := s.cache.NotifyUpdate(ctx); err != nil {
			logger.LegacyPrintf("service.tls_fp_profile", "[TLSFPProfileService] Failed to notify cache update: %v", err)
		}
	}
}
