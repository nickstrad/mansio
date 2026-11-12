# mansio-server Makefile: benchmarks, focused test runs and the deterministic simulation, one section per area.
# The full Go gate is `deno task go:ci` at the repo root. `make help` lists the documented targets. Targets that
# say "under the bench lock" hold results/.bench-lock (scripts/lock.sh) so two benchmarks never share the machine.

.PHONY: help
help:
	@grep -hE "^#   make " Makefile | sed "s/^#   //"

# ---------------------------------------------------------------------------------------------------------
# coldload: how fast a SQLite file in SeaweedFS (S3) becomes a ready virtual actor, by size and concurrency;
# also the tests and fuzzing of the virtual-actor host and the S3 client. Results: docs/notes/coldload/.
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
                 ./internal/objstore ./cmd/bench/coldload
SEAWEED  := docker compose -f deploy/seaweedfs/docker-compose.yml

.PHONY: coldload-build coldload-test coldload-test-s3 coldload-fuzz coldload-up coldload-down coldload-plan \
        coldload-bench coldload-smoke coldload-report coldload-clean coldload-clean-objects coldload-clean-all \
        coldload-clean-fuzz coldload-disk coldload-check

coldload-build:
	go build -o bin/coldload ./cmd/bench/coldload

coldload-test:
	go vet $(COLDLOAD_PKGS)
	go test -race -count=1 $(COLDLOAD_PKGS)
	go test -count=1 -tags noassert $(COLDLOAD_PKGS)

coldload-test-s3: coldload-up
	COLDLOAD_S3=localhost:8333 go test -count=1 -run 'S3|Objstore' ./cmd/bench/coldload

coldload-fuzz:
	go test -run='^$$' -fuzz=FuzzHostModel -fuzztime=$(FUZZTIME) ./internal/vactor

coldload-check: coldload-test
	@test -z "$$(gofmt -l cmd/bench/coldload internal/coldload internal/vactor internal/objstore internal/assert internal/errs internal/logx)" \
		|| (echo "needs gofmt:"; gofmt -l cmd/bench/coldload internal/coldload internal/vactor internal/objstore; exit 1)

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

# ---------------------------------------------------------------------------------------------------------
# sqlbench: one goroutine (actor) per SQLite database under open-loop generated load; tests, fuzzing, coverage
# and benchmark runs of the actor and its load generator. Results: docs/notes/sqlbench/.
#
#   make sqlbench-test                   vet + unit/integration tests, -race and -tags noassert
#   make sqlbench-e2e                    build the binary and run it end to end (-tags e2e)
#   make sqlbench-check                  gofmt + sqlbench-test + sqlbench-e2e
#   make sqlbench-fuzz                   every sqlbench fuzz target for FUZZTIME each, under the bench lock
#   make sqlbench-cover                  coverage of sqlbench packages (per function, lowest first)
#   make sqlbench-run CONFIG=smoke       build, run configs/$(CONFIG).json under the bench lock, print the report

SQLBENCH_PKGS := ./internal/actor ./internal/bench ./internal/panicx ./internal/load ./internal/stats ./internal/schema \
                 ./internal/workload ./internal/retry ./internal/outcome ./internal/sqlitedsn ./cmd/bench/sqlbench ./queries
SQLBENCH_FUZZ := internal/actor:FuzzActorModel internal/schema:FuzzGenerate internal/workload:FuzzStatementsRun \
                 internal/load:FuzzPacerSendsTheIntegratedRate internal/stats:FuzzRecordFormatRoundTrip \
                 internal/retry:FuzzDelayBounds internal/outcome:FuzzParse

.PHONY: sqlbench-test sqlbench-e2e sqlbench-check sqlbench-fuzz sqlbench-cover sqlbench-run

sqlbench-test:
	go vet $(SQLBENCH_PKGS)
	go test -race -count=1 $(SQLBENCH_PKGS)
	go test -count=1 -tags noassert $(SQLBENCH_PKGS)

sqlbench-e2e:
	go test -count=1 -tags e2e ./cmd/bench/sqlbench

sqlbench-check:
	@test -z "$$(gofmt -l $(patsubst ./%,%,$(SQLBENCH_PKGS)))" || (echo "needs gofmt:"; gofmt -l $(patsubst ./%,%,$(SQLBENCH_PKGS)); exit 1)
	$(MAKE) sqlbench-test sqlbench-e2e

sqlbench-fuzz:
	LOCK_WHO=sqlbench scripts/lock.sh results/.bench-lock sh -c 'set -e; for t in $(SQLBENCH_FUZZ); do \
		go test -run="^$$" -fuzz="^$${t#*:}$$" -fuzztime=$(FUZZTIME) ./$${t%%:*}; done'

sqlbench-cover:
	go test -count=1 -coverpkg=$$(echo $(SQLBENCH_PKGS) | tr ' ' ,) -coverprofile=results/sqlbench-cover.out $(SQLBENCH_PKGS) >/dev/null
	go tool cover -func=results/sqlbench-cover.out | sort -k3 -n | head -25
	go tool cover -func=results/sqlbench-cover.out | tail -1

sqlbench-run:
	scripts/run.sh configs/$(CONFIG).json

# ---------------------------------------------------------------------------------------------------------
# front: agentd's network front (tickets, admission, wire codes) and the frontbench load client.
# Results: docs/notes/front/.
#
#   make front-test                      vet + tests, -race and -tags noassert
#   make front-e2e                       build agentd + frontbench and run them together (-tags e2e)
#   make front-check                     gofmt + front-test + front-e2e
#   make front-fuzz                      every front fuzz target for FUZZTIME each, under the bench lock
#   make front-bench                     the frontbench matrix recorded in docs/notes/front, under the bench lock

FRONT_PKGS := ./internal/ticket/... ./internal/front/... ./cmd/agentd ./cmd/bench/frontbench
FRONT_DIRS := internal/ticket internal/front cmd/agentd cmd/bench/frontbench
FRONT_FUZZ := internal/ticket:FuzzVerify internal/front:FuzzDecodeRequest internal/front:FuzzLimiterModel

.PHONY: front-test front-e2e front-check front-fuzz front-bench

front-test:
	go vet $(FRONT_PKGS)
	go test -race -count=1 $(FRONT_PKGS)
	go test -count=1 -tags noassert $(FRONT_PKGS)

front-e2e:
	go test -count=1 -tags e2e ./cmd/bench/frontbench

front-check:
	@test -z "$$(gofmt -l $(FRONT_DIRS))" || (echo "needs gofmt:"; gofmt -l $(FRONT_DIRS); exit 1)
	$(MAKE) front-test front-e2e

front-fuzz:
	LOCK_WHO=front scripts/lock.sh results/.bench-lock sh -c 'set -e; for t in $(FRONT_FUZZ); do \
		go test -run="^$$" -fuzz="^$${t#*:}$$" -fuzztime=$(FUZZTIME) ./$${t%%:*}; done'

front-bench:
	LOCK_WHO=front scripts/lock.sh results/.bench-lock scripts/front/bench.sh

# ---------------------------------------------------------------------------------------------------------
# sim: deterministic simulation of one agent and of a cluster from a seed (internal/sim). Notes:
# docs/notes/determinism/.
#
#   make sim-test                        the simulations' default seeds (single node and cluster)
#   make sim-swarm SEEDS=2000            nightly swarm: many seeds, varied fault rates, shrink failures

SEEDS ?= 2000

.PHONY: sim-test sim-swarm

sim-test:
	go test -count=1 ./internal/sim

sim-swarm:
	scripts/sim-swarm.sh $(SEEDS)
