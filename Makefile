STUBS ?= stubs
# A calibrated profile measured from a real provider stays out of the repository (local/ is ignored):
#   make edge CONFIG=local/routes.prod.json
CONFIG ?= routes.json

# NDC edge mock (Go). Offer count, latency and carrier come from the JSON configs.
build:       ; cd edge && go build -o ndc-edge-mock .
	cd client && go build -o ../bin/ndc ./cmd/ndc && go build -o ../bin/ndc-mcp ./cmd/ndc-mcp

edge:        ; cd edge && go build -o ndc-edge-mock . && ./ndc-edge-mock -stubs $(abspath $(STUBS)) \
                 -config ../$(CONFIG) -versions ../versions.json -airlines ../airlines.json -translations ../translations
edge-check:  ; ./check-edge.sh
check-flow:  ; ./check-flow.sh
check-xlate: ; ./check-translate.sh
check-proxy: ; ./check-proxy.sh
check-mcp:   ; ./check-mcp.sh
check-replicas: ; STUBS=$(STUBS) ./check-replicas.sh
unit:        ; cd edge && go test -race ./...
image:       ; docker build -t ndc-mock:dev .
bruno:       ; cd bruno && npx --yes @usebruno/cli@latest run --env local -r
test:        ; $(MAKE) build && $(MAKE) unit && $(MAKE) edge-check && $(MAKE) check-flow && $(MAKE) check-xlate && $(MAKE) check-proxy && $(MAKE) check-mcp && $(MAKE) check-replicas && $(MAKE) bruno

# Environment-specific downstream stubs (WireMock). Not part of the public tool.
down:        ; cd local && docker-compose up -d
down-check:  ; cd local && ./check.sh
down-stop:   ; cd local && docker-compose down

.PHONY: build edge edge-check check-flow check-xlate check-proxy check-mcp check-replicas unit image bruno test down down-check down-stop
