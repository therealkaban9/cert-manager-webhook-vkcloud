FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /webhook .
FROM gcr.io/distroless/static-debian12:nonroot
LABEL version="cert-manager-webhook-vkcloud:0.3.0"
COPY --from=build /webhook /webhook
USER 65532:65532
ENTRYPOINT ["/webhook"]
