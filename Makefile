# Everything targets the kind cluster by name, never the current kubectl context.
CLUSTER  := workloadguard
CONTEXT  := kind-$(CLUSTER)
VERSION  ?= 0.1.0
IMAGE    := workloadguard:$(VERSION)
KUBECTL  := kubectl --context $(CONTEXT)

.PHONY: all cluster samples image load deploy port-forward verify test vet clean

## all: cluster, sample workloads, build and deploy the tool, then verify
all: cluster samples deploy verify

## cluster: create the kind cluster (no-op if it exists)
cluster:
	./hack/setup.sh

## samples: deploy the sample workloads and wait for them
samples:
	$(KUBECTL) apply -k examples/
	for ns in tenant-a tenant-b shared legacy; do \
		$(KUBECTL) -n $$ns rollout status deploy --timeout=180s || exit 1; \
	done

## image: build the container image
image:
	docker build -t $(IMAGE) .

## load: copy the image into the kind nodes (no registry needed)
load: image
	kind load docker-image $(IMAGE) --name $(CLUSTER)

## deploy: build, load and deploy the tool, then wait for it to be ready
deploy: load
	$(KUBECTL) apply -k deploy/
	# Same tag, new image: restart so the pod picks up the freshly loaded build.
	$(KUBECTL) -n workloadguard rollout restart deploy/workloadguard
	$(KUBECTL) -n workloadguard rollout status deploy/workloadguard --timeout=120s

## port-forward: reach the API on localhost:8080
port-forward:
	$(KUBECTL) -n workloadguard port-forward svc/workloadguard 8080:80

## verify: check connectivity between the sample workloads
verify:
	./hack/verify.sh

## test: run the unit tests
test:
	go test ./...

## vet: static checks
vet:
	go vet ./...
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

## clean: delete the kind cluster
clean:
	kind delete cluster --name $(CLUSTER)

## help: list targets
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST)
