# チャットを単体で動かす

規則で即答するチャットボット。**Docker があれば `docker compose up -d` だけで動く**。

```bash
cd examples/standalone
docker compose up -d --build
# → http://127.0.0.1:8080/
```

止めるのは `docker compose down`。

---

## 何ができるものか

**よくある問いを、安く・速く・正しく捌く**ための道具。賢さの担当ではない。

答え方は二段になっている。

1. **規則**（既定・これだけで動く） — 質問を意図に分類し、書いてある答えを返す。
   LLM を起こさないので CPU をほぼ使わず、**答えは常に書いたとおり**
2. **LLM**（任意） — 1 で拾えなかったものだけ ollama へ回す

この順序が肝心で、**LLM は最後の手段**として置いてある。
料金・営業時間・技術構成のように答えが決まっているものを 1B のモデルに推測させる
理由は無い（平気で嘘をつく）。

そのため **既定では LLM を使わない**。ollama も起動しない
（イメージ **約7GB** もモデル約1.8GB も置かれない）。
規則だけで動かすなら、必要なのは **RAM 320MB 程度**とディスク数百MB。

### 中身と枠組みが分かれている

コードには文言が一行も無い。人格・回答・画面の文言・顔は
すべて [`content/`](../apps/chat-ai/content/) の YAML にある。
**コードを触らずに別物にできる**（見本: [`../content-minimal/`](../content-minimal/) ＝ 整体院の受付）。

> ⚠️ 同梱の人格（黒猫武将こてつ）は**配布元の沢蛍由来のもの**。
> そのまま動かして試すのは構わないが、**自分の設置で使うなら差し替えること**。

---

## 構成

```
  ブラウザ ──▶ chat-gateway (Go)  ──▶ chat-ai (Python)  ──▶ ollama（任意）
   :8080        画面を配る            意図分類して規則で答える     LLM
                レート制限            拾えなければ LLM へ回す
                順番待ち              content/ の YAML を読む
                入力長の上限
```

- 外に出るのは **前段だけ**。後段に `ports` を付けてはならない
  （直接叩けると前段のレート制限を素通りされる）
- 会話は**サーバーに残さない**。控えはブラウザの中だけ

---

## 自分の内容に差し替える

```bash
cp -r ../content-minimal ./my-content
$EDITOR my-content/persona.yaml   # 名前・URL・画面の文言
$EDITOR my-content/intents.yaml   # 意図（例文・誤爆止め・回答）
$EDITOR my-content/trivia.yaml    # 小噺

cp .env.example .env
# .env に2行そろえて書く:
#   CHAT_CONTENT_HOST_DIR=./my-content
#   CHAT_CONTENT_DIR=/content
docker compose up -d
```

書き方は [`apps/chat-ai/content/README.md`](../apps/chat-ai/content/README.md) に全部ある。
顔の画像を置きたいなら `persona.yaml` の `ui.avatar` にファイル名を書き、
同じフォルダに置く（空にすれば顔の要素ごと消える）。

**書き換えたら必ず確かめること。** 意図を足すと既存が壊れる。
特に誤爆（新しい意図が既存の問いを奪う）は気づきにくい。

```bash
docker compose exec -T chat-ai python -m pytest -q
```

⚠ 意図分類の一覧（`test_router.py` の `CASES`）は**同梱の内容に向けて書いてある**。
自分の内容に差し替えると、その一覧は**理由を告げて丸ごと飛ばされる**
（`-rs` を付けると理由が出る）。枠組み自体の試験はそのまま走り続けるので、
「飛ばされた ＝ 壊した」ではない。
**自分の内容に合わせて `CASES` を書き換えて使うこと。**

### 自分の在庫・予約システムに繋ぐ

天気・調べ物・小噺は枠組みが持っている。
それ以外（在庫照会など）は **設置ごとの追加**として Python モジュールを足す。

```python
# my_shop.py
async def search(text: str) -> str:
    ...                       # 好きに調べて、文字列を返すだけ
def register() -> dict:
    return {"products": search}    # ← intents.yaml の handler: products と対応
```

**置き場所は内容ファイルの隣**（`CHAT_CONTENT_DIR` の中）。連携は
「この設置の中身」の一部なので、人格や回答と同じフォルダに置く。

```
my-content/
  persona.yaml
  intents.yaml
  my_shop.py      ← ここ
```

`.env` に3行:

```
CHAT_CONTENT_HOST_DIR=./my-content
CHAT_CONTENT_DIR=/content
CHAT_SITE_HANDLERS=my_shop
```

`intents.yaml` 側で `handler: products` と書けば、この処理が呼ばれる。
見本は [`examples/site-handler/my_shop.py`](../examples/site-handler/my_shop.py)、
読み込みの作法は [`plugins.py`](../apps/chat-ai/plugins.py)。

⚠ 名前を書いたのに置き忘れていれば、**起動時に探した場所を添えて止まる**
（黙って動き続けない）。

⚠ **追加を持たない設置では、それを参照する意図が起動時に自動で外れる。**
押しても何も起きない案内を残すほうが不親切なため。

### 日本の外へ配るとき

天気と調べ物は**既定が日本向け**（国内は気象庁、調べ物は日本語版 Wikipedia）。
`.env` で切り離せる。

```
WEATHER_SOURCE=open-meteo    # 気象庁を使わず全部 Open-Meteo で引く
WIKIPEDIA_LANG=en            # 英語版を引く
CHAT_USER_AGENT=my-site chat (+https://example.com/)   # ⚠ 必ず書き換える
```

引ける地名は `content/places.yaml` に `名前: [緯度, 経度, タイムゾーン]` で足す。

⚠ **情報源の切り離しであって、多言語化ではない。** 天気の言い表しなどは
日本語のまま残る。詳しくは
[`content/README.md`](../apps/chat-ai/content/README.md)。

---

## 設定

全項目と既定値は [`.env.example`](../.env.example) にある。よく触るのはこのあたり。

| 項目 | 既定 | 何を決めるか |
|---|---|---|
| `CHAT_PORT` | `8080` | 待ち受けるポート |
| `CHAT_BIND` | `127.0.0.1` | 待ち受ける宛先。⚠ 下の注意を読むまで変えない |
| `CHAT_CONTENT_DIR` | （空＝同梱） | 中身の置き場所。`/content` と書くと差し替わる |
| `CHAT_SITE_HANDLERS` | （空） | 設置ごとの追加処理 |
| `CHAT_RATE_PER_MIN` / `CHAT_RATE_BURST` | `12` / `5` | IP ごとのレート制限 |
| `CHAT_MAX_MESSAGE_CHARS` | `2000` | 1通の長さの上限 |
| `CHAT_MAX_HISTORY` | `8` | 後段へ送る往復数 |
| `CHAT_LLM_ENABLED` | `0` | 後段が LLM を使える状態か |
| `CHAT_ALLOW_LLM` | `0` | この口から LLM を使わせるか |
| `CHAT_MODELS` | `qwen2.5:3b,gemma3:1b` | 使ってよいモデル（許可リスト）。先頭が既定。3B なのは口調を保てるため（⚠ 常駐前提） |

真偽の項目は `1/0` のほか `true/false`・`yes/no`・`on/off` も使える
（前段・後段で解釈は揃えてある）。
⚠ **読めない値を書くと起動時に止まる。** 既定値で黙って動き続けない
——「設定したつもりが効いていない」まま動くほうが、はるかに厄介だから。

---

## LLM を有効にする

```bash
# .env に CHAT_LLM_ENABLED=1 と CHAT_ALLOW_LLM=1 を書いてから
docker compose --profile llm up -d --build
```

起動すると `CHAT_MODELS` のモデルを順に自動 pull する（数百MB〜数GB）。
画面の状態表示で進み具合が見える。

⚠ **GPU 無しなら 1〜3B 級に限ること。** 7B 以上は CPU では実用にならない。
⚠ **`CHAT_MAX_CONCURRENT` を 1 から上げないこと。** CPU 推論では
2本流しても捌ける数は増えず、両方が遅くなるだけ。

---

## 公開するときの注意

**このまま外に出さないこと。** 既定は `127.0.0.1` にだけ出している。
外へ出すなら、少なくとも次を守る。

### 1. 前に TLS を張った逆代理を置く

Caddy か nginx を前に置き、`chat-gateway` は逆代理からだけ届くようにする。
`chat-ai` を直接外に出してはならない（前段のレート制限を素通りされる）。

### 2. レート制限を緩めない

`CHAT_RATE_PER_MIN` / `CHAT_RATE_BURST` は IP ごとの上限。
規則だけの応答は軽いが、LLM を有効にすると **1回の質問が数十秒 CPU を占める**。
制限が無ければ、一人で全部持っていける。

### 3. `CHAT_ALLOW_LLM` の意味を理解する

「LLM を使えるか」は**二つ**あり、両方 1 でなければ LLM は動かない。

| 項目 | どこ | 意味 |
|---|---|---|
| `CHAT_LLM_ENABLED` | 後段 | サーバーとして LLM を使える状態か（ollama が要る） |
| `CHAT_ALLOW_LLM` | 前段 | **この口**から LLM を使わせるか |

分けてあるのは、**同じ頭を性質の違う口が共有する**ため。
Web（匿名・誰でも来る）は `0`、LINE（友だち追加済み・相手が判る）は `1`、
というように口ごとに変えられる。

⚠ この項目は**ブラウザからは変えられない**。前段は受け取った JSON を
そのまま転送せず、自分の設定値で組み直して後段へ送る。

### 4. 匿名の口で LLM を有効にするのは、よく考えてから

小さいモデルは嘘をつく。実際にこの lab で観測したもの:

- 「Go は日本で開発された言語」
- 実在する商品について「販売しておりません」

看板として置くなら、**間違った答えが一つ出る損失**のほうが、
「何でも答えられる」利点より大きいことが多い。
この lab の公開版が `CHAT_LLM_ENABLED=0` なのはそのため
（負荷ではなく中身の理由）。

規則だけの応答は、書いたことしか言わない。**それが強みでござる。**

### 5. 事実は必ず内容ファイルに書く

料金・営業時間・URL のように答えが決まっているものを LLM に推測させない。
できないこと（計算・最新の出来事・医療や法律の相談）は
`cant_do` のような意図で**先に断る**。
心構えは [`content/README.md`](../apps/chat-ai/content/README.md#書くときの心構え) に。

---

## 動いているか確かめる

```bash
curl -s http://127.0.0.1:8080/healthz                 # → {"status":"ok","version":"0.1.1"}
curl -s http://127.0.0.1:8080/api/persona | head -c 200   # 画面に配る中身
docker compose exec -T chat-ai python -m pytest -q    # 意図分類の回帰
docker compose logs -f chat-gateway
```

起動時のログに **意図を何件読んだか**と、
**外した意図があればその理由**が出る。数が合わなければそこを見る。

---

## 版

前段と後段は**揃った版**で配る。どちらの `/healthz` にも出るので、
配った先で「何が動いているのか」を確かめられる（イメージのタグは付け替えられる）。

```bash
curl -s http://127.0.0.1:8080/healthz            # 前段
docker compose logs chat-ai | head -1            # 後段（起動ログの先頭）
```

変更の記録は [`CHANGELOG.md`](../apps/chat-ai/CHANGELOG.md)。
1.0.0 より前なので、**細かい変更でも中身が入れ替わることがある**。

---

## ライセンス

コードは MIT（[`LICENSE`](../LICENSE)）。
⚠ **モデルの重みは別**。gemma3 は MIT ではなく Gemma 利用規約に従う。
[`THIRD-PARTY.md`](../THIRD-PARTY.md) を読むこと。
