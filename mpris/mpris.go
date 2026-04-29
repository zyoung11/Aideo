// Package mpris 提供 MPRIS2 D-Bus 协议的纯内存实现。
// 调用方只需实现 PlayerInfo 接口，库负责所有 D-Bus 通信细节。
// 封面艺术通过内存中的 []byte 传递，内部编码为 data: URL。
package mpris

import (
	"encoding/base64"
	"fmt"
	"math"
	"time"

	"github.com/godbus/dbus/v5"
)

// MediaMetadata 描述当前曲目的元数据。ArtData/ArtMIME 为空表示无封面。
type MediaMetadata struct {
	TrackID  string
	Title    string
	Artist   string
	Album    string
	ArtData  []byte
	ArtMIME  string
	Duration int64
}

type PlayerInfo interface {
	Play()
	Pause()
	PlayPause()
	Stop()
	Next()
	Previous()
	Seek(offset int64) int64

	Position() int64
	Volume() float64
	SetVolume(v float64)
	Rate() float64
	SetRate(r float64)
	Metadata() MediaMetadata

	CanGoNext() bool
	CanGoPrevious() bool
	CanSeek() bool
	CanControl() bool
}

// ===== Server =====

type Server struct {
	conn     *dbus.Conn
	player   PlayerInfo
	identity string

	startTime  time.Time
	stopped    bool
	stopChan   chan struct{}
	playing    bool
	savedPos   int64
	lastMeta   MediaMetadata
}

// New 创建 MPRIS 服务端。
func New(player PlayerInfo, identity string) (*Server, error) {
	if identity == "" {
		identity = "MediaPlayer2"
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("连接 D-Bus 失败: %w", err)
	}
	return &Server{
		conn:     conn,
		player:   player,
		identity: identity,
		stopChan: make(chan struct{}),
		lastMeta: player.Metadata(),
	}, nil
}

// Start 注册 D-Bus 服务并开始监听。
func (s *Server) Start() error {
	path := dbus.ObjectPath("/org/mpris/MediaPlayer2")

	if err := s.conn.Export(s, path, "org.freedesktop.DBus.Properties"); err != nil {
		return fmt.Errorf("导出 Properties 接口失败: %w", err)
	}
	if err := s.conn.Export(s, path, "org.mpris.MediaPlayer2"); err != nil {
		return fmt.Errorf("导出 MediaPlayer2 接口失败: %w", err)
	}
	if err := s.conn.Export(s, path, "org.mpris.MediaPlayer2.Player"); err != nil {
		return fmt.Errorf("导出 Player 接口失败: %w", err)
	}

	name := "org.mpris.MediaPlayer2." + s.identity
	reply, err := s.conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("请求服务名 %s 失败: %w", name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("服务名 %s 已被占用", name)
	}

	go s.updateLoop()
	return nil
}

// Close 释放 D-Bus 资源。
func (s *Server) Close() {
	if s.stopped {
		return
	}
	s.stopped = true
	close(s.stopChan)
	time.Sleep(30 * time.Millisecond)
	name := "org.mpris.MediaPlayer2." + s.identity
	s.conn.ReleaseName(name)
	s.conn.Close()
}

// NotifySeek 播放器 seek 后通知位置变更。
func (s *Server) NotifySeek(position int64) {
	s.savedPos = position
	s.emitPlayerProperties(map[string]any{"Position": position})
}

// NotifyMetadata 元数据变化后通知。
func (s *Server) NotifyMetadata() {
	s.lastMeta = s.player.Metadata()
	s.emitPlayerProperties(map[string]any{"Metadata": s.buildMetadata()})
}

// NotifyPlayback 播放/暂停状态变化后通知。
func (s *Server) NotifyPlayback(playing bool) {
	if playing && !s.playing {
		s.startTime = time.Now().Add(-time.Duration(s.savedPos) * time.Microsecond)
	}
	s.playing = playing
	status := "Paused"
	if playing {
		status = "Playing"
	}
	s.emitPlayerProperties(map[string]any{"PlaybackStatus": status})
}

// NotifyVolume 音量变化后通知。
func (s *Server) NotifyVolume(v float64) {
	s.emitPlayerProperties(map[string]any{"Volume": v})
}

// NotifyRate 播放速率变化后通知。
func (s *Server) NotifyRate(r float64) {
	s.emitPlayerProperties(map[string]any{"Rate": r})
}

// NotifyCapabilities 通知前后跳转能力变更。
func (s *Server) NotifyCapabilities() {
	if s.stopped {
		return
	}
	s.emitPlayerProperties(map[string]any{
		"CanGoNext":     s.player.CanGoNext(),
		"CanGoPrevious": s.player.CanGoPrevious(),
		"CanSeek":       s.player.CanSeek(),
	})
}

// ===== org.mpris.MediaPlayer2 =====

func (s *Server) Quit() *dbus.Error { return nil }
func (s *Server) Raise() *dbus.Error { return nil }
func (s *Server) CanQuit() (bool, *dbus.Error) { return true, nil }
func (s *Server) CanRaise() (bool, *dbus.Error) { return false, nil }
func (s *Server) HasTrackList() (bool, *dbus.Error) { return false, nil }
func (s *Server) Identity() (string, *dbus.Error) { return s.identity, nil }
func (s *Server) DesktopEntry() (string, *dbus.Error) { return "", nil }
func (s *Server) SupportedUriSchemes() ([]string, *dbus.Error) { return []string{"file"}, nil }
func (s *Server) SupportedMimeTypes() ([]string, *dbus.Error) {
	return []string{"audio/flac", "audio/mpeg", "audio/ogg", "audio/wav"}, nil
}

// ===== org.mpris.MediaPlayer2.Player =====

func (s *Server) Next() *dbus.Error     { s.player.Next(); return nil }
func (s *Server) Previous() *dbus.Error { s.player.Previous(); return nil }
func (s *Server) Pause() *dbus.Error {
	s.player.Pause()
	s.NotifyPlayback(false)
	return nil
}
func (s *Server) Play() *dbus.Error {
	s.player.Play()
	s.NotifyPlayback(true)
	return nil
}
func (s *Server) PlayPause() *dbus.Error {
	s.player.PlayPause()
	s.NotifyPlayback(s.player.Position() > s.savedPos || !s.playing)
	return nil
}
func (s *Server) Stop() *dbus.Error {
	s.player.Stop()
	s.NotifyPlayback(false)
	return nil
}

func (s *Server) Seek(offset int64) (int64, *dbus.Error) {
	newPos := s.player.Seek(offset)
	s.NotifySeek(newPos)
	return newPos, nil
}

func (s *Server) SetPosition(trackID dbus.ObjectPath, position int64) *dbus.Error {
	s.player.Seek(position - s.currentPosition())
	s.NotifySeek(position)
	return nil
}

func (s *Server) OpenUri(uri string) *dbus.Error {
	return dbus.MakeFailedError(fmt.Errorf("不支持打开 URI"))
}

// ===== org.freedesktop.DBus.Properties =====

func (s *Server) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	switch iface {
	case "org.mpris.MediaPlayer2":
		return s.getRootProp(prop)
	case "org.mpris.MediaPlayer2.Player":
		return s.getPlayerProp(prop)
	}
	return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("未知接口: %s", iface))
}

func (s *Server) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	switch iface {
	case "org.mpris.MediaPlayer2":
		return map[string]dbus.Variant{
			"CanQuit":             dbus.MakeVariant(true),
			"CanRaise":            dbus.MakeVariant(false),
			"HasTrackList":        dbus.MakeVariant(false),
			"Identity":            dbus.MakeVariant(s.identity),
			"DesktopEntry":        dbus.MakeVariant(""),
			"SupportedUriSchemes": dbus.MakeVariant([]string{"file"}),
			"SupportedMimeTypes":  dbus.MakeVariant([]string{"audio/flac", "audio/mpeg", "audio/ogg", "audio/wav"}),
		}, nil
	case "org.mpris.MediaPlayer2.Player":
		return map[string]dbus.Variant{
			"PlaybackStatus": dbus.MakeVariant(s.playbackStatus()),
			"LoopStatus":     dbus.MakeVariant("None"),
			"Rate":           dbus.MakeVariant(s.player.Rate()),
			"Shuffle":        dbus.MakeVariant(false),
			"Metadata":       dbus.MakeVariant(s.buildMetadata()),
			"Volume":         dbus.MakeVariant(s.player.Volume()),
			"Position":       dbus.MakeVariant(s.currentPosition()),
			"MinimumRate":    dbus.MakeVariant(0.1),
			"MaximumRate":    dbus.MakeVariant(4.0),
			"CanGoNext":      dbus.MakeVariant(s.player.CanGoNext()),
			"CanGoPrevious":  dbus.MakeVariant(s.player.CanGoPrevious()),
			"CanPlay":        dbus.MakeVariant(true),
			"CanPause":       dbus.MakeVariant(true),
			"CanSeek":        dbus.MakeVariant(s.player.CanSeek()),
			"CanControl":     dbus.MakeVariant(s.player.CanControl()),
		}, nil
	}
	return nil, dbus.MakeFailedError(fmt.Errorf("未知接口: %s", iface))
}

func (s *Server) Set(iface, prop string, value dbus.Variant) *dbus.Error {
	if iface != "org.mpris.MediaPlayer2.Player" {
		return dbus.MakeFailedError(fmt.Errorf("未知接口: %s", iface))
	}
	switch prop {
	case "Volume":
		v := value.Value().(float64)
		v = clamp(v, 0.0, 1.0)
		s.player.SetVolume(v)
		s.emitPlayerProperties(map[string]any{"Volume": v})
	case "Rate":
		r := value.Value().(float64)
		r = clamp(r, 0.1, 4.0)
		s.player.SetRate(r)
		s.emitPlayerProperties(map[string]any{"Rate": r})
	case "LoopStatus", "Shuffle":
	default:
		return dbus.MakeFailedError(fmt.Errorf("属性 %s 不可写", prop))
	}
	return nil
}

// ===== 内部方法 =====

func (s *Server) getRootProp(prop string) (dbus.Variant, *dbus.Error) {
	switch prop {
	case "CanQuit":
		return dbus.MakeVariant(true), nil
	case "CanRaise":
		return dbus.MakeVariant(false), nil
	case "HasTrackList":
		return dbus.MakeVariant(false), nil
	case "Identity":
		return dbus.MakeVariant(s.identity), nil
	case "DesktopEntry":
		return dbus.MakeVariant(""), nil
	case "SupportedUriSchemes":
		return dbus.MakeVariant([]string{"file"}), nil
	case "SupportedMimeTypes":
		return dbus.MakeVariant([]string{"audio/flac", "audio/mpeg", "audio/ogg", "audio/wav"}), nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("未知属性: %s", prop))
}

func (s *Server) getPlayerProp(prop string) (dbus.Variant, *dbus.Error) {
	switch prop {
	case "PlaybackStatus":
		return dbus.MakeVariant(s.playbackStatus()), nil
	case "LoopStatus":
		return dbus.MakeVariant("None"), nil
	case "Rate":
		return dbus.MakeVariant(s.player.Rate()), nil
	case "Shuffle":
		return dbus.MakeVariant(false), nil
	case "Metadata":
		return dbus.MakeVariant(s.buildMetadata()), nil
	case "Volume":
		return dbus.MakeVariant(s.player.Volume()), nil
	case "Position":
		return dbus.MakeVariant(s.currentPosition()), nil
	case "MinimumRate":
		return dbus.MakeVariant(0.1), nil
	case "MaximumRate":
		return dbus.MakeVariant(4.0), nil
	case "CanGoNext":
		return dbus.MakeVariant(s.player.CanGoNext()), nil
	case "CanGoPrevious":
		return dbus.MakeVariant(s.player.CanGoPrevious()), nil
	case "CanPlay":
		return dbus.MakeVariant(true), nil
	case "CanPause":
		return dbus.MakeVariant(true), nil
	case "CanSeek":
		return dbus.MakeVariant(s.player.CanSeek()), nil
	case "CanControl":
		return dbus.MakeVariant(s.player.CanControl()), nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("未知属性: %s", prop))
}

func (s *Server) currentPosition() int64 {
	if s.playing && !s.startTime.IsZero() {
		elapsed := time.Since(s.startTime)
		pos := int64(elapsed.Microseconds())
		meta := s.player.Metadata()
		if meta.Duration > 0 && pos >= meta.Duration {
			pos = pos % meta.Duration
		}
		s.savedPos = pos
		return pos
	}
	return s.savedPos
}

func (s *Server) playbackStatus() string {
	if s.playing {
		return "Playing"
	}
	if s.stopped || s.savedPos == 0 {
		return "Stopped"
	}
	return "Paused"
}

func (s *Server) buildMetadata() map[string]dbus.Variant {
	m := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath("/org/mpris/MediaPlayer2/TrackList/NoTrack")),
		"mpris:length":  dbus.MakeVariant(s.lastMeta.Duration),
		"xesam:title":   dbus.MakeVariant(s.lastMeta.Title),
		"xesam:artist":  dbus.MakeVariant([]string{s.lastMeta.Artist}),
		"xesam:album":   dbus.MakeVariant(s.lastMeta.Album),
	}
	if artURL := s.buildArtURL(); artURL != "" {
		m["mpris:artUrl"] = dbus.MakeVariant(artURL)
	}
	return m
}

func (s *Server) buildArtURL() string {
	if len(s.lastMeta.ArtData) == 0 || s.lastMeta.ArtMIME == "" {
		return ""
	}
	return fmt.Sprintf("data:%s;base64,%s", s.lastMeta.ArtMIME, base64.StdEncoding.EncodeToString(s.lastMeta.ArtData))
}

func (s *Server) emitPlayerProperties(changed map[string]any) {
	if s.stopped {
		return
	}
	s.conn.Emit(
		dbus.ObjectPath("/org/mpris/MediaPlayer2"),
		"org.freedesktop.DBus.Properties.PropertiesChanged",
		"org.mpris.MediaPlayer2.Player",
		changed,
		[]string{},
	)
}

func (s *Server) updateLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			if s.stopped {
				return
			}
			pos := s.currentPosition()
			s.emitPlayerProperties(map[string]any{"Position": pos})
		}
	}
}

func clamp[T ~float64](v, lo, hi T) T {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

var _ = math.Log2
