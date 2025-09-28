package tts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Stream 建立与 CosyVoice 的流式会话并返回会话对象。
func (d *cosyVoiceDriver) Stream(ctx context.Context, req SpeechStreamRequest) (SpeechStreamSession, error) {
	if d == nil || !d.Enabled() {
		return nil, ErrDisabled
	}
	if strings.TrimSpace(d.apiKey) == "" {
		return nil, ErrDisabled
	}

	voiceID := strings.TrimSpace(req.VoiceID)
	if voiceID == "" {
		voiceID = d.defaultVoice
	}
	if voiceID == "" {
		return nil, errors.New("tts: cosyvoice default voice not configured")
	}

	format := strings.TrimSpace(req.Format)
	if format == "" && req.ResolvedVoice != nil {
		if v := strings.TrimSpace(req.ResolvedVoice.Format); v != "" {
			format = v
		}
	}
	if format == "" {
		format = d.format
	}
	if format == "" {
		format = "mp3"
	}

	sampleRate := d.sampleRate
	if req.ResolvedVoice != nil && req.ResolvedVoice.SampleRate > 0 {
		sampleRate = req.ResolvedVoice.SampleRate
	}
	if sampleRate <= 0 {
		sampleRate = 22050
	}

	model := d.model
	if req.ResolvedVoice != nil {
		if m := strings.TrimSpace(req.ResolvedVoice.Model); m != "" {
			model = m
		}
	}
	if model == "" {
		model = "cosyvoice-v3"
	}

	speed := req.Speed
	if speed <= 0 {
		speed = 1.0
	}
	if speed < 0.5 {
		speed = 0.5
	}
	if speed > 1.6 {
		speed = 1.6
	}

	pitch := req.Pitch
	if pitch <= 0 {
		pitch = 1.0
	}
	if pitch < 0.7 {
		pitch = 0.7
	}
	if pitch > 1.4 {
		pitch = 1.4
	}

	header := http.Header{}
	header.Set("Authorization", "bearer "+d.apiKey)
	header.Set("User-Agent", "auralis-cosyvoice-client/1.0")
	if d.workspace != "" {
		header.Set("X-DashScope-WorkSpace", d.workspace)
	}
	if d.dataInspection != "" {
		header.Set("X-DashScope-DataInspection", d.dataInspection)
	}

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 8 * time.Second,
	}

	conn, resp, err := dialer.DialContext(ctx, d.endpoint, header)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if len(body) > 0 {
				return nil, fmt.Errorf("tts: cosyvoice stream connect failed: %v (%s)", err, strings.TrimSpace(string(body)))
			}
		}
		return nil, fmt.Errorf("tts: cosyvoice stream connect failed: %w", err)
	}

	taskID := uuid.NewString()

	params := map[string]any{
		"header": map[string]any{
			"action":    "run-task",
			"task_id":   taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"task_group": "audio",
			"task":       "tts",
			"function":   "SpeechSynthesizer",
			"model":      model,
			"parameters": map[string]any{
				"text_type":   "PlainText",
				"voice":       voiceID,
				"format":      strings.ToLower(format),
				"sample_rate": sampleRate,
				"volume":      d.volume,
				"rate":        speed,
				"pitch":       pitch,
			},
			"input": map[string]any{},
		},
	}

	if instr := strings.TrimSpace(req.Instructions); instr != "" {
		params["payload"].(map[string]any)["parameters"].(map[string]any)["instruction"] = instr
	}
	if emo := strings.TrimSpace(req.Emotion); emo != "" {
		params["payload"].(map[string]any)["parameters"].(map[string]any)["emotion"] = emo
	}

	if err := conn.WriteJSON(params); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tts: cosyvoice run-task failed: %w", err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	stream := &cosyVoiceStream{
		driver:  d,
		conn:    conn,
		taskID:  taskID,
		audioCh: make(chan SpeechStreamChunk, 8),
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
		ctx:     streamCtx,
		cancel:  cancel,
		metadata: SpeechStreamMetadata{
			VoiceID:    voiceID,
			Provider:   d.ProviderID(),
			Format:     strings.ToLower(format),
			MimeType:   encodingToMime(format),
			SampleRate: sampleRate,
			Speed:      speed,
			Pitch:      pitch,
			Emotion:    strings.TrimSpace(req.Emotion),
		},
	}
	if stream.metadata.MimeType == "" {
		stream.metadata.MimeType = "audio/mpeg"
	}

	go stream.listen()

	readyCtx, cancelReady := context.WithTimeout(ctx, 3*time.Second)
	defer cancelReady()
	if err := stream.waitForReady(readyCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		stream.Close()
		return nil, err
	}

	if initial := strings.TrimSpace(req.InitialText); initial != "" {
		if err := stream.AppendText(ctx, initial); err != nil {
			stream.Close()
			return nil, err
		}
	}

	return stream, nil
}

// cosyVoiceStream 管理 CosyVoice 流式连接的生命周期。
type cosyVoiceStream struct {
	driver    *cosyVoiceDriver
	conn      *websocket.Conn
	taskID    string
	metadata  SpeechStreamMetadata
	audioCh   chan SpeechStreamChunk
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	errMu     sync.Mutex
	err       error
	sendMu    sync.Mutex
	finalized bool
	closeOnce sync.Once
	sequence  int32
}

// Metadata 返回流式会话的元数据。
func (s *cosyVoiceStream) Metadata() SpeechStreamMetadata {
	return s.metadata
}

// Audio 提供流式音频数据的通道。
func (s *cosyVoiceStream) Audio() <-chan SpeechStreamChunk {
	return s.audioCh
}

// AppendText 向会话追加新的待合成文本。
func (s *cosyVoiceStream) AppendText(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := s.selectContext(ctx); err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.finalized {
		return errors.New("tts: cosyvoice stream already finalized")
	}
	payload := map[string]any{
		"header": map[string]any{
			"action":    "continue-task",
			"task_id":   s.taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"input": map[string]any{
				"text": text,
			},
		},
	}
	if err := s.conn.WriteJSON(payload); err != nil {
		errWrapped := fmt.Errorf("tts: cosyvoice continue-task failed: %w", err)
		s.setErr(errWrapped)
		return errWrapped
	}
	return nil
}

// Finalize 通知服务端结束文本输入并等待收尾。
func (s *cosyVoiceStream) Finalize(ctx context.Context) error {
	if err := s.selectContext(ctx); err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.finalized {
		return nil
	}
	payload := map[string]any{
		"header": map[string]any{
			"action":    "finish-task",
			"task_id":   s.taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"input": map[string]any{},
		},
	}
	if err := s.conn.WriteJSON(payload); err != nil {
		errWrapped := fmt.Errorf("tts: cosyvoice finish-task failed: %w", err)
		s.setErr(errWrapped)
		return errWrapped
	}
	s.finalized = true
	return nil
}

// Err 返回流式会话过程中出现的错误。
func (s *cosyVoiceStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// Close 关闭底层连接并释放资源。
func (s *cosyVoiceStream) Close() error {
	s.cancel()
	s.closeOnce.Do(func() {
		_ = s.conn.Close()
	})
	return nil
}

// listen 持续读取服务端消息并分发到通道。
func (s *cosyVoiceStream) listen() {
	defer func() {
		s.signalReady()
		s.closeOnce.Do(func() {
			_ = s.conn.Close()
		})
		close(s.audioCh)
		close(s.done)
		s.cancel()
	}()

	for {
		if s.driver.timeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.driver.timeout))
		}

		msgType, data, err := s.conn.ReadMessage()
		if err != nil {
			if s.ctx.Err() != nil {
				s.setErr(s.ctx.Err())
			} else if ce, ok := err.(*websocket.CloseError); ok {
				if ce.Code != websocket.CloseNormalClosure && ce.Code != websocket.CloseGoingAway {
					s.setErr(fmt.Errorf("tts: cosyvoice stream read failed: %w", err))
				}
			} else if !errors.Is(err, io.EOF) {
				s.setErr(fmt.Errorf("tts: cosyvoice stream read failed: %w", err))
			}
			return
		}

		signal := func() {
			s.signalReady()
		}

		switch msgType {
		case websocket.BinaryMessage:
			if len(data) == 0 {
				continue
			}
			chunkData := make([]byte, len(data))
			copy(chunkData, data)
			sequence := s.nextSequence()
			select {
			case s.audioCh <- SpeechStreamChunk{Sequence: sequence, Audio: chunkData}:
			case <-s.ctx.Done():
				return
			}
		case websocket.TextMessage:
			var event cosyVoiceEvent
			if err := json.Unmarshal(data, &event); err != nil {
				log.Printf("tts: cosyvoice stream parse event failed: %v", err)
				continue
			}
			if s.taskID != "" && event.Header.TaskID != "" && !strings.EqualFold(event.Header.TaskID, s.taskID) {
				continue
			}
			evt := strings.ToLower(strings.TrimSpace(event.Header.Event))
			switch evt {
			case "task-started", "meta", "meta-info":
				signal()
			case "task-failed":
				signal()
				message := strings.TrimSpace(event.Header.ErrorMessage)
				if message == "" {
					message = "unknown error"
				}
				s.setErr(fmt.Errorf("tts: cosyvoice task failed: %s (%s)", message, event.Header.ErrorCode))
				return
			case "task-finished":
				signal()
				return
			default:
				// ignore other events
			}
		default:
			// ignore other message types
		}
	}
}

// waitForReady 等待会话就绪或错误发生。
func (s *cosyVoiceStream) waitForReady(ctx context.Context) error {
	for {
		select {
		case <-s.ready:
			return nil
		case <-s.done:
			if err := s.Err(); err != nil {
				return err
			}
			return errors.New("tts: cosyvoice stream closed")
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			if err := s.Err(); err != nil {
				return err
			}
			return s.ctx.Err()
		}
	}
}

// signalReady 标记会话已准备好输出。
func (s *cosyVoiceStream) signalReady() {
	s.readyOnce.Do(func() {
		close(s.ready)
	})
}

// setErr 记录会话过程中出现的首个错误。
func (s *cosyVoiceStream) setErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

// nextSequence 递增生成音频片段的序号。
func (s *cosyVoiceStream) nextSequence() int {
	return int(atomic.AddInt32(&s.sequence, 1))
}

// selectContext 检查外部上下文是否已取消。
func (s *cosyVoiceStream) selectContext(ctx context.Context) error {
	select {
	case <-s.ctx.Done():
		if err := s.Err(); err != nil {
			return err
		}
		return s.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
