FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/codearts ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/codearts /usr/local/bin/codearts
ENV PORT=29137 DATA_DIR=/data
VOLUME /data
EXPOSE 29137
CMD ["codearts"]