# Shared Tool CI Template (M9.4.d)

Staging directory for the GitHub Actions workflow template that every
Watchkeeper tool repo — the platform `watchkeeper-tools` repo AND
every customer-private tool repo — consumes via a single
`workflow_call` reference.

## Files

| File                            | Purpose                                                       |
| ------------------------------- | ------------------------------------------------------------- |
| `tool-ci.yml`                   | The reusable workflow. Trigger: `workflow_call` only.         |
| `example-consumer-workflow.yml` | One-line stub a consumer drops at `.github/workflows/ci.yml`. |
| `README.md`                     | This file — publishing flow + versioning policy.              |

## Publishing flow

The platform `watchkeeper-tools` repo is the canonical home of the
template (per the roadmap M9.4.d text). The core repo (this one)
authors the file; on each release the file is copied to the platform
repo:

```sh
# In a release operator's shell, with both repos checked out side-by-side:
cp wathkeepers/tools-builtin/ci-template/tool-ci.yml \
   watchkeeper-tools/.github/workflows/tool-ci.yml

# Then in watchkeeper-tools/:
git add .github/workflows/tool-ci.yml
git commit -m "release: tool-ci.yml vX.Y.Z (mirror of wathkeepers @ <sha>)"
git tag -a vX.Y.Z -m "tool-ci.yml release vX.Y.Z"
git push origin main vX.Y.Z
```

Consumers pin to a tag:

```yaml
uses: watchkeeper-tools/watchkeeper-tools/.github/workflows/tool-ci.yml@vX.Y.Z
```

## Versioning policy

Semver across two axes:

- **Major (`vX.0.0`)** — the gate vocabulary changed (a new
  `GateName` landed in the M9.4.b reviewer; an old gate retired) OR
  an input default changed in a way that could break existing
  consumers (coverage floor raised, signing default flipped).
- **Minor (`v1.Y.0`)** — a new optional input added; a per-gate
  failure surface expanded backward-compatibly; a new pinned actions
  SHA bumped; an input default bumped backward-compatibly within
  its existing range (e.g., Node.js patch / minor security update).
- **Patch (`v1.0.Z`)** — clarification-only doc edits; pure bug fixes
  that do not change accepted inputs.

### Coverage threshold default

The template's `coverage_threshold` input defaults to **80**. The
`wathkeepers` core repo's `vitest.config.ts` floor is **60** — the
divergence is deliberate: tool repos ship a single focused tool and
should sit at a stricter floor than the core orchestrator package
suite. A consumer who wants to match the core baseline passes
`coverage_threshold: 60` explicitly in their workflow stub. The
operator-runbook's per-gate remediation guide flags this as a common
"local-green / CI-red" trap.

The `wathkeepers` core repo's `core/pkg/approval/tool_ci_template_test.go`
test pins the gate-vocabulary contract: it reads `tool-ci.yml` and
asserts every `GateName` const has a step with the matching `name:`
value. A breaking vocabulary change in the reviewer therefore forces
a coordinated template bump.

## Local validation

The template is yamllint-checked by the core repo's meta-ci job
(GitHub Action: `ibiqlik/action-yamllint`) and the Go contract test
runs in the core repo's `go-ci` job. To reproduce yamllint locally,
install yamllint via pipx (NOT pnpm — yamllint is a Python tool, not
an npm package):

```sh
pipx install yamllint==1.x.y     # one-time install
yamllint -c .yamllint.yml tools-builtin/ci-template/tool-ci.yml
```

The Go contract test mirrors the CI gate:

```sh
go test -run TestM94D_Template ./core/pkg/approval/...
```

## Consumer expectations

A tool repo consuming this template must have:

- `manifest.json` at the repo root, conforming to
  `core/pkg/toolregistry/manifest.go:Manifest` (required:
  `name`, `version`, `capabilities`, `schema`, `dry_run_mode`).
  Top-level keys outside the documented set fail the
  `capability_declaration` gate (mirrors the Go decoder's
  `DisallowUnknownFields`).
- `package.json` with `pnpm install` working under
  `pnpm@${pnpm_version}` (default `10.33.0`).
- `pnpm-lock.yaml` at the repo root. The template runs
  `pnpm install --frozen-lockfile` which fails without a lockfile.
- `tsconfig.json` for `tsc --noEmit`.
- `src/**/*.ts` source files (non-test) and `src/**/*.test.ts`
  vitest test files. Symlinks under `src/` ARE followed by both the
  `undeclared_fs_net` walker and the `sign` step's bundle hash;
  cycles are detected via a realpath visited-set.

See `docs/operator-runbook.md` for the per-gate failure-and-remediation
guide.
