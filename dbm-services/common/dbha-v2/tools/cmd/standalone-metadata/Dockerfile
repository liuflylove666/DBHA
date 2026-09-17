FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /standalone-metadata ./tools/cmd/standalone-metadata

FROM alpine:3.22
RUN adduser -D -H -u 10001 app
USER app
COPY --from=build /standalone-metadata /usr/local/bin/standalone-metadata
EXPOSE 8080
ENTRYPOINT ["standalone-metadata"]
