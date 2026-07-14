FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /bunny-course-backend .

FROM alpine:3.19

RUN apk add --no-cache ffmpeg ca-certificates

WORKDIR /root/

COPY --from=builder /bunny-course-backend .

COPY --from=builder /app/database ./database

RUN mkdir -p uploads public/output

EXPOSE 5000

CMD ["./bunny-course-backend"]