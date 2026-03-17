FROM golang:alpine
WORKDIR /log-generator
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -o log-generator ./main.go
ENTRYPOINT ["./log-generator"]
