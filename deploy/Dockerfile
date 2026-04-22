# syntax=docker/dockerfile:1.10
# Watchkeeper core — placeholder Dockerfile. Real build lands in M1/M10.
FROM golang:1.26-alpine AS build

WORKDIR /src

RUN apk add --no-cache --virtual .build-deps git=2.47.0-r0 || apk add --no-cache git

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/watchkeeper ./core/cmd/watchkeeper

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app

COPY --from=build /out/watchkeeper /app/watchkeeper

USER nonroot:nonroot

ENTRYPOINT ["/app/watchkeeper"]
