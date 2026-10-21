package main

import (
	"strings"
	"testing"
)

// stripMentions は LINE が寄越す index / length を使って名指し部分を削る。
//
// ここが本文取り出しの要で、間違えやすい所が2つある。
//   - index / length は **UTF-16 コード単位**。Go の byte でも rune でもない。
//     日本語や絵文字（サロゲートペア）が前にあると、素直な slice ではずれる。
//   - **後ろから消す**必要がある。前から消すと後続の位置がずれる。
//
// 「消せたか」ではなく「どこを消したか」を確かめたいので、
// 期待値は削り取った後の文字列そのもので書く。
func TestStripMentions(t *testing.T) {
	tests := []struct {
		name string
		text string
		ms   []lineMentionee
		want string
	}{
		{
			name: "先頭の名指しを削る",
			text: "@こてつ 東京の天気は？",
			// "@こてつ" は UTF-16 で 4 単位（@ こ て つ）
			ms:   []lineMentionee{{Index: 0, Length: 4, Type: "user", IsSelf: true}},
			want: "東京の天気は？",
		},
		{
			name: "絵文字（サロゲートペア）が前にあっても位置がずれない",
			// 🐱 は UTF-16 で 2 単位。rune 数で数えると 1 になり、ここで1つずれる。
			text: "🐱@こてつ ping",
			ms:   []lineMentionee{{Index: 2, Length: 4, Type: "user", IsSelf: true}},
			want: "🐱 ping",
		},
		{
			name: "名指しが複数あっても後ろから消せている",
			// "@a"=2, " "=1, "@こてつ"=4
			text: "@a @こてつ ping",
			ms: []lineMentionee{
				{Index: 0, Length: 2, Type: "user"},
				{Index: 3, Length: 4, Type: "user", IsSelf: true},
			},
			want: "ping",
		},
		{
			name: "与えられた順が昇順でなくても結果は同じ",
			text: "@a @こてつ ping",
			ms: []lineMentionee{
				{Index: 3, Length: 4, Type: "user", IsSelf: true},
				{Index: 0, Length: 2, Type: "user"},
			},
			want: "ping",
		},
		{
			name: "文の途中の名指しも削れる",
			text: "おい @こてつ 起きろ",
			// "おい "=3, "@こてつ"=4
			ms:   []lineMentionee{{Index: 3, Length: 4, Type: "user", IsSelf: true}},
			want: "おい  起きろ",
		},
		{
			name: "名指しだけなら空になる（呼ばれただけ）",
			text: "@こてつ",
			ms:   []lineMentionee{{Index: 0, Length: 4, Type: "user", IsSelf: true}},
			want: "",
		},
		{
			name: "範囲外の位置は無視して本文を壊さない",
			text: "@こてつ ping",
			ms:   []lineMentionee{{Index: 99, Length: 5, Type: "user", IsSelf: true}},
			want: "@こてつ ping",
		},
		{
			name: "負の位置も無視する",
			text: "@こてつ ping",
			ms:   []lineMentionee{{Index: -1, Length: 3, Type: "user", IsSelf: true}},
			want: "@こてつ ping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripMentions(tt.text, tt.ms); got != tt.want {
				t.Errorf("stripMentions(%q)\n  得た値: %q\n  期待値: %q", tt.text, got, tt.want)
			}
		})
	}
}

// lineTruncate は LINE の 5000 文字上限に収める。
// ⚠ rune で数えること。byte で切るとマルチバイト文字の途中で割れて壊れる。
func TestLineTruncate(t *testing.T) {
	short := "みじかい"
	if got := lineTruncate(short); got != short {
		t.Errorf("上限内は変えないこと: %q", got)
	}

	long := strings.Repeat("あ", lineTextLimit+500)
	got := lineTruncate(long)
	if !strings.HasSuffix(got, "…（以下省略）") {
		t.Errorf("切ったことを示す語尾が無い: ...%q", string([]rune(got)[len([]rune(got))-10:]))
	}
	if n := len([]rune(strings.TrimSuffix(got, "…（以下省略）"))); n != lineTextLimit {
		t.Errorf("本文が %d 文字。%d 文字であるべき", n, lineTextLimit)
	}
	// 5000 文字（LINE の上限）を超えていないこと。
	if n := len([]rune(got)); n > 5000 {
		t.Errorf("切った後も上限超過: %d 文字", n)
	}
}

// splitCSV は許可グループの読み取りに使う。空要素や空白で足をすくわれないこと。
func TestSplitCSV(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"C123", []string{"C123"}},
		{" C123 , C456 ", []string{"C123", "C456"}},
		{"C123,,C456,", []string{"C123", "C456"}},
	}
	for _, tt := range tests {
		got := splitCSV(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("splitCSV(%q) = %v, 期待 %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitCSV(%q) = %v, 期待 %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

// 冪等性。同じ webhookEventId が二度来ても、二度目は捨てる。
// LINE の再送は既定オフだが、有効にした瞬間に二重返信が出る。
func TestLineSeenBefore(t *testing.T) {
	if lineSeenBefore("evt-1") {
		t.Error("初回は未見のはず")
	}
	if !lineSeenBefore("evt-1") {
		t.Error("二度目は既見として捨てるはず")
	}
	if lineSeenBefore("evt-2") {
		t.Error("別のイベントは未見のはず")
	}
	// id が空のときは覚えようがない。覚えた気にならないこと
	// （覚えると、id を持たないイベントが一括で捨てられる）。
	if lineSeenBefore("") || lineSeenBefore("") {
		t.Error("空の id は常に未見として扱うはず")
	}
}

// レート制限は groupId 単位で効く。
// ⚠ IP 単位にすると、webhook は LINE のサーバから来るのでバケツが1つに潰れる。
func TestLimiterPerKey(t *testing.T) {
	l := newLimiter(60, 2) // 毎分60（=毎秒1）・バースト2

	if !l.allow("G1") || !l.allow("G1") {
		t.Fatal("バースト分は通るはず")
	}
	if l.allow("G1") {
		t.Error("バーストを使い切ったら止めるはず")
	}
	// 別のグループは影響を受けない（ここが IP 単位との違い）。
	if !l.allow("G2") {
		t.Error("別グループは別のバケツであるはず")
	}
}

// ---- 答え方の選択（answer_mode）----
//
// 画面から「規則を優先／LLMだけ／規則だけ」を選ぶ。
// ⚠ 前段でやるのは**綴りの検め**だけ。知らない値で会話そのものを失敗させない
//
//	（後段は Literal で検証するので、素通しすると 422 になる）。
func TestNormalizeAnswerMode(t *testing.T) {
	tests := []struct{ in, want string }{
		{"auto", "auto"},
		{"llm", "llm"},
		{"rules", "rules"},
		{"", "auto"},              // 指定なしは既定
		{"LLM", "auto"},           // 綴り違いは既定へ（大文字小文字は区別する）
		{"rules_only", "auto"},    // 古い画面が送ってきても壊さない
		{"'; DROP TABLE", "auto"}, // 素性の知れない値も既定へ
	}
	for _, tt := range tests {
		if got := normalizeAnswerMode(tt.in); got != tt.want {
			t.Errorf("normalizeAnswerMode(%q) = %q, 期待 %q", tt.in, got, tt.want)
		}
	}
}

// 答え方は**権限ではない**。閉じてある口を開く力は無いこと。
//
// allow_llm（設置側の設定）が偽なら、"llm" を選んでも後段で LLM は動かない。
// 前段は allow_llm をそのまま送るだけで、答え方に応じて書き換えたりしない——
// ここで両者を混ぜると、「答え方の選択で権限が動く」道が生まれる。
func TestAnswerModeIsNotAPermission(t *testing.T) {
	for _, mode := range []string{"auto", "llm", "rules", "でたらめ"} {
		req := &chatRequest{AnswerMode: mode}
		req.AnswerMode = normalizeAnswerMode(req.AnswerMode)
		if req.AnswerMode != "auto" && req.AnswerMode != "llm" && req.AnswerMode != "rules" {
			t.Errorf("検めた後に知らない値が残った: %q", req.AnswerMode)
		}
	}
}

// ---- 投稿の頭で答え方・モデルを指定する（LINE_PREFIXES）----
//
// LINE には画面が無いので、選ばせるにはこれしか手が無い。
//
//	「規 東京の天気は？」→ 規則だけで答える
//	「低 むかし話をして」→ 小さいモデルで答える
func TestParseLinePrefixes(t *testing.T) {
	got := parseLinePrefixes(" 規=@rules, 低=gemma3:1b ,, 高=qwen2.5:3b , こわれ ")
	want := map[string]string{"規": "@rules", "低": "gemma3:1b", "高": "qwen2.5:3b"}
	if len(got) != len(want) {
		t.Fatalf("読めた数が違う: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q → %q, 期待 %q", k, got[k], v)
		}
	}
	// 未設定なら空。指定の仕組みごと無いことになる。
	if len(parseLinePrefixes("")) != 0 {
		t.Error("未設定なら空であるはず")
	}
}

func TestTakeLinePrefix(t *testing.T) {
	saved := linePrefixes
	linePrefixes = map[string]string{"規": "@rules", "低": "gemma3:1b"}
	defer func() { linePrefixes = saved }()

	tests := []struct {
		name              string
		in                string
		mode, model, rest string
	}{
		{"答え方を指定", "規 東京の天気は？", "rules", "", "東京の天気は？"},
		{"モデルを指定", "低 むかし話をして", "", "gemma3:1b", "むかし話をして"},
		{"全角の空白でも切れる", "規　東京の天気は？", "rules", "", "東京の天気は？"},
		{"指定が無ければ本文のまま", "東京の天気は？", "", "", "東京の天気は？"},
		{"知らない字は本文のまま", "超 東京の天気は？", "", "", "超 東京の天気は？"},

		// ⚠ 頭の一語だけを見る。文中に現れても反応してはいけない。
		{"文中の同じ字には反応しない", "規則について教えて", "", "", "規則について教えて"},
		{"文の途中の字にも反応しない", "これは規 という字だ", "", "", "これは規 という字だ"},

		// ⚠ 取り除いた結果が空になるなら、それは指定ではなく本文とみなす。
		{"指定だけで本文が無ければ本文扱い", "規", "", "", "規"},
		{"指定の後が空白だけでも本文扱い", "規   ", "", "", "規   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, model, rest := takeLinePrefix(tt.in)
			if mode != tt.mode || model != tt.model || rest != tt.rest {
				t.Errorf("takeLinePrefix(%q)\n  得た値: mode=%q model=%q rest=%q\n  期待値: mode=%q model=%q rest=%q",
					tt.in, mode, model, rest, tt.mode, tt.model, tt.rest)
			}
		})
	}
}

// 指定の仕組みが無い設置では、本文が一切変わらないこと。
// ⚠ ここが崩れると、頭の一語が黙って消える設置が生まれる。
func TestTakeLinePrefixDisabled(t *testing.T) {
	saved := linePrefixes
	linePrefixes = map[string]string{}
	defer func() { linePrefixes = saved }()

	for _, in := range []string{"規 東京の天気は？", "低 むかし話", "ふつうの質問"} {
		mode, model, rest := takeLinePrefix(in)
		if mode != "" || model != "" || rest != in {
			t.Errorf("未設定なのに解釈した: %q → %q/%q/%q", in, mode, model, rest)
		}
	}
}
