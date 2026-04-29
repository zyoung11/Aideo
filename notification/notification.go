// Package notification 提供纯内存桌面通知，通过 D-Bus org.freedesktop.Notifications 发送，无需临时文件。
package notification

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os/exec"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Icon 代表通知图标，Data 为原始图片字节，MIME 为图像类型（如 "image/png"）。
// 若为 nil 则不附带图标。
type Icon struct {
	Data []byte
	MIME string
}

var dbusConn *dbus.Conn

// Available 检查通知功能是否可用（D-Bus 会话总线是否可达）。
func Available() bool {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		return false
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func ensureConn() (*dbus.Conn, error) {
	if dbusConn != nil {
		return dbusConn, nil
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("连接 D-Bus 会话总线失败: %w", err)
	}
	dbusConn = conn
	return conn, nil
}

// Send 异步发送桌面通知（不阻塞调用方）。
// appName 为应用名，title 为标题，body 为正文，icon 为可选图标。
func Send(appName, title, body string, icon *Icon) {
	go func() {
		safeTitle := sanitize(title)
		safeBody := sanitize(body)

		if safeTitle == "" {
			safeTitle = " "
		}
		if safeBody == "" {
			safeBody = " "
		}

		conn, err := ensureConn()
		if err != nil {
			return
		}

		hints := make(map[string]dbus.Variant)
		if icon != nil && len(icon.Data) > 0 {
			if hint, err := buildImageHint(icon.Data, icon.MIME); err == nil {
				hints["image-data"] = hint
			}
		}

		obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
		call := obj.Call(
			"org.freedesktop.Notifications.Notify",
			0,
			appName,
			uint32(0),
			"",
			safeTitle,
			safeBody,
			[]string{},
			hints,
			int32(-1),
		)
		if call.Err != nil {
			_ = call.Err
		}
	}()
}

func sanitize(s string) string {
	replacer := strings.NewReplacer(
		"&", "",
		";", "",
		"|", "",
		"*", "",
		"~", "",
		"<", "",
		">", "",
		"^", "",
		"(", "",
		")", "",
		"[", "",
		"]", "",
		"{", "",
		"}",
		"$", "",
		"\"", "",
	)
	return strings.TrimSpace(replacer.Replace(s))
}

func buildImageHint(data []byte, mime string) (dbus.Variant, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		pngData := encodePNG(data)
		if pngData == nil {
			return dbus.Variant{}, fmt.Errorf("无法解析图片: %w", err)
		}
		data = pngData
		img, _, err = image.Decode(bytes.NewReader(data))
		if err != nil {
			return dbus.Variant{}, fmt.Errorf("无法解析图片: %w", err)
		}
	}

	bounds := img.Bounds()
	width := int32(bounds.Dx())
	height := int32(bounds.Dy())

	rowStride := width * 4
	raw := make([]byte, height*rowStride)
	for y := 0; y < int(height); y++ {
		for x := 0; x < int(width); x++ {
			r, g, b, a := img.At(x+bounds.Min.X, y+bounds.Min.Y).RGBA()
			off := int32(y)*rowStride + int32(x)*4
			raw[off] = byte(r >> 8)
			raw[off+1] = byte(g >> 8)
			raw[off+2] = byte(b >> 8)
			raw[off+3] = byte(a >> 8)
		}
	}

	hasAlpha := int32(1)
	bitsPerSample := int32(8)
	channels := int32(4)

	sig := dbus.SignatureOf(width, height, rowStride, hasAlpha, bitsPerSample, channels, raw)

	return dbus.MakeVariantWithSignature(
		[]any{width, height, rowStride, hasAlpha, bitsPerSample, channels, raw},
		sig,
	), nil
}

func encodePNG(data []byte) []byte {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
