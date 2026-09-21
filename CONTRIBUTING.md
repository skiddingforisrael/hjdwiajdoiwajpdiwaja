# Contributing

Cone welcomes focused bug fixes and performance improvements.

1. Fork the repository and create a branch from `master`.
2. Keep credentials out of commits. Copy `.env_example.go` to `.env.go` only for local testing.
3. Run `gofmt` on changed Go files.
4. Run `go test -race ./...`, `go vet ./...`, and `go build ./...`.
5. Open a pull request explaining the behavior change and how it was tested.

For mesh-output changes, include a small synthetic regression test. Do not
commit copyrighted texture packs, generated Roblox assets, or compiled release
binaries. Tagged-release automation builds binaries from the reviewed source.
