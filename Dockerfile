# Один Dockerfile на все Go-сервисы. Какой собрать — задаёт аргумент SERVICE:
#   docker build --build-arg SERVICE=services/order-service -t order-service .

# ---- сборка ----
FROM golang:1.27-alpine AS build
WORKDIR /src

# сначала только зависимости — этот слой кэшируется между сборками
COPY go.mod go.sum ./
RUN go mod download

COPY pkg ./pkg
COPY services ./services
COPY mocks ./mocks

ARG SERVICE
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./${SERVICE}

# ---- итоговый образ: только бинарник ----
FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
