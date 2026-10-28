FROM golang:1.23-bookworm AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ledgerline-ai-gateway ./cmd/ledgerline-ai-gateway

FROM alpine:3.20
RUN adduser -D -u 10001 ledgerline
USER ledgerline
COPY --from=build /out/ledgerline-ai-gateway /ledgerline-ai-gateway
COPY redaction.yaml /redaction.yaml
# This is an MCP stdio server, not a long-running networked service -- it's
# meant to be exec'd interactively by an MCP client (or `docker compose run
# -i`), not started as a steady-state daemon. See README.md.
ENTRYPOINT ["/ledgerline-ai-gateway", "-redaction-policy", "/redaction.yaml"]
