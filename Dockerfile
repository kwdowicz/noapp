FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/noapp ./cmd/server

FROM alpine:3.22
RUN addgroup -S app && adduser -S app -G app
COPY --from=build /out/noapp /usr/local/bin/noapp
USER app
EXPOSE 8080
ENTRYPOINT ["noapp"]
