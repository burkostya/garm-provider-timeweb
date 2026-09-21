# Releasing

Releases contain static Linux `amd64` and `arm64` provider executables, configuration examples, documentation, license, build metadata, and a SHA-256 checksum file. The archive architecture describes the **GARM controller host**. This release creates Linux `amd64` runners only.

## Local verification

Use Go 1.26.8 to match CI, GNU Make, GNU tar, GNU date, and gzip on Linux:

```bash
go mod verify
make check
make dist VERSION=v0.1.0
(cd dist && sha256sum --check SHA256SUMS)
```

`make check` runs formatting checks, all Go tests with the race detector, `go vet`, and a static build. `make dist` cross-compiles both architectures without calling Timeweb. A cloud account or token is not required for these checks.

The archive names are:

- `garm-provider-timeweb_v0.1.0_linux_amd64.tar.gz`
- `garm-provider-timeweb_v0.1.0_linux_arm64.tar.gz`
- `SHA256SUMS`

Inspect the `amd64` executable on an `amd64` Linux host and check the archive contents before publishing. `BUILDINFO` records the tag, commit, source date, and Go toolchain. Builds use `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, the source commit timestamp, sorted archive entries, fixed archive ownership, and gzip without timestamps. Repeating a build with identical source, metadata, toolchain, and build tools should produce identical checksums.

You can select a Go executable with `GO=/absolute/path/to/go`. `COMMIT` and `SOURCE_DATE_EPOCH` override the source metadata when building outside a full checkout.

## Publish a version

1. Complete local verification and review the CI result for the exact commit.
2. Add release notes at `docs/releases/<tag>.md` and set the same tag in the root `VERSION` file.
3. For a production-ready release, complete [the live smoke test](smoke-test.md) and record its results. Do not mark an untested cloud integration as verified.
4. Commit the release changes and merge or push them to `main`. A change to `VERSION` on `main` starts the Release workflow. After checks succeed, it atomically creates the tag at that exact commit and publishes the release. Choose a fresh version: the workflow refuses a `VERSION` value whose tag already exists, including a tag created concurrently while the build runs.
5. Check the [Release workflow](https://github.com/burkostya/garm-provider-timeweb/actions/workflows/release.yml) and inspect the attached archives and checksums on the resulting GitHub Release.

Maintainers with Git tag access can alternatively tag an already-verified commit and push that tag:

   ```bash
   git tag -a v0.1.0 -m 'v0.1.0'
   git push origin v0.1.0
   ```

Tag pushes run the same checks and build path, requiring that the pushed tag already exists. Path filters apply to branch pushes, so changing ordinary source files without changing `VERSION` runs CI without publishing a release.

The workflow repeats checks against the exact release source before building or publishing. It uses only the repository's `GITHUB_TOKEN` with `contents: write`; no cloud credentials or third-party release action is needed. A tag created by this token does not recursively start the tag-push workflow. [GitHub workflow trigger documentation](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow).

The workflow creates a new release and does not overwrite an existing release on a rerun. If publishing fails after creating the tag or a partial release, inspect that state before recovery; do not move a release tag to a new commit.

`v0.1.0` is deliberately published as a GitHub **prerelease**, despite its numeric tag, while live Timeweb validation is pending. Tags containing a hyphen, such as `v0.2.0-rc.1`, are also prereleases. Other version tags publish stable releases, so maintainers must finish the live validation before tagging the next stable version.

## Upstream provider listing

GARM's provider table is maintained by the GARM project. Publishing this repository does not add Timeweb to that table or imply upstream endorsement. Once the provider has a usable release and a recorded smoke test, submit a separate change to [GARM's supported providers](https://github.com/cloudbase/garm#supported-providers), initially identifying the provider as experimental if appropriate.
