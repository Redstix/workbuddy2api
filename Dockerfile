# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
# Go 模块代理：默认走国内代理。官方 proxy.golang.org 在中国大陆不可达，go mod download
# 会长时间卡在这一层（看着像构建挂了）。海外/CI 构建可覆盖回官方：
#   --build-arg GOPROXY=https://proxy.golang.org,direct
# 用 ARG + ENV 而不是直接 ENV：ARG 可被 --build-arg 覆盖。注意传空值（GOPROXY=）不会
# 变成「直连 VCS」，而是回落到 Go 自带的 GOROOT/go.env 默认（proxy.golang.org,direct）——
# 也就是又变回国内不可达。所以覆盖时请给实际值，别给空。
ARG GOPROXY=https://goproxy.cn,direct
# GOSUMDB=off：顺手省掉向 sum.golang.org 取校验记录这一步（每模块一次额外往返）。注意这不
# 是「不得不关」—— 实测 GOSUMDB=sum.golang.org 配 GOPROXY=https://goproxy.cn 时
# goproxy.cn 会代理 sumdb（GET /sumdb/sum.golang.org/supported 返回 200），校验是通的。
# 关掉只是省一跳。模块内容完整性不受影响 —— go.sum 已钉住全部依赖，go mod download 仍逐个
# 校验哈希（失败即报 checksum mismatch），GOSUMDB 只影响「新模块首次入 go.sum」这一场景。
# 需要恢复校验库校验时覆盖：--build-arg GOSUMDB=sum.golang.org
ARG GOSUMDB=off
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}
# 依赖层独立缓存：只在 go.mod/go.sum 变动时重新下载；--mount 的模块缓存跨构建复用。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/activity_bin /app/activity_bin
COPY login.sh signin.sh credit.sh trial.sh /app/
# 国际版注册地区自动完善模块（login.sh global 分支 import；scripts/ 无测试/缓存）
COPY scripts/global_region.py /app/scripts/global_region.py
COPY scripts/task_common.py /app/scripts/task_common.py
COPY scripts/task_runner.py /app/scripts/task_runner.py
COPY scripts/school_open_day_2026.py /app/scripts/school_open_day_2026.py
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
RUN sed -i 's/\r$//' /app/scripts/*.py && chmod 755 /app/scripts/*.py
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
