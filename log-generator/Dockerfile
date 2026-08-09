FROM golang:1.26-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY log-generator/ ./log-generator/
RUN CGO_ENABLED=0 GOOS=linux go build -o /log-generator ./log-generator

FROM scratch
COPY --from=build /log-generator /log-generator
ENTRYPOINT ["/log-generator"]
