FROM golang:alpine
WORKDIR /log-verifier
COPY go.mod go.sum ./
RUN go mod download
COPY *.go .
RUN CGO_ENABLED=0 GOOS=linux go build -o log-verifier .
ENTRYPOINT ["./log-verifier"]
