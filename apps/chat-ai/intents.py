"""意図の読み込み。

**ここには文言を書かない。** 人格も回答も `content/` の中にある。

  content/persona.yaml  … 名前・URL・LLM への指示・心得の外のときの返し
  content/intents.yaml  … 意図の一覧（例文・誤爆止め・回答）
  content/trivia.yaml   … 小噺

分けてある理由:

  この頭は「よく来る問いを安く正しく捌く」ための**枠組み**であって、
  黒猫武将こてつのためのものではない。別の店に据えるなら、
  差し替わるのは文言だけで、意図分類も順番待ちもレート制限もそのまま使える。
  文言がコードに埋まっていると、その切り分けができない。

置き場所は `CHAT_CONTENT_DIR` で差し替えられる。既定はこのファイルの隣の `content/`。

⚠ 回答の中の `{public}` `{source}` は persona.yaml の `site` に置き換わる。
   置換は str.format ではなく素の replace で行う。回答文には Markdown の表や
   波括弧が入り得るので、format にすると書式指定と解釈されて壊れる。
"""

import logging

import yaml

import settings
from router import Intent

log = logging.getLogger("chat-ai.intents")

# 置き場所の決め方は settings.py に一本化してある（weather.py も同じ所を見るため）。
CONTENT_DIR = settings.CONTENT_DIR


def _load(name: str):
    path = CONTENT_DIR / name
    if not path.is_file():
        # 文言が無ければ何も答えられない。起動を続ける意味が無いのでここで止める。
        raise SystemExit(f"設定エラー: 内容ファイルが見つかりません: {path}")
    try:
        with path.open(encoding="utf-8") as f:
            return yaml.safe_load(f)
    except yaml.YAMLError as e:
        raise SystemExit(f"設定エラー: {path} を読めません: {e}") from None


_persona = _load("persona.yaml") or {}
_site = _persona.get("site") or {}

PUBLIC = _site.get("public_url", "")
GITHUB = _site.get("source_url", "")
NAME = _persona.get("name", "案内役")


def _fill(text):
    """回答文の目印を、この設置の値に置き換える。"""
    if not isinstance(text, str):
        return text
    return text.replace("{public}", PUBLIC).replace("{source}", GITHUB)


SYSTEM_PROMPT = _fill(_persona.get("system_prompt", ""))

# 回答文のうち「LLM を使っているかどうか」で変わる部分は、この目印を埋めておき
# 応答時に差し替える。同じ頭を複数の口（Web / LINE）が共有し、口ごとに LLM の
# 有無が違うため、文面を1つに固定すると**どちらかで嘘になる**。
LLM_SLOT = _persona.get("llm_slot", "〔LLM〕")

_note = _persona.get("llm_note") or {}


def _note_pair(node) -> tuple:
    node = node or {}
    return _fill(node.get("enabled", "")), _fill(node.get("disabled", ""))


LLM_NOTE_ON, LLM_NOTE_OFF = _note_pair(_note)

# 名前つきの差し替え。llm_note の中に入れ子で書いたものは、
# 回答文の 〔LLM:名前〕 に対応する。
#
#   なぜ要るか: 「なぜ LLM を使っておらぬのか」のような問いは、答えが丸ごと
#   LLM の有無で変わる。一文の差し替え（〔LLM〕）では足りず、といって回答を
#   どちらか一方に固定すると、もう一方の設置で**嘘になる**。
_KEYED_NOTES = {k: _note_pair(v) for k, v in _note.items() if isinstance(v, dict)}


def _keyed_slot(key: str) -> str:
    """〔LLM〕 → 〔LLM:key〕。目印の綴りは persona 側で変えられるので、
    括りの最後の一文字の内側に名前を差し込む形で組み立てる。"""
    return LLM_SLOT[:-1] + ":" + key + LLM_SLOT[-1]


# 「この場のこと」を指す語。案内役の名や屋号を書く。
# 調べ物（wikipedia.py）が、この設置自身のことを外の百科事典に問い合わせないようにする。
SELF_WORDS = [w for w in (_persona.get("self_words") or []) if isinstance(w, str)]

# ルールで拾えず、LLM も無効なときの返し。
# 「分かりません」だけで終わらせると、利用者は何を聞けばよいか分からないまま去る。
UNKNOWN = _fill(_persona.get("unknown", ""))

TRIVIA = [_fill(t) for t in (_load("trivia.yaml") or [])]


# ---- 画面まわり ----
#
# 表題も名乗りも質問例も、この設置のもの。前段(Go)に埋め込むと差し替えられなくなるので、
# 内容ファイルに置いて前段へ渡す。前段は器のまま保つ。

def _fill_deep(node):
    """入れ子の辞書・配列の中の文字列をまとめて置き換える。"""
    if isinstance(node, dict):
        return {k: _fill_deep(v) for k, v in node.items()}
    if isinstance(node, list):
        return [_fill_deep(v) for v in node]
    return _fill(node)


UI = _fill_deep(_persona.get("ui") or {})

# 顔の画像。内容ファイルの隣に置く。無ければ顔を出さない（それで困らない作りにする）。
_avatar_name = (UI.get("avatar") or "").strip()
AVATAR_PATH = (CONTENT_DIR / _avatar_name) if _avatar_name else None
if AVATAR_PATH is not None and not AVATAR_PATH.is_file():
    log.warning("顔の画像が見つからない（顔なしで動かす）: %s", AVATAR_PATH)
    AVATAR_PATH = None


def ui_payload(llm_enabled: bool) -> dict:
    """前段が画面を組み立てるのに要るものを返す。

    LLM の有無で変わる一文はここで確定させる。前段に「どちらの文か」を
    判断させると、口ごとに設定が違うときに食い違う。
    """
    intro = dict(UI.get("intro") or {})
    note = intro.pop("note", None) or {}
    intro["note"] = note.get("enabled" if llm_enabled else "disabled", "")
    return {
        "name": NAME,
        "title": UI.get("title") or NAME,
        "heading": UI.get("heading") or NAME,
        "badge": UI.get("badge") or "",
        "home": UI.get("home") or {},
        "intro": intro,
        "mode_label": (UI.get("mode_label") or {}).get(
            "enabled" if llm_enabled else "disabled", ""),
        "examples": UI.get("examples") or [],
        # 画面の文言（ボタン・報せ）。**書かれていなくてよい。**
        # 前段が既定を持っており、ここに書かれた分だけが上書きされる。
        # 案内役の口調に合わせたい、あるいは別の言語で出したいときに使う。
        "labels": UI.get("labels") or {},
        "has_avatar": AVATAR_PATH is not None,
    }


def llm_note(enabled: bool) -> str:
    return LLM_NOTE_ON if enabled else LLM_NOTE_OFF


def apply_llm_notes(text: str, enabled: bool) -> str:
    """回答文の中の LLM 目印を、いまの設定に合わせて確定させる。

    名前つきを先に差し替える。〔LLM〕 は 〔LLM:reason〕 の一部には一致しない
    （閉じの一文字まで含めて見るため）が、順序を明示しておく。
    """
    for key, (on, off) in _KEYED_NOTES.items():
        text = text.replace(_keyed_slot(key), on if enabled else off)
    return text.replace(LLM_SLOT, llm_note(enabled))


# ---- 小噺 ----
#
# 中身は内容ファイルにあるが、「順に出す」のは枠組みの仕事。
# 無作為にすると同じ話が続くことがあり、それが最も興を削ぐ。
_trivia_turn = 0


def tell_trivia(text: str) -> str:
    """小噺を一つ、順繰りに返す。

    ⚠ 締めで「他には？」と促してはならぬ。それは confused（品書き）に取られる。
       自分で促した言葉が別の所へ飛ぶのは、最も間の抜けた不具合でござる。
    """
    global _trivia_turn
    if not TRIVIA:
        return "語れる話を持ち合わせておらぬ。"
    item = TRIVIA[_trivia_turn % len(TRIVIA)]
    _trivia_turn += 1
    return item + "\n\nまだ幾つかござる。「**もう一つ**」と申されよ。"


# ---- 意図の組み立て ----

def build_intents(handlers) -> list:
    """内容ファイルから意図の一覧を組み立てる。

    handlers は動的に答える処理の辞書。**この設置に無い処理を参照している意図は、
    黙って落とさず記録したうえで外す。** 例えば店や予約システムを持たない設置では
    商品検索の意図ごと消えるのが正しく、押しても何も起きぬ案内を残すのは不親切。

    matcher は `<名前>_matches` という綴りで handlers から引く。
    """
    # 小噺は枠組みが持つ。内容ファイルを差し替えれば中身だけ変わる。
    registry = {"trivia": tell_trivia, **(handlers or {})}

    intents, skipped = [], []
    for i, raw in enumerate(_load("intents.yaml") or []):
        name = raw.get("name")
        if not name:
            raise SystemExit(f"設定エラー: intents.yaml の {i + 1} 番目に name がありません")

        # ⚠ 「登録されていない」と「登録された値が None」を混同しないこと。
        #    分類だけを試すときは処理を呼ばないので None を登録する。
        #    値で判じると、その場合に意図が丸ごと消える（実際に回帰試験が15件落ちた）。
        handler = None
        if raw.get("handler"):
            if raw["handler"] not in registry:
                skipped.append(f"{name}(handler={raw['handler']})")
                continue
            handler = registry[raw["handler"]]

        matcher = None
        if raw.get("matcher"):
            key = raw["matcher"] + "_matches"
            if key not in registry:
                skipped.append(f"{name}(matcher={raw['matcher']})")
                continue
            matcher = registry[key]

        # 書かれているかどうかで判ずる（解決した値ではなく）。
        if not raw.get("handler") and not raw.get("answer"):
            raise SystemExit(f"設定エラー: 意図 {name!r} に answer も handler もありません")

        intents.append(Intent(
            name,
            raw.get("examples") or [],
            answer=_fill(raw.get("answer")),
            handler=handler,
            must=raw.get("must") or [],
            priority=raw.get("priority", 0),
            matcher=matcher,
        ))

    names = [it.name for it in intents]
    if len(names) != len(set(names)):
        dup = sorted({n for n in names if names.count(n) > 1})
        raise SystemExit(f"設定エラー: intents.yaml に同じ name が複数あります: {', '.join(dup)}")

    if skipped:
        log.info("この設置に無い処理を参照する意図を外した: %s", ", ".join(skipped))
    log.info("意図を %d 件読み込んだ（%s）", len(intents), CONTENT_DIR)
    return intents
