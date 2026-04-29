// Package tconfig 提供通用 TOML 配置加载与按键绑定管理。
// 核心特性：Key 类型支持 TOML 中写字符串或字符串数组；加载时自动校验按键冲突。
package tconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"
)

// ===== 特殊按键常量 =====

const (
	KeyArrowUp = 1000 + iota
	KeyArrowDown
	KeyArrowLeft
	KeyArrowRight
	KeyEnter
	KeyBackspace
)

var specialKeyMap = map[string]rune{
	"enter":      KeyEnter,
	"space":      ' ',
	"backspace":  KeyBackspace,
	"esc":        '\x1b',
	"tab":        '\t',
	"arrowup":    KeyArrowUp,
	"arrowdown":  KeyArrowDown,
	"arrowleft":  KeyArrowLeft,
	"arrowright": KeyArrowRight,
	"up":         KeyArrowUp,
	"down":       KeyArrowDown,
	"left":       KeyArrowLeft,
	"right":      KeyArrowRight,
}

// ===== Key 类型 =====

// Key 代表一个按键或一组按键，每个按键可以是单个字符或 specialKeyMap 中的名称。
type Key []string

func (k *Key) UnmarshalTOML(data []byte) error {
	var single string
	if err := toml.Unmarshal(data, &single); err == nil {
		*k = []string{single}
		return nil
	}
	var multi []string
	if err := toml.Unmarshal(data, &multi); err == nil {
		*k = multi
		return nil
	}
	return fmt.Errorf("键必须是字符串或字符串数组")
}

// ===== 按键转换 =====

func stringToRune(s string) (rune, error) {
	s = strings.ToLower(s)
	if r, ok := specialKeyMap[s]; ok {
		return r, nil
	}
	runes := []rune(s)
	if len(runes) == 1 {
		return runes[0], nil
	}
	return 0, fmt.Errorf("无效按键: '%s'", s)
}

// IsKey 检查给定 rune 是否匹配 Key 中的任意按键。
func IsKey(key rune, actionKeys Key) bool {
	for _, keyStr := range actionKeys {
		if r, err := stringToRune(keyStr); err == nil && r == key {
			return true
		}
	}
	return false
}

// KeyNames 返回 Key 中所有按键的可打印名称。
func (k Key) KeyNames() []string { return []string(k) }

// ===== 加载配置 =====

// Load 加载 TOML 配置。若文件不存在则用 defaultContent 创建。
// config 必须是指向 struct 的指针，struct 中所有 Key 类型字段会自动校验按键冲突。
func Load(configDir, configName, defaultContent string, config any) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	configFile := filepath.Join(configDir, configName)

	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if err := os.WriteFile(configFile, []byte(defaultContent), 0644); err != nil {
			return fmt.Errorf("写入默认配置文件失败: %w", err)
		}
		if _, err := toml.Decode(defaultContent, config); err != nil {
			return fmt.Errorf("解析默认配置失败: %w", err)
		}
	} else {
		if _, err := toml.DecodeFile(configFile, config); err != nil {
			return fmt.Errorf("解析配置文件失败: %w", err)
		}
	}

	return validateKeymap(reflect.ValueOf(config).Elem())
}

func validateKeymap(v reflect.Value) error {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if err := validateKeyFields(field, t.Field(i).Name); err != nil {
			return err
		}
	}
	return nil
}

func validateKeyFields(v reflect.Value, prefix string) error {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}

	t := v.Type()
	seen := make(map[rune]string)

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		fieldName := t.Field(i).Name
		scope := prefix + "." + fieldName

		if keys, ok := field.Interface().(Key); ok {
			for _, keyStr := range keys {
				r, err := stringToRune(keyStr)
				if err != nil {
					return fmt.Errorf("[%s] %s: %w", scope, keyStr, err)
				}
				if existing, dup := seen[r]; dup {
					return fmt.Errorf("[%s] 按键冲突: '%s' 同时分配给 '%s' 和 '%s'", prefix, keyStr, existing, fieldName)
				}
				seen[r] = fieldName
			}
		} else if field.Kind() == reflect.Struct {
			if err := validateKeyFields(field, scope); err != nil {
				return err
			}
		}
	}
	return nil
}
