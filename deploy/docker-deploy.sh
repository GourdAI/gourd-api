#!/bin/bash
# =============================================================================
# Sub2API One-Click Docker Deployment Script
# =============================================================================
# Prepares and starts a Sub2API deployment with Docker Compose. The deployment
# files are bundled inside this script, so the script itself downloads nothing:
# it only needs Docker and (for secret generation) openssl.
#
# The script:
#   - Writes docker-compose.yml and .env.example from the bundled templates
#   - Generates secure secrets (JWT_SECRET, TOTP_ENCRYPTION_KEY, POSTGRES_PASSWORD)
#   - Creates data directories (data/, postgres_data/, redis_data/)
#   - Starts the stack and waits for the application health check
#   - Displays the generated credentials
#
# Usage:
#   # Recommended: download this script, then run it inside an empty deployment
#   # directory (the script writes its files into the current directory)
#   curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/docker-deploy.sh -o docker-deploy.sh
#   bash docker-deploy.sh
#
#   # One-liner alternative (the script prompts for nothing)
#   curl -sSL https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy/docker-deploy.sh | bash
#
# Options:
#   --no-start    Prepare deployment files only; do not start services
#   --force       Regenerate deployment files and secrets. Use this only for a
#                 fresh deployment: regenerating POSTGRES_PASSWORD breaks access
#                 to data in an existing postgres_data/ directory
#   -h, --help    Show this help
#
# After the first run, manage the stack with:
#   docker compose logs -f sub2api
#   docker compose ps
#   docker compose down
#   docker compose pull && docker compose up -d   # update to the latest image
# =============================================================================

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

print_info() {
    printf '%s\n' "${BLUE}[INFO]${NC} $1"
}

print_success() {
    printf '%s\n' "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    printf '%s\n' "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    printf '%s\n' "${RED}[ERROR]${NC} $1"
}

# ---------------------------------------------------------------------------
# Bundled deployment templates
# ---------------------------------------------------------------------------
# These two blocks are bundled copies of the repository files:
#   - deploy/docker-compose.local.yml  (written as docker-compose.yml)
#   - deploy/.env.example              (used to generate .env)
# They must stay byte-identical to the repository files. Regenerate them with:
#   sh deploy/tools/sync-bundled-templates.sh
# ---------------------------------------------------------------------------

write_bundled_compose() {
    cat > "$1" <<'SUB2API_COMPOSE_TEMPLATE_EOF'
# =============================================================================
# Sub2API Docker Compose - Local Directory Version
# =============================================================================
# This configuration uses local directories for data storage instead of named
# volumes, making it easy to migrate the entire deployment by simply copying
# the deploy directory.
#
# Quick Start:
#   1. Copy .env.example to .env and configure
#   2. mkdir -p data postgres_data redis_data
#   3. docker-compose -f docker-compose.local.yml up -d
#   4. Check logs: docker-compose -f docker-compose.local.yml logs -f sub2api
#   5. Access: http://localhost:8080
#
# Migration to New Server:
#   1. docker-compose -f docker-compose.local.yml down
#   2. tar czf sub2api-deploy.tar.gz deploy/
#   3. Transfer to new server and extract
#   4. docker-compose -f docker-compose.local.yml up -d
# =============================================================================

services:
  # ===========================================================================
  # Sub2API Application
  # ===========================================================================
  sub2api:
    image: weishaw/sub2api:latest
    container_name: sub2api
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    ports:
      - "${BIND_HOST:-0.0.0.0}:${SERVER_PORT:-8080}:8080"
    volumes:
      # Local directory mapping for easy migration
      - ./data:/app/data:Z
      # Optional: Mount custom config.yaml (uncomment and create the file first)
      # Copy config.example.yaml to config.yaml, modify it, then uncomment:
      # - ./config.yaml:/app/data/config.yaml
    environment:
      # =======================================================================
      # Auto Setup (REQUIRED for Docker deployment)
      # =======================================================================
      - AUTO_SETUP=true

      # =======================================================================
      # Server Configuration
      # =======================================================================
      - SERVER_HOST=0.0.0.0
      - SERVER_PORT=8080
      - SERVER_MODE=${SERVER_MODE:-release}
      - ENABLE_SERVER_TIMING=${ENABLE_SERVER_TIMING:-false}
      - RUN_MODE=${RUN_MODE:-standard}
      - UPDATE_GITHUB_TOKEN=${UPDATE_GITHUB_TOKEN:-}

      # =======================================================================
      # Database Configuration (PostgreSQL)
      # =======================================================================
      - DATABASE_HOST=postgres
      - DATABASE_PORT=5432
      - DATABASE_USER=${POSTGRES_USER:-sub2api}
      - DATABASE_PASSWORD=${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
      - DATABASE_DBNAME=${POSTGRES_DB:-sub2api}
      - DATABASE_SSLMODE=disable
      - DATABASE_MAX_OPEN_CONNS=${DATABASE_MAX_OPEN_CONNS:-50}
      - DATABASE_MAX_IDLE_CONNS=${DATABASE_MAX_IDLE_CONNS:-10}
      - DATABASE_CONN_MAX_LIFETIME_MINUTES=${DATABASE_CONN_MAX_LIFETIME_MINUTES:-30}
      - DATABASE_CONN_MAX_IDLE_TIME_MINUTES=${DATABASE_CONN_MAX_IDLE_TIME_MINUTES:-5}

      # =======================================================================
      # Redis Configuration
      # =======================================================================
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - REDIS_USERNAME=${REDIS_USERNAME:-}
      - REDIS_PASSWORD=${REDIS_PASSWORD:-}
      - REDIS_DB=${REDIS_DB:-0}
      - REDIS_POOL_SIZE=${REDIS_POOL_SIZE:-1024}
      - REDIS_MIN_IDLE_CONNS=${REDIS_MIN_IDLE_CONNS:-10}
      - REDIS_ENABLE_TLS=${REDIS_ENABLE_TLS:-false}

      # =======================================================================
      # Admin Account (auto-created on first run)
      # =======================================================================
      - ADMIN_EMAIL=${ADMIN_EMAIL:-admin@sub2api.local}
      - ADMIN_PASSWORD=${ADMIN_PASSWORD:-}

      # =======================================================================
      # JWT Configuration
      # =======================================================================
      # IMPORTANT: Set a fixed JWT_SECRET to prevent login sessions from being
      # invalidated after container restarts. If left empty, a random secret
      # will be generated on each startup.
      # Generate a secure secret: openssl rand -hex 32
      - JWT_SECRET=${JWT_SECRET:-}
      - JWT_EXPIRE_HOUR=${JWT_EXPIRE_HOUR:-24}

      # =======================================================================
      # Setup Configuration
      # =======================================================================
      - SETUP_MIGRATION_TIMEOUT_SECONDS=${SETUP_MIGRATION_TIMEOUT_SECONDS:-0}

      # =======================================================================
      # TOTP (2FA) Configuration
      # =======================================================================
      # IMPORTANT: Set a fixed encryption key for TOTP secrets. If left empty,
      # a random key will be generated on each startup, causing all existing
      # TOTP configurations to become invalid (users won't be able to login
      # with 2FA).
      # Generate a secure key: openssl rand -hex 32
      - TOTP_ENCRYPTION_KEY=${TOTP_ENCRYPTION_KEY:-}

      # =======================================================================
      # Timezone Configuration
      # This affects ALL time operations in the application:
      # - Database timestamps
      # - Usage statistics "today" boundary
      # - Subscription expiry times
      # - Log timestamps
      # Common values: Asia/Shanghai, America/New_York, Europe/London, UTC
      # =======================================================================
      - TZ=${TZ:-Asia/Shanghai}

      # =======================================================================
      # Gemini OAuth Configuration (for Gemini accounts)
      # =======================================================================
      - GEMINI_OAUTH_CLIENT_ID=${GEMINI_OAUTH_CLIENT_ID:-}
      - GEMINI_OAUTH_CLIENT_SECRET=${GEMINI_OAUTH_CLIENT_SECRET:-}
      - GEMINI_OAUTH_SCOPES=${GEMINI_OAUTH_SCOPES:-}
      - GEMINI_QUOTA_POLICY=${GEMINI_QUOTA_POLICY:-}

      # Built-in OAuth client secrets (optional)
      # SECURITY: This repo does not embed third-party client_secret.
      - GEMINI_CLI_OAUTH_CLIENT_SECRET=${GEMINI_CLI_OAUTH_CLIENT_SECRET:-}
      - ANTIGRAVITY_OAUTH_CLIENT_SECRET=${ANTIGRAVITY_OAUTH_CLIENT_SECRET:-}
      - ANTIGRAVITY_USER_AGENT_VERSION=${ANTIGRAVITY_USER_AGENT_VERSION:-}

      # =======================================================================
      # Security Configuration (URL Allowlist)
      # =======================================================================
      # Enable URL allowlist validation (false to skip allowlist checks)
      - SECURITY_URL_ALLOWLIST_ENABLED=${SECURITY_URL_ALLOWLIST_ENABLED:-false}
      # Allow insecure HTTP URLs when allowlist is disabled (default: true; set to false to require https)
      - SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=${SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP:-true}
      # Allow private IP addresses for upstream/pricing/CRS (default: true; set to false to block private hosts)
      - SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=${SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS:-true}
      # Upstream hosts whitelist (comma-separated, only used when enabled=true)
      - SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS=${SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS:-}

      # =======================================================================
      # Update Configuration (在线更新配置)
      # =======================================================================
      # Proxy for accessing GitHub (online updates + pricing data)
      # Examples: http://host:port, socks5://host:port
      - UPDATE_PROXY_URL=${UPDATE_PROXY_URL:-}

      # =======================================================================
      # Gateway Upstream, Scheduling & Image Configuration
      # =======================================================================
      # Gateway values come from Compose .env or the shell.
      # Fallbacks mirror Sub2API backend defaults so omitted values preserve behavior.
      - GATEWAY_FORCE_CODEX_CLI=${GATEWAY_FORCE_CODEX_CLI:-false}
      - GATEWAY_OPENAI_COMPACT_MODEL=${GATEWAY_OPENAI_COMPACT_MODEL:-gpt-5.5}
      - SUB2API_IMAGES_MAIN_MODEL=${SUB2API_IMAGES_MAIN_MODEL:-gpt-5.6-luna}
      - GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT=${GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT:-0}
      - GATEWAY_OPENAI_WS_FORCE_HTTP=${GATEWAY_OPENAI_WS_FORCE_HTTP:-false}
      - GATEWAY_OPENAI_HTTP2_ENABLED=${GATEWAY_OPENAI_HTTP2_ENABLED:-true}
      - GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1=${GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1:-true}
      - GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD=${GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD:-2}
      - GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS=${GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS:-60}
      - GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS=${GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS:-600}
      - GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_FAILURE_THRESHOLD=${GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_FAILURE_THRESHOLD:-2}
      - GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_WINDOW_SECONDS=${GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_WINDOW_SECONDS:-60}
      - GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_TTL_SECONDS=${GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_TTL_SECONDS:-600}
      - GATEWAY_MAX_BODY_SIZE=${GATEWAY_MAX_BODY_SIZE:-268435456}
      - GATEWAY_MAX_CONNS_PER_HOST=${GATEWAY_MAX_CONNS_PER_HOST:-1024}
      - GATEWAY_MAX_IDLE_CONNS=${GATEWAY_MAX_IDLE_CONNS:-2560}
      - GATEWAY_MAX_IDLE_CONNS_PER_HOST=${GATEWAY_MAX_IDLE_CONNS_PER_HOST:-120}
      - GATEWAY_SCHEDULING_STICKY_SESSION_MAX_WAITING=${GATEWAY_SCHEDULING_STICKY_SESSION_MAX_WAITING:-3}
      - GATEWAY_SCHEDULING_STICKY_SESSION_WAIT_TIMEOUT=${GATEWAY_SCHEDULING_STICKY_SESSION_WAIT_TIMEOUT:-120s}
      - GATEWAY_SCHEDULING_FALLBACK_WAIT_TIMEOUT=${GATEWAY_SCHEDULING_FALLBACK_WAIT_TIMEOUT:-30s}
      - GATEWAY_SCHEDULING_FALLBACK_MAX_WAITING=${GATEWAY_SCHEDULING_FALLBACK_MAX_WAITING:-100}
      - GATEWAY_SCHEDULING_LOAD_BATCH_ENABLED=${GATEWAY_SCHEDULING_LOAD_BATCH_ENABLED:-true}
      - GATEWAY_SCHEDULING_SLOT_CLEANUP_INTERVAL=${GATEWAY_SCHEDULING_SLOT_CLEANUP_INTERVAL:-30s}
      - GATEWAY_SCHEDULING_DB_FALLBACK_ENABLED=${GATEWAY_SCHEDULING_DB_FALLBACK_ENABLED:-true}
      - GATEWAY_SCHEDULING_DB_FALLBACK_TIMEOUT_SECONDS=${GATEWAY_SCHEDULING_DB_FALLBACK_TIMEOUT_SECONDS:-0}
      - GATEWAY_SCHEDULING_DB_FALLBACK_MAX_QPS=${GATEWAY_SCHEDULING_DB_FALLBACK_MAX_QPS:-0}
      - GATEWAY_SCHEDULING_OUTBOX_POLL_INTERVAL_SECONDS=${GATEWAY_SCHEDULING_OUTBOX_POLL_INTERVAL_SECONDS:-1}
      - GATEWAY_SCHEDULING_OUTBOX_LAG_WARN_SECONDS=${GATEWAY_SCHEDULING_OUTBOX_LAG_WARN_SECONDS:-5}
      - GATEWAY_SCHEDULING_OUTBOX_LAG_REBUILD_SECONDS=${GATEWAY_SCHEDULING_OUTBOX_LAG_REBUILD_SECONDS:-10}
      - GATEWAY_SCHEDULING_OUTBOX_LAG_REBUILD_FAILURES=${GATEWAY_SCHEDULING_OUTBOX_LAG_REBUILD_FAILURES:-3}
      - GATEWAY_SCHEDULING_OUTBOX_BACKLOG_REBUILD_ROWS=${GATEWAY_SCHEDULING_OUTBOX_BACKLOG_REBUILD_ROWS:-10000}
      - GATEWAY_SCHEDULING_FULL_REBUILD_INTERVAL_SECONDS=${GATEWAY_SCHEDULING_FULL_REBUILD_INTERVAL_SECONDS:-300}
      - GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT=${GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT:-900}
      - GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL=${GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL:-10}
      - GATEWAY_IMAGE_NONSTREAM_KEEPALIVE_INTERVAL=${GATEWAY_IMAGE_NONSTREAM_KEEPALIVE_INTERVAL:-0}
      - GATEWAY_IMAGE_CONCURRENCY_ENABLED=${GATEWAY_IMAGE_CONCURRENCY_ENABLED:-false}
      - GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS=${GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS:-0}
      - GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE=${GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE:-reject}
      - GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS=${GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS:-30}
      - GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS=${GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS:-100}
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      - sub2api-network
    healthcheck:
      test: ["CMD", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://localhost:8080/health"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 30s

  # ===========================================================================
  # PostgreSQL Database
  # ===========================================================================
  postgres:
    image: postgres:18-alpine
    container_name: sub2api-postgres
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    volumes:
      # Local directory mapping for easy migration
      - ./postgres_data:/var/lib/postgresql/data:Z
    environment:
      - POSTGRES_USER=${POSTGRES_USER:-sub2api}
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
      - POSTGRES_DB=${POSTGRES_DB:-sub2api}
      - PGDATA=/var/lib/postgresql/data
      - TZ=${TZ:-Asia/Shanghai}
    networks:
      - sub2api-network
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-sub2api} -d ${POSTGRES_DB:-sub2api}"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 10s
    # 注意：不暴露端口到宿主机，应用通过内部网络连接
    # 如需调试，可临时添加：ports: ["127.0.0.1:5433:5432"]

  # ===========================================================================
  # Redis Cache
  # ===========================================================================
  redis:
    image: redis:8-alpine
    container_name: sub2api-redis
    restart: unless-stopped
    ulimits:
      nofile:
        soft: 100000
        hard: 100000
    volumes:
      # Local directory mapping for easy migration
      - ./redis_data:/data:Z
    # The command is one quoted script for the inner `sh -c`. Compose keeps
    # the newlines inside the quoted string, so every line needs a trailing
    # `\` — without it, `redis-server` on the first line runs with no flags
    # at all, and the --save/--appendonly/--appendfsync lines are never read.
    command: >
        sh -c '
          redis-server \
          --save 60 1 \
          --appendonly yes \
          --appendfsync everysec \
          ${REDIS_PASSWORD:+--requirepass "$REDIS_PASSWORD"}'
    environment:
      - TZ=${TZ:-Asia/Shanghai}
      # REDISCLI_AUTH is used by redis-cli for authentication (safer than -a flag)
      - REDISCLI_AUTH=${REDIS_PASSWORD:-}
    networks:
      - sub2api-network
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 5s

# =============================================================================
# Networks
# =============================================================================
networks:
  sub2api-network:
    driver: bridge
SUB2API_COMPOSE_TEMPLATE_EOF
}

write_bundled_env_example() {
    cat > "$1" <<'SUB2API_ENV_TEMPLATE_EOF'
# =============================================================================
# Sub2API Container Environment Configuration
# =============================================================================
# Copy this file to .env and modify as needed:
#   cp .env.example .env
#   chmod 600 .env
#   nano .env
#
# Then start with Docker Compose or Apple container:
#   docker compose up -d
#   ./apple-container.sh up
# =============================================================================

# -----------------------------------------------------------------------------
# Server Configuration
# -----------------------------------------------------------------------------
# IPv4 bind address for host port mapping
BIND_HOST=0.0.0.0

# Server port exposed on the host (Apple container requires 1025-65535)
SERVER_PORT=8080

# Server mode: release or debug
SERVER_MODE=release

# Optional token used only for GitHub Release API update checks. Release asset
# downloads remain anonymous. GITHUB_TOKEN and GH_TOKEN are not used.
UPDATE_GITHUB_TOKEN=

# Return Server-Timing for authenticated requests made by the Admin web UI
ENABLE_SERVER_TIMING=false

# Apple container image overrides (ignored by Docker Compose). Pin release tags
# or digests for repeatable operator-managed deployments.
APPLE_CONTAINER_SUB2API_IMAGE=weishaw/sub2api:latest
APPLE_CONTAINER_POSTGRES_IMAGE=postgres:18-alpine
APPLE_CONTAINER_REDIS_IMAGE=redis:8-alpine

# Optional fixed IPv4 CIDR for the private Apple container network. Leave empty
# to let Apple container select one. Set only when host tooling needs a stable
# network gateway, and choose a CIDR that does not overlap your LAN or VPN.
APPLE_CONTAINER_NETWORK_SUBNET=

# -----------------------------------------------------------------------------
# Logging Configuration
# 日志配置
# -----------------------------------------------------------------------------
# 日志级别：debug/info/warn/error
LOG_LEVEL=info
# 日志格式：json/console
LOG_FORMAT=json
# 每条日志附带的 service 字段
LOG_SERVICE_NAME=sub2api
# 每条日志附带的 env 字段
LOG_ENV=production
# 是否输出调用方位置信息
LOG_CALLER=true
# 堆栈输出阈值：none/error/fatal
LOG_STACKTRACE_LEVEL=error

# 输出开关（建议容器内保持双输出）
# 是否输出到 stdout/stderr
LOG_OUTPUT_TO_STDOUT=true
# 是否输出到文件
LOG_OUTPUT_TO_FILE=true
# 日志文件路径（留空自动推导）：
# - 设置 DATA_DIR：${DATA_DIR}/logs/sub2api.log
# - 未设置 DATA_DIR：/app/data/logs/sub2api.log
LOG_OUTPUT_FILE_PATH=

# 滚动配置
# 单文件最大体积（MB）
LOG_ROTATION_MAX_SIZE_MB=100
# 保留历史文件数量（0 表示不限制）
LOG_ROTATION_MAX_BACKUPS=10
# 历史日志保留天数（0 表示不限制）
LOG_ROTATION_MAX_AGE_DAYS=7
# 是否压缩历史日志
LOG_ROTATION_COMPRESS=true
# 滚动文件时间戳是否使用本地时间
LOG_ROTATION_LOCAL_TIME=true

# 采样配置（高频重复日志降噪）
LOG_SAMPLING_ENABLED=false
# 每秒前 N 条日志不采样
LOG_SAMPLING_INITIAL=100
# 之后每 N 条保留 1 条
LOG_SAMPLING_THEREAFTER=100

# Global max request body size in bytes (default: 256MB)
# 全局最大请求体大小（字节，默认 256MB）
# Applies to all requests, especially important for h2c first request memory protection
# 适用于所有请求，对 h2c 第一请求的内存保护尤为重要
SERVER_MAX_REQUEST_BODY_SIZE=268435456

# Gateway max request body size in bytes (default: 256MB)
# 网关请求体最大字节数（默认 256MB）
GATEWAY_MAX_BODY_SIZE=268435456

# Enable HTTP/2 Cleartext (h2c) for client connections
# 启用 HTTP/2 Cleartext (h2c) 客户端连接
SERVER_H2C_ENABLED=true
# H2C max concurrent streams (default: 50)
# H2C 最大并发流数量（默认 50）
SERVER_H2C_MAX_CONCURRENT_STREAMS=50
# H2C idle timeout in seconds (default: 75)
# H2C 空闲超时时间（秒，默认 75）
SERVER_H2C_IDLE_TIMEOUT=75
# H2C max read frame size in bytes (default: 1048576 = 1MB)
# H2C 最大帧大小（字节，默认 1048576 = 1MB）
SERVER_H2C_MAX_READ_FRAME_SIZE=1048576
# H2C max upload buffer per connection in bytes (default: 2097152 = 2MB)
# H2C 每个连接的最大上传缓冲区（字节，默认 2097152 = 2MB）
SERVER_H2C_MAX_UPLOAD_BUFFER_PER_CONNECTION=2097152
# H2C max upload buffer per stream in bytes (default: 524288 = 512KB)
# H2C 每个流的最大上传缓冲区（字节，默认 524288 = 512KB）
SERVER_H2C_MAX_UPLOAD_BUFFER_PER_STREAM=524288

# 运行模式: standard (默认) 或 simple (内部自用)
# standard: 完整 SaaS 功能，包含计费/余额校验；simple: 隐藏 SaaS 功能并跳过计费/余额校验
RUN_MODE=standard

# Timezone
TZ=Asia/Shanghai

# Optional mobile Alipay flow. Unset uses the value saved in Admin Settings.
# Enable only for official Alipay instances with face-to-face payment enabled.
# ALIPAY_MOBILE_PRECREATE_DEEP_LINK=true

# -----------------------------------------------------------------------------
# PostgreSQL Configuration (REQUIRED)
# -----------------------------------------------------------------------------
POSTGRES_USER=sub2api
POSTGRES_PASSWORD=change_this_secure_password
POSTGRES_DB=sub2api
# PostgreSQL 监听端口（同时用于 PG 服务端和应用连接，默认 5432）
DATABASE_PORT=5432

# -----------------------------------------------------------------------------
# PostgreSQL 服务端参数（可选）
# -----------------------------------------------------------------------------
# POSTGRES_MAX_CONNECTIONS：PostgreSQL 服务端允许的最大连接数。
# 必须 >=（所有 Sub2API 实例的 DATABASE_MAX_OPEN_CONNS 之和）+ 预留余量（例如 20%）。
POSTGRES_MAX_CONNECTIONS=1024
# POSTGRES_SHARED_BUFFERS：PostgreSQL 用于缓存数据页的共享内存。
# 常见建议：物理内存的 10%~25%（容器内存受限时请按实际限制调整）。
# 8GB 内存容器参考：1GB。
POSTGRES_SHARED_BUFFERS=1GB
# POSTGRES_EFFECTIVE_CACHE_SIZE：查询规划器“假设可用的 OS 缓存大小”（不等于实际分配）。
# 常见建议：物理内存的 50%~75%。
# 8GB 内存容器参考：6GB。
POSTGRES_EFFECTIVE_CACHE_SIZE=4GB
# POSTGRES_MAINTENANCE_WORK_MEM：维护操作内存（VACUUM/CREATE INDEX 等）。
# 值越大维护越快，但会占用更多内存。
# 8GB 内存容器参考：128MB。
POSTGRES_MAINTENANCE_WORK_MEM=128MB

# -----------------------------------------------------------------------------
# PostgreSQL 连接池参数（可选，默认与程序内置一致）
# -----------------------------------------------------------------------------
# 说明：
# - 这些参数控制 Sub2API 进程到 PostgreSQL 的连接池大小（不是 PostgreSQL 自身的 max_connections）。
# - 多实例/多副本部署时，总连接上限约等于：实例数 * DATABASE_MAX_OPEN_CONNS。
# - 连接池过大可能导致：数据库连接耗尽、内存占用上升、上下文切换增多，反而变慢。
# - 建议结合 PostgreSQL 的 max_connections 与机器规格逐步调优：
#   通常把应用总连接上限控制在 max_connections 的 50%~80% 更稳妥。
#
# DATABASE_MAX_OPEN_CONNS：最大打开连接数（活跃+空闲），达到后新请求会等待可用连接。
# 典型范围：50~500（取决于 DB 规格、实例数、SQL 复杂度）。
DATABASE_MAX_OPEN_CONNS=256
# DATABASE_MAX_IDLE_CONNS：最大空闲连接数（热连接），建议 <= MAX_OPEN。
# 太小会频繁建连增加延迟；太大会长期占用数据库资源。
DATABASE_MAX_IDLE_CONNS=128
# DATABASE_CONN_MAX_LIFETIME_MINUTES：单个连接最大存活时间（单位：分钟）。
# 用于避免连接长期不重建导致的中间件/LB/NAT 异常或服务端重启后的“僵尸连接”。
# 设置为 0 表示不限制（一般不建议生产环境）。
DATABASE_CONN_MAX_LIFETIME_MINUTES=30
# DATABASE_CONN_MAX_IDLE_TIME_MINUTES：空闲连接最大存活时间（单位：分钟）。
# 超过该时间的空闲连接会被回收，防止长时间闲置占用连接数。
# 设置为 0 表示不限制（一般不建议生产环境）。
DATABASE_CONN_MAX_IDLE_TIME_MINUTES=5

# -----------------------------------------------------------------------------
# Redis Configuration
# -----------------------------------------------------------------------------
# Redis 监听端口（同时用于应用连接和 Redis 服务端，默认 6379）
REDIS_PORT=6379
# Redis ACL username; leave empty for the default user
REDIS_USERNAME=
# Leave empty for no password (default for local development)
REDIS_PASSWORD=
REDIS_DB=0
# Redis 服务端最大客户端连接数（可选）
REDIS_MAXCLIENTS=50000
# Redis 连接池大小（默认 1024）
REDIS_POOL_SIZE=4096
# Redis 最小空闲连接数（默认 10）
REDIS_MIN_IDLE_CONNS=256
REDIS_ENABLE_TLS=false

# -----------------------------------------------------------------------------
# Admin Account
# -----------------------------------------------------------------------------
# Email for the admin account
ADMIN_EMAIL=admin@sub2api.local

# Password for admin account
# Leave empty to auto-generate (will be shown in logs on first run)
ADMIN_PASSWORD=

# -----------------------------------------------------------------------------
# JWT Configuration
# -----------------------------------------------------------------------------
# IMPORTANT: Set a fixed JWT_SECRET to prevent login sessions from being
# invalidated after container restarts. If left empty, a random secret will
# be generated on each startup, causing all users to be logged out.
# Generate a secure secret: openssl rand -hex 32
JWT_SECRET=
JWT_EXPIRE_HOUR=24
# Access Token 有效期（分钟）
# 优先级说明：
# - >0: 按分钟生效（优先于 JWT_EXPIRE_HOUR）
# - =0: 回退使用 JWT_EXPIRE_HOUR
JWT_ACCESS_TOKEN_EXPIRE_MINUTES=0

# -----------------------------------------------------------------------------
# Setup Configuration
# -----------------------------------------------------------------------------
# Database migration timeout during initial setup, in seconds.
# Leave 0 to use the built-in default of 60 seconds.
SETUP_MIGRATION_TIMEOUT_SECONDS=0

# -----------------------------------------------------------------------------
# TOTP (2FA) Configuration
# TOTP（双因素认证）配置
# -----------------------------------------------------------------------------
# IMPORTANT: Set a fixed encryption key for TOTP secrets. If left empty, a
# random key will be generated on each startup, causing all existing TOTP
# configurations to become invalid (users won't be able to login with 2FA).
# Generate a secure key: openssl rand -hex 32
# 重要：设置固定的 TOTP 加密密钥。如果留空，每次启动将生成随机密钥，
# 导致现有的 TOTP 配置失效（用户无法使用双因素认证登录）。
TOTP_ENCRYPTION_KEY=

# -----------------------------------------------------------------------------
# Configuration File (Optional)
# -----------------------------------------------------------------------------
# Path to custom config file (relative to docker-compose.yml directory)
# Copy config.example.yaml to config.yaml and modify as needed
# Leave unset to use default ./config.yaml
#CONFIG_FILE=./config.yaml

# -----------------------------------------------------------------------------
# Built-in OAuth Client Secrets (Optional)
# -----------------------------------------------------------------------------
# SECURITY NOTE:
# - 本项目不会在代码仓库中内置第三方 OAuth client_secret。
# - 如需使用“内置客户端”（而不是自建 OAuth Client），请在运行环境通过 env 注入。
#
# Gemini CLI built-in OAuth client_secret（用于 Gemini code_assist/google_one 内置登录流）
# GEMINI_CLI_OAUTH_CLIENT_SECRET=
#
# Antigravity OAuth client_secret（用于 Antigravity OAuth 登录流）
# ANTIGRAVITY_OAUTH_CLIENT_SECRET=
#
# Antigravity User-Agent 版本号（后台设置 antigravity_user_agent_version 优先；留空使用内置默认 1.23.2）
# ANTIGRAVITY_USER_AGENT_VERSION=

# -----------------------------------------------------------------------------
# Rate Limiting (Optional)
# 速率限制（可选）
# -----------------------------------------------------------------------------
# Cooldown time (in minutes) when upstream returns 529 (overloaded)
# 上游返回 529（过载）时的冷却时间（分钟）
RATE_LIMIT_OVERLOAD_COOLDOWN_MINUTES=10

# -----------------------------------------------------------------------------
# Gateway Scheduling (Optional)
# 调度缓存与受控回源配置（缓存就绪且命中时不读 DB）
# -----------------------------------------------------------------------------
# Force Codex CLI mode: treat all /openai/v1/responses requests as Codex CLI.
# 强制按 Codex CLI 处理 /openai/v1/responses 请求（用于网关未透传/改写 User-Agent 的兜底）。
#
# 注意：开启后会影响所有客户端的行为（不仅限于 VS Code / Codex CLI），请谨慎开启。
#
# 默认：false
GATEWAY_FORCE_CODEX_CLI=false
# OpenAI /responses/compact 上游模型（默认 gpt-5.5）。
# 当 compact 端点暂未支持更新模型时，可通过这里降级规避失败。
GATEWAY_OPENAI_COMPACT_MODEL=gpt-5.5

# OAuth Images /responses 主控模型，与 image_generation.model 独立。
# 上游下线主控模型时可修改此项并重建/重启容器，无需重新编译。
SUB2API_IMAGES_MAIN_MODEL=gpt-5.6-luna
# OpenAI/Codex 等待上游响应头超时（秒）；0 表示不使用本地响应头超时截断。
GATEWAY_OPENAI_RESPONSE_HEADER_TIMEOUT=0
# 全局强制 OpenAI Responses 上游使用 HTTP/SSE，不使用 WebSocket。
# 当代理或网络导致上游 WebSocket 反复重连时可设为 true；这不会禁用 HTTP/2。
# 如需回退 HTTP/1.1，请另行设置 GATEWAY_OPENAI_HTTP2_ENABLED=false。
# 将此项保存在持久化 .env 中，镜像更新或容器重建后会在启动时重新读取。
GATEWAY_OPENAI_WS_FORCE_HTTP=false
# OpenAI HTTP 上游默认启用 HTTP/2；如需紧急回滚可设为 false。
GATEWAY_OPENAI_HTTP2_ENABLED=true
GATEWAY_OPENAI_HTTP2_ALLOW_PROXY_FALLBACK_TO_HTTP1=true
GATEWAY_OPENAI_HTTP2_FALLBACK_ERROR_THRESHOLD=2
GATEWAY_OPENAI_HTTP2_FALLBACK_WINDOW_SECONDS=60
GATEWAY_OPENAI_HTTP2_FALLBACK_TTL_SECONDS=600
GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_FAILURE_THRESHOLD=2
GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_WINDOW_SECONDS=60
GATEWAY_OPENAI_PROXY_STREAM_CIRCUIT_TTL_SECONDS=600
# 上游连接池：每主机最大连接数（默认 1024；流式/HTTP1.1 场景可调大，如 2400/4096）
GATEWAY_MAX_CONNS_PER_HOST=2048
# 上游连接池：最大空闲连接总数（默认 2560；账号/代理隔离 + 高并发场景可调大）
GATEWAY_MAX_IDLE_CONNS=8192
# 上游连接池：每主机最大空闲连接（默认 120）
GATEWAY_MAX_IDLE_CONNS_PER_HOST=4096
# 粘性会话最大排队长度
GATEWAY_SCHEDULING_STICKY_SESSION_MAX_WAITING=3
# 粘性会话等待超时（时间段，例如 45s）
GATEWAY_SCHEDULING_STICKY_SESSION_WAIT_TIMEOUT=120s
# 兜底排队等待超时（时间段，例如 30s）
GATEWAY_SCHEDULING_FALLBACK_WAIT_TIMEOUT=30s
# 兜底最大排队长度
GATEWAY_SCHEDULING_FALLBACK_MAX_WAITING=100
# 启用调度批量负载计算
GATEWAY_SCHEDULING_LOAD_BATCH_ENABLED=true
# 并发槽位清理周期（时间段，例如 30s）
GATEWAY_SCHEDULING_SLOT_CLEANUP_INTERVAL=30s
# 是否允许受控回源到 DB（默认 true，保持现有行为）
GATEWAY_SCHEDULING_DB_FALLBACK_ENABLED=true
# 受控回源超时（秒），0 表示不额外收紧超时
GATEWAY_SCHEDULING_DB_FALLBACK_TIMEOUT_SECONDS=0
# 受控回源限流（实例级 QPS），0 表示不限制
GATEWAY_SCHEDULING_DB_FALLBACK_MAX_QPS=0
# outbox 轮询周期（秒）
GATEWAY_SCHEDULING_OUTBOX_POLL_INTERVAL_SECONDS=1
# outbox 滞后告警阈值（秒）
GATEWAY_SCHEDULING_OUTBOX_LAG_WARN_SECONDS=5
# outbox 触发强制重建阈值（秒）
GATEWAY_SCHEDULING_OUTBOX_LAG_REBUILD_SECONDS=10
# outbox 连续滞后触发次数
GATEWAY_SCHEDULING_OUTBOX_LAG_REBUILD_FAILURES=3
# outbox 积压触发重建阈值（行数）
GATEWAY_SCHEDULING_OUTBOX_BACKLOG_REBUILD_ROWS=10000
# 全量重建周期（秒）
GATEWAY_SCHEDULING_FULL_REBUILD_INTERVAL_SECONDS=300

# -----------------------------------------------------------------------------
# Image Generation Keepalive & Concurrency (Optional)
# 图片生成保活与并发隔离配置（可选）
# -----------------------------------------------------------------------------
# 图片流式上游数据间隔超时（秒）。0 表示禁用；非 0 时必须为 60-1800。
GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT=900
# 图片流式 keepalive 间隔（秒）。0 表示禁用；非 0 时必须为 5-60。
GATEWAY_IMAGE_STREAM_KEEPALIVE_INTERVAL=10
# 图片非流式 JSON keepalive 间隔（秒）。默认 0 禁用；首个心跳后 HTTP 状态会固化为 200。
GATEWAY_IMAGE_NONSTREAM_KEEPALIVE_INTERVAL=0
# 是否启用进程级图片生成并发限制。默认 false，保持历史行为。
GATEWAY_IMAGE_CONCURRENCY_ENABLED=false
# 当前进程允许同时处理的图片生成请求数。0 表示不限制。
GATEWAY_IMAGE_CONCURRENCY_MAX_CONCURRENT_REQUESTS=0
# 图片并发超限策略：reject 直接返回 429；wait 等待空闲槽位。
GATEWAY_IMAGE_CONCURRENCY_OVERFLOW_MODE=reject
# wait 模式下等待空闲图片槽位的最长时间（秒）。
GATEWAY_IMAGE_CONCURRENCY_WAIT_TIMEOUT_SECONDS=30
# wait 模式下当前进程允许排队等待的最大图片请求数。0 表示不允许等待队列。
GATEWAY_IMAGE_CONCURRENCY_MAX_WAITING_REQUESTS=100

# -----------------------------------------------------------------------------
# Dashboard Aggregation (Optional)
# -----------------------------------------------------------------------------
# Enable aggregation job
# 启用仪表盘预聚合
DASHBOARD_AGGREGATION_ENABLED=true
# Refresh interval (seconds)
# 刷新间隔（秒）
DASHBOARD_AGGREGATION_INTERVAL_SECONDS=60
# Lookback window (seconds)
# 回看窗口（秒）
DASHBOARD_AGGREGATION_LOOKBACK_SECONDS=120
# Allow manual backfill
# 允许手动回填
DASHBOARD_AGGREGATION_BACKFILL_ENABLED=false
# Backfill max range (days)
# 回填最大跨度（天）
DASHBOARD_AGGREGATION_BACKFILL_MAX_DAYS=31
# Recompute recent N days on startup
# 启动时重算最近 N 天
DASHBOARD_AGGREGATION_RECOMPUTE_DAYS=2
# Retention windows (days)
# 保留窗口（天）
DASHBOARD_AGGREGATION_RETENTION_USAGE_LOGS_DAYS=90
DASHBOARD_AGGREGATION_RETENTION_HOURLY_DAYS=180
DASHBOARD_AGGREGATION_RETENTION_DAILY_DAYS=730

# -----------------------------------------------------------------------------
# Security Configuration
# -----------------------------------------------------------------------------
# URL Allowlist Configuration
# 启用 URL 白名单验证（false 则跳过白名单检查，仅做基本格式校验）
SECURITY_URL_ALLOWLIST_ENABLED=false

# 关闭白名单时，是否允许 http:// URL（默认 true，设为 false 则只允许 https://）
# ⚠️ 警告：允许 HTTP 存在安全风险（明文传输），生产环境建议设为 false
# Allow insecure HTTP URLs when allowlist is disabled (default: true; set to false to require https)
# ⚠️ WARNING: Allowing HTTP has security risks (plaintext transmission)
#             Recommended to set false in production
SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=true

# 是否允许本地/私有 IP 地址用于上游/定价/CRS（仅在可信网络中使用）
# Allow localhost/private IPs for upstream/pricing/CRS (use only in trusted networks)
SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=true

# -----------------------------------------------------------------------------
# Gemini OAuth (OPTIONAL, required only for Gemini OAuth accounts)
# -----------------------------------------------------------------------------
# Sub2API supports TWO Gemini OAuth modes:
#
# 1. Code Assist OAuth (需要 GCP project_id)
#    - Uses: cloudcode-pa.googleapis.com (Code Assist API)
#    - Auto scopes: cloud-platform + userinfo.email + userinfo.profile
#    - OAuth Client: Can use built-in Gemini CLI client (留空即可)
#    - Requires: Google Cloud Platform project with Code Assist enabled
#
# 2. AI Studio OAuth (不需要 project_id)
#    - Uses: generativelanguage.googleapis.com (AI Studio API)
#    - Default scopes: generative-language
#    - OAuth Client: Requires your own OAuth 2.0 Client (内置 Gemini CLI client 不能申请 generative-language scope)
#    - Requires: Create OAuth 2.0 Client in GCP Console + OAuth consent screen
#    - Setup Guide: https://ai.google.dev/gemini-api/docs/oauth
#    - ⚠️ IMPORTANT: OAuth Client 必须发布为正式版本 (Production)
#      Testing 模式限制: 只能添加 100 个测试用户, refresh token 7 天后过期
#      发布步骤: GCP Console → OAuth consent screen → PUBLISH APP
#
# Configuration:
# Leave empty to use the built-in Gemini CLI OAuth client (Code Assist OAuth only).
# To enable AI Studio OAuth, set your own OAuth client ID/secret here.
GEMINI_OAUTH_CLIENT_ID=
GEMINI_OAUTH_CLIENT_SECRET=
# Optional; leave empty to auto-select scopes based on oauth_type
GEMINI_OAUTH_SCOPES=

# -----------------------------------------------------------------------------
# Gemini Quota Policy (OPTIONAL, local simulation)
# -----------------------------------------------------------------------------
# JSON overrides for local quota simulation (Code Assist only).
# Example:
# GEMINI_QUOTA_POLICY={"tiers":{"LEGACY":{"pro_rpd":50,"flash_rpd":1500,"cooldown_minutes":30},"PRO":{"pro_rpd":1500,"flash_rpd":4000,"cooldown_minutes":5},"ULTRA":{"pro_rpd":2000,"flash_rpd":0,"cooldown_minutes":5}}}
GEMINI_QUOTA_POLICY=

# -----------------------------------------------------------------------------
# Ops Monitoring Configuration (运维监控配置)
# -----------------------------------------------------------------------------
# Enable ops monitoring features (background jobs and APIs)
# 是否启用运维监控功能（后台任务和接口）
# Set to false to hide ops menu in sidebar and disable all ops features
# 设置为 false 可在左侧栏隐藏运维监控菜单并禁用所有运维监控功能
OPS_ENABLED=true

# -----------------------------------------------------------------------------
# Update Configuration (在线更新配置)
# -----------------------------------------------------------------------------
# Proxy URL for accessing GitHub (used for online updates and pricing data)
# 用于访问 GitHub 的代理地址（用于在线更新和定价数据获取）
# Supports: http, https, socks5, socks5h
# Examples:
#   HTTP proxy: http://127.0.0.1:7890
#   SOCKS5 proxy: socks5://127.0.0.1:1080
#   With authentication: http://user:pass@proxy.example.com:8080
# Leave empty for direct connection (recommended for overseas servers)
# 留空表示直连（适用于海外服务器）
UPDATE_PROXY_URL=
SUB2API_ENV_TEMPLATE_EOF
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

usage() {
    cat <<'SUB2API_USAGE_EOF'
Usage: ./docker-deploy.sh [options]

One-click Sub2API deployment. Prepares docker-compose.yml and .env from the
templates bundled in this script, generates secure secrets, and starts the
stack with Docker Compose.

Options:
  --no-start    Prepare deployment files only; do not start services
  --force       Regenerate deployment files and secrets. Use this only for a
                fresh deployment: regenerating POSTGRES_PASSWORD breaks access
                to data in an existing postgres_data/ directory
  -h, --help    Show this help

Environment:
  SUB2API_DEPLOY_HEALTH_TIMEOUT   Seconds to wait for the application health
                                  check to pass (default: 600)
SUB2API_USAGE_EOF
}

generate_secret() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    elif [ -r /dev/urandom ]; then
        od -An -v -tx1 -N32 /dev/urandom | tr -d ' \n'
        printf '\n'
    else
        print_error "Cannot generate a secure secret: install openssl or provide /dev/urandom."
        exit 1
    fi
}

# replace_env_value <file> <key> <value>
# Replaces the first "<key>=..." line, leaving every other line untouched.
replace_env_value() {
    env_file="$1"
    key="$2"
    value="$3"
    awk -v key="$key" -v value="$value" '
        BEGIN { replaced = 0 }
        $0 ~ ("^" key "=") && !replaced {
            print key "=" value
            replaced = 1
            next
        }
        { print }
    ' "$env_file" > "$env_file.tmp"
    mv "$env_file.tmp" "$env_file"
}

# env_value <key> <default>
env_value() {
    key="$1"
    fallback="$2"
    value=$(grep -E "^${key}=" .env 2>/dev/null | head -n 1 | cut -d= -f2- || true)
    if [ -n "$value" ]; then
        printf '%s' "$value"
    else
        printf '%s' "$fallback"
    fi
}

# require_secret_value <key>
# Fails when the generated secret is missing or malformed.
require_secret_value() {
    key="$1"
    value=$(env_value "$key" "")
    length=$(printf '%s' "$value" | wc -c | tr -d ' ')
    if [ "$length" -ne 64 ]; then
        print_error "Failed to generate a valid ${key} in .env (length ${length}, expected 64)."
        exit 1
    fi
}

check_docker_available() {
    if ! command -v docker >/dev/null 2>&1; then
        print_error "Docker is not installed or not in PATH."
        print_info "Deployment files are ready in the current directory."
        print_info "Install Docker (20.10+) with the Compose v2 plugin, then run: docker compose up -d"
        exit 1
    fi
    if ! docker compose version >/dev/null 2>&1; then
        print_error "Docker Compose v2 is required (the 'docker compose' plugin)."
        print_info "Deployment files are ready in the current directory."
        print_info "Install the Compose v2 plugin, then run: docker compose up -d"
        exit 1
    fi
}

# Waits until the sub2api container reports a healthy status.
wait_for_health() {
    timeout_seconds="${SUB2API_DEPLOY_HEALTH_TIMEOUT:-600}"
    case "$timeout_seconds" in
        ''|*[!0-9]*) timeout_seconds=600 ;;
    esac

    elapsed=0
    interval=5
    last_status=""

    while [ "$elapsed" -lt "$timeout_seconds" ]; do
        status=$(docker inspect --format '{{.State.Health.Status}}' sub2api 2>/dev/null || true)
        if [ "$status" != "$last_status" ]; then
            case "$status" in
                healthy)
                    print_success "Application health check passed."
                    ;;
                unhealthy)
                    print_warning "Application health check is failing; still waiting..."
                    ;;
                starting)
                    print_info "Application is starting (health check pending)..."
                    ;;
                *)
                    print_info "Waiting for the sub2api container to start..."
                    ;;
            esac
            last_status="$status"
        fi
        if [ "$status" = "healthy" ]; then
            return 0
        fi
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done
    return 1
}

print_completion() {
    secrets_generated="$1"
    services_started="$2"

    echo "=========================================="
    print_success "Deployment complete!"
    echo "=========================================="
    echo ""

    if [ "$secrets_generated" = true ]; then
        echo "Generated secure credentials (saved to .env):"
        echo "  POSTGRES_PASSWORD:     ${postgres_password}"
        echo "  JWT_SECRET:            ${jwt_secret}"
        echo "  TOTP_ENCRYPTION_KEY:   ${totp_key}"
        echo ""
        print_warning "Keep these credentials safe and do not share them publicly!"
    else
        print_info "Existing credentials are kept in .env."
        print_info "View them with: grep -E '^(JWT_SECRET|TOTP_ENCRYPTION_KEY|POSTGRES_PASSWORD)=' .env"
    fi
    echo ""

    echo "Deployment files:"
    echo "  docker-compose.yml    Docker Compose configuration"
    echo "  .env                  Environment variables (generated secrets)"
    echo "  data/                 Application data"
    echo "  postgres_data/        PostgreSQL data"
    echo "  redis_data/           Redis data"
    echo ""

    server_port=$(env_value "SERVER_PORT" "8080")
    echo "Access Web UI:"
    echo "  http://localhost:${server_port}"
    echo ""
    echo "Useful commands:"
    echo "  docker compose logs -f sub2api                # follow application logs"
    echo "  docker compose ps                             # show container status"
    echo "  docker compose restart sub2api                # restart the application"
    echo "  docker compose pull && docker compose up -d   # update to the latest image"
    echo "  docker compose down                           # stop the stack (data is preserved)"
    echo ""
    print_info "If ADMIN_PASSWORD is not set in .env, the admin password is auto-generated on"
    print_info "first startup. Find it with:"
    print_info "  docker compose logs sub2api | grep \"admin password\""
    echo ""

    if [ "$services_started" != true ]; then
        print_warning "Services are not running yet. Start them with: docker compose up -d"
        echo ""
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    FORCE=false
    START=true

    for arg in "$@"; do
        case "$arg" in
            --no-start) START=false ;;
            --force) FORCE=true ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                print_error "Unknown option: $arg"
                echo ""
                usage
                exit 2
                ;;
        esac
    done

    echo ""
    echo "=========================================="
    echo "  Sub2API One-Click Docker Deployment"
    echo "=========================================="
    echo ""

    # Refuse to run inside the repository's deploy/ directory: the tracked
    # docker-compose.local.yml next to this script identifies a source
    # checkout, and the script writes docker-compose.yml / .env.example /
    # .env into the current directory.
    if [ -f docker-compose.local.yml ]; then
        print_error "This looks like the repository's deploy/ directory (docker-compose.local.yml found)."
        print_info "The script writes its deployment files into the current directory, so run it"
        print_info "from a separate empty directory instead, for example:"
        print_info "  mkdir -p ~/sub2api-deploy && cd ~/sub2api-deploy"
        print_info "  bash /path/to/docker-deploy.sh"
        print_info "See deploy/README.md for details."
        exit 1
    fi

    if ! command -v openssl >/dev/null 2>&1 && [ ! -r /dev/urandom ]; then
        print_error "openssl (or /dev/urandom) is required to generate secure secrets."
        exit 1
    fi

    env_exists=false
    if [ -f .env ]; then
        env_exists=true
    fi

    if [ "$env_exists" = true ]; then
        if [ "$FORCE" = true ]; then
            print_warning "Existing .env detected; regenerating secrets (--force)."
            print_warning "A regenerated POSTGRES_PASSWORD will NOT match existing PostgreSQL data"
            print_warning "in postgres_data/. Use --force only for a fresh deployment."
        else
            print_warning "Existing .env detected: keeping the current credentials."
            print_warning "Pass --force only to regenerate secrets for a fresh deployment."
        fi
    fi

    # docker-compose.yml (bundled local-directory template)
    tmp_compose=".docker-compose.yml.new"
    write_bundled_compose "$tmp_compose"
    if [ -f docker-compose.yml ] && ! cmp -s docker-compose.yml "$tmp_compose"; then
        cp docker-compose.yml docker-compose.yml.bak
        print_warning "Existing docker-compose.yml differs; backed up to docker-compose.yml.bak"
    fi
    mv "$tmp_compose" docker-compose.yml
    print_success "Wrote docker-compose.yml"

    # .env.example (reference copy of the bundled template)
    write_bundled_env_example ".env.example"
    print_success "Wrote .env.example"

    # .env (generated secrets)
    secrets_generated=false
    jwt_secret=""
    totp_key=""
    postgres_password=""

    if [ "$env_exists" != true ] || [ "$FORCE" = true ]; then
        write_bundled_env_example ".env"
        jwt_secret=$(generate_secret)
        totp_key=$(generate_secret)
        postgres_password=$(generate_secret)
        replace_env_value ".env" "JWT_SECRET" "$jwt_secret"
        replace_env_value ".env" "TOTP_ENCRYPTION_KEY" "$totp_key"
        replace_env_value ".env" "POSTGRES_PASSWORD" "$postgres_password"
        require_secret_value "JWT_SECRET"
        require_secret_value "TOTP_ENCRYPTION_KEY"
        require_secret_value "POSTGRES_PASSWORD"
        chmod 600 .env
        secrets_generated=true
        print_success "Generated .env with secure credentials"
    else
        print_info "Keeping existing .env"
    fi

    # Data directories (local directories for easy backup/migration)
    mkdir -p data postgres_data redis_data
    print_success "Created data directories (data/, postgres_data/, redis_data/)"
    echo ""

    if [ "$START" != true ]; then
        print_info "Deployment files are ready. Services were NOT started (--no-start)."
        echo ""
        print_completion "$secrets_generated" "false"
        exit 0
    fi

    check_docker_available

    print_info "Starting services (docker compose up -d)..."
    if ! docker compose up -d; then
        print_error "docker compose up failed. Check the output above for details."
        exit 1
    fi
    print_success "Services started"
    echo ""

    print_info "Waiting for Sub2API to become healthy (first run can take a few minutes)..."
    if ! wait_for_health; then
        echo ""
        print_error "Sub2API did not pass its health check within ${SUB2API_DEPLOY_HEALTH_TIMEOUT:-600} seconds."
        print_info "Inspect the logs with: docker compose logs --tail=100 sub2api"
        print_info "The stack keeps running; re-check later with: docker compose ps"
        exit 1
    fi
    echo ""

    print_completion "$secrets_generated" "true"
}

main "$@"
