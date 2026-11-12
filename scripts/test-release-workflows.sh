#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
verify_script="${root_dir}/scripts/verify-release-tag.sh"
authority_script="${root_dir}/scripts/verify-release-authority.sh"
fast_lane_script="${root_dir}/scripts/release-fast-lane.sh"
signing_key_test="${root_dir}/scripts/test-release-signing-key.sh"
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

assert_workflow_job_needs() {
  local workflow_file=$1
  local job_name=$2
  local dependency=$3

  ruby -ryaml - "${workflow_file}" "${job_name}" "${dependency}" <<'RUBY'
workflow_file, job_name, dependency = ARGV

begin
  workflow = YAML.safe_load(
    File.read(workflow_file),
    permitted_classes: [],
    permitted_symbols: [],
    aliases: false
  )
rescue Psych::Exception, SystemCallError => error
  abort "FAIL: cannot parse #{workflow_file}: #{error.message}"
end

jobs = workflow["jobs"]
abort "FAIL: #{workflow_file} does not declare jobs" unless jobs.is_a?(Hash)
abort "FAIL: #{workflow_file} does not declare jobs.#{dependency}" unless jobs.key?(dependency)

job = jobs[job_name]
abort "FAIL: #{workflow_file} does not declare jobs.#{job_name}" unless job.is_a?(Hash)

needs = job["needs"]
dependencies = needs.is_a?(String) ? [needs] : needs
unless dependencies.is_a?(Array) && dependencies.all? { |item| item.is_a?(String) }
  abort "FAIL: #{workflow_file} jobs.#{job_name}.needs must be a job name or list of job names"
end
unless dependencies.include?(dependency)
  abort "FAIL: #{workflow_file} jobs.#{job_name}.needs does not include #{dependency}"
end
RUBY
}

assert_workflow_job_needs "${root_dir}/.github/workflows/release.yml" release harness-smoke
assert_workflow_job_needs "${root_dir}/.github/workflows/release.yml" release release-authority
assert_workflow_job_needs "${root_dir}/.github/workflows/release.yml" release fast-lane
assert_workflow_job_needs "${root_dir}/.github/workflows/release.yml" harness-smoke fast-lane
assert_workflow_job_needs "${root_dir}/.github/workflows/release.yml" fast-lane release-authority

# The switchable fast lane may skip harness-smoke and nothing else. Evaluate the
# real job conditions over every outcome of the jobs they depend on, so a
# reworded condition that opens a publish path without a passed authority check,
# or without either a passed smoke or an attested skip, fails here.
ruby -ryaml - "${root_dir}/.github/workflows/release.yml" <<'RUBY'
release_file = ARGV.fetch(0)
workflow = YAML.safe_load(File.read(release_file), permitted_classes: [], permitted_symbols: [], aliases: false)
jobs = workflow.fetch('jobs')

fast_lane = jobs.fetch('fast-lane')
abort 'FAIL: fast-lane must only read contents and commit statuses' unless fast_lane['permissions'] == { 'contents' => 'read', 'statuses' => 'read' }
abort 'FAIL: fast-lane must run unconditionally after release-authority' if fast_lane.key?('if')
abort 'FAIL: fast-lane must read the FAST_LANE Actions variable' unless fast_lane.dig('env', 'FAST_LANE') == '${{ vars.FAST_LANE }}'
abort 'FAIL: fast-lane output is not the decision step output' unless fast_lane['outputs'] == { 'skip_remote_gates' => '${{ steps.decide.outputs.skip_remote_gates }}' }
decide = fast_lane.fetch('steps').find { |step| step['id'] == 'decide' }
unless decide && decide['run'].to_s.strip == './scripts/release-fast-lane.sh "${GITHUB_REPOSITORY}" "${RELEASE_TAG}" "${GITHUB_OUTPUT}"'
  abort 'FAIL: fast-lane decision does not use the shared release-fast-lane.sh'
end

# Only harness-smoke may consult the fast-lane decision to skip itself, and no
# step anywhere may: signing, notarization, provenance, and publication steps
# stay unconditional on the switch.
jobs.each do |name, job|
  next if %w[harness-smoke release].include?(name)
  abort "FAIL: jobs.#{name} consults the fast-lane decision" if job['if'].to_s.include?('fast-lane')
end
jobs.each do |name, job|
  (job['steps'] || []).each do |step|
    if step['if'].to_s =~ /fast-lane|skip_remote_gates|FAST_LANE/
      abort "FAIL: jobs.#{name} step '#{step['name']}' is conditional on the fast lane"
    end
  end
end
['Verify immutable release ref', 'Import Apple Developer ID cert to keychain', 'Install cosign', 'Attest build provenance'].each do |step_name|
  abort "FAIL: release job lost step '#{step_name}'" unless jobs.fetch('release').fetch('steps').any? { |step| step['name'] == step_name }
end

# Evaluate a job-level condition with GitHub's semantics for the subset used
# here, for a run that is not cancelled. A condition without a status function
# gets GitHub's implicit success() over the job's needs. Anything outside that
# subset aborts instead of being guessed at.
def evaluate(job, results, outputs)
  expression = job['if'].to_s
  needs = Array(job['needs'])
  text = expression.strip.sub(/\A\$\{\{\s*/, '').sub(/\s*\}\}\z/, '')
  text = 'true' if text.empty?
  unless text.match?(/\b(?:success|failure|always|cancelled)\(\)/)
    text = "#{needs.all? { |need| results.fetch(need) == 'success' }} && (#{text})"
  end
  text = text.gsub('!cancelled()', 'true')
  text = text.gsub(/needs\.([a-z0-9_-]+)\.result/) { results.fetch(Regexp.last_match(1)).inspect }
  text = text.gsub(/needs\.([a-z0-9_-]+)\.outputs\.([a-z0-9_]+)/) do
    outputs.fetch(Regexp.last_match(1), {}).fetch(Regexp.last_match(2), '').inspect
  end
  unless text.match?(/\A(?:true|false|"[a-z]*"|'[a-z]*'|==|!=|&&|\|\||[()]|\s)+\z/)
    abort "FAIL: unsupported job condition: #{expression}"
  end
  eval(text) # only the whitelisted tokens above reach eval
end

# authority, fast-lane result, fast-lane output, smoke outcome when it runs,
# then the expected [smoke runs, release runs].
scenarios = [
  ['success', 'success', 'false', 'success', [true, true]],
  ['success', 'success', 'false', 'failure', [true, false]],
  ['success', 'success', 'false', 'cancelled', [true, false]],
  ['success', 'success', 'true', 'success', [false, true]],
  ['success', 'success', '', 'success', [true, true]],
  ['success', 'failure', '', 'success', [true, true]],
  ['success', 'failure', '', 'failure', [true, false]],
  ['success', 'failure', 'true', 'success', [false, false]],
  ['failure', 'skipped', '', 'success', [false, false]],
  ['cancelled', 'skipped', '', 'success', [false, false]],
]
scenarios.each do |authority, fast_lane_result, skip, smoke_outcome, (want_smoke, want_release)|
  results = { 'release-authority' => authority, 'fast-lane' => fast_lane_result }
  outputs = { 'fast-lane' => { 'skip_remote_gates' => skip } }
  abort "FAIL: fast-lane scheduling differs for #{authority}" unless evaluate(fast_lane, results, {}) == (authority == 'success')
  smoke_runs = evaluate(jobs.fetch('harness-smoke'), results, outputs)
  results['harness-smoke'] = smoke_runs ? smoke_outcome : 'skipped'
  release_runs = evaluate(jobs.fetch('release'), results, outputs)
  label = "authority=#{authority} fast-lane=#{fast_lane_result}/#{skip.inspect} smoke=#{results['harness-smoke']}"
  abort "FAIL: harness-smoke #{smoke_runs ? 'ran' : 'skipped'} for #{label}" unless smoke_runs == want_smoke
  abort "FAIL: release #{release_runs ? 'ran' : 'was blocked'} for #{label}" unless release_runs == want_release
end
RUBY

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
ssh-keygen -q -t ed25519 -N '' -f "${temp_dir}/release-signing-key"
git -C "${fixture_repo}" config gpg.format ssh
git -C "${fixture_repo}" config user.signingkey "${temp_dir}/release-signing-key"
printf 'first\n' > "${fixture_repo}/fixture.txt"
git -C "${fixture_repo}" add fixture.txt
git -C "${fixture_repo}" commit -q -m first
first_commit=$(git -C "${fixture_repo}" rev-parse HEAD)
git -C "${fixture_repo}" tag -s v1.2.3 -m v1.2.3 "${first_commit}"
printf 'second\n' >> "${fixture_repo}/fixture.txt"
git -C "${fixture_repo}" commit -q -am second
second_commit=$(git -C "${fixture_repo}" rev-parse HEAD)
git -C "${fixture_repo}" tag -s v1.2.4 -m v1.2.4 "${second_commit}"
git -C "${fixture_repo}" tag -s v1.2.5-rc.1 -m v1.2.5-rc.1 "${second_commit}"
git -C "${fixture_repo}" -c tag.gpgSign=false tag -a v1.2.6 -m v1.2.6 "${second_commit}"
git -C "${fixture_repo}" -c tag.gpgSign=false tag v1.2.7 "${second_commit}"
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

# Annotated-but-unsigned and lightweight tags are not release identities even
# when they point at the checked-out commit.
for bad_tag in v1.2.6 v1.2.7; do
  git -C "${fixture_repo}" checkout -q --detach "refs/tags/${bad_tag}"
  if (
    cd "${fixture_repo}"
    "${verify_script}" --verify "${bad_tag}" workflow_dispatch "${temp_dir}/${bad_tag}-output" "${temp_dir}/${bad_tag}-env"
  ) 2> "${temp_dir}/${bad_tag}-error"; then
    fail "unsigned release tag was accepted: ${bad_tag}"
  fi
done
grep -Fq 'annotated but unsigned' "${temp_dir}/v1.2.6-error" || fail 'unsigned annotated tag rejection was not explicit'
grep -Fq 'lightweight tag' "${temp_dir}/v1.2.7-error" || fail 'lightweight tag rejection was not explicit'

[[ -x "${authority_script}" ]] || fail 'shared release-authority verifier is missing or not executable'
"${authority_script}" --self-test
[[ -x "${fast_lane_script}" ]] || fail 'release fast-lane decision script is missing or not executable'
"${fast_lane_script}" --self-test
[[ -x "${signing_key_test}" ]] || fail 'release signing-key preflight test is missing or not executable'
"${signing_key_test}"

# Workflow fixtures keep namespace-qualified dispatch checkouts and share one verifier.
for workflow in release.yml e2b-template.yml worker-image.yml; do
  workflow_path="${root_dir}/.github/workflows/${workflow}"
  grep -Fq "format('refs/tags/{0}', inputs.tag)" "${workflow_path}" || fail "${workflow} does not namespace manual tags"
  grep -Fq 'Checkout release automation' "${workflow_path}" || fail "${workflow} does not preserve current automation for older retries"
  grep -Fq 'RUNNER_TEMP}/verify-release-tag.sh" --verify' "${workflow_path}" || fail "${workflow} bypasses the staged shared release verifier"
  grep -Fq 'scripts/verify-release-authority.sh' "${workflow_path}" || fail "${workflow} does not stage the authority verifier from the current workflow"
  grep -Fq "verify-release-authority.sh \"\${GITHUB_REPOSITORY}\" \"\${RELEASE_TAG}\"" "${workflow_path}" || fail "${workflow} does not verify live tag authority"
  for pin in RELEASE_TAG_CREATOR RELEASE_TAGGER_EMAIL RELEASE_TAG_SIGNERS; do
    assert_line "          ${pin}: \${{ vars.${pin} }}" "${workflow_path}"
  done
done
# The fast lane is a release.yml concern only: the image and template
# publishers never consult it.
for workflow in e2b-template.yml worker-image.yml; do
  if grep -Eq 'FAST_LANE|fast-lane|skip_remote_gates|local-verify' "${root_dir}/.github/workflows/${workflow}"; then
    fail "${workflow} consults the fast lane"
  fi
done

assert_workflow_job_needs "${root_dir}/.github/workflows/release.yml" harness-smoke release-authority
assert_workflow_job_needs "${root_dir}/.github/workflows/worker-image.yml" build-and-push release-authority
assert_workflow_job_needs "${root_dir}/.github/workflows/e2b-template.yml" build-template release-authority

ci_workflow="${root_dir}/.github/workflows/ci.yml"
grep -Fq 'release-authority-visibility:' "${ci_workflow}" || fail 'CI does not host the same-token authority visibility control'
grep -Fq "verify-release-authority.sh --check-actor-visibility \"\${GITHUB_REPOSITORY}\"" "${ci_workflow}" || fail 'CI visibility control does not use the shared verifier'
grep -Fq "GH_TOKEN: \${{ github.token }}" "${ci_workflow}" || fail 'CI visibility control does not use the same least-privilege github.token'

grep -Fq "git tag -s \"\$tag\" \"\$release_sha\" -m \"\$tag\"" "${root_dir}/RELEASING.md" || fail 'release docs do not require a signed tag'
grep -Fq -- '--audit-policy RenseiAI/donmai' "${root_dir}/RELEASING.md" || fail 'release docs omit the administrator policy audit'
if grep -Fq "git tag -a \"\$tag\"" "${root_dir}/RELEASING.md"; then
  fail 'release docs still instruct operators to create an unsigned annotated tag'
fi

grep -Fq 'GORELEASER_CURRENT_TAG' "${verify_script}" || fail 'verified tag is not exported to GoReleaser'
grep -Fq 'is_prerelease=' "${verify_script}" || fail 'common verifier does not expose prerelease status'
grep -Fq 'publish_rolling_aliases=' "${verify_script}" || fail 'common verifier does not expose rolling alias policy'
grep -Fq 'publish_homebrew_cask=' "${verify_script}" || fail 'common verifier does not expose Homebrew publication policy'
grep -Fq 'GORELEASER_MAKE_LATEST' "${root_dir}/.goreleaser.yaml" || fail 'GoReleaser latest policy is not wired to the verified release policy'
grep -Fq 'GORELEASER_PUBLISH_HOMEBREW' "${root_dir}/.goreleaser.yaml" || fail 'GoReleaser Homebrew policy is not wired to the verified release policy'
grep -Fq 'skip_upload:' "${root_dir}/.goreleaser.yaml" || fail 'GoReleaser Homebrew publisher cannot be skipped for safe retries'
grep -Fq 'com.apple.quarantine' "${root_dir}/.goreleaser.yaml" || fail 'Homebrew cask install hook does not clear the quarantine flag'
ruby -ryaml - "${root_dir}/.goreleaser.yaml" <<'RUBY'
config = YAML.safe_load(File.read(ARGV.fetch(0)), permitted_classes: [], permitted_symbols: [], aliases: false)
cask = config.fetch('homebrew_casks').fetch(0)
hook = cask.fetch('hooks').fetch('post').fetch('install')
abort 'FAIL: cask post-install hook does not clear the quarantine flag' unless hook.include?('com.apple.quarantine')
abort 'FAIL: cask post-install hook does not target the staged binary' unless hook.include?('staged_path')
RUBY
grep -Fq 'runner.temp }}/goreleaser.yaml' "${root_dir}/.github/workflows/release.yml" || fail 'manual retries can fall back to an older GoReleaser publication policy'
grep -Fq 'binary_signs:' "${root_dir}/.goreleaser.yaml" || fail 'Apple signing is not in the pre-archive binary signing pipe'
grep -Fq 'cmd: donmai-sign-and-notarize' "${root_dir}/.goreleaser.yaml" || fail 'Apple signing does not use the staged current release script'
grep -Fq 'donmai-darwin' "${root_dir}/.goreleaser.yaml" || fail 'Apple signing is not scoped to Darwin binaries'
certificate_count=$(grep -Fc 'certificate: "${artifact}.pem"' "${root_dir}/.goreleaser.yaml")
[[ "${certificate_count}" == 2 ]] || fail 'archive and checksum certificates are not both registered with GoReleaser'
grep -Fq 'install -m 0755 scripts/sign-and-notarize.sh' "${root_dir}/.github/workflows/release.yml" || fail 'manual retries can pair the current config with an older signing script'
grep -Fq 'version: "v2.17.1"' "${root_dir}/.github/workflows/release.yml" || fail 'release GoReleaser version is not pinned to the verified pipeline implementation'
ruby -ryaml - "${root_dir}/.github/workflows/release.yml" "${root_dir}/.github/workflows/release-signing-integration.yml" <<'RUBY'
release_file, integration_file = ARGV
release = YAML.safe_load(File.read(release_file), permitted_classes: [], permitted_symbols: [], aliases: false)
integration = YAML.safe_load(File.read(integration_file), permitted_classes: [], permitted_symbols: [], aliases: false)
abort 'FAIL: signing integration must have read-only contents permission' unless integration['permissions'] == { 'contents' => 'read' }

# Psych's YAML 1.1 parser reads the GitHub Actions `on` key as true.
triggers = integration[true] || integration['on']
required_paths = %w[
  .github/workflows/release-signing-integration.yml
  .github/workflows/release.yml
  .goreleaser.yaml
  scripts/sign-and-notarize.sh
  scripts/test-sign-and-notarize.sh
  scripts/test-release-workflows.sh
  scripts/verify-go-compiler.sh
  scripts/verify-release-binaries.sh
  go.mod
]
%w[pull_request push].each do |event|
  entry = triggers.is_a?(Hash) && triggers[event]
  abort "FAIL: signing integration missing bounded #{event} trigger" unless entry.is_a?(Hash) && entry['branches'] == ['main']
  abort "FAIL: signing integration #{event} paths changed" unless entry['paths'] == required_paths
end
abort 'FAIL: signing integration has an unreviewed trigger' unless triggers.keys.sort == %w[pull_request push]

jobs = integration['jobs']
abort 'FAIL: signing integration must declare one job' unless jobs.is_a?(Hash) && jobs.keys == ['signing-integration']
job = jobs['signing-integration']
abort 'FAIL: signing integration job has no timeout' unless job['timeout-minutes'].is_a?(Integer) && job['timeout-minutes'] <= 30
steps = job['steps']
abort 'FAIL: signing integration job has no steps' unless steps.is_a?(Array)
release_action = release.fetch('jobs').fetch('release').fetch('steps').find { |step| step['uses'].to_s.start_with?('goreleaser/goreleaser-action@') }
integration_action = steps.find { |step| step['uses'].to_s.start_with?('goreleaser/goreleaser-action@') }
abort 'FAIL: signing integration GoReleaser action is absent or differs from release' unless integration_action && release_action && integration_action['uses'] == release_action['uses']
abort 'FAIL: signing integration GoReleaser version differs from release' unless integration_action.dig('with', 'version') == release_action.dig('with', 'version')
abort 'FAIL: signing integration must install GoReleaser without publishing' unless integration_action.dig('with', 'install-only') == true && !integration_action.dig('with', 'args')
run = steps.find { |step| step['name'] == 'Hermetic signing pipeline' }
abort 'FAIL: GoReleaser signing integration is not enrolled in CI' unless run && run['run'].strip == 'bash scripts/test-sign-and-notarize.sh --goreleaser'
abort 'FAIL: signing integration must use the release Go toolchain floor' unless run['env'] == { 'GOTOOLCHAIN' => 'go1.26.11', 'GOWORK' => 'off' }
RUBY
if awk '/^signs:/{in_signs=1} in_signs && /^[[:space:]]+cmd:.*sign-and-notarize/{found=1} END{exit found ? 0 : 1}' "${root_dir}/.goreleaser.yaml"; then
  fail 'archive/checksum signs pipe still invokes the archive-mutating Apple signer'
fi
grep -Fq 'steps.release.outputs.image_tags' "${root_dir}/.github/workflows/worker-image.yml" || fail 'worker image tags do not come from verified release policy'
grep -Fq 'steps.release.outputs.e2b_template_ref' "${root_dir}/.github/workflows/e2b-template.yml" || fail 'E2B build does not use the immutable version target'
grep -Fq 'steps.release.outputs.e2b_additional_tags' "${root_dir}/.github/workflows/e2b-template.yml" || fail 'E2B rolling default policy is not event-scoped'
grep -Fq 'RUNNER_TEMP}/build-e2b-template.cjs' "${root_dir}/.github/workflows/e2b-template.yml" || fail 'older E2B retries can lose the current tagged-build wrapper'

# Pull requests exercise the release image's real multi-platform build without
# inheriting any release credentials or publication surface.
pr_workflow="${root_dir}/.github/workflows/worker-image-pr.yml"
worker_workflow="${root_dir}/.github/workflows/worker-image.yml"
assert_line '  pull_request:' "${pr_workflow}"
assert_line '  contents: read' "${pr_workflow}"
assert_line '          platforms: linux/amd64,linux/arm64' "${pr_workflow}"
assert_line '          push: false' "${pr_workflow}"
assert_line "            DONMAI_VERSION=pr-\${{ github.event.pull_request.number }}-\${{ github.sha }}" "${pr_workflow}"
assert_line '          cache-from: type=gha' "${pr_workflow}"
assert_line '          cache-to: type=gha,mode=max' "${pr_workflow}"

permission_count=$(grep -Ec '^  [a-z-]+: (read|write|none)$' "${pr_workflow}")
[[ "${permission_count}" == 1 ]] || fail 'PR worker workflow permissions are not contents:read only'

if grep -Eq '^  (push|workflow_dispatch|schedule|workflow_call):' "${pr_workflow}"; then
  fail 'PR worker workflow has a non-pull-request trigger'
fi
if grep -Eq 'docker/login-action@|\$\{\{ secrets\.|^[[:space:]]+tags:|^[[:space:]]+outputs:|^[[:space:]]+push: true$|^[[:space:]]+load: true$' "${pr_workflow}"; then
  fail 'PR worker workflow contains credentials or an image export target'
fi
if grep -Eq '^[[:space:]]*uses: [^#[:space:]]+@v[0-9]' "${pr_workflow}"; then
  fail 'PR worker workflow contains a mutable action version tag'
fi
if ! grep -Eq '^[[:space:]]*uses: actions/checkout@[0-9a-f]{40}( |$)' "${pr_workflow}"; then
  fail 'PR worker workflow checkout action is not pinned to a commit SHA'
fi

# Cache isolation: both image builds use the GitHub Actions cache backend
# (cache-from: type=gha, cache-to: type=gha,mode=max), whose writes are
# scoped to the git ref that produced them — a pull_request build's entries
# never feed the tag-triggered release build — so the PR build stays warm
# without ever influencing what a release publishes. Neither build touches
# a persistent builder: no useblacksmith/* action may appear in either file.
if grep -Eq '^[[:space:]]*uses:[[:space:]]*useblacksmith/' "${pr_workflow}"; then
  fail 'PR worker workflow must not use the persistent builder (cache isolation)'
fi
if grep -Eq '^[[:space:]]*uses:[[:space:]]*useblacksmith/' "${worker_workflow}"; then
  fail 'release worker workflow must not use the persistent builder (cache isolation)'
fi
for action in docker/setup-buildx-action docker/build-push-action; do
  release_ref=$(grep -Eo "${action}@[0-9a-f]{40}" "${worker_workflow}")
  [[ -n "${release_ref}" ]] || fail "release worker workflow does not pin ${action}"
done
for action in docker/setup-buildx-action docker/build-push-action; do
  pr_ref=$(grep -Eo "${action}@[0-9a-f]{40}" "${pr_workflow}")
  [[ -n "${pr_ref}" ]] || fail "PR worker workflow does not pin ${action}"
done

# Actions shared by both workflows still pin identical SHAs.
for action in docker/setup-qemu-action; do
  release_ref=$(grep -Eo "${action}@[0-9a-f]{40}" "${worker_workflow}")
  pr_ref=$(grep -Eo "${action}@[0-9a-f]{40}" "${pr_workflow}")
  [[ -n "${release_ref}" ]] || fail "release worker workflow does not pin ${action}"
  [[ "${pr_ref}" == "${release_ref}" ]] || fail "PR worker workflow does not reuse ${release_ref}"
done

# Both image builds share the GitHub Actions cache backend; the release
# build must carry the same cache-from/cache-to lines as the PR build.
for line in '          cache-from: type=gha' '          cache-to: type=gha,mode=max'; do
  assert_line "${line}" "${worker_workflow}"
done

# Runner policy: every workflow job runs on a standard hosted runner —
# ubuntu-latest for Linux (ubuntu-24.04 where a job is pinned while the suite
# is not yet green on the 26.04 image), macos-15 for the signed macOS release — and the
# guard still reports on merge_group so queued PRs cannot stall. Job names
# stay identical so required checks keep reporting. No self-hosted runner
# may appear anywhere.
ruby -ryaml - "${root_dir}" <<'RUBY'
root_dir = ARGV.fetch(0)
allowed = %w[ubuntu-latest ubuntu-24.04 macos-15]
abort 'FAIL: no workflow files found' if Dir.glob(File.join(root_dir, '.github/workflows/*.yml')).empty?
Dir.glob(File.join(root_dir, '.github/workflows/*.yml')).sort.each do |path|
  raw = File.binread(path).force_encoding('UTF-8')
  abort "FAIL: #{File.basename(path)} is not valid UTF-8" unless raw.valid_encoding?
  workflow = YAML.safe_load(raw, permitted_classes: [], permitted_symbols: [], aliases: false)
  jobs = workflow['jobs']
  next unless jobs.is_a?(Hash)
  jobs.each do |name, job|
    runner = job['runs-on']
    runners = runner.is_a?(Array) ? runner : [runner]
    runners.each do |label|
      abort "FAIL: #{File.basename(path)} jobs.#{name} runs on #{label.inspect}" unless allowed.include?(label)
    end
  end
  content = File.binread(path).force_encoding('UTF-8')
  abort "FAIL: #{File.basename(path)} references a persistent builder" if content.match?(/useblacksmith\//)
  abort "FAIL: #{File.basename(path)} uses a self-hosted runner" if content.match?(/self-hosted/)
end
guard = YAML.safe_load(File.read(File.join(root_dir, '.github/workflows/guard-b.yml')), permitted_classes: [], permitted_symbols: [], aliases: false)
triggers = guard[true] || guard['on']
abort 'FAIL: guard-b lost its pull_request trigger' unless triggers.is_a?(Hash) && triggers.key?('pull_request')
abort 'FAIL: guard-b must keep running on every PR and on merge_group' unless triggers.key?('merge_group')
ci = YAML.safe_load(File.read(File.join(root_dir, '.github/workflows/ci.yml')), permitted_classes: [], permitted_symbols: [], aliases: false)
abort 'FAIL: CI lost its test job' unless ci.fetch('jobs').key?('test')
abort 'FAIL: CI lost its release-contracts job' unless ci.fetch('jobs').key?('release-contracts')
release = YAML.safe_load(File.read(File.join(root_dir, '.github/workflows/release.yml')), permitted_classes: [], permitted_symbols: [], aliases: false)
abort 'FAIL: release job lost its macOS runner' unless release.fetch('jobs').fetch('release')['runs-on'] == 'macos-15'
steps = release.fetch('jobs').fetch('release').fetch('steps')
abort 'FAIL: release job lost its signing step' unless steps.any? { |step| step['name'] == 'Import Apple Developer ID cert to keychain' }
scan = YAML.safe_load(File.read(File.join(root_dir, '.github/workflows/security-scan.yml')), permitted_classes: [], permitted_symbols: [], aliases: false)
%w[secret-scan govulncheck semgrep].each do |job|
  abort "FAIL: security scan lost job #{job}" unless scan.fetch('jobs').key?(job)
end
# AC-1: the scheduled scanner blocks and retains the scanned SHA.
scan_raw = File.read(File.join(root_dir, '.github/workflows/security-scan.yml'))
govuln = scan.fetch('jobs').fetch('govulncheck')
abort 'FAIL: govulncheck lost its blocking name' if govuln['name'].to_s.match?(/report-only/i)
abort 'FAIL: govulncheck must keep the 10-minute timeout' unless govuln['timeout-minutes'] == 10
abort 'FAIL: govulncheck must run the compiler guard' unless govuln.fetch('steps').any? { |step| step['run'].to_s.include?('verify-go-compiler.sh') }
run_script = govuln.fetch('steps').select { |step| step['name'] == 'govulncheck' }.map { |step| step['run'].to_s }.join("\n")
abort 'FAIL: govulncheck must fail closed' if run_script.match?(/::warning::govulncheck/)
abort 'FAIL: govulncheck must run with pipefail' unless run_script.include?('pipefail')
abort 'FAIL: govulncheck must run with errexit' unless run_script.include?('set -euo pipefail')
abort 'FAIL: govulncheck must not swallow failures' if run_script.match?(/\|\|/)
abort 'FAIL: govulncheck must not negate the scanner' if run_script.match?(/!\s*govulncheck/)
abort 'FAIL: govulncheck must not disable errexit' if run_script.match?(/set \+e/)
abort 'FAIL: govulncheck must not mask failures' if run_script.match?(/;\s*exit\s+0/)
abort 'FAIL: govulncheck must scan all packages' unless run_script.include?('govulncheck ./...')
abort 'FAIL: govulncheck must not continue-on-error' if govuln['continue-on-error'] == true || govuln.fetch('steps').any? { |step| step['continue-on-error'] == true }
abort 'FAIL: govulncheck must retain the scanned SHA' unless govuln.to_s.match?(/github\.sha|GITHUB_SHA/)
upload = govuln.fetch('steps').find { |step| step['uses'].to_s.start_with?('actions/upload-artifact@') }
abort 'FAIL: govulncheck must upload its report' unless upload && upload.dig('with', 'if-no-files-found') == 'error'
abort 'FAIL: govulncheck must retain its report' unless upload.dig('with', 'retention-days').is_a?(Integer)
# The weekly schedule stays on the head branch.
triggers = scan['on'] || scan[true]
abort 'FAIL: security scan lost its weekly schedule' unless triggers.is_a?(Hash) && Array(triggers['schedule']).any? { |entry| entry['cron'].to_s.match?(/\*/) }
# AC-2: CI and release validate the actual compiler.
ci = YAML.safe_load(File.read(File.join(root_dir, '.github/workflows/ci.yml')), permitted_classes: [], permitted_symbols: [], aliases: false)
contracts = ci.fetch('jobs').fetch('release-contracts')
abort 'FAIL: release-contracts lost its compiler guard' unless contracts.fetch('steps').any? { |step| step['run'].to_s.include?('verify-go-compiler.sh') }
release_steps = release.fetch('jobs').fetch('release').fetch('steps')
guard_index = release_steps.index { |step| step['name'] == 'Verify Go compiler' && step['run'].to_s.include?('verify-go-compiler.sh') }
goreleaser_index = release_steps.index { |step| step['uses'].to_s.start_with?('goreleaser/goreleaser-action@') }
abort 'FAIL: release job lost its compiler guard' unless guard_index
abort 'FAIL: release compiler guard must run before the build' unless guard_index < goreleaser_index
abort 'FAIL: release compiler guard must use the staged script (manual retries)' unless release_steps[guard_index]['run'].to_s.include?('RUNNER_TEMP')
# The build must upload to a non-public draft only: inspection gates
# publicity. Pinned v2.17.1 has no publish-only command (no `continue`, no
# build skip: its dist pipe fails on a non-empty dist without --clean, and
# CleanPipe removes dist), so a second `release --clean` would rebuild and
# discard the inspected bytes. The gated undraft below flips a flag on the
# already-uploaded assets and never touches dist.
build_args = release_steps[goreleaser_index].dig('with', 'args').to_s
abort 'FAIL: release build must create a draft so nothing is public before inspection' unless build_args.include?('--draft')
abort 'FAIL: release build must not skip publish (the draft must carry the inspected assets)' if build_args.include?('--skip=publish')
release_runs = release_steps.select { |step| step.to_s.include?('release --clean') }
abort 'FAIL: release must build exactly once (a second release rebuilds and discards inspection)' unless release_runs == [release_steps[goreleaser_index]]
integration = YAML.safe_load(File.read(File.join(root_dir, '.github/workflows/release-signing-integration.yml')), permitted_classes: [], permitted_symbols: [], aliases: false)
integration_steps = integration.fetch('jobs').fetch('signing-integration').fetch('steps')
abort 'FAIL: signing integration lost its compiler guard' unless integration_steps.any? { |step| step['run'].to_s.include?('verify-go-compiler.sh') }
# AC-3: every binary is inspected with go version -m before upload.
verify_index = release_steps.index { |step| step['name'] == 'Verify release binaries' }
attest_index = release_steps.index { |step| step['name'] == 'Attest build provenance' }
publish_index = release_steps.index { |step| step['name'] == 'Publish release' }
publish_run = release_steps.fetch(publish_index, {})['run'].to_s
abort 'FAIL: publish must not rerun GoReleaser (it rebuilds and discards the inspected dist)' if publish_run.include?('goreleaser')
abort 'FAIL: publish must not clean (it would discard the inspected dist)' if publish_run.include?('--clean')
abort 'FAIL: publish must release the gated draft without rebuilding' unless publish_run.include?('gh release edit') && publish_run.include?('--draft=false')
abort 'FAIL: publish must target the release tag on every path' unless publish_run.include?('RELEASE_TAG')
abort 'FAIL: publish must confirm the release left draft status' unless publish_run.include?('isDraft')
cleanup = release_steps.find { |step| step['name'].to_s.match?(/unverified draft/i) }
abort 'FAIL: release job lost its unverified-draft cleanup' unless cleanup
abort 'FAIL: draft cleanup must run on failure only' unless cleanup['if'].to_s.include?('failure()')
abort 'FAIL: draft cleanup must never delete the signed tag' if cleanup['run'].to_s.include?('--cleanup-tag')
abort 'FAIL: draft cleanup must only delete drafts' unless cleanup['run'].to_s.include?('isDraft')
abort 'FAIL: draft cleanup must delete only drafts (== "true")' unless cleanup['run'].to_s.include?('== "true"')
abort 'FAIL: draft cleanup must not match public releases' if cleanup['run'].to_s.match?(/==\s*"false"/)
# B1: draft isolation — no public write before inspection.
reject_index = release_steps.index { |step| step['name'] == 'Reject already-public release' }
abort 'FAIL: release job lost its already-public guard' unless reject_index
abort 'FAIL: already-public guard must run before the build' unless reject_index < goreleaser_index
reject_run = release_steps[reject_index]['run'].to_s
abort 'FAIL: already-public guard must read the live release' unless reject_run.include?('gh release view') && reject_run.include?('isDraft')
abort 'FAIL: already-public guard must refuse a public release' unless reject_run.include?('already exists and is public') && reject_run.include?('exit 1')
abort 'FAIL: already-public guard must allow a missing release' unless reject_run.include?('not found')
abort 'FAIL: already-public guard must fail closed on unreadable status' unless reject_run.include?('cannot determine release status')
abort 'FAIL: already-public guard must allow only an explicit draft' unless reject_run.include?('== "true"')
abort 'FAIL: already-public guard must fail closed on unexpected values' unless reject_run.include?('unexpected isDraft')
build_step = release_steps[goreleaser_index]
abort 'FAIL: draft build must suppress the tap upload' unless build_step['env'].to_s.include?('GORELEASER_PUBLISH_HOMEBREW') && build_step['env'].to_s.include?('false')
abort 'FAIL: draft build must not skip the cask file generation (the gated tap step needs dist/homebrew)' if build_args.include?('--skip=homebrew')
cask_index = release_steps.index { |step| step['name'] == 'Publish Homebrew cask' }
abort 'FAIL: release job lost its gated cask publish' unless cask_index
abort 'FAIL: cask publish must run after the gated release publish' unless publish_index < cask_index
abort 'FAIL: cask publish must run before provenance upload' unless cask_index < attest_index
cask_step = release_steps[cask_index]
abort 'FAIL: cask publish must run only for automatic stable' unless cask_step['if'].to_s.include?("env.GORELEASER_PUBLISH_HOMEBREW == 'true'")
abort 'FAIL: cask publish must not rerun GoReleaser (it rebuilds)' if cask_step['run'].to_s.include?('goreleaser')
abort 'FAIL: cask publish must reuse the inspected cask file' unless cask_step['run'].to_s.include?('dist/homebrew/Casks/donmai.rb')
abort 'FAIL: cask publish must fail closed without inspected artifacts' unless cask_step['run'].to_s.include?('missing') && cask_step['run'].to_s.include?('exit 1')
abort 'FAIL: cask publish must push the tap update' unless cask_step['run'].to_s.include?('push origin HEAD:main')
# B2: Latest policy rides with the gated publish.
abort 'FAIL: publish must carry the verified Latest policy' unless publish_run.include?('--latest=') && publish_run.include?('GORELEASER_MAKE_LATEST')
# B3: publication scheduling — publish runs only on success, never after failure.
if release_steps[publish_index].key?('if')
  abort 'FAIL: publish must run only on success (no event condition; an always() would publish after failed inspection)'
end
abort 'FAIL: publish must not run on failure' if publish_run.include?('always()') || release_steps[publish_index]['if'].to_s.include?('always()')
abort 'FAIL: release job lost its binary inspection' unless verify_index
abort 'FAIL: release job lost its publish step' unless publish_index
abort 'FAIL: binary inspection must run before provenance upload' unless verify_index < attest_index
abort 'FAIL: binary inspection must run after the build' unless goreleaser_index < verify_index
abort 'FAIL: binary inspection must gate publication' unless verify_index < publish_index
abort 'FAIL: publication must run before provenance upload' unless publish_index < attest_index
verify_run = release_steps[verify_index]['run'].to_s
abort 'FAIL: binary inspection must use the staged config' unless verify_run.include?('RUNNER_TEMP') && verify_run.include?('goreleaser.yaml')
abort 'FAIL: binary inspection must target the release tag on every path' unless verify_run.include?('RELEASE_TAG')
abort 'FAIL: binary inspection must retain the scanned SHA' unless verify_run.include?('GITHUB_SHA')
if release_steps[verify_index].key?('if')
  abort 'FAIL: binary inspection must run on every path (no event condition)'
end
abort 'FAIL: binary inspection does not inspect with go version -m' unless File.read(File.join(root_dir, 'scripts/verify-release-binaries.sh')).include?('go version -m')
# The expected artifact set comes from the GoReleaser config, never a copy.
builds = YAML.safe_load(File.read(File.join(root_dir, '.goreleaser.yaml')), permitted_classes: [], permitted_symbols: [], aliases: false).fetch('builds')
abort 'FAIL: GoReleaser config declares no builds' unless builds.is_a?(Array) && !builds.empty?
combos = builds.flat_map { |build| Array(build['goos']).product(Array(build['goarch'])) }
abort 'FAIL: GoReleaser config declares no platform matrix' if combos.empty?
inspector = File.read(File.join(root_dir, 'scripts/verify-release-binaries.sh'))
abort 'FAIL: binary inspector does not derive from the GoReleaser config' unless inspector.include?("goreleaser.yaml") && inspector.include?("builds")
abort 'FAIL: binary inspector must discover via build id' unless inspector.include?("build['id']") || inspector.include?('build["id"]')
abort 'FAIL: binary inspector must normalize the v tag' unless inspector.include?("#v") || inspector.include?("TrimPrefix") || inspector.include?("${tag#v}")
abort 'FAIL: binary inspector must fail closed on producer errors' if inspector.match?(/done\s+<\s+<\(expected_binaries\)/)
staged = release_steps.find { |step| step['name'] == 'Stage release automation' }['run'].to_s
%w[verify-go-compiler.sh verify-release-binaries.sh].each do |script|
  abort "FAIL: manual retries can fall back to an older #{script}" unless staged.include?(script)
end
RUBY

# AC-2: the compiler guard pins its contract directly: floor and newer stable
# pass; old, missing, malformed, and prerelease compilers fail.
compiler_script="${root_dir}/scripts/verify-go-compiler.sh"
[[ -x "${compiler_script}" ]] || fail 'Go compiler guard is missing or not executable'
"${compiler_script}" --self-test
floor=$(grep -E '^go [0-9]+\.[0-9]+\.[0-9]+$' "${root_dir}/go.mod" | head -1 | cut -d' ' -f2)
[[ -n "${floor}" ]] || fail 'cannot read Go floor from go.mod'
"${compiler_script}" --floor "${floor}" --version-output "go version go${floor} darwin/arm64" || fail 'floor compiler was rejected'
"${compiler_script}" --floor "${floor}" --version-output 'go version go1.27.3 darwin/arm64' || fail 'newer stable compiler was rejected'
if "${compiler_script}" --floor "${floor}" --version-output 'go version go1.0.0 darwin/arm64' >/dev/null 2>&1; then
  fail 'old compiler was accepted'
fi
if "${compiler_script}" --floor "${floor}" --version-output '' >/dev/null 2>&1; then
  fail 'missing compiler output was accepted'
fi
if "${compiler_script}" --floor "${floor}" --version-output 'go version go1.26 darwin/arm64' >/dev/null 2>&1; then
  fail 'malformed compiler output was accepted'
fi
if "${compiler_script}" --floor "${floor}" --version-output 'go version go1.26.10rc1 darwin/arm64' >/dev/null 2>&1; then
  fail 'prerelease compiler was accepted'
fi
if PATH="${temp_dir}" "${compiler_script}" >/dev/null 2>&1; then
  fail 'missing go toolchain was accepted'
fi

# AC-3: real binary inspection refuses missing or stale metadata.
binaries_script="${root_dir}/scripts/verify-release-binaries.sh"
[[ -x "${binaries_script}" ]] || fail 'release binary inspector is missing or not executable'
"${binaries_script}" --self-test

# A dist of good binaries with no archives is not shippable. Probe the
# empty-archive refusal directly so deleting the guard fails this contract
# even if the self-test above is ever trimmed.
no_archives_dir="${temp_dir}/no-archives"
mkdir -p "${no_archives_dir}/dist/onlybin_linux_amd64_v1" "${no_archives_dir}/src"
cat >"${no_archives_dir}/src/main.go" <<'GO'
package main
func main() {}
GO
cat >"${no_archives_dir}/src/go.mod" <<'GO'
module example.com/noarchives

go 1.26.10
GO
(cd "${no_archives_dir}/src" && GOWORK=off GOTOOLCHAIN=local go build -ldflags="-X main.version=1.2.3" -o "${no_archives_dir}/dist/onlybin_linux_amd64_v1/donmai" .)
cat >"${no_archives_dir}/goreleaser.yaml" <<'YAML'
version: 2
builds:
  - id: onlybin
    main: ./src
    binary: donmai
    goos: [linux]
    goarch: [amd64]
YAML
if (cd "${no_archives_dir}" && bash "${binaries_script}" --config goreleaser.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then
  fail 'binary inspector accepted a dist with no archives'
fi

# B4: digest failures and malformed digests are refused, in both hash
# branches. Probe the inspector directly with failing/malformed hashers so
# deleting the digest validation fails this contract even if the self-test
# cases above are ever trimmed.
digest_probe_dir="${temp_dir}/digest-probe"
mkdir -p "${digest_probe_dir}/dist/digestbin_linux_amd64_v1" "${digest_probe_dir}/src"
cat >"${digest_probe_dir}/src/main.go" <<'GO'
package main
func main() {}
GO
cat >"${digest_probe_dir}/src/go.mod" <<'GO'
module example.com/digestprobe

go 1.26.10
GO
(cd "${digest_probe_dir}/src" && GOWORK=off GOTOOLCHAIN=local go build -ldflags="-X main.version=1.2.3" -o "${digest_probe_dir}/dist/digestbin_linux_amd64_v1/donmai" .)
printf 'payload' >"${digest_probe_dir}/dist/donmai_1.2.3_linux_amd64.tar.gz"
cat >"${digest_probe_dir}/goreleaser.yaml" <<'YAML'
version: 2
builds:
  - id: digestbin
    main: ./src
    binary: donmai
    goos: [linux]
    goarch: [amd64]
YAML
mkdir -p "${digest_probe_dir}/fake-fail"
printf '#!/bin/sh\nexit 1\n' >"${digest_probe_dir}/fake-fail/shasum"
printf '#!/bin/sh\nexit 1\n' >"${digest_probe_dir}/fake-fail/sha256sum"
chmod +x "${digest_probe_dir}/fake-fail/shasum" "${digest_probe_dir}/fake-fail/sha256sum"
if (cd "${digest_probe_dir}" && PATH="${digest_probe_dir}/fake-fail:${PATH}" bash "${binaries_script}" --config goreleaser.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then
  fail 'binary inspector accepted a failing digest'
fi
mkdir -p "${digest_probe_dir}/fake-malformed"
printf '#!/bin/sh\necho "not-a-digest  x"\n' >"${digest_probe_dir}/fake-malformed/shasum"
printf '#!/bin/sh\necho "not-a-digest  x"\n' >"${digest_probe_dir}/fake-malformed/sha256sum"
chmod +x "${digest_probe_dir}/fake-malformed/shasum" "${digest_probe_dir}/fake-malformed/sha256sum"
if (cd "${digest_probe_dir}" && PATH="${digest_probe_dir}/fake-malformed:${PATH}" bash "${binaries_script}" --config goreleaser.yaml --dist dist --tag v1.2.3 --floor "${floor}" >/dev/null 2>&1); then
  fail 'binary inspector accepted a malformed digest'
fi

# B1/B2/B3: execute the gated workflow shells against mocked gh/git.
# Every shell below is extracted from the production workflow file, not a
# copy, so editing the workflow without updating the guard fails here.
workflow_extract_dir="${temp_dir}/workflow-extract"
mkdir -p "${workflow_extract_dir}"
ruby -ryaml - "${root_dir}/.github/workflows/release.yml" "${workflow_extract_dir}" <<'RUBY'
release_file, out_dir = ARGV
workflow = YAML.safe_load(File.read(release_file), permitted_classes: [], permitted_symbols: [], aliases: false)
steps = workflow.fetch('jobs').fetch('release').fetch('steps')
{
  'Reject already-public release' => 'reject.sh',
  'Publish release' => 'publish.sh',
  'Remove unverified draft' => 'cleanup.sh',
  'Publish Homebrew cask' => 'cask.sh',
}.each do |name, file|
  step = steps.find { |s| s['name'] == name }
  abort "FAIL: workflow lost step #{name}" unless step && step['run']
  File.write(File.join(out_dir, file), step['run'])
end
RUBY
# B1: the already-public guard refuses a public release, allows a draft or a
# missing release, and fails closed on an unreadable status.
for mock_state in true false missing error; do
  mock_bin="${temp_dir}/mock-gh-${mock_state}"
  mkdir -p "${mock_bin}"
  cat >"${mock_bin}/gh" <<MOCK
#!/bin/sh
if [ "\$1" = "release" ] && [ "\$2" = "view" ]; then
  case "${mock_state}" in
    true) echo "true"; exit 0 ;;
    false) echo "false"; exit 0 ;;
    missing) echo "release not found" >&2; exit 1 ;;
    error) echo "API rate limit" >&2; exit 1 ;;
  esac
fi
echo "unexpected gh call: \$*" >&2
exit 1
MOCK
  chmod +x "${mock_bin}/gh"
done
if ! PATH="${temp_dir}/mock-gh-true:${PATH}" RELEASE_TAG=v1.2.3 bash "${workflow_extract_dir}/reject.sh" >/dev/null 2>&1; then
  fail 'already-public guard rejected a draft release'
fi
if PATH="${temp_dir}/mock-gh-false:${PATH}" RELEASE_TAG=v1.2.3 bash "${workflow_extract_dir}/reject.sh" >/dev/null 2>&1; then
  fail 'already-public guard accepted a public release'
fi
if ! PATH="${temp_dir}/mock-gh-missing:${PATH}" RELEASE_TAG=v1.2.3 bash "${workflow_extract_dir}/reject.sh" >/dev/null 2>&1; then
  fail 'already-public guard rejected a missing release'
fi
if PATH="${temp_dir}/mock-gh-error:${PATH}" RELEASE_TAG=v1.2.3 bash "${workflow_extract_dir}/reject.sh" >/dev/null 2>&1; then
  fail 'already-public guard accepted an unreadable release status'
fi
# B1 fail-closed: empty, null, and garbage success responses must refuse,
# and a public response with a stderr diagnostic must still refuse.
for malformed in empty null garbage; do
  mock_malformed_bin="${temp_dir}/mock-gh-${malformed}"
  mkdir -p "${mock_malformed_bin}"
  case "${malformed}" in
    empty) malformed_output="" ;;
    null) malformed_output="null" ;;
    garbage) malformed_output="maybe" ;;
  esac
  cat >"${mock_malformed_bin}/gh" <<MOCK
#!/bin/sh
if [ "\$1" = "release" ] && [ "\$2" = "view" ]; then
  echo "${malformed_output}"
  exit 0
fi
echo "unexpected gh call: \$*" >&2
exit 1
MOCK
  chmod +x "${mock_malformed_bin}/gh"
  if PATH="${mock_malformed_bin}:${PATH}" RELEASE_TAG=v1.2.3 bash "${workflow_extract_dir}/reject.sh" >/dev/null 2>&1; then
    fail "already-public guard accepted a ${malformed} release status"
  fi
done
mock_warning_bin="${temp_dir}/mock-gh-public-warning"
mkdir -p "${mock_warning_bin}"
cat >"${mock_warning_bin}/gh" <<'MOCK'
#!/bin/sh
if [ "$1" = "release" ] && [ "$2" = "view" ]; then
  echo "false"
  echo "warning: rate limited" >&2
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
MOCK
chmod +x "${mock_warning_bin}/gh"
if PATH="${mock_warning_bin}:${PATH}" RELEASE_TAG=v1.2.3 bash "${workflow_extract_dir}/reject.sh" >/dev/null 2>&1; then
  fail 'already-public guard accepted a public release with a stderr diagnostic'
fi
# B2: the gated publish carries the verified Latest policy on both paths.
for latest_value in true false; do
  mock_publish_bin="${temp_dir}/mock-publish-${latest_value}"
  mkdir -p "${mock_publish_bin}"
  publish_log="${temp_dir}/publish-args-${latest_value}.log"
  rm -f "${publish_log}"
  cat >"${mock_publish_bin}/gh" <<MOCK
#!/bin/sh
if [ "\$1" = "release" ] && [ "\$2" = "edit" ]; then
  printf '%s\n' "\$*" >>"${publish_log}"
  exit 0
fi
if [ "\$1" = "release" ] && [ "\$2" = "view" ]; then
  echo "false"
  exit 0
fi
echo "unexpected gh call: \$*" >&2
exit 1
MOCK
  chmod +x "${mock_publish_bin}/gh"
  if ! PATH="${mock_publish_bin}:${PATH}" RELEASE_TAG=v1.2.3 GORELEASER_MAKE_LATEST="${latest_value}" bash "${workflow_extract_dir}/publish.sh" >/dev/null 2>&1; then
    fail "gated publish failed for Latest=${latest_value}"
  fi
  grep -Fq -- "--draft=false" "${publish_log}" || fail "gated publish omits --draft=false for Latest=${latest_value}"
  grep -Fq -- "--latest=${latest_value}" "${publish_log}" || fail "gated publish omits --latest=${latest_value}"
done
# B3: the unverified-draft cleanup deletes only drafts, never a public
# release or an error response. Inverting the comparison would delete the
# public release here and fail this contract.
for cleanup_state in true false error; do
  mock_cleanup_bin="${temp_dir}/mock-cleanup-${cleanup_state}"
  mkdir -p "${mock_cleanup_bin}"
  cleanup_log="${temp_dir}/cleanup-delete-${cleanup_state}.log"
  rm -f "${cleanup_log}"
  cat >"${mock_cleanup_bin}/gh" <<MOCK
#!/bin/sh
if [ "\$1" = "release" ] && [ "\$2" = "view" ]; then
  case "${cleanup_state}" in
    true) echo "true"; exit 0 ;;
    false) echo "false"; exit 0 ;;
    error) echo "API error" >&2; exit 1 ;;
  esac
fi
if [ "\$1" = "release" ] && [ "\$2" = "delete" ]; then
  printf '%s\n' "\$*" >>"${cleanup_log}"
  exit 0
fi
echo "unexpected gh call: \$*" >&2
exit 1
MOCK
  chmod +x "${mock_cleanup_bin}/gh"
  PATH="${mock_cleanup_bin}:${PATH}" RELEASE_TAG=v1.2.3 bash "${workflow_extract_dir}/cleanup.sh" >/dev/null 2>&1 || true
  if [ "${cleanup_state}" = "true" ]; then
    grep -Fq -- "release delete" "${cleanup_log}" || fail 'cleanup did not delete an unverified draft'
  else
    if [ -f "${cleanup_log}" ]; then
      fail "cleanup deleted on ${cleanup_state} response"
    fi
  fi
done
# B1: the gated cask step refuses without inspected artifacts instead of
# pushing an empty tap update.
cask_empty_dir="${temp_dir}/cask-empty"
mkdir -p "${cask_empty_dir}"
cp "${workflow_extract_dir}/cask.sh" "${cask_empty_dir}/cask.sh"
mkdir -p "${temp_dir}/mock-git-empty"
cat >"${temp_dir}/mock-git-empty/git" <<'MOCK'
#!/bin/sh
echo "unexpected git call: $*" >&2
exit 1
MOCK
chmod +x "${temp_dir}/mock-git-empty/git"
if (cd "${cask_empty_dir}" && PATH="${temp_dir}/mock-git-empty:${PATH}" RELEASE_TAG=v1.2.3 HOMEBREW_TAP_GITHUB_TOKEN=dummy bash ./cask.sh >/dev/null 2>&1); then
  fail 'cask publish accepted a missing cask file'
fi
# B2: the gated cask step publishes the inspected cask file when it changes.
# Extracted from the production workflow, so deleting the push fails here:
# the mutated shell still exits 0, but no push is recorded.
cask_changed_dir="${temp_dir}/cask-changed"
mkdir -p "${cask_changed_dir}/dist/homebrew/Casks"
printf 'cask "donmai" do\n  version "1.2.3"\nend\n' >"${cask_changed_dir}/dist/homebrew/Casks/donmai.rb"
cp "${workflow_extract_dir}/cask.sh" "${cask_changed_dir}/cask.sh"
mkdir -p "${temp_dir}/mock-git-changed"
cask_push_log="${temp_dir}/cask-push.log"
rm -f "${cask_push_log}"
cat >"${temp_dir}/mock-git-changed/git" <<MOCK
#!/bin/sh
log="${cask_push_log}"
printf '%s\n' "git \$*" >>"\${log}"
if [ "\$1" = "clone" ]; then
  dest=""
  for arg in "\$@"; do dest="\$arg"; done
  mkdir -p "\${dest}/Casks"
  printf 'cask "donmai" do\n  version "old"\nend\n' >"\${dest}/Casks/donmai.rb"
  exit 0
fi
if [ "\$1" = "-C" ]; then
  shift; dir="\$1"; shift
  sub="\$1"; shift
  case "\${sub}" in
    config) exit 0 ;;
    add) exit 0 ;;
    diff) exit 1 ;;
    commit) printf '%s\n' "git commit \$*" >>"\${log}"; exit 0 ;;
    push) printf '%s\n' "git push \$*" >>"\${log}"; exit 0 ;;
    *) exit 0 ;;
  esac
fi
exit 0
MOCK
chmod +x "${temp_dir}/mock-git-changed/git"
if ! (cd "${cask_changed_dir}" && PATH="${temp_dir}/mock-git-changed:${PATH}" RELEASE_TAG=v1.2.3 HOMEBREW_TAP_GITHUB_TOKEN=dummy bash ./cask.sh >/dev/null 2>&1); then
  fail 'cask publish failed on a changed cask file'
fi
grep -Fq -- "git push origin HEAD:main" "${cask_push_log}" || fail 'cask publish did not push the tap update'

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

bash "${root_dir}/scripts/test-sign-and-notarize.sh"
bash "${root_dir}/scripts/test-fast-lane.sh"
bash "${root_dir}/scripts/test-podman-go-test.sh"

printf 'release workflow tests: PASS\n'
