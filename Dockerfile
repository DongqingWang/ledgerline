FROM golang:1.23-bookworm AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ledgerline-fx ./cmd/ledgerline-fx

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ledgerline-fx /ledgerline-fx
COPY rates_seed.json /rates_seed.json
ENTRYPOINT ["/ledgerline-fx", "-seed-file", "/rates_seed.json"]
