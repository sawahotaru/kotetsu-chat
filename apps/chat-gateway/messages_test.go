package main

import (
	"strings"
	"testing"
	"unicode"
)

// 利用者の画面に出す文言には、上限の数値や処理の仕組みを書かない。
// 数字（半角・全角）が入ると、設定を変えたときに文言と食い違うだけでなく、
// 外からは覚える必要のない内部の事情を教えることになる。
func TestUserMessagesHideLimits(t *testing.T) {
	msgs := map[string]string{
		"msgTooManyRequests": msgTooManyRequests,
		"msgQueued":          msgQueued,
		"msgQueueFull":       msgQueueFull,
	}
	for name, msg := range msgs {
		t.Run(name, func(t *testing.T) {
			if msg == "" {
				t.Fatal("文言が空")
			}
			for _, r := range msg {
				if unicode.IsDigit(r) {
					t.Errorf("数字 %q が入っている: %q", r, msg)
				}
			}
			for _, word := range []string{"CPU", "推論"} {
				if strings.Contains(msg, word) {
					t.Errorf("%q を含んではならない: %q", word, msg)
				}
			}
		})
	}
}
