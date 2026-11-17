# The site puller (cmd/site-pull), built on its own so the identity image is
# unchanged. git is in the image, so the VM host needs none. Build context is
# ./identity:
#   docker build -t ngp-site-pull:local -f identity/site-pull.Dockerfile identity
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/site-pull ./cmd/site-pull
RUN CGO_ENABLED=0 go build -o /site-pull ./cmd/site-pull

FROM alpine:3.24
RUN apk add --no-cache ca-certificates git
COPY --from=build /site-pull /site-pull
# The unit overrides this with --user set to the owner of the checkout's site/
# (provision/vm-setup.sh writes it to /etc/ngp-site-pull.env).
USER 65532:65532
ENTRYPOINT ["/site-pull"]
