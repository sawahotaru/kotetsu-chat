# kotetsu-chat

**規則で答えるチャット。LLM は最後の手段として置いてある。**

料金・営業時間・在庫のように**答えが決まっているもの**を、1B のモデルに推測させる理由は無い。
このチャットは質問を意図に分類して書いてある答えを返し、
拾えなかったものだけを（使うなら）小さな LLM へ回す。

```bash
git clone https://github.com/sawahotaru/kotetsu-chat.git
cd kotetsu-chat
docker compose up -d --build
# → http://127.0.0.1:8080/
```

動いているものは <https://lab.4510.be/chat/> で触れる（LLM を切った状態で運用中）。

---

## 何のための道具か

小さな商いの窓口——整体院、小売、教室——に置くことを想定している。
そこへ来る問いは、たいてい**同じものが繰り返し来る**。

| | 規則（既定） | LLM（任意） |
|---|---|---|
| 速さ | 数ミリ秒 | CPU で数十秒 |
| 正しさ | **書いたとおり** | 取り繕うことがある |
| 費用 | ほぼゼロ | CPU を占有する |
| 守備範囲 | 書いた分だけ | 何でも（当たるとは限らない） |

**この順序が設計の要点。** 賢さではなく、当てにできることを採っている。
だから既定では LLM を使わず、ollama も起動しない。

### 軽い

規則だけで動かすなら **RAM 320MB 程度**とディスク数百MB。
LLM を使わない間、ollama のイメージ（約1.5GB）もモデル（約1.8GB）も**置かれない**。
使いたくなったときに、自分の手元へ入れればよい（下記）。

---

## 中身と枠組みが分かれている

**コードには文言が一行も無い。** 名前・口調・回答・画面の文言・質問例・顔の画像は、
すべて内容フォルダの YAML にある。

```
apps/                  枠組み（意図分類・順番待ち・レート制限）
  chat-gateway/        前段: Go。入口、順番待ち、レート制限、画面の配信
    line.go            ← LINE の口（任意。既定では起動しない）
  chat-ai/             後段: Python。意図を読み、心得たものを即答する
    weather.py         ← 同梱の処理（外せる）
    wikipedia.py       ← 同梱の処理（外せる）
    content/           ← 同梱の見本その1（黒猫武将こてつ）
content-minimal/       ← 同梱の見本その2（整体院の受付・標準語）
```

**口（どこから話しかけられるか）と、処理（何に答えられるか）は、どちらも足し引きできる。**
Web の窓口のほかに LINE の口が同梱してあり、環境変数だけで生やす。
同じイメージを2つ起動して使い分ける（片方だけ LLM を通す、といった切り分けができる）。

差し替えは環境変数2行で済む。コードには触れない。

```bash
cp -r content-minimal my-content
$EDITOR my-content/persona.yaml    # 名前・URL・画面の文言
$EDITOR my-content/intents.yaml    # 意図（例文・誤爆止め・回答）

# .env に:
#   CHAT_CONTENT_HOST_DIR=./my-content
#   CHAT_CONTENT_DIR=/content
```

書き方は [`apps/chat-ai/content/README.md`](apps/chat-ai/content/README.md) にすべてある。

> ⚠️ **同梱の人格「黒猫武将こてつ」は、配布元の沢蛍由来のものです。**
> そのまま動かして試すのは構いませんが、**自分の設置で使うなら差し替えてください。**
> まっさらから始めるなら `content-minimal/` を写して使うのが早道です。

### 自分の在庫・予約システムに繋ぐ

意図から呼ぶ処理を Python で足せる。見本は
[`examples/site-handler/my_shop.py`](examples/site-handler/my_shop.py)。
**内容フォルダの隣に置く**——連携は「この設置の中身」の一部なので、
フォルダごと差し替えれば繋ぎ先ごと入れ替わる。

登録していない処理を参照している意図は、**起動時に自動で外れる**。
店を持たない設置では商品検索の意図ごと消えるのが正しく、
押しても何も起きない案内を残すのは不親切だから。

---

## できること（枠組みが最初から持っているもの）

- **意図分類** — 文字N-gram の類似度。形態素解析も学習も要らない（依存ライブラリ無し）
- **天気** — 気象庁（日本全国）／ Open-Meteo（海外）。鍵は不要。**外せる**
- **調べ物** — Wikipedia を引いて**出典を添えて**返す。推測で埋めない。**外せる**
- **小噺** — 順繰りに出す（無作為だと同じ話が続く）
- **守り** — レート制限、同時実行数の制限、順番待ち、長さの上限
- **LLM**（任意） — ollama へ。**口ごとに**有効/無効を切り替えられる

鍵の要る外部サービスは一つも使っていない。**API キーの設定は無い。**

---

## 能力を足す

**意図は YAML、処理は Python。**名前で結びつく。

```yaml
# my-content/intents.yaml
- name: 在庫を調べる
  examples: [在庫, 残ってる, 買える]
  handler: stock          # ↓ register() の名前と対応
```

```python
# my-content/my_shop.py
def register() -> dict:
    return {"stock": check_stock}      # (text: str) -> str。async でもよい
```

```bash
CHAT_SITE_HANDLERS=my_shop
```

**イメージも枠組みのコードも触らない。**書くのはこの Python 1枚だけ。
見本は [`examples/site-handler/my_shop.py`](examples/site-handler/my_shop.py)。

### 天気と調べ物も同じ仕組みで載っている

枠組みが**常に**持つのは小噺だけ。天気と調べ物は同梱してあるが、
どちらも他人のサービス（気象庁・Wikipedia）に触る処理なので、外せるようにしてある。

```bash
CHAT_HANDLERS=wikipedia          # 天気を外す
CHAT_HANDLERS=                   # 両方外す（決め打ちの答えだけで動く）
```

- 読み込まれなかった処理を参照する意図は、**起動時に自分から外れる**（記録に残る）
- 同じ処理名を二つが名乗ると**止まる**。差し替えたいなら先に `CHAT_HANDLERS` から外す
- ⚠️ **外したら案内文も直すこと。** 意図は落ちるが案内文は静的なテキストなので残る。
  「天気を聞かれよ」と促しておいて答えられぬのが、一番間の抜けた壊れ方でござる

---

## LLM を使いたいなら

ollama は同梱していない。使うときだけ、自分の手元へ入る。

```bash
# .env に書く
CHAT_LLM_ENABLED=1      # 後段: LLM を使える状態にする
CHAT_ALLOW_LLM=1        # 前段: この口から LLM を使わせる

docker compose --profile llm up -d    # ollama ごと起動し、モデルを自動で pull する
```

GPU が無いなら **1〜3B 級に限る**（既定は `gemma3:1b`）。7B 以上は CPU では実用にならない。
ディスクの空きを確認してから増やすこと（1モデル 0.8〜2GB）。

> ⚠️ **匿名の相手に開いた口で LLM を有効にするのは、よく考えてから。**
> 小さなモデルは、実在する商品を「取り扱っておりません」と平然と答える。
> 判断の分かれ目は [`docs/setup.md`](docs/setup.md) の「公開するときの注意」に書いてある。

---

## 詳しいこと

| 文書 | 中身 |
|---|---|
| [`docs/setup.md`](docs/setup.md) | 設定の一覧、公開するときの注意、動作確認の方法 |
| [`apps/chat-ai/content/README.md`](apps/chat-ai/content/README.md) | 内容ファイルの書き方（意図・回答・目印） |
| [`CHANGELOG.md`](CHANGELOG.md) | 変更の記録 |
| [`THIRD-PARTY.md`](THIRD-PARTY.md) | 外部の情報源と、その利用条件 |

## ライセンス

MIT（[`LICENSE`](LICENSE)）。同梱の人格「こてつ」の扱いは
[`THIRD-PARTY.md`](THIRD-PARTY.md) を参照のこと。
