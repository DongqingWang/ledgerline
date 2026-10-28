FROM golang:1.23-bookworm AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ledgerd ./cmd/ledgerd

FROM alpine:3.20
RUN adduser -D -u 10001 ledgerline && mkdir -p /data && chown ledgerline:ledgerline /data
WORKDIR /data
USER ledgerline
COPY --from=build /out/ledgerd /ledgerd
ENTRYPOINT ["/ledgerd"]
