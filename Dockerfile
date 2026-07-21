FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/noapp ./cmd/server \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/outbox-relay ./cmd/outbox-relay \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/realtime ./cmd/realtime

FROM alpine:3.22
RUN addgroup -S app && adduser -S app -G app
COPY --from=build /out/noapp /usr/local/bin/noapp
COPY --from=build /out/outbox-relay /usr/local/bin/outbox-relay
COPY --from=build /out/realtime /usr/local/bin/realtime
USER app
EXPOSE 8080
CMD ["noapp"]
