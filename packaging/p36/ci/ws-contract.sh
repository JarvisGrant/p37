#!/usr/bin/env bash
# 工作区协议验收（v0.4.0）。
#
# 用法（P36 VM 或任意 Linux/macOS 开发机）：
#   P37_BIN=/usr/local/bin/p37 bash packaging/p36/ci/ws-contract.sh
#
# 覆盖：
#   - 5 个独立进程并发 ws-ensure：只产生 1 个工作区，ws_id 相同
#   - 再并发 5 次：ws_id 不变
#   - ws_drop 后 include_dropped 可查 tombstone
#   - 进程退出/重开后 line/version 仍在（同一卷的本地真相源）
#
# 真正的 P36 stop → rebuild 需要在 P36 侧停止/重建实例后，再对同一
# /srv/repos/<app> 跑本脚本的持久性段；脚本不会自行 systemctl stop/start。
set -euo pipefail

P37_BIN="${P37_BIN:-p37}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/p37-ws-contract.XXXXXX")"
ROOT="$WORK/repo"
SRC="$WORK/src"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$SRC"
echo "hello" > "$SRC/a.txt"
"$P37_BIN" init "$ROOT" >/dev/null
"$P37_BIN" import "$ROOT" "$SRC" >/dev/null

say() { echo "==> $*"; }

say "5 个独立进程并发 ws-ensure 同名工作区"
for i in 1 2 3 4 5; do
  "$P37_BIN" ws-ensure "$ROOT" --name root-turn-1 --task t --json \
    > "$WORK/out.$i" 2> "$WORK/err.$i" &
done
wait
if grep -q . "$WORK"/err.* 2>/dev/null; then
  echo "并发 ensure 有错误："; cat "$WORK"/err.*; exit 1
fi
IDS="$(jq -r '.ws_id' "$WORK"/out.* | sort -u)"
COUNT="$(printf '%s\n' "$IDS" | grep -c . || true)"
[ "$COUNT" -eq 1 ] || { echo "ws_id 不唯一: $IDS"; exit 1; }
WS="$(printf '%s\n' "$IDS" | head -n1)"
WS_COUNT="$("$P37_BIN" ws-list "$ROOT" --json | jq 'length')"
[ "$WS_COUNT" -eq 1 ] || { echo "ws_list=$WS_COUNT，期望 1"; exit 1; }
echo "WS_IDS=$WS"
echo "WS_LIST_COUNT=$WS_COUNT"

say "再次并发 ensure，ws_id 不变"
for i in 1 2 3 4 5; do
  "$P37_BIN" ws-ensure "$ROOT" --name root-turn-1 --json > "$WORK/again.$i" &
done
wait
jq -r '.ws_id' "$WORK"/again.* | sort -u | grep -qx "$WS" \
  || { echo "再次 ensure 的 ws_id 变了"; exit 1; }

say "drop 后 tombstone 可查"
"$P37_BIN" ws-drop "$ROOT" "$WS" >/dev/null
"$P37_BIN" ws-list "$ROOT" --include-dropped --json \
  | jq -e --arg ws "$WS" '.[] | select(.ws_id==$ws and .state=="tombstone")' >/dev/null
echo "TOMBSTONE_OK ws=$WS"

say "进程退出/重开后的本地真相源"
"$P37_BIN" status "$ROOT" >/dev/null
"$P37_BIN" ws-list "$ROOT" --include-dropped --json \
  | jq -e --arg ws "$WS" '.[] | select(.ws_id==$ws and .reason=="abandoned")' >/dev/null
echo "PERSIST_OK"

say "PASS（若在 P36 上，请再重启实例后重跑本脚本的持久性段）"
