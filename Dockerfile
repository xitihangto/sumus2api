# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
# TARGETPLATFORM 让 buildx 一次产出多架构镜像；单架构 docker build 时自动取本机架构。
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# 先拷贝依赖清单，利用层缓存（本项目无第三方依赖，这一步很快）
COPY go.mod ./
RUN go mod download

COPY . .

# 静态编译：CGO 关闭，产物无动态库依赖，可直接丢进 alpine
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/sumus2api ./cmd/server

# ---------- 运行阶段 ----------
FROM alpine:3.21

# ca-certificates：上游 https://sumus.im 需要
# tzdata：支持 TZ 环境变量（容器内日志时区）
# su-exec：以非 root 身份运行
RUN apk add --no-cache ca-certificates tzdata su-exec

WORKDIR /app

COPY --from=builder /out/sumus2api /app/sumus2api
COPY config.template.json /app/config.json
COPY scripts/export-cookie.mjs /app/export-cookie.mjs
COPY README.md /app/README.md
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# 账号与状态目录（compose 挂载点），权限收紧
RUN mkdir -p /app/accounts /app/data \
    && chmod 700 /app/accounts /app/data \
    && chmod +x /usr/local/bin/docker-entrypoint.sh

# 非 root 运行用户
RUN addgroup -g 10001 -S sumus && adduser -u 10001 -S sumus -G sumus \
    && chown -R sumus:sumus /app

EXPOSE 7864

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-config", "/app/config.json"]
