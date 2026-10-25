FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/server .
RUN mkdir -m 0700 -p /out/relay-cache

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
COPY --from=build --chown=65532:65532 /out/relay-cache /var/lib/alloy-config-server/relay-cache
EXPOSE 8080
ENTRYPOINT ["/server"]
