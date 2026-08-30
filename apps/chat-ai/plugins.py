"""処理（ハンドラ）の読み込み。

**枠組みが常に持つのは trivia だけ。**小噺は外の世界に触らず、内容ファイルから
返すだけなので人格の一部と見なす。

weather / wikipedia は**同梱してあるが枠組みの一部ではない**。どちらも他人の
サービス（気象庁 / Open-Meteo / Wikipedia）に触る処理で、その可用性と規約に
枠組みを縛りつける筋合いがない。だから名前で読み込む物として扱う。

そして この lab の店(ec-api)や予約システム(clinic)への問い合わせは、
**設置ごとの追加**。置き場所が違うだけで、読み込み方は同じ。

    CHAT_HANDLERS=weather,wikipedia     # 既定。要らなければ減らす（空にもできる）
    CHAT_SITE_HANDLERS=my_shop          # 設置ごとの追加。省略可・カンマ区切り

二つに分かれているのは**意味の違い**であって仕組みの違いではない。
実際の読み込みは両方を繋げた一本の一覧で行う。

⚠ **既定を空にしない。**同梱の内容ファイル（intents.yaml）は案内文で
   「天気であれば申せる」「東京の天気は？と聞かれよ」と**直に促している**。
   意図は自動で外れるが、この案内文は静的なテキストなので残る。
   促しておいて答えられぬのが、この作りで最も間の抜けた壊れ方でござる。
   外すなら**内容ファイルも一緒に差し替えること**。

モジュールは `register()` を持ち、名前→処理 の辞書を返す:

    def register() -> dict:
        return {"products": search_products, "clinic": clinic_availability}

**置き場所は内容ファイルの隣**（`CHAT_CONTENT_DIR` の中）。

    my-content/
      persona.yaml
      intents.yaml
      my_shop.py     ← ここ

そこに置く理由は、**連携は「この設置の中身」の一部**だから。
人格も回答も差し替えるのに、在庫の引き方だけイメージを焼き直さねばならぬのは筋が悪い。
内容フォルダを丸ごと差し替えれば、繋ぎ先ごと入れ替わる。

同梱の `labinfo.py` だけは例外でイメージの中にある（この lab に固有の見本）。
イメージに同梱した名前も、内容ファイルの隣に置いた名前も、同じように書けばよい。

未指定なら何も読み込まない。読み込まれなかった処理を参照する意図は
`intents.build_intents()` が起動時に自動で外す（記録に残る）ので、
**追加を消しても起動する**。ここが片側だけ実装されていたのを直したのが本モジュール。

⚠ 指定した名前が読めないときは**黙って飛ばさず止める**。
   書いたのに効いていない、という一番気づきにくい失敗を作らないため。
   「消したいなら環境変数からも消す」——それが意思表示になる。
"""

import importlib
import logging
import os
import sys

import settings

log = logging.getLogger("chat-ai.plugins")


class PluginError(SystemExit):
    """連携モジュールが読めない・作法に合っていない。

    設定の誤りなので、追跡（traceback）ではなく一行の理由で止める。
    """

    def __init__(self, message: str) -> None:
        super().__init__(f"設定エラー: {message}")


# 同梱してあり、既定で読み込む処理。
#
# ⚠ ここを空にしてはいけない。同梱の内容ファイルが案内文で天気と調べ物を
#   直に挙げており、外すと「促しておいて答えられぬ」状態になる（冒頭の注記）。
DEFAULT_HANDLERS = "weather,wikipedia"


def _names(raw: str) -> list:
    return [n.strip() for n in raw.split(",") if n.strip()]


def load_handlers(spec: str | None = None) -> dict:
    """読み込む処理を決め、名前→処理 の辞書にまとめて返す。

    spec を渡さなければ CHAT_HANDLERS（既定 DEFAULT_HANDLERS）と
    CHAT_SITE_HANDLERS を繋げた一覧を使う。渡した場合はそれだけを使う（試験用）。
    """
    if spec is None:
        names = _names(os.getenv("CHAT_HANDLERS", DEFAULT_HANDLERS))
        names += [n for n in _names(os.getenv("CHAT_SITE_HANDLERS", "")) if n not in names]
    else:
        names = _names(spec)

    if not names:
        log.info("読み込む処理は無し（内容ファイルの決め打ちの答えだけで動く）")
        return {}

    # 内容ファイルの隣を探せるようにする。
    #
    # ⚠ 名指しされたものしか import しないので、置いてあるだけの .py は動かない。
    #   末尾に足すのは、同じ名前が枠組み側にもあったとき**枠組みを優先**するため
    #   （内容フォルダに weather.py を置かれて中身が入れ替わる、を防ぐ）。
    content_dir = str(settings.CONTENT_DIR)
    if content_dir not in sys.path:
        sys.path.append(content_dir)

    handlers: dict = {}
    for name in names:
        try:
            module = importlib.import_module(name)
        except ImportError as e:
            raise PluginError(
                f"CHAT_HANDLERS / CHAT_SITE_HANDLERS の {name!r} を読み込めません: {e}\n"
                f"  {name}.py を内容ファイルの隣（{settings.CONTENT_DIR}）に置いたか、\n"
                f"  消したのなら環境変数からも外したか確かめてください"
            ) from None

        register = getattr(module, "register", None)
        if not callable(register):
            raise PluginError(f"{name} に register() がありません（名前→処理 の辞書を返す関数）")

        provided = register()
        if not isinstance(provided, dict):
            raise PluginError(f"{name}.register() が辞書を返しませんでした（{type(provided).__name__}）")

        # 同じ名前を二つが名乗ると、どちらが効いているか誰にも分からなくなる。
        #
        # ⚠ 以前は同梱の処理（weather 等）だけが**黙って**上書きされていた。
        #   読み込みが二系統（app.py の CORE_HANDLERS と、ここ）に分かれており、
        #   この検査を通らなかったため。一本化してからは同じ扱いになる。
        #   差し替えたいなら「先に外してから名乗る」——それが意思表示になる。
        for key in provided:
            if key in handlers:
                raise PluginError(
                    f"処理 {key!r} が複数から登録されています（{name} と重複）\n"
                    f"  同梱の処理を差し替えたいのなら、CHAT_HANDLERS からその名前を外してください"
                )
        handlers.update(provided)
        log.info("追加を読み込んだ: %s（%s）", name, ", ".join(provided) or "処理なし")

    return handlers
