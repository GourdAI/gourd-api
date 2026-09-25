#!/bin/bash
# =============================================================================
# Sub2API Local-Source Docker Deployment Script
# =============================================================================
# Builds the Sub2API image from the source tree this script lives in, then
# starts it with Docker Compose. The application container always runs YOUR
# code: the generated compose file points the service at a locally built image
# (sub2api:local) instead of the published weishaw/sub2api image.
#
# Difference from deploy/docker-deploy.sh:
#   - docker-deploy.sh is download-free and self-contained, but it runs the
#     PUBLISHED image from Docker Hub, so it can never contain local changes.
#   - this script must be run inside a source checkout and compiles that
#     checkout (frontend + Go backend) into the image it starts.
#
# Usage:
#   cd /path/to/sub2api
#   bash deploy/docker-deploy-local.sh
#
# The deployment files are written into a separate directory (default
# $HOME/sub2api-deploy) so the checkout stays clean. An existing deployment in
# that directory is reused: .env secrets and the data/, postgres_data/ and
# redis_data/ directories are kept, and only the application image is rebuilt
# and swapped.
#
# Options:
#   --dir PATH      Deployment directory (default: $HOME/sub2api-deploy)
#   --no-build      Reuse the existing local image instead of rebuilding
#   --no-start      Write the deployment files only; no build and no start
#   --force         Regenerate the .env secrets. Use this only for a fresh
#                   deployment: a new POSTGRES_PASSWORD makes the existing
#                   postgres_data/ directory unusable
#   -h, --help      Show this help
#
# Environment:
#   SUB2API_LOCAL_IMAGE            Image tag to build (default: sub2api:local)
#   SUB2API_NPM_REGISTRY           npm registry used by the frontend build
#                                  (default: https://registry.npmmirror.com;
#                                   set to https://registry.npmjs.org to use
#                                   the official one)
#   SUB2API_GOPROXY                Go module proxy (default: the Dockerfile ARG)
#   SUB2API_GOSUMDB                Go checksum database (default: the Dockerfile ARG)
#   SUB2API_DEPLOY_HEALTH_TIMEOUT  Seconds to wait for the health check
#                                  (default: 900; a first source build is slow)
#
# After the first run, manage the stack from the deployment directory:
#   docker compose logs -f sub2api
#   docker compose ps
#   docker compose restart sub2api
#   bash deploy/docker-deploy-local.sh          # rebuild after a code change
#
# Do NOT run `docker compose pull` in the deployment directory: it is meant for
# registry images, and this stack is built from source.
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
# Helpers (shared shape with deploy/docker-deploy.sh; both scripts are meant
# to be copied around on their own, so they stay independent)
# ---------------------------------------------------------------------------

usage() {
    cat <<'SUB2API_LOCAL_USAGE_EOF'
Usage: bash deploy/docker-deploy-local.sh [options]

Build the Sub2API image from the current source checkout and start it with
Docker Compose. The deployment files are written into a separate directory,
which is reused on later runs (secrets and data are preserved).

Options:
  --dir PATH      Deployment directory (default: $HOME/sub2api-deploy)
  --no-build      Reuse the existing local image instead of rebuilding
  --no-start      Write the deployment files only; no build and no start
  --force         Regenerate the .env secrets (fresh deployments only)
  -h, --help      Show this help

Environment:
  SUB2API_LOCAL_IMAGE            Image tag to build (default: sub2api:local)
  SUB2API_NPM_REGISTRY           npm registry for the frontend build
  SUB2API_GOPROXY                Go module proxy (default: Dockerfile value)
  SUB2API_GOSUMDB                Go checksum DB (default: Dockerfile value)
  SUB2API_DEPLOY_HEALTH_TIMEOUT  Seconds to wait for the health check (default: 900)
SUB2API_LOCAL_USAGE_EOF
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
        print_info "Deployment files are ready in ${deploy_dir}."
        print_info "Install Docker (20.10+) with the Compose v2 plugin, then run:"
        print_info "  cd ${deploy_dir} && docker compose build && docker compose up -d"
        exit 1
    fi
    if ! docker compose version >/dev/null 2>&1; then
        print_error "Docker Compose v2 is required (the 'docker compose' plugin)."
        print_info "Deployment files are ready in ${deploy_dir}."
        print_info "Install the Compose v2 plugin, then run:"
        print_info "  cd ${deploy_dir} && docker compose build && docker compose up -d"
        exit 1
    fi
}

# Waits until the sub2api container reports a healthy status.
wait_for_health() {
    timeout_seconds="${SUB2API_DEPLOY_HEALTH_TIMEOUT:-900}"
    case "$timeout_seconds" in
        ''|*[!0-9]*) timeout_seconds=900 ;;
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

# ---------------------------------------------------------------------------
# Compose generation
# ---------------------------------------------------------------------------

# resolve_source_version <repo_root>
# Best-effort version string for the build (empty means "let the Dockerfile
# decide"). The .git directory is excluded from the Docker build context, so
# the version has to be resolved on the host and passed in as a build arg.
resolve_source_version() {
    repo_dir="$1"
    version=""
    if command -v git >/dev/null 2>&1 && [ -d "${repo_dir}/.git" ]; then
        version=$(git -C "$repo_dir" describe --tags --always --dirty 2>/dev/null || true)
    fi
    if [ -z "$version" ] && [ -r "${repo_dir}/backend/cmd/server/VERSION" ]; then
        version=$(tr -d '\r\n' < "${repo_dir}/backend/cmd/server/VERSION")
        if command -v git >/dev/null 2>&1 && [ -d "${repo_dir}/.git" ]; then
            version="${version}-dirty"
        fi
    fi
    printf '%s' "$version"
}

resolve_source_commit() {
    repo_dir="$1"
    if command -v git >/dev/null 2>&1 && [ -d "${repo_dir}/.git" ]; then
        git -C "$repo_dir" rev-parse --short HEAD 2>/dev/null || printf 'unknown'
    else
        printf 'unknown'
    fi
}

# write_local_compose <canonical_compose> <output> <context> <image> <npm> <goproxy> <gosumdb> <version> <commit>
# Rewrites the repository compose template so the sub2api service is built from
# <context> and tagged <image> instead of running the published registry image.
write_local_compose() {
    src="$1"
    dst="$2"
    ctx="$3"
    img="$4"
    npm="$5"
    goproxy="$6"
    gosumdb="$7"
    version="$8"
    commit="$9"

    awk -v img="$img" -v ctx="$ctx" -v npm="$npm" -v goproxy="$goproxy" \
        -v gosumdb="$gosumdb" -v version="$version" -v commit="$commit" '
        BEGIN { replaced = 0; dup = 0 }
        /^    image:[ \t]*weishaw\/sub2api:/ {
            if (replaced) { dup = 1; exit }
            replaced = 1
            print "    image: " img
            print "    build:"
            print "      context: \"" ctx "\""
            print "      dockerfile: Dockerfile"
            print "      args:"
            if (npm != "")     print "        NPM_CONFIG_REGISTRY: \"" npm "\""
            if (goproxy != "") print "        GOPROXY: \"" goproxy "\""
            if (gosumdb != "") print "        GOSUMDB: \"" gosumdb "\""
            if (version != "") print "        VERSION: \"" version "\""
            if (commit != "")  print "        COMMIT: \"" commit "\""
            next
        }
        { print }
        END {
            if (dup) {
                print "local compose template: found more than one sub2api image line" > "/dev/stderr"
                exit 3
            }
            if (!replaced) {
                print "local compose template: no \"image: weishaw/sub2api:\" line found" > "/dev/stderr"
                exit 4
            }
        }
    ' "$src" > "$dst"
}

print_completion() {
    services_started="$1"

    echo "=========================================="
    print_success "Local-source deployment complete!"
    echo "=========================================="
    echo ""
    echo "Source checkout:   ${repo_root}"
    echo "Built from:        ${source_version:-unknown}${source_dirty_note}"
    echo "Application image: ${image_ref} (built locally, never pulled)"
    echo ""

    if [ "$secrets_generated" = true ]; then
        echo "Generated secure credentials (saved to ${deploy_dir}/.env):"
        echo "  POSTGRES_PASSWORD:     ${postgres_password}"
        echo "  JWT_SECRET:            ${jwt_secret}"
        echo "  TOTP_ENCRYPTION_KEY:   ${totp_key}"
        echo ""
        print_warning "Keep these credentials safe and do not share them publicly!"
    else
        print_info "Existing credentials are kept in ${deploy_dir}/.env."
        print_info "View them with: grep -E '^(JWT_SECRET|TOTP_ENCRYPTION_KEY|POSTGRES_PASSWORD)=' .env"
    fi
    echo ""

    echo "Deployment files (${deploy_dir}):"
    echo "  docker-compose.yml    Build-from-source Compose configuration"
    echo "  .env                  Environment variables (generated secrets)"
    echo "  data/                 Application data"
    echo "  postgres_data/        PostgreSQL data"
    echo "  redis_data/           Redis data"
    echo ""

    if [ "$services_started" = true ]; then
        server_port=$(env_value "SERVER_PORT" "8080")
        echo "Access Web UI:"
        echo "  http://localhost:${server_port}"
        echo ""
    fi

    echo "Useful commands (run them inside ${deploy_dir}):"
    echo "  docker compose logs -f sub2api     # follow application logs"
    echo "  docker compose ps                  # show container status"
    echo "  docker compose restart sub2api     # restart without rebuilding"
    echo "  docker image inspect ${image_ref} --format '{{.Id}} {{.Created}}'"
    echo "                                     # confirm the image was built just now"
    echo ""
    print_info "To deploy newer code: edit the source, then re-run this script."
    print_info "Never run 'docker compose pull' here - this stack is built from source."
    echo ""

    if [ "$secrets_generated" != true ]; then
        print_info "If ADMIN_PASSWORD is not set in .env, the admin password is auto-generated on"
        print_info "first startup. Find it with:"
        print_info "  docker compose logs sub2api | grep \"admin password\""
        echo ""
    fi

    if [ "$services_started" != true ]; then
        print_warning "Services were NOT started."
        if [ "$BUILD" != true ]; then
            print_warning "Build and start them with: cd ${deploy_dir} && docker compose up -d --build"
        else
            print_warning "Start them with: cd ${deploy_dir} && docker compose up -d"
        fi
        echo ""
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    FORCE=false
    START=true
    BUILD=true
    deploy_dir="${SUB2API_DEPLOY_DIR:-$HOME/sub2api-deploy}"

    while [ "$#" -gt 0 ]; do
        case "$1" in
            --no-start) START=false ;;
            --no-build) BUILD=false ;;
            --force) FORCE=true ;;
            --dir)
                if [ "$#" -lt 2 ]; then
                    print_error "--dir requires a path."
                    exit 2
                fi
                deploy_dir="$2"
                shift
                ;;
            --dir=*) deploy_dir="${1#--dir=}" ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                print_error "Unknown option: $1"
                echo ""
                usage
                exit 2
                ;;
        esac
        shift
    done

    echo ""
    echo "=========================================="
    echo "  Sub2API Local-Source Docker Deployment"
    echo "=========================================="
    echo ""

    # Locate the source checkout from this script's own path: the build context
    # must be the repository root, so the script only works inside a checkout.
    script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
    repo_root=$(CDPATH= cd -- "${script_dir}/.." && pwd)

    compose_template="${repo_root}/deploy/docker-compose.local.yml"
    env_template="${repo_root}/deploy/.env.example"
    dockerfile="${repo_root}/Dockerfile"

    if [ ! -f "$compose_template" ] || [ ! -f "$env_template" ] || [ ! -f "$dockerfile" ]; then
        print_error "This script must live inside a Sub2API source checkout."
        print_info "Expected ${compose_template}, ${env_template} and ${dockerfile}."
        print_info "Use deploy/docker-deploy.sh instead to run the published image."
        exit 1
    fi

    if [ -z "$deploy_dir" ]; then
        print_error "--dir requires a non-empty path."
        exit 2
    fi

    # Refuse to write into the repository's deploy/ directory: docker-compose.yml
    # and .env.example there are tracked files.
    if [ -f "${deploy_dir}/docker-compose.local.yml" ]; then
        print_error "The deployment directory looks like the repository's deploy/ directory."
        print_info "Pass a separate directory, for example: --dir \"\$HOME/sub2api-deploy\""
        exit 1
    fi

    mkdir -p "$deploy_dir"
    deploy_dir=$(CDPATH= cd -- "$deploy_dir" && pwd)
    # Work inside the deployment directory: .env lookups and docker compose both
    # resolve relative to the current directory.
    cd "$deploy_dir"
    print_info "Source:   ${repo_root}"
    print_info "Deploy to: ${deploy_dir}"
    echo ""

    image_ref="${SUB2API_LOCAL_IMAGE:-sub2api:local}"
    npm_registry="${SUB2API_NPM_REGISTRY-https://registry.npmmirror.com}"
    goproxy="${SUB2API_GOPROXY:-}"
    gosumdb="${SUB2API_GOSUMDB:-}"
    source_version=$(resolve_source_version "$repo_root")
    source_commit=$(resolve_source_commit "$repo_root")
    case "$source_version" in
        *-dirty) source_dirty_note=" (uncommitted changes)" ;;
        *) source_dirty_note="" ;;
    esac

    # docker-compose.yml (repository template, rewritten to build from source)
    tmp_compose="${deploy_dir}/.docker-compose.yml.new"
    if ! write_local_compose "$compose_template" "$tmp_compose" \
        "$repo_root" "$image_ref" "$npm_registry" "$goproxy" "$gosumdb" \
        "$source_version" "$source_commit"; then
        rm -f "$tmp_compose"
        print_error "Could not generate a build-from-source compose file."
        print_info "The repository template may have changed: ${compose_template}"
        exit 1
    fi
    if [ -f "${deploy_dir}/docker-compose.yml" ] && \
        ! cmp -s "${deploy_dir}/docker-compose.yml" "$tmp_compose"; then
        cp "${deploy_dir}/docker-compose.yml" "${deploy_dir}/docker-compose.yml.bak"
        print_warning "Existing docker-compose.yml differs; backed up to docker-compose.yml.bak"
    fi
    mv "$tmp_compose" "${deploy_dir}/docker-compose.yml"
    print_success "Wrote ${deploy_dir}/docker-compose.yml (builds ${image_ref} from ${repo_root})"

    # .env.example (reference copy straight from the repository)
    cp "$env_template" "${deploy_dir}/.env.example"
    print_success "Wrote ${deploy_dir}/.env.example"

    # .env (generated secrets, preserved across re-runs)
    secrets_generated=false
    jwt_secret=""
    totp_key=""
    postgres_password=""

    env_exists=false
    if [ -f "${deploy_dir}/.env" ]; then
        env_exists=true
    fi

    if [ "$env_exists" = true ] && [ "$FORCE" != true ]; then
        print_info "Keeping existing .env (credentials unchanged)."
    else
        if [ "$env_exists" = true ]; then
            print_warning "Existing .env detected; regenerating secrets (--force)."
            print_warning "A regenerated POSTGRES_PASSWORD will NOT match existing PostgreSQL data"
            print_warning "in postgres_data/. Use --force only for a fresh deployment."
        fi
        if ! command -v openssl >/dev/null 2>&1 && [ ! -r /dev/urandom ]; then
            print_error "openssl (or /dev/urandom) is required to generate secure secrets."
            exit 1
        fi
        cp "$env_template" "${deploy_dir}/.env"
        jwt_secret=$(generate_secret)
        totp_key=$(generate_secret)
        postgres_password=$(generate_secret)
        replace_env_value "${deploy_dir}/.env" "JWT_SECRET" "$jwt_secret"
        replace_env_value "${deploy_dir}/.env" "TOTP_ENCRYPTION_KEY" "$totp_key"
        replace_env_value "${deploy_dir}/.env" "POSTGRES_PASSWORD" "$postgres_password"
        require_secret_value "JWT_SECRET"
        require_secret_value "TOTP_ENCRYPTION_KEY"
        require_secret_value "POSTGRES_PASSWORD"
        chmod 600 "${deploy_dir}/.env"
        secrets_generated=true
        print_success "Generated ${deploy_dir}/.env with secure credentials"
    fi

    # Data directories (local directories for easy backup/migration)
    mkdir -p "${deploy_dir}/data" "${deploy_dir}/postgres_data" "${deploy_dir}/redis_data"
    print_success "Created data directories (data/, postgres_data/, redis_data/)"
    echo ""

    if [ "$START" != true ]; then
        print_info "Deployment files are ready. Nothing was built or started (--no-start)."
        echo ""
        print_completion "false"
        exit 0
    fi

    check_docker_available

    if [ "$BUILD" = true ]; then
        # The Dockerfile uses BuildKit cache mounts; enable BuildKit explicitly so
        # the build also works on engines where it is still opt-in.
        DOCKER_BUILDKIT=1
        export DOCKER_BUILDKIT
        print_info "Building ${image_ref} from ${repo_root} ..."
        print_info "A first build compiles the frontend and the Go backend: it can take 10-30"
        print_info "minutes and needs a few GB of free disk space. Later builds reuse the cache."
        echo ""
        if ! docker compose build sub2api; then
            echo ""
            print_error "The image build failed. Fix the error above, then re-run this script."
            print_info "Common causes: no network access to the npm/Go proxies (override with"
            print_info "SUB2API_NPM_REGISTRY / SUB2API_GOPROXY), or not enough memory."
            exit 1
        fi
        echo ""
    else
        print_info "Skipping the image build (--no-build); reusing ${image_ref}."
        if ! docker image inspect "$image_ref" >/dev/null 2>&1; then
            print_error "Image ${image_ref} does not exist locally. Drop --no-build to build it."
            exit 1
        fi
    fi

    print_info "Starting services (docker compose up -d)..."
    if ! docker compose up -d; then
        print_error "docker compose up failed. Check the output above for details."
        exit 1
    fi
    print_success "Services started"
    echo ""

    image_id=$(docker image inspect "$image_ref" --format '{{.Id}}' 2>/dev/null || true)
    image_created=$(docker image inspect "$image_ref" --format '{{.Created}}' 2>/dev/null || true)
    print_info "Running image: ${image_ref} ${image_id:-unknown} built ${image_created:-unknown}"
    echo ""

    print_info "Waiting for Sub2API to become healthy (first run can take a few minutes)..."
    if ! wait_for_health; then
        echo ""
        print_error "Sub2API did not pass its health check within ${SUB2API_DEPLOY_HEALTH_TIMEOUT:-900} seconds."
        print_info "Inspect the logs with: docker compose logs --tail=100 sub2api"
        print_info "The stack keeps running; re-check later with: docker compose ps"
        exit 1
    fi
    echo ""

    print_completion "true"
}

main "$@"
