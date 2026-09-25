#!/bin/sh
# =============================================================================
# Regenerates the bundled deployment templates inside deploy/docker-deploy.sh
# =============================================================================
# docker-deploy.sh embeds byte-identical copies of:
#   - deploy/docker-compose.local.yml  (written to docker-compose.yml on deploy)
#   - deploy/.env.example              (used to generate .env on deploy)
#
# Run this script after changing either canonical file, then commit both files:
#   sh deploy/tools/sync-bundled-templates.sh
#
# deploy/tests/docker-deploy-test.sh fails whenever the bundled copies drift
# from the canonical files.
# =============================================================================

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DEPLOY_DIR=$(CDPATH= cd -- "${SCRIPT_DIR}/.." && pwd)

TARGET="${DEPLOY_DIR}/docker-deploy.sh"
COMPOSE_SRC="${DEPLOY_DIR}/docker-compose.local.yml"
ENV_SRC="${DEPLOY_DIR}/.env.example"

COMPOSE_START='    cat > "$1" <<'"'"'SUB2API_COMPOSE_TEMPLATE_EOF'"'"''
COMPOSE_END='SUB2API_COMPOSE_TEMPLATE_EOF'
ENV_START='    cat > "$1" <<'"'"'SUB2API_ENV_TEMPLATE_EOF'"'"''
ENV_END='SUB2API_ENV_TEMPLATE_EOF'

for file in "$TARGET" "$COMPOSE_SRC" "$ENV_SRC"; do
    if [ ! -f "$file" ]; then
        printf 'missing required file: %s\n' "$file" >&2
        exit 1
    fi
done

tmp=$(mktemp "${TMPDIR:-/tmp}/sub2api-docker-deploy.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM

awk \
    -v compose_src="$COMPOSE_SRC" \
    -v env_src="$ENV_SRC" \
    -v compose_start="$COMPOSE_START" \
    -v compose_end="$COMPOSE_END" \
    -v env_start="$ENV_START" \
    -v env_end="$ENV_END" '
    function emit(path,   line) {
        while ((getline line < path) > 0) {
            print line
        }
        close(path)
    }
    state == 0 {
        print
        if ($0 == compose_start) {
            state = 1
        }
        next
    }
    state == 1 {
        if ($0 == compose_end) {
            emit(compose_src)
            print compose_end
            state = 2
        }
        next
    }
    state == 2 {
        print
        if ($0 == env_start) {
            state = 3
        }
        next
    }
    state == 3 {
        if ($0 == env_end) {
            emit(env_src)
            print env_end
            state = 4
        }
        next
    }
    state == 4 {
        print
    }
    END {
        if (state != 4) {
            printf "marker scan failed: final state %d (expected 4)\n", state > "/dev/stderr"
            exit 1
        }
    }
' "$TARGET" > "$tmp"

mv "$tmp" "$TARGET"
printf 'bundled templates synced into %s\n' "$TARGET"
