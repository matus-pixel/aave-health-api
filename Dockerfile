FROM golang:1.27-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/aave-health-api ./main.go

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && addgroup -S app && adduser -S -G app app
COPY --from=builder /out/aave-health-api /usr/local/bin/aave-health-api

USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/aave-health-api"]
