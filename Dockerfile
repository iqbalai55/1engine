# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/provider-bootstrap ./cmd/provider-bootstrap

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 app

COPY --from=build /out/server /app/server
COPY --from=build /out/provider-bootstrap /app/provider-bootstrap

USER app

EXPOSE 8080

ENTRYPOINT ["/app/server"]
