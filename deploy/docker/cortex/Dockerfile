FROM golang:1.23-alpine AS build

WORKDIR /src

ARG RUNTIME_VERSION=dev
ARG MCP_CONTRACT_HASH=unknown
ARG CORE_SCHEMA_HASH=unknown
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
  -ldflags "-s -w \
    -X neuralmail/internal/release.RuntimeVersion=${RUNTIME_VERSION} \
    -X neuralmail/internal/release.MCPContractHash=${MCP_CONTRACT_HASH} \
    -X neuralmail/internal/release.CoreSchemaHash=${CORE_SCHEMA_HASH} \
    -X neuralmail/internal/release.BuildCommit=${BUILD_COMMIT} \
    -X neuralmail/internal/release.BuildTime=${BUILD_TIME}" \
  -o /out/nerve-runtime ./cmd/neuralmaild

FROM alpine:3.20

RUN addgroup -S neuralmail && adduser -S neuralmail -G neuralmail

USER neuralmail
WORKDIR /app

COPY --from=build /out/nerve-runtime /app/nerve-runtime
RUN ln -s /app/nerve-runtime /app/neuralmaild
COPY configs /app/configs
COPY internal/store/migrations /app/internal/store/migrations

EXPOSE 8088
ENTRYPOINT ["/app/nerve-runtime"]
CMD ["serve"]
