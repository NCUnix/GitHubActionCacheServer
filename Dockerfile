FROM --platform=$BUILDPLATFORM golang:alpine AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
RUN if [ "$TARGETARCH" = arm ]; then \
        export GOARM="${TARGETVARIANT#v}"; \
    fi \
    && CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
        go build -trimpath -ldflags="-s -w" -o /out/cache-server ./cmd/server

FROM alpine:latest

COPY --from=build --chmod=0755 /out/cache-server /cache-server

RUN apk add --no-cache ca-certificates \
    && addgroup -S cache \
    && adduser -S -G cache cache

USER cache:cache
EXPOSE 3000
ENTRYPOINT ["/cache-server"]
