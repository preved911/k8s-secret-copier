FROM golang:1.17-stretch AS build
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download -x

COPY . .
RUN go build -o k8s-secret-copier cmd/k8s-secret-copier/main.go

FROM scratch
COPY --from=build /build/k8s-secret-copier /usr/local/bin/k8s-secret-copier
ENTRYPOINT ["/usr/local/bin/k8s-secret-copier"]
