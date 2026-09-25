#!/bin/sh
# =============================================================================
# One-click Docker deployment test
# =============================================================================
# Guards deploy/docker-deploy.sh:
#   - the deployment templates bundled inside it stay byte-identical to
#     deploy/docker-compose.local.yml and deploy/.env.example
#   - the script does not download anything from the repository at runtime
#   - a dry run (--no-start) produces the expected files, secrets, and data
#     directories without Docker; re-runs keep existing secrets; --force
#     regenerates them
#   - the repository-directory guard and the sync tool behave as documented
# =============================================================================

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

script=deploy/docker-deploy.sh
compose=deploy/docker-compose.local.yml
env_example=deploy/.env.example
sync_tool=deploy/tools/sync-bundled-templates.sh

fail() {
    printf 'docker-deploy test failed: %s\n' "$1" >&2
    exit 1
}

[ -s "$script" ] || fail 'deploy/docker-deploy.sh is missing or empty'

work=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-docker-deploy-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

# ---------------------------------------------------------------------------
# 1. The bundled templates must match the canonical repository files
# ---------------------------------------------------------------------------
extract_compose() {
    awk '
      /cat > "\$1" <<'"'"'SUB2API_COMPOSE_TEMPLATE_EOF'"'"'/ { inblock = 1; next }
      inblock && /^SUB2API_COMPOSE_TEMPLATE_EOF$/ { inblock = 0; next }
      inblock { print }
    ' "$script"
}

extract_env() {
    awk '
      /cat > "\$1" <<'"'"'SUB2API_ENV_TEMPLATE_EOF'"'"'/ { inblock = 1; next }
      inblock && /^SUB2API_ENV_TEMPLATE_EOF$/ { inblock = 0; next }
      inblock { print }
    ' "$script"
}

extract_compose > "$work/compose.extracted"
extract_env > "$work/env.extracted"

cmp -s "$compose" "$work/compose.extracted" || \
    fail 'the compose template bundled in deploy/docker-deploy.sh differs from deploy/docker-compose.local.yml (run: sh deploy/tools/sync-bundled-templates.sh)'
cmp -s "$env_example" "$work/env.extracted" || \
    fail 'the env template bundled in deploy/docker-deploy.sh differs from deploy/.env.example (run: sh deploy/tools/sync-bundled-templates.sh)'

# ---------------------------------------------------------------------------
# 2. The script must be self-contained: no repository downloads at runtime
# ---------------------------------------------------------------------------
strip_bundles() {
    awk '
      /<<'"'"'SUB2API_COMPOSE_TEMPLATE_EOF'"'"'/ { skip = 1; next }
      /<<'"'"'SUB2API_ENV_TEMPLATE_EOF'"'"'/ { skip = 1; next }
      skip == 1 && /^SUB2API_COMPOSE_TEMPLATE_EOF$/ { skip = 0; next }
      skip == 1 && /^SUB2API_ENV_TEMPLATE_EOF$/ { skip = 0; next }
      skip == 0 { print }
    ' "$script"
}

strip_bundles | sed 's/#.*$//' > "$work/code.sh"

if grep -qE 'raw\.githubusercontent\.com' "$work/code.sh"; then
    fail 'deploy/docker-deploy.sh must not fetch deployment files from the repository at runtime'
fi
if grep -qE '(^|[[:space:];&|(])(curl|wget)([[:space:]]|$)' "$work/code.sh"; then
    fail 'deploy/docker-deploy.sh must not invoke curl or wget'
fi

# The script is commonly piped to a shell, where the shebang does not apply:
# both POSIX sh and bash must be able to parse it.
sh -n "$script" || fail 'deploy/docker-deploy.sh is not POSIX-sh parseable'
if command -v bash >/dev/null 2>&1; then
    bash -n "$script" || fail 'deploy/docker-deploy.sh has a bash syntax error'
fi

# ---------------------------------------------------------------------------
# 3. Dry run: --no-start prepares a complete deployment without Docker
# ---------------------------------------------------------------------------
run_dir="$work/run"
mkdir -p "$run_dir"
cp "$script" "$run_dir/docker-deploy.sh"
chmod +x "$run_dir/docker-deploy.sh"

if ! ( cd "$run_dir" && sh docker-deploy.sh --no-start ) > "$run_dir/first.log" 2>&1; then
    cat "$run_dir/first.log" >&2
    fail 'docker-deploy.sh --no-start failed'
fi
grep -q 'Services were NOT started' "$run_dir/first.log" || \
    fail '--no-start must report that services were not started'

cmp -s "$compose" "$run_dir/docker-compose.yml" || \
    fail '--no-start did not write the expected docker-compose.yml'
cmp -s "$env_example" "$run_dir/.env.example" || \
    fail '--no-start did not write the expected .env.example'

for dir in data postgres_data redis_data; do
    [ -d "$run_dir/$dir" ] || fail "--no-start did not create $dir/"
done

[ ! -e "$run_dir/.env.tmp" ] || fail 'secret generation left a temporary file behind'

env_file="$run_dir/.env"
[ -f "$env_file" ] || fail '--no-start did not create .env'

for key in JWT_SECRET TOTP_ENCRYPTION_KEY POSTGRES_PASSWORD; do
    grep -qE "^${key}=[0-9a-f]{64}$" "$env_file" || \
        fail "generated ${key} is not a 64-character hex secret"
done

jwt_secret=$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)
totp_secret=$(grep -E '^TOTP_ENCRYPTION_KEY=' "$env_file" | cut -d= -f2-)
postgres_secret=$(grep -E '^POSTGRES_PASSWORD=' "$env_file" | cut -d= -f2-)
[ "$jwt_secret" != "$totp_secret" ] || fail 'generated secrets must differ from each other'
[ "$jwt_secret" != "$postgres_secret" ] || fail 'generated secrets must differ from each other'

# Only the three secret values change: the generated .env keeps the template shape.
template_lines=$(wc -l < "$env_example" | tr -d ' ')
generated_lines=$(wc -l < "$env_file" | tr -d ' ')
[ "$template_lines" = "$generated_lines" ] || \
    fail "generated .env has $generated_lines lines, expected $template_lines"

# Re-run: existing secrets are preserved, and the shebang entry point works.
if ! ( cd "$run_dir" && ./docker-deploy.sh --no-start ) > "$run_dir/second.log" 2>&1; then
    cat "$run_dir/second.log" >&2
    fail 're-running docker-deploy.sh --no-start failed'
fi
[ "$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)" = "$jwt_secret" ] || \
    fail 're-running the script must keep existing secrets'

# --force regenerates the secrets.
if ! ( cd "$run_dir" && sh docker-deploy.sh --no-start --force ) > "$run_dir/third.log" 2>&1; then
    cat "$run_dir/third.log" >&2
    fail 'docker-deploy.sh --no-start --force failed'
fi
[ "$(grep -E '^JWT_SECRET=' "$env_file" | cut -d= -f2-)" != "$jwt_secret" ] || \
    fail '--force must regenerate secrets'

# A modified docker-compose.yml is backed up before the bundled one is restored.
printf '# customized by the test\n' > "$run_dir/docker-compose.yml"
if ! ( cd "$run_dir" && sh docker-deploy.sh --no-start ) > "$run_dir/fourth.log" 2>&1; then
    cat "$run_dir/fourth.log" >&2
    fail 're-running after compose customization failed'
fi
[ -f "$run_dir/docker-compose.yml.bak" ] || \
    fail 'a differing docker-compose.yml must be backed up to docker-compose.yml.bak'
cmp -s "$compose" "$run_dir/docker-compose.yml" || \
    fail 'the bundled docker-compose.yml must be refreshed'

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

# The repository-directory guard refuses to run next to the source checkout.
guard_dir="$work/guard"
mkdir -p "$guard_dir"
cp "$script" "$guard_dir/docker-deploy.sh"
chmod +x "$guard_dir/docker-deploy.sh"
touch "$guard_dir/docker-compose.local.yml"
if ( cd "$guard_dir" && sh docker-deploy.sh --no-start ) > "$guard_dir/guard.log" 2>&1; then
    fail 'the repository-directory guard must refuse to run next to docker-compose.local.yml'
fi
grep -q 'deploy/ directory' "$guard_dir/guard.log" || \
    fail 'the repository-directory guard must explain itself'
[ ! -e "$guard_dir/docker-compose.yml" ] || \
    fail 'the repository-directory guard must not write deployment files'

# --help exits 0; unknown options are rejected.
( cd "$run_dir" && sh docker-deploy.sh --help ) > /dev/null 2>&1 || fail '--help must exit 0'
if ( cd "$run_dir" && sh docker-deploy.sh --bogus ) > /dev/null 2>&1; then
    fail 'unknown options must be rejected'
fi

# ---------------------------------------------------------------------------
# 4. The sync tool is idempotent, picks up canonical changes, and never
#    clobbers a file without markers
# ---------------------------------------------------------------------------
sync_dir="$work/sync"
mkdir -p "$sync_dir/deploy/tools"
cp "$script" "$sync_dir/deploy/docker-deploy.sh"
cp "$compose" "$sync_dir/deploy/docker-compose.local.yml"
cp "$env_example" "$sync_dir/deploy/.env.example"
cp "$sync_tool" "$sync_dir/deploy/tools/sync-bundled-templates.sh"

if ! ( cd "$sync_dir" && sh deploy/tools/sync-bundled-templates.sh ) > "$sync_dir/sync.log" 2>&1; then
    cat "$sync_dir/sync.log" >&2
    fail 'sync-bundled-templates.sh failed on an already synced script'
fi
cmp -s "$sync_dir/deploy/docker-deploy.sh" "$script" || \
    fail 'sync-bundled-templates.sh is not idempotent'

printf '# drift\n' >> "$sync_dir/deploy/docker-compose.local.yml"
if ! ( cd "$sync_dir" && sh deploy/tools/sync-bundled-templates.sh ) > "$sync_dir/sync2.log" 2>&1; then
    cat "$sync_dir/sync2.log" >&2
    fail 'sync-bundled-templates.sh failed after a canonical change'
fi
cmp -s "$sync_dir/deploy/docker-deploy.sh" "$script" && \
    fail 'sync-bundled-templates.sh did not pick up the canonical change'

cp "$compose" "$sync_dir/deploy/docker-compose.local.yml"
if ! ( cd "$sync_dir" && sh deploy/tools/sync-bundled-templates.sh ) > "$sync_dir/sync3.log" 2>&1; then
    cat "$sync_dir/sync3.log" >&2
    fail 'sync-bundled-templates.sh failed after restoring the canonical file'
fi
cmp -s "$sync_dir/deploy/docker-deploy.sh" "$script" || \
    fail 'sync-bundled-templates.sh did not restore the bundled template'

broken_dir="$work/broken"
mkdir -p "$broken_dir/deploy/tools"
printf 'no markers here\n' > "$broken_dir/deploy/docker-deploy.sh"
cp "$compose" "$broken_dir/deploy/docker-compose.local.yml"
cp "$env_example" "$broken_dir/deploy/.env.example"
cp "$sync_tool" "$broken_dir/deploy/tools/sync-bundled-templates.sh"
if ( cd "$broken_dir" && sh deploy/tools/sync-bundled-templates.sh ) > "$broken_dir/sync.log" 2>&1; then
    fail 'sync-bundled-templates.sh must fail when the markers are missing'
fi
grep -q 'no markers here' "$broken_dir/deploy/docker-deploy.sh" || \
    fail 'sync-bundled-templates.sh must not clobber a file without markers'

printf 'docker-deploy one-click test passed\n'
