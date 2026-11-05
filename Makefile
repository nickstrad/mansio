# ts-db-go Makefile. One section per workstream; add yours as a new section (edit under
# `scripts/lock.sh Makefile ...`). `make help` lists the documented targets.

.PHONY: help
help:
	@grep -hE "^#   make " Makefile | sed "s/^#   //"

# ---------------------------------------------------------------------------------------------------------
# coldload (owner: coldload agent): SQLite files in SeaweedFS → virtual actors. State (archived): docs/state_files/STATE-coldload.md
#
#   make coldload-bench CONFIG=sizes     SeaweedFS up → seed → run → report → full cleanup (KEEP=1 keeps data)
#   make coldload-smoke                  the same with configs/coldload/smoke.json (~10 s)
#   make coldload-test                   unit + integration tests, both assertion builds
#   make coldload-test-s3                S3 contract suite against SeaweedFS (starts it)
#   make coldload-fuzz                   model-based fuzz of the virtual-actor host (FUZZTIME=30s)
#   make coldload-clean[-objects|-all|-fuzz]   free disk (see scripts/coldload/cleanup.sh)
#   make coldload-disk                   what coldload is using now

CONFIG   ?= smoke
FUZZTIME ?= 30s
KEEP     ?=
COLDLOAD_PKGS := ./internal/assert ./internal/errs ./internal/logx ./internal/coldload ./internal/vactor/... \
                 ./internal/objstore ./cmd/coldload
SEAWEED  := docker compose -f deploy/seaweedfs/docker-compose.yml

.PHONY: coldload-build coldload-test coldload-test-s3 coldload-fuzz coldload-up coldload-down coldload-plan \
        coldload-bench coldload-smoke coldload-report coldload-clean coldload-clean-objects coldload-clean-all \
        coldload-clean-fuzz coldload-disk coldload-check

coldload-build:
	go build -o bin/coldload ./cmd/coldload

coldload-test:
	go vet $(COLDLOAD_PKGS)
	go test -race -count=1 $(COLDLOAD_PKGS)
	go test -count=1 -tags noassert $(COLDLOAD_PKGS)

coldload-test-s3: coldload-up
	COLDLOAD_S3=localhost:8333 go test -count=1 -run 'S3|Objstore' ./cmd/coldload

coldload-fuzz:
	go test -run='^$$' -fuzz=FuzzHostModel -fuzztime=$(FUZZTIME) ./internal/vactor

coldload-check: coldload-test
	@test -z "$$(gofmt -l cmd/coldload internal/coldload internal/vactor internal/objstore internal/assert internal/errs internal/logx)" \
		|| (echo "needs gofmt:"; gofmt -l cmd/coldload internal/coldload internal/vactor internal/objstore; exit 1)

coldload-up:
	@curl -s -o /dev/null localhost:8333 || { $(SEAWEED) up -d && for i in $$(seq 30); do curl -s -o /dev/null localhost:8333 && break; sleep 1; done; }

coldload-down:
	$(SEAWEED) down

coldload-plan: coldload-build
	bin/coldload -config configs/coldload/$(CONFIG).json plan

# Full benchmark. Always ends with cleanup: local files after every run; SeaweedFS objects and volume too
# unless KEEP=1 (keep them to rerun quickly with `scripts/coldload/run.sh configs/coldload/$(CONFIG).json run`).
coldload-bench: coldload-up
	@status=0; scripts/coldload/run.sh configs/coldload/$(CONFIG).json all || status=$$?; \
	if [ -n "$(KEEP)" ]; then scripts/coldload/cleanup.sh local; else scripts/coldload/cleanup.sh all; fi; \
	exit $$status

coldload-smoke:
	$(MAKE) coldload-bench CONFIG=smoke

coldload-report:
	scripts/coldload/report.sh results/coldload/$(CONFIG)

coldload-clean:
	scripts/coldload/cleanup.sh local

coldload-clean-objects:
	scripts/coldload/cleanup.sh objects

coldload-clean-all:
	scripts/coldload/cleanup.sh all

coldload-clean-fuzz:
	scripts/coldload/cleanup.sh fuzz

coldload-disk:
	@echo "results/coldload: $$(du -sh results/coldload 2>/dev/null | cut -f1)"
	@echo "templates:        $$(du -sh results/coldload/templates 2>/dev/null | cut -f1 || echo none)"
	@echo "seaweedfs volume: $$(docker system df -v 2>/dev/null | awk '$$1 == "seaweedfs_seaweed-data" {print $$3}' || true)"
	@echo "go fuzz cache:    $$(du -sh $$(go env GOCACHE)/fuzz 2>/dev/null | cut -f1)"
	@echo "host free:        $$(df -h . | awk 'NR == 2 {print $$4}')"
