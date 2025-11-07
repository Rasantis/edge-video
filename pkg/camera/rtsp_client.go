package camera

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/pion/rtp"
)

// RTSPClient gerencia uma conexão RTSP persistente para captura contínua de frames
type RTSPClient struct {
	cameraID    string
	url         string
	ctx         context.Context
	cancel      context.CancelFunc
	client      *gortsplib.Client
	mu          sync.RWMutex
	frameBuffer chan []byte
	connected   bool

	// Decoders para H264/H265
	h264Decoder *rtph264.Decoder
	h265Decoder *rtph265.Decoder

	// Configurações
	reconnectInterval time.Duration
	frameBufferSize   int
	jpegQuality       int
}

// RTSPClientConfig configurações do cliente RTSP
type RTSPClientConfig struct {
	CameraID          string
	URL               string
	FrameBufferSize   int           // Tamanho do buffer de frames (padrão: 30)
	ReconnectInterval time.Duration // Intervalo de reconexão (padrão: 5s)
	JPEGQuality       int           // Qualidade JPEG 1-100 (padrão: 80)
}

// NewRTSPClient cria um novo cliente RTSP com conexão persistente
func NewRTSPClient(ctx context.Context, config RTSPClientConfig) (*RTSPClient, error) {
	if config.FrameBufferSize <= 0 {
		config.FrameBufferSize = 30
	}
	if config.ReconnectInterval <= 0 {
		config.ReconnectInterval = 5 * time.Second
	}
	if config.JPEGQuality <= 0 || config.JPEGQuality > 100 {
		config.JPEGQuality = 80
	}

	clientCtx, cancel := context.WithCancel(ctx)

	client := &RTSPClient{
		cameraID:          config.CameraID,
		url:               config.URL,
		ctx:               clientCtx,
		cancel:            cancel,
		frameBuffer:       make(chan []byte, config.FrameBufferSize),
		reconnectInterval: config.ReconnectInterval,
		frameBufferSize:   config.FrameBufferSize,
		jpegQuality:       config.JPEGQuality,
	}

	return client, nil
}

// Start inicia a captura contínua de frames em background
func (r *RTSPClient) Start() error {
	go r.captureLoop()
	return nil
}

// captureLoop loop principal de captura com reconexão automática
func (r *RTSPClient) captureLoop() {
	log.Printf("[%s] iniciando captura RTSP", r.cameraID)

	for {
		select {
		case <-r.ctx.Done():
			log.Printf("[%s] encerrando captura RTSP", r.cameraID)
			return
		default:
			if err := r.connect(); err != nil {
				log.Printf("[%s] erro ao conectar: %v, tentando novamente em %v",
					r.cameraID, err, r.reconnectInterval)
				time.Sleep(r.reconnectInterval)
				continue
			}

			// Captura contínua enquanto conectado
			if err := r.captureFrames(); err != nil {
				log.Printf("[%s] erro na captura: %v, reconectando...", r.cameraID, err)
				r.disconnect()
				time.Sleep(r.reconnectInterval)
				continue
			}
		}
	}
}

// connect estabelece conexão RTSP e configura decoders
func (r *RTSPClient) connect() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.connected {
		return nil
	}

	log.Printf("[%s] conectando a %s", r.cameraID, r.url)

	// Criar cliente RTSP
	c := &gortsplib.Client{
		Transport: func() *gortsplib.Transport {
			v := gortsplib.TransportTCP
			return &v
		}(),
	}

	// Parse URL
	u, err := base.ParseURL(r.url)
	if err != nil {
		return fmt.Errorf("URL inválida: %w", err)
	}

	// Conectar ao servidor RTSP
	if err := c.Start(u.Scheme, u.Host); err != nil {
		return fmt.Errorf("falha ao conectar: %w", err)
	}

	// Obter lista de tracks (streams de vídeo/áudio)
	session, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return fmt.Errorf("falha no DESCRIBE: %w", err)
	}

	// Encontrar track de vídeo
	var videoFormat format.Format
	videoMedia := session.FindFormat(&videoFormat)
	if videoMedia == nil {
		c.Close()
		return fmt.Errorf("nenhum track de vídeo encontrado")
	}

	// Configurar decoder baseado no formato
	switch videoFormat.(type) {
	case *format.H264:
		r.h264Decoder = &rtph264.Decoder{}
		if err := r.h264Decoder.Init(); err != nil {
			c.Close()
			return fmt.Errorf("falha ao inicializar decoder H264: %w", err)
		}
		log.Printf("[%s] usando codec H264", r.cameraID)

	case *format.H265:
		r.h265Decoder = &rtph265.Decoder{}
		if err := r.h265Decoder.Init(); err != nil {
			c.Close()
			return fmt.Errorf("falha ao inicializar decoder H265: %w", err)
		}
		log.Printf("[%s] usando codec H265", r.cameraID)

	default:
		c.Close()
		return fmt.Errorf("formato de vídeo não suportado: %T", videoFormat)
	}

	// Setup track para receber dados
	if _, err := c.Setup(session.BaseURL, videoMedia, 0, 0); err != nil {
		c.Close()
		return fmt.Errorf("falha no SETUP: %w", err)
	}

	// Iniciar streaming
	if _, err := c.Play(nil); err != nil {
		c.Close()
		return fmt.Errorf("falha no PLAY: %w", err)
	}

	r.client = c
	r.connected = true
	log.Printf("[%s] conectado com sucesso", r.cameraID)

	return nil
}

// disconnect fecha a conexão RTSP
func (r *RTSPClient) disconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.client != nil {
		r.client.Close()
		r.client = nil
	}
	r.connected = false
	r.h264Decoder = nil
	r.h265Decoder = nil

	log.Printf("[%s] desconectado", r.cameraID)
}

// captureFrames captura frames continuamente do stream RTSP
func (r *RTSPClient) captureFrames() error {
	r.mu.RLock()
	client := r.client
	h264Dec := r.h264Decoder
	h265Dec := r.h265Decoder
	r.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("cliente não conectado")
	}

	// Buffer para acumular NAL units (H264/H265)
	var nalus [][]byte
	var lastKeyFrame time.Time

	for {
		select {
		case <-r.ctx.Done():
			return nil
		default:
		}

		// Ler pacote RTP do stream
		pkt := &rtp.Packet{}
		_, err := client.ReadPacket(pkt)
		if err != nil {
			return fmt.Errorf("erro ao ler pacote: %w", err)
		}

		// Decodificar baseado no codec
		if h264Dec != nil {
			// Decodificar H264
			au, err := h264Dec.Decode(pkt)
			if err != nil {
				// Alguns erros são esperados durante streaming, ignorar
				continue
			}

			// Access Unit completo (frame completo)
			if au != nil {
				nalus = au

				// Processar apenas keyframes periódicos para economizar CPU
				// Para 18 FPS, processar ~1 frame por segundo é suficiente
				now := time.Now()
				if now.Sub(lastKeyFrame) < time.Second {
					continue
				}
				lastKeyFrame = now

				// Converter para JPEG
				if jpegData, err := r.nalusToJPEG(nalus); err == nil {
					select {
					case r.frameBuffer <- jpegData:
					default:
						// Buffer cheio, descartar frame mais antigo
						select {
						case <-r.frameBuffer:
						default:
						}
						r.frameBuffer <- jpegData
					}
				}
				nalus = nil
			}

		} else if h265Dec != nil {
			// Decodificar H265
			au, err := h265Dec.Decode(pkt)
			if err != nil {
				continue
			}

			if au != nil {
				nalus = au

				now := time.Now()
				if now.Sub(lastKeyFrame) < time.Second {
					continue
				}
				lastKeyFrame = now

				if jpegData, err := r.nalusToJPEG(nalus); err == nil {
					select {
					case r.frameBuffer <- jpegData:
					default:
						select {
						case <-r.frameBuffer:
						default:
						}
						r.frameBuffer <- jpegData
					}
				}
				nalus = nil
			}
		}
	}
}

// nalusToJPEG converte NAL units para imagem JPEG
// Nota: Esta é uma implementação simplificada que usa a primeira NAL unit decodificável
// Para produção, considere usar bibliotecas de decodificação mais robustas como libav/ffmpeg via CGO
func (r *RTSPClient) nalusToJPEG(nalus [][]byte) ([]byte, error) {
	// Por enquanto, retornamos os NAL units como estão
	// A decodificação completa H264->YUV->JPEG requer bibliotecas mais complexas
	// Esta é uma implementação temporária que retorna o frame compactado

	// TODO: Implementar decodificação completa H264/H265 -> Image -> JPEG
	// Por enquanto, vamos simular retornando dados vazios para compilação
	// A implementação real precisaria de uma biblioteca como github.com/nareix/joy4
	// ou integração com ffmpeg via CGO

	return nil, fmt.Errorf("decodificação JPEG não implementada ainda")
}

// GetFrame obtém o próximo frame disponível (bloqueante até timeout)
func (r *RTSPClient) GetFrame(timeout time.Duration) ([]byte, error) {
	select {
	case frame := <-r.frameBuffer:
		return frame, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout aguardando frame")
	case <-r.ctx.Done():
		return nil, fmt.Errorf("contexto cancelado")
	}
}

// IsConnected retorna se o cliente está conectado
func (r *RTSPClient) IsConnected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.connected
}

// Close fecha o cliente RTSP e libera recursos
func (r *RTSPClient) Close() error {
	r.cancel()
	r.disconnect()
	close(r.frameBuffer)
	return nil
}
