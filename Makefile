.DEFAULT_GOAL := help
SHELL := /bin/bash

BINARY      := bin/server
PKG         := ./cmd/server
MIGRATION   := internal/database/migration

# Tailwind is the standalone CLI, downloaded on demand into .tools/ (gitignored).
# Pinning the version here is what makes the CSS build reproducible without a
# package.json — there is no Node dependency anywhere in this project.
#
# .tools/ rather than bin/ so `make clean` does not force a 50MB re-download of a
# toolchain that has nothing to do with this project's build output.
#
# The version is part of the FILENAME, not just this variable. A bare
# .tools/tailwindcss is a file target make considers satisfied the moment it
# exists, so bumping TAILWIND_VERSION would silently keep building with whatever
# binary was already on disk. Production was found on an older v4 exactly that
# way — its app.css still carried v3's leading-`!` important modifier, which
# v4.3.3 no longer emits.
TAILWIND_VERSION := v4.3.3
TAILWIND_BIN     := .tools/tailwindcss-$(TAILWIND_VERSION)
CSS_IN           := static/css/input.css
CSS_OUT          := static/css/app.css

# Load .env so the db targets can reach MariaDB with the same credentials the
# app uses. Missing .env is not fatal — the targets that need it will say so.
ifneq (,$(wildcard .env))
include .env
export
endif

MYSQL_ARGS := -h $(or $(DB_HOST),127.0.0.1) -P $(or $(DB_PORT),3306) -u $(or $(DB_USER),root) $(if $(DB_PASSWORD),-p$(DB_PASSWORD),)

.PHONY: help build run dev test test-unit vet vulncheck sqlc migrate seed db-create db-drop db-reset db-fresh db-test-drop db-test-shell clean tailwind tailwind-watch assets icons fonts js tidy

help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# The CSS is a build output, not a side quest: `static/css/app.css` is gitignored,
# so it never arrives with a `git pull`, and Tailwind generates only the classes
# it finds in template/. A binary built without it is paired with whatever CSS was
# last produced on that machine — which is how production ended up serving a
# stylesheet missing every class unique to the public mobile drawer, while the
# rest of the site looked fine. Making it a prerequisite costs ~80ms and removes
# the deploy step someone has to remember.
build: tailwind ## Compile the server binary into bin/ (rebuilds the CSS first)
	go build -o $(BINARY) $(PKG)

run: build ## Build and run once
	./$(BINARY)

dev: tailwind ## Run with hot reload (air) and Tailwind in watch mode
	@command -v air >/dev/null || { echo "air not found: go install github.com/air-verse/air@latest"; exit 1; }
	@# Tailwind watches template/ and input.css; air watches Go and SQL only, so
	@# the two never trigger each other (see .air.toml). The trap kills the
	@# watcher when air exits — otherwise Ctrl-C leaves an orphan rebuilding CSS.
	@$(TAILWIND_BIN) -i $(CSS_IN) -o $(CSS_OUT) --watch=always >/dev/null 2>&1 & \
	 TW_PID=$$!; \
	 trap 'kill $$TW_PID 2>/dev/null' EXIT INT TERM; \
	 air

# TEST_DB_NAME is the database the harness owns. It DROPS and recreates it on
# every run, so internal/testsupport refuses any name not ending in "_test" —
# a mis-set value must never be able to reach the development database.
TEST_DB_NAME ?= appskep_tpj_test

test: ## Run the whole suite, including the DB-backed tests (needs MariaDB)
	# -count=1 so a green run is never a cached one, and no -short so a missing
	# database is a hard failure here: `make test` passing must never mean the
	# concurrency test quietly skipped.
	go test ./... -race -count=1

test-unit: ## Run only the tests that need no database
	go test ./... -race -count=1 -short

vet: ## Run go vet
	go vet ./...

vulncheck: ## Scan dependencies and reachable code for known vulnerabilities
	# Run, not installed as a dependency: govulncheck is a tool, and adding it to
	# go.mod would put its own dependency tree into this module's build.
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

tidy: ## Tidy go.mod / go.sum
	go mod tidy

sqlc: ## Regenerate internal/database/sqlc from query/ and migration/
	@command -v sqlc >/dev/null || { echo "sqlc not found: brew install sqlc"; exit 1; }
	sqlc generate

db-create: ## Create the database if it does not exist
	mysql $(MYSQL_ARGS) -e "CREATE DATABASE IF NOT EXISTS \`$(DB_NAME)\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

db-drop: ## Drop the database (destructive)
	mysql $(MYSQL_ARGS) -e "DROP DATABASE IF EXISTS \`$(DB_NAME)\`;"

migrate: db-create ## Apply the schema migrations
	@for f in $(MIGRATION)/0*.sql; do \
		[ -e "$$f" ] || { echo "no migrations yet"; exit 0; }; \
		echo "applying $$f"; \
		mysql $(MYSQL_ARGS) $(DB_NAME) < "$$f" || exit 1; \
	done

seed: ## Load development seed data
	@if [ -e $(MIGRATION)/seed_dev.sql ]; then \
		mysql $(MYSQL_ARGS) $(DB_NAME) < $(MIGRATION)/seed_dev.sql; \
	else echo "no seed file yet"; fi

# Until go-live, 0001_schema.sql is the single source of truth and there are no
# numbered ALTER migrations. `migrate` uses CREATE TABLE IF NOT EXISTS, so it is
# re-runnable but blind to edits — changing a column means recreating the
# database. That is what these two targets are for.
db-reset: ## Drop, recreate and re-apply the schema (DESTRUCTIVE — dev only)
	@if [ "$(ENV)" = "production" ]; then echo "refusing to db-reset in production"; exit 1; fi
	@$(MAKE) db-drop
	@$(MAKE) db-create
	@$(MAKE) migrate

db-fresh: ## db-reset + seed — the normal loop after a schema edit (DESTRUCTIVE)
	@$(MAKE) db-reset
	@$(MAKE) seed

# The test database is created, migrated and seeded by internal/testsupport on
# the first DB-backed test, so `make test` needs neither of these. They exist for
# inspecting the state a failing test left behind, and for cleaning up after.
db-test-drop: ## Drop the test database (it is recreated by the next `make test`)
	@case "$(TEST_DB_NAME)" in *_test) ;; *) echo "refusing: TEST_DB_NAME=$(TEST_DB_NAME) does not end in _test"; exit 1 ;; esac
	mysql $(MYSQL_ARGS) -e "DROP DATABASE IF EXISTS \`$(TEST_DB_NAME)\`;"

db-test-shell: ## Open a mysql shell on the test database
	mysql $(MYSQL_ARGS) $(TEST_DB_NAME)

# Download the pinned standalone CLI for this platform. An unknown platform is a
# hard error: silently skipping the CSS build would serve an unstyled site and
# look like a template bug rather than a missing toolchain.
$(TAILWIND_BIN):
	@mkdir -p .tools
	@os=$$(uname -s); arch=$$(uname -m); \
	case "$$os/$$arch" in \
	  Darwin/arm64)        asset=tailwindcss-macos-arm64 ;; \
	  Darwin/x86_64)       asset=tailwindcss-macos-x64 ;; \
	  Linux/aarch64)       asset=tailwindcss-linux-arm64 ;; \
	  Linux/arm64)         asset=tailwindcss-linux-arm64 ;; \
	  Linux/x86_64)        asset=tailwindcss-linux-x64 ;; \
	  *) echo "unsupported platform $$os/$$arch — download the tailwindcss $(TAILWIND_VERSION) binary manually into $(TAILWIND_BIN)"; exit 1 ;; \
	esac; \
	echo "downloading $$asset $(TAILWIND_VERSION)"; \
	curl -fsSL -o $(TAILWIND_BIN) \
	  "https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/$$asset" \
	  || { rm -f $(TAILWIND_BIN); echo "download failed"; exit 1; }
	@chmod +x $(TAILWIND_BIN)

tailwind: $(TAILWIND_BIN) ## Build static/css/app.css once
	$(TAILWIND_BIN) -i $(CSS_IN) -o $(CSS_OUT) --minify

tailwind-watch: $(TAILWIND_BIN) ## Rebuild static/css/app.css on change
	$(TAILWIND_BIN) -i $(CSS_IN) -o $(CSS_OUT) --watch

# The vendored assets below are committed, so these targets are only run when a
# version or the icon list changes — never as part of a normal build.
assets: icons fonts js ## Re-vendor the icon sprite, fonts and Turbo

icons: ## Rebuild static/img/icons.svg from lucide + simple-icons (needs network)
	./scripts/build-icons.sh

fonts: ## Re-download the self-hosted woff2 fonts (needs network)
	./scripts/build-fonts.sh

js: ## Re-download static/js/turbo.js (needs network)
	./scripts/build-js.sh

clean: ## Remove build artifacts
	rm -rf bin tmp $(CSS_OUT)
