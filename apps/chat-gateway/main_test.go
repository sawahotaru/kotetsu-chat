package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// setupRate はレート制限のテストの前準備をする。
// buckets を空にし、差し替えたパッケージ変数は t.Cleanup で戻す。
func setupRate(t *testing.T, nets []netip.Prefix, max int) {
	t.Helper()
	oldNets, oldMax := trustedNets, maxBuckets
	bucketsMu.Lock()
	oldBuckets, oldOverflow := buckets, overflowCount
	buckets = map[string]*bucket{}
	overflowCount = 0
	bucketsMu.Unlock()
	trustedNets, maxBuckets = nets, max
	t.Cleanup(func() {
		trustedNets, maxBuckets = oldNets, oldMax
		bucketsMu.Lock()
		buckets, overflowCount = oldBuckets, oldOverflow
		bucketsMu.Unlock()
	})
}

func mustNets(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	nets, err := parseTrustedNets(s)
	if err != nil {
		t.Fatalf("parseTrustedNets(%q): %v", s, err)
	}
	return nets
}

func newReq(remote string, xff ...string) *http.Request {
	r := &http.Request{RemoteAddr: remote, Header: http.Header{}}
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// 利用者の IP は、信頼する逆代理のレンジを右から飛ばして、最初の信頼しない IP を採る。
// 左側は送信者が自由に書けるので、どれだけ変えても結果が変わってはならない。
func TestClientIP(t *testing.T) {
	setupRate(t, mustNets(t, "10.0.0.0/8 203.0.113.0/24"), 16384)

	const proxy = "10.0.0.5:40000"
	tests := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"通常の2段", proxy, []string{"198.51.100.7, 203.0.113.9"}, "198.51.100.7"},
		{"送信者が書いた値は使わない", proxy, []string{"192.0.2.1, 198.51.100.7, 203.0.113.9"}, "198.51.100.7"},
		{"XFFが複数行", proxy, []string{"192.0.2.1", "198.51.100.7, 203.0.113.9"}, "198.51.100.7"},
		{"信頼しない接続元はXFFを見ない", "192.0.2.50:4444", []string{"198.51.100.7"}, "192.0.2.50"},
		{"XFFなし", proxy, nil, "10.0.0.5"},
		{"全部が信頼レンジ", proxy, []string{"203.0.113.9"}, "203.0.113.9"},
		{"解釈できない値", proxy, []string{"not-an-ip, 203.0.113.9"}, "invalid"},
		{"IPv6", proxy, []string{"2001:db8::1, 203.0.113.9"}, "2001:db8::/64"},
		{"IPv4射影とポート付き", proxy, []string{"[::ffff:198.51.100.7]:1234, 203.0.113.9"}, "198.51.100.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientIP(newReq(tt.remote, tt.xff...)); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("送信者が書いた値を毎回変えても同じキー", func(t *testing.T) {
		for i := 1; i <= 50; i++ {
			xff := fmt.Sprintf("192.0.2.%d, 198.51.100.7, 203.0.113.9", i)
			if got := clientIP(newReq(proxy, xff)); got != "198.51.100.7" {
				t.Fatalf("送信者の値=192.0.2.%d で clientIP = %q, want 198.51.100.7", i, got)
			}
		}
	})

	t.Run("長大なXFF", func(t *testing.T) {
		xff := strings.Repeat("x", 10000) + ", 203.0.113.9"
		start := time.Now()
		got := clientIP(newReq(proxy, xff))
		if got != invalidKey {
			t.Errorf("clientIP = %q, want %q", got, invalidKey)
		}
		if d := time.Since(start); d >= time.Second {
			t.Errorf("処理に %v かかった（1 秒未満であること）", d)
		}
	})

	t.Run("信頼レンジが続きすぎる", func(t *testing.T) {
		xff := strings.TrimSuffix(strings.Repeat("203.0.113.9, ", 100), ", ")
		if got := clientIP(newReq(proxy, xff)); got != invalidKey {
			t.Errorf("clientIP = %q, want %q", got, invalidKey)
		}
	})

	t.Run("IPv6は64単位", func(t *testing.T) {
		a := clientIP(newReq(proxy, "2001:db8::ffff:2, 203.0.113.9"))
		b := clientIP(newReq(proxy, "2001:db8:0:1::1, 203.0.113.9"))
		if a != "2001:db8::/64" {
			t.Errorf("1 つ目 = %q, want 2001:db8::/64", a)
		}
		if b != "2001:db8:0:1::/64" {
			t.Errorf("2 つ目 = %q, want 2001:db8:0:1::/64", b)
		}
	})
}

func TestParseTrustedNets(t *testing.T) {
	t.Run("privateとCIDR", func(t *testing.T) {
		nets, err := parseTrustedNets("private, 203.0.113.0/24")
		if err != nil || len(nets) != 7 {
			t.Fatalf("len=%d err=%v, want 7 件・error なし", len(nets), err)
		}
	})
	t.Run("読めない値", func(t *testing.T) {
		if _, err := parseTrustedNets("999.0.0.0/8"); err == nil {
			t.Error("error が返らなかった")
		}
	})
	t.Run("空は0件でエラーにしない", func(t *testing.T) {
		nets, err := parseTrustedNets("")
		if err != nil || len(nets) != 0 {
			t.Fatalf("len=%d err=%v, want 0 件・error なし", len(nets), err)
		}
	})
	t.Run("IP1つ", func(t *testing.T) {
		nets, err := parseTrustedNets("198.51.100.7")
		if err != nil || len(nets) != 1 {
			t.Fatalf("len=%d err=%v, want 1 件・error なし", len(nets), err)
		}
		if got := nets[0].String(); got != "198.51.100.7/32" {
			t.Errorf("got %q, want 198.51.100.7/32", got)
		}
	})
}

func TestAllowBurst(t *testing.T) {
	setupRate(t, nil, 16384)
	for i := 0; i < rateBurst+1; i++ {
		want := i < rateBurst
		if got := allow("198.51.100.1"); got != want {
			t.Fatalf("%d 回目: allow = %v, want %v", i+1, got, want)
		}
	}
}

func TestAllowKeyLengthCap(t *testing.T) {
	setupRate(t, nil, 16384)
	for i := 0; i < 1000; i++ {
		allow(fmt.Sprintf("%0*d", maxKeyLen+1, i)) // maxKeyLen+1 バイトの相異なるキー
	}
	bucketsMu.Lock()
	n := len(buckets)
	bucketsMu.Unlock()
	if n > 1 {
		t.Errorf("len(buckets) = %d, 増分は 1 以下のはず", n)
	}
}

func TestAllowBucketCap(t *testing.T) {
	setupRate(t, nil, 100)
	overflowAllowed := 0
	for i := 0; i < 1000; i++ {
		ok := allow(fmt.Sprintf("2001:db8::%x", i))
		if i >= 100 && ok {
			overflowAllowed++
		}
	}
	bucketsMu.Lock()
	n := len(buckets)
	bucketsMu.Unlock()
	if n > 101 {
		t.Errorf("len(buckets) = %d, want 101 以下", n)
	}
	if overflowAllowed != rateBurst {
		t.Errorf("上限を超えた分で通ったのは %d 回, want %d 回（共通の枠を分け合う）", overflowAllowed, rateBurst)
	}
}

func TestSweepBucketsOnce(t *testing.T) {
	setupRate(t, nil, 16384)
	var logs bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOut) })

	now := time.Now()
	bucketsMu.Lock()
	buckets["198.51.100.1"] = &bucket{tokens: 1, last: now.Add(-31 * time.Minute)}
	buckets["198.51.100.2"] = &bucket{tokens: 1, last: now.Add(-1 * time.Minute)}
	overflowCount = 3
	bucketsMu.Unlock()

	sweepBucketsOnce(now)

	bucketsMu.Lock()
	_, oldLeft := buckets["198.51.100.1"]
	_, newLeft := buckets["198.51.100.2"]
	oc := overflowCount
	bucketsMu.Unlock()
	if oldLeft {
		t.Error("31 分前のバケットが残っている")
	}
	if !newLeft {
		t.Error("1 分前のバケットが消えた")
	}
	if oc != 0 {
		t.Errorf("overflowCount = %d, want 0", oc)
	}
	if !strings.Contains(logs.String(), "3 件") {
		t.Errorf("共通の枠に回した件数が記録されていない: %q", logs.String())
	}
	if strings.Contains(logs.String(), "198.51.100") {
		t.Error("記録に IP が出ている")
	}
}
