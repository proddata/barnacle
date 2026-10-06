FROM golang:1.25.14-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /barnacle .
FROM alpine:3.21
RUN apk upgrade --no-cache && addgroup -S barnacle && adduser -S -G barnacle barnacle
COPY --from=build /barnacle /usr/local/bin/barnacle
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/doc/barnacle/
COPY THIRD-PARTY-LICENSES/ /usr/share/doc/barnacle/THIRD-PARTY-LICENSES/
USER barnacle
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/barnacle"]
