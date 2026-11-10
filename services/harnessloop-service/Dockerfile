FROM golang:1.22-alpine AS builder

RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories || true

WORKDIR /build

ENV GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum* ./
RUN go mod download || true

COPY pkg ./pkg
COPY proto ./proto
COPY services ./services

RUN CGO_ENABLED=0 GOOS=linux go build -o harnessloop-service ./services/harnessloop-service/cmd

FROM golang:1.22-alpine

RUN apk add --no-cache ca-certificates tzdata || true

WORKDIR /app
COPY --from=builder /build/harnessloop-service /app/harnessloop-service

EXPOSE 50008

ENTRYPOINT ["/app/harnessloop-service"]