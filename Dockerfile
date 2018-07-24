FROM golang:1.10-alpine as golang

ADD . /go/src/github.com/jacohend/bank-vaults
WORKDIR /go/src/github.com/jacohend/bank-vaults

RUN go install ./cmd/bank-vaults


FROM alpine:3.7

RUN apk add --no-cache ca-certificates

COPY --from=golang /go/bin/bank-vaults /usr/local/bin/bank-vaults

ENTRYPOINT ["/usr/local/bin/bank-vaults"]
