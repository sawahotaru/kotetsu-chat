# 内容ファイル（差し替える所）

このフォルダが**この案内役の中身のすべて**。ここを書き換えれば別人になる。
コードには文言を一切書かない。

> ⚠️ **この人格（黒猫武将こてつ）は、配布元の沢蛍由来のものです。**
> 自分の設置で使うなら差し替えてください。
> 名前・口調・回答・小噺・画面の文言・顔の絵まで、**すべてこのフォルダの中**にあります。
> まっさらから始めるなら [`examples/content-minimal/`](../../../content-minimal/) を写して使ってください。

| ファイル | 中身 |
|---|---|
| `persona.yaml` | 名前・この設置の URL・LLM への指示・心得の外のときの返し |
| `intents.yaml` | 意図の一覧（例文・誤爆止め・回答） |
| `trivia.yaml` | 小噺。「何か面白い話を」に順繰りで返す |

置き場所は `CHAT_CONTENT_DIR` で差し替えられる。既定は `apps/chat-ai/content/`。
最小の見本は [`examples/content-minimal/`](../../../content-minimal/)。

```bash
# 自分の内容で動かす
cp -r examples/content-minimal my-content
$EDITOR my-content/*.yaml
CHAT_CONTENT_DIR=/path/to/my-content docker compose up -d chat-ai
```

## 目印

回答文の中で使える。

| 目印 | 置き換わるもの |
|---|---|
| `{public}` | `persona.yaml` の `site.public_url` |
| `{source}` | `persona.yaml` の `site.source_url` |
| `〔LLM〕` | LLM を使う設置かどうかで文面が変わる（`persona.yaml` の `llm_note`） |
| `〔LLM:名前〕` | 同上。ただし**長い文**。`llm_note` の中に入れ子で書いたものを呼ぶ |

`〔LLM〕` があるのは、**同じ内容を複数の口が共有する**ため。
Web は LLM 無し・LINE は有りといった設置で、文面を1つに固定すると
**どちらかで嘘になる**。使う側の設置に合わせて差し替わるようにしてある。

`〔LLM:名前〕` は、**答えが一文ではなく丸ごと入れ替わる**とき用。
「なぜ LLM を使っていないのか」がその例で、有りの設置と無しの設置とで
書くことがまるきり違う。書き方:

```yaml
llm_note:
  enabled: 心得ぬ問いは小さな LLM に回しておる      # 〔LLM〕
  disabled: この窓口では LLM を使っておらぬ         # 〔LLM〕
  reason:                                      # 〔LLM:reason〕
    enabled: |-
      いまは灯しておる。なれど主役ではない。……
    disabled: |-
      切ってある理由は重さではない。……
```

⚠ `persona.yaml` に無い名前を書くと、`〔LLM:typo〕` が**そのまま画面に出る**
（黙って消すと書き間違いに気づけないため、わざとそうしてある）。
`test_router.py` の `test_no_slot_is_left_unresolved` が拾う。

## 意図の書き方

```yaml
- name: hours                      # 記録に出る名前。重複させない
  examples: [営業時間は, 何時まで]   # 該当する言い方。多いほど当たりやすい
  must: [営業, 何時]                # このどれかを含まないと採用しない（誤爆止め）
  priority: 2                      # 僅差の取り合いを決める。閾値は跨がせない
  answer: |                        # 定型の回答。Markdown が使える
    **平日** 9:00〜19:00
    ご予約は {public} から。
```

**`must` は誤爆止めであって、当たりやすくする道具ではない。**
ここに書いた語を一つも含まない問いは、例文にどれだけ似ていても採用されない。
`must` を書き忘れると別の意図を奪い、書きすぎると自分の意図に届かなくなる。

判定は文字の重なり（N-gram）で行う。形態素解析も学習済みの模型も使わない。
軽くて表記ゆれに強い代わりに、**語が長いほど薄まる**。
「織田信長とは」のような長い語は閾値を割るので、そういう意図には `matcher` を使う。

## 動的に答える意図

`answer` の代わりに `handler` を書くと、その名前の処理が呼ばれる。

| 名前 | 中身 | 備考 |
|---|---|---|
| `trivia` | `trivia.yaml` を順に返す | 枠組みが持つ。常に使える |
| `weather` | 気象庁／Open-Meteo | 日本の地名は全都道府県に対応 |
| `wikipedia` | 日本語版 Wikipedia の要約 | `matcher: wikipedia` と併せて使う |
| `products` `clinic` | この lab の店・予約システムへの問い合わせ | **設置ごとの追加**。無ければ意図ごと外れる |

⚠ **登録されていない処理を参照した意図は、起動時に外され記録に残る。**
店を持たない設置で商品検索の意図が消えるのは正しい振る舞いで、
押しても何も起きない案内を残すほうが不親切だから。

### 自分の処理を足す

`register()` を持つ Python ファイルを**このフォルダに置く**。

```python
# my-content/my_shop.py
async def search(text: str) -> str:
    ...                              # 好きに調べて、文字列を返すだけ
def register() -> dict:
    return {"products": search}      # ← intents.yaml の handler: products と対応
```

`CHAT_SITE_HANDLERS=my_shop` と書けば読み込まれる（複数ならカンマ区切り）。

内容ファイルと同じフォルダに置く理由は、**連携は「この設置の中身」の一部**だから。
人格も回答も差し替えるのに、在庫の引き方だけイメージを焼き直さねばならぬのは筋が悪い。
このフォルダを丸ごと差し替えれば、繋ぎ先ごと入れ替わる。

⚠ 名前を書いたのに置き忘れていれば、**起動時に探した場所を添えて止まる**。
⚠ 同梱と同じ**モジュール名**（`weather.py` など）を置いても**同梱が勝つ**。
  内容を差し替えただけで天気の引き方が入れ替わっては困るため。

### 同梱の処理を差し替える・外す

天気（`weather`）と調べ物（`wikipedia`）は同梱されているが、**枠組みの一部ではない**。
どちらも他人のサービスに触る処理なので、名前で読み込む物として扱っている。
枠組みが常に持つのは小噺（`trivia`）だけ。

```sh
CHAT_HANDLERS=wikipedia              # 天気を外す
CHAT_HANDLERS=                       # 全部外す（決め打ちの答えだけで動く）
```

差し替えるときは **先に外してから名乗る**。

```sh
CHAT_HANDLERS=wikipedia              # 同梱の weather を外し
CHAT_SITE_HANDLERS=my_weather        # 自分のを register() で "weather" として名乗る
```

⚠ 外さずに同じ名前を名乗ると**起動時に止まる**。どちらが効いているか分からない状態を作らないため。

🚨 **外したら案内文も直すこと。** 意図は自動で落ちるが、**案内文は静的なテキストなので残る**。
同梱の `intents.yaml` は「天気であれば申せる」「東京の天気は？と聞かれよ」と促しているので、
天気を外したまま案内文を残すと、**自分で促しておいて答えられぬ**という一番間の抜けた壊れ方になる。

## 書き換えたら確かめる

意図を足すと既存が壊れる。特に**誤爆**（新しい意図が既存の問いを奪う）は気づきにくい。

```bash
docker compose exec -T chat-ai python -m pytest -q
```

期待する行き先を並べたものが `test_router.py` にある。
**自分の内容に合わせて書き換えて使うこと。** 過去に踏んだ事故もそのまま残してある。

意図を足したのに一覧へ足し忘れる、が一番起きる。
`test_every_intent_is_covered` が**試験の無い意図を見つけて落ちる**ので、
足したら必ず一つは書くことになる。

⚠ `CASES` は**同梱の内容に向けて書いてある**。自分の内容に差し替えると、
その一覧は理由を告げて**丸ごと飛ばされる**（`-rs` で理由が出る）。
枠組み自体の試験は走り続けるので、「飛ばされた ＝ 壊した」ではない。

## 書くときの心構え

- **事実は必ずここに書く。** 料金・URL・技術構成のような答えの決まっているものを
  LLM に推測させない。小さな模型は平気で嘘をつく
- **無いものを「ある」と書かない。** 実装していない機能を案内すると、
  それは商品の嘘と同じになる
- **できないことを先に宣言する。** 計算・翻訳・最新の出来事のように
  間違えると相手が困るものは、`cant_do` のような意図で正直に断る
- **医療・法律・金銭の相談は受けない。** 断ったうえで、できることへ寄せる

## 画面（`persona.yaml` の `ui:`）

表題・肩書き・戻り先・名乗り・質問例・顔は、すべてここで決まる。
前段（Go）は器のままで、起動時に `/api/persona` から取りに行く。

```yaml
ui:
  title: みどり整体院 ご案内     # ブラウザのタブ
  heading: みどり整体院          # 画面上部
  badge: ""                     # 空にすると出ない
  home: {url: "", label: ""}    # 空にすると戻りリンクを出さない
  avatar: avatar.jpg            # この content/ 内のファイル名。空なら顔ごと消える
  intro:
    heading: ご案内
    lead: |                     # Markdown 可
      みどり整体院の受付です。
    ability: お答えできるのは**営業時間・ご予約**です。
    prompt: 下のボタンを押すか、ご質問を入力してください。
    note:                       # LLM の有無で変わる一文
      enabled: 決められた答えが無いご質問は、AI がお答えします。
      disabled: 決められた答えのあるご質問にのみお答えしています。
  examples: [営業時間は？, 予約したい]
```

顔の画像は `content/` に置いて `avatar:` にファイル名を書く。
**頁に焼き込まない**ので、画像を置き換えるだけで顔が変わる。
`avatar: ""` にすると丸い枠ごと消える（顔が無くても成り立つ作りにしてある）。

### ボタンと報せの文言（`ui.labels`）

「送信」「会話をクリア」といった画面の道具の文言。
**書かなくてよい。** 書かなければ前段が持つ既定（日本語）が出る。
書いた分だけが上書きされる。

案内役の口調に合わせたいとき、あるいは**別の言語で出したい**ときに使う。

```yaml
ui:
  labels:
    send: Send
    stop: Stop
    clear: Clear conversation
    examples: Examples
    inputPlaceholder: Type a message (Enter to send / Shift+Enter for a new line)
```

| 鍵 | 出る所 |
|---|---|
| `send` / `stop` | 送信ボタン（生成中は `stop` に変わる） |
| `clear` / `examples` / `home` | 画面下の帯と、戻りリンクの既定 |
| `systemPrompt` / `systemPromptHint` | システムプロンプトの開閉と説明（LLM 有効時のみ） |
| `inputPlaceholder` | 入力欄の薄い文字 |
| `modeLlm` / `modeRules` | `mode_label` を書かなかったときの既定 |
| `routeRule` / `routeNoneName` / `routeNone` / `seconds` | 吹き出しの下の「どちらが答えたか」 |
| `error` / `emptyAnswer` / `stopped` / `cleared` / `restored` | 会話中の報せ |
| `ollamaDown` / `modelPulling` / `modelsFailed` / `modelsFailedOption` | モデルまわりの報せ（LLM 有効時のみ） |

⚠ **これは本格的な多言語化ではない。** 一つの設置につき一つの言語という前提で、
文言を差し替えられるようにしただけ。閲覧者ごとに切り替える仕掛けは無い。
見本は [`examples/content-minimal/persona.yaml`](../../../content-minimal/persona.yaml)。

### 天気・調べ物の情報源（日本の外へ配るとき）

枠組みが持つ `weather` と `wikipedia` は、**既定が日本向け**になっている
（国内は気象庁、調べ物は日本語版 Wikipedia）。切り離す口がある。

| 環境変数 | 既定 | 何が変わるか |
|---|---|---|
| `WEATHER_SOURCE` | `auto` | `open-meteo` にすると気象庁を使わず、全部 Open-Meteo で引く |
| `WIKIPEDIA_LANG` | `ja` | 引く言語版（`en` なら英語版） |
| `WIKIPEDIA_SOURCE_LABEL` | 自動 | 出典の名乗り。`ja` のときは「日本語版 Wikipedia」 |
| `CHAT_USER_AGENT` | lab のもの | 外部を叩くときの名乗り。**配るなら必ず書き換える** |

引ける地名は `content/` に `places.yaml` を置いて足す。
Open-Meteo のジオコーディングは日本語の地名を通さないので、座標を自分で書く。

```yaml
# content/places.yaml   名前: [緯度, 経度, タイムゾーン]
ミュンヘン: [48.1374, 11.5755, Europe/Berlin]
Munich:     [48.1374, 11.5755, Europe/Berlin]   # 呼び方ごとに書いてよい
```

書き方を誤ると**起動時に行番号ではなく地名付きで止まる**（黙って引けないままにしない）。

⚠ **これは情報源の切り離しであって、多言語化ではない。**
天気の言い表し（「くもり」「明後日」）や調べ物の前後の言い回しは
`weather.py` / `wikipedia.py` の日本語のまま。
別の言語で配るなら、そちらも合わせて直す必要がある。

⚠ `WEATHER_SOURCE=open-meteo` にすると、都道府県名では引けなくなる
（`places.yaml` に書いた地名だけになる）。
「引けるふり」をしないよう、案内の文面もそれに合わせて変わる。

### ⚠ YAML に `: `（コロン＋空白）を書くとき

値の中にコロンと空白が続くと、YAML はそこを構造の区切りと解釈して壊れる。

```yaml
mode_label:
  enabled: 答え方: 規則で即答      # ✗ 壊れる
  enabled: "答え方: 規則で即答"    # ○ 引用符で囲む
```

壊れていれば**起動時に行番号付きで止まる**ので、黙って動き続けることはない。
