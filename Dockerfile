FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /hermit .
FROM alpine:3.21
RUN addgroup -S hermit && adduser -S -G hermit hermit
COPY --from=build /hermit /usr/local/bin/hermit
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/doc/hermit/
COPY THIRD-PARTY-LICENSES/ /usr/share/doc/hermit/THIRD-PARTY-LICENSES/
USER hermit
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hermit"]
