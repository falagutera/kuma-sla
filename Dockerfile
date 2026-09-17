# --- build stage -----------------------------------------------------------
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# static binary; timetzdata embeds the tz database so TZ works on scratch
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -tags timetzdata -o /kuma-sla .

# --- runtime stage ---------------------------------------------------------
FROM scratch
COPY --from=build /kuma-sla /kuma-sla

# Container defaults: mount the Kuma DB read-only at /data and configs at /config.
ENV KUMA_DB=/data/kuma.db \
    CONFIG_DIR=/config \
    LISTEN=0.0.0.0:3002

EXPOSE 3002
USER 65534:65534
ENTRYPOINT ["/kuma-sla"]
