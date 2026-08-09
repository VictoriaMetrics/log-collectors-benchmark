FROM golang:1.26-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY log-verifier/ ./log-verifier/
RUN CGO_ENABLED=0 GOOS=linux go build -o /log-verifier ./log-verifier

FROM scratch
COPY --from=build /log-verifier /log-verifier
ENTRYPOINT ["/log-verifier"]
