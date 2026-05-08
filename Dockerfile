FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/murmur ./cmd/murmur

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/murmur /murmur
EXPOSE 4444
ENTRYPOINT ["/murmur"]
