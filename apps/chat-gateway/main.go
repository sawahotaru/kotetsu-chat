// チャットの前段（ゲートウェイ）。
//
// 役割はネットワーク境界の守りに限る。プロンプト構築やモデル呼び出しは
// 一切ここでやらず、後段の chat-ai（Python）へ丸ごと委ねる。
//
//	ブラウザ ──SSE── chat-gateway(Go) ──NDJSON── chat-ai(Python) ──> ollama
//
// ここで守っているもの:
//   - 同時生成数の制限。VM は 4 コア CPU のみで GPU が無い。2本同時に走らせると
//     どちらも遅くなるうえ、同居している ec-api / clinic まで巻き添えになる。
//     だから「並列で捌く」のではなく「並ばせて1本ずつ流す」。
//   - IP ごとのレート制限（トークンバケット）。
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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// ---- 設定（環境変数。すべて既定値を持つ）----

// 配る物の版。⚠ 後段(settings.py の VERSION)と CHANGELOG.md の見出しに揃えること。
//
// 突き合わせは起動時に行わない。照らすには互いを呼ぶ必要があり、
// 起動の順番に依存する脆い確認になるため。代わりに両方の /healthz に出しておき、
// 食い違っていれば見て分かるようにしてある。
const version = "0.1.3"

var (
	aiBase         = env("CHAT_AI_URL", "http://chat-ai:8000")
	maxConcurrent  = envInt("CHAT_MAX_CONCURRENT", 1)
	queueWait      = time.Duration(envInt("CHAT_QUEUE_WAIT_SECONDS", 30)) * time.Second
	genTimeout     = time.Duration(envInt("CHAT_GEN_TIMEOUT_SECONDS", 180)) * time.Second
	maxMessageRune = envInt("CHAT_MAX_MESSAGE_CHARS", 2000)
	maxHistory     = envInt("CHAT_MAX_HISTORY", 8)
	rateBurst      = envInt("CHAT_RATE_BURST", 5)
	ratePerMin     = envInt("CHAT_RATE_PER_MIN", 12)

	// この口（Web）から来た質問に LLM を使わせるか。既定は **false**。
	//
	// Web は匿名で誰でも来る。そこで 1B のモデルに推測させると、実在する商品を
	// 「販売しておりません」と答えるような嘘が、そのまま lab の顔になる。
	// 相手が特定できる口（LINE 等）は別のアダプタとして足し、そちらで true にする。
	allowLLM = envBool("CHAT_ALLOW_LLM", false)

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
	if queueWait >= genTimeout {
		log.Fatalf("設定エラー: CHAT_QUEUE_WAIT_SECONDS(%.0f秒) は CHAT_GEN_TIMEOUT_SECONDS(%.0f秒) より短くしてください"+
			"（順番待ちだけで生成の持ち時間を使い切ってしまう）",
			queueWait.Seconds(), genTimeout.Seconds())
	}
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

func release() { <-slots }

// ---- レート制限（IPごとのトークンバケット）----

type bucket struct {
	tokens float64
	last   time.Time
}

var (
	bucketsMu sync.Mutex
	buckets   = map[string]*bucket{}
)

// allow は該当IPが1回分の生成をしてよいかを返す。
func allow(ip string) bool {
	now := time.Now()
	refill := float64(ratePerMin) / 60.0 // 1秒あたりの回復量

	bucketsMu.Lock()
	defer bucketsMu.Unlock()

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
	for range time.Tick(10 * time.Minute) {
		cutoff := time.Now().Add(-30 * time.Minute)
		bucketsMu.Lock()
		for ip, b := range buckets {
			if b.last.Before(cutoff) {
				delete(buckets, ip)
			}
		}
		bucketsMu.Unlock()
	}
}

// clientIP は Caddy が付ける X-Forwarded-For の左端＝本来のクライアントを取る。
// Caddy 側で trusted_proxies を設定済みなので、ここに来る値は詐称されていない前提でよい。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
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
}

// upstreamRequest は後段へ送る形。
// 受け取った JSON を素通しせず、この構造体に**組み直して**送る。
// そのため利用者が知らない項目を混ぜても後段には届かないし、
// allow_llm のように利用者に決めさせたくない項目はここで固定できる。
type upstreamRequest struct {
	chatRequest
	AllowLLM bool `json:"allow_llm"`
}

func main() {
	validateConfig()

	slots = make(chan struct{}, maxConcurrent)
	go sweepBuckets()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleUI)
	mux.HandleFunc("/api/models", handleModels)
	// 画面に出す文言と顔は後段が持つ。前段は器のまま保ち、そのまま中継する。
	// ここに埋め込むと、内容ファイルを差し替えても表題や名乗りが変わらなくなる。
	mux.HandleFunc("/api/persona", proxyGet("/persona"))
	mux.HandleFunc("/api/avatar", proxyGet("/avatar"))
	mux.HandleFunc("/api/chat", handleChat)
	// 生きているか、と、何の版か。版を出すのは、配った先で
	// 「どれが動いているのか」を確かめる術が他に無いため
	// （イメージのタグは付け替えられる）。
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// client_params は画面が見る。効かない欄を見せないため（押しても無視される物を出さない）。
		fmt.Fprintf(w, "{\"status\":\"ok\",\"version\":%q,\"client_params\":%t}\n", version, allowClientParams)
	})

	addr := ":" + env("PORT", "8080")
	log.Printf("chat-gateway %s 起動 addr=%s ai=%s 同時生成=%d レート=%d回/分(バースト%d) LLM=%v 利用者設定=%v",
		version, addr, aiBase, maxConcurrent, ratePerMin, rateBurst, allowLLM, allowClientParams)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aiBase+"/models", nil)
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
		writeJSONError(w, http.StatusTooManyRequests,
			fmt.Sprintf("リクエストが多すぎます。1分あたり %d 回までです。", ratePerMin))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "ストリーミングに対応していません")
		return
	}

	// ここから SSE。ヘッダを先に送ってしまえば、待ち時間中もブラウザは接続を保つ。
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Caddy/Cloudflare にバッファさせない（貯め込まれるとストリームの意味が無い）。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	done := r.Context().Done()

	// 順番待ち。空きが無いあいだは待っていることを画面に伝える。
	if len(slots) >= maxConcurrent {
		sendEvent(w, flusher, "status", map[string]string{
			"message": "順番待ちです（このデモはCPU推論のため1件ずつ処理します）",
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

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
