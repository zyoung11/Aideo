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
