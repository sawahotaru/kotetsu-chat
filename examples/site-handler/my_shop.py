"""設置ごとの追加処理（連携）の見本。

**枠組みの一部ではない。** 自分の在庫・予約・会員システムに繋ぐための入口で、
書くのは「外から取ってきて、日本語の一文にする」ところだけ。

置き方は二つ。どちらでも同じように動く。

  1. 内容ファイルの隣（推奨）— `CHAT_CONTENT_DIR` の中に置く

        my-content/
          persona.yaml
          intents.yaml
          my_shop.py     ← ここ

     内容フォルダを丸ごと差し替えれば、**繋ぎ先ごと入れ替わる**。
     人格だけ差し替えて在庫の引き方が古いまま、という食い違いが起きない。

  2. イメージの中（`apps/chat-ai/` に置いてビルドし直す）

そのうえで環境変数で名乗らせる:

    CHAT_SITE_HANDLERS=my_shop        # 複数なら my_shop,my_clinic

そして `intents.yaml` の意図から `handler:` で呼ぶ:

    - name: shop
      examples: [商品を教えて, 何が買えるの]
      must: [商品, 買える]
      handler: products          # ← 下の register() が返す名前

⚠ **登録していない処理を参照している意図は、起動時に自動で外れる**（記録に残る）。
   店を持たない設置では商品検索の意図ごと消えるのが正しく、
   押しても何も起きぬ案内を残すのは不親切だから。

⚠ **読み取りだけにすること。** 注文の確定や予約の書き込みをチャットにさせない。
   案内はして、実行は本物の画面へ誘導する。取り違えたときの損害が桁違いになる。
"""

import logging
import os
import re

import httpx

log = logging.getLogger("chat-ai.my_shop")

# ⚠ 接続先はコードに直書きせず環境変数で受ける。
#    同じ内容を別のホストへ置くとき、このファイルを編集せずに済む。
SHOP_API = os.getenv("MY_SHOP_API", "https://example.com/api")
SHOP_URL = os.getenv("MY_SHOP_URL", "https://example.com/shop")

# ⚠ 必ず短い上限を付ける。相手が黙り込んだとき、会話ごと止まってしまう。
TIMEOUT = httpx.Timeout(8.0, connect=4.0)


async def search_products(text: str) -> str:
    """商品を探して、日本語の一覧にして返す。

    引数は利用者の発話そのもの。ここから予算などを読み取る。
    戻り値は Markdown（太字・リンク・表が使える）。
    """
    budget = _extract_budget(text)

    try:
        async with httpx.AsyncClient(timeout=TIMEOUT) as client:
            resp = await client.get(f"{SHOP_API}/products", params={"size": 100})
            resp.raise_for_status()
            items = resp.json()
    except (httpx.HTTPError, ValueError) as e:
        # ⚠ 失敗しても会話は続ける。**利用者に行き先を残す**のが肝心。
        log.warning("商品の取得に失敗: %s", e)
        return f"ただいま在庫を確認できませんでした。お店は {SHOP_URL} でご覧いただけます。"

    if budget is not None:
        items = [i for i in items if _price(i) is not None and _price(i) <= budget]
    if not items:
        return f"ご希望に合う品が見つかりませんでした。お店は {SHOP_URL} をご覧ください。"

    lines = [f"- **{i.get('name', '(名称不明)')}** … {_price(i):,.0f} 円" for i in items[:5]]
    head = f"{budget:,.0f} 円以内で" if budget is not None else "こちらなど"
    return "\n".join([f"{head}いかがでしょう。", "", *lines, "", f"すべては {SHOP_URL} で。"])


def _price(item: dict):
    try:
        return float(item.get("price"))
    except (TypeError, ValueError):
        return None


def _extract_budget(text: str):
    """「3000円以内」「5千円くらい」から予算を拾う。取れなければ None。"""
    m = re.search(r"(\d[\d,]*)\s*円", text)
    if m:
        return float(m.group(1).replace(",", ""))
    m = re.search(r"(\d+)\s*千円", text)
    if m:
        return float(m.group(1)) * 1000
    m = re.search(r"(\d+)\s*万円", text)
    if m:
        return float(m.group(1)) * 10000
    return None


def register() -> dict:
    """この追加が提供する処理を名乗る（読み込み側から呼ばれる入口）。

    ここに書いた名前が `intents.yaml` の `handler:` と対応する。
    """
    return {
        "products": search_products,
    }
