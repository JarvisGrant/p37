#!/usr/bin/env bash
# P36 / Ubuntu 验收脚本。
#
# 设计为在 GitHub Actions 的 Ubuntu runner（有 systemd、sudo）上运行；
# 也可以在有 sudo 的 Ubuntu VM 上手工跑。脚本自己调用 sudo，不要求整段以
# root 执行，这样可以复用 runner 的 Go/PATH。
#
# 输入：
#   P37_VERSION        版本号（例如 0.1.0）；缺省从 tar 文件名推断
#   P37_ARTIFACT_DIR   含 p37-linux-amd64-<ver>.tar.gz 与其 .sha256 的目录
#   P37_BOOTSTRAP_DIR  含 p37-run/p37-mcp/p37-mount/config.toml/p37d.service/90-p37.conf
#   P37_TAG            可选，用于校验 manifest.commit 与 tag commit 一致
#   P37_ROOT           多仓库根目录，默认 /srv/repos
#   P37_REPO           旧单仓库目录，默认 /srv/repo（会注册为 default）
#   P37_REPO_NAME      验收用 repo 名，默认 accept
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"

LOG="$ROOT/acceptance-output.log"
: > "$LOG"
exec > >(tee -a "$LOG") 2>&1

say() { echo "==> $*"; }

LEGACY="${P37_REPO:-/srv/repo}"
ROOTDIR="${P37_ROOT:-/srv/repos}"
REPO_NAME="${P37_REPO_NAME:-accept}"
REPO_DIR=""
P37_TMP="$ROOTDIR/.p37/tmp"
ARTIFACT_DIR="${P37_ARTIFACT_DIR:-$ROOT/dist}"
BOOTSTRAP_DIR="${P37_BOOTSTRAP_DIR:-$ROOT/dist/p36-bootstrap}"
VERSION="${P37_VERSION:-}"
TAG="${P37_TAG:-}"

if [ -z "$VERSION" ]; then
  TARBALL="$(ls "$ARTIFACT_DIR"/p37-linux-amd64-*.tar.gz | head -n 1)"
  VERSION="$(basename "$TARBALL" | sed -e 's/^p37-linux-amd64-//' -e 's/\.tar\.gz$//')"
fi
TARBALL="$ARTIFACT_DIR/p37-linux-amd64-$VERSION.tar.gz"
SHA256="$TARBALL.sha256"
[ -f "$TARBALL" ] || { echo "缺少 $TARBALL"; exit 1; }
[ -f "$SHA256" ] || { echo "缺少 $SHA256"; exit 1; }
[ -d "$BOOTSTRAP_DIR" ] || { echo "缺少 $BOOTSTRAP_DIR"; exit 1; }

say "release $VERSION"
sha256sum --version >/dev/null 2>&1 || true
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$ARTIFACT_DIR" && sha256sum -c "$(basename "$SHA256")")
else
  (cd "$ARTIFACT_DIR" && shasum -a 256 -c "$(basename "$SHA256")")
fi

WORK="$(mktemp -d /tmp/p37-acceptance.XXXXXX)"
SEED="$WORK/seed"
PAYLOAD="$WORK/payload.ext4"
LOOP=""
ROOT_LOOP=""
ROOT_IMG=""
ROOT_FALLBACK=0
P37D_PID=""
cleanup() {
  set +e
  if [ -n "${P37D_PID:-}" ]; then sudo kill -TERM "$P37D_PID" >/dev/null 2>&1; fi
  sudo systemctl stop p37d.service >/dev/null 2>&1
  sudo rm -f /etc/systemd/system/p37d.service.d/98-no-nnp.conf
  sudo systemctl daemon-reload >/dev/null 2>&1
  sudo umount -l /run/p37-payload >/dev/null 2>&1
  if [ -n "$LOOP" ]; then sudo losetup -d "$LOOP" >/dev/null 2>&1; fi
  if [ -n "$ROOT_LOOP" ]; then
    sudo umount -l "$ROOTDIR" >/dev/null 2>&1
    sudo losetup -d "$ROOT_LOOP" >/dev/null 2>&1
  fi
  sudo rm -f /dev/disk/by-label/P37PAYLOAD
  rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$SEED"
tar -xzf "$TARBALL" -C "$SEED"

say "tar 内容"
find "$SEED" -maxdepth 1 -type f -printf '%f\n' | sort | tee tar-files.txt
[ "$(wc -l < tar-files.txt | tr -d ' ')" = "3" ]
for f in p37 p37d manifest.json; do grep -qx "$f" tar-files.txt; done

say "二进制版本"
"$SEED/p37" --version
"$SEED/p37d" --version
"$SEED/p37d" --help > p37d-help.txt 2>&1 || true
"$SEED/p37" init --help > p37-help.txt 2>&1 || true
grep -q -- '--stdio' p37d-help.txt
grep -q -- '--check' p37d-help.txt
grep -q -- '--if-missing' p37-help.txt
grep -q -- '--config' p37-help.txt

say "manifest"
cat "$SEED/manifest.json"
got_version="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$SEED/manifest.json")"
got_commit="$(sed -n 's/.*"commit"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$SEED/manifest.json")"
[ "$got_version" = "$VERSION" ]
[ "$got_commit" = "$(git rev-parse HEAD)" ]
if [ -n "$TAG" ]; then
  [ "$got_commit" = "$(git rev-parse "$TAG^{commit}")" ]
fi
"$SEED/p37d" --version | grep -q "version=$VERSION"

say "系统依赖"
sudo apt-get update -qq
sudo apt-get install -y -qq e2fsprogs util-linux jq

say "用户与多仓库目录"
if ! id p36 >/dev/null 2>&1; then
  sudo useradd -m -s /bin/bash p36
fi
sudo usermod -aG disk p36
sudo mkdir -p "$(dirname "$ROOTDIR")"
ROOT_FS="$(stat -f -c %T "$(dirname "$ROOTDIR")")"
if [ "$ROOT_FS" != "ext4" ]; then
  echo "$ROOTDIR 所在文件系统是 $ROOT_FS，用 loop ext4 模拟 L2"
  ROOT_IMG="$WORK/repos.ext4"
  truncate -s 2G "$ROOT_IMG"
  sudo mkfs.ext4 -q -F -m 0 "$ROOT_IMG"
  ROOT_LOOP="$(sudo losetup --find --show "$ROOT_IMG")"
  sudo mkdir -p "$ROOTDIR"
  sudo mount "$ROOT_LOOP" "$ROOTDIR"
fi
sudo install -d -o p36 -g p36 -m 0750 "$ROOTDIR"
sudo install -d -o p36 -g p36 -m 0750 "$LEGACY"
sudo install -d -o p36 -g p36 -m 0755 /run/p37-payload

say "安装 L1 胶水"
sudo install -m 0755 "$BOOTSTRAP_DIR/p37" /usr/local/bin/p37
sudo install -m 0755 "$BOOTSTRAP_DIR/p37-run" /usr/local/bin/p37-run
sudo install -m 0755 "$BOOTSTRAP_DIR/p37-mcp" /usr/local/bin/p37-mcp
sudo install -m 0755 "$BOOTSTRAP_DIR/p37-mount" /usr/local/bin/p37-mount
sudo install -D -m 0644 "$BOOTSTRAP_DIR/config.toml" /usr/local/share/p37/config.toml
sudo install -D -m 0644 "$BOOTSTRAP_DIR/p37d.service" /etc/systemd/system/p37d.service
sudo install -D -m 0644 "$BOOTSTRAP_DIR/90-p37.conf" /etc/sysctl.d/90-p37.conf
sudo install -D -m 0644 "$BOOTSTRAP_DIR/tmpfiles-p37.conf" /etc/tmpfiles.d/p37.conf
sudo sysctl --system >/dev/null
# runner 可能已有更高优先级的 sysctl.d 覆盖 90-p37.conf；验收时显式确保额度。
sudo sysctl -w fs.inotify.max_user_watches=1048576 fs.inotify.max_user_instances=4096 >/dev/null
sudo systemd-tmpfiles --create /etc/tmpfiles.d/p37.conf

say "生成 96 MiB payload"
truncate -s 96M "$PAYLOAD"
sudo mkfs.ext4 -q -F -L P37PAYLOAD -m 0 -d "$SEED" "$PAYLOAD"
# mkfs.ext4 -d 可能创建 lost+found；payload 根目录要求严格三文件。
sudo debugfs -w -R "rmdir /lost+found" "$PAYLOAD" >/dev/null 2>&1 || true
sudo chmod 0444 "$PAYLOAD"
LOOP="$(sudo losetup --find --show "$PAYLOAD")"
sudo mkdir -p /dev/disk/by-label
sudo ln -sfn "$LOOP" /dev/disk/by-label/P37PAYLOAD
ls -l "$LOOP" /dev/disk/by-label/P37PAYLOAD

# 显式检查需要 CAP_SYS_ADMIN 才能 probe overlay；用 setpriv 模拟 unit 的
# p36 + ambient CAP_SYS_ADMIN（NNP 由 systemd 服务本身覆盖）。
as_p36_caps() {
  sudo setpriv --reuid=p36 --regid=p36 --init-groups \
    --inh-caps=+sys_admin --ambient-caps=+sys_admin \
    env TMPDIR="$P37_TMP" "$@"
}

ROOT_FALLBACK=0

# GitHub-hosted runner 可能禁止非 root 进程挂载 overlay（即使有 ambient
# CAP_SYS_ADMIN）。此时为了继续跑功能验收，退化为 root 直接进程并在日志里
# 明确标出 ROOT_FALLBACK=1；真实 P36 仍应用 User=p36。
as_caps() {
  if [ "$ROOT_FALLBACK" = 1 ]; then
    sudo env TMPDIR="$P37_TMP" "$@"
  else
    as_p36_caps "$@"
  fi
}

as_app() {
  if [ "$ROOT_FALLBACK" = 1 ]; then
    sudo env HOME=/root TMPDIR="$P37_TMP" "$@"
  else
    sudo -u p36 env HOME=/home/p36 TMPDIR="$P37_TMP" "$@"
  fi
}

say "初始化 legacy /srv/repo（迁移验收）"
sudo install -d -o p36 -g p36 -m 0700 "$ROOTDIR/.p37"
sudo install -d -o p36 -g p36 -m 0700 "$P37_TMP"
as_app /usr/local/bin/p37 init "$LEGACY" --if-missing --config /usr/local/share/p37/config.toml

say "创建多仓库验收 repo"
as_app /usr/local/bin/p37 --root "$ROOTDIR" repo create "$REPO_NAME" --json | tee repo-create.json
REPO_DIR="$(as_app /usr/local/bin/p37 --root "$ROOTDIR" repo info "$REPO_NAME" --json | jq -r .path)"
[ -n "$REPO_DIR" ] && [ -d "$REPO_DIR/.p37" ] || { echo "repo create 后 path 无效: $REPO_DIR"; exit 1; }
echo "REPO_DIR=$REPO_DIR"

say "启动 systemd 服务"
[ -d /run/systemd/system ] || {
  echo "runner 没有 systemd；P36 验收要求真实 unit，请换 VM runner" >&2
  exit 1
}
sudo systemctl daemon-reload
sudo systemctl enable p37d.service

wait_socket() {
  for _ in $(seq 1 100); do
    sudo test -S "$ROOTDIR/.p37/p37d.sock" && return 0
    sleep 0.2
  done
  return 1
}

P37D_PID=""
start_root_daemon() {
  sudo env P37_ROOT="$ROOTDIR" P37_REPO="$LEGACY" \
    P37_PAYLOAD_LABEL=P37PAYLOAD \
    P37_PAYLOAD_MOUNT=/run/p37-payload \
    P37_CONFIG=/usr/local/share/p37/config.toml \
    TMPDIR="$P37_TMP" \
    sh -c 'echo $$ > /run/p37d-root.pid; exec /usr/local/bin/p37-run' \
    > p37d-root.log 2>&1 &
  for _ in $(seq 1 100); do
    if sudo test -S "$ROOTDIR/.p37/p37d.sock"; then
      P37D_PID="$(sudo cat /run/p37d-root.pid 2>/dev/null || true)"
      return 0
    fi
    sleep 0.2
  done
  return 1
}

stop_root_daemon() {
  if [ -n "$P37D_PID" ]; then
    sudo kill -TERM "$P37D_PID" 2>/dev/null || true
  fi
  for _ in $(seq 1 100); do
    sudo test ! -S "$ROOTDIR/.p37/p37d.sock" && break
    sleep 0.2
  done
  # socket 先于进程退出删除；必须等进程真正退出，否则新实例会撞上 flock。
  for _ in $(seq 1 100); do
    sudo kill -0 "$P37D_PID" 2>/dev/null || break
    sleep 0.2
  done
  P37D_PID=""
  sudo rm -f /run/p37d-root.pid
}

NNP_FAILED=0
sudo systemctl start p37d.service || true
if ! wait_socket; then
  NNP_FAILED=1
  echo "NoNewPrivileges=yes 首次启动未成功，记录并尝试去掉 NNP"
  sudo systemctl stop p37d.service || true
  sudo systemctl reset-failed p37d.service || true
  sudo systemctl status p37d.service --no-pager || true
  sudo journalctl -u p37d.service -n 160 --no-pager || true
  sudo mkdir -p /etc/systemd/system/p37d.service.d
  printf '[Service]\nNoNewPrivileges=no\n' | sudo tee /etc/systemd/system/p37d.service.d/98-no-nnp.conf >/dev/null
  sudo systemctl daemon-reload
  sudo systemctl reset-failed p37d.service || true
  sudo systemctl start p37d.service || true
  if ! wait_socket; then
    if [ "${P37_ALLOW_ROOT_FALLBACK:-0}" != "1" ]; then
      echo "p37d 在 User=p36 + auto 下仍未启动；默认不允许 root fallback。"
      echo "如果只是测试环境需要 root 对照，设置 P37_ALLOW_ROOT_FALLBACK=1。"
      sudo systemctl status p37d.service --no-pager || true
      sudo journalctl -u p37d.service -n 160 --no-pager || true
      exit 1
    fi
    echo "允许 root fallback：去掉 NNP 后仍未启动，退化到 root 直接进程"
    ROOT_FALLBACK=1
    sudo systemctl stop p37d.service || true
    sudo systemctl reset-failed p37d.service || true
    sudo systemctl status p37d.service --no-pager || true
    sudo journalctl -u p37d.service -n 160 --no-pager || true
    if ! start_root_daemon; then
      echo "root 直接进程仍无法启动，验收失败"
      cat p37d-root.log || true
      exit 1
    fi
  fi
fi
echo "NNP_FAILED=$NNP_FAILED ROOT_FALLBACK=$ROOT_FALLBACK"

say "多仓库 registry / legacy default"
as_app /usr/local/bin/p37 --root "$ROOTDIR" repo list --json | tee repo-list.json
jq -e '.[] | select(.name == "default")' repo-list.json >/dev/null
jq -e --arg n "$REPO_NAME" '.[] | select(.name == $n)' repo-list.json >/dev/null



# --check / doctor 适合 daemon 启动前：先让 p37-run 完成挂载与 init，
# 停服（tmpfiles 创建的目录不会随 unit 清理，payload 保留），在 daemon 停止状态下检查。
say "停服后做启动前检查（payload 应保留）"
if [ "$ROOT_FALLBACK" = 1 ]; then
  stop_root_daemon
else
  sudo systemctl stop p37d.service
fi
[ -x /run/p37-payload/p37 ] || { echo "停服后 payload 被卸载，p37 不可用"; exit 1; }

say "服务外 findmnt：payload"
findmnt -T /run/p37-payload | tee findmnt-payload.txt

say "payload 根目录"
find /run/p37-payload -maxdepth 1 -mindepth 1 -printf '%f\n' | sort | tee payload-root.txt
[ "$(wc -l < payload-root.txt | tr -d ' ')" = "3" ]
for f in p37 p37d manifest.json; do grep -qx "$f" payload-root.txt; done

say "p37d --check --strict"
as_caps /run/p37-payload/p37d --check --strict --json --root "$ROOTDIR" | tee check.json

say "p37 doctor（copy 模式下 OverlayFS FAIL 是预期，不作为硬门禁）"
set +e
as_caps /run/p37-payload/p37 doctor "$REPO_DIR" > doctor.txt 2>&1
DOCTOR_EXIT=$?
set -e
cat doctor.txt
echo "DOCTOR_EXIT=$DOCTOR_EXIT"

say "重新启动 p37d（NNP_FAILED=$NNP_FAILED ROOT_FALLBACK=$ROOT_FALLBACK）"
if [ "$ROOT_FALLBACK" = 1 ]; then
  start_root_daemon || { echo "root 直接进程重启失败"; cat p37d-root.log; exit 1; }
else
  sudo systemctl start p37d.service
  wait_socket || { echo "检查后重启 socket 未出现"; exit 1; }
fi

say "p37-mcp initialize 探针"
cat > "$WORK/init.json" <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"ci-probe","version":"0"}}}
EOF
chmod 0755 "$WORK"
chmod 0644 "$WORK/init.json"
as_app env P37_ROOT="$ROOTDIR" sh -c "(cat '$WORK/init.json'; sleep 1) | /usr/local/bin/p37-mcp" > mcp-probe.json
grep -q '"id":1' mcp-probe.json
cat mcp-probe.json

say "两个 agent：ws_open/ws_sync/ws_land"
go test -c -tags p36acceptance -o "$WORK/p36-acceptance.test" ./packaging/p36/acceptance
sudo install -m 0755 "$WORK/p36-acceptance.test" /usr/local/bin/p37-acceptance
as_app env P37_ROOT="$ROOTDIR" P37_REPO="$LEGACY" P37_REPO_NAME="$REPO_NAME" \
  P37_REPO_DIR="$REPO_DIR" P37_MCP=/usr/local/bin/p37-mcp \
  P37_ACCEPTANCE_OUT="$REPO_DIR/.p37/acceptance.json" \
  P37_ACCEPTANCE_WS="$REPO_DIR/.p37/acceptance-ws.txt" \
  /usr/local/bin/p37-acceptance -test.v | tee acceptance-mcp.log
sudo cp "$REPO_DIR/.p37/acceptance.json" acceptance.json
sudo cp "$REPO_DIR/.p37/acceptance-ws.txt" acceptance-ws.txt
cat acceptance.json

MAT="$(jq -r '.keeper_materialize // empty' acceptance.json)"
case "$MAT" in
  copy|overlay) ;;
  *) echo "无法从 acceptance.json 读取 materialize（得到 %q）" "$MAT"; exit 1 ;;
esac
echo "MATERIALIZE=$MAT"
if [ "$DOCTOR_EXIT" -ne 0 ] && [ "$MAT" != "copy" ]; then
  echo "doctor 退出码 $DOCTOR_EXIT，且 materialize=$MAT，不是预期的 copy 回退"
  exit 1
fi
if [ "$DOCTOR_EXIT" -ne 0 ]; then
  echo "DOCTOR_EXIT=$DOCTOR_EXIT（copy 模式下 OverlayFS FAIL 预期）"
fi

WS1="$(head -n 1 acceptance-ws.txt)"
say "服务外 findmnt：workspace $WS1（MATERIALIZE=$MAT）"
as_app findmnt -T "$REPO_DIR/workspaces/$WS1" | tee findmnt-workspace.txt
if [ "$MAT" = "overlay" ]; then
  grep -q overlay findmnt-workspace.txt
else
  grep -q ext4 findmnt-workspace.txt
fi

say "重启 p37d"
if [ "$ROOT_FALLBACK" = 1 ]; then
  stop_root_daemon
  start_root_daemon || { echo "root 直接进程重启失败"; cat p37d-root.log; exit 1; }
else
  sudo systemctl restart p37d.service
  wait_socket || { echo "重启后 socket 未出现"; exit 1; }
fi
as_app findmnt -T "$REPO_DIR/workspaces/$WS1" | tee findmnt-workspace-after-restart.txt
if [ "$MAT" = "overlay" ]; then
  grep -q overlay findmnt-workspace-after-restart.txt
else
  grep -q ext4 findmnt-workspace-after-restart.txt
fi
as_app /run/p37-payload/p37 ws-list "$REPO_DIR" | tee ws-list.txt

say "GC 拒绝（daemon 运行中）"
set +e
as_app /run/p37-payload/p37 gc "$REPO_DIR" --prune > gc-running.out 2>&1
GC_RUNNING_CODE=$?
set -e
cat gc-running.out
[ "$GC_RUNNING_CODE" -ne 0 ]

say "停服后 GC"
if [ "$ROOT_FALLBACK" = 1 ]; then
  stop_root_daemon
else
  sudo systemctl stop p37d.service
fi
[ -x /run/p37-payload/p37 ] || { echo "停服后 payload 被卸载，p37 不可用"; exit 1; }
set +e
as_app /run/p37-payload/p37 gc "$REPO_DIR" --prune --pack > gc-stopped.out 2>&1
GC_STOPPED_CODE=$?
set -e
cat gc-stopped.out
[ "$GC_STOPPED_CODE" -eq 0 ]

say "acceptance ok (NNP_FAILED=$NNP_FAILED ROOT_FALLBACK=$ROOT_FALLBACK MATERIALIZE=$MAT)"
{
  echo "NNP_FAILED=$NNP_FAILED"
  echo "ROOT_FALLBACK=$ROOT_FALLBACK"
  echo "MATERIALIZE=$MAT"
} | tee -a "$GITHUB_STEP_SUMMARY" 2>/dev/null || true
