FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/aggregator ./cmd/aggregator

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 aggregator
COPY --from=build /out/aggregator /usr/local/bin/aggregator
USER aggregator
ENTRYPOINT ["/usr/local/bin/aggregator"]
CMD ["run"]
