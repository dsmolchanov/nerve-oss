FROM golang:1.25.13-alpine AS build

WORKDIR /src

ARG RUNTIME_MANIFEST_VERSION=1
ARG ARTIFACT_ROLE
ARG CORE_SCHEMA_MIN_REQUIRED
ARG CORE_SCHEMA_MAX_SUPPORTED
ARG RUNTIME_VERSION=dev
ARG MCP_CONTRACT_HASH=unknown
ARG CORE_SCHEMA_HASH=unknown
ARG OUTBOUND_POLICY_VERSION=unknown
ARG OUTBOUND_POLICY_SHA256=unknown
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

COPY go.mod ./
RUN apk add --no-cache bash
RUN go mod download

COPY . .
RUN bash scripts/release/runtime_schema_window.sh > /tmp/runtime-window && \
  if [ "$RUNTIME_MANIFEST_VERSION" = 2 ]; then \
    test "$(cat /tmp/runtime-window)" = "2:${CORE_SCHEMA_MIN_REQUIRED}:${CORE_SCHEMA_MAX_SUPPORTED}"; \
  fi
RUN CGO_ENABLED=0 GOOS=linux go build \
  -ldflags "-s -w \
    -X neuralmail/internal/startup.CompiledSchemaWindow=$(cat /tmp/runtime-window) \
    -X neuralmail/internal/release.RuntimeVersion=${RUNTIME_VERSION} \
    -X neuralmail/internal/release.MCPContractHash=${MCP_CONTRACT_HASH} \
    -X neuralmail/internal/release.CoreSchemaHash=${CORE_SCHEMA_HASH} \
    -X neuralmail/internal/release.OutboundPolicyVersion=${OUTBOUND_POLICY_VERSION} \
    -X neuralmail/internal/release.OutboundPolicySHA256=${OUTBOUND_POLICY_SHA256} \
    -X neuralmail/internal/release.BuildCommit=${BUILD_COMMIT} \
  -X neuralmail/internal/release.BuildTime=${BUILD_TIME}" \
  -o /out/nerve-runtime ./cmd/neuralmaild
RUN CGO_ENABLED=0 GOOS=linux go build \
  -ldflags "-s -w -X neuralmail/internal/startup.CompiledSchemaWindow=$(cat /tmp/runtime-window)" \
  -o /out/nerve-migrate ./cmd/nerve-migrate
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w -X neuralmail/internal/startup.CompiledSchemaWindow=$(cat /tmp/runtime-window)" -o /out/neuralmail ./cmd/neuralmail

RUN GIT_COMMIT="$BUILD_COMMIT" bash scripts/release/generate_runtime_manifest.sh "$RUNTIME_VERSION" /out/runtime-manifest.json

FROM alpine:3.20

ARG RUNTIME_VERSION=dev
ARG MCP_CONTRACT_HASH=unknown
ARG CORE_SCHEMA_HASH=unknown
ARG CORE_SCHEMA_MIN_REQUIRED=unknown
ARG CORE_SCHEMA_MAX_SUPPORTED=unknown
ARG OUTBOUND_POLICY_VERSION=unknown
ARG OUTBOUND_POLICY_SHA256=unknown
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

LABEL io.nerve.runtime.version="${RUNTIME_VERSION}" \
      io.nerve.runtime.mcp-contract-sha256="${MCP_CONTRACT_HASH}" \
      io.nerve.runtime.core-schema-sha256="${CORE_SCHEMA_HASH}" \
      io.nerve.runtime.core-schema-min-required="${CORE_SCHEMA_MIN_REQUIRED}" \
      io.nerve.runtime.core-schema-max-supported="${CORE_SCHEMA_MAX_SUPPORTED}" \
      io.nerve.runtime.outbound-policy-version="${OUTBOUND_POLICY_VERSION}" \
      io.nerve.runtime.outbound-policy-sha256="${OUTBOUND_POLICY_SHA256}" \
      io.nerve.runtime.build-commit="${BUILD_COMMIT}" \
      io.nerve.runtime.build-time="${BUILD_TIME}"

RUN addgroup -S neuralmail && adduser -S neuralmail -G neuralmail

USER neuralmail
WORKDIR /app

COPY --from=build /out/nerve-runtime /app/nerve-runtime
COPY --from=build /out/runtime-manifest.json /app/runtime-manifest.json
COPY --from=build /out/nerve-migrate /app/nerve-migrate
COPY --from=build /out/neuralmail /app/neuralmail
RUN ln -s /app/nerve-runtime /app/neuralmaild
COPY configs /app/configs
COPY internal/store/migrations /app/internal/store/migrations

ENV NERVE_HTTP_ADDR=0.0.0.0:8088

EXPOSE 8088
ENTRYPOINT ["/app/nerve-runtime"]
CMD ["serve"]
