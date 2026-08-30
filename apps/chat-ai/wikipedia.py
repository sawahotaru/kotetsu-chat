"""調べ物（日本語版 Wikipedia）。

なぜ LLM ではなくこれを使うのか:

  ここに載っている LLM は 1B 級で、知らない事柄を平気で作る。
  「〇〇とは何か」に推測で答えさせるのは、この lab で最もやってはいけないことに近い。
  外から事実を引いて**そのまま渡す**なら、間違えようがないし出典も添えられる。
  これは賢さの問題ではなく、**責任の所在の問題**でござる——という設計判断。

  副産物として、LLM が無効な本番（CHAT_LLM_ENABLED=0）でも調べ物が成立する。
  規則だけで動く口に「知識」を足せるのは、この形だからこそ。

API はキー不要・無料。2段で引く:

  1. 検索  /w/api.php?action=query&list=search … 表記ゆれを吸収して正式な項目名を得る
  2. 要約  /api/rest_v1/page/summary/{title}  … 冒頭の要約と項目のURLを得る

⚠ 1 を飛ばして 2 を直接叩くと、「ネコ」は引けても「猫の飼い方」は引けない。
   利用者の言い回しをそのまま項目名として扱えないので、検索を挟む必要がある。
"""

import logging
import os
import re
import time
import unicodedata
import urllib.parse

import httpx

import intents
import settings

log = logging.getLogger("chat-ai.wikipedia")

# どの言語版を引くか。既定は日本語版。
#
# ⚠ **これは情報源の切り替えであって、多言語化ではない。**
#   引いてくる要約はその言語になるが、前後の言い回し（「見当たらなんだ」等）は
#   この摘要に書いてある日本語のまま。別の言語で配るなら、そちらも合わせて直す。
#   分かった上で使うこと。
LANG = os.getenv("WIKIPEDIA_LANG", "ja").strip() or "ja"

SEARCH = f"https://{LANG}.wikipedia.org/w/api.php"
SUMMARY = f"https://{LANG}.wikipedia.org/api/rest_v1/page/summary/{{title}}"
PAGE_URL = f"https://{LANG}.wikipedia.org/wiki/{{title}}"

# 出典の名乗り。日本語版のときは従来の呼び方をそのまま保つ。
SOURCE_LABEL = os.getenv("WIKIPEDIA_SOURCE_LABEL", "").strip() or (
    "日本語版 Wikipedia" if LANG == "ja" else f"Wikipedia ({LANG})"
)

TIMEOUT = httpx.Timeout(10.0, connect=5.0)

# Wikipedia は素性の知れない大量アクセスを嫌う（規約で UA の明示を求めている）。
# 気象庁と同じく、名乗ったうえで叩く。⚠ 配るなら CHAT_USER_AGENT を書き換えること。
HEADERS = settings.HEADERS

CACHE_TTL_SECONDS = int(os.getenv("WIKIPEDIA_CACHE_SECONDS", "3600"))

# 返す要約の長さ。長すぎると読まれず、短すぎると用を成さない。
MAX_EXTRACT = 420

# key -> (取得時刻, 本文)
_cache: dict[str, tuple[float, str]] = {}


def _cached(key: str) -> str | None:
    hit = _cache.get(key)
    if hit is None:
        return None
    fetched, value = hit
    if time.monotonic() - fetched > CACHE_TTL_SECONDS:
        del _cache[key]
        return None
    return value


def _store(key: str, value: str) -> str:
    if len(_cache) > 200:
        _cache.clear()
    _cache[key] = (time.monotonic(), value)
    return value


# 「〜とは」「〜について教えて」から調べたい語を切り出す。
# 長い順に試す（「について教えて」が「教えて」に食われないように）。
_TAIL = re.compile(
    r"(について(詳しく)?(教えて|知りたい|説明して)"
    # 「〜って何者」「〜とは誰」。人物を訊く形は独立して持つ。
    # 「って(何)」だけだと「って何者？」の「者？」が余って末尾一致に失敗する
    # （実際に「織田信長って何者？」を取りこぼした）。
    r"|(とは|って|は)(何者|なにもの|誰|だれ)(ですか|でしょうか)?"
    r"|は(何|なに)を(した|やった)(人|人物)(ですか)?"
    # 「〜とはどんな飲み物ですか」「〜ってどんな会社？」。
    # 名詞を並べて数えるとすぐ漏れる（「どんな物」は拾えて「どんな飲み物」は落ちた）ので、
    # 短い任意の語として受ける。長さを縛って、文ごと飲み込まないようにする。
    r"|(とは|って)どんな.{0,12}?(ですか|でしょうか)?"
    r"|(とは|って)どういう(意味|こと|もの)(ですか)?"
    r"|とは(何|なに)(ですか|でしょうか)?"
    r"|って(何|なに)(ですか)?"
    r"|の意味(を教えて|は)?"
    # ⚠ 「〇〇を教えて」は入れない。広すぎる。
    #    「料金プランを教えて」「使い方を教えて」「ニュースを教えて」まで調べ物と見なし、
    #    商談の入口が百科事典に吸われていた（実測で発覚）。
    #    調べ物と断定してよいのは「について教えて」まで。
    r"|を(調べて|説明して|検索して)"
    r"|とは"
    r"|ってなに"
    r"|って何"
    r"|を検索(して)?"
    r")[?？。、\s]*$"
)

# 切り出した語の末尾に残る飾り。「織田信長という人」→「織田信長」。
_TERM_TAIL = re.compile(r"(という|っていう|と言う)(人物|人|方|もの|物)?$")

# 文頭に付きがちな飾り。
_HEAD = re.compile(r"^(ねえ|ねぇ|あの|すみません|ちょっと|教えて[、,]?)[\s、,]*")

# 切り出した語がこれらだけなら、Wikipedia を引く意味が無い。
# 「あなたとは」「ここについて教えて」のような、この場のことを聞かれている場合。
#
# ⚠ **設置固有の語（案内役の名・屋号）はここに書かない。**
#    以前は「こてつ」「4510lab」が直に書いてあった。別の設置に配ると、
#    その屋号を尋ねられたときに百科事典を引きに行ってしまう
#    （「みどり整体院とは？」で Wikipedia を叩く）。
#    設置ごとの語は persona.yaml の self_words から足す。
_SELF_WORDS = {
    "あなた", "君", "お前", "ここ", "この", "そこ", "それ", "これ",
    "このサイト", "貴殿", "自分",
} | set(intents.SELF_WORDS)


def extract_term(text: str) -> str:
    """問いから調べたい語を取り出す。取れなければ空文字。"""
    t = text.strip()
    t = _HEAD.sub("", t)
    m = _TAIL.search(t)
    if not m:
        return ""
    term = t[: m.start()].strip()
    term = _TERM_TAIL.sub("", term).strip()
    # 「〇〇の意味」の「の」など、末尾に残る助詞を落とす
    term = re.sub(r"[はがのをにでとも]$", "", term).strip()
    term = term.strip("「」『』\"'　 ")
    if len(term) < 2 or term.lower() in _SELF_WORDS:
        return ""
    # 長すぎるものは文であって語ではない。検索に投げても碌な結果にならない。
    if len(term) > 40:
        return ""
    return term


def _fold(s: str) -> str:
    """比較用の正規化。全角半角・大小・ひらがな/カタカナの差を潰す。

    「ねこ」で引いて項目名が「ネコ」でも同じ物と見なせるようにする。
    """
    t = unicodedata.normalize("NFKC", s).lower()
    # ひらがな → カタカナ（コードポイントが 0x60 離れている）
    return "".join(chr(ord(c) + 0x60) if "ぁ" <= c <= "ゖ" else c for c in t)


def _relevant(term: str, title: str) -> bool:
    """検索結果がその語の項目と言えるかを判じる。

    ⚠ これが無いと事故る。Wikipedia の全文検索は緩く、実在しない語を投げても
       本文のどこかが掠っただけの項目を返す（「ズブズブ田」→「電通」を実際に踏んだ）。
       関わりの無い項目を出典付きで出すのは、推測で答えるより性質が悪い。
    """
    a, b = _fold(term), _fold(title)
    if a in b or b in a:
        return True
    # 部分的な言い換え（「忠犬ハチ公」対「ハチ公の像」等）を拾うため、
    # 文字の重なりでも見る。半分以上が共有されていれば同じ物を指していると見なす。
    common = set(a) & set(b)
    return len(common) / len(set(a)) >= 0.5 if a else False


async def _search_title(client: httpx.AsyncClient, term: str) -> str | None:
    """表記ゆれを吸収して、正式な項目名を1つ返す。

    上位3件のうち、その語の項目と言えるものだけを採る。
    どれも掠っていなければ None（＝見つからなかった）を返す。
    """
    resp = await client.get(
        SEARCH,
        params={
            "action": "query",
            "list": "search",
            "srsearch": term,
            "srlimit": 3,
            "format": "json",
            "utf8": 1,
        },
    )
    resp.raise_for_status()
    hits = resp.json().get("query", {}).get("search", [])
    for hit in hits:
        if _relevant(term, hit["title"]):
            return hit["title"]
    if hits:
        log.info("関わりの薄い結果しか無かったので捨てる: 候補=%s", [h["title"] for h in hits])
    return None


def _trim(extract: str) -> str:
    """要約を読める長さに収める。文の途中では切らない。"""
    if len(extract) <= MAX_EXTRACT:
        return extract
    cut = extract[:MAX_EXTRACT]
    # 最後の句点で切る。無ければ諦めて三点リーダを付ける。
    dot = cut.rfind("。")
    return cut[: dot + 1] if dot > MAX_EXTRACT // 2 else cut + "…"


async def answer(text: str) -> str:
    """「〇〇とは」に Wikipedia の要約で答える。

    ⚠ 外部が落ちてもチャットは動き続けるべきなので、例外は投げず文字列で返す。
    """
    term = extract_term(text)
    if not term:
        return (
            "はて、何について調べればよろしいか。\n\n"
            "「**〇〇とは？**」「**〇〇について教えて**」と申していただければ、"
            "百科事典を引いてまいり申す。"
        )

    cached = _cached(term)
    if cached is not None:
        return cached

    try:
        async with httpx.AsyncClient(timeout=TIMEOUT, headers=HEADERS) as client:
            title = await _search_title(client, term)
            if not title:
                return (
                    f"「{term}」で引いてみたが、百科事典には見当たらなんだ。\n"
                    "言い方を変えて、もう一度お試しくだされ。"
                )

            resp = await client.get(
                SUMMARY.format(title=urllib.parse.quote(title, safe="")),
                follow_redirects=True,
            )
            resp.raise_for_status()
            data = resp.json()
    except (httpx.HTTPError, ValueError) as e:
        log.warning("Wikipedia の取得に失敗: %s", e)
        return (
            "百科事典まで使いを出したが、戻ってこなんだ。\n"
            "しばし置いてから、もう一度お試しくだされ。"
        )

    # 曖昧さ回避のページ。要約が「〜は以下のいずれかを指す」となり役に立たない。
    if data.get("type") == "disambiguation":
        return (
            f"「{title}」は幾つもの意味を持つ語でござった。\n"
            "もう少し絞って——たとえば分野を添えて——お尋ねくだされ。"
        )

    extract = (data.get("extract") or "").strip()
    if not extract:
        return f"「{title}」の項目は見つかったが、要約が空でござった。"

    url = (
        data.get("content_urls", {}).get("desktop", {}).get("page")
        or PAGE_URL.format(title=urllib.parse.quote(title, safe=""))
    )

    body = (
        f"**{data.get('title', title)}**\n\n"
        f"{_trim(extract)}\n\n"
        f"— 出典: [{SOURCE_LABEL}]({url})\n"
        "※ 拙者が考えたのではなく、引いてまいったものでござる。"
    )
    return _store(term, body)


def matches(text: str) -> bool:
    """調べ物の言い回しかどうか。Intent の matcher として使う。

    「語が切り出せた＝調べ物の形をしている」と見なす。
    lab のことを指す語（「このサイト」「こてつ」）は extract_term が弾くので、
    ここが lab_about や kotetsu を奪うことは無い。
    """
    return bool(extract_term(text))


def register() -> dict:
    """この処理を名乗る（plugins.load_handlers から呼ばれる）。

    `matches` は `<名前>_matches` の綴りで登録する。intents.yaml の
    `matcher: wikipedia` から引かれ、語の並びではなく自前の判定で拾える。

    ⚠ 同梱してあるが**枠組みの一部ではない**（weather.register の注記を参照）。
    """
    return {"wikipedia": answer, "wikipedia_matches": matches}
