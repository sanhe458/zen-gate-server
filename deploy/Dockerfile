# Zen Gate Server — 最小化多阶段构建
#   docker build -t zen-gate-server .
#   docker run -d --name zen-gate -p 127.0.0.1:8787:8787 \
#     -v zen-gate-data:/data -e ZEN_GATE_HOME=/data zen-gate-server
#
# 想让局域网其他机器访问，把端口映射改成 8787:8787 并在设置里绑定 0.0.0.0 + 设 WebUI 密码。

FROM golang:1.25-alpine AS build
WORKDIR /src
# 纯标准库实现 + 单模块，无需 go mod download 预热
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.buildVersion=1.0.0 -X zen-gate-server/internal/server.Version=1.0.0" \
      -o /out/zen-gate-server ./cmd/zen-gate-server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 10001 zengate && \
    mkdir -p /data && chown zengate:zengate /data
# 时区：容器内默认 UTC，如需东八区
ENV TZ=Asia/Shanghai
ENV ZEN_GATE_HOME=/data
COPY --from=build /out/zen-gate-server /usr/local/bin/zen-gate-server
USER zengate
# 默认绑 0.0.0.0 才能被宿主机端口映射访问；生产请配合 WebUI 密码
EXPOSE 8787
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8787/health >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/usr/local/bin/zen-gate-server"]
CMD ["-host", "0.0.0.0", "-port", "8787"]
