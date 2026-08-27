"""調べ物の「語の切り出し」の単体試験。

外部へは出ない（Wikipedia は叩かず、文字列処理だけを見る）ので速い。

ここが緩むと事故る形は二つある:

  取りこぼす … 「織田信長とは」が語を返さず、調べ物の意図に落ちない
  奪う       … 「このサイトとは」から語を取ってしまい、lab_about を奪う

どちらも実際に踏んだ。両方向を必ず試す。
"""

import pytest

from wikipedia import _relevant, extract_term, matches

# 語が取れること。取れた語も確かめる（空でなければ良し、では緩すぎる）。
EXTRACT = [
    ("量子力学とは何ですか", "量子力学"),
    ("ハチ公について教えて", "ハチ公"),
    ("アルデンテって何", "アルデンテ"),
    ("フェルマーの最終定理の意味を教えて", "フェルマーの最終定理"),
    ("織田信長とは", "織田信長"),
    ("織田信長という人とは？", "織田信長"),
    ("織田信長って何者？", "織田信長"),
    ("織田信長って誰", "織田信長"),
    ("エスプレッソとはどんな飲み物ですか", "エスプレッソ"),
    ("「枕草子」とは", "枕草子"),
    ("ねえ、光合成とは何ですか", "光合成"),   # 前置きを落とす
]

# 語を取ってはいけないもの。取ると別の意図を奪う。
NO_EXTRACT = [
    "こんにちは",
    "ありがとう",
    "何ができますか",
    "このサイトとは",        # lab_about のもの
    "こてつって誰",          # kotetsu のもの
    "lab とは",              # lab_about のもの
    "あなたは誰ですか",      # kotetsu のもの
    "とは",                  # 語が無い
]


@pytest.mark.parametrize("text,want", EXTRACT, ids=[t for t, _ in EXTRACT])
def test_extract_term(text, want):
    assert extract_term(text) == want


@pytest.mark.parametrize("text", NO_EXTRACT)
def test_no_term(text):
    assert extract_term(text) == "", "この言い回しから語を取ると、別の意図を奪う"


def test_long_sentence_is_not_a_term():
    """文であって語ではないものは投げない（検索が碌な結果を返さない）。"""
    assert extract_term("あ" * 50 + "とは") == ""


def test_matches_follows_extract_term():
    """matcher は「語が切り出せたか」と同じであること。"""
    for text, _ in EXTRACT:
        assert matches(text) is True
    for text in NO_EXTRACT:
        assert matches(text) is False


# 検索結果がその語の項目と言えるか。
# ⚠ ここが緩いと、掠っただけの項目を出典付きで出す（「ズブズブ田」→「電通」を実際に踏んだ）。
RELEVANT = [
    ("ハチ公", "忠犬ハチ公", True),
    ("ねこ", "ネコ", True),            # ひらがな/カタカナの差を潰す
    ("ｱﾙﾃﾞﾝﾃ", "アルデンテ", True),     # 半角/全角の差も
    ("ズブズブ田", "電通", False),
    ("量子力学", "野球", False),
]


@pytest.mark.parametrize("term,title,want", RELEVANT, ids=[f"{t}/{ti}" for t, ti, _ in RELEVANT])
def test_relevant(term, title, want):
    assert _relevant(term, title) is want
