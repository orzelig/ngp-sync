# Builds brave/go-sync at GO_SYNC_REF with the ngp JWT-auth patch applied.
FROM public.ecr.aws/docker/library/golang:1.27 AS builder
ARG GO_SYNC_REF=master
WORKDIR /src
RUN git clone https://github.com/brave/go-sync.git . \
 && git checkout --detach "${GO_SYNC_REF}"
COPY 0001-ngp-jwt-auth.patch /tmp/0001-ngp-jwt-auth.patch
RUN git apply --index /tmp/0001-ngp-jwt-auth.patch
RUN CGO_ENABLED=0 GOOS=linux go build -o main .

FROM alpine:3.24
RUN apk add --update ca-certificates
COPY --from=builder /src/main main
EXPOSE 8295
CMD ["./main"]
