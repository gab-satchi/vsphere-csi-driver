ARG servercoreTag="ltsc2019"
ARG golangTag=windowsservercore-1809

#FROM golang:${golangTag} as builder
#
## This build arg is the version to embed in the CSI binary
#ARG VERSION=unknown
#
## This build arg controls the GOPROXY setting
#ARG GOPROXY
#
#WORKDIR /build
#COPY go.mod go.sum ./
#COPY pkg/    pkg/
#COPY cmd/    cmd/
#ENV CGO_ENABLED=0
#ENV GOPROXY ${GOPROXY:-https://proxy.golang.org}
#RUN go build -a -ldflags="-w -s -extldflags=static -X sigs.k8s.io/vsphere-csi-driver/pkg/csi/service.Version=${VERSION}" -o vsphere-csi ./cmd/vsphere-csi



## Main Stage
FROM golang:${golangTag}

ADD .build/bin/vsphere-csi.windows_amd64 /bin/vsphere-csi
#COPY build/vsphere-csi.windows_amd64 /bin/vsphere-csi

ENTRYPOINT ["/bin/vsphere-csi"]