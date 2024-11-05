FROM golang:alpine AS app-builder
WORKDIR /go/src/app
COPY . .
RUN CGO_ENABLED=0 go install -ldflags '-extldflags "-static"' -tags timetzdata -buildvcs=false

FROM scratch
COPY --from=app-builder /go/bin/nirn-proxy /nirn-proxy

COPY --from=alpine:latest /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY .env /

EXPOSE 9000
EXPOSE 8080
ENTRYPOINT ["/nirn-proxy"]