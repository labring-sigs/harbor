FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /harbor-mock main.go

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app
COPY --from=builder /harbor-mock /usr/local/bin/harbor-mock
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/harbor-mock"]
