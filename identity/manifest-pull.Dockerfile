# The manifest puller (cmd/manifest-pull), built on its own so the identity
# image is unchanged. Build context is ./identity:
#   docker build -t ngp-manifest-pull:local -f identity/manifest-pull.Dockerfile identity
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/manifest-pull ./cmd/manifest-pull
RUN CGO_ENABLED=0 go build -o /manifest-pull ./cmd/manifest-pull

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=build /manifest-pull /manifest-pull
USER 65532:65532
ENTRYPOINT ["/manifest-pull"]
