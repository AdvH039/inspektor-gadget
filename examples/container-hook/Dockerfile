FROM golang:1.22.8@sha256:ed01175ba5c08d20d824d3a3387ec2e392827e1116212ff0ab3e6db7b7de63ba AS builder

# Cache go modules so they won't be downloaded at each build
COPY go.mod go.sum /gadget/
COPY examples/go.mod examples/go.sum /gadget/examples/
RUN cd /gadget && go mod download

COPY ./ /gadget
RUN cd /gadget/examples/container-hook && make container-hook-static

FROM busybox@sha256:9ae97d36d26566ff84e8893c64a6dc4fe8ca6d1144bf5b87b2b85a32def253c7
COPY --from=builder /gadget/examples/container-hook/container-hook-static /bin/container-hook
ENTRYPOINT ["/bin/container-hook"]
