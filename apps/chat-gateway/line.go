// LINE の口（アダプタ）。
//
// Web の口が SSE でブラウザへ流すのに対し、こちらは「即 200 を返して、後から reply する」形。
// 頭（意図分類・生成）は同じ chat-ai を使う。違うのは器だけ。
//
//	LINE ──webhook──> ここ ──即200
//	                   └─ 有界キュー ─> ワーカー1本 ─> chat-ai ─> ollama
//	                                                      └─> reply（無料・60秒以内）
//
// ここで守っているもの:
//   - 署名検証。**本文を読む前に上限を掛ける**（検証前は誰でも到達できる）。
//   - 有界キュー。goroutine を無制限に生やすと、全員が順番待ちで詰まり、
//     全員の replyToken が 60 秒で切れて**誰にも返せなくなる**。
//   - 締切。生成に与える持ち時間は「45秒」ではなく **45秒 − 待たされた時間**。
//     間に合わないと分かった仕事は、生きているトークンで「混んでいる」と伝えて畳む。
//
// 送信は reply だけ（無料・無制限）。応答で push は使わない。
// push は「こちらから話しかける」ときにしか要らず、それは別の機能（§3.14 の /internal/push）。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// ---- 設定 ----

var (
	lineChannelSecret = os.Getenv("LINE_CHANNEL_SECRET")
	lineAccessToken   = os.Getenv("LINE_CHANNEL_ACCESS_TOKEN")
	lineAllowedGroups = splitCSV(os.Getenv("LINE_ALLOWED_GROUP_IDS"))

	// 生成に使ってよい時間（イベント受信時刻から数える）。
	lineReplyDeadline = time.Duration(envInt("LINE_REPLY_DEADLINE_SEC", 45)) * time.Second
	// これを過ぎたら replyToken が死ぬ手前とみなし、何も送らずに捨てる。
	lineTokenSafe = time.Duration(envInt("LINE_TOKEN_SAFE_SEC", 50)) * time.Second

	lineQueueSize = envInt("LINE_QUEUE_SIZE", 3)
	lineLimiter   = newLimiter(envInt("LINE_RATE_PER_MIN", 20), envInt("LINE_RATE_BURST", 5))

	// 発信の口（§3.14）。空ならルートごと生えない＝この設置には発信機能が無い。
	linePushToken = os.Getenv("LINE_PUSH_TOKEN")
)

const (
	// 署名検証より前＝誰でも到達できる場所なので、必ず上限を掛ける。
	// LINE の webhook 本体は数KB程度なので 512KB で十分すぎる。
	lineMaxWebhookBody = 512 * 1024
	lineMaxPushBody    = 64 * 1024

	// LINE のテキスト上限は 5000 文字。余裕を見て切る。
	lineTextLimit = 4900

	// 同じ webhookEventId を覚えておく時間。
	lineSeenTTL = 10 * time.Minute

	lineAPIReply = "https://api.line.me/v2/bot/message/reply"
	lineAPIPush  = "https://api.line.me/v2/bot/message/push"

	msgBusy   = "ただいま立て込んでおり申した。少し間を置いてもう一度お尋ねくだされ。"
	msgEmpty  = "うまく言葉が出ませなんだ。すまぬが、もう一度お尋ねくだされ。"
	msgUsage  = "拙者を呼ぶときは、名を選んだうえで用件を続けてくだされ。（例: @こてつ 東京の天気は？）"
	msgCutoff = "\n\n…（長くなったのでここまで）"
)

func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// lineWebhookRoute は webhook の経路を返す。
//
// ⚠ Caddy は `handle /line/*`（プレフィックスを剥がさない）で通すこと。
// `handle_path` にすると `/webhook` が届いてしまい 404 になる。
func lineWebhookRoute() string {
	if slug := strings.Trim(os.Getenv("LINE_WEBHOOK_SLUG"), "/"); slug != "" {
		return "/line/" + slug + "/webhook"
	}
	return "/line/webhook"
}

// validateLineConfig は LINE モードのときだけ呼ばれる。
//
// ⚠ 秘密の有無で「どちらの口か」を決めてはいけない。決めるのは CHAT_MODE。
//
//	秘密の不在が別モードの合図になると、「LINE のつもりなのに秘密が無い」を
//	異常として検出できなくなる。この口は CHAT_ALLOW_LLM=1 で動くので、
//	web 側へ倒れると **LLM 有効の匿名チャット**として起動してしまう。
func validateLineConfig() {
	if lineChannelSecret == "" {
		log.Fatalf("設定エラー: CHAT_MODE=line ですが LINE_CHANNEL_SECRET が空です（署名検証ができません）")
	}
	if lineAccessToken == "" {
		log.Fatalf("設定エラー: CHAT_MODE=line ですが LINE_CHANNEL_ACCESS_TOKEN が空です（返信ができません）")
	}
	if lineTokenSafe <= lineReplyDeadline {
		log.Fatalf("設定エラー: LINE_TOKEN_SAFE_SEC(%.0f秒) は LINE_REPLY_DEADLINE_SEC(%.0f秒) より大きくしてください"+
			"（生成を諦めた後に「混んでいる」と伝えるための余地）",
			lineTokenSafe.Seconds(), lineReplyDeadline.Seconds())
	}
	if lineTokenSafe >= 60*time.Second {
		log.Fatalf("設定エラー: LINE_TOKEN_SAFE_SEC(%.0f秒) は 60 未満にしてください"+
			"（replyToken の寿命がおよそ60秒。過ぎると何も送れない）", lineTokenSafe.Seconds())
	}
}

// ---- レート制限（キーごとのトークンバケット）----
//
// ⚠ 既存の clientIP() を流用しないこと。
//
//	LINE の webhook は **LINE のサーバから来る**ので、IP で分けると
//	バケツが全利用者で1つに潰れ、事実上機能しない。groupId で分ける。
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	perMin  int
	burst   int
}

func newLimiter(perMin, burst int) *limiter {
	return &limiter{buckets: map[string]*bucket{}, perMin: perMin, burst: burst}
}

func (l *limiter) allow(key string) bool {
	now := time.Now()
	refill := float64(l.perMin) / 60.0

	l.mu.Lock()
	defer l.mu.Unlock()

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

func (l *limiter) sweep() {
	for range time.Tick(10 * time.Minute) {
		cutoff := time.Now().Add(-30 * time.Minute)
		l.mu.Lock()
		for k, b := range l.buckets {
			if b.last.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// ---- 冪等性（同じイベントを二度処理しない）----
//
// LINE の再送は既定オフだが、**有効にした瞬間に二重返信が出る**。先に入れておく。
var (
	lineSeenMu sync.Mutex
	lineSeen   = map[string]time.Time{}
)

func lineSeenBefore(id string) bool {
	if id == "" {
		return false
	}
	now := time.Now()
	lineSeenMu.Lock()
	defer lineSeenMu.Unlock()
	if t, ok := lineSeen[id]; ok && now.Sub(t) < lineSeenTTL {
		return true
	}
	lineSeen[id] = now
	return false
}

func sweepLineSeen() {
	for range time.Tick(5 * time.Minute) {
		cutoff := time.Now().Add(-lineSeenTTL)
		lineSeenMu.Lock()
		for id, t := range lineSeen {
			if t.Before(cutoff) {
				delete(lineSeen, id)
			}
		}
		lineSeenMu.Unlock()
	}
}

// ---- 数え取り（文面は残さない。件数だけ）----
var (
	lineCountSigFail atomic.Int64
	lineCountDup     atomic.Int64
	lineCountBusy    atomic.Int64
	lineCountCutoff  atomic.Int64
	lineCountReply   atomic.Int64
)

// reportLineCounters は動きのあった数だけを定期的に出す。
// 一件ずつ出すと、外からの当てずっぽうな POST でログが埋まる。
func reportLineCounters() {
	for range time.Tick(10 * time.Minute) {
		sig, dup := lineCountSigFail.Swap(0), lineCountDup.Swap(0)
		busy, cut, rep := lineCountBusy.Swap(0), lineCountCutoff.Swap(0), lineCountReply.Swap(0)
		if sig+dup+busy+cut+rep == 0 {
			continue
		}
		log.Printf("LINE: 直近10分 返信=%d 打ち切り=%d 混雑=%d 重複破棄=%d 署名不一致=%d",
			rep, cut, busy, dup, sig)
	}
}

// ---- webhook の形 ----

type lineMentionee struct {
	Index  int    `json:"index"`  // ⚠ UTF-16 コード単位。Go の byte/rune 位置ではない
	Length int    `json:"length"` // 同上
	Type   string `json:"type"`   // "user" / "all"
	IsSelf bool   `json:"isSelf"`
}

type lineEvent struct {
	Type            string `json:"type"`
	WebhookEventID  string `json:"webhookEventId"`
	ReplyToken      string `json:"replyToken"`
	DeliveryContext struct {
		IsRedelivery bool `json:"isRedelivery"`
	} `json:"deliveryContext"`
	Source struct {
		Type    string `json:"type"`
		GroupID string `json:"groupId"`
		RoomID  string `json:"roomId"`
	} `json:"source"`
	Message struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Mention *struct {
			Mentionees []lineMentionee `json:"mentionees"`
		} `json:"mention"`
	} `json:"message"`
}

type lineWebhookBody struct {
	Events []lineEvent `json:"events"`
}

// ---- 仕事の受け渡し ----

type lineJob struct {
	replyToken string
	question   string
	at         time.Time // イベントを受け取った時刻。締切はここから数える
}

var lineJobs chan lineJob

// startLine はキューとワーカー、掃除役を起こす。main() から LINE モードのときだけ呼ぶ。
func startLine() {
	lineJobs = make(chan lineJob, lineQueueSize)
	go lineWorker()
	go sweepLineSeen()
	go lineLimiter.sweep()
	go reportLineCounters()
}

// ---- 署名検証 ----

func validLineSignature(body []byte, got string) bool {
	mac := hmac.New(sha256.New, []byte(lineChannelSecret))
	mac.Write(body)
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	// ⚠ == で比べないこと（タイミング攻撃対策）。
	return hmac.Equal([]byte(want), []byte(got))
}

// ---- 入口 ----

func handleLineWebhook(w http.ResponseWriter, r *http.Request) {
	// ⚠ 生バイトのまま読む。デコード後に再エンコードした JSON では署名が一致しない。
	// ⚠ io.ReadAll を裸で使わない。ここは検証より前＝誰でも到達できる。
	body, err := io.ReadAll(io.LimitReader(r.Body, lineMaxWebhookBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// ⚠ メソッドで 405 を返さない。スモークテストが 400 を見て生死を判じるので、
	//    「署名が無い＝400」に揃える（405 が混ざると期待コードが割れる）。
	sig := r.Header.Get("x-line-signature")
	if sig == "" || !validLineSignature(body, sig) {
		// ⚠ 本文の中身はログに出さない。件数だけ。
		lineCountSigFail.Add(1)
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}

	var hook lineWebhookBody
	if err := json.Unmarshal(body, &hook); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// ★ ここで返す。生成の完了を待たない。
	w.WriteHeader(http.StatusOK)

	for _, ev := range hook.Events {
		q, ok := lineAccept(ev)
		if !ok {
			continue
		}
		select {
		case lineJobs <- lineJob{replyToken: ev.ReplyToken, question: q, at: time.Now()}:
		default:
			// 満杯。replyToken を「混雑のお知らせ」に使い切る。
			// 期限切れさせるより、無料・即時・正直。
			lineCountBusy.Add(1)
			token := ev.ReplyToken
			go func() {
				defer recoverLine("混雑通知")
				lineReply(token, msgBusy)
			}()
		}
	}
}

// lineAccept は「答えるべきイベントか」を判じ、答えるなら質問文を返す。
// 条件を満たさないものは**黙って捨てる**（エラー返信をしない）。
func lineAccept(ev lineEvent) (string, bool) {
	if ev.Type != "message" || ev.Message.Type != "text" || ev.ReplyToken == "" {
		return "", false
	}
	// グループ（と複数人トーク）だけ。1対1は今回のスコープ外。
	src := ev.Source.GroupID
	if src == "" {
		src = ev.Source.RoomID
	}
	if ev.Source.Type != "group" && ev.Source.Type != "room" {
		return "", false
	}
	if src == "" {
		return "", false
	}
	if len(lineAllowedGroups) > 0 && !contains(lineAllowedGroups, src) {
		return "", false
	}

	// 名指しされたときだけ答える。
	// ⚠ @all には反応しない（グループ全体宛てに割り込むため）。
	if ev.Message.Mention == nil {
		return "", false
	}
	named := false
	for _, m := range ev.Message.Mention.Mentionees {
		if m.Type == "all" {
			continue
		}
		if m.IsSelf {
			named = true
		}
	}
	if !named {
		return "", false
	}

	if lineSeenBefore(ev.WebhookEventID) {
		lineCountDup.Add(1)
		return "", false
	}
	if !lineLimiter.allow(src) {
		// 毎回「制限中です」と返すと、それ自体が騒がしい。黙って捨てる。
		return "", false
	}

	q := stripMentions(ev.Message.Text, ev.Message.Mention.Mentionees)
	if q == "" {
		return msgUsage, true // 呼ばれただけ。使い方を返す
	}
	return q, true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// stripMentions は本文から名指し部分を取り除く。
//
// ⚠ index / length は **UTF-16 コード単位**。Go の byte 位置でも rune 位置でもない。
//
//	日本語や絵文字が入ると素直な slice ではずれるので、UTF-16 に直してから削る。
//
// ⚠ 後ろから消すこと。前から消すと後続の位置がずれる。
func stripMentions(text string, ms []lineMentionee) string {
	u := utf16.Encode([]rune(text))

	// index の降順に並べ替える（挿入ソートで足りる。数は多くて数個）。
	idx := make([]lineMentionee, len(ms))
	copy(idx, ms)
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && idx[j].Index > idx[j-1].Index; j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}

	for _, m := range idx {
		start, end := m.Index, m.Index+m.Length
		if start < 0 || end > len(u) || start > end {
			continue // 壊れた位置は触らない
		}
		u = append(u[:start], u[end:]...)
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

// ---- ワーカー ----

// recoverLine は panic でプロセスごと落ちるのを防ぐ。
//
// ⚠ net/http はハンドラ内の panic を拾うが、**自前で立てた goroutine の panic は拾わない**。
// 信頼できない外部 JSON を扱う以上、ここは必ず要る。
func recoverLine(where string) {
	if v := recover(); v != nil {
		log.Printf("LINE: %s の処理で panic した（復帰する）: %v", where, v)
	}
}

func lineWorker() {
	for job := range lineJobs {
		runLineJob(job)
	}
}

func runLineJob(job lineJob) {
	defer recoverLine("応答")

	// ⚠ キューに積めたことは、間に合うことを意味しない。
	//    ワーカーは1本なので、前の生成が長引けば取り出した時点で既に古い。
	//    生成を始める前に、必ず経過時間を見る。
	elapsed := time.Since(job.at)
	if elapsed >= lineReplyDeadline {
		if elapsed < lineTokenSafe {
			// まだトークンは生きている。混雑を伝えて畳む。
			lineCountBusy.Add(1)
			lineReply(job.replyToken, msgBusy)
		} else {
			// トークンが死ぬ手前。送っても弾かれるので捨てる。
			log.Printf("LINE: 順番待ちが長く、返信を諦めた 待ち=%.1fs", elapsed.Seconds())
		}
		return
	}

	// 生成に与える持ち時間は「45秒」ではなく **45秒 − 待たされた時間**。
	text, route, truncated, err := askChatAI(job.question, lineReplyDeadline-elapsed)
	if err != nil {
		log.Printf("LINE: 後段への問い合わせに失敗した: %v", err)
	}
	if truncated {
		lineCountCutoff.Add(1)
	}
	if text == "" {
		// 45秒あって1文字も出ないのは故障。遅れて本文を届ける場面ではない。
		text = msgEmpty
	}
	lineReply(job.replyToken, text)
	lineCountReply.Add(1)
	log.Printf("LINE: 応答 route=%s 所要=%.1fs 打ち切り=%v", route, time.Since(job.at).Seconds(), truncated)
}

// askChatAI は後段へ問い、NDJSON を1本の文字列に畳んで返す。
//
// ⚠ 既存の stream() を流用しない。あれはブラウザ向けに SSE へ変換する関数で、
// LINE には要らない（SSE を経由する意味がない）。
func askChatAI(question string, budget time.Duration) (text, route string, truncated bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	// ⚠ 受け取った JSON を転送せず、ここで組み直す。
	//    allow_llm はサーバー側の設定値から。system / temperature は**送らない**
	//    （後段は system が空でなければ人格定義を丸ごと差し替えるため）。
	body, err := json.Marshal(upstreamRequest{
		chatRequest: chatRequest{Messages: []message{{Role: "user", Content: question}}},
		AllowLLM:    allowLLM,
	})
	if err != nil {
		return "", "none", false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, aiBase+"/chat", bytes.NewReader(body))
	if err != nil {
		return "", "none", false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "none", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "none", false, errors.New("後段が " + resp.Status + " を返した")
	}

	var b strings.Builder
	route = "none"
	sc := bufio.NewScanner(resp.Body)
	// 1行が長くなることがあるので既定の 64KB から広げる。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Route   string `json:"route"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue // 壊れた行は落とす。生成そのものは続ける
		}
		switch ev.Type {
		case "token":
			b.WriteString(ev.Text)
		case "done":
			if ev.Route != "" {
				route = ev.Route
			}
		case "error":
			return ev.Message, "none", false, nil
		}
		// "status"（「生成中…」）は画面向けの通知なので使わない
	}
	// 締切で打ち切られたか。ここまでの本文はそのまま活かす。
	if ctx.Err() != nil {
		truncated = true
	}

	text = strings.TrimSpace(b.String())
	if truncated && text != "" {
		text += msgCutoff
	}
	return text, route, truncated, sc.Err()
}

// ---- LINE への送信 ----

func lineTruncate(s string) string {
	r := []rune(s)
	if len(r) <= lineTextLimit {
		return s
	}
	return string(r[:lineTextLimit]) + "…（以下省略）"
}

type lineMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// lineReply は replyToken で返す。**無料・無制限**。応答は必ずこちら。
func lineReply(replyToken, text string) {
	if replyToken == "" || strings.TrimSpace(text) == "" {
		return // 空文字は LINE が 400 を返す
	}
	payload := map[string]any{
		"replyToken": replyToken,
		"messages":   []lineMessage{{Type: "text", Text: lineTruncate(text)}},
	}
	linePost(lineAPIReply, payload, "reply")
}

// linePush は宛先を指定して送る。**通数を消費する**ので応答には使わない（§3.14）。
func linePush(to, text string) error {
	if to == "" || strings.TrimSpace(text) == "" {
		return errors.New("宛先と本文が要ります")
	}
	payload := map[string]any{
		"to":       to,
		"messages": []lineMessage{{Type: "text", Text: lineTruncate(text)}},
	}
	return linePost(lineAPIPush, payload, "push")
}

func linePost(url string, payload any, kind string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+lineAccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("LINE: %s の送信に失敗した: %v", kind, err)
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	if resp.StatusCode >= 300 {
		// ⚠ 応答本文には利用者の文面が混ざりうるので出さない。状態だけ。
		log.Printf("LINE: %s が拒否された status=%d", kind, resp.StatusCode)
		return errors.New("LINE が " + resp.Status + " を返した")
	}
	return nil
}

// ---- 発信の口（§3.14）----
//
// Bot から話しかける機能そのものは本書のスコープ外。継ぎ目だけ用意しておく。
//
// ⚠ Caddy には通さないこと。公開するのは /line/* だけ。
// 外から叩ければ、誰でもこの Bot の名前で任意のメッセージを送れることになる。
func handleInternalPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST で送ってください")
		return
	}
	if !hmac.Equal([]byte(r.Header.Get("X-Push-Token")), []byte(linePushToken)) {
		writeJSONError(w, http.StatusUnauthorized, "合言葉が違います")
		return
	}
	var req struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, lineMaxPushBody)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "リクエストの形式が正しくありません")
		return
	}
	if err := linePush(req.To, req.Text); err != nil {
		writeJSONError(w, http.StatusBadGateway, "送信できませんでした")
		return
	}
	// ⚠ 宛先も本文もログに出さない。
	log.Printf("LINE: push を1通送った")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write([]byte("{\"status\":\"sent\"}\n"))
}
