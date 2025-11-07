# Stage 1: Build
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copiar go.mod e go.sum
COPY go.mod go.sum ./

# Copiar o código fonte para resolver dependências locais
COPY . .

# Baixar e sincronizar dependências
RUN go mod download
RUN go mod tidy

# Construir a aplicação sem CGO
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /camera-collector ./cmd/edge-video

# Stage 2: Final image
FROM alpine:latest

# Instala FFmpeg para captura de streams RTSP
RUN apk add --no-cache ffmpeg

WORKDIR /app

# Copiar o binário construído
COPY --from=builder /camera-collector .

# CORREÇÃO: Copiar config.yaml padrão para a imagem
# Pode ser sobrescrito via volume se necessário
COPY --from=builder /app/config.yaml ./config.yaml

# Comando para iniciar a aplicação
CMD ["./camera-collector"]
