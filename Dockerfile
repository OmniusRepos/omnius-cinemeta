# Stdlib-only Go service -> tiny static binary in alpine.
FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o cinemeta .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=build /app/cinemeta /cinemeta
EXPOSE 8080
CMD ["/cinemeta"]
