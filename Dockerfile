FROM golang:1.25-alpine AS build

WORKDIR /app
COPY . .

RUN go mod download && go build -o matrix-alert-forwarder .

FROM alpine:latest AS final

WORKDIR /
COPY --from=build /app/matrix-alert-forwarder ./matrix-alert-forwarder
COPY --from=build /app/scripts ./scripts

EXPOSE 8080
ENTRYPOINT ["./matrix-alert-forwarder"]
