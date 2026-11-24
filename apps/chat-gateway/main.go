// チャットの前段（ゲートウェイ）。
//
// 役割はネットワーク境界の守りに限る。プロンプト構築やモデル呼び出しは
// 一切ここでやらず、後段の chat-ai（Python）へ丸ごと委ねる。
//
//	ブラウザ ──SSE── chat-gateway(Go) ──NDJSON── chat-ai(Python) ──> ollama
//
// ここで守っているもの:
//   - 同時生成数の制限。GPU の無い機械で 2 本同時に走らせると、どちらも遅くなり、
//     同じ機械の他の仕事にも響く。
//     だから「並列で捌く」のではなく「並ばせて1本ずつ流す」。
//   - IP ごとのレート制限（トークンバケット）。
//   - 画面が取りに来る一覧・文言・顔にも、会話とは別の枠でレート制限。
//   - 順番待ちの人数の上限。超えた分は待たせずに断る。
//   - 入力長・履歴長の上限。後段に着く前に切る。
//   - 生成中にブラウザが閉じられたら後段へのリクエストも切る（CPUを空ける）。
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// ---- 設定（環境変数。すべて既定値を持つ）----

// 配る物の版。⚠ 後段(settings.py の VERSION)と変更の記録に揃えること。
//
// 突き合わせは起動時に行わない。照らすには互いを呼ぶ必要があり、
// 起動の順番に依存する脆い確認になるため。代わりに両方の起動ログに出しておき、
// 食い違っていれば見て分かるようにしてある。
const version = "0.5.1"

var (
	aiBase         = env("CHAT_AI_URL", "http://chat-ai:8000")
	maxConcurrent  = envInt("CHAT_MAX_CONCURRENT", 1)
	queueWait      = time.Duration(envInt("CHAT_QUEUE_WAIT_SECONDS", 30)) * time.Second
	genTimeout     = time.Duration(envInt("CHAT_GEN_TIMEOUT_SECONDS", 180)) * time.Second
	maxMessageRune = envInt("CHAT_MAX_MESSAGE_CHARS", 2000)
	maxHistory     = envInt("CHAT_MAX_HISTORY", 8)
	rateBurst      = envInt("CHAT_RATE_BURST", 5)
	ratePerMin     = envInt("CHAT_RATE_PER_MIN", 12)

	// 利用者の IP を決めるときに飛ばす逆代理のレンジ（解釈は validateConfig で行う）。
	trustedNetsRaw = os.Getenv("CHAT_TRUSTED_NETS")

	// 飛ばす逆代理の段数（直接の接続元を 1 段目と数える。解釈は validateConfig）。
	trustedHopsRaw = os.Getenv("CHAT_TRUSTED_HOPS")

	// レート制限のために覚えておく利用者の数の上限。超えた分は共通の枠を分け合う。
	maxBuckets = envInt("CHAT_RATE_MAX_KEYS", 16384)

	// 画面が取りに来る一覧・文言・顔（/api/models・/api/persona・/api/avatar）の回数の枠。
	// 会話の枠とは分ける。画面を開くたびに取りに来るため、分け合うと話せる回数が減る。
	readRatePerMin = envInt("CHAT_READ_RATE_PER_MIN", 60)
	readRateBurst  = envInt("CHAT_READ_RATE_BURST", 30)

	// 生成中のほかに、順番を待たせてよい人数。超えた分は待たせずに断る。
	maxQueue = envInt("CHAT_MAX_QUEUE", 8)

	// trustedNetsRaw を解釈した結果。テストから差し替えられるように var で持つ。
	trustedNets []netip.Prefix

	// trustedHopsRaw を解釈した結果。テストから差し替えられるように var で持つ。
	trustedHops int

	// この口（Web）から来た質問に LLM を使わせるか。既定は **false**。
	//
	// Web は匿名で誰でも来る。そこで 1B のモデルに推測させると、実在する商品を
	// 「販売しておりません」と答えるような嘘が、そのまま lab の顔になる。
	// 相手が特定できる口（LINE 等）は別のアダプタとして足し、そちらで true にする。
	allowLLM = envBool("CHAT_ALLOW_LLM", false)

	// どちらの口として起動するか。"line" で LINE の口になる。
	//
	// ⚠ **秘密（LINE_CHANNEL_SECRET）の有無でモードを決めてはいけない。**
	//
	//	秘密の不在が「別モードの合図」になると、「LINE のつもりなのに秘密が無い」を
	//	異常として検出できなくなる。LINE 用のコンテナは CHAT_ALLOW_LLM=1 で動くので、
	//	そこで web 側へ倒れると **/ と /api/chat を生やした LLM 有効の匿名チャット**
	//	として起動してしまう。この分岐が防いでいるものが、設定漏れ1つで復活する。
	//	だからモードは独立した変数で決め、compose 側にべた書きする。
	lineMode = env("CHAT_MODE", "web") == "line"

	// 利用者に system / temperature を決めさせるか。既定は **false**。
	//
	// ⚠ これを true にすると、**匿名の利用者が人格定義を丸ごと差し替えられる**。
	//
	//	後段は system が空でなければ既定を捨てて採用する
	//	（app.py の `system = req.system.strip() or DEFAULT_SYSTEM`）。
	//	つまり 2000 文字ぶんの指示を外から差し込めるということで、
	//	公開している展示物なら「こてつに何でも言わせて画面を撮る」が成立する。
	//	画面側は LLM 無効時にこの欄を隠すが、**隠れているのは画面だけで API は受け付ける**。
	//	守りを画面に置いてはならない。
	//
	// 手元でモデルの振る舞いを試すときだけ 1 にする（配る物なので口は残す）。
	allowClientParams = envBool("CHAT_ALLOW_CLIENT_PARAMS", false)

	// 利用者に「答え方」を選ばせるか。既定は **false**＝設置側が決める。
	//
	// ⚠ これは allow_llm（権限）とは別のもの。権限の**中**で誰が選ぶか、という話。
	//
	//	口ごとに違ってよい:
	//	  公開 Web   … false。訪ねてきた人に「答え方」を選ばせても意味が分からない
	//	  手元の dev … true。見比べるための口なので選ばせる
	//	  LINE       … false。代わりに投稿の頭の字で指定させる（LINE_PREFIXES）
	allowClientMode = envBool("CHAT_ALLOW_CLIENT_MODE", false)

	// 設置側が決める答え方。allowClientMode が false のときはこれで固定する。
	defaultAnswerMode = normalizeAnswerMode(env("CHAT_ANSWER_MODE", "auto"))
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ⚠ 読めない値を既定値で握り潰さない。
//
//	以前は警告だけ出して既定値で動いていたが、それでは
//	「設定したつもりが効いていない」状態のまま動き続ける。
//	使った人が困るまで誰も気づかず、配る物では最も多い苦情の元になる。
//	直せる情報を添えて、その場で止める。
func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("設定エラー: %s=%q は整数として読めません（既定 %d）", k, v, def)
	}
	if n < 1 {
		log.Fatalf("設定エラー: %s=%d は 1 以上にしてください", k, n)
	}
	return n
}

// envBool は真偽の設定を読む。
//
// ⚠ 受け付ける綴りは **後段(Python)の _env_bool と必ず揃えること**。
//
//	以前ここは `== "1"` だけで、CHAT_ALLOW_LLM=true と書くと
//	黙って false になっていた。後段は true を受け取るので、
//	同じ .env を書いたのに前段と後段で解釈が割れる。
//	配る物では、この手の「片方だけ効く」が最も追いにくい不具合になる。
func envBool(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	log.Fatalf("設定エラー: %s=%q は 1/0（true/false）で指定してください", k, os.Getenv(k))
	return def // 到達しない
}

// validateConfig は項目をまたぐ整合を確かめる。
func validateConfig() {
	if !strings.HasPrefix(aiBase, "http://") && !strings.HasPrefix(aiBase, "https://") {
		log.Fatalf("設定エラー: CHAT_AI_URL=%q は http:// か https:// で始めてください", aiBase)
	}
	if rateBurst > ratePerMin {
		log.Fatalf("設定エラー: CHAT_RATE_BURST(%d) は CHAT_RATE_PER_MIN(%d) 以下にしてください",
			rateBurst, ratePerMin)
	}
	if readRateBurst > readRatePerMin {
		log.Fatalf("設定エラー: CHAT_READ_RATE_BURST(%d) は CHAT_READ_RATE_PER_MIN(%d) 以下にしてください",
			readRateBurst, readRatePerMin)
	}
	if queueWait >= genTimeout {
		log.Fatalf("設定エラー: CHAT_QUEUE_WAIT_SECONDS(%.0f秒) は CHAT_GEN_TIMEOUT_SECONDS(%.0f秒) より短くしてください"+
			"（順番待ちだけで生成の持ち時間を使い切ってしまう）",
			queueWait.Seconds(), genTimeout.Seconds())
	}
	nets, err := parseTrustedNets(trustedNetsRaw)
	if err != nil {
		log.Fatalf("設定エラー: CHAT_TRUSTED_NETS=%q を読めません（%v）。CIDR か IP を、カンマか空白で区切って書いてください（private でループバックと私設レンジ）",
			trustedNetsRaw, err)
	}
	trustedNets = nets
	h, err := parseTrustedHops(trustedHopsRaw)
	if err != nil {
		log.Fatalf("設定エラー: CHAT_TRUSTED_HOPS=%q を読めません（%v）。0〜%d の整数で書いてください（0 は段数で止めない）",
			trustedHopsRaw, err, maxHops)
	}
	trustedHops = h
	if lineMode {
		validateLineConfig() // 足りなければここで止まる（別モードへ倒れない）
	}
}

// parseTrustedNets は CHAT_TRUSTED_NETS を読む。区切りはカンマと空白。
// private の 1 語は、ループバックと私設レンジに展開する。
// 空なら 0 件でエラーにしない（何も信頼しない＝X-Forwarded-For を見ない）。
func parseTrustedNets(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	for _, f := range fields {
		if f == "private" {
			for _, c := range []string{
				"127.0.0.0/8", "::1/128", "10.0.0.0/8",
				"172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
			} {
				out = append(out, netip.MustParsePrefix(c))
			}
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q は CIDR でも IP でもありません", f)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// parseTrustedHops は CHAT_TRUSTED_HOPS を読む。空なら既定の段数。
// 0 は「段数で止めない」。envInt は 0 を受け付けないので使わない。
func parseTrustedHops(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultTrustedHops, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 0 || n > maxHops {
		return 0, fmt.Errorf("%d は範囲外です", n)
	}
	return n, nil
}

// ---- 同時実行の制御 ----

// slots は「生成してよい権利」を配るチャネル。容量が同時生成数の上限になる。
var slots chan struct{}

// acquire は権利が空くまで待つ。待っている間にブラウザが閉じられたら諦める。
func acquire(done <-chan struct{}) error {
	timer := time.NewTimer(queueWait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return nil
	case <-done:
		return errors.New("クライアントが離脱した")
	case <-timer.C:
		return errors.New("順番待ちがタイムアウトした")
	}
}

// chatInFlight は受け付けた会話の数（生成中＋順番待ち）。
var chatInFlight atomic.Int64

// admitChat は会話を受け付けてよいかを返す。true を返したら必ず leaveChat を呼ぶこと。
func admitChat() bool {
	if chatInFlight.Add(1) > int64(maxConcurrent+maxQueue) {
		chatInFlight.Add(-1)
		return false
	}
	return true
}

func leaveChat() { chatInFlight.Add(-1) }

func release() { <-slots }

// ---- レート制限（IPごとのトークンバケット）----

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	maxKeyLen   = 64         // キーの長さの上限（IPv6 の文字列表現の最大 45 字より大きい）
	maxHops     = 16         // X-Forwarded-For を右から辿る数の上限
	overflowKey = "overflow" // 上限を超えた分がまとまる共通のキー
	invalidKey  = "invalid"  // IP として読めなかった要求がまとまる共通のキー

	defaultTrustedHops = 1 // CHAT_TRUSTED_HOPS が空のときの段数
)

var (
	bucketsMu     sync.Mutex
	buckets       = map[string]*bucket{}
	overflowCount int // 共通のキーへ回した回数。bucketsMu で守る
)

// allow は該当のキー（利用者）が1回分の生成をしてよいかを返す。
func allow(ip string) bool {
	now := time.Now()
	refill := float64(ratePerMin) / 60.0 // 1秒あたりの回復量

	bucketsMu.Lock()
	defer bucketsMu.Unlock()

	// キーが長すぎる、または件数が上限でまだ無いキーは、共通のキーにまとめる。
	if len(ip) > maxKeyLen {
		ip = overflowKey
		overflowCount++
	} else if _, exists := buckets[ip]; !exists && len(buckets) >= maxBuckets {
		ip = overflowKey
		overflowCount++
	}

	b, ok := buckets[ip]
	if !ok {
		b = &bucket{tokens: float64(rateBurst), last: now}
		buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * refill
	if b.tokens > float64(rateBurst) {
		b.tokens = float64(rateBurst)
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepBuckets は満タンに戻った古いIPを捨てる。放置するとIPの数だけメモリが増える。
func sweepBuckets() {
	for now := range time.Tick(10 * time.Minute) {
		sweepBucketsOnce(now)
		readLimiter.sweepOnce(now)
	}
}

// sweepBucketsOnce は 1 回分の掃除。30 分より古いものを捨て、
// 上限を超えて共通の枠に回した件数があれば記録する（IP は出さない）。
func sweepBucketsOnce(now time.Time) {
	cutoff := now.Add(-30 * time.Minute)
	bucketsMu.Lock()
	defer bucketsMu.Unlock()
	for ip, b := range buckets {
		if b.last.Before(cutoff) {
			delete(buckets, ip)
		}
	}
	if overflowCount > 0 {
		log.Printf("レート制限: 記録の上限（%d 件）を超えたため共通の枠に回した要求 %d 件", maxBuckets, overflowCount)
		overflowCount = 0
	}
}

// ipLimiter は allow() と同じ規則（長さ・件数の上限と共通の枠、30 分で掃除）を持つ別の枠。
// 会話の枠（allow）とは記録を共有しない。
//
// ⚠ line.go の limiter とは別物。
type ipLimiter struct {
	name     string
	mu       sync.Mutex
	buckets  map[string]*bucket
	perMin   int
	burst    int
	overflow int // 共通の枠へ回した回数。mu で守る
}

func newIPLimiter(name string, perMin, burst int) *ipLimiter {
	return &ipLimiter{name: name, buckets: map[string]*bucket{}, perMin: perMin, burst: burst}
}

func (l *ipLimiter) allow(key string) bool {
	now := time.Now()
	refill := float64(l.perMin) / 60.0

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(key) > maxKeyLen {
		key = overflowKey
		l.overflow++
	} else if _, exists := l.buckets[key]; !exists && len(l.buckets) >= maxBuckets {
		key = overflowKey
		l.overflow++
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * refill
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepOnce は 30 分より古い記録を捨て、共通の枠へ回した件数を記録する（IP は出さない）。
func (l *ipLimiter) sweepOnce(now time.Time) {
	cutoff := now.Add(-30 * time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
	if l.overflow > 0 {
		log.Printf("レート制限（%s）: 記録の上限（%d 件）を超えたため共通の枠に回した要求 %d 件", l.name, maxBuckets, l.overflow)
		l.overflow = 0
	}
}

// readLimiter は一覧・文言・顔の口が分け合う枠。main で作る。
var readLimiter *ipLimiter

// readLimited は読み取りの口を枠で包む。枠を超えたら後段へ問い合わせずに 429 を返す。
func readLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !readLimiter.allow(clientIP(r)) {
			writeJSONError(w, http.StatusTooManyRequests, msgTooManyRequests)
			return
		}
		next(w, r)
	}
}

// clientIP は利用者の IP を決める。
// 直接の接続元が信頼する逆代理（CHAT_TRUSTED_NETS）のときだけ X-Forwarded-For を見る。
// 右から辿り、信頼する逆代理のレンジを飛ばして、最初に出会った信頼しない IP を採る。
// 飛ばすのは直接の接続元を含めて CHAT_TRUSTED_HOPS 段まで。その次の値は、
// 信頼するレンジに入っていても利用者とみなす（レンジだけでは、逆代理と同じレンジから
// 送ってくる利用者を区別できないため）。左側は送信者が自由に書けるので、そこまでは辿らない。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return invalidKey
	}
	cur, err := netip.ParseAddr(host)
	if err != nil {
		return invalidKey
	}
	cur = cur.WithZone("").Unmap()
	if !isTrusted(cur) {
		return ipKey(cur) // 逆代理を経ていない接続。X-Forwarded-For は見ない
	}

	var hops []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(line, ",")...)
	}
	skipped := 1 // 直接の接続元（信頼する逆代理）を 1 段目と数える
	for i, n := len(hops)-1, 0; i >= 0; i, n = i-1, n+1 {
		if n >= maxHops {
			return invalidKey // 信頼するレンジが続きすぎる
		}
		a, ok := parseForwardedAddr(hops[i])
		if !ok {
			return invalidKey // ヘッダーの生の文字列はキーにしない
		}
		if trustedHops > 0 && skipped >= trustedHops {
			return ipKey(a) // 決めた段数を飛ばした。ここより左は送信者が書ける
		}
		if !isTrusted(a) {
			return ipKey(a)
		}
		cur = a
		skipped++
	}
	return ipKey(cur) // すべて信頼するレンジだった
}

// parseForwardedAddr は X-Forwarded-For の 1 要素を IP にする。
// ポート付き・角括弧付き・IPv4 射影・ゾーン付きを正規化する。
func parseForwardedAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().WithZone("").Unmap(), true
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

// isTrusted は a が信頼する逆代理のレンジに入るかを返す。
func isTrusted(a netip.Addr) bool {
	for _, p := range trustedNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ipKey はレート制限のキーにする。IPv4 は IP そのもの、IPv6 は /64 単位。
func ipKey(a netip.Addr) string {
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

// ---- リクエスト/レスポンスの型 ----

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest はブラウザから受け取る形。
// ⚠ ここに allow_llm を持たせてはいけない。持たせると利用者が自分で true にできてしまう。
//
// ⚠ System / Temperature は受け取るが、**そのまま後段へ渡してはいけない**。
//
//	CHAT_ALLOW_CLIENT_PARAMS が false（既定）なら handleChat が空に落とす。
//	項目自体を消さないのは、手元で試すときに口を残しておくため。
//	どちらも omitempty なので、空にすれば後段の JSON からは消え、後段の既定が効く。
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	System      string    `json:"system,omitempty"`

	// 答え方。画面の「答え方」の選択がこれを送る。
	//
	//	auto  … 規則で拾えたら規則、拾えなければ LLM（既定）
	//	llm   … 常に LLM
	//	rules … 常に規則
	//
	// ⚠ これは allow_llm と違い、**受け取ってよい**。
	//
	//	allow_llm は「この口で LLM を使ってよいか」という**権限**で、設置側が握る。
	//	こちらは「その権限の中で今回どう答えさせるか」でしかない。
	//	閉じてある口（allow_llm=false）に "llm" を指定しても LLM は動かない。
	//
	// なぜ選ばせるか: 以前は「1往復目か」「score がいくつか」という**こちらの推測**で
	// 経路が決まっていた。使う側から見ると、同じ問いが場面によって規則にも LLM にも
	// なり、しかもなぜそうなったか分からない。選べるほうが筋が通る。
	AnswerMode string `json:"answer_mode,omitempty"`
}

// upstreamRequest は後段へ送る形。
// 受け取った JSON を素通しせず、この構造体に**組み直して**送る。
// そのため利用者が知らない項目を混ぜても後段には届かないし、
// allow_llm のように利用者に決めさせたくない項目はここで固定できる。
type upstreamRequest struct {
	chatRequest
	AllowLLM bool `json:"allow_llm"`
}

// 画面から受け取ってよい「答え方」。ここに無い綴りは既定へ落とす。
//
// ⚠ 素通しにしないのは、後段の Literal 検証で 422 になるのを前段で防ぐため。
//
//	利用者の打ち間違いや古い画面が、会話そのものを失敗させるべきではない。
var answerModes = map[string]bool{"auto": true, "llm": true, "rules": true}

// normalizeAnswerMode は受け取った綴りを検める。知らない値は "auto" にする。
func normalizeAnswerMode(v string) string {
	if answerModes[v] {
		return v
	}
	return "auto"
}

func main() {
	validateConfig()

	slots = make(chan struct{}, maxConcurrent)
	readLimiter = newIPLimiter("読み取り", readRatePerMin, readRateBurst)
	go sweepBuckets()

	mux := http.NewServeMux()
	// 生きているか。どちらの口でも出す。版は起動ログに出す。
	mux.HandleFunc("/healthz", handleHealthz)

	if lineMode {
		// LINE 専用のプロセス。**Web の口は生やさない。**
		//
		// ⚠ ここを分岐にせず「LINE の経路を足すだけ」にすると、同じイメージを使う以上
		//	LINE 用プロセスも / と /api/chat を持つ。しかもこの口は CHAT_ALLOW_LLM=1。
		//	つまり **匿名・無制限・LLM 有効のチャットAPI** が中で動いていて、
		//	それを止めているのが逆代理の設定 1 行だけ、という状態になる。
		//	守りを設定ファイルではなくコードに置くための分岐。
		startLine()
		mux.HandleFunc(lineWebhookRoute(), handleLineWebhook)
		if linePushToken != "" {
			// 発信の口。合言葉が無ければ**ルートごと生やさない**＝この設置には存在しない。
			// ⚠ 逆代理には通さないこと（docker ネットワーク内からのみ）。
			mux.HandleFunc("/internal/push", handleInternalPush)
		}
	} else {
		registerWeb(mux)
	}

	addr := ":" + env("PORT", "8080")
	mode := "web"
	if lineMode {
		mode = "line"
	}
	log.Printf("chat-gateway %s 起動 口=%s addr=%s ai=%s 同時生成=%d レート=%d回/分(バースト%d) LLM=%v 利用者設定=%v 答え方=%s(選択%v) 信頼レンジ=%d件 段数=%d 記録の上限=%d件 読み取り=%d回/分(バースト%d) 待ちの上限=%d",
		version, mode, addr, aiBase, maxConcurrent, ratePerMin, rateBurst, allowLLM, allowClientParams, defaultAnswerMode, allowClientMode,
		len(trustedNets), trustedHops, maxBuckets, readRatePerMin, readRateBurst, maxQueue)
	if lineMode {
		log.Printf("LINE: 経路=%s 締切=%.0f秒 諦め=%.0f秒 待ち行列=%d 発信の口=%v",
			lineWebhookRoute(), lineReplyDeadline.Seconds(), lineTokenSafe.Seconds(),
			lineQueueSize, linePushToken != "")
	}
	srv := &http.Server{
		Addr:        addr,
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		// 生成は長く流れ続けるので書き込みには締切を置かない（0=無制限）。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// registerWeb は Web の口の経路を mux に載せる（試験から同じ配線を使うため関数にしてある）。
func registerWeb(mux *http.ServeMux) {
	mux.HandleFunc("/", handleUI)
	mux.HandleFunc("/api/models", readLimited(handleModels))
	// 画面に出す文言と顔は後段が持つ。前段は器のまま保ち、そのまま中継する。
	// ここに埋め込むと、内容ファイルを差し替えても表題や名乗りが変わらなくなる。
	mux.HandleFunc("/api/persona", readLimited(proxyGet("/persona"+allowLLMQuery())))
	mux.HandleFunc("/api/avatar", readLimited(proxyGet("/avatar")))
	mux.HandleFunc("/api/chat", handleChat)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// client_params は画面が見る。効かない欄を見せないため（押しても無視される物を出さない）。
	// version は返さない（公開物の安全方針: 版数は外に出さない。起動ログには出している）。
	fmt.Fprintf(w,
		"{\"status\":\"ok\",\"client_params\":%t,\"client_mode\":%t,\"answer_mode\":%q}\n",
		allowClientParams, allowClientMode, defaultAnswerMode)
}

func handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 履歴はサーバーに残らない。UI もキャッシュさせずに毎回取り直す。
	w.Header().Set("Cache-Control", "no-store")
	w.Write(uiHTML)
}

// proxyGet は後段の GET をそのまま中継する。
//
// 中身には触らない。触れば「後段が持っている」という前提が崩れ、
// 内容ファイルを差し替えても前段の細工が残ってしまう。
// allowLLMQuery は「この口が LLM を使うか」を後段への問い合わせに添える。
//
// ⚠ 後段(chat-ai)は口ごとの設定を知らない。LLM を使うかは口ごとに違い（Web=0 / dev,LINE=1）、
//
//	それを知っているのはこの前段だけ。添えずに聞くと、後段は自分の CHAT_LLM_ENABLED
//	だけで答えるため、**LLM を使わない Web の口でも画面が「＋LLM」と名乗り、
//	モデル選択欄まで出る**（そして答えは必ず規則から返る＝切り替わらない）。
//	文言と一覧の確定は後段に置いたままにしたいので、判断材料だけをここから渡す。
func allowLLMQuery() string {
	if allowLLM {
		return "?allow_llm=1"
	}
	return "?allow_llm=0"
}

func proxyGet(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, aiBase+path, nil)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "リクエストを作れませんでした")
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "AIサービスに接続できませんでした")
			return
		}
		defer resp.Body.Close()

		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

// handleModels は後段の一覧をそのまま返す（どのモデルが使えるか・準備できているか）。
func handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aiBase+"/models"+allowLLMQuery(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "リクエストを作れませんでした")
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "AIサービスに接続できませんでした")
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST で送ってください")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "リクエストの形式が正しくありません")
		return
	}

	// 利用者に決めさせない項目を、後段へ渡る前にここで落とす。
	//
	// ⚠ エラーにはしない。**黙って捨てる**。
	//	画面は temperature を常に送っている（欄が隠れていてもスライダの既定値 0.7 が乗る）。
	//	弾く作りにすると、公開版の会話が丸ごと 400 になる。
	if !allowClientParams {
		req.System = ""
		req.Temperature = nil
	}

	if err := validate(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !allow(clientIP(r)) {
		writeJSONError(w, http.StatusTooManyRequests, msgTooManyRequests)
		return
	}

	// 待ちの人数に上限を置く。超えた分は SSE を始めずに断る（接続を抱え込まない）。
	if !admitChat() {
		writeJSONError(w, http.StatusServiceUnavailable, msgQueueFull)
		return
	}
	defer leaveChat()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "ストリーミングに対応していません")
		return
	}

	// ここから SSE。ヘッダを先に送ってしまえば、待ち時間中もブラウザは接続を保つ。
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 途中の逆代理・CDN にバッファさせない（貯め込まれるとストリームの意味が無い）。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	done := r.Context().Done()

	// 順番待ち。空きが無いあいだは待っていることを画面に伝える。
	if len(slots) >= maxConcurrent {
		sendEvent(w, flusher, "status", map[string]string{
			"message": msgQueued,
		})
	}
	if err := acquire(done); err != nil {
		sendEvent(w, flusher, "error", map[string]string{
			"message": "混み合っています。しばらくしてからもう一度お試しください。",
		})
		return
	}
	defer release()

	stream(w, flusher, r, &req)
}

func validate(req *chatRequest) error {
	if len(req.Messages) == 0 {
		return errors.New("メッセージが空です")
	}
	if len(req.Messages) > maxHistory {
		// 古いものから落とす。エラーにはしない（会話が続けられなくなるだけなので）。
		req.Messages = req.Messages[len(req.Messages)-maxHistory:]
	}
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return errors.New("role は user か assistant のみです")
		}
		if len([]rune(m.Content)) > maxMessageRune {
			return fmt.Errorf("1メッセージは %d 文字までです", maxMessageRune)
		}
	}
	if len([]rune(req.System)) > maxMessageRune {
		return fmt.Errorf("システムプロンプトは %d 文字までです", maxMessageRune)
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return errors.New("temperature は 0〜2 の範囲で指定してください")
	}
	return nil
}

// stream は後段の NDJSON を SSE に載せ替えて流す。
func stream(w http.ResponseWriter, flusher http.Flusher, r *http.Request, req *chatRequest) {
	ctx, cancel := context.WithTimeout(r.Context(), genTimeout)
	defer cancel()

	// ⚠ 権限（allow_llm）は設置側の設定そのまま。
	//   答え方を利用者に選ばせるかどうかも、口ごとの設定で決まる。
	if allowClientMode {
		req.AnswerMode = normalizeAnswerMode(req.AnswerMode)
	} else {
		req.AnswerMode = defaultAnswerMode
	}

	body, err := json.Marshal(upstreamRequest{chatRequest: *req, AllowLLM: allowLLM})
	if err != nil {
		sendEvent(w, flusher, "error", map[string]string{"message": "リクエストを組み立てられませんでした"})
		return
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, aiBase+"/chat", bytes.NewReader(body))
	if err != nil {
		sendEvent(w, flusher, "error", map[string]string{"message": "リクエストを作れませんでした"})
		return
	}
	upstream.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(upstream)
	if err != nil {
		sendEvent(w, flusher, "error", map[string]string{"message": "AIサービスに接続できませんでした"})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg := "AIサービスがエラーを返しました"
		var e struct {
			Detail string `json:"detail"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 8*1024)).Decode(&e) == nil && e.Detail != "" {
			msg = e.Detail
		}
		sendEvent(w, flusher, "error", map[string]string{"message": msg})
		return
	}

	sc := bufio.NewScanner(resp.Body)
	// 1行が長くなることがある（生成した文をまとめて返す場合）ので既定の64KBから広げる。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // 壊れた行は落とす。生成そのものは続ける。
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, line); err != nil {
			return // ブラウザが閉じた。defer cancel() で後段も切れる。
		}
		flusher.Flush()
	}
	if err := sc.Err(); err != nil {
		sendEvent(w, flusher, "error", map[string]string{"message": "生成が中断されました"})
	}
}

func sendEvent(w http.ResponseWriter, flusher http.Flusher, name string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	flusher.Flush()
}

// 利用者の画面に出す文言。上限の数値や処理の仕組みは書かない。
const (
	msgTooManyRequests = "リクエストが多すぎます。しばらく時間をおいてからお試しください。"
	msgQueued          = "順番待ちです。前の方の応答が終わるまでお待ちください。"
	msgQueueFull       = "混み合っています。しばらくしてからもう一度お試しください。"
)

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
