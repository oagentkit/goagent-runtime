#!/usr/bin/env bash
# 依赖面门禁：纯判据包不得链数据库驱动（runtime issue #10 的机器判据）。
#
# 为什么要有这条：`storage` 曾把「判据面（Provider 契约 + 内存实现）」与
# 「绑定胶水（OpenSQLite → sqliteadapter）」打包在同一个包里。Go 的依赖是
# **包级**的 ⇒ 只要 import 了 storage，modernc 驱动就进链接面 ⇒ 宿主自己
# 带一个同名注册 "sqlite" 的驱动时，同二进制 init 即 panic：
# `panic: sql: Register called twice for driver sqlite`。
#
# 判据口径：纯判据包的**生产依赖面与测试依赖面**都不得出现数据库驱动。
# 失败形态是**变红**；判据跑不起来（go list 失败）同样判红——不许报绿。
set -uo pipefail

cd "$(dirname "$0")/.."

# 声明面：纯判据包（只有契约与判据实现，零驱动边）。
PURE_PKGS=${PURE_PKGS:-"./storage"}

# 被禁的驱动：它们在 init 里往进程级全局表注册驱动名，
# 同名第二次注册直接 panic（宿主侧事故的机制面）。
FORBIDDEN_DRIVERS=(
  "modernc.org/sqlite"
  "github.com/mattn/go-sqlite3"
  "github.com/glebarez/go-sqlite"
  "github.com/jackc/pgx"
)

fail=0

check_pkg() {
  local pkg=$1 mode=$2 label=$3
  local deps
  if ! deps=$(go list -deps $mode "$pkg" 2>&1); then
    echo "✗ go list -deps $mode $pkg 失败——判据跑不起来不许报绿："
    printf '%s\n' "$deps" | head -5
    fail=1
    return
  fi
  for drv in "${FORBIDDEN_DRIVERS[@]}"; do
    if printf '%s\n' "$deps" | grep -q "^${drv}"; then
      echo "✗ $pkg（$label 依赖面）链入驱动 $drv —— 纯判据包不得带驱动边（issue #10）"
      fail=1
    fi
  done
}

for pkg in $PURE_PKGS; do
  check_pkg "$pkg" "" "生产"
  check_pkg "$pkg" "-test" "测试"
done

if [ "$fail" -ne 0 ]; then
  echo "依赖面门禁判红：纯判据包带了驱动边（宿主侧同名驱动会在 init 阶段 panic）"
  exit 1
fi
echo "✅ 依赖面门禁通过：${PURE_PKGS} 的生产与测试依赖面均无数据库驱动"
