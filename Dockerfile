# syntax=docker/dockerfile:1

FROM golang:1.16-alpine

WORKDIR /app

COPY go.mod ./
COPY .env ./
COPY go.sum ./
RUN go mod download

COPY *.go ./

RUN go build -o /message-relay

CMD [ "/message-relay" ]