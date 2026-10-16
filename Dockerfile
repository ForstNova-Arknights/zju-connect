# syntax=docker/dockerfile:1

# ─────────────────────────────────────────────────────────────────────
# 构建阶段
#
# ★ 关键点：`--platform=$BUILDPLATFORM` 让**编译始终跑在原生架构**上
#   （CI 上是 amd64），再由 Go 自己的交叉编译产出各目标架构二进制。
#   好处：多架构构建**不需要 QEMU**，快得多也不会因模拟而偶发失败。
#
#   ⚠️ 上游 Dockerfile 里写死了 `ENV GOARCH=amd64`，它会覆盖 buildx 的
#   目标平台，导致 workflow 虽声明 linux/arm64、镜像里装的却是 amd64
#   二进制（运行即 exec format error）。本文件修掉了这个问题。
# ─────────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT_ID

ENV CGO_ENABLED=0

WORKDIR /src

# 先只拷依赖清单，让 go mod download 这层能被缓存
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    # linux/arm/v6 -> GOARM=6，linux/arm/v7 -> GOARM=7，其它架构为空
    GOARM="${TARGETVARIANT#v}"; \
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" GOARM="${GOARM}" \
    go build -v -o /out/zju-connect -trimpath \
        -ldflags "-s -w -buildid= \
                  -X main.zjuConnectVersion=${VERSION} \
                  -X main.CommitID=${COMMIT_ID}" . ; \
    echo "built for ${TARGETOS}/${TARGETARCH}${TARGETVARIANT:+ v$TARGETVARIANT}"

# ─────────────────────────────────────────────────────────────────────
# 运行阶段
#
# 选 alpine 而不是 distroless：这个客户端出问题时几乎一定要进容器看
# 日志、抓包、改配置，有个 shell 值这几 MB。运行也不需要 root/TUN。
# ─────────────────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && update-ca-certificates

COPY --from=build /out/zju-connect /usr/local/bin/zju-connect

# /data/config.toml  配置（含凭据，建议只读挂载）
# /data/state/       运行时状态，client_data.json 落在这里（要可写 + 持久化）
RUN mkdir -p /data/state
WORKDIR /data

# SOCKS5 / HTTP 代理
EXPOSE 1080 8888

# HTTP 代理没有健康检查端点（普通 GET / 会被当成代理请求处理），
# 所以只确认进程还活着。
HEALTHCHECK --interval=60s --timeout=5s --start-period=40s --retries=3 \
    CMD pidof zju-connect >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/zju-connect"]
CMD ["--config", "/data/config.toml"]
