package main

import (
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"os"

	timage "aideo/image"
)

// ==================== 主函数 ====================

func main() {
	// 顶层恢复，确保终端状态恢复
	defer func() {
		if r := recover(); r != nil {
			fmt.Print(ENABLE_MOUSE)
			fmt.Print(SHOW_CURSOR)
			fmt.Print(LEAVE_ALTERNATE)
			fmt.Fprintf(os.Stderr, "panic: %v\n", r)
			os.Exit(1)
		}
	}()

	if len(os.Args) < 2 {
		fmt.Println("Usage: aideo <image/video file>")
		fmt.Println("Supported formats: .jpg .jpeg .png .mp4 .mov .mkv")
		fmt.Println("Example: aideo photo.jpg")
		fmt.Println("Example: aideo video.mp4")
		os.Exit(1)
	}

	filename := os.Args[1]

	// 如果是视频文件，直接播放视频
	if isVideoFile(filename) {
		if !timage.IsSixelAvailable() {
			fmt.Println("Error: terminal does not support Sixel protocol, cannot play video")
			os.Exit(1)
		}
		initVideoPlayback(filename, timage.ProtocolSixel)
		return
	}

	if err := runImagePlayer(filename); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	return
}

func loadImageAsRGBA(filename string) (*image.RGBA, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("cannot open file: %v", err)
	}
	defer file.Close()

	img, _, err := image.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("解码图像失败: %v", err)
	}

	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, rgba.Bounds(), img, bounds.Min, draw.Src)
	return rgba, nil
}
