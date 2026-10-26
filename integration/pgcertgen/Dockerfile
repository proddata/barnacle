FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -o /pgcertgen main.go

FROM alpine:3.24
COPY --from=build /pgcertgen /usr/local/bin/pgcertgen
ENTRYPOINT ["/usr/local/bin/pgcertgen"]
