#!/bin/sh
# sumus2api 容器入口：
#   1. 强制清空 config.json 里的 api_key（避免镜像内残留密钥）
#   2. 若以 root 启动，降权到非 root 用户 sumus
#   3. exec 启动网关
set -eu

CONFIG="${SUMUS2API_CONFIG:-/app/config.json}"

# 1. 清空 api_key。用 sed 就地改写，保持 JSON 结构不变。
#    容器里的 config 由镜像模板 + 挂载卷共同决定；无论如何都不允许带密钥运行。
if [ -f "$CONFIG" ]; then
    sed -i 's/"api_key"[[:space:]]*:[[:space:]]*"[^"]*"/"api_key": ""/' "$CONFIG"
fi

# 2. 降权：root 启动时用 su-exec 切到 sumus 用户
if [ "$(id -u)" = "0" ]; then
    # 保证挂载卷权限正确（宿主机 uid 可能不同）
    chown -R sumus:sumus /app/accounts /app/data 2>/dev/null || true
    exec su-exec sumus /app/sumus2api "$@"
fi

exec /app/sumus2api "$@"
