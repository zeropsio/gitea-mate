.PHONY: test lab-test lint build

# The unit suite. Everything runs against httptest fakes, so it needs no
# network and no credentials.
test:
	go test ./...

# The integration test against a real Gitea. It skips itself unless
# GITEA_LAB_URL and GITEA_LAB_ADMIN_TOKEN are set, and the sub-tests that mint a
# token skip unless GITEA_LAB_ADMIN_USER and GITEA_LAB_ADMIN_PASSWORD are too.
# The credentials reach it only through the environment — never a file, a
# fixture or a commit.
lab-test:
	go test ./internal/gitea/ -run TestLab -v -count=1

lint:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -o broker ./cmd/broker
