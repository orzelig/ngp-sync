FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /identity .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=build /identity /identity
VOLUME /data
ENV DATA_FILE=/data/identity.json
EXPOSE 8300
CMD ["/identity"]
