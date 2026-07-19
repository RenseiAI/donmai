#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
verify_script="${root_dir}/scripts/verify-release-tag.sh"
e2b_script="${root_dir}/scripts/build-e2b-template.cjs"
temp_dir=$(mktemp -d)
trap 'rm -rf "${temp_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_line() {
  local expected=$1
  local file=$2

  grep -Fqx -- "${expected}" "${file}" || fail "missing line '${expected}' in ${file}"
}

assert_no_line() {
  local unexpected=$1
  local file=$2

  if grep -Fqx -- "${unexpected}" "${file}"; then
    fail "unexpected line '${unexpected}' in ${file}"
  fi
}

valid_tags=(
  v0.0.0
  v1.2.3
  v1.2.3-0
  v1.2.3-alpha
  v1.2.3-alpha.1
  v1.2.3-0.3.7
  v1.2.3-x.7.z.92
  v1.2.3-01alpha
)
for tag in "${valid_tags[@]}"; do
  "${verify_script}" --validate "${tag}" || fail "valid tag rejected: ${tag}"
done

invalid_tags=(
  v1.2
  v1.2.3foo
  v1.2.3-
  v1.2.3-alpha.
  v1.2.3-alpha..1
  v1.2.3-01
  v01.2.3
  v1.02.3
  v1.2.03
  v1.2.3+build
  v1.2.3.
  latest
  main
  feature/v1.2.3
)
for tag in "${invalid_tags[@]}"; do
  if "${verify_script}" --validate "${tag}" >/dev/null 2>&1; then
    fail "invalid tag accepted: ${tag}"
  fi
done

fixture_repo="${temp_dir}/repo"
git init -q "${fixture_repo}"
git -C "${fixture_repo}" config user.name 'Release Test'
git -C "${fixture_repo}" config user.email 'release-test@example.com'
printf 'first\n' > "${fixture_repo}/fixture.txt"
git -C "${fixture_repo}" add fixture.txt
git -C "${fixture_repo}" commit -q -m first
first_commit=$(git -C "${fixture_repo}" rev-parse HEAD)
git -C "${fixture_repo}" tag -a v1.2.3 -m v1.2.3 "${first_commit}"
printf 'second\n' >> "${fixture_repo}/fixture.txt"
git -C "${fixture_repo}" commit -q -am second
second_commit=$(git -C "${fixture_repo}" rev-parse HEAD)
git -C "${fixture_repo}" tag -a v1.2.4 -m v1.2.4 "${second_commit}"
git -C "${fixture_repo}" tag -a v1.2.5-rc.1 -m v1.2.5-rc.1 "${second_commit}"
git -C "${fixture_repo}" branch v1.2.3 "${second_commit}"

# An automatic publication of the current stable tag advances all rolling
# aliases and is the only policy branch allowed to update the Homebrew cask.
git -C "${fixture_repo}" checkout -q --detach refs/tags/v1.2.4
push_output="${temp_dir}/push-output"
push_env="${temp_dir}/push-env"
(
  cd "${fixture_repo}"
  "${verify_script}" --verify v1.2.4 push "${push_output}" "${push_env}"
)
assert_line 'tag=v1.2.4' "${push_output}"
assert_line "commit=${second_commit}" "${push_output}"
assert_line 'is_prerelease=false' "${push_output}"
assert_line 'publish_rolling_aliases=true' "${push_output}"
assert_line 'publish_homebrew_cask=true' "${push_output}"
assert_line 'goreleaser_make_latest=true' "${push_output}"
assert_line 'e2b_additional_tags=default' "${push_output}"
assert_line 'ghcr.io/renseiai/donmai-worker:v1.2.4' "${push_output}"
assert_line 'ghcr.io/renseiai/donmai-worker:latest' "${push_output}"
assert_line 'GORELEASER_CURRENT_TAG=v1.2.4' "${push_env}"
assert_line 'GORELEASER_MAKE_LATEST=true' "${push_env}"
assert_line 'RELEASE_IS_PRERELEASE=false' "${push_env}"
assert_line 'RELEASE_PUBLISH_ROLLING_ALIASES=true' "${push_env}"
assert_line 'GORELEASER_PUBLISH_HOMEBREW=true' "${push_env}"

# A manual retry of the current highest stable tag keeps every rolling target
# unchanged and skips Homebrew while safely republishing versioned artifacts.
git -C "${fixture_repo}" checkout -q --detach refs/tags/v1.2.4
current_manual_output="${temp_dir}/current-manual-output"
current_manual_env="${temp_dir}/current-manual-env"
(
  cd "${fixture_repo}"
  "${verify_script}" --verify v1.2.4 workflow_dispatch "${current_manual_output}" "${current_manual_env}"
)
assert_line 'tag=v1.2.4' "${current_manual_output}"
assert_line 'is_prerelease=false' "${current_manual_output}"
assert_line 'publish_rolling_aliases=false' "${current_manual_output}"
assert_line 'publish_homebrew_cask=false' "${current_manual_output}"
assert_line 'goreleaser_make_latest=false' "${current_manual_output}"
assert_line 'e2b_additional_tags=' "${current_manual_output}"
assert_line 'ghcr.io/renseiai/donmai-worker:v1.2.4' "${current_manual_output}"
assert_no_line 'ghcr.io/renseiai/donmai-worker:latest' "${current_manual_output}"
assert_line 'GORELEASER_PUBLISH_HOMEBREW=false' "${current_manual_env}"

# A namespace-qualified older-tag checkout must beat a same-name branch, detach
# HEAD, and preserve every stable alias by skipping Homebrew and rolling targets.
git -C "${fixture_repo}" checkout -q --detach refs/tags/v1.2.3
older_manual_output="${temp_dir}/older-manual-output"
older_manual_env="${temp_dir}/older-manual-env"
(
  cd "${fixture_repo}"
  "${verify_script}" --verify v1.2.3 workflow_dispatch "${older_manual_output}" "${older_manual_env}"
)
assert_line 'tag=v1.2.3' "${older_manual_output}"
assert_line "commit=${first_commit}" "${older_manual_output}"
assert_line 'is_prerelease=false' "${older_manual_output}"
assert_line 'publish_rolling_aliases=false' "${older_manual_output}"
assert_line 'publish_homebrew_cask=false' "${older_manual_output}"
assert_line 'goreleaser_make_latest=false' "${older_manual_output}"
assert_line 'e2b_template_ref=donmai-worker:v1.2.3' "${older_manual_output}"
assert_line 'e2b_additional_tags=' "${older_manual_output}"
assert_line 'ghcr.io/renseiai/donmai-worker:v1.2.3' "${older_manual_output}"
assert_no_line 'ghcr.io/renseiai/donmai-worker:latest' "${older_manual_output}"
assert_line 'GORELEASER_CURRENT_TAG=v1.2.3' "${older_manual_env}"
assert_line 'GORELEASER_MAKE_LATEST=false' "${older_manual_env}"
assert_line 'RELEASE_IS_PRERELEASE=false' "${older_manual_env}"
assert_line 'RELEASE_PUBLISH_ROLLING_ALIASES=false' "${older_manual_env}"
assert_line 'GORELEASER_PUBLISH_HOMEBREW=false' "${older_manual_env}"

# A prerelease tag push publishes immutable version targets only. It must not
# advance stable aliases or update the stable Homebrew cask.
git -C "${fixture_repo}" checkout -q --detach refs/tags/v1.2.5-rc.1
prerelease_output="${temp_dir}/prerelease-output"
prerelease_env="${temp_dir}/prerelease-env"
(
  cd "${fixture_repo}"
  "${verify_script}" --verify v1.2.5-rc.1 push "${prerelease_output}" "${prerelease_env}"
)
assert_line 'tag=v1.2.5-rc.1' "${prerelease_output}"
assert_line "commit=${second_commit}" "${prerelease_output}"
assert_line 'is_prerelease=true' "${prerelease_output}"
assert_line 'publish_rolling_aliases=false' "${prerelease_output}"
assert_line 'publish_homebrew_cask=false' "${prerelease_output}"
assert_line 'goreleaser_make_latest=false' "${prerelease_output}"
assert_line 'e2b_template_ref=donmai-worker:v1.2.5-rc.1' "${prerelease_output}"
assert_line 'e2b_additional_tags=' "${prerelease_output}"
assert_line 'ghcr.io/renseiai/donmai-worker:v1.2.5-rc.1' "${prerelease_output}"
assert_no_line 'ghcr.io/renseiai/donmai-worker:latest' "${prerelease_output}"
assert_line 'GORELEASER_CURRENT_TAG=v1.2.5-rc.1' "${prerelease_env}"
assert_line 'GORELEASER_MAKE_LATEST=false' "${prerelease_env}"
assert_line 'RELEASE_IS_PRERELEASE=true' "${prerelease_env}"
assert_line 'RELEASE_PUBLISH_ROLLING_ALIASES=false' "${prerelease_env}"
assert_line 'GORELEASER_PUBLISH_HOMEBREW=false' "${prerelease_env}"

# A branch checkout is rejected even when the branch has the same name as the tag.
# Set HEAD symbolically because `git checkout refs/heads/...` intentionally
# detaches, which would not exercise the branch requirement.
git -C "${fixture_repo}" symbolic-ref HEAD refs/heads/v1.2.3
if (
  cd "${fixture_repo}"
  "${verify_script}" --verify v1.2.3 workflow_dispatch "${temp_dir}/branch-output" "${temp_dir}/branch-env"
) 2> "${temp_dir}/branch-error"; then
  fail 'same-name branch checkout was accepted'
fi
grep -Fq 'Release checkout must be detached' "${temp_dir}/branch-error" || fail 'branch rejection did not report detached requirement'

# A detached checkout at any commit other than the peeled tag commit is rejected.
git -C "${fixture_repo}" checkout -q --detach "${second_commit}"
if (
  cd "${fixture_repo}"
  "${verify_script}" --verify v1.2.3 workflow_dispatch "${temp_dir}/mismatch-output" "${temp_dir}/mismatch-env"
) 2> "${temp_dir}/mismatch-error"; then
  fail 'tag/HEAD mismatch was accepted'
fi
grep -Fq 'does not match v1.2.3' "${temp_dir}/mismatch-error" || fail 'tag mismatch did not report the mismatched tag'

# Workflow fixtures keep namespace-qualified dispatch checkouts and share one verifier.
for workflow in release.yml e2b-template.yml worker-image.yml; do
  workflow_path="${root_dir}/.github/workflows/${workflow}"
  grep -Fq "format('refs/tags/{0}', inputs.tag)" "${workflow_path}" || fail "${workflow} does not namespace manual tags"
  grep -Fq 'Checkout release automation' "${workflow_path}" || fail "${workflow} does not preserve current automation for older retries"
  grep -Fq 'RUNNER_TEMP}/verify-release-tag.sh" --verify' "${workflow_path}" || fail "${workflow} bypasses the staged shared release verifier"
done

grep -Fq 'GORELEASER_CURRENT_TAG' "${verify_script}" || fail 'verified tag is not exported to GoReleaser'
grep -Fq 'is_prerelease=' "${verify_script}" || fail 'common verifier does not expose prerelease status'
grep -Fq 'publish_rolling_aliases=' "${verify_script}" || fail 'common verifier does not expose rolling alias policy'
grep -Fq 'publish_homebrew_cask=' "${verify_script}" || fail 'common verifier does not expose Homebrew publication policy'
grep -Fq 'GORELEASER_MAKE_LATEST' "${root_dir}/.goreleaser.yaml" || fail 'GoReleaser latest policy is not wired to the verified release policy'
grep -Fq 'GORELEASER_PUBLISH_HOMEBREW' "${root_dir}/.goreleaser.yaml" || fail 'GoReleaser Homebrew policy is not wired to the verified release policy'
grep -Fq 'skip_upload:' "${root_dir}/.goreleaser.yaml" || fail 'GoReleaser Homebrew publisher cannot be skipped for safe retries'
grep -Fq 'runner.temp }}/goreleaser.yaml' "${root_dir}/.github/workflows/release.yml" || fail 'manual retries can fall back to an older GoReleaser publication policy'
grep -Fq 'steps.release.outputs.image_tags' "${root_dir}/.github/workflows/worker-image.yml" || fail 'worker image tags do not come from verified release policy'
grep -Fq 'steps.release.outputs.e2b_template_ref' "${root_dir}/.github/workflows/e2b-template.yml" || fail 'E2B build does not use the immutable version target'
grep -Fq 'steps.release.outputs.e2b_additional_tags' "${root_dir}/.github/workflows/e2b-template.yml" || fail 'E2B rolling default policy is not event-scoped'
grep -Fq 'RUNNER_TEMP}/build-e2b-template.cjs' "${root_dir}/.github/workflows/e2b-template.yml" || fail 'older E2B retries can lose the current tagged-build wrapper'

# The E2B build wrapper must pass the versioned target on every build and add
# the rolling default only when requested by an automatic tag push.
node - "${e2b_script}" <<'NODE'
const assert = require('node:assert/strict')
const scriptPath = process.argv[2]
const { buildTemplate } = require(scriptPath)

async function exercise(additionalTags) {
  const calls = []
  function Template() {
    return {
      fromDockerfile(dockerfile) {
        calls.push({ dockerfile })
        return {
          setStartCmd(start, ready) {
            calls.push({ start, ready })
            return { built: true }
          },
        }
      },
    }
  }
  Template.build = async (template, target, options) => {
    calls.push({ template, target, options })
    return { templateId: 'tpl_123', buildId: 'build_456', tags: options.tags }
  }

  const result = await buildTemplate({
    sdk: { Template, defaultBuildLogger: () => 'logger' },
    dockerfile: 'FROM alpine:3.20\n',
    fileContextPath: '/tmp/context',
    templateRef: 'donmai-worker:v1.2.3',
    additionalTags,
    apiKey: 'test-key',
  })

  const buildCall = calls.find((call) => call.target)
  assert.equal(buildCall.target, 'donmai-worker:v1.2.3')
  assert.deepEqual(buildCall.options.tags, additionalTags)
  assert.equal(result.buildId, 'build_456')
}

Promise.resolve()
  .then(() => exercise([]))
  .then(() => exercise(['default']))
  .catch((error) => {
    console.error(error)
    process.exitCode = 1
  })
NODE

printf 'release workflow tests: PASS\n'
