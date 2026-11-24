package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// setupRate はレート制限のテストの前準備をする。
// buckets を空にし、差し替えたパッケージ変数は t.Cleanup で戻す。
func setupRate(t *testing.T, nets []netip.Prefix, max int) {
	t.Helper()
	oldNets, oldMax, oldHops := trustedNets, maxBuckets, trustedHops
	bucketsMu.Lock()
	oldBuckets, oldOverflow := buckets, overflowCount
	buckets = map[string]*bucket{}
	overflowCount = 0
	bucketsMu.Unlock()
	trustedNets, maxBuckets, trustedHops = nets, max, 0
	t.Cleanup(func() {
		trustedNets, maxBuckets, trustedHops = oldNets, oldMax, oldHops
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

// 飛ばす逆代理の段数（trustedHops）で止める。逆代理と同じレンジから来た利用者の値は、
// 送信者が左に何を書いても利用者のキーとして変わらない。
func TestClientIPHopLimit(t *testing.T) {
	setupRate(t, mustNets(t, "10.0.0.0/8 203.0.113.0/24"), 16384)
	trustedHops = 2

	const proxy = "10.0.0.5:40000"

	t.Run("通常の2段", func(t *testing.T) {
		if got := clientIP(newReq(proxy, "198.51.100.7, 203.0.113.9")); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want 198.51.100.7", got)
		}
	})

	t.Run("送信者が書いた値は使わない", func(t *testing.T) {
		if got := clientIP(newReq(proxy, "192.0.2.1, 198.51.100.7, 203.0.113.9")); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want 198.51.100.7", got)
		}
	})

	t.Run("逆代理と同じレンジの利用者", func(t *testing.T) {
		if got := clientIP(newReq(proxy, "192.0.2.1, 203.0.113.50, 203.0.113.9")); got != "203.0.113.50" {
			t.Errorf("clientIP = %q, want 203.0.113.50", got)
		}
	})

	t.Run("逆代理と同じレンジの利用者は送信者の値を変えても同じキー", func(t *testing.T) {
		for i := 1; i <= 50; i++ {
			xff := fmt.Sprintf("192.0.2.%d, 203.0.113.50, 203.0.113.9", i)
			if got := clientIP(newReq(proxy, xff)); got != "203.0.113.50" {
				t.Errorf("左端 192.0.2.%d: clientIP = %q, want 203.0.113.50", i, got)
			}
		}
	})

	t.Run("決めた段の左の解釈できない値は見ない", func(t *testing.T) {
		if got := clientIP(newReq(proxy, "not-an-ip, 203.0.113.50, 203.0.113.9")); got != "203.0.113.50" {
			t.Errorf("clientIP = %q, want 203.0.113.50", got)
		}
	})

	t.Run("信頼レンジが続いても決めた段で止まる", func(t *testing.T) {
		xff := strings.TrimSuffix(strings.Repeat("203.0.113.9, ", 100), ", ")
		if got := clientIP(newReq(proxy, xff)); got != "203.0.113.9" {
			t.Errorf("clientIP = %q, want 203.0.113.9", got)
		}
	})

	t.Run("逆代理を1段だけ経た接続", func(t *testing.T) {
		if got := clientIP(newReq(proxy, "198.51.100.7")); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want 198.51.100.7", got)
		}
	})

	t.Run("解釈できない値", func(t *testing.T) {
		if got := clientIP(newReq(proxy, "not-an-ip, 203.0.113.9")); got != invalidKey {
			t.Errorf("clientIP = %q, want %q", got, invalidKey)
		}
	})

	t.Run("段数1", func(t *testing.T) {
		trustedHops = 1
		defer func() { trustedHops = 2 }()
		if got := clientIP(newReq(proxy, "192.0.2.1, 203.0.113.9")); got != "203.0.113.9" {
			t.Errorf("clientIP = %q, want 203.0.113.9", got)
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

func TestParseTrustedHops(t *testing.T) {
	for _, s := range []string{"", "  	"} {
		n, err := parseTrustedHops(s)
		if err != nil || n != defaultTrustedHops {
			t.Errorf("parseTrustedHops(%q) = %d, %v, want %d・error なし", s, n, err, defaultTrustedHops)
		}
	}
	for s, want := range map[string]int{"0": 0, " 2 ": 2, "16": 16} {
		n, err := parseTrustedHops(s)
		if err != nil || n != want {
			t.Errorf("parseTrustedHops(%q) = %d, %v, want %d・error なし", s, n, err, want)
		}
	}
	for _, s := range []string{"17", "-1", "abc"} {
		if _, err := parseTrustedHops(s); err == nil {
			t.Errorf("parseTrustedHops(%q): error が返らなかった", s)
		}
	}
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

// setupRead は読み取りの枠を差し替える。終わったら元に戻す。
func setupRead(t *testing.T, perMin, burst int) {
	t.Helper()
	old := readLimiter
	readLimiter = newIPLimiter("試験", perMin, burst)
	t.Cleanup(func() { readLimiter = old })
}

// 読み取りの口はバーストまで通し、超えたら 429 で後段へ届かない。別の利用者は影響を受けない。
// 会話の枠とは記録を共有しない。
func TestReadLimit(t *testing.T) {
	setupRate(t, nil, 16384)
	setupRead(t, 1, 30) // 回復を 1 回/分にして時間の揺れを除く

	calls := 0
	h := readLimited(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	do := func(remote string) int {
		w := httptest.NewRecorder()
		h(w, newReq(remote))
		return w.Code
	}

	// 1 回の表示が 4 回 × 7 回の連続表示
	for i := 0; i < 28; i++ {
		if c := do("198.51.100.1:1234"); c != http.StatusOK {
			t.Fatalf("%d 回目: %d, want 200", i+1, c)
		}
	}
	for i := 0; i < 2; i++ {
		if c := do("198.51.100.1:1234"); c != http.StatusOK {
			t.Fatalf("バースト内 %d 回目: %d, want 200", 29+i, c)
		}
	}
	if c := do("198.51.100.1:1234"); c != http.StatusTooManyRequests {
		t.Errorf("バースト超過: %d, want 429", c)
	}
	if calls != 30 {
		t.Errorf("後段に届いた回数 = %d, want 30", calls)
	}
	if c := do("198.51.100.2:1234"); c != http.StatusOK {
		t.Errorf("別の利用者: %d, want 200", c)
	}

	bucketsMu.Lock()
	n := len(buckets)
	bucketsMu.Unlock()
	if n != 0 {
		t.Errorf("会話の枠の記録が %d 件ある（分かれていない）", n)
	}
	if !allow("198.51.100.1") {
		t.Error("読み取りで使い切った利用者が、会話の枠でも断られた")
	}
}

// 3 つの読み取りの口が、実際の配線（registerWeb）で枠を通っている。
func TestReadLimitWiring(t *testing.T) {
	seen := make(chan string, 64) // 後段が受けたパス（応答の前に入れる）
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	oldBase := aiBase
	aiBase = backend.URL
	t.Cleanup(func() { aiBase = oldBase })
	setupRate(t, nil, 16384)
	setupRead(t, 1, 3)

	mux := http.NewServeMux()
	registerWeb(mux)

	cases := []struct {
		path, upstream, remote string
	}{
		{"/api/persona", "/persona", "198.51.100.11:1"},
		{"/api/models", "/models", "198.51.100.12:1"},
		{"/api/avatar", "/avatar", "198.51.100.13:1"},
	}
	for _, c := range cases {
		for i := 1; i <= 4; i++ {
			r := httptest.NewRequest("GET", c.path, nil)
			r.RemoteAddr = c.remote
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			want := http.StatusOK
			if i == 4 {
				want = http.StatusTooManyRequests
			}
			if w.Code != want {
				t.Errorf("%s %d 回目: %d, want %d", c.path, i, w.Code, want)
			}
		}
		got := 0
		for len(seen) > 0 {
			if <-seen == c.upstream {
				got++
			}
		}
		if got != 3 {
			t.Errorf("%s の後段への問い合わせ = %d, want 3", c.path, got)
		}
	}
}

// 受け付けた数（生成中＋順番待ち）が上限に達したら断り、断った分は数に残らない。
func TestAdmitChat(t *testing.T) {
	oldC, oldQ := maxConcurrent, maxQueue
	maxConcurrent, maxQueue = 1, 2
	chatInFlight.Store(0)
	t.Cleanup(func() {
		maxConcurrent, maxQueue = oldC, oldQ
		chatInFlight.Store(0)
	})

	for i := 1; i <= 3; i++ {
		if !admitChat() {
			t.Fatalf("%d 回目が断られた", i)
		}
	}
	if admitChat() {
		t.Error("上限を超えたのに受け付けた")
	}
	if n := chatInFlight.Load(); n != 3 {
		t.Errorf("chatInFlight = %d, want 3（断った分が残っている）", n)
	}
	leaveChat()
	if !admitChat() {
		t.Error("1 件終わった後なのに断られた")
	}
}

// 上限に達しているときの会話は、SSE を始めずに 503 と JSON で断る。
func TestChatQueueCap(t *testing.T) {
	setupRate(t, nil, 16384)
	oldC, oldQ, oldSlots, oldBase := maxConcurrent, maxQueue, slots, aiBase
	maxConcurrent, maxQueue = 1, 2
	slots = make(chan struct{}, 1)
	aiBase = "http://127.0.0.1:1"
	chatInFlight.Store(3)
	t.Cleanup(func() {
		maxConcurrent, maxQueue, slots, aiBase = oldC, oldQ, oldSlots, oldBase
		chatInFlight.Store(0)
	})

	r := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	handleChat(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(w.Body.String(), msgQueueFull) {
		t.Errorf("本文に msgQueueFull が無い: %q", w.Body.String())
	}
	if n := chatInFlight.Load(); n != 3 {
		t.Errorf("chatInFlight = %d, want 3", n)
	}
}
