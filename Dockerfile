# syntax=docker/dockerfile:1

FROM golang:1.21 as builder

COPY ./src /dyndns
WORKDIR /dyndns
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o /updater

FROM alpine:latest
COPY --from=builder /updater /zone-dyndns/updater
WORKDIR  /zone-dyndns
ENTRYPOINT [ "/zone-dyndns/updater" ]