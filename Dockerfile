FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/codearts ./cmd/server \
 && go build -trimpath -ldflags="-s -w" -o /out/cnlogin ./cmd/cnlogin \
 && go build -trimpath -ldflags="-s -w" -o /out/cncredit ./cmd/cncredit \
 && go build -trimpath -ldflags="-s -w" -o /out/cnmodels ./cmd/cnmodels

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/codearts /usr/local/bin/codearts
# 国内码道工具链：容器内可直接 cnlogin 扫码登录 / 查签到 / 拉模型目录
COPY --from=build /out/cnlogin /usr/local/bin/cnlogin
COPY --from=build /out/cncredit /usr/local/bin/cncredit
COPY --from=build /out/cnmodels /usr/local/bin/cnmodels
ENV PORT=29137 DATA_DIR=/data
VOLUME /data
EXPOSE 29137
CMD ["codearts"]
