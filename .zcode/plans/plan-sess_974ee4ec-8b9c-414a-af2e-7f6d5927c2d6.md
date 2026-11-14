# GitHub Actions: CI + Release (Docker image & standalone binaries)

## Context

`myhealthz` (Go 1.26, static CGO-free binary, distroless Containerfile) has no CI or release automation. This adds two workflows: a quality-gate CI on every push/PR, and a tag-driven release that publishes a multi-arch container image to GHCR and standalone binaries to GitHub Releases. All action versions verified against their GitHub releases pages today (2026-11-14).

## Files to create

### 1. `.github/workflows/ci.yml` — quality gate
- **Triggers:** `push` to `main`, `pull_request`. Add `concurrency` group to cancel superseded runs.
- **Job `test`:** ubuntu-latest → checkout@v7, setup-go@v7 (`go-version-file: go.mod` → picks Go 1.26), then `go build`, `go vet ./...`, `go test ./... -race -coverprofile=coverage.out -covermode=atomic` (runner has gcc, so `-race` works; production builds stay CGO-free). Upload coverage as artifact (upload-artifact@v7).
- **Job `lint`:** golangci/golangci-lint-action@v9 (`version: latest`) — covers the same ground as the Makefile `lint` target's staticcheck/golangci-lint.

### 2. `.github/workflows/release.yml` — distribution on tag push `v*`
Top-level `permissions`: `contents: write`, `packages: write`.

- **Job `test`** — same steps as CI test job; blocks release of broken code.
- **Job `image`** (`needs: test`):
  - setup-qemu-action@v4 + setup-buildx-action@v4 (multi-arch)
  - login-action@v4 → `ghcr.io` using `${{ github.actor }}` / `${{ secrets.GITHUB_TOKEN }}`
  - metadata-action@v6 → tags: `{{version}}` (e.g. `1.2.3`), `{{major}}.{{minor}}` (e.g. `1.2`), `latest`; OCI labels from go.mod/README
  - build-push-action@v7 → `platforms: linux/amd64,linux/arm64`, `context: .`, `file: Containerfile`, `push: true`, `cache-from/to: type=gha`
- **Job `binaries`** (`needs: test`): build matrix over `[linux/amd64, linux/arm64, darwin/amd64, darwin/arm64]` — `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"` (same flags as `make build`), packaged as `myhealthz_<version>_<os>_<arch>.tar.gz`, uploaded per-target with upload-artifact@v7.
- **Job `release`** (`needs: [test, image, binaries]`): download-artifact@v8 merges binary artifacts into `dist/`, generates `checksums.txt` (sha256), then softprops/action-gh-release@v3 attaches `dist/*` with `generate_release_notes: true`.

### 3. `.github/dependabot.yml`
`github-actions` ecosystem, weekly — keeps these actions on latest majors going forward.

### 4. `README.md` — short "Install" section
Points at `ghcr.io/sriharip316/myhealthz:<tag>` and the prebuilt binaries on the Releases page (notes: linux amd64/arm64, darwin amd64/arm64).

## Action versions used (latest as of 2026-11-14)

| Action | Version |
|---|---|
| actions/checkout | v7 (7.0.1) |
| actions/setup-go | v7 (7.4.0) |
| actions/upload-artifact | v7 (7.0.2) |
| actions/download-artifact | v8 (8.0.2) |
| docker/setup-qemu-action | v4 (4.4.0) |
| docker/setup-buildx-action | v4 (4.4.1) |
| docker/login-action | v4 (4.8.0) |
| docker/metadata-action | v6 (6.2.0) |
| docker/build-push-action | v7 (7.0.0) |
| softprops/action-gh-release | v3 (3.0.4) |
| golangci/golangci-lint-action | v9 (9.3.0) |

## Verification

1. Lint the workflow YAML with `actionlint` if installable locally, else parse-validate YAML (python3 -c yaml.safe_load).
2. `make build test vet` locally to confirm the CI commands pass as written.
3. Push to `main` and confirm both CI jobs go green (user does the push).
4. Cut a throwaway tag (e.g. `v0.0.1`) to exercise the full release path: GHCR image (both arches), tarballs + checksums on the GitHub Release, then delete the tag/release if it was only a smoke test.

## Notes

- GHCR works with the built-in `GITHUB_TOKEN` (packages:write) — no secrets to configure.
- First GHCR publish makes the package private by default under your account; flip it to public in the package settings if you want anonymous pulls.
- No code changes to `myhealthz` itself (no version stamping via ldflags — `main.go` has no version var; can be added later if wanted).
