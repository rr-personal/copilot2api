FROM golang:1.26-alpine AS build
ARG COMMIT_SHA=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w -X main.commit=${COMMIT_SHA}" -o /copilot2api .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /copilot2api /copilot2api
ENV HOME=/root
ENV COPILOT2API_HOST=0.0.0.0
ENV COPILOT2API_PORT=7777
ENV COPILOT2API_CONTROL_PORT=7778
EXPOSE 7777 7778
ENTRYPOINT ["/copilot2api"]
