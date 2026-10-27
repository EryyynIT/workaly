.PHONY: test proto smoke

test:
	cd auth-service && go test -race ./...
	cd auth-service && go vet ./...
	cargo fmt --manifest-path user-service/Cargo.toml -- --check
	cargo test --locked --manifest-path user-service/Cargo.toml
	cargo clippy --locked --manifest-path user-service/Cargo.toml --all-targets -- -D warnings

proto:
	bash scripts/generate-proto.sh

smoke:
	python3 scripts/smoke.py
