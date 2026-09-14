# syntax=docker/dockerfile:1

# ---- 构建阶段：编译 API 与一次性验收程序，均为纯静态二进制 ----
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tidegram .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/verify ./cmd/verify

# ---- API 运行镜像 ----
FROM scratch AS api
COPY --from=build /out/tidegram /tidegram
ENV PORT=8080
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=5s --timeout=2s --retries=10 CMD ["/tidegram", "-healthcheck"]
ENTRYPOINT ["/tidegram"]

# ---- 一次性验收镜像（compose 中的 verify 服务） ----
FROM scratch AS verify
COPY --from=build /out/verify /verify
USER 65532:65532
ENTRYPOINT ["/verify"]
