FROM golang:1.23-bookworm AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/balance-reader .

FROM alpine:3.20
RUN adduser -D -u 10001 ledgerline
USER ledgerline
COPY --from=build /out/balance-reader /balance-reader
ENTRYPOINT ["/balance-reader"]
