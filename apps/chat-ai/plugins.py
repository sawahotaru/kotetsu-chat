"""設置ごとの追加処理（連携）の読み込み。

枠組みが常に持つのは weather / wikipedia / trivia の三つだけ。
それ以外——この lab なら店(ec-api)や予約システム(clinic)への問い合わせ——は
**設置ごとの追加**であって、枠組みの一部ではない。

そこで、追加の処理は外の Python モジュールに置き、環境変数で読み込む:

    CHAT_SITE_HANDLERS=my_shop          # 省略可。複数なら my_shop,my_clinic

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


def load_site_handlers(spec: str | None = None) -> dict:
    """CHAT_SITE_HANDLERS を読み、名前→処理 の辞書にまとめて返す。"""
    raw = os.getenv("CHAT_SITE_HANDLERS", "") if spec is None else spec
    names = [n.strip() for n in raw.split(",") if n.strip()]
    if not names:
        log.info("設置ごとの追加は無し（枠組みの処理だけで動く）")
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
                f"CHAT_SITE_HANDLERS の {name!r} を読み込めません: {e}\n"
                f"  {name}.py を内容ファイルの隣（{settings.CONTENT_DIR}）に置いたか、\n"
                f"  消したのなら CHAT_SITE_HANDLERS からも外したか確かめてください"
            ) from None

        register = getattr(module, "register", None)
        if not callable(register):
            raise PluginError(f"{name} に register() がありません（名前→処理 の辞書を返す関数）")

        provided = register()
        if not isinstance(provided, dict):
            raise PluginError(f"{name}.register() が辞書を返しませんでした（{type(provided).__name__}）")

        # 同じ名前を二つの追加が名乗ると、どちらが効いているか誰にも分からなくなる。
        for key in provided:
            if key in handlers:
                raise PluginError(f"処理 {key!r} が複数の追加から登録されています（{name} と重複）")
        handlers.update(provided)
        log.info("追加を読み込んだ: %s（%s）", name, ", ".join(provided) or "処理なし")

    return handlers
