#!/usr/bin/env bash
#
# 重新生成离线评测集与基线（P2 前置项 B1/B2 的交付物之一）。
#
# 为什么必须是一条脚本而不是"README 里贴几条命令"：
# 基线数字的唯一价值是可复现。命令散在文档里，最常见的结果是
# 快照被重新生成、而基线文件还是旧的 —— 报告里的数字与数据对不上，
# 却没有任何地方会报错。把整条链路写成一个入口，这种情况就不可能发生。
#
# 用法：
#   ./regenerate.sh synthetic            # 合成评测集（默认；不需要数据库、不需要 API key）
#   ./regenerate.sh real '<MySQL DSN>'   # 从真实库导出快照与基线
#
# 合成模式的输出**逐字节可复现**：它用 SOURCE_DATE_EPOCH 把"生成时间"
# 固定下来，并在写盘后再生成一次做 cmp 校验。校验失败会以非零码退出 ——
# 一份"看起来一样"但字节不同的快照会让 git diff 永远处于噪声状态。
#
# P2 起它还跑一遍 cmd/evalp2（语义召回 + 融合 vs 基线）：
# P2 §4.7 的完成标志就是"有 Recall@K / NDCG 的新旧对比数字"，
# 而那组数字必须与快照同源、同一次生成，否则两次运行之间数据变了、
# 数字却没人重新跑。
set -euo pipefail

cd "$(dirname "$0")/../.."   # -> backend/

# 2026-01-01T00:00:00Z。用可复现构建领域的通用变量名而不是自定义参数：
# 这样任何 CI 或打包工具都不需要为这个仓库学一套新约定。
FIXED_EPOCH="${SOURCE_DATE_EPOCH:-1767225600}"
SYNTH_SEED="${SYNTH_SEED:-20261008}"

mode="${1:-synthetic}"

run() {
  echo "+ $*"
  "$@"
}

case "$mode" in
  synthetic)
    out=testdata/eval/snapshot-synthetic.json
    tmp="$(mktemp -t evalseed-XXXXXX.json)"
    trap 'rm -f "$tmp"' EXIT

    export SOURCE_DATE_EPOCH="$FIXED_EPOCH"
    run go run ./cmd/evalseed \
      -seed "$SYNTH_SEED" \
      -out "$out" \
      -json testdata/eval/baseline-synthetic.json \
      -markdown testdata/eval/baseline-synthetic.md \
      -lexical-probe

    # P2 的效果对比（语义召回 vs 基线）。参数写在这里而不是让人凭记忆敲：
    # 一份"数字变了但参数没记"的结果无法复核，而 P2 §4.7 要求结果含参数。
    run go run ./cmd/evalp2 \
      -snapshot "$out" \
      -k 5,10,20 \
      -recall-quota 0.3 \
      -explore-quota 0.1 \
      -json testdata/eval/p2-synthetic.json \
      -markdown testdata/eval/p2-synthetic.md

    # 逐字节复现校验：再生成一次到临时文件并逐字节比较。
    run go run ./cmd/evalseed -seed "$SYNTH_SEED" -out "$tmp" >/dev/null
    if ! cmp -s "$out" "$tmp"; then
      echo "!! 同一 seed 两次生成的快照不是逐字节一致的：可复现性已被破坏" >&2
      echo "   常见原因：往快照里写了 time.Now()、依赖了 map 遍历顺序、" >&2
      echo "   或者随机数消费顺序与参数相关。" >&2
      exit 1
    fi
    echo "OK: $out 逐字节可复现（seed=$SYNTH_SEED, SOURCE_DATE_EPOCH=$FIXED_EPOCH）"
    ;;

  real)
    dsn="${2:-${MYSQL_DSN:-}}"
    if [[ -z "$dsn" ]]; then
      echo "用法: ./regenerate.sh real '<MySQL DSN>'" >&2
      echo "例如: ./regenerate.sh real 'root:123456@tcp(127.0.0.1:3307)/feedflow?charset=utf8mb4&parseTime=True&loc=Local'" >&2
      exit 2
    fi
    run go run ./cmd/evalbaseline \
      -dsn "$dsn" \
      -snapshot-out testdata/eval/snapshot.json \
      -json testdata/eval/baseline.json \
      -markdown testdata/eval/baseline.md

    # 真实数据上的 P2 对比。样本量不足时报告会显式标注"不可作为结论"，
    # 但那仍然值得跑：它能证明"在同一份真实数据上，两条路径都能算出数字"。
    run go run ./cmd/evalp2 \
      -snapshot testdata/eval/snapshot.json \
      -k 5,10,20 \
      -recall-quota 0.3 \
      -explore-quota 0.1 \
      -json testdata/eval/p2.json \
      -markdown testdata/eval/p2.md
    ;;

  *)
    echo "未知模式: $mode（可选 synthetic | real）" >&2
    exit 2
    ;;
esac
