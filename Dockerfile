FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/tgnas ./cmd/tgnas

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H app

COPY --from=build /out/tgnas /usr/local/bin/tgnas
USER app
EXPOSE 9000
WORKDIR /app
CMD ["tgnas"]
