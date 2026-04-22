# syntax=docker/dockerfile:1.10
# Watchkeeper core — placeholder Dockerfile. Real build lands in M1/M10.
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/watchkeeper ./core/cmd/watchkeeper

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app

COPY --from=build /out/watchkeeper /app/watchkeeper

USER nonroot:nonroot

ENTRYPOINT ["/app/watchkeeper"]
