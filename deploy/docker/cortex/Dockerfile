FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/neuralmaild ./cmd/neuralmaild

FROM alpine:3.20

RUN addgroup -S neuralmail && adduser -S neuralmail -G neuralmail

USER neuralmail
WORKDIR /app

COPY --from=build /out/neuralmaild /app/neuralmaild
COPY configs /app/configs
COPY internal/store/migrations /app/internal/store/migrations

EXPOSE 8088
ENTRYPOINT ["/app/neuralmaild", "serve"]
