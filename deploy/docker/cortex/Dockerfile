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
RUN CGO_ENABLED=0 GOOS=linux go build \
  -ldflags "-s -w" \
  -o /out/nerve-migrate ./cmd/nerve-migrate

FROM alpine:3.20

ARG RUNTIME_VERSION=dev
ARG MCP_CONTRACT_HASH=unknown
ARG CORE_SCHEMA_HASH=unknown
ARG CORE_SCHEMA_MIN_REQUIRED=unknown
ARG CORE_SCHEMA_MAX_SUPPORTED=unknown
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

LABEL io.nerve.runtime.version="${RUNTIME_VERSION}" \
      io.nerve.runtime.mcp-contract-sha256="${MCP_CONTRACT_HASH}" \
      io.nerve.runtime.core-schema-sha256="${CORE_SCHEMA_HASH}" \
      io.nerve.runtime.core-schema-min-required="${CORE_SCHEMA_MIN_REQUIRED}" \
      io.nerve.runtime.core-schema-max-supported="${CORE_SCHEMA_MAX_SUPPORTED}" \
      io.nerve.runtime.build-commit="${BUILD_COMMIT}" \
      io.nerve.runtime.build-time="${BUILD_TIME}"

RUN addgroup -S neuralmail && adduser -S neuralmail -G neuralmail

USER neuralmail
WORKDIR /app

COPY --from=build /out/nerve-runtime /app/nerve-runtime
COPY --from=build /out/nerve-migrate /app/nerve-migrate
RUN ln -s /app/nerve-runtime /app/neuralmaild
COPY configs /app/configs
COPY internal/store/migrations /app/internal/store/migrations

EXPOSE 8088
ENTRYPOINT ["/app/nerve-runtime"]
CMD ["serve"]
