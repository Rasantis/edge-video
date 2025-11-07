package camera

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/T3-Labs/edge-video/internal/metadata"
	"github.com/T3-Labs/edge-video/internal/storage"
	"github.com/T3-Labs/edge-video/pkg/mq"
	"github.com/T3-Labs/edge-video/pkg/util"
	"github.com/go-redis/redis/v8"
	"github.com/streadway/amqp"
)

type Config struct {
	ID  string
	URL string
}

type Capture struct {
	ctx           context.Context
	config        Config
	interval      time.Duration
	compressor    *util.Compressor
	publisher     mq.Publisher
	redisStore    *storage.RedisStore
	metaPublisher *metadata.Publisher

	// Stream RTSP otimizado (conexão persistente)
	stream *RTSPStream
}

func NewCapture(
	ctx context.Context,
	config Config,
	interval time.Duration,
	compressor *util.Compressor,
	publisher mq.Publisher,
	redisStore *storage.RedisStore,
	metaPublisher *metadata.Publisher,
) *Capture {
	// Calcular FPS alvo a partir do intervalo configurado
	// interval = 1s / targetFPS, então targetFPS = 1s / interval
	targetFPS := int(float64(time.Second) / float64(interval))
	if targetFPS <= 0 {
		targetFPS = 18 // Fallback para 18 fps
	}

	// Criar stream RTSP otimizado com conexão persistente
	stream, err := NewRTSPStream(ctx, RTSPStreamConfig{
		CameraID:          config.ID,
		URL:               config.URL,
		FrameBufferSize:   30,                // Buffer de 30 frames (~1-2s)
		ReconnectInterval: 5 * time.Second,   // Reconectar após 5s em caso de falha
		JPEGQuality:       5,                 // Qualidade 5 (balanço qualidade/tamanho)
		TargetFPS:         targetFPS,         // FPS calculado do config (ex: 18)
	})

	if err != nil {
		log.Printf("[%s] erro ao criar stream RTSP: %v", config.ID, err)
		// Retornar Capture sem stream - vai falhar graciosamente
		return &Capture{
			ctx:           ctx,
			config:        config,
			interval:      interval,
			compressor:    compressor,
			publisher:     publisher,
			redisStore:    redisStore,
			metaPublisher: metaPublisher,
			stream:        nil,
		}
	}

	return &Capture{
		ctx:           ctx,
		config:        config,
		interval:      interval,
		compressor:    compressor,
		publisher:     publisher,
		redisStore:    redisStore,
		metaPublisher: metaPublisher,
		stream:        stream,
	}
}

func (c *Capture) Start() {
	// Verificar se stream foi criado com sucesso
	if c.stream == nil {
		log.Printf("[%s] erro: stream RTSP não foi inicializado", c.config.ID)
		return
	}

	// Iniciar stream RTSP (background com reconexão automática)
	if err := c.stream.Start(); err != nil {
		log.Printf("[%s] erro ao iniciar stream: %v", c.config.ID, err)
		return
	}

	log.Printf("[%s] stream RTSP iniciado com sucesso", c.config.ID)

	// Goroutine para publicar frames no intervalo configurado
	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()

		for {
			select {
			case <-c.ctx.Done():
				log.Printf("[%s] parando captura", c.config.ID)
				if c.stream != nil {
					c.stream.Close()
				}
				return
			case <-ticker.C:
				c.captureAndPublish()
			}
		}
	}()
}

func (c *Capture) captureAndPublish() {
	// Verificar se stream está disponível
	if c.stream == nil {
		log.Printf("[%s] stream não disponível", c.config.ID)
		return
	}

	// Obter frame do stream (com timeout de 2s)
	// Timeout de 2s é adequado pois o stream já está buffering frames
	frameData, err := c.stream.GetFrame(2 * time.Second)
	if err != nil {
		// Não logar timeout como erro - é esperado ocasionalmente
		if err.Error() != "timeout aguardando frame" {
			log.Printf("[%s] erro ao obter frame: %v", c.config.ID, err)
		}
		return
	}

	if len(frameData) == 0 {
		log.Printf("[%s] frame vazio obtido", c.config.ID)
		return
	}

	log.Printf("[%s] obtido frame do stream (%d bytes)", c.config.ID, len(frameData))

	// Publicação principal (síncrona ou assíncrona, dependendo da implementação do publisher)
	err = c.publisher.Publish(c.ctx, c.config.ID, frameData)
	if err != nil {
		log.Printf("erro ao publicar frame da câmera %s: %v", c.config.ID, err)
	}

	// Operações assíncronas de Redis e Metadados
	if c.redisStore.Enabled() {
		go func() {
			timestamp := time.Now()
			// TODO: Obter width/height do frame real se possível
			width, height := 1280, 720

			key, err := c.redisStore.SaveFrame(c.ctx, c.config.ID, timestamp, frameData)
			if err != nil {
				// Tratar erro de conexão com Redis (ex: logar)
				if errors.Is(err, redis.ErrClosed) {
					log.Printf("redis store error (connection closed): %v", err)
				} else {
					log.Printf("redis store error: %v", err)
				}
				return
			}

			if c.metaPublisher.Enabled() {
				err = c.metaPublisher.PublishMetadata(c.config.ID, timestamp, key, width, height, len(frameData), "jpeg")
				if err != nil {
					// Tratar erro de conexão com RabbitMQ (ex: logar)
					if amqpErr, ok := err.(*amqp.Error); ok && amqpErr.Code == amqp.ChannelError {
						log.Printf("metadata publish error (channel closed): %v", err)
					} else {
						log.Printf("metadata publish error: %v", err)
					}
				}
			}
		}()
	}
}