FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X github.com/lykuroai/Native-LLM-Platform/gwcore.Version=${VERSION}" \
      -o /out/private-gateway ./cmd/private-gateway/

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/private-gateway /usr/local/bin/private-gateway
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/private-gateway"]
CMD ["serve", "-config", "/etc/lykuro/gateway/gateway.yaml"]
