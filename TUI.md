# TUI 架构文档

## 一、终端全屏（Alternate Screen）

在 `main.go` 中，通过 ANSI 转义序列实现全屏切换：

```go
// 进入全屏（alternate screen buffer）+ 隐藏光标
fmt.Print("\x1b[?1049h\x1b[?25l")
defer fmt.Print("\x1b[2J\x1b[?1049l\x1b[?25h")
```

| 转义序列                   | 作用                                |
| ---------------------- | --------------------------------- |
| `\x1b[?1049h`          | 切换到 alternate screen buffer（全屏模式） |
| `\x1b[?1049l`          | 退出全屏，恢复到原始屏幕                      |
| `\x1b[?25l`            | 隐藏光标                              |
| `\x1b[?25h`            | 显示光标                              |
| `\x1b[2J\x1b[3J\x1b[H` | 清屏并将光标移到左上角                       |

**为什么用 alternate screen？**

切换到 alternate screen 后，TUI 的所有输出不会影响用户之前运行的命令。退出时自动恢复，终端内容保持干净。

### Raw 模式输入

```go
oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
defer term.Restore(int(os.Stdin.Fd()), oldState)
```

`term.MakeRaw()` 关闭终端的规范模式（canonical mode），使程序能直接读取每个按键字符，而不是等用户按回车后才收到。配合 `golang.org/x/term` 包实现。

---

## 二、窗口大小自适应

### 1. 信号监听机制

在 `main.go` 的 `App.Run()` 中注册信号监听：

```go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT)
defer signal.Stop(sigCh)
```

`SIGWINCH` 是操作系统在终端窗口大小变化时发送的信号。

### 2. 动态获取终端尺寸

渲染函数中实时获取窗口像素尺寸：

```go
w, h, err := term.GetSize(int(os.Stdout.Fd()))
```

启动时获取字符单元格尺寸（用于图片缩放计算）：

```go
func getCellSize() (width, height int, err error) {
    fmt.Print("\x1b[16t")  // 终端查询：获取单元格像素大小
    // 读取终端响应并解析
    // ...
}
```

---

## 三、渲染管线

### 完整流程

```
用户调整窗口大小
    ↓
操作系统发送 SIGWINCH
    ↓
main.go 的 select 收到信号
    ↓
currentPage.HandleSignal(SIGWINCH)
    ↓
p.View() 重新渲染
    ↓
displayAlbumArt() 获取新尺寸 → 选择布局 → 渲染封面
    ↓
updateStatus() 更新
```

### 渲染命令

每次 `View()` 开始时先清屏：

```go
fmt.Print("\x1b[2J\x1b[3J\x1b[H")  // 清屏 + 清除滚动缓冲区 + 光标归位
```

---

## 四、图片居中与布局算法

### 1. 图片居中实现

在 `player.go` 的 `displayAlbumArt()` 方法中，根据终端尺寸和布局模式计算图片位置：

```go
// 计算居中位置
startCol, startRow = (w - imageWidthInChars) / 2, (h - imageHeightInChars) / 2
```

**关键步骤：**

1. 获取终端尺寸（字符数）：`w, h, err := term.GetSize(int(os.Stdout.Fd()))`
2. 计算图片尺寸（字符数）：`imageWidthInChars = (finalImgW + cellW - 1) / cellW`
3. 计算居中位置：`(终端宽度 - 图片宽度) / 2`

### 2. 图片左右边纯净实现

渲染图片后，清除图片右侧的空白区域，确保没有残留字符：

```go
if imageWidthInChars > 0 && startCol + imageWidthInChars <= w {
    fillStartCol := startCol + imageWidthInChars
    for row := startRow; row < startRow + imageHeightInChars; row++ {
        fmt.Printf("\x1b[%d;%dH\x1b[K", row, fillStartCol)
    }
}
```

**解释：**

- `\x1b[K`：清除从光标位置到行尾的内容
- 循环遍历图片的每一行，在图片右侧开始位置清除该行
- 确保图片右侧区域干净，没有之前渲染的残留字符

### 3. 动态响应终端大小变化

**信号处理机制：**

```go
// main.go 中注册信号监听
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT)

// 主循环中处理信号
case sig := <-sigCh:
    if sig == syscall.SIGINT {
        return nil
    }
    if err := currentPage.HandleSignal(sig); err != nil {
        return err
    }
```

**页面信号处理：**

```go
// player.go 中处理 SIGWINCH 信号
func (p *PlayerPage) HandleSignal(sig os.Signal) error {
    if sig == syscall.SIGWINCH {
        p.View()  // 重新渲染整个页面
    }
    return nil
}
```

**重新渲染流程：**

1. 收到 `SIGWINCH` 信号
2. 调用当前页面的 `HandleSignal` 方法
3. 触发 `View()` 方法重新渲染
4. `displayAlbumArt()` 获取新的终端尺寸
5. 重新计算布局和位置
6. 清除屏幕并重新绘制所有内容

### 4. 通用TUI实现要点

**核心组件：**

1. **终端尺寸获取**：使用 `golang.org/x/term` 包的 `GetSize()`
2. **信号监听**：监听 `syscall.SIGWINCH` 处理窗口大小变化
3. **布局算法**：根据宽高比和绝对尺寸选择布局模式
4. **坐标计算**：使用字符坐标系统（行、列）进行定位
5. **清屏与光标控制**：使用 ANSI 转义序列精确控制

**通用实现模板：**

```go
// 1. 进入全屏模式
fmt.Print("\x1b[?1049h\x1b[?25l")
defer fmt.Print("\x1b[2J\x1b[?1049l\x1b[?25h")

// 2. 设置原始输入模式
oldState, _ := term.MakeRaw(int(os.Stdin.Fd()))
defer term.Restore(int(os.Stdin.Fd()), oldState)

// 3. 信号监听
go func() {
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGWINCH)
    for {
        sig := <-sigCh
        if sig == syscall.SIGWINCH {
            // 触发重新渲染
            render()
        }
    }
}()

// 4. 渲染函数
type LayoutMode int
const (
    LayoutWide    LayoutMode = iota  // 宽布局
    LayoutNarrow                     // 窄布局
    LayoutMinimal                    // 最小布局
)

func render() {
    // 获取终端尺寸
    w, h, _ := term.GetSize(int(os.Stdout.Fd()))

    // 确定布局模式
    var mode LayoutMode
    if w >= 100 && float64(w)/float64(h) > 2.0 {
        mode = LayoutWide
    } else if w < 40 || h < 10 {
        mode = LayoutMinimal
    } else {
        mode = LayoutNarrow
    }

    // 清屏
    fmt.Print("\x1b[2J\x1b[3J\x1b[H")

    // 根据模式渲染
    switch mode {
    case LayoutWide:
        renderWideLayout(w, h)
    case LayoutNarrow:
        renderNarrowLayout(w, h)
    case LayoutMinimal:
        renderMinimalLayout(w, h)
    }
}

// 5. 居中计算函数
func centerX(text string, totalWidth int) int {
    return (totalWidth - len(text)) / 2
}

func centerItem(itemWidth, totalWidth int) int {
    return (totalWidth - itemWidth) / 2
}
```
