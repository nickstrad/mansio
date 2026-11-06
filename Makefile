# mansio local infrastructure: a SeaweedFS S3-compatible object store in Docker.
#
# Quickstart:   make up && make s3-init
#
# S3 endpoint (dev only):   http://localhost:8333
# Test credentials (DEV ONLY, public, not secrets; defined in docker-compose.yml):
#   access key id:      mansiodevkey
#   secret access key:  mansiodevsecret
#   region:             us-east-1   (SeaweedFS ignores it)
#   bucket:             mansio-test   (created by `make s3-init`)
#
# The Go server's durable agents and dbtool take the endpoint, bucket and keys in their config
# (server/README.md); the e2e suite starts its own SeaweedFS (work-scripts/bash/e2e-linux.sh).
#
# Requires Docker with Compose v2 (`docker compose`). `make s3-init` also needs the AWS CLI and
# installs it when missing (pip, falling back to apt).

SHELL := /bin/bash
.DEFAULT_GOAL := help

S3_ENDPOINT   := http://localhost:8333
S3_BUCKET     := mansio-test
# Deliberately not named AWS_*: a developer's real AWS credentials in the environment must never be
# sent to (or silently used for) the local store.
S3_ACCESS_KEY := mansiodevkey
S3_SECRET_KEY := mansiodevsecret
S3_REGION     := us-east-1

UP_TIMEOUT_SECONDS := 60

.PHONY: help up down restart ps logs clean s3-init db-shell

help: ## Show this help
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

up: ## Start the stack and wait (max 60s) until every service is healthy
	docker compose up -d
	@deadline=$$((SECONDS + $(UP_TIMEOUT_SECONDS))); \
	echo "Waiting up to $(UP_TIMEOUT_SECONDS)s for services to become healthy..."; \
	while true; do \
	  ids=$$(docker compose ps -q); \
	  bad=0; \
	  for id in $$ids; do \
	    st=$$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' $$id); \
	    case "$$st" in healthy|running) ;; *) bad=1 ;; esac; \
	  done; \
	  if [ -n "$$ids" ] && [ $$bad -eq 0 ]; then echo "All services healthy."; exit 0; fi; \
	  if [ $$SECONDS -ge $$deadline ]; then \
	    echo "ERROR: services not healthy after $(UP_TIMEOUT_SECONDS)s" >&2; \
	    docker compose ps >&2; docker compose logs --tail 30 >&2; exit 1; \
	  fi; \
	  sleep 2; \
	done

down: ## Stop and remove containers (keeps the seaweedfs-data volume)
	docker compose down

restart: down up ## down, then up

ps: ## Show service status
	docker compose ps

logs: ## Follow service logs
	docker compose logs -f

clean: ## Remove containers AND the seaweedfs-data volume (asks first)
	@echo "WARNING: this deletes the containers AND the seaweedfs-data volume (all stored objects)." >&2
	@printf 'Continue? [y/N] '; \
	read -r answer; \
	if [ "$$answer" = "y" ]; then \
	  docker compose down -v; \
	else \
	  echo "Aborted; nothing deleted."; \
	fi

s3-init: ## Create the mansio-test bucket and print endpoint + test credentials
	@if ! command -v aws >/dev/null 2>&1; then \
	  echo "AWS CLI not found; installing..."; \
	  (pip install awscli || pip3 install awscli || pip3 install --break-system-packages awscli \
	    || (apt-get update && apt-get install -y awscli)) \
	    || { echo "ERROR: could not install the AWS CLI; install it manually and re-run." >&2; exit 1; }; \
	fi
	@export AWS_ACCESS_KEY_ID=$(S3_ACCESS_KEY) AWS_SECRET_ACCESS_KEY=$(S3_SECRET_KEY) AWS_DEFAULT_REGION=$(S3_REGION); \
	if aws --endpoint-url $(S3_ENDPOINT) s3api head-bucket --bucket $(S3_BUCKET) >/dev/null 2>&1; then \
	  echo "Bucket $(S3_BUCKET) already exists."; \
	else \
	  aws --endpoint-url $(S3_ENDPOINT) s3api create-bucket --bucket $(S3_BUCKET) >/dev/null \
	    && echo "Created bucket $(S3_BUCKET)."; \
	fi
	@echo
	@echo "S3 endpoint:        $(S3_ENDPOINT)"
	@echo "Bucket:             $(S3_BUCKET)"
	@echo "Access key id:      $(S3_ACCESS_KEY)   (dev-only test credential)"
	@echo "Secret access key:  $(S3_SECRET_KEY)   (dev-only test credential)"
	@echo "Region:             $(S3_REGION)"

# Same dev defaults as the postgres service in docker-compose.yml (DEV ONLY).
POSTGRES_USER ?= mansio
POSTGRES_DB   ?= mansio

db-shell: ## Open an interactive psql shell in the postgres service
	docker compose exec postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)
