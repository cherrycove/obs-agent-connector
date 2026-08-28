#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${ROOT_DIR}/dist"
APP_NAME="obs-agent-connector"
VERSION="${VERSION:-}"

rm -rf "${DIST_DIR}"
mkdir -p "${DIST_DIR}"

if [[ -z "${VERSION}" ]]; then
  if git -C "${ROOT_DIR}" describe --tags --exact-match >/dev/null 2>&1; then
    VERSION="$(git -C "${ROOT_DIR}" describe --tags --exact-match)"
  else
    VERSION="dev"
  fi
fi

build() {
  local goos="$1"
  local goarch="$2"
  local suffix=""
  if [[ "${goos}" == "windows" ]]; then
    suffix=".exe"
  fi

  local output="${DIST_DIR}/${APP_NAME}-${goos}-${goarch}${suffix}"
  echo "Building ${goos}/${goarch}"
  GOOS="${goos}" GOARCH="${goarch}" CGO_ENABLED=0 \
    go build -trimpath \
      -ldflags="-s -w -X github.com/GuanceCloud/obs-agent-connector/internal/app.version=${VERSION} -X github.com/GuanceCloud/obs-agent-connector/internal/adapters/claude/buildinfo.Version=${VERSION} -X github.com/GuanceCloud/obs-agent-connector/internal/adapters/codebuddy/buildinfo.Version=${VERSION} -X github.com/GuanceCloud/obs-agent-connector/internal/adapters/codex/buildinfo.Version=${VERSION} -X github.com/GuanceCloud/obs-agent-connector/internal/adapters/cursor/buildinfo.Version=${VERSION} -X github.com/GuanceCloud/obs-agent-connector/internal/adapters/dcode/buildinfo.Version=${VERSION} -X github.com/GuanceCloud/obs-agent-connector/internal/adapters/grok/buildinfo.Version=${VERSION} -X github.com/GuanceCloud/obs-agent-connector/internal/adapters/kiro/buildinfo.Version=${VERSION}" \
      -o "${output}" "${ROOT_DIR}/cmd/obs-agent-connector"
}

package_tar() {
  local name="$1"
  tar -C "${DIST_DIR}" -czf "${DIST_DIR}/${name}.tar.gz" "${name}"
}

package_zip() {
  local name="$1"
  (cd "${DIST_DIR}" && zip -q "${name}.zip" "${name}.exe")
}

write_checksums() {
  local files=("$@")
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${files[@]}"
    return
  fi

  shasum -a 256 "${files[@]}"
}

build darwin arm64
build darwin amd64
build linux amd64
build linux arm64
build windows amd64
build windows arm64

install -m 0755 "${ROOT_DIR}/scripts/install.sh" "${DIST_DIR}/install.sh"
install -m 0644 "${ROOT_DIR}/scripts/install.ps1" "${DIST_DIR}/install.ps1"

package_tar "${APP_NAME}-darwin-arm64"
package_tar "${APP_NAME}-darwin-amd64"
package_tar "${APP_NAME}-linux-amd64"
package_tar "${APP_NAME}-linux-arm64"
package_zip "${APP_NAME}-windows-amd64"
package_zip "${APP_NAME}-windows-arm64"

printf '%s\n' "${VERSION}" > "${DIST_DIR}/latest.txt"

(
  cd "${DIST_DIR}"
  write_checksums \
    "install.sh" \
    "install.ps1" \
    "latest.txt" \
    "${APP_NAME}"-darwin-*.tar.gz \
    "${APP_NAME}"-linux-*.tar.gz \
    "${APP_NAME}"-windows-*.zip \
    > SHA256SUMS
)

echo "Release artifacts written to ${DIST_DIR}"
