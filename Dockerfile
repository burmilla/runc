FROM golang:1.27.3

RUN apt-get update && apt-get install -y \
    build-essential \
    pkg-config \
    --no-install-recommends \
    && apt-get clean

# The tree is GOPATH/vendor based, not Go modules
ENV GO111MODULE=off GOTOOLCHAIN=local

COPY script/tmpmount /
WORKDIR /go/src/github.com/opencontainers/runc
ENTRYPOINT ["/tmpmount"]

ADD . /go/src/github.com/opencontainers/runc
