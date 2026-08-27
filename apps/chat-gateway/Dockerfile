# チャットの前段（Go）: 静的バイナリを distroless に載せる（最軽・ARM64対応）
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod .
COPY main.go .
COPY ui.html .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /chat-gateway .

FROM gcr.io/distroless/static-debian12
COPY --from=build /chat-gateway /chat-gateway
EXPOSE 8080
ENTRYPOINT ["/chat-gateway"]
