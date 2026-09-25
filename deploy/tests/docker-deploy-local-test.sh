#!/bin/sh
# =============================================================================
# Local-source Docker deployment test
# =============================================================================
# Guards deploy/docker-deploy-local.sh:
#   - it must run inside a source checkout and reuse the repository templates
#     (docker-compose.local.yml / .env.example) instead of bundled copies
#   - the generated compose file builds the application from the checkout and
#     never references the published registry image, so `up` cannot pull it
#   - the script starts the stack with a local build, never with `compose pull`
#   - a dry run (--no-start) produces the expected files, secrets and data
#     directories without Docker; re-runs keep existing secrets; --force
#     regenerates them
#   - the deployment-directory guard, the --dir option and a template without
#     the expected image line all fail loudly
# =============================================================================

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

script=deploy/docker-deploy-local.sh
compose=deploy/docker-compose.local.yml
env_example=deploy/.env.example

fail() {
    printf 'docker-deploy-local test failed: %s\n' "$1" >&2
    exit 1
}

[ -s "$script" ] || fail 'deploy/docker-deploy-local.sh is missing or empty'

work=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-docker-deploy-local-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

# ---------------------------------------------------------------------------
# 1. Syntax: the script is commonly run through `sh script.sh`, so it must be
#    POSIX-sh parseable as well as bash-parseable.
# ---------------------------------------------------------------------------
sh -n "$script" || fail 'deploy/docker-deploy-local.sh is not POSIX-sh parseable'
if command -v bash >/dev/null 2>&1; then
    bash -n "$script" || fail 'deploy/docker-deploy-local.sh has a bash syntax error'
fi

# ---------------------------------------------------------------------------
# 2. No registry image, no pulling: the app container must come from a build
# ---------------------------------------------------------------------------
if grep -qE '^[[:space:]]*image:[[:space:]]*"?weishaw/sub2api' "$script"; then
    fail 'deploy/docker-deploy-local.sh must not run the published sub2api image'
fi
# Only executable docker invocations count: the script deliberately tells the
# operator not to run `compose pull`, so drop comments and output statements.
if sed 's/#.*$//' "$script" | grep -vE '^[[:space:]]*(print_|echo|printf|cat)' | \
    grep -qE 'docker([[:space:]]+[^[:space:]]+)*[[:space:]]+pull'; then
    fail 'deploy/docker-deploy-local.sh must not pull images'
fi
if grep -qE 'raw\.githubusercontent\.com' "$script"; then
    fail 'deploy/docker-deploy-local.sh must not fetch files from the repository at runtime'
fi
if sed 's/#.*$//' "$script" | grep -vE '^[[:space:]]*(print_|echo|printf|cat)' | \
    grep -qE '(^|[[:space:];&|(])(curl|wget)([[:space:]]|$)'; then
    fail 'deploy/docker-deploy-local.sh must not invoke curl or wget'
fi

# It must build, and it must read the canonical repository templates.
grep -qE 'docker compose build' "$script" || \
    fail 'deploy/docker-deploy-local.sh must build the local image'
grep -q 'docker-compose.local.yml' "$script" || \
    fail 'deploy/docker-deploy-local.sh must reuse deploy/docker-compose.local.yml'
grep -q '\.env\.example' "$script" || \
    fail 'deploy/docker-deploy-local.sh must reuse deploy/.env.example'

# ---------------------------------------------------------------------------
# 3. Dry run in a separate deployment directory
# ---------------------------------------------------------------------------
fake=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-local-fake.XXXXXX")
trap 'rm -rf "$work" "$fake"' EXIT HUP INT TERM
mkdir -p "$fake/repo/deploy"
cp "$script" "$fake/repo/deploy/docker-deploy-local.sh"
cp "$compose" "$fake/repo/deploy/docker-compose.local.yml"
cp "$env_example" "$fake/repo/deploy/.env.example"
printf 'FROM scratch\n' > "$fake/repo/Dockerfile"

run_dir="$fake/deploydir"
mkdir -p "$run_dir"

if ! ( cd "$run_dir" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start \
        --dir "$run_dir" ) > "$run_dir/first.log" 2>&1; then
    cat "$run_dir/first.log" >&2
    fail 'docker-deploy-local.sh --no-start failed'
fi
grep -q 'Nothing was built or started' "$run_dir/first.log" || \
    fail '--no-start must report that nothing was built or started'

[ -f "$run_dir/docker-compose.yml" ] || fail '--no-start did not write docker-compose.yml'
cmp -s "$env_example" "$run_dir/.env.example" || \
    fail '--no-start did not copy deploy/.env.example verbatim'

for dir in data postgres_data redis_data; do
    [ -d "$run_dir/$dir" ] || fail "--no-start did not create $dir/"
done
[ ! -e "$run_dir/.docker-compose.yml.new" ] || \
    fail 'compose generation left a temporary file behind'
[ ! -e "$run_dir/.env.tmp" ] || fail 'secret generation left a temporary file behind'

# The generated compose keeps the published image out and builds from source.
if grep -qE 'image:[[:space:]]*"?weishaw/sub2api' "$run_dir/docker-compose.yml"; then
    fail 'the generated compose must not reference the published sub2api image'
fi
grep -qE 'image:[[:space:]]*sub2api:local$' "$run_dir/docker-compose.yml" || \
    fail 'the generated compose must pin the locally built image tag'
grep -qE 'context: "'"$fake"'/repo"' "$run_dir/docker-compose.yml" || \
    fail 'the generated compose must build from the source checkout'
grep -q 'dockerfile: Dockerfile' "$run_dir/docker-compose.yml" || \
    fail 'the generated compose must name the repository Dockerfile'
grep -q 'NPM_CONFIG_REGISTRY: "https://registry.npmmirror.com"' "$run_dir/docker-compose.yml" || \
    fail 'the generated compose must pass the npm registry build arg'
# postgres/redis services are inherited from the canonical template unchanged.
grep -q 'image: postgres:18-alpine' "$run_dir/docker-compose.yml" || \
    fail 'the generated compose must keep the postgres service'
grep -q 'image: redis:8-alpine' "$run_dir/docker-compose.yml" || \
    fail 'the generated compose must keep the redis service'
grep -q 'DATABASE_PASSWORD=${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}' \
    "$run_dir/docker-compose.yml" || \
    fail 'the generated compose must keep the database wiring'

# Only the sub2api service block changed: the rest of the template is verbatim,
# which keeps this script honest about the canonical compose file.
awk '/^    image:[ \t]*weishaw\/sub2api:/ { skip = 1; next }
     skip == 1 && /^    container_name:/ { skip = 0 }
     skip == 0 { print }' "$compose" > "$work/compose.rest"
awk '/^    image:[ \t]*sub2api:local$/ { skip = 1; next }
     skip == 1 && /^    container_name:/ { skip = 0 }
     skip == 0 { print }' "$run_dir/docker-compose.yml" > "$work/generated.rest"
cmp -s "$work/compose.rest" "$work/generated.rest" || \
    fail 'the generated compose differs from the canonical template outside the image block'

env_file="$run_dir/.env"
[ -f "$env_file" ] || fail '--no-start did not create .env'
for key in JWT_SECRET TOTP_ENCRYPTION_KEY POSTGRES_PASSWORD; do
    grep -qE "^${key}=[0-9a-f]{64}$" "$env_file" || \
        fail "generated ${key} is not a 64-character hex secret"
done
[ "$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)" != \
    "$(grep -E '^POSTGRES_PASSWORD=' "$env_file" | cut -d= -f2-)" ] || \
    fail 'generated secrets must differ from each other'

template_lines=$(wc -l < "$env_example" | tr -d ' ')
generated_lines=$(wc -l < "$env_file" | tr -d ' ')
[ "$template_lines" = "$generated_lines" ] || \
    fail "generated .env has $generated_lines lines, expected $template_lines"

# Re-run: secrets are preserved and the compose file is refreshed in place.
if ! ( cd "$run_dir" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start \
        --dir "$run_dir" ) > "$run_dir/second.log" 2>&1; then
    cat "$run_dir/second.log" >&2
    fail 're-running docker-deploy-local.sh --no-start failed'
fi
[ "$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)" = \
    "$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)" ] || \
    fail 'the generated .env must be stable'
grep -q 'Keeping existing .env' "$run_dir/second.log" || \
    fail 'a re-run must keep the existing credentials'
[ ! -e "$run_dir/docker-compose.yml.bak" ] || \
    fail 'an unchanged compose file must not be backed up'

# --force regenerates the secrets.
jwt_before=$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)
if ! ( cd "$run_dir" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start --force \
        --dir "$run_dir" ) > "$run_dir/third.log" 2>&1; then
    cat "$run_dir/third.log" >&2
    fail 'docker-deploy-local.sh --no-start --force failed'
fi
[ "$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)" != "$jwt_before" ] || \
    fail '--force must regenerate secrets'

# A customised compose file is backed up before being refreshed.
printf '# customized by the test\n' > "$run_dir/docker-compose.yml"
if ! ( cd "$run_dir" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start \
        --dir "$run_dir" ) > "$run_dir/fourth.log" 2>&1; then
    cat "$run_dir/fourth.log" >&2
    fail 're-running after compose customization failed'
fi
[ -f "$run_dir/docker-compose.yml.bak" ] || \
    fail 'a differing docker-compose.yml must be backed up to docker-compose.yml.bak'

# Generated .env permissions (skipped on Windows filesystems, where chmod
# semantics differ from POSIX).
case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) : ;;
    *)
        perms=$(ls -l "$env_file" | cut -c1-10)
        case "$perms" in
            -rw-------*) : ;;
            *) fail "generated .env permissions are '$perms', expected 600" ;;
        esac
        ;;
esac

# --dir PATH and --dir=PATH both work, including relative paths.
rel_dir="$fake/relative"
if ! ( cd "$fake" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start --dir relative ) \
    > "$fake/rel.log" 2>&1; then
    cat "$fake/rel.log" >&2
    fail '--dir PATH failed'
fi
[ -f "$rel_dir/docker-compose.yml" ] || fail '--dir PATH did not create the deployment directory'
if ! ( cd "$fake" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start --dir "$fake/eqsign" ) \
    > "$fake/eq.log" 2>&1; then
    cat "$fake/eq.log" >&2
    fail '--dir=PATH failed'
fi
[ -f "$fake/eqsign/docker-compose.yml" ] || fail '--dir=PATH did not write the compose file'

# A custom image tag is honoured.
if ! ( cd "$fake" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start \
        --dir "$fake/custom" ) > "$fake/custom.log" 2>&1; then
    cat "$fake/custom.log" >&2
    fail 'deployment with a custom directory failed'
fi
SUB2API_LOCAL_IMAGE=sub2api:dev-1
export SUB2API_LOCAL_IMAGE
if ! ( cd "$fake" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start \
        --dir "$fake/custom" ) > "$fake/custom2.log" 2>&1; then
    cat "$fake/custom2.log" >&2
    fail 'deployment with SUB2API_LOCAL_IMAGE failed'
fi
grep -q 'image: sub2api:dev-1$' "$fake/custom/docker-compose.yml" || \
    fail 'SUB2API_LOCAL_IMAGE must set the built image tag'
unset SUB2API_LOCAL_IMAGE

# ---------------------------------------------------------------------------
# 4. Guards
# ---------------------------------------------------------------------------
# Refuses to write into the repository's own deploy/ directory.
if ( cd "$fake/repo/deploy" && sh ./docker-deploy-local.sh --no-start --dir . ) \
    > "$fake/guard.log" 2>&1; then
    fail 'the script must refuse to deploy into the repository deploy/ directory'
fi
grep -q 'deploy/ directory' "$fake/guard.log" || \
    fail 'the deployment-directory guard must explain itself'

# Refuses to run outside a checkout.
lonely="$fake/lonely"
mkdir -p "$lonely/deploy"
cp "$script" "$lonely/deploy/docker-deploy-local.sh"
if ( cd "$lonely" && sh deploy/docker-deploy-local.sh --no-start --dir "$fake/lonely-out" ) \
    > "$fake/lonely.log" 2>&1; then
    fail 'the script must require a source checkout'
fi
grep -q 'source checkout' "$fake/lonely.log" || \
    fail 'the checkout requirement must be explained'

# Fails loudly when the canonical template no longer has the expected image line.
drift="$fake/drift"
mkdir -p "$drift/deploy"
cp "$script" "$drift/deploy/docker-deploy-local.sh"
cp "$env_example" "$drift/deploy/.env.example"
printf 'FROM scratch\n' > "$drift/Dockerfile"
printf 'services:\n  sub2api:\n    container_name: sub2api\n' \
    > "$drift/deploy/docker-compose.local.yml"
if ( cd "$drift" && sh deploy/docker-deploy-local.sh --no-start --dir "$drift/out" ) \
    > "$drift/drift.log" 2>&1; then
    fail 'the script must fail when the compose template has no sub2api image line'
fi
[ ! -e "$drift/out/docker-compose.yml" ] || \
    fail 'a failed compose generation must not leave a partial docker-compose.yml'

# --help exits 0; unknown options and a missing --dir value are rejected.
( cd "$fake" && sh "$fake/repo/deploy/docker-deploy-local.sh" --help ) > /dev/null 2>&1 || \
    fail '--help must exit 0'
if ( cd "$fake" && sh "$fake/repo/deploy/docker-deploy-local.sh" --bogus ) \
    > /dev/null 2>&1; then
    fail 'unknown options must be rejected'
fi
if ( cd "$fake" && sh "$fake/repo/deploy/docker-deploy-local.sh" --no-start --dir ) \
    > /dev/null 2>&1; then
    fail '--dir without a value must be rejected'
fi

printf 'docker-deploy-local one-click test passed\n'
