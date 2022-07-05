FROM golang:1 as dev
WORKDIR /root
RUN go install github.com/cortesi/modd/cmd/modd@latest
COPY go.* .
RUN go mod download
COPY . .
CMD modd -f modd.conf

FROM golang:1 as build
WORKDIR /root
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o serverbin ./cmd/server/server.go

FROM gcr.io/distroless/static as prod
COPY --from=grushor/transactions /root/serverbin /serverbin
CMD ["/serverbin"]
