FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /gateway /gateway
COPY configs/gateway.json /etc/gateway/gateway.json
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/gateway", "-config", "/etc/gateway/gateway.json"]
