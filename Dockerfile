FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/tgs3 ./cmd/tgs3

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H app
COPY --from=build /out/tgs3 /usr/local/bin/tgs3
USER app
EXPOSE 9000
ENTRYPOINT ["tgs3"]
CMD ["-config", "/etc/tgs3/config.yaml"]
