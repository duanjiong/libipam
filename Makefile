CRD_OPTIONS ?= "crd:trivialVersions=true"

GENERATED_FILES:=./lib/apis/v1alpha1/zz_generated.deepcopy.go

.PHONY: gen-files manifests
## Force rebuild generated go utilities (e.g. deepcopy-gen) and generated files
gen-files:
	rm -rf $(GENERATED_FILES)
	make $(GENERATED_FILES)

./lib/apis/v1alpha1/zz_generated.deepcopy.go:
	controller-gen object:headerFile=./docs/boilerplate.go.txt paths="./lib/apis/v1alpha1"

# Generate manifests e.g. CRD, RBAC etc.
manifests:
	controller-gen $(CRD_OPTIONS) rbac:roleName=manager-role webhook paths="./..." output:crd:artifacts:config=config/crd/bases

macvtap:
	go get github.com/googleapis/gnostic@v0.4.0
	go build ./plugin/macvtap/ipam.go

# Build the docker image
docker-build:
	docker build . -t ${IMG} -f ./build/Dockerfile

# Push the docker image
docker-push:
	docker push ${IMG}

