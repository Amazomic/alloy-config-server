FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY *.go ./
COPY web ./web
# tidy дозаполнит go.sum и indirect-зависимости; версии прямых зависимостей зафиксированы в go.mod
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
