package camera

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

// RTSPStream gerencia um stream RTSP contínuo via FFmpeg otimizado
// Mantém um único processo FFmpeg persistente por câmera (não um por frame)
type RTSPStream struct {
	cameraID          string
	url               string
	ctx               context.Context
	cancel            context.CancelFunc
	cmd               *exec.Cmd
	stdout            io.ReadCloser
	mu                sync.RWMutex
	frameBuffer       chan []byte
	running           bool
	reconnectInterval time.Duration
	frameBufferSize   int
	jpegQuality       int
}

// RTSPStreamConfig configuração do stream RTSP
type RTSPStreamConfig struct {
	CameraID          string
	URL               string
	FrameBufferSize   int           // Tamanho do buffer de frames (padrão: 30)
	ReconnectInterval time.Duration // Intervalo de reconexão (padrão: 5s)
	JPEGQuality       int           // Qualidade JPEG 2-31 (menor = melhor, padrão: 5)
}

// NewRTSPStream cria um novo stream RTSP otimizado
func NewRTSPStream(ctx context.Context, config RTSPStreamConfig) (*RTSPStream, error) {
	if config.FrameBufferSize <= 0 {
		config.FrameBufferSize = 30
	}
	if config.ReconnectInterval <= 0 {
		config.ReconnectInterval = 5 * time.Second
	}
	if config.JPEGQuality <= 0 || config.JPEGQuality > 31 {
		config.JPEGQuality = 5
	}

	streamCtx, cancel := context.WithCancel(ctx)

	stream := &RTSPStream{
		cameraID:          config.CameraID,
		url:               config.URL,
		ctx:               streamCtx,
		cancel:            cancel,
		frameBuffer:       make(chan []byte, config.FrameBufferSize),
		reconnectInterval: config.ReconnectInterval,
		frameBufferSize:   config.FrameBufferSize,
		jpegQuality:       config.JPEGQuality,
	}

	return stream, nil
}

// Start inicia a captura contínua
func (s *RTSPStream) Start() error {
	go s.streamLoop()
	return nil
}

// streamLoop loop principal com reconexão automática
func (s *RTSPStream) streamLoop() {
	log.Printf("[%s] iniciando stream RTSP otimizado", s.cameraID)

	for {
		select {
		case <-s.ctx.Done():
			log.Printf("[%s] encerrando stream RTSP", s.cameraID)
			s.stop()
			return
		default:
			if err := s.startFFmpegStream(); err != nil {
				log.Printf("[%s] erro no stream: %v, reconectando em %v",
					s.cameraID, err, s.reconnectInterval)
				s.stop()

				// Aguarda antes de reconectar
				select {
				case <-time.After(s.reconnectInterval):
				case <-s.ctx.Done():
					return
				}
				continue
			}
		}
	}
}

// startFFmpegStream inicia processo FFmpeg em modo streaming contínuo
func (s *RTSPStream) startFFmpegStream() error {
	s.mu.Lock()

	log.Printf("[%s] iniciando processo FFmpeg streaming", s.cameraID)

	// FFmpeg em modo streaming: captura contínua de frames JPEG
	// -re: lê o input na taxa de frames nativa
	// -fflags nobuffer: minimiza buffering para menor latência
	// -flags low_delay: reduz delay
	// -vsync 0: não sincroniza frames (mantém taxa original)
	s.cmd = exec.CommandContext(
		s.ctx,
		"ffmpeg",
		"-rtsp_transport", "tcp",          // TCP é mais confiável que UDP
		"-fflags", "nobuffer",             // Minimiza buffering
		"-flags", "low_delay",             // Baixa latência
		"-i", s.url,                       // URL RTSP
		"-vsync", "0",                     // Não ajusta framerate
		"-f", "image2pipe",                // Output como pipe de imagens
		"-vcodec", "mjpeg",                // Codec MJPEG (JPEG stream)
		"-q:v", fmt.Sprintf("%d", s.jpegQuality), // Qualidade (2-31, menor=melhor)
		"-",                               // Output para stdout
	)

	// Capturar stdout para ler frames
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("erro ao criar pipe stdout: %w", err)
	}
	s.stdout = stdout

	// Capturar stderr para logging (não bloqueia)
	stderrPipe, err := s.cmd.StderrPipe()
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("erro ao criar pipe stderr: %w", err)
	}

	// Iniciar processo
	if err := s.cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("erro ao iniciar FFmpeg: %w", err)
	}

	s.running = true
	s.mu.Unlock()

	// Goroutine para logar stderr (não bloqueia captura)
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			// Log apenas erros relevantes, não todo o output do FFmpeg
			if bytes.Contains([]byte(line), []byte("error")) ||
			   bytes.Contains([]byte(line), []byte("Error")) {
				log.Printf("[%s] FFmpeg: %s", s.cameraID, line)
			}
		}
	}()

	// Goroutine para ler frames continuamente
	go s.readFrames(stdout)

	// Aguardar término do processo
	err = s.cmd.Wait()
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	if err != nil && s.ctx.Err() == nil {
		return fmt.Errorf("processo FFmpeg terminou: %w", err)
	}

	return nil
}

// readFrames lê frames JPEG continuamente do stdout do FFmpeg
func (s *RTSPStream) readFrames(stdout io.ReadCloser) {
	reader := bufio.NewReaderSize(stdout, 256*1024) // Buffer de 256KB

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		// Ler próximo frame JPEG
		// JPEG começa com FF D8 e termina com FF D9
		frame, err := s.readJPEGFrame(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("[%s] erro ao ler frame: %v", s.cameraID, err)
			}
			return
		}

		if len(frame) == 0 {
			continue
		}

		// Enviar frame para buffer (não bloqueia)
		select {
		case s.frameBuffer <- frame:
			// Frame enviado com sucesso
		default:
			// Buffer cheio: remove frame mais antigo e adiciona novo
			select {
			case <-s.frameBuffer:
			default:
			}
			select {
			case s.frameBuffer <- frame:
			default:
			}
		}
	}
}

// readJPEGFrame lê um frame JPEG completo do reader
// JPEG: inicia com FF D8, termina com FF D9
func (s *RTSPStream) readJPEGFrame(reader *bufio.Reader) ([]byte, error) {
	// Procurar início do JPEG (FF D8)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 0xFF {
			b2, err := reader.ReadByte()
			if err != nil {
				return nil, err
			}
			if b2 == 0xD8 {
				// Encontrou início do JPEG
				frame := []byte{0xFF, 0xD8}

				// Ler até encontrar fim do JPEG (FF D9)
				for {
					b, err := reader.ReadByte()
					if err != nil {
						return nil, err
					}
					frame = append(frame, b)

					if b == 0xFF {
						b2, err := reader.ReadByte()
						if err != nil {
							return nil, err
						}
						frame = append(frame, b2)

						if b2 == 0xD9 {
							// Encontrou fim do JPEG
							return frame, nil
						}
					}

					// Limite de segurança: frame não deve exceder 10MB
					if len(frame) > 10*1024*1024 {
						return nil, fmt.Errorf("frame muito grande: %d bytes", len(frame))
					}
				}
			}
		}
	}
}

// GetFrame obtém próximo frame disponível (bloqueante)
func (s *RTSPStream) GetFrame(timeout time.Duration) ([]byte, error) {
	select {
	case frame := <-s.frameBuffer:
		if len(frame) == 0 {
			return nil, fmt.Errorf("frame vazio")
		}
		return frame, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout aguardando frame")
	case <-s.ctx.Done():
		return nil, fmt.Errorf("contexto cancelado")
	}
}

// TryGetFrame tenta obter frame sem bloquear (não-bloqueante)
func (s *RTSPStream) TryGetFrame() ([]byte, error) {
	select {
	case frame := <-s.frameBuffer:
		if len(frame) == 0 {
			return nil, fmt.Errorf("frame vazio")
		}
		return frame, nil
	default:
		return nil, fmt.Errorf("nenhum frame disponível")
	}
}

// IsRunning retorna se o stream está ativo
func (s *RTSPStream) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// stop para o processo FFmpeg
func (s *RTSPStream) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil && s.cmd.Process != nil {
		log.Printf("[%s] parando processo FFmpeg", s.cameraID)
		s.cmd.Process.Kill()
		s.cmd = nil
	}
	s.running = false
}

// Close fecha o stream e libera recursos
func (s *RTSPStream) Close() error {
	log.Printf("[%s] fechando stream RTSP", s.cameraID)
	s.cancel()
	s.stop()

	// Aguardar um pouco para limpeza
	time.Sleep(100 * time.Millisecond)

	// Fechar canal de frames
	s.mu.Lock()
	if s.frameBuffer != nil {
		close(s.frameBuffer)
		s.frameBuffer = nil
	}
	s.mu.Unlock()

	return nil
}
